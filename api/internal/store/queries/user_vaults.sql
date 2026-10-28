-- name: GetUserVaultState :one
SELECT vault_mode, vault_epoch FROM users WHERE id = $1;

-- name: LockUserVaultState :one
SELECT vault_mode, vault_epoch FROM users WHERE id = $1 FOR UPDATE;

-- name: LockVaultAuthState :one
-- Lifecycle operations lock the durable mode/epoch and current login hash together.
SELECT vault_mode, vault_epoch, password_hash FROM users WHERE id = $1 FOR UPDATE;

-- name: LockUserVault :one
SELECT * FROM user_vaults WHERE user_id = $1 FOR UPDATE;

-- name: AdvanceUserVaultMode :one
UPDATE users SET vault_mode = @vault_mode, vault_epoch = vault_epoch + 1
WHERE id = @id RETURNING vault_mode, vault_epoch;

-- name: CountUnprotectedUsers :one
SELECT count(*) FROM users WHERE vault_mode = 'unprotected';

-- name: GetUserVault :one
-- The user's KEK salt + wrapped DEK. The plaintext DEK is never selected (it is
-- never stored); the caller derives the KEK from the login password and unwraps
-- in memory. pgx.ErrNoRows means no wrapper; explicit enable or required
-- enrollment may create one.
SELECT * FROM user_vaults WHERE user_id = $1;

-- name: CreateUserVaultIfAbsent :one
-- Lifecycle creation holds the owner users-row lock before checking wrapper
-- presence. DO NOTHING defensively prevents replacing a live DEK; a conflicting
-- insert returns pgx.ErrNoRows and aborts the lifecycle transaction.
-- The independent KEK salt prevents password_hash from supplying the wrapping key.
INSERT INTO user_vaults (user_id, kek_salt, wrapped_dek)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO NOTHING
RETURNING *;

-- name: DeleteUserVault :execrows
-- Drops ONLY the wrapper. The vault-disable transaction must first reseal all
-- owned secret and account slots under master_aad while holding the owner fence.
-- Deleting a wrapper with remaining DEK material would strand those credentials.
DELETE FROM user_vaults WHERE user_id = $1;

-- name: ListUsersNeedingVaultLockNotice :many
-- PRD #890 M1: the users the boot reconciler should DM about their locked vault.
-- A user qualifies when ALL hold:
--   • they have a user_vaults row (they actually use personal secrets);
--   • they are Slack-deliverable — the same gate GetSlackDeliveryForUser uses:
--     slack_notify = true AND slack_link_confirmed_at IS NOT NULL AND
--     slack_resolved_id IS NOT NULL;
--   • they have NOT already been notified for this lock-episode
--     (user_vaults.lock_notified_at IS NULL — the dedup key);
--   • they have work the lock actually blocks — mirroring what the claim gates
--     withhold, NOT "any non-terminal work": ≥1 run in queued/awaiting_approval/
--     awaiting_input (ClaimRun claims only status='queued'; the other two return to
--     queued on resume), OR ≥1 schedule that will really fire (enabled AND
--     status='active' AND next_fire_at IS NOT NULL, mirroring ClaimDueSchedules).
-- The WHERE stays fast via the EXISTS pair; the two scalar COUNT subqueries then
-- carry the pending-work totals for the notification's Facts line (closed ints).
SELECT
    u.id,
    u.slack_resolved_id,
    (SELECT count(*) FROM runs r
        WHERE r.user_id = u.id
          AND r.status IN ('queued', 'awaiting_approval', 'awaiting_input'))::bigint AS pending_runs,
    (SELECT count(*) FROM run_schedules s
        WHERE s.user_id = u.id
          AND s.enabled AND s.status = 'active' AND s.next_fire_at IS NOT NULL)::bigint AS pending_schedules
FROM user_vaults v
JOIN users u ON u.id = v.user_id
WHERE v.lock_notified_at IS NULL
  AND u.slack_notify = true
  AND u.slack_link_confirmed_at IS NOT NULL
  AND u.slack_resolved_id IS NOT NULL
  AND (
        EXISTS (
            SELECT 1 FROM runs r
            WHERE r.user_id = u.id
              AND r.status IN ('queued', 'awaiting_approval', 'awaiting_input')
        )
     OR EXISTS (
            SELECT 1 FROM run_schedules s
            WHERE s.user_id = u.id
              AND s.enabled AND s.status = 'active' AND s.next_fire_at IS NOT NULL
        )
      )
ORDER BY u.id;

-- name: ClaimVaultLockNotice :one
-- PRD #890 M1: atomically claim the notify slot for one user. Only the row that
-- comes back has been claimed by THIS caller, so N api pods booting together send
-- exactly ONE DM per user — the mark is set before Notify runs (at-most-once dedup).
-- A user already marked returns no row (pgx.ErrNoRows) and is not re-sent.
UPDATE user_vaults SET lock_notified_at = now()
WHERE user_id = $1 AND lock_notified_at IS NULL
RETURNING user_id;

-- name: ClearVaultLockNotice :exec
-- PRD #890 M1: re-arm the notice on a successful unlock, ending the lock-episode so a
-- later deploy that locks the user again can notify afresh. The AND lock_notified_at
-- IS NOT NULL guard makes this a no-op write on the hot login path when already clear.
UPDATE user_vaults SET lock_notified_at = NULL
WHERE user_id = $1 AND lock_notified_at IS NOT NULL;
