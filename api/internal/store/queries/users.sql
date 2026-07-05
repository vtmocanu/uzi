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
