package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/repoconfig"
	"github.com/jackc/pgx/v5"
)

// SaveConfig is a first-writer-wins snapshot; retries reuse it.
func (s *Store) SaveConfig(ctx context.Context, id, sha string, cfg repoconfig.Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE review_run SET repo_config=$2, config_sha=$3, budget_usd=$4
 WHERE id=$1 AND state='running' AND repo_config IS NULL`, id, data, sha, cfg.BudgetUSD)
	return err
}

type Approval struct {
	Repo           string
	PRNumber       int
	InstallationID int64
	HeadSHA        string
	Actor          string
	ReviewID       int64
	CreatedAt      time.Time
}
type MergeSignal struct {
	DeliveryID, Actor string
	ReviewID          int64
}

// RecordApproval only records an event; the controller checks permissions and
// advances the state. Include reviewed to cover confirmation arriving between
// publishing the vote and entering awaiting_approval. GitHub timestamps have
// second precision. Old approvals cannot unlock a later run.
func (s *Store) RecordApproval(ctx context.Context, delivery string, a Approval) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "INSERT INTO webhook_delivery (delivery_id) VALUES ($1) ON CONFLICT DO NOTHING", delivery)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO merge_signal (delivery_id,run_id,actor,review_id)
   SELECT $1,id,$2,$3 FROM review_run WHERE repo=$4 AND pr_number=$5 AND installation_id=$6
   AND state IN ('reviewed','awaiting_approval') AND verdict='approve'
   AND repo_config->>'approvalMode'='manual' AND ($7='' OR head_sha=$7)
   AND date_trunc('second',reviewed_at) <= $8
   ORDER BY created_at DESC LIMIT 1`, delivery, a.Actor, a.ReviewID, a.Repo, a.PRNumber, a.InstallationID, a.HeadSHA, a.CreatedAt)
		return err
	})
}
func (s *Store) MergeSignals(ctx context.Context, id string) ([]MergeSignal, error) {
	rows, err := s.pool.Query(ctx, "SELECT delivery_id,actor,review_id FROM merge_signal WHERE run_id=$1 AND NOT handled ORDER BY delivery_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MergeSignal
	for rows.Next() {
		var a MergeSignal
		if err := rows.Scan(&a.DeliveryID, &a.Actor, &a.ReviewID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) IgnoreMergeSignal(ctx context.Context, delivery string) error {
	_, err := s.pool.Exec(ctx, "UPDATE merge_signal SET handled=true WHERE delivery_id=$1", delivery)
	if err != nil {
		return fmt.Errorf("handle merge signal: %w", err)
	}
	return nil
}
