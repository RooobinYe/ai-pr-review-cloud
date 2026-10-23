package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
)

func TestUsageAndStats(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	done, _, _ := s.RecordTrigger(ctx, "d1", newRun("a"))
	failed, _, _ := s.RecordTrigger(ctx, "d2", newRun("b"))
	queued, _, _ := s.RecordTrigger(ctx, "d3", newRun("c"))
	if _, err := s.Claim(ctx, 2, jobName); err != nil {
		t.Fatal(err)
	}

	turn1 := Usage{Turn: 1, Model: "deepseek/deepseek-flash", InputTokens: 100, OutputTokens: 10, CacheReadTokens: 1000, CostUSD: 0.01}
	turn2 := Usage{Turn: 2, Model: "deepseek/deepseek-flash", InputTokens: 50, OutputTokens: 5, CacheReadTokens: 2000, CostUSD: 0.02}
	for _, u := range []Usage{turn1, turn1, turn2} { // turn 1 reported twice
		if err := s.RecordUsage(ctx, done, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordUsage(ctx, failed, Usage{Turn: 1, Model: "deepseek/deepseek-v4-pro", InputTokens: 7, CostUSD: 0.005}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, "00000000-0000-0000-0000-000000000000", turn1); !errors.Is(err, ErrNotFound) {
		t.Errorf("usage for an unknown run: err %v, want ErrNotFound", err)
	}

	now := time.Now()
	if err := s.RecordFetchTiming(ctx, done, now.Add(-2*time.Second), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(ctx, done, review.Result{SchemaVersion: 3, Verdict: review.VerdictApprove, Body: "ok", Comments: []review.Comment{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, done, run.StateReviewed, run.StateDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, failed, run.StateRunning, run.StateFailed, "runner exited with code 1: boom"); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Get(ctx, done); err != nil || !r.FetchTimed || r.BudgetUSD != 0.5 {
		t.Errorf("done run %+v, err %v; want fetch timed and budget 0.5", r, err)
	}

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Runs[run.StateDone] != 1 || st.Runs[run.StateFailed] != 1 || st.Runs[run.StateQueued] != 1 {
		t.Errorf("runs by state %v (queued run %s)", st.Runs, queued)
	}
	if st.Verdicts[review.VerdictApprove] != 1 || st.Failures["runner exited with code 1: boom"] != 1 || st.Retries != 0 {
		t.Errorf("verdicts %v, failures %v, retries %d", st.Verdicts, st.Failures, st.Retries)
	}
	if len(st.Done) != 1 {
		t.Fatalf("done runs %+v", st.Done)
	}
	d := st.Done[0]
	if d.CostUSD < 0.0299 || d.CostUSD > 0.0301 {
		t.Errorf("done run cost %v, want 0.03 (turn 1 counted once)", d.CostUSD)
	}
	// The fetch timestamps come from this test's clock, not the database's, so
	// startup and review may come out negative and be left out.
	for _, phase := range []string{PhaseQueue, PhaseFetch, PhasePublish, PhaseTotal} {
		if _, ok := d.Seconds[phase]; !ok {
			t.Errorf("done run has no %s duration: %v", phase, d.Seconds)
		}
	}
	if d.Seconds[PhaseFetch] != 1 {
		t.Errorf("fetch took %vs, want 1", d.Seconds[PhaseFetch])
	}
	if len(st.Usage) != 2 || st.Usage[0].Model != "deepseek/deepseek-flash" || st.Usage[0].InputTokens != 150 || st.Usage[0].CacheReadTokens != 3000 {
		t.Errorf("usage %+v", st.Usage)
	}
	if c := st.CostByRepo["o/r"]; c < 0.0349 || c > 0.0351 {
		t.Errorf("cost by repo %v", st.CostByRepo)
	}
}
