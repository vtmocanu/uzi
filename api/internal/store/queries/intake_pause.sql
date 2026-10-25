-- PRD #3454: instance-wide intake pause ("maintenance mode"). The claim SQL reads the row
-- through fn_instance_intake_paused(); these queries serve routes, readouts and transitions.

-- name: GetInstanceIntakePause :one
SELECT * FROM instance_intake_pause WHERE id;

-- name: LockInstanceIntakePause :one
-- Serializes concurrent transitions; the claim path never takes this lock.
SELECT * FROM instance_intake_pause WHERE id FOR UPDATE;

-- name: EnsureInstanceIntakePauseRow :exec
-- Re-seeds a deleted singleton (inactive) so a transition can proceed and lift the
-- fail-closed pause a missing row implies.
INSERT INTO instance_intake_pause (id) VALUES (true) ON CONFLICT (id) DO NOTHING;

-- name: UpdateInstanceIntakePause :one
-- Right-hand column references are the row's pre-update values. since is stamped here
-- (transaction now()) only on off to on, kept on a reason-only edit, cleared on off.
-- keep_attribution leaves by_user_id/by_label untouched (a reason-only edit).
UPDATE instance_intake_pause SET
    active     = @active::boolean,
    since      = CASE WHEN NOT @active::boolean THEN NULL
                      WHEN NOT active THEN now()
                      ELSE since END,
    by_user_id = CASE WHEN @keep_attribution::boolean THEN by_user_id ELSE sqlc.narg('by_user_id')::uuid END,
    by_label   = CASE WHEN @keep_attribution::boolean THEN by_label ELSE sqlc.narg('by_label')::text END,
    reason     = sqlc.narg('reason')::text
WHERE id
RETURNING *;

-- name: InsertInstanceIntakePauseEvent :exec
INSERT INTO instance_intake_pause_events (by_user_id, by_label, action, reason)
VALUES (sqlc.narg('by_user_id')::uuid, sqlc.narg('by_label')::text, @action::text, sqlc.narg('reason')::text);

-- name: ListInstanceIntakePauseEvents :many
SELECT * FROM instance_intake_pause_events ORDER BY at DESC, id DESC LIMIT @row_limit::int;

-- name: RunAdmittableForEphemeralProvision :one
-- PRD #3454: the ephemeral provisioner's per-candidate recheck, run inside provisionOne's
-- transaction right after the advisory lock. Plain snapshot read: NO row lock (a FOR UPDATE /
-- FOR SHARE here deadlocks against the lease path, see workersvc/ephemeral_lease.go). false means
-- the pause is on and the run is not admitted; no row means the run is gone.
SELECT (NOT fn_instance_intake_paused() OR fn_run_admitted_during_intake_pause(r))::boolean AS admittable
FROM runs r
WHERE r.id = @run_id;
