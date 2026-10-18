-- name: ListWorkerUpgradeBlockers :many
-- Caller supplies only authorized response workers. Assignment, not heartbeat
-- snapshots, lanes or claim release, is the controller's busy authority.
WITH authorized AS (
    SELECT id, user_id FROM workers WHERE id = ANY(@worker_ids::uuid[])
), assigned AS MATERIALIZED (
    SELECT w.id AS worker_id, w.user_id AS worker_owner, r.user_id, r.id,
           r.kind, r.status, r.health_reason, r.hold_reason, r.limit_resets_at,
           r.retry_not_before, r.recovery_wait_cause, r.recovery_retry_not_before
    FROM authorized w JOIN runs r ON r.worker_id = w.id
    WHERE r.status NOT IN ('completed', 'failed', 'cancelled')
), mismatches AS (
    SELECT worker_id, bool_or(user_id <> worker_owner)::boolean AS owner_mismatch
    FROM assigned GROUP BY worker_id
), safe AS (
    SELECT worker_id, id AS run_id, kind, status, health_reason, hold_reason,
           limit_resets_at, retry_not_before, recovery_wait_cause, recovery_retry_not_before,
           count(*) OVER (PARTITION BY worker_id)::bigint AS safe_count,
           row_number() OVER (PARTITION BY worker_id ORDER BY id) AS position
    FROM assigned WHERE user_id = worker_owner
)
SELECT w.id AS worker_id, w.user_id AS worker_owner,
       COALESCE(m.owner_mismatch, false)::boolean AS owner_mismatch,
       COALESCE(s.safe_count, 0)::bigint AS safe_count,
       s.run_id, s.kind, s.status, s.health_reason, s.hold_reason,
       s.limit_resets_at, s.retry_not_before, s.recovery_wait_cause, s.recovery_retry_not_before
FROM authorized w
LEFT JOIN mismatches m ON m.worker_id = w.id
LEFT JOIN safe s ON s.worker_id = w.id AND s.position <= 5
ORDER BY w.id, s.run_id;
