package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/repoconfig"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func (c *Controller) loadConfig(ctx context.Context, r store.Run) (store.Run, error) {
	if r.Config != nil {
		return r, nil
	}
	gh := c.gh.Client(r.InstallationID)
	pull, err := gh.GetPull(ctx, r.Repo, r.PRNumber)
	if err != nil {
		return r, err
	}
	if pull.Base.SHA == "" {
		return r, fmt.Errorf("PR has no base commit for repository policy")
	}
	data, err := gh.RepositoryConfig(ctx, r.Repo, pull.Base.SHA)
	if err != nil {
		return r, err
	}
	cfg, err := repoconfig.Parse(data, r.BudgetUSD, c.cfg.Job.Language)
	if err != nil {
		return r, &githubapp.APIError{Status: http.StatusUnprocessableEntity, Message: err.Error()}
	}
	if err = c.store.SaveConfig(ctx, r.ID, pull.Base.SHA, cfg); err != nil {
		return r, err
	}
	return c.store.Get(ctx, r.ID)
}

// decideMerge is the sole policy decision for both immediate and confirmed merges.
func decideMerge(r store.Run, confirmed bool) bool {
	if r.Config == nil || r.Verdict != review.VerdictApprove {
		return false
	}
	return r.Config.ApprovalMode == repoconfig.Auto || (r.Config.ApprovalMode == repoconfig.Manual && confirmed)
}

func (c *Controller) move(ctx context.Context, r store.Run, to run.State) error {
	err := c.store.Transition(ctx, r.ID, r.State, to, "")
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	return err
}

func (c *Controller) afterReview(ctx context.Context, r store.Run) error {
	next := run.StateDone
	if decideMerge(r, false) {
		next = run.StateMerging
	} else if r.Config != nil && r.Config.ApprovalMode == repoconfig.Manual && r.Verdict == review.VerdictApprove {
		next = run.StateAwaitingApproval
	}
	if next != run.StateDone {
		// Check again even when a previous pass already recorded the vote.
		p, err := c.gh.Client(r.InstallationID).GetPull(ctx, r.Repo, r.PRNumber)
		if err != nil {
			return err
		}
		if p.Head.SHA != r.HeadSHA || p.State != "open" {
			next = run.StateDone
		}
		// Remove the completed Job before entering any waiting state. Background
		// GC can briefly leave terminating Pods, but no runner is kept for approval.
		if err := c.deleteJob(ctx, r.JobName); err != nil {
			return err
		}
	}
	return c.move(ctx, r, next)
}

func (c *Controller) awaitApproval(ctx context.Context, r store.Run) error {
	gh := c.gh.Client(r.InstallationID)
	p, err := gh.GetPull(ctx, r.Repo, r.PRNumber)
	if err != nil {
		return err
	}
	if p.Head.SHA != r.HeadSHA || p.State != "open" {
		return c.move(ctx, r, run.StateSuperseded)
	}
	signals, err := c.store.MergeSignals(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, s := range signals {
		allowed, err := gh.CanWrite(ctx, r.Repo, s.Actor)
		if err != nil {
			return err
		}
		if allowed && s.ReviewID != 0 {
			rv, err := gh.GetReview(ctx, r.Repo, r.PRNumber, s.ReviewID)
			if err != nil && githubapp.StatusOf(err) != http.StatusNotFound {
				return err
			}
			allowed = err == nil && rv.User.Login == s.Actor && rv.State == "APPROVED" && rv.CommitID == r.HeadSHA
		}
		if decideMerge(r, allowed) {
			return c.move(ctx, r, run.StateMerging)
		}
		if err := c.store.IgnoreMergeSignal(ctx, s.DeliveryID); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) merge(ctx context.Context, r store.Run) error {
	gh := c.gh.Client(r.InstallationID)
	err := c.once(ctx, &r, store.ActionMerge, func() (string, error) {
		p, err := gh.GetPull(ctx, r.Repo, r.PRNumber)
		if err != nil {
			return "", err
		}
		if p.Head.SHA != r.HeadSHA {
			return "", &githubapp.APIError{Status: http.StatusConflict, Message: "PR head changed since review"}
		}
		// Recover when GitHub merged but the controller stopped before recording it.
		if p.Merged {
			if p.MergeCommitSHA == "" {
				return "", fmt.Errorf("merged PR has no merge commit yet")
			}
			return p.MergeCommitSHA, nil
		}
		if p.State != "open" {
			return "", &githubapp.APIError{Status: http.StatusMethodNotAllowed, Message: "PR is closed"}
		}
		return gh.MergePull(ctx, r.Repo, r.PRNumber, r.HeadSHA)
	})
	if status := githubapp.StatusOf(err); status == http.StatusMethodNotAllowed || status == http.StatusConflict {
		return c.fail(ctx, r, r.State, err.Error())
	}
	if err != nil {
		return err
	}
	return c.move(ctx, r, run.StateDone)
}
