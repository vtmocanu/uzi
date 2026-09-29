-- Product tokens and the product registry (PRD #1907). Schema and rationale:
-- migrations/00269_product_tokens.sql.
--
-- TOKEN_HASH IS NEVER PROJECTED BY ANY QUERY IN THIS FILE. Every product_tokens read
-- and RETURNING spells its columns out, so the sha256 of a credential is absent from
-- every generated row type and no DTO edit can leak it (the reasoning on
-- ListAllCLITokensForAdmin in cli_tokens.sql, applied to the whole table rather than to
-- the admin list alone). Do not "simplify" a product_tokens query to SELECT * /
-- RETURNING * or sqlc.embed(product_tokens). (The products queries may use *: that
-- table holds no credential material.)
--
-- name: GetProductTokenForAuth :one
-- The /api/v1 auth lookup for a uzp_ Bearer (RequireV1Caller, PRD #1907 D4). Every
-- condition is re-read on EVERY request and every one fails closed (no row):
--   - the token is not revoked;
--   - the token has not expired, carrying the NULL trap verbatim: a bare
--     `expires_at > now()` evaluates to NULL, hence false, for a never-expiring token
--     (D10 allows "never"), i.e. it would silently reject exactly the unattended
--     integration token. Fail closed on revoked/expired, never on a NULL expiry;
--   - the product is enabled and not soft-deleted, so disabling or deleting a product
--     cuts off all of its tokens on the next request (D8, D9);
--   - the owning user is active, so deactivating a user kills their product tokens (D8).
-- Inner joins on purpose: a missing product or user row is also no row.
SELECT t.id,
       t.user_id,
       t.product_id,
       t.scopes,
       p.name AS product_name
  FROM product_tokens t
  JOIN products p ON p.id = t.product_id
  JOIN users u ON u.id = t.user_id
 WHERE t.token_hash = $1
   AND NOT t.revoked
   AND (t.expires_at IS NULL OR t.expires_at > now())
   AND p.enabled
   AND p.deleted_at IS NULL
   AND u.is_active;

-- name: TouchProductToken :exec
-- Coarse (at most once a minute per token) last-used stamp, identical in shape to
-- TouchCLIToken: ONE update sets BOTH last_used_at and last_used_ip, skipped when the
-- row was touched within the last minute. An empty ip string becomes NULL rather than
-- being cast to inet, so a non-IP RemoteAddr fallback can never error the update.
UPDATE product_tokens
   SET last_used_at = now(),
       last_used_ip = NULLIF(sqlc.arg(client_ip)::text, '')::inet
 WHERE id = sqlc.arg(id)
   AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: LockProductTokenMint :exec
-- Serializes one (user, product) pair's mints (the D15 cap: at most 10 active tokens per
-- user per product). Under READ COMMITTED two concurrent mints would each count against
-- their own snapshot and both pass a cap check that was true when each looked; the
-- minting handler therefore runs, in ONE transaction: this lock, then
-- CountActiveProductTokensForUserProduct, then CreateProductToken. The same reasoning as
-- store.HostedProvisionLockClass (migrate.go).
--
-- Two-int advisory lock: class 1970958452 = 0x757A7074 ("uzpt"), the value of
-- store.ProductTokenMintLockClass in migrate.go, distinct from every other class
-- constant there; TestProductTokenMintLockClassMatchesSQL pins this literal to it. The
-- two-int space is disjoint from the one-bigint space RegistrationLockKey /
-- SettingsMutationLockKey use. The objid is hashtext of the
-- (user, product) pair, so two unrelated pairs can collide: a moment of contention,
-- never a correctness problem. XACT-scoped: released on commit or rollback, so it must
-- run on a transaction-bound Queries (on a bare pool it would release immediately).
SELECT pg_advisory_xact_lock(
    1970958452,
    hashtext(sqlc.arg(user_id)::uuid::text || ':' || sqlc.arg(product_id)::uuid::text)
);

-- name: CountActiveProductTokensForUserProduct :one
-- The D15 cap count: tokens of one user for one product that are not revoked and not
-- expired (the NULL trap again: a never-expiring token IS active). Run under
-- LockProductTokenMint, in the minting transaction.
SELECT count(*)
  FROM product_tokens
 WHERE user_id = $1
   AND product_id = $2
   AND NOT revoked
   AND (expires_at IS NULL OR expires_at > now());

-- name: CreateProductToken :one
-- Mint a product token. The SERVER sets expires_at from the user's choice among the
-- offered lifetimes (D10, NULL = never); the client never proposes a timestamp.
INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, user_id, product_id, name, token_prefix, scopes, revoked,
          created_at, last_used_at, last_used_ip, expires_at;

-- name: ListProductTokensForUser :many
-- The per-user product-token list (Settings > Access), metadata only: the value is never
-- stored and the hash is not projected. Joined for the product name. Includes tokens
-- of disabled or soft-deleted products (they still exist, and the user may want to
-- revoke them). Newest first.
SELECT t.id,
       t.product_id,
       p.name AS product_name,
       t.name,
       t.token_prefix,
       t.scopes,
       t.revoked,
       t.created_at,
       t.last_used_at,
       t.last_used_ip,
       t.expires_at
  FROM product_tokens t
  JOIN products p ON p.id = t.product_id
 WHERE t.user_id = $1
 ORDER BY t.created_at DESC, t.id ASC;

-- name: RevokeProductToken :execrows
-- Soft-delete one of the CALLER'S product tokens. Owner-scoped by user_id, so a foreign
-- or unknown id touches zero rows (the handler maps that to 404), never a cross-user
-- revoke.
UPDATE product_tokens SET revoked = true
 WHERE id = $1 AND user_id = $2 AND NOT revoked;

-- name: RevokeAllProductTokens :exec
-- The panic button's product half (D8): revoke every un-revoked product token of one
-- user. Nothing calls it yet: PRD #1907 M5 wires it into the existing revoke-all
-- handler (POST /api/me/cli-tokens/revoke-all), in the SAME transaction as
-- RevokeAllCLITokens. Idempotent, and scoped to $1.
UPDATE product_tokens SET revoked = true WHERE user_id = $1 AND NOT revoked;

-- name: AdminRevokeProductToken :execrows
-- An admin revokes one product token by id (D8). Product tokens are product
-- credentials, not personal CLI credentials, so PRD #64's "an admin never revokes a
-- user's CLI token" rule is unchanged: this touches product_tokens only. Zero rows for
-- an unknown or already-revoked id.
UPDATE product_tokens SET revoked = true WHERE id = $1 AND NOT revoked;

-- name: ListAllProductTokensForAdmin :many
-- The factory-wide product-credential inventory (admin), sibling of
-- ListAllCLITokensForAdmin, and it carries that query's security rule verbatim:
-- COLUMNS ARE PROJECTED EXPLICITLY, AND THAT IS A SECURITY BOUNDARY, NOT A STYLE
-- CHOICE. token_hash is omitted here rather than dropped in the DTO, so it is absent
-- from the generated Go struct and a future DTO edit CANNOT leak it: the sha256 of a
-- credential is offline-crackable, and an admin-wide list of them would turn a
-- visibility feature into a credential-disclosure surface.
--
-- Revoked rows and tokens of soft-deleted products are INCLUDED: they are the incident
-- trail (D9), so an audit view that hid them would hide exactly what an investigation
-- needs. Revoked rows sort last.
SELECT t.id,
       t.user_id,
       u.email AS owner_email,
       t.product_id,
       p.name AS product_name,
       t.name,
       t.token_prefix,
       t.scopes,
       t.revoked,
       t.created_at,
       t.last_used_at,
       t.last_used_ip,
       t.expires_at
  FROM product_tokens t
  JOIN users u ON u.id = t.user_id
  JOIN products p ON p.id = t.product_id
 ORDER BY t.revoked ASC, u.email ASC, t.created_at DESC, t.id ASC;

-- name: CreateProduct :one
-- Register a product (admin). A live product with the same case-insensitive name fails
-- uq_products_live_name (23505; the handler maps it to 409).
INSERT INTO products (name, description, created_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListProducts :many
-- Every product, soft-deleted ones included (admin registry view), each with its count
-- of ACTIVE tokens (not revoked, not expired, the NULL trap spelled out) so the delete
-- confirm can say how many tokens it stops. Live products first, then by name.
SELECT p.id,
       p.name,
       p.description,
       p.enabled,
       p.deleted_at,
       p.created_by,
       p.created_at,
       p.updated_at,
       (SELECT count(*)
          FROM product_tokens t
         WHERE t.product_id = p.id
           AND NOT t.revoked
           AND (t.expires_at IS NULL OR t.expires_at > now()))::bigint AS active_token_count
  FROM products p
 ORDER BY (p.deleted_at IS NOT NULL) ASC, lower(p.name) ASC, p.created_at ASC, p.id ASC;

-- name: ListEnabledProducts :many
-- The user mint picker: only products a token could actually be used with right now.
SELECT *
  FROM products
 WHERE enabled
   AND deleted_at IS NULL
 ORDER BY lower(name) ASC, id ASC;

-- name: GetProduct :one
-- One product by id, soft-deleted included (callers check enabled / deleted_at).
SELECT * FROM products WHERE id = $1;

-- name: CountActiveProductTokensForProduct :one
-- Active (not revoked, not expired) tokens of one product, across all users.
SELECT count(*)
  FROM product_tokens
 WHERE product_id = $1
   AND NOT revoked
   AND (expires_at IS NULL OR expires_at > now());

-- name: UpdateProduct :one
-- Admin edit of the mutable fields (description, enabled). Guarded by
-- deleted_at IS NULL so a soft-deleted product can never be re-enabled (no row: the
-- handler maps it to 404/409); the products_deleted_is_disabled CHECK backs this up.
-- The name is immutable here: it is the label users recognise tokens by.
UPDATE products
   SET description = $2,
       enabled = $3,
       updated_at = now()
 WHERE id = $1
   AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteProduct :execrows
-- Soft delete (D9): set deleted_at and clear enabled in one statement, so every token
-- of the product fails the auth lookup on its next request while the token rows stay
-- for the audit trail. Zero rows for an unknown or already-deleted product.
UPDATE products
   SET deleted_at = now(),
       enabled = false,
       updated_at = now()
 WHERE id = $1
   AND deleted_at IS NULL;
