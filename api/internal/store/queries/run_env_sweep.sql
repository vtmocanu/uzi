-- PRD #4416 M2: the retired-version sweeper and the history-full answer. Both read
-- fn_run_env_run_held (migration 00543) as the one definition of a held run.

-- name: ListRunEnvSweepUsers :many
-- Owners with work for the sweeper, after a cursor and in id order: a retired or pending-deletion
-- version (run_env_vars_sweep_idx), or a variable identity that no row and no receipt references
-- any more (run_env_var_ids_user_idx).
SELECT u.user_id::uuid AS user_id
FROM (
    (SELECT v.user_id FROM run_env_vars v
     WHERE (v.retired_at IS NOT NULL OR v.pending_delete_at IS NOT NULL) AND v.user_id > @after::uuid
     GROUP BY v.user_id ORDER BY v.user_id LIMIT @max_users::int)
    UNION
    (SELECT i.user_id FROM run_env_var_ids i
     WHERE i.user_id > @after::uuid
       AND NOT EXISTS (SELECT 1 FROM run_env_vars v WHERE v.var_id = i.var_id)
       AND NOT EXISTS (SELECT 1 FROM run_env_receipts rc WHERE rc.var_id = i.var_id)
     GROUP BY i.user_id ORDER BY i.user_id LIMIT @max_users::int)
) u
ORDER BY u.user_id
LIMIT @max_users::int;

-- name: LockUserForRunEnvSweepNowait :one
-- The sweeper's per-owner lock. FOR SHARE conflicts with every users-row writer (FOR UPDATE
-- and a plain UPDATE), so a busy owner fails at once (55P03) and is skipped until the next
-- pass; it never waits and it blocks no claim finish (FOR KEY SHARE).
SELECT id FROM users WHERE id = @user_id FOR SHARE NOWAIT;

-- name: DeleteRunEnvSweepVersions :execrows
-- One bounded batch of retired or pending-deletion versions of the owner that no HELD run holds
-- a secret receipt for. A plain receipt never kept a version.
DELETE FROM run_env_vars v
USING (
    SELECT e.id FROM run_env_vars e
    WHERE e.user_id = @user_id
      AND (e.retired_at IS NOT NULL OR e.pending_delete_at IS NOT NULL)
      AND NOT EXISTS (SELECT 1 FROM run_env_receipts rc
                      WHERE rc.var_id = e.var_id AND rc.rev = e.rev AND rc.secret AND fn_run_env_run_held(rc.run_id))
    ORDER BY e.id
    LIMIT @batch::int
    FOR UPDATE OF e SKIP LOCKED
) d
WHERE v.id = d.id;

-- name: DeleteRunEnvSweepIdentities :execrows
-- One bounded batch of the owner's variable identities that no version row and no receipt
-- references.
DELETE FROM run_env_var_ids i
USING (
    SELECT x.var_id FROM run_env_var_ids x
    WHERE x.user_id = @user_id
      AND NOT EXISTS (SELECT 1 FROM run_env_vars v WHERE v.var_id = x.var_id)
      AND NOT EXISTS (SELECT 1 FROM run_env_receipts rc WHERE rc.var_id = x.var_id)
    ORDER BY x.var_id
    LIMIT @batch::int
    FOR UPDATE OF x SKIP LOCKED
) d
WHERE i.var_id = d.var_id;

-- name: ListRunsHoldingRunEnvVersions :many
-- Up to @max_runs held runs that keep a retired or pending-deletion secret version of the owner
-- sealed: the answer behind env_history_full.
SELECT held.run_id::uuid AS run_id
FROM (
    SELECT DISTINCT rc.run_id
    FROM run_env_vars v
    JOIN run_env_receipts rc ON rc.var_id = v.var_id AND rc.rev = v.rev AND rc.secret
    WHERE v.user_id = @user_id AND (v.retired_at IS NOT NULL OR v.pending_delete_at IS NOT NULL)
      AND fn_run_env_run_held(rc.run_id)
) held
ORDER BY held.run_id
LIMIT @max_runs::int;
