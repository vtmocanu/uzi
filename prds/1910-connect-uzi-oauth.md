# PRD #1910: Connect uzi, an OAuth consent flow for external products

**Issue**: #1910
**Status**: Draft (revised 2026-09-29 after buddy and security review)
**Priority**: Medium
**Depends on**: PRD #1907 (product registration, the `product_tokens` table, `RequireV1Caller`, route lock and per-request checks). Live acceptance (M9) also needs PRD #1908 (the `/api/v1/jobs` endpoints the tokens are for).
**Related**: PRD #1906, PRD #1909, PRD #64 (CLI tokens and the browser-brokered `uzi login` flow this PRD mirrors), PRD #45 (OIDC login)

## Problem

PRD #1907 lets an external product act for a uzi user: the user mints a product token in uzi and pastes it into the product. That works, but it has three costs:

1. **Copy-paste is the credential transport.** A long-lived bearer token passes through the user's clipboard, chat messages and tickets.
2. **Nothing expires on its own.** A pasted token lives until someone revokes it; a leak is silent.
3. **It does not scale past technical users.** "Open Settings, mint a token with these scopes, paste it here" is a support burden for non-developers.

External products acting for their users should not require copy-pasting tokens. The standard answer is an OAuth 2.0 authorization-code flow: the product sends the user to uzi, the user logs in and approves, and the product receives short-lived access tokens plus a refresh token, bound to that user, that product and the approved scopes.

## Current state (verified on `main`, 2026-09-29)

- **uzi is an OAuth/OIDC client today, never a server.** `api/internal/oidc` and `api/internal/handler/oidc.go` implement login *against* an IdP. `api/go.mod` carries `golang.org/x/oauth2` and `github.com/coreos/go-oidc/v3` (both client libraries); no authorization-server library is vendored.
- **A browser-approval flow with PKCE already exists** for `uzi login` (PRD #64 M5), `api/internal/handler/cli_auth_flow.go`:
  - the client sends only a PKCE S256 challenge to `POST /api/auth/cli/start` (`CLIAuthStart`, :130); `s256Challenge` is at :114; the start endpoint sits behind `authLimiter` with byte caps and `termsafe` checks on its inputs (:138-160);
  - a short-lived pending row lives in `cli_auth_requests` (`api/internal/store/migrations/00068_cli_auth_requests.sql`, TTL `cliAuthRequestTTL` = 5 min, :38);
  - the human approves in an authenticated SPA tab: `GET /api/auth/cli/request/{id}` returns consent metadata (:376), `POST /api/auth/cli/approve` / `deny` (:409, :496), under `RequireAuth` + CSRF (`api/internal/handler/routes_auth.go:37-45`). The approving tab must also type a `user_code` shown by the CLI (`routes_auth.go:40-44`), which binds the approval to the person who started the flow;
  - the token is minted claim-first inside the poll transaction and never stored, not even sealed (:26-32).
- **The SPA consent page and login redirect-back exist.** `web/src/pages/CliAuth.tsx:89` sends a signed-out user to `/login?next=...`; `safeNextPath` in `web/src/pages/Login.tsx:39` admits only rooted, non-protocol-relative, backslash-free paths.
- **OIDC login always lands on `/`.** The OIDC callback deliberately has no `next` / `return_to` parameter, because that closed an open redirect (`api/internal/handler/oidc.go:167-169`). A server-side return path must not be reintroduced.
- **Session cookies are `SameSite=Strict`**: `uzi_auth` and the CSRF cookie (`api/internal/auth/cookie.go:54`, `:64`). The OIDC state cookie is `Lax` precisely because "Strict cookies are dropped on such a navigation" (a top-level cross-site GET redirect, `api/internal/handler/oidc.go:26-29`). A product redirecting the user's browser to uzi is the same kind of navigation, so **the first request of the flow arrives without the uzi session**.
- **Bearer tokens are sha256 at rest with a display prefix** (`api/internal/store/migrations/00067_cli_tokens.sql`), looked up by hash in `middleware.RequireUser` (`api/internal/middleware/cli_auth.go:39`, lookup :53, scope ceiling :85). Token class prefixes are registered in `clitoken.Prefixes` (`api/internal/clitoken/clitoken.go:40-49`), which the secret scrubber consumes (`api/internal/secretscrub/secretscrub_test.go:50-54`). PRD #1907 adds product tokens in their own `product_tokens` table with the `uzp_` class, resolved only by `RequireV1Caller`.
- **Clickjacking and Referer leaks are already covered at the web tier**: `X-Frame-Options DENY`, `frame-ancestors 'none'` and `Referrer-Policy no-referrer` in both compose (`web/nginx.conf:10,11,20`) and the chart (`deploy/chart/templates/web-configmap.yaml:27,29`).
- **One public origin.** All browser and API traffic goes through the web tier's `location /api/` proxy (`web/nginx.conf:53`); `FRONTEND_ORIGIN` is the user-facing origin and decides `CookieSecure` (`api/internal/config/config.go:58-61`).

## Decisions

### D1. Build a minimal authorization server in-tree; do not adopt a general OAuth framework

Scope is deliberately narrow: **one grant (authorization code with mandatory PKCE S256) plus refresh tokens, confidential clients only, admin-registered clients, exact redirect-URI match, opaque tokens.** No implicit or password grant, no public clients, no dynamic client registration, no OpenID Connect provider (no ID tokens), no JWT access tokens.

- **Why not a framework** (for example `ory/fosite`): it brings a large storage interface and many flows we would have to disable and keep disabled, a new dependency on a security-critical path, and a second token model beside the one PRD #1907 already enforces. The flow we need is small, and uzi already has its load-bearing parts: a PKCE challenge check, a pending-request table, an authenticated consent page, claim-first minting in a transaction, and hashed bearer tokens (see *Current state*).
- **The invariant this relies on:** the server implements only the narrow surface above, and every behavior follows the OAuth 2.0 Security BCP (RFC 9700), RFC 6749 §4.1 / §5 / §6, RFC 7636 (PKCE) and RFC 9207 (`iss`), pinned by a named test per rule (M7). Any request to widen the surface (public clients, another grant) reopens this decision.
- **Cost accepted:** hand-rolled security code. Mitigated by the checklist tests, the auditor review in M7, and by keeping the code in one package.

### D2. The authorize endpoint is unauthenticated, rate-limited, and binds the flow to the browser

Because the session cookie is `SameSite=Strict`, `GET /api/oauth/authorize` cannot see the user's session. It therefore:

1. is behind `authLimiter` (per IP), like `POST /api/auth/cli/start`, and caps its inputs before anything is stored: `state` at most 512 bytes, the scope list bounded by the product's allowed scopes (D5), `code_challenge` exactly 43 base64url characters (RFC 7636 S256 output);
2. validates `client_id` and `redirect_uri` first. **If either is unknown or does not match exactly, it shows a static error page and never redirects** (RFC 6749 §4.1.2.1), so it cannot become an open redirect. The error page never echoes any query parameter;
3. validates the rest (`response_type=code`, `code_challenge` present with `code_challenge_method=S256`, `plain` rejected, `state` present, requested scopes a subset of the product's allowed scopes). Failures here redirect to the registered URI with `error`, the original `state` and `iss`;
4. **binds the pending request to this browser**: it generates a random nonce, sets it in an HttpOnly, `Secure` (per `CookieSecure`), `SameSite=Lax` cookie scoped to `Path=/api/oauth`, and stores only its sha256 on the pending row. The OIDC state cookie is the precedent (`oidc.go:26-29`): `Lax` survives the top-level cross-site navigation, `Strict` would not;
5. stores the short-lived pending request (5 min, like `cliAuthRequestTTL`) and redirects to the SPA route `/connect?request=<id>`.

The SPA page's own `fetch` calls are same-site, so the Strict session cookie and the Lax binding cookie are both sent. The metadata `GET` and the approve/deny `POST`s (under `/api/oauth/...`) **require the binding cookie to match the pending row's hash**; a `/connect?request=<id>` link opened in any other browser shows an error. This replaces the CLI flow's typed `user_code` with the same guarantee: only the browser that started the flow can approve it.

**Returning after login (explicit decision).** A signed-out user is sent to `/login`. The SPA stores `/connect?request=<id>` in `sessionStorage` as `next` before leaving, and the login page (password or OIDC) reads it back after sign-in and validates it with `safeNextPath` before navigating. The server-side OIDC callback is unchanged and still lands on `/` (no server-side return path, preserving the fix at `oidc.go:167-169`); the SPA at `/` picks up the stored `next`. This works on `sso-only` instances. OIDC JIT provisioning and group gating apply unchanged.

### D3. Consent is explicit, always shown, and single-use

The consent page shows, as plain text (never rendered as Markdown or HTML): the product's admin-set name, the redirect host, each requested scope in plain words, and the fixed line **"This product may run jobs automatically on your behalf, as you, using your model credential configured in uzi."** Approve and Deny are `POST`s under `RequireAuth` + CSRF + the D2 binding cookie. Consent is shown on every authorization, even when an active grant already covers the scopes: connecting is rare, and a silent re-grant is the riskier default.

Approve is a **single-use claim**: one transaction moves the pending row from `pending` to `approved` (a conditional update that affects exactly one row), and a second approve or a deny after approve fails. On approve, the server records or updates the grant, issues a **single-use authorization code** (random, sha256 at rest, 60 s lifetime, bound to client, user, redirect URI, scopes and PKCE challenge), and returns the redirect URL (registered URI + `code` + `state` + `iss`). The SPA navigates to it with `window.location.assign`. That sink is safe only because the URL is built server-side from the stored, registered URI; the page must not accept a URL from the query string. The approve and deny responses carry `Cache-Control: no-store`.

**Re-consent replaces, never widens silently.** When a user approves again for a product they already granted, the grant's scopes become exactly the newly approved set, and every access and refresh token issued under the earlier approval is revoked in the same transaction, together with any **unredeemed authorization code** for that grant. Every revocation path (re-consent, user or admin grant revoke, "Revoke all") invalidates the grant's outstanding codes too, and the token endpoint re-checks the grant is live when redeeming a code, so a code approved before a revoke cannot be exchanged after it. The "Connected products" list therefore always matches what is live.

### D4. Tokens reuse PRD #1907's product token and its enforcement

- **Access token** = a PRD #1907 `product_tokens` row (`uzp_` class) with a short lifetime (default 1 h, configurable) and a nullable `grant_id` column added by M1. It goes through exactly the same `RequireV1Caller` lookup, route lock, admin-flag clearing and per-request checks as a pasted product token. No second enforcement path. Its scopes are the grant's current scopes.
- **Refresh token** = a separate opaque token (own class prefix, sha256 at rest) that is **never accepted as a bearer** on any route. It is valid only at the token endpoint, and only when presented together with the client credentials of the client it was issued to.
- **No rotation for these confidential clients (simplest safe choice).** RFC 9700 §4.14.2 requires rotation or sender-constraining only for public clients; for a confidential client, RFC 6749 §6 already binds the refresh token to client authentication, so a stolen refresh token is useless without the client secret. Rotation with reuse detection would add a failure mode (a retried refresh after a lost response, or a product running several replicas, revokes the whole grant) for no gain against that threat. A refresh therefore returns a new access token and keeps the same refresh token. Revisit only if public clients are ever allowed (D1).
- **Code replay**: a second exchange of the same code fails and revokes tokens already issued from it (RFC 6749 §4.1.2, RFC 9700 §4.2.4).
- **PKCE verifier**: must be 43 to 128 characters from the RFC 7636 unreserved set; anything else is `invalid_grant`.
- **Lifetimes**: refresh tokens expire after 30 days idle and 90 days absolute (configurable), after which the user reconnects.
- **Token responses** carry `Cache-Control: no-store` and `Pragma: no-cache` (RFC 6749 §5.1).
- New class prefixes (refresh token, client secret) are added to the registry the secret scrubber reads, and to every copy of the uzi token pattern that PRD #1907 consolidates, so they are redacted like `uzc_` / `uza_` / `uzp_`.

### D5. Clients are confidential and admin-registered, with an allowed scope list

M1 adds to PRD #1907's product record:

- one or more redirect URIs, exact match. `https` required; `http` only for loopback **IP literals** (`http://127.0.0.1:<port>/...`, `http://[::1]:<port>/...`), never `localhost`, so local development works without trusting name resolution;
- a client secret (random, shown once, sha256 at rest; rotation replaces it immediately). The token endpoint authenticates the client with HTTP Basic (`client_secret_basic`). Products are server-side apps; a product that cannot keep a secret is out of scope (D1);
- **an allowed scope list** (a subset of `jobs:run`, `jobs:read`), set by the admin. Authorize rejects any request for a scope outside it. PRD #1907 does not carry a per-product scope list, so B2 owns this column.

### D6. Grants are revocable by the user and the admin, and survive password changes

A grant = (user, product, scopes). The user sees **Settings → Access → Connected products** (product, scopes, connected at, last used) and can revoke one; an admin can list grants per product and revoke one or all, consistent with PRD #1907's rule that admins may revoke individual product tokens (product credentials, not personal CLI credentials; PRD #64's no-admin-revoke rule for CLI tokens is unchanged). Revoking a grant kills every access and refresh token in it at once.

- **The existing "Revoke all"** (`/api/me/cli-tokens/revoke-all` and the web button, extended by PRD #1907 to product tokens) also revokes every OAuth grant and its refresh tokens, in the same transaction. The lost-laptop panic button therefore leaves nothing live.
- **Revoking a single OAuth-issued access token** from PRD #1907's manual token list revokes its whole grant (otherwise the product would simply refresh). B1's list marks such tokens as belonging to a connected product.
- A deactivated user's tokens stop working through the existing `is_active` check. Grants, like CLI and product tokens, are not tied to the session `token_version`: a background product must keep working across logouts and password changes, so revocation is explicit.
- `POST /api/oauth/revoke` (RFC 7009) lets a product disconnect itself.

### D7. Public origin only, no new ingress

`/api/oauth/authorize`, `/api/oauth/token` and `/api/oauth/revoke` are served on `FRONTEND_ORIGIN` through the existing `/api/` proxy. Authorize uses `authLimiter` (D2); the token and revoke endpoints get their own limiter (per IP and per client), sized like the CLI poll limiter decision in `routes_auth.go`. The `iss` value returned with codes and errors is the instance's `FRONTEND_ORIGIN`, so a product that serves several uzi instances can detect a mix-up (RFC 9207).

## Out of scope

- Public clients (browser-only or mobile products), the implicit and password grants, device flow for products.
- OpenID Connect provider features: ID tokens, userinfo, discovery documents (products are configured by hand from the admin page).
- Dynamic client registration; self-service product registration by non-admins.
- JWT or self-contained access tokens; token introspection for third parties.
- Any change to how pasted PRD #1907 tokens work: they keep working beside OAuth-issued ones.
- Anything product-specific. Products are generic registered clients.

## Milestones

Every milestone: `task gate:api` (plus `task gate:web` where the SPA changes), and the new live-DB tests run with `UZI_TEST_DATABASE_URL` set, proving `RUN > 0`, zero `SKIP`, named tests `--- PASS` (`.claude/rules/go.md`, *Live-DB tests*). Test fixtures that look like tokens are assembled at runtime (`.claude/rules/prds.md`). No file under `.github/workflows/` is created or changed. Migration numbers are drafts, renumbered at merge.

- [ ] **M1. Client registration fields.** Draft migration adds redirect URIs, a client-secret hash and an allowed scope list to the PRD #1907 product record, a nullable `grant_id` to `product_tokens`, and the grants table. Admin API (and admin UI) to set redirect URIs and scopes and rotate the secret (shown once). Validation: exact-URI format, `https` except loopback IP literals. Live-DB tests.
- [ ] **M2. Authorize endpoint.** `GET /api/oauth/authorize` behind `authLimiter`, with the D2 input caps, validation order, binding cookie, the pending-request table (5 min TTL, swept like `cli_auth_requests`), and the redirect to `/connect?request=<id>`. A table-driven test covers every reject path and every cap, and asserts that an unknown client or a mismatched redirect URI never produces a `Location` header and never echoes a parameter.
- [ ] **M3. Consent page and approve/deny.** `GET` request metadata and `POST` approve/deny under `RequireAuth` + CSRF + binding-cookie match; single-use pending→approved claim; code issuance (single use, 60 s, hashed); server-built redirect URL with `state` and `iss`; re-consent replaces scopes and revokes earlier tokens. SPA `/connect` page reusing the `CliAuth.tsx` patterns, plain-text rendering of product-supplied strings, and the `sessionStorage` `next` return path validated by `safeNextPath` for both password and OIDC login. `task gate:web`.
- [ ] **M4. Token endpoint: code exchange.** `POST /api/oauth/token` `grant_type=authorization_code`: client auth, code lookup and single-use claim in one transaction, redirect-URI match, PKCE S256 verify with the RFC 7636 verifier format check, mint a PRD #1907 access token with `grant_id` plus a refresh token, `Cache-Control: no-store`. Code replay revokes. Rate limiter. Live-DB tests for each failure.
- [ ] **M5. Refresh and revoke.** `grant_type=refresh_token` (client-authenticated, no rotation per D4), idle and absolute expiry, `POST /api/oauth/revoke`. Live-DB tests, including a concurrent double-refresh (both succeed with the same refresh token) and refresh with another client's credentials (refused).
- [ ] **M6. Revocation surfaces.** Settings → Access "Connected products" (list, revoke); admin grant list and revoke per product; the existing "Revoke all" also revokes grants; revoking an OAuth-issued token from B1's manual list revokes its grant. Matching CLI verbs, per the repo rule that new API surfaces get a CLI check (`api/cmd/uzi/`, `docs/cli.md`). Tests for both "Revoke all" entry points.
- [ ] **M7. Security verification.** One named test per rule this PRD relies on: exact redirect match; no redirect and no echo on bad client or URI; authorize rate limit and input caps; `plain` PKCE rejected; missing PKCE rejected; verifier format enforced; code bound to client and URI; code single use and replay revokes; refresh token refused as a bearer everywhere; refresh requires the issuing client's credentials; OAuth access token subject to the same route lock and admin-flag clearing as a pasted product token; CSRF required on approve and deny; `state` echoed unchanged; `iss` present; `no-store` on token and approve responses; loopback `localhost` redirect rejected; re-consent revokes earlier tokens; "Revoke all" leaves no live grant; a code issued before a revoke or re-consent cannot be redeemed after it (race test). Two named attack tests: (a) **consent fixation**: a `/connect?request=<id>` created in the attacker's browser cannot be viewed or approved from the victim's browser (binding cookie mismatch); (b) **session swap**: a pending request approved once cannot be approved again, and its code cannot be redeemed with a different client or PKCE verifier. New prefixes are covered by the secret-scrub test. `auditor` review of the package.
- [ ] **M8. Docs and ADR.** User doc section "Connected products", operator doc for registering a product client (redirect URIs, scopes, secret handling), `docs/auth-design.md` section on the authorization server; an ADR recording D1's narrow-surface invariant and the no-rotation choice for confidential clients (adr/NNNN-oauth-authorization-server.md, numbered by this PRD). `task docs:sync` and `task check-docs:web`.
- [ ] **M9. Live acceptance on hosted k8s (maintainer-owned, part of completion).** A throwaway confidential test client completes connect, job call, refresh, revoke against a deployed instance behind the real ingress, with both a password user and an OIDC user (including an `sso-only` configuration). Confirms the Strict-cookie handoff and the Lax binding cookie (D2) in real browsers. The PRD is not done until M9 passes.

## Dependency plan

B2 is phase 3: it needs PRD #1907 merged, and its live acceptance needs PRD #1908 deployed.

| Milestone | Needs |
|---|---|
| M1 | PRD #1907 merged (product record, `product_tokens`, `RequireV1Caller`) |
| M2, M3 | M1 |
| M4 | M3 |
| M5 | M4 |
| M6 | M5 |
| M7 | M2 to M6 |
| M8 | M7 |
| M9 | M8 released, and PRD #1908 deployed (a real `/api/v1/jobs` call) |

## Success criteria

- A registered product completes connect, token exchange and a `/api/v1/jobs` call without the user copying any token.
- Only the browser that started a connect can approve it.
- An OAuth-issued access token is refused on every route a pasted product token is refused on (same enforcement, proven by M7).
- A refresh token is never accepted as a bearer, and is useless without the issuing client's secret.
- An unknown client or a mismatched redirect URI never redirects anywhere.
- The user can see and revoke each connected product; "Revoke all" leaves nothing live; revocation takes effect on the next request.
- Pasted PRD #1907 tokens keep working unchanged.
- M9 passes on a hosted cluster.

## Risks

| Risk | Mitigation |
|---|---|
| Hand-rolled OAuth gets a rule wrong | Narrow surface (D1), one named test per rule (M7), auditor review, ADR fixing the surface |
| Consent fixation or session swap through a shared `/connect` link | Browser-binding cookie checked on metadata and approve (D2); single-use approve claim (D3); attack tests in M7 |
| Open redirect through `redirect_uri`, the SPA sink or the login return path | Exact match before any redirect (D2); SPA navigates only to the server-built URL (D3); `next` kept client-side and validated by `safeNextPath`, no server-side return path (D2) |
| Unauthenticated authorize floods the pending table | `authLimiter`, input caps, 5 min TTL sweep (D2) |
| Strict session cookie breaks the flow in some browser | Handoff through an unauthenticated authorize endpoint to a same-site SPA page (D2); verified live in M9 |
| Leaked refresh token | Bound to client authentication (needs the client secret too), idle and absolute expiry, user and admin revoke, "Revoke all" |
| Consent phishing (a lookalike product name) | Only admins register products; the consent page shows the redirect host beside the name |
| Token endpoint brute force | Own rate limiter; codes and secrets are 256-bit random |

## Open questions

- Resolved 2026-09-29: lifetimes are 1 h access, refresh 30 d idle and 90 d absolute. Access-token expiry never cancels an already authorized job (PRD #1908).

## Decision Log

- 2026-09-29: PRD drafted. Chose a minimal in-tree authorization server over a general OAuth framework (D1); consent always shown (D3); access tokens reuse PRD #1907's enforcement (D4).
- 2026-09-29: Revised after buddy and security review. Resolved storage: access tokens are `product_tokens` rows with a new `grant_id` (B1 chose its own table). Added a browser-binding `Lax` cookie and a single-use approve claim against consent fixation and session swap (D2, D3); `authLimiter` and input caps on authorize (D2); the OIDC return path is client-side `sessionStorage` `next` via `safeNextPath`, keeping the server callback's no-return-path fix (D2). Re-consent replaces scopes and revokes earlier tokens; per-product allowed scopes owned by B2 (D3, D5). Dropped refresh rotation for confidential clients: client auth already binds refresh tokens, and rotation made retries and multi-replica products revoke grants (D4). Added `no-store`, `iss` (RFC 9207), RFC 7636 verifier checks, loopback IP literals only, and no parameter echo on the error page. "Revoke all" and admin revoke cover grants, consistent with B1 (D6). Consent text says jobs run on the user's model credential configured in uzi. Live acceptance (M9) is part of completion.
