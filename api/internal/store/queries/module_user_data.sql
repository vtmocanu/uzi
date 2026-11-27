-- Issue #4784: per-user JSON documents for runtime web modules.

-- name: ValidateModuleUserDataJSON :one
-- A nil body binds as SQL NULL; report it as JSON null so the caller rejects both alike.
SELECT coalesce(jsonb_typeof(sqlc.arg(data)::jsonb), 'null')::text;

-- name: GetModuleUserData :one
SELECT data, revision FROM module_user_data WHERE user_id = $1 AND module_id = $2;

-- name: LockUserForModuleData :one
SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE;

-- name: CountModuleUserData :one
SELECT count(*) FROM module_user_data WHERE user_id = $1;

-- name: InsertModuleUserData :one
INSERT INTO module_user_data (user_id, module_id, data, revision)
VALUES ($1, $2, $3::jsonb, nextval('module_user_data_revision_seq'))
ON CONFLICT DO NOTHING RETURNING revision;

-- name: UpdateModuleUserData :one
UPDATE module_user_data
   SET data = $3::jsonb, revision = nextval('module_user_data_revision_seq'), updated_at = now()
 WHERE user_id = $1 AND module_id = $2 AND revision = $4
RETURNING revision;

-- name: DeleteModuleUserData :one
DELETE FROM module_user_data WHERE user_id = $1 AND module_id = $2 AND revision = $3 RETURNING revision;
