package controller

// Failure scenarios of docs/architecture.md §4.3: every component may stop at
// any point, and the run still finishes without repeating a GitHub write.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func (h *harness) putSession(token string, turn int, jsonl string) int {
	return h.call(http.MethodPut, "/runs/"+runID+"/session?turn="+strconv.Itoa(turn), token, []byte(jsonl)).Code
}

// losePod fails the current attempt's Job the way a deleted or evicted Pod
// does: the Pod is gone, so there is no exit code to go by.
func (h *harness) losePod() {
	h.kube.pods = nil
	h.kube.finish(h.state().JobName, kube.JobFailed)
}

func TestLostPodResumesFromSnapshot(t *testing.T) {
	h := newHarness()
	h.reconcile()
	first := h.state()
	firstToken := h.runToken()
	if code := h.putSession(firstToken, 3, `{"type":"session"}`+"\n"); code != http.StatusNoContent {
		t.Fatalf("put session: %d", code)
	}

	h.losePod()
	h.reconcile()
	if r := h.state(); r.State != run.StateQueued || r.Attempt != 1 {
		t.Fatalf("after losing the pod: %+v, want queued again", r)
	}
	h.reconcile() // the controller woke itself to claim the retry
	r := h.state()
	if r.State != run.StateRunning || r.Attempt != 2 || r.JobName != JobName(runID, 2) {
		t.Fatalf("second attempt: %+v", r)
	}
	if !strings.Contains(r.Error, "BackoffLimitExceeded") {
		t.Errorf("retry reason %q", r.Error)
	}
	if _, ok := h.kube.jobs[r.JobName]; !ok {
		t.Fatal("no job for the second attempt")
	}
	updates := h.gh.checkRuns[100]
	if len(h.gh.checkRuns) != 1 || updates[len(updates)-1].Output.Title != "Reviewing (attempt 2 of 3)" {
		t.Errorf("check runs %+v, want the first one showing the retry", h.gh.checkRuns)
	}

	// The new attempt resumes; the old one's token no longer works.
	if code := h.call(http.MethodGet, "/runs/"+runID+"/session", firstToken, nil).Code; code != http.StatusUnauthorized {
		t.Errorf("old attempt's token: %d, want 401", code)
	}
	rec := h.call(http.MethodGet, "/runs/"+runID+"/session", h.runToken(), nil)
	if rec.Code != http.StatusOK || rec.Header().Get(sessionTurnHeader) != "3" || !strings.Contains(rec.Body.String(), `"session"`) {
		t.Fatalf("get session: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if first.JobName == r.JobName || h.runToken() == firstToken {
		t.Error("second attempt reuses the first one's job or token")
	}

	h.post(result(review.VerdictApprove))
	h.reconcile()
	if got := h.state(); got.State != run.StateDone || len(h.gh.posted) != 1 || len(h.gh.checkRuns) != 1 {
		t.Errorf("state %s, %d reviews, %d check runs; want done with one of each", got.State, len(h.gh.posted), len(h.gh.checkRuns))
	}
}

func TestResultBeforeReconcileRecoversUnrecordedCheck(t *testing.T) {
	h := newHarness()
	h.reconcile()
	delete(h.store.runs[runID].Actions, store.ActionCheckRunCreate)
	h.restart()
	h.post(result(review.VerdictApprove))
	h.reconcile()
	h.reconcile()
	updates := h.gh.checkRuns[100]
	if h.state().State != run.StateDone || len(h.gh.checkRuns) != 1 || len(h.gh.posted) != 1 {
		t.Fatalf("state %s, checks %d, reviews %d", h.state().State, len(h.gh.checkRuns), len(h.gh.posted))
	}
	if updates[len(updates)-1].Status != "completed" || h.state().Actions[store.ActionCheckRunComplete] != "100" {
		t.Fatalf("check was not completed: %+v", updates)
	}
}

func TestRetriesStopAtMaxAttempts(t *testing.T) {
	h := newHarness()
	h.reconcile()
	for range 3 {
		h.losePod()
		h.reconcile()
		h.reconcile()
	}
	r := h.state()
	if r.State != run.StateFailed || r.Attempt != 3 || !strings.Contains(r.Error, "gave up after 3 attempts") {
		t.Fatalf("run %+v", r)
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "failure" {
		t.Errorf("check run %+v, want failure", cr[len(cr)-1])
	}
}

func TestFinalFailuresAreNotRetried(t *testing.T) {
	tests := []struct {
		name string
		fail func(h *harness)
		want string
	}{
		{"final exit code", func(h *harness) {
			h.kube.pods = []kube.Pod{{Status: kube.PodStatus{ContainerStatuses: []kube.ContainerStatus{
				{Name: "runner", State: kube.ContainerState{Terminated: &kube.ContainerStateTerminated{ExitCode: run.ExitFinal, Message: "controller rejected the result"}}},
			}}}}
			h.kube.finish(h.state().JobName, kube.JobFailed)
		}, "runner exited with code 3"},
		{"deadline", func(h *harness) {
			h.kube.finishWith(h.state().JobName, kube.JobFailed, "DeadlineExceeded")
		}, "did not finish within its 1800-second deadline"},
	}
	for _, tt := range tests {
		h := newHarness()
		h.reconcile()
		tt.fail(h)
		h.reconcile()
		if r := h.state(); r.State != run.StateFailed || r.Attempt != 1 || !strings.Contains(r.Error, tt.want) {
			t.Errorf("%s: run %+v", tt.name, r)
		}
	}
}

// brokenKube cannot create Jobs, like a cluster with a bad quota or webhook.
type brokenKube struct{ *fakeKube }

func (brokenKube) CreateJob(context.Context, kube.Job) (kube.Job, error) {
	return kube.Job{}, errBroken
}

var errBroken = &githubapp.APIError{Status: http.StatusInternalServerError, Message: "admission webhook unavailable"}

func TestStuckAttemptTimesOut(t *testing.T) {
	h := newHarness()
	h.c.kube = brokenKube{h.kube}
	h.reconcile()
	if r := h.state(); r.State != run.StateRunning {
		t.Fatalf("transient error failed the run: %+v", r)
	}
	h.clock = h.clock.Add(h.c.attemptDeadline() + time.Second)
	h.reconcile()
	r := h.state()
	if r.State != run.StateFailed || !strings.Contains(r.Error, "attempt 1 timed out") {
		t.Fatalf("run %+v", r)
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "failure" {
		t.Errorf("check run %+v, want failure", cr[len(cr)-1])
	}
}

func TestRestartAfterVoteBeforeRecordDoesNotVoteTwice(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.post(result(review.VerdictApprove, review.Comment{Path: "a.go", Line: 3, Severity: review.SeverityHigh, Confidence: review.ConfidenceHigh, Body: "bug"}))
	h.store.crashOn = store.ActionReview
	h.reconcile()
	if len(h.gh.posted) != 1 || h.state().Done(store.ActionReview) {
		t.Fatalf("setup: %d reviews posted, recorded %v", len(h.gh.posted), h.state().Actions)
	}

	h.restart()
	h.reconcile()
	r := h.state()
	if r.State != run.StateDone || len(h.gh.posted) != 1 {
		t.Fatalf("state %s with %d reviews, want done with the one review", r.State, len(h.gh.posted))
	}
	if r.Actions[store.ActionReview] != "901" {
		t.Errorf("recorded review %q, want the existing 901", r.Actions[store.ActionReview])
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "success" {
		t.Errorf("check run %+v, want success from the recovered APPROVE", cr[len(cr)-1])
	}
}

func TestRestartAfterCheckRunBeforeRecordReusesIt(t *testing.T) {
	h := newHarness()
	h.store.crashOn = store.ActionCheckRunCreate
	h.reconcile()
	if len(h.gh.checkRuns) != 1 || len(h.kube.jobs) != 0 {
		t.Fatalf("setup: %d check runs, %d jobs", len(h.gh.checkRuns), len(h.kube.jobs))
	}
	h.restart()
	h.reconcile()
	if r := h.state(); len(h.gh.checkRuns) != 1 || r.Actions[store.ActionCheckRunCreate] != "100" || len(h.kube.jobs) != 1 {
		t.Errorf("%d check runs, recorded %v, %d jobs; want the first check run reused", len(h.gh.checkRuns), r.Actions, len(h.kube.jobs))
	}
}

func TestNewCommitSupersedesRun(t *testing.T) {
	h := newHarness()
	h.reconcile()
	oldJob := h.state().JobName
	oldToken := h.runToken()

	const newID = "0b4c1f6e-0000-4000-8000-000000000002"
	const queuedID = "0b4c1f6e-0000-4000-8000-000000000003"
	h.store.runs[newID] = &store.Run{ID: newID, Repo: "o/r", PRNumber: 7, HeadSHA: "3333333333333333333333333333333333333333",
		InstallationID: 42, State: run.StateQueued, CreatedAt: time.Unix(2, 0), Actions: map[store.ActionKind]string{}}
	// A run of a third commit never got to start.
	h.store.runs[queuedID] = &store.Run{ID: queuedID, Repo: "o/r", PRNumber: 7, HeadSHA: "4444444444444444444444444444444444444444",
		InstallationID: 42, State: run.StateQueued, CreatedAt: time.Unix(1, 0), Actions: map[store.ActionKind]string{}}
	h.reconcile()

	if r := h.state(); r.State != run.StateSuperseded || !r.Done(store.ActionCheckRunComplete) {
		t.Fatalf("old run %+v", r)
	}
	if _, ok := h.kube.jobs[oldJob]; ok {
		t.Error("superseded run's job still exists")
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "cancelled" {
		t.Errorf("old check run %+v, want cancelled", cr[len(cr)-1])
	}
	if q, _ := h.store.Get(context.Background(), queuedID); q.State != run.StateSuperseded || q.Attempt != 0 {
		t.Errorf("queued run %+v, want superseded without starting", q)
	}
	if n, _ := h.store.Get(context.Background(), newID); n.State != run.StateRunning {
		t.Errorf("newest run %+v, want running", n)
	}
	if code := h.postWithToken(result(review.VerdictApprove), oldToken); code == http.StatusNoContent {
		t.Error("superseded run accepted a result")
	}
	if len(h.gh.posted) != 0 {
		t.Error("superseded run voted")
	}
}

func TestSessionAndUsageAPI(t *testing.T) {
	h := newHarness()
	h.reconcile()
	token := h.runToken()

	if code := h.call(http.MethodGet, "/runs/"+runID+"/session", token, nil).Code; code != http.StatusNotFound {
		t.Errorf("get without snapshot: %d, want 404", code)
	}
	if code := h.putSession("wrong", 1, "x"); code != http.StatusUnauthorized {
		t.Errorf("put with wrong token: %d, want 401", code)
	}
	if code := h.call(http.MethodPut, "/runs/"+runID+"/session?turn=x", token, nil).Code; code != http.StatusBadRequest {
		t.Errorf("put without turn: %d, want 400", code)
	}
	h.putSession(token, 5, "five")
	h.putSession(token, 4, "four") // a delayed upload must not roll back
	if got := h.call(http.MethodGet, "/runs/"+runID+"/session", token, nil).Body.String(); got != "five" {
		t.Errorf("session %q, want the newest turn", got)
	}

	u := store.Usage{Turn: 2, Model: "deepseek/deepseek-flash", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 900, CostUSD: 0.001}
	body, _ := json.Marshal(u)
	if code := h.call(http.MethodPut, "/runs/"+runID+"/usage", token, body).Code; code != http.StatusNoContent {
		t.Errorf("put usage: %d", code)
	}
	if got := h.store.usage[runID][2]; got != u {
		t.Errorf("usage %+v, want %+v", got, u)
	}
	for _, bad := range []string{`{"turn":1}`, `{"turn":1,"model":"m","input_tokens":-1}`, `{"turn":1,"model":"m","extra":1}`} {
		if code := h.call(http.MethodPut, "/runs/"+runID+"/usage", token, []byte(bad)).Code; code != http.StatusBadRequest {
			t.Errorf("usage %s: %d, want 400", bad, code)
		}
	}

	h.post(result(review.VerdictApprove))
	if code := h.putSession(token, 6, "six"); code != http.StatusConflict {
		t.Errorf("snapshot after the result: %d, want 409", code)
	}
}

func TestRedeliverFailedWebhooks(t *testing.T) {
	h := newHarness()
	h.gh.deliveries = []githubapp.Delivery{
		{ID: 1, GUID: "a", StatusCode: 503, DeliveredAt: h.clock.Add(-time.Minute)},
		{ID: 2, GUID: "b", StatusCode: 202, DeliveredAt: h.clock.Add(-time.Minute)},
	}
	h.c.Redeliver(context.Background())
	if len(h.gh.redelivered) != 1 || h.gh.redelivered[0] != 1 {
		t.Errorf("redelivered %v, want [1]", h.gh.redelivered)
	}
}
