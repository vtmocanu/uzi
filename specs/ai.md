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
- **Rotating `UZI_SECRET_KEY` invalidates every stored bot PAT** — no re-encrypt
  path in MVP; every user must reconnect. Accepted (see §17).
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
