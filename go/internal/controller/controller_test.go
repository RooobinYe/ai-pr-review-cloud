package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

const (
	runID   = "0b4c1f6e-0000-4000-8000-000000000001"
	headSHA = "2222222222222222222222222222222222222222"
	oldSHA  = "1111111111111111111111111111111111111111"
	bot     = "ai-pr-review-dev[bot]"
)

type harness struct {
	c     *Controller
	store *fakeStore
	gh    *fakeGitHub
	kube  *fakeKube
	keys  *fakeKeys
	mux   *http.ServeMux
	clock time.Time
}

func newHarness() *harness {
	s := newFakeStore(store.Run{ID: runID, Repo: "o/r", PRNumber: 7, HeadSHA: headSHA, InstallationID: 42, State: run.StateQueued, BudgetUSD: 0.5})
	gh := &fakeGitHub{head: headSHA}
	k := newFakeKube()
	h := &harness{store: s, gh: gh, kube: k, keys: &fakeKeys{}, clock: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	s.now = h.now
	h.restart()
	return h
}

func (h *harness) now() time.Time { return h.clock }

// restart replaces the controller with a fresh one over the same store,
// GitHub and cluster, as a controller Pod restart would.
func (h *harness) restart() {
	cfg := Config{MaxRunning: 2, MaxAttempts: 3, BotLogin: bot, EgressHosts: []string{"github.com:443"}, Job: JobConfig{
		FetchImage: "fetch:dev", RunnerImage: "runner:dev", ControllerURL: "http://ctl", LLMEnv: map[string]string{"LLM_MODEL": "deepseek-flash"}, ActiveDeadlineSeconds: 1800,
	}}
	h.c = New(cfg, h.store, h.gh, h.kube, h.keys, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.c.now = h.now
	h.mux = http.NewServeMux()
	h.c.Routes(h.mux)
}

func (h *harness) reconcile() { h.c.Reconcile(context.Background()) }

func (h *harness) state() store.Run {
	r, _ := h.store.Get(context.Background(), runID)
	return r
}

// runToken is the callback token the controller gave the run's runner.
func (h *harness) runToken() string {
	return string(h.kube.secrets[runSecretName(h.state().JobName)].Data["RUN_TOKEN"])
}

func (h *harness) post(res review.Result) int { return h.postWithToken(res, h.runToken()) }

func (h *harness) postWithToken(res review.Result, token string) int {
	body, _ := json.Marshal(res)
	return h.call(http.MethodPost, "/runs/"+runID+"/result", token, body).Code
}

// call sends a runner callback with the given bearer token.
func (h *harness) call(method, path, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func result(v review.Verdict, comments ...review.Comment) review.Result {
	if comments == nil {
		comments = []review.Comment{}
	}
	return review.Result{
		SchemaVersion: review.SchemaVersion,
		PR:            review.PR{Repo: "o/r", Number: 7, HeadSHA: headSHA},
		Model:         "deepseek/deepseek-flash",
		GeneratedAt:   "2026-10-23T00:00:00Z",
		Verdict:       v,
		Body:          "Looks fine.",
		Comments:      comments,
	}
}

func approvedReview(id int64, login, sha string) githubapp.Review {
	r := githubapp.Review{ID: id, State: "APPROVED", CommitID: sha}
	r.User.Login = login
	return r
}

func TestApproveFlow(t *testing.T) {
	h := newHarness()
	h.gh.reviews = []githubapp.Review{approvedReview(1, bot, oldSHA), approvedReview(2, "human", oldSHA), approvedReview(3, bot, headSHA)}

	h.reconcile()
	r := h.state()
	if r.State != run.StateRunning || r.JobName != JobName(runID, 1) {
		t.Fatalf("after claim: %+v", r)
	}
	if len(h.gh.dismissed) != 1 || h.gh.dismissed[0] != 1 {
		t.Errorf("dismissed %v, want only the bot's stale approval", h.gh.dismissed)
	}
	if cr := h.gh.checkRuns[100]; len(cr) != 1 || cr[0].Status != "in_progress" || cr[0].HeadSHA != headSHA {
		t.Errorf("check run %+v", cr)
	}
	job, ok := h.kube.jobs[r.JobName]
	if !ok {
		t.Fatal("job not created")
	}
	secret := h.kube.secrets[runSecretName(r.JobName)]
	if secret.StringData["GITHUB_TOKEN"] != "ro-42-o/r-0" || len(secret.StringData["RUN_TOKEN"]) != 64 || len(secret.StringData["PROXY_TOKEN"]) != 64 ||
		secret.StringData["RUN_TOKEN"] == secret.StringData["PROXY_TOKEN"] || secret.Metadata.OwnerReferences[0].UID != job.Metadata.UID {
		t.Errorf("run secret %+v", secret)
	}
	key, ok := h.keys.live[secret.StringData["LLM_API_KEY"]]
	if !ok || key.MaxBudgetUSD != 0.5 || key.Alias != r.JobName || key.Duration != 35*time.Minute || len(key.Models) != 1 || key.Models[0] != "deepseek-flash" {
		t.Errorf("virtual key %+v (live %v)", key, ok)
	}

	h.reconcile() // nothing new while the job runs
	if len(h.gh.tokens) != 1 || len(h.gh.checkRuns) != 1 || len(h.gh.dismissed) != 1 {
		t.Errorf("repeated side effects: tokens %d, check runs %d, dismissals %d", len(h.gh.tokens), len(h.gh.checkRuns), len(h.gh.dismissed))
	}

	if code := h.post(result(review.VerdictApprove, review.Comment{Path: "a.go", Line: 3, Severity: review.SeverityLow, Body: "nit"})); code != http.StatusNoContent {
		t.Fatalf("post result: %d", code)
	}
	h.kube.finish(r.JobName, kube.JobComplete)
	h.reconcile()

	if got := h.state(); got.State != run.StateDone {
		t.Fatalf("state %s, want done", got.State)
	}
	if len(h.keys.live) != 0 {
		t.Errorf("virtual keys still live after the vote: %v", h.keys.live)
	}
	if len(h.gh.posted) != 1 {
		t.Fatalf("posted %d reviews", len(h.gh.posted))
	}
	rv := h.gh.posted[0]
	if rv.Event != "APPROVE" || rv.CommitID != headSHA || len(rv.Comments) != 1 || rv.Comments[0].Side != "RIGHT" || !strings.Contains(rv.Comments[0].Body, "**low**") {
		t.Errorf("review %+v", rv)
	}
	if cr := h.gh.checkRuns[100]; len(cr) != 2 || cr[1].Conclusion != "success" {
		t.Errorf("check run updates %+v", cr)
	}

	h.reconcile()
	if code := h.post(result(review.VerdictApprove)); code != http.StatusNoContent {
		t.Errorf("duplicate post: %d, want 204", code)
	}
	if len(h.gh.posted) != 1 || len(h.gh.checkRuns[100]) != 2 {
		t.Error("finished run produced more GitHub writes")
	}
}

func TestRequestChangesVotesComment(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.post(result(review.VerdictRequestChanges))
	h.reconcile()
	if rv := h.gh.posted[0]; rv.Event != "COMMENT" {
		t.Errorf("event %s, want COMMENT", rv.Event)
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "neutral" {
		t.Errorf("conclusion %s, want neutral", cr[len(cr)-1].Conclusion)
	}
}

func TestApproveAfterHeadMovedVotesComment(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.gh.head = "3333333333333333333333333333333333333333"
	h.post(result(review.VerdictApprove))
	h.reconcile()
	if rv := h.gh.posted[0]; rv.Event != "COMMENT" {
		t.Errorf("event %s, want COMMENT for a stale head", rv.Event)
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "neutral" {
		t.Errorf("check run %+v, want neutral when the approval was not cast", cr[len(cr)-1])
	}
}

func TestRejectedInlineCommentsAreFolded(t *testing.T) {
	h := newHarness()
	h.gh.rejectLines = true
	h.reconcile()
	h.post(result(review.VerdictRequestChanges, review.Comment{Path: "a.go", Line: 99, Severity: review.SeverityHigh, Body: "bug"}))
	h.reconcile()
	if len(h.gh.posted) != 1 {
		t.Fatalf("posted %d reviews", len(h.gh.posted))
	}
	if rv := h.gh.posted[0]; len(rv.Comments) != 0 || !strings.Contains(rv.Body, "`a.go:99`") {
		t.Errorf("folded review %+v", rv)
	}
	if h.state().State != run.StateDone {
		t.Error("run not done")
	}
}

func TestJobFailureIsReported(t *testing.T) {
	h := newHarness()
	h.reconcile()
	job := h.state().JobName
	h.kube.pods = []kube.Pod{{Status: kube.PodStatus{InitContainerStatuses: []kube.ContainerStatus{
		{Name: "fetch", State: kube.ContainerState{Terminated: &kube.ContainerStateTerminated{ExitCode: run.ExitFinal, Message: "fetch: 404 Not Found"}}},
	}}}}
	h.kube.finish(job, kube.JobFailed)
	h.reconcile()

	r := h.state()
	if r.State != run.StateFailed || !strings.Contains(r.Error, "fetch exited with code 3: fetch: 404 Not Found") {
		t.Fatalf("run %+v", r)
	}
	if len(h.keys.live) != 0 {
		t.Errorf("virtual keys still live after the failure: %v", h.keys.live)
	}
	cr := h.gh.checkRuns[100]
	if last := cr[len(cr)-1]; last.Conclusion != "failure" || !strings.Contains(last.Output.Summary, "404") {
		t.Errorf("check run %+v", last)
	}
	h.reconcile()
	if len(h.gh.checkRuns[100]) != len(cr) {
		t.Error("failure reported twice")
	}
	if code := h.post(result(review.VerdictApprove)); code != http.StatusConflict {
		t.Errorf("late result: %d, want 409", code)
	}
}

func TestBudgetExceeded(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.kube.pods = []kube.Pod{{Status: kube.PodStatus{ContainerStatuses: []kube.ContainerStatus{
		{Name: "runner", State: kube.ContainerState{Terminated: &kube.ContainerStateTerminated{ExitCode: 1, Message: "budget_exceeded: the run spent its model budget"}}},
	}}}}
	h.kube.finish(h.state().JobName, kube.JobFailed)
	h.reconcile()
	if r := h.state(); r.State != run.StateFailed || r.Error != "budget_exceeded: the run spent its model budget" {
		t.Errorf("run %+v", r)
	}
}

func TestSucceededJobWithoutResultFails(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.kube.finish(h.state().JobName, kube.JobComplete)
	h.reconcile()
	if r := h.state(); r.State != run.StateFailed || !strings.Contains(r.Error, "without submitting") {
		t.Errorf("run %+v", r)
	}
}

func TestMissingSecretIsRecreated(t *testing.T) {
	h := newHarness()
	h.reconcile()
	name := runSecretName(h.state().JobName)
	firstKey := string(h.kube.secrets[name].Data["LLM_API_KEY"])
	clear(h.kube.secrets)
	h.reconcile()
	if len(h.kube.secrets) != 1 || len(h.gh.tokens) != 2 {
		t.Errorf("secrets %d, tokens %d", len(h.kube.secrets), len(h.gh.tokens))
	}
	if key := string(h.kube.secrets[name].Data["LLM_API_KEY"]); key == "" || key == firstKey {
		t.Error("recreated Secret must contain a new model key")
	}
}

func TestResultValidation(t *testing.T) {
	h := newHarness()
	h.reconcile()
	other := result(review.VerdictApprove)
	other.PR.HeadSHA = oldSHA
	if code := h.post(other); code != http.StatusUnprocessableEntity {
		t.Errorf("result for another commit: %d, want 422", code)
	}
	bad := result(review.VerdictApprove)
	bad.Body = ""
	if code := h.post(bad); code != http.StatusBadRequest {
		t.Errorf("invalid result: %d, want 400", code)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/nope/result", strings.NewReader(mustJSON(result(review.VerdictApprove)))))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown run: %d, want 404", rec.Code)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestMaxRunning(t *testing.T) {
	h := newHarness()
	h.c.cfg.MaxRunning = 0
	h.reconcile()
	if h.state().State != run.StateQueued || len(h.kube.jobs) != 0 {
		t.Error("run claimed beyond MaxRunning")
	}
}

func TestPermanentGitHubErrorFailsRun(t *testing.T) {
	h := newHarness()
	h.c.gh = failingGitHub{h.gh, &githubapp.APIError{Status: http.StatusNotFound, Message: "Not Found"}}
	h.reconcile()
	if r := h.state(); r.State != run.StateFailed || !strings.Contains(r.Error, "404") {
		t.Errorf("run %+v, want failed with the GitHub error", r)
	}
	if len(h.kube.jobs) != 0 {
		t.Error("job created for a run that cannot be reviewed")
	}
}

func TestPermanent(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{&githubapp.APIError{Status: 404}, true},
		{&githubapp.APIError{Status: 403, Message: "Resource not accessible by integration"}, true},
		{&githubapp.APIError{Status: 403, Message: "You have exceeded a secondary rate limit"}, false},
		{&githubapp.APIError{Status: 502}, false},
		{io.ErrUnexpectedEOF, false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := permanent(tt.err); got != tt.want {
			t.Errorf("permanent(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// failingGitHub fails every read of reviews, the first GitHub call of a run.
type failingGitHub struct {
	*fakeGitHub
	err error
}

func (f failingGitHub) Client(int64) GitHubClient { return f }

func (f failingGitHub) ListReviews(context.Context, string, int) ([]githubapp.Review, error) {
	return nil, f.err
}

func TestResultNeedsRunToken(t *testing.T) {
	h := newHarness()
	h.reconcile()
	for _, token := range []string{"", "wrong"} {
		if code := h.postWithToken(result(review.VerdictApprove), token); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", token, code)
		}
	}
	if h.state().State != run.StateRunning {
		t.Error("unauthenticated result accepted")
	}
}

func TestRefusedVoteFailsRun(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.post(result(review.VerdictApprove))
	h.gh.refuse(&githubapp.APIError{Status: http.StatusForbidden, Message: "Resource not accessible by integration"})
	h.reconcile()
	if r := h.state(); r.State != run.StateFailed || !strings.Contains(r.Error, "403") {
		t.Fatalf("run %+v, want failed", r)
	}
	if cr := h.gh.checkRuns[100]; cr[len(cr)-1].Conclusion != "failure" {
		t.Errorf("check run %+v, want failure", cr[len(cr)-1])
	}
}

func TestFailureBeforeCheckRunIsStillReported(t *testing.T) {
	h := newHarness()
	h.c.gh = failingGitHub{h.gh, &githubapp.APIError{Status: http.StatusForbidden, Message: "dismissal not allowed"}}
	h.reconcile()
	r := h.state()
	if r.State != run.StateFailed {
		t.Fatalf("run %+v, want failed", r)
	}
	cr := h.gh.checkRuns[100]
	if len(cr) != 1 || cr[0].Name != CheckName || cr[0].HeadSHA != headSHA || cr[0].Conclusion != "failure" || !strings.Contains(cr[0].Output.Summary, "dismissal not allowed") {
		t.Errorf("check run %+v", cr)
	}
	if !r.Done(store.ActionCheckRunComplete) {
		t.Error("failure report not recorded")
	}
}

func TestFailedRunWithoutCheckRunIsReportedOnRecovery(t *testing.T) {
	h := newHarness()
	// The controller stopped between marking the run failed and reporting it.
	h.store.runs[runID].State, h.store.runs[runID].Error = run.StateFailed, "boom"
	h.reconcile()
	cr := h.gh.checkRuns[100]
	if len(cr) != 1 || cr[0].Conclusion != "failure" || cr[0].Output.Summary != "boom" {
		t.Errorf("check runs %+v", h.gh.checkRuns)
	}
	h.reconcile()
	if len(h.gh.checkRuns) != 1 {
		t.Error("failure reported twice")
	}
}
