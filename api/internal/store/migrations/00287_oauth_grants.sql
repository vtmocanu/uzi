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
    -- The requester's network buckets, derived by the handler from the trusted client IP
    -- (TRUSTED_PROXIES) and the fairness keys of the live-pending caps, finest first:
    --   source_prefix  IPv6 /64 or IPv4 address (a 6to4 2002::/16 address counts as the IPv4 it embeds),
    --   source_mid     IPv6 /56 or IPv4 /24,
    --   source_wide    IPv6 /48 or IPv4 /24,
    -- each in canonical CIDR (or dotted) form. Several tiers, so neither one /64 nor one delegated
    -- /56 or /48 can fill a product's table and lock its real users out.
    source_prefix    text NOT NULL,
    source_mid       text NOT NULL,
    source_wide      text NOT NULL,
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

-- GET /api/oauth/authorize caps the live pending requests per (product, source) at three network
-- tiers, per product and overall (CountLivePendingOAuthRequests), first lock-free and then again
-- under a per-product advisory lock. Three partial indexes on status = 'pending', one per tier,
-- serve the per-source counts as range scans; the per-product count scans the product_id prefix of
-- whichever of them the planner picks (all three lead with it). The global count is NOT served by them: the planner reads it through
-- idx_oauth_authorize_requests_expires (partial on pending/denied/superseded) with a status
-- filter, and TestOAuthSweepAndCapQueriesUseIndexesLiveDB asserts the index of each subquery.
CREATE INDEX idx_oauth_authorize_requests_pending ON oauth_authorize_requests (product_id, source_prefix, expires_at)
    WHERE status = 'pending';
CREATE INDEX idx_oauth_authorize_requests_pending_mid ON oauth_authorize_requests (product_id, source_mid, expires_at)
    WHERE status = 'pending';
CREATE INDEX idx_oauth_authorize_requests_pending_wide ON oauth_authorize_requests (product_id, source_wide, expires_at)
    WHERE status = 'pending';

-- A grant's unredeemed codes are superseded by name (re-consent, a newer code).
CREATE INDEX idx_oauth_authorize_requests_grant ON oauth_authorize_requests (grant_id) WHERE grant_id IS NOT NULL;

-- NO ACTION: a grant is never hard-deleted except through its user's cascade, which removes the
-- tokens and the grant in one statement.
ALTER TABLE product_tokens
    ADD COLUMN grant_id uuid REFERENCES oauth_grants ON DELETE NO ACTION;

-- Two partial indexes bound what a grant's refresh loop can make the mint checks cost. A product
-- that refreshes and then RFC 7009 revokes the new access token repeatedly keeps its live count at
-- zero and still adds one row per round, so the grant's full row history grows without bound; both
-- per-mint counts (run under the grant lock) must therefore read only the rows they count:
--   * (grant_id, created_at) serves the mint-rate bound (CountGrantTokensMintedSince, revoked rows
--     included) and, as a grant_id prefix, the grant's revoke sweeps and the NO ACTION FK check;
--   * (grant_id, expires_at) WHERE NOT revoked serves the ten-live-token count
--     (CountLiveGrantTokens), which reads only unrevoked rows past now();
--   * (grant_id, last_used_at DESC) WHERE last_used_at IS NOT NULL serves the connections lists'
--     last-used column as a top-1 probe per grant (ListLiveOAuthGrantsForUser/ForProduct), so a
--     grant's never-pruned token history is not read to find its latest use.
CREATE INDEX idx_product_tokens_grant ON product_tokens (grant_id, created_at) WHERE grant_id IS NOT NULL;
CREATE INDEX idx_product_tokens_grant_live ON product_tokens (grant_id, expires_at) WHERE grant_id IS NOT NULL AND NOT revoked;
CREATE INDEX idx_product_tokens_grant_last_used ON product_tokens (grant_id, last_used_at DESC) WHERE grant_id IS NOT NULL AND last_used_at IS NOT NULL;

-- +goose Down
DROP INDEX idx_product_tokens_grant_last_used;
DROP INDEX idx_product_tokens_grant_live;
DROP INDEX idx_product_tokens_grant;
ALTER TABLE product_tokens DROP COLUMN grant_id;
DROP INDEX idx_oauth_authorize_requests_grant;
DROP INDEX idx_oauth_authorize_requests_pending_wide;
DROP INDEX idx_oauth_authorize_requests_pending_mid;
DROP INDEX idx_oauth_authorize_requests_pending;
DROP INDEX idx_oauth_authorize_requests_code_expires;
DROP INDEX idx_oauth_authorize_requests_expires;
DROP TABLE oauth_authorize_requests;
DROP INDEX idx_oauth_grants_product;
DROP INDEX uq_oauth_grants_live;
DROP TABLE oauth_grants;
