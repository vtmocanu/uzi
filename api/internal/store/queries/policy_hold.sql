-- name: ApplyPolicyHold :one
-- Caller holds the worker/run mutation fence and has reconciled the recorded identity.
WITH held AS (
    UPDATE runs SET status='paused', status_since=now(), updated_at=now(),
        budget_paused_seconds = budget_paused_seconds + CASE
            WHEN status IN ('awaiting_approval','awaiting_input')
            THEN GREATEST(0,EXTRACT(EPOCH FROM (now()-status_since))::int) ELSE 0 END,
        hold_reason='codex_policy_hold',
        session_id=COALESCE(sqlc.narg('session_id')::text,session_id),
        hold_captured_head=sqlc.narg('head')::text,
        checkpoint_contains_latest=sqlc.narg('checkpoint_contains_latest')::boolean,
        policy_hold_published= @published::boolean,
        policy_preserve_required=true,policy_pending_fresh=false,
        policy_outcome_id= @outcome_id,policy_continuation_mode= @continuation_mode,
        policy_resolved_at=NULL,policy_applying_worker_id= @worker_id,
        policy_applying_generation= @claim_generation,policy_source_generation= @source_generation,
        policy_refusal_count=CASE WHEN @actual_refusal::boolean
            THEN LEAST(policy_refusal_count::bigint+1,2147483647)::integer ELSE policy_refusal_count END,
        policy_refusal_tag=CASE WHEN @actual_refusal::boolean THEN sqlc.narg('policy_tag')::text ELSE policy_refusal_tag END,
        policy_refusal_phase=CASE WHEN @actual_refusal::boolean THEN sqlc.narg('phase')::text ELSE policy_refusal_phase END,
        policy_refusal_correlation_id=CASE WHEN @actual_refusal::boolean THEN sqlc.narg('correlation_id')::text ELSE policy_refusal_correlation_id END,
        pause_requested_at=NULL,pause_mode=NULL,pause_after_count=NULL,
        health='ok',health_reason=NULL,health_since=NULL
    WHERE runs.id= @id AND runs.worker_id= @worker_id AND runs.user_id= @user_id
      AND status IN ('claimed','running') AND claim_released_at IS NULL
      AND claim_generation= @claim_generation AND stop_kind IS NULL
      AND kind IN ('issue','task','prompt','self_improve') AND NOT interactive AND harness='codex'
      AND (policy_applying_generation IS NULL OR policy_applying_generation < @claim_generation)
    RETURNING runs.*
), settled AS (
    UPDATE run_user_inputs SET consumed_at=COALESCE(consumed_at,now()),applied_at=now()
    WHERE run_id IN (SELECT id FROM held) AND kind IN ('pause','pause_cancel') AND applied_at IS NULL
)
SELECT * FROM held;
