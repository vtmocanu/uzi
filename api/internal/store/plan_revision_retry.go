package store

import (
	"context"

	"github.com/google/uuid"
)

// SettlePlanRevisionRetry is called only after publication while holding the run
// lock in the state transaction. The receipt is part of that same commit.
func (q *Queries) SettlePlanRevisionRetry(ctx context.Context, runID, workerID uuid.UUID, generation, activeID int64, predecessors []int64, receipt []byte) (int64, error) {
	tag, err := q.db.Exec(ctx, `
 WITH settled AS (
 UPDATE run_user_inputs SET applied_at = now(),
 disposition = CASE WHEN id = $4 THEN disposition ELSE 'superseded' END
 WHERE run_id = $1 AND (id = $4 OR id = ANY($5::bigint[]))
 AND kind = 'revise_plan' AND applied_at IS NULL AND consumed_at IS NOT NULL
 AND consumed_worker_id = $2 AND consumed_claim_generation = $3
 RETURNING id
 ), proof AS (
 UPDATE runs SET gate_presented_payload = gate_presented_payload ||
 jsonb_build_object('revision_retry_receipt', $6::jsonb)
 WHERE id = $1 AND (SELECT count(*) FROM settled) = COALESCE(cardinality($5::bigint[]), 0) + 1
 )
 SELECT id FROM settled`, runID, workerID, generation, activeID, predecessors, receipt)
	return tag.RowsAffected(), err
}
