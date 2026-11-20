-- PRD #4416 M2: the retired-version sweep (workersvc.SweepRunEnvRetired, pass run_env_retired_sweep).
-- A retired or pending-deletion run_env_vars row is deleted once no run that holds a secret receipt for
-- it is still held (fn_run_env_run_held, migration 00499). Every statement here runs in the pass's
-- per-user transaction, after LockUserVaultStateForShareNowait on the owner row.

-- name: ListRunEnvSweepUsers :many
-- The next batch of users that own a retired or pending-deletion row, after the given cursor in user_id
-- order (the zero UUID starts at the beginning). Reads run_env_vars_retained_user_idx only.
SELECT DISTINCT user_id
FROM run_env_vars
WHERE user_id > @after_user_id::uuid
  AND (retired_at IS NOT NULL OR pending_delete_at IS NOT NULL)
ORDER BY user_id
LIMIT @max_users::int;

-- name: DeleteReleasedRunEnvVars :execrows
-- Deletes up to max_rows of one user's retired or pending-deletion rows that no still-held run has a
-- secret receipt for. A row nobody holds a receipt for is released at once. SKIP LOCKED leaves a row a
-- concurrent writer holds for the next tick.
WITH releasable AS (
    SELECT v.id
    FROM run_env_vars v
    WHERE v.user_id = @user_id
      AND (v.retired_at IS NOT NULL OR v.pending_delete_at IS NOT NULL)
      AND NOT EXISTS (
          SELECT 1 FROM run_env_receipts rc
          WHERE rc.var_id = v.var_id AND rc.rev = v.rev AND rc.secret
            AND fn_run_env_run_held(rc.run_id))
    ORDER BY v.id
    LIMIT @max_rows::int
    FOR UPDATE OF v SKIP LOCKED
)
DELETE FROM run_env_vars WHERE id IN (SELECT id FROM releasable);

-- name: DeleteUnreferencedRunEnvVarIDs :execrows
-- Deletes up to max_rows of one user's variable identities that no run_env_vars row and no receipt
-- names any more, so run_env_var_ids does not grow with every variable ever deleted.
WITH unreferenced AS (
    SELECT i.var_id
    FROM run_env_var_ids i
    WHERE i.user_id = @user_id
      AND NOT EXISTS (SELECT 1 FROM run_env_vars v WHERE v.var_id = i.var_id)
      AND NOT EXISTS (SELECT 1 FROM run_env_receipts rc WHERE rc.var_id = i.var_id)
    ORDER BY i.var_id
    LIMIT @max_rows::int
    FOR UPDATE OF i SKIP LOCKED
)
DELETE FROM run_env_var_ids WHERE var_id IN (SELECT var_id FROM unreferenced);
