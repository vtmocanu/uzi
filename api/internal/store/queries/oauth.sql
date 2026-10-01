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
INSERT INTO oauth_authorize_requests (product_id, redirect_uri, scopes, state, code_challenge, binding_hash, source_prefix, expires_at)
VALUES (sqlc.arg(product_id), sqlc.arg(redirect_uri), sqlc.arg(scopes)::text[], sqlc.arg(state), sqlc.arg(code_challenge), sqlc.arg(binding_hash)::bytea, sqlc.arg(source_prefix), sqlc.arg(expires_at))
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
-- handler runs, in ONE transaction, this lock, then CountLivePendingOAuthRequests, then
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
-- How many live (pending, unexpired) authorize requests exist for the (product, source), for the
-- product and overall, each counted up to its limit so the work is bounded however many rows an
-- attacker piles up. The unauthenticated authorize endpoint checks them against its caps (source
-- first, the fairness bound; the other two are storage backstops) BEFORE inserting, under
-- LockOAuthAuthorize. idx_oauth_authorize_requests_pending serves all three subqueries.
SELECT
    (SELECT count(*) FROM (
        SELECT 1 FROM oauth_authorize_requests
         WHERE status = 'pending' AND expires_at > now()
           AND oauth_authorize_requests.product_id = sqlc.arg(for_product)
           AND oauth_authorize_requests.source_prefix = sqlc.arg(for_source)
         LIMIT sqlc.arg(source_limit)::int
    ) s)::bigint AS source_pending,
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
