# PRD #1907: Product tokens: register external products and let users mint route-locked, per-user tokens for a stable `/api/v1`

**Issue**: #1907
**Priority**: High
**Status**: In progress: M1-M6 implemented (2026-09-29), M7 (hosted k8s acceptance) open. Reviewed 2026-09-29: peer review plus a security review; findings folded in, see the Decision Log.
**Part of**: external-product integration, phase 1 (in parallel with PRD #1906's pre-integration milestones). PRD #1908 (phase 2) builds on it; PRD #1909 and PRD #1910 follow in phase 3.
**Builds on**: PRD #64 (CLI tokens, `RequireUser`, the `uzc_`/`uza_` scope ceiling), PRD #982 (api/SPA wire-contract fixtures), PRD #45 (OIDC login, account JIT provisioning).
**Related**: PRD #1908 (adds the first `/api/v1/jobs` endpoints, the per-product `allowed_job_types` allowance and the OpenAPI breaking-change gate), PRD #1910 (replaces copy-paste minting with an OAuth consent flow), PRD #1906 (egress site lists and the per-product site-list allowance).

## Problem

External products want to integrate with uzi **on behalf of their users**: a product sends work to uzi, uzi runs it as that user (on that user's own worker and model credential configured in uzi), and the product reads the result back. Today uzi has no safe credential for that.

The only Bearer credentials a third party could hold are CLI tokens (PRD #64):

- `uzc_` (scope `user`) carries **the owner's full authority** on every route mounted under `RequireUser`: runs, schedules, secrets metadata, memory, workers, repos, chat. Handing one to another application hands it the whole account.
- `uza_` (scope `admin_ro`) is worse: read access across the whole instance.

The scope column is a ceiling on **admin** authority only. `middleware.RequireUser` (`api/internal/middleware/cli_auth.go:39`) looks the token up in `cli_tokens`, loads the owner, and for any non-`admin_ro` token clears `IsAdmin` on a copy of the user row (`cli_auth.go:84-86`). Nothing else narrows what the token can do. So the obvious shortcut, adding a `product` value to the `cli_tokens.scope` CHECK (`api/internal/store/migrations/00067_cli_tokens.sql:39-40`), would still hand a product token every owner route in uzi: `RequireUser` would find the row, clear `IsAdmin`, and admit it, exactly as it admits a `uzc_`. `RequireUser` is mounted from nine route files (`handler.go`, `routes_admin.go`, `routes_auth.go`, `routes_chat.go`, `routes_judge.go`, `routes_me.go`, `routes_repos.go`, `routes_schedules.go`, `routes_workers.go`), so "remember to deny it everywhere" is not a control.

There is also no stable external API. Every route lives under one unversioned `/api/*` tree (`api/internal/handler/handler.go:842`) that the web SPA and the CLI both consume and that changes freely between releases. An external product built against it breaks on an ordinary refactor.

## Goal

1. An admin **registers** an external product. Unregistered applications cannot obtain a product credential.
2. A user **mints a product token** for a registered product in Settings, and pastes it into the product. The token acts **as that user**, so work it starts runs on the user's own worker and model credential.
3. The token works **only on `/api/v1/*`** and is refused on every other route, by construction rather than by a per-route check.
4. Every request is checked against the token's scopes and its product's state.
5. The user can revoke a token; an admin can revoke one compromised product token, or cut off a whole product at once.
6. `/api/v1` is a **stable, versioned contract**: an OpenAPI description, contract tests, additive-only changes, and a deprecation window. The internal `/api/*` stays free to change.

## Current state (verified on `main`, 2026-09-29)

- **CLI token storage.** `cli_tokens` (`00067_cli_tokens.sql:31-43`): sha256 at rest, `token_prefix` display stub, soft-delete `revoked`, `last_used_at`/`last_used_ip`, nullable `expires_at`, `scope IN ('user','admin_ro')`, `user_id ... ON DELETE CASCADE` (`:35`). The auth lookup `GetCLITokenByHash` (`api/internal/store/queries/cli_tokens.sql:1-9`) carries the NULL-expiry trap (`expires_at IS NULL OR expires_at > now()`). Only migration 00067 defines `cli_tokens`.
- **Token format.** `api/internal/clitoken/clitoken.go`: `ScopeUser`/`ScopeAdminRO` (`:34-35`), class prefixes `uzc_`/`uza_` (`:41-42`), `Prefixes` slice (`:49`); `FromAuthorizationHeader` (`:108`) does not check the prefix, and the prefix is documented as a label: authority comes from the row's `scope`.
- **Auth paths.** `RequireUser` dispatches on presence of a parseable `Bearer` header, never falling back on failure (`cli_auth.go:13-29`): Bearer goes to the `cli_tokens` lookup only (`:45-58`), otherwise the cookie path with CSRF. `RequireAuth` reads only the cookie (`auth.go:51`). `/api/ws` sits behind `RequireUser` (`handler.go:1018-1021`). `RequireWorker` resolves `workers.token_hash`; `RequireController` compares one fixed hash. Nothing else in `api/` reads the Authorization header. So a Bearer value unknown to `cli_tokens` fails closed on every existing route.
- **Admin-ness reaches handlers as a parameter.** Existing run code takes the flag from the context user, for example `GetRunForViewer(ctx, user.ID, user.IsAdmin, id)` (`api/internal/handler/runs_lifecycle.go:446`, `ws.go:88`). A context user with `IsAdmin` set makes reused handlers act as admin.
- **Minting.** `/api/me/cli-tokens` is cookie-only (`RequireAuth`), with list, mint, revoke and `POST /revoke-all`, the panic button (`api/internal/handler/routes_me.go:77-85`). Names are capped at 200 bytes (`cli_tokens.go:21`) and pass `termsafe.Validate` (`cli_tokens.go:116`). Webui-minted `uzc_` never expires; bounded tokens get 90 days (`cli_tokens.go:21-28`).
- **Admin visibility.** `GET /api/admin/cli-tokens` lists every CLI token with its owner, read-only **by decision**: there is no admin revoke of a user's personal CLI credential (`api/internal/handler/routes_admin.go:95-114`). It uses `authLimiter.PerUserMiddleware`.
- **Rate limiting.** `Limiter.PerUserMiddleware` (`api/internal/middleware/ratelimit.go:109`) keys on (route pattern, context user id) and must run after the middleware that sets the user.
- **Secret scrubbing: three copies.** The `uz[caw]_[A-Za-z0-9_-]{16,}` pattern lives in `secretscrub.go:45` (`uziTokenPattern`), `api/internal/issuedraft/issuedraft.go:495` and `api/internal/workersvc/ci_fix_snapshot.go:42`. `TestMintedPrefixesScrubbedOnBothPaths` (`secretscrub_test.go:52`) ranges over `clitoken.Prefixes` plus `jointoken.Prefix` only.
- **Token-literal gate.** `scripts/check-token-literals.sh:119` covers provider families (glpat, ghp, github_pat, sk-ant, xoxb); no uzi prefix, and gitleaks' default rules know none either.
- **Routers.** `Handler.Routes` (`handler.go:837`) and `Handler.WorkerRoutes` (`handler.go:1064`) build the two route tables.
- **Wire-contract tests.** PRD #982 pins DTO shapes with recorded fixtures under `fixtures/api-contract/` (`api/internal/apitypes/contract_test.go`, `api/internal/handler/contract_test.go`), Go half plus a vitest half. There is no OpenAPI description and no versioned prefix.
- **Web.** `web/src/components/CliTokens.tsx` renders Settings > Access; mock data in `web/src/mocks/data/cliTokens.ts`.
- **Docs.** `docs/cli.md` "Managing tokens" documents revoke-all as the lost-laptop control. Password change and logout do not revoke CLI tokens today.
- **Migration head** at drafting: `00268`.

## Decisions

- **D1. Separate table and a separate token class, not a new `cli_tokens.scope` value.** Product tokens live in a new `product_tokens` table with their own class prefix `uzp_`. Because `RequireUser` resolves Bearer tokens only against `cli_tokens`, a `uzp_` token is unknown to it and gets the existing `401 invalid CLI token` on every route it guards. Deny-by-default then holds **structurally**, for every current route and every route added later, with zero changes to the nine route files. Reusing `cli_tokens` would make isolation depend on a check every mount has to remember (the failure the Problem describes).
- **D2. `/api/v1` has its own authenticating middleware, `RequireV1Caller`, and nothing else authenticates there.** It is mounted once on the `/api/v1` subtree, owned by this PRD; PRD #1908 and later PRDs mount their endpoints under it and add no auth of their own. **Bearer only, no cookie** (no CSRF surface on the external API). It dispatches on the token's class prefix to pick the table, deterministically, never by trying one table and falling back to the other:
  - `uzp_` → `product_tokens` lookup only;
  - `uzc_` → the existing `cli_tokens` lookup, then the **row's `scope` must be `user`** (the prefix is only a label; authority comes from the row), so a user can call their own `/api/v1` from scripts and the uzi CLI;
  - anything else (`uza_`, a cookie, a malformed header, a `cli_tokens` row with scope `admin_ro`) → 401.
  The resolved caller goes into the context as a `V1Principal` (user, and for `uzp_` the product id, token id and scopes), and the user is also set via the existing user-context key so shared helpers and `PerUserMiddleware` work.
- **D3. `RequireV1Caller` always clears `IsAdmin`.** On its copy of the user row, for both token kinds, before anything else sees it. `/api/v1` never runs with admin authority, so a handler reused from the internal API (which takes `user.IsAdmin` as a parameter) cannot act as admin because an admin minted the token. Admin views of job runs stay on the existing non-v1 routes.
- **D4. Per-request checks, all live, all fail-closed.** For a `uzp_` caller, every request re-reads: token not revoked and not expired (same NULL-trap query shape as `GetCLITokenByHash`), product exists, is enabled and is not deleted, owning user active. A lookup error is a 401, never a pass.
- **D5. Scopes are generic and small.** `jobs:run` (start and cancel jobs) and `jobs:read` (read status and results). Stored as `text[]` on the token with a CHECK that each element is a known scope and the array is non-empty. A `RequireScope(s)` middleware gates each v1 route. A `uzc_` caller on `/api/v1` holds both scopes (it is the user acting directly).
- **D6. Product allowances belong to the PRDs that create what they allow.** B1's `products` row has no allowance columns. PRD #1908 adds `allowed_job_types` and enforces it at job creation (it also creates the job-type registry); PRD #1906 adds the per-product site-list allowance in its job-integration milestone. So there is never a window where a product token can use a capability without that allowance being checked.
- **D7. Tokens are bound to the minting user.** `product_tokens.user_id` is the user who minted it; the token can never act as anyone else. There is no "act as user X" parameter on `/api/v1`, ever. Any end-user label a product sends (a display name, an email) is **untrusted attribution for audit only** and is never used for an authorization decision (PRD #1908 stores it on the job). A `uzp_` token is a bearer credential: "bound to one product" is a label on the row, not proof of which application presents it; PRD #1910's client credentials address that.
- **D8. Revocation.**
  - **User:** revokes one token. The **existing** Revoke all (`POST /api/me/cli-tokens/revoke-all` and its web button) is extended to revoke the user's CLI tokens **and** product tokens in one transaction (and PRD #1910 extends it again to its grants), so the panic button users already know leaves nothing live.
  - **Admin, one token:** an admin can revoke an individual product token (cookie-only write). Product tokens are **product credentials**, not personal CLI credentials, so PRD #64's rule that an admin does not revoke a user's CLI token is unchanged and still holds for `cli_tokens`.
  - **Admin, whole product:** an admin disables a product, which kills every token for it on the next request (D4 re-reads the product).
  - Deactivating a user kills their product tokens (D4 checks `users.is_active`).
  - **Not revoking:** password change and logout do not kill `uzp_` tokens, exactly as they do not kill CLI tokens today. Only revoke, Revoke all, admin revoke, product disable, user deactivation or expiry do. The docs say so.
- **D9. Deleting a product is soft.** "Delete" sets `deleted_at` and disables the product; its tokens stop working but their rows, `last_used_at`, `last_used_ip` and revoked state stay for the audit trail. `product_tokens.product_id` is `ON DELETE RESTRICT`, so a hard delete cannot erase that trail. `products.created_by` is `ON DELETE SET NULL`, so deleting the admin who created a product is never blocked. `product_tokens.user_id` stays `ON DELETE CASCADE`, as for `cli_tokens`.
- **D10. Expiry: user-chosen, default bounded, "never" allowed.** Products integrate unattended, so a never-expiring token must be possible, but it is not the default. The mint form offers 30 days, 90 days (default), 1 year, or never. The list shows `last_used_at`, `last_used_ip` and expiry, as for CLI tokens. The server stores the chosen expiry; the client cannot set an arbitrary timestamp.
- **D11. Untrusted display strings are validated on write.** Product name and description (admin-written, shown to every user in the mint picker and in `uzi admin products` output) and product-token names (user-written, shown in the admin inventory beside the owner's email) all pass `termsafe.Validate` and byte caps: name 200 bytes (as `maxCLITokenNameBytes`), description 1000 bytes.
- **D12. `/api/v1` is the stable external contract; the internal `/api/*` is not.** Recorded as an ADR (adr/NNNN-product-api-v1-contract.md, number assigned at filing):
  - `/api/v1` is described by a checked-in OpenAPI 3.1 document, `api/openapi/v1.yaml`, owned by this PRD; later PRDs extend it.
  - Changes are **additive only**: new paths, new optional request fields, new response fields. Removing or renaming a path, field or enum value, or making an optional request field required, is breaking.
  - A breaking change needs a deprecation window of at least two minor releases and at least 90 days (whichever is later) with the old shape still served, announced in the changelog, or a new `/api/v2` prefix.
  - Every `/api/v1` DTO gets PRD #982-style recorded fixtures, and a route test asserts the spec and the chi router list the same `/api/v1` operations, in both directions.
  - The automated breaking-change gate arrives with the first real endpoints, in PRD #1908, as a pinned upstream tool behind a wrapper script (the `scripts/golangci-lint.sh` pattern). B1 does not write its own OpenAPI diff checker: with one endpoint it would cost more than it protects.
  - Only `RequireV1Caller` may resolve a `uzp_` token.
  - The internal `/api/*` remains unversioned and free to change; products must not call it.
- **D13. B1 ships exactly one v1 endpoint: `GET /api/v1/whoami`.** It returns the caller's user (id, display name), the product (id, name) when present, and the token's scopes. It proves the whole chain (mint, auth, principal, spec, fixtures) end to end before PRD #1908 adds jobs.
- **D14. Minting and product administration stay cookie-only**, following PRD #64: minting a credential is a browser action with CSRF, and admin writes are `RequireAuth` + `RequireAdmin`.
- **D15. Rate limiting is per user, and tokens are capped.** `/api/v1` uses `PerUserMiddleware` (it runs after `RequireV1Caller`, which sets the user), so minting more tokens buys no extra budget. A user may hold at most 10 active (not revoked, not expired) product tokens per product; minting an eleventh is a 409.
- **D16. The token-literal gate is not extended for `uzp_`.** `check:token-literals` exists for provider token families that GitHub Push Protection and gitleaks recognise; a `uzp_` literal is not rejected by either, exactly like `uzc_` and `uza_` today. Tests still assemble token-shaped strings at runtime by convention, and the scrubbers (M1) are what keep live tokens out of logs.

## Out of scope

- The OAuth consent flow ("Connect uzi"), refresh tokens and product client credentials: PRD #1910.
- Job endpoints, job types, `allowed_job_types`, the end-user audit label's storage, and the OpenAPI breaking-change gate: PRD #1908.
- The per-product site-list allowance and egress enforcement: PRD #1906.
- Files, uploads, artifact storage, product skill sets: PRD #1909.
- Per-product allowed repos.
- Any change to the internal `/api/*` routes' auth, `RequireUser`, or `cli_tokens`.
- CLI mint verbs (minting stays a browser action, D14).

## Milestones

Every milestone's gate: `task gate:api` (with `-race`, `-count=1`), plus `task gate:web` for M4 and M5. Token-shaped test strings are assembled at runtime (`"uz" + "p_" + strings.Repeat("a", 32)`), never written as one literal. New `*LiveDB` tests go in the `handler` or `store` packages, which the live-DB sweep already enumerates. No change under `.github/workflows/**`.

### M1: Data model, token class and scrubbing

**Status (2026-09-29): done.** Evidence: migration `00270_product_tokens.sql`; `TestProductTokenAuthLookupLiveDB`, `TestProductTokenAuthDeletedAtTermLiveDB` and `TestProductTokenSchemaConstraintsLiveDB` (NULL-expiry trap, revoked, expired, disabled, deleted, inactive user, each failing closed); `TestProductTokenLifecycleQueriesLiveDB`; `producttoken` package tests; `TestMintedPrefixesScrubbedOnBothPaths` ranges over `producttoken.Prefix` and all three scrub copies use `uz[capw]_`.

- Migration (draft number, renumbered at merge): `products` (`id`, `name` unique, `description`, `enabled`, `deleted_at`, `created_by` FK `ON DELETE SET NULL`, timestamps) and `product_tokens` (`id`, `user_id` FK `ON DELETE CASCADE`, `product_id` FK `ON DELETE RESTRICT`, `name`, `token_hash bytea UNIQUE`, `token_prefix`, `scopes text[]` with a known-scope CHECK and a non-empty CHECK, `revoked`, `created_at`, `last_used_at`, `last_used_ip`, `expires_at`).
- sqlc queries: the auth lookup (joins product and user, carries the NULL-expiry trap, filters `enabled`, `deleted_at IS NULL` and `is_active`), touch (≤1/min like `TouchCLIToken`), mint (with the D15 active-token count), list-own, revoke-own, admin revoke-one, admin list, and a revoke-all-own that the existing revoke-all handler calls in the same transaction as its CLI-token revoke.
- `producttoken` package: generate (`uzp_` prefix, 256-bit random body, sha256), parse, constant-time compare. Mirrors `clitoken`, and exports its prefix.
- **All three scrub copies** extended to `uz[capw]_` (`secretscrub.go:45`, `issuedraft.go:495`, `ci_fix_snapshot.go:42`), and `TestMintedPrefixesScrubbedOnBothPaths` extended to range over the `producttoken` prefix too, so a `uzp_` token is proven redacted on every path, including issue drafts and CI-fix snapshots.
- **Done when:** migration applies and rolls back on a live DB; store tests cover the NULL-expiry trap, revoked, expired, disabled product, deleted product, inactive user, each failing closed; the scrub test covers `uzp_` on all three paths.

### M2: `RequireV1Caller` and structural isolation

**Status (2026-09-29): done.** Evidence: `TestV1IsolationLiveDB` (route walk over both routers), `TestV1AdminStripLiveDB`, `TestV1CallerLiveDB`, `TestRequireV1CallerProductTokenPath`, `TestRequireV1CallerCLITokenScopeUser`, `TestRequireV1CallerRefusals`, `TestRequireScope`. Mutation runs at eeef4b1b (2026-09-29, live DB): (a) `RequireUser` taught to accept a `uzp_` token, then `TestV1IsolationLiveDB` went RED (a real `uzp_` got 403 where an unknown one got 401 on internal routes), restored GREEN; (b) the `IsAdmin` clear removed from `RequireV1Caller`: `TestV1AdminStripLiveDB` and `TestRequireV1CallerProductTokenPath` / `TestRequireV1CallerCLITokenScopeUser` RED ("IsAdmin survived RequireV1Caller"), restored GREEN.

- Middleware per D2/D3/D4, `V1Principal` in context, `RequireScope` per D5.
- **Isolation test (the load-bearing one):** a `*LiveDB` handler test mints a real `uzp_` token, walks **both** production routers (`Routes()` and `WorkerRoutes()`) with `chi.Walk`, and for every route outside `/api/v1` asserts that a request carrying the `uzp_` token gets **exactly the same response** (status and body) as the same request carrying an unknown random Bearer token of the same shape, and causes no handler side effect. This holds for public routes too (`/api/health`, `/api/version`, `/api/branding`, the login and `cli/*` auth routes answer both callers the same way), so no exemption list is needed. It iterates the live route tables, so a route added later is covered automatically. `/api/ws` is covered explicitly.
- **Admin-strip test:** an admin-minted `uzp_` token, and an admin's `uzc_` token, each yield a context user with `IsAdmin == false` under `/api/v1`.
- **Mutation discipline** (`.claude/rules/go.md` *Mutation testing*): (a) teach `RequireUser` to also accept `product_tokens` rows and watch the isolation test go red; (b) remove the `IsAdmin` clear from `RequireV1Caller` and watch the admin-strip test go red. Restore each from a `cp` backup and watch it go green. Record all four runs in the PR.
- Unit tests: prefix dispatch never falls back (`uzp_` with a cookie present is still the product path; unknown `uzp_` is 401 even with a valid cookie), `uza_` refused on `/api/v1`, a `cli_tokens` row with scope `admin_ro` presented under a `uzc_`-shaped value is refused, missing scope is 403.
- **Done when:** the isolation and admin-strip tests are green, and each is red under its mutation.

### M3: `/api/v1` mount, `whoami`, OpenAPI and fixtures

**Status (2026-09-29): done.** Evidence: `routes_v1.go`, `api/openapi/v1.yaml`, `TestV1OpenAPIRouteParity` (both directions) and `TestV1OpenAPISchemasMatchDTOs`, `TestV1WhoamiLiveDB`, `TestV1SubtreeRequiresV1CallerLiveDB`, `TestV1RateLimitPerUserLiveDB`, fixtures `fixtures/api-contract/v1_whoami.full.json` and `v1_whoami.zero.json` pinned by `apitypes/contract_test.go`.

- Mount `/api/v1` with `RequireV1Caller` then the D15 per-user limiter; `GET /api/v1/whoami` (D13).
- `api/openapi/v1.yaml` describing it; recorded fixtures for its DTO under `fixtures/api-contract/`.
- Route/spec parity test (D12), in both directions.
- **Done when:** removing `whoami` from the spec (or from the router) reddens the parity test, and the fixture test pins the DTO shape.

### M4: Product registry (admin)

**Status (2026-09-29): done.** Evidence: `TestAdminProductRegistryLiveDB`, `TestAdminProductRoutesAuthLiveDB`, `TestAdminProductActionsKillTokenOnNextWhoamiLiveDB` (disable, soft delete and admin revoke each 401 on the next `whoami`), `TestAdminDeleteDisabledProductStopsNothingLiveDB`, `TestAdminListProductTokensTruncatedLiveDB`; web `AdminProducts.test.tsx`. The admin Products page is the credential inventory (see the Decision Log).

- Admin API, cookie-only writes: create, list, update (description, enabled), soft delete (D9), and revoke one product token (D8). Name and description validated per D11.
- `GET /api/admin/products` also readable by `uza_` (read-only, like `/api/admin/cli-tokens`).
- The admin credential inventory (a sibling of `/api/admin/cli-tokens`, same limiter) lists product tokens with owner and product, metadata only, each with an admin revoke action in the web UI.
- Web: an admin **Products** page (list, create, enable/disable, delete with a confirm that says how many tokens it stops), and the product-token rows in the credential inventory.
- **Done when:** disabling a product, soft-deleting it, and admin-revoking one token each make that token 401 on the next `/api/v1/whoami` (live-DB tests), and a soft-deleted product's token rows are still listed.

### M5: User minting in Settings > Access

**Status (2026-09-29): done.** Evidence: `TestMintUseRevokeProductTokenLiveDB` (mint, `whoami`, revoke, 401), `TestRevokeAllKillsCLIAndProductTokensLiveDB`, `TestMintProductTokenCapLiveDB`, `TestMintProductTokenCapRaceLiveDB` (RED with the `LockProductTokenMint` call removed: two 201s and 11 active rows; GREEN restored), `TestMintProductTokenExpiredFreesCapLiveDB`, `TestMintProductTokenExpiryChoicesLiveDB`, `TestProductTokenRoutesRefuseBearerLiveDB`; web `ProductTokens.test.tsx` and the Revoke all cases in `CliTokens.test.tsx`.

- Cookie-only `/api/me/product-tokens`: list, mint (D10 expiry, D11 name validation, D15 cap), revoke one.
- The existing `POST /api/me/cli-tokens/revoke-all` and its web button revoke CLI and product tokens in one transaction (D8). Both entry points are tested: the endpoint directly, and the web button's call.
- Web: a **Product tokens** section beside CLI tokens: pick an enabled product, name, scopes (checkboxes), expiry; the token is shown once with a copy button; the list shows product, prefix, scopes, last used, last IP, expiry, revoke. Mock-mode data added.
- **Done when:** mint, use on `whoami`, revoke, and 401 afterwards pass end to end (live-DB handler test plus a web component test); Revoke all kills a live `uzp_` token.

### M6: Docs, spec and ADR

**Status (2026-09-29): done.** Evidence: `docs/product-tokens.md`, `docs/cli.md` "Managing tokens", the mirror refreshed by `task docs:sync`, `adr/1907-product-api-v1-contract.md`, `specs/human.md` entry. The optional items shipped: `uzi admin products` and the `uzp_` CLI error. `task check-docs:web` and `task gate:api` (incl. `TestEmbeddedDocsMatchSource`) passed on 2026-09-29.

- New `docs/product-tokens.md` (audience `user`, with an operator section for product registration): what a product token is, what it can and cannot reach, revocation (including that password change and logout do not revoke it), expiry, the Revoke all change. `docs/cli.md` "Managing tokens" updated. `task docs:sync` run and the mirror committed.
- The D12 ADR written.
- `specs/human.md` gains the requirement (tagged `(AI-synced YYYY-MM-DD)`).
- Optional, may slip to a follow-up: `uzi admin products` (read-only list over `uza_`), and a clear CLI error when `UZI_TOKEN` holds a `uzp_` token ("product tokens only work on /api/v1").
- **Done when:** `task check-docs:web` and `task gate:api` (`TestEmbeddedDocsMatchSource`) pass.

### M7: Hosted k8s acceptance (maintainer-owned)

**Status (2026-09-29): not done; maintainer-owned.** Nothing here has been verified on a hosted k8s deployment.

- On a hosted k8s deployment running this release: register a product, mint a `uzp_` token in the browser, call `GET /api/v1/whoami` through the public ingress, confirm the same token is refused on a sample of internal routes (a run list, `/api/ws`), revoke it, and confirm 401.
- **Done when:** the maintainer records the results on the issue. Part of completion, not an optional post-release step.

## Dependency plan

| Milestone | Depends on | Parallel with |
|---|---|---|
| M1 | none | none |
| M2 | M1 | M4 |
| M3 | M2 | M4, M5 |
| M4 | M1 | M2, M3 |
| M5 | M1, M2 | M3, M4 |
| M6 | M3, M4, M5 | none |
| M7 | M6, a release containing M1-M6 | none |

Across PRDs (phases):

| Phase | Work |
|---|---|
| 1 | this PRD, in parallel with PRD #1906's milestones before its job integration |
| 2 | PRD #1908, which depends on this PRD's M2, M3 and M5 |
| 3 | PRD #1906's job integration and live acceptance (after C1), PRD #1909 (after C1 and A), PRD #1910 (after this PRD; its live acceptance needs C1) |

This PRD does not depend on PRD #1906. PRD #1910 replaces M5's copy-paste with consent but keeps `product_tokens`, `RequireV1Caller` and the D8 revocation rules.

## Success criteria

- A `uzp_` token gets the same response as an unknown Bearer token on every route outside `/api/v1`, in both routers, proven by the route-walk test, which goes red when `RequireUser` is mutated to accept it.
- `/api/v1` never runs with admin authority, proven by the admin-strip test and its mutation.
- A `uzp_` token reaches `GET /api/v1/whoami` and sees only its own user and product.
- Revoking the token, Revoke all, admin revoke, disabling or soft-deleting the product, deactivating the user, or expiry each cut it off on the next request.
- `uzp_` tokens are scrubbed on all three scrub paths.
- Verified live on hosted k8s (M7).

## Risks

- **R1: A future route mounts its own Bearer auth that accepts `product_tokens`.** Mitigation: the route-walk test covers every route in both live tables; the ADR states that only `RequireV1Caller` may resolve `uzp_`.
- **R2: `/api/v1` accepting `uzc_` widens what a leaked user CLI token can do.** It adds nothing: a `uzc_` already reaches everything `/api/v1` offers (the user's own jobs), and `/api/v1` strips admin authority. Accepted.
- **R3: Never-expiring tokens.** Allowed for unattended use (D10), visible in the list with last-used and IP, killable by the user, by an admin and via product disable.
- **R4: A request already past the checks when a product is disabled completes.** Harmless in B1 (`whoami` only). Whether disabling a product cancels jobs already running is PRD #1908's decision.
- **R5: A new scrub site added later misses `uzp_`.** Mitigation: the prefix-binding test ranges over exported prefix constants, so it fails if any registered path stops redacting a registered prefix.

## Open questions

- **Q1 (resolved 2026-09-29).** Deprecation window: a deprecated `/api/v1` element keeps working for **at least two minor releases and at least 90 days**, whichever is later, because the release cadence makes a release count alone too short.
- **Q2 (resolved 2026-09-29).** Cap: 10 active tokens per user per product (expired and revoked tokens do not count).

## Decision Log

- 2026-09-29: PRD drafted. Separate `product_tokens` table and `uzp_` class chosen over a new `cli_tokens.scope` value, because `RequireUser` would otherwise admit product tokens on every owner route (D1).
- 2026-09-29, review round (peer review plus a security review):
  - `RequireV1Caller` always clears `IsAdmin` (D3), because reused run handlers take admin-ness as a parameter and an admin-minted token would otherwise read every user's runs. Tested and mutation-checked.
  - `/api/v1` accepts `uzc_` (row scope `user`, not the prefix) and `uzp_` only; `uza_` and cookies are refused. B1 owns `RequireV1Caller`, the spec and the parity test; PRD #1908 adds no auth of its own.
  - The isolation test compares a `uzp_` request with an unknown-Bearer request on every route of both routers, instead of asserting "never 2xx", which public routes would fail.
  - Scrubbing covers all three pattern copies, bound by the prefix test.
  - Product allowances move out of B1: `allowed_job_types` to PRD #1908, the site-list allowance to PRD #1906's job integration, so no capability is ever usable without its allowance check (D6, replacing the draft's D5 and D9).
  - Admins may revoke an individual product token (a product credential, so PRD #64's CLI-token rule is unchanged); product delete is soft with `ON DELETE RESTRICT` to keep the audit trail; `created_by` is `ON DELETE SET NULL` (D8, D9).
  - The existing Revoke all is extended, not a new endpoint (D8). Password change and logout do not revoke product tokens, as for CLI tokens.
  - Display strings validated with `termsafe` and byte caps (D11). Per-user rate limit plus a per-product active-token cap (D15), replacing the per-token bucket that more tokens could multiply.
  - The in-repo OpenAPI diff checker is dropped as over-engineering for one endpoint; PRD #1908 adds a pinned tool with its first real endpoints (D12). CLI parity becomes optional. `check:token-literals` is not extended for `uzp_` (D16).
  - Hosted k8s acceptance is part of completion (M7).
- 2026-09-29, implementation decisions:
  - `/api/v1` reuses the existing credential-surface per-user limiter (`authLimiter`, `RATE_LIMIT_MAX` per `RATE_LIMIT_WINDOW`, default 10 per minute) rather than a new one. Mounted with `r.Use` on the subtree, it keys one budget per user across all of `/api/v1`. Enough for `whoami`; PRD #1908, whose job polling needs more, is where a dedicated limiter belongs (`TestV1RateLimitPerUserLiveDB`).
  - No web credential inventory existed to extend, so the admin Products page lists product tokens per product, each with an admin revoke action.
  - The mint picker is `GET /api/me/product-tokens/products` (enabled, live products) rather than a separate `/api/products`, keeping every user-facing product-token route under one cookie-only mount.
  - The cap race is closed by a transaction-scoped `pg_advisory_xact_lock` keyed on (user_id, product_id), taken before the count, with count and insert in one transaction. `FOR UPDATE` cannot lock rows that do not exist yet, so two concurrent mints at 9 active tokens both passed the count. The lock class is `store.ProductTokenMintLockClass`, taken by the `LockProductTokenMint` query. Proven by `TestMintProductTokenCapRaceLiveDB` and its mutation (lock removed, test red).
  - Review finding: the token lists are bounded, with a `truncated` flag. `GET /api/me/product-tokens` returns at most 200 rows and the admin list 1000, active tokens first and then newest first, so the cut drops revoked and expired history before any active token; the web UI shows a notice when the flag is set.
  - Admin PATCH of a product is a single `COALESCE` update (NULL keeps the current value), with no read-merge, so concurrent PATCHes of different fields cannot overwrite each other (`TestUpdateProductConcurrentPatchesLiveDB`). A soft-deleted product cannot be changed or re-enabled (409).
  - Delete's `stopped_token_count` is 0 for an already-disabled product, because the disable had already refused its tokens (`TestAdminDeleteDisabledProductStopsNothingLiveDB`).
  - The optional M6 items shipped: the read-only `uzi admin products`, and a clear CLI error when `UZI_TOKEN` holds a `uzp_` token.
