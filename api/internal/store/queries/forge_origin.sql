-- name: LockForgeOriginWorker :one
-- Match ClaimRun lock order: claimant worker, run, then origin. Run ownership is
-- checked again under the run lock; this first read grants no run authority.
SELECT w.id FROM workers w
WHERE w.id = @worker_id::uuid AND EXISTS (
    SELECT 1 FROM runs r WHERE r.id = @run_id::uuid AND r.worker_id = w.id AND r.user_id = w.user_id)
FOR UPDATE;

-- name: ObserveParkedForgeOrigin :execrows
-- Caller holds the authenticated exact-generation run lock and has applied its
-- forge park in this transaction. M2 must use the same authorized projection,
-- locking claimant worker then run before any cooldown/reservation row.
INSERT INTO forge_origin_cooldowns
    (user_id, worker_id, origin, failure_step, cooled_until, last_reason, observed_at)
SELECT m.user_id, m.worker_id, m.origin, 0,
       clock_timestamp() + (sqlc.arg('base_ms')::float8 * interval '1 millisecond'),
       'transient_clone_fetch_exhausted', clock_timestamp()
FROM run_forge_origins m
JOIN runs r ON r.id = m.run_id AND r.user_id = m.user_id
    AND r.worker_id = m.worker_id AND r.claim_generation = m.generation
JOIN workers w ON w.id = m.worker_id AND w.user_id = m.user_id
WHERE r.id = sqlc.arg('run_id')::uuid AND r.worker_id = sqlc.arg('worker_id')::uuid
  AND r.claim_generation = sqlc.arg('generation')::bigint
  AND r.status = 'recovery_wait' AND r.recovery_wait_cause = 'forge_unreachable'
  AND r.claim_released_at IS NULL
  AND EXISTS (SELECT 1 FROM authorized_forge_origins a
              WHERE a.repo_id = r.repo_id AND a.connection_id = m.connection_id
                AND a.user_id = m.user_id AND a.origin = m.origin)
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

-- name: CanonicalForgeOrigin :one
-- Empty means unknown and is never a cooldown key (the table rejects it).
SELECT COALESCE(canonical_forge_origin(@raw::text), '')::text AS origin;
