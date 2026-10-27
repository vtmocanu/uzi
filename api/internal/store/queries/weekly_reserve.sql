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
