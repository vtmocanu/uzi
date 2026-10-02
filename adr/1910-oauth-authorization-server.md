# ADR-1910: uzi is a minimal in-tree OAuth authorization server for registered products

**Status**: Accepted (PRD #1910 M1-M6 implemented and audited; the M6 audit found one High, fixed with `TestGrantListLastUsedIsATopOneIndexProbeLiveDB`, and two Lows, addressed in the docs and this record)
**Date**: 2026-10-01
**Issue**: [vtmocanu/uzi#1910](https://github.com/vtmocanu/uzi/issues/1910)
**PRD**: [prds/done/1910-connect-uzi-oauth.md](../prds/done/1910-connect-uzi-oauth.md)

## Decision (summary)

> uzi runs a deliberately small OAuth 2.0 authorization server in-tree: the
> authorization-code grant with mandatory PKCE S256 plus refresh, confidential
> clients only, admin-registered clients, exact redirect-URI match and opaque
> tokens. An access token is an ordinary `product_tokens` row with a `grant_id`, so
> `RequireV1Caller` is its only enforcement path. Every mutation of a grant takes
> the grant row lock first (approve and Revoke all first take a per-user advisory lock, so a
> first consent not yet committed cannot slip past Revoke all), and revoking a grant revokes everything under it in one
> transaction. The token and revoke endpoints join ADR-1907's compatibility promise.

## Context

[ADR-1907](1907-product-api-v1-contract.md) lets a user mint a `uzp_` token and
paste it into a product. That puts a long-lived bearer credential on the
clipboard, gives a product no way to renew it, asks non-developers to open
Settings and pick scopes, and proves nothing about which application presents the
token. PRD #1910 replaces the paste with a consent flow: the product sends the
user's browser to uzi, the user approves, and the product receives a short-lived
access token and a refresh token, until the connection is revoked.

## Decision

1. **The surface is exactly this, and widening it reopens the decision (D1).**
   One grant (authorization code, PKCE `S256` mandatory), the refresh grant,
   confidential clients, admin-registered clients, exact redirect-URI match,
   opaque tokens. There is no implicit or password grant, no public client, no
   dynamic registration, no OpenID Connect provider feature (no ID token,
   userinfo or discovery) and no JWT access token. Adding any of them is a new
   decision, not an extension of this one. Each behaviour follows RFC 6749
   sections 4.1, 5 and 6, RFC 7636, RFC 7009, RFC 9207 and RFC 9700, and each rule
   is pinned by a named test (the index below). The protocol logic lives in one
   package, `api/internal/oauthsrv`, with no HTTP or SQL in it. A framework such
   as `ory/fosite` was rejected: a large storage interface and many flows to keep
   disabled, a new dependency on a security-critical path, and a second token
   model beside ADR-1907's. The accepted cost is hand-rolled security code,
   mitigated by the named tests, the M6 audit and this record.
2. **Client = product (D2).** `client_id` is the product's id; there is no second
   identifier. An admin sets redirect URIs, allowed scopes and a `uzs_` client
   secret (sha256 at rest). The token endpoint authenticates the client with HTTP
   Basic only.
3. **A grant is (user, product, scopes), and an access token is a product token
   (D5).** At most one live grant per (user, product), enforced by a partial unique
   index, holding the one live refresh token (`uzr_`, sha256 at rest). The
   access token has a one-hour lifetime and goes through `RequireV1Caller`
   unchanged. A refresh token is never accepted as a bearer.
4. **No refresh-token rotation for these clients (D5).** RFC 9700 section 4.14.2
   requires rotation or sender-constraining of refresh tokens only for public
   clients, and RFC 6749 section 6 already binds a refresh token to client
   authentication. Rotation would make a retried refresh after a lost response,
   or a product running several replicas, revoke the grant. A refresh returns a
   new access token and keeps the refresh token, and **the refresh response omits
   `refresh_token`** (RFC 6749 section 6 allows either that or returning the same
   one; omitting it makes it plain that nothing changed, and the response has its
   own type, `apitypes.OAuthRefreshResponse`). The lifetimes are constants: access
   1 hour, refresh 30 days idle (from the last refresh, else from issue) and 90
   days absolute from `oauth_grants.consented_at`, which every re-consent resets:
   a user who approves again after the 90 days, through the normal approve path,
   gets a working refresh token whatever the age of the grant row.
5. **Bound on live access tokens (D5).** Every mint is refused while the grant
   already holds 10 unexpired, unrevoked access tokens, counted under the grant
   lock. The refusal is a 429 with `Retry-After` (seconds until the oldest live
   token expires) and `{"error":"temporarily_unavailable"}`.

   The live count alone is no bound on stored rows: a product that refreshes and
   then RFC 7009-revokes each new access token keeps its live count at zero and
   adds a row per round. So every mint, a code exchange and a refresh alike, is
   also refused once 30 access tokens were minted for the grant in the last hour,
   revoked ones included, again under the grant lock and again a 429 with
   `Retry-After` (seconds until the oldest of them leaves the hour). Both counts
   read only their own rows: `idx_product_tokens_grant (grant_id, created_at)`
   serves the rate count and `idx_product_tokens_grant_live (grant_id, expires_at)
   WHERE NOT revoked` the live count, and `TestGrantTokenCountsUseIndexesLiveDB`
   EXPLAINs both. A product refreshes about once an hour, so the bound is far
   above honest use.

   Resource-bound note. The connections lists show each grant's last use. A
   grant's token history is never pruned (about 64,800 rows for one 90-day grant
   at the 30-an-hour cap), so both lists read it through
   `idx_product_tokens_grant_last_used (grant_id, last_used_at DESC)` as a top-1
   probe per grant (`ORDER BY last_used_at DESC LIMIT 1`), never an aggregate,
   pinned by `TestGrantListLastUsedIsATopOneIndexProbeLiveDB`. Indexing
   `last_used_at` costs the throttled `TouchProductToken` write its HOT update on
   every product token, manual ones included; accepted as a once-a-minute write. Two statements
   still read every UNREVOKED row of one grant: the scope check on approve
   (`OAuthGrantHasTokenOutsideScopes`) and the token sweep on revoke
   (`RevokeOAuthGrantProductTokens`). They run only on a user, admin or product
   revoke or approve action and are normally served by `idx_product_tokens_grant_live`,
   reading the grant's unrevoked rows (on a table small enough that a scan is cheaper
   than the index, the planner may scan `product_tokens` instead). Those rows are bounded by the mint rate
   times the grant's lifetime since its last revoke (expired tokens are not
   revoked, because that would cancel their jobs). Pruning stays out of scope.
6. **Grant tokens are not manual tokens (D5).** Rows with a `grant_id` are
   excluded from the user and admin token lists, from the per-product active
   counts and from the 10-token manual mint cap. Their lifecycle is the
   connection's.
7. **Revoking a grant revokes everything under it, in one transaction (D6).** It
   sets `revoked_at`, sets `revoked` on every token with that `grant_id` (expired
   or not, so the existing `ListRevokedProductJobs` sweep cancels the jobs of an
   already expired token), clears the refresh token and supersedes unredeemed
   codes. Every path calls the one helper, `revokeGrantLocked`. Revoke all, the
   owner's or an admin's revoke of one grant token by id, a code replay and the
   product's RFC 7009 revoke with its refresh token reach it, and so do the
   user's and the admin's connection revoke (`revokeGrantOfTokenTx`, the same
   transaction the revoke-by-token-id routes use).
8. **Every grant mutation serializes on the grant row (D8).** The lock order is
   the grant row (several grants in ascending id order), then that grant's
   `product_tokens` rows, then its `oauth_authorize_requests` rows, on every
   path. The two paths that create or sweep a user's grants, approve and Revoke
   all, take one more lock before all of those: a per-user transaction-scoped
   advisory lock (`store.OAuthUserLockClass`, `LockOAuthUserGrants`). A
   first-consent approve inserts a grant that no row lock can wait for until it
   commits, so without it a Revoke all could finish while that approve commits
   afterwards, leaving a live grant and a redeemable code. Approve and deny
   also re-read the product row `FOR SHARE` (`GetProductForShare`) in their
   transaction and decide the registration check (client enabled, redirect URI
   still registered, scopes still allowed) on that locked row, holding the share
   lock through commit, so an admin who removes a redirect URI, disables the
   client or narrows its scopes while a user is on the consent page either
   commits first, and the request is refused with 409 and no redirect (deny
   still marks it denied; approve rolls back and it stays pending), or waits
   until the approve or deny commits. The share lock adds no cycle: approve and
   deny take it before any grant, token or request lock, and no transaction
   holding an admin product lock (`SetProductOAuthClient`, `UpdateProduct`,
   `GetProductForUpdate` on delete and the skills stage and apply, the secret
   rotation) goes on to take a grant-side lock or an OAuth advisory lock. A
   grant-lock holder can still wait on the product row: the `product_tokens`
   insert of an exchange or refresh takes `FOR KEY SHARE` through its foreign
   key, which a `FOR UPDATE` blocks, but that holder never waits for a grant.
   Accepted limit: Postgres lets a new share locker pass a waiting writer, so a
   steady stream of approves and denies on one product can delay an admin
   registration change. The delay has no fixed bound: an approve holds the
   product share lock while it waits for its grant lock, so contention on a
   grant extends it. The full order is: per-user lock (approve
   and Revoke all), product row (`FOR SHARE`, approve and deny), grants
   ascending, tokens, requests. What it guarantees: an
   approve that took the lock first commits before Revoke all reads the grants,
   so the revoke covers its grant and code; an approve that waits behind Revoke
   all runs after it and creates a live grant of its own (a consent given after
   the button was pressed). Revoke all's plain product-token sweep skips grant
   tokens (`grant_id IS NULL`) as defence in depth: grants are revoked only
   through `revokeGrantLocked`, and the per-user lock plus the grant rows Revoke
   all holds already keep a concurrent grant or exchange out, so a plain sweep
   can never leave a grant with its access token revoked but its grant and
   refresh token live. After the lock the path re-checks and then acts: a redemption re-reads
   the request, the grant's `revoked_at`, the client and the user, and counts the
   live tokens, so a revoke cannot miss a token a concurrent exchange is inserting.
9. **Contract extension (D7).** `POST /api/oauth/token` and
   `POST /api/oauth/revoke` are part of the external contract: ADR-1907's
   compatibility promise (additive only, a deprecation window for anything
   breaking) applies to their request and response shapes, and
   `api/openapi/v1.yaml` gains an `oauth2` security scheme (authorization-code
   flow URLs and the two scopes) as a new alternative entry beside `bearerAuth`,
   which `check:api-v1-compat` rates as an additive change. The flow's
   `refreshUrl` is the token endpoint, which serves `grant_type=refresh_token`.
   Two extensions to RFC 6749 are deliberate and are part of that promise:
   - a **database or lookup error is a 503 `{"error":"temporarily_unavailable"}`
     with `Retry-After`**, never `invalid_grant` or `invalid_client`, because a
     product that sees `invalid_grant` discards the connection (the lesson of
     #1992 applied here from day one); `temporarily_unavailable` is an
     authorization-endpoint code in RFC 6749 section 4.1.2.1, not a section 5.2
     token error;
   - a **429 `temporarily_unavailable` with `Retry-After`** for the 10-token and hourly mint bounds
     and for the per-IP and per-client limiters: the request is not malformed, the
     product should reuse its token or wait.

    The revoke endpoint follows RFC 7009 with three decisions worth recording.
    A **refresh token** revokes the whole grant (D6). An **access token** issued
    by a grant is revoked **alone**: RFC 7009 section 2.1 lets the server revoke
    the grant too, but D6 lists only the refresh path as ending the connection,
    and a product that drops one access token must be able to refresh. A **manual
    (pasted) `uzp_` token** has no grant, so it was not issued to the client by
    OAuth: it answers 200 and is **not** revoked there; its owner revokes it. A
    LIVE token that belongs to another client (any product token of another
    product, or another product's grant) is 400 `invalid_grant` and revokes
    nothing; an unknown token, or an access token that is already revoked or
    expired and not the client's own, is 200 (see the exact rule below). `token_type_hint` is
    advisory: refresh is tried first unless the hint is `access_token`, and the
    other type is always tried next. A product that is disabled or deleted, or
    whose client registration was cleared, no longer authenticates as a client,
    so its refresh and revoke requests are 401 `invalid_client`; the grant itself
    is untouched and works again when the product does.

    **Whose state is wrong decides the refresh error.** `invalid_grant` is "stop
    using this refresh token": revoked, idle or absolute expiry, superseded by a
    new consent, another client's, or the user is deactivated (nothing is revoked
    and reactivation restores the grant, but a product may discard the connection
    meanwhile, which is the price of RFC 6749 section 5.2 having no better code for
    an account problem). `invalid_client` (401 with `WWW-Authenticate`) is "your
    registration is the problem, keep the refresh token": the product is disabled,
    deleted, no longer a client, or its allowed scopes no longer cover what the
    token would carry. It is the same answer whether the state is seen at
    authentication or under the grant lock, so a product that stopped being a
    client while its request waited for the lock gets the 401 its authentication
    would have got a moment earlier, and a product must never discard a connection
    on it. The scopes checked are the REQUESTED ones when `scope` is sent, so a
    product narrowed below the grant can still refresh with the scopes it keeps.
    The revoke endpoint's dead-token rule is exact: an access token that is
    already revoked, or expired and not the client's own, answers 200 before the
    other-client check (as a revoked refresh token, whose hash is cleared, is
    simply unknown); only a **live** token of another product is 400
    `invalid_grant`; the client's own expired access token answers 200 and is
    marked revoked so the revoked-jobs sweep covers it.
10. **A replay after an ambiguous commit revokes (D6).** A second redemption of an
    already redeemed code revokes the grant only when it passes every binding
    check (client authentication, the code's client, the exact redirect URI and the
    PKCE verifier); anything less is a plain `invalid_grant` that revokes nothing.
    The consequence to know: if a 503 answers an exchange whose transaction in
    fact committed, the product's retry presents a used code with every binding
    correct, which is a fully bound replay and revokes the connection. That is the
    RFC 6749 section 4.1.2 rule working as written, not a defect; the product
    starts a new connection. A 503 before the commit leaves the code unconsumed,
    so the retry succeeds.

## Consequences

- A product gets a connection the user can see and revoke, short-lived access
  tokens and unattended renewal. The cost is hand-rolled protocol code and a
  contract (two endpoints and an `oauth2` scheme) that ADR-1907's window now
  covers.
- A client sees three codes beyond RFC 6749 section 5.2 behaviour: the 503, the
  per-IP, per-client and 10-token 429s, all `temporarily_unavailable`. A client
  library must treat them as retry signals.
- The per-client rate budget is one bucket per product, shared by all of its
  users (`OAUTH_RATE_LIMIT_MAX`, tunable).
- A replay that is not fully bound is deliberately not punished: a stolen code
  injected into the honest client's callback cannot be used to disconnect the
  user.
- A product can stay connected (refresh) and disconnect itself (RFC 7009), and
  users and admins can list and revoke connections (M5). The M6 audit is done;
  every rule in the index below has a named test.
- Clearing a product's OAuth registration refuses every refresh and narrowing it
  refuses a refresh for any scope it lost, but either leaves live access tokens with their old scopes until they expire (at most
  one hour). Disabling the product cuts access immediately. This is documented in
  [Registering an OAuth client](../docs/oauth-clients.md).
- A refresh does not rotate, so a leaked refresh token is good until the user or
  product revokes the connection or the 30-day idle and 90-day absolute limits
  end it; the exposure is bounded by those limits and by revoke, not by reuse
  detection.

## Rule-to-test index

Each row names the tests that pin a rule. Tests whose names end
in `LiveDB` need a database (`./e2e/run-store-it.sh`); the others run in
`task test:api` and `task gate:web`.

| Rule | Pinned by |
|---|---|
| D1 only the listed surface exists: no public client, no other grant, Basic only, PKCE S256 and `code` only | `TestValidateAuthorizeRejects`, `TestOAuthTokenRequestRulesLiveDB` (unsupported grant type), `TestOAuthTokenClientAuthenticationLiveDB` (a body secret alone, a bearer, a non-client) |
| D1 no discovery or OpenID Connect provider routes: the `/api/oauth` surface is exactly the six listed routes and nothing is served under `/.well-known` | `TestOAuthServerHasNoOIDCProviderRoutes` |
| D1 protocol logic in one package, RFC verifier and PKCE rules | `TestValidVerifier`, `TestVerifierMatchesS256`, `TestParseTokenForm`, `TestParseScopes`, `TestValidateScopes`, `TestRedirectURLs` |
| D2 redirect-URI format and exact match, client secret class and constant-time check | `TestValidateRedirectURI`, `TestValidateRedirectURIs`, `TestValidateRedirectURIsErrorDoesNotEchoTheURI`, `TestGenerateSecret`, `TestSecretMatches`, `TestAdminProductOAuthClientLiveDB`, `TestAdminProductOAuthValidationLiveDB`, `TestAdminProductOAuthUnknownAndDeletedLiveDB`, `TestAdminProductOAuthRoutesAuthLiveDB`, `TestOAuthTokenRotatedSecretCutsOffTheOldOneLiveDB` |
| D2 pasted `uzp_` tokens keep working for every product, client or not, beside a connection's access token | `TestManualTokenWorksForAClientProductLiveDB` |
| D2 `uzs_` and `uzr_` are registered and scrubbed | `TestMintedPrefixesScrubbedOnBothPaths`, `TestScrubSecretShapesMintedUziPrefixes`, `TestScrubKnownTokensMintedUziPrefixes`, `TestGenerateRefreshToken` |
| D3 authorize is unauthenticated, validated before any redirect, caps, binding cookie, bounded storage | `TestCheckClientRejectsBeforeAnyRedirect`, `TestOAuthAuthorizeStaticErrorWithoutDB`, `TestOAuthAuthorizeStaticErrorLiveDB`, `TestOAuthAuthorizeRejectsRedirectToRegisteredURILiveDB`, `TestOAuthAuthorizeSuccessStoresHashAndSetsCookieLiveDB`, `TestOAuthAuthorizePendingCapLiveDB`, `TestOAuthSourceBucketsFor`, `TestWellFormedOAuthNonce`, `TestOAuthSweepAndCapQueriesUseIndexesLiveDB`, `TestOAuthAuthorizeLockClassMatchesSQL` |
| D3 authorize is behind `authLimiter`'s per-IP middleware, and approve carries the per-user limiter | `TestOAuthAuthorizeIsBehindThePerIPAuthLimiter`, `TestEveryRouteCarriesItsExpectedPerUserLimiter` (the approve row) |
| D3 returning after login: a signed-out user on `/connect` is sent to `/login?next=`, the path is kept in `sessionStorage` for an OIDC login, and the consent page resumes the request | web: `pendingReturn.test.ts`, `AppShell.pendingReturn.test.tsx`, `Connect.test.tsx` |
| D4 consent is explicit, bound to the browser, single use | `TestOAuthRequestMetadataLiveDB`, `TestOAuthConsentFixationLiveDB`, `TestOAuthApproveRedirectCarriesCodeStateIssLiveDB`, `TestOAuthApproveRedirectKeepsRegisteredQueryLiveDB`, `TestOAuthDoubleApproveAndDenyLiveDB`, `TestOAuthConcurrentApproveLiveDB`, `TestOAuthLosingApproveRollsBackSupersedeLiveDB`, `TestOAuthDenyStaleRequestIsRefusedLiveDB`, `TestOAuthSweepKeepsRecentCodesLiveDB`; web: `Connect.test.tsx` |
| D5 code exchange mints an access token and the grant's refresh token; the token works on `/api/v1` and nowhere a pasted token is refused; the refresh token is no bearer | `TestOAuthTokenExchangeSuccessLiveDB`, `TestOAuthTokenAccessTokenIsRefusedWhereAPastedTokenIsLiveDB` |
| D5 the access token carries only the grant's scopes; a code lives 60 seconds | `TestOAuthTokenAccessTokenCarriesOnlyTheGrantsScopesLiveDB`, `TestOAuthTokenCodeExpiresAfterSixtySecondsLiveDB` |
| D5 one live grant per (user, product); re-consent rules | `TestOAuthConcurrentFirstConsentsShareOneGrantLiveDB`, `TestOAuthReconsentLiveDB`, `TestOAuthApproveAfterRevokedGrantCreatesNewGrantLiveDB` |
| D5 the two per-mint counts read only the rows they count: the time column sits in the Index Cond of the plan, never in a Filter | `TestGrantTokenCountsUseIndexesLiveDB` (store; red when either index is reduced to `(grant_id)`) |
| D5 ten live access tokens per grant, counted under the grant lock | `TestOAuthTokenLiveTokenCapLiveDB`, `TestOAuthTokenCapIsCountedUnderTheGrantLockLiveDB` |
| D5 grant tokens are not manual tokens (lists, counts, mint cap) | `TestGrantTokensAreNotManualTokensLiveDB`; web: "Revoke all counts live OAuth connections" in `ProductTokens.test.tsx` |
| D5 live connections are counted apart from manual tokens: `live_connection_count` on a product and `stopped_connection_count` on its delete, so the delete confirm names both | `TestProductConnectionCountsLiveDB`, `TestProductDTOTags` (the wire tags), the `product.*`, `admin_delete_product.*` and `rotate_product_client_secret.*` contract fixtures with `TestContractFixturesMatchMarshal`, `TestContractFullFixtureDecodesStrict` and `apiContract.test.ts`; `TestAdminProductsTable` (the `CONNECTIONS` column); web: the delete-confirm and toast wording in `AdminProducts.test.tsx`, the mock's counts in `mockApi.productTokens.test.ts` |
| D5 refresh grant: a working access token, no rotation (the response omits `refresh_token`, the stored hash is unchanged, the same token refreshes again), concurrent double refresh | `TestOAuthRefreshSuccessLiveDB`, `TestOAuthRefreshConcurrentDoubleLiveDB`, `TestParseTokenFormRefreshAndRevokeParameters`, the `oauth_refresh_response.*` contract fixtures with `TestContractFixturesMatchMarshal` and `TestOAuthConsentDTOTags` (the wire tags) |
| D5 refresh lifetimes: 30 days idle (from the last refresh, else issue), 90 days absolute from `consented_at`, a re-consent restarts the absolute clock | `TestOAuthRefreshLifetimesLiveDB`, `TestOAuthRefreshAfterReconsentRestartsTheAbsoluteClockLiveDB` |
| D5 refresh `scope` narrows to a subset of the grant's current scopes, never widens; the grant is unchanged | `TestOAuthRefreshScopeLiveDB`, `TestOAuthRefreshScopeCannotWidenLiveDB` |
| D5 the ten-live-token bound on refresh, counted under the grant lock | `TestOAuthRefreshLiveTokenCapLiveDB`, `TestOAuthRefreshCapIsCountedUnderTheGrantLockLiveDB` |
| D5 the per-grant mint-rate bound (30 an hour, revoked included): a refresh-then-revoke loop is refused with a 429 and a Retry-After that counts down to the oldest mint leaving the hour; both per-mint counts are index-served | `TestOAuthRefreshThenRevokeLoopIsRateBoundedLiveDB`, `TestGrantTokenCountsUseIndexesLiveDB` (store) |
| D5 a refresh token is never a bearer | `TestOAuthTokenAccessTokenIsRefusedWhereAPastedTokenIsLiveDB` (every route), `TestOAuthRefreshTokenIsNotABearerLiveDB` |
| D7 refresh authenticates the client like the code exchange; another client's refresh is `invalid_grant` and changes nothing; the token request rules apply | `TestOAuthRefreshClientAuthenticationLiveDB`, `TestOAuthRefreshRequestRulesLiveDB` |
| D7 whose state is wrong decides the refresh error: a disabled, deleted, non-client or scope-narrowed product is 401 `invalid_client` with `WWW-Authenticate` and nothing is revoked, an inactive user or dead grant is `invalid_grant`; a narrowing `scope` the product still allows is served | `TestOAuthRefreshRechecksClientAndUserLiveDB` |
| D8 refresh re-checks the hash, client, product, user and scopes under the grant lock: a product that stops being a client while the request waits for the lock gets `invalid_client`, mints nothing, and the same refresh token works once restored | `TestOAuthRefreshRechecksClientAndUserLiveDB`, `TestOAuthRefreshProductDisabledWhileWaitingForTheLockLiveDB` (disabled, deleted, redirect URIs cleared, scopes narrowed, scopes narrowed with a narrowing request) |
| D6 code replay revokes the grant only after every binding check | `TestOAuthTokenReplayRevokesGrantLiveDB`, `TestOAuthTokenReplayWithBrokenBindingRevokesNothingLiveDB`, `TestOAuthTokenReplayAfterCodeExpiryStillRevokesLiveDB`, `TestOAuthTokenAnotherClientsCodeLiveDB` |
| D6 a code approved before a revoke or a re-consent cannot be redeemed after it | `TestOAuthTokenCodeApprovedBeforeGrantRevokeIsDeadLiveDB`, `TestOAuthTokenCodeApprovedBeforeReconsentIsSupersededLiveDB`, `TestRevokeAllRacingCodeExchangeLiveDB` |
| D6 the owner revoking a grant token by id kills the grant; a foreign id is 404; a manual token revokes alone | `TestRevokeMyProductTokenOnGrantTokenKillsGrantLiveDB`, `TestRevokeMyProductTokenForeignGrantAndManualTokenLiveDB` |
| D6 an admin revoking a grant token by id kills the grant | `TestAdminRevokeGrantTokenKillsGrantLiveDB` |
| D6 Revoke all leaves no live grant, and its button counts grants; its warning tells connected products to connect again | `TestRevokeAllRevokesGrantsLiveDB`, `TestRevokeAllRacingCodeExchangeLiveDB`, `TestRevokeAllVersusFirstConsentApproveLiveDB`; web: "Revoke all counts live OAuth connections" in `ProductTokens.test.tsx`, `CliTokens.test.tsx` (including the connect-again sentence), `mockApi.productTokens.test.ts` |
| D6 password change and logout do not revoke a connection: both bump `token_version`, and the access token and the refresh token still work afterwards (uzi has no password-change endpoint yet, so the password half drives the `UpdatePassword` query directly) | `TestPasswordChangeAndLogoutDoNotRevokeAConnectionLiveDB` |
| D6 the connections lists read each grant's last use as a top-1 index probe, not the grant's whole token history | `TestGrantListLastUsedIsATopOneIndexProbeLiveDB` (store) |
| D6 revoke cancels jobs of an expired access token | `TestCancelRevokedProductJobsExpiredGrantTokenLiveDB` |
| D6 the product revokes its own connection with its refresh token (grant, access tokens, refresh token), idempotently | `TestOAuthRevokeWithRefreshTokenKillsGrantLiveDB` |
| D6 the owner's revoke of a connection (`POST /api/me/oauth-connections/{id}/revoke`) is owner-scoped (a foreign, unknown or already-revoked id is 404 and changes nothing), needs the CSRF header, and is cookie-only | `TestUserRevokeConnectionIsOwnerScopedLiveDB`, `TestConnectionRevokeRoutesRefuseBearerLiveDB` (uzc_, uza_, uzp_ and uzr_ Bearers), `TestEveryRouteCarriesItsExpectedPerUserLimiter` (its row) |
| D6 an admin's revoke of a connection (`POST /api/admin/oauth-connections/{id}/revoke`) is admin-only and cookie-only: a non-admin session is 403, a `uza_` Bearer is 401, an unknown id is 404 | `TestAdminConnectionRoutesAuthorizationLiveDB`, `TestConnectionRevokeRoutesRefuseBearerLiveDB`, `TestEveryRouteCarriesItsExpectedPerUserLimiter` (its row) |
| D6 after a revoke from either list the next `/api/v1` call with the connection's access token is 401, the next refresh is `invalid_grant`, and a second revoke is 404 | `TestConnectionRevokeKillsApiAndRefreshLiveDB` (user and admin) |
| D6 a connection whose access tokens have all expired is listed (for its owner and for the admin) and revocable from both; the revoke reaches the expired tokens, so the sweep cancels the jobs they created | `TestConnectionWithOnlyExpiredTokensIsListedAndRevocableLiveDB` (user and admin), `TestCancelRevokedProductJobsExpiredGrantTokenLiveDB` |
| D6 the admin's per-product list (`GET /api/admin/products/{id}/connections`) shows only that product's live grants with the user's id and email and no token or hash, newest consent first, cut at the 1000-row bound with `truncated`; an unknown product is 404 | `TestAdminProductConnectionsListLiveDB`, `TestAdminConnectionRoutesAuthorizationLiveDB`, `TestEveryRouteCarriesItsExpectedPerUserLimiter` (its row); the `admin_oauth_connection.*` contract fixtures with `TestContractFixturesMatchMarshal`, `TestContractFullFixtureDecodesStrict`, `TestOAuthConsentDTOTags` and `apiContract.test.ts` |
| D6 Settings, Access, Connected products lists every live grant whatever the state of its tokens, renders names as plain text, confirms before a revoke, drops the row and refreshes the Revoke all count; the admin Connections panel does the same per product | web: `OAuthConnections.test.tsx`, `ProductConnections.test.tsx`, the connection twins in `mockApi.productTokens.test.ts` and `mockApi.parity.test.ts` |
| D6 `uzi admin products connections <product>` is read-only and prints a table or `{connections, truncated}` JSON | `TestAdminProductsConnectionsTable`, `TestAdminProductsConnectionsJSONEmptyAndTruncated`, `TestAdminProductsConnectionsUnknownProduct`, `TestAdminProductsConnectionsHostileStringsEscaped`, `TestHTTPFileRequests` (the client call), `TestSkillMatchesCommandTree` |
| D7 token request rules: form only, no repeated parameter, one client-authentication method, 401 with `WWW-Authenticate` | `TestOAuthTokenRequestRulesLiveDB`, `TestOAuthTokenClientAuthenticationLiveDB` |
| D7 a storage error is a 503, never a credential error, on every step including the replay revoke, a refresh and an RFC 7009 revoke | `TestOAuthTokenStorageErrorIsNeverACredentialErrorLiveDB`, `TestOAuthTokenReplayStorageErrorIsA503LiveDB`, `TestOAuthRefreshStorageErrorIsNeverACredentialErrorLiveDB`, `TestOAuthRevokeStorageErrorIsA503LiveDB` |
| D7 limiter per IP and per client, per-client budget drawn only after authentication, OAuth-shaped 429, `no-store` on every method | `TestOAuthTokenPerClientBudgetIgnoresFailedAuthenticationLiveDB`, `TestOAuthRevokePerClientBudgetIgnoresFailedAuthenticationLiveDB`, `TestOAuthTokenIsBehindThePerIPOAuthLimiter`, `TestOAuthRevokeIsBehindThePerIPOAuthLimiter`, `TestOAuthRoutesAreNoStoreOnEveryMethod` (both paths) |
| D7 `/api/me/oauth-connections` is cookie-only and lists live grants whatever their tokens' state | `TestOAuthConnectionsRouteRefusesBearerLiveDB`, `TestMyOAuthConnectionsLiveDB`, `TestEveryRouteCarriesItsExpectedPerUserLimiter` (its row) |
| D7 the `oauth2` scheme is an additive alternative and nothing else in `v1.yaml` moved; the test fails cleanly, never panics, on a spec without the scheme | `TestV1OpenAPIRouteParity`, the `check:api-v1-compat` gate |
| D7 RFC 7009 revoke: client-authenticated like the token endpoint, a live token of another client is `invalid_grant` and revokes nothing, unknown, expired and revoked tokens are 200, the hint is advisory | `TestOAuthRevokeRequestRulesLiveDB`, `TestOAuthRevokeAnotherClientsTokenIsInvalidGrantLiveDB`, `TestOAuthRevokeUnknownTokensAreOKLiveDB`, `TestOAuthRevokeHintIsAdvisoryLiveDB` |
| D7 a dead (revoked, or expired and another client's) access token is 200 before the other-client check, a revoked refresh token is 200 for anyone, a live token of another client stays `invalid_grant` with nothing revoked, the client's own expired token is marked revoked | `TestOAuthRevokeDeadTokenIsOKBeforeTheOtherClientCheckLiveDB` |
| D7 revoking an access token revokes that token only; a manual token is not revoked there | `TestOAuthRevokeAccessTokenRevokesOnlyThatTokenLiveDB`, `TestOAuthRevokeHintIsAdvisoryLiveDB` (access token cases) |
| D8 lock order grant, tokens, requests on every path; replay takes no request-row lock | `TestOAuthTokenReplayTakesNoRequestRowLockLiveDB`, `TestOAuthTokenCapIsCountedUnderTheGrantLockLiveDB`, `TestOAuthTokenConcurrentSameCodeLiveDB`, `TestRevokeAllRacingCodeExchangeLiveDB` |
| D8 per-user lock first on approve and Revoke all: both orderings of a first-consent approve against Revoke all leave no live grant or redeemable code the button missed | `TestRevokeAllVersusFirstConsentApproveLiveDB` (both orderings), `TestOAuthUserLockClassMatchesSQL`, `TestProductTokenMintLockClassMatchesSQL` (class collision enumeration) |
| D8 approve and deny re-check the registration under a product `FOR SHARE` lock: a redirect URI removed before the locked re-read gets no code or deny redirect (409; deny marks the request denied, approve leaves it pending), and an admin registration change issued during an approve waits for it to commit | `TestOAuthApproveRegistrationRemovedBeforeLockedRecheckLiveDB`, `TestOAuthDenyRegistrationRemovedBeforeLockedRecheckLiveDB`, `TestOAuthAdminRegistrationChangeWaitsForApproveLiveDB` |
| D8 approve and first consent under the grant lock | `TestOAuthConcurrentApproveLiveDB`, `TestOAuthConcurrentFirstConsentsShareOneGrantLiveDB`, `TestOAuthLosingApproveRollsBackSupersedeLiveDB` |
| D8 refresh vs revoke and refresh vs re-consent races: a refresh blocked on the grant lock while a revoke or re-consent commits mints nothing, and no live token survives Revoke all | `TestOAuthRefreshVersusGrantMutationLiveDB` (owner revoke, `revokeGrantLocked`, re-consent), `TestOAuthRefreshVersusRevokeAllLiveDB` |
| D8 refresh vs `POST /api/oauth/revoke` with the refresh token, both queued on the grant lock in either order: the losing refresh mints nothing that survives (revoke first: no token is minted; refresh first: the minted token is revoked with the grant), no live grant or unrevoked token remains | `TestOAuthRefreshVersusOAuthRevokeLiveDB` (both orders) |
| D8 access-token revoke vs refresh on the same grant: both complete without a deadlock, the revoked token stays revoked, the refreshed one works, the grant stays live | `TestOAuthAccessTokenRevokeVersusRefreshLiveDB` |

## References

- [PRD #1910](../prds/done/1910-connect-uzi-oauth.md): D1-D8, M1-M6.
- [ADR-1907](1907-product-api-v1-contract.md): the `/api/v1` contract and `RequireV1Caller`.
- `api/internal/oauthsrv`, `api/internal/handler/oauth.go`, `api/internal/handler/oauth_token.go`, `api/internal/handler/oauth_revoke.go`, `api/internal/store/queries/oauth.sql`, `api/openapi/v1.yaml`.
- [Connecting a product](../docs/connect-a-product.md) (users) and [Registering an OAuth client](../docs/oauth-clients.md) (admins).
