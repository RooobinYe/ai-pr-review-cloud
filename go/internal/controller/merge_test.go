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
	for _, mode := range []string{repoconfig.Vote, repoconfig.Auto, repoconfig.Manual} {
		for _, verdict := range review.Verdicts {
			for _, confirmed := range []bool{false, true} {
				r := store.Run{Config: &repoconfig.Config{ApprovalMode: mode}, Verdict: verdict}
				want := verdict == review.VerdictApprove && (mode == repoconfig.Auto || mode == repoconfig.Manual && confirmed)
				if decideMerge(r, confirmed) != want {
					t.Errorf("%s %s confirmed=%v", mode, verdict, confirmed)
				}
			}
		}
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
func TestModes(t *testing.T) {
	for _, mode := range []string{repoconfig.Vote, repoconfig.Auto, repoconfig.Manual} {
		for _, blocked := range []bool{false, true} {
			t.Run(mode+"/blocked="+map[bool]string{true: "yes", false: "no"}[blocked], func(t *testing.T) {
				h := newHarness()
				h.gh.config = []byte("approvalMode: " + mode)
				h.reconcile()
				res := result(review.VerdictApprove)
				if blocked {
					res.Comments = []review.Comment{{Path: "a.go", Line: 3, Severity: review.SeverityHigh, Confidence: review.ConfidenceHigh, Body: "bug"}}
				}
				if code := h.post(res); code != 204 {
					t.Fatal(code)
				}
				h.reconcile()
				if blocked || mode == repoconfig.Vote {
					if h.state().State != run.StateDone || len(h.gh.merges) != 0 {
						t.Fatalf("run %+v", h.state())
					}
					if blocked && (h.gh.posted[0].Event != "COMMENT" || h.state().Verdict != review.VerdictRequestChanges || !strings.Contains(h.state().Result.Body, "failOn=high")) {
						t.Fatal("failOn not enforced")
					}
					return
				}
				if len(h.kube.jobs) != 0 || len(h.keys.live) != 0 {
					t.Fatal("runner or key kept alive")
				}
				if mode == repoconfig.Manual {
					if h.state().State != run.StateAwaitingApproval {
						t.Fatal(h.state().State)
					}
					h.restart()
					h.reconcile()
					if h.state().State != run.StateAwaitingApproval {
						t.Fatal("restart stopped waiting")
					}
					h.store.signals = map[string][]store.MergeSignal{runID: {{DeliveryID: "d1", Actor: "human"}}}
					h.reconcile()
					if len(h.store.signals[runID]) != 0 || h.state().State != run.StateAwaitingApproval {
						t.Fatal("unprivileged confirmation allowed")
					}
					h.gh.writable = true
					h.store.signals[runID] = []store.MergeSignal{{DeliveryID: "d2", Actor: "human"}}
					h.reconcile()
				}
				h.reconcile()
				h.restart()
				h.reconcile()
				if h.state().State != run.StateDone || len(h.gh.merges) != 1 || h.gh.merges[0] != headSHA {
					t.Fatalf("run %+v merges %v", h.state(), h.gh.merges)
				}
			})
		}
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
func TestMergeRecoveryAndRejections(t *testing.T) {
	for _, status := range []int{0, 405, 409, 422, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHarness()
			h.gh.config = []byte("approvalMode: auto")
			h.reconcile()
			h.post(result(review.VerdictApprove))
			h.reconcile()
			if status == 0 {
				h.store.crashOn = store.ActionMerge
			} else {
				h.gh.mergeErr = &githubapp.APIError{Status: status, Message: "cannot merge"}
			}
			h.reconcile()
			if status == 405 || status == 409 || status == 422 {
				if h.state().State != run.StateFailed {
					t.Fatal(h.state().State)
				}
				cr := h.gh.checkRuns[100]
				if cr[len(cr)-1].Conclusion != "failure" {
					t.Fatal("merge refusal not visible")
				}
				return
			}
			if h.state().State != run.StateMerging {
				t.Fatal(h.state().State)
			}
			h.gh.mergeErr = nil
			h.restart()
			h.reconcile()
			if h.state().State != run.StateDone || len(h.gh.merges) != 1 {
				t.Fatalf("state %s merges %v", h.state().State, h.gh.merges)
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
	if h.post(res) != 204 || h.post(res) != 204 {
		t.Fatal("callback not idempotent")
	}
	if strings.Count(h.state().Result.Body, "AI Review policy") != 1 {
		t.Fatal("duplicated policy explanation")
	}
}
