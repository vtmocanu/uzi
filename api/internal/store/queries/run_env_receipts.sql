-- PRD #4416 M2: run environment variable delivery receipts and the claim-time reads.
-- A receipt names a (var_id, rev) a run's claim delivered; it holds no value and no hash.

-- name: ListRunEnvEffectiveForRun :many
-- The versions a claim of this run delivers: the enabled, live repo-scope rows of the
-- run's owner and repo. The ciphertext and the columns its AAD is rebuilt from come back
-- with each row; the caller opens them (a locked vault withholds the claim, never a value).
-- vault_epoch is read in the same statement as the ciphertext so the caller opens the pair
-- with secretopen.OpenSealedAtEpoch (a rewrap between read and open is a stale seal).
SELECT e.id, e.var_id, e.rev, e.name, e.secret, e.description, e.ciphertext, e.sealed_with,
       e.aad_version, e.repo_id, e.user_id, u.vault_epoch
FROM runs r
JOIN users u ON u.id = r.user_id
JOIN run_env_vars e ON e.user_id = r.user_id AND e.repo_id = r.repo_id
WHERE r.id = @run_id
  AND e.disabled_at IS NULL AND e.retired_at IS NULL AND e.pending_delete_at IS NULL
ORDER BY e.name;

-- name: ListRunEnvEffectiveKeysForRun :many
-- ListRunEnvEffectiveForRun without the ciphertext: the finish transaction's recompute.
SELECT e.var_id, e.rev, e.name, e.secret
FROM runs r
JOIN run_env_vars e ON e.user_id = r.user_id AND e.repo_id = r.repo_id
WHERE r.id = @run_id
  AND e.disabled_at IS NULL AND e.retired_at IS NULL AND e.pending_delete_at IS NULL
ORDER BY e.name;

-- name: ListRunEnvRedactForRun :many
-- The secret versions this run was delivered that the claim does NOT deliver now (changed,
-- disabled, deleted or retired since): the worker still masks them. The row columns come
-- back NULL when no run_env_vars row of the run's owner holds the version any more, which
-- the caller reads as missing (the run cannot be masked completely). One row per receipt.
-- vault_epoch rides in the same statement as the ciphertext (see ListRunEnvEffectiveForRun).
SELECT rc.var_id, rc.rev, rc.name, rc.scope,
       v.id AS row_id, v.ciphertext, v.sealed_with, v.aad_version, v.secret AS row_secret,
       v.repo_id AS row_repo_id, v.user_id AS row_user_id, v.name AS row_name,
       u.vault_epoch
FROM run_env_receipts rc
JOIN runs r ON r.id = rc.run_id
JOIN users u ON u.id = r.user_id
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = r.user_id
WHERE rc.run_id = @run_id AND rc.secret
  AND NOT EXISTS (
      SELECT 1 FROM run_env_vars e
      WHERE e.user_id = r.user_id AND e.repo_id = r.repo_id
        AND e.var_id = rc.var_id AND e.rev = rc.rev
        AND e.disabled_at IS NULL AND e.retired_at IS NULL AND e.pending_delete_at IS NULL)
ORDER BY rc.var_id, rc.rev;

-- name: ListRunEnvSecretReceiptVersionsForRun :many
-- Every secret version this run was ever delivered, current or not, with the owner's stored
-- row (NULL row columns when no row of the run's owner holds it): the maintenance endpoint that
-- hands a worker everything it must mask for the run. One row per secret receipt.
-- vault_epoch rides in the same statement as the ciphertext (see ListRunEnvEffectiveForRun).
SELECT rc.var_id, rc.rev, rc.name, rc.scope,
       v.id AS row_id, v.ciphertext, v.sealed_with, v.aad_version, v.secret AS row_secret,
       v.repo_id AS row_repo_id, v.user_id AS row_user_id, v.name AS row_name,
       u.vault_epoch
FROM run_env_receipts rc
JOIN runs r ON r.id = rc.run_id
JOIN users u ON u.id = r.user_id
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = r.user_id
WHERE rc.run_id = @run_id AND rc.secret
ORDER BY rc.var_id, rc.rev;

-- name: WorkerHoldsOpenCustodyForRun :one
-- Whether the worker took an open recovery custody hold on the run (the redact maintenance route's
-- second authorization, beside runs.worker_id). An inventory-guarded hold additionally requires
-- the worker to still be its live holder on this run. Read-only; no generation or repo check.
SELECT EXISTS (
    SELECT 1 FROM recovery_custody_holds h
    WHERE h.run_id = @run_id AND h.user_id = @user_id AND h.state = 'open'
      AND h.original_worker_id = @worker_id
      AND (NOT h.inventory_guarded OR (h.live_worker_id = @worker_id AND h.live_run_id = @run_id))
)::boolean AS held;

-- name: RunEnvRedactOpen :one
-- Whether the redact route still answers for the requested run, which is not the governing run.
-- A run that is not a cross-check child is served while it is not terminal. A cross-check child
-- is served only while it is not terminal and its cross_checks row is open (a pending plan
-- verdict or a pending code outcome, before its deadline). Either is also served while
-- fn_run_env_publication_pending holds (an open custody hold, a capture preparing or uploading,
-- a held publication prepared, invoked or of unknown outcome, a terminal preservation bound or
-- publishing).
SELECT (
    r.status NOT IN ('completed', 'failed', 'cancelled')
    OR fn_run_env_publication_pending(r.id)
)::boolean AS open
FROM runs r
WHERE r.id = @run_id AND r.kind <> 'cross_check'
UNION ALL
SELECT (
    fn_run_env_publication_pending(r.id)
    OR (r.status NOT IN ('completed', 'failed', 'cancelled')
        AND EXISTS (
            SELECT 1 FROM cross_checks x
            WHERE x.checker_run_id = r.id AND x.deadline_at > now()
              AND ((x.stage = 'plan' AND x.verdict = 'pending')
                   OR (x.stage = 'code' AND x.outcome = 'pending'))))
)::boolean AS open
FROM runs r
WHERE r.id = @run_id AND r.kind = 'cross_check';

-- name: ListRunEnvRedactKeysForRun :many
-- ListRunEnvRedactForRun without the ciphertext: the finish transaction's recompute.
-- present is false when no row of the owner holds the version.
SELECT rc.var_id, rc.rev, rc.name, rc.scope, (v.id IS NOT NULL)::boolean AS present
FROM run_env_receipts rc
JOIN runs r ON r.id = rc.run_id
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = r.user_id
WHERE rc.run_id = @run_id AND rc.secret
  AND NOT EXISTS (
      SELECT 1 FROM run_env_vars e
      WHERE e.user_id = r.user_id AND e.repo_id = r.repo_id
        AND e.var_id = rc.var_id AND e.rev = rc.rev
        AND e.disabled_at IS NULL AND e.retired_at IS NULL AND e.pending_delete_at IS NULL)
ORDER BY rc.var_id, rc.rev;

-- name: UpsertRunEnvReceipt :exec
-- Record that the claim at this generation delivered one version. A later claim of the same
-- run keeps first_claim_generation and advances last_claim_generation (never backwards).
INSERT INTO run_env_receipts (run_id, user_id, var_id, rev, scope, name, secret,
                              first_claim_generation, last_claim_generation)
VALUES (@run_id, @user_id, @var_id, @rev, @scope, @name, @secret, @generation::bigint, @generation::bigint)
ON CONFLICT (run_id, var_id, rev) DO UPDATE
SET last_claim_generation = GREATEST(run_env_receipts.last_claim_generation, EXCLUDED.last_claim_generation);

-- name: RunEnvSecretVersionReceiptExists :one
-- Whether any run holds a SECRET receipt for one version. Plain receipts never retain a
-- version. Behind store.RunEnvVersionHasReceipt.
SELECT EXISTS (SELECT 1 FROM run_env_receipts WHERE var_id = @var_id AND rev = @rev AND secret) AS held;

-- name: RunHasSecretRunEnvReceipts :one
SELECT EXISTS (SELECT 1 FROM run_env_receipts WHERE run_id = @run_id AND secret) AS held;

-- name: ListRunEnvReceiptsForRunGeneration :many
-- What the run's current claim generation delivered, for the run page: names, scope and
-- the secret flag only.
SELECT rc.var_id, rc.rev, rc.name, rc.scope, rc.secret
FROM run_env_receipts rc
JOIN runs r ON r.id = rc.run_id
WHERE rc.run_id = @run_id AND rc.last_claim_generation = r.claim_generation
ORDER BY rc.name, rc.var_id, rc.rev;

-- name: LockUserVaultStateForKeyShareNowait :one
-- The owner's vault mode and epoch under FOR KEY SHARE NOWAIT: it fails at once (55P03) when
-- a writer holds the row FOR UPDATE, and it does not conflict with a plain UPDATE of a
-- non-key users column (FOR NO KEY UPDATE). The run_env_vars writers that take the users row
-- FOR UPDATE are the variable writes (handler run_env_vars.go via LockUserVaultState), the
-- vault lifecycle and rewrap (LockVaultAuthState), the account reset
-- (DeleteAllRunEnvVarsForOwner, inside the vault lifecycle lock) and a user delete. The
-- retired-version sweeper deletes under FOR SHARE NOWAIT and skips versions that
-- fn_run_env_run_held reports a run as holding; a repo delete cascades the repo's rows,
-- runs and receipts with no users lock.
SELECT vault_mode, vault_epoch FROM users WHERE id = @user_id FOR KEY SHARE NOWAIT;

-- PRD #4416: the claim finisher's park for a run whose secret run-env history cannot be
-- masked completely (a receipted secret version is missing or cannot be opened). It is
-- ParkRunOverrideUnavailableRun's twin with the hold reason fixed to
-- run_env_history_unavailable: the same exact-claim fence (status 'claimed' at this worker
-- and generation, claim unreleased), the same undelivered-claim shape and the same
-- one-claim Fresh restore, because the payload was never delivered. No promoter reads this
-- hold: only the owner's resume leaves it.
-- name: ParkRunEnvHistoryUnavailableRun :execrows
WITH fresh_restored AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT r.id, 'resume', '{"policy_fresh":"restored","generation":' || r.claim_generation::text || '}'
    FROM runs r
    WHERE r.id = @id AND r.worker_id = @worker_id AND r.claim_generation = @claim_generation
      AND r.claim_released_at IS NULL AND r.status = 'claimed'
      AND r.policy_fresh_consumed_generation = @claim_generation::bigint
      AND r.policy_fresh_confirmed_generation IS NULL AND r.policy_preserve_required
    RETURNING 1
)
UPDATE runs SET
    policy_pending_fresh = CASE WHEN runs.policy_fresh_consumed_generation = @claim_generation::bigint AND runs.policy_fresh_confirmed_generation IS NULL AND runs.policy_preserve_required
        THEN true ELSE runs.policy_pending_fresh END,
    policy_fresh_consumed_generation = CASE WHEN runs.policy_fresh_consumed_generation = @claim_generation::bigint AND runs.policy_fresh_confirmed_generation IS NULL AND runs.policy_preserve_required
        THEN NULL ELSE runs.policy_fresh_consumed_generation END,
    status = 'paused',
    status_since = now(),
    hold_reason = 'run_env_history_unavailable',
    claim_released_at = now(),
    codex_cap_hash = NULL,
    codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.worker_id = @worker_id
  AND runs.claim_generation = @claim_generation
  AND runs.claim_released_at IS NULL
  AND runs.status = 'claimed';
