package controller

import (
	"net/http"
	"strings"
	"testing"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/repoconfig"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func TestDecideMerge(t *testing.T) {
	tests := []struct {
		name, mode      string
		verdict         review.Verdict
		confirmed, want bool
	}{
		{"vote approval", repoconfig.Vote, review.VerdictApprove, false, false},
		{"vote confirmed approval", repoconfig.Vote, review.VerdictApprove, true, false},
		{"vote rejection", repoconfig.Vote, review.VerdictRequestChanges, true, false},
		{"auto approval", repoconfig.Auto, review.VerdictApprove, false, true},
		{"auto confirmed approval", repoconfig.Auto, review.VerdictApprove, true, true},
		{"auto rejection", repoconfig.Auto, review.VerdictRequestChanges, false, false},
		{"auto confirmed rejection", repoconfig.Auto, review.VerdictRequestChanges, true, false},
		{"manual waits", repoconfig.Manual, review.VerdictApprove, false, false},
		{"manual confirmed approval", repoconfig.Manual, review.VerdictApprove, true, true},
		{"manual rejection", repoconfig.Manual, review.VerdictRequestChanges, false, false},
		{"manual confirmed rejection", repoconfig.Manual, review.VerdictRequestChanges, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Run{Config: &repoconfig.Config{ApprovalMode: tt.mode}, Verdict: tt.verdict}
			if got := decideMerge(r, tt.confirmed); got != tt.want {
				t.Errorf("decideMerge() = %v, want %v", got, tt.want)
			}
		})
	}
	if decideMerge(store.Run{Verdict: review.VerdictApprove}, true) {
		t.Fatal("legacy run may not merge")
	}
}

func TestRepositoryPolicySnapshot(t *testing.T) {
	h := newHarness()
	h.gh.config = []byte("approvalMode: manual\nbudgetUSD: 0.12\nlanguage: en")
	h.reconcile()
	r := h.state()
	if r.Config == nil || r.ConfigSHA != oldSHA || r.BudgetUSD != 0.12 {
		t.Fatalf("snapshot %+v", r)
	}
	job := h.kube.jobs[r.JobName]
	found := false
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "REVIEW_LANGUAGE" && e.Value == "en" {
			found = true
		}
	}
	if !found || h.keys.issued[0].MaxBudgetUSD != 0.12 {
		t.Fatal("policy not applied to runner and key")
	}
	h.gh.config = []byte("approvalMode: auto\nbudgetUSD: 8")
	h.kube.finish(r.JobName, kube.JobFailed)
	h.reconcile()
	h.restart()
	h.reconcile()
	if len(h.gh.configRefs) != 1 || h.gh.configRefs[0] != oldSHA || h.state().Config.ApprovalMode != repoconfig.Manual {
		t.Fatal("retry changed policy")
	}
}

func TestInvalidPolicyFailsBeforeJob(t *testing.T) {
	h := newHarness()
	h.gh.config = []byte("approvalMode: typo")
	h.gh.reviews = []githubapp.Review{approvedReview(1, bot, oldSHA)}
	h.reconcile()
	if len(h.gh.dismissed) != 1 {
		t.Fatal("invalid config kept a stale approval")
	}
	if h.state().State != run.StateFailed || len(h.kube.jobs) != 0 || len(h.keys.issued) != 0 {
		t.Fatalf("run %+v", h.state())
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "failure" {
		t.Fatal("missing failed check")
	}
}

func TestAutoMergeFlow(t *testing.T) {
	h := newHarness()
	h.gh.config = []byte("approvalMode: auto")
	h.reconcile()
	if code := h.post(result(review.VerdictApprove)); code != http.StatusNoContent {
		t.Fatalf("post result: %d", code)
	}
	h.reconcile()
	if got := h.state().State; got != run.StateMerging {
		t.Fatalf("state %s, want merging", got)
	}
	if len(h.kube.jobs) != 0 || len(h.keys.live) != 0 {
		t.Fatal("runner or key kept alive after review")
	}
	if len(h.gh.posted) != 1 || h.gh.posted[0].Event != "APPROVE" {
		t.Fatalf("reviews %+v", h.gh.posted)
	}
	h.reconcile()
	h.restart()
	h.reconcile()
	if h.state().State != run.StateDone || len(h.gh.merges) != 1 || h.gh.merges[0] != headSHA {
		t.Fatalf("run %+v merges %v", h.state(), h.gh.merges)
	}
}

func TestManualMergeWaitsForWriter(t *testing.T) {
	h := newHarness()
	h.gh.config = []byte("approvalMode: manual")
	h.reconcile()
	if code := h.post(result(review.VerdictApprove)); code != http.StatusNoContent {
		t.Fatalf("post result: %d", code)
	}
	h.reconcile()
	if got := h.state().State; got != run.StateAwaitingApproval {
		t.Fatalf("state %s, want awaiting_approval", got)
	}
	if len(h.kube.jobs) != 0 || len(h.keys.live) != 0 {
		t.Fatal("runner or key kept alive while waiting")
	}
	h.restart()
	h.reconcile()
	if h.state().State != run.StateAwaitingApproval || len(h.gh.merges) != 0 {
		t.Fatal("restart did not preserve waiting")
	}

	h.store.signals = map[string][]store.MergeSignal{runID: {{DeliveryID: "reader", Actor: "human"}}}
	h.reconcile()
	if len(h.store.signals[runID]) != 0 || h.state().State != run.StateAwaitingApproval {
		t.Fatal("unprivileged confirmation was not ignored")
	}
	h.gh.writable = true
	h.store.signals[runID] = []store.MergeSignal{{DeliveryID: "writer", Actor: "human"}}
	h.reconcile()
	if got := h.state().State; got != run.StateMerging {
		t.Fatalf("state %s after writer confirmation, want merging", got)
	}
	h.reconcile()
	h.restart()
	h.reconcile()
	if h.state().State != run.StateDone || len(h.gh.merges) != 1 || h.gh.merges[0] != headSHA {
		t.Fatalf("run %+v merges %v", h.state(), h.gh.merges)
	}
}

func TestFailOnPreventsApprovalAndMerge(t *testing.T) {
	for _, mode := range []string{repoconfig.Vote, repoconfig.Auto, repoconfig.Manual} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness()
			h.gh.config = []byte("approvalMode: " + mode)
			h.reconcile()
			res := result(review.VerdictApprove, review.Comment{Path: "a.go", Line: 3, Severity: review.SeverityHigh, Confidence: review.ConfidenceHigh, Body: "bug"})
			if code := h.post(res); code != http.StatusNoContent {
				t.Fatalf("post result: %d", code)
			}
			h.reconcile()
			if r := h.state(); r.State != run.StateDone || r.Verdict != review.VerdictRequestChanges {
				t.Fatalf("run %+v", r)
			}
			if len(h.gh.posted) != 1 || h.gh.posted[0].Event != "COMMENT" || len(h.gh.merges) != 0 {
				t.Fatalf("reviews %+v, merges %v", h.gh.posted, h.gh.merges)
			}
			cr := h.gh.checkRuns[100]
			if cr[len(cr)-1].Conclusion != "neutral" {
				t.Fatalf("check %+v, want neutral", cr[len(cr)-1])
			}
		})
	}
}

func TestManualReviewConfirmation(t *testing.T) {
	for _, tt := range []struct {
		name, state, sha string
		want             bool
	}{
		{"approve", "APPROVED", headSHA, true}, {"dismissed", "DISMISSED", headSHA, false}, {"old head", "APPROVED", oldSHA, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness()
			h.gh.config = []byte("approvalMode: manual")
			h.gh.writable = true
			h.reconcile()
			h.post(result(review.VerdictApprove))
			h.reconcile()
			rv := approvedReview(12, "human", tt.sha)
			rv.State = tt.state
			h.gh.reviews = append(h.gh.reviews, rv)
			h.store.signals = map[string][]store.MergeSignal{runID: {{DeliveryID: "d", Actor: "human", ReviewID: 12}}}
			h.reconcile()
			if (h.state().State == run.StateMerging) != tt.want {
				t.Fatal(h.state().State)
			}
		})
	}
}

func TestRestartAfterMergeBeforeRecordDoesNotMergeTwice(t *testing.T) {
	h := newHarness()
	h.gh.config = []byte("approvalMode: auto")
	h.reconcile()
	h.post(result(review.VerdictApprove))
	h.reconcile()
	h.store.crashOn = store.ActionMerge
	h.reconcile()
	if h.state().State != run.StateMerging || h.state().Done(store.ActionMerge) || len(h.gh.merges) != 1 {
		t.Fatalf("setup: run %+v, merges %v", h.state(), h.gh.merges)
	}
	h.restart()
	h.reconcile()
	if r := h.state(); r.State != run.StateDone || r.Actions[store.ActionMerge] != "merged-sha" || len(h.gh.merges) != 1 {
		t.Fatalf("run %+v, merges %v", r, h.gh.merges)
	}
}

func TestMergeErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   run.State
	}{
		{"branch protection", http.StatusMethodNotAllowed, run.StateFailed},
		{"head changed", http.StatusConflict, run.StateFailed},
		{"invalid request", http.StatusUnprocessableEntity, run.StateFailed},
		{"temporary outage", http.StatusServiceUnavailable, run.StateMerging},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness()
			h.gh.config = []byte("approvalMode: auto")
			h.reconcile()
			h.post(result(review.VerdictApprove))
			h.reconcile()
			h.gh.mergeErr = &githubapp.APIError{Status: tt.status, Message: "cannot merge"}
			h.reconcile()
			if got := h.state().State; got != tt.want {
				t.Fatalf("state %s, want %s", got, tt.want)
			}
			if tt.want == run.StateFailed {
				cr := h.gh.checkRuns[100]
				if cr[len(cr)-1].Conclusion != "failure" {
					t.Fatal("merge refusal not visible")
				}
				h.reconcile()
				if len(h.gh.checkRuns[100]) != len(cr) {
					t.Fatal("failure reported twice")
				}
				return
			}
			h.gh.mergeErr = nil
			h.restart()
			h.reconcile()
			if h.state().State != run.StateDone || len(h.gh.merges) != 1 {
				t.Fatalf("run %+v, merges %v", h.state(), h.gh.merges)
			}
		})
	}
}

func TestChangedOrClosedPullNeverMerges(t *testing.T) {
	for _, mode := range []string{repoconfig.Auto, repoconfig.Manual} {
		for _, closed := range []bool{false, true} {
			h := newHarness()
			h.gh.config = []byte("approvalMode: " + mode)
			h.reconcile()
			h.post(result(review.VerdictApprove))
			h.reconcile()
			if closed {
				h.gh.closed = true
			} else {
				h.gh.head = oldSHA
			}
			h.reconcile()
			h.reconcile()
			if len(h.gh.merges) != 0 || !h.state().State.Terminal() {
				t.Fatalf("mode %s run %+v", mode, h.state())
			}
		}
	}
}

func TestResultThresholdStoredOnce(t *testing.T) {
	h := newHarness()
	h.reconcile()
	res := result(review.VerdictApprove, review.Comment{Path: "a", Line: 1, Severity: review.SeverityCritical, Confidence: review.ConfidenceHigh, Body: "bug"})
	if h.post(res) != http.StatusNoContent || h.post(res) != http.StatusNoContent {
		t.Fatal("callback not idempotent")
	}
	if strings.Count(h.state().Result.Body, "AI Review policy") != 1 {
		t.Fatal("duplicated policy explanation")
	}
}
