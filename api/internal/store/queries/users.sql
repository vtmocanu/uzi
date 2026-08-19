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

-- name: SetUserWaitOnLimit :one
-- Flip a user's usage-limit park default (PRD #35 Decision 7). Per-user consent to
-- a run PARKING rather than failing when the owner's Anthropic window is exhausted;
-- default false, set from the user's own Settings page.
--
-- It is the DEFAULT a new run inherits, never a retroactive switch: every run
-- carries its own runs.wait_on_limit, stamped at creation, and flipping this changes
-- nothing about a run that already exists (parked or otherwise). That is the same
-- shape as autopilot_enabled above and is what makes the per-run toggle
-- (SetRunWaitOnLimit) a separate write rather than a filter over this one.
UPDATE users SET wait_on_limit = $2 WHERE id = $1
RETURNING *;

-- name: SetUserJudgeEnabled :one
-- Flip a user's run-retrospective opt-in (PRD #46 Decision 7). Per-user consent to
-- spend the user's own Anthropic tokens judging every finished run; default false.
-- The caller passes the target id: the session user for PUT /api/me/judge, or an
-- admin-chosen id (from the path) for the admin per-user toggle.
UPDATE users SET judge_enabled = $2 WHERE id = $1
RETURNING *;

-- name: SetUserCIAutofixEnabled :one
-- Flip a user's automatic CI-fix opt-in (PRD #71). Per-user consent to spend the
-- user's own Anthropic tokens auto-fixing failed pipelines on their agent MR
-- branches; default false. The caller passes the target id: the session user for
-- PUT /api/me/ci-autofix, or an admin-chosen id (from the path) for the admin toggle.
UPDATE users SET ci_autofix_enabled = $2 WHERE id = $1
RETURNING *;

-- name: SetUserJudgeAnthropicSecret :one
-- Point a user's JUDGE lane at one of their own Anthropic credentials, or clear the
-- binding back to their default (PRD #104 M4, D1). Per-user, not per-worker: which
-- credential reviews your work is a property of you, not of whichever worker
-- claimed the retrospective.
--
-- Nothing here validates ownership of @judge_anthropic_secret_id — the composite FK
-- from 00079 does, in the database. A foreign secret id has no (users.id, id) pair
-- in user_secrets and this UPDATE raises a foreign_key_violation. The handler
-- checks too, so the caller gets a 404 rather than a constraint violation, but the
-- FK is the layer that holds when the handler check does not.
UPDATE users SET judge_anthropic_secret_id = @judge_anthropic_secret_id WHERE id = @id
RETURNING *;

-- name: GetUserJudgeAnthropicSecret :one
-- The user's judge-lane binding, read at judge-claim time. NULL ⇒ resolve their
-- default token. Selects only the binding: the judge claim needs no other user
-- column, and a narrow read keeps it obvious that nothing else about the user
-- influences which credential a retrospective spends.
SELECT judge_anthropic_secret_id FROM users WHERE id = @id;

-- name: GetUserDefaultModel :one
-- The current user's per-user default worker model (PRD #17); NULL = inherit.
SELECT default_model FROM users WHERE id = $1;

-- name: SetUserDefaultModel :one
-- Sets (or clears, when @default_model is NULL) the current user's default
-- worker model. Own-user only; the caller passes the session user's id.
UPDATE users SET default_model = @default_model WHERE id = @id
RETURNING default_model;

-- name: GetUserJudgeModel :one
-- The current user's per-user judge model override (PRD #69 M2); NULL = inherit
-- the instance judge_model. Read at judge-claim assembly, keyed on the run owner.
-- Selects only the column: the judge claim needs no other user field, and a
-- narrow read keeps resolution self-contained and cheap (Decision 5).
SELECT judge_model FROM users WHERE id = $1;

-- name: SetUserJudgeModel :one
-- Sets (or clears, when @judge_model is NULL) the current user's per-user judge
-- model. NULL inherits the instance judge_model. Own-user only; caller passes the
-- session user's id.
UPDATE users SET judge_model = @judge_model WHERE id = @id
RETURNING judge_model;

-- name: GetUserSummaryModel :one
-- The current user's per-user run-summary model override (PRD #362 M2); NULL =
-- inherit the instance summary_model. Read at ISSUE-run claim assembly, keyed on
-- the run owner. Selects only the column: the claim needs no other user field, and
-- a narrow read keeps resolution self-contained and cheap (Decision 8). Mirrors
-- GetUserJudgeModel but rides the issue-run claim rather than the judge claim.
SELECT summary_model FROM users WHERE id = $1;

-- name: SetUserSummaryModel :one
-- Sets (or clears, when @summary_model is NULL) the current user's per-user
-- run-summary model. NULL inherits the instance summary_model. Own-user only;
-- caller passes the session user's id.
UPDATE users SET summary_model = @summary_model WHERE id = @id
RETURNING summary_model;

-- name: GetUserSettings :one
-- The current user's own (non-secret) settings surface: default worker model
-- (PRD #17), per-user judge model override (PRD #69), per-user run-summary model
-- override (PRD #362 M2), UI theme override (PRD #21), and the sidebar token-meter
-- choice (00123). NULL = inherit / use the instance default / default-token-only.
-- Own-user only; the caller passes the session user's id. summary_model rides this
-- one-row read for the settings surface (GetUserSummaryModel stays the narrow read
-- for issue-run claim assembly), so the settings response needs no second query.
SELECT default_model, judge_model, summary_model, theme, sidebar_token_ids FROM users WHERE id = $1;

-- name: SetUserTheme :one
-- Sets (or clears, when @theme is NULL) the current user's theme override.
-- NULL falls the user back to the instance default. Own-user only.
UPDATE users SET theme = @theme WHERE id = @id
RETURNING theme;

-- name: SetUserSidebarTokens :one
-- Replaces the user's whole sidebar token-meter set (00123): the non-default
-- Anthropic tokens whose rate meters ride the sidebar rail. The handler has
-- already filtered the ids to the caller's own anthropic_token secrets; an
-- empty array is a valid "default-only" choice. Own-user only.
UPDATE users SET sidebar_token_ids = @sidebar_token_ids::uuid[] WHERE id = @id
RETURNING sidebar_token_ids;

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
