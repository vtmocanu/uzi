# Auth design

uzi's auth is a hand-rolled email+password flow, deliberately compared against the two inspiration projects (`inspiration/bottega`, `inspiration/multica`) and designed to beat both. No SSO/OAuth, no OTP, no email verification for this MVP (see [plan.md](../plan.md) "later stuff" and the PRD's "Out of scope").

## Comparison with the inspirations

| Concern | multica does | bottega does | uzi does |
|---|---|---|---|
| Auth flow | Passwordless OTP + Google OAuth | Username+password, bcrypt(12) | Email+password, argon2id (OWASP-preferred over bcrypt) |
| Registration | Auto-provision on first login, allowlist knobs | First-user-only `/register`, rest admin-created | Open self-registration + first-user-becomes-admin (bottega's in-tx re-check idea, made Postgres-safe via `pg_advisory_xact_lock`) |
| Session | JWT (HS256, 30d) in HttpOnly cookie, no revocation | JWT in localStorage (XSS risk), `token_version` revocation (bumped on logout only) | JWT in HttpOnly cookie (multica) + `token_version` revocation (bottega) — beats both |
| CSRF | HMAC-bound double-submit cookie (`HMAC(nonce, authToken)`) | N/A (bearer header) | Same HMAC-bound double-submit as multica |
| Secrets at boot | Dev-fallback JWT secret (footgun) | Refuses to start on missing/placeholder `JWT_SECRET` | Refuse-to-start guard (bottega), no dev fallback |
| DB | Postgres 17, bespoke migration runner (~130 numbered migrations) | SQLite, hand-rolled idempotent migrations in code | Postgres 17 + goose (standard tool) — beats both bespoke approaches |
| Rate limiting | Redis-backed, send/verify | `express-rate-limit` on register/login | In-process per-IP limiter on register/login (no Redis dependency), real client IP taken from the `X-Forwarded-For` hop set by our own nginx |
| Local dev | docker-compose, loopback-bound | None | docker-compose, loopback-bound (same pattern as multica) |

## Password storage

`api/internal/auth/argon2.go` hashes with argon2id at the OWASP Password Storage Cheat Sheet parameters: 19 MiB memory, 2 iterations, 1 thread, 16-byte salt, 32-byte key, encoded as a standard PHC string (`$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`). Parameters are read back out of the stored hash on verify, so a future cost bump doesn't invalidate existing hashes. Password length is enforced server-side (12–1024 chars); the client adds strength feedback but the server floor is authoritative.

Login compares in constant time and, for an unknown email, still runs `VerifyPassword` against a fixed dummy hash before returning the generic "invalid credentials" error — so response timing doesn't reveal whether the email exists.

## Session: cookie JWT + revocation

On register/login, the API mints an HS256 JWT (`golang-jwt/v5`, `{user_id, token_version}` payload, TTL `AUTH_TOKEN_TTL`) and sets it as `uzi_auth`: `HttpOnly`, `SameSite=Strict`, `Secure` when `FRONTEND_ORIGIN` is `https://`. JavaScript can never read it.

Every authenticated request (`RequireAuth` middleware, `api/internal/middleware/auth.go`) re-validates the JWT signature/expiry, loads the user, and rejects if `is_active` is false or if the token's `token_version` no longer matches the DB row. Three actions bump `token_version` and so kill every session issued before them:

- **Logout** (`POST /api/auth/logout`) — bumps the caller's own version.
- **Admin deactivation** (`PATCH /api/admin/users/:id`) — bumps the target's version, so a deactivated user's live session dies immediately, not just their next login.
- **Password change** — the `UpdatePassword` query (`api/internal/store/queries/users.sql`) bumps `token_version` alongside the hash update, so a credential rotation revokes old sessions too. No HTTP endpoint calls it yet; changing your own password isn't in the M1–M6 shell scope, but the DB-level guarantee is already in place for when it's added.

This is real revocation: multica has no way to kill a live session short of its 30-day expiry.

### Rolling refresh

Once a token is past the halfway point of its TTL, `RequireAuth` mints a fresh token at the current `token_version` and re-issues both cookies, sliding the expiry forward. An active user is never logged out mid-session (beating multica's hard 30-day expiry); refreshing only past half-life, rather than on every request, bounds the window in which concurrent requests could momentarily see mismatched cookie pairs. If a revoking bump (logout, deactivation) happens later in the same request that refreshed, the newly minted token still carries the pre-bump version and is rejected on its next use, so revocation always wins.

## CSRF

Because the session lives in a cookie, the browser attaches it automatically to any request, cross-site or not — cookie presence alone can't prove the request came from the SPA. `api/internal/auth/cookie.go` pairs the auth cookie with a second, readable cookie `uzi_csrf` containing `hex(nonce) + "." + hex(HMAC-SHA256(nonce, jwt))`. The SPA reads that cookie and echoes it back in the `X-CSRF-Token` header on every state-changing request (`web/src/lib/api.ts`); `ValidateCSRF` recomputes the HMAC over the header's nonce using the *current* auth cookie's JWT and compares in constant time. Binding the token to the JWT (not just a random value) means a token stolen or planted from a sibling context is useless without the JWT itself. GET/HEAD/OPTIONS are exempt.

## First-user-admin

`POST /api/auth/register` (`api/internal/handler/auth.go`) counts existing users and inserts the new row inside one transaction that first takes `pg_advisory_xact_lock(RegistrationLockKey)` (`api/internal/store/migrate.go`, key `0x757A69`, i.e. "uzi"). Bottega's SQLite version gets away with a plain in-transaction count-then-insert because SQLite serializes writers; on Postgres, READ COMMITTED would let two concurrent first registrations both see zero users and both become admin. The advisory lock serializes the count-and-insert across concurrent callers, so exactly one registration ever wins admin — verified by `scripts/smoke.sh`'s 5-way concurrent registration race.

## Rate limiting and the X-Forwarded-For trust model

`api/internal/middleware/ratelimit.go` is an in-process, per-`(route, client IP)` fixed-window limiter (`RATE_LIMIT_MAX` per `RATE_LIMIT_WINDOW`, default 10/minute) applied to `/api/auth/register` and `/api/auth/login`. No Redis dependency, unlike multica.

The "client IP" is read from `X-Forwarded-For`, but only when the direct TCP peer (`RemoteAddr`) falls inside `TRUSTED_PROXIES`. `web/nginx.conf` overwrites `X-Forwarded-For` with `$remote_addr` before proxying to `api` — so any header a browser or attacker sent is discarded at the edge, and the API only ever sees the value nginx itself set. A request that reaches `api` directly (bypassing nginx, e.g. from a non-trusted source) has its `X-Forwarded-For` ignored entirely and falls back to `RemoteAddr`.

## Accepted limitations

- **Email enumeration on duplicate registration.** `POST /api/auth/register` returns `409` with "an account with this email already exists" for a taken email, which lets a caller confirm whether an address is registered. Accepted for an internal/local MVP (falls short of OWASP ASVS L1 on this point); login itself gives no such signal.
- **Shared rate-limit bucket in local compose.** Docker's port publishing NATs every host connection to `127.0.0.1:8080` through its userland proxy before it reaches nginx, so nginx (and therefore the API, via the trusted `X-Forwarded-For` hop) sees one source IP for all traffic on a given laptop, regardless of which browser tab or process originated it. In this topology the register/login rate limit is effectively shared across everything hitting the local stack. This is fail-safe (stricter than intended, never more permissive) and only affects the single-operator local demo; a real deployment behind a proper reverse proxy sees distinct client IPs per `TRUSTED_PROXIES` design.
- **No password reset.** There is no "forgot password" flow (no email sending in this MVP) — see [plan.md](../plan.md) "later stuff".
- **No email verification.** Registration accepts any syntactically valid address; nothing is sent to confirm ownership.
- **No 2FA.** Single factor (password) only.
