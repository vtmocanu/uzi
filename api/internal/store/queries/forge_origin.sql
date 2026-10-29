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
    (user_id, worker_id, origin, failure_step, cooled_until, last_reason, observed_at)
SELECT m.user_id, m.worker_id, m.origin, 0,
       clock_timestamp() + (sqlc.arg('base_ms')::float8 * interval '1 millisecond'),
       'transient_clone_fetch_exhausted', clock_timestamp()
FROM mapping m
ON CONFLICT (user_id, worker_id, origin) DO UPDATE SET
    failure_step = LEAST(forge_origin_cooldowns.failure_step + 1, 30),
    observation_version = forge_origin_cooldowns.observation_version + 1,
    cooled_until = clock_timestamp() +
        (LEAST(sqlc.arg('base_ms')::float8 * power(2::float8,
            LEAST(forge_origin_cooldowns.failure_step + 1, 30)),
            sqlc.arg('cap_ms')::float8) * interval '1 millisecond'),
    last_reason = 'transient_clone_fetch_exhausted',
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

-- name: ClearForgeOriginOnCloneSuccess :execrows
-- Caller locks authenticated worker, then exact run. Lock the current authorized
-- mapping without waiting on repo deletion; a refusal rolls back this one attempt.
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
)
DELETE FROM forge_origin_cooldowns c USING mapping m
WHERE c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
    AND c.incarnation = @incarnation::uuid AND c.reservation_incarnation = @incarnation::uuid
    AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
    AND c.reservation_version = @version::bigint
    AND c.reservation_observation_version = @observation_version::bigint
    AND c.observation_version = @observation_version::bigint;

-- name: CanonicalForgeOrigin :one
-- Empty means unknown and is never a cooldown key (the table rejects it).
SELECT COALESCE(canonical_forge_origin(@raw::text), '')::text AS origin;

-- name: ListOwnerWorkerForgeOriginCooldowns :many
-- Display only, no locks or reservations. One batch for response worker IDs;
-- at most 32 origins per worker and 1024 total, soonest deadline then canonical
-- origin. A truncated batch omits advisory fields rather than claiming health.
SELECT w.id AS worker_id, c.origin, c.cooled_until,
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
          AND m.origin = c.origin AND m.reservation_incarnation = c.incarnation
          AND c.reservation_incarnation = c.incarnation AND m.reservation_version = c.reservation_version
       ), '')::text AS half_open_run_id
FROM workers w
CROSS JOIN LATERAL (
    SELECT fc.* FROM forge_origin_cooldowns fc
    WHERE fc.user_id = w.user_id AND fc.worker_id = w.id
      AND EXISTS (SELECT 1 FROM authorized_forge_origins a
                  WHERE a.user_id = fc.user_id AND a.origin = fc.origin)
    ORDER BY fc.cooled_until, fc.origin LIMIT 32
) c
WHERE w.user_id = @user_id::uuid AND w.id = ANY(@worker_ids::uuid[])
ORDER BY w.id, c.cooled_until, c.origin
LIMIT 1024;

-- name: ListOwnerRunForgeOriginCooldowns :many
-- At most one row per requested owner run. Current authorized identity only;
-- repo-less, isolated and chat runs do not use this direct-forge cooldown lane.
SELECT r.id AS run_id, c.origin, c.cooled_until,
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
          AND m.origin = c.origin AND m.reservation_incarnation = c.incarnation
          AND c.reservation_incarnation = c.incarnation AND m.reservation_version = c.reservation_version
       ), '')::text AS half_open_run_id
FROM runs r
JOIN workers w ON w.id = r.worker_id AND w.user_id = r.user_id
JOIN authorized_forge_origins current_origin ON current_origin.repo_id = r.repo_id AND current_origin.user_id = r.user_id
JOIN forge_origin_cooldowns c ON c.user_id = r.user_id AND c.worker_id = w.id AND c.origin = current_origin.origin
WHERE r.user_id = @user_id::uuid AND r.id = ANY(@run_ids::uuid[])
  AND r.kind <> 'chat' AND r.egress_profile_id IS NULL
  AND r.status NOT IN ('completed','failed','cancelled')
ORDER BY r.id;
