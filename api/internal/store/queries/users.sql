-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name, is_admin)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByOIDCSubject :one
-- Primary OIDC login lookup (PRD #45): match the stable (issuer, subject) identity.
SELECT * FROM users WHERE oidc_issuer = $1 AND oidc_subject = $2;

-- name: LinkUserOIDC :one
-- Attach an IdP identity to an existing (verified-email-matched) account, but ONLY
-- if the row is not already bound to a subject (audit H1): the WHERE oidc_subject IS
-- NULL guard makes a re-link a no-op that returns no row, so the caller can reject an
-- email match against a row already bound to a DIFFERENT subject instead of
-- overwriting it. The caller asserts exactly one row was returned.
UPDATE users SET oidc_issuer = $2, oidc_subject = $3
WHERE id = $1 AND oidc_subject IS NULL
RETURNING *;

-- name: CreateUserOIDC :one
-- JIT-provision a passwordless OIDC user (PRD #45, Decision 7): password_hash is
-- NULL so password login always fails constant-time. is_admin follows the
-- first-user rule, decided by the caller under the advisory lock.
INSERT INTO users (email, password_hash, display_name, is_admin, oidc_issuer, oidc_subject)
VALUES ($1, NULL, $2, $3, $4, $5)
RETURNING *;

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

-- name: SetUserJudgeEnabled :one
-- Flip a user's run-retrospective opt-in (PRD #46 Decision 7). Per-user consent to
-- spend the user's own Anthropic tokens judging every finished run; default false.
-- The caller passes the target id: the session user for PUT /api/me/judge, or an
-- admin-chosen id (from the path) for the admin per-user toggle.
UPDATE users SET judge_enabled = $2 WHERE id = $1
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

-- name: SetUserAdmin :one
-- Authoritative sync of is_admin from OIDC group membership (PRD #55): membership in
-- an UZI_OIDC_ADMIN_GROUPS group grants, leaving the group demotes, on the user's
-- next OIDC login. is_admin is reloaded per-request by RequireAuth (not carried in
-- the JWT), so a flip propagates to live sessions without a token_version bump. The
-- seed-admin demotion exemption is enforced in the caller, not here.
UPDATE users SET is_admin = @is_admin WHERE id = @id
RETURNING *;
