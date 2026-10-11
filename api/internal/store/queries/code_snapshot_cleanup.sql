-- name: GetCodeSnapshotCleanup :one
-- Answers for any lead the caller's user owns, whether or not a code cross-check row or
-- child exists, so a worker can tell a valid "no row" / "no child" answer from a failed
-- lookup. cc.lead_claim_generation is the lead claim generation that pinned the snapshot
-- (the worker pins with the same value), and is 0 when there is no row. The child is
-- joined without a worker condition: a child on another worker that can still run must
-- not read as "no child".
SELECT lead.id AS lead_run_id,
       lead.status AS lead_status,
       COALESCE(lead.worker_id = sqlc.arg(worker_id)::uuid AND lead.claim_released_at IS NULL, false)::boolean AS owned_by_worker,
       (cc.lead_run_id IS NOT NULL)::boolean AS row_present,
       COALESCE(cc.lead_claim_generation, 0)::bigint AS lead_claim_generation,
       cc.head_commit,
       cc.outcome,
       (cc.checker_run_id IS NOT NULL)::boolean AS child_mapped,
       child.id AS checker_run_id,
       COALESCE(child.claim_generation, 0)::bigint AS checker_claim_generation,
       child.status AS checker_status
FROM runs AS lead
LEFT JOIN cross_checks AS cc ON cc.lead_run_id = lead.id AND cc.stage = 'code'
LEFT JOIN runs AS child ON child.id = cc.checker_run_id
    AND child.user_id = lead.user_id
    AND child.kind = 'cross_check'
    AND child.target_run_id = lead.id
WHERE lead.id = sqlc.arg(lead_run_id)::uuid
  AND lead.user_id = sqlc.arg(user_id)::uuid;
