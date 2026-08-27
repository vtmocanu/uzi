# PRD #45: OIDC SSO login — Keycloak / Pocket ID

**GitLab Issue**: [#45](https://github.com/vtmocanu/uzi/-/issues/45)
**Status**: Complete (2026-07-12, MR !44 merged). Keycloak verified against a real instance; Pocket ID walkthrough verification remains a manual user step (passkey bootstrap).
**Priority**: Medium
**Created**: 2026-07-12
**Depends on**: PRD #1 (auth), PRD #32 (user vault) — both done

## Problem

uzi only supports local email+password accounts. Teams already running an identity
provider (Keycloak at work, Pocket ID in homelabs) must hand out and manage a second
set of credentials, cannot enforce central auth policy (MFA/passkeys, offboarding),
and `docs/auth-design.md` explicitly punted: "No SSO/OAuth, no OTP … for this MVP."
This PRD supersedes that punt for OIDC (SAML stays out of scope).

## Solution Overview

1. Add a standard **OIDC Authorization Code + PKCE (S256)** login flow to the API:
   `GET /api/auth/oidc/login` (redirect to IdP) and `GET /api/auth/oidc/callback`
   (code exchange, ID-token verification, session issuance via the existing
   `issueSession` chokepoint).
2. **Single provider, env-configured** (`UZI_OIDC_*` vars, discovery via
   `/.well-known/openid-configuration`). Setting `UZI_OIDC_ISSUER_URL` enables the
   feature; restart to change.
3. **Coexists with password login**; a new `UZI_PASSWORD_LOGIN_ENABLED` kill-switch
   lets operators go SSO-only later (user decision, 2026-07-12).
4. **JIT provisioning**: first OIDC login auto-creates the user (subject to
   `UZI_ALLOWED_EMAIL_DOMAINS` and `UZI_REGISTRATION_ENABLED`); existing accounts
   are linked by verified email (user decision, 2026-07-12).
5. **Admin stays uzi-managed** (`is_admin` flag; first-user-is-admin rule reused).
   No groups-claim mapping in this iteration (user decision, 2026-07-12).
6. **Vault**: OIDC-created users have no password, so the PRD #32 KEK has nothing to
   derive from. They set a dedicated **vault passphrase** instead; the DEK hierarchy
   is unchanged.
7. **Docs**: operator guide with step-by-step **Keycloak** and **Pocket ID**
   walkthroughs (explicit user requirement), plus `configuration.md` and
   `auth-design.md` updates.

## Design Decisions

1. **Authorization Code + PKCE with a confidential client, no implicit/hybrid.**
   Library: `github.com/coreos/go-oidc/v3` — a **new direct dependency** (pin the
   version; it pulls in `go-jose`, which has a CVE history and no CI dep-scanner
   here, so version review is a manual gate — audit L7) — plus
   `golang.org/x/oauth2`, already an indirect dep at `api/go.mod:28`. PKCE is
   always on (S256); both Keycloak and Pocket ID support it. The client secret is
   env-only, never stored in the DB, so `secretbox` is not involved (same trust
   level as `JWT_SECRET`).

2. **The callback converges on the existing session path.** The callback calls
   `issueSession` (`api/internal/handler/auth.go:378`) exactly like password login:
   same HS256 JWT cookie, same CSRF cookie, same `token_version` revocation, same
   rolling refresh in `RequireAuth`. uzi does NOT keep IdP tokens: no refresh
   tokens stored, no IdP session tracking, no RP-initiated logout. Session lifetime
   is uzi's `AUTH_TOKEN_TTL`, decoupled from the IdP session (rejected alternative:
   mirroring IdP session lifetime — complexity with no threat-model gain for a
   loopback deployment).

3. **Flow CSRF via `state` + `nonce` + PKCE verifier in a short-lived sealed cookie.**
   The callback is a top-level cross-site GET redirect from the IdP, so uzi's
   `X-CSRF-Token` header scheme can't apply and the cookie must be `SameSite=Lax`
   (Strict cookies are dropped on cross-site navigation). `/api/auth/oidc/login`
   generates `{state, nonce, pkce_verifier}`, seals them with the master `secretbox`
   into a `uzi_oidc_state` cookie (HttpOnly, `Secure` per `CookieSecure`, 10 min
   TTL — bump if IdP MFA/passkey-enrollment flows prove longer; review Nit5). The callback validates the cookie **before** the code exchange (audit M1,
   2026-07-12): cookie absent / won't decrypt / `state` ≠ query `state` → generic
   `oidc_state` error with NO token-endpoint call (a missing cookie is a hard
   reject, never a skip — and exchanging first would let cookieless hits drive
   uzi→IdP amplification). Only then exchange the code and compare the ID-token
   `nonce` claim. The cookie is deleted on every callback outcome. The success
   redirect is a fixed server-side relative path (`/`) — no `next`/`return_to`
   param exists, closing open-redirect. Both endpoints sit behind the existing
   `authLimiter`.

4. **Identity key is `(issuer, subject)`, stored as columns on `users`.** Migration
   (landed as `00056`; renumber-at-merge convention still applies if main moves) adds `oidc_issuer TEXT`,
   `oidc_subject TEXT`, a partial unique index on `(oidc_issuer, oidc_subject)`, and
   relaxes `password_hash` to nullable. A separate `user_identities` table was
   rejected: uzi supports exactly one provider, and the table buys nothing until
   multi-provider is real (revisit then). `email` remains the human key; `sub` is
   the join key (emails can be reassigned in an IdP, subjects cannot).

5. **Linking and JIT require `email_verified=true`.** Matching an existing account
   by an unverified email is an account-takeover vector (attacker registers
   victim@example.com at a sloppy IdP). Both IdPs expose a per-user
   `email_verified` flag and **both default to unverified** — Pocket ID's per-user
   column defaults false (v2.2.0+; the app-level `emailsVerified` toggle or its
   email-verification flow flips it), Keycloak has the per-user "Email Verified"
   toggle. Both docs walkthroughs MUST cover enabling it, or logins die at
   `oidc_forbidden` out of the box (fact-check finding, 2026-07-12). No override
   env var (secure default, no footgun). Login order at callback:
   1. match `(issuer, subject)` → login;
   2. else match verified email → link **only if the row's `oidc_subject` is
      NULL** (`LinkUserOIDC` carries `WHERE oidc_subject IS NULL`; assert one row
      affected) → login. An email match on a row already bound to a *different*
      subject is rejected and logged, never overwritten — blind backfill would let
      a recycled/reassigned IdP email take over an existing uzi account (audit H1,
      2026-07-12). No auto-relink on subject change; IdP migrations are manual
      (out of scope);
   3. else JIT-create if `UZI_REGISTRATION_ENABLED` and domain allowed
      (`emailDomainAllowed`, reused from `auth.go:73`) — first-ever user becomes
      admin via a passwordless generalization of the advisory-locked first-admin
      path: `createUserFirstAdmin` (`auth.go:177`) hardcodes a non-null hash, so
      it is generalized (or gets a sibling) to insert via `CreateUserOIDC` with
      NULL hash while preserving the `pg_advisory_xact_lock` atomicity (review
      B2, 2026-07-12). The `email` claim is canonicalized exactly like Register
      (`mail.ParseAddress(lower(trim(...)))`, `auth.go:105-114`) before the
      allowlist check, the link lookup, and storage — otherwise link matching
      and UNIQUE diverge from password-registered emails (review N4). JIT must
      handle the concurrent-first-login race: a `23505`
      on the email or `(issuer,subject)` unique constraint means "already exists →
      re-fetch and login", same as Register's duplicate-email path (`auth.go:145`)
      (audit L6). On a fresh instance the docs MUST tell operators to pre-seed the
      admin (`UZI_SEED_EMAIL`) and/or set `UZI_ALLOWED_EMAIL_DOMAINS` *before*
      enabling OIDC — otherwise the first person org-wide to click the SSO button
      becomes admin (audit M2);
   4. else redirect to `/login?error=oidc_forbidden`.
   Note: `UZI_REGISTRATION_ENABLED=false` blocks JIT too (user-approved semantics);
   linking to an existing account still works, so SSO-only shops pre-create users
   or leave registration on. Deactivated users are rejected at step 1/2 — the
   callback replicates Login's `!IsActive` check (`auth.go:254`, `issueSession`
   doesn't do it) and redirects with its own enumerated code,
   `/login?error=oidc_deactivated` (review N5).

6. **Passwordless users get a vault passphrase, not a weaker vault.** The PRD #32
   KEK is `Argon2id(login password)` (`api/internal/vault/vault.go:11`); sealing
   OIDC users' secrets under the master key instead was rejected — it would
   silently downgrade exactly the users the SSO feature attracts. New endpoint
   `POST /api/vault/passphrase` (create-only, refuses when a vault row exists)
   creates the vault with a user-chosen passphrase (min length =
   `auth.MinPasswordLen`, 12 — a weak passphrase must not undercut the PRD #32 KEK;
   audit L1); the existing unlock banner and
   `/api/vault/unlock` (`UnlockExisting`) work unchanged since the vault never
   cared what the "password" string is. The OIDC callback **never** calls
   `vault.Unlock` — first-unlock *creates* a vault keyed by whatever string it is
   handed, so an accidental unlock with an empty/derived string would mint a wrong
   vault and permanently block the create-only passphrase endpoint (audit M4;
   tested: JIT login leaves no `user_vaults` row). The session payload's vault
   object grows an `exists` bit next to `unlocked` (`auth.go:346`) so the SPA can
   deterministically choose create-passphrase dialog vs unlock banner without
   probing for a 409 (review N1). The SPA prompts for a
   passphrase when a passwordless user first saves a secret (or hits the locked
   banner). Linked
   users (password + OIDC) keep their password-derived vault; OIDC login does NOT
   unlock it (no password in hand) — the banner already covers that case.
   Password-change rewrap paths are untouched; "change passphrase" is deferred
   (delete + re-create vault loses sealed secrets — acceptable, secrets are
   re-enterable tokens; documented).

7. **`password_hash = NULL` means password login always fails, constant-time.**
   Implementation trap (audit H2): nullable `password_hash` regenerates the sqlc
   field to `pgtype.Text`, and feeding an empty/invalid hash to `VerifyPassword`
   returns `ErrInvalidHash`, which `Login` maps to **500** — a 500-vs-401 oracle
   distinguishing OIDC-only accounts from wrong passwords. `Login` must branch on
   `!PasswordHash.Valid` FIRST, run the dummy-hash burn (`auth.go:235`), and
   return the identical 401. Explicit M5 test: known OIDC-only email + any
   password → 401 with wrong-password-like timing. The sqlc type change ripples
   to every `PasswordHash` site — enumerated (review N2): Register's `CreateUser`
   (`auth.go:200`), `seedAdmin` (`main.go:476`), `UpdatePasswordParams`, and the
   store tests passing `PasswordHash: "x"` (`slack_integration_test.go:49`,
   `:130`). Change-password
   requires the current password, so OIDC-only users can't set one (deferred;
   workaround: none needed, they have SSO).

8. **Boot validation, lockout guard, issuer scheme.** `UZI_OIDC_*` is all-or-nothing
   (issuer/client-id/client-secret must be set together, else refuse to start —
   matches the placeholder-secret refusal pattern in `config.go:190`).
   `UZI_PASSWORD_LOGIN_ENABLED=false` with OIDC unconfigured refuses to boot (total
   lockout); it also disables `POST /api/auth/register` (no point minting password
   accounts that can never log in; audit M3). Issuer must be `https://`, except
   loopback hosts for development (mirrors the `FORGE_ALLOWED_BASE_URLS` posture;
   the issuer host is implicitly trusted — its discovery doc dictates where the
   client secret is sent — noted in `specs/ai.md` and the operator doc, audit L3).
   OIDC discovery failure at boot logs loudly and leaves OIDC
   **configured-but-degraded** (login attempts retry discovery; `oidc_enabled`
   stays true so the button doesn't vanish on an IdP blip) rather than
   crash-looping the API. Known availability trade-off (audit M3): with password
   login disabled and the IdP down, nobody can log in — break-glass is the
   operator flipping `UZI_PASSWORD_LOGIN_ENABLED=true` and restarting (the seed
   admin keeps a `password_hash`); documented in `docs/oidc.md` troubleshooting.
   All outbound OIDC calls (discovery, JWKS, token) use an `http.Client` with an
   explicit timeout, mirroring the forge/Slack client posture (audit L3).

9. **SPA integration via `/api/auth/config`.** `AuthConfig` (`auth.go:363`) grows
   `oidc_enabled`, `oidc_provider_name`, `password_login_enabled`. Login page shows
   a "Sign in with {provider name}" button (`UZI_OIDC_PROVIDER_NAME`, default
   "SSO") as a full-page navigation to `/api/auth/oidc/login` (no fetch). With
   password login disabled the form is hidden entirely — but the SSO button is
   gated on *configured*, not on discovery having succeeded, so the lazy
   discovery-retry stays reachable from the UI when the IdP was down at boot
   (review B1); the degraded state is additionally surfaced on the admin settings
   page (review Nit6). Callback errors surface as `/login?error=<code>`
   (enumerated codes only — the SPA switches on known codes and never renders the
   raw value; details go to the server log through the existing scrubbing
   conventions, never the URL). `mockApi.authConfig` must return the new fields
   (`oidc_enabled:false`, `password_login_enabled:true`) so MOCK_MODE keeps
   rendering (review N6).

10. **Claims mapping is minimal**: `email` (required), `name` → `display_name`
    (JIT only, length-capped; not refreshed on later logins — uzi lets users edit
    their own display name and must not fight the IdP), `sub`/`iss` → identity.
    `email_verified` counts as verified only when it is boolean `true` — string
    `"true"`, absent, or anything else rejects (audit L2). Email drift
    at the IdP after linking is logged as a warning, not auto-applied (email is
    UNIQUE in uzi; auto-rename can collide — deferred).

## Technical Design

### API (api/)

- `api/internal/config/config.go`: `OIDCIssuerURL`, `OIDCClientID`,
  `OIDCClientSecret`, `OIDCScopes` (default `openid profile email`),
  `OIDCProviderName` (default `SSO`), `PasswordLoginEnabled` (default true) +
  validation per decision 8. Redirect URL derived as
  `FRONTEND_ORIGIN + /api/auth/oidc/callback`.
- New `api/internal/oidc` package wrapping `go-oidc`: provider discovery (lazy,
  cached), auth-URL builder, code exchange + ID-token verification. go-oidc checks
  issuer/aud/sig (JWKS); `nonce` and `email_verified` comparison is NOT automatic —
  the wrapper does it explicitly (go-oidc only exposes `IDToken.Nonce`).
- `api/internal/handler/oidc.go`: the two handlers + state-cookie seal/open;
  wired in `handler.go:200-211` outside `RequireAuth`, behind `authLimiter`.
- Migration `00056_oidc.sql` (landed number) + sqlc queries: `GetUserByOIDCSubject`,
  `LinkUserOIDC`, `CreateUserOIDC` (NULL password_hash); regenerate sqlc.
- `POST /api/vault/passphrase` (create-only, RequireAuth) per decision 6.
- `Login` handles NULL `password_hash` per decision 7; `AuthConfig` per decision 9.

### Web (web/)

- `web/src/lib/api.ts`: extend `AuthConfig` type; no new fetch for the redirect.
- `web/src/pages/Login.tsx`: SSO button, `?error=` banner, form hidden when
  `password_login_enabled=false`. `Register.tsx`: same gating hint.
- Vault UX: passphrase-create dialog for passwordless users (reuses the unlock
  banner surface in `AuthContext`/settings; wording switches on a
  `has_password:false` field added to `Me`/`sessionPayload`).

### Docs + specs

- New `docs/oidc.md` (`audience: operator`, no `order`): how the flow works, env
  var reference, **Keycloak walkthrough** (realm client, confidential + standard
  flow, redirect URI, per-user Email Verified toggle, PKCE), **Pocket ID
  walkthrough** (new OIDC client, callback URL, client secret, passkey note, and
  enabling email verification — per-user flag defaults false), fresh-instance
  admin pre-seeding (audit M2), IdP-outage break-glass, a note that uzi logout
  does not end the IdP session (immediate SSO re-login is expected; review Nit3),
  troubleshooting
  (discovery failure, unverified email, domain rejected). Split into
  `oidc-keycloak.md`/`oidc-pocketid.md` only if it outgrows one page.
- `docs/configuration.md`: `UZI_OIDC_*` + `UZI_PASSWORD_LOGIN_ENABLED` rows.
- `docs/auth-design.md`: replace the "No SSO/OAuth" line with a pointer here;
  document the vault-passphrase variant next to the PRD #32 material.
- `.env.example`, `docker-compose.yml` api env block.
- `specs/ai.md`: decision entries. `specs/human.md`: add the OIDC requirement
  (needs user approval per specs contract).

## Milestones

- [x] **M1 — Config + provider bootstrap**: `UZI_OIDC_*`/`UZI_PASSWORD_LOGIN_ENABLED`
      parsing, boot validation + lockout guard, `go get` of pinned
      `go-oidc/v3` (+ transitive `go-jose` review), `api/internal/oidc` package
      with discovery, env plumbing (`.env.example`, `docker-compose.yml`).
      `go build`, config unit tests.
- [x] **M2a — Schema + passwordless groundwork**: migration + sqlc regen, nullable
      `password_hash` ripple (all sites from decision 7), NULL-hash Login 401
      path, `AuthConfig` fields. This wide, risky change lands and is tested
      before any flow work (review N3).
- [x] **M2b — OIDC login flow (backend)**: the two handlers, state/nonce/PKCE
      cookie, verify → link/JIT → `issueSession`, register gating when password
      login is off. Validated with a `httptest` fake IdP.
- [x] **M3 — Vault passphrase for passwordless users**: `POST /api/vault/passphrase`
      (min length 12), `has_password` + `vault.exists` in session payload, SPA
      passphrase-create dialog + banner wording. Vault tests extended.
- [x] **M4 — Web login UX**: SSO button, error-code banner, password-form gating
      on Login/Register, `mockApi.authConfig` fields, admin-settings OIDC status
      line. `npm run typecheck` + vitest. (M3 and M4 both touch
      `web/src/lib/api.ts` — serialize those two edits despite being Phase 3;
      review Nit4.)
- [x] **M5 — Tests green**: Go tests covering the callback matrix (missing state
      cookie, state mismatch, bad nonce, unverified / non-boolean `email_verified`,
      domain rejected, JIT-first-admin, link, email-matches-different-subject
      rejection, concurrent-JIT 23505, deactivated, registration-disabled), the
      NULL-hash 401 oracle test, OIDC-login-leaves-no-vault-row, web tests for the
      gated login page. Full `go test ./...`, `npm test`.
- [x] **M6 — Docs + specs**: `docs/oidc.md` with Keycloak + Pocket ID walkthroughs,
      `configuration.md`, `auth-design.md`, specs updates; `npm run build`
      (check-docs) green.
- [x] **M7 — Live validation**: manual end-to-end against a real Pocket ID (or
      Keycloak dev realm) via docker compose with dummy-env isolation; findings
      folded back. (Done vs real Keycloak 26.0 — all success-criteria scenarios,
      incl. real `email_verified=false` refusal; dev-topology finding folded into
      `docs/oidc.md`. Pocket ID walkthrough verification remains a manual user
      step: its admin bootstrap needs a passkey, not headless-scriptable.)

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 (parallel) | M1, M2a | — | config.go, oidc pkg, .env.example, compose · migration, sqlc, auth.go |
| 2 | M2b | M1, M2a | handlers, cookie, handler.go |
| 3 (parallel) | M3, M4, M6 | M2b | vault/handlers+SPA · web pages · docs (M3/M4 share `web/src/lib/api.ts` — serialize) |
| 4 | M5, M7 | M2b–M4 | tests, live stack |

## Implementation notes (2026-07-12, folded back from review/audit/fact-check/live waves)

- **Login endpoint gated on `UZI_PASSWORD_LOGIN_ENABLED`** (`5f70685`): the PRD's
  Decision 8 wording assumed it ("accounts that can never log in", the lockout
  trade-off), but only Register was specified. Fact-check caught the gap: an
  ungated `POST /api/auth/login` in SSO-only mode bypasses IdP offboarding.
  Uniform 403 before body/DB; break-glass semantics unchanged.
- **Singleflight discovery** (`83a28e0`): concurrent logins during an IdP outage
  collapse onto one in-flight discovery instead of serializing on the provider
  mutex (auditor wave-1/2 note; reviewed + race-stress-tested, collapse test
  committed in M5).
- **M2b review batch** (`af6fc61`): server-side issued-at TTL inside the sealed
  state cookie; race-path log PII removed (subject only); deactivated check
  hoisted above `LinkUserOIDC` (reject-before-mutate); `issueSession` failure on
  the callback redirects `oidc_error` instead of raw JSON 500; empty `code` maps
  to `oidc_exchange`; Decision-10 email-drift Warn implemented (user id +
  subject, no addresses).
- **Vault passphrase guard** (`c1b6a0c`): endpoint also 409s accounts that HAVE
  a password (defense-in-depth vs self-brick; SPA never offers it to them).
- **go-jose bumped to v4.1.4** (`a7abfc5`) for GO-2026-4945 (the L7 manual gate
  working as intended, twice).
- **Mock scenario toggle** (`b35fd62`): `?mock=oidc|oidc-degraded|sso-only`
  makes the M3/M4 surfaces reachable in MOCK_MODE demo/QA builds (web-ux
  finding; N6 extension).
- **Dev-topology constraint** (M7, `ac900c1`): containerized api cannot reach a
  host-loopback IdP, and non-loopback `http` issuers are rejected by design —
  documented in `docs/oidc.md` with the two working dev topologies + a
  system-trust-store TLS note.
- **Replay posture** documented in `docs/auth-design.md`: stateless RP, safety
  rests on IdP single-use codes (RFC 6749); Keycloak + Pocket ID both enforce.

## Out of Scope

- SAML, LDAP, multiple simultaneous OIDC providers, admin-UI provider config.
- Groups/roles-claim → `is_admin` mapping (revisit if requested).
- RP-initiated logout / IdP back-channel logout; refresh-token storage.
- Auto-updating email/display_name from the IdP after first login.
- Vault passphrase change/recovery flows (documented limitation).
- E2E-harness IdP container in `./e2e/` (M7 covers live validation manually).

## Success Criteria

- With Keycloak or Pocket ID configured per the docs, "Sign in with SSO" logs a
  new user in end-to-end, creates them JIT with correct first-admin behavior, and
  a linked existing user keeps their data, password, and vault.
- Password login, worker auth, and all existing tests are unaffected when
  `UZI_OIDC_*` is unset (feature fully dormant).
- An OIDC-only user can store an Anthropic token behind a vault passphrase with
  PRD #32 guarantees intact (nothing user-secret sealed under the master key).
- No long-lived secret (client secret, ID/access tokens, PII beyond enumerated
  error codes) ever appears in URLs, logs, or API responses. The single-use
  authorization `code` in the callback query string is inherent to the flow and
  acceptable (review Nit2).
- `docs/oidc.md` walkthroughs verified against real Keycloak and Pocket ID
  instances (M7).
