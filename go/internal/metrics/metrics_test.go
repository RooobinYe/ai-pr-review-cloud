package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func TestFailureReason(t *testing.T) {
	for _, tc := range []struct{ msg, want string }{
		{"runner job failed: DeadlineExceeded Job was active longer than specified deadline", ReasonTimeout},
		{"fetch exited with code 1: PR head moved to abc", ReasonFetch},
		{"fetch is stuck: ImagePullBackOff back-off pulling image", ReasonFetch},
		{"runner exited with code 1: runner: model did not call submit_review after 2 reminders", ReasonNoReview},
		{"runner exited without submitting a review", ReasonNoReview},
		{"runner exited with code 1: 400 Budget has been exceeded", ReasonBudget},
		{"runner exited with code 137: OOMKilled", ReasonRunner},
		{"review: POST /repos/o/r/pulls/1/reviews: 422 Unprocessable Entity", ReasonGitHub},
		{"check_run_create: 403 Resource not accessible by integration", ReasonGitHub},
		{"mint fetch token: 404 Not Found", ReasonGitHub},
		{"", ReasonOther},
	} {
		if got := FailureReason(tc.msg); got != tc.want {
			t.Errorf("FailureReason(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

func snapshot() store.Stats {
	return store.Stats{
		Runs:     map[run.State]int{run.StateDone: 3, run.StateFailed: 2, run.StateQueued: 4, run.StateRunning: 1, run.StatePreviewing: 1},
		Verdicts: map[review.Verdict]int{review.VerdictApprove: 2, review.VerdictRequestChanges: 1},
		Retries:  1,
		Failures: map[string]int{"runner job failed: DeadlineExceeded": 1, "fetch exited with code 1: x": 1},
		Done: []store.DoneRun{
			{Seconds: map[string]float64{store.PhaseQueue: 0.5, store.PhaseReview: 45, store.PhaseTotal: 60}, CostUSD: 0.012},
			{Seconds: map[string]float64{store.PhaseQueue: 3, store.PhaseReview: 200, store.PhaseTotal: 230}, CostUSD: 0.08},
			{Seconds: map[string]float64{store.PhaseQueue: 1, store.PhaseTotal: 75}, CostUSD: 0.03},
		},
		Usage: []store.ModelUsage{{Model: "deepseek/deepseek-flash", InputTokens: 900, OutputTokens: 300, CacheReadTokens: 12000, CostUSD: 0.122}},
		CostByRepo: map[string]float64{
			"o/b":        0.1,
			`o/"quoted"`: 0.022,
		},
	}
}

func TestWrite(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, snapshot()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE review_runs_total counter\n",
		`review_runs_total{state="done"} 3`,
		`review_runs_total{state="superseded"} 0`,
		"review_queue_depth 4\n",
		"review_running 2\n",
		`review_verdicts_total{verdict="request_changes"} 1`,
		"review_retries_total 1\n",
		`review_failures_total{reason="timeout"} 1`,
		`review_failures_total{reason="fetch"} 1`,
		`review_failures_total{reason="github"} 0`,
		"# TYPE review_run_duration_seconds histogram\n",
		`review_run_duration_seconds_bucket{phase="review",le="60"} 1`,
		`review_run_duration_seconds_bucket{phase="review",le="300"} 2`,
		`review_run_duration_seconds_bucket{phase="review",le="+Inf"} 2`,
		`review_run_duration_seconds_count{phase="review"} 2`,
		`review_run_duration_seconds_sum{phase="review"} 245`,
		`review_run_duration_seconds_count{phase="total"} 3`,
		`review_run_duration_seconds_count{phase="fetch"} 0`,
		`review_run_cost_usd_bucket{le="0.02"} 1`,
		`review_run_cost_usd_bucket{le="0.03"} 2`,
		`review_run_cost_usd_bucket{le="+Inf"} 3`,
		"review_run_cost_usd_count 3\n",
		`review_tokens_total{model="deepseek/deepseek-flash",direction="cache_read"} 12000`,
		`review_model_cost_usd_total{model="deepseek/deepseek-flash"} 0.122`,
		`review_cost_usd_total{repo="o/\"quoted\""} 0.022`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if i, j := strings.Index(out, `repo="o/\"quoted\""`), strings.Index(out, `repo="o/b"`); i > j {
		t.Error("repositories are not sorted")
	}
}

func TestHandler(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ok := Handler(func(context.Context) (store.Stats, error) { return snapshot(), nil }, log)
	rec := httptest.NewRecorder()
	ok.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Errorf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	down := Handler(func(context.Context) (store.Stats, error) { return store.Stats{}, errors.New("db down") }, log)
	rec = httptest.NewRecorder()
	down.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d with the database down, want 503", rec.Code)
	}
}
