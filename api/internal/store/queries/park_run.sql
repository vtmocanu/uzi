-- name: CreateWorkerPauseInput :one
-- Ordinary now-pause fields and input, fenced to this worker's live generation.
-- The service holds the run lock while authorizing the persisted plan and budget.
WITH paused_req AS (
    UPDATE runs SET
        pause_requested_at = now(),
        pause_mode = 'now',
        pause_after_count = COALESCE(jsonb_array_length(runs.milestones_completed), 0),
        updated_at = now()
    WHERE runs.id = @id AND runs.worker_id = @worker_id
      AND runs.claim_generation = @claim_generation
      AND runs.claim_released_at IS NULL AND runs.status = 'running'
      AND runs.kind = 'issue' AND runs.interactive = false
      AND runs.pause_requested_at IS NULL
      AND runs.pause_mode IS DISTINCT FROM 'wall'
      AND runs.stop_kind IS NULL AND runs.budget_finalize_seconds = 0
      AND NOT EXISTS (SELECT 1 FROM run_user_inputs
          WHERE run_id = runs.id AND kind IN ('stop', 'cancel'))
    RETURNING runs.id
)
INSERT INTO run_user_inputs (run_id, kind, body)
SELECT paused_req.id, 'pause', 'now' FROM paused_req
RETURNING *;

-- name: ConsumeWorkerPauseInputs :exec
-- The direct pause response delivers the control. Settle ordinary pause inputs
-- in the same run-row transaction so neither steering nor replay re-triggers it.
UPDATE run_user_inputs SET consumed_at = COALESCE(consumed_at, now()),
    applied_at = COALESCE(applied_at, now()),
    consumed_worker_id = @worker_id, consumed_claim_generation = @claim_generation
WHERE run_id = @run_id AND kind = 'pause' AND body IN ('now', 'milestone')
  AND applied_at IS NULL;

-- name: WorkerPauseHasStopInput :one
SELECT EXISTS (SELECT 1 FROM run_user_inputs
    WHERE run_id = @run_id AND kind IN ('stop', 'cancel'))::boolean;
