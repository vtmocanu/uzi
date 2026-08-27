-- name: ListAppSettings :many
SELECT * FROM app_settings ORDER BY key;

-- name: ListAppSettingsForUpdate :many
-- Row-locks the settings rows for the duration of the caller's transaction so a
-- concurrent settings PUT blocks here and reads this writer's committed values
-- (closes the cross-key prd_label != autopilot_label TOCTOU: PRD #19 M2).
SELECT * FROM app_settings ORDER BY key FOR UPDATE;

-- name: UpsertAppSetting :one
INSERT INTO app_settings (key, value, updated_by, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (key) DO UPDATE
    SET value      = EXCLUDED.value,
        updated_by = EXCLUDED.updated_by,
        updated_at = now()
RETURNING *;

-- name: DeleteAppSettings :execrows
-- Retire a set of app_settings keys (PRD #590 M2): the boot-migration deletes the
-- legacy selfimprove_* keys after materializing the self_improve schedule row.
DELETE FROM app_settings WHERE key = ANY(@keys::text[]);
