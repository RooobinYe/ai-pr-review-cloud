// Package controller drives review runs through their state machine
// (docs/architecture.md §3, §6). It is the only component with side effects:
// it creates runner Jobs and performs every GitHub write, recording each write
// in github_action so a restart never repeats one.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// Store is the subset of *store.Store the controller uses.
type Store interface {
	Claim(ctx context.Context, maxRunning int, jobName func(id string, attempt int) string) ([]store.Run, error)
	Supersede(ctx context.Context) ([]string, error)
	Active(ctx context.Context) ([]store.Run, error)
	Get(ctx context.Context, id string) (store.Run, error)
	RecordAction(ctx context.Context, runID string, kind store.ActionKind, externalID string) error
	SubmitResult(ctx context.Context, id string, res review.Result) error
	Transition(ctx context.Context, id string, from, to run.State, reason string) error
	PutSession(ctx context.Context, id string, turn int, jsonl []byte) error
	GetSession(ctx context.Context, id string) ([]byte, int, error)
	PutUsage(ctx context.Context, id string, u store.Usage) error
}

// GitHubClient is the subset of *githubapp.Client the controller uses.
type GitHubClient interface {
	GetPull(ctx context.Context, repo string, number int) (githubapp.Pull, error)
	ListReviews(ctx context.Context, repo string, number int) ([]githubapp.Review, error)
	DismissReview(ctx context.Context, repo string, number int, reviewID int64, message string) error
	CreateReview(ctx context.Context, repo string, number int, r githubapp.NewReview) (int64, error)
	CreateCheckRun(ctx context.Context, repo string, cr githubapp.CheckRun) (int64, error)
	FindCheckRun(ctx context.Context, repo, sha, name, externalID string) (int64, error)
	UpdateCheckRun(ctx context.Context, repo string, id int64, cr githubapp.CheckRun) error
}

// GitHub hands out installation clients and fetch's read-only tokens, and
// manages the App's webhook deliveries.
type GitHub interface {
	Client(installationID int64) GitHubClient
	FetchToken(ctx context.Context, installationID int64, repo string) (string, error)
	ListDeliveries(ctx context.Context) ([]githubapp.Delivery, error)
	Redeliver(ctx context.Context, deliveryID int64) error
}

// Kube is the subset of *kube.Client the controller uses.
type Kube interface {
	CreateJob(ctx context.Context, job kube.Job) (kube.Job, error)
	GetJob(ctx context.Context, name string) (kube.Job, error)
	DeleteJob(ctx context.Context, name string) error
	CreateSecret(ctx context.Context, s kube.Secret) error
	GetSecret(ctx context.Context, name string) (kube.Secret, error)
	ListPods(ctx context.Context, selector string) ([]kube.Pod, error)
}

// Config tunes the controller.
type Config struct {
	// MaxRunning caps runs with a live runner Job.
	MaxRunning int
	// MaxAttempts caps runner Jobs per run; a failed attempt is retried
	// from the last session snapshot until then.
	MaxAttempts int
	// BotLogin is the App's bot user, "<slug>[bot]", whose approvals it dismisses.
	BotLogin string
	Job      JobConfig
}

type Controller struct {
	cfg   Config
	store Store
	gh    GitHub
	kube  Kube
	log   *slog.Logger
	wake  chan struct{}
	now   func() time.Time
}

func New(cfg Config, s Store, gh GitHub, k Kube, log *slog.Logger) *Controller {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	return &Controller{cfg: cfg, store: s, gh: gh, kube: k, log: log, wake: make(chan struct{}, 1), now: time.Now}
}

// startupGrace is how long an attempt may take to get its Job going before
// the Job's own deadline starts counting.
const startupGrace = 5 * time.Minute

// attemptDeadline bounds one attempt from its claim, covering the time spent
// creating the Job as well as the Job's activeDeadlineSeconds.
func (c *Controller) attemptDeadline() time.Duration {
	return time.Duration(c.cfg.Job.ActiveDeadlineSeconds)*time.Second + startupGrace
}

// Wake makes the loop reconcile now instead of at the next tick.
func (c *Controller) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run reconciles every interval, and whenever woken, until ctx is done.
func (c *Controller) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		c.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.wake:
		}
	}
}

// Reconcile supersedes outdated runs, claims queued runs and advances every
// active run by as many steps as it can. Errors are logged and retried on the
// next pass.
func (c *Controller) Reconcile(ctx context.Context) {
	if ids, err := c.store.Supersede(ctx); err != nil {
		c.log.Error("supersede runs", "err", err)
	} else {
		for _, id := range ids {
			c.log.Info("run superseded by a newer commit", "run_id", id)
		}
	}
	if claimed, err := c.store.Claim(ctx, c.cfg.MaxRunning, JobName); err != nil {
		c.log.Error("claim runs", "err", err)
	} else {
		for _, r := range claimed {
			c.runLog(r).Info("claimed run", "attempt", r.Attempt, "job", r.JobName)
		}
	}
	runs, err := c.store.Active(ctx)
	if err != nil {
		c.log.Error("list active runs", "err", err)
		return
	}
	for _, r := range runs {
		if ctx.Err() != nil {
			return
		}
		if err := c.advance(ctx, r); err != nil {
			c.runLog(r).Error("advance run", "state", r.State, "err", err)
		}
	}
}

func (c *Controller) runLog(r store.Run) *slog.Logger {
	return c.log.With("run_id", r.ID, "repo", r.Repo, "pr", r.PRNumber, "head_sha", r.HeadSHA)
}

func (c *Controller) advance(ctx context.Context, r store.Run) error {
	switch r.State {
	case run.StateRunning, run.StateReviewed:
		step := c.advanceRunning
		if r.State == run.StateReviewed {
			step = c.publish
		}
		err := step(ctx, r)
		if permanent(err) {
			// Retrying cannot help; the run would be retried forever and a
			// running one would hold its slot.
			return c.fail(ctx, r, r.State, err.Error())
		}
		return err
	case run.StateFailed, run.StateSuperseded:
		step := c.reportFailure
		if r.State == run.StateSuperseded {
			step = c.cancel
		}
		err := step(ctx, r)
		if permanent(err) {
			// There is nowhere left to report the outcome; stop trying.
			c.runLog(r).Warn("cannot report outcome on GitHub", "state", r.State, "err", err)
			return c.store.RecordAction(ctx, r.ID, store.ActionCheckRunComplete, "unreported")
		}
		return err
	}
	return nil
}

// once performs a GitHub action unless the run already did, and records it.
func (c *Controller) once(ctx context.Context, r *store.Run, kind store.ActionKind, do func() (string, error)) error {
	if r.Done(kind) {
		return nil
	}
	id, err := do()
	if err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	if err := c.store.RecordAction(ctx, r.ID, kind, id); err != nil {
		return err
	}
	if r.Actions == nil {
		r.Actions = map[store.ActionKind]string{}
	}
	r.Actions[kind] = id
	c.runLog(*r).Info("github action done", "kind", kind, "external_id", id)
	return nil
}

func (c *Controller) advanceRunning(ctx context.Context, r store.Run) error {
	if elapsed := c.now().Sub(r.ClaimedAt); elapsed > c.attemptDeadline() {
		// Also covers attempts whose Job could never be created, or whose
		// GitHub calls kept failing.
		if err := c.deleteJob(ctx, r.JobName); err != nil {
			return err
		}
		return c.fail(ctx, r, run.StateRunning, fmt.Sprintf("attempt %d timed out: no result %s after it was claimed", r.Attempt, elapsed.Round(time.Second)))
	}
	gh := c.gh.Client(r.InstallationID)
	err := c.once(ctx, &r, store.ActionDismissStaleApprovals, func() (string, error) {
		reviews, err := gh.ListReviews(ctx, r.Repo, r.PRNumber)
		if err != nil {
			return "", err
		}
		stale := StaleApprovals(reviews, c.cfg.BotLogin, r.HeadSHA)
		for _, id := range stale {
			msg := fmt.Sprintf("New commit %s pushed; AI Review will vote on it separately.", r.HeadSHA[:7])
			if err := gh.DismissReview(ctx, r.Repo, r.PRNumber, id, msg); err != nil {
				return "", err
			}
		}
		return strconv.Itoa(len(stale)), nil
	})
	if err != nil {
		return err
	}
	err = c.once(ctx, &r, store.ActionCheckRunCreate, func() (string, error) {
		// A previous controller may have created it and stopped before recording it.
		id, err := gh.FindCheckRun(ctx, r.Repo, r.HeadSHA, CheckName, r.ID)
		if err == nil && id == 0 {
			id, err = gh.CreateCheckRun(ctx, r.Repo, InProgressCheckRun(r.ID, r.HeadSHA))
		}
		return strconv.FormatInt(id, 10), err
	})
	if err != nil {
		return err
	}

	job, err := c.kube.GetJob(ctx, r.JobName)
	if errors.Is(err, kube.ErrNotFound) {
		return c.startJob(ctx, r)
	}
	if err != nil {
		return err
	}
	if _, failed := job.Status.Condition(kube.JobFailed); failed {
		reason, retryable := c.jobFailure(ctx, job)
		// A result may have arrived just before the Pod failed.
		if cur, err := c.store.Get(ctx, r.ID); err != nil || cur.State != run.StateRunning {
			return err
		}
		return c.retryOrFail(ctx, r, reason, retryable)
	}
	if _, complete := job.Status.Condition(kube.JobComplete); complete {
		// A result moves the run to reviewed before the runner exits. Re-read
		// in case it arrived after this pass listed the run.
		if cur, err := c.store.Get(ctx, r.ID); err != nil || cur.State != run.StateRunning {
			return err
		}
		return c.fail(ctx, r, run.StateRunning, "runner exited without submitting a review")
	}
	// The controller may have stopped between creating the Job and its Secret.
	if _, err := c.kube.GetSecret(ctx, runSecretName(r.JobName)); !errors.Is(err, kube.ErrNotFound) {
		return err
	}
	return c.createRunSecret(ctx, r, job)
}

func (c *Controller) startJob(ctx context.Context, r store.Run) error {
	job, err := c.kube.CreateJob(ctx, BuildJob(c.cfg.Job, r))
	if errors.Is(err, kube.ErrAlreadyExists) {
		job, err = c.kube.GetJob(ctx, r.JobName)
	}
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	c.runLog(r).Info("created job", "job", r.JobName)
	return c.createRunSecret(ctx, r, job)
}

// createRunSecret mints fetch's read-only GitHub token and the runner's
// callback token. The Secret is created after the Job so it can be owned by
// it; the Pod's containers wait until it exists.
func (c *Controller) createRunSecret(ctx context.Context, r store.Run, job kube.Job) error {
	githubToken, err := c.gh.FetchToken(ctx, r.InstallationID, r.Repo)
	if err != nil {
		return fmt.Errorf("mint fetch token: %w", err)
	}
	runToken := make([]byte, 32)
	if _, err := rand.Read(runToken); err != nil {
		return err
	}
	err = c.kube.CreateSecret(ctx, BuildRunSecret(job, githubToken, hex.EncodeToString(runToken)))
	if err != nil && !errors.Is(err, kube.ErrAlreadyExists) {
		return fmt.Errorf("create run secret: %w", err)
	}
	return nil
}

// jobFailure explains a failed Job from its Pod, falling back to the Job
// condition, and reports whether another attempt could succeed. A Pod that
// was deleted, evicted or killed leaves no exit code to go by and is retried.
func (c *Controller) jobFailure(ctx context.Context, job kube.Job) (reason string, retryable bool) {
	cond, _ := job.Status.Condition(kube.JobFailed)
	if cond.Reason == "DeadlineExceeded" {
		return fmt.Sprintf("runner did not finish within its %d-second deadline", c.cfg.Job.ActiveDeadlineSeconds), false
	}
	pods, err := c.kube.ListPods(ctx, "job-name="+job.Metadata.Name)
	if err == nil {
		for _, p := range pods {
			if msg, final := PodFailure(p); msg != "" {
				return msg, !final
			}
		}
	}
	return fmt.Sprintf("runner job failed: %s %s", cond.Reason, cond.Message), true
}

// retryOrFail queues a failed attempt again, or fails the run when the
// failure is final or the run is out of attempts. The next attempt resumes
// from the run's last session snapshot.
func (c *Controller) retryOrFail(ctx context.Context, r store.Run, reason string, retryable bool) error {
	if !retryable {
		return c.fail(ctx, r, run.StateRunning, reason)
	}
	if r.Attempt >= c.cfg.MaxAttempts {
		return c.fail(ctx, r, run.StateRunning, fmt.Sprintf("gave up after %d attempts; the last one failed: %s", r.Attempt, reason))
	}
	err := c.store.Transition(ctx, r.ID, run.StateRunning, run.StateQueued, reason)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	c.runLog(r).Warn("attempt failed; retrying", "attempt", r.Attempt, "reason", reason)
	// Showing the retry is cosmetic and the update idempotent, so a failure
	// here is only logged; the next attempt opens no new Check Run.
	if id, err := strconv.ParseInt(r.Actions[store.ActionCheckRunCreate], 10, 64); err == nil {
		cr := RetryingCheckRun(reason, r.Attempt+1, c.cfg.MaxAttempts)
		if err := c.gh.Client(r.InstallationID).UpdateCheckRun(ctx, r.Repo, id, cr); err != nil {
			c.runLog(r).Warn("show retry on check run", "err", err)
		}
	}
	c.Wake()
	return nil
}

// deleteJob deletes a run's Job and its Pods, if there is one.
func (c *Controller) deleteJob(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	if err := c.kube.DeleteJob(ctx, name); err != nil && !errors.Is(err, kube.ErrNotFound) {
		return fmt.Errorf("delete job: %w", err)
	}
	return nil
}

func (c *Controller) fail(ctx context.Context, r store.Run, from run.State, reason string) error {
	err := c.store.Transition(ctx, r.ID, from, run.StateFailed, reason)
	if errors.Is(err, store.ErrConflict) {
		return nil // the run moved on meanwhile, e.g. a result arrived
	}
	if err != nil {
		return err
	}
	r.State, r.Error = run.StateFailed, reason
	c.runLog(r).Warn("run failed", "reason", reason)
	return c.reportFailure(ctx, r)
}

// checkRun returns the run's Check Run: the recorded one, or one a previous
// controller created and stopped before recording. It returns 0 if there is none.
func checkRun(ctx context.Context, gh GitHubClient, r store.Run) (int64, error) {
	if id, opened := r.Actions[store.ActionCheckRunCreate]; opened {
		return strconv.ParseInt(id, 10, 64)
	}
	return gh.FindCheckRun(ctx, r.Repo, r.HeadSHA, CheckName, r.ID)
}

// reportFailure completes the run's Check Run as failed, creating one if the
// run failed before opening it, so every failure is visible on the PR.
func (c *Controller) reportFailure(ctx context.Context, r store.Run) error {
	gh := c.gh.Client(r.InstallationID)
	return c.once(ctx, &r, store.ActionCheckRunComplete, func() (string, error) {
		id, err := checkRun(ctx, gh, r)
		if err != nil {
			return "", err
		}
		if id != 0 {
			return strconv.FormatInt(id, 10), gh.UpdateCheckRun(ctx, r.Repo, id, FailedCheckRun(r.Error))
		}
		cr := FailedCheckRun(r.Error)
		cr.Name, cr.HeadSHA, cr.ExternalID = CheckName, r.HeadSHA, r.ID
		id, err = gh.CreateCheckRun(ctx, r.Repo, cr)
		return strconv.FormatInt(id, 10), err
	})
}

// cancel stops a superseded run: it deletes the runner Job, if any, and
// completes the Check Run it opened. A run superseded while queued opened
// none and needs none.
func (c *Controller) cancel(ctx context.Context, r store.Run) error {
	if err := c.deleteJob(ctx, r.JobName); err != nil {
		return err
	}
	gh := c.gh.Client(r.InstallationID)
	return c.once(ctx, &r, store.ActionCheckRunComplete, func() (string, error) {
		if r.Attempt == 0 {
			return "none", nil
		}
		id, err := checkRun(ctx, gh, r)
		if err != nil || id == 0 {
			return "none", err
		}
		return strconv.FormatInt(id, 10), gh.UpdateCheckRun(ctx, r.Repo, id, SupersededCheckRun())
	})
}

// publish casts the vote for a reviewed run and completes its Check Run.
func (c *Controller) publish(ctx context.Context, r store.Run) error {
	if r.Result == nil {
		return errors.New("reviewed run has no result")
	}
	res := *r.Result
	gh := c.gh.Client(r.InstallationID)
	// The vote actually cast. If the controller restarted after voting, the
	// Check Run falls back to the verdict's vote.
	event := ReviewEvent(r.Verdict)
	err := c.once(ctx, &r, store.ActionReview, func() (string, error) {
		reviews, err := gh.ListReviews(ctx, r.Repo, r.PRNumber)
		if err != nil {
			return "", err
		}
		// A previous controller may have voted and stopped before recording it.
		if prior, ok := FindVote(reviews, c.cfg.BotLogin, r.ID); ok {
			event = prior.Event
			c.runLog(r).Info("vote already cast; recording it", "review_id", prior.ID)
			return strconv.FormatInt(prior.ID, 10), nil
		}
		event, err = c.voteEvent(ctx, gh, r)
		if err != nil {
			return "", err
		}
		id, err := gh.CreateReview(ctx, r.Repo, r.PRNumber, BuildReview(r.ID, res, event))
		if githubapp.StatusOf(err) == http.StatusUnprocessableEntity && len(res.Comments) > 0 {
			c.runLog(r).Warn("inline comments rejected; folding them into the review body", "err", err)
			id, err = gh.CreateReview(ctx, r.Repo, r.PRNumber, BuildFoldedReview(r.ID, res, event))
		}
		return strconv.FormatInt(id, 10), err
	})
	if err != nil {
		return err
	}
	if checkRunID, opened := r.Actions[store.ActionCheckRunCreate]; opened {
		err = c.once(ctx, &r, store.ActionCheckRunComplete, func() (string, error) {
			id, err := strconv.ParseInt(checkRunID, 10, 64)
			if err != nil {
				return "", err
			}
			return checkRunID, gh.UpdateCheckRun(ctx, r.Repo, id, ReviewedCheckRun(res, event))
		})
		if err != nil {
			return err
		}
	}
	// vote is the only approval mode until M4 adds auto and manual (§6.3):
	// the run ends once the vote is cast.
	if err := c.store.Transition(ctx, r.ID, run.StateReviewed, run.StateDone, ""); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	c.runLog(r).Info("run done", "verdict", res.Verdict)
	return nil
}

// voteEvent picks the review event. An approval is only cast while the
// reviewed commit is still the PR head: the newer commit's run has already
// dismissed earlier approvals, and a late APPROVE would count for code it
// never saw.
func (c *Controller) voteEvent(ctx context.Context, gh GitHubClient, r store.Run) (string, error) {
	event := ReviewEvent(r.Verdict)
	if event != "APPROVE" {
		return event, nil
	}
	pull, err := gh.GetPull(ctx, r.Repo, r.PRNumber)
	if err != nil {
		return "", err
	}
	if pull.Head.SHA != r.HeadSHA {
		c.runLog(r).Info("PR head moved; voting COMMENT instead of APPROVE", "pr_head", pull.Head.SHA)
		return "COMMENT", nil
	}
	return event, nil
}

// AppGitHub adapts a GitHub App to the controller's GitHub interface.
type AppGitHub struct{ App *githubapp.App }

func (a AppGitHub) Client(installationID int64) GitHubClient {
	return a.App.Installation(installationID)
}

func (a AppGitHub) ListDeliveries(ctx context.Context) ([]githubapp.Delivery, error) {
	return a.App.ListDeliveries(ctx)
}

func (a AppGitHub) Redeliver(ctx context.Context, deliveryID int64) error {
	return a.App.Redeliver(ctx, deliveryID)
}

// FetchToken mints a token that can only read the one repository under review.
func (a AppGitHub) FetchToken(ctx context.Context, installationID int64, repo string) (string, error) {
	t, err := a.App.InstallationToken(ctx, installationID, &githubapp.TokenScope{
		Repositories: []string{githubapp.RepoName(repo)},
		Permissions:  map[string]string{"contents": "read", "pull_requests": "read", "metadata": "read"},
	})
	return t.Token, err
}

// permanent reports whether a GitHub error will not go away on retry, such as
// an uninstalled App (404) or missing permissions (403). Rate limits, which
// GitHub also reports as 403, are transient.
func permanent(err error) bool {
	switch githubapp.StatusOf(err) {
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	case http.StatusForbidden:
		return !strings.Contains(strings.ToLower(err.Error()), "rate limit")
	}
	return false
}
