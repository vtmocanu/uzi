# PRD #1: Simple WebUI with User Registration

**GitLab Issue**: [vtmocanu/uzi#1](https://github.com/vtmocanu/uzi/-/issues/1)
**Status**: Complete (2026-07-03, MR !1 merged)
**Priority**: High
**Created**: 2026-07-03

## Problem

uzi (the AI dark factory) has no user-facing surface and no identity model. Every future factory feature (job submission, monitoring, agent control) needs an authenticated web entry point. This PRD establishes the foundation: a minimal web shell with user registration, login, and session management, backed by PostgreSQL, runnable end-to-end on a laptop via docker-compose (per plan.md).

## Solution Overview

A Go API + React SPA web shell with simple email+password registration stored in the DB. **No SSO/OAuth, no email OTP** (user decision, 2026-07-03). Design cherry-picks the best of bottega's approach and improves on its weaknesses:

| Concern | bottega does | uzi will do |
|---|---|---|
| Auth flow | **Username**+password, bcrypt(12) | **Email**+password, **argon2id** (OWASP-preferred over bcrypt) |
| Registration | First-user-only `/register`, rest admin-created | **Open self-registration** + first-user-becomes-admin (bottega's in-tx re-check idea, made Postgres-safe via `pg_advisory_xact_lock` — see Auth design) |
| Session | JWT in **localStorage** (XSS risk), `token_version` revocation (bumped on logout only) | JWT in **HttpOnly cookie** **+ `token_version` revocation** — beats it on both axes |
| CSRF | N/A (bearer header) | HMAC-bound double-submit cookie (`HMAC(nonce, authToken)`) |
| Secrets at boot | Refuses to start on missing/placeholder `JWT_SECRET` | Refuse-to-start guard (bottega), no dev fallback |
| DB | SQLite, hand-rolled idempotent migrations in code | **Postgres 17 + standard migration tool (goose)** — avoids a bespoke migration runner |
| Rate limiting | express-rate-limit on register/login | In-process per-IP limiter on register/login (no Redis dep for MVP), real client IP taken from `X-Forwarded-For` set by our nginx (trust only that hop) |
| Local dev | None | docker-compose, loopback-bound |

## Technical Design

### Stack (user-confirmed)

- **Backend**: Go + chi + pgx/v5 + sqlc, `golang-jwt/v5`, argon2id via `golang.org/x/crypto/argon2`.
- **Frontend**: React 18 + Vite + TypeScript SPA (no SSR needed for this shell). Tailwind for styling.
- **DB**: PostgreSQL 17 (`postgres:17` image), named volume for persistence (plan.md requirement).
- **Migrations**: goose (standard tool; deliberate improvement over bottega's bespoke runner).
- **Compose**: `docker-compose.yml` with 3 services — `db`, `api`, `web` (nginx serving the built SPA, proxying `/api` to `api`). All ports bound to `127.0.0.1`.

### Auth design

Endpoints:

- `POST /api/auth/register` — email + password. Min length 12 **enforced server-side** (client adds zxcvbn-style strength feedback). argon2id hash (OWASP params: m=19 MiB, t=2, p=1, 16-byte salt, 32-byte key). First registered user gets `is_admin=true`; the check-and-insert runs inside a transaction holding `pg_advisory_xact_lock` (bottega's in-tx re-check alone is only safe on SQLite's single-writer model; Postgres READ COMMITTED would let two concurrent first-registrations both become admin).
- `POST /api/auth/login` — verify hash, issue JWT (HS256, `JWT_SECRET` from env, TTL default 7d via `AUTH_TOKEN_TTL`) in `HttpOnly; SameSite=Strict; Secure(when https)` cookie + readable CSRF cookie `HMAC-SHA256(nonce, jwt)`. Generic "invalid credentials" error (no unknown-email vs bad-password distinction).
- `POST /api/auth/logout` — bumps `token_version`, clears cookies.
- `GET /api/auth/me` — current user (safe serialization, never returns hashes).
- `GET /api/admin/users` + `PATCH /api/admin/users/:id` (activate/deactivate) — admin-only.

Session/revocation:

- JWT payload: `{user_id, token_version}`. Every authenticated request compares `token_version` against DB (one PK read; cache later if it matters) and rejects inactive users. Bump triggers: logout, password change, **admin deactivation** — any of these kills all of the user's sessions (real revocation, not just an HttpOnly cookie with no invalidation path).
- **Rolling refresh**: authenticated requests re-issue the cookie, sliding the expiry (bottega's rolling-token idea, done cookie-side) — active users never get logged out mid-session (unlike a fixed-length session that expires unconditionally), beating bottega's XSS-readable localStorage storage too.
- CSRF check on all state-changing methods when cookie-authenticated.
- Rate limit register/login per-IP (real IP from nginx's `X-Forwarded-For`, trusting only that hop); constant-time comparisons.
- Server refuses to start on missing/empty/placeholder `JWT_SECRET`.

Accepted trade-offs: open self-registration means duplicate-email 409 reveals registered emails (account enumeration) — accepted for an internal/local MVP; noted against OWASP ASVS L1.

Out of scope: SSO/OAuth, email OTP/magic links, email verification, password reset, 2FA (deferred; see plan.md "later stuff").

### Configuration (env)

| Var | Default (compose) | Notes |
|---|---|---|
| `JWT_SECRET` | — (required, boot guard) | `openssl rand -hex 64` |
| `DATABASE_URL` | set by compose from `POSTGRES_*` | pgx pool |
| `POSTGRES_USER/PASSWORD/DB` | `uzi`/generated/`uzi` | compose-internal |
| `AUTH_TOKEN_TTL` | `168h` (7d) | session TTL |
| `FRONTEND_ORIGIN` | `http://127.0.0.1:8080` | derives cookie `Secure` flag |
| `RATE_LIMIT_MAX/WINDOW` | `10`/`1m` | register+login |

Same-origin nginx proxy (SPA + `/api` on one origin) — no CORS configuration needed by design.

### Schema (initial)

```sql
users (
  id uuid PK default gen_random_uuid(),
  email text UNIQUE NOT NULL,
  password_hash text NOT NULL,
  display_name text,
  is_admin boolean NOT NULL DEFAULT false,
  is_active boolean NOT NULL DEFAULT true,
  token_version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_login timestamptz
)
```

### Pages (MVP shell, user-confirmed "minimal shell")

1. Landing (public) — name + tagline, links to login/register.
2. Register / Login forms.
3. Dashboard (protected) — shows logged-in user info; placeholder for factory UI.
4. Admin → Users (protected, admin-only) — list users, activate/deactivate.

## User Journey

1. `docker compose up` → stack starts, migrations auto-apply.
2. Open `http://127.0.0.1:8080` → landing page.
3. Register first account → becomes admin → redirected to dashboard.
4. Second user registers → regular user.
5. Admin sees both in Admin→Users; deactivating one blocks their login and kills their live sessions (token_version bump).
6. `docker compose down && up` → users and sessions' data survive (named volume).

## Milestones

- [x] **M1 — Repo scaffold + compose skeleton**: Go module, React/Vite app, docker-compose with Postgres 17 + named volume; `docker compose up` serves a hello page through nginx→api→db healthcheck chain.
- [x] **M2 — DB layer + migrations**: goose wired, `users` table migration, sqlc queries; migrations run on api startup.
- [x] **M3 — Auth API complete**: register/login/logout/me + admin user endpoints with argon2id, cookie JWT + CSRF + rolling refresh, token_version revocation (logout/password-change/deactivation), advisory-locked first-user-admin, rate limiting, JWT_SECRET boot guard. Verified with curl scenario script incl. a concurrent-first-registration race test.
- [x] **M4 — WebUI shell**: landing, register, login, dashboard, admin user-list pages wired to the API; auth context + protected routes.
- [x] **M5 — E2E validation + hardening pass**: scripted scenario (register→login→dashboard→admin→deactivate→revocation→persistence-across-restart); auditor pass on the auth surface; fix blockers.
- [x] **M6 — Docs**: terse README quick-start (compose up → register), docs/ pages for configuration + auth design decisions; ARCHITECTURE.md seeded.

## Success Criteria

- Fresh clone → `docker compose up` → registered + logged in within 2 minutes, no manual steps besides creating `.env` with `JWT_SECRET`.
- Passwords stored only as argon2id hashes; JWT never readable by JS; logout invalidates all of a user's sessions.
- Data survives `docker compose down/up`.
- Auth design demonstrably ≥ bottega: cookie storage (bottega uses XSS-readable localStorage), real revocation (not just a cookie with no invalidation path), standard migrations (not a bespoke runner).

## Risks

- **Scope creep toward a full agent-orchestration platform** — mitigation: milestones frozen to the shell; factory features are separate PRDs.
- **Security subtleties in hand-rolled auth** — mitigation: patterns copied from audited inspiration code, auditor milestone (M5), OWASP ASVS L1 as the checklist.
- **Compose portability (mac/linux)** — mitigation: loopback-bound ports, no host networking, named volumes only.

## Decision Log

- 2026-07-03 (user): no SSO/OAuth — simple registration in DB only.
- 2026-07-03 (user): password + revocation flow; Go API + React/Vite; minimal-shell scope.
- 2026-07-03 (AI): argon2id over bcrypt; goose over bespoke migrations; nginx as SPA server/proxy; 7d JWT TTL.
- 2026-07-03 (AI, post-review): `pg_advisory_xact_lock` for first-user-admin (bottega's in-tx re-check is SQLite-only safe); rolling cookie refresh + env TTL; token_version bump also on admin deactivation; server-side password validation; real-IP rate limiting behind nginx; env table added. Factual fixes: bottega is username+password (not email); bottega bumps token_version on logout only.
