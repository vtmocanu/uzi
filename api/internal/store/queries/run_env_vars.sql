-- PRD #4416 M1: run environment variables. Every write runs inside a transaction that
-- holds LockSecretMutation and then FenceVaultWrite, like the other secret writers.

-- name: ListRunEnvVarsForRepo :many
-- Live (enabled and disabled) and pending-deletion rows of one repo scope, name order.
-- Retired versions are history and never listed. The ciphertext is returned so the
-- handler can open plain values; secret values are never opened for a response. vault_epoch is
-- the owner's users.vault_epoch read in the same statement as the ciphertext, so the handler
-- opens each value at the epoch its material was read at (secretopen.OpenSealedAtEpoch).
SELECT r.id, r.var_id, r.name, r.secret, r.description, r.ciphertext, r.sealed_with, r.value_bytes, r.rev,
       r.disabled_at, r.pending_delete_at, r.created_at, r.updated_at, r.aad_version, u.vault_epoch
FROM run_env_vars r
JOIN users u ON u.id = r.user_id
WHERE r.user_id = @user_id AND r.repo_id = @repo_id AND r.retired_at IS NULL
ORDER BY r.name, (r.pending_delete_at IS NOT NULL), r.rev DESC;

-- name: GetLiveRunEnvVarByName :one
-- The live row of a repo-scope name. A plain read: the handler's planning read runs
-- outside any transaction.
SELECT id, var_id, user_id, repo_id, name, secret, description, ciphertext, sealed_with, value_bytes,
       rev, retired_at, disabled_at, pending_delete_at, created_at, updated_at, aad_version
FROM run_env_vars
WHERE user_id = @user_id AND repo_id = @repo_id AND name = @name
  AND retired_at IS NULL AND pending_delete_at IS NULL;

-- name: GetLiveRunEnvVarByNameWithEpoch :one
-- The live row of a repo-scope name with the owner's users.vault_epoch read in the same
-- statement, so the handler opens a plain value at the epoch its ciphertext was read at
-- (secretopen.OpenSealedAtEpoch). A plain read outside any transaction.
SELECT r.id, r.var_id, r.user_id, r.repo_id, r.name, r.secret, r.description, r.ciphertext, r.sealed_with,
       r.value_bytes, r.rev, r.retired_at, r.disabled_at, r.pending_delete_at, r.created_at, r.updated_at,
       r.aad_version, u.vault_epoch
FROM run_env_vars r
JOIN users u ON u.id = r.user_id
WHERE r.user_id = @user_id AND r.repo_id = @repo_id AND r.name = @name
  AND r.retired_at IS NULL AND r.pending_delete_at IS NULL;

-- name: GetLiveRunEnvVarByNameForUpdate :one
-- The same row, locked for the write that follows inside the secret-mutation transaction.
SELECT id, var_id, user_id, repo_id, name, secret, description, ciphertext, sealed_with, value_bytes,
       rev, retired_at, disabled_at, pending_delete_at, created_at, updated_at, aad_version
FROM run_env_vars
WHERE user_id = @user_id AND repo_id = @repo_id AND name = @name
  AND retired_at IS NULL AND pending_delete_at IS NULL
FOR UPDATE;

-- name: LockRepoKeyShare :one
-- Pins the repo row for the rest of the transaction, so a concurrent repo delete cannot
-- cascade into the env rows this write is about to touch (and cannot deadlock with it).
-- No row: the repo is gone.
SELECT 1::int AS present FROM repos WHERE id = @repo_id FOR KEY SHARE;

-- name: GetRunEnvScopeUsage :one
-- What a repo scope holds against the per-scope limits: live rows and rows pending
-- deletion count together toward both limits.
SELECT (count(*) FILTER (WHERE pending_delete_at IS NULL))::bigint AS live_count,
       (count(*) FILTER (WHERE pending_delete_at IS NULL AND disabled_at IS NOT NULL))::bigint AS disabled_count,
       (count(*) FILTER (WHERE pending_delete_at IS NOT NULL))::bigint AS pending_count,
       coalesce(sum(value_bytes), 0)::bigint AS value_bytes
FROM run_env_vars
WHERE user_id = @user_id AND repo_id = @repo_id AND retired_at IS NULL;

-- name: GetRunEnvFormBudgetUsage :one
-- The secret value bytes a run of this repo can register for masking: every secret row of
-- the user in this repo's scope or the user scope, whatever its state (live, disabled,
-- pending deletion or retired), because a claim's redact set can carry any of them.
SELECT coalesce(sum(value_bytes), 0)::bigint AS secret_bytes
FROM run_env_vars
WHERE user_id = @user_id AND secret AND (repo_id = @repo_id OR repo_id IS NULL);

-- name: GetRunEnvRetiredUsage :one
-- The per-user history budget: retired versions across both scopes.
SELECT count(*)::bigint AS retired_count,
       coalesce(sum(value_bytes), 0)::bigint AS value_bytes
FROM run_env_vars
WHERE user_id = @user_id AND retired_at IS NOT NULL;

-- name: InsertRunEnvVar :one
INSERT INTO run_env_vars (var_id, user_id, repo_id, name, secret, description, ciphertext, sealed_with,
                          value_bytes, rev, disabled_at, aad_version)
VALUES (@var_id, @user_id, sqlc.narg('repo_id'), @name, @secret, @description, @ciphertext, @sealed_with,
        @value_bytes, @rev, sqlc.narg('disabled_at'), @aad_version)
RETURNING id, var_id, name, secret, description, ciphertext, sealed_with, value_bytes, rev,
          disabled_at, pending_delete_at, created_at, updated_at, aad_version;

-- name: UpdateRunEnvVarDescription :one
-- A description-only save changes the live row in place: no new version.
UPDATE run_env_vars SET description = @description, updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING id, var_id, name, secret, description, ciphertext, sealed_with, value_bytes, rev,
          disabled_at, pending_delete_at, created_at, updated_at, aad_version;

-- name: SetRunEnvVarDisabled :one
-- Disable and Enable are metadata-only on the live version.
UPDATE run_env_vars
SET disabled_at = CASE WHEN @disabled::boolean THEN coalesce(disabled_at, now()) ELSE NULL END,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING id, var_id, name, secret, description, ciphertext, sealed_with, value_bytes, rev,
          disabled_at, pending_delete_at, created_at, updated_at, aad_version;

-- name: RetireRunEnvVarVersion :execrows
-- Keep a secret version a run still holds a receipt for: sealed, never injected or shown.
UPDATE run_env_vars SET retired_at = now()
WHERE id = @id AND user_id = @user_id AND secret AND retired_at IS NULL AND pending_delete_at IS NULL;

-- name: MarkRunEnvVarPendingDeletion :execrows
UPDATE run_env_vars SET pending_delete_at = now(), updated_at = now()
WHERE id = @id AND user_id = @user_id AND secret AND retired_at IS NULL AND pending_delete_at IS NULL;

-- name: DeleteRunEnvVarVersion :execrows
DELETE FROM run_env_vars WHERE id = @id AND user_id = @user_id;

-- name: ListRunEnvVarCountsForUser :many
-- Live variables per repo for the repo DTO's env_var_count (enabled and disabled;
-- rows pending deletion are not counted).
SELECT repo_id::uuid AS repo_id, count(*)::bigint AS var_count
FROM run_env_vars
WHERE user_id = @user_id AND repo_id IS NOT NULL
  AND retired_at IS NULL AND pending_delete_at IS NULL
GROUP BY repo_id;

-- name: CountRunEnvVarsForRepo :one
-- Live variables of one repo, for the repo DTO of the single-repo responses.
SELECT count(*)::bigint AS var_count FROM run_env_vars
WHERE user_id = @user_id AND repo_id = @repo_id AND retired_at IS NULL AND pending_delete_at IS NULL;
