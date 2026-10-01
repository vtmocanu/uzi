-- PRD #1910 M2: the consent half of the OAuth authorization server (oauth_grants and
-- oauth_authorize_requests, migration 00283). Lock order everywhere (D8): the grant row first, then
-- that grant's product_tokens rows, then its oauth_authorize_requests rows. The approve transaction
-- claims the request it decides LAST (after the grant lock, the narrowing revoke and the superseding
-- of the grant's earlier codes), so it never holds a request row while waiting for a grant. Only the
-- TTL sweep and deny lock request rows without a grant lock; neither ever waits for a grant.

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
-- transaction in D8 lock order: the unlocked code lookup (no lock), the grant FOR UPDATE, the
-- grant's token count, then the request row FOR UPDATE, the conditional redeem, the access-token
-- insert and the refresh-token store. A failed check returns without writing, so nothing is
-- consumed on failure.

-- name: GetOAuthAuthorizeRequestByCodeHash :one
-- The request an authorization code belongs to, found by the sha256 of the presented code. NO row
-- lock: the handler needs the grant id to take the grant lock first (D8), then re-reads the request
-- with LockOAuthAuthorizeRequest.
SELECT * FROM oauth_authorize_requests WHERE code_hash = $1;

-- name: LockOAuthGrant :one
-- A grant by id, row-locked for the rest of the transaction (D8: every grant mutation serializes
-- here first). No row means the grant is gone (its user was deleted).
SELECT * FROM oauth_grants WHERE id = $1 FOR UPDATE;

-- name: LockOAuthAuthorizeRequest :one
-- The request row, locked AFTER its grant (D8) and re-read under that lock, so a redemption sees a
-- supersession or a concurrent redemption that committed while it waited for the grant.
SELECT * FROM oauth_authorize_requests WHERE id = $1 FOR UPDATE;

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

-- name: RedeemOAuthAuthorizeRequest :one
-- approved -> redeemed in one conditional UPDATE, the code's single use. Zero rows (ErrNoRows)
-- means it was no longer approved or its code expired: the handler answers invalid_grant. Run
-- inside the redemption transaction after every check passed.
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
