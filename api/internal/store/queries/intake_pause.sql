-- PRD #3454: instance-wide intake pause ("maintenance mode"). The claim SQL reads the row
-- through fn_instance_intake_paused(); these queries serve routes, readouts and transitions.

-- name: GetInstanceIntakePause :one
SELECT * FROM instance_intake_pause WHERE id;

-- name: LockInstanceIntakePause :one
-- Serializes concurrent transitions; the claim path never takes this lock.
SELECT * FROM instance_intake_pause WHERE id FOR UPDATE;

-- name: UpdateInstanceIntakePause :one
UPDATE instance_intake_pause SET
    active     = @active::boolean,
    since      = sqlc.narg('since')::timestamptz,
    by_user_id = sqlc.narg('by_user_id')::uuid,
    by_label   = sqlc.narg('by_label')::text,
    reason     = sqlc.narg('reason')::text
WHERE id
RETURNING *;

-- name: InsertInstanceIntakePauseEvent :exec
INSERT INTO instance_intake_pause_events (by_user_id, by_label, action, reason)
VALUES (sqlc.narg('by_user_id')::uuid, sqlc.narg('by_label')::text, @action::text, sqlc.narg('reason')::text);

-- name: ListInstanceIntakePauseEvents :many
SELECT * FROM instance_intake_pause_events ORDER BY at DESC, id DESC LIMIT @row_limit::int;
