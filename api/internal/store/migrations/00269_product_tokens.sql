-- +goose Up

-- Product tokens (PRD #1907 M1): an admin registers an external product, and a user
-- mints a route-locked, per-user uzp_ Bearer token for it. The token acts AS the
-- minting user, only on /api/v1 (RequireV1Caller, M2), gated by its scopes and by its
-- product's live state.
--
-- A SEPARATE TABLE, NOT A NEW cli_tokens.scope VALUE (D1). middleware.RequireUser
-- resolves Bearer tokens against cli_tokens only, so a uzp_ token is unknown to it and
-- fails closed on every internal route, current and future, with no per-route deny.
-- Reusing cli_tokens would make that isolation depend on every mount remembering a check.

-- products: the registry of external applications allowed to hold product tokens.
-- Allowance columns (allowed_job_types, site lists) are deliberately absent: each lands
-- with the PRD that creates the capability it allows (D6), so a capability is never
-- usable without its allowance check.
--
-- Delete is SOFT (D9): deleted_at is set and enabled cleared, so every token of the
-- product stops working (the auth lookup re-reads both on each request) while the token
-- rows, last_used_at/last_used_ip and revoked state survive as the audit trail. The
-- CHECK below makes "deleted but still enabled" unrepresentable, so no UPDATE can
-- resurrect a deleted product by flipping enabled alone.
CREATE TABLE products (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 200),
    description text NOT NULL DEFAULT '' CHECK (octet_length(description) <= 1000),
    enabled     boolean NOT NULL DEFAULT true,
    deleted_at  timestamptz,
    -- ON DELETE SET NULL: deleting the admin who registered a product is never blocked
    -- by it (D9).
    created_by  uuid REFERENCES users ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT products_deleted_is_disabled CHECK (deleted_at IS NULL OR NOT enabled)
);

-- Name uniqueness is over LIVE products only, case-insensitively. A soft-deleted
-- product frees its name so an admin can re-register the same application later (the
-- deleted row stays, distinguishable by deleted_at and its own id, for the audit trail).
-- lower(name) because the name is what a user picks from in the mint picker: "Acme" and
-- "acme" side by side would be a confusable pair, not two products.
CREATE UNIQUE INDEX uq_products_live_name ON products (lower(name)) WHERE deleted_at IS NULL;

-- product_tokens: sha256 at rest, exactly like cli_tokens (00067); the plaintext uzp_
-- value is shown once at mint and never stored. Column types and meanings mirror
-- cli_tokens deliberately: token_prefix is the "uzp_a1b2" display stub, revoked is a
-- soft delete that keeps the incident trail and keeps the unique hash poisoned against
-- reuse, last_used_at + last_used_ip are the whole forensic surface (one coarse, at most
-- once per minute, write), and expires_at is server-set with NULL meaning never, so the
-- auth lookup must spell out `expires_at IS NULL OR expires_at > now()` (the NULL trap).
--
-- user_id is the minting user and the ONLY identity the token can act as (D7).
-- ON DELETE CASCADE as for cli_tokens. product_id is ON DELETE RESTRICT (D9): a hard
-- product delete cannot silently erase the token audit trail.
--
-- scopes is a non-empty subset of the known scope vocabulary (D5), mirrored by
-- producttoken.Scopes in Go. The array must also carry no NULL element: `<@` treats a
-- NULL element as unmatched, and array_position(..., NULL) spells that out rather than
-- relying on it.
CREATE TABLE product_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    product_id   uuid NOT NULL REFERENCES products ON DELETE RESTRICT,
    name         text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 200),
    token_hash   bytea NOT NULL UNIQUE,          -- sha256(uzp_...), mirrors cli_tokens.token_hash
    token_prefix text NOT NULL,                  -- "uzp_a1b2" display stub
    scopes       text[] NOT NULL,
    revoked      boolean NOT NULL DEFAULT false, -- soft delete: keeps the incident trail
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    last_used_ip inet,                           -- same at-most-once-a-minute write as last_used_at
    expires_at   timestamptz,                    -- server-set; NULL = never (D10)
    CONSTRAINT product_tokens_scopes_nonempty CHECK (cardinality(scopes) > 0),
    CONSTRAINT product_tokens_scopes_known CHECK (
        scopes <@ ARRAY['jobs:run', 'jobs:read']::text[]
        AND array_position(scopes, NULL) IS NULL
    )
);

-- The auth point-read keys on token_hash (its UNIQUE constraint indexes it). This index
-- serves the per-user list, the per-user revoke-all and the per-(user, product)
-- active-token count, all of which filter user_id (and revoked).
CREATE INDEX idx_product_tokens_user ON product_tokens (user_id, revoked);

-- Serves the per-product active-token count (the admin delete confirm) and the FK
-- check ON DELETE RESTRICT runs when a product row is deleted.
CREATE INDEX idx_product_tokens_product ON product_tokens (product_id);

-- +goose Down
DROP INDEX idx_product_tokens_product;
DROP INDEX idx_product_tokens_user;
DROP TABLE product_tokens;
DROP INDEX uq_products_live_name;
DROP TABLE products;
