-- +goose Up

-- PRD #1910 M1: a product becomes an OAuth client (client_id = the product id). The admin sets the
-- exact-match redirect URIs and the allowed scope list, and rotates a confidential client secret
-- (uzs_ class, sha256 at rest, shown once). Additive: every existing product defaults to "not a
-- client" (no URIs, no scopes, no secret), and pasted uzp_ tokens are unaffected.
--
-- The CHECKs are the database half of oauthsrv's validation (at most 5 URIs, scopes drawn from the
-- product-token scope vocabulary); the URI shape itself is validated in Go only. The secret hash
-- and its display prefix are set and cleared together.
ALTER TABLE products
    ADD COLUMN redirect_uris              text[]      NOT NULL DEFAULT '{}'
        CONSTRAINT products_redirect_uris_check CHECK (cardinality(redirect_uris) <= 5),
    ADD COLUMN oauth_scopes               text[]      NOT NULL DEFAULT '{}'
        CONSTRAINT products_oauth_scopes_check CHECK (oauth_scopes <@ ARRAY['jobs:run', 'jobs:read']::text[]),
    ADD COLUMN client_secret_hash         bytea,
    ADD COLUMN client_secret_prefix       text,
    ADD COLUMN client_secret_rotated_at   timestamptz,
    ADD CONSTRAINT products_client_secret_pair_check
        CHECK ((client_secret_hash IS NULL) = (client_secret_prefix IS NULL));

-- +goose Down
ALTER TABLE products
    DROP CONSTRAINT products_client_secret_pair_check,
    DROP COLUMN client_secret_rotated_at,
    DROP COLUMN client_secret_prefix,
    DROP COLUMN client_secret_hash,
    DROP COLUMN oauth_scopes,
    DROP COLUMN redirect_uris;
