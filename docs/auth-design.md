---
title: Auth design
audience: design
---

# Auth design

uzi's auth is a hand-rolled email+password flow, deliberately compared against bottega ([bottega](https://github.com/vdaubry/bottega) — vendored as a submodule under `inspiration/` when this was written, removed 2026-08-03) and designed to beat it. No OTP, no email verification for password accounts, for this MVP (see [plan.md](../plan.md) "later stuff" and the PRD's "Out of scope"). SSO is covered separately below (PRD #45).

## Comparison with bottega

| Concern | bottega does | uzi does |
|---|---|---|
| Auth flow | Username+password, bcrypt(12) | Email+password, argon2id (OWASP-preferred over bcrypt) |
| Registration | First-user-only `/register`, rest admin-created | Open self-registration + first-user-becomes-admin (bottega's in-tx re-check idea, made Postgres-safe via `pg_advisory_xact_lock`) |
| Session | JWT in localStorage (XSS risk), `token_version` revocation (bumped on logout only) | JWT in HttpOnly cookie + `token_version` revocation — beats it on both axes |
| CSRF | N/A (bearer header) | HMAC-bound double-submit cookie (`HMAC(nonce, authToken)`) |
| Secrets at boot | Refuses to start on missing/placeholder `JWT_SECRET` | Refuse-to-start guard (bottega), no dev fallback |
| DB | SQLite, hand-rolled idempotent migrations in code | Postgres 17 + goose (standard tool) — avoids a bespoke migration runner |
| Rate limiting | `express-rate-limit` on register/login | In-process per-IP limiter on register/login (no Redis dependency), real client IP taken from the `X-Forwarded-For` hop set by our own nginx |
| Local dev | None | docker-compose, loopback-bound |

## Password storage

`api/internal/auth/argon2.go` hashes with argon2id at the OWASP Password Storage Cheat Sheet parameters: 19 MiB memory, 2 iterations, 1 thread, 16-byte salt, 32-byte key, encoded as a standard PHC string (`$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`). Parameters are read back out of the stored hash on verify, so a future cost bump doesn't invalidate existing hashes. Password length is enforced server-side (12–1024 chars); the client adds strength feedback but the server floor is authoritative.

Login compares in constant time and, for an unknown email, still runs `VerifyPassword` against a fixed dummy hash before returning the generic "invalid credentials" error — so response timing doesn't reveal whether the email exists.

## Session: cookie JWT + revocation

On register/login, the API mints an HS256 JWT (`golang-jwt/v5`, `{user_id, token_version}` payload, TTL `AUTH_TOKEN_TTL`) and sets it as `uzi_auth`: `HttpOnly`, `SameSite=Strict`, `Secure` when `FRONTEND_ORIGIN` is `https://`. JavaScript can never read it.

Every authenticated request (`RequireAuth` middleware, `api/internal/middleware/auth.go`) re-validates the JWT signature/expiry, loads the user, and rejects if `is_active` is false or if the token's `token_version` no longer matches the DB row. Three actions bump `token_version` and so kill every session issued before them:

- **Logout** (`POST /api/auth/logout`) — bumps the caller's own version.
- **Admin deactivation** (`PATCH /api/admin/users/:id`) — bumps the target's version, so a deactivated user's live session dies immediately, not just their next login.
- **Password change** — the `UpdatePassword` query (`api/internal/store/queries/users.sql`) bumps `token_version` alongside the hash update, so a credential rotation revokes old sessions too. No HTTP endpoint calls it yet; changing your own password isn't in the M1–M6 shell scope, but the DB-level guarantee is already in place for when it's added.

This is real revocation: a compromised or logged-out session dies immediately, not just at its next natural expiry.

### Rolling refresh

Once a token is past the halfway point of its TTL, `RequireAuth` mints a fresh token at the current `token_version` and re-issues both cookies, sliding the expiry forward. An active user is never logged out mid-session; refreshing only past half-life, rather than on every request, bounds the window in which concurrent requests could momentarily see mismatched cookie pairs. If a revoking bump (logout, deactivation) happens later in the same request that refreshed, the newly minted token still carries the pre-bump version and is rejected on its next use, so revocation always wins.

## CSRF

Because the session lives in a cookie, the browser attaches it automatically to any request, cross-site or not — cookie presence alone can't prove the request came from the SPA. `api/internal/auth/cookie.go` pairs the auth cookie with a second, readable cookie `uzi_csrf` containing `hex(nonce) + "." + hex(HMAC-SHA256(nonce, jwt))`. The SPA reads that cookie and echoes it back in the `X-CSRF-Token` header on every state-changing request (`web/src/lib/api.ts`); `ValidateCSRF` recomputes the HMAC over the header's nonce using the *current* auth cookie's JWT and compares in constant time. Binding the token to the JWT (not just a random value) means a token stolen or planted from a sibling context is useless without the JWT itself. GET/HEAD/OPTIONS are exempt.

## First-user-admin

`POST /api/auth/register` (`api/internal/handler/auth.go`) counts existing users and inserts the new row inside one transaction that first takes `pg_advisory_xact_lock(RegistrationLockKey)` (`api/internal/store/migrate.go`, key `0x757A69`, i.e. "uzi"). Bottega's SQLite version gets away with a plain in-transaction count-then-insert because SQLite serializes writers; on Postgres, READ COMMITTED would let two concurrent first registrations both see zero users and both become admin. The advisory lock serializes the count-and-insert across concurrent callers, so exactly one registration ever wins admin — verified by `scripts/smoke.sh`'s 5-way concurrent registration race.

## Registration controls

Two operator-set knobs (PRD #5) gate who may register, enforced server-side in the register handler (`api/internal/handler/auth.go`) and surfaced to the SPA so the form can hide itself or hint the policy before submit:

- **`UZI_REGISTRATION_ENABLED`** (default `true`) — a kill-switch. When `false`, `POST /api/auth/register` returns **403** with a static message and the register route shows a "registration is disabled" notice; login is untouched. A malformed value refuses to start (a security switch fails loud, not silently open).
- **`UZI_ALLOWED_EMAIL_DOMAINS`** (default empty = allow all) — a case-insensitive, exact-match domain allowlist (no subdomain wildcards). A non-allowlisted address is rejected with **403** naming the allowed domains. The domain is taken from the *parsed* addr-spec (`mail.ParseAddress(...).Address`, split on its final `@`), never the raw input, so display-name/comment forms (`Alice <alice@x>`) and quoted local parts still yield the true domain; the stored email is canonicalized to that addr-spec.

Both policy rejections use **403** (the request is well-formed, the policy forbids it); `400` remains for malformed input and `409` for a duplicate email. Enforcement lives **only** in `Register` — never in a shared helper — so the operator-provisioned admin seed (`seedAdmin()` in `api/cmd/server/main.go`, a direct `CreateUser`) is exempt by construction: the operator sets both the seed email and the allowlist, so gating one on the other would only create bootstrap deadlocks.

A new unauthenticated endpoint **`GET /api/auth/config`** returns `{registration_enabled, allowed_email_domains}` — uzi's first unauthenticated JSON surface besides `/health`. It sits *outside* `RequireAuth`, behind the same auth rate limiter as register/login, and exposes only operator-set, user-visible policy (never user data or secrets — this endpoint's shape is a security boundary). The register page consumes it to disable itself or hint the allowed domains, with client-side pre-validation; the server stays authoritative.

## OIDC single sign-on

uzi can also authenticate against a single external OIDC provider (Keycloak or Pocket ID) instead of, or alongside, its own password accounts — see [oidc.md](oidc.md) for the operator setup guide and `prds/done/45-oidc-sso-login.md` for the full decision log. Highlights relevant to this page:

- The callback converges on the same `issueSession` chokepoint as password login: identical JWT cookie, identical CSRF cookie, identical `token_version` revocation, identical rolling refresh. uzi keeps no IdP tokens (no refresh tokens, no IdP session tracking, no RP-initiated logout), so the uzi session's lifetime (`AUTH_TOKEN_TTL`) is fully decoupled from the IdP's own session — logging out of uzi does not end the IdP session, and a live IdP session logs the user straight back in on the next click.
- Identity is keyed on `(issuer, subject)`, stored directly on `users`; email stays the human key and the join key for linking an existing password account or JIT-provisioning a new one, both gated on the IdP's `email_verified` claim being boolean `true` — an unverified email is an account-takeover vector, so there is no override.
- **Replay posture**: the callback keeps no server-side record of which authorization codes or `state` values it has already consumed, by design. Replay safety rests entirely on the IdP enforcing single-use authorization codes, as RFC 6749 requires — both Keycloak and Pocket ID do.
- **Vault**: an OIDC-only user has no password, so the PRD #32 vault KEK (`Argon2id(login password)`, see [vault-threat-model.md](vault-threat-model.md)) has nothing to derive from. Such a user instead sets a dedicated **vault passphrase** (same 12-character floor as a password) through a create-only endpoint; the DEK hierarchy underneath is unchanged, and a linked user (password + OIDC) keeps using their password-derived vault as before, unaffected by an OIDC login. Changing or recovering a lost vault passphrase is deferred — deleting and re-creating the vault would lose any already-sealed secrets — so this is a documented limitation, not an oversight.

## Rate limiting and the X-Forwarded-For trust model

`api/internal/middleware/ratelimit.go` is an in-process, per-`(route, client IP)` fixed-window limiter (`RATE_LIMIT_MAX` per `RATE_LIMIT_WINDOW`, default 10/minute) applied to `/api/auth/register` and `/api/auth/login`. No Redis dependency.

The "client IP" is read from `X-Forwarded-For`, but only when the direct TCP peer (`RemoteAddr`) falls inside `TRUSTED_PROXIES`. `web/nginx.conf` overwrites `X-Forwarded-For` with `$remote_addr` before proxying to `api` — so any header a browser or attacker sent is discarded at the edge, and the API only ever sees the value nginx itself set. A request that reaches `api` directly (bypassing nginx) has its `X-Forwarded-For` ignored entirely and falls back to `RemoteAddr`.

**`TRUSTED_PROXIES` is empty by default, and on compose that is not a compromise — it is the only safe setting.** The subtlety is that "a non-trusted source" above quietly assumed the attacker is *outside*. On compose the attacker to worry about is *inside*: the `agent` container runs a model against a user's cloned repo, which is semi-hostile by design (it is why `agent/src/guardrails.ts` exists), and it shares the compose network with `api`. Any CIDR broad enough to be written by hand — `172.16.0.0/12`, `10.0.0.0/8` — therefore covers the agent, making it a *trusted proxy*, at which point it can send a rotating `X-Forwarded-For`, land in a fresh rate-limit bucket per request, and defeat the brute-force control on the login endpoint outright. No IP-based rule can separate the two: they are on one network by design.

Emptying it costs nothing here, which is what makes this an easy call rather than a trade. `web` publishes on loopback only, so every browser connection arrives through Docker's userland proxy and nginx sees the **bridge gateway** — itself inside `172.16.0.0/12`. `ClientIP` returns the rightmost *non-trusted* entry, finds none, and falls back to `RemoteAddr` = the nginx container IP. That is precisely the key an empty set produces: the same bucket, by a shorter path (and the same shared bucket the next bullet already documents and accepts). The only address the trusted-CIDR filter did *not* swallow was an attacker's invented public IP — so its only working function was the exploit.

Set `TRUSTED_PROXIES` only for a reverse proxy you actually run, at *that proxy's address*. The Kubernetes deployment sets it explicitly per-cluster and never reads this default; the hosted-worker hop is scoped separately (see [configuration.md](configuration.md) §The optional TLS listener).

## Accepted limitations

- **Email enumeration on duplicate registration.** `POST /api/auth/register` returns `409` with "an account with this email already exists" for a taken email, which lets a caller confirm whether an address is registered. Accepted for an internal/local MVP (falls short of OWASP ASVS L1 on this point); login itself gives no such signal.
- **Shared rate-limit bucket in local compose.** Docker's port publishing NATs every host connection to `127.0.0.1:8080` through its userland proxy before it reaches nginx, so nginx (and therefore the API, via the trusted `X-Forwarded-For` hop) sees one source IP for all traffic on a given laptop, regardless of which browser tab or process originated it. In this topology the register/login rate limit is effectively shared across everything hitting the local stack. This is fail-safe (stricter than intended, never more permissive) and only affects the single-operator local demo; a real deployment behind a proper reverse proxy sees distinct client IPs per `TRUSTED_PROXIES` design.
- **No password reset.** There is no "forgot password" flow (no email sending in this MVP) — see [plan.md](../plan.md) "later stuff".
- **No email verification.** Registration accepts any syntactically valid address; nothing is sent to confirm ownership.
- **No 2FA.** Single factor (password) only.
