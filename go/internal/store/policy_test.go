package store

import (
	"context"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/repoconfig"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"testing"
	"time"
)

func TestPolicyAndApprovalPersistence(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	id, _, err := s.RecordTrigger(ctx, "pr", newRun("head"))
	if err != nil {
		t.Fatal(err)
	}
	approval := Approval{Repo: "o/r", PRNumber: 7, InstallationID: 42, Actor: "human", CreatedAt: time.Now().Add(-time.Minute)}
	if err := s.RecordApproval(ctx, "early", approval); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, 1, jobName); err != nil {
		t.Fatal(err)
	}
	cfg, err := repoconfig.Parse([]byte("approvalMode: manual\nbudgetUSD: 0.1\nlanguage: en"), 0.5, "zh")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(ctx, id, "base", cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ApprovalMode = repoconfig.Auto
	cfg.BudgetUSD = 10
	if err := s.SaveConfig(ctx, id, "new-base", cfg); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Config.ApprovalMode != repoconfig.Manual || r.BudgetUSD != 0.1 || r.ConfigSHA != "base" {
		t.Fatalf("snapshot overwritten: %+v", r)
	}
	if err := s.SubmitResult(ctx, id, review.Result{Verdict: review.VerdictApprove}); err != nil {
		t.Fatal(err)
	}
	// A human can react as soon as the vote is visible, before the state moves.
	duringPublish := approval
	duringPublish.CreatedAt = time.Now().Add(time.Second)
	if err := s.RecordApproval(ctx, "during-publish", duringPublish); err != nil {
		t.Fatal(err)
	}
	if signals, err := s.MergeSignals(ctx, id); err != nil || len(signals) != 1 {
		t.Fatalf("lost publication-window confirmation: %+v %v", signals, err)
	}
	if err := s.IgnoreMergeSignal(ctx, "during-publish"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, id, run.StateReviewed, run.StateAwaitingApproval, ""); err != nil {
		t.Fatal(err)
	}
	// A webhook delivered after waiting started still cannot reuse an early click.
	if err := s.RecordApproval(ctx, "old-time", approval); err != nil {
		t.Fatal(err)
	}
	approval.CreatedAt = time.Now().Add(time.Second)
	for _, tt := range []struct {
		id, sha      string
		installation int64
	}{
		{"wrong-head", "other", 42}, {"wrong-installation", "head", 99}, {"comment", "", 42}, {"review", "head", 42},
	} {
		approval.HeadSHA, approval.InstallationID = tt.sha, tt.installation
		if tt.id == "review" {
			approval.ReviewID = 123
		}
		if err := s.RecordApproval(ctx, tt.id, approval); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordApproval(ctx, tt.id, approval); err != nil {
			t.Fatal(err)
		}
	}
	signals, err := s.MergeSignals(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || signals[0].DeliveryID != "comment" || signals[1].ReviewID != 123 {
		t.Fatalf("signals %+v", signals)
	}
	if err := s.IgnoreMergeSignal(ctx, "comment"); err != nil {
		t.Fatal(err)
	}
	if signals, err = s.MergeSignals(ctx, id); err != nil || len(signals) != 1 {
		t.Fatalf("signals %+v err %v", signals, err)
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active %+v err %v", active, err)
	}
	if err := s.RecordAction(ctx, id, ActionCheckRunComplete, "100"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, id, run.StateAwaitingApproval, run.StateMerging, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApproval(ctx, "late", approval); err != nil {
		t.Fatal(err)
	}
	if signals, err = s.MergeSignals(ctx, id); err != nil || len(signals) != 1 {
		t.Fatal("confirmation attached after waiting")
	}
	if err := s.Transition(ctx, id, run.StateMerging, run.StateFailed, "merge refused"); err != nil {
		t.Fatal(err)
	}
	active, err = s.Active(ctx)
	if err != nil || len(active) != 1 || active[0].Done(ActionCheckRunComplete) {
		t.Fatalf("failed merge cannot report: %+v %v", active, err)
	}
}
func TestSupersededApprovalCheckIsCancelled(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	id, _, err := s.RecordTrigger(ctx, "old", newRun("old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, 1, jobName); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(ctx, id, review.Result{Verdict: review.VerdictApprove}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, id, run.StateReviewed, run.StateAwaitingApproval, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAction(ctx, id, ActionCheckRunComplete, "100"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordTrigger(ctx, "new", newRun("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Supersede(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != run.StateSuperseded || r.Done(ActionCheckRunComplete) {
		t.Fatalf("old run %+v", r)
	}
}
