package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CancelFrozenMissingRuns runs inside the heartbeat transaction after snapshot
// application and parent capture. It rechecks the frozen incarnation,
// omission and pending-outcome fences before completing an already queued cancel.
// The finite candidate result bounds attempts to one per run; any error rolls back
// the heartbeat, including sibling cancellations. The caller context bounds time.
func (q *Queries) CancelFrozenMissingRuns(ctx context.Context, worker uuid.UUID, cutoff pgtype.Timestamptz, frozen []byte, parents []uuid.UUID) ([]uuid.UUID, error) {
	const candidates = `
 WITH frozen AS MATERIALIZED (
 SELECT * FROM jsonb_to_recordset($3::jsonb) AS f(
 id uuid, kind text, target_run_id uuid, worker_id uuid, claim_generation bigint,
 cross_check_id uuid, parent_lead_id uuid)
 )
 SELECT r.id FROM runs r JOIN frozen f ON f.id = r.id
 LEFT JOIN cross_checks cc ON r.kind = 'cross_check' AND cc.checker_run_id = r.id
 LEFT JOIN runs parent ON parent.id = CASE WHEN r.kind = 'cross_check' THEN cc.lead_run_id ELSE r.id END
 AND parent.kind <> 'cross_check' AND parent.user_id = r.user_id
 WHERE r.worker_id = $1 AND r.kind <> 'chat'
 AND r.status IN ('running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
 AND r.stop_kind = 'cancelled' AND r.claim_released_at IS NULL AND r.status_since < $2
 AND r.kind IS NOT DISTINCT FROM f.kind AND r.target_run_id IS NOT DISTINCT FROM f.target_run_id
 AND r.worker_id IS NOT DISTINCT FROM f.worker_id AND r.claim_generation IS NOT DISTINCT FROM f.claim_generation
 AND cc.id IS NOT DISTINCT FROM f.cross_check_id AND parent.id IS NOT DISTINCT FROM f.parent_lead_id
 AND ((parent.id = ANY($4::uuid[]) AND (r.kind <> 'cross_check' OR
 (cc.id = f.cross_check_id AND cc.lead_run_id = f.parent_lead_id)))
 OR (r.kind = 'cross_check' AND cc.id IS NULL AND f.cross_check_id IS NULL AND f.parent_lead_id IS NULL))
 AND NOT EXISTS (SELECT 1 FROM worker_active_runs a WHERE a.worker_id = r.worker_id AND a.run_id = r.id AND a.claim_generation = r.claim_generation)
 AND NOT EXISTS (SELECT 1 FROM worker_active_runs a WHERE a.worker_id = r.worker_id AND a.run_id = r.id
 AND a.claim_generation = r.claim_generation AND a.terminal_pending AND a.terminal_pending_until > now())
 AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = r.worker_id AND w.pending_overflow_until > now())
 AND ($5::uuid IS NULL OR r.id = $5)
 ORDER BY (r.kind = 'cross_check'), r.id`
	rows, err := q.db.Query(ctx, candidates, worker, cutoff, frozen, parents, nil)
	if err != nil {
		return nil, err
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// LockWorkerRecoveryParents already holds every mapped parent. Cancel parents
	// before checkers, letting CancelRunByWorker and the parent-exit trigger take
	// cross_checks/child locks in their usual order. Do not prelock checker rows:
	// a parent exit may settle the same checker later in this batch.
	cancelled := []uuid.UUID{}
	for _, id := range ids {
		// Recheck under the individual run lock after earlier parent exits have run
		// their triggers, without holding a sibling checker ahead of those exits.
		var locked uuid.UUID
		err := q.db.QueryRow(ctx, candidates+" FOR UPDATE OF r", worker, cutoff, frozen, parents, id).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		n, err := q.CancelRunByWorker(ctx, CancelRunByWorkerParams{ID: id, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}})
		if err != nil {
			return nil, err
		}
		if n != 1 {
			continue
		}
		if _, err := q.db.Exec(ctx, `UPDATE run_user_inputs SET applied_at = now(), consumed_at = COALESCE(consumed_at, now())
   WHERE run_id = $1 AND kind = 'cancel' AND applied_at IS NULL`, id); err != nil {
			return nil, err
		}
		cancelled = append(cancelled, id)
	}
	return cancelled, nil
}
