-- PRD #4416 M2: delivery receipts and the run-env reads of a claim. A receipt names a
-- (var_id, rev) a run was given; it carries no value and no hash.

-- name: ListRunEnvEffectiveForRun :many
-- The effective injection set of one run (M2: the repo scope only): the enabled live
-- versions of the run's user and repo, with the sealed value and the columns the AAD is
-- rebuilt from.
SELECT v.var_id, v.rev, v.name, v.secret, v.description, 'repo'::text AS scope, v.repo_id,
       v.ciphertext, v.sealed_with, v.aad_version
FROM runs r
JOIN run_env_vars v ON v.user_id = r.user_id AND v.repo_id = r.repo_id
WHERE r.id = @run_id
  AND v.retired_at IS NULL AND v.pending_delete_at IS NULL AND v.disabled_at IS NULL
ORDER BY v.name;

-- name: ListRunEnvRedactForRun :many
-- Every secret version the run holds a receipt for that is not in the effective set,
-- one row per (var_id, rev) across all receipt generations. The row is LEFT JOINed: a
-- receipted version whose row is gone comes back with NULL ciphertext, so the caller can
-- park the run instead of silently delivering less. The effective predicate is the one of
-- ListRunEnvEffectiveForRun, repeated here.
SELECT rc.var_id, rc.rev, rc.scope, rc.name,
       v.repo_id, v.ciphertext, v.sealed_with, v.aad_version
FROM runs r
JOIN (SELECT DISTINCT run_id, var_id, rev, scope, name FROM run_env_receipts WHERE secret) rc
  ON rc.run_id = r.id
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = r.user_id
WHERE r.id = @run_id
  AND NOT (v.id IS NOT NULL AND v.repo_id = r.repo_id
           AND v.retired_at IS NULL AND v.pending_delete_at IS NULL AND v.disabled_at IS NULL)
ORDER BY rc.var_id, rc.rev;

-- name: ListRunEnvSecretReceiptVersionsForRun :many
-- The maintenance endpoint's read: every secret version the run holds a receipt for, live or
-- retired (unlike ListRunEnvRedactForRun it does not exclude the effective set), one row per
-- (var_id, rev). LEFT JOINed so a receipted version whose row is gone comes back with NULL
-- ciphertext and the caller answers run_env_history_unavailable instead of delivering less.
SELECT rc.var_id, rc.rev, rc.scope, rc.name,
       v.repo_id, v.ciphertext, v.sealed_with, v.aad_version
FROM runs r
JOIN (SELECT DISTINCT run_id, var_id, rev, scope, name FROM run_env_receipts WHERE secret) rc
  ON rc.run_id = r.id
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = r.user_id
WHERE r.id = @run_id
ORDER BY rc.var_id, rc.rev;

-- name: ListRunEnvEffectiveKeysForRun :many
-- The (var_id, rev) twin of ListRunEnvEffectiveForRun: no value is read. The claim's finish
-- transaction recomputes it under the owner fence and compares it with the assembled list.
SELECT v.var_id, v.rev
FROM runs r
JOIN run_env_vars v ON v.user_id = r.user_id AND v.repo_id = r.repo_id
WHERE r.id = @run_id
  AND v.retired_at IS NULL AND v.pending_delete_at IS NULL AND v.disabled_at IS NULL
ORDER BY v.var_id, v.rev;

-- name: ListRunEnvRedactKeysForRun :many
-- The (var_id, rev) twin of ListRunEnvRedactForRun, same predicate, no value read.
SELECT rc.var_id, rc.rev
FROM runs r
JOIN (SELECT DISTINCT run_id, var_id, rev FROM run_env_receipts WHERE secret) rc
  ON rc.run_id = r.id
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = r.user_id
WHERE r.id = @run_id
  AND NOT (v.id IS NOT NULL AND v.repo_id = r.repo_id
           AND v.retired_at IS NULL AND v.pending_delete_at IS NULL AND v.disabled_at IS NULL)
ORDER BY rc.var_id, rc.rev;

-- name: UpsertRunEnvReceipt :exec
-- Append-only: a re-delivery of the same version keeps first_claim_generation and advances
-- last_claim_generation (never backwards).
INSERT INTO run_env_receipts (run_id, user_id, var_id, rev, scope, name, secret,
                              first_claim_generation, last_claim_generation)
VALUES (@run_id, @user_id, @var_id, @rev, @scope, @name, @secret,
        @claim_generation::bigint, @claim_generation::bigint)
ON CONFLICT (run_id, var_id, rev) DO UPDATE
SET last_claim_generation = GREATEST(run_env_receipts.last_claim_generation, EXCLUDED.last_claim_generation);

-- name: ExistsRunEnvSecretReceipt :one
-- Whether any run holds a secret receipt for one version (store.RunEnvVersionHasReceipt).
SELECT EXISTS (SELECT 1 FROM run_env_receipts WHERE var_id = @var_id AND rev = @rev AND secret)::boolean AS held;

-- name: RunHasSecretRunEnvReceipts :one
SELECT EXISTS (SELECT 1 FROM run_env_receipts WHERE run_id = @run_id AND secret)::boolean AS held;

-- name: LockUserVaultStateForShareNowait :one
-- The share lock the claim's finish transaction takes on the owner row: it conflicts with
-- the FOR UPDATE a variable write (or a vault lifecycle change) holds, and NOWAIT turns that
-- conflict into 55P03 for the caller to requeue on, instead of a wait inside the claim.
SELECT vault_mode, vault_epoch FROM users WHERE id = @id FOR SHARE NOWAIT;

-- name: ListRunInjectedEnv :many
-- The names a claim delivered (plain and secret), for the run page and the lead prompt:
-- the receipts whose last delivery is the given claim generation. No value.
SELECT name, scope, secret
FROM run_env_receipts
WHERE run_id = @run_id AND last_claim_generation = @claim_generation::bigint
ORDER BY name;

-- name: ListRunsHoldingRunEnvVersion :many
-- The runs that hold a secret receipt for one version, for the env_history_full answer
-- and for retention. M2 counts every holder; the sweeper refines which holders still need
-- the version (terminal, no custody hold) and shares this query.
SELECT run_id
FROM run_env_receipts
WHERE var_id = @var_id AND rev = @rev AND secret
ORDER BY run_id
LIMIT @max_runs::int;
