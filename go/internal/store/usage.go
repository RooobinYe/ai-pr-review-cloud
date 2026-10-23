package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
)

// Usage is one turn of model usage reported by the runner.
type Usage struct {
	Turn             int     `json:"turn"`
	Model            string  `json:"model"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

func (s *Store) RunCostUSD(ctx context.Context, runID string) (float64, error) {
	var cost float64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd), 0)::float8 FROM run_usage WHERE run_id = $1`, runID).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("read run cost: %w", err)
	}
	return cost, nil
}

// RecordUsage stores one turn of a run's usage. A repeated report of the same
// turn replaces the earlier one instead of adding to it.
func (s *Store) RecordUsage(ctx context.Context, runID string, u Usage) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_usage (run_id, turn, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (run_id, turn) DO UPDATE SET model = EXCLUDED.model, input_tokens = EXCLUDED.input_tokens,
			output_tokens = EXCLUDED.output_tokens, cache_read_tokens = EXCLUDED.cache_read_tokens,
			cache_write_tokens = EXCLUDED.cache_write_tokens, cost_usd = EXCLUDED.cost_usd`,
		runID, u.Turn, u.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("record usage for run %s: %w", runID, err)
	}
	return nil
}

// RecordFetchTiming stores when the run's fetch init container ran.
func (s *Store) RecordFetchTiming(ctx context.Context, runID string, started, finished time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE review_run SET fetch_started_at = $2, fetch_finished_at = $3 WHERE id = $1`,
		runID, started, finished)
	if err != nil {
		return fmt.Errorf("record fetch timing for run %s: %w", runID, err)
	}
	return nil
}

// Phases of a run's latency (docs/architecture.md §7.1), in lifecycle order.
const (
	PhaseQueue   = "queue"   // created to claimed by the controller
	PhaseStartup = "startup" // claimed to fetch starting: Job creation, scheduling, image pull
	PhaseFetch   = "fetch"   // the fetch init container cloning the PR
	PhaseReview  = "review"  // fetch done to result received: the model exploring and submitting
	PhasePublish = "publish" // result received to done: voting and completing the Check Run
	PhaseTotal   = "total"   // created to done
)

var Phases = []string{PhaseQueue, PhaseStartup, PhaseFetch, PhaseReview, PhasePublish, PhaseTotal}

// DoneRun is the latency and cost of one run that reached done.
type DoneRun struct {
	// Seconds maps each phase to its duration; phases without timestamps are absent.
	Seconds map[string]float64
	CostUSD float64
}

// ModelUsage totals the usage of one model.
type ModelUsage struct {
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

// Stats is a consistent snapshot that the metrics of §7.1 are computed from.
// Everything is derived from the tables, so it survives controller restarts
// and every replica reports the same values.
type Stats struct {
	Runs     map[run.State]int
	Verdicts map[review.Verdict]int // of done runs
	// Retries counts attempts beyond the first over all runs.
	Retries int
	// Failures counts failed runs by their error message.
	Failures   map[string]int
	Done       []DoneRun
	Usage      []ModelUsage
	CostByRepo map[string]float64
}

// Stats reads the metrics snapshot in one read-only transaction.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{
		Runs: map[run.State]int{}, Verdicts: map[review.Verdict]int{},
		Failures: map[string]int{}, CostByRepo: map[string]float64{},
	}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := forEach(ctx, tx, `SELECT state, count(*) FROM review_run GROUP BY state`, func(rows pgx.Rows) error {
			var state run.State
			var n int
			err := rows.Scan(&state, &n)
			st.Runs[state] = n
			return err
		})
		if err != nil {
			return err
		}
		err = forEach(ctx, tx, `SELECT verdict, count(*) FROM review_run WHERE state = 'done' GROUP BY verdict`, func(rows pgx.Rows) error {
			var v review.Verdict
			var n int
			err := rows.Scan(&v, &n)
			st.Verdicts[v] = n
			return err
		})
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(GREATEST(attempt - 1, 0)), 0) FROM review_run`).Scan(&st.Retries); err != nil {
			return err
		}
		err = forEach(ctx, tx, `SELECT COALESCE(error, ''), count(*) FROM review_run WHERE state = 'failed' GROUP BY 1`, func(rows pgx.Rows) error {
			var msg string
			var n int
			err := rows.Scan(&msg, &n)
			st.Failures[msg] = n
			return err
		})
		if err != nil {
			return err
		}
		err = forEach(ctx, tx, `
			SELECT extract(epoch FROM r.started_at - r.created_at)::float8,
			       extract(epoch FROM r.fetch_started_at - r.started_at)::float8,
			       extract(epoch FROM r.fetch_finished_at - r.fetch_started_at)::float8,
			       extract(epoch FROM r.reviewed_at - r.fetch_finished_at)::float8,
			       extract(epoch FROM r.finished_at - r.reviewed_at)::float8,
			       extract(epoch FROM r.finished_at - r.created_at)::float8,
			       COALESCE((SELECT sum(u.cost_usd) FROM run_usage u WHERE u.run_id = r.id), 0)::float8
			FROM review_run r WHERE r.state = 'done'`, func(rows pgx.Rows) error {
			var d [6]*float64
			var cost float64
			if err := rows.Scan(&d[0], &d[1], &d[2], &d[3], &d[4], &d[5], &cost); err != nil {
				return err
			}
			dr := DoneRun{Seconds: map[string]float64{}, CostUSD: cost}
			for i, phase := range Phases {
				if d[i] != nil && *d[i] >= 0 {
					dr.Seconds[phase] = *d[i]
				}
			}
			st.Done = append(st.Done, dr)
			return nil
		})
		if err != nil {
			return err
		}
		err = forEach(ctx, tx, `
			SELECT model, sum(input_tokens), sum(output_tokens), sum(cache_read_tokens), sum(cache_write_tokens), sum(cost_usd)::float8
			FROM run_usage GROUP BY model ORDER BY model`, func(rows pgx.Rows) error {
			var u ModelUsage
			err := rows.Scan(&u.Model, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.CostUSD)
			st.Usage = append(st.Usage, u)
			return err
		})
		if err != nil {
			return err
		}
		return forEach(ctx, tx, `
			SELECT r.repo, sum(u.cost_usd)::float8 FROM run_usage u JOIN review_run r ON r.id = u.run_id GROUP BY r.repo`, func(rows pgx.Rows) error {
			var repo string
			var cost float64
			err := rows.Scan(&repo, &cost)
			st.CostByRepo[repo] = cost
			return err
		})
	})
	if err != nil {
		return Stats{}, fmt.Errorf("read stats: %w", err)
	}
	return st, nil
}

func forEach(ctx context.Context, tx pgx.Tx, sql string, scan func(pgx.Rows) error) error {
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
