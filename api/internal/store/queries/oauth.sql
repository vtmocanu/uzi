-- PRD #1910 M2: the consent half of the OAuth authorization server (oauth_grants and
-- oauth_authorize_requests, migration 00287). Lock order everywhere (D8): the user's grant-creation
-- advisory lock first, on the two paths that create or sweep a user's grants (approve and Revoke
-- all; LockOAuthUserGrants), then, on approve and deny only, the product row FOR SHARE
-- (GetProductForShare, the registration re-check; deny takes no per-user lock), then the grant
-- row (one user's grants in ascending id order), then that grant's product_tokens rows, then its
-- oauth_authorize_requests rows. The approve transaction claims the request it decides LAST
-- (after the grant lock, the narrowing revoke and the superseding of the grant's earlier codes),
-- so it never holds a request row while waiting for a grant. Only the TTL sweep and deny lock
-- request rows without a grant lock (deny after the product share lock); neither ever waits for a
-- grant.

-- name: CreateOAuthAuthorizeRequest :one
-- Store the pending request GET /api/oauth/authorize validated. The caller has already capped and
-- validated every field (oauthsrv), so nothing here is attacker-sized. binding_hash is the sha256
-- of the browser-binding cookie nonce; the plaintext nonce is never stored.
INSERT INTO oauth_authorize_requests (product_id, redirect_uri, scopes, state, code_challenge, binding_hash, source_prefix, source_mid, source_wide, expires_at)
VALUES (sqlc.arg(product_id), sqlc.arg(redirect_uri), sqlc.arg(scopes)::text[], sqlc.arg(state), sqlc.arg(code_challenge), sqlc.arg(binding_hash)::bytea, sqlc.arg(source_prefix), sqlc.arg(source_mid), sqlc.arg(source_wide), sqlc.arg(expires_at))
RETURNING *;

-- name: GetOAuthAuthorizeRequest :one
-- The request by id (the URL-carried key). The handler compares binding_hash in constant time
-- before exposing anything.
SELECT * FROM oauth_authorize_requests WHERE id = $1;

-- name: ClaimOAuthAuthorizeRequest :one
-- The single-use approve claim: pending -> approved in one conditional UPDATE, binding the row to
-- the approving user. Zero rows (pgx.ErrNoRows) means not pending, expired or unknown: a second
-- approve, an approve after deny, or a race lost; the handler maps it to 409. Runs inside the
-- approve transaction, after the grant lock (lock order), so a later failure or a lost claim rolls
-- the whole approve back and the request stays pending.
UPDATE oauth_authorize_requests
   SET status = 'approved', user_id = sqlc.arg(user_id)
 WHERE id = sqlc.arg(id) AND status = 'pending' AND expires_at > now()
RETURNING *;

-- name: DenyOAuthAuthorizeRequest :one
-- pending -> denied, guarded like the claim; zero rows is a 409 in the handler.
UPDATE oauth_authorize_requests
   SET status = 'denied', user_id = sqlc.arg(user_id)
 WHERE id = sqlc.arg(id) AND status = 'pending' AND expires_at > now()
RETURNING *;

-- name: InsertOAuthGrant :one
-- First consent: insert the live grant, conflict-safe against uq_oauth_grants_live. On a conflict
-- no row is returned (pgx.ErrNoRows) and the handler locks the live grant with a SEPARATE
-- statement (LockLiveOAuthGrant): an ON CONFLICT DO NOTHING can meet a row its own statement
-- snapshot cannot see (PRD #1910 D8).
INSERT INTO oauth_grants (user_id, product_id, scopes)
VALUES (sqlc.arg(user_id), sqlc.arg(product_id), sqlc.arg(scopes)::text[])
ON CONFLICT (user_id, product_id) WHERE revoked_at IS NULL DO NOTHING
RETURNING *;

-- name: LockLiveOAuthGrant :one
-- The user's live grant for the product, row-locked for the rest of the transaction. No row means
-- there is none (or it was revoked meanwhile); the caller retries the insert.
SELECT * FROM oauth_grants
 WHERE user_id = sqlc.arg(user_id) AND product_id = sqlc.arg(product_id) AND revoked_at IS NULL
 FOR UPDATE;

-- name: ReconsentOAuthGrant :one
-- Re-consent on a LOCKED live grant: replace the scopes with the newly approved set, stamp
-- consented_at (the 90-day absolute refresh expiry counts from it) and clear the refresh token, so
-- the previous one stops working. Guarded on revoked_at IS NULL as a belt on the lock.
UPDATE oauth_grants
   SET scopes = sqlc.arg(scopes)::text[],
       consented_at = now(),
       refresh_token_hash = NULL,
       refresh_token_prefix = NULL,
       refresh_issued_at = NULL,
       refresh_last_used_at = NULL
 WHERE id = sqlc.arg(id) AND revoked_at IS NULL
RETURNING *;

-- name: OAuthGrantHasTokenOutsideScopes :one
-- True when some unrevoked access token of the grant, expired or not, holds a scope that is not in
-- the given set, i.e. the new scope set drops something a token carries. Re-consent revokes the
-- grant's access tokens only then (PRD #1910 D6). Expiry is deliberately ignored: an expired
-- jobs:run token may have created a still-running job, and the ListRevokedProductJobs sweep cancels
-- by revoked, not by expiry. Run with the grant already locked.
SELECT EXISTS (
    SELECT 1
      FROM product_tokens
     WHERE grant_id = sqlc.arg(grant_id)
       AND NOT revoked
       AND NOT (scopes <@ sqlc.arg(scopes)::text[])
) AS drops_scope;

-- name: RevokeOAuthGrantProductTokens :execrows
-- Set revoked on every unrevoked access token of the grant, expired or not, so the existing
-- ListRevokedProductJobs sweep cancels the jobs they created. Run with the grant already locked
-- (lock order: grant, then product_tokens).
UPDATE product_tokens SET revoked = true WHERE grant_id = $1 AND NOT revoked;

-- name: SupersedeOAuthGrantCodes :execrows
-- Mark the grant's issued-but-unredeemed codes superseded (a re-consent or a newer code replaces
-- them). Run with the grant already locked, after its product_tokens (lock order).
UPDATE oauth_authorize_requests
   SET status = 'superseded'
 WHERE grant_id = $1 AND status = 'approved';

-- name: IssueOAuthAuthorizationCode :one
-- Attach the authorization code to the request the approve just claimed: sha256 at rest, 60 s
-- expiry, bound to the grant. Guarded on status='approved' and the approving user.
UPDATE oauth_authorize_requests
   SET code_hash = sqlc.arg(code_hash)::bytea,
       code_expires_at = sqlc.arg(code_expires_at),
       grant_id = sqlc.arg(grant_id)
 WHERE id = sqlc.arg(id) AND status = 'approved' AND user_id = sqlc.arg(user_id)
RETURNING *;

-- name: DeleteExpiredOAuthAuthorizeRequests :execrows
-- TTL sweep (beside DeleteExpiredCLIAuthRequests, in the handler's start path and on the server's
-- sweeper ticker). pending, denied and superseded rows go once past expires_at. approved and
-- redeemed rows carry a code M3's replay detection (RFC 6749 section 4.1.2) must still recognise,
-- so they live until their code expired more than 10 minutes ago. Two ORed range predicates, one
-- per index (idx_oauth_authorize_requests_expires, _code_expires), so the planner can BitmapOr
-- them; a CASE over status could use neither.
DELETE FROM oauth_authorize_requests
 WHERE (status IN ('pending', 'denied', 'superseded') AND expires_at < now())
    OR (status IN ('approved', 'redeemed') AND code_expires_at < now() - interval '10 minutes');

-- name: LockOAuthAuthorize :exec
-- Serializes one product's authorize inserts, so the pending caps cannot be passed by concurrent
-- requests under READ COMMITTED (each would count against its own snapshot): the authorize
-- handler runs, in ONE transaction, SetOAuthAuthorizeLockTimeout, this lock, then a recount with CountLivePendingOAuthRequests, then
-- CreateOAuthAuthorizeRequest. The same reasoning as LockProductTokenMint.
--
-- Two-int advisory lock: class 1970958177 = 0x757A6F61 ("uzoa"), the value of
-- store.OAuthAuthorizeLockClass in migrate.go, distinct from every other class constant there
-- (TestOAuthAuthorizeLockClassMatchesSQL pins this literal and TestProductTokenMintLockClassMatchesSQL
-- enumerates the collision check). The objid is hashtext of the product id, so two products can
-- collide: a moment of contention, never a correctness problem. XACT-scoped: released on commit
-- or rollback, so it must run on a transaction-bound Queries.
SELECT pg_advisory_xact_lock(
    1970958177,
    hashtext(sqlc.arg(product_id)::uuid::text)
);

-- name: CountLivePendingOAuthRequests :one
-- How many live (pending, unexpired) authorize requests exist for the product's three source
-- tiers (finest /64 or address, then /56 or /24, then /48 or /24), for the product and overall,
-- each counted up to its limit so the work is bounded however many rows an attacker piles up.
-- The unauthenticated authorize endpoint checks them against its caps (the source tiers are the
-- fairness bounds; product and global are storage backstops) BEFORE inserting, first without
-- the lock (a refusal never queues on it) and again under LockOAuthAuthorize. The subqueries
-- are in this order: the three pending indexes serve the first four, and
-- idx_oauth_authorize_requests_expires the global one (store test asserts each).
SELECT
    (SELECT count(*) FROM (
        SELECT 1 FROM oauth_authorize_requests
         WHERE status = 'pending' AND expires_at > now()
           AND oauth_authorize_requests.product_id = sqlc.arg(for_product)
           AND oauth_authorize_requests.source_prefix = sqlc.arg(for_prefix)
         LIMIT sqlc.arg(prefix_limit)::int
    ) s)::bigint AS prefix_pending,
    (SELECT count(*) FROM (
        SELECT 1 FROM oauth_authorize_requests
         WHERE status = 'pending' AND expires_at > now()
           AND oauth_authorize_requests.product_id = sqlc.arg(for_product)
           AND oauth_authorize_requests.source_mid = sqlc.arg(for_mid)
         LIMIT sqlc.arg(mid_limit)::int
    ) m)::bigint AS mid_pending,
    (SELECT count(*) FROM (
        SELECT 1 FROM oauth_authorize_requests
         WHERE status = 'pending' AND expires_at > now()
           AND oauth_authorize_requests.product_id = sqlc.arg(for_product)
           AND oauth_authorize_requests.source_wide = sqlc.arg(for_wide)
         LIMIT sqlc.arg(wide_limit)::int
    ) w)::bigint AS wide_pending,
    (SELECT count(*) FROM (
        SELECT 1 FROM oauth_authorize_requests
         WHERE status = 'pending' AND expires_at > now() AND oauth_authorize_requests.product_id = sqlc.arg(for_product)
         LIMIT sqlc.arg(product_limit)::int
    ) p)::bigint AS product_pending,
    (SELECT count(*) FROM (
        SELECT 1 FROM oauth_authorize_requests
         WHERE status = 'pending' AND expires_at > now()
         LIMIT sqlc.arg(global_limit)::int
    ) g)::bigint AS global_pending;

-- name: SetOAuthAuthorizeLockTimeout :exec
-- Bounds the wait of the LockOAuthAuthorize that follows, in this transaction only (set_config
-- is_local = true): a lock held too long answers the authorize request temporarily_unavailable
-- (SQLSTATE 55P03) instead of pinning an API pool connection.
SELECT set_config('lock_timeout', sqlc.arg(timeout)::text, true);

-- PRD #1910 M3: the token endpoint's authorization_code redemption. The handler runs it in ONE
-- transaction in D8 lock order: the unlocked code lookup (no lock), the grant FOR UPDATE, an
-- unlocked re-read of the request under that lock (the grant lock serializes every grant-keyed
-- writer of the request, and the redeem UPDATE is itself conditional), the grant's token count,
-- the access-token insert and the refresh-token store, and only LAST the conditional redeem
-- UPDATE, which is the first lock on the request row. The replay arm revokes through
-- revokeGrantLocked (grant, then its tokens, then its requests) and never locks the request row
-- first. A failed check returns without writing, so nothing is consumed on failure.

-- name: GetOAuthAuthorizeRequestByCodeHash :one
-- The request an authorization code belongs to, found by the sha256 of the presented code. NO row
-- lock: the handler needs the grant id to take the grant lock first (D8), then re-reads the request
-- with GetOAuthAuthorizeRequest (still without a row lock).
SELECT * FROM oauth_authorize_requests WHERE code_hash = $1;

-- name: LockOAuthGrant :one
-- A grant by id, row-locked for the rest of the transaction (D8: every grant mutation serializes
-- here first). No row means the grant is gone (its user was deleted).
SELECT * FROM oauth_grants WHERE id = $1 FOR UPDATE;

-- name: CountLiveGrantTokens :one
-- The ten-live-token bound (D5): the grant's unrevoked, unexpired access tokens and the earliest
-- expiry among them (what Retry-After counts down to). Grant tokens always carry an expiry, so
-- there is no NULL-expiry case here, unlike CountActiveProductTokensForUserProduct. Run with the
-- grant locked, so no concurrent mint of this grant can change the count.
SELECT count(*)::bigint AS live, min(expires_at)::timestamptz AS oldest_expires_at
  FROM product_tokens
 WHERE grant_id = $1
   AND NOT revoked
   AND expires_at > now();

-- name: CountGrantTokensMintedSince :one
-- The per-grant mint-rate bound: how many access tokens were minted for the grant in the last
-- window_seconds (revoked and expired rows included, so revoking a fresh token buys no extra
-- mint), and the seconds until the oldest of them leaves the window (what Retry-After counts down
-- to). Run with the grant locked, so no concurrent mint of this grant can change the count. Served
-- by idx_product_tokens_grant (grant_id, created_at): it reads only the window's rows, never the
-- grant's whole history. The window is measured on the database clock.
SELECT count(*)::bigint AS minted,
       GREATEST(1, COALESCE(ceil(extract(epoch FROM (min(created_at) + sqlc.arg(window_seconds)::bigint * interval '1 second' - now()))), 1))::int AS retry_after_seconds
  FROM product_tokens
 WHERE grant_id = sqlc.arg(grant_id)
   AND created_at > now() - sqlc.arg(window_seconds)::bigint * interval '1 second';

-- name: OAuthGrantRefreshWithinLifetimes :one
-- Whether the grant's refresh token is inside both lifetimes (PRD #1910 D5), evaluated on the
-- DATABASE clock so application and database clock skew cannot shift either bound: idle (from the
-- last refresh, else from issue) at most idle_seconds ago, and the user's latest consent at most
-- absolute_seconds ago. A grant with no refresh issue time is outside. Run with the grant locked.
SELECT COALESCE(
         COALESCE(refresh_last_used_at, refresh_issued_at) >= now() - sqlc.arg(idle_seconds)::bigint * interval '1 second'
         AND consented_at >= now() - sqlc.arg(absolute_seconds)::bigint * interval '1 second',
         false)::boolean AS within
  FROM oauth_grants
 WHERE id = sqlc.arg(id);

-- name: RedeemOAuthAuthorizeRequest :one
-- approved -> redeemed in one conditional UPDATE, the code's single use. Zero rows (ErrNoRows)
-- means it was no longer approved or its code expired: the handler answers invalid_grant. Run
-- inside the redemption transaction after every check passed, as its LAST write (D8: this is the
-- first lock the transaction takes on the request row, after the grant and its tokens).
UPDATE oauth_authorize_requests
   SET status = 'redeemed'
 WHERE id = $1 AND status = 'approved' AND code_expires_at > now()
RETURNING *;

-- name: CreateGrantProductToken :one
-- Mint an access token for a grant: the CreateProductToken insert (guarded on the product being
-- enabled and not soft-deleted) plus grant_id. token_hash is never projected.
INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes, expires_at, grant_id)
SELECT sqlc.arg(user_id)::uuid,
       p.id,
       sqlc.arg(name)::text,
       sqlc.arg(token_hash)::bytea,
       sqlc.arg(token_prefix)::text,
       sqlc.arg(scopes)::text[],
       sqlc.arg(expires_at)::timestamptz,
       sqlc.arg(grant_id)::uuid
  FROM products p
 WHERE p.id = sqlc.arg(product_id)::uuid
   AND p.enabled
   AND p.deleted_at IS NULL
RETURNING id, user_id, product_id, name, token_prefix, scopes, revoked,
          created_at, last_used_at, last_used_ip, expires_at, grant_id;

-- name: SetOAuthGrantRefreshToken :one
-- Store the grant's refresh token (sha256 + display prefix), replacing any previous one: a code
-- exchange on a grant that somehow still holds a refresh token replaces it, so at most one is ever
-- live. refresh_last_used_at is reset; the idle clock then counts from refresh_issued_at. Guarded
-- on revoked_at IS NULL as a belt on the grant lock.
UPDATE oauth_grants
   SET refresh_token_hash = sqlc.arg(refresh_token_hash)::bytea,
       refresh_token_prefix = sqlc.arg(refresh_token_prefix)::text,
       refresh_issued_at = now(),
       refresh_last_used_at = NULL
 WHERE id = sqlc.arg(id) AND revoked_at IS NULL
RETURNING *;

-- name: RevokeOAuthGrantRow :execrows
-- The grant half of a revoke (D6): revoked_at, and the refresh token cleared so it can no longer be
-- presented. Run with the grant locked, after RevokeOAuthGrantProductTokens and
-- SupersedeOAuthGrantCodes (lock order); the row is kept as the audit trail. Idempotent.
UPDATE oauth_grants
   SET revoked_at = now(),
       refresh_token_hash = NULL,
       refresh_token_prefix = NULL,
       refresh_issued_at = NULL,
       refresh_last_used_at = NULL
 WHERE id = $1 AND revoked_at IS NULL;

-- name: LockOAuthUserGrants :exec
-- Serializes one user's grant CREATION against that user's Revoke all. A first-consent approve
-- inserts a grant row that no other transaction can see, or lock, until it commits, so
-- LockLiveOAuthGrantsForUser cannot wait for it: without this lock a Revoke all could finish
-- while an approve that already inserted its grant (and issued its code) commits afterwards,
-- leaving a live grant and a redeemable code after the panic button. The approve transaction
-- (handler.OAuthApprove) and the revoke-all transaction (handler.RevokeAllCLITokens) therefore
-- each take this lock FIRST, before any other lock. Lock order everywhere: this per-user lock,
-- then the user's grants in ascending id order, then each grant's product_tokens, then its
-- oauth_authorize_requests rows. Guaranteed: an approve that started first commits before the
-- revoke-all reads the grants, so the revoke covers its grant and code; an approve that waits
-- behind a revoke-all runs after it and creates a live grant of its own, which is a consent
-- given after the button was pressed. Paths that lock an EXISTING grant by id (token exchange,
-- refresh, per-token revoke) need only the grant lock and do not take this one.
--
-- Two-int advisory lock: class 1970958197 = 0x757A6F75 ("uzou"), the value of
-- store.OAuthUserLockClass in migrate.go, distinct from every other class constant there
-- (TestOAuthUserLockClassMatchesSQL pins this literal and TestProductTokenMintLockClassMatchesSQL
-- enumerates the collision check). The objid is hashtext of the user id, so two users can
-- collide: a moment of contention, never a correctness problem. XACT-scoped: released on commit
-- or rollback, so it must run on a transaction-bound Queries.
SELECT pg_advisory_xact_lock(
    1970958197,
    hashtext(sqlc.arg(user_id)::uuid::text)
);

-- name: LockLiveOAuthGrantsForUser :many
-- Every live grant of one user, row-locked in ASCENDING id order for the rest of the transaction
-- (D8: several grants are always locked in id order). Revoke all runs this right after
-- LockOAuthUserGrants and before any product_tokens UPDATE, so a code exchange or refresh of one of these grants either committed
-- before (and its token is then revoked) or waits and finds the grant revoked.
SELECT * FROM oauth_grants
 WHERE user_id = $1 AND revoked_at IS NULL
 ORDER BY id
 FOR UPDATE;

-- name: GetOwnProductTokenGrantID :one
-- The grant of one of the CALLER'S unrevoked product tokens, read WITHOUT a lock so the handler can
-- take the grant lock first (D8). grant_id is NULL for a manual token and never changes after
-- insert. Owner-scoped like RevokeProductToken: a foreign, unknown or already-revoked id is
-- pgx.ErrNoRows (the handler's 404).
SELECT grant_id FROM product_tokens WHERE id = $1 AND user_id = $2 AND NOT revoked;

-- name: GetProductTokenGrantID :one
-- AdminRevokeProductToken's counterpart of GetOwnProductTokenGrantID: any user's unrevoked token.
SELECT grant_id FROM product_tokens WHERE id = $1 AND NOT revoked;

-- name: ListLiveOAuthGrantsForUser :many
-- The caller's live grants (revoked_at IS NULL) whatever the state of their access tokens: a grant
-- whose tokens all expired is still a live connection. last_used_at is the later of the refresh
-- token's last use and the latest use of any of the grant's access tokens (GREATEST skips NULLs). The access-token half is a
-- top-1 probe of idx_product_tokens_grant_last_used per grant, never a read of the grant's whole
-- never-pruned token history.
-- Bounded by the one-live-grant-per-(user, product) index: at most one row per product.
SELECT g.id,
       g.product_id,
       p.name AS product_name,
       g.scopes,
       g.consented_at,
       g.created_at,
       g.refresh_issued_at,
       GREATEST(
           g.refresh_last_used_at,
           (SELECT t.last_used_at FROM product_tokens t
             WHERE t.grant_id = g.id AND t.last_used_at IS NOT NULL
             ORDER BY t.last_used_at DESC LIMIT 1)
       )::timestamptz AS last_used_at
  FROM oauth_grants g
  JOIN products p ON p.id = g.product_id
 WHERE g.user_id = sqlc.arg(user_id)
   AND g.revoked_at IS NULL
 ORDER BY g.consented_at DESC, g.id ASC;

-- PRD #1910 M4: the refresh_token grant and the RFC 7009 revoke endpoint. Both run in ONE
-- transaction in D8 lock order: an unlocked read finds the grant (or token) id, then the grant is
-- locked FOR UPDATE (LockOAuthGrant) and EVERYTHING is re-checked under the lock, then the writes.

-- name: GetOAuthGrantIDByRefreshHash :one
-- The grant that holds the presented refresh token, found by its sha256 WITHOUT a lock: it only
-- yields the id the handler then locks (D8). Nothing it returns is trusted until the handler
-- compares the hash again under the lock. A revoked grant has no refresh hash, so it is no row.
SELECT id FROM oauth_grants WHERE refresh_token_hash = $1;

-- name: TouchOAuthGrantRefreshUse :execrows
-- Stamp a successful refresh: the 30-day idle clock counts from refresh_last_used_at (or from
-- refresh_issued_at before the first refresh). The refresh token itself is NOT rotated (D5). Run
-- with the grant locked; guarded on revoked_at IS NULL as a belt on the lock.
UPDATE oauth_grants SET refresh_last_used_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: GetProductTokenForOAuthRevoke :one
-- The product token a client presents to POST /api/oauth/revoke, found by its sha256 WITHOUT a
-- lock (the handler locks the grant first when there is one, D8). Expired and revoked rows are
-- returned too (expired is judged on the database clock), because a revoked or expired token
-- answers 200 like an unknown one, before any other-client check. token_hash is never projected.
SELECT id, product_id, grant_id, revoked, (expires_at IS NOT NULL AND expires_at <= now())::boolean AS expired
  FROM product_tokens WHERE token_hash = $1;

-- name: RevokeOAuthGrantAccessToken :execrows
-- Revoke ONE access token of a grant (RFC 7009 on an access token, which does not revoke the grant).
-- Run with the grant locked, before any product_tokens write of this transaction. Scoped to the
-- grant so a token that is not this grant's is never touched; idempotent.
UPDATE product_tokens SET revoked = true WHERE id = $1 AND grant_id = $2 AND NOT revoked;

-- name: ListLiveOAuthGrantsForProduct :many
-- One product's live grants (revoked_at IS NULL) for the admin connections list (PRD #1910 M5),
-- whatever the state of their access tokens, with the owner's id and email as the admin product-
-- token inventory shows them. Columns are projected explicitly: no refresh hash, no token column
-- of any kind. last_used_at is as in ListLiveOAuthGrantsForUser. BOUNDED: a product can have one
-- live grant per user, so the handler passes its named cap PLUS ONE and reports "truncated" when
-- the extra row came back; newest consent first, so the cut drops the oldest connections.
SELECT g.id,
       g.user_id,
       u.email AS owner_email,
       g.scopes,
       g.consented_at,
       g.created_at,
       GREATEST(
           g.refresh_last_used_at,
           (SELECT t.last_used_at FROM product_tokens t
             WHERE t.grant_id = g.id AND t.last_used_at IS NOT NULL
             ORDER BY t.last_used_at DESC LIMIT 1)
       )::timestamptz AS last_used_at
  FROM oauth_grants g
  JOIN users u ON u.id = g.user_id
 WHERE g.product_id = sqlc.arg(product_id)
   AND g.revoked_at IS NULL
 ORDER BY g.consented_at DESC, g.id ASC
 LIMIT sqlc.arg(max_rows);

-- name: GetProductForShare :one
-- One product by id, soft-deleted included, ROW-LOCKED FOR SHARE for the rest of the transaction
-- (PRD #1910 D8). The approve and deny handlers re-read the product registration (redirect URIs,
-- enabled, scopes) under this lock, so an admin registration change either commits before the
-- read and is seen, or waits until the transaction ends. Must run on a transaction-bound
-- Queries; on a bare pool the lock is released as soon as the statement ends. FOR SHARE
-- conflicts with every admin product writer (e.g. SetProductOAuthClient, UpdateProduct,
-- SoftDeleteProduct, RotateProductClientSecret, GetProductForUpdate, the product_skills.sql
-- UPDATEs) but not with the FOR KEY
-- SHARE foreign-key checks of grant, token and request inserts. Position in the D8 lock order:
-- before any grant, token or request lock (approve takes it after its per-user lock; deny, which
-- has no per-user lock, takes it first).
SELECT * FROM products WHERE id = $1 FOR SHARE;
