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
-- schema defaults (00199); a later refresher (m2) advances them. Returns the whole
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
-- by the composite FK (00200) to a secret the same user owns. status is 'staging'
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
--
-- SECURITY HARDENING (PRD #1147 audit): material-revision CAS. The added
-- `AND material_revision = @material_revision` fences the reconcile relink against a
-- manual replace that races a slow discovery. A user replacing the alias's underlying
-- material calls BumpCodexMaterialRevision, which advances material_revision and drops
-- the account link precisely because the old binding is now invalid. If a discovery
-- that started BEFORE the replace then relinks blindly, it would re-point the freshly
-- replaced alias at the STALE account it resolved. Requiring the revision the discovery
-- observed makes that stale relink match 0 rows; the service treats 0 rows as a lost
-- race and re-resolves. :execrows stays: 0 = the alias was replaced under us.
UPDATE codex_credential_state
SET status = 'linked', provider_account_id = @provider_account_id, updated_at = now()
WHERE user_secret_id = @user_secret_id AND user_id = @user_id
    AND material_revision = @material_revision::bigint;

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

-- name: CountCodexSecrets :one
-- How many codex-kind credentials the user holds across BOTH kinds (PRD #1147 M1),
-- read inside the mutation transaction so a create can decide whether this is the
-- user's FIRST codex credential (and must therefore be forced default, mirroring the
-- anthropic first-token rule but spanning openai_api_key + codex_auth together — the
-- two share one default slot per user_secrets_codex_one_default_key). Under the
-- advisory lock this count is stable against a concurrent create.
SELECT count(*) FROM user_secrets
WHERE user_id = @user_id AND kind IN ('openai_api_key', 'codex_auth');

-- name: ClearCodexDefaults :execrows
-- Clear the user's current codex default across BOTH codex kinds (PRD #1147 M1). The
-- first half of the create-as-default and set-default swap: the codex kinds share ONE
-- default slot (user_secrets_codex_one_default_key), so this must span openai_api_key
-- AND codex_auth rather than one kind, unlike ClearDefaultUserSecret. Run under the
-- advisory lock so it cannot race a concurrent promote into two defaults. Affects the
-- single codex default row, or 0 when there is none. anthropic_token is untouched: it
-- keeps its own separate default via 00077's per-kind index.
UPDATE user_secrets SET is_default = false, updated_at = now()
WHERE user_id = @user_id AND is_default AND kind IN ('openai_api_key', 'codex_auth');

-- name: InsertCodexSecret :one
-- Store a NEW codex-kind secret (PRD #1147 M1), setting is_default to @want_default
-- EXACTLY as the caller asks — the caller decides. Deliberately NOT InsertUserSecret:
-- that query force-defaults the FIRST secret OF A KIND, which here would mint a SECOND
-- codex default the moment a user adds their first openai_api_key while already holding
-- a codex_auth default (the two kinds share one default slot), violating
-- user_secrets_codex_one_default_key. The caller (h.CreateCodexAuth/CreateOpenAIAPIKey)
-- decides want_default from CountCodexSecrets across both kinds instead. auto_eligible
-- takes its schema default (false) — neither codex kind opts into the anthropic pool,
-- which 00087's kind CHECK requires. Returns metadata only, never the ciphertext.
INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
VALUES (@user_id, @kind, @label, @want_default, @ciphertext, @sealed_with)
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: ListCodexCredentialStatesForUser :many
-- Every codex_credential_state row this user owns (PRD #1147 M1): the (secret id,
-- status) pairs the all-kinds list endpoint merges into each codex row's DTO so the UI
-- can show staging/linked/failed/static. Owner-scoped; anthropic rows have no state
-- row here and carry an empty status.
SELECT user_secret_id, status FROM codex_credential_state
WHERE user_id = @user_id;
