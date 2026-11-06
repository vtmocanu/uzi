-- Account reset links (PRD #3989). Lock order everywhere: the vault lifecycle
-- mutex, the lifecycle state locks (advisory secret mutation, workers, runs, users
-- row, wrapper), then the link row. Issue takes only the users rows.

-- name: LockUsersForResetIssue :many
-- Id-ordered FOR UPDATE of the issue target and the issuing admin, so two
-- concurrent issues (or an issue racing a demotion) lock in one global order and
-- serialize on the target. has_password drives the link kind.
SELECT id, email, is_active, is_admin, (password_hash IS NOT NULL)::boolean AS has_password
FROM users
WHERE id = ANY(@ids::uuid[])
ORDER BY id
FOR UPDATE;

-- name: SupersedeLiveResetLinksForTarget :execrows
-- Kills every live link for one target. No expiry term: an expired but
-- unsuperseded link still occupies the one-live-per-user unique index.
UPDATE account_reset_links
SET superseded_at = now()
WHERE user_id = @user_id AND redeemed_at IS NULL AND superseded_at IS NULL;

-- name: InsertResetLink :one
INSERT INTO account_reset_links (user_id, token_hash, kind, target_is_admin, issued_by, expires_at)
VALUES (@user_id, @token_hash, @kind, @target_is_admin, @issued_by, @expires_at)
RETURNING *;

-- name: GetResetLinkState :one
-- Unlocked read by token hash for preview and for the redeem pre-lock lookup. The
-- target and issuer users rows are joined unlocked; callers treat every failed
-- predicate as one generic not-found.
SELECT l.id, l.user_id, l.kind, l.target_is_admin, l.issued_by, l.created_at, l.expires_at,
       l.redeemed_at, l.superseded_at,
       t.is_active AS target_is_active,
       (t.password_hash IS NOT NULL)::boolean AS target_has_password,
       i.is_active AS issuer_is_active,
       i.is_admin AS issuer_is_admin
FROM account_reset_links l
JOIN users t ON t.id = l.user_id
JOIN users i ON i.id = l.issued_by
WHERE l.token_hash = @token_hash;

-- name: LockResetLinkForRedeem :one
-- Same columns as GetResetLinkState, locking only the link row. The joined target
-- and issuer columns are unlocked reads and may be stale. Callers must check
-- redeemed_at and superseded_at on the returned locked row: every issuer or target
-- deactivation or demotion supersedes the live links through this link row
-- (SupersedeLiveResetLinksForUser, called after the users-row update in the same
-- transaction; the caller that writes is_admin or is_active wires that), so a
-- redeem holding the link lock is serialized against it. Inside vault.Reset the
-- target users row is already locked FOR UPDATE (LockVaultAuthState, owner ==
-- target); only the issuer row is unlocked. Locking the issuer row here would
-- invert the users-then-link order of a demoting writer.
SELECT l.id, l.user_id, l.kind, l.target_is_admin, l.issued_by, l.created_at, l.expires_at,
       l.redeemed_at, l.superseded_at,
       t.is_active AS target_is_active,
       (t.password_hash IS NOT NULL)::boolean AS target_has_password,
       i.is_active AS issuer_is_active,
       i.is_admin AS issuer_is_admin
FROM account_reset_links l
JOIN users t ON t.id = l.user_id
JOIN users i ON i.id = l.issued_by
WHERE l.token_hash = @token_hash
FOR UPDATE OF l;

-- name: MarkResetLinkRedeemed :execrows
-- Guarded: zero rows means another redemption already won.
UPDATE account_reset_links
SET redeemed_at = now()
WHERE id = @id AND redeemed_at IS NULL AND superseded_at IS NULL;

-- name: SetUserCredentialsResetAt :exec
UPDATE users SET credentials_reset_at = now() WHERE id = @id;

-- name: DeleteAllUserSecretsForOwner :execrows
-- Every user_secrets row of every kind and sealing marker. FKs null the run,
-- worker and judge bindings and cascade the rate-limit and Codex state rows.
DELETE FROM user_secrets WHERE user_id = @user_id;

-- name: DeleteAllCodexProviderAccountsForOwner :execrows
-- Whole account rows (live and recovery slots together). Cascades the refresh
-- intent, rate-limit and admission-throttle rows keyed on the account.
DELETE FROM codex_provider_account WHERE user_id = @user_id;

-- name: ClearSidebarCodexAccounts :exec
-- sidebar_codex_account_ids has no FK; NULL is the column default (00239).
UPDATE users SET sidebar_codex_account_ids = NULL WHERE id = @id;

-- name: LockOwnerWorkersForReset :many
-- Worker-before-run-before-users matches setRecoveryWaitLocked, so the
-- credential-binding SET NULL cascades below never invert that order.
SELECT id FROM workers
WHERE user_id = @user_id AND anthropic_secret_id IS NOT NULL
ORDER BY id
FOR UPDATE;

-- name: LockOwnerRunsForReset :many
SELECT id FROM runs
WHERE user_id = @user_id
  AND (anthropic_secret_id IS NOT NULL OR codex_secret_id IS NOT NULL
       OR limit_dead_secret_id IS NOT NULL OR credential_override_secret_id IS NOT NULL)
ORDER BY id
FOR UPDATE;

-- name: LockOwnerSecretsForReset :many
-- Run creation share-locks the pinned user_secrets row and then needs KEY SHARE on
-- users (runs_user_id_fkey) for its INSERT. Reset locks users FOR UPDATE, so it
-- must hold the secret rows first (after runs, before users): otherwise its secret
-- DELETE waits on the creator's share lock while the creator waits on users.
SELECT id FROM user_secrets
WHERE user_id = @user_id
ORDER BY id
FOR UPDATE;

-- name: DenyApprovedCLIAuthRequestsForUser :execrows
-- An approved, unconsumed request would otherwise mint a fresh CLI token after the
-- reset. A pending request has no user_id until approval, so this query cannot
-- match it; that case is the caller's concern.
UPDATE cli_auth_requests SET status = 'denied'
WHERE user_id = @user_id AND status = 'approved';

-- name: SupersedeLiveResetLinksForUser :execrows
-- Admin deactivation or demotion: kills every live link the user issued and, when
-- as_target, every live link targeting them.
UPDATE account_reset_links
SET superseded_at = now()
WHERE redeemed_at IS NULL AND superseded_at IS NULL
  AND (issued_by = @id OR (@as_target::boolean AND user_id = @id));
