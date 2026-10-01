# PRD #1910: Connect uzi, an OAuth consent flow for external products

**Issue**: #1910
**Status**: Draft (rewritten 2026-10-01 against merged #1907, #1908, #1909 and #1976; vertical milestones)
**Priority**: Medium
**Depends on**: PRD #1907 (merged: `products`, `product_tokens`, `RequireV1Caller`, the `/api/v1` contract). The jobs a connected product runs come from PRD #1908 (merged).
**Related**: PRD #1909, PRD #1976, PRD #64 (the browser-brokered `uzi login` flow this mirrors), PRD #45 (OIDC login), #1935 (admin token inventory filters), #1992 (`/api/v1` 401 on a lookup error)

## Problem

PRD #1907 lets an external product act for a uzi user: the user mints a `uzp_` product token in Settings and pastes it into the product. That works, but:

1. **Copy-paste is the credential transport.** A long-lived bearer token passes through the clipboard, chat and tickets.
2. **Pasted tokens are long-lived and have no refresh.** A pasted token lives 30 days, 90 days (the default), a year or forever, and a product cannot renew it, so integrations pick a long or never-expiring token and a leak stays useful for that long.
3. **It does not scale past technical users.** "Open Settings, mint a token with these scopes, paste it here" is a support burden for non-developers.
4. **A pasted token is a bearer credential.** Nothing proves which application presents it (#1907 D7).

## Outcome

A registered product sends the user's browser to uzi, the user signs in and approves, and the product receives a short-lived access token plus a refresh token, bound to that user, that product and the approved scopes. The product then calls `/api/v1` exactly as with a pasted token, and keeps working unattended until the connection is revoked or expires. The user and an admin can see every connection and revoke it at any time, including after its access tokens have expired.

Acceptance examples:

1. **Connect.** An admin has given product P a redirect URI `https://p.example/cb`, scopes `jobs:run jobs:read` and a client secret. User U, signed out, clicks "Connect uzi" in P. U's browser lands on uzi's login, U signs in (password or SSO), sees a consent page naming P, `p.example`, both scopes and the line about running jobs automatically, and approves. P receives `code` and `state` at `/cb`, exchanges the code with its secret and PKCE verifier, and `GET /api/v1/whoami` with the access token answers U and P. U copied nothing.
2. **Stay connected.** Two hours later P's access token has expired. P refreshes with its refresh token and client secret, gets a new access token, and creates a job. No user action.
3. **Disconnect.** U opens Settings → Access → Connected products, sees P (even though no access token is currently unexpired) and clicks Revoke. P's next `/api/v1` call is 401, its next refresh is `invalid_grant`, and P's non-terminal jobs are cancelled by the existing revoke sweep.

## Current state (verified on `main` at `1697be81`, 2026-10-01)

- **uzi is an OAuth/OIDC client today, never a server.** `api/internal/oidc` and `api/internal/handler/oidc.go` log users in against an IdP; `api/go.mod` carries only client libraries (`golang.org/x/oauth2`, `github.com/coreos/go-oidc/v3`).
- **Products and product tokens** (`api/internal/store/migrations/00270_product_tokens.sql`):
  - `products` (id uuid, name, description, `enabled`, soft delete `deleted_at` with `products_deleted_is_disabled`). Later PRDs added `allowed_job_types` (00275), skills-source columns (00278) and the `product_egress_profiles` allowance table (00281). There is **no** redirect URI, client secret or per-product scope list.
  - `product_tokens` (user_id CASCADE, product_id RESTRICT, `token_hash` sha256 UNIQUE, `token_prefix`, `scopes text[]` CHECKed against `jobs:run`/`jobs:read`, soft-delete `revoked`, `last_used_at`/`last_used_ip`, nullable `expires_at` = never).
  - Queries in `api/internal/store/queries/product_tokens.sql`: `GetProductTokenForAuth` re-checks revoked, expiry (NULL trap), product enabled and not deleted, and user active on every request; `RevokeAllProductTokens` is the panic button's product half; `AdminRevokeProductToken` revokes one token by id; `CountActiveProductTokensForUserProduct` backs the 10-per-user-per-product cap (#1907 D15); `ListProductTokensForUser` (200 rows) and `ListAllProductTokensForAdmin` (1000 rows) feed the two lists.
- **`/api/v1` auth.** `RequireV1Caller` (`api/internal/middleware/v1_auth.go:103`) dispatches on the class prefix: `uzp_` → `product_tokens` only, `uzc_` → `cli_tokens` with row scope `user`, anything else 401. It always clears `IsAdmin` (#1907 D3) and answers every failure, including a lookup error, with the same 401 (`v1Unauthorized`, :76; the lookup-error case is #1992). `RequireScope` (:240) gates each route; routes are in `api/internal/handler/routes_v1.go:47-74`.
- **Revocation and jobs.** `ListRevokedProductJobs` (`api/internal/store/queries/jobs.sql:329`) cancels non-terminal `kind='job'` runs whose creating `product_tokens` row is **revoked**, whose product is disabled or deleted, or whose owner is inactive; expiry never cancels. Its comment already names this PRD: "a future revoke path such as the PRD #1910 OAuth grant revoke must set `product_tokens.revoked` too, or this sweep misses it". `job_origins.product_token_id` (00275) is the link, `ON DELETE SET NULL`.
- **Revoke all.** `POST /api/me/cli-tokens/revoke-all` (`handler.RevokeAllCLITokens`, `api/internal/handler/cli_tokens.go:211`) revokes CLI and product tokens in one transaction. The button in `web/src/components/CliTokens.tsx:143` (`canRevokeAll`) is **hidden when the active CLI count plus the active product-token count is zero**; `ProductTokens.tsx` counts only unexpired, unrevoked tokens (`isProductTokenActive`, :51) and offers Revoke only on those.
- **Admin surfaces.** Admin reads (`GET /api/admin/products`, `/product-tokens`, per-product skills and egress lists) use `authLimiter.PerUserMiddleware` (`api/internal/handler/routes_admin.go:122-133`); writes are cookie-only (:226-241), including `POST /product-tokens/{id}/revoke` → `AdminRevokeProductToken` (`api/internal/handler/admin_products.go:462`). The CLI is read-only for products: `uzi admin products` (`api/cmd/uzi/admin.go:303`) plus `skills` and `egress-profiles` subcommands.
- **A browser-approval flow with PKCE already exists** for `uzi login` (PRD #64), `api/internal/handler/cli_auth_flow.go`: `s256Challenge` (:114), `CLIAuthStart` (:130) behind `authLimiter` with byte caps and `termsafe` checks, pending rows in `cli_auth_requests` with `cliAuthRequestTTL` = 5 min (:38) swept by `DeleteExpiredCLIAuthRequests` (opportunistically in the start handler, :167, and on a timer in `api/cmd/server/main.go:771`), consent metadata `CLIAuthGetRequest` (:376) and `CLIAuthApprove` / `CLIAuthDeny` (:409, :496) under `RequireAuth` + CSRF (`api/internal/handler/routes_auth.go:37-45`), with a typed `user_code` binding the approval to the person who started it. Tokens are minted claim-first and never stored.
- **Login return path.** `web/src/pages/CliAuth.tsx:89` sends a signed-out user to `/login?next=…`; `safeNextPath` (`web/src/pages/Login.tsx:39`) admits only rooted, non-protocol-relative, backslash-free paths and feeds `navigate()`. **OIDC login always lands on `/`**: the callback redirects to a fixed path with no `next`/`return_to`, which closed an open redirect (`api/internal/handler/oidc.go:167-169`).
- **Cookies.** `uzi_auth` and the CSRF cookie are `SameSite=Strict` (`api/internal/auth/cookie.go:54`, `:64`). The OIDC state cookie is `SameSite=Lax`, scoped to `/api/auth/oidc`, because Strict cookies are dropped on a top-level cross-site navigation (`oidc.go:25-32`, set at :498). A product redirecting the browser to uzi is that kind of navigation, so **the first request of the flow arrives without the uzi session**.
- **Web tier headers** (`web/nginx.conf:10-20`, chart `deploy/chart/templates/web-configmap.yaml:27-29`): `X-Frame-Options DENY`, `Referrer-Policy no-referrer`, and a CSP with `frame-ancestors 'none'` and **`form-action 'self'`**. `form-action` may also block a form POST whose response redirects off-origin (browsers apply it to redirects after submission), so the consent page must not navigate to the product by submitting a form.
- **One public origin.** All browser and API traffic goes through the web tier's `location /api/` proxy (`web/nginx.conf:53`); `FrontendOrigin` decides `CookieSecure` (`api/internal/config/config.go:59-63`).
- **Token classes and scrubbing.** Prefixes in use: `uzc_`, `uza_` (`api/internal/clitoken/clitoken.go:41-49`), `uzw_` (`jointoken`), `uzp_` (`api/internal/producttoken/producttoken.go:38`), `uzf_` (`api/internal/fetchctl/fetchctl.go:57`). The uzi scrub pattern `uz[capw]_[A-Za-z0-9_-]{16,}` has three copies (`api/internal/secretscrub/secretscrub.go:46`, `api/internal/issuedraft/issuedraft.go:495`, `api/internal/workersvc/ci_fix_snapshot.go:42`), each bound to the prefix registry by a test.
- **Contract.** `api/openapi/v1.yaml` declares only `bearerAuth` (:403-406). ADR `adr/1907-product-api-v1-contract.md` fixes the `/api/v1` compatibility promise.
- **Route-table guards.** `api/internal/handler/route_limiter_mounts_test.go` derives limiter mounts from the real `h.Routes`; `route_auth_boundary_test.go` does the same for the worker/controller auth groups.

## Decisions

### D1. A minimal authorization server in-tree, not a general OAuth framework

One grant (authorization code with mandatory PKCE S256) plus refresh, confidential clients only, admin-registered clients, exact redirect-URI match, opaque tokens. No implicit or password grant, no public clients, no dynamic registration, no OpenID Connect provider features (no ID tokens, userinfo or discovery), no JWT access tokens.

- **Why not a framework** (for example `ory/fosite`): a large storage interface and many flows to disable and keep disabled, a new dependency on a security-critical path, and a second token model beside #1907's. uzi already has the load-bearing parts (PKCE check, pending-request table, authenticated consent page, claim-first minting, hashed bearer tokens).
- **Invariant:** only the surface above exists, and each behaviour follows RFC 6749 §4.1/§5/§6, RFC 7636, RFC 7009, RFC 9207 and the OAuth 2.0 Security BCP (RFC 9700), each pinned by a named test. Widening the surface (public clients, another grant) reopens this decision.
- **Cost accepted:** hand-rolled security code, mitigated by the named tests, an auditor review (M6), the ADR, and one package (`api/internal/oauthsrv`) holding the protocol logic.

### D2. Client = product; admin-registered and confidential

- `client_id` is the product's id (uuid). No second identifier.
- The admin sets on the product: one or more **redirect URIs** (exact match; `https` required, `http` only for loopback IP literals `http://127.0.0.1:<port>/…` and `http://[::1]:<port>/…`, never `localhost`; no fragment; at most 5 URIs of at most 2048 bytes each), an **allowed scope list** (non-empty subset of `jobs:run`, `jobs:read`), and a **client secret** (`uzs_` class, 256-bit random, shown once, sha256 at rest; rotation replaces it immediately).
- A product is an OAuth client only when it has at least one redirect URI, a non-empty allowed scope list and a secret. Pasted `uzp_` tokens keep working for every product, client or not.
- The token and revoke endpoints authenticate the client with HTTP Basic (`client_secret_basic`) only.

### D3. Authorize is unauthenticated, rate-limited, and binds the flow to the browser

Because the session cookie is Strict, `GET /api/oauth/authorize` cannot see the session. It:

1. sits behind `authLimiter` (per IP) and caps inputs before storing anything: `state` 1..512 bytes, `scope` at most 64 bytes, `code_challenge` exactly 43 base64url characters;
2. validates `client_id` and `redirect_uri` **first**. An unknown, disabled or deleted product, a product that is not a client (D2), or a URI that is not an exact match shows a **static error page that never redirects and never echoes a parameter** (RFC 6749 §4.1.2.1), so it cannot become an open redirect;
3. validates the rest (`response_type=code`, `code_challenge_method=S256`, `plain` and missing PKCE rejected, `state` present, requested scopes a non-empty subset of the product's allowed scopes; a missing `scope` means all allowed scopes). Failures here redirect to the registered URI with `error`, the original `state` and `iss`;
4. binds the request to this browser: a random nonce in an HttpOnly, `Secure` per `CookieSecure`, `SameSite=Lax` cookie with `Path=/api/oauth`, only its sha256 on the pending row (the OIDC state cookie is the precedent);
5. stores the pending request (5 min) and redirects to the SPA route `/connect?request=<id>`.

The SPA's own `fetch` calls are same-site, so the Strict session cookie and the Lax binding cookie are both sent. The metadata `GET` and the approve/deny `POST`s (under `/api/oauth/requests/…`) require the binding cookie to match; a `/connect` link opened in another browser shows an error. This replaces the CLI flow's typed `user_code` with the same guarantee.

**Returning after login.** A signed-out user on `/connect` is sent to `/login?next=<path>`, and the SPA also stores that path in `sessionStorage` before leaving. Password login uses `next` as today. After OIDC login the server still lands on `/` (no server-side return path; `oidc.go:167-169` unchanged), and the authenticated SPA consumes the stored path **once**, validated by `safeNextPath`, then deletes it. Works on `sso-only` instances; OIDC JIT provisioning and group gating apply unchanged.

### D4. Consent is explicit, always shown, single-use

- The page shows, as plain text (never Markdown or HTML): the product's name and description, the redirect host, each scope in plain words, and the fixed line **"This product may run jobs automatically on your behalf, as you, using your model credential configured in uzi."** Consent is shown on every authorization, even when a live grant already covers the scopes.
- Approve and Deny are `POST`s under `RequireAuth` + CSRF + the binding cookie, with `Cache-Control: no-store`. Approve is a single-use claim (one conditional update moves the row from `pending` to `approved`; a second approve or a deny after approve fails).
- On approve the server creates or updates the user's live grant for the product, issues a **single-use authorization code** (256-bit random, sha256 at rest, 60 s, bound to the grant, client, redirect URI, scopes and PKCE challenge), and returns the redirect URL (registered URI + `code` + `state` + `iss`). The SPA navigates with `window.location.assign` to that server-built URL only; it never takes a URL from the query string, and it never submits a form for this navigation (the CSP `form-action 'self'` would block it).
- Codes carry no class prefix: uzi never logs them, they live 60 s, are single use, and are useless without the client secret and the PKCE verifier.

### D5. Grants, access tokens and refresh tokens

- **Grant** = (user, product, scopes). At most one live grant per (user, product) (partial unique index on `revoked_at IS NULL`). The grant row also holds the **one live refresh token** (`uzr_` class, sha256 + display prefix, issued-at, last-used-at), so there is no refresh-token table.
- **Access token** = a `product_tokens` row (`uzp_`) with a new nullable `grant_id`, lifetime 1 h, scopes = the grant's scopes at mint. It goes through `RequireV1Caller` unchanged: same lookup, route lock, admin-flag clearing and per-request checks. No second enforcement path.
- **Refresh token** is never accepted as a bearer anywhere: `RequireV1Caller` has no `uzr_` arm and `RequireUser` knows only `cli_tokens`. It is valid only at the token endpoint with the issuing client's credentials.
- **No refresh-token rotation** for these confidential clients. RFC 9700 §4.14.2 requires rotation or sender-constraining only for public clients; RFC 6749 §6 already binds the refresh token to client authentication. Rotation would make a retried refresh after a lost response, or a product running several replicas, revoke the grant. A refresh returns a new access token and keeps the refresh token.
- **Lifetimes** (constants, not settings): access 1 h; refresh 30 days idle, 90 days absolute from consent; after either the user reconnects.
- **Refresh `scope`** (optional, RFC 6749 §6) may narrow the new access token to a subset of the grant's current scopes, never widen it (`invalid_scope`); the grant itself is unchanged.
- **Bound on live access tokens:** every access-token mint (code exchange and refresh alike) is refused while the grant already holds 10 unexpired, unrevoked access tokens, checked under the grant lock (D8). The refusal is **429 with `Retry-After`** (seconds until the oldest live token expires) and `{"error":"temporarily_unavailable"}`, an explicit extension to RFC 6749 §5.2: the request is not malformed, the product should reuse its current token. Stored rows are bounded only by the limiter-capped mint rate; they are kept after expiry (audit trail, and the job-revoke sweep keys on them), and pruning is out of scope.
- **Grant tokens are not manual tokens.** `grant_id` rows are excluded from `ListProductTokensForUser`, `ListAllProductTokensForAdmin` and `CountActiveProductTokensForUserProduct` (the D15 cap). Their lifecycle is the connection's, not the user's (they are minted and renewed by the product and revoked as a set), and an hourly history would bury manual tokens in the 200/1000-row lists. Connections have their own lists (M5).

### D6. Revoking a grant revokes everything under it, in one transaction

Every revocation path sets the grant's `revoked_at`, sets `revoked = true` on **every** `product_tokens` row with that `grant_id` (expired or not), clears its refresh token, and invalidates its unredeemed codes. Setting `revoked` is what lets the existing `ListRevokedProductJobs` sweep cancel jobs any of those tokens created, with no change to the sweep. The paths:

- the user revokes the connection (M5);
- an admin revokes the connection (M5), or revokes one product token by id that has a `grant_id` (`AdminRevokeProductToken` then revokes its grant; otherwise the product would just refresh);
- the owner revokes one product token by id that has a `grant_id` through the existing `DELETE /api/me/product-tokens/{id}` (`RevokeMyProductToken`): grant tokens are no longer listed there, but the route stays reachable by id, so it revokes the whole grant, still owner-scoped (a foreign id is 404). Manual tokens keep today's single-token behaviour (M3);
- **Revoke all** revokes every grant of the user alongside CLI and product tokens (M3), and its button counts live grants, so it stays offered when only a connection is live;
- the product revokes itself via `POST /api/oauth/revoke` (RFC 7009) with its refresh token (M4);
- **code replay**: a second redemption of an already redeemed code revokes the grant (RFC 6749 §4.1.2) **only when the replay passes every binding check**: client authentication, the code's client, the exact redirect URI and the PKCE verifier. A client-authentication failure is D7's 401 `invalid_client`; after successful authentication, a failed code, client, URI or PKCE binding is `invalid_grant`. Neither revokes anything. Client authentication alone is not enough: a stolen code injected into the honest client's callback reaches that client's authenticated exchange with the wrong verifier, and must not let an attacker disconnect the user.

**Re-consent** (approving again while a grant is live) replaces the grant's scopes with the newly approved set, revokes the previous refresh token and unredeemed codes, and revokes the earlier access tokens **only when the new set drops a scope they hold**. Reconnecting with the same or wider scopes (for example after the 90-day absolute expiry) therefore never cancels running jobs, while narrowing takes effect on the next request.

Not revoking, as for every other uzi token: password change and logout. A disabled product or deactivated user refuses refresh and access on the next request; re-enabling restores the grant.

### D7. Endpoints, origin and errors

- `/api/oauth/authorize` (GET), `/api/oauth/token` (POST, form-encoded), `/api/oauth/revoke` (POST), and the SPA-facing `/api/oauth/requests/{id}` (GET), `/approve` and `/deny` (POST), all on `FrontendOrigin` through the existing `/api/` proxy. No new ingress.
- The token and revoke endpoints have their own limiter keyed per IP and per `client_id`, a 16 KiB body cap, and send `Cache-Control: no-store` and `Pragma: no-cache`.
- `iss` is `FrontendOrigin` on every code and error redirect (RFC 9207).
- **Token endpoint requests** (RFC 6749 §§2.3, 3.1, 5.2): form-encoded only; a repeated parameter is `invalid_request`; using more than one client-authentication method (Basic plus a body `client_secret`, or a body `client_id` that differs from Basic's) is `invalid_request`; failed or missing Basic authentication is **401 `invalid_client` with `WWW-Authenticate: Basic`**.
- **Revoke endpoint** (RFC 7009 §§2.1-2.2): form-encoded, client-authenticated like the token endpoint; `token_type_hint` is advisory, an unknown hint is ignored and a misleading one falls through to the other supported type (refresh, then access); an unknown, expired or already-revoked token answers 200; a token issued to another client is refused with 400 `invalid_grant` (RFC 6749 §5.2 names a grant "issued to another client" under that code) and revokes nothing.
- **A database or lookup error is never an OAuth credential error.** The token and revoke endpoints answer **503 with `{"error":"temporarily_unavailable"}` and `Retry-After`**, an explicit extension (`temporarily_unavailable` is an authorization-endpoint code in RFC 6749 §4.1.2.1, not a §5.2 token error), never `invalid_grant` or `invalid_client`, because a product that sees `invalid_grant` discards the connection. The same lesson as #1992, applied here from day one.
- These endpoints are part of the external contract: the ADR extends ADR-1907's compatibility promise to them, and `api/openapi/v1.yaml` gains an `oauth2` security scheme (authorization-code flow URLs and scopes) beside `bearerAuth`, an additive change.

### D8. Every grant mutation serializes on the grant row

- **Lock order**, everywhere: the grant row(s) `FOR UPDATE` first (several grants, as in Revoke all, in ascending id order), then that grant's `product_tokens` rows, then its `oauth_authorize_requests` rows. No path takes a token or request row lock before its grant's.
- **Who takes it:** approve (re-consent updates the live grant under the lock; a first consent inserts conflict-safely and, on a unique-index conflict, locks the live grant with `SELECT … FOR UPDATE` in a **separate statement** (an `ON CONFLICT DO NOTHING` can meet a row its own statement snapshot cannot see), retrying if that grant was meanwhile revoked, then proceeds as re-consent), code redemption, refresh, RFC 7009 revoke, and every D6 revoke path.
- **Re-check under the lock, then act:** refresh re-reads the presented refresh hash against the grant's current one, idle and absolute expiry, `revoked_at`, product and user state and the grant's **current** scopes, and only then mints; code redemption re-reads `revoked_at` and the code's supersession. So a refresh validated before a revoke or re-consent cannot mint after it, and a revoke cannot miss a token a concurrent refresh is inserting.

## Modules and seams

- **`api/internal/oauthsrv`** (new): pure protocol logic with no HTTP or SQL: request validation and caps (D3), redirect-URI matching (D2), PKCE verification and verifier format (43-128 chars, RFC 7636 unreserved set), error-redirect construction with `state` and `iss`, scope parsing. Table-driven unit tests live here. Hides the RFC detail from the handlers.
- **Store** (`api/internal/store/queries/oauth.sql`, new; migrations draft-numbered above the live head at landing): product client columns; `oauth_grants`; `oauth_authorize_requests` (pending → approved → redeemed | denied | superseded, binding hash, code hash, code expiry, TTL sweep beside `DeleteExpiredCLIAuthRequests`); `product_tokens.grant_id` (nullable FK, `ON DELETE NO ACTION`: grants are never hard-deleted except through the owning user's cascade, which removes both in one statement; indexed). Every state change that must be single-use is one conditional `UPDATE … RETURNING` inside the handler's transaction; every grant mutation takes the grant lock first (D8).
- **Handlers** (`api/internal/handler/oauth*.go`): authorize, request metadata, approve/deny, token, revoke; mounted in a new `mountOAuthRoutes` from `Handler.Routes`, so the route-table tests see them.
- **Token classes:** `uzr_` (refresh) and `uzs_` (client secret) join the prefix registry; `r` and `s` are **added** to the three scrub-pattern copies' existing class set (keep every class already there, including `f` from #2035), and their registry-bound tests cover the new classes.
- **Web:** `web/src/pages/Connect.tsx` (consent; reuses `CliAuth.tsx` patterns), a small pending-return helper beside `safeNextPath`, "Connected products" in `AccessSettings.tsx`, the OAuth client section and connections list on the admin product card, mock data in `web/src/mocks/data/`.
- **CLI:** read-only, matching the product surfaces today. `uzi admin products` shows whether a product is a client and its scopes; `uzi admin products connections <product>` lists its grants. Writes stay browser-only (#1907 D14).

### Resource bounds (untrusted input)

| Caller | Resource | Bound | Enforced in |
|---|---|---|---|
| anyone (authorize) | pending rows | `authLimiter` per IP × 5 min TTL; inputs capped before insert | route mount, `oauthsrv` validation, sweep |
| anyone (token, revoke) | CPU, DB lookups | per-IP and per-client limiter; 16 KiB body | route mount, `http.MaxBytesReader` |
| a client | live access tokens | ≤ 10 unexpired per grant, on every mint (exchange and refresh), under the grant lock | token handler (D5, D8) |
| a client | stored `product_tokens` rows | one per mint; mint rate bounded by the per-client limiter; pruning out of scope | limiter |
| a user | grants | one live grant per (user, product) | partial unique index |
| an admin | redirect URIs | ≤ 5 × 2048 bytes, exact format | admin handler + CHECK |
| product strings on consent | rendering | admin-written, `termsafe`-validated (#1907 D11), rendered as text | existing validation, `Connect.tsx` |

Nothing irreversible happens on a declared value before verification: a code is redeemed only after client authentication, URI and PKCE checks; a replay revokes only after all of them pass (D6); RFC 7009 revoke acts only on a token issued to the authenticated client.

## Testing decisions

- **Protocol rules** as table tests in `oauthsrv` (every reject path, every cap, URI matching, PKCE).
- **Single-use and races** as live-DB tests (`./e2e/run-store-it.sh`, `RUN > 0`, zero `SKIP`, named tests `--- PASS`): approve claim, code redemption, code-before-revoke, concurrent double refresh (both succeed with the same refresh token), **refresh vs. revoke** (no live token survives the revoke), **refresh vs. re-consent** (a refresh validated before re-consent never mints with the new scopes or the old refresh token), concurrent exchanges and mixed exchange/refresh at 9 live tokens (exactly one more succeeds), and the re-consent rules (same or wider scopes keep running jobs and old tokens; narrowing revokes them; the old refresh token fails after either). Follow `cli_auth_flow_livedb_test.go` and `product_tokens_livedb_test.go`.
- **Enforcement parity:** an OAuth access token is refused on every route a pasted `uzp_` is refused on, reusing the `v1_isolation_livedb_test.go` pattern; a `uzr_` refresh token is refused as a bearer on `/api/v1` and on internal routes.
- **Route tables:** the new routes appear in `route_limiter_mounts_test.go` with their limiters.
- **Revocation → jobs:** a grant revoke cancels a job created by an earlier, already expired access token of that grant (`ListRevokedProductJobs`).
- **Web:** vitest for `Connect.tsx` (plain-text rendering, server-built redirect only, no form submit), the pending-return helper (rejects what `safeNextPath` rejects, consumed once), "Connected products" (shown with all access tokens expired), and the Revoke all button counting grants.
- **Fixtures:** token-shaped strings are assembled at runtime (`.claude/rules/prds.md`); new DTOs get recorded contract fixtures (PRD #982 pattern).
- **Rule-to-test index:** the ADR carries a table mapping each D1-D8 rule to its named test. Each milestone adds the rows for the rules it implements, with their tests; no rule's test is deferred to a later milestone.
- **Gates per milestone:** `task gate:api`, plus `task gate:web` where the SPA changes, plus `task scan:secrets` with the canaries-detected line in the run's evidence (this PRD touches the scrubbers and adds token-shaped fixtures, `.claude/rules/prds.md`); wherever a milestone changes `docs/*.md`, `task docs:sync` and `task check-docs:web`. No file under `.github/workflows/` is created or changed.

## Milestones

Each milestone is one verifiable behaviour, with its tests and docs inside it.

- [ ] **M1. An admin can make a product an OAuth client.** Migration: redirect URIs, allowed scopes, client-secret hash/prefix/rotated-at on `products`. Cookie-only admin writes to set URIs and scopes and to rotate the secret (shown once); validation per D2. Admin product card section; `uzi admin products` shows client status and scopes. `uzs_` registered and scrubbed (three pattern copies + tests). Operator docs: registering a client.
  - Blocked by: none.
  - Acceptance: admin sets two URIs and one scope, rotates the secret and sees it once; `localhost`, a fragment, `http` on a non-loopback host and a sixth URI are refused; `uzi admin products --json` shows the client fields and never the secret or its hash.
- [ ] **M2. A product sends a user through consent and receives a code.** Migrations: `oauth_grants`, `oauth_authorize_requests`. `GET /api/oauth/authorize` (D3), binding cookie, pending sweep, `/connect` page with metadata/approve/deny (D4), return after password and OIDC login, grant create and re-consent scope replacement, code issuance, supersession of earlier unredeemed codes.
  - Blocked by: M1.
  - Acceptance: a table test covers every authorize reject path and cap, and an unknown client or mismatched URI never yields a `Location` header nor echoes a parameter; **consent fixation** (a request created in browser A cannot be viewed or approved from browser B) and **double approve** tests pass; signed-out password and `sso-only` OIDC users land back on `/connect` after login; the redirect carries `code`, unchanged `state` and `iss`; approve and deny responses are `no-store`.
- [ ] **M3. The product exchanges the code and calls `/api/v1`.** `POST /api/oauth/token` with `authorization_code`: Basic client auth, code redemption in one transaction with the grant locked, exact URI match, PKCE S256 and verifier format, a `product_tokens` access token with `grant_id`, the grant's `uzr_` refresh token, limiter, 503 on a lookup error (D7). Grant lock order (D8). Ten-live-token cap on exchange (D5). Token-endpoint request rules (D7). Code replay revokes the grant only after every binding check (D6). Re-consent revocation rules (D6). `RevokeMyProductToken` on a grant token revokes the grant (D6). Grant rows excluded from the manual lists and the D15 cap (D5). Revoke all revokes grants and its button counts them. `uzr_` registered and scrubbed. OpenAPI `oauth2` scheme. ADR adr/1910-oauth-authorization-server.md recording D1's invariant, D5's no-rotation choice and D7's contract extension. User docs: connecting a product.
  - Blocked by: M2.
  - Acceptance: `/api/v1/whoami` with the issued token answers the user and product; the token is refused wherever a pasted `uzp_` is; a code is refused after 60 s, on a second use, with another client, another URI or a wrong verifier; a code approved before a revoke or a re-consent cannot be redeemed after it (race test); a replay with the wrong verifier or another client revokes nothing, a fully bound replay revokes the grant; a repeated parameter or two auth methods is `invalid_request`, bad Basic is 401 with `WWW-Authenticate`; a DB failure on the token endpoint is a 503, not `invalid_grant`; the eleventh live token is a 429 with `Retry-After`; the owner revoking a grant token by id kills the grant, a foreign id is 404 and a manual token still revokes alone; Revoke all leaves no live grant and the button is shown when only a grant is live.
- [ ] **M4. The product stays connected and can disconnect itself.** `grant_type=refresh_token` (client-authenticated, no rotation, 30 d idle / 90 d absolute, optional narrowing `scope`, the ten-live-token cap, re-checks under the grant lock per D8) and `POST /api/oauth/revoke` (RFC 7009, D7) for the client's own refresh or access token.
  - Blocked by: M3.
  - Acceptance: a concurrent double refresh succeeds twice with the same refresh token; refresh with another client's credentials, after idle or absolute expiry, for a disabled product or an inactive user is refused; a `scope` wider than the grant is `invalid_scope` and a narrower one yields a narrower token; the eleventh live access token is a 429; refresh-vs-revoke and refresh-vs-re-consent race tests pass; a refresh token presented as a bearer is 401 everywhere; RFC 7009 revoke with the refresh token kills the grant and its access tokens, a token issued to another client is refused and revokes nothing (RFC 7009 §2.1), and an unknown or already-revoked token answers 200 and an unknown or misleading `token_type_hint` still finds the token (§2.2), and a storage failure is a 503.
- [ ] **M5. Users and admins see and revoke connections.** Settings → Access → **Connected products** (product, scopes, connected at, last used, Revoke) listing every live grant whatever the state of its access tokens; admin per-product connections list and revoke (cookie-only write, read under `authLimiter.PerUserMiddleware`); `AdminRevokeProductToken` on a grant token revokes its grant; the admin delete confirm counts live connections beside tokens; `uzi admin products connections <product>`; user and CLI docs (`docs/product-tokens.md` revocation table, `docs/cli.md`).
  - Blocked by: M3.
  - Acceptance: a grant whose access tokens have all expired is listed and revocable; a revoke from either list makes the next `/api/v1` call 401 and the next refresh `invalid_grant`, and the revoke sweep cancels a job created by an expired access token of that grant; an admin revoking one grant token kills the grant.
- [ ] **M6. Whole-flow security audit.** The `auditor` reviews `api/internal/oauthsrv`, the OAuth handlers and queries and `Connect.tsx` against D1-D8 and RFC 9700, and checks the ADR's rule-to-test index for completeness. Every finding is fixed, with its regression test, or recorded with a reason in the Decision Log. This milestone verifies; it is not where a slice's own tests are written.
  - Blocked by: M4, M5.
  - Acceptance: the auditor's report is attached to the PR; every index row names a test that passes; no finding is left unaddressed.

M4 and M5 are parallel candidates after M3 (token endpoint vs. lists and UI); run them in parallel only when their files are disjoint.

Live acceptance on a hosted cluster (real ingress, password and `sso-only` OIDC users, real browsers for the Strict/Lax cookie handoff) is tracked in a linked `acceptance` issue owned by the maintainer. It does not block merge.

## Out of scope

- Public clients, the implicit and password grants, device flow for products.
- OpenID Connect provider features: ID tokens, userinfo, discovery or metadata documents (RFC 8414).
- Dynamic or self-service client registration; client-secret overlap during rotation.
- JWT access tokens; token introspection for third parties.
- Pruning expired grant access-token rows (kept for the audit trail and the revoke sweep; revisit on a measured row-count need).
- Configurable lifetimes.
- Any change to pasted `uzp_` tokens, which keep working beside OAuth-issued ones.
- The `/api/v1` lookup-error 401 (#1992) and admin inventory filters (#1935).
- Letting `CliAuth.tsx` use the new pending-return helper for `uzi login` on `sso-only` instances (a separate small fix).
- Anything product-specific.

## Risks

| Risk | Mitigation |
|---|---|
| Hand-rolled OAuth gets a rule wrong | Narrow surface (D1), one named test per rule, auditor review (M6), ADR |
| Consent fixation or session swap through a shared `/connect` link | Browser-binding cookie on metadata, approve and deny (D3); single-use approve (D4) |
| Open redirect via `redirect_uri`, the SPA sink or the login return | Exact match before any redirect (D3); server-built URL only (D4); `safeNextPath` on a client-side return, no server return path (D3) |
| Unauthenticated authorize floods the pending table | `authLimiter`, caps before insert, 5 min TTL sweep |
| Strict session cookie breaks the flow in some browser | Unauthenticated authorize hands off to a same-site SPA page; verified in the acceptance issue |
| Leaked refresh token | Useless without the client secret; idle and absolute expiry; user, admin and product revoke; Revoke all |
| A DB blip disconnects every product | 503 `temporarily_unavailable`, never `invalid_grant` (D7) |
| Reconnecting cancels running jobs | Re-consent revokes access tokens only when scopes narrow (D6) |
| Access-token rows grow without bound | ≤ 10 live per grant on every mint, limiter-bounded mint rate, excluded from manual lists (D5); pruning parked |
| A refresh or exchange races a revoke or re-consent | One lock order on the grant row and re-checks under it (D8); race tests |
| Consent phishing with a lookalike product name | Only admins register products; consent shows the redirect host |

## Decision Log

- 2026-09-29: PRD drafted. Minimal in-tree authorization server (D1); consent always shown; access tokens reuse #1907's enforcement.
- 2026-09-29: Revised after buddy and security review: browser-binding Lax cookie and single-use approve; `authLimiter` and input caps; client-side `sessionStorage` return path for OIDC; re-consent replaces scopes; per-product allowed scopes; no refresh rotation for confidential clients; `no-store`, `iss`, RFC 7636 verifier checks, loopback IP literals only, no parameter echo; Revoke all and admin revoke cover grants.
- 2026-10-01: Rewritten against merged code (`1697be81`) with the buddy. **Kept as one PRD** rather than split into "connect" and "manage connections": a safe "connect" half still needs a grant-revoke surface that works once every access token has expired (`ProductTokens.tsx` offers Revoke only on unexpired tokens; `CliTokens.tsx` hides Revoke all at zero active tokens), so the split would have left little in the second PRD. Milestones re-sliced vertically; tests and docs ride in each slice; live acceptance moved to a linked `acceptance` issue, per the current PRD rules.
- 2026-10-01: The refresh token lives on the grant row (one live refresh token per grant), not in its own table; codes live on the pending-request row. Fewer tables, same guarantees.
- 2026-10-01: Grant revocation sets `product_tokens.revoked` on every token of the grant, so the existing `ListRevokedProductJobs` sweep cancels the grant's jobs unchanged, as its own comment requires. Rejected: a `grant_id` on `job_origins` and a sweep change.
- 2026-10-01: Grant-issued tokens are excluded from the manual token lists and the D15 cap, and bounded at 10 live per grant on every mint. Reason: their lifecycle belongs to the connection (minted by the product, revoked as a set), and their hourly history would bury manual tokens in the 200/1000-row lists. Rejected: counting them against D15, which would couple a product's refresh pattern to the user's manual-mint budget.
- 2026-10-01: Re-consent revokes earlier access tokens only when scopes narrow, so reconnecting after the 90-day expiry never cancels running jobs. Rejected: always revoking (cancels jobs on every reconnect) and never revoking (a narrowing would wait up to 1 h).
- 2026-10-01: Code replay revokes the grant only when the replay passes client authentication, client, redirect-URI and PKCE checks. Rejected: revoking on any replay, or on client authentication alone, either of which lets an attacker holding a stolen code (for example by injecting it into the honest client's callback) disconnect a user.
- 2026-10-01 (buddy re-review): another client's token at RFC 7009 revoke is `invalid_grant`, not `unauthorized_client` (that code is about the grant type); D6 separates authentication failure (401 `invalid_client`) from binding failure (`invalid_grant`); D8's first-consent conflict lock is a separate statement.
- 2026-10-01 (buddy review): added D8 (one lock order on the grant row, re-checks under it, race tests); the cap applies to code exchange too and answers 429 with `Retry-After`; refresh `scope` may only narrow; token and revoke endpoint request rules pinned (RFC 6749 §§2.3/3.1/5.2, RFC 7009 §§2.1-2.2); `RevokeMyProductToken` revokes a grant token's whole grant; the rule-to-test index is built per milestone and M6 only audits; `task scan:secrets` joins the gate line.
- 2026-10-01: Token-endpoint lookup errors are 503 `temporarily_unavailable` (D7), applying #1992's lesson before the endpoint ships.
- 2026-10-01: Lifetimes are constants (1 h, 30 d idle, 90 d absolute), not settings; the maintainer fixed the values on 2026-09-29 and no caller needs to change them.
- 2026-10-01: `client_id` is the product id; redirect URIs capped at 5 × 2048 bytes; at most one live grant per (user, product).
