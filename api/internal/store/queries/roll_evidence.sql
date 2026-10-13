-- name: StampWorkerRollHeartbeat :one
-- Caller holds the worker row lock and has applied a fully validated heartbeat snapshot.
UPDATE workers SET roll_heartbeat_at = now()
WHERE id = @id AND kind = 'hosted' AND registered_pod_uid IS NOT NULL
  AND snapshot_register_nonce = @register_nonce
  AND snapshot_epoch = @snapshot_epoch
RETURNING roll_heartbeat_at;

-- name: CordonHostedWorkerImageRoll :execrows
-- Take ownership only of a new drain. Existing legacy/maintenance drains keep their owner.
UPDATE workers
SET draining_reason = CASE WHEN draining_since IS NULL AND NOT maintenance_fenced
                            AND maintenance_phase NOT IN ('requested','ready','stopping','recycling')
                          THEN 'image_roll' ELSE draining_reason END,
    draining_since = COALESCE(draining_since, now()),
    lease_since = NULL, lease_repo_id = NULL, lease_branch = NULL,
    updated_at = now()
WHERE workers.id = @id AND kind = 'hosted';

-- name: UncordonHostedWorkerImageRoll :execrows
-- Release only this reason; custody and active maintenance retain their independent fence.
UPDATE workers w
SET draining_since = CASE WHEN draining_reason = 'image_roll'
                           AND stats_disk_pressure_streak < 2 AND dind_pressure_streak < 2
                           AND NOT maintenance_fenced
                           AND maintenance_phase NOT IN ('requested','ready','stopping','recycling')
                           AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.live_worker_id = w.id AND h.state = 'open')
                         THEN NULL ELSE draining_since END,
    draining_reason = CASE WHEN draining_reason = 'image_roll'
                           AND stats_disk_pressure_streak < 2 AND dind_pressure_streak < 2
                           AND NOT maintenance_fenced
                           AND maintenance_phase NOT IN ('requested','ready','stopping','recycling')
                           AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.live_worker_id = w.id AND h.state = 'open')
                         THEN NULL ELSE draining_reason END,
    updated_at = now()
WHERE w.id = @id AND kind = 'hosted';

-- name: CordonHostedWorkerPressureRecycle :execrows
-- Take ownership only of a new drain. Existing legacy/maintenance drains keep their owner.
UPDATE workers
SET draining_reason = CASE WHEN draining_since IS NULL AND NOT maintenance_fenced AND NOT maintenance_owns_drain
                            AND maintenance_phase NOT IN ('requested','ready','stopping','recycling')
                          THEN 'pressure_recycle' ELSE draining_reason END,
    draining_since = COALESCE(draining_since, now()),
    lease_since = NULL, lease_repo_id = NULL, lease_branch = NULL,
    updated_at = now()
WHERE workers.id = @id AND kind = 'hosted';

-- name: UncordonHostedWorkerPressureRecycle :execrows
-- Caller acquired GetWorkerForUpdate in a prior statement of a READ COMMITTED transaction.
-- Release only this reason; custody and active maintenance retain their independent fence.
UPDATE workers w
SET draining_since = CASE WHEN draining_reason = 'pressure_recycle'
                           AND stats_disk_pressure_streak < 2 AND dind_pressure_streak < 2
                           AND NOT maintenance_fenced AND NOT maintenance_owns_drain
                           AND maintenance_phase NOT IN ('requested','ready','stopping','recycling')
                           AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.live_worker_id = w.id AND h.state = 'open')
                         THEN NULL ELSE draining_since END,
    draining_reason = CASE WHEN draining_reason = 'pressure_recycle'
                           AND stats_disk_pressure_streak < 2 AND dind_pressure_streak < 2
                           AND NOT maintenance_fenced AND NOT maintenance_owns_drain
                           AND maintenance_phase NOT IN ('requested','ready','stopping','recycling')
                           AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.live_worker_id = w.id AND h.state = 'open')
                         THEN NULL ELSE draining_reason END,
    updated_at = now()
WHERE w.id = @id AND kind = 'hosted';
