-- +goose Up

-- CLI browser-brokered auth requests (PRD #64 M5): the short-lived, poll-based
-- handshake behind `uzi login`. The CLI mints a PKCE verifier, sends only its
-- code_challenge (base64url(S256(verifier))) here, and polls; the human approves in
-- an already-authenticated browser tab (where password OR OIDC login happens). No
-- loopback listener, no session JWT in a URL — so it works over SSH and in
-- containers, where a redirect-listener flow does not.
--
-- The token is NEVER stored on this row, not even sealed: approve only marks the row
-- (status='approved', user_id from the session, scope from the consent screen), and
-- the token is minted claim-first INSIDE the poll transaction and returned once. So a
-- DB dump contains no CLI token in any form.
--
--   code_challenge  base64url(S256(verifier)); the poll re-derives S256 of the
--                   presented verifier and must match this, in the SAME tx as the
--                   claim, rolling back on mismatch (never marking denied).
--   client_desc     hostname/os shown on the consent screen so the human knows what
--                   they are approving. UNTRUSTED display text.
--   user_code       an anti-phishing confirmation the human TYPES from their own
--                   terminal (Crockford base32, rendered XXXX-XXXX). UNIQUE only to
--                   make a collision a loud insert failure the handler retries — it
--                   is NEVER a lookup key (the request_id in the URL is), which is the
--                   invariant that keeps its ~40 bits sufficient. Do NOT add a
--                   "enter your code at /device" entry point without revisiting that.
--   status          pending → approved → consumed (success), or pending → denied. The
--                   consumed transition is the claim-first mint guard.
--   user_id         NULL until approve, then the session user (nullable by design).
--   scope           chosen on the consent screen at approve (default 'user'); the
--                   server applies the expiry matrix at mint, the client never
--                   proposes a lifetime.
--   expires_at      NOT NULL, ~5 min. NOT NULL here (unlike cli_tokens) so the NULL
--                   trap does not apply — every guard can use a bare `expires_at >
--                   now()`.
CREATE TABLE cli_auth_requests (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code_challenge text NOT NULL,                -- base64url(S256(verifier))
    client_desc    text NOT NULL,                -- hostname/os, shown on the consent screen
    user_code      text NOT NULL UNIQUE,         -- anti-phishing confirmation code (typed, never a lookup key)
    status         text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'approved', 'denied', 'consumed')),
    user_id        uuid REFERENCES users ON DELETE CASCADE,   -- set at approve
    scope          text NOT NULL DEFAULT 'user'
                   CHECK (scope IN ('user', 'admin_ro')),     -- chosen on the consent screen
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL          -- ~5 min
);

-- +goose Down
DROP TABLE cli_auth_requests;
