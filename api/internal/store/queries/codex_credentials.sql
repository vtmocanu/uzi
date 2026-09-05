-- name: ListUserSecretsAll :many
-- Every one of a user's secrets across ALL kinds, metadata only (PRD #1147 M1) —
-- the future all-kinds list endpoint. ListUserSecretsForKind stays the per-kind
-- listing; this is its cross-kind sibling and never selects the ciphertext. Ordered
-- by kind, then default-first, then label, so each kind's group reads default-first
-- exactly as the per-kind list does.
SELECT id, kind, label, is_default, auto_eligible, created_at, updated_at
FROM user_secrets
WHERE user_id = @user_id
ORDER BY kind, is_default DESC, lower(label) ASC;

-- name: InsertCodexProviderAccount :one
-- Create the authoritative account row for a Codex login (PRD #1147 M1). The
-- generation/revision counters, coordination state and recovery slot take their
-- schema defaults (00198); a later refresher (m2) advances them. Returns the whole
-- row so the caller has the minted id and defaults without a re-read.
INSERT INTO codex_provider_account (user_id, provider_user_id, workspace_account_id, sealed_login, sealed_with)
VALUES (@user_id, @provider_user_id, @workspace_account_id, @sealed_login, @sealed_with)
RETURNING *;

-- name: GetCodexProviderAccountByTuple :one
-- Resolve a Codex account by its canonical identity tuple (PRD #1147 M1),
-- owner-scoped. The tuple is unique per user (codex_provider_account_tuple_key), so
-- this is :one. pgx.ErrNoRows means the user holds no account for that tuple yet.
SELECT * FROM codex_provider_account
WHERE user_id = @user_id
  AND provider_user_id = @provider_user_id
  AND workspace_account_id = @workspace_account_id;

-- name: GetCodexProviderAccountByID :one
-- Fetch one Codex account by identity, owner-scoped in the predicate so a caller
-- supplying another user's account id reads no row rather than that user's account.
SELECT * FROM codex_provider_account
WHERE user_id = @user_id AND id = @id;

-- name: InsertCodexCredentialState :one
-- Create the per-alias state row for a codex-kind secret (PRD #1147 M1). Owner-scoped
-- by the composite FK (00199) to a secret the same user owns. status is 'staging'
-- for a codex_auth awaiting link or 'static' for a standalone openai_api_key;
-- material_revision and the account link take their schema defaults.
INSERT INTO codex_credential_state (user_secret_id, user_id, status)
VALUES (@user_secret_id, @user_id, @status)
RETURNING *;

-- name: GetCodexCredentialState :one
-- Fetch one alias's state, owner-scoped so a foreign secret id reads no row.
-- pgx.ErrNoRows means the alias has no state row (or is not this user's).
SELECT * FROM codex_credential_state
WHERE user_secret_id = @user_secret_id AND user_id = @user_id;

-- name: LinkCodexCredentialState :execrows
-- Bind an alias to a resolved provider account (PRD #1147 M1): move it to 'linked'
-- and record the account id. Owner-scoped; affects the single alias row, or 0 when
-- the alias is not this user's.
UPDATE codex_credential_state
SET status = 'linked', provider_account_id = @provider_account_id, updated_at = now()
WHERE user_secret_id = @user_secret_id AND user_id = @user_id;

-- name: SetCodexCredentialStateStatus :execrows
-- Move an alias to an arbitrary status and record last_error alongside it (PRD #1147
-- M1) — the failure path writes 'failed' with the reason, and clearing a prior error
-- writes the new status with a NULL last_error. Owner-scoped. Deliberately does NOT
-- touch provider_account_id: a status change is not a re-link.
UPDATE codex_credential_state
SET status = @status, last_error = @last_error, updated_at = now()
WHERE user_secret_id = @user_secret_id AND user_id = @user_id;

-- name: BumpCodexMaterialRevision :execrows
-- The underlying material changed (PRD #1147 M1): advance material_revision, set the
-- new status, and DROP the account link (a material change invalidates the previous
-- binding, which must be re-resolved). Owner-scoped; affects the single alias row.
UPDATE codex_credential_state
SET material_revision = material_revision + 1,
    status = @status,
    provider_account_id = NULL,
    updated_at = now()
WHERE user_secret_id = @user_secret_id AND user_id = @user_id;
