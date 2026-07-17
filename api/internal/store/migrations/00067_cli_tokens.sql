-- +goose Up

-- CLI tokens (PRD #64): the Bearer credential the `uzi` CLI presents so humans and
-- agents can drive the factory headlessly. sha256 at rest exactly like a worker
-- join token (jointoken / workers.token_hash) — the plaintext is shown once at mint
-- and never stored. Two class prefixes mark the kind for humans and secret scanners:
-- uzc_ (a user token, capped to its owner's own authority) and uza_ (admin_ro,
-- read-only reach across the whole factory).
--
-- scope is a CEILING, not a label: it is enforced live in middleware.RequireUser,
-- which hands any non-admin_ro token a COPY of the user row with is_admin cleared,
-- so an admin's default-scope uzc_ is indistinguishable from a non-admin's token
-- everywhere — not merely under /api/admin/*. admin-ness is always read live from
-- the user row (never the credential), so demoting an admin instantly neuters their
-- uza_ token with no revocation step.
--
-- Diverges from `workers` (token_hash only, hard delete) on purpose (Decision 10):
-- a worker is disposable infrastructure, a CLI token is a human-held credential with
-- an incident story, so it keeps:
--   token_prefix  the "uzc_a1b2" display stub (4 chars after the underscore) — the
--                 only way to name a row in the list without revealing the token.
--   revoked       soft delete: keeps the incident trail AND keeps the unique hash
--                 permanently poisoned against reuse.
--   last_used_at  + last_used_ip: the ENTIRE forensic surface — there is no
--                 per-request audit log (Risk 8). last_used_ip is additionally the
--                 only detection control ("was it used by someone who isn't me?").
--                 Both are written on the same coarse (≤1/min) update.
--   expires_at    nullable, server-set per the expiry matrix. NULL is the
--                 never-expiring webui-minted agent/CI token, which is exactly why
--                 the auth lookup must spell out `expires_at IS NULL OR expires_at >
--                 now()`: a bare `expires_at > now()` is NULL — hence false — for
--                 precisely the token this table exists to serve (the NULL trap).
CREATE TABLE cli_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name         text NOT NULL,
    token_hash   bytea NOT NULL UNIQUE,          -- sha256(uzc_…/uza_…), mirrors workers.token_hash
    token_prefix text NOT NULL,                  -- "uzc_a1b2" display stub (multica 011)
    scope        text NOT NULL DEFAULT 'user'
                 CHECK (scope IN ('user', 'admin_ro')),
    revoked      boolean NOT NULL DEFAULT false, -- soft delete: keeps the incident trail
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    last_used_ip inet,                           -- the only detection control; same ≤1/min write
    expires_at   timestamptz                     -- server-set; NULL = never (webui agent/CI token)
);

-- The auth point-read keys on token_hash (its UNIQUE constraint already indexes it).
-- This index serves the per-user list (GET /api/me/cli-tokens) and the revoke-all
-- UPDATE, both of which filter (user_id, revoked).
CREATE INDEX idx_cli_tokens_user ON cli_tokens (user_id, revoked);

-- +goose Down
DROP INDEX idx_cli_tokens_user;
DROP TABLE cli_tokens;
