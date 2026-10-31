-- Write-ahead publication intent (issue #1324). The side table is guarded by
-- guard_publication_resume_row: enrollment, candidate updates and retirement here require the
-- API opt-in below, set in the same transaction as the writer. A run reaching a terminal status
-- also closes its intent through close_publication_resume_trg (00386), without the opt-in.

-- name: OptInPublicationResume :exec
SELECT set_config('uzi.publication_resume_api', 'v1', true);

-- name: GetActivePublicationResume :one
SELECT * FROM run_publication_resume p WHERE p.run_id = @run_id AND p.state = 'active';

-- name: GetPublicationResume :one
SELECT * FROM run_publication_resume p WHERE p.run_id = @run_id AND p.enrollment_seq = @enrollment_seq;

-- name: HasInactivePublicationResumeAtGeneration :one
-- True when a retired or completed enrollment of the run exists at or above the generation. A
-- fresh enrollment at such a generation would resurrect a closed one (no resurrection).
SELECT EXISTS (
    SELECT 1 FROM run_publication_resume p
    WHERE p.run_id = @run_id AND p.state <> 'active' AND p.claim_generation >= @claim_generation::bigint
)::boolean AS retired;

-- name: EnrollPublicationResume :one
-- The caller holds the run row lock, so MAX+1 cannot race; the partial unique index backs it.
INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, worker_id, intent, state)
SELECT @run_id::uuid, COALESCE(MAX(p.enrollment_seq), 0) + 1, @claim_generation::bigint, @worker_id::uuid, @intent::json, 'active'
FROM run_publication_resume p WHERE p.run_id = @run_id::uuid
RETURNING *;

-- name: UpdatePublicationCandidate :one
-- Compare-and-swap on candidate_seq; zero rows means a stale seq or a non-active enrollment.
UPDATE run_publication_resume SET intent = @intent::json, candidate_seq = @new_candidate_seq::int
WHERE run_id = @run_id AND enrollment_seq = @enrollment_seq AND state = 'active'
  AND candidate_seq = @expected_candidate_seq::int
RETURNING *;

-- name: RetirePublicationResume :one
UPDATE run_publication_resume SET state = 'retired', retired_reason = @reason::text
WHERE run_id = @run_id AND state = 'active'
RETURNING *;

-- name: CompletePublicationResume :one
UPDATE run_publication_resume SET state = 'completed'
WHERE run_id = @run_id AND state = 'active'
RETURNING *;

-- name: ParkRunPublicationForgeUnreachable :one
-- The publication-phase forge park (issue #1324 D8). Unlike ParkRunForgeUnreachable it never
-- touches forge_park_count, so the pre-clone forge cap neither ends nor changes it. A NULL
-- retry_not_before is the owner hold (the elapsed bound was reached); otherwise it is a timed
-- retry. Claim, worker, custody and session are kept. Run inside the park transaction after the
-- worker and run locks, with the generation and active intent verified in Go.
UPDATE runs SET
    status                    = 'recovery_wait',
    status_since              = now(),
    recovery_wait_count       = recovery_wait_count + 1,
    recovery_wait_cause       = 'forge_unreachable',
    recovery_wait_phase       = 'publication',
    recovery_retry_not_before = sqlc.narg('retry_not_before')::timestamptz,
    session_id                = COALESCE(sqlc.narg('session_id'), session_id),
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at                = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'running' AND claim_generation = @claim_generation::bigint
  AND claim_released_at IS NULL AND COALESCE(stop_kind, '') = ''
  AND kind NOT IN ('judge', 'job', 'cross_check')
RETURNING *;

-- name: MarkPublicationParked :one
-- The side-row half of a publication park: the wait clock starts at the first park and is never
-- reset by a later one, the lifetime counters grow, and the hold reason is set only on an owner
-- hold (NULL for a timed retry). Each park sets the handoff to the newly verified value or NULL
-- (never keeps an older one, which could let a peer claim from a stale tip), and the custodian to
-- the reporting worker, which holds the captured source. @now comes from Go so the clock is
-- deterministic.
UPDATE run_publication_resume SET
    wait_started_at = COALESCE(wait_started_at, @now::timestamptz),
    first_wait_at   = COALESCE(first_wait_at, @now::timestamptz),
    park_total      = park_total + 1,
    hold_reason     = sqlc.narg('hold_reason')::text,
    handoff         = sqlc.narg('handoff')::json,
    worker_id       = @worker_id::uuid
WHERE run_id = @run_id AND state = 'active'
RETURNING *;

-- name: GetRunForgeOriginCooldownUntil :one
-- The per-origin cooldown (issue #3585) of the origin stamped on the run's current claim. Read
-- only: a publication park never writes the cooldown. No row means no cooldown applies.
SELECT c.cooled_until FROM run_forge_origins m
JOIN forge_origin_cooldowns c ON c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
WHERE m.run_id = @run_id::uuid AND m.worker_id = @worker_id::uuid;

-- name: ResumePublicationForgeHold :one
-- Owner Resume of a publication-phase owner hold (issue #1324): one ordinary queued attempt with
-- the intent, custody and source kept. Same grants as ResumeForgeUnreachableHold (none) and the
-- same budget guard; it matches only a NULL-stamp hold of the publication phase that still has an
-- active intent. The phase column is cleared by normalize_recovery_wait_phase on leaving the park.
UPDATE runs SET
    status = 'queued', status_since = now(),
    recovery_wait_cause = NULL, recovery_retry_not_before = NULL,
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN runs.started_at IS NOT NULL
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id AND status = 'recovery_wait'
  AND recovery_wait_cause = 'forge_unreachable' AND recovery_wait_phase = 'publication'
  AND recovery_retry_not_before IS NULL
  AND COALESCE(stop_kind, '') = ''
  AND EXISTS (SELECT 1 FROM run_publication_resume p WHERE p.run_id = runs.id AND p.state = 'active')
  AND (runs.kind IN ('chat', 'judge') OR runs.interactive OR runs.started_at IS NULL
       OR (COALESCE(runs.budget_wall_seconds, @global_timeout_seconds::int) + runs.budget_extension_seconds + runs.budget_finalize_seconds)
          - (GREATEST(0, EXTRACT(EPOCH FROM (runs.status_since - runs.started_at))::int) - runs.budget_paused_seconds) > 0)
RETURNING *;

-- name: AdvancePublicationEpisode :one
-- A new owner episode: the wait clock restarts at the next park. The intent, lifetime
-- diagnostics (first_wait_at, park_total) and handoff are kept.
UPDATE run_publication_resume SET episode = episode + 1, wait_started_at = NULL, hold_reason = NULL
WHERE run_id = @run_id AND state = 'active'
RETURNING *;

-- name: HoldPublicationRunsWithoutCustodian :many
-- Issue #1324 D5. A queued run with an active intent and no handoff is claimable only by its
-- custodian worker (ClaimRun). When that worker cannot serve it, hold the run for its owner
-- instead of leaving it queued forever: the worker row is gone, lacks the capability, has a stale
-- heartbeat while neither draining nor in maintenance nor inside the stale-requeue grace, or has
-- been draining or in maintenance beyond the affinity ceiling. The liveness arms mirror ClaimRun's
-- owner-pin test; an image roll (draining inside the ceiling) never holds. Run rows are taken
-- SKIP LOCKED and the worker rows are read unlocked.
WITH candidates AS MATERIALIZED (
    SELECT r.id FROM runs r
    JOIN run_publication_resume p ON p.run_id = r.id AND p.state = 'active' AND p.handoff IS NULL
    WHERE r.status = 'queued'
      AND (
          -- The custodian is the side row's worker_id (runs.worker_id is cleared by the resume
          -- writers); a NULL one is held.
          p.worker_id IS NULL
          OR NOT EXISTS (
              SELECT 1 FROM workers w
              WHERE w.id = p.worker_id AND w.user_id = r.user_id
                AND 'publication_resume_v1' = ANY(w.protocol_capabilities)
                AND ((w.last_heartbeat_at IS NOT NULL AND w.last_heartbeat_at >= @heartbeat_cutoff::timestamptz)
                     OR (r.stale_requeue_generation = r.claim_generation
                         AND COALESCE(r.status_since > sqlc.narg('stale_requeue_cutoff')::timestamptz, false))
                     OR ((w.draining_since IS NOT NULL OR w.maintenance_phase IN ('requested','ready','stopping','recycling') OR w.maintenance_fenced)
                         AND r.updated_at >= @affinity_cutoff::timestamptz)))
          -- The affinity ceiling is its own conjunct: a draining or fenced custodian past it is
          -- held whatever its heartbeat says, because ClaimRun refuses a fenced claimant.
          OR EXISTS (
              SELECT 1 FROM workers w
              WHERE w.id = p.worker_id AND w.user_id = r.user_id
                AND (w.draining_since IS NOT NULL OR w.maintenance_phase IN ('requested','ready','stopping','recycling') OR w.maintenance_fenced)
                AND r.updated_at < @affinity_cutoff::timestamptz))
    ORDER BY r.id
    FOR UPDATE OF r SKIP LOCKED
), held AS (
    UPDATE runs SET
        status = 'recovery_wait', status_since = now(),
        recovery_wait_cause = 'forge_unreachable', recovery_wait_phase = 'publication',
        recovery_retry_not_before = NULL,
        health = 'ok', health_reason = NULL, health_since = NULL,
        updated_at = now()
    FROM candidates c
    WHERE runs.id = c.id AND runs.status = 'queued'
    RETURNING runs.id, runs.user_id
), marked AS (
    UPDATE run_publication_resume p SET hold_reason = 'publication_custodian_unavailable'
    FROM held WHERE p.run_id = held.id AND p.state = 'active'
    RETURNING p.run_id
)
SELECT held.id, held.user_id FROM held JOIN marked ON marked.run_id = held.id;

-- name: ListOwnerRunPublicationIntents :many
-- Display only (issue #1324 D10): the active intent of each requested owner run, projected to the
-- fields the run DTO shows. No locks; the intent body is never returned whole.
SELECT p.run_id, p.enrollment_seq, p.state, p.wait_started_at, p.first_wait_at, p.park_total,
       p.episode, p.hold_reason,
       COALESCE(p.intent->>'branch', '')::text AS branch,
       COALESCE(p.intent->>'original_head', '')::text AS original_head,
       COALESCE(p.intent->>'candidate', '')::text AS candidate
FROM run_publication_resume p
JOIN runs r ON r.id = p.run_id
WHERE r.user_id = @user_id::uuid AND p.run_id = ANY(@run_ids::uuid[]) AND p.state = 'active';

-- name: OptInRunFences :exec
-- Both transaction-local opt-ins in one round trip: checkpoint coordinates and publication resume.
SELECT set_config('uzi.task_checkpoint_coordinates_api', 'v1', true),
       set_config('uzi.publication_resume_api', 'v1', true);
