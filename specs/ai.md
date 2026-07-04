# uzi — AI Design & Implementation Decisions

Decisions the AI made within the human constraints in [specs/human.md](./human.md).
Records the *why* and the human item each serves. This file is replaceable: a
rebuild may decide differently, but must still satisfy every item in human.md.

Behavior and constraints are authoritative here; specific file paths/library
versions are given as the current realization, not a requirement.

---

## 1. Stack

Serves human: "Stack: Go API + React/Vite SPA"; "PostgreSQL"; "local docker-compose".

- **Backend**: Go, `chi` router, `pgx/v5` (pgxpool) driver, `sqlc` for typed
  query code, `golang-jwt/v5`, `golang.org/x/crypto/argon2`.
  - Why: multica-proven combination; sqlc gives compile-checked SQL without an ORM.
- **Frontend**: React 18 + Vite + TypeScript SPA, Tailwind for styling.
  - Why: SPA is simpler than multica's Next.js — no SSR needed for this shell.
- **DB**: PostgreSQL 17 (`postgres:17`), named docker volume `pgdata` for persistence.
  - Why: serves the "persistent storage" requirement; survives `compose down/up`.
- **Migrations**: `goose` (standard tool).
  - Why: deliberate improvement over BOTH inspirations' bespoke runners
    (multica's ~130 numbered migrations / 176 up-files; bottega's hand-rolled
    idempotent in-code migrations).

Repo layout: `api/` (Go module, `cmd/server` + `internal/{config,store,auth,middleware,handler,httpx}`),
`web/` (Vite app + `nginx.conf` + `Dockerfile`), `docker-compose.yml`, `scripts/smoke.sh`.

## 2. Password hashing

Serves human: "user/password stored in DB"; best-practice bar.

- **argon2id**, OWASP Password Storage params: m=19456 KiB (19 MiB), t=2, p=1,
  16-byte salt, 32-byte key.
- Stored in **PHC format** `$argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>`;
  params are **parsed from the stored hash on verify**, so cost can be raised
  later without breaking existing hashes.
- Constant-time comparison (`crypto/subtle`).
- Why argon2id over bcrypt: bottega uses bcrypt(12); OWASP prefers argon2id.

## 3. Sessions / JWT

Serves human: "auth flow: password + revocation".

- **JWT HS256**, signed with `JWT_SECRET`. Payload: `{user_id, token_version}`
  plus registered claims (`sub`, `iat`, `exp`).
- Delivered in an **HttpOnly, SameSite=Strict** cookie (`uzi_auth`);
  `Secure` flag derived from `FRONTEND_ORIGIN` scheme (https ⇒ Secure).
  - Why cookie (not localStorage): beats bottega's localStorage (XSS-readable).
- **TTL** default 168h (7d) via `AUTH_TOKEN_TTL`.
- Parse pins the algorithm to HS256 (`WithValidMethods`) to block alg-confusion
  / `alg=none` forgery.
- **Boot guard**: server refuses to start if `JWT_SECRET` is missing, empty, a
  known placeholder, or shorter than 16 chars. No dev fallback (multica shipped
  a footgun dev fallback; bottega refused — we adopt bottega's stance).

## 4. Revocation (token_version)

Serves human: "revocation".

- `users.token_version` (int, default 1) is embedded in the JWT and compared
  against the DB **on every authenticated request**.
- Bump triggers (each kills ALL of the user's live sessions): **logout**,
  **password change** (future), **admin deactivation**.
  - Why: real revocation — beats multica (HttpOnly cookie but NO revocation).
- Every authenticated request also **re-reads the user from the DB** and rejects
  inactive accounts. `is_admin` is always read from the DB row, **never trusted
  from a claim**.

## 5. Rolling refresh (improvement over inspirations)

- The auth cookie is re-issued **only once the token is past half its TTL**
  (not on every request as bottega/multica-style rolling would).
  - Why half-TTL: keeps active users logged in while shrinking the window in
    which concurrent requests could observe a mismatched cookie pair.
- Both cookies (`uzi_auth` + `uzi_csrf`) are **always re-issued together** because
  the CSRF token is HMAC-bound to the JWT (see §6).

## 6. CSRF

Serves human: "auth flow" (state-changing safety).

- Readable (non-HttpOnly) cookie `uzi_csrf` = `hex(nonce).hex(HMAC-SHA256(nonce, jwt))`.
- SPA echoes it in the `X-CSRF-Token` header; server recomputes the HMAC over
  the auth cookie's JWT and compares (`hmac.Equal`) — HMAC-bound double-submit,
  not a plain cookie==header compare.
- Enforced only on **state-changing methods when cookie-authenticated**; safe
  methods (GET/HEAD/OPTIONS) always pass.
  - Why HMAC-bound: an attacker who plants a cookie on a sibling subdomain still
    can't forge a valid token without the JWT. Pattern from multica.

## 7. First-user-becomes-admin

Serves human: "admin user list" (needs a first admin) + "open self-registration".

- Open self-registration; the **first** account ever created gets `is_admin=true`.
- Check-and-insert runs in **one transaction holding `pg_advisory_xact_lock`**
  (fixed key) so two concurrent first-registrations cannot both become admin.
  - Why the lock: bottega's in-tx re-check is only race-safe on SQLite's single
    writer; Postgres READ COMMITTED would let both concurrent txns see zero users.
    The advisory xact lock serializes registrations on Postgres.

## 8. Auth endpoints

- `POST /api/auth/register` — email + password. Email lowercased+trimmed and
  validated (`net/mail`). Password min 12 chars enforced **server-side**
  (client adds strength feedback only), max 1024 (argon2 DoS guard). Logs the
  new user in (sets cookies). Returns 201.
- `POST /api/auth/login` — verify hash; generic "invalid credentials" for both
  unknown-email and wrong-password. **Timing equalized**: on unknown email,
  verify against a precomputed dummy hash so response time doesn't leak account
  existence. Deactivated accounts get 403. Updates `last_login`. Sets cookies.
- `POST /api/auth/logout` — bumps `token_version`, clears both cookies.
- `GET /api/auth/me` — current user (safe DTO; never returns hash/token_version).
- `GET /api/admin/users` — admin-only list.
- `PATCH /api/admin/users/{id}` — admin-only activate/deactivate. Deactivation
  bumps the target's `token_version` (kills live sessions). **Self-lockout guard**:
  an admin cannot deactivate their own account.
- `GET /api/health` — pings DB; used by the compose healthcheck chain.

Router: rate limiter applied **per-route** to register + login only;
`RequireAuth` group for logout/me; `RequireAuth`+`RequireAdmin` for `/admin/*`.

## 9. Delivery / container hardening

Serves human: "local docker-compose demo".

- **nginx** (`nginxinc/nginx-unprivileged`, uid 101, listens on 8080) serves the
  built SPA and proxies `/api` to the api service **same-origin** — no CORS by
  design. Restrictive CSP (`default-src 'self'`, no inline script/eval),
  `X-Content-Type-Options`, `X-Frame-Options DENY`, `Referrer-Policy no-referrer`.
  SPA fallback (`try_files … /index.html`).
- **api** image is `gcr.io/distroless/static-debian12:nonroot`; static CGO-off
  build; migrations embedded via `go:embed` so the runtime image needs only the
  binary. Container healthcheck is the binary probing itself (`/server -health`)
  because distroless has no shell/curl.
- **All base images are digest-pinned** (postgres, golang, node, nginx, distroless).
- **Only `web` is published**, at `127.0.0.1:8080`. `db` and `api` are
  unpublished — reachable only on the private compose network.
  - Why loopback binding: copies multica's `127.0.0.1` pattern; nothing exposed
    on the LAN.

## 10. Migrations at startup

Serves human: "docker compose up" one-command demo.

- Migrations run automatically at api startup, **before** serving, via a
  temporary `database/sql` DB opened with the pgx stdlib driver (`goose` needs
  `*sql.DB`); the app then uses a `pgxpool` for queries.
- Startup **waits/retries** for Postgres to be reachable (belt-and-suspenders
  alongside compose `depends_on: service_healthy`).

## 11. Rate limiting

Serves human: best-practice / abuse resistance.

- In-process **per-IP fixed-window** limiter (no Redis dependency for the MVP,
  unlike multica), keyed by `(route, client-IP)`, with a **background sweeper**
  evicting expired buckets. Defaults: 10 requests / 1m (`RATE_LIMIT_MAX`,
  `RATE_LIMIT_WINDOW`).
- Client IP honored from `X-Forwarded-For` **only when the direct peer
  (RemoteAddr) is within `TRUSTED_PROXIES` CIDRs**; otherwise a spoofed XFF is
  ignored. nginx **overwrites** XFF with `$remote_addr` (not
  `$proxy_add_x_forwarded_for`), discarding any client-supplied value. Empty
  `TRUSTED_PROXIES` ⇒ never trust XFF.

## 12. Users schema

```
users(
  id            uuid PK default gen_random_uuid(),
  email         text UNIQUE NOT NULL,   -- stored lowercased + trimmed
  password_hash text NOT NULL,
  display_name  text,
  is_admin      boolean NOT NULL default false,
  is_active     boolean NOT NULL default true,
  token_version int     NOT NULL default 1,
  created_at    timestamptz NOT NULL default now(),
  last_login    timestamptz
)
```

`pgcrypto` extension enabled for `gen_random_uuid()`.

## 13. Configuration (env)

| Var | Default | Notes |
|---|---|---|
| `JWT_SECRET` | — (required, boot guard) | HS256 key; `openssl rand -hex 64` |
| `DATABASE_URL` | set by compose | pgx pool DSN |
| `POSTGRES_USER/PASSWORD/DB` | `uzi`/required/`uzi` | compose-internal |
| `API_ADDR` | `:8080` | api listen addr (unpublished) |
| `AUTH_TOKEN_TTL` | `168h` | session TTL + cookie MaxAge |
| `FRONTEND_ORIGIN` | `http://127.0.0.1:8080` | scheme derives cookie `Secure` |
| `RATE_LIMIT_MAX` / `RATE_LIMIT_WINDOW` | `10` / `1m` | register+login |
| `TRUSTED_PROXIES` | compose: private CIDRs + loopback | XFF trust set |

## 14. Verification

- `scripts/smoke.sh`: scripted e2e scenario
  (register → login → dashboard → admin → deactivate → revocation →
  persistence-across-restart), incl. a concurrent-first-registration race test.

## 15. Known / accepted limitations (documented, not bugs)

- **Email enumeration** via duplicate-email 409 on register — accepted for an
  internal MVP (noted against OWASP ASVS L1). (Login itself is
  timing-equalized and generic, so login does not leak existence.)
- In the local compose NAT topology **all clients share one rate-limit bucket**
  (they arrive as the nginx hop / NAT address). Fail-safe; per-client
  granularity needs a front proxy that preserves real client IPs.
- No password reset / email verification / 2FA (deferred; see human.md).
- Secret scanner and CI are deferred. Minor-tag → digest pinning is done.
- **Rotating `UZI_SECRET_KEY` invalidates every stored secret** — forge bot
  PATs and per-user Anthropic tokens alike; no re-encrypt path in MVP, so each
  user reconnects/re-pastes. Accepted (see §17, §29).
- **Forgejo label-swap atomicity** is GitLab-only; single-column enforcement is
  best-effort on any future non-GitLab driver (see §16, §20).
- **Move-to-Closed is unsupported** from the board; closing/reopening stays on the
  forge (see §20).

---

# PRD #2 — Forge integration & label-synced kanban

Serves human Feature #2. Sections below extend the design for the forge layer;
all of it lives inside the existing `api` service (no new container).

## 16. Forge abstraction

Serves human: "forge-generic design, GitLab first, Forgejo later"; "uzi only sees
issues the bot has rights to".

- **`Forge` interface** in `api/internal/forge/` (`VerifyToken`, `ListProjects`,
  `ListLabels`, `EnsureLabels`, `ListIssues`, `UpdateIssueLabels`) over a neutral
  domain vocabulary (`BotIdentity`, `Project`, `Label`, `Issue`). `forge.New(Type,
  baseURL, token, timeout)` selects a driver by `Type` (maps 1:1 to
  `forge_connections.forge_type`); **no caller ever imports a driver**.
  - Why: neither inspiration abstracts the forge (multica hand-rolls GitHub-only
    `net/http`; bottega shells out to `gh`). A Forgejo driver later must not touch
    callers, schema shape, or UI flows.
- **GitLab driver** (`gitlab.go`): official client `gitlab.com/gitlab-org/api/client-go/v2`
  (successor to `xanzy/go-gitlab`). Membership discovery uses
  `min_access_level=Developer`; label moves use the **atomic** `add_labels`/
  `remove_labels` issue update. Base URL is per-connection (self-hosted-first).
- **Wrapper** on every call: `FORGE_HTTP_TIMEOUT`-bounded `*http.Client` (closes
  multica's untimeouted `http.DefaultClient` wart), 429/`Retry-After` +
  `RateLimit-*` handling, pagination, and **secret redaction** — a `redactor`
  (`redact.go`) scrubs the PAT and any `Authorization`/`PRIVATE-TOKEN` value from
  every returned error before it can reach a log or response (redaction unit test
  required and present).
- **Forgejo** deferred: interface, schema (`forge_type`), and UI copy stay
  forge-neutral. Forgejo has no atomic add+remove label call, so
  `UpdateIssueLabels` would be non-atomic and single-column enforcement
  best-effort there.

## 17. Bot PAT encryption at rest

Serves human: "each user connects their own bot PAT"; best-practice (secret at rest).

- **`api/internal/secretbox`**: generic AES-256-GCM box (12-byte random nonce
  prepended to ciphertext). Deliberately **not PAT-specific** — earmarked to also
  hold per-user Anthropic OAuth tokens in the agent PRD.
- Master key from **`UZI_SECRET_KEY`** (base64, exactly 32 bytes). `secretbox.LoadKey`
  runs at boot with a **refuse-to-start guard** identical in stance to `JWT_SECRET`:
  aborts if missing, not valid base64, not 32 bytes, or a **low-entropy placeholder**
  (e.g. all-zero).
- One `*secretbox.Box` built in `main.go`, handed to `forgesvc.Service`, which seals
  the PAT into `forge_connections.token_ciphertext` on connect and opens it on every
  forge call. **Never returned to the client** after save; re-connect to rotate a PAT.
- **Accepted limitation**: rotating `UZI_SECRET_KEY` invalidates all stored tokens —
  no re-encrypt path in MVP (see §15).

## 18. SSRF guard on base_url

Serves human: best-practice (server makes authenticated outbound calls to a
user-supplied base URL).

- **`FORGE_ALLOWED_BASE_URLS`** admin allowlist (comma-separated, default
  `https://gitlab.example.com`). `NormalizeForgeBaseURL` requires **https** and
  canonicalizes to `scheme://host[:port]` (strips path/query/fragment) before every
  comparison. Boot fails if the list is empty or any entry is malformed/non-https.
- The Settings → Forge base-URL dropdown offers **exactly this set** (`GET
  /api/forge/config`), so a free-text URL can't even be attempted — closes SSRF
  (cloud metadata, internal services, loopback) without per-request IP filtering.
  If free-text URLs are ever allowed, private/loopback/link-local ranges must be
  resolved and rejected at that point.

## 19. Forge schema (migration `00002_forge.sql`)

Serves human: "repo list + picker"; "board on labels"; "forge as source of truth".

- Goose versions for this PRD reserved `00002`–`00009` (PRD #3 starts `00010`) so
  parallel branches don't collide at `goose up`.
- **`forge_connections`** — one row per `(user_id, forge_type, base_url)`;
  encrypted PAT + verified bot identity (`bot_username`, `bot_forge_user_id`).
- **`repos`** — projects from the bot's membership list, keyed by the forge's
  **stable numeric project id** (not the renamable path). Upserted `enabled=false`
  on every listing call so enable/disable always has a row id to target.
- **`board_columns`** — ordered label names per repo. The implicit **Open**
  (no column label) and **Closed** (issue `state`) columns are never stored.
- **`issues`** — a **cache, never authoritative**. uzi's only owned board state is
  column config; every issue field is overwritten from the forge each sync.
  `has_prd_link` is stored as a bool computed at fetch time; the description itself
  is never persisted.
- All four tables cascade-delete from `forge_connections.user_id`, so every
  repo/board/issue row is scoped to one user's bot world.
  - Why keyed by forge id + explicit `(repo_id, forge_issue_iid)` FK: beats both
    inspirations' brittle text-convention issue↔work mapping and multica's
    FK-less JSONB repo registry.

## 20. Kanban semantics (GitLab-board compatible)

Serves human: "per-repo kanban board based on GitLab labels, two-way synced".

- **Columns = labels.** Per repo: implicit **Open** (no column label) + ordered
  label columns (default seeded `In Progress`, `Upcoming`, `Later` =
  `forgesvc.DefaultColumns`) + implicit **Closed**. A card is in a column iff the
  issue carries that label (GitLab board-list semantics).
- **Single column label enforced.** A move issues one atomic
  `UpdateIssueLabels(add=[target], remove=[other column labels])`. Move-to-**Open**
  removes all column labels. Move-to-**Closed** is **unsupported** (returns 400;
  closing/reopening stays on the forge — the card links out).
- **Conflict handling**: an issue arriving with multiple column labels renders in
  the highest-positioned column with a **conflict badge**; the next uzi-side move
  normalizes it.
- **Default columns seeded on the forge** on first board open (`EnsureLabels`) — a
  deliberate, documented side effect visible in GitLab's own label list. Columns
  are reconfigurable to any project label afterward. **Column count capped at 10**
  (`maxBoardColumns`).

## 21. PRD-label filter + PRD-link sanity check

Serves human: "board/agents work only PRD-labeled issues, sanity-checked to contain
a link to the PRD file".

- Board shows **only `PRD`-labeled issues** (`ListIssues(labels=["PRD"], …)`).
- **PRD-link check** computed at fetch time from the issue description: must contain
  a relative path or absolute URL to a PRD file. Regex:
  `(?i)(?:https?://\S+/-/blob/[^\s)]+/)?prds/[\w.-]+\.md(?:[#?][^\s)]*)?` — matches
  subdir paths (e.g. `prds/done/…`). Result stored as `issues.has_prd_link`
  (description not persisted). Failing cards render a warning badge and are
  **excluded from future agent pickup**.

## 22. Sync engine

Serves human: "kept in sync two-way between uzi and GitLab".

- **Forge is the source of truth**; `issues` is a cache. `api/internal/forgesvc.Service`
  is shared by handlers and the poller (`IncrementalSync`/`FullSync` against a
  narrow `IssueStore` interface, unit-tested with a fake store + mocked `Forge`).
- **Incremental pull** (`api/internal/poller.Engine`, one background goroutine):
  per enabled repo, `ListIssues(labels=PRD, state=all, updated_after=HWM)` each
  `FORGE_POLL_INTERVAL` (default 60s). **HWM = max `updated_at` returned by the
  forge**, never the client clock (skew would drop updates); GitLab's
  `updated_after` is inclusive at second granularity, so boundary rows re-fetch and
  dedupe by upsert.
- **Full reconcile** every `FORGE_RECONCILE_EVERY`-th tick (default 10) and always
  on a repo's first poll after enable: fetch the complete PRD set with no
  `updated_after`, upsert everything, and **evict cache rows the forge no longer
  returns**. Eviction is the only way to see de-labeling/close-with-label-removed/
  deletion, which an `updated_after` filter structurally cannot report. Manual
  `POST /api/repos/:id/sync` and the Refresh button run the same full sync.
- **Push is forge-first**: a move calls `UpdateIssueLabels` before touching the
  cache and updates the cache only on success — a failed forge write leaves the
  card put (**snap-back**), never an optimistic divergence. Conflicts resolve
  last-writer-wins at the forge (persisted-truth-over-event-claim, multica's lesson).
- **EMPIRICAL FINDING (verified 2026-07-03, E2E vs gitlab.example.com)**: GitLab
  throttles issue `updated_at` bumps to **~1 per ~60s window, independent of
  add-vs-remove**. Consequence: changes at normal human cadence (spaced past the
  throttle window) are caught incrementally within one poll; multiple changes inside
  one window share a single bump, so only the last is guaranteed incrementally and
  earlier ones wait for the reconcile tier. **Freshness contract**: normal-cadence
  edits within one poll interval; bunched-window edits + de-label/close/reopen/delete
  within one reconcile interval.
- **Webhooks deferred** (laptop compose is unreachable from the forge): the design
  keeps a `ChangeSource` seam (poller now; webhook receiver later, authenticated via
  GitLab's HMAC-SHA256 signing token preferred over legacy `X-Gitlab-Token`).
- **Graceful shutdown ordering**: `main.go` drains HTTP (`srv.Shutdown`), cancels the
  root context (stops the next tick), then `pollerWG.Wait()`s an in-flight sync — all
  before the deferred `pool.Close()`, so a mid-tick query never races the pool.

## 23. Rate limiting & poller bounding

Serves human: best-practice (protect the upstream forge; abuse resistance).

- **Per-user forge limiter** (`PerUserMiddleware` over the same fixed-window
  `mw.Limiter` PRD #1 uses, but keyed per user, not per IP) on the forge-proxying
  endpoints only: verify, projects, move, sync. Budget `FORGE_RATE_LIMIT_MAX`/
  `FORGE_RATE_LIMIT_WINDOW` (default 30/min), separate from the auth limiter.
- **Poller bounding**: per-tick **bounded concurrency of 4**
  (`defaultMaxConcurrency`, semaphore) + a **per-tick deadline** clamped to one poll
  interval, so a slow forge can't let ticks pile up. In-memory per-repo state
  (`hwm`, poll count) — a disabled repo drops out; a re-enabled one restarts with a
  fresh full reconcile.

## 24. Startup admin seed

Serves human: "seed an admin from env so the user survives DB wipes; admin role;
never overwrite an existing user".

- `UZI_SEED_EMAIL` / `UZI_SEED_PASSWORD` / `UZI_SEED_NAME`. Runs in `main.go` after
  migrations. Empty `UZI_SEED_EMAIL` disables seeding.
- **Create-only-if-absent**: if a user with that email exists, leave it untouched
  (idempotent, safe to leave set across restarts; never overwrites a password).
- Same **argon2id** params and **password-length policy** as registration (shared
  `auth.MinPasswordLen`/`MaxPasswordLen`, refactored to one place). Seeded user is
  `is_admin=true`.
- **Boot-refusal on invalid seed input** (bad email, or password outside 12–1024):
  a set-but-invalid seed is a loud misconfiguration, not a silent skip.
- **23505-tolerant**: a duplicate-key on insert is treated as "already seeded"
  (replica-safe against a concurrent create).

### Forge-connection seed (extends the admin seed)

- `UZI_SEED_FORGE_PAT` / `UZI_SEED_FORGE_BASE_URL` / `UZI_SEED_FORGE_REPOS`
  optionally seed a forge connection **belonging to the seed admin** and enable a
  set of repos, so the whole demo (admin + bot connection + tracked repos)
  survives a DB wipe from env alone. Lives in `api/internal/seed` (narrow `Store`
  + `ForgeService` interfaces for unit-testing against a mocked `Forge`, mirroring
  `forgesvc.IssueStore`); `main.go` calls it after the admin seed and before the
  poller starts. Empty `UZI_SEED_FORGE_PAT` disables it.
- **Reuses the connect flow's primitives** (`svc.ForgeForToken` → `VerifyToken` →
  `svc.EncryptToken` → `q.UpsertForgeConnection`, then `q.UpsertRepo` /
  `q.SetRepoEnabledForUser`) rather than duplicating the handler — same encryption
  path, same verified-identity capture.
- **Create-only, never re-verify**: if the seed admin already has a connection for
  `(gitlab, base_url)`, do nothing at all (no overwrite, no re-verify) — consistent
  with never-touch-existing-user.
- **Static config is boot-fatal** (`config.loadSeedForge`): `UZI_SEED_FORGE_PAT`
  set without `UZI_SEED_EMAIL`, or a base URL outside `FORGE_ALLOWED_BASE_URLS`,
  refuses to start. `UZI_SEED_FORGE_BASE_URL` defaults to the first allowlisted
  entry and is stored normalized (matches a connection's `base_url`).
- **Runtime forge failure is NON-fatal** (deliberately unlike the static guards): a
  network error / 401 at seed time logs and skips, and boot continues — the forge
  being down must not kill the stack; the seed retries next boot. Both forge calls
  (verify + list-projects) run **before any write**, so a mid-seed forge failure
  leaves no half-created connection the create-only guard would then strand.
- **Repo enable + warning**: every visible project is upserted as a repo
  (`enabled=false`, like the ListProjects handler); those whose
  `path_with_namespace` is in `UZI_SEED_FORGE_REPOS` are enabled. A requested repo
  the bot can't see is logged as a **warning** and skipped, not fatal.
- **PAT never logged** (the driver already redacts; the seed logs only the bot
  username / base URL / counts).

## 25. Forge configuration (env, extends §13)

| Var | Default | Notes |
|---|---|---|
| `UZI_SECRET_KEY` | — (required, boot guard) | base64 32B; encrypts bot PATs; rotation invalidates stored tokens |
| `FORGE_ALLOWED_BASE_URLS` | `https://gitlab.example.com` | comma-separated https allowlist (SSRF guard) |
| `FORGE_POLL_INTERVAL` | `60s` | per enabled repo |
| `FORGE_RECONCILE_EVERY` | `10` | full reconcile every Nth poll; poller clamps `<1` to 1 |
| `FORGE_HTTP_TIMEOUT` | `15s` | every forge call |
| `FORGE_RATE_LIMIT_MAX` / `FORGE_RATE_LIMIT_WINDOW` | `30` / `1m` | per-user, forge-proxying endpoints only |
| `UZI_SEED_EMAIL` | — (optional) | set to seed an admin at startup; disables seeding when empty |
| `UZI_SEED_PASSWORD` | — (required if seed email set) | 12–1024 chars or boot fails |
| `UZI_SEED_NAME` | — (optional) | display name for the seeded admin |
| `UZI_SEED_FORGE_PAT` | — (optional) | set to seed a forge connection (owned by the seed admin) at startup; requires `UZI_SEED_EMAIL` or boot fails |
| `UZI_SEED_FORGE_BASE_URL` | first `FORGE_ALLOWED_BASE_URLS` entry | forge base URL for the seeded connection; must be allowlisted |
| `UZI_SEED_FORGE_REPOS` | — (optional) | comma-separated `path_with_namespace` list to enable; unseen repos warn, not fatal |

Invalid numeric/duration forge vars fall back to defaults (same as §13); only
`UZI_SECRET_KEY`, a malformed `FORGE_ALLOWED_BASE_URLS`, and an invalid seed
(admin or forge: bad email/password, PAT without email, non-allowlisted forge
base URL) refuse to start. A forge outage during the forge-connection seed is
non-fatal (see §24).

## 26. API surface (forge; all authenticated, PRD #1 session/CSRF)

- `POST/GET/DELETE /api/forge/connections` (+ `POST .../verify`); `GET /api/forge/config`
  (allowlisted base URLs for the dropdown).
- `GET /api/forge/connections/:id/projects` — live membership; upserts `repos`
  (`enabled=false`).
- `PUT /api/repos/:id` (enable/disable) · `GET /api/repos` (enabled repos).
- `GET /api/repos/:id/board` · `PUT /api/repos/:id/board/columns` · `POST
  /api/repos/:id/issues/:iid/move {to_column}` · `POST /api/repos/:id/sync`.
- Every repo/board endpoint authorizes through the owning connection's `user_id`
  (bottega's membership-authz shape) — a user only ever sees their own bot's world.

## 27. Compose worktree isolation

Serves human: "local docker-compose MVP".

- **Dropped the pinned `name:` in `docker-compose.yml`** so Compose derives the
  project name from the directory. A hardcoded `name: uzi` made every git worktree
  share one set of containers and the `pgdata` volume; per-directory names give each
  worktree an isolated stack.

## 28. E2E test convention

Serves human: best-practice (real-forge verification of the sync contract).

- `UZI_E2E_BOT_PAT` / `UZI_E2E_BOT_USERNAME` / `UZI_E2E_PROJECT` in a **gitignored
  `.env`**, **never read by the app** (not among `Config`'s fields — grep-verifiable).
  A live E2E suite should **skip (not fail)** when they are unset, mirroring
  `scripts/smoke.sh`'s require-a-running-stack stance.

---

# PRD #3 — Agent templates & per-user Anthropic token

## 29. Shared secret-at-rest (secretbox + `UZI_SECRET_KEY`)

Serves human: "Anthropic token encrypted in the DB"; best-practice bar.

- **`api/internal/secretbox/`**: AES-256-GCM (`Seal`/`Open`), per-message 12-byte
  random nonce prepended to ciphertext; GCM gives integrity (tampered row →
  decrypt error, not garbled plaintext). `LoadKey` reads a base64 32-byte key
  from an env var; empty ⇒ treated as unset.
  - Why: multica's secretbox pattern, applied to provider creds (which multica
    itself stored plaintext). NaCl-style name, AES-GCM construction.
- **Boot guard**: `config.Load` calls `secretbox.LoadKey("UZI_SECRET_KEY")`;
  missing / non-base64 / wrong-length aborts start **before** any DB connection.
  Key held for the process lifetime as one shared `*secretbox.Box` (`main.go`),
  not re-derived per request.
- **Provenance**: the secretbox util + config wiring were cherry-picked
  **byte-identical** from PRD #2's branch (958f9b3 → e6f17cb) so the two parallel
  branches merge conflict-free (whichever lands M1 first ships it). Its comments
  are still bot-PAT-oriented (PRD #2's use); genericizing them is a tracked
  post-merge follow-up, not a behavior change.
- **Rotation** invalidates all stored secrets across every feature at once
  (§15). Accepted, matches PRD #2.

## 30. `user_secrets` table + Anthropic token API

Serves human: "each user stores their own Anthropic token via the webui,
encrypted in the DB".

- **Generic kind-keyed table** (migration `00010`), chosen over a column on
  `users` so the next secret kind needs no shape change:
  ```
  user_secrets(
    id uuid PK, user_id uuid → users ON DELETE CASCADE,
    kind text CHECK (kind IN ('anthropic_token')),  -- new kind = one ALTER-CHECK migration
    ciphertext bytea,                                -- secretbox.Seal(secret)
    created_at, updated_at timestamptz,
    UNIQUE(user_id, kind)
  )
  ```
- **API** (current user only; no admin path to another user's value):
  - `PUT /api/me/secrets/anthropic_token` `{token}` → sanity-check → `Seal` →
    upsert. **Response is metadata-only** `{kind, created_at, updated_at}`.
  - `GET /api/me/secrets` → metadata list.
  - `DELETE /api/me/secrets/anthropic_token` → idempotent (absent ⇒ 204).
- **Token sanity check**: trimmed, non-empty, **1–4096 bytes**, no interior
  whitespace/control chars. **No format assumption** (no `sk-ant-` prefix check —
  Anthropic prefixes are not a documented contract); accepts both `setup-token`
  OAuth tokens and console API keys.
- **No reveal endpoint** (re-paste to rotate; multica's reveal+audit is more than
  MVP needs). **No live verification** (validated on first agent run, PRD #4 —
  avoids burning quota / hard-coding endpoint behavior).
- **Redaction**: token never logged, never in error strings; validation errors
  carry no token bytes. Enforced by a handler-level redaction test grepping logs
  for the plaintext fixture.

## 31. `agent_templates` store + builtins reconciler

Serves human: "agent templates stored in DB, editable via the UI (agents
themselves sit with code)".

- **Schema** (migration `00011`): `id, name (UNIQUE), description, model (NULL=inherit),
  tools (jsonb, NULL=inherit all), prompt_body, is_builtin, updated_by (→users ON
  DELETE SET NULL), created_at, updated_at`. No versioning/history in MVP;
  `updated_by`/`updated_at` give minimal attribution.
- **Builtin source of truth is Go, not SQL**: the repo's seven `.claude/agents/*.md`
  (coder, reviewer, auditor, tester, documenter, fact-checker, spec-keeper) are
  embedded **byte-identical** under `api/internal/agenttmpl/builtins/` (embed can't
  reference paths outside the module root, so they are copies; a **drift test**
  enforces they stay identical to the checked-in originals), parsed once at
  package init.
  - Why Go over a SQL seed: an **idempotent startup reconciler**
    (`store.ReconcileBuiltinTemplates`, `ON CONFLICT DO NOTHING`) inserts missing
    builtins and **never overwrites** an existing row, so admin edits survive
    restarts and future releases can add/upgrade builtins without a
    non-re-runnable seed.
- **Builtin lifecycle**: editable; **not deletable** (`DELETE` → 409); **Reset**
  (`POST /:id/reset`, builtins only, 400 on non-builtin) re-applies the embedded
  definition. Guarantees the core roles always exist for PRD #4.

## 32. Renderer (template → Claude Code subagent Markdown)

Serves human: templates are the definition PRD #4 will consume.

- **`agenttmpl.Render`** is a **pure function**, no DB dependency (so golden-file
  tests can pin output). Fixed field order: **name, description, tools, model**;
  `tools`/`model` lines **omitted when empty** (inherit).
- **`tools` is an inline comma-separated string** (`tools: Bash, Read, …`), not a
  YAML sequence — matches this repo's own `.claude/agents/*.md`. Built with
  ordered `strings.Builder`, **not `yaml.Marshal`** (a map marshal would reorder
  keys and break byte-stability).
- **Byte-match guarantee**: a builtin's rendered output byte-matches the
  checked-in source file, pinned by golden-file tests.
- **`GET /api/agent-templates/:id/rendered`** serves the Markdown **raw**
  (`text/markdown`, `X-Content-Type-Options: nosniff`), not wrapped in JSON, for
  any authenticated user.

## 33. Template validation & frontmatter/secret hardening

Serves human: best-practice bar; admin-edited free-form content is untrusted.

- **`name`**: kebab-case `^[a-z0-9]+(-[a-z0-9]+)*$`, unique, and **immutable after
  creation** (structural: `UPDATE` never touches `name`) — it is the subagent
  filename + PRD #4 routing key; rename = create-new + delete-old (non-builtins;
  builtins never renamed).
- **`description`, `prompt_body`**: non-empty. **`model`**: NULL or non-empty token
  (no CHECK — upstream accepts aliases *and* full model IDs, incl. `fable`).
  **`tools`**: NULL or JSON array of non-empty strings; `[]` **normalized to NULL**
  (inherit-all) so empty-list and inherit render identically.
- **Frontmatter-injection hardening**: reject control chars
  (`unicode.IsControl` + U+FFFD) in `description`, `model`, and each tool name,
  and **commas in tool names** (they'd break the inline comma-joined `tools`
  line). `prompt_body` is **exempt** (renders after the frontmatter, its newlines
  are ordinary Markdown).
- **Secret guardrail**: server **hard-rejects** a high-confidence full `sk-ant-…`
  token in `description`/`prompt_body`; the **UI warns** (non-blocking) on looser
  credential-ish patterns and unknown tool names, so prompts that merely *mention*
  token formats stay savable.
- **Concurrency**: last-write-wins (`updated_at`/`updated_by` attribute it); an
  `If-Match`-style precondition is a noted follow-up, out of scope.

## 34. Template API surface & authorization

Serves human: "editable via the UI"; admin-only writes (USER-CONFIRMED 2026-07-03).

- **Any authenticated user**: `GET /api/agent-templates`, `GET /:id`,
  `GET /:id/rendered` (they'll run these roles in PRD #4).
- **Admin only** (`RequireAdmin`): `POST`, `PUT /:id` (name ignored — immutable),
  `DELETE /:id` (409 for builtins), `POST /:id/reset` (400 for non-builtins).
  - Why admin-write / all-read: closes bottega's hole where any user rewrites the
    shared prompts everyone else's agents run with. Per-user forks deferred to
    PRD #4.
- All under PRD #1 session + CSRF.

## 35. Tooling & parallel-safety (PRD #2/#3 coordination)

- **Goose version ranges reserved**: PRD #2 `00002`–`00009`, PRD #3 `00010`+.
  Duplicate goose versions from parallel branches merge cleanly in git but fail
  at `goose up`; reserving ranges prevents the silent-until-runtime break.
- **Shared frontend shell files** (sidebar nav, Settings layout, route table) are
  touched by both PRDs — expected small merge conflicts, kept in dedicated commits.
- **sqlc** regen via pinned `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate`.
- **Build gate** uses `-buildvcs=false` (worktree VCS-stamp quirk).
- **E2E**: `scripts/smoke-prd3.sh` — seed → admin edit → non-admin blocked (403) →
  render stable → token set/rotate/delete → restart persistence → DB dump shows
  only ciphertext.

---

# PRD #4 — Agent Runtime: Workers, Job Queue & Live Run View

Serves human Feature #4. **Status: M1 + M2 built and merged; M3–M7 not yet built.**
Sections below cover only what ships in M1 (server: schema, join tokens, worker
protocol, sweeper, web-facing run/worker API) and M2 (worker: TS skeleton, git
worktree lifecycle, run state machine driven by a stub executor). The SDK
executor, plan-gate/MR workflow, live web UI, E2E, and docs (M3–M7) are designed
in the PRD (`prds/4-agent-runtime-workers.md`) but are **not** realized in code
yet; where a decision here anticipates them it says so.

Two moving parts split along plan.md's server/client trust boundary: the **api**
service gains the run queue + worker registry (no new container); a new
top-level **`agent/`** TypeScript worker runs one-per-user, outbound-only.

## 36. Runtime architecture & trust boundary

Serves human: "server/client architecture; each user has their own client"; "agents
only create MRs (primary directive)"; "connection server/agent should be encrypted".

- **Outbound-only worker** (multica's daemon model, AI choice 2026-07-03 over
  API-spawned or shared workers): the worker opens every connection to the api and
  authenticates with a Bearer join token; it never listens. No `docker.sock` in the
  api; the model maps cleanly to later pods/VMs. In the compose demo the worker is
  an on-network service, not a remote client.
- **Server is the sole key holder.** Per claimed run the api decrypts that user's
  bot PAT (PRD #2 `forge_connections`) and Anthropic token (PRD #3 `user_secrets`)
  and returns them **only in the claim response** — never persisted on the worker
  beyond the run, never logged (reuses the PRD #2/#3 redaction discipline).
- **TLS on the worker↔api hop is DEFERRED** — the join token authenticates now;
  transport encryption arrives with a TLS-terminated ingress when the worker
  becomes remote. plan.md's "connection should be encrypted" is therefore only
  **partially met** by the MVP (authentication yes, encryption later). **Deferral
  accepted by the user 2026-07-04** — join-token auth now, transport TLS with the
  remote-worker/ingress; recorded as an accepted gap, not a satisfied requirement.

## 37. Runtime schema (migration `00020_workers_runs.sql`)

Serves human: Feature #4 job queue + worker registry + lossless live stream.

- Goose range **`00020`+** reserved above PRD #2 (`00002`–`00009`) and PRD #3
  (`00010`+); goose tolerates gaps so the range is conflict-free. One migration
  landed (`00020`); the range stays reserved for M3+.
- **`workers`** — `id, user_id→users ON DELETE CASCADE, name, token_hash bytea
  UNIQUE (sha256 of the join token), status ('offline'|'online', CHECK-constrained),
  last_heartbeat_at, version, timestamps`. Status is heartbeat-derived; **"busy" is
  never stored** — it is derived at read time from an active claimed/running/
  awaiting_approval run.
- **`runs`** — the unit of work; an issue can accumulate several runs over its life.
  `repo_id` is **uuid** (repos.id is uuid in PRD #2, not bigint). `issue_iid` +
  `issue_title` + `issue_description` are **snapshotted at queue time** so a run is
  self-contained even after PRD #2 reconcile evicts the `issues` cache row. State
  machine `queued → claimed → running → awaiting_approval → running → completed |
  failed | cancelled`, CHECK-constrained. Resume/affinity/liveness columns:
  `worker_id→workers ON DELETE SET NULL` (affinity), `session_id` (SDK resume),
  `last_seq` (message high-water mark), `requeue_count` (bounded re-queue),
  `iteration_count` (loop cap), `branch`, `mr_iid`, `failure_reason`, `plan_md`,
  and `claimed_at/started_at/finished_at`.
- **One-non-terminal-run-per-issue** enforced by a **partial UNIQUE index**
  `(repo_id, issue_iid) WHERE status NOT IN (completed,failed,cancelled)` — fixes
  bottega's check-then-insert TOCTOU race with a DB constraint. Terminal runs are
  excluded so an issue can be re-run once its prior run finishes.
- **`run_messages`** — `bigserial id, run_id→runs ON DELETE CASCADE, seq int,
  kind, agent, payload jsonb, created_at`, **UNIQUE(run_id, seq)**. Per-run gapless
  seq makes the worker's batched append idempotent and the browser stream replayable
  (fixes multica's lossy buffer). `kind ∈ {text, thinking, tool_use, tool_result,
  status, error, user_message, plan}`.
- **`run_user_inputs`** — steering channel: `bigserial id, run_id→runs ON DELETE
  CASCADE, kind CHECK ∈ {follow_up, approve_plan, reject_plan, cancel}, body,
  consumed_at, created_at`. A partial index on `(run_id, id) WHERE consumed_at IS
  NULL` serves the FIFO consume scan.
- Indexes: `idx_runs_claimable (user_id, status, created_at)` backs the claim scan;
  per-fk indexes on user/repo/worker.

## 38. Join token mechanism (`api/internal/jointoken`)

Serves human: "each user has their own client" (the worker must authenticate); best-practice.

- **Net-new** vs PRD #1 (no reusable token-hash precedent). Token = `uzw_` prefix +
  32 crypto/rand bytes, base64url. Shown **once** at issuance; only **sha256(token)**
  is stored in `workers.token_hash`. Prefix aids humans + secret scanners and is
  covered by the hash.
- **Unsalted sha256 is deliberate and safe**: the token is uniformly random 256-bit
  data, so there is no low-entropy keyspace to precompute — the per-record salt that
  password hashing needs buys nothing here.
- `RequireWorker` middleware extracts the Bearer credential, hashes it, looks up the
  row by an **indexed equality on the full 32-byte digest**, then does a
  **constant-time compare** (`crypto/subtle`) as belt-and-suspenders. Lookup failure
  and mismatch both return an indistinguishable 401 (a prober learns nothing about
  which tokens exist). No CSRF step — the credential is a held bearer secret, not an
  ambient browser cookie.

## 39. Worker protocol (`/api/worker/*`, Bearer join token)

Serves human: "each user's client connects to the server"; job queue.

Six endpoints, all under `RequireWorker`, **no worker id in any URL path** — identity
is always the Bearer token (AI wire decision 2026-07-03). Worker routes are excluded
from the browser rate limiters.

- `POST /register` — brings the worker online **and recovers its orphaned runs**
  (see §41). Body `{version, name}`; `name` is accepted for wire compatibility but
  **ignored** — the authoritative name is the label chosen at token issuance, not
  something the worker may overwrite. (This was the third wire bug: M1's
  DisallowUnknownFields 400'd the posted `name` until it was declared-and-ignored.)
- `POST /heartbeat` — refreshes liveness (default every 15s; server marks offline
  after 45s of silence).
- `POST /runs/claim` — atomic claim (§40). **204** when idle (no body); **200** with
  the full claim payload otherwise.
- `POST /runs/{id}/messages` — `{messages:[{seq, kind, agent, payload}]}`; batch is
  validated **all-or-nothing** (one invalid message rejects the whole batch, nothing
  written) then persisted and the run's `last_seq` advanced. Idempotent on
  `(run_id, seq)`. Ownership-checked.
- `POST /runs/{id}/state` — `{status, plan_md?, branch?, mr_iid?, failure_reason?,
  iteration_count?, session_id?}`. **The wire key is `status`** (matches
  `runs.status`, multica, and the worker client) — the second wire bug was M1
  decoding `state` while M2 sent `status`; fixed to `status`. A report that lands on
  an already-terminal run returns **409** with the run's real status; the worker
  treats 409 as success and stops (learning it was cancelled). `session_id` is
  persisted atomically with the transition when present (empty = no change) — the
  resume prerequisite.
- `GET /runs/{id}/inputs` — consumes + returns pending steering inputs FIFO as
  `{inputs:[{id, kind, body, created_at}]}`. **Delivery marks consumed** (no separate
  ack): a worker crash right after the GET drops that input — accepted MVP trade-off
  (user re-sends); an explicit ack endpoint is a clean later addition.

## 40. Claim payload & atomic claim (`workersvc/claim.go`, `ClaimRun` query)

Serves human: "server/client; agents work an issue"; secret-at-rest (server holds keys).

- **Atomic claim**: `UPDATE runs SET status='claimed', worker_id=…` selecting the
  oldest `queued` run **for the worker's user** via `FOR UPDATE SKIP LOCKED`
  (multica) so concurrent workers claim disjoint runs without blocking.
- **Worker affinity**: a re-queued run with a prior `worker_id` sorts its original
  worker first and is claimable by **others only after `WORKER_AFFINITY_GRACE`**
  (default 2m) — so a resume lands on the disk that still holds the session +
  worktree. Ordering `COALESCE(worker_id = @worker_id, false) DESC, created_at ASC`.
- **Claim payload shape** (AI wire contract, pinned across the M1/M2 branches):
  flat run fields at top level (`run_id, issue_iid, issue_title, issue_description,
  status, branch?, session_id?, last_seq, iteration_count, requeue_count, plan_md?`)
  plus nested objects:
  - `repo: {id, url, clone_url, default_branch?}` — `clone_url` is `url + ".git"`,
    **tokenless by contract** (the PAT is supplied out-of-band, never embedded in the
    URL — a credentialed URL would persist in the bare repo's on-disk config and
    defeat the guarantee). Clone from `clone_url`, not `url`.
  - `secrets: {forge_username, forge_pat, anthropic_oauth_token}` — decrypted for
    this run only; the **sole** secret-delivery channel; never logged/persisted.
  - `agents: [{name, description, prompt_body, tools, model}]` — an **array** of
    PRD #3 templates as **structured fields** (not `.claude/agents/*.md` files),
    ready to map onto programmatic SDK `AgentDefinition`s in M3. `tools` nil =
    inherit-all; `model` nil = inherit. (Deviation from M2's initial single-`template`
    assumption; reconciled to the array.)
  - `config: {run_timeout_seconds, idle_timeout_seconds, max_iterations}` — **caps in
    SECONDS on the wire** (the first-fixed field-unit bug was M2 expecting `_ms`);
    any ms consumer converts at the use site.
- **Claim-time credential failure is non-retryable**: if the bot PAT or Anthropic
  token is missing/undecryptable, the run is **failed immediately with a static
  reason** (carrying no secret bytes) and the claim answers **idle** — a broken run
  never wedges the worker in a claim loop. A run that vanished between claim and
  assembly (cascading forge-connection delete) also answers idle.

## 41. Run lifecycle, sweeper, orphan recovery (`workersvc/service.go`)

Serves human: liveness never trusted to the agent; "restart-resilience".

- **State transitions** (`SetState`) are all guarded against terminal statuses, so a
  cancel that raced in makes the worker's report a no-op (`applied=false` → 409).
- **Server sweeper** (goroutine beside the PRD #2 poller; also run once at boot as the
  orphan sweep — bottega): each pass, in order, (1) marks heartbeat-stale workers
  offline; (2) reclaims claimed-but-never-started runs past `ClaimGrace` (fixed 5m,
  not an env var); (3) fails running runs past `RUN_TIMEOUT`; (4) fails stale-worker
  runs **over** the re-queue cap; (5) re-queues stale-worker runs **under** the cap
  (incrementing `requeue_count`). Fail-over-cap runs before re-queue so a run that
  just hit the cap isn't re-queued.
- **Bounded re-queue**: `RUN_MAX_REQUEUES` (default 1) caps worker-death re-queues;
  `0` is supported (fail immediately on worker death).
- **Register-time orphan recovery**: a re-registering worker has, by definition, just
  restarted and is executing nothing, so any run it still holds is orphaned — failed
  over cap, else re-queued **to the same worker** (affinity) for resume. This is what
  makes `docker compose down && up` recover: a fresh worker's own restart signal,
  which the server cannot infer from heartbeats (a fresh heartbeat would otherwise
  defeat staleness detection). The sweeper still covers never-returning workers.

## 42. Web-facing run + worker management API (M1)

Serves human: "start a run from a card"; "generate a worker join token"; "see run
messages / correct them" (the SPA consuming these is M5, but the endpoints ship in M1).

- All under PRD #1 session + CSRF; every route authorizes through the owning user
  (`GetRunByIDForUser`, `GetRepoForUser`) — a user only ever touches their own runs/
  workers. Admin cross-user views are M5.
- **Workers**: `POST /api/workers` (issues a worker, returns the plaintext join token
  **once**), `GET /api/workers` (list with derived busy status), `DELETE
  /api/workers/{id}` — **409 while the worker still owns a non-terminal run** (the FK
  is ON DELETE SET NULL; deleting would orphan the run past every sweep and wedge the
  one-active-run index).
- **Runs**: `POST /api/repos/{id}/runs` (queue a run — repo owned + issue is a cached
  PRD issue **with a PRD link**; title snapshotted from cache, description from the
  request; duplicate active run → 409 via the unique index), `GET /api/runs/{id}`,
  `GET /api/runs/{id}/messages` (replay after a seq), `POST /api/runs/{id}/inputs`
  (submit steering).
- **No-live-poller server-side transition**: a `cancel` or `reject_plan` for a run
  with no live poller (still `queued`, or its worker gone stale) is applied
  **server-side** directly to `cancelled`/`failed` — the input is never stranded
  waiting for a `GET /inputs` that will never come. "Live poller" = run assigned to a
  worker whose heartbeat is within `WORKER_HEARTBEAT_STALE`. Other inputs
  (follow-up/approve) are enqueued for the worker to consume.

## 43. Worker runtime (M2): TS project, config, loop (`agent/`)

Serves human: "client based on the Anthropic SDK, one per user" (SDK wiring is M3;
M2 builds the container + protocol + git around a stub executor).

- **TypeScript, Node 22** (AI choice 2026-07-03 over Python): bottega is the
  production-proven reference for exactly this stack (Agent SDK + per-user OAuth
  token), the SDK's TS build bundles the Claude Code binary, and the team stack stays
  Go + TS. The **executor is isolated behind one interface** so the SDK surface
  (M3) is swappable and a future OpenAI executor is a second implementation, not a
  rewrite.
- **Outbound loop** (`worker.ts`): register-with-retry once, then a heartbeat loop
  and a claim loop run concurrently until abort. Claim → execute a run to a terminal
  state → immediately try the next; otherwise wait `WORKER_POLL_INTERVAL` (3s). **One
  run at a time** in M2.
- **Config** (`config.ts`, from env): `UZI_API_URL`, `UZI_WORKER_TOKEN`, `UZI_DATA_DIR`
  (default `/data`), `UZI_WORKER_NAME` (default hostname), plus interval knobs that
  accept **either a Go-style duration** (`15s`, `500ms`, `2h`) or a bare integer as
  ms — so the same knob reads identically server-side, in compose, and here.
- **Terminal-callback reliability** (`client.ts`): every `/state` report — terminal
  and non-terminal alike — is retried with bounded backoff on transient failures
  (5xx/408/429/network); an "already terminal" response (409, or a <500 body
  mentioning "terminal") is treated as **success** (multica) so a lost ack/duplicate
  replay is safe; other 4xx are fatal.

## 44. Git: bare-clone cache + worktree lifecycle (`agent/src/git.ts`)

Serves human: "agents use worktrees to work in parallel"; primary directive (no
ambient credentials).

- **Layout under `UZI_DATA_DIR`**: `repos/<host>+<ns>+<repo>.git` one bare clone per
  repo kept across runs (multica's cache), `worktrees/<repo>/issue-<iid>` one
  worktree per run removed on terminal state. `bareDirName` joins host + path
  segments with `+` (illegal in forge path segments → collision-free).
- **Branch is `agent/issue-{iid}`** (dot-agent-deck naming). **Branch-exists ⇒
  attach** (worktree-as-ledger idempotency for resume / same-issue re-run), else
  create off the resolved default branch. Bare clone converted to remote-tracking
  refspec so `agent/*` heads the worktrees lock never collide with fetched refs.
- **Per-bare-path serialization** (chained promises): git's lockfiles can't take
  parallel mutations on the same bare repo.
- `safe.directory=*` (container UID ≠ volume owner makes the ownership check useless
  and breaking) + `GIT_TERMINAL_PROMPT=0` (auth failure → clear error, not a hang).

## 45. Worker-holds-PAT + PAT-off-argv (primary directive, reconciles sparse-env)

Serves human: "agents only ever create MRs, never write to main"; "secrets never
outside the DB"; "verify agents can't modify main directly".

- **The worker process — not the agent — holds the PAT and performs every
  authenticated network git op** (clone, fetch, and in M4 push + MR). The agent only
  edits files and makes local commits (no network, no credential). This is the
  design correction that reconciles the sparse-env guarantee with pushing: the agent
  subprocess **never sees the PAT at all**, which is what makes the "worker
  disk/agent env contain no PAT" criterion actually hold and turns the M3 push-guard
  into pure defense-in-depth.
- **PAT delivered to git via env-scoped config only**: `GIT_CONFIG_COUNT` +
  `GIT_CONFIG_KEY_n=http.extraHeader` + `GIT_CONFIG_VALUE_n="PRIVATE-TOKEN: <pat>"`
  (git ≥ 2.31). **NOT `git -c`** — whose values land on argv and are readable in the
  container process table (`ps`, `/proc/<pid>/cmdline`) while an agent subprocess may
  be alive during the push. The env path keeps the PAT off argv, off on-disk config,
  and out of logs (git plumbing logs args only, never env). GitLab reads
  `PRIVATE-TOKEN` for authenticated requests (auditor decision).
- **Deferred to M4 (before live clones)**: host-scoping the `http.extraHeader` (so
  the token isn't sent on a redirect to another host) and pinning
  `http.followRedirects`. M2's fixture repos are local and ignore the header, so its
  efficacy against a live GitLab is validated when M4 wires real clones.

## 46. Message batching / gapless seq (`agent/src/batcher.ts`)

Serves human: "see the messages the agents produce"; lossless stream (beats multica).

- Buffers emitted messages and flushes seq-numbered batches every
  `WORKER_MESSAGE_BATCH_INTERVAL` (500ms), **continuing numbering from the claim's
  `last_seq`** so a resuming worker never collides seqs. A failed batch is put back
  at the head and retried, so the server (idempotent on `(run_id, seq)`) never sees a
  gap in what it receives. `close()` awaits any in-flight flush then drains with
  bounded retries. The server persists before broadcasting, so this channel is a
  liveness optimization, **never the source of truth**.

## 47. Executor interface + stub (M2 boundary) (`agent/src/executor.ts`, `runner.ts`)

Serves human: Feature #4 agent execution (the real SDK executor is M3).

- **`Executor` interface** (`run(ctx) → {branch}`) is the seam M3 replaces with the
  Claude Agent SDK. `RunContext` gives the executor the issue snapshot, the
  checked-out worktree path, the branch, and an `emit` for stream messages.
- **`StubExecutor`** (M2): writes a marker file, makes a **single local commit** on
  `agent/issue-{iid}` (no SDK, no network, no push), so tests prove the full
  claim → worktree → work → report loop with **no AI in the loop**. Commit identity
  pinned per-invocation via `-c` (nothing written to worktree config), gpg signing
  forced off.
- **`RunRunner`** drives one claim through `running → clone/worktree → execute →
  completed | failed`, always tearing down the worktree (keeping the bare clone) in
  `finally`. Per-run secrets are registered with the logger's scrubber before any
  work; the claim payload itself is never logged.

## 48. Worker image & delivery (`agent/Dockerfile`)

Serves human: "local docker-compose demo"; worker is a container per user.

- The worker image **cannot be distroless** (unlike the api): the SDK's Bash tool
  needs a real shell and the git flow needs `git` + coreutils, so it is a **minimal
  Node + git + bash base**. The api image stays distroless.
- Shipped as a compose profile (`--profile agent`) / `docker run`; `UZI_DATA_DIR` is
  a persistent volume holding repos, worktrees, and (M3) the pinned SDK session dir,
  so `compose down && up` doesn't wipe caches or sessions.

## 49. Runtime configuration (env, extends §13/§25)

Serves human: operability of the run queue + worker liveness.

Server-side (defaults): `RUN_TIMEOUT` 2h, `RUN_IDLE_TIMEOUT` 10m (worker-enforced,
shipped in the claim), `RUN_MAX_ITERATIONS` 5 (worker-enforced), `RUN_MAX_REQUEUES`
1 (`0` = never re-queue), `WORKER_HEARTBEAT_INTERVAL` 15s, `WORKER_HEARTBEAT_STALE`
45s, `WORKER_POLL_INTERVAL` 3s, `WORKER_AFFINITY_GRACE` 2m. Claimed-never-started
grace is fixed at 5m in code, not an env var. Invalid numeric/duration values fall
back to defaults (PRD #1/#2 convention). Worker-side: `UZI_API_URL`,
`UZI_WORKER_TOKEN`, `UZI_DATA_DIR`, `UZI_WORKER_NAME`, and the interval knobs above
(duration-or-ms).

## 50. Guardrail layering (design; enforcement lands M3)

Serves human: "agents only ever create MRs, never write to main — primary directive."

Layered so no single layer is load-bearing, and none trusts the model:
1. **GitLab role**: the bot is Developer and `main` is protected (documented project
   config; PAT least-privilege verification is a plan.md fast-follow).
2. **Worker-owned network git** (§45): the agent literally has no push credential, so
   protected-branch writes are impossible regardless of what the model attempts —
   **this is realized now** (M2), the strongest layer.
3. **SDK `PreToolUse` deny-hook** (M3, defense-in-depth): deny `git push`, any
   `--force`/`-f`, remote mutation, and credential-reading commands (`git config
   --get`, `env`, and `ps`/`/proc` probes per the auditor ask).
4. **Permission mode `bypassPermissions` + deny-hook + `disallowedTools`** (M3): not
   `default` (hangs headless) nor `dontAsk` (too restrictive for the coder). Read-only
   subagents constrained via per-`AgentDefinition` `tools` allowlist (subagents
   inherit the parent mode).
5. **`settingSources: []`** (M3): nothing from the cloned repo's `.claude/*` can grant
   the agent extra permissions — a repo-borne prompt-injection defense none of the
   inspirations has.
Only layers 1–2 exist in M1+M2; 3–5 are M3 and are recorded as design intent.

## 51. Session resume (design; wiring lands M3)

Serves human: "restart-resilience"; "correct a running agent".

- The SDK writes transcripts under `$HOME/.claude/projects/…`; M3 pins `HOME`/the
  session dir onto the persistent `UZI_DATA_DIR` volume so restarts don't wipe
  sessions. On resume the worker uses the claim's `session_id` + `last_seq`. M1
  already persists `session_id` from state reports (the prerequisite); M2 carries it
  through the claim payload. If a re-queued run lands on a worker whose disk lacks the
  session (affinity grace expired), it falls back to a fresh session + branch-attach,
  which re-triggers the plan gate (documented, acceptable).

## 52. Runtime testing & validation posture

Serves human: testing-credentials policy (never mint an OAuth token); best-practice.

- **All runtime tests use dummy credentials + the stub executor** (never a live
  Anthropic session). M2's Vitest suite (25 tests) exercises the client, git cache
  (incl. the secret-flow test asserting the PAT is off argv), batcher, and runner
  against a fake api; M1's Go suite runs the service against a fake store.
- **Live compose smoke** (dummy creds, per policy) verified the full control plane on
  the merged tip: migrations→00020, register→online, heartbeats, 3s claim polling/204,
  claim-time credential-failure → run failed with a static reason, stale worker →
  offline at ~49s.
- **Deferred to M6**: Postgres-backed `SKIP LOCKED`/partial-index tests (the Go suite
  uses a fake store, so the concurrency + index semantics are not yet exercised
  against real Postgres). An optional final live E2E uses the user's **existing**
  token, provided at that moment, never minted.

## 53. Validation-wave lessons (2026-07-04) & runtime coordination

- **Lenient fakes hid wire truth**: three contract bugs survived green suites on both
  M1 and M2 because each side's fake encoded its own assumption — (1) `/state` key
  `status` vs `state`; (2) claim `config` units `_seconds` vs `_ms`; (3) `register`
  body `name` vs DisallowUnknownFields (found only by the live smoke). **Rule
  institutionalized: every milestone pairing gets a live smoke before merge; a
  cross-branch contract test is queued for the integration branch.**
- **Milestone parallel-safety**: M1 (`api/`) and M2 (`agent/`) touch disjoint trees
  and pinned the wire contract in the PRD decision log so they could be built in
  parallel; the shared truth is the JSON shape, not shared code.
- **Docker smoke isolation**: compose project names are daemon-global — every smoke
  needs a unique `-p` and a scratchpad `--env-file` (a bare `up` falls back to the
  worktree `.env` whose placeholder `UZI_SECRET_KEY` intentionally crashes the api).
  The user's own stack runs on host :8080, so smokes publish elsewhere.

---

# PRD #7 — In-app Docs Section (terse howtos with screenshots)

Serves human Feature #7 ("a docs section on uzi with relevant howtos … include
screenshots"). **Status: M1–M5 built and merged on `prd-7-docs-section`; the only
remaining work is swapping placeholder screenshots for Vlad's real captures in one
final commit.** Adds **zero** new services, API routes, DB tables, or env vars —
the whole feature is a build-time bundle plus SPA routes.

## 54. Docs bundling: repo `docs/` as the single source of truth

Serves human: "a docs section … howtos"; best-practice (no drift between repo docs
and in-app docs).

- The in-app docs are the **same `docs/*.md` files** the repo already carries — the
  web build imports them at **build time**, so there is structurally no second copy
  to drift. Chosen over an API endpoint or a duplicated `web/src/docs/` tree (single
  source of truth, no runtime moving parts); the cost is a widened compose build
  context, judged cheap.
- **Mechanism** (`web/src/lib/docs.ts`): a Vite eager glob imports `../../../docs/*.md`
  as raw strings (`query: "?raw"`) and `../../../docs/img/*` as hashed asset URLs
  (`query: "?url"`, emitted as lazily-fetched files, **not** inlined into the JS
  bundle so per-image cost is a page download bounded by the size budget in §58).
- **Sibling-layout invariant**: the globs resolve **relative to the source file**, so
  the `web/`↔`docs/` sibling layout must survive into the image. `docker-compose.yml`
  `web.build` is `{ context: ., dockerfile: web/Dockerfile }` (repo root, not `./web`);
  the Dockerfile copies `web/` and `docs/` into `/app/web` + `/app/docs` and builds
  with `WORKDIR /app/web`. The validator (§58) reaches docs at a **different** depth
  (`../../docs` from `web/scripts/`); both only work under the preserved layout.
- **Root `.dockerignore`** (net-new, applies only to the web image now that its
  context is the repo root — `web/.dockerignore` stops applying): excludes `.git`,
  `inspiration/`, `api/`, `agent/`, `e2e/`, `web/node_modules`, `web/dist`, and — via
  bare + `**/` `.env*` globs at every depth — all env files. It must **not**
  blanket-exclude `*.md` (the way `web/.dockerignore` does), or it would strip the
  very files this feature bundles.
- **Dev**: Vite `server.fs.allow` includes the repo root (raw imports are read off
  disk in dev; the production rollup build is unaffected).

## 55. Frontmatter contract & hand-rolled parser

Serves human: "howtos … include screenshots" (which pages appear in-app); self-
describing docs (adding a page never touches web code).

- Every `docs/*.md` carries minimal YAML frontmatter `title` / `order` / `audience`
  where `audience ∈ {user, operator, design, contributor}`. **Only `audience: user`
  pages** are listed and routable in-app, ordered by `order` (integer, unique among
  user pages). Slug = filename (`gitlab-bot-setup.md` → `/docs/gitlab-bot-setup`).
  `docs/README.md` is excluded from the glob and from validation.
- **`audience` over a hardcoded page list in web code**: docs stay self-describing;
  `operator`/`design`/`contributor` pages (installation, configuration, auth-design,
  proc-hardening, dev-conventions) stay repo-only because the in-app audience already
  has a running instance.
- **Hand-rolled ~15-line parser** (`parseFrontmatter`), not `gray-matter` (which
  drags Buffer polyfills into the browser bundle). It consumes **only a leading
  `---\n` fence at byte 0**; a `---` later in a body (e.g. inside agent-templates.md's
  embedded code fence) is content. `order` parses to a number or null; unknown
  `audience` values are ignored (fall to the default).
- **Graceful-no-frontmatter at the viewer**: a file with no/malformed leading fence
  renders as `audience: design` (repo-only) rather than erroring — so a pre-content
  build stays green and M1 did not depend on M2's content. The **build gate (§58)**
  is stricter: it *fails* on missing/invalid frontmatter (README exempt). The two are
  intentionally asymmetric — graceful at runtime, enforced at build.

## 56. Viewer: routes, rendering, link/image rewriting, XSS posture

Serves human: "docs section on uzi" (in the webui, where users hit the moments that
need it); best-practice (safe rendering of repo content).

- **Routes**: `/docs` (index — user pages only, one-line auto-summary each via
  `summarize()`, the first paragraph after the H1 stripped of markdown) and
  `/docs/:slug` (`web/src/pages/Docs.tsx`, `DocPage.tsx`). An unknown slug renders a
  not-found state **inside the docs shell** with a link back to `/docs`, not the
  App-level catch-all redirect. A "Docs" nav link shows in **both** authenticated and
  logged-out states (`web/src/components/Layout.tsx`).
- **Public, unauthenticated** — bot-setup and token howtos are exactly what a user
  needs *before* they can do anything in uzi, and nothing in `docs/` is secret (the
  stack is loopback-only; the files are world-readable in the repo).
- **Renderer**: `react-markdown` + `remark-gfm` (existing docs use GFM tables), added
  with the lockfile as an explicit M1 step. **Deliberately no `rehype-raw`**: content
  is repo-authored/reviewed, so raw HTML stays **inert** instead of needing a
  sanitizer — the smallest safe pipeline (multica's `rehype-raw`+sanitize is for
  untrusted LLM output; dead weight here). No nginx CSP change needed (same-origin
  `img-src 'self'`, class-based Tailwind, no inline scripts).
- **Link rewriting** (`resolveHref`, pure + unit-tested by injecting the is-user-page
  predicate): a relative `*.md` resolving to a bundled **user** page → the in-app
  `/docs/:slug` route (react-router `<Link>`); any other relative target (repo-only
  doc, `../plan.md`, `auth-design.md`) → the **pinned GitLab blob base**
  `https://gitlab.example.com/vtmocanu/uzi/-/blob/main/` + repo-relative path.
  `#anchor` fragments are preserved in both cases (existing docs lean on them).
  External `http(s)` links get `target=_blank` + `rel="noopener noreferrer"`.
- **Defense-in-depth XSS**: `javascript:`/`vbscript:`/`data:`/`file:` schemes are
  **independently neutralized** to an empty href/src in `resolveHref`/`resolveImageSrc`
  (ASCII control/space chars stripped first so `java\tscript:` can't slip past),
  rather than trusting react-markdown's `defaultUrlTransform` alone. Protocol-relative
  `//host` and the `/\` variant are classified as external, not in-app routes. This
  only fires on a mistake (content is trusted) but closes the door structurally.
- **Images**: relative `img/*` srcs resolve through the hashed-asset URL map;
  absolute/root-absolute srcs pass through; dangerous schemes → empty.

## 57. Content curation & house style

Serves human: howtos that a small-attention-span human will actually read.

- **Six `user` pages** (one more than the PRD's original five — `worker-setup` was
  promoted to `user`): `getting-started` (order 10, the golden path, mostly links),
  `gitlab-bot-setup` (20), `board` (30), `anthropic-token` (40), `agent-templates`
  (50), `worker-setup` (60).
- **House style** (build-*warned*, not gated): task-titled, numbered steps, one
  screenshot per major step, no design rationale (link to design docs instead),
  target **≤ 60 body lines** per page.
- **Nothing cut outright**: trimmed material relocates to an explicit destination —
  `installation`/`configuration` → `operator`; `auth-design`/`proc-hardening` →
  `design`; the E2E-test-bot convention split out of gitlab-bot-setup → the
  `contributor` page `dev-conventions`; agent-templates' renderer/validation
  internals → ARCHITECTURE.md's Agent templates section. The relocation mapping was
  recorded in the M2 commit, not left implicit.
- **User pages are em-dash-free** (the user's global writing preference for
  reader-facing content); internal/design docs are unaffected.

## 58. Build-time validation gate (`web/scripts/check-docs.mjs`)

Serves human: docs must not silently rot (there is no CI yet — the image build is
the only gate).

- Runs standalone (`npm run check-docs`) and as the **first step of `npm run build`**
  (so it also runs inside the web image build). Its frontmatter parser **mirrors**
  the viewer's so the gate accepts exactly what the viewer parses.
- **Fails the build** on: missing/invalid frontmatter (README exempt); a `user` page
  missing/duplicate/non-numeric `order`; **reference-style links** (`[label]: url`,
  `[text][ref]`) — invisible to the existence check and easy to break, so the
  convention is inline-links-only; broken relative doc→doc / doc→img links; any
  `docs/img/*` over the **300 KB** per-image budget (ships to every visitor).
- **Warns only** on a `user` page over the 60-line budget.
- **Context-aware link checks**: the web image context is trimmed to `web/` + `docs/`,
  so repo-root targets (`../ARCHITECTURE.md`, `../plan.md`) are absent there by
  design (the viewer rewrites them to GitLab anyway). Links resolving **inside**
  `docs/` are always checked; targets **outside** `docs/` are checked **only in a
  full checkout** (`.git` present), so the containerized build stays green.

## 59. Screenshots: placeholders now, real captures as one final commit

Serves human: "include screenshots" + user decision 2026-07-04 (real shots provided
by Vlad, landing as a single final commit after everything else).

- Stored in `docs/img/`, kebab-named `<page>-<step>.png` (e.g. `board-move-card.png`),
  each ≤ 300 KB (§58), meaningful alt text describing the intended shot.
- **The filename is the swap contract**: pages ship now with clearly-marked generated
  **placeholder** PNGs (7 of them, produced by the dev-only
  `web/scripts/gen-doc-placeholders.sh`); Vlad's real captures replace them **in
  place** so no page markdown changes. Implementation never blocks on captures.
- **Still pending**: the real-screenshot swap is the one open item on this PRD — a
  single final commit after all other milestones.

## 60. Meta-docs & handoff

Serves human: the docs feature must be maintainable (a contributor can add a page).

- `docs/README.md` — contributor authoring guide (frontmatter contract, line budget,
  screenshot conventions, inline-links-only rule); exempt from the glob and the gate.
- ARCHITECTURE.md gains a short **Docs** section; the root `README.md` Documentation
  list includes the pages added since the PRD's original table (`getting-started`,
  `board`, `dev-conventions`) plus a `/docs` pointer.
