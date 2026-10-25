-- name: GetWorkerForOwnerForUpdate :one
SELECT * FROM workers WHERE id = @worker_id AND user_id = @owner_id FOR UPDATE;

-- name: CordonWorkerForOwner :one
UPDATE workers SET manual_cordoned_at = clock_timestamp(),
 manual_cordoned_by = @actor_id::uuid, manual_cordoned_by_label = @actor_label::text,
 manual_cordon_reason = sqlc.narg('reason')::text, updated_at = now()
WHERE id = @worker_id AND user_id = @owner_id AND NOT ephemeral
 AND kind IN ('hosted','external') AND manual_cordoned_at IS NULL
RETURNING *;

-- name: UncordonWorkerForOwner :one
UPDATE workers SET manual_cordoned_at = NULL, manual_cordoned_by = NULL,
 manual_cordoned_by_label = NULL, manual_cordon_reason = NULL, updated_at = now()
WHERE id = @worker_id AND user_id = @owner_id AND NOT ephemeral
 AND manual_cordoned_at IS NOT NULL
RETURNING *;

-- name: InsertWorkerCordonEvent :exec
INSERT INTO worker_cordon_events (owner_id, worker_id, actor_id, actor_label, action, occurred_at, reason)
VALUES (@owner_id, @worker_id, @actor_id, @actor_label, @action, clock_timestamp(), sqlc.narg('reason')::text);
