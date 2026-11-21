-- name: SetCodexSwitchStopped :one
-- Caller holds worker then run locks. One eligibility set serves mutation and replay.
-- A paused row satisfying this predicate is equivalent to stopped settlement.
-- This is history-free: the predicate cannot prove which event caused the pause.
WITH clock AS MATERIALIZED (
    SELECT clock_timestamp() AS stamp
), eligible AS MATERIALIZED (
    SELECT r.id, r.status FROM runs r CROSS JOIN clock
    JOIN workers w ON w.id = r.worker_id AND w.user_id = r.user_id
    WHERE r.id = sqlc.arg(run_id)::uuid AND w.id = sqlc.arg(worker_id)::uuid AND r.user_id = sqlc.arg(user_id)::uuid
      AND r.claim_generation = sqlc.arg(claim_generation)::bigint AND r.claim_released_at IS NULL
      AND w.snapshot_register_nonce = sqlc.arg(register_nonce)::text
      AND r.codex_claim_epoch = sqlc.arg(expected_epoch)::bigint
      AND r.codex_secret_id IS NOT NULL AND r.codex_cap_hash IS NOT NULL
      AND r.harness = 'codex' AND r.review_target_run_id IS NULL
      AND r.kind NOT IN ('chat', 'judge', 'job', 'cross_check')
      AND r.status IN ('running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup', 'paused')
      AND r.credential_switch_requested_at IS NULL AND r.credential_switch_generation IS NULL
      AND r.hold_reason IS NULL AND NOT r.policy_pending_fresh
      AND (r.policy_outcome_id IS NULL OR r.policy_resolved_at IS NOT NULL)
      AND r.pause_requested_at IS NULL AND r.pause_mode IS NULL
      AND r.stop_kind IS NULL AND r.stop_reason IS NULL AND r.reserve_hold_reason IS NULL
      AND NOT EXISTS (
          SELECT 1 FROM run_user_inputs i WHERE i.run_id = r.id
            AND i.kind IN ('pause', 'pause_cancel', 'cancel', 'stop', 'scope', 'revise_plan', 'reject_plan', 'resume')
            AND i.applied_at IS NULL AND i.disposition IS DISTINCT FROM 'superseded'
            AND NOT EXISTS (SELECT 1 FROM run_milestone_reconciliations a
                            WHERE a.run_id = i.run_id AND i.id <= a.stop_cutoff
                              AND i.kind IN ('stop', 'cancel', 'reject_plan')))
      AND (r.kind IN ('chat', 'judge') OR r.interactive OR r.started_at IS NULL
           OR (COALESCE(r.budget_wall_seconds, sqlc.arg(timeout_seconds)::int) + r.budget_extension_seconds + r.budget_finalize_seconds)
              - (GREATEST(0, EXTRACT(EPOCH FROM
                   ((CASE WHEN r.status = 'paused' THEN r.status_since ELSE clock.stamp END) - r.started_at))::int)
                 - r.budget_paused_seconds) > 0)
), changed AS (
    UPDATE runs r SET status = 'paused', status_since = clock.stamp,
        updated_at = clock.stamp, health = 'ok', health_reason = NULL, health_since = NULL
    FROM eligible e CROSS JOIN clock WHERE r.id = e.id AND e.status <> 'paused'
    RETURNING r.id
)
SELECT (EXISTS (SELECT 1 FROM changed)
        OR EXISTS (SELECT 1 FROM eligible WHERE status = 'paused'))::boolean AS accepted;
