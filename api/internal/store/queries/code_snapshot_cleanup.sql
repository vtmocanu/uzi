-- name: GetCodeSnapshotCleanup :one
SELECT lead.id AS lead_run_id,
       cc.head_commit,
       cc.outcome,
       lead.status AS lead_status,
       COALESCE(lead.worker_id = sqlc.arg(worker_id)::uuid AND lead.claim_released_at IS NULL, false)::boolean AS owned_by_worker,
       child.id AS checker_run_id,
       child.claim_generation AS checker_claim_generation,
       child.status AS checker_status
FROM runs AS lead
JOIN cross_checks AS cc ON cc.lead_run_id = lead.id AND cc.stage = 'code'
JOIN runs AS child ON child.id = cc.checker_run_id
    AND child.user_id = lead.user_id
    AND child.kind = 'cross_check'
    AND child.target_run_id = lead.id
    AND child.worker_id = sqlc.arg(worker_id)::uuid
WHERE lead.id = sqlc.arg(lead_run_id)::uuid
  AND lead.user_id = sqlc.arg(user_id)::uuid;
