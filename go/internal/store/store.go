// Package store persists review runs in PostgreSQL. It owns transactional
// invariants only; it never calls GitHub, Kubernetes, or the model proxy.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
)

// Migrations holds the ordered schema migrations, applied by the controller at startup.
//
//go:embed migrations/*.sql
var Migrations embed.FS

var (
	// ErrNotFound means no run has the given ID.
	ErrNotFound = errors.New("run not found")
	// ErrConflict means the run was not in the state the caller expected.
	ErrConflict = errors.New("run is not in the expected state")
)

// ActionKind names one GitHub side effect of a run. The github_action.kind
// CHECK constraint mirrors these values; keep both in sync.
type ActionKind string

const (
	ActionDismissStaleApprovals ActionKind = "dismiss_stale_approvals"
	ActionCheckRunCreate        ActionKind = "check_run_create"
	ActionCheckRunComplete      ActionKind = "check_run_complete"
	ActionReview                ActionKind = "review"
	ActionMerge                 ActionKind = "merge"
)

var ActionKinds = []ActionKind{ActionDismissStaleApprovals, ActionCheckRunCreate, ActionCheckRunComplete, ActionReview, ActionMerge}

// NewRun is a review trigger recorded by the gateway.
type NewRun struct {
	Repo           string
	PRNumber       int
	HeadSHA        string
	InstallationID int64
	BudgetUSD      float64
}

// Run is one row of review_run plus the GitHub actions it has completed.
type Run struct {
	ID             string
	Repo           string
	PRNumber       int
	HeadSHA        string
	InstallationID int64
	State          run.State
	Attempt        int
	JobName        string
	Verdict        review.Verdict
	Result         *review.Result
	Error          string
	BudgetUSD      float64
	CreatedAt      time.Time
	// FetchTimed reports whether the fetch phase's timing has been recorded.
	FetchTimed bool
	// Actions maps each completed GitHub action to its external ID.
	Actions map[ActionKind]string
}

// Done reports whether the run already performed the action.
func (r Run) Done(kind ActionKind) bool {
	_, ok := r.Actions[kind]
	return ok
}

// Store is a PostgreSQL-backed run store.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database at url. Connections are made lazily.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Arbitrary advisory lock keys that serialize Migrate and Claim across
// controller replicas.
const (
	migrationLock = 0x61707263
	claimLock     = 0x61707264
)

// Migrate applies every migration not applied yet, each in its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock) //nolint:errcheck // released with the session anyway

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	names, err := fs.Glob(Migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		sql, err := Migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "INSERT INTO schema_migration (version) VALUES ($1) ON CONFLICT DO NOTHING", name)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			_, err = tx.Exec(ctx, string(sql))
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate %s: %w", name, err)
		}
	}
	return nil
}

// RecordTrigger records a webhook delivery and queues a run for it. It returns
// created=false when the delivery was seen before or the revision already has a run.
func (s *Store) RecordTrigger(ctx context.Context, deliveryID string, nr NewRun) (id string, created bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "INSERT INTO webhook_delivery (delivery_id) VALUES ($1) ON CONFLICT DO NOTHING", deliveryID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO review_run (repo, pr_number, head_sha, installation_id, state, budget_usd)
			VALUES ($1, $2, $3, $4, 'queued', $5)
			ON CONFLICT (repo, pr_number, head_sha) DO NOTHING
			RETURNING id`,
			nr.Repo, nr.PRNumber, nr.HeadSHA, nr.InstallationID, nr.BudgetUSD).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		created = err == nil
		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("record trigger: %w", err)
	}
	return id, created, nil
}

const runColumns = `r.id, r.repo, r.pr_number, r.head_sha, r.installation_id, r.state, r.attempt,
	COALESCE(r.job_name, ''), COALESCE(r.verdict, ''), r.result, COALESCE(r.error, ''), r.budget_usd::float8, r.created_at,
	r.fetch_finished_at IS NOT NULL,
	COALESCE((SELECT jsonb_object_agg(a.kind, a.external_id) FROM github_action a
	          WHERE a.run_id = r.id AND a.external_id IS NOT NULL), '{}')`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var result, actions []byte
	err := row.Scan(&r.ID, &r.Repo, &r.PRNumber, &r.HeadSHA, &r.InstallationID, &r.State, &r.Attempt,
		&r.JobName, &r.Verdict, &result, &r.Error, &r.BudgetUSD, &r.CreatedAt, &r.FetchTimed, &actions)
	if err != nil {
		return Run{}, err
	}
	if result != nil {
		var res review.Result
		if err := json.Unmarshal(result, &res); err != nil {
			return Run{}, fmt.Errorf("run %s: decode result: %w", r.ID, err)
		}
		r.Result = &res
	}
	if err := json.Unmarshal(actions, &r.Actions); err != nil {
		return Run{}, fmt.Errorf("run %s: decode actions: %w", r.ID, err)
	}
	return r, nil
}

func collectRuns(rows pgx.Rows) ([]Run, error) {
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// Get returns one run.
func (s *Store) Get(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, "SELECT "+runColumns+" FROM review_run r WHERE r.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// Claim moves the oldest queued runs to running, keeping at most maxRunning
// runs in flight. jobName names the Job for a run's attempt. Concurrent callers
// are serialized, so they neither claim the same run nor exceed maxRunning.
func (s *Store) Claim(ctx context.Context, maxRunning int, jobName func(id string, attempt int) string) ([]Run, error) {
	var claimed []Run
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", claimLock); err != nil {
			return err
		}
		var inFlight int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM review_run WHERE state IN ('running', 'previewing')").Scan(&inFlight); err != nil {
			return err
		}
		if inFlight >= maxRunning {
			return nil
		}
		rows, err := tx.Query(ctx, "SELECT "+runColumns+` FROM review_run r WHERE r.state = 'queued'
			ORDER BY r.created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, maxRunning-inFlight)
		if err != nil {
			return err
		}
		queued, err := collectRuns(rows)
		if err != nil {
			return err
		}
		for _, r := range queued {
			r.State, r.Attempt = run.StateRunning, r.Attempt+1
			r.JobName = jobName(r.ID, r.Attempt)
			if _, err := tx.Exec(ctx, `UPDATE review_run SET state = $2, attempt = $3, job_name = $4, started_at = now(), updated_at = now()
				WHERE id = $1`, r.ID, r.State, r.Attempt, r.JobName); err != nil {
				return err
			}
			claimed = append(claimed, r)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim runs: %w", err)
	}
	return claimed, nil
}

// Active returns the runs the controller still has to act on: running and
// reviewed runs, and failed runs whose failure is not on a Check Run yet.
func (s *Store) Active(ctx context.Context) ([]Run, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+runColumns+` FROM review_run r
		WHERE r.state IN ('running', 'reviewed')
		   OR (r.state = 'failed'
		       AND NOT EXISTS (SELECT 1 FROM github_action a WHERE a.run_id = r.id AND a.kind = 'check_run_complete' AND a.external_id IS NOT NULL))
		ORDER BY r.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	runs, err := collectRuns(rows)
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	return runs, nil
}

// RecordAction marks a GitHub action of the run as done.
func (s *Store) RecordAction(ctx context.Context, runID string, kind ActionKind, externalID string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO github_action (run_id, kind, external_id) VALUES ($1, $2, $3)
		ON CONFLICT (run_id, kind) DO UPDATE SET external_id = EXCLUDED.external_id`, runID, kind, externalID)
	if err != nil {
		return fmt.Errorf("record %s for run %s: %w", kind, runID, err)
	}
	return nil
}

// SubmitResult stores the runner's result and moves the run from running to reviewed.
func (s *Store) SubmitResult(ctx context.Context, id string, res review.Result) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE review_run SET state = 'reviewed', verdict = $2, result = $3, reviewed_at = now(), updated_at = now()
		WHERE id = $1 AND state = 'running'`, id, res.Verdict, data)
	if err != nil {
		return fmt.Errorf("submit result for run %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// Transition moves a run from one state to another, recording reason as its
// error when non-empty. It returns ErrConflict if the run is no longer in from.
func (s *Store) Transition(ctx context.Context, id string, from, to run.State, reason string) error {
	if !run.CanTransition(from, to) {
		return fmt.Errorf("transition %s -> %s is not allowed", from, to)
	}
	// Reasons quote external messages; PostgreSQL rejects invalid UTF-8.
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	tag, err := s.pool.Exec(ctx, `UPDATE review_run SET state = $3, error = COALESCE(NULLIF($4, ''), error), updated_at = now(),
		finished_at = CASE WHEN $5 THEN now() ELSE finished_at END
		WHERE id = $1 AND state = $2`, id, from, to, reason, to.Terminal())
	if err != nil {
		return fmt.Errorf("transition run %s to %s: %w", id, to, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}
