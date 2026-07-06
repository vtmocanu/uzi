-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name, is_admin)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at ASC;

-- name: SetLastLogin :exec
UPDATE users SET last_login = now() WHERE id = $1;

-- name: SetUserAutopilotEnabled :one
-- Flip a user's autopilot opt-in (PRD #19 M3, Decision 4). Per-user consent to
-- unattended runs; default false, set from the user's own Settings page.
UPDATE users SET autopilot_enabled = $2 WHERE id = $1
RETURNING *;

-- name: GetUserDefaultModel :one
-- The current user's per-user default worker model (PRD #17); NULL = inherit.
SELECT default_model FROM users WHERE id = $1;

-- name: SetUserDefaultModel :one
-- Sets (or clears, when @default_model is NULL) the current user's default
-- worker model. Own-user only; the caller passes the session user's id.
UPDATE users SET default_model = @default_model WHERE id = @id
RETURNING default_model;

-- name: GetUserSettings :one
-- The current user's own (non-secret) settings surface: default worker model
-- (PRD #17) and UI theme override (PRD #21). Both NULL = inherit / use the
-- instance default. Own-user only; the caller passes the session user's id.
SELECT default_model, theme FROM users WHERE id = $1;

-- name: SetUserTheme :one
-- Sets (or clears, when @theme is NULL) the current user's theme override.
-- NULL falls the user back to the instance default. Own-user only.
UPDATE users SET theme = @theme WHERE id = @id
RETURNING theme;

-- name: BumpTokenVersion :one
UPDATE users SET token_version = token_version + 1 WHERE id = $1
RETURNING token_version;

-- name: UpdatePassword :exec
UPDATE users
SET password_hash = $2, token_version = token_version + 1
WHERE id = $1;

-- name: SetUserActive :one
UPDATE users
SET is_active = @is_active,
    -- Deactivation revokes all live sessions by bumping token_version;
    -- reactivation leaves it untouched.
    token_version = CASE WHEN @is_active THEN token_version ELSE token_version + 1 END
WHERE id = @id
RETURNING *;
