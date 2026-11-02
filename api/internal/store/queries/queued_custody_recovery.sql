-- name: RefreshFrozenQueuedCustodyTargets :one
-- Read only the already locked parent set. Before recovery writes, an empty
-- requeued set validates the complete frozen tuple against the post-lock view.
-- Afterwards only successful requeue IDs may refresh status and count;
-- every other identity/control/release field must still match. No locks expand.
SELECT COALESCE(jsonb_agg(to_jsonb(f) || jsonb_build_object(
    'status', r.status, 'status_since', r.status_since, 'requeue_count', r.requeue_count)), '[]'::jsonb)::jsonb AS frozen_targets
FROM jsonb_to_recordset(@frozen_targets::jsonb) AS f(
          id uuid,
          kind text,
          target_run_id uuid,
          worker_id uuid,
          user_id uuid,
          status text,
          status_since timestamptz,
          requeue_count integer,
          claimed_at timestamptz,
          claim_generation bigint,
          worker_recovery_episode bigint,
          requeue_episode_baseline integer,
          claim_released_at timestamptz,
          released_worker_id uuid,
          released_worker_nonce text,
          pause_requested_at timestamptz,
          pause_mode text,
          credential_switch_requested_at timestamptz,
          credential_switch_generation bigint,
          stop_kind text,
          stop_reason text,
          hold_reason text)
JOIN runs r ON r.id = f.id
WHERE r.id = ANY(@locked_parent_ids::uuid[]) AND r.kind NOT IN ('chat', 'cross_check')
  AND f.id IS NOT DISTINCT FROM r.id
  AND f.kind IS NOT DISTINCT FROM r.kind
  AND f.target_run_id IS NOT DISTINCT FROM r.target_run_id
  AND f.worker_id IS NOT DISTINCT FROM r.worker_id
  AND f.user_id IS NOT DISTINCT FROM r.user_id
  AND f.claimed_at IS NOT DISTINCT FROM r.claimed_at
  AND f.claim_generation IS NOT DISTINCT FROM r.claim_generation
  AND f.worker_recovery_episode IS NOT DISTINCT FROM r.worker_recovery_episode
  AND f.requeue_episode_baseline IS NOT DISTINCT FROM r.requeue_episode_baseline
  AND f.claim_released_at IS NOT DISTINCT FROM r.claim_released_at
  AND f.released_worker_id IS NOT DISTINCT FROM r.released_worker_id
  AND f.released_worker_nonce IS NOT DISTINCT FROM r.released_worker_nonce
  AND f.pause_requested_at IS NOT DISTINCT FROM r.pause_requested_at
  AND f.pause_mode IS NOT DISTINCT FROM r.pause_mode
  AND f.credential_switch_requested_at IS NOT DISTINCT FROM r.credential_switch_requested_at
  AND f.credential_switch_generation IS NOT DISTINCT FROM r.credential_switch_generation
  AND f.stop_kind IS NOT DISTINCT FROM r.stop_kind
  AND f.stop_reason IS NOT DISTINCT FROM r.stop_reason
  AND f.hold_reason IS NOT DISTINCT FROM r.hold_reason
  AND ((f.status IS NOT DISTINCT FROM r.status
        AND f.status_since IS NOT DISTINCT FROM r.status_since
        AND f.requeue_count = r.requeue_count)
       OR (r.id = ANY(@requeued_ids::uuid[]) AND f.status IN ('running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
           AND r.status = 'queued' AND r.requeue_count = f.requeue_count + 1));

-- name: LockStaleQueuedCustodyWorkers :many
-- Settings admission precedes every worker lock. Lock the full worker set before
-- any recovery parent locks; the caller rechecks staleness under these locks.
SELECT w.id FROM workers w
WHERE (w.last_heartbeat_at IS NULL OR w.last_heartbeat_at < @cutoff)
  AND EXISTS (SELECT 1 FROM runs r WHERE r.worker_id = w.id
      AND r.status = 'queued' AND r.claim_generation > 0)
ORDER BY w.id FOR UPDATE OF w;

-- name: LockFrozenQueuedCustodyHolds :many
-- Parents (including every non-checker target) are already locked. This is an
-- independent statement before the writer's fresh count. NO KEY UPDATE excludes
-- custody settlement while allowing FK key-share readers.
SELECT h.id FROM recovery_custody_holds h
JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
WHERE h.state = 'open' AND h.generation BETWEEN 1 AND r.claim_generation
  AND r.worker_id = @worker_id
  AND r.kind NOT IN ('chat','cross_check') AND r.status = 'queued'
  AND r.id = ANY(@locked_parent_ids::uuid[])
  AND EXISTS (
      SELECT 1 FROM jsonb_to_recordset(@frozen_targets::jsonb) AS f(
          id uuid,
          kind text,
          target_run_id uuid,
          worker_id uuid,
          user_id uuid,
          status text,
          status_since timestamptz,
          requeue_count integer,
          claimed_at timestamptz,
          claim_generation bigint,
          worker_recovery_episode bigint,
          requeue_episode_baseline integer,
          claim_released_at timestamptz,
          released_worker_id uuid,
          released_worker_nonce text,
          pause_requested_at timestamptz,
          pause_mode text,
          credential_switch_requested_at timestamptz,
          credential_switch_generation bigint,
          stop_kind text,
          stop_reason text,
          hold_reason text)
      WHERE f.id IS NOT DISTINCT FROM r.id
        AND f.kind IS NOT DISTINCT FROM r.kind
        AND f.target_run_id IS NOT DISTINCT FROM r.target_run_id
        AND f.worker_id IS NOT DISTINCT FROM r.worker_id
        AND f.user_id IS NOT DISTINCT FROM r.user_id
        AND f.status IS NOT DISTINCT FROM r.status
        AND f.status_since IS NOT DISTINCT FROM r.status_since
        AND f.requeue_count IS NOT DISTINCT FROM r.requeue_count
        AND f.claimed_at IS NOT DISTINCT FROM r.claimed_at
        AND f.claim_generation IS NOT DISTINCT FROM r.claim_generation
        AND f.worker_recovery_episode IS NOT DISTINCT FROM r.worker_recovery_episode
        AND f.requeue_episode_baseline IS NOT DISTINCT FROM r.requeue_episode_baseline
        AND f.claim_released_at IS NOT DISTINCT FROM r.claim_released_at
        AND f.released_worker_id IS NOT DISTINCT FROM r.released_worker_id
        AND f.released_worker_nonce IS NOT DISTINCT FROM r.released_worker_nonce
        AND f.pause_requested_at IS NOT DISTINCT FROM r.pause_requested_at
        AND f.pause_mode IS NOT DISTINCT FROM r.pause_mode
        AND f.credential_switch_requested_at IS NOT DISTINCT FROM r.credential_switch_requested_at
        AND f.credential_switch_generation IS NOT DISTINCT FROM r.credential_switch_generation
        AND f.stop_kind IS NOT DISTINCT FROM r.stop_kind
        AND f.stop_reason IS NOT DISTINCT FROM r.stop_reason
        AND f.hold_reason IS NOT DISTINCT FROM r.hold_reason)
ORDER BY h.run_id, h.id FOR NO KEY UPDATE OF h;

-- name: ParkFrozenQueuedCustodyExhausted :many
-- State-only recovery: durable publication uncertainty, captures and evidence are
-- retained observations, never publication liveness. Do not reuse the active-run
-- exhaustion writer here: it clears controls and increments the Codex epoch.
UPDATE runs r SET status = 'recovery_wait', status_since = now(),
    recovery_wait_cause = 'worker_requeue_exhausted',
    recovery_retry_not_before = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    claim_released_at = COALESCE(r.claim_released_at, now()),
    released_worker_id = CASE WHEN r.claim_released_at IS NULL THEN r.worker_id ELSE r.released_worker_id END,
    released_worker_nonce = CASE WHEN r.claim_released_at IS NULL
        THEN sqlc.narg(released_worker_nonce)::text ELSE r.released_worker_nonce END,
    codex_cap_hash = NULL,
    updated_at = now()
WHERE r.worker_id = @worker_id AND r.id = ANY(@locked_parent_ids::uuid[])
  AND r.status = 'queued' AND r.claim_generation > 0
  AND r.kind NOT IN ('chat','cross_check')
  AND r.pause_requested_at IS NULL AND r.credential_switch_requested_at IS NULL
  AND r.stop_reason IS NULL AND r.stop_kind IS NULL AND r.hold_reason IS NULL
  AND (r.requeue_count - r.requeue_episode_baseline) >= @max_requeues
  AND EXISTS (
      SELECT 1 FROM jsonb_to_recordset(@frozen_targets::jsonb) AS f(
          id uuid,
          kind text,
          target_run_id uuid,
          worker_id uuid,
          user_id uuid,
          status text,
          status_since timestamptz,
          requeue_count integer,
          claimed_at timestamptz,
          claim_generation bigint,
          worker_recovery_episode bigint,
          requeue_episode_baseline integer,
          claim_released_at timestamptz,
          released_worker_id uuid,
          released_worker_nonce text,
          pause_requested_at timestamptz,
          pause_mode text,
          credential_switch_requested_at timestamptz,
          credential_switch_generation bigint,
          stop_kind text,
          stop_reason text,
          hold_reason text)
      WHERE f.id IS NOT DISTINCT FROM r.id
        AND f.kind IS NOT DISTINCT FROM r.kind
        AND f.target_run_id IS NOT DISTINCT FROM r.target_run_id
        AND f.worker_id IS NOT DISTINCT FROM r.worker_id
        AND f.user_id IS NOT DISTINCT FROM r.user_id
        AND f.status IS NOT DISTINCT FROM r.status
        AND f.status_since IS NOT DISTINCT FROM r.status_since
        AND f.requeue_count IS NOT DISTINCT FROM r.requeue_count
        AND f.claimed_at IS NOT DISTINCT FROM r.claimed_at
        AND f.claim_generation IS NOT DISTINCT FROM r.claim_generation
        AND f.worker_recovery_episode IS NOT DISTINCT FROM r.worker_recovery_episode
        AND f.requeue_episode_baseline IS NOT DISTINCT FROM r.requeue_episode_baseline
        AND f.claim_released_at IS NOT DISTINCT FROM r.claim_released_at
        AND f.released_worker_id IS NOT DISTINCT FROM r.released_worker_id
        AND f.released_worker_nonce IS NOT DISTINCT FROM r.released_worker_nonce
        AND f.pause_requested_at IS NOT DISTINCT FROM r.pause_requested_at
        AND f.pause_mode IS NOT DISTINCT FROM r.pause_mode
        AND f.credential_switch_requested_at IS NOT DISTINCT FROM r.credential_switch_requested_at
        AND f.credential_switch_generation IS NOT DISTINCT FROM r.credential_switch_generation
        AND f.stop_kind IS NOT DISTINCT FROM r.stop_kind
        AND f.stop_reason IS NOT DISTINCT FROM r.stop_reason
        AND f.hold_reason IS NOT DISTINCT FROM r.hold_reason)
  -- Same owner count and bounded continuation exemption as ClaimRun. An own
  -- open hold is also mandatory, so ordinary and generation-zero work cannot park.
  AND @custody_hold_limit::integer > 0
  AND (SELECT count(*) FROM recovery_custody_holds h
      WHERE h.id = ANY(@locked_hold_ids::uuid[])
        AND h.run_id = r.id AND h.user_id = r.user_id AND h.state = 'open'
        AND h.generation BETWEEN 1 AND r.claim_generation) >= @custody_hold_limit
  AND fn_custody_admission_count(r.user_id, @heartbeat_cutoff::timestamptz) >= @custody_hold_limit
  -- A fresh current-generation execution or pending outcome still owns this run.
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
      WHERE a.worker_id = r.worker_id AND a.run_id = r.id AND a.claim_generation = r.claim_generation
        AND r.claim_released_at IS NULL
        AND (a.reported_at >= @snapshot_fresh_cutoff::timestamptz
             OR (a.terminal_pending AND a.terminal_pending_until > now())))
  AND NOT EXISTS (SELECT 1 FROM workers w
      WHERE w.id = r.worker_id AND w.pending_overflow_until > now())
  -- Durable publication enrollment and historical finalize generation marks
  -- do not prove live authority. Only an attested requeue in this transaction
  -- carries a usable finalize continuation into the queued write.
  AND NOT (r.id = ANY(COALESCE(@finalize_requeued_ids::uuid[], ARRAY[]::uuid[]))
      AND @max_requeues > 0 AND r.worker_recovery_episode = 0
      AND (r.requeue_count - r.requeue_episode_baseline) > @max_requeues
      AND r.finalize_resume_generation = r.claim_generation)
RETURNING r.id, r.status;
