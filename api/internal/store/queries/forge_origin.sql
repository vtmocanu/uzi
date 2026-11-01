-- name: LockForgeOriginWorker :one
-- Match ClaimRun lock order: claimant worker, run, then origin. Run ownership is
-- checked again under the run lock; this first read grants no run authority.
WITH owner_guard AS MATERIALIZED (
    -- Parent deletion must wait before we hold worker/run rows and need FK protection.
    SELECT u.id FROM users u
    JOIN workers w ON w.user_id = u.id AND w.id = @worker_id::uuid
    JOIN runs r ON r.user_id = u.id AND r.worker_id = w.id
    WHERE r.id = @run_id::uuid
    FOR KEY SHARE OF u
)
SELECT w.id FROM workers w JOIN owner_guard u ON u.id = w.user_id
WHERE w.id = @worker_id::uuid
FOR UPDATE OF w;

-- name: ObserveParkedForgeOrigin :execrows
-- Caller holds the authenticated exact-generation run lock and has applied its
-- forge park in this transaction. M2 must use the same authorized projection,
-- locking claimant worker then run before any cooldown/reservation row.
-- Git-surface evidence: sets git_unverified, leaves the API flag alone. The reason
-- is a closed set the table CHECK enforces (transient_clone_fetch_exhausted for the
-- pre-clone park, transient_publication_exhausted for a publication park).
WITH mapping AS MATERIALIZED (
    SELECT m.user_id, m.worker_id, m.origin
    FROM run_forge_origins m
    JOIN runs r ON r.id = m.run_id AND r.user_id = m.user_id
        AND r.worker_id = m.worker_id AND r.claim_generation = m.generation
    JOIN workers w ON w.id = m.worker_id AND w.user_id = m.user_id
    JOIN authorized_forge_origins a
      ON a.repo_id = r.repo_id AND a.connection_id = m.connection_id
        AND a.user_id = m.user_id AND a.origin = m.origin
    WHERE r.id = sqlc.arg('run_id')::uuid AND r.worker_id = sqlc.arg('worker_id')::uuid
      AND r.claim_generation = sqlc.arg('generation')::bigint
      AND r.status = 'recovery_wait' AND r.recovery_wait_cause = 'forge_unreachable'
      AND r.claim_released_at IS NULL
    -- OF the view locks both its repo and connection rows. NOWAIT avoids
    -- inversion with deletion paths that already hold runs before mappings.
    -- A lock refusal aborts the whole park, including custody and counters.
    FOR SHARE OF a NOWAIT
)
INSERT INTO forge_origin_cooldowns
    (user_id, worker_id, origin, failure_step, cooled_until, last_reason, observed_at, git_unverified, api_unverified)
SELECT m.user_id, m.worker_id, m.origin, 0,
       clock_timestamp() + (sqlc.arg('base_ms')::float8 * interval '1 millisecond'),
       sqlc.arg('reason')::text, clock_timestamp(), true, false
FROM mapping m
ON CONFLICT (user_id, worker_id, origin) DO UPDATE SET
    failure_step = LEAST(forge_origin_cooldowns.failure_step + 1, 30),
    observation_version = forge_origin_cooldowns.observation_version + 1,
    cooled_until = clock_timestamp() +
        (LEAST(sqlc.arg('base_ms')::float8 * power(2::float8,
            LEAST(forge_origin_cooldowns.failure_step + 1, 30)),
            sqlc.arg('cap_ms')::float8) * interval '1 millisecond'),
    last_reason = sqlc.arg('reason')::text,
    git_unverified = true,
    observed_at = clock_timestamp();

-- name: ObserveParkedForgeApiOrigin :execrows
-- API-surface twin of ObserveParkedForgeOrigin (issue #1324 D9): same lock order, same
-- backoff and observation_version advance, keyed on the claim snapshot's server-derived
-- api_origin (never a worker-sent URL) and checked against the current authorized
-- projection. Sets api_unverified; an equal Git and API origin share one row whose Git
-- flag is untouched. A new row starts git-verified.
WITH mapping AS MATERIALIZED (
    SELECT m.user_id, m.worker_id, m.api_origin AS origin
    FROM run_forge_origins m
    JOIN runs r ON r.id = m.run_id AND r.user_id = m.user_id
        AND r.worker_id = m.worker_id AND r.claim_generation = m.generation
    JOIN workers w ON w.id = m.worker_id AND w.user_id = m.user_id
    JOIN authorized_forge_origins a
      ON a.repo_id = r.repo_id AND a.connection_id = m.connection_id
        AND a.user_id = m.user_id AND a.api_origin = m.api_origin
    WHERE r.id = sqlc.arg('run_id')::uuid AND r.worker_id = sqlc.arg('worker_id')::uuid
      AND r.claim_generation = sqlc.arg('generation')::bigint
      AND r.status = 'recovery_wait' AND r.recovery_wait_cause = 'forge_unreachable'
      AND r.claim_released_at IS NULL
      AND m.api_origin IS NOT NULL
    FOR SHARE OF a NOWAIT
)
INSERT INTO forge_origin_cooldowns
    (user_id, worker_id, origin, failure_step, cooled_until, last_reason, observed_at, git_unverified, api_unverified)
SELECT m.user_id, m.worker_id, m.origin, 0,
       clock_timestamp() + (sqlc.arg('base_ms')::float8 * interval '1 millisecond'),
       sqlc.arg('reason')::text, clock_timestamp(), false, true
FROM mapping m
ON CONFLICT (user_id, worker_id, origin) DO UPDATE SET
    failure_step = LEAST(forge_origin_cooldowns.failure_step + 1, 30),
    observation_version = forge_origin_cooldowns.observation_version + 1,
    cooled_until = clock_timestamp() +
        (LEAST(sqlc.arg('base_ms')::float8 * power(2::float8,
            LEAST(forge_origin_cooldowns.failure_step + 1, 30)),
            sqlc.arg('cap_ms')::float8) * interval '1 millisecond'),
    last_reason = sqlc.arg('reason')::text,
    api_unverified = true,
    observed_at = clock_timestamp();

-- name: GetForgeOriginCooldown :one
-- M2 read contract: never reserve from an unlocked run; compare incarnation AND
-- observation_version when binding a half-open probe. Expiry alone is not success.
SELECT c.* FROM forge_origin_cooldowns c
JOIN workers w ON w.id = c.worker_id AND w.user_id = c.user_id
WHERE c.user_id = @user_id::uuid AND c.worker_id = @worker_id::uuid AND c.origin = @origin::text;

-- name: LockForgeOriginCooldown :one
-- M2 contract: call only after locking claimant worker then exact-generation run.
-- CAS reservation against incarnation + observation_version; retain the reserved
-- observation version separately when later failures advance observation_version.
SELECT c.* FROM forge_origin_cooldowns c
JOIN workers w ON w.id = c.worker_id AND w.user_id = c.user_id
WHERE c.user_id = @user_id::uuid AND c.worker_id = @worker_id::uuid AND c.origin = @origin::text
FOR UPDATE OF c;

-- name: RunForgeOriginClaimRefused :one
-- Only after GetRunClaimContext returned no rows: an extant owner-scoped
-- claimed run was refused by its snapshot predicate. Do not re-evaluate the
-- mapping: a concurrent repair must not relabel that refusal as foreign ownership.
-- Grants no execution authority; assembly cleanup still fences the exact mint.
SELECT EXISTS (
    SELECT 1 FROM runs r
    JOIN repos rp ON rp.id = r.repo_id
    JOIN forge_connections c ON c.id = rp.connection_id AND c.user_id = r.user_id
    WHERE r.id = @run_id::uuid AND r.status = 'claimed'
)::boolean AS refused;

-- name: GetForgeOriginReservation :one
-- A token is advisory telemetry, never network or custody authority.
SELECT c.reservation_incarnation, c.reservation_version, c.reservation_observation_version
FROM run_forge_origins m
JOIN runs r ON r.id = m.run_id AND r.user_id = m.user_id
    AND r.worker_id = m.worker_id AND r.claim_generation = m.generation
JOIN authorized_forge_origins a ON a.repo_id = r.repo_id AND a.user_id = m.user_id
    AND a.connection_id = m.connection_id AND a.origin = m.origin
JOIN forge_origin_cooldowns c ON c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
WHERE m.run_id = @run_id::uuid AND m.worker_id = @worker_id::uuid AND m.generation = @generation::bigint
    AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
    AND c.incarnation = m.reservation_incarnation AND c.reservation_incarnation = m.reservation_incarnation
    AND c.reservation_version = m.reservation_version
    AND c.observation_version = c.reservation_observation_version;

-- name: GetForgeApiOriginReservation :one
-- API twin of GetForgeOriginReservation: the reservation captured on the claim for the
-- server-derived api_origin. Advisory telemetry, never network or custody authority.
SELECT c.reservation_incarnation, c.reservation_version, c.reservation_observation_version
FROM run_forge_origins m
JOIN runs r ON r.id = m.run_id AND r.user_id = m.user_id
    AND r.worker_id = m.worker_id AND r.claim_generation = m.generation
JOIN authorized_forge_origins a ON a.repo_id = r.repo_id AND a.user_id = m.user_id
    AND a.connection_id = m.connection_id AND a.api_origin = m.api_origin
JOIN forge_origin_cooldowns c ON c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.api_origin
WHERE m.run_id = @run_id::uuid AND m.worker_id = @worker_id::uuid AND m.generation = @generation::bigint
    AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
    AND c.incarnation = m.api_reservation_incarnation AND c.reservation_incarnation = m.api_reservation_incarnation
    AND c.reservation_version = m.api_reservation_version
    AND c.observation_version = c.reservation_observation_version;

-- name: ClearForgeOriginOnCloneSuccess :one
-- Git-surface success (issue #1324 D9). Caller locks authenticated worker, then exact run.
-- Lock the current authorized mapping without waiting on repo deletion; a refusal rolls
-- back this one attempt. Clears only git_unverified; the row is deleted only when no API
-- failure remains on it. The two branches act on disjoint rows of one snapshot.
WITH mapping AS MATERIALIZED (
    SELECT m.* FROM run_forge_origins m
    JOIN runs r ON r.id = m.run_id AND r.user_id = m.user_id
        AND r.worker_id = m.worker_id AND r.claim_generation = m.generation
    JOIN authorized_forge_origins a ON a.repo_id = r.repo_id AND a.user_id = m.user_id
        AND a.connection_id = m.connection_id AND a.origin = m.origin
    WHERE m.run_id = @run_id::uuid AND m.worker_id = @worker_id::uuid AND m.generation = @generation::bigint
        AND r.status IN ('claimed', 'running') AND r.claim_released_at IS NULL AND r.stop_kind IS NULL
        AND m.reservation_incarnation = @incarnation::uuid AND m.reservation_version = @version::bigint
    FOR SHARE OF a NOWAIT
), removed AS (
    DELETE FROM forge_origin_cooldowns c USING mapping m
    WHERE c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
        AND c.incarnation = @incarnation::uuid AND c.reservation_incarnation = @incarnation::uuid
        AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
        AND c.reservation_version = @version::bigint
        AND c.reservation_observation_version = @observation_version::bigint
        AND c.observation_version = @observation_version::bigint
        AND c.git_unverified AND NOT c.api_unverified
    RETURNING 1
), cleared AS (
    UPDATE forge_origin_cooldowns c SET git_unverified = false
    FROM mapping m
    WHERE c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
        AND c.incarnation = @incarnation::uuid AND c.reservation_incarnation = @incarnation::uuid
        AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
        AND c.reservation_version = @version::bigint
        AND c.reservation_observation_version = @observation_version::bigint
        AND c.observation_version = @observation_version::bigint
        AND c.git_unverified AND c.api_unverified
    RETURNING 1
)
SELECT ((SELECT count(*) FROM removed) + (SELECT count(*) FROM cleared))::bigint AS applied;

-- name: ClearForgeApiOriginOnSuccess :one
-- API-surface success (issue #1324 D9). Same locking as ClearForgeOriginOnCloneSuccess.
-- Applies only to the half-open reservation captured on THIS run's claim for the
-- server-derived api_origin (exact run, generation, incarnation, reservation version and
-- unchanged observation_version); a Git success, completion, broker success, another
-- origin's row or a stale tuple never reaches it. Clears only api_unverified; deletes the
-- row only when no Git failure remains on it.
WITH mapping AS MATERIALIZED (
    SELECT m.* FROM run_forge_origins m
    JOIN runs r ON r.id = m.run_id AND r.user_id = m.user_id
        AND r.worker_id = m.worker_id AND r.claim_generation = m.generation
    JOIN authorized_forge_origins a ON a.repo_id = r.repo_id AND a.user_id = m.user_id
        AND a.connection_id = m.connection_id AND a.api_origin = m.api_origin
    WHERE m.run_id = @run_id::uuid AND m.worker_id = @worker_id::uuid AND m.generation = @generation::bigint
        AND r.status IN ('claimed', 'running') AND r.claim_released_at IS NULL AND r.stop_kind IS NULL
        AND m.api_reservation_incarnation = @incarnation::uuid AND m.api_reservation_version = @version::bigint
    FOR SHARE OF a NOWAIT
), removed AS (
    DELETE FROM forge_origin_cooldowns c USING mapping m
    WHERE c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.api_origin
        AND c.incarnation = @incarnation::uuid AND c.reservation_incarnation = @incarnation::uuid
        AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
        AND c.reservation_version = @version::bigint
        AND c.reservation_observation_version = @observation_version::bigint
        AND c.observation_version = @observation_version::bigint
        AND c.api_unverified AND NOT c.git_unverified
    RETURNING 1
), cleared AS (
    UPDATE forge_origin_cooldowns c SET api_unverified = false
    FROM mapping m
    WHERE c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.api_origin
        AND c.incarnation = @incarnation::uuid AND c.reservation_incarnation = @incarnation::uuid
        AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
        AND c.reservation_version = @version::bigint
        AND c.reservation_observation_version = @observation_version::bigint
        AND c.observation_version = @observation_version::bigint
        AND c.api_unverified AND c.git_unverified
    RETURNING 1
)
SELECT ((SELECT count(*) FROM removed) + (SELECT count(*) FROM cleared))::bigint AS applied;

-- name: CanonicalForgeOrigin :one
-- Empty means unknown and is never a cooldown key (the table rejects it).
SELECT COALESCE(canonical_forge_origin(@raw::text), '')::text AS origin;

-- name: ListOwnerWorkerForgeOriginCooldowns :many
-- Display only, no locks or reservations. One batch for response worker IDs;
-- at most 32 origins per worker and 1024 total, soonest deadline then canonical
-- origin. A truncated batch omits advisory fields rather than claiming health.
SELECT w.id AS worker_id, c.origin, c.cooled_until, c.git_unverified, c.api_unverified,
       ('forge_clone_success_v1' = ANY(w.protocol_capabilities))::boolean AS success_reporting,
       (c.reservation_run_id IS NOT NULL)::boolean AS reserved,
       COALESCE((SELECT probe.id::text FROM runs probe
        JOIN run_forge_origins m ON m.run_id = probe.id AND m.user_id = probe.user_id
          AND m.worker_id = probe.worker_id AND m.generation = probe.claim_generation
        JOIN authorized_forge_origins a ON a.repo_id = probe.repo_id AND a.user_id = m.user_id
          AND a.connection_id = m.connection_id AND a.origin = m.origin
        WHERE probe.id = c.reservation_run_id AND probe.user_id = c.user_id
          AND probe.worker_id = c.worker_id AND probe.claim_generation = c.reservation_generation
          AND probe.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup')
          AND probe.claim_released_at IS NULL AND probe.stop_kind IS NULL
          AND c.reservation_incarnation = c.incarnation
          AND ((m.origin = c.origin AND m.reservation_incarnation = c.incarnation
                AND m.reservation_version = c.reservation_version)
            OR (m.api_origin = c.origin AND m.api_reservation_incarnation = c.incarnation
                AND m.api_reservation_version = c.reservation_version))
       ), '')::text AS half_open_run_id
FROM workers w
CROSS JOIN LATERAL (
    SELECT fc.* FROM forge_origin_cooldowns fc
    WHERE fc.user_id = w.user_id AND fc.worker_id = w.id
      AND EXISTS (SELECT 1 FROM authorized_forge_origins a
                  WHERE a.user_id = fc.user_id AND (a.origin = fc.origin OR a.api_origin = fc.origin))
    ORDER BY fc.cooled_until, fc.origin LIMIT 32
) c
WHERE w.user_id = @user_id::uuid AND w.id = ANY(@worker_ids::uuid[])
ORDER BY w.id, c.cooled_until, c.origin
LIMIT 1024;

-- name: ListOwnerRunForgeOriginCooldowns :many
-- At most two rows per requested owner run (the Git and the API origin), the later
-- deadline first. Current authorized identity only;
-- repo-less, isolated and chat runs do not use this direct-forge cooldown lane.
SELECT r.id AS run_id, c.origin, c.cooled_until, c.git_unverified, c.api_unverified,
       ('forge_clone_success_v1' = ANY(w.protocol_capabilities))::boolean AS success_reporting,
       (c.reservation_run_id IS NOT NULL)::boolean AS reserved,
       COALESCE((SELECT probe.id::text FROM runs probe
        JOIN run_forge_origins m ON m.run_id = probe.id AND m.user_id = probe.user_id
          AND m.worker_id = probe.worker_id AND m.generation = probe.claim_generation
        JOIN authorized_forge_origins a ON a.repo_id = probe.repo_id AND a.user_id = m.user_id
          AND a.connection_id = m.connection_id AND a.origin = m.origin
        WHERE probe.id = c.reservation_run_id AND probe.user_id = c.user_id
          AND probe.worker_id = c.worker_id AND probe.claim_generation = c.reservation_generation
          AND probe.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup')
          AND probe.claim_released_at IS NULL AND probe.stop_kind IS NULL
          AND c.reservation_incarnation = c.incarnation
          AND ((m.origin = c.origin AND m.reservation_incarnation = c.incarnation
                AND m.reservation_version = c.reservation_version)
            OR (m.api_origin = c.origin AND m.api_reservation_incarnation = c.incarnation
                AND m.api_reservation_version = c.reservation_version))
       ), '')::text AS half_open_run_id
FROM runs r
JOIN workers w ON w.id = r.worker_id AND w.user_id = r.user_id
JOIN authorized_forge_origins current_origin ON current_origin.repo_id = r.repo_id AND current_origin.user_id = r.user_id
JOIN forge_origin_cooldowns c ON c.user_id = r.user_id AND c.worker_id = w.id
  AND c.origin IN (current_origin.origin, current_origin.api_origin)
WHERE r.user_id = @user_id::uuid AND r.id = ANY(@run_ids::uuid[])
  AND r.kind <> 'chat' AND r.egress_profile_id IS NULL
  AND r.status NOT IN ('completed','failed','cancelled')
ORDER BY r.id, c.cooled_until DESC, c.origin;
