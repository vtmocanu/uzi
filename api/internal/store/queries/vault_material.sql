-- These queries run after the owner users row is locked, behind the shared
-- secret-mutation advisory lock. The conversion protocol requires every credential
-- writer to take the same users-row fence before persistence. That exclusion
-- keeps preflight totals and bounded pages stable until the crypto-only
-- conversion commits or rolls back.
-- Counts include disabled secrets and canonical accounts without aliases.

-- name: HasMasterVaultMaterial :one
-- Unlock's steady state need not load ciphertext when all slots already use DEK.
SELECT EXISTS (
    SELECT 1 FROM user_secrets s
    WHERE s.user_id = @user_id AND s.sealed_with IN ('master', 'master_aad')
    UNION ALL
    SELECT 1 FROM codex_provider_account a
    WHERE a.user_id = @user_id
      AND (a.sealed_with IN ('master', 'master_aad')
           OR a.recovery_sealed_with IN ('master', 'master_aad'))
);

-- name: GetVaultMaterialTotals :one
WITH secrets AS (
    SELECT count(*)::bigint AS material_rows,
           coalesce(sum(octet_length(s.ciphertext)), 0)::bigint AS ciphertext_bytes,
           coalesce(max(octet_length(s.ciphertext)), 0)::bigint AS max_row_bytes
    FROM user_secrets s WHERE s.user_id = @user_id
), accounts AS (
    SELECT (count(*) + count(a.recovery_sealed))::bigint AS material_rows,
           coalesce(sum(octet_length(a.sealed_login)::bigint
                        + coalesce(octet_length(a.recovery_sealed), 0)::bigint), 0)::bigint AS ciphertext_bytes,
           coalesce(max(octet_length(a.sealed_login)::bigint
                        + coalesce(octet_length(a.recovery_sealed), 0)::bigint), 0)::bigint AS max_row_bytes
    FROM codex_provider_account a WHERE a.user_id = @user_id
)
SELECT (secrets.material_rows + accounts.material_rows)::bigint AS material_rows,
       (secrets.ciphertext_bytes + accounts.ciphertext_bytes)::bigint AS ciphertext_bytes,
       greatest(secrets.max_row_bytes, accounts.max_row_bytes)::bigint AS max_row_bytes
FROM secrets CROSS JOIN accounts;

-- name: LockVaultSecretMaterialBatch :many
-- Materialize at most 64 metadata rows, then fetch only the prefix whose actual
-- ciphertext fits 1 MiB. Do not return an oversized first row: preflight rejects
-- max_row_bytes > 1 MiB before any ciphertext collection is loaded.
WITH candidates AS MATERIALIZED (
    SELECT s.id, octet_length(s.ciphertext)::bigint AS ciphertext_bytes
    FROM user_secrets s
    WHERE s.user_id = @user_id
      AND (sqlc.narg('after_id')::uuid IS NULL OR s.id > sqlc.narg('after_id')::uuid)
    ORDER BY s.id LIMIT 64
), bounded AS (
    SELECT id, sum(ciphertext_bytes) OVER (ORDER BY id) AS batch_bytes
    FROM candidates
)
SELECT s.id, s.kind, s.ciphertext, s.sealed_with
FROM user_secrets s JOIN bounded b ON b.id = s.id
WHERE s.user_id = @user_id AND b.batch_bytes <= 1048576
ORDER BY s.id
FOR UPDATE OF s;

-- name: LockVaultAccountMaterialBatch :many
-- Both slots of an account travel together. The 64-material-row budget counts a
-- populated recovery slot separately, and both slots share the 1 MiB byte budget.
WITH candidates AS MATERIALIZED (
    SELECT a.id,
           (octet_length(a.sealed_login)::bigint
             + coalesce(octet_length(a.recovery_sealed), 0)::bigint) AS ciphertext_bytes,
           (1 + CASE WHEN a.recovery_sealed IS NULL THEN 0 ELSE 1 END)::bigint AS material_rows
    FROM codex_provider_account a
    WHERE a.user_id = @user_id
      AND (sqlc.narg('after_id')::uuid IS NULL OR a.id > sqlc.narg('after_id')::uuid)
    ORDER BY a.id LIMIT 64
), bounded AS (
    SELECT id, sum(ciphertext_bytes) OVER (ORDER BY id) AS batch_bytes,
           sum(material_rows) OVER (ORDER BY id) AS batch_rows
    FROM candidates
)
SELECT a.id, a.sealed_login, a.sealed_with, a.recovery_sealed, a.recovery_sealed_with
FROM codex_provider_account a JOIN bounded b ON b.id = a.id
WHERE a.user_id = @user_id
  AND b.batch_bytes <= 1048576 AND b.batch_rows <= 64
ORDER BY a.id
FOR UPDATE OF a;

-- name: ResealVaultSecretMaterial :execrows
-- Crypto only: preserve timestamps, defaults, enablement and material revisions.
UPDATE user_secrets SET ciphertext = @ciphertext, sealed_with = @sealed_with
WHERE user_id = @user_id AND id = @id;

-- name: ResealVaultAccountMaterial :execrows
-- Update the live/recovery discriminators with their bytes in one statement,
-- preserving recovery pairing and every provider/coordination revision and flag.
UPDATE codex_provider_account
SET sealed_login = @sealed_login, sealed_with = @sealed_with,
    recovery_sealed = @recovery_sealed, recovery_sealed_with = @recovery_sealed_with
WHERE user_id = @user_id AND id = @id;
