-- PRD #3522 M3: weekly usage reserve, queued-run side. The claim gate itself lives in ClaimRun
-- (runtime.sql); this file holds the sweeper's marker pass reads/writes. Readings are in
-- anthropic_rate_limits.sql (ListReserveClaudeTokens) and codex_rate_limits.sql
-- (ListReserveCodexAliases).

-- name: ListReserveMarkOwners :many
-- The owners the sweeper's markReserveHolds pass must visit: a user with a threshold set that has
-- at least one queued non-chat run (the run may need a marker), and ANY user holding a queued run
-- that still carries a marker (the threshold was cleared, so the marker must be dropped). One row
-- per owner.
SELECT DISTINCT r.user_id
FROM runs r
JOIN users u ON u.id = r.user_id
WHERE r.status = 'queued' AND r.kind <> 'chat'
  AND (u.weekly_reserve_pct IS NOT NULL OR r.reserve_hold_reason IS NOT NULL);

-- name: ListQueuedRunsForReserve :many
-- One owner's queued non-chat runs, whole rows (the marker pass evaluates the same fields the
-- ClaimRun gate reads: kind, harness, codex alias, override, dead-credential exclusion, first
-- start, exemption). The owner's queue is bounded by their own run volume.
SELECT * FROM runs
WHERE user_id = @user_id AND status = 'queued' AND kind <> 'chat'
ORDER BY created_at, id;

-- name: ListReserveWorkers :many
-- The owner's registered workers with just the fields that decide which Anthropic lane a claim
-- on that worker resolves to, and whether the worker pins a queued run to itself (affinity).
SELECT id, ephemeral, ephemeral_run_id, anthropic_bind_mode, anthropic_secret_id,
       last_heartbeat_at, draining_since, maintenance_fenced, maintenance_phase
FROM workers
WHERE user_id = @user_id;

-- name: SetRunReserveHoldReason :execrows
-- Writes (or clears, with a NULL reason) a QUEUED run's weekly-reserve hold marker, only on
-- change. updated_at is deliberately untouched: the affinity ceiling and spread grace read it,
-- and a display marker must not extend either. Guarded status = 'queued' so a claim that won the
-- race (ClaimRun clears the marker itself) is never overwritten, and guarded on the marker value
-- the caller READ (@expected, NULL for none) so a claim-requeue that wrote its own reason between
-- the sweeper's read and this write is not clobbered by a verdict computed from the older read.
UPDATE runs SET reserve_hold_reason = sqlc.narg('reason')::text
WHERE id = @id AND status = 'queued'
  AND reserve_hold_reason IS DISTINCT FROM sqlc.narg('reason')::text
  AND reserve_hold_reason IS NOT DISTINCT FROM sqlc.narg('expected')::text;

-- PRD #3522 M4a: the weekly reserve's pause side. Go decides WHICH runs (the reserveEnforced gate and
-- the fresh verdict of each run's recorded credential); these statements apply the decision by run id
-- with the guards repeated, so a run that changed between the list and the write is left alone.

-- name: ListReservePauseOwners :many
-- The owners the sweeper's reserve pause pass must visit: a user with a threshold set that has a
-- running run (a pause may be requested), and ANY user holding a non-terminal run that carries
-- reserve state (a pending reserve request, a weekly_reserve pause, or a manual-resume exemption), so
-- clearing the threshold or flipping the toggle still reaches them.
SELECT u.id AS user_id FROM users u
WHERE u.weekly_reserve_pct IS NOT NULL
  AND EXISTS (SELECT 1 FROM runs r WHERE r.user_id = u.id AND r.status = 'running')
UNION
SELECT r.user_id FROM runs r
WHERE r.status NOT IN ('completed', 'failed', 'cancelled')
  AND (r.pause_mode = 'reserve' OR r.hold_reason = 'weekly_reserve' OR r.reserve_exempt_until IS NOT NULL);

-- name: ListReservePauseRunsForOwner :many
-- One owner's non-terminal, non-chat runs the reserve pause pass may act on: running runs (a pause
-- may be requested), runs carrying a pending reserve request, weekly_reserve pauses (release) and runs
-- carrying a manual-resume exemption (expiry). Whole rows: Go evaluates the same fields the claim
-- gate reads.
SELECT * FROM runs
WHERE user_id = @user_id AND kind <> 'chat'
  AND status NOT IN ('completed', 'failed', 'cancelled')
  AND (status = 'running' OR pause_mode = 'reserve' OR hold_reason = 'weekly_reserve'
       OR reserve_exempt_until IS NOT NULL)
ORDER BY created_at, id;

-- name: ListReserveOverrideRuns :many
-- One owner's QUEUED runs that carry a manual-resume exemption: the set ClaimRun's
-- reserve_override_ended list is computed from (an exemption the reading-aware rule has ended
-- early, which the time-only SQL test cannot see).
SELECT * FROM runs
WHERE user_id = @user_id AND status = 'queued' AND reserve_exempt_until IS NOT NULL;

-- name: RequestReservePauses :many
-- File a SYSTEM-authored pause request (mode 'reserve') on each listed running run, and in the SAME
-- statement the kind='pause' body='milestone' steering input the worker drains (CreatePauseInput's
-- shape; the worker is served 'milestone' for a reserve request, so both harnesses park at the
-- milestone boundary). Never overwrites a pending request of any mode (pause_requested_at IS NULL):
-- an owner or wall request stands. The owner guards (threshold set, pause toggle on) and the pause
-- allowlist (non-interactive issue/task/prompt/self_improve) are repeated here.
WITH requested AS (
    UPDATE runs SET
        pause_requested_at = now(),
        pause_mode         = 'reserve',
        pause_after_count  = COALESCE(jsonb_array_length(runs.milestones_completed), 0),
        updated_at         = now()
    WHERE runs.id = ANY(sqlc.arg('ids')::uuid[])
      AND runs.status = 'running'
      AND runs.kind IN ('issue', 'task', 'prompt', 'self_improve')
      AND runs.interactive = false
      AND runs.pause_requested_at IS NULL
      AND EXISTS (SELECT 1 FROM users u WHERE u.id = runs.user_id
                  AND u.weekly_reserve_pct IS NOT NULL AND u.weekly_reserve_pause_running)
    RETURNING runs.id, runs.user_id, runs.status
),
reserve_input AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT id, 'pause', 'milestone' FROM requested
)
SELECT id, user_id, status FROM requested;

-- name: WithdrawReservePausesRunning :many
-- Withdraw a pending RESERVE request on each listed running run: the CancelPauseInput shape (clear
-- the three pause columns, write the kind='pause_cancel' input the worker drains). Only where
-- pause_mode = 'reserve', so an owner or wall request that overwrote it is never touched.
WITH withdrawn AS (
    UPDATE runs SET
        pause_requested_at = NULL,
        pause_mode         = NULL,
        pause_after_count  = NULL,
        updated_at         = now()
    WHERE runs.id = ANY(sqlc.arg('ids')::uuid[])
      AND runs.status = 'running'
      AND runs.pause_mode = 'reserve'
    RETURNING runs.id, runs.user_id, runs.status
),
cancel_input AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT id, 'pause_cancel', NULL::text FROM withdrawn
)
SELECT id, user_id, status FROM withdrawn;

-- name: WithdrawReservePausesIdle :many
-- Withdraw a pending RESERVE request on each listed non-terminal run that is not running and not
-- paused (queued, claimed, limit_wait, pool_wait, recovery_wait and the awaiting_* parks, where the
-- request survives the involuntary parks and would otherwise re-arm on the first running report).
-- Clears the three pause columns and settles the run's unapplied kind='pause' inputs (the
-- settled_inputs shape of SettleCredentialDisabledPause), so a resumed flight is never handed a
-- stale pause. A claimed or awaiting_* row may have a live worker that already drained the pause, so
-- those also get a kind='pause_cancel' input. Only where pause_mode = 'reserve'.
WITH withdrawn AS (
    UPDATE runs SET
        pause_requested_at = NULL,
        pause_mode         = NULL,
        pause_after_count  = NULL,
        updated_at         = now()
    WHERE runs.id = ANY(sqlc.arg('ids')::uuid[])
      AND runs.pause_mode = 'reserve'
      AND runs.status IN ('queued', 'claimed', 'limit_wait', 'pool_wait', 'recovery_wait',
                          'awaiting_input', 'awaiting_approval', 'awaiting_followup')
    RETURNING runs.id, runs.user_id, runs.status
),
settled_inputs AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    WHERE u.run_id IN (SELECT id FROM withdrawn) AND u.kind = 'pause' AND u.applied_at IS NULL
),
cancel_input AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT id, 'pause_cancel', NULL::text FROM withdrawn
    WHERE status IN ('claimed', 'awaiting_input', 'awaiting_approval', 'awaiting_followup')
)
SELECT id, user_id, status FROM withdrawn;

-- name: ClearReserveOverride :execrows
-- Drop an ended manual-resume exemption (all five columns) so the display and the claim gate agree.
-- Guarded on the grant time the caller READ, so a fresh grant that landed between the list and this
-- write is never cleared. updated_at is untouched (display state).
UPDATE runs SET
    reserve_exempt_until         = NULL,
    reserve_exempt_granted_at    = NULL,
    reserve_exempt_credential_id = NULL,
    reserve_exempt_window_reset  = NULL,
    reserve_exempt_prior_pct     = NULL
WHERE id = @id
  AND status NOT IN ('completed', 'failed', 'cancelled')
  AND reserve_exempt_until IS NOT NULL
  AND reserve_exempt_granted_at IS NOT DISTINCT FROM sqlc.narg('expected_granted_at')::timestamptz;
