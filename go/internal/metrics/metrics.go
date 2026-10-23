// Package metrics renders the controller's Prometheus metrics
// (docs/architecture.md §7.1) from a store snapshot. Every value is derived
// from the database at scrape time, so counters survive controller restarts
// and agree across replicas. The text exposition format is written by hand to
// keep the controller free of the Prometheus client's dependencies.
package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// Histogram buckets: phase latencies span seconds (publish) to tens of
// minutes (a large PR's review); per-run cost spans a tenth of a cent to dollars.
var (
	DurationBuckets = []float64{1, 2, 5, 10, 20, 30, 60, 90, 120, 180, 300, 600, 1200, 1800}
	CostBuckets     = []float64{0.005, 0.01, 0.02, 0.03, 0.05, 0.075, 0.1, 0.2, 0.5, 1}
)

// Failure reasons, the label values of review_failures_total.
const (
	ReasonTimeout  = "timeout"   // the Job hit activeDeadlineSeconds
	ReasonFetch    = "fetch"     // the fetch init container failed
	ReasonNoReview = "no_review" // the model never called submit_review
	ReasonBudget   = "budget"    // the model proxy refused the run's key (M2)
	ReasonRunner   = "runner"    // the runner container failed otherwise
	ReasonGitHub   = "github"    // GitHub refused a write
	ReasonOther    = "other"
)

// FailureReason classifies a failed run's error message, as written by the
// controller (PodFailure, jobFailure, once), into a small set of labels.
func FailureReason(msg string) string {
	switch {
	case strings.Contains(msg, "DeadlineExceeded"):
		return ReasonTimeout
	case strings.Contains(msg, "submit_review"), strings.Contains(msg, "without submitting"):
		return ReasonNoReview
	case strings.Contains(strings.ToLower(msg), "budget"):
		return ReasonBudget
	case strings.HasPrefix(msg, "fetch "):
		return ReasonFetch
	case strings.HasPrefix(msg, "runner "):
		return ReasonRunner
	case strings.HasPrefix(msg, "mint fetch token: "):
		return ReasonGitHub
	}
	for _, kind := range store.ActionKinds {
		if strings.HasPrefix(msg, string(kind)+": ") {
			return ReasonGitHub
		}
	}
	return ReasonOther
}

// Write renders the snapshot in the Prometheus text exposition format.
func Write(w io.Writer, st store.Stats) error {
	var b strings.Builder

	family(&b, "review_runs_total", "counter", "Runs that ended, by final state.")
	for _, s := range run.States {
		if s.Terminal() {
			sample(&b, "review_runs_total", labels("state", string(s)), float64(st.Runs[s]))
		}
	}
	family(&b, "review_queue_depth", "gauge", "Runs waiting for a runner slot.")
	sample(&b, "review_queue_depth", "", float64(st.Runs[run.StateQueued]))
	family(&b, "review_running", "gauge", "Runs with a live runner Job.")
	sample(&b, "review_running", "", float64(st.Runs[run.StateRunning]+st.Runs[run.StatePreviewing]))

	family(&b, "review_verdicts_total", "counter", "Done runs by the model's verdict.")
	for _, v := range []review.Verdict{review.VerdictApprove, review.VerdictRequestChanges} {
		sample(&b, "review_verdicts_total", labels("verdict", string(v)), float64(st.Verdicts[v]))
	}
	family(&b, "review_retries_total", "counter", "Runner attempts beyond the first.")
	sample(&b, "review_retries_total", "", float64(st.Retries))

	failures := map[string]int{}
	for msg, n := range st.Failures {
		failures[FailureReason(msg)] += n
	}
	family(&b, "review_failures_total", "counter", "Failed runs by reason.")
	for _, reason := range []string{ReasonTimeout, ReasonFetch, ReasonNoReview, ReasonBudget, ReasonRunner, ReasonGitHub, ReasonOther} {
		sample(&b, "review_failures_total", labels("reason", reason), float64(failures[reason]))
	}

	family(&b, "review_run_duration_seconds", "histogram", "Latency of done runs by phase: queue, startup, fetch, review, publish and total.")
	for _, phase := range store.Phases {
		var values []float64
		for _, d := range st.Done {
			if v, ok := d.Seconds[phase]; ok {
				values = append(values, v)
			}
		}
		histogram(&b, "review_run_duration_seconds", "phase", phase, DurationBuckets, values)
	}
	family(&b, "review_run_cost_usd", "histogram", "Model cost of each done run.")
	costs := make([]float64, len(st.Done))
	for i, d := range st.Done {
		costs[i] = d.CostUSD
	}
	histogram(&b, "review_run_cost_usd", "", "", CostBuckets, costs)

	family(&b, "review_tokens_total", "counter", "Model tokens reported by runners, by model and direction.")
	for _, u := range st.Usage {
		for _, d := range []struct {
			direction string
			n         int64
		}{{"input", u.InputTokens}, {"output", u.OutputTokens}, {"cache_read", u.CacheReadTokens}, {"cache_write", u.CacheWriteTokens}} {
			sample(&b, "review_tokens_total", labels("model", u.Model, "direction", d.direction), float64(d.n))
		}
	}
	family(&b, "review_model_cost_usd_total", "counter", "Model cost reported by runners, by model.")
	for _, u := range st.Usage {
		sample(&b, "review_model_cost_usd_total", labels("model", u.Model), u.CostUSD)
	}
	family(&b, "review_cost_usd_total", "counter", "Model cost reported by runners, by repository.")
	for _, repo := range slices.Sorted(maps.Keys(st.CostByRepo)) {
		sample(&b, "review_cost_usd_total", labels("repo", repo), st.CostByRepo[repo])
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func family(b *strings.Builder, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func sample(b *strings.Builder, name, labels string, v float64) {
	fmt.Fprintf(b, "%s%s %s\n", name, labels, strconv.FormatFloat(v, 'g', -1, 64))
}

// histogram writes cumulative buckets, _sum and _count, with an optional
// label shared by every series.
func histogram(b *strings.Builder, name, label, value string, buckets, values []float64) {
	var sum float64
	counts := make([]int, len(buckets))
	for _, v := range values {
		sum += v
		for i, le := range buckets {
			if v <= le {
				counts[i]++
			}
		}
	}
	pair := func(extra ...string) string {
		if label == "" {
			return labels(extra...)
		}
		return labels(append([]string{label, value}, extra...)...)
	}
	for i, le := range buckets {
		sample(b, name+"_bucket", pair("le", strconv.FormatFloat(le, 'g', -1, 64)), float64(counts[i]))
	}
	sample(b, name+"_bucket", pair("le", "+Inf"), float64(len(values)))
	sample(b, name+"_sum", pair(), sum)
	sample(b, name+"_count", pair(), float64(len(values)))
}

// labels renders name/value pairs as {a="1",b="2"}, escaping values.
func labels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, kv[i], escape.Replace(kv[i+1])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Handler serves /metrics from a fresh snapshot on every scrape.
func Handler(stats func(context.Context) (store.Stats, error), log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
		defer cancel()
		st, err := stats(ctx)
		if err != nil {
			log.Error("read metrics", "err", err)
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := Write(w, st); err != nil {
			log.Warn("write metrics", "err", err)
		}
	})
}
