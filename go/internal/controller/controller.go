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
	Active(ctx context.Context) ([]store.Run, error)
	Get(ctx context.Context, id string) (store.Run, error)
	RecordAction(ctx context.Context, runID string, kind store.ActionKind, externalID string) error
	SubmitResult(ctx context.Context, id string, res review.Result) error
	Transition(ctx context.Context, id string, from, to run.State, reason string) error
	RecordUsage(ctx context.Context, runID string, u store.Usage) error
	RecordFetchTiming(ctx context.Context, runID string, started, finished time.Time) error
}

// GitHubClient is the subset of *githubapp.Client the controller uses.
type GitHubClient interface {
	GetPull(ctx context.Context, repo string, number int) (githubapp.Pull, error)
	ListReviews(ctx context.Context, repo string, number int) ([]githubapp.Review, error)
	DismissReview(ctx context.Context, repo string, number int, reviewID int64, message string) error
	CreateReview(ctx context.Context, repo string, number int, r githubapp.NewReview) (int64, error)
	CreateCheckRun(ctx context.Context, repo string, cr githubapp.CheckRun) (int64, error)
	UpdateCheckRun(ctx context.Context, repo string, id int64, cr githubapp.CheckRun) error
}

// GitHub hands out installation clients and fetch's read-only tokens.
type GitHub interface {
	Client(installationID int64) GitHubClient
	FetchToken(ctx context.Context, installationID int64, repo string) (string, error)
}

// Kube is the subset of *kube.Client the controller uses.
type Kube interface {
	CreateJob(ctx context.Context, job kube.Job) (kube.Job, error)
	GetJob(ctx context.Context, name string) (kube.Job, error)
	CreateSecret(ctx context.Context, s kube.Secret) error
	GetSecret(ctx context.Context, name string) (kube.Secret, error)
	ListPods(ctx context.Context, selector string) ([]kube.Pod, error)
}

// Config tunes the controller.
type Config struct {
	// MaxRunning caps runs with a live runner Job.
	MaxRunning int
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
}

func New(cfg Config, s Store, gh GitHub, k Kube, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, store: s, gh: gh, kube: k, log: log, wake: make(chan struct{}, 1)}
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

// Reconcile claims queued runs and advances every active run by as many steps
// as it can. Errors are logged and retried on the next pass.
func (c *Controller) Reconcile(ctx context.Context) {
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
	case run.StateFailed:
		err := c.reportFailure(ctx, r)
		if permanent(err) {
			// There is nowhere left to report the failure; stop trying.
			c.runLog(r).Warn("cannot report failure on GitHub", "err", err)
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
		id, err := gh.CreateCheckRun(ctx, r.Repo, InProgressCheckRun(r.ID, r.HeadSHA))
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
		return c.fail(ctx, r, run.StateRunning, c.jobFailure(ctx, job))
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

// jobFailure explains a failed Job from its Pod, falling back to the Job condition.
func (c *Controller) jobFailure(ctx context.Context, job kube.Job) string {
	pods, err := c.kube.ListPods(ctx, "job-name="+job.Metadata.Name)
	if err == nil {
		for _, p := range pods {
			if msg := PodFailure(p); msg != "" {
				return msg
			}
		}
	}
	cond, _ := job.Status.Condition(kube.JobFailed)
	return fmt.Sprintf("runner job failed: %s %s", cond.Reason, cond.Message)
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

// reportFailure completes the run's Check Run as failed, creating one if the
// run failed before opening it, so every failure is visible on the PR.
func (c *Controller) reportFailure(ctx context.Context, r store.Run) error {
	gh := c.gh.Client(r.InstallationID)
	return c.once(ctx, &r, store.ActionCheckRunComplete, func() (string, error) {
		if checkRunID, opened := r.Actions[store.ActionCheckRunCreate]; opened {
			id, err := strconv.ParseInt(checkRunID, 10, 64)
			if err != nil {
				return "", err
			}
			return checkRunID, gh.UpdateCheckRun(ctx, r.Repo, id, FailedCheckRun(r.Error))
		}
		cr := FailedCheckRun(r.Error)
		cr.Name, cr.HeadSHA, cr.ExternalID = CheckName, r.HeadSHA, r.ID
		id, err := gh.CreateCheckRun(ctx, r.Repo, cr)
		return strconv.FormatInt(id, 10), err
	})
}

// publish casts the vote for a reviewed run and completes its Check Run.
func (c *Controller) publish(ctx context.Context, r store.Run) error {
	if r.Result == nil {
		return errors.New("reviewed run has no result")
	}
	res := *r.Result
	if !r.FetchTimed {
		c.recordFetchTiming(ctx, r)
	}
	gh := c.gh.Client(r.InstallationID)
	// The vote actually cast. If the controller restarted after voting, the
	// Check Run falls back to the verdict's vote.
	event := ReviewEvent(r.Verdict)
	err := c.once(ctx, &r, store.ActionReview, func() (string, error) {
		var err error
		event, err = c.voteEvent(ctx, gh, r)
		if err != nil {
			return "", err
		}
		id, err := gh.CreateReview(ctx, r.Repo, r.PRNumber, BuildReview(res, event))
		if githubapp.StatusOf(err) == http.StatusUnprocessableEntity && len(res.Comments) > 0 {
			c.runLog(r).Warn("inline comments rejected; folding them into the review body", "err", err)
			id, err = gh.CreateReview(ctx, r.Repo, r.PRNumber, BuildFoldedReview(res, event))
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

// recordFetchTiming copies when fetch ran from the runner Pod into the run, for
// the fetch phase metric (§7.1). It only feeds metrics, so a failure is logged
// and publishing goes on; the Pod lingers until the Job's TTL, so a later pass
// can still record it.
func (c *Controller) recordFetchTiming(ctx context.Context, r store.Run) {
	pods, err := c.kube.ListPods(ctx, "job-name="+r.JobName)
	if err != nil {
		c.runLog(r).Warn("read runner pod for fetch timing", "err", err)
		return
	}
	for _, p := range pods {
		if started, finished, ok := FetchTiming(p); ok {
			if err := c.store.RecordFetchTiming(ctx, r.ID, started, finished); err != nil {
				c.runLog(r).Warn("record fetch timing", "err", err)
			}
			return
		}
	}
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
