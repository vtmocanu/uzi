-- +goose Up

-- PRD #1910 M2: the consent half of uzi's OAuth authorization server. Three additions:
--
--   * oauth_grants: one LIVE grant per (user, product) (the partial unique index), holding the
--     approved scopes and, from M3/M4 on, the one live refresh token (sha256 + display prefix,
--     so there is no refresh-token table). Revocation is soft (revoked_at) and keeps the row as
--     the audit trail. consented_at is set on the first consent AND on every re-consent: the
--     90-day absolute refresh expiry counts from it, not from created_at (row creation).
--   * oauth_authorize_requests: the pending request GET /api/oauth/authorize stores, and the
--     authorization code once approved (sha256 at rest, 60 s, single use). pending -> approved
--     -> redeemed, or denied / superseded. binding_hash is the sha256 of the nonce in the
--     browser's binding cookie, so a /connect link opened in another browser cannot read or
--     approve the request.
--   * product_tokens.grant_id: an access token minted for a grant. NULL for every pasted token.
--
-- Additive: no existing row changes.

CREATE TABLE oauth_grants (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ON DELETE CASCADE: deleting a user removes their grants (and, through
    -- oauth_authorize_requests.user_id and product_tokens.user_id, everything under them).
    user_id               uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    -- ON DELETE RESTRICT, as product_tokens: a hard product delete cannot erase the trail.
    product_id            uuid NOT NULL REFERENCES products ON DELETE RESTRICT,
    scopes                text[] NOT NULL,
    refresh_token_hash    bytea UNIQUE,
    refresh_token_prefix  text,
    refresh_issued_at     timestamptz,
    refresh_last_used_at  timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    consented_at          timestamptz NOT NULL DEFAULT now(),
    revoked_at            timestamptz,
    CONSTRAINT oauth_grants_scopes_nonempty CHECK (cardinality(scopes) > 0),
    CONSTRAINT oauth_grants_scopes_known CHECK (
        scopes <@ ARRAY['jobs:run', 'jobs:read']::text[]
        AND array_position(scopes, NULL) IS NULL
    )
);

-- At most one live grant per (user, product): approve inserts conflict-safely against this index.
CREATE UNIQUE INDEX uq_oauth_grants_live ON oauth_grants (user_id, product_id) WHERE revoked_at IS NULL;

-- Serves the product-side lookups (admin connection list, delete-product counts) and the FK check.
CREATE INDEX idx_oauth_grants_product ON oauth_grants (product_id);

CREATE TABLE oauth_authorize_requests (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id       uuid NOT NULL REFERENCES products ON DELETE RESTRICT,
    redirect_uri     text NOT NULL,
    scopes           text[] NOT NULL,
    state            text NOT NULL,
    code_challenge   text NOT NULL,
    binding_hash     bytea NOT NULL,
    status           text NOT NULL DEFAULT 'pending',
    user_id          uuid REFERENCES users ON DELETE CASCADE,
    grant_id         uuid REFERENCES oauth_grants,
    code_hash        bytea UNIQUE,
    code_expires_at  timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    CONSTRAINT oauth_authorize_requests_status_check CHECK (
        status IN ('pending', 'approved', 'redeemed', 'denied', 'superseded')
    ),
    CONSTRAINT oauth_authorize_requests_scopes_known CHECK (
        cardinality(scopes) > 0
        AND scopes <@ ARRAY['jobs:run', 'jobs:read']::text[]
        AND array_position(scopes, NULL) IS NULL
    )
);

-- The TTL sweep (DeleteExpiredOAuthAuthorizeRequests) is two ORed range predicates, one per index,
-- so the planner can BitmapOr them instead of scanning the table:
--   * rows with no live code (pending, denied, superseded) go once past expires_at; the index is
--     partial on exactly those statuses, so approved and redeemed rows (the long-lived ones) are
--     not in it;
--   * rows carrying a code (approved, redeemed) go once code_expires_at is more than 10 minutes old.
CREATE INDEX idx_oauth_authorize_requests_expires ON oauth_authorize_requests (expires_at)
    WHERE status IN ('pending', 'denied', 'superseded');
CREATE INDEX idx_oauth_authorize_requests_code_expires ON oauth_authorize_requests (code_expires_at)
    WHERE code_expires_at IS NOT NULL;

-- GET /api/oauth/authorize caps the live pending requests per product and overall before it
-- inserts (CountLivePendingOAuthRequests): the per-product count is an index range scan here, and
-- the global one is a bounded scan of the same partial index.
CREATE INDEX idx_oauth_authorize_requests_pending ON oauth_authorize_requests (product_id, expires_at)
    WHERE status = 'pending';

-- A grant's unredeemed codes are superseded by name (re-consent, a newer code).
CREATE INDEX idx_oauth_authorize_requests_grant ON oauth_authorize_requests (grant_id) WHERE grant_id IS NOT NULL;

-- NO ACTION: a grant is never hard-deleted except through its user's cascade, which removes the
-- tokens and the grant in one statement.
ALTER TABLE product_tokens
    ADD COLUMN grant_id uuid REFERENCES oauth_grants ON DELETE NO ACTION;

CREATE INDEX idx_product_tokens_grant ON product_tokens (grant_id) WHERE grant_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_product_tokens_grant;
ALTER TABLE product_tokens DROP COLUMN grant_id;
DROP INDEX idx_oauth_authorize_requests_grant;
DROP INDEX idx_oauth_authorize_requests_pending;
DROP INDEX idx_oauth_authorize_requests_code_expires;
DROP INDEX idx_oauth_authorize_requests_expires;
DROP TABLE oauth_authorize_requests;
DROP INDEX idx_oauth_grants_product;
DROP INDEX uq_oauth_grants_live;
DROP TABLE oauth_grants;
