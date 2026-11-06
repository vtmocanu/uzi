-- +goose Up

-- Admin-issued, one-time account reset links (PRD #3989). An admin hands the
-- plaintext token to a user out of band; redeeming it deletes the user's model
-- credentials and vault wrapper (vault.Reset) and, for a password account, sets a
-- new password. Only sha256(token) is stored (the worker join-token pattern); the
-- plaintext is shown once at issue.
--
--   kind             'password' (the target has a login password) or 'vault_only'
--                    (OIDC-only). Re-checked against the users row at redeem.
--   target_is_admin  snapshot at issue for the audit trail and the UI warning.
--   redeemed_at      set once, under the link row lock, by the single winner.
--   superseded_at    set when a newer link for the same target is issued, or when
--                    the issuer or target is deactivated or demoted.
--
-- At most one LIVE link per target (unredeemed and unsuperseded): the partial
-- unique index below is the backstop for the issue path, which also takes the
-- target users row FOR UPDATE and supersedes the previous live link first.
-- Expiry is deliberately NOT part of the predicate (now() is not immutable);
-- an expired-but-unsuperseded link is superseded by the next issue, so it never
-- blocks it.
--
-- issued_by uses ON DELETE CASCADE: no query deletes users (accounts are
-- deactivated), so the rule only matters for operator cleanup, and a link whose
-- issuer is gone must not remain redeemable (redeem re-checks the issuer is an
-- active admin anyway), so the link dies with the issuer.
CREATE TABLE account_reset_links (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    kind            text NOT NULL CHECK (kind IN ('password', 'vault_only')),
    target_is_admin boolean NOT NULL,
    issued_by       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    redeemed_at     timestamptz,
    superseded_at   timestamptz
);

CREATE UNIQUE INDEX account_reset_links_one_live_per_user
    ON account_reset_links (user_id)
    WHERE redeemed_at IS NULL AND superseded_at IS NULL;

-- Supersession by issuer (admin deactivated or demoted) scans on this column.
CREATE INDEX account_reset_links_issued_by ON account_reset_links (issued_by);

-- The per-user credentials-reset marker (PRD #3989 module 3): stamped at redeem,
-- read at run claim to scope the credentials_reset park to a reset user.
ALTER TABLE users ADD COLUMN credentials_reset_at timestamptz;

-- +goose Down
ALTER TABLE users DROP COLUMN credentials_reset_at;
DROP TABLE account_reset_links;
