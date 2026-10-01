# ADR-1910: uzi is a minimal in-tree OAuth authorization server for registered products

**Status**: Accepted (PRD #1910 M1-M3 implemented; M4 refresh and revoke endpoints, M5 connection lists and M6 audit are not done)
**Date**: 2026-10-01
**Issue**: [vtmocanu/uzi#1910](https://github.com/vtmocanu/uzi/issues/1910)
**PRD**: [prds/1910-connect-uzi-oauth.md](../prds/1910-connect-uzi-oauth.md)

## Decision (summary)

> uzi runs a deliberately small OAuth 2.0 authorization server in-tree: the
> authorization-code grant with mandatory PKCE S256 plus refresh, confidential
> clients only, admin-registered clients, exact redirect-URI match and opaque
> tokens. An access token is an ordinary `product_tokens` row with a `grant_id`, so
> `RequireV1Caller` is its only enforcement path. Every mutation of a grant takes
> the grant row lock first, and revoking a grant revokes everything under it in one
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
   new access token and keeps the refresh token. The lifetimes are constants:
   access 1 hour, refresh 30 days idle and 90 days absolute from consent.
5. **Bound on live access tokens (D5).** Every mint is refused while the grant
   already holds 10 unexpired, unrevoked access tokens, counted under the grant
   lock. The refusal is a 429 with `Retry-After` (seconds until the oldest live
   token expires) and `{"error":"temporarily_unavailable"}`.
6. **Grant tokens are not manual tokens (D5).** Rows with a `grant_id` are
   excluded from the user and admin token lists, from the per-product active
   counts and from the 10-token manual mint cap. Their lifecycle is the
   connection's.
7. **Revoking a grant revokes everything under it, in one transaction (D6).** It
   sets `revoked_at`, sets `revoked` on every token with that `grant_id` (expired
   or not, so the existing `ListRevokedProductJobs` sweep cancels the jobs of an
   already expired token), clears the refresh token and supersedes unredeemed
   codes. Every path calls the one helper, `revokeGrantLocked`. Revoke all, the
   owner's or an admin's revoke of one grant token by id, and a code replay reach
   it in M3; the user's and admin's connection revoke and RFC 7009 revoke follow.
8. **Every grant mutation serializes on the grant row (D8).** The lock order is
   the grant row (several grants in ascending id order), then that grant's
   `product_tokens` rows, then its `oauth_authorize_requests` rows, on every
   path. After the lock the path re-checks and then acts: a redemption re-reads
   the request, the grant's `revoked_at`, the client and the user, and counts the
   live tokens, so a revoke cannot miss a token a concurrent exchange is inserting.
9. **Contract extension (D7).** `POST /api/oauth/token` and
   `POST /api/oauth/revoke` are part of the external contract: ADR-1907's
   compatibility promise (additive only, a deprecation window for anything
   breaking) applies to their request and response shapes, and
   `api/openapi/v1.yaml` gains an `oauth2` security scheme (authorization-code
   flow URLs and the two scopes) as a new alternative entry beside `bearerAuth`,
   which `check:api-v1-compat` rates as an additive change. Two extensions to RFC
   6749 are deliberate and are part of that promise:
   - a **database or lookup error is a 503 `{"error":"temporarily_unavailable"}`
     with `Retry-After`**, never `invalid_grant` or `invalid_client`, because a
     product that sees `invalid_grant` discards the connection (the lesson of
     #1992 applied here from day one); `temporarily_unavailable` is an
     authorization-endpoint code in RFC 6749 section 4.1.2.1, not a section 5.2
     token error;
   - a **429 `temporarily_unavailable` with `Retry-After`** for the 10-token bound
     and for the per-IP and per-client limiters: the request is not malformed, the
     product should reuse its token or wait.
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
- M4 (refresh and RFC 7009 revoke), M5 (connection lists, user and admin revoke
  of a connection, admin counts) and M6 (audit) are not built; their rows in the
  index below say so.

## Rule-to-test index

Each row names the tests that pin a rule implemented so far. Tests whose names end
in `LiveDB` need a database (`./e2e/run-store-it.sh`); the others run in
`task test:api` and `task gate:web`. A row marked M4 or M5 is pending in that
milestone.

| Rule | Pinned by |
|---|---|
| D1 only the listed surface exists: no public client, no other grant, Basic only, PKCE S256 and `code` only | `TestValidateAuthorizeRejects`, `TestOAuthTokenRequestRulesLiveDB` (unsupported grant type), `TestOAuthTokenClientAuthenticationLiveDB` (a body secret alone, a bearer, a non-client) |
| D1 protocol logic in one package, RFC verifier and PKCE rules | `TestValidVerifier`, `TestVerifierMatchesS256`, `TestParseTokenForm`, `TestParseScopes`, `TestValidateScopes`, `TestRedirectURLs` |
| D2 redirect-URI format and exact match, client secret class and constant-time check | `TestValidateRedirectURI`, `TestValidateRedirectURIs`, `TestValidateRedirectURIsErrorDoesNotEchoTheURI`, `TestGenerateSecret`, `TestSecretMatches`, `TestAdminProductOAuthClientLiveDB`, `TestAdminProductOAuthValidationLiveDB`, `TestAdminProductOAuthUnknownAndDeletedLiveDB`, `TestAdminProductOAuthRoutesAuthLiveDB`, `TestOAuthTokenRotatedSecretCutsOffTheOldOneLiveDB` |
| D2 `uzs_` and `uzr_` are registered and scrubbed | `TestMintedPrefixesScrubbedOnBothPaths`, `TestScrubSecretShapesMintedUziPrefixes`, `TestScrubKnownTokensMintedUziPrefixes`, `TestGenerateRefreshToken` |
| D3 authorize is unauthenticated, validated before any redirect, caps, binding cookie, bounded storage | `TestCheckClientRejectsBeforeAnyRedirect`, `TestOAuthAuthorizeStaticErrorWithoutDB`, `TestOAuthAuthorizeStaticErrorLiveDB`, `TestOAuthAuthorizeRejectsRedirectToRegisteredURILiveDB`, `TestOAuthAuthorizeSuccessStoresHashAndSetsCookieLiveDB`, `TestOAuthAuthorizePendingCapLiveDB`, `TestOAuthSourceBucketsFor`, `TestWellFormedOAuthNonce`, `TestOAuthSweepAndCapQueriesUseIndexesLiveDB`, `TestOAuthAuthorizeLockClassMatchesSQL` |
| D4 consent is explicit, bound to the browser, single use | `TestOAuthRequestMetadataLiveDB`, `TestOAuthConsentFixationLiveDB`, `TestOAuthApproveRedirectCarriesCodeStateIssLiveDB`, `TestOAuthApproveRedirectKeepsRegisteredQueryLiveDB`, `TestOAuthDoubleApproveAndDenyLiveDB`, `TestOAuthConcurrentApproveLiveDB`, `TestOAuthLosingApproveRollsBackSupersedeLiveDB`, `TestOAuthDenyStaleRequestIsRefusedLiveDB`, `TestOAuthSweepKeepsRecentCodesLiveDB`; web: `Connect.test.tsx`, `pendingReturn.test.ts`, `AppShell.pendingReturn.test.tsx` |
| D5 code exchange mints an access token and the grant's refresh token; the token works on `/api/v1` and nowhere a pasted token is refused; the refresh token is no bearer | `TestOAuthTokenExchangeSuccessLiveDB`, `TestOAuthTokenAccessTokenIsRefusedWhereAPastedTokenIsLiveDB` |
| D5 the access token carries only the grant's scopes; a code lives 60 seconds | `TestOAuthTokenAccessTokenCarriesOnlyTheGrantsScopesLiveDB`, `TestOAuthTokenCodeExpiresAfterSixtySecondsLiveDB` |
| D5 one live grant per (user, product); re-consent rules | `TestOAuthConcurrentFirstConsentsShareOneGrantLiveDB`, `TestOAuthReconsentLiveDB`, `TestOAuthApproveAfterRevokedGrantCreatesNewGrantLiveDB` |
| D5 ten live access tokens per grant, counted under the grant lock | `TestOAuthTokenLiveTokenCapLiveDB`, `TestOAuthTokenCapIsCountedUnderTheGrantLockLiveDB` |
| D5 grant tokens are not manual tokens (lists, counts, mint cap) | `TestGrantTokensAreNotManualTokensLiveDB`; web: "Revoke all counts live OAuth connections" in `ProductTokens.test.tsx` |
| D5 refresh grant: no rotation, idle and absolute expiry, narrowing `scope`, ten-token bound on refresh | M4 |
| D6 code replay revokes the grant only after every binding check | `TestOAuthTokenReplayRevokesGrantLiveDB`, `TestOAuthTokenReplayWithBrokenBindingRevokesNothingLiveDB`, `TestOAuthTokenReplayAfterCodeExpiryStillRevokesLiveDB`, `TestOAuthTokenAnotherClientsCodeLiveDB` |
| D6 a code approved before a revoke or a re-consent cannot be redeemed after it | `TestOAuthTokenCodeApprovedBeforeGrantRevokeIsDeadLiveDB`, `TestOAuthTokenCodeApprovedBeforeReconsentIsSupersededLiveDB`, `TestRevokeAllRacingCodeExchangeLiveDB` |
| D6 the owner revoking a grant token by id kills the grant; a foreign id is 404; a manual token revokes alone | `TestRevokeMyProductTokenOnGrantTokenKillsGrantLiveDB`, `TestRevokeMyProductTokenForeignGrantAndManualTokenLiveDB` |
| D6 an admin revoking a grant token by id kills the grant | `TestAdminRevokeGrantTokenKillsGrantLiveDB` |
| D6 Revoke all leaves no live grant, and its button counts grants | `TestRevokeAllRevokesGrantsLiveDB`, `TestRevokeAllRacingCodeExchangeLiveDB`; web: "Revoke all counts live OAuth connections" in `ProductTokens.test.tsx`, `CliTokens.test.tsx`, `mockApi.productTokens.test.ts` |
| D6 revoke cancels jobs of an expired access token | `TestCancelRevokedProductJobsExpiredGrantTokenLiveDB` |
| D6 user revoke of a connection, admin revoke of a connection, RFC 7009 revoke by the product | M5 (user and admin), M4 (RFC 7009) |
| D7 token request rules: form only, no repeated parameter, one client-authentication method, 401 with `WWW-Authenticate` | `TestOAuthTokenRequestRulesLiveDB`, `TestOAuthTokenClientAuthenticationLiveDB` |
| D7 a storage error is a 503, never a credential error, on every step including the replay revoke | `TestOAuthTokenStorageErrorIsNeverACredentialErrorLiveDB`, `TestOAuthTokenReplayStorageErrorIsA503LiveDB` |
| D7 limiter per IP and per client, per-client budget drawn only after authentication, OAuth-shaped 429, `no-store` on every method | `TestOAuthTokenPerClientBudgetIgnoresFailedAuthenticationLiveDB`, `TestOAuthTokenIsBehindThePerIPOAuthLimiter`, `TestOAuthRoutesAreNoStoreOnEveryMethod` |
| D7 `/api/me/oauth-connections` is cookie-only and lists live grants whatever their tokens' state | `TestOAuthConnectionsRouteRefusesBearerLiveDB`, `TestMyOAuthConnectionsLiveDB`, `TestEveryRouteCarriesItsExpectedPerUserLimiter` (its row) |
| D7 the `oauth2` scheme is an additive alternative and nothing else in `v1.yaml` moved | `TestV1OpenAPIRouteParity`, the `check:api-v1-compat` gate |
| D7 RFC 7009 revoke endpoint | M4 |
| D8 lock order grant, tokens, requests on every path; replay takes no request-row lock | `TestOAuthTokenReplayTakesNoRequestRowLockLiveDB`, `TestOAuthTokenCapIsCountedUnderTheGrantLockLiveDB`, `TestOAuthTokenConcurrentSameCodeLiveDB`, `TestRevokeAllRacingCodeExchangeLiveDB` |
| D8 approve and first consent under the grant lock | `TestOAuthConcurrentApproveLiveDB`, `TestOAuthConcurrentFirstConsentsShareOneGrantLiveDB`, `TestOAuthLosingApproveRollsBackSupersedeLiveDB` |
| D8 refresh vs revoke and refresh vs re-consent races | M4 |

## References

- [PRD #1910](../prds/1910-connect-uzi-oauth.md): D1-D8, M1-M6.
- [ADR-1907](1907-product-api-v1-contract.md): the `/api/v1` contract and `RequireV1Caller`.
- `api/internal/oauthsrv`, `api/internal/handler/oauth.go`, `api/internal/handler/oauth_token.go`, `api/internal/store/queries/oauth.sql`, `api/openapi/v1.yaml`.
- [Connecting a product](../docs/connect-a-product.md) (users) and [Registering an OAuth client](../docs/oauth-clients.md) (admins).
