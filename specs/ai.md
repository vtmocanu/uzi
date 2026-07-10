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
- **Untrusted-markdown remote images** in the run view load loopback-only; a remote
  `<img>` in LLM output is an accepted beacon risk until a CSP `img-src` restriction
  lands (see §61).
- **Worker `debug` logs contain full (redacted) run frames by design** — may include
  sensitive repo content; enable `UZI_LOG_LEVEL=debug` only when inspecting (see §65).

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
   config; **now continuously verified by PRD #5's privcheck** — see §95–§97, which
   turns this layer from documented-and-hoped into checked-at-save-and-periodically).
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

---

# PRD #11 — Run View UX: markdown plan, boxed auto-scroll activity, terse events

Serves human Feature #11. **Status: M0–M5 built and merged on `prd-11-run-view-ux`.**
Adds **zero** new services, DB tables, env vars, or API/worker-protocol routes: the
feature is a web-side re-render of payloads that already reach the browser, plus one
additive worker field and one debug-log line. Coordinates with PRD #7 (shares its
markdown pipeline rather than forking a parallel one — a user-stated coordination
requirement). M2+M3 shipped as **one commit** (`d45f41f`) because `RunView`,
`ActivityFeed`, and `RunEvent` are mutually dependent and can't compile half-wired.

## 61. Shared markdown core + per-caller trust policy

Serves human: "the plan renders as markdown"; best-practice (untrusted LLM output).
Coordinates with PRD #7 §56 (which first added react-markdown for trusted docs).

- **`web/src/components/MarkdownCore.tsx`** — the policy-free core: pins the pipeline
  (`react-markdown` v10 + `remark-gfm`, **no `rehype-raw`** — raw HTML stays inert
  text) and the one policy-neutral element override (GFM tables scroll inside their
  own `overflow-x-auto` container). Link/image **behaviour is deliberately not decided
  here**; each caller injects its own `a`/`img` via `components` (caller overrides merge
  over the base, so even the table wrapper is replaceable).
  - Why the split: PRD #7 had shipped `DocMarkdown` with the docs trust policy baked
    in. Instead of a parallel renderer for the run view (which the user asked to
    avoid), M0 extracted the shared core and refactored `DocMarkdown` into one caller.
    `DocMarkdown`'s observable behaviour is unchanged — PRD #7's `docs.test.ts` (19
    tests) still passes and docs pages render identically.
- **Two callers, two policies, never mixed:**
  - `DocMarkdown` (**trusted repo docs**, PRD #7): `rewriteHref`/`resolveImageSrc`,
    internal links become same-tab SPA `<Link>`s, hashed-asset images.
  - `Markdown.tsx` (**untrusted LLM output**, PRD #11): links treated as **external
    only** — new tab + `rel="noopener noreferrer"`, never rewritten to SPA routes (a
    model must not forge in-app navigation); images size-capped via CSS
    (`max-h-64 max-w-full`, `loading="lazy"`) so a remote/oversized `<img>` can't blow
    up the box; dangerous schemes neutralized to inert text / an alt-text placeholder.
- **Untrusted-caller sanitization is layered**: react-markdown v10's `defaultUrlTransform`
  is the **primary** sanitizer (strips `javascript:`/`data:`/`file:` to `""` before the
  overrides run); `schemeIsDangerous` (reused from PRD #7's `lib/docs.ts`) is
  **independent defense-in-depth** so a future urlTransform change can't reopen the hole.
- Both callers use the shared `.docs-prose` typography class for a consistent
  dark-theme look with the plan panel.
- **Accepted risk** (audit): a remote `<img>` in LLM output is a potential beacon;
  acceptable **loopback-only**. Revisit with a CSP `img-src` restriction before any
  multi-user exposure (see §15).

## 62. Activity feed: bounded box + follow mode

Serves human: "activity is a bounded auto-scroll box following live runs; pause on
scroll-up; one-click resume".

- **`web/src/lib/useFollowScroll.ts`** — `useFollowScroll(itemCount)`. Load-bearing
  rule (PRD review H2): `follow` lives in a **ref** updated by the `onScroll` handler
  (true when within 48px of the bottom), and on append we scroll to bottom **iff the
  ref is true**. We **never re-derive "am I at bottom?" after React appended nodes** —
  `scrollHeight` has already grown by then, so the classic post-append check detaches on
  the first message and fights the user.
- Scrolling is **instant** (`scrollTop = scrollHeight`), never `behavior:"smooth"` (a
  burst of frames makes smooth scrolling lag); no focus stealing.
- **Paused affordance**: while follow is off and messages arrive, an accruing `newCount`
  drives a "**{n} new ↓**" pill in the Activity header; clicking `jumpToBottom` re-arms
  follow and clears it, and scrolling back to the bottom also re-arms. `isNearBottom` is
  a pure function of the three scroll metrics (testable without a live DOM).
- **`useReconnectingBanner(connected, delayMs=3000)`** — promotes a WS disconnect to a
  visible "Reconnecting…" banner only after ~3s, so a brief blip doesn't flash. The
  replay burst on reconnect flows through the same follow rules ("{n} new ↓" if paused).
- **`ActivityFeed.tsx`** hosts the bounded container: `max-h-[65vh] overflow-auto`,
  `[overscroll-behavior:contain]` (no scroll-chaining into the page), styled like the
  plan box; `role="log"` + `aria-live="polite"`.

## 63. Terse per-kind event rendering

Serves human: "no raw JSON anywhere in the run view"; "long tool calls visibly show as
running (no dead-air)".

- **`web/src/components/RunEvent.tsx`** renders one readable line per event; the
  `JSON.stringify` fallback is **deleted**. Pure helpers (`toolSummary`, `resultToText`,
  `formatDuration`, `describeStatus`, `describeError`, `buildToolIndex`) are exported and
  unit-tested in isolation. Kinds: `text | thinking | tool_use | tool_result | status |
  error | plan`, plus a muted **unknown-kind fallback** (`RunMessage.kind` is a free
  string; raw frames live in worker debug logs). The schema-declared-but-currently-
  unemitted `user_message` kind (§37) intentionally lands on that fallback.
- **`text`** → markdown via the untrusted `Markdown`. **`thinking`** → collapsed dimmed
  one-liner, expandable. **`tool_use`** → `⚙ {name} — {summary}`; per-tool summary
  (`Bash`→command first line, `Read/Write/Edit`→path, `Glob/Grep`→pattern[+path],
  `Task`→`subagent_type: gist` so sub-agent spawns are recognizable, `WebFetch`→url,
  `WebSearch`→query, else compact `key: value`), truncated ~160 chars with a `more`/`less`
  expander. **`plan`** → terse "📋 plan submitted (awaiting approval)" one-liner (the body
  lives in `PlanPanel`; not rendered twice, never hits the fallback).
- **In-flight tool spinner (kills dead-air)**: a `tool_use` with no matching result yet
  renders a spinner + live elapsed timer (`RunningIndicator`, 1s tick) while the run is
  live; a settled call shows a **client-computed** duration
  (`result.created_at − tool_use.created_at`), no payload support needed; a non-live run
  with no result shows "no result".
- **Tool pairing by id, never adjacency**: `buildToolIndex` maps `tool_use_id →
  tool_use.id` across the full message array (spanning agent groups), so parallel calls
  (`useA, useB, resA, resB`) pair A→resA / B→resB. A result whose id isn't among the
  tool_use ids is an **orphan** and renders standalone. First-writer-wins on duplicate ids.
- **`tool_result`** folds under its call: small results (≤8 lines) inline, larger collapse
  to "show {n} lines"; **`is_error` results auto-expand** with a rose ✗. `content` may be a
  **string or an array of blocks** (`mapUser` passes it through as-is) — `resultToText`
  walks the array, extracts text blocks, and flags non-text blocks (images) as a labeled
  "non-text result … omitted" placeholder (known lost signal, accepted). Rendered as a
  React-escaped `<pre>`, **NOT markdown** (tool output is not prose and must not be
  re-parsed).
- **`status` has two shapes** (`describeStatus`): worker progress `{text}` → rendered
  as-is, muted (already human-readable, e.g. "worktree ready on…"); SDK frame `{event}` →
  `init` → "agent started ({model})"; `result` **discriminated by `subtype`** (not
  `event:"success"`) → `success` → "agent finished ({duration}, {n} turns, ${cost})" from
  the newly-forwarded fields (§64), else "status: result/{subtype}".
- **`error`** (`describeError`): worker `{text}`, or SDK `{event, subtype, errors[]}` →
  render **`subtype`** (the useful signal: `error_max_turns` vs `error_during_execution`) +
  joined `errors[]`; no assumed `message` field. Rose-tinted.
- **Performance / a11y**: each row is `React.memo`-ized keyed by immutable `seq` (append-
  only stream ⇒ memo trivially valid; markdown parse memoized per message via the memoized
  `Markdown`). DOM cap: past 1000 messages, render only the most recent 500 behind a "Show
  {n} earlier messages" expander (cap-and-expand chosen over a virtualization dep for this
  MVP; revisit on evidence). Collapsibles are real `<button aria-expanded>`; ✓/✗ carry text
  labels, not color alone.
- **Cap-straddle fix** (`2f38f9b`): fold-skip of a `tool_result` is scoped to
  `visibleToolUseIds` (ids present in the visible window), not the full index. Otherwise a
  call capped out past the 500-message window while its result stays visible would skip the
  result AND never render its call — vanishing it. Now such a result renders standalone.
- Messages are grouped into consecutive **agent blocks**; the last block is marked
  "active" while the run is live. Timestamps demoted to agent-group boundaries + hover.

## 64. Additive worker forwarding (`mapResult`)

Serves human: the finish line's duration/cost (part of the terse, useful event set).

- **`agent/src/sdk-messages.ts` `mapResult`** forwards `duration_ms` and `total_cost_usd`
  from the SDK `result` frame alongside the existing `num_turns`. **Additive only** —
  unguarded passthrough in the same style as `num_turns`: when the SDK omits a field it
  lands `undefined` and JSON-serialization drops it, so nothing new appears on the wire
  when absent; the web `describeStatus` type-guards each field before display. Success is
  discriminated `subtype === "success" && is_error !== true`; everything else maps to an
  `error` message carrying `subtype` + `errors[]`.
  - This is the **only** non-web change in the PRD; no existing field changes shape (per
    the user constraint that the run view needed almost no protocol change).

## 65. Raw frames → worker debug logs (`MessageBatcher.emit`)

Serves human: "raw frames belong in the worker's docker logs behind `UZI_LOG_LEVEL=debug`,
not the browser"; explicit user rejection of an in-UI raw-JSON toggle.

- Hook point is **`MessageBatcher.emit`** (`agent/src/batcher.ts`) — the single chokepoint
  every outgoing run message passes through, already holding a run-scoped child logger and
  the redacted payload. One added line: `log.debug("run event", { seq, kind, agent, payload })`.
  Default `info` behaviour is unchanged (no new info-level lines).
- **Gate**: the existing `UZI_LOG_LEVEL` env (compose-plumbed, default `info`).
  `UZI_LOG_LEVEL=debug` → `docker logs -f uzi-agent-1` shows every (redacted) frame. This is
  documented as *the* way to see raw events; there is deliberately **no "show raw JSON"
  toggle in the web UI** (user rejection — JSON in the browser helps nobody, `docker logs` is
  the debug surface).
- **Double redaction**: the payload is already redacted before buffering (`redact.ts`), and
  the child logger independently scrubs the serialized line against the `SecretRegistry`
  (worker token, forge PAT, Anthropic OAuth token, git credentials). Both are exact-substring
  scrubs — **accepted limitation** (a mangled/split secret could theoretically slip;
  spot-checked deterministically in M4's `batcher.test.ts`).
- **Accepted risk** (audit): debug logs may contain sensitive repo content **by design** —
  the point is full frames. Enable `debug` only when intentionally inspecting (see §15).

## 66. Docs & validation posture

Serves human: operability ("how to see raw events"); PRD #4 testing-credentials policy.

- `docs/worker-setup.md` (env-var table) + `docs/configuration.md` document
  `UZI_LOG_LEVEL=debug` as the raw-run-event surface; a line notes the UI shows terse events
  while raw frames live in worker logs.
- **Tests** (vitest, dummy data — no live Anthropic session, per PRD #4's never-mint policy):
  `RunEvent.test.tsx` (per-kind renderers, parallel-call pairing, error auto-expand,
  unknown-kind fallback), `ActivityFeed.test.tsx` (grouping, cap-straddle), `useFollowScroll.test.tsx`
  (follow-mode logic), `batcher.test.ts` (debug log path + redaction, deterministic),
  `sdk-messages.test.ts` (additive forwarding).
- The **live visual walkthrough** (plan approval, streaming activity, long tool call,
  reconnect, `docker logs` at debug) is a **manual-validation item for the tester** — it needs
  a real Anthropic session beyond the sandbox and must never be required to prove a milestone.

---

# PRD #12 — Board–Run Lifecycle: auto column moves, run-aware cards, in-app issue view

Serves human Feature #12 — wires the PRD #2 board and the PRD #4 runtime together so
the board reflects run state without hand-dragging. **Status: M1–M4 built, reviewed,
audited APPROVED (2026-07-04/05); M5 (docs + live validation + spec sync + MR) in
progress.** Adds one migration (`00021`) and one leaf package; no new
services/containers/env vars. Full rationale + Decision Log: `prds/12-board-run-lifecycle.md`.

## 67. Lifecycle notifier + column automation

Serves human: "issues move columns automatically with the run lifecycle — In Progress
while agents work, a review column once the MR is open; no hand-dragging".

- **Single `runLifecycle.Notify(runID, status)` hook** at **every** status-write site
  (7: SetState running/completed/failed; `SubmitInput` server-side cancel/reject;
  `Sweep`; register orphan recovery; `MarkRunFailedByID`; `CreateRun` for queued).
  AutoMove subscribes and reacts to `queued → In Progress`, `completed → Human Review`,
  `failed`/`cancelled → origin`; ignores the rest.
  - Why a status-change notifier, not a SetState hook: `cancelled` is unreachable
    through SetState and five transition sites never pass through it (fact-check found
    them). One hook at the write sites is the only point that sees all transitions.
    (PRD Decision #4.)
- **In Progress applied at run CREATION (queued), not first `running`.** The click is
  the intent signal and gives instant board feedback; and the server cannot distinguish
  a first `running` from a requeue-resume (`SetRunRunning` uses `COALESCE(started_at,
  now())`, prior status gone), so a running-triggered move would re-drag the card on
  every resume and fight manual placement. (PRD Decision #1.)
- **Notify is inline at 5 sites; the 2 batch sites (Sweep, orphan recovery) stamp
  `move_pending_since` only** and let the reconciler restore origin — the "no forge I/O
  in the liveness sweep path" rule (§69) makes an inline forge call wrong there.
- **`bcast.PublishState` stays an independent hook, NOT multiplexed through the
  notifier** (despite PRD Decision #4's literal wording): verified no transition loses
  its WS emission and none double-emits.
- **AutoMove reuses the board drag's forge-first path** (`board.PlanLabelMove`: set the
  target column label, strip other column labels, one atomic `UpdateIssueLabels`, then
  the local snapshot). Credentials resolved server-side from the run's `user_id`+`repo_id`
  (claim-context pattern, extended to also return `forge_project_id` + the repo's column
  set, which the base claim query omits). Label writes attributed to the bot user
  (run-starter ≠ label author misattribution accepted + documented). Closed-issue guard:
  never auto-move a closed issue.

## 68. Origin-column snapshot + restore

Serves human: "a failed run must not silently strand the card or lose its backlog
placement" (the restore rule behind Feature #12's automatic moves).

- **`runs.origin_column` snapshotted at CreateRun** from the issue's current column
  (`""` = Open/no column, `NULL` = unknown/pre-migration). Failed/cancelled restore this
  column, never a hardcoded Open — a card run from "Later" returns to "Later".
  (PRD Decision #2.)
- **`NULL` origin never restores** (unknown snapshot ≠ Open; never strip a human's
  column label on a guess).
- **`runs.board_column`** records the column the automation last *successfully* wrote
  (NULL until first success) — needed because "the automation's last column" is
  otherwise not a stored fact and is undefined after a failed write.
- **Manual-drag guard** (restores + every retry): move only iff the issue's current
  column `== COALESCE(board_column, origin_column)`; any other value means a human
  placed the card — skip and clear the pending marker.

## 69. Move-retry reconciliation

Serves human: "failed forge column moves are RETRIED via reconciliation — not dropped,
not a persisted move queue" (2026-07-04 user review).

- **Why retry at all**: `completed` is terminal, so a dropped `completed → Human Review`
  write would never self-heal — the original "next lifecycle event heals it" stance was
  wrong for the terminal event. (PRD Decision #3.)
- **`runs.move_pending_since` (timestamptz) stamped in the SAME transaction as the
  status write** at every AutoMove-reacting site — closes the crash window between
  status-persist and AutoMove that a flag-on-failure would miss. A newer transition
  re-stamps (new deadline); a retry failure never re-stamps (else the deadline never
  elapses).
- **Cleared** by AutoMove on success (records `board_column`), by the guard on
  manual-drag detection, and by the board `MoveIssue` handler for the issue's runs on
  any manual drag ("a manual drag heals it", literal).
- **Reconciler uses its own TOTAL status→column map** ({queued, running,
  awaiting_approval}→In Progress; completed→Human Review; failed/cancelled→origin), NOT
  the notifier's partial map — by retry time a queued run has usually advanced to
  `running`, which the live hook deliberately ignores (§67), so reusing the partial map
  would no-op the most common retry forever. (PRD Decision #3, key review correction.)
- **Own goroutine/ticker, isolated from the 15s liveness `Sweep`**: forge I/O (15s
  timeout) against a down forge must not starve worker-loss recovery. Picks up markers
  older than a short grace (avoid racing the inline AutoMove) within a 30-min window;
  re-reads run status + issue labels immediately before the write to narrow the clobber
  window (GitLab labels have no CAS — acknowledged residual race, ~15s cadence makes it
  rare).
- **30-min give-up LEAVES the marker** (a silent clear would hide the drift behind a
  correct-looking badge); warn-log. The stale marker is cleared later by the next
  transition or manual drag, and is available to a future "column out of sync" indicator.
- Reconciliation, not a queue: no move payload is stored; recomputing from current
  status is idempotent, survives restarts, and cannot apply a stale move.

## 70. Human Review column + board-load retrofit

Serves human: "the MR opening moves the issue to a review column".

- **Board order everywhere: In Progress, Human Review, Upcoming, Later** (the two
  workflow columns lead, backlog buckets follow). Fresh boards get it from
  `DefaultColumns`.
- **Idempotent board-load retrofit** (`GetBoard`): a repo with columns but no Human
  Review gets the label ensured and the column inserted at In Progress's position + 1.
  Because `board_columns.position` is not unique and `InsertBoardColumn` neither shifts
  nor is position-keyed, the retrofit first bumps displaced columns up one
  (`ShiftBoardColumnsFrom`) so Human Review lands directly after In Progress.
- **Never created as a run side effect** — a run mutating curated column config was
  reviewed as wrong.

## 71. `internal/board` leaf package

Serves human: best-practice (avoid a service→service import cycle).

- Column constants + `ResolveColumn`/`PlanLabelMove` extracted into a leaf
  `api/internal/board` package so both `forgesvc` (board handler) and `workersvc`
  (AutoMove) call the shared move-planning logic without a service→service import.

## 72. Run-aware board DTO

Serves human: "the board must show that runs happened / are happening — badges + MR link".

- **Each card gains `latest_run: {id, status, mr_iid, failure_reason, owner_name,
  worker_name, created_at, updated_at, is_mine, run_count} | null`** (newest by
  `created_at`).
- **`DISTINCT ON (issue_iid) … ORDER BY created_at DESC`, NOT the PRD's `LEFT JOIN
  LATERAL`**: sqlc v1.30 doesn't propagate LATERAL nullability (types the joined run
  columns non-nullable → scan panic on a no-run issue). DISTINCT ON returns only
  issues-that-ran, mapped onto cards Go-side (`assembleCards`), rides the M1 composite
  index `runs (repo_id, issue_iid, created_at DESC)`; the created_at tie is unreachable
  under the one-non-terminal-run-per-issue constraint.
- **`is_mine`** server-computed (`run.user_id == viewer`) so the owner's user id is
  never exposed; the run-view link renders only for the owner (a non-owner would 403 on
  `GetRunByIDForUser`). **`run_count`** window count powers the ×N badge without a
  client fan-in.
- **`owner_name` falls back to the owner's email** — harmless today (per-connection
  repos ⇒ every board self-owned) but a tracked multi-user revisit item: switch to a
  neutral label before boards are ever shared. **Superseded by §163 (PRD #33):**
  display-name-or-empty, email dropped from the board queries entirely, and the DTO's
  `failure_reason` owner-gated.

## 73. Board badges, attention strip, self-refresh

Serves human: "the board must update itself (no manual Refresh)"; card badge/MR-link signal.

- **Badge taxonomy** (`Board.tsx` IssueCard): queued (neutral, instant feedback);
  running (pulsing + elapsed + worker name); awaiting_approval (amber, loudest card);
  failed (rose + failure_reason tooltip); stopped (neutral, never rose — see §74);
  completed → MR chip `!{mr_iid}` linking the MR, or a plain "completed" badge when
  `mr_iid` is null (**completed-without-MR is never invisible**); a subtle ×N count when
  an issue has >1 run.
- **MR chip URL built client-side, `isHttpsUrl`-guarded** with a plain-text `!N`
  fallback (same trust pattern as the docs/run-view link handling).
- **Attention strip**: a persistent banner above the columns when any of the user's runs
  on the repo is `awaiting_approval` ("N run(s) awaiting your approval →"),
  column-independent — this is the state where a human is the blocker and a worker is
  held busy. Fed by the `ListRuns` `?repo_id=&issue_iid=` filter.
- **Self-refresh**: the board polls `getBoard` every ~10s while mounted,
  **visibility-gated** (pause on `document.hidden`); this is what surfaces auto-moves +
  badges without Refresh. The start-run gate switched off the `listRuns` fan-in onto
  `latest_run` (one fewer request, no race).
- Known limitation (out of scope, documented): no MR-state tracking — a chip can
  advertise an already-merged MR, and Human Review never auto-drains on merge. Named
  follow-up PRD. **Update:** PRD #24 landed the close/reopen watcher (§89–§91), and
  §160 (PRD #33) now surfaces its `mr_state` on the chip (merged/closed variants);
  auto-drain on merge is already handled by the agent MR's `Closes #N` + poller sync,
  not a gap.

## 74. Deliberate-stop neutral styling (cross-surface)

Serves human: "a deliberate human stop is not breakage" (consistent styling across
board, Runs list, issue view).

- A cancel or plan-rejection renders as a neutral "stopped" badge (never rose) **across
  all three surfaces**, implemented as **exact server-literal matching in
  `isStoppedRun`**: failure_reason ∈ {"run cancelled", "plan rejected"} or status
  `cancelled`. Replaces the earlier `/cancel/i` substring heuristic (fragile,
  surface-local).
  - Why exact literals: a live-poller cancel reaches the server as SetState `failed`
    with reason "run cancelled" (status `cancelled` only occurs via the server-side
    no-poller branch), so the badge must be derived from status + failure_reason.
    Matching the exact strings the server writes is deterministic where a substring
    heuristic drifts.
  - **Known residual** (documented in-code): a live-poller *plan reject* carrying the
    user's verbatim free-text reason stays "failed" — no client heuristic can catch
    arbitrary user text.
  - **Superseded by §161–§162 (PRD #33):** the string heuristic (`STOPPED_FAILURE_REASONS`)
    is deleted in favor of a server-stamped `runs.stop_kind`; `isStoppedRun` becomes
    status/stop_kind-based (terminal-guarded), and the verbatim-reason residual above is
    fixed.
- **Auto-move toast suppression for manual drags**: a `suppressToastIids` ref silences
  the "#42 → column" toast for a card the user just dragged, released after 11s (> the
  10s poll). Accepted tradeoff: a genuine auto-move of a just-dragged card inside the
  window is applied silently.
- **Tone logic single-sourced**: `runStatusTone` hoisted to `runBadge.ts`, consumed by
  both RunsList and IssueView — kills cross-surface tone drift.

## 75. In-app issue view

Serves human: "clicking an issue stays in-platform (in-app view with its runs + a
start-run action); GitLab reachable via an explicit icon".

- **Route `/repos/:repoId/issues/:iid`** (`IssueView.tsx`): title, iid, column badge +
  non-column label chips, author, description rendered as markdown (**mandatory** — it
  carries the PRD link and is the run-decision basis), full run history (status, started,
  duration, worker, MR link, → run view), gated Start run, explicit GitLab link.
- **Card interaction** (`Board.tsx`): the title becomes an in-app `<Link
  draggable={false}>` (a native `<a>` is draggable and hijacks the card's HTML5 drag
  payload — a real bug, not cosmetic); a small GitLab glyph keeps the forge one click
  away. RunView gains a breadcrumb back to its issue + board.
- **Issue-detail endpoint `GET /api/repos/{id}/issues/{iid}`** returns card fields +
  description only (no `latest_run`). The description is fetched **live via the
  connection PAT, never cached**; behind the per-user forge rate budget; the forge's
  PAT-redacted error text is reflected in the 502 body (conscious auditor-noted choice).
- **Run-history MR rendered as a LINK** (not a bare chip): the project base is derived
  client-side from `issue.web_url` by stripping `/-/issues/N` (`lib/forgeUrls.ts`),
  https-guarded with a plain-text `!N` fallback — same trust pattern as the board chip.
- **Run-history timestamp is `started_at ?? created_at`** (PRD §3 "started").
- **Start-run gate derives from full history** (`activeRunInHistory`) — equivalent to
  the board's `latest_run` gate under the one-non-terminal-run index.

---

# PRD #14 — Multica-inspired "ember" UI redesign

Serves human Feature #14 — reskins the whole web UI to the selected "ember" design.
**Status: M1–M5 built, reviewed, audited, browser- and real-stack-validated; M6
(review gate + merge) landed on `main` as `2efd83b`.** `web/` + docs only; no
backend/schema/env changes. Full rationale + milestone log: `prds/14-multica-ui-redesign.md`.

## 76. Ember design tokens (CSS-variable theme layer)

Serves human: "multica-inspired ember design selected as the real UI"; best-practice
(themeable, no palette sprawl).

- **Design tokens are CSS custom properties** on `:root` / `[data-theme="ember"]`
  in `web/src/index.css` (bg/surface/text/accent/ring …); **Tailwind reads the
  variables only** (`bg-surface`, `text-muted`, `ring-ring`), never raw palette
  classes.
- **Enforced by a grep gate**: `grep -rE 'slate-|orange-|indigo-'` over `web/src/**`
  (excluding `index.css`, the one sanctioned home for the variable definitions and
  the token-backed `@apply` prose block) returns **zero hits** — no allowlist. Verified
  green after the reskin.
  - Why variables + one gate: a single token file makes a future light theme or the
    deferred mission-control/minimal identities a `data-theme` override block, not a
    refactor, and the gate keeps raw colors from creeping back page-by-page (the
    original UI's failure mode).

## 77. Design-over-logic merge with PRD #12

Serves human: Feature #14 reskin **without** regressing Feature #12 behavior.

- The prototype was authored before PRD #12 landed on `main`. Resolution rule when
  the two disagree: **PRD #12's board/run-lifecycle behavior is authoritative; the
  prototype's design vocabulary is re-applied on top** ("behavior wins, then restyle").
  Prototype `Board.tsx`/`RunView.tsx`/`RunsList.tsx` were **not** applied wholesale —
  their tokens/primitives were layered over #12's logic (badges, auto-move, attention
  strip, in-app `IssueView`, drag semantics all preserved).
  - Why: #12's vitest suite pins exactly the behavior the reskin must keep; the merge
    order inverted from the PRD's original plan (#12 shipped first), so #12 is the base.

## 78. Forge-type nav icon: web-side connection join

Serves human: "board nav entries carry the GitLab logo, generic git icon fallback";
"no backend change".

- `GitLabIcon` (tanuki) and `GitIcon` (generic git mark) added as inline
  single-path SVGs in `web/src/components/icons.tsx` (no new dependency).
- **`forge_type` is not on the `Repo` DTO** the nav renders from, so `AppShell`
  additionally calls `api.listConnections()` and maps `connection_id → forge_type` —
  a **web-only join**, keeping the "no backend change" scope. `gitlab` → tanuki;
  anything else (or a join failure) → `GitIcon` fallback.
  - Why the join over a DTO field: avoids a schema/handler change for a cosmetic nav
    detail; the fallback path is dormant today (`forge_types` is server-hardcoded to
    `["gitlab"]`) but lets a future Forgejo driver light up the git mark automatically.

## 79. Mock demo mode: inert behind `VITE_UZI_MOCK`

Serves human: best-practice (cheap zero-backend demo) without touching real builds.

- `web/src/mocks/` (`mockApi` + a mock WS `engine`/`socket` + fixture `data`/`store`),
  `web/Dockerfile.mock`, `web/nginx.mock.conf` ship as an **opt-in demo build**
  (`VITE_UZI_MOCK=1`). `web/src/lib/api.ts` gates on `import.meta.env.VITE_UZI_MOCK`
  (`MOCK_MODE`): flag unset ⇒ `api = realApi`, `createRunSocket` returns a real
  `WebSocket`.
- **Real builds execute no mock behavior** — the mock module is a statically-dead
  branch; its bytes still ship in the bundle (not tree-shaken; the size cost is
  accepted). Neither mock file is referenced by the real `docker-compose.yml`.
  Verified per-release by a `dist`-grep for a mock-only string (e.g. `andrei@uzi.local`).
  - Why keep the bytes: the cheapest way to demo uzi with no backend and to prototype
    future themes over the real component tree; correctness rests on unreachability,
    not on stripping.

## 80. Route guards: GuestRoute + global 401 handler

Serves human: Feature #14 shell must degrade gracefully on session loss (real-stack
validation item 4) — the mock hid every real-API auth seam.

- **`GuestRoute`** (`web/src/components/RouteGuards.tsx`): an authenticated user
  hitting `/`, `/login`, or `/register` is redirected to `/dashboard` (inverse of
  `ProtectedRoute`).
- **Global 401 handler**: `request()` fires one app-registered handler
  (`setUnauthorizedHandler`) **before** throwing, so every 401 is handled centrally
  instead of each page inventing its own 401 string. `AuthContext` registers it and
  clears the user; the redirect then happens declaratively through the existing route
  guards. Composes safely — the initial `me()` probe's expected 401 just clears an
  already-null user; a background poll's swallowed 401 still triggers it.
  - Why central: in this API a 401 uniformly means session-invalid (403 is authz), so
    one handler is correct and avoids a wall of per-page 401 errors on session expiry.

## 81. Unified run-status colors across surfaces

Serves human: "run status colors unified across all surfaces".

- One tone vocabulary in `web/src/lib/runBadge.ts` (`BadgeTone`:
  neutral/info/warning/ok/danger) drives the badge color on **board card, runs list,
  run view, and issue history** alike. Extends PRD #12's tones with **`info`**
  (claimed/running) and **`ok`** (completed) so those states stop reading as the same
  neutral gray. `runStatusTone` is the single source consumed by every surface, killing
  cross-surface tone drift.

## 82. Themed focus-visible ring

Serves human: "a themed focus-visible ring".

- A token-backed ring (`--ring` = brand) applied via `@apply outline-none ring-2
  ring-ring ring-offset-2 ring-offset-ink` on `a/button/input/select/textarea` and
  focusable `[tabindex]`, **scoped to `:focus-visible`** so it shows for keyboard/AT
  users and not on mouse click, replacing the default UA outline.

## 83. Dark-only for now

Serves human: Feature #14 (ember is dark-only; light mode not requested).

- The ember theme ships **dark-only**. A later light mode (or porting the deferred
  mission-control/minimal identities) is a `data-theme` block over the §76 tokens, not
  a refactor — the reason the tokens exist.

## 84. Compose test isolation (`env -i`)

Serves human: best-practice (real-stack validation must not touch the user's real
data), reinforcing §27.

- Real-stack test stacks are launched as
  `env -i HOME=$HOME PATH=$PATH docker compose --env-file <dummy> -p <unique> …`:
  a **shell-exported real secret overrides `--env-file`**, so a bare `--env-file` run
  can still seed the real admin/forge. Stripping the environment (`env -i`) plus a
  unique project name gives a fully isolated stack. Recorded in CLAUDE.md as a testing
  convention after M4 surfaced the footgun.

---

# PRD #23 — Web UX polish: live dashboard, collapsible sidebar, hide empty board columns

Serves human Feature #23 (the three UX gaps observed during the issue-#20 smoke test;
human.md entry pending user confirmation). **Status: M1–M4 built, reviewed, audited,
and browser-validated on branch `prd-23-web-ux-polish`.** `web/` + `docs/` only — no
API, schema, agent, env, or Go change anywhere. Full rationale + Decision Log:
`prds/23-web-ux-live-dashboard-sidebar-board.md`.

## 85. Shared visibility-aware poll hook + live dashboard

Serves human: "the dashboard should update live — a run reaching `awaiting_approval`
must show without a manual refresh."

- **Liveness is polling, not `/api/ws`.** The WS endpoint is per-run
  (`/api/ws?run=<id>`); a fleet-wide event channel is out of scope, and the board
  (PRD #12/#73) already proved a 10s visibility-gated poll. The dashboard reuses that
  precedent rather than inventing a new push transport.
- **`web/src/lib/usePollWhileVisible(cb, intervalMs)`**: the board's inline poll effect
  extracted to one hook (pure logic split out for tests, the `runBadge.ts` discipline).
  Fires `cb` every `intervalMs`, **skips a tick while `document.hidden`**, and fires an
  **immediate catch-up on `visibilitychange` → visible**. Both Board and Dashboard
  consume it; Board's hand-rolled copy is deleted.
  - **Latest-cb-in-ref (useInterval pattern)**: the interval effect keys only on
    `[intervalMs]` and reads `cb` through a ref updated each render. Keying on `[cb]`
    would tear down and recreate the interval every render for an inline-arrow caller
    (resetting the clock, never firing); the hook must not silently depend on callers
    memoising with `useCallback`.
- **First-load vs re-poll error split** (both consumers): skeletons show **only
  pre-first-load**. A first-load failure leaves state null so skeletons stay; a
  **background re-poll failure keeps the last-good data** — a transient poll error must
  never blank a populated page back to skeletons. (The board's `load()`/`poll()`
  split, now mirrored on the dashboard; its old `catch { setData(null) }` was
  deliberately not reused for re-polls.)
- **Re-poll fetches only the volatile endpoints** (`listRuns` + `listWorkers`).
  Repos/templates/secrets/connections change rarely and stay **mount-only**; the poll
  merges runs + derived worker counts into the existing overview. (Trimmed to these two
  on review — reviewer #10.)
- **Test net**: there were **no existing Board/Dashboard component tests**, so the
  refactor is not covered for free. M1 adds jsdom + fake-timer tests for the hook
  (interval fire, hidden pause, visibility catch-up, latest-cb-without-interval-reset,
  teardown) plus a Dashboard test pinning the first-load/re-poll error split — the
  safety net for both consumers.

## 86. Collapsible desktop sidebar (icon rail)

Serves human: "the desktop sidebar should be collapsible."

- **Collapse = icon rail, not fully hidden** (`w-14`, was `w-60`): logo mark only,
  icon-only nav items with native `title` tooltips, thin `border-t` separators instead
  of group labels, avatar-only footer. Every destination stays one click away — the
  pattern matches the multica-derived shell language rather than removing nav.
- **State lives in `AppShell`, initialised lazily** (`useState(() => prefs.get(...))`,
  §87) — not a post-mount effect, which would flash expanded→collapsed on first paint.
  Persisted per browser under `uzi.sidebar.collapsed`.
- **Width/padding classes are literal strings in a ternary**
  (`collapsed ? "lg:pl-14" : "lg:pl-60"`, same for `w-14`/`w-60`) so Tailwind's JIT
  emits both — never interpolated class names.
- **Board children fold into the "Boards" parent when collapsed**: on the rail every
  repo would render as an identical forge glyph, so per-repo board entries are hidden
  and `/repos` stands in (reviewer #7).
- **Toggle** is a real `<button>` at the footer edge (persistent, not hover-only) with
  `aria-expanded={!collapsed}` (tracks the sidebar, not a popup) + an `aria-label`;
  keyboard/AT operable. **The mobile sheet is unchanged** — it is already a full-width
  overlay that collapses by nature and passes neither collapse prop.

## 87. UI preferences in localStorage (`web/src/lib/prefs.ts`)

Serves human: the two toggles above must survive a reload; best-practice (no schema
churn for cosmetics).

- **Preferences are per-browser `localStorage`, not the API** — cosmetic, per-device,
  not worth a table or endpoints. Revisit only if cross-device settings become a theme.
- **Typed helper** `prefs.get<T>(key, fallback)` / `prefs.set<T>(key, value)`:
  JSON-encoded, every access guarded by `typeof window` (SSR/test without a DOM) and
  wrapped in `try/catch` so a disabled/quota-full store returns the fallback / silently
  drops the write and **can never throw into render**. First localStorage use in the
  app; later prefs reuse it. (multica's localStorage helpers guard the same way;
  their `useSyncExternalStore` reactivity is skipped — no two components watch one key.)
- Keys: `uzi.sidebar.collapsed` (§86) and `uzi.board.<repoId>.hideEmpty` (§88).

## 88. Derived hide-empty board columns (`web/src/lib/boardColumns.ts`)

Serves human: "empty board columns should be hideable." (The adjacent "columns should
auto refresh" ask needed **no change** — the board already polls every 10s while
visible and the server poller syncs the forge every ~1m; forge-side changes appear
within ~70s worst case. What this feature adds is that the hide/unhide decision is
recomputed on every one of those polls, so a hidden column can never go stale.)

- **A "Hide empty" toolbar tick box**, persisted per repo (`uzi.board.<repoId>.hideEmpty`
  via §87). State is re-read on `repoId` change (the route swaps `:id` **without
  remounting**, so a lazy init alone would keep the first repo's value).
- **Emptiness is derived at render from the freshly-polled `cardsByColumn`, never
  stored per column.** Pure `visibleColumns(columns, count, hideEmpty, dragActive)`
  (`boardColumns.ts`, unit-tested runBadge-style; `Board.tsx` owns the DOM):
  keep a column iff `!hideEmpty || count > 0 || dragActive`. Because there is no stored
  per-column state, a column that **gains** a card (auto-move, forge sync, another
  user) reappears on the next poll and one that **empties** disappears — there is no
  unhide event to handle, and it is unfailable. **No column is exempt** (an empty Open
  or Closed lane hides too — the tick box is the user's choice).
- **Drag reveals hidden empties as drop targets**: while a drag is active
  (`dragIid != null`) every column renders, and a lane visible only because of the drag
  is **dimmed** (`opacity-60`) so it reads as a transient target. A **"N hidden" hint**
  next to the box keeps hidden lanes discoverable.
- **Accepted v1 cosmetics** (reviewer #6/#12): revealing empties on drag-start shifts
  the lane row mid-drag (reserved-ghost-space is more code than the feature); the hint
  reads "0 hidden" during a drag (`columns.length - visible.length` with all lanes
  shown). Revisit only if either annoys in practice.
- **A11y bar applied post-validation** (M4 follow-up, non-blocking): the checkbox label
  gets `py-1.5` so its hit target clears the WCAG 2.5.8 24px minimum; the "N hidden"
  hint uses `text-muted` (not `text-faint`) for AA contrast at 12px — both stay within
  the §76 theme tokens, no hardcoded colors.

---

# PRD #24 — MR closed without merging → card back to In Progress

Serves human Feature #24 (close-unmerged → In Progress; user, 2026-07-05). The
board's column automation was driven exclusively by **run status** (§67); nothing
watched **MR state**, so a reviewer closing an agent's MR without merging (the
"rejected, redo it" signal) left the card parked in Human Review forever. This PRD
adds an edge-triggered MR-state watcher that moves the card back to In Progress on
close, and symmetrically restores it on reopen. Merged MRs trigger nothing — the
existing `Closes #N` → issue-close → sync path (§22) owns that outcome. Section
numbers continue past PRD #23's #88; the numbered decisions below realize the PRD's
Decision Log (`prds/24-mr-close-rework.md`).

## 89. Detection: poller-based, edge-triggered on `runs.mr_state`

Serves human Feature #24; reuses the PRD #2 poller (§22–§23).

- **Poller-based, not webhook-based.** uzi's target is a laptop where only `web`
  publishes a loopback-only port, so GitLab can never reach in — webhooks are
  structurally unavailable (same reason as §22's deferred webhooks). The MR check
  joins the existing per-repo poller tick and inherits its redaction, timeout, and
  bounded-concurrency posture for free.
- **Edge-triggered, not level-triggered.** The move fires once per opened→closed
  (and closed→opened) transition, tracked by persisting the last-seen MR state on
  the run in new column **`runs.mr_state`** (migration `00029`; NULL = never
  observed, no backfill). A level trigger ("MR is closed and card is in Human
  Review → move") would re-fight a human who deliberately drags the card back after
  the close; an edge touches the board exactly once per state change.
- **`mr_state` is watcher-owned** — the sole writer is `SetRunMRState` (§90); no
  run-status path writes it (`SetRunCompleted` rewrites `mr_iid` but never
  `mr_state`; requeue/sweep touch only non-terminal runs). Asserted in a query test.
  Why: the run stays `completed` — closing an MR is review feedback, not a
  run-status event — so the watcher and the run-lifecycle reconciler (§67) act on
  disjoint triggers (MR-state edges vs run-status markers) and cannot fight.
- **Poll cadence IS the retry loop** (see the state-persistence contract in §91):
  the watcher needs no durable move marker of its own, unlike run-lifecycle's
  `move_pending_since` (§69).

## 90. Forge method + schema + candidate query

Serves human Feature #24; extends the forge seam (§16) and runtime schema (§37).

- **Forge interface grows `GetMergeRequest(ctx, projectID, mrIID) (MergeRequest,
  error)`** with a neutral `MergeRequest{IID, State, WebURL}` type. `State` is one
  of the `MRState*` constants (`opened|closed|merged|locked`, as GitLab reports on
  the single-MR GET — distinguishable from the multi-MR list). GitLab driver only
  (Forgejo still deferred, §16); read-only; errors pass the existing PAT-scrubbing
  redactor (§16). `IsKnownMRState` gates recording (§91).
- **Migration `00029`** (`ALTER TABLE runs ADD COLUMN mr_state text`; reversible;
  no backfill). Reserved slot in the goose ledger (`00021` head, `00030+` PRD #5,
  `00036+` #19, `00040+` #6, `00050+` #16) so parallel branches don't collide.
- **`ListMRWatchCandidates` (Decision 4 candidate rule)**: per repo, `DISTINCT ON
  (issue_iid)` the issue's **latest run OVERALL** (newest `created_at`, riding
  `idx_runs_issue_history`), **then** filter `status='completed' AND mr_iid IS NOT
  NULL`. Order matters: filtering `completed` *inside* the `DISTINCT ON` would pick
  the latest *completed* run and silently watch a **superseded MR** while a newer
  rework run exists. So a non-completed latest run yields **no candidate** (an
  in-flight rework run suppresses the watch entirely — this closes the mid-rework
  reopen misfire), and a completed latest run with NULL `mr_iid` yields none either
  (never fall back to an older run's MR). Plus: issue open, and a **coarse** column
  prefilter (`jsonb_exists(labels,'Human Review') OR mr_state='closed'` — the
  reopen-edge watch, Decision 10). The SQL prefilter is deliberately **not**
  `board.ResolveColumn` (highest-position-wins across multiple column labels isn't
  cheaply expressible in SQL); it only bounds how many MRs get polled — the Go
  `ResolveColumn` check in the watcher is authoritative (§91).
- **`SetRunMRState`** — the sole `mr_state` writer; touches `mr_state` + `updated_at`
  only, never the run's status.

## 91. Watcher: edges, guards, state-persistence contract (`forgesvc.SyncMRStates`)

Serves human Feature #24; the primary-directive guard shape mirrors §67/§68.

- **Wiring**: `poller.syncRepo` calls `SyncMRStates` **after** the issue sync each
  tick (so MR checks see the freshest issue cache) and **only on a successful
  sync** (an early return when the forge is unreachable skips it). Per-candidate
  errors are log-and-skipped inside; only candidate-enumeration failure surfaces to
  the poller. Store seam: **widened the existing `forgesvc.IssueStore`** interface
  (rather than a new seam — TD §3 left the choice to implementation) with
  `ListMRWatchCandidates`, `GetIssueByIID`, `ListBoardColumns`, `SetRunMRState`, so
  the watcher stays fake-testable like the sync paths.
- **Two edges, symmetric.** `opened→closed` → move Human Review → In Progress
  (rework needed). `closed→opened` (Decision 6) → move In Progress → Human Review
  (a reviewer who closed by accident and reopened must not leave the board lying).
  Each fires once per transition.
- **Guards mirror `runlifecycle.apply`** (load issue → closed-skip → resolve-column
  → source-column compare → `AutoMove`, forge-first): never move a **closed** issue
  (Closed is terminal placement, not a workflow column); the card must currently
  sit in the edge's **expected source column** via `board.ResolveColumn` (a human
  who dragged it elsewhere wins — manual drags are never re-fought). A guard-skip
  **consumes the edge** (records state; we never retry a move a human pre-empted).
- **State-persistence contract (the anti-stuck-card invariant).** `mr_state`
  advances only on: (a) a successful move, (b) a deliberate guard-skip (manual drag
  / closed issue), or (c) a no-op observation (`merged`, `locked`, NULL bootstrap).
  On a forge-side **failure** (the MR read *or* the move) `mr_state` is **left
  as-is**, so the next poller tick re-observes the same edge and retries — the
  poller cadence is the retry loop. Consuming the edge on failure would re-create
  exactly the stuck-card bug this PRD fixes. Modeled in code as a three-value
  `moveOutcome` (applied / skipped → consume; deferred → leave).
- **Write ordering: forge move FIRST, then `SetRunMRState`.** A crash in between
  re-fires the edge next tick, and the source-column guard makes the retry a no-op
  (card already moved) — except the narrow window where a human dragged the card
  back to the source column in the crash gap, which then gets re-moved once;
  accepted (crash-window-sized) and noted in the watcher's comments.
- **NULL bootstrap (Decision 9)**: the first observation of a NULL-`mr_state` run
  records the state **without moving**. Acting on NULL→closed can't distinguish
  "closed and stuck" from "closed yesterday, already triaged elsewhere", so
  pre-migration runs (e.g. the motivating #9) never cause a wave of moves — a single
  manual drag heals a pre-existing stuck card (documented in `docs/board.md`).
- **Known-states-only recording (post-audit hardening, reverses an earlier
  record-verbatim choice).** An unrecognized or empty forge state is ignored
  **entirely** — no `mr_state` write — in both the bootstrap and transition paths,
  leaving the prior baseline (or NULL) intact. Because edges fire on exact string
  compares, recording garbage would mask the next real close until a full reopen
  cycle re-synced the baseline; ignoring lets a transient glitch self-heal.
  `merged`/`locked` are known but **record-only, never move** (a merge closes the
  issue via `Closes #N`; `locked` is transient during merge processing).
- **The watcher records only `mr_state`, never `runs.board_column`.** After a
  close-edge move a completed run's `board_column` still reads Human Review while
  the card sits in In Progress — safe **only** because completed runs are terminal
  for run-lifecycle (marker resolved, no further transitions); noted at the
  `board_column` definition so future code never re-stamps `move_pending_since` on
  completed runs.

## 92. Web + docs

Serves human Feature #24; no new surface required.

- **No new web surface.** The board converges via the existing sync, and MR links
  already render on cards/run views. Exposing `mr_state` on the runs API for an
  optional `closed`/`merged` chip next to the `MR !N` chips is a noted nice-to-have,
  **not built** (Open Question 2); prior art if pursued is multica's derived
  PR-status enum (precompute a display status, don't surface the raw forge string).
  **Built by §160 (PRD #33):** `mr_state` is now on both run DTOs and rendered as a
  derived-enum chip (`mrChipState`, the multica pattern) at all five MR-chip surfaces.
- **Docs**: `docs/board.md` gains the MR-close/reopen automation and the Decision 9
  note (pre-existing stuck cards heal with one manual drag).

---

# PRD #19 — Admin settings (app_settings) + autopilot label

Serves human Feature #19 (instance settings infrastructure + the one-label-in-GitLab
autopilot user story; user, 2026-07-05 — the human.md autopilot entry + the
failure-comment wording were **approved by the user (2026-07-05)** and are recorded in
`specs/human.md` Feature #19). **Status: M1–M5 built, reviewed, and audited on branch
`prd-19-admin-settings-autopilot`; M6 (e2e + docs + specs) in progress.** Two threads land together: a generic key/value
settings store whose first two tenants are the configurable `prd_label` and
`autopilot_label`, and an autopilot path where adding a label in GitLab runs a PRD
issue end to end with zero uzi interaction. Section numbers continue past PRD #24's
#92. Full rationale + Decision Log: `prds/19-admin-settings-and-autopilot.md`.

## 93. app_settings: generic KV store + cached accessor (`api/internal/settings`)

Serves human: "settings infrastructure with the two labels as its first tenants"
(user, 2026-07-05); best-practice (no one-off `prd_label` column, no speculative
framework).

- **Generic key/value table, two seeded tenants** (migration `00036_app_settings`):
  `app_settings(key TEXT PK, value TEXT NOT NULL, updated_by UUID NULL REFERENCES
  users ON DELETE SET NULL, updated_at TIMESTAMPTZ)`, seeded `prd_label='PRD'` +
  `autopilot_label='autopilot'`. Chosen over a typed column so the next setting
  (registration policy, self-improvement toggle) slots in with no schema change.
  **No secret material ever** lives here — values are admin-readable plaintext;
  secrets stay in env / secretbox (§17/§29).
- **Read-through, per-process, TTL cache** (`settings.Cache`, `SETTINGS_CACHE_TTL`
  default `5s`): RWMutex-guarded fast path reads a **copy-on-write snapshot** pointer
  under `RLock`; the store fetch never runs under the lock (fetch happens between an
  `RUnlock` and a `Lock`), so concurrent poller + handler reads never serialize.
  **Stale-on-error**: a refresh past TTL that fails serves the prior snapshot with a
  nil error; only a *cold* cache + DB error propagates. **Invalidate-after-commit**:
  a settings PUT invalidates only after `tx.Commit`.
- **Defaults compiled in, once** (`settings.Defaults` / `DefaultPRDLabel` /
  `DefaultAutopilotLabel`): accessors return `(value, error)` where the value is the
  compiled-in default even on a cold error, so every caller can treat resolution as
  best-effort (ignore the error, still get a usable label). A missing row never
  breaks boot. The migration seed is the one unavoidable SQL-literal copy of the
  defaults, kept in sync by comment; changing a default needs a **new** migration
  (00036 is immutable once shipped).
- **Admin API** `GET /api/admin/settings` + `PUT /api/admin/settings` (admin-gated
  like `ListUsers`/`PatchUser`). PUT body is a **partial** `{settings:{<key>:<value>}}`
  map (UI sends both keys, API tolerates one). Web: an admin-only "Instance settings"
  page (its own route/nav, **not** the per-user Settings shell).
- **Per-process cache is correct for the single-`api` compose stack**; a second
  replica would lag a PUT by up to the TTL — noted for a future k8s deployment, not
  fixed in the MVP.

## 94. Label validation + cross-key TOCTOU fix (`settings.ValidateLabel` / `Effective` / `ValidateMerged`)

Serves human: the two labels are user-editable and must never collapse to a state
that auto-runs every PRD issue; best-practice (concurrent-write safety on Postgres).

- **Per-value rules** (`ValidateLabel`): non-empty / not whitespace-only, ≤ 64 runes,
  **no comma** (GitLab's label-list separator). **Cross-key rule** (Decision 8):
  `prd_label != autopilot_label` — equal values would make every PRD issue also an
  autopilot issue. Changing a label **never creates it on the forge** (users create
  labels in GitLab themselves, as with columns §20); an autopilot label without the
  PRD label is invisible to uzi (the sync filter only sees PRD issues) — no run, no
  comment.
- **The cross-key check is authoritative inside the write tx**, against the two rows
  read `FOR UPDATE` (`ListAppSettingsForUpdate` → `settings.Effective(rows)` builds
  the committed effective map, merged with the pending writes, then `ValidateMerged`).
  A second concurrent single-key PUT **blocks on the row lock** and validates against
  the first writer's committed value, so two PUTs can never both pass and land
  `prd_label == autopilot_label`. The pre-tx cache check is kept only as a cheap
  fast-fail (and to keep the nil-pool handler tests valid); a stale cache can at worst
  false-reject a change the committed state would accept.
- **Proof without a live-Postgres harness**: this repo has no live-DB handler harness
  (handlers hold concrete `*store.Queries`/`*pgxpool.Pool`; the discipline is to
  extract pure helpers). `settings.TestEffectiveDrivesMergedValidationFromCommittedRows`
  proves the merge/validate reads the passed committed rows, not the cache; the real
  two-connection race is exercised in the M6 e2e stack.
- **Constraint for future settings keys** (carry-item): `FOR UPDATE` locks only
  *existing* rows. Both label rows exist because migration 00036 seeds them and
  nothing deletes them. Any future delete/reset-to-absent path, or a new
  cross-validated key **without a seed row**, reopens the race for concurrent INSERTs
  — seed every cross-validated key, and revisit this fix before adding a settings-row
  delete path.

## 95. Configurable prd_label end-to-end: LabelConfig injection + bootstrap delivery

Serves human: "an instance that wants a different PRD convention must not have to
fork the code."

- **`forgesvc.PRDLabel` const removed**; `forgesvc` depends on a small `LabelConfig`
  interface (`PRDLabel(ctx) (string, error)`) injected at `New`, resolving through the
  shared settings cache at the three use sites (both sync paths + issue creation). The
  const is gone; `settings.DefaultPRDLabel` is the sole compiled-in default.
- **Fail-safe label fallback**: `New` tolerates a **nil** resolver and the accessor
  returns the default even on a cold-cache error, so a settings outage degrades to the
  default label rather than an empty filter that would match nothing (or everything).
  Resolution is best-effort everywhere — the sync must never fail because settings are
  briefly unreachable.
- **Web receives the labels on the existing session/bootstrap response** (login /
  register / me now return `{user, prd_label, autopilot_label}`) — no new endpoint.
  `AuthContext` holds both, falling back to compiled defaults per field until they
  resolve; `Board.tsx` reads them instead of hardcoding `"PRD"` and excludes **both**
  labels from column suggestions.
- **Board effect of a change is documented, not instant**: old-label issues drop off
  boards only after the forced resync completes (§96) — the forge is the source of
  truth (§22), so this is correct, not a bug.

## 96. ForceReconcile: coalescing resync signal on a label change (`poller.Engine`)

Serves human: a `prd_label` change must reconverge the board without a restart or a
manual refresh.

- **No forced-resync mechanism existed** (the poller decided FullSync from an
  in-memory `pollCount % reconcileEvery` private to the Engine goroutine, unreachable
  from the settings handler). Added `Engine.ForceReconcile()` — a **non-blocking send**
  on a **cap-1 buffered** `forceReconcile chan struct{}`, handled in the Run
  goroutine's `select` (same goroutine that runs a tick, so the per-repo state reset
  needs no lock). The reset zeroes every enabled repo's `pollCount`; the next tick
  full-syncs all of them (FullSync ignores the HWM, so leaving it intact is fine).
- **The PUT returns after signalling** — it does not block on the sync itself, which
  needs each connection's decrypted PAT and belongs in the poller. **Coalescing**: a
  burst of PUTs collapses to a single pending reconcile (cap-1 + non-blocking send).
  The handler signals only when a submitted value actually differs from the committed
  one.
- **Fires on any label change** (prd or autopilot), though only `prd_label` affects the
  sync filter until autopilot ships — spec-endorsed and harmless (a redundant
  full-sync at worst).

## 97. Autopilot mapping + consent surface (`human_username`, `autopilot_enabled`, forge methods)

Serves human: "a third party must never be able to spend your Anthropic tokens without
your opt-in"; "self-declared forge username mapping" (user, 2026-07-05).

- **Consent is per-user opt-in, default off** (Decision 4/7). `users.autopilot_enabled
  BOOLEAN NOT NULL DEFAULT false`; no mapping or no opt-in → no run. A per-repo toggle
  would add surface without adding consent — the per-user switch plus "the repo must be
  connected by that user" already bounds who can trigger a run under one's token. The
  opt-in UI copy states the honest trade (Decision 7): autopilot removes the
  pre-execution human review of untrusted issue text, leaving the MR review as the
  remaining human gate; the repo guardrails (Developer role + protected `main`,
  worker-held PAT, deny-hook, `settingSources:[]`) are unchanged, so the blast radius
  stays "an MR you must review."
- **Self-declared human username** (`forge_connections.human_username TEXT NULL`): uzi
  only knows the *bot* identity from the PAT, so attribution to a human GitLab account
  needs a stored mapping. **Per-host partial unique index**
  `uq_forge_connections_host_human_username ON (base_url, human_username) WHERE
  human_username IS NOT NULL` (NULLs exempt) — a duplicate mapping is a **409**.
  Uniqueness is **exact-match, trimmed, case-sensitive**: a case-variant squat is not
  blocked, consistent with the PRD's accepted v1 identity-squat risk (documented;
  revisit case-folding only if it bites).
- **Verified-or-warned save** (`humanUsernameWarning`, a pure helper): before writing,
  best-effort `UserExists` on the forge — `""` (exists) stores clean, `not-found`
  stores with a warning, a lookup *error* stores with a different warning (a forge blip
  must never block the save). The write's unique-index violation is the authoritative
  uniqueness gate; verification only ever warns. Sending `human_username: ""` clears the
  mapping.
- **Forge interface additions** (GitLab driver only, all errors through the
  PAT-scrubbing redactor §16): `UserExists`, `ListIssueLabelEvents` (resource label
  events — who added which label when), `CreateIssueNote`. Neutral types `LabelEvent`
  and `IssueNote`. `ListIssueLabelEvents`/`CreateIssueNote` are consumed by the trigger
  (§98) and terminal hook (§99).

## 98. Autopilot trigger in the poller: transition-once detection (`poller.Autopilot`)

Serves human: "add one label in GitLab → uzi runs the PRD end to end; never re-run or
re-comment spuriously" (user, 2026-07-05); primary directive unchanged.

- **Detection runs only in the poller, after sync** (never in shared `forgesvc` — that
  is reached by `CreateIssue` and manual board refresh, which must never spawn runs).
  `syncRepo` calls the detector after the issue sync each tick, as a sibling of the
  PRD #24 MR-close watcher (§91), so it reads the freshest issue cache. Candidates come
  from a cache query (`ListAutopilotCandidateIssues`: open issues carrying the
  autopilot label).
- **Transition-once, event-id dedup, in a dedicated ledger** (Decision 5). The `issues`
  cache is evictable (§22), so "never re-trigger / never re-comment" state cannot live
  there. `autopilot_triggers(repo_id, issue_iid, last_event_id, handled_at)` (PK
  `(repo_id, issue_iid)`) records the last handled label-event id and survives cache
  eviction. `detectOne` skips when `stored_last_event_id >= current_add_event_id`:
  GitLab resource-label-event ids are a **global monotonic sequence**, so a larger id
  is strictly a later application. A remove+re-add mints a larger id → re-handled — the
  **only** retry path (failed runs never auto-retry; remove+re-add is the deliberate
  human retry gesture, and the natural GitLab one).
- **Latest-wins add resolution**: events are read oldest-first; the detector keeps the
  *last* event touching the label and acts only if its action is `add`. If the latest
  touch is a `remove` while the cache still lists the label (a sync race), it returns
  nil → clean skip (no run, no comment, **no record**), self-healing next sync.
- **Active-run pre-check before eligibility** (`HasActiveRunForIssue`): an active run →
  record the event and return, **swallowing silently** (no comment). This stops an
  active *manual* run from drawing a spurious "no eligible user" autopilot comment, and
  consumes the event so a re-add during a run never queues a post-run re-run (Decision
  5: no queued re-runs in v1; removing the label mid-run does not cancel).
- **Eligibility collapses to the repo owner**: the connection context is keyed by the
  repo's `connection_id`, and a repo is connected by exactly one user, so the only user
  who can satisfy "repo connected by that user" is the owner. Attribution therefore
  matches the owner's `human_username` against **adder-first, issue-author-fallback**
  (Decision 3) — both resolve to the same owner, so the ordering is preserved but does
  not change the outcome. The owner must be opted in and hold an Anthropic token.
- **Two ordering disciplines, one per outcome** (Decision 6):
  - **create-then-record** (a run starts): create the run, *then* record the event id.
    A crash between leaves the run active, so the next tick's active-run pre-check
    swallows the re-detection and records — never a double run.
  - **record-then-comment** (no eligible user / no PRD link / description too large):
    record the event id *first*, post the one explanatory comment only if the record
    persisted. A crash between loses one comment, never double-posts. These comments
    are fixed em-dash-free templates that spell out the remove+re-add retry gesture.
- **Closed-issue exclusion is an intentional asymmetry** (Decision, reviewer M4): the
  candidate query filters `state = 'opened'`, so a closed issue is never (re)detected
  even if relabeled — a stricter gate than the manual **Start run** path, justified
  because autopilot spends a user's tokens unattended.
- **Multi-owner-same-project behavior** (documented, carry-item): two users connecting
  the same forge project = two repo rows; one label add can produce a run per eligible
  owner and a factually-wrong "no eligible user" comment from the other connection.
  Consent never crosses (each connection spends only its own owner's tokens); this
  mirrors uzi's existing manual multi-owner model. Documented; if it bites, dedup
  comments or widen the active-run check by forge project id.
- **`MaxIssueDescriptionBytes` unified** (validator M4 follow-up): the description cap
  (256 KiB) is a single exported `workersvc.MaxIssueDescriptionBytes`, enforced as the
  first statement inside the shared `createRun`, so the manual path (→ 422) and the
  autopilot path (→ oversize routes to a `too large` comment) hit the identical cap in
  one place instead of two mirrored consts.

## 99. Autopilot execution: server-authoritative auto-approve + terminal comments

Serves human: "the run happens with zero uzi interaction; the outcome comes back as one
issue comment (MR link on success)" (user, 2026-07-05); primary directive unchanged.

- **`auto_approve` is a server-authoritative, run-scoped claim flag.** `runs.auto_approve
  BOOLEAN NOT NULL DEFAULT false` is set only by the autopilot trigger; the API delivers
  it **top-level in `ClaimPayload`** (next to `Status`/`Branch`), read from the runs row
  — deliberately **not** in `ClaimConfig`, which is documented as worker-enforced caps
  from instance params, not from the run. The worker cannot set or spoof it. **Resume
  invariant** (structural, not a special path): `assembleClaim` re-reads
  `run.AutoApprove` on every claim, so a requeued/resumed autopilot run re-delivers
  `auto_approve = true` — otherwise an unattended resume would hang at the plan gate
  forever.
- **Auto-approve is a verdict source at the existing gate, not a bypass.** When the
  claim carries the flag, the worker still emits the `kind:"plan"` run_message (the plan
  stays in the audit trail, Decision 2) and **flushes it** before deciding; `gatePlan`
  then resolves immediately with an approve verdict. The run **never enters
  `awaiting_approval`** (no state flicker, no column-automation churn) and never waits on
  `/inputs`. The manual path and the executor are unchanged — the gate is still
  worker-enforced.
- **Terminal comments post from one run-lifecycle hook** (Decision 6). A run reaches
  terminal-failed via two mutually exclusive paths (worker `SetState('failed')` → inline
  notify → `notifyOnce`; the sweeper's bulk terminal write → the reconcile loop →
  `reconcileOne`), so both funnel through a single `maybeTerminalComment` — never
  duplicated per call site. Only `completed` (success + MR link) and `failed` comment;
  `cancelled` is a human stop, not an autopilot outcome. The comment rides the row both
  lifecycle observers already load, so no second query.
- **Per-run comment marker, claimed atomically** (Decision 6 split, validator-affirmed
  *more* correct than the PRD's literal `autopilot_triggers`). Terminal-comment dedup
  lives on `runs.autopilot_commented_at`, claimed by `ClaimAutopilotTerminalComment`
  (`:execrows`, `WHERE id=@id AND auto_approve = true AND autopilot_commented_at IS
  NULL`). The `WHERE` does double duty: a manual run is never claimed, and of racing
  invocations exactly one gets `rows == 1`. Record-then-comment (claim first, post only
  on `rows > 0`): a failed forge post after a successful claim loses ≤ 1 comment, never
  double-posts. The marker is **per-run**, not per-issue, deliberately: a retry
  (remove+re-add → new run, same issue) posts its own outcome comment — a per-issue
  marker would suppress it.
- **Template-only comments, no `failure_reason` interpolation** (Decision 6, reviewer
  M5, security-positive). The failure comment is a **fixed template plus a run link
  only**. `failure_reason` can be worker-supplied free text and only the forge-driver
  error path goes through the PAT-scrubbing redactor, so echoing it into a member-visible
  GitLab comment would be an unredacted info-leak / injection surface; the
  access-controlled run page carries the detail. Both comment bodies interpolate only
  trusted values (run uuid, `mr_iid`, repo `web_url`, config `FrontendOrigin`) — never
  issue title/description or `failure_reason`. The run link is appended only when
  `FrontendOrigin` is set (a bare unroutable path is worse than none; the compose default
  sets it). This wording (run link, not failure reason) was **approved by the user
  (2026-07-05)** and is the recorded `specs/human.md` Feature #19 success criterion.

## 100. Autopilot web surface + docs

Serves human Feature #19; minimal new surface.

- **Web**: a per-connection "Your forge identity (for autopilot)" username field on the
  Forge settings page (verified-or-warned save, §97); an "Autopilot" per-user opt-in card
  on Settings with the honest Decision-7 copy; an "autopilot" badge in the run views
  (`RunView`/`RunsList`) reading `run.auto_approve`. The admin "Instance settings" page
  (§93) carries the two label fields with client-side mirror validation (server stays the
  source of truth).
- **Docs**: `docs/admin-settings.md` (the two labels, validation rules, resync-on-change
  board effect, "create labels in GitLab yourself", no secrets in settings) and
  `docs/autopilot.md` (label workflow, verified-or-warned case-sensitive mapping with the
  identity-squat note, per-user opt-in with the honest consent framing, retry-by-re-add
  gesture, what the fixed success/failure comments look like, closed-issue exclusion,
  multi-owner-same-project behavior, and the removing-label-mid-run / re-add-while-active
  semantics).

---

# PRD #5 — Access Control & PAT Least-Privilege Hardening

Serves human Feature #5 (registration domain allowlist + registration toggle + PAT
least-privilege verification; user chose this scope — option A — precisely so it runs
parallel to PRD #4's M3–M7 with no file overlap). Two thin, same-theme workstreams:
tighten who registers, and verify the bot PAT can do no more than open MRs — making
PRD #4's "GitLab-side bot = Developer + protected main" guardrail (§50) *checked*
instead of hoped. Section numbers continue past PRD #24's #92. Realizes
`prds/5-access-control-pat-hardening.md`.

## 93. Registration controls (server)

Serves human: "allow registration only from configurable email domains"; "enable/disable registration".

- **Env-var config, not a DB settings table** (`UZI_REGISTRATION_ENABLED` bool
  default `true`; `UZI_ALLOWED_EMAIL_DOMAINS` comma list, empty = allow all,
  reproducing today's behavior bit-for-bit). Why: matches every existing knob
  (§13/§25); the compose MVP has one operator; a DB/admin-UI settings surface is
  deferred with the SSO/KC work.
- **Kill-switch is a security control ⇒ a set-but-malformed value aborts boot** (loud
  misconfiguration, same stance as the seed guards) — unlike the mechanical tuning
  knobs that silently default. `UZI_ALLOWED_EMAIL_DOMAINS` is lowercased,
  **exact-match only** (no subdomain wildcards: `a.example.com` ≠ `example.com`),
  IDN matched byte-wise (no IDNA folding — irrelevant for example.com, noted for
  completeness).
- **Enforcement lives only in `Register` (`handler/auth.go`), never a shared helper
  the seed path could inherit**, after the existing `mail.ParseAddress`, before any
  DB work. Both policy rejections (disabled, domain-not-allowed) return **403** with
  distinct messages — the request is well-formed, the policy forbids it; **400 stays
  for malformed input**. Domain-list disclosure in the message is acceptable for an
  internal tool (the register page hints the same list anyway).
- **Domain extracted from the parsed addr-spec** (`mail.ParseAddress(...).Address`,
  final-`@` split), never the raw input — `mail.ParseAddress` also accepts
  display-name/comment forms (`Alice <alice@example.com>`) whose raw final-`@`
  suffix is junk; the stored email is **canonicalized to `addr.Address`** (the
  handler previously stored the raw string).
- **Seed admin exempt from the allowlist**: `seedAdmin()` (`api/cmd/server/main.go`)
  calls `CreateUser` directly, never the handler. The operator sets both the seed
  email and the allowlist, so gating one on the other would only create bootstrap
  deadlocks (config-lockout guard).
- **`GET /api/auth/config`** → `{registration_enabled, allowed_email_domains}` —
  uzi's **first unauthenticated JSON surface besides `/health`** (the authed
  `ForgeConfig`, §26, is not a precedent). Registered **outside `RequireAuth`**,
  behind the **auth rate limiter** like register/login. Its shape is a security
  boundary: only operator-set, user-visible policy — nothing else, ever.

## 94. Registration UX (web)

Serves human Feature #5 (registration controls, user-facing).

- The register page consumes `/auth/config`: registration disabled → the register
  form/route is replaced by a "registration is disabled" notice (**login flow
  untouched**); domains restricted → a hint under the email field + client-side
  pre-validation. The **server stays authoritative** (client checks are UX only); a
  server rejection renders its message inline.

## 95. PAT least-privilege verification — forge interface + rules

Serves human: "can uzi verify the glpat does not have more permissions than needed
for an MR — per repo, at save, and afterwards?" (plan.md line 48); primary directive
(agents must not modify main).

- **Three neutral-domain `Forge` methods** (GitLab driver only, same discipline as
  the existing methods; Forgejo still deferred, §16): `TokenInfo`
  (`GET /personal_access_tokens/self` — scopes/active/expiry), `ProjectRole`
  (`GET /projects/:id/members/all/:user_id` — **effective** direct-or-inherited
  access level, 404 = not a member), `DefaultBranchProtection`
  (`GET /projects/:id/protected_branches/:name`, 404 = unprotected). The admin flag
  needs **no new method**: `BotIdentity.IsAdmin` rides on the `GET /user` that
  `VerifyToken` already makes (no second round-trip). All pass the existing
  PAT-scrubbing redactor; reports carry scopes/roles/branch names but never token
  material.
- **Rules** (`PrivilegeReport`): scopes must equal exactly `{api}` (uzi's documented
  minimum — `docs/gitlab-bot-setup.md`; more = over-privilege violation, fewer
  already breaks connect); `active == true` and not expired (expiry within **14 days
  = warning**, not violation); `is_admin == false` (absent field = non-admin = pass,
  because GitLab emits `is_admin` only for admin callers). Per enabled repo:
  effective role **exactly Developer(30)** (>30 = violation; <30 or **not-a-member
  404** = explicit finding, since repos aren't auto-disabled on downgrade); default
  branch **protected and not Developer(30)-pushable** — else Developer-role
  enforcement is vacuous. **Direct per-user bot push grants** on the protected branch
  are detected; **group-inherited push grants are NOT detected** (documented
  limitation, deliberately not implemented).
- **Two-tier model** (false-positive fatigue guard): *violations* are only things
  that break the primary directive; everything advisory is a *warning*, visually
  distinct.
- **Introspection unsupported (GitLab <15.5, 404 on `/self`) = warning, never a hard
  save-block** — older-instance tolerance; the only allowlisted forge
  (gitlab.example.com) is ≥15.5.

## 96. privcheck package: enforcement, persistence, periodic sweep

Serves human Feature #5 ("at save, and afterwards").

- **New `api/internal/privcheck` package** (keeps `forgesvc` sync-only): a `checker`
  computes the report; `service`/`sweep` run and persist it. Report
  `status = ok | warnings | violations` (plus an `error`/"check failed" state when the
  forge call itself fails — a revoked token is exactly what the report must surface,
  not a crash).
- **Block-at-save vs warn-after tiering**: at `CreateConnection`, after
  `VerifyToken`, token-level checks run against the plaintext; any token-level
  **violation ⇒ 422, nothing stored** (the one moment uzi holds the plaintext and the
  user is present to fix it). Per-repo findings **only warn** — membership changes
  happen on the forge after save, repos are enabled/disabled over time, and blocking
  issue-sync over a role finding would punish the wrong action.
- **On-demand `POST /api/forge/connections/{id}/privilege-check`** — owner-only,
  behind the per-user forge rate limiter (heaviest forge route: 1 + 2×repos upstream
  calls); runs the full report (token + all enabled repos), persists, returns it.
- **Periodic sweep** modeled on the **worker sweeper (§41), not the poller**: an
  **async boot pass runs inside the sweep goroutine** at API start so
  grandfathered/never-checked connections surface within seconds of deploy (not one
  interval later), then it ticks on `UZI_PRIVILEGE_CHECK_INTERVAL` (default `24h`).
  Per-repo fan-out uses **bounded concurrency 4** (poller discipline, polite to the
  forge). **`0` disables the sweep AND the boot back-fill**; a malformed value
  silently defaults (`parseNonNegDuration`, tuning-knob stance — `0` is a legitimate
  value so `parseDuration`'s reject-0 is unusable). Failures are recorded *in the
  report*; a connection/repo deleted mid-sweep is a tolerated 0-row write-back.
  Single-instance assumption (no leader election — the shared gap with poller/sweeper,
  deferred to the k8s work).
- **Persistence: jsonb report on `forge_connections`** (migration **`00030`**,
  merge-time numbering respected per the CLAUDE.md convention): `privilege_report
  jsonb`, `privilege_checked_at`, `privilege_status` (denormalized for cheap
  list/badge queries). One report per connection with repo findings embedded — **no
  normalized findings table, no history** (current-state display only). All nullable:
  **`NULL` status = never checked**, rendered as an explicit "unchecked" badge, never
  as ✓; the boot sweep back-fills it right after deploy.

## 97. Web privilege surfacing

Serves human Feature #5 (drift becomes visible without anyone asking).

- Settings → Forge: a per-connection badge (**least-privilege ✓ / N warnings / N
  violations / unchecked / check failed**, with `checked_at`), expandable to the
  finding list, plus a "Check privileges" button beside the existing "Verify". Repos
  page: a per-repo badge on repos with role/branch-protection findings, tooltip with
  the specific violation. A 422 save rejection renders the violations as the error
  with a link to `docs/gitlab-bot-setup.md`. Admin fleet-wide privilege dashboard is
  out of scope.

## 98. Configuration additions (env, extends §13/§25)

| Var | Default | Notes |
|---|---|---|
| `UZI_REGISTRATION_ENABLED` | `true` | registration kill-switch; **malformed value aborts boot** |
| `UZI_ALLOWED_EMAIL_DOMAINS` | — (empty = all) | comma list, lowercased exact-match, no wildcards; seed admin exempt |
| `UZI_PRIVILEGE_CHECK_INTERVAL` | `24h` | periodic PAT re-check; **`0` disables sweep + boot back-fill**; malformed → silent default |

---

# PRD #17 — Builtin lead template (opus) + worker model selection from the UI

Serves human Feature #17 (lead orchestrator as an editable builtin on opus;
per-user default worker model; user, 2026-07-05). Before this PRD the lead /
main-thread was the only agent role with no template: `agent_templates` held the
seven subagents, the worker found no `lead` row, left `baseOptions.model` unset,
and the SDK silently fell back to the account default (observed: `claude-sonnet-5`)
— invisible and unconfigurable. This PRD ships `lead` as the **eighth** builtin on
`opus`, makes it editable/resettable like the others, and adds a per-user
default-model knob that overrides it for a user's own runs. Section numbers
continue past PRD #24's #92; the decisions below realize the PRD's Design Decisions
(`prds/17-lead-template-and-model-selection.md`), whose per-decision attributions
carry provenance. Builds on PRD #3 (agent templates) and PRD #4 (runtime/claim).

## 93. Decouple builtins from `.claude/agents/`; lead is the eighth builtin

Serves human: "lead ships as a builtin with a real orchestrator prompt on opus";
"builtins are the single source, `.claude/agents/` is the dev team's" (user,
2026-07-05, superseding the same-day dual-home choice).

- **`api/internal/agenttmpl/builtins/` is now the single source of truth** for
  product templates (git-versioned, `go:embed`-shipped, boot-seeded). The earlier
  1:1 mirror to `.claude/agents/*.md` is dissolved: `.claude/agents/` becomes purely
  the repo's own Claude Code dev agent-team and is free to drift (it already carried
  a `web-ux` role with no builtin twin). Rationale: a product `lead.md` under
  `.claude/agents/` would masquerade as a spawnable dev teammate, and the dual-home
  shipped nothing at runtime (only the embedded copy is ever used).
- **Ordered before PRD #16 so #16 inherits the decoupled convention** (user). The
  decouple is a small, self-contained first commit (PRD Risk: decouple-first).
- **Tests: golden byte-match → parse/validity + round-trip.** The two tests pinning
  builtins to the checked-in `.claude/agents/*.md` files (`TestRenderBuiltinsByteMatch`,
  `TestEmbeddedCopiesMatchRepo`) are removed. Replacements: `TestBuiltinsParseAndValid`
  (each embedded file parses; name/description non-empty; names unique; model passes
  `ValidateModel`), `TestBuiltinsRoundTripRender` (parse→`Render` is byte-for-byte
  stable), and `TestBuiltinsSetIsExactlyEight` (grown from Seven; `builtinNames` slice
  now eight). `TestCoderInheritsAllTools` kept. Stale "mirrors `.claude/agents/`"
  comments in `builtins.go`/`render.go` swept.
- **`lead.md`**: `name: lead`, `model: opus`, **no `tools:` line** (null-tools =
  inherit-all, the `coder` contract — the lead needs the full toolset). Body is a
  **role-agnostic** orchestrator persona (plan-first, approval gate, `signal_done`,
  never touch `main`); it deliberately does **not** hard-list the seven role names —
  the invokable subagent set is injected per turn by the worker's `delegatesLine`
  (`agent/src/prompt.ts`). The shipped name matches the worker's `LEAD_NAME_RE`
  (`/^(lead|orchestrator)$/i`), asserted by an agent unit test, so `assembleAgents`
  routes its `prompt_body`/`model` to the main thread rather than registering a
  subagent — no worker change was needed for pickup.
- **Upgrade seeding is automatic**: the idempotent boot reconciler
  (`ReconcileBuiltinTemplates`, `ON CONFLICT (name) DO NOTHING`) inserts the lead into
  existing DBs and preserves edited rows; `ResetAgentTemplate` works for it like any
  builtin.
- **Collision warning (AI):** when a builtin insert is skipped (`n==0`) the reconciler
  reads the shadowing row back and warns **only when it is `is_builtin=false`** — an
  admin's own custom `lead`/`orchestrator` blocks the builtin and can't be reset (the
  worker still routes it by name; an operator can rename/delete it to adopt the
  builtin). A failed read-back logs at **debug** rather than being silently dropped.
  The reconciler's query dependency is narrowed to a `builtinReconcilerQueries`
  interface so the collision path is unit-testable without a live DB.

## 94. Lead prompt body augments, never replaces, the guardrails

Serves human Feature #17 + the primary directive (`main` is never touched).

- The worker composes the lead system prompt as **claude_code preset + template
  `prompt_body` + `LEAD_GUARDRAIL_APPEND`** (`agent/src/prompt.ts`). The template body
  is persona / workflow guidance only; the primary-directive guardrails are appended
  by the worker regardless of what the template says, so **editing the lead template
  cannot weaken guardrails** (asserted by existing + new tests). The four independent
  guardrail layers (§45, §50) are untouched.

## 95. Shared model validator homed in `agenttmpl` (single rule source)

Serves human: "model choices offered as aliases + a custom escape hatch" (user);
best-practice (two surfaces must not drift). AI-decided homing.

- **`agenttmpl.ValidateModel` is the single server-side rule source**
  (`api/internal/agenttmpl/model.go`), placed in this neutral, dependency-free package
  so both the template-editor handler and the per-user default-model endpoint
  thin-wrap it without an import cycle (the handler maps its result to
  `pgtype.Text` + an HTTP error); the builtin validity tests call it directly. The
  prior template-model check (allowed interior spaces, no length cap) is replaced by
  it. The web `ModelSelect` mirror (§96) is a **client hint only** — the server rule
  is authoritative.
- **Decision-4 rules**: blank / whitespace-only ⇒ `("", nil)` = inherit (the caller
  stores NULL); a non-blank value must be a **single token** — trimmed, no interior
  whitespace, no control chars / U+FFFD, **≤ 100 bytes** (`MaxModelLen`, kept in
  lockstep with the web `MAX_MODEL_LEN`) — returned trimmed. A typo in a custom ID is
  accepted here and only surfaces as a run-time SDK error (the API cannot enumerate
  valid IDs without calling Anthropic).

## 96. Shared `ModelSelect` control (web)

Serves human: aliases + custom escape hatch (user). AI-decided component shape.

- **`web/src/components/ModelSelect.tsx`**: a select over `inherit` (empty), the
  `MODEL_ALIASES` (`opus`, `sonnet`, `haiku`, `fable`) and `custom` (reveals a
  free-text input for a full model ID). Used by both `AgentTemplateEditor` (replacing
  the datalist-backed input) and the new Settings section. **`MODEL_ALIASES` is the
  single source** of the curated set.
- **Custom-init rule**: an incoming value not in the alias list (e.g.
  `claude-fable-5`) initializes into the `custom` state with the text prefilled —
  never silently reset to inherit. An intentionally-emptied custom field is not
  re-derived back to the incoming value.
- **Submit gating preserved**: the custom value keeps flowing through
  `frontmatterFieldWarning`, so an injection-suspect model string still disables submit
  in the template editor.
- **Lead badged "orchestrator"** on the Agents page (a `brand`-tone badge, tooltip
  "the main agent thread that plans and delegates") so it reads differently from
  invokable subagents; otherwise it edits and resets like any other builtin.

## 97. Per-user default worker model (api + web)

Serves human: "per-user default worker model" (user, 2026-07-05 — the default follows
per-user run ownership, not a global setting).

- **`users.default_model text` (nullable; NULL = inherit)** — migration
  `00030_user_default_model.sql` (drafted `00022`; renumbered to the next free slot
  above the live head per the CLAUDE.md goose convention), with a `+goose Down`. One
  scalar per user ⇒ a column on `users`, not a new table. (`SELECT *` / `RETURNING *`
  on `users` regenerate the sqlc `User` struct — expected, harmless.)
- **Companion `GetUserDefaultModel` + `UpdateUserDefaultModel` sqlc queries.** The
  owner default is read at claim time via the companion `GetUserDefaultModel` (a
  targeted lookup, **not** a JOIN widening of the claim-context query).
- **API `GET` / `PUT /api/me/settings`**, session-authenticated (PRD #1 cookie +
  CSRF), **own-user only** — no admin path to another user's value. `PUT` validates
  through `ValidateModel` (§95) and stores `""` as NULL.
- **Web**: a "Worker model" section on `Settings.tsx` under the Anthropic-token block,
  using `ModelSelect`, explaining precedence (the per-user default overrides the lead
  template's model; empty = inherit the lead template's model, opus by default).

## 98. Claim plumbing + model precedence (api + agent)

Serves human: "the run owner's default model wins over the lead template model"
(Decision 6, review finding + user, 2026-07-05).

- **`ClaimConfig.default_model *string` with `omitempty`**
  (`api/internal/workersvc/claim.go`) — omitted on the wire when the owner's default is
  NULL (absent ≠ explicit null), populated from `GetUserDefaultModel` at claim time.
- **Agent**: `protocol.ts` `ClaimConfig` gains `default_model`; the runner threads it
  into `RunContext.config`; `sdk-executor.ts` resolves it through the pure helper
  **`resolveLeadModel(configModel, templateModel) = (configModel ?? templateModel) || undefined`**
  and applies it **set-only-when-defined** (`if (leadModel) baseOptions.model = leadModel`)
  — an unset model **omits** the SDK `model` key rather than sending an explicit empty
  override, so it falls back to the SDK / account default.
- **Precedence (lead / main thread)**: run owner's `default_model` → lead template
  `model` → SDK / account default. **Why user-default-wins** (flipped from the earlier
  template-wins draft): the builtin lead pins `opus`, so under template-wins the
  per-user setting would be inert on a default install (only active after an admin
  globally cleared the lead model). User-wins keeps both features live — `opus` is the
  instance-wide default for every user with the setting unset (NULL), and a user who
  picks a model overrides it for their own runs only. Null-model **subagents follow
  the main thread**, so this governs them too; subagent templates carrying an explicit
  `model` are unaffected.

---

# PRD #16 — Agent Skills (builtin / global / user / repo) + first CICD skill

Serves human Feature #16 (skills at global + per-user scope, allocatable to each
agent; first builtin skill = ci-cd-norms; repo-borne skills the worker detects;
builtin skills editable/resettable). Skills are SDK-native named Markdown playbooks:
only `name`+`description` sit in context always, the body loads on demand
(progressive disclosure). Section numbers continue past PRD #17's #98. Builds on
PRD #3 (agent-template store + reconciler), PRD #4 (claim payload, SDK executor,
guardrail layers), and PRD #17 (decoupled-builtins convention, `lead` as an
existing builtin routed to the main thread). Full rationale in
`prds/16-agent-skills.md` (Decision Log); user-facing guide in `docs/skills.md`;
cross-service map in ARCHITECTURE.md "Agent skills".

## 99. Skill scopes + storage schema (`skills`, `agent_skill_allocations`)

Serves human: "skills at global scope and per-user scope; allocate global or user
skills to each agent"; "repos may carry skills the worker detects".

- **Three server scopes on one `skills` table** (`scope ∈ builtin|global|user`,
  CHECK-constrained) plus a fourth **repo source** resolved worker-side (not a DB
  row). Migration drafted `00050_skills.sql` (**renumber-at-merge** to the next free
  slot above the live head per the CLAUDE.md goose convention; live head is
  `00031` at time of writing, so it lands ~`00032`). One table, not three, because
  scope is a discriminator column, not a shape difference.
- **Columns**: `name` (kebab-case `^[a-z0-9][a-z0-9-]{0,63}$`, **immutable after
  creation** — it is the skill identity + allocation key), `description` (one line,
  always in context — what the model routes on), `body` (SKILL.md markdown **body
  only**; frontmatter is synthesized at delivery, never stored — so a stored skill
  can't carry capability-granting frontmatter keys), `user_id` (→users ON DELETE
  CASCADE; `CHECK ((scope='user') = (user_id IS NOT NULL))` binds ownership to
  scope), `updated_by` (→users ON DELETE SET NULL), timestamps.
- **Two partial-unique name indexes**: `uq_skills_shared_name (name) WHERE scope <>
  'user'` and `uq_skills_user_name (user_id, name) WHERE scope = 'user'`. Consequence
  (**intended, not a bug**): a builtin and a global can never share a name; a user
  may own a name that collides with a shared one (resolved by precedence at
  assembly, §102).
- **`agent_skill_allocations`** — the overlay model: `template_id` (→agent_templates
  ON DELETE CASCADE), `skill_id` (→skills ON DELETE CASCADE), `user_id` (**NULL =
  shared/admin-managed; non-NULL = that user's private overlay**). **No surrogate
  PK**; row identity is `uq_allocations (template_id, skill_id, COALESCE(user_id,
  '0000…'))`, so a shared allocation and a user's overlay of the same skill to the
  same template coexist as distinct rows.
  - Why the overlay shape (over multica's flat `agent_skill`): uzi runs belong to
    users with private skills, so allocation must split shared (everyone) from
    per-user overlay; multica is workspace-flat and has no such split.
- **`repos.repo_skills_enabled BOOLEAN NOT NULL DEFAULT false`** added in the same
  migration — the per-repo opt-in flag for repo-borne skills (§105).

## 100. Builtin skills: `skilltmpl` reconciler, no `.claude/skills/` mirror

Serves human: "builtin skills ship with uzi, editable and resettable like builtin
agent templates".

- **Builtin-ness is a `scope` value**, not an `is_builtin` bool (the deliberate
  divergence from `agent_templates`): the reconciler keys on `(name, scope='builtin')`.
- **`api/internal/skilltmpl/`** mirrors PRD #3's `agenttmpl`: builtin SKILL.md files
  Go-embedded from `skilltmpl/builtins/<name>/SKILL.md` as the **single home**, parsed
  at init. **Adopts PRD #17's decoupled convention from day one**: **no `.claude/skills/`
  copy** (a product skill there would load into the dev team's own Claude Code sessions
  — the masquerade #17 evicted `lead.md` for) and **no golden byte-match test**.
  Instead **parse/validity tests** over the embedded files (frontmatter parses, name
  matches the regex, description is a non-empty single line, names unique).
- **Startup reconciler** with the same semantics as agent templates: idempotent
  `ON CONFLICT DO NOTHING` insert of missing builtins, **never overwrites** an edited
  row. Builtin lifecycle: **editable; resettable** (`POST /:id/reset` re-applies the
  embedded definition, builtins only); **never deletable** (409). Guarantees the core
  skills always exist.

## 101. Read + allocation-read authz (viewer-scoped, NOT the templates all-shared read)

Serves human: per-user private skills (a user's own skills are his); best-practice
(no cross-user leak).

- **This is explicitly NOT the agent-templates pattern.** Agent templates are all
  all-shared reads; copying that handler verbatim would leak every user's private
  skills. Instead **every skill read returns builtin ∪ global ∪ the caller's own user
  skills** — nothing else. **Admins additionally see all users' private skills** (they
  administer the system and can read the DB anyway — honest, not a hidden leak; surfaced
  in the UI as a labelled "Other users" group, §108).
- **Allocation reads follow the same rule**: `GET` on a template's skills returns the
  shared rows plus **the caller's own overlay rows only** — never another user's overlay
  rows nor the skill names behind them. Pinned by authz tests (a user's private skill
  must never appear in another user's run, listing, or allocation view).

## 102. Allocation write rules + name-collision precedence

Serves human: "allocate global or user skills to each agent"; shared vs per-user overlay.

- **`PUT /api/agent-templates/{id}/skills`** — replace-set semantics, split into a
  **shared half (admin-only)** and a **mine half (any authenticated user)**. Server-enforced
  reference rules: **shared rows may reference only builtin/global skills**; **user
  overlay rows may reference builtin/global or the owner's own user skills**. A user's
  runs get the **union** of shared ∪ their overlay for each template.
- **Name-collision precedence: user > global > builtin > repo.** When two skills in a
  run's assembled set share a `name`, the highest-precedence one's body wins and the
  **shadowed skill is dropped entirely from the run** (one body per name ever loads —
  run-wide shadowing, not a per-template override). Repo skills always rank last
  (§105).

## 103. Claim assembly: union / dedup / precedence / caps (`api/internal/workersvc`)

Serves human: skills delivered to the agent for a run; caps.

- For the claiming run's user, per template: allocated skills = shared rows ∪ that
  user's overlay rows. `ClaimPayload.skills` is the **deduplicated union across all
  templates** (`{name, description, body}`); each `ClaimAgent.skills` is that template's
  allocated **names**; `ClaimRepo.skills_enabled` mirrors the repo flag;
  `ClaimConfig.skill_max_bytes` / `skills_max_per_run` carry the server's configured
  caps so the **worker enforces the same limits with no hardcoded drift**.
- **`SKILLS_MAX_PER_RUN` is enforced here, at assembly** (not at save time): every
  template ships in every claim, so the per-run union spans all templates and no single
  allocation save can see the whole picture. Overflow is **dropped lowest-precedence-first**.
- **Assembly-time drops (shadowed + over-limit) ride the claim as `ClaimPayload.skills_dropped`**
  (`{name, reason}`, reason ∈ `shadowed` | `over_limit`) — **the server never writes
  `run_messages`**; the **worker** emits the corresponding run-message log lines because
  the worker owns the gapless per-run `seq` (ARCHITECTURE.md invariant).
- **Skills re-assemble on every claim including resume.** A skill deleted between claim
  and resume disappears from the resumed session even if the approved plan referenced it
  — **accepted, one log line** (not a hard failure).
- Both wire directions covered by a **cross-side contract test** (the PRD #4 M1/M2
  lenient-fakes-drifted lesson), not two independent lenient fakes.

## 104. Worker delivery: local plugin dir outside the clone, explicit `skills` list

Serves human: worker delivers skills to the agent; primary directive (the injection
defense — `settingSources: []` — never loosens).

- **Delivery channel = a synthesized local SDK plugin dir materialized OUTSIDE the
  clone** (sibling of the worktree, never inside it): `.claude-plugin/plugin.json`
  (`{"name":"uzi"}`) + `skills/<name>/SKILL.md` per surviving skill. Chosen because
  `settingSources: []` blocks all filesystem skill discovery (`~/.claude/skills`,
  `<cwd>/.claude/skills`) — the repo-injection defense — and **must stay off**. multica's
  native-discovery materialization into the workdir was **rejected** precisely because it
  requires project-settings loading ON; `plugins: [{type:'local', path}]` loads skills
  independent of `settingSources`, so the isolation knob is never touched. The dir is
  **rebuilt from the claim on every claim, including resume** (plugins/skills are
  re-applied to a resumed session, not baked into the original).
- **Frontmatter synthesized as quoted, fully-escaped double-quoted YAML scalars** —
  not merely newline-stripped. Escaped: YAML metacharacters (`:`,`#`,`|`,`>`,`&`,`*`,`!`,
  leading spaces, `---`), **all C0 control chars (<0x20), DEL+C1 (0x7f–0x9f)**, and the
  **Unicode line separators U+2028/U+2029** (some YAML parsers treat them as breaks). The
  body sits below the frontmatter and can never redefine it. Full metacharacter matrix is
  test-pinned (frontmatter-injection guard, an audit focus).
- **Top-level `skills` option is ALWAYS an explicit list** — never omitted (omission is
  **not** "skills off": the CLI's own defaults still apply) and never `'all'` (which
  enables every discovered skill). It is set to the run's **full plugin-qualified union**
  (`uzi:<name>` / `plugin:skill` qualifier). Correct under either reading of the SDK's
  ambiguous top-level-vs-subagent gating docs, and it gives the main-thread `lead`
  orchestrator visibility (the `lead` is the main session, routed there by
  `assembleAgents`, so it has no `AgentDefinition.skills` slot — allocating a skill
  specifically to `lead` only affects union membership).
- **Per-subagent scoping via each `AgentDefinition.skills`** (delivered/allocated skills
  are per-template), re-filtered to what actually survived precedence + caps. Repo skills
  are the exception (all-templates, §105).
- Shared assembly path `agent/src/skills-run.ts` is used by **both** the SDK executor
  (production) and the stub executor (E2E) so they can't drift into two lenient
  implementations.

## 105. Repo-borne skills: opt-in, default off, skills-only, lowest precedence

Serves human: "repos may carry skills the worker detects; per-repo opt-in, default off".

- **Only when `ClaimRepo.skills_enabled`** (the repo owner or an admin flipped
  `repos.repo_skills_enabled`): after checkout the worker enumerates
  `<clone>/.claude/skills/*/SKILL.md`, parses **only `name` + `description`** from the
  frontmatter, and **drops every other frontmatter key** (`allowed-tools` and friends
  **grant capabilities** — stripping them is the security point, and it matches how
  server-stored skills carry body-only). It re-synthesizes escaped frontmatter with the
  same materializer, applies the same name regex + size cap, and places repo skills at
  **lowest precedence** (a delivered skill of the same name always wins; the repo skill
  is skipped + logged).
- **Symlinks are never followed**: the skills dir itself must be a real directory (a
  symlinked dir is skipped), and a symlinked `SKILL.md` is never read (`lstat` +
  `isDirectory`/`isFile` guards) — a repo can't escape its tree via a link.
- **Repo skills carry no allocation, so they apply to ALL templates in the run**
  (appended to every subagent's `AgentDefinition.skills`, not just the lead's — the
  top-level union only covers the main-thread session, so without this a repo skill would
  never reach a subagent).
- **Nothing else under the repo's `.claude/` is ever read for loading**: no hooks,
  no settings, no commands, no `CLAUDE.md`. This is the **only** clone-borne config uzi
  loads, and only through its own controlled channel.
- **Caps re-enforced worker-side over the combined delivered ∪ repo set**: delivered DB
  skills count against `SKILLS_MAX_PER_RUN`; repo skills (lowest precedence) evict first,
  so a run can never reach 2× the cap.
- **Trust rationale**: repo `.claude/` is exactly the config class `settingSources: []`
  exists to block; repo skills are the mildest member but still get trusted-affordance
  framing. They're trustworthy exactly when the repo's MR-review discipline is — which
  uzi can't know for arbitrary repos — so the owner asserts it per repo, default off.
  Loading skills-only through uzi's own channel weakens **no** existing guardrail: a
  hostile repo skill still can't push (the worker holds the PAT), still hits the
  `PreToolUse` deny-hook, and still can't load hooks/settings. Pinned by a hostile-repo
  test (flag off ⇒ zero repo `.claude/` influence; flag on ⇒ skills only).

## 106. SDK skill-loading model (verified against installed SDK; no allowlist widening)

Serves human: primary directive (tools allowlists are a guardrail — not silently widened).

- Verified against the installed `@anthropic-ai/claude-agent-sdk` during the M4 build:
  a tools-`'Skill'` grant is **deprecated** (`sdk.d.ts:44`), and `AgentDefinition.skills`
  is the **single enable switch** for a subagent to expand its allocated skill. The M4
  integration test confirms a **tools-restricted subagent** (reviewer/tester allowlists
  exclude many tools) can be wired to expand its allocated skill **with no allowlist
  change**.
- Consequence: the earlier open contingency ("if skill expansion needs a `Skill` grant,
  the assembler must widen the allowlist") is **resolved as not needed** — **no tools
  allowlist is widened anywhere in the landed code**, including with repo skills enabled.
  The guardrail layers stay exactly as PRD #4/#17 left them.

## 107. Skills configuration (env, extends §13/§25/§49)

| Var | Default | Notes |
|---|---|---|
| `SKILL_MAX_BYTES` | `65536` | per-skill body cap; server checks at save, worker checks repo skills; delivered to worker via `ClaimConfig` |
| `SKILLS_MAX_PER_RUN` | `32` | per-run union cap across all templates; enforced at claim assembly (drop lowest-precedence-first) and re-enforced worker-side over delivered ∪ repo |

Both are server env, delivered to the worker in `ClaimConfig` so the two sides
enforce identical limits (no drift). Admin raises either instance-wide.

## 108. Web UI: Skills page, allocation panel, repo toggle

Serves human: "users allocate global or their own skills to each agent"; skills UI.

- **Skills page (`/skills`)**: groups **Builtin / Global / Mine** (create/edit,
  markdown body editor, name locked after create; builtin rows show Edit + Reset, never
  Delete; admin sees Global create, everyone sees Mine). Admins additionally get a
  labelled, **view-only "Other users" group** listing other users' private skills — the
  honest UI face of the admin-sees-all read (§101): admins can see them, but only the
  owner can edit or delete, and the group is clearly marked as such.
- **Agent template detail (`AgentDetail.tsx`)**: allocation panel with a **shared half
  (admin-editable)** and a **"my skills for this agent" half (self-service)**, rendered as
  the **union the user's runs will actually get**.
- **Repos page**: a per-repo **"Load repo skills" toggle** (repo owner or admin) with
  explicit warning copy stating what it trusts and what it still never loads.
- Responsive + component (vitest) coverage, consistent with the rest of the web app.

## 109. First builtin skill: `ci-cd-norms`

Serves human: "the first builtin skill is ci-cd-norms, researched from internal-kb
and the example-app repos, covering the example CI/CD norm and example-app as the worked exception".

- Authored at `api/internal/skilltmpl/builtins/ci-cd-norms/SKILL.md` (researched from
  internal-kb `shared/infrastructure/ci-pipeline.md`, `organizations/myorg/infrastructure/
  deployments.md`, and the example-app + `argo-apps` repos). Structure: the **default
  norm** (thin `.gitlab-ci.yml` including a bundle from private `myorg/pipelines`;
  lint→build→audit→push→cleanup with `SKIP_*` toggles; Harbor `harbor.example.com` for
  images + OCI charts; **CI never deploys** — ArgoCD app-of-apps in `myorg/k8s/argo-apps`
  does; secrets via Infisical), an **exception-detection rule** (no `include:` of
  `myorg/pipelines` ⇒ exception; follow its local convention, never "normalize" unasked),
  **example-app as the worked exception** (hand-rolled DAG pipeline, kaniko with
  protected-ref-only cache writes, tag-only 4-artifact publish, chart-in-repo consumed as
  Harbor OCI via multi-source ArgoCD app, manual `targetRevision` release ritual), and a
  **verify-live section** for facts the KB doesn't pin (bundle contents, pinned tool
  versions, push-credential var names).
- **No invented facts**: every claim traces to a internal-kb page or the repos; volatile
  items sit in verify-live. Editable in place by admins (so infra drift is fixed without a
  uzi release).

## 110. Deferred (PRD #16 out of scope)

- Multi-file skills (multica's `skill_file`; v1 is SKILL.md-only); skill import from hubs
  / the Anthropic skills repo; a skills catalog/marketplace; per-run ad-hoc skill
  selection (allocation is per-template); auto-detecting which skills a run *should* have
  had; forgejo parity. Recorded so a rebuild knows these were consciously omitted.

---

# PRD #21 — Mission-control theme (data-theme port of the ops-console identity)

Serves human Feature #21 (a second, user-selectable theme on top of PRD #14's
ember tokens; server-side theme preference with an admin default). PRD #14 shipped
ember as the sole theme with all look-and-feel in CSS variables precisely so
alternate identities could ship later as `data-theme` overrides; this PRD builds
the switching mechanism and ports the mission-control prototype onto ember's token
slots. Section numbers continue past PRD #16's #110. Realizes
`prds/21-mission-control-theme.md` (its Decision Log carries build-time provenance);
builds on PRD #14 (tokens), PRD #17 (`/api/me/settings`), and PRD #19 (`app_settings`).

## 111. Theme = tokens only; the no-branch rule; base-selector hardening

Serves human: "mission-control theme selectable"; PRD #14's token architecture.

- **A theme is CSS-variable values and nothing else.** Every color, radius, font,
  and the backdrop grid is a token in `web/src/index.css`; components reference only
  the Tailwind names those tokens back (`bg-ink`, `text-fg`, `rounded-lg`, …), never
  a variable or raw palette value. **No component file may branch on the active
  theme** — `if (theme === "mission")` exists nowhere. This is the load-bearing rule:
  a theme port that would need a component edit means the underlying feature belongs
  in tokens. Mission's *structural* ideas (status bar, nav regrouping, kicker labels,
  control density, agent-lane hues, gate-pulse ring) are deliberately out of scope.
- **Base-selector hardening**: the default block is
  `:root:not([data-theme]), [data-theme="ember"]` (was `:root, [data-theme="ember"]`),
  so a `[data-theme="mission"]` override wins regardless of source order rather than by
  specificity accident. A theme block is a **complete token set**, not a diff against
  ember — `[data-theme="mission"]` redefines every token the base block defines.
- **`[data-theme]` lives on `<html>`.** PRD #14's grep gate (zero raw palette
  classes / ad-hoc hex/rgb outside `index.css`) stays green under this feature.

## 112. Canonical Go theme registry + resolution chain (server-authoritative)

Serves human: "resolution: user override > admin default > ember"; best-practice
(two write surfaces must not drift).

- **`api/internal/theme`** is the canonical theme list and the `theme.Resolve`
  resolution chain (**user override → admin instance default → `ember`**), computed
  server-side. The web theme module (`web/src/lib/theme.ts`, `resolveTheme`) mirrors
  the same list + chain so the SPA re-derives the identical answer client-side without
  a round trip.
- **Both write surfaces validate against the Go registry** (admin `default_theme` PUT
  and the user `theme` PUT), so a bogus value is rejected at write and can never fall
  back silently at render — same shared-validator discipline PRD #17 uses for models.
- Adding a theme within the existing slot set is **exactly four edits** (Success
  Criterion 5): the Go registry entry (canonical), the web registry entry (mirror),
  one `[data-theme]` CSS block, and one entry in the pre-paint allowlist (§116). No
  handler/component/migration change. A theme needing a new slot is the two-step
  Decision-3 pattern (§117): add the slot theme-agnostically first, then color it.

## 113. Server-side persistence: instance default in #19's `app_settings`, user override on `users`

Serves human: "theme preference is server-side with an admin-set instance default"
(user, 2026-07-05, superseding a device-local draft).

- **Instance default tenants into PRD #19's `app_settings`** (no new table, no new
  settings endpoints — user-approved direction "update 21, 19 is in flight"):
  `default_theme` is one seed row in `settings.Defaults` (`"ember"`), so `Known()`,
  the GET shape, and the fallback all follow. If PRD #21 ever landed before #19 M1,
  M1 here holds — a parallel settings table must not be built.
- **User override is a nullable `users.theme` column** — migration
  `00041_user_theme.sql` (drafted 00040; PRD #16's `00040_skills` landed first, so it
  renumbered to 00041, the next free slot above the live head at land time per the
  CLAUDE.md goose convention). One scalar per user ⇒ a column on `users`, not a table.
  `NULL` = "use default".
- **Per-key validation dispatch** in #19's admin settings handler: the handler ran
  `settings.ValidateLabel()` on every submitted key; this PRD refactors it to
  `settings.Validate(key, value)` (switch: label rules for the label keys,
  theme-registry check for `default_theme`) without touching `ValidateMerged`'s
  cross-key label rule. Absorbed here as a small M1 refactor on #19's surface.
- **A theme-only settings change does NOT `ForceReconcile`.** Only a label change
  re-filters boards; `default_theme` is presentation-only. The dispatch is an
  extracted `settings.LabelChanged` helper (a pure function unit-tested in the always-on
  `go test` gate — a fake-reconciler handler test was declined because `h.pool` is a
  concrete `*pgxpool.Pool` needing a live DB that would skip in the plain gate).

## 114. Three theme fields on `/api/auth/me`; client re-resolves, ignores the server-resolved one

Serves human: the Appearance picker must render "Use default (<name>)"; server-side
resolution.

- **`GET /api/auth/me` carries three theme fields**: the resolved theme, the user's
  raw override (nullable), and the instance-default theme id. Resolved-only is not
  enough (review blocker): with an override active the SPA could render neither
  "Use default (<name>)" nor the picker's selected state, since the default otherwise
  lives only in the admin-only settings endpoint. Non-admins get everything they need
  from `/me`, so no admin read is required to render the picker.
- **The client intentionally ignores the server's resolved field and re-resolves
  itself** (§112 mirror). This is deliberate forward-compat, not dead code: a server
  predating these fields degrades to `ember` rather than throwing on a missing field.
- **Application point is session bootstrap** (`web/src/auth/AuthContext.tsx`, not the
  originally-anticipated `main.tsx`): on every login/`me` refresh it re-resolves and
  stamps `<html data-theme>`, so a change (the user's, or the admin default) restyles
  live with no reload.

## 115. User theme pref extends PRD #17's `/api/me/settings` (PATCH-like); no new route

Serves human: server-side user override; PRD #17 coordination.

- **No new `/me/prefs/*` route.** The pref is shaped exactly like `default_model`, so
  `userSettingsDTO` gains `theme` next to `default_model` on the existing
  `GET/PUT /api/me/settings` ("prefs" already names the localStorage helper
  `web/src/lib/prefs.ts`, so a `/prefs` route would misname).
- **`PUT` is PATCH-like, presence-detected via `json.RawMessage`**: a field present is
  applied (`theme: null` clears the override), a field absent is left unchanged — so
  the worker-model card and the Appearance picker save independently over the one
  endpoint without clobbering each other. `default_model`'s prior behavior is unchanged
  (the client always sends it on a model save). Own-user only; validates the theme
  against the Go registry (§112).

## 116. Pre-paint via an external CSP-safe script + localStorage cache

Serves human: no flash-of-wrong-theme on cold load; best-practice (works in the
nginx-served build, not just dev).

- **`localStorage` (`uzi.theme`) is demoted to a pre-paint cache** of the last resolved
  theme — the server value is authoritative. `web/public/theme-preinit.js` stamps
  `data-theme` from the cache before first paint; `applyTheme()` refreshes the cache
  each time the server-resolved value wins. Stored values are validated against the
  registry (fallback `ember`), storage access is try/caught (private-mode safe); a
  missing/blocked cache leaves `data-theme` unset ⇒ `ember`, never a half-themed page.
- **External file, not an inline `<head>` script** (M1 auditor High): `web/nginx.conf`
  / `web/nginx.mock.conf` set `script-src 'self'` with no `'unsafe-inline'`, so an
  inline script is CSP-blocked and would silently never run in any nginx-served image
  (it only worked under `vite dev`). Chosen over adding a `'sha256-…'` hash to both
  confs plus a drift guard — the hash route is fragile (Vite can minify the inline
  script at build so the source hash wouldn't match served bytes). Keeps "no inline
  scripts" literally true with no CSP change; only the CSP comment is refreshed.
- **The allowlist inside `theme-preinit.js` hardcodes the theme ids** and is a
  deliberate fourth edit point (§112): the file runs before the app bundle, so it
  can't import the web registry. A theme added without this edit still renders once
  `me()` resolves — it just flashes `ember` for one frame on a cold load.

## 117. queue/neutral status tones added theme-agnostically, as fg/border/surface triples

Serves human: "mission's six-tone status language incl. the violet queue tone, added
theme-agnostically" (user decision, 2026-07-05).

- Ember had four status tones (`ok/warn/danger/info`); queued/stopped rendered via
  palette neutrals hardcoded in `runBadge.ts`/`ui.tsx` that token values couldn't
  reach. To carry mission's violet queue **without per-theme component logic**, two
  slots are added to the existing tone maps: **`queue`** (queued) and **`neutral`**
  (idle/stopped). `runBadge`/`RUN_STATUS_TONES` map `queued → "queue"` **once, for all
  themes**; the `runBadge` tests update for the tone-name change only (PRD #14's
  tone-unification discipline). Mission's cyan-run and sky-info both flow through
  `--info`.
- **Ember populates the new slots with its current neutral gray — zero visual change**;
  mission populates them violet. With nothing set anywhere, everything renders ember
  pixel-identical to before, the `--queue`=gray no-op included.
- **Each slot is a border/surface/fg TRIPLE, not a single token** (supersedes the PRD's
  earlier single-token phrasing; M2 reviewer confirmed "required, not
  over-engineering"). A single hue-at-opacity token (the four original tones' pattern)
  can't reproduce ember's solid `border-edge bg-raised text-muted` pill; the triple
  keeps that pill pixel-identical while a theme can retint it, mirroring the mission
  prototype's own `--uzi-status-*-{fg,border,surface}` schema.
- **Tailwind `neutral` caution**: `tailwind.config.js` defines `neutral` as an object
  (`neutral.{fg,border,surface}`) inside Tailwind's default palette, which already ships
  a `neutral-50…950` gray scale. The two merge: `bg-neutral-border` resolves to the
  token, but `bg-neutral-500` stays stock gray. Nothing in `web/src` uses a bare
  `neutral-<number>` today; a future edit reaching for one expecting theme-awareness
  gets untethered gray — use `queue-*` / `neutral-{fg,border,surface}` explicitly.
- **Blueprint grid** ships as background tokens consumed by the existing `body` rule
  (default themes set it to none); any `[data-theme="mission"]` CSS stays inside
  `index.css` next to the token blocks.

## 118. Mock parity + versioned mock persistence

Serves human: "mock demo persists settings across reload" (user approved 2026-07-05,
scoped settings-only); Decision 6 (the `VITE_UZI_MOCK=1` build is the review vehicle).

- Mock mode always logs in as admin, so the admin flow must work. **mockApi implements
  in-memory**: the `theme` field on its existing `getMySettings`/`putMySettings`, the
  three theme fields on `/me`, and **#19's admin settings `default_theme` key** (if #19
  lands a mock first, extend it; else this PRD adds it) — so the full picker flow (user
  override + admin default) is exercised in the demo build without a real backend.
- **Mock settings persist across reload** via a versioned `localStorage` key
  `uzi.mock.v1` (`MOCK_SETTINGS_KEY`), **settings-only**, discard-on-version-mismatch.
  It is **independent of the `uzi.theme` pre-paint cache key** (§116) — different key,
  different purpose (mock backend state vs. real pre-paint cache), so they don't
  interfere.

---

# PRD #22 — PRDLESS label: run an issue without a PRD link

Serves human Feature #22 (an admin-controlled escape-hatch label that lets an issue run
without a `prds/*.md` link — configurable name, feature on/off, both in admin settings;
enabled out of the box; default name `PRDLESS`; the label can be added/removed directly
from the uzi web UI). User-stated 2026-07-05. Section numbers continue past PRD #21's
#118. Full rationale + Decision Log: `prds/22-prdless-label.md`.

**Status (branch `prd-22-prdless-label`):** prd-21 landed on main and is merged into this
branch. M1 (strict per-key validation + admin-settings toggle/name UI), M2 (gate bypass,
manual + autopilot), M4 (UI label toggle endpoint + forgesvc helper), and M5 (docs) are
built. M3 (bootstrap fields + web badges) is built too; M6 (e2e) is the remaining
milestone — each decision below flags the milestone that owns it.

## 119. prdless settings keys + on-by-default resolution

Serves human: "name configurable, feature toggleable on/off, both in admin settings";
"enabled out of the box"; "default name PRDLESS".

- **Two `app_settings` keys** on the PRD #19 KV store (§93): `prdless_enabled`
  (`"true"`/`"false"`) and `prdless_label`, with **compiled-in defaults** (`true`,
  `"PRDLESS"`) in `settings.Defaults` and typed accessors alongside `PRDLabel`/
  `AutopilotLabel`. **No migration, no seed rows**: `Cache.All`/`Effective` range over
  `Defaults`, so absent rows are synthesized server-side and `GET /api/admin/settings`
  always returns both keys. On-by-default falls straight out of the default map — an
  absent `prdless_enabled` row means enabled.
- **Unspecified = on is the single meaning.** A malformed stored `prdless_enabled` value
  (not `"true"`/`"false"`) resolves to the compiled default (enabled), exactly like an
  absent row — a junk value can never silently flip a default-on feature *off*. The
  accessor keeps this tolerance as defense-in-depth alongside the strict write validation
  (below), so a value written directly to the DB (bypassing the handler) still resolves
  safely.
- **Strict per-key validation (M1)** registers into prd-21's `settings.Validate(key, value)`
  per-key switch: `prdless_enabled` → strict bool parse (exactly `"true"`/`"false"`);
  `prdless_label` → label rules (non-empty, ≤ 64 runes, no comma) **plus pairwise-distinct**
  (in `ValidateMerged`) from both `prd_label` and `autopilot_label`, validated on the
  **post-merge** set and **regardless of the toggle state** (a disabled-but-colliding label
  must be renamed first, so re-enabling is always safe; equal to `prd_label` would exempt
  every issue, equal to `autopilot_label` would conflate "hands-off" with "spec-less"). Each
  distinctness rejection names the key to change.
- **prdless keys excluded from the `ForceReconcile` `changed` set** (§96, M1): they don't
  affect the poller's PRD-label filter, so a prdless PUT must not trigger a repo resync
  (the precedent prd-21's presentation-only `default_theme` key sets via `LabelChanged`).

## 120. Gate bypass: policy in the callers, enforcement in the shared service (M2, built)

Serves human: "the label lets an issue run without a prds/*.md link."

- **One enforcement point, two policy sites.** `workersvc` gains **no settings
  dependency**. Each caller that already holds a forge-fresh issue snapshot computes
  `allowWithoutPRD := prdlessEnabled && contains(issue.Labels, prdlessLabel)` and passes
  that single bool down; the shared `createRun` gate becomes
  `if !issue.HasPrdLink && !allowWithoutPRD { return ErrNoPRDLink }`. The manual handler
  passes it into `CreateRun`, the autopilot poller into `CreateAutopilotRun` — **both
  paths**, since the shared gate means the signature change touches both anyway.
- **Decided on the forge-fresh snapshot, exact match.** Both callers already re-fetch the
  issue from the forge immediately before run creation (manual handler; autopilot poller's
  `GetIssue`), so a just-added label takes effect without waiting for a poller cycle. Match
  is **exact — case- and whitespace-sensitive** — the same discipline board column labels
  use. `has_prd_link` **cache semantics are untouched** (the flag still means "description
  links a PRD file"; the bypass is layered at the decision point, never baked into the
  cached flag).
- **Evaluated once, at run creation.** Disabling the feature or removing the label neither
  stops nor re-gates an already queued/claimed/running/awaiting-approval run — prdless
  inherits the existing invariant that resume/requeue/claim never re-check `HasPrdLink`.
  Toggling off blocks only *new* runs. Deliberate.
- **Run is otherwise identical.** No new run flag, no claim-payload change, no prompt
  variant: same state machine, planning turn, approval gate, guardrails. A thin
  description is caught by the retained planning turn + human approval.
- **422 message extended** when the feature is enabled instance-wide, interpolating the
  label name from settings ("...add a prds/*.md link (or the PRDLESS label) before starting
  a run").
- **Best-effort settings read, fails OPEN to the default.** A settings read error at the
  run-create gate degrades to enabled=true (the already-safe default) rather than blocking
  every run start on a settings hiccup — deliberately the opposite of the mutating endpoint
  (§121).

## 121. UI label toggle: endpoint + forge-first single-label helper (M4, built)

Serves human: "add/remove the label directly from the uzi web UI, not only in GitLab."

- **Dedicated endpoint** `POST /api/repos/{id}/issues/{iid}/prdless`, body `{"apply": bool}`,
  under `RequireAuth` + CSRF **plus the per-user forge limiter** like every other
  forge-writing route. The **label name is resolved server-side from settings** — the
  client never names a label. Returns **422 when the feature is disabled**.
- **New single-label forgesvc helper, NOT the column-move path.** `AutoMove`/
  `PlanLabelMove` strip all *other* column labels to enforce single-column membership;
  reusing them would clobber columns. The helper adds or removes **only** the one prdless
  label and preserves everything else, keeping forge-first discipline:
  - **Idempotent cached-labels diff first**: already-present-on-apply / already-absent-on-
    remove → local no-op success, **no forge call**.
  - Otherwise **`EnsureLabels` on apply only** (auto-creates the label the first time it is
    applied from uzi, color **`#ec9a29`** = `forgesvc.PrdlessLabelColor`), then **one**
    `UpdateIssueLabels` with a single-element add-or-remove set.
  - **Cache updated incrementally on forge success only** — add/remove the one label on the
    current cached set, never a wholesale overwrite from stale data. `HasPrdLink` carried
    verbatim.
- **Card response** built like `MoveIssue`'s — column-position map + `latest_run`
  re-hydration — so a single-card replace doesn't blank the run badge. **No optimistic
  update** on the web side (wait for the 200; forge-first).
- **Toggle affordance.** The web toggle (IssueView primary, board card secondary) is
  visible only when `prdless_enabled`, which the SPA reads from the session bootstrap
  (§122). **Generic arbitrary-label editing from uzi is out of scope**: labels are board
  semantics (columns, PRD, autopilot, prdless); free-form label management stays in GitLab.

## 122. Web bootstrap + badges, and the quality-gate framing (M3, built)

Serves human: the escape hatch is discoverable in the UI without weakening the primary
directive (agents never touch `main`).

- **Session bootstrap** (`sessionPayload`, `handler/auth.go`, M3) gains `prdless_label`
  (string) and `prdless_enabled` (bool — its first bool field), alongside the existing
  labels + theme fields, so the SPA gates the toggle and badge on `prdless_enabled` and
  knows the label name. A server predating the fields omits both; the SPA treats the
  feature as off.
- **Badge** (M3): the board card and issue view replace the "no PRD link" warning badge
  with a distinct badge showing the configured label name (tone `brand` — uzi's accent,
  Open Question 1 resolved; title "PRD-link gate bypassed by label") when the feature is
  enabled and the issue carries the label. The board column-suggestion filter also excludes
  `prdless_label` (it is a workflow marker, never a column).
- **Quality gate, not a security boundary.** The bypass is a **gate exception, not a mode**,
  and never weakens any of the four `main`-protection layers; the human still clicks Start
  and approves the plan. Who can apply the label is bounded by GitLab membership (Reporter+)
  plus any uzi session on a connected repo — the **same population that already moves board
  cards** (also a forge label write). The two "toggles" are different populations: the
  *settings* toggle is admin-only; the *label* apply/remove is any uzi session on a
  connected repo.
- **Composition is allowed by design**: an issue carrying PRD + autopilot + PRDLESS runs
  unattended with no PRD link — all three are explicit opt-ins; M2 covers it with a
  composition test. Called out as a docs caveat.

---

# PRD #6 — CI status integration & CI-fix agent

Serves human Feature #6 (CI status visible in uzi + an agent that fixes broken CI
and uzi verifies the fix) and the "uzi keeps its own dummy CI" item. Two halves along
the PRD #4 dependency boundary: a display half on PRD #2 machinery only (M1–M3), and a
fix-agent half riding PRD #4's run machinery (M4–M7). Section numbers continue past PRD
#22's #122. Realizes `prds/6-ci-status-integration.md` (its Decision Log carries the
user-vs-AI provenance and full rationale); builds on PRD #2 (forge layer + poller), PRD
#4 (runs, worker protocol, plan gate, MR flow), and PRD #12 (`runs.mr_iid`/`mr_state`).

## 123. Forge pipeline read methods (`forge.go` + `gitlab.go`)

Serves human: "check and display CI status" — read the forge's pipeline state through
the same neutral-domain + redaction discipline as every other forge call.

- **Four additive methods** behind the existing `Forge` interface (pure additions —
  `forge.go` is a three-way merge point with PRD #4/#5, all rebased trivially):
  `LatestPipeline(projectID, ref)`, `LatestMRPipeline(projectID, mrIID)`,
  `ListPipelineJobs(projectID, pipelineID)`, `JobLogTail(projectID, jobID, maxBytes)`.
  Plus `Pipeline{ID,Ref,SHA,Status,WebURL,CreatedAt,UpdatedAt}` / `Job{ID,Name,Stage,
  Status,WebURL}` domain types and `ErrNoPipeline` (ref has no CI ⇒ "no CI" in the
  cache, not an error).
- **`LatestMRPipeline` exists because branch-ref filtering misses detached and
  merged-results pipelines** (they run on `refs/merge-requests/:iid/head`, never under
  the source-branch ref). GitLab groups `merge_request_event` pipelines first in the
  list response, so `[0]` is NOT newest — the driver selects **max-by-id** from the
  returned page.
- **`JobLogTail` is a full-download-then-client-truncate**: the trace endpoint has no
  range/tail parameter, so it downloads the trace (with a **16 MiB fail-closed
  ceiling**) and keeps the last `maxBytes`. Tail, not head, because failures conclude
  logs. Confined to fix-trigger time, never the poll tick.
- Redaction: the four methods pass through the driver's existing PAT-scrubbing redactor
  like all forge calls (snapshot-specific scrubbing is §129).

## 124. Pipeline sync + `pipeline_statuses` cache (poller tick)

Serves human: "display CI status" on the board/repo view, within one poll interval.

- **`pipeline_statuses`** (migration drafted `00040`, final number assigned at land time
  per the goose convention): a **latest-per-`(repo_id, ref)`** cache, `UNIQUE(repo_id,
  ref)`, upserted every tick. Forge is the source of truth; uzi caches, never invents.
- **Rides the existing poller tick** — `forgesvc.SyncPipelines` runs after the issue sync
  inside `poller.Engine.syncRepo`; no second ticker, no new interval knob, same bounded
  concurrency + jitter. Refs no longer watched are **evicted on the reconcile tick**
  (mirrors the issues-cache eviction).
- **Watched refs per enabled repo** = `default_branch` (via `LatestPipeline`) + the
  `runs.branch` of that repo's runs that are non-terminal, or terminal-with-an-MR within
  `CI_WATCH_RUN_WINDOW`, each read via `LatestMRPipeline(mr_iid)` when the run has an MR
  else `LatestPipeline(branch)`. Newest first, capped at `CI_WATCH_MAX_REFS` (hitting the
  cap is logged, not silent). `CI_WATCH_MAX_REFS=0` disables pipeline sync entirely
  (badges + Fix CI gone) — reproduces pre-PRD-#6 behavior bit-for-bit.
- **`CI_WATCH_RUN_WINDOW` default is `336h`, NOT `14d`** — Go `time.ParseDuration` has no
  `d` unit; the docs carry this footgun note.
- **DTO enrichment (read paths)**: `repoDTO` gains `pipeline` (default branch) so both
  `GET /repos` and the per-connection projects listing carry it (non-enabled projects ⇒
  `null`); `GET /repos/{id}/board` gains it at board level and per-card for the most
  recent run's branch.

## 125. `ci_fix` run kind: schema, trigger, failure snapshot

Serves human: "spin up an agent to review what happened and if it can fix it".

- **Schema evolution on PRD #4's `runs`** (migration in the same `00040`+ landing group):
  `kind text NOT NULL DEFAULT 'issue'` (`issue|ci_fix`), `issue_iid` **DROP NOT NULL**,
  `pipeline_id`/`pipeline_ref`/`failure_snapshot(jsonb)`/`fix_verdict` columns, and a
  `runs_kind_shape` CHECK (issue ⇒ issue_iid present; ci_fix ⇒ pipeline_id+pipeline_ref
  present). `DEFAULT 'issue'` backfills existing rows untouched.
- **`ci_fix` as a run kind with nullable `issue_iid`, NOT auto-created GitLab issues**
  (rejected alternative, PRD Decision Log): auto-creating a forge issue per CI failure
  would spam the board and make the PRD-link check meaningless. Nullable `issue_iid` +
  CHECK is the honest shape.
- **Exclusion**: a partial unique index `uq_runs_one_active_ci_fix (repo_id,
  pipeline_ref) WHERE kind='ci_fix' AND status NOT IN terminal` (mirrors the
  one-active-per-issue index, which also gains a `kind='issue'` predicate —
  defensive-only, NULL `issue_iid` never collides). The two partial indexes are disjoint
  and can't express the **cross-kind same-branch** rule (an issue run and a ci_fix run
  targeting the same branch would collide in one worktree), so **both `CreateRun` and the
  trigger endpoint check it at trigger time**; `ErrBranchInUse` maps to **409**, and git's
  "branch already checked out" is the race backstop. (The e2e surfaced this.)
- **Trigger**: `POST /api/repos/{id}/ci-fix-runs {ref}` (under `/api`, no `/v1`) —
  validates the cache shows `failed` for the ref, no active fix for the ref, no active
  run of any kind on that branch, and the same worker+token preconditions as `CreateRun`.
- **Failure snapshot frozen at queue time** (self-containment, same reason PRD #4
  snapshots issues — the cache row gets overwritten): pipeline id/sha/web_url + up to
  `CI_FIX_MAX_JOBS` failed jobs, each with `JobLogTail(…, CI_FIX_LOG_TAIL_BYTES)`.
- **Manual trigger only in MVP**; auto-spawn on failure deferred (burns tokens
  unattended — `multica`'s ungated autopilot is the audited weakness). The plan gate keeps
  a human in the loop.

## 126. Wire contract: claim payload + `fix_verdict` state report

Serves human: server/worker split; best-practice (two lenient fakes each invent a field
differently — the PRD #4 M1+M2 lesson).

- **Claim payload** gains `kind` and, for `ci_fix`, `pipeline: {id, ref, sha, web_url,
  failed_jobs: [{name, stage, web_url, log_tail}]}`.
- **State report (worker→server)** gains an optional `fix_verdict` on the `completed`
  report so a `not_code` outcome travels the wire (`SetRunCompleted` extended). Both
  directions are pinned by a **cross-side contract test**, not two independent fakes.
- **Inbound `fix_verdict` is clamped to `not_code` only** — `verified`/`fix_failed` are
  pipeline-sync-authoritative (§128); a forged inbound `verified`/`fix_failed` drops to
  NULL so the worker can't self-certify a passing fix.

## 127. Worker `ci_fix` workflow (extends PRD #4's executor, guardrails verbatim)

Serves human: "spin up an agent to fix it"; PRIMARY DIRECTIVE holds for ci_fix exactly
as for issue runs.

- **Kind-aware executor**: worktree on the failing ref → lead diagnoses from the snapshot
  (may re-run failing commands locally via Bash; cannot touch the forge) → plan gate
  posts root cause + fix **or** a `not_code` verdict → on approval, implement⇄review as in
  PRD #4.
  - **ref = default branch** → fix on new branch `ci-fix/pipeline-{id}`, worker pushes +
    opens a new MR linking the failing pipeline (no issue to link).
  - **ref = an agent run branch** → fix commits land **on that same branch**, the existing
    MR updates (no second MR — bottega's PR-feedback shape).
  - **`not_code`** (infra/flaky/secret/runner) → run completes with the diagnosis as its
    result, `fix_verdict='not_code'`, no MR.
- **Log tails are untrusted data, never instructions**: the lead prompt frames the
  snapshot as quoted evidence. Fencing uses a **per-prompt random nonce tag
  `<job_log_{hex}>`**; `job.name`/`stage` are sanitized (backticks/CRLF stripped) before
  prose interpolation.
- **Every PRD #4 guardrail holds unchanged** (worker-held PAT, `PreToolUse` deny-hook,
  `bypassPermissions`+`disallowedTools`, `settingSources:[]`, sparse env, iteration cap,
  watchdogs). Push targets are non-protected branches, so no guardrail loosening; a ci_fix
  attempting `git push origin main` fails identically to an issue run. The M7 auditor pass
  proved hostile log content cannot steer a push to `main`.

## 128. Verification loop — passive stamp keyed on `runs.branch`

Serves human: "if the code was bad ⇒ uzi verifies its work", mechanically.

- **Passive stamp inside the pipeline sync step** (no new loop, no worker involvement,
  no auto-retry): when the sync sees a terminal pipeline on a fix run's branch, it stamps
  `fix_verdict` — `success ⇒ 'verified'`, `failed ⇒ 'fix_failed'`.
- **Keyed on `runs.branch` (the fix branch), not `pipeline_ref` (the failed ref)** — they
  differ for a default-branch fix (`pipeline_ref='main'`, `branch='ci-fix/pipeline-{id}'`);
  keying on `pipeline_ref` would never stamp or false-stamp from unrelated `main` commits.
- **`observed pipeline id > snapshot pipeline id` guard** ensures the stamp reacts to the
  fix push, not the original failing pipeline — this is also what disambiguates the
  agent-branch case where `branch == pipeline_ref`.
- Observed via `LatestMRPipeline` of the fix run's MR (catches detached/merged-results
  pipelines); the max-by-id pipeline must be **terminal** to stamp.
- **Stamp-target selection** (a branch can host sequential fix runs over time): the ci_fix
  run with `branch = ref AND fix_verdict IS NULL AND snapshot id < observed id`, newest
  first.
- `fix_failed` is **surfaced, not auto-retried** (user fires another fix run, which gets
  the new snapshot). A fix branch that ages out of the window before a terminal pipeline
  keeps `fix_verdict = NULL`, shown as "unverified" — honest, not fabricated.

## 129. Snapshot redaction (dedicated scrubber over the driver scrub)

Serves human: best-practice — job logs are the most attacker-influenceable text uzi feeds
an agent, and teammates' pipelines may print tokens.

- A **dedicated snapshot log scrubber** runs on top of the driver's by-value PAT scrub,
  because a snapshot can contain secrets the driver never saw: **9 GitLab token families**
  (`glpat`/`gloas`/`glrt`/`glcbt`/`glptt`/`glsoat`/`glimt`/`glagent`/`gldt-`), `sk-ant-`,
  and `PRIVATE-TOKEN`/`Authorization`/`Bearer` header lines to EOL.
- **Join tokens and Anthropic tokens have no by-value scrub** in the snapshot path (they
  aren't in the failure snapshot's provenance) — header-shape coverage only; documented.
- **Arbitrary third-party secrets** a pipeline prints are the documented residual risk:
  size-capped, stored server-side like any run message (owner/admin-only authz), but uzi
  can't know every secret shape.

## 130. Web: pipeline badges + Fix CI + verdict chips

Serves human: "display CI status" on cards/repo view; the fix affordance.

- **Five-tone badge taxonomy** (`web/src/lib/pipelineBadge.ts`): `passed` (success),
  `failed`, `running` (created/waiting/preparing/pending/running/scheduled), `attention`
  (manual), `neutral` (canceled/skipped/no CI); **unknown status ⇒ neutral**; success is
  **labeled "passed"**. Reuses `ui.tsx` `Badge` tones; shows `synced_at` staleness on
  hover.
- Badge per Repos row (default branch), board header (default branch), and per card
  whose run branch has a status. A **failed** state renders the **Fix CI** button,
  precondition-disabled with a reason (mirroring "Start run").
- **Fix-run view** reuses PRD #4's `/runs/:id` as-is; the header shows the failing
  pipeline link + a **verdict chip** (`verified ✓` / `fix failed ✗` / `not a code problem`
  / `unverified`).
- **All forge-provided `web_url` links guarded by `isHttpsUrl`** (PipelineBadge +
  CIFixRunHeader) — a forge-supplied URL is not trusted into an `href` unchecked.

## 131. Configuration (env, extends §13/§25)

| Var | Default | Notes |
|---|---|---|
| `CI_WATCH_RUN_WINDOW` | `336h` | run-branch watch window (`14d`; Go has no `d` unit) |
| `CI_WATCH_MAX_REFS` | `20` | cap on watched refs/repo; **`0` disables pipeline sync + Fix CI** |
| `CI_FIX_MAX_JOBS` | `10` | failed jobs captured per snapshot |
| `CI_FIX_LOG_TAIL_BYTES` | `32768` | trailing bytes of each job trace snapshotted |

Pipeline sync rides `FORGE_POLL_INTERVAL` — no new interval.

## 132. Dummy CI pipeline for the uzi repo itself

Serves human: "setup a local dummy CI for uzi, kept (merged to main), so we can see the
PRD working" (user, 2026-07-06).

- A minimal committed `.gitlab-ci.yml` at the repo root — kept, not throwaway — so uzi's
  own repo produces real pipeline statuses for the feature to display and fix against.
- **`lint`/`smoke` echo-placeholder stages** named for future real gates + a **`demo-fail`
  job gated on `UZI_CI_DEMO_FAIL=1`** so a red pipeline can be produced on demand to
  exercise the Fix-CI path end to end.

---

# PRD #28 — Docs Search (client-side full-text search on /docs)

Serves human Feature #28 ("a search box for the docs page; full-text with snippets;
on the `/docs` index only"). **Status: M1–M3 built and merged on `feature/prd-28-docs-search`;
M4 (this spec sync) closing out.** Adds **zero** new services, API routes, DB tables,
env vars, or dependencies — a pure web-side extension of PRD #7's bundled-docs corpus.
Extends §54–60.

## 133. Search core — pure tokenized substring search (`web/src/lib/docsearch.ts`)

Serves human: "full-text search with snippets" (not a title/summary filter).
Best-practice at this scale (11 short in-memory pages); the pure module keeps a
library swap contained if the corpus outgrows substring search.

- **Hand-rolled client-side search, zero new deps** — rejected `fuse.js` (bottega's is a
  declared-but-unused dependency, nothing to copy) and multica's server-route
  Fumadocs/Orama search (uzi has no docs backend by design; PRD #7 rejected a docs
  service/framework). The corpus is already fully in the browser as raw strings, so
  search needs no network at all.
- **Corpus = `audience: user` pages only** (`listUserDocs()` — exactly what the index
  lists). Same pure/bound split as §54's docs.ts: `buildIndex` + `searchIndex` are
  fixture-testable; `searchDocs()` binds them to the real corpus, memoized once (the
  bundle's corpus is fixed).
- **Matching**: case-insensitive, multi-token AND — every whitespace token must appear
  as a substring in title, headings, or body. Matching/counting use **`indexOf` loops,
  never `new RegExp(token)`** — query tokens are user input full of regex metacharacters
  by design (`.env`, `--profile`, `c++`); a test pins literal matching.
- **Short-query + length guards**: whole query under `MIN_QUERY_LENGTH` (2) chars → no
  search (normal index shown); tokens shorter than 2 chars are dropped after tokenizing
  (so `"a b"` doesn't match near-every doc); query truncated at `MAX_QUERY_LENGTH` (256)
  before scanning (defensive; no real query approaches it).
- **Ranking**: title-hit tier > heading-hit tier > body-only tier; within a tier, more
  total token occurrences first; slug `localeCompare` as stable tiebreak (same spirit as
  §55's `sortDocsForIndex`).
- **Markdown stripping for indexing** (`stripDocBody`, single fence-tracked pass):
  reduces links/images to their label and drops emphasis/backtick markers (reusing
  §56's `summarize()` regex approach), but **keeps fenced-code and table-cell text**
  (users search for commands like `docker compose --profile agent up`) and **preserves
  intra-word underscores** (`\b_+|_+\b` only strips word-boundary underscores) so
  identifiers like `UZI_WORKER_TOKEN` stay searchable — a deliberate deviation from
  `summarize()`'s blanket underscore strip. Fence delimiter and GFM table-alignment
  rows are dropped; heading markers dropped but heading text retained (and collected
  separately for the ranking tier). A `#` inside a fence is code, not a heading.
- **Snippet**: `SearchResult = { doc, snippet, ranges }`. Snippet is a ~160-char window
  (`SNIPPET_WINDOW`, `SNIPPET_LEAD` 60 chars of pre-match context) centered on the
  earliest body match, word-boundary trimmed with leading/trailing `…`; a **title/
  heading-only hit falls back to the doc's existing `summary`**. `ranges` are
  **snippet-relative, sorted, and merged/non-overlapping** (overlapping tokens like
  `work` + `worker` collapse into one range) so the UI's split-and-mark is a straight
  fold with no nested `<mark>`s. A token matching only in title/headings simply
  contributes no body range (tested via the mixed title-token + body-token case).

## 134. Index UI — search box on `/docs` (`web/src/pages/Docs.tsx`)

Serves human: "search box on the `/docs` index only" (not on `/docs/:slug`, not global
nav — 2026-07-09). Best-practice a11y + dark-theme consistency.

- The repo's `Input` primitive with `type="search"`, labelled, placeholder "Search
  docs…" at the top of the index — not a bare `<input>`, so focus/border styling stays
  consistent. **No debounce** (11 docs, synchronous, instantaneous).
- **Empty/short query renders the existing ordered card list unchanged**; an active
  query renders result cards (same `Card` language): title + snippet with matched tokens
  wrapped in `<mark>`. A no-results state lives inside a `Card`, matching the existing
  empty state.
- **`<mark>` highlighting via React elements only** (split text + `<mark>` nodes),
  **never `dangerouslySetInnerHTML`**. Explicitly styled `bg-warn/25 text-fg` — there is
  no highlight token in the theme and the UA-default opaque yellow is unusable on the
  dark theme, so the UA background/color is overridden.
- **A11y**: `aria-live="polite"` scoped to the **result-count/no-results line only**
  (not the result list), so screen readers announce "N docs match" per keystroke without
  re-reading every snippet. Input is labelled.
- **Keyboard**: `Escape` clears the query; `/` anywhere on the index focuses the box,
  skipped when focus is already in an input/textarea.
- No nginx/CSP change (no new asset types, no inline script).

## 135. Test & docs surface

Serves human: the behavioral gate must be automated (no browser automation in-repo).

- **jsdom component tests are the behavioral gate** (`Docs.test.tsx`, existing vitest +
  testing-library) — a body-only term from a real bundled page is found, snippet marked,
  clear restores the index. Unit tests (`docsearch.test.ts`) pin stripping edge cases
  (code fences, tables, links, intra-word underscores), ranking tiers, multi-token AND,
  snippet windowing, the short-query/length guards, regex-metachar tokens, overlapping-
  range merging, and the mixed title-token + body-token snippet.
- **M3 compose smoke rescoped to curl-assertable checks** (the built image serves `/docs`
  HTML + JS bundle); search itself is client-side JS with no browser automation in-repo,
  so jsdom (M2) is the automated gate and a manual browser check is a nice-to-have, not a
  gate.
- **Adding a future docs page requires no search work** — the corpus derives from the
  same glob/frontmatter pipeline as the index (§54–55). `docs/README.md`'s add-a-page
  section notes that `user` pages are automatically searchable (nothing to register).

---

# PRD #32 — Per-user vault: password-wrapped secrets

Serves human Feature #32 (an operator with the DB + env/Infisical/etcd master key must
not recover any user's Anthropic token; each user's secrets keyed by their own login
password; auto-unlock at login with the key in server memory until restart/lock; locked
runs queue; forgotten password ⇒ unrecoverable). **Status: built (PRD #32,
`prds/32-user-vault-password-wrapped-secrets.md` carries the full Decision Log +
user-vs-AI provenance). These sections record the as-built system; where implementation
diverged from the design, the as-built decision and its reason are called out inline
("as-built").** Builds on PRD #3 (`user_secrets` + `secretbox`, §29–30) and gates PRD #19
autopilot's claims. Landed as migration `00044_user_vaults.sql` (the draft number was
already the next free slot above head `00043`). Extends §17/§29 (secretbox), §40–41 (claim
path), §24 (seed).

## 136. Key hierarchy & crypto (Bitwarden model, per-user)

Serves human: "secrets encrypted with a key derived from the user's own login password;
the server stores only the wrapped key".

- **Two-tier envelope**, Bitwarden/1Password shape (master password → KEK → DEK → data):
  - **DEK** = 32 random bytes (`crypto/rand`), one per user, generated on vault creation;
    seals that user's `user_secrets` rows via `secretbox`.
  - **KEK** = `Argon2id(login password, kek_salt)`, derived at unlock, used to unwrap the
    DEK, then discarded. Never persisted.
  - **`wrapped_dek = secretbox(KEK, DEK)`** is the only at-rest form of the DEK: the DEK
    is never written unwrapped, the KEK is never written at all.
- **`kek_salt` is a dedicated random 16-byte salt, independent of the auth-hash salt.**
  Load-bearing: the login flow *stores* its Argon2 output in `users.password_hash`;
  reusing the same salt+params would make `password_hash` itself equal the KEK — i.e.
  leak the KEK at rest. A distinct salt ⇒ distinct derivation ⇒ the KEK result is never
  persisted anywhere. Cost params reuse `auth/argon2.go` (t=2, 19 MiB); a higher KEK cost
  is a documented option.
- **Reuses `secretbox` AES-256-GCM (§29) for BOTH layers** — the wrap (KEK→DEK) and the
  data seal (DEK→secret) are the same construction with different keys. No new primitive.
- **GCM auth failure doubles as wrong-password detection**: a wrong password derives a
  wrong KEK, so unwrapping `wrapped_dek` fails GCM authentication cleanly ⇒
  `ErrWrongPassword`. No separate password-verifier is stored, and this is no more of an
  oracle than login already is (endpoint sits behind the auth-class rate limiter, §138).
- **AAD binding (as-built).** `secretbox` grew `SealWithAAD`/`OpenWithAAD`; the plain
  `Seal`/`Open` now delegate with nil AAD, byte-identical to the legacy construction so
  master-sealed rows written before the vault still open. Two distinct bindings: a
  DEK-sealed **secret** is bound to `user_id || 0x00 || kind` (the 0x00 separator keeps
  `(id,"ab")||"" ≠ (id,"a")||"b"`); the **wrapped DEK** is bound to
  `"uzi-vault-dek\x00" || user_id`. Both defend against a DB-*write* operator swapping a
  ciphertext (or a whole `user_vaults` row) between users — a swap fails GCM auth rather
  than yielding a working key (outside the passive threat model — not disclosure — but a
  few lines). The `kind` / `sealed_with` string literals are centralized as
  `store.KindAnthropicToken` and `store.SealedWith{Master,DEK}`
  (`api/internal/store/enums.go`) so no two seal sites can disagree on the AAD and
  silently break decryption.
- **KEK cost is a private copy of the auth Argon2 params, not an import** (the `kek*`
  consts in `vault.go`; t=2, 19 MiB, p=1). The KEK and the login hash are separate
  security domains: a vault-protected deployment can raise the KEK cost alone (residual #1)
  without coupling the two. Kept `>=` the login-hash cost.

## 137. Storage & lazy rewrap (`user_vaults`, `user_secrets.sealed_with`)

Serves human: server stores only the wrapped key; forgotten password ⇒ unrecoverable by
design.

- **New `user_vaults`** (migration `00044_user_vaults.sql`): `user_id PK → users ON DELETE
  CASCADE, kek_salt bytea, wrapped_dek bytea, created_at, updated_at` — one row per user,
  cascade-scoped like every other per-user table.
- **`user_secrets` gains `sealed_with TEXT NOT NULL DEFAULT 'master' CHECK IN
  ('master','dek')`** so `vault.Open` knows which box to use per ciphertext. New saves
  always seal `'dek'`.
- **Vault-row creation is race-safe and login/register-only (as-built).** The insert is
  `CreateUserVaultIfAbsent` (`INSERT ... ON CONFLICT (user_id) DO NOTHING RETURNING`):
  exactly one of N concurrent first-unlocks wins; a loser re-reads the winner's row and
  caches THAT persisted DEK (unwrapped with its identical password), so the in-memory DEK
  always matches what is stored. The design's M1 `UpsertUserVault` was **removed** — a
  `DO UPDATE` would overwrite an existing wrapped DEK and orphan every secret sealed under
  the old one. Creation happens ONLY on the login/register paths (which hold a
  freshly verified password), **never** on the interactive unlock endpoint (§138): the
  endpoint uses a no-create `UnlockExisting`, so a wrong password can never mint a fresh,
  differently-keyed vault and self-lock the user out of their real secrets.
- **Lazy rewrap on unlock (as-built): best-effort, never fails the unlock.** Pre-existing
  rows are master-key-sealed and cannot be rewrapped without the user's password. On each
  unlock, that user's still-`'master'` rows are opened with the master box, resealed with
  the DEK, and flipped `'master' → 'dek'` — **one guarded UPDATE per row**
  (`RewrapUserSecret`, `WHERE ... sealed_with='master'`, so a concurrent flip is a no-op),
  not one transaction across all rows as first sketched. Every step is logged (never with
  secret bytes) and a fault leaves the row `'master'` for the next unlock to retry; a
  rewrap error must never fail an otherwise-valid login/unlock. Dormant accounts never
  rewrap.
- **Migration progress is admin-visible**: `GET /api/admin/vault-migration` returns the
  count of still-`sealed_with='master'` rows (`CountMasterSealedSecrets`), surfaced as a
  progress notice in AdminSettings (Instance settings).
- **Rewrap protects going-forward only, never retroactively.** An operator could have
  snapshotted the DB before the rewrap, so any token that ever existed master-sealed is
  potentially already leaked; the real fix is to **rotate the token**, not rewrap it. A
  one-time, per-browser-dismissible UI notice (Settings → Secrets; localStorage key
  `uzi.vault.rotateNoticeDismissed`) says exactly that ("protected from now on"; rotate
  for full protection), never "protected retroactively". Until rewrap, legacy `'master'`
  rows are withheld only by the runtime claim gate (§139) — a runtime control, not a
  cryptographic one.
- **Reset (password lost) must DELETE `user_vaults` + all `'dek'` rows** and prompt
  re-entry — silently keeping unreadable ciphertext would be a worse bug than the
  by-design data loss. Change/reset endpoints don't exist yet; constraints recorded for
  when they land (change: unwrap old-KEK → rewrap new-KEK in the same tx as
  `password_hash`, transparent to the user).

## 138. `api/internal/vault` package, DEK cache & wire-in

Serves human: auto-unlock at login; key kept in server memory until restart/explicit
lock; lock/unlock/status surface.

- **New `api/internal/vault`** package: `Unlock` / `Lock` / `Unlocked` / `Seal` / `Open`
  over a store, a master box (for lazy rewrap), and an in-process DEK cache
  (`map[uuid.UUID][]byte` + `sync.RWMutex`). DEKs are never logged or serialized and are
  best-effort zeroized on Lock (Go gives no guarantee — stated in a comment, not oversold).
- **The DEK cache IS the "unlocked" state**, held in API process memory from unlock until
  pod restart or explicit Lock (the user's choice over session-TTL caching — keeps
  overnight/autopilot runs working while unlocked). `Seal`/`Open` return `ErrLocked` when
  the user isn't cached.
- **Unlock at login** (`Login`, right after `VerifyPassword`, reusing the same plaintext
  before it leaves scope; a first-ever unlock creates DEK + wrap; a failure is non-fatal —
  logged loudly, the session still issues with the vault left locked and the SPA shows the
  unlock banner). **At register (as-built)** the vault is created+unlocked by a standalone
  `vault.Unlock` call AFTER the user-create transaction commits — NOT a row insert inside
  it. Register holds a `pg_advisory_xact_lock`; running the vault's Argon2 KEK derivation
  inside that lock would serialize all registrations, so it stays outside. Crash-safety
  comes not from tx atomicity but from Unlock's create-on-first-login: a crash between
  user-create and vault-create is healed by the next login. Login now runs Argon2id twice
  on the success path (verify + KEK) — keep the login rate limiter strict.
- **Endpoints inside `RequireAuth`** (so unlock is not a pre-auth oracle and CSRF
  applies): `POST /api/vault/unlock {password}` → 204/403, `POST /api/vault/lock` → 204,
  `GET /api/vault/status`. Unlock is rate-limited with the **per-user**
  `PerUserMiddleware`, not the per-IP auth limiter — it is authenticated, and a stolen
  JWT would otherwise make it an online password-guessing oracle sharing a NAT bucket
  with other users' logins. The endpoint calls `UnlockExisting`; a **wrong password**
  (`ErrWrongPassword`) and a **no-vault user** (`ErrNoVault`) return an **identical 403** —
  it never distinguishes the two, and (unlike login/register) never creates a vault.
- **Constant-cost auth surfaces (as-built), so timing is never an oracle.** Every
  wrong-credential answer costs exactly one Argon2. *Login*: a known email runs the real
  `VerifyPassword`; an unknown email burns one Argon2 against a fixed `dummyHash`. The KEK
  derivation (a second Argon2) runs ONLY after a successful verify — once success has
  already revealed the email exists — so it needs no counterweight on the failure paths. A
  design-stage "second dummy burn" on the known-email/wrong-password branch was implemented
  then **reverted** (commit `1a4cfe5`): it fired the KEK-cost Argon2 only when the email
  existed, which was itself an email-existence timing oracle. *Unlock endpoint*: both 403
  paths burn one KEK-cost Argon2 — the wrong-password path against the vault's real
  `kek_salt`, the no-vault path against a fixed `dummyKEKSalt` using the same `kek*`
  params, so a future cost bump self-tracks.
- **Vault status rides `/api/me`** (`sessionPayload`) so the SPA shell learns lock state
  in one round-trip; the web client refreshes it via `AuthContext.refresh()` on window
  focus, on any 409 `vault_locked` response, and after lock/unlock calls (there is no
  global WS to push it — the only socket is per-run).
- **Secrets save** (`handler/secrets.go`) seals via `vault.Seal`; returns 409
  `vault_locked` when locked (only reachable if the pod restarted mid-session).
- **Web surface**: header badge (🔓/🔒 with tooltip), a locked banner with a password
  field that unlocks without a full re-login, a Lock action in the settings menu,
  `queued` own-runs rendered as "waiting for vault unlock", and an irrecoverability
  notice on the token-save form.

## 139. Claim gating & lock-race handling (`workersvc`)

Serves human: locked-owner runs queue as "waiting for vault unlock" instead of claiming
or failing; unlock resumes them within a poll cycle.

- **Single Go gate, no SQL change**: `ClaimRun` is already scoped `r.user_id = @user_id`
  (from the worker's own identity), so a one-line `if s.vlt != nil &&
  !s.vlt.Unlocked(wkr.UserID) { return idle }` before `ClaimRun` keeps a locked owner's
  runs `queued`. Autopilot and the poller need no special code — this gate is the single
  enforcement point.
- **`assembleClaim` opens the Anthropic token via `vault.Open(run.UserID, kind,
  sealed_with, …)`**; bot-PAT decryption is unchanged (master box, §140). With no vault
  wired (nil — tests, or a deployment that opts out), the token opens under the master box:
  the pre-vault behavior.
- **Lock-race sentinel `errVaultLocked` (as-built): a bare sentinel, requeue not fail.**
  If Lock lands between `ClaimRun` and `assembleClaim`, `vault.Open` returns `ErrLocked`,
  which maps to `errVaultLocked`. This must **NOT** take the terminal
  `errCredentialUnavailable` path (`MarkRunFailedByID` — reserved for a *genuine* crypto
  fault, which still fails the run). Instead `RequeueClaimedRunToQueued` resets the
  just-claimed run `claimed → queued` (`WHERE id=@id AND status='claimed'`), **keeps
  `worker_id` for resume affinity, and does NOT bump `requeue_count`** — so a persistently
  locked vault can never trip the requeue cap — then reports idle.
- **One shared `*vault.Vault`, wired via `SetVault` seams.** The vault is constructed once
  in `main.go` and injected into BOTH `workersvc` (`SetVault`, same optional-dependency
  pattern as `SetBroadcaster`/`SetLifecycle`) and the HTTP handlers (`Handler.SetVault`),
  so a login-time unlock and a claim-time `Unlocked` check see the same DEK cache. The
  additive seam keeps the claim wire-in's files disjoint from the auth/endpoint wire-in.
- **Sweeper verified safe** (2026-07-10): no sweep query touches `status='queued'` and
  there is no queued-age timeout, so locked-owner runs sit indefinitely by design; a
  resumed run re-enters the claim gate and waits rather than failing.

## 140. Scope boundary, seed exemption & residual risks

Serves human: "materially harder, not impossible"; user accepts the residuals.

The as-built residual-risk list and operator hardening steps also ship as an in-app
operator doc, `docs/vault-threat-model.md` (`audience: operator`), linking back to the PRD
for rationale.

- **Forge bot PAT stays under `UZI_SECRET_KEY` (master key), outside the vault.** The
  poller must sync issues 24/7 with no user present, so the connection PAT cannot be
  password-wrapped. `UZI_SECRET_KEY` therefore remains, but only for connection-level
  secrets (and legacy not-yet-rewrapped rows). Accepted residual: an operator can still
  recover the bot PAT (Developer-role, blast radius bounded by PRD #5's least-privilege
  checks), but no user's personal Anthropic token.
- **Seed admin explicitly exempt.** `UZI_SEED_PASSWORD` / `UZI_SEED_ANTHROPIC_TOKEN` are
  env vars, so the vault is only as strong as env for that one account. As-built boot
  order in `main.go`: seed the admin user → **boot-unlock the seed admin's vault**
  (`vlt.Unlock` with the seeded password; first boot creates the vault here) → seed the
  Anthropic token, which the seed **DEK-seals** under the now-unlocked vault via a narrow
  `VaultSealer` interface (the subset of `*vault.Vault` the seed needs — not the earlier
  "`Sealer` grows a variant" sketch). Boot-unlock is **fatal when seeding is configured** —
  a seed admin whose vault can't be unlocked at boot is a broken bootstrap, matching the
  other seed steps' loud-on-misconfig stance — and populating the cache is what lets a
  fresh headless deploy run overnight autopilot before any interactive login. Post-boot
  hardening (documented): change the seed password, rotate the token, remove `UZI_SEED_*`
  from the deployed env. **Caveat once a change-password flow lands**: a stale
  `UZI_SEED_PASSWORD` left in the env would make the next (fatal) boot-unlock fail, so
  removing it becomes mandatory rather than merely advisable.
- **Dominant residual: `users.password_hash` is an offline brute-force oracle.** An
  operator with the DB can crack the login password against the stored Argon2id hash →
  KEK → DEK → tokens. The scheme's real strength is **password entropy + Argon2 cost, not
  the 256-bit DEK** (`MinPasswordLen` is 12). Documented loudly; a higher KEK Argon2 cost
  and a higher password floor are the noted mitigations for vault-protected deployments. As
  shipped the KEK reuses the login-hash cost (t=2, 19 MiB); raising it alone is a one-line
  change to the `kek*` consts in `vault.go`, and the no-vault unlock timing burn derives
  with those same params so the parity self-tracks any bump.
- **Other documented residuals**: live-pod memory dump captures cached DEKs + in-flight
  plaintext (DEK-in-RAM is the *common* state under until-restart caching; an optional
  idle auto-lock is a future off-by-default knob that would break overnight runs);
  trojaned image; the worker holds the plaintext Anthropic token for the run's duration
  (per-run short-lived tokens are the future-PRD answer); DEK cache is per-process, so
  the API stays single-replica (never replicate DEKs across pods); and a one-time rollout
  stall — on the deploy that ships this, every existing user's runs stop claiming until
  their next login (call out in release/ops notes).

## 141. Rejected alternatives

Serves human: the chosen approach is one of several; record why.

- **Key as a mounted file / fetched from Infisical at boot** — rejected: the same
  operators read Infisical and can exec into the pod to read the file.
- **Vault transit / KMS envelope encryption** — rejected for this deployment: cluster
  operators also administer Vault/Infisical, so it adds audit, not confidentiality.
- **Global manual unseal at boot (Vault-style Shamir shards)** — viable, but rejected in
  favor of per-user unlock: no ops ceremony on every deploy, per-user granularity, and
  the unlocker owns the secret. Layerable later for the connection PATs if wanted.
- **Per-run short-lived Anthropic tokens** (worker never holds a long-lived credential) —
  out of scope: depends on what the Anthropic OAuth flow permits; noted as a future PRD
  that composes with this one.
- **Inspiration check**: bottega (host ambient creds), multica (plaintext creds), and
  dot-agent-deck (ambient creds) all store provider credentials plaintext-equivalent;
  none does password-wrapped envelope encryption — this beats them by following
  Bitwarden's master-password key hierarchy.

---

# PRD #25 — Slack Integration

Serves human: "Users only learn that an agent run finished, failed, or is parked at the
plan-approval gate by keeping the webui open... There is no push channel of any kind"
(PRD #25 Problem statement, no dedicated human.md Feature entry yet — this PRD originated
directly from GitLab issue vtmocanu/uzi#25, reviewed by a 3-agent design/fact-check/security
pass before build). Realizes `prds/25-slack-integration.md` (its Decision Log carries the
full user-vs-AI provenance); builds on PRD #4 (runs, steering inputs), PRD #19
(`app_settings`), and `secretbox`. Section numbers continue past PRD #6's #132.

## 142. Socket Mode manager: outbound-only supervisor, settings-driven hot-reload

Serves human: "outbound-only... no inbound HTTP, no public URL" (Decision Log,
user-confirmed 2026-07-06).

- **`slacksvc.Manager`** polls the settings cache (no watch/pubsub exists) and, while
  `slack_enabled` is true with both tokens present, keeps one `socketmode.Client`
  connection up with exponential backoff; any token or enable change is diffed against
  the running connection's pair and hot-restarts the socket — no api reboot. Idle
  otherwise, so an unconfigured or disabled instance is a strict no-op.
- **Poll cadence doubles as the hot-reload latency floor** (default 5s): a token rotated
  in Settings can leave the *previous* socket briefly live for up to one poll before
  teardown — documented in `docs/slack.md`, not hidden as instant.
- **Connection state** (`disabled|connecting|connected|error:auth|error:connection`) is
  exposed through the handler for the admin webui's status chip; the error states carry
  only a coarse class, never the underlying Slack error text (which could embed a token).
- Debug logging is left off deliberately: slack-go's default logging would eventually print
  the Socket Mode connection URL, which carries a `?ticket=` query — a live-session
  credential the token-shape redaction patterns would miss.
- Reconnect **backoff resets after a stable connect**, and the long-lived websocket is
  deliberately **unbounded** (liveness is the Socket Mode ping, not a deadline) — only the
  `auth.test` validator and the initial handshake are time-bounded, by `SLACK_HTTP_TIMEOUT`
  (default 15s).

## 143. Settings secret-key class + ENV overlay (net-new registry work)

Serves human: "Config from ENV or webui... sealed at rest... ENV wins" (Decision Log,
user-confirmed).

- The pre-existing `app_settings` registry was plaintext-only (`Cache.All()` fed the admin
  GET verbatim; `Validate`'s default branch rejected token-length strings) — verified before
  building, not assumed reusable. Two structural, net-new additions instead:
  1. **`settings.SecretKeys`** (`slack_bot_token`, `slack_app_token`): sealed with
     `secretbox` + base64 before `UpsertAppSetting`, and kept **out of `Defaults`** —
     since every value-producing read (`All`/`Effective`/the admin DTO) ranges over
     `Defaults`, a secret key is structurally unable to leak through those paths; the API
     can only ever report `configured: true|false` for one. Reading the decrypted value is
     only possible through slacksvc's own accessors.
  2. **ENV-source overlay**: `SLACK_BOT_TOKEN`/`SLACK_APP_TOKEN`/`UZI_PUBLIC_BASE_URL`.
     `GET /api/admin/settings` reports a per-key `source: "env"|"db"|"default"`; `PUT`
     returns `409` on a write to an env-sourced key, so the webui's greyed-out fields
     reflect enforced server policy, not just a UI hint.
- **AI decision, diverges from the PRD's "thread `config.Config` into the settings
  handler"**: the ENV overlay and the decrypt accessors instead live on the settings
  cache itself via a one-shot `ConfigureSecrets(box, env)` call at boot, so
  env-over-db-over-default precedence has exactly one implementation shared by all three
  readers (GET source-reporting, decrypt accessors, `slacksvc`'s own token reads) instead
  of being re-derived at the handler layer. Reviewed and accepted as the better placement.
- Saving a token **validates live** before it is stored: `auth.test` for the bot token,
  a Socket Mode handshake (`apps.connections.open`) for the app token — mirroring
  multica's bring-your-own-app pattern — and a validation failure never echoes the
  submitted token back.
- Non-secret keys `slack_enabled` (default `"false"`) and `public_base_url` (default
  `http://127.0.0.1:8080`, `http(s)`-only) follow the existing PRDLESS-key precedent: no
  seeded row, synthesized from `Defaults` when absent.
- **`settings.LabelChanged` was narrowed to a whitelist** (`prd_label`/`autopilot_label`):
  previously any settings write triggered a repo resync, so a Slack-token or `slack_enabled`
  write would needlessly force one. Only the two label keys now do.

## 144. Persistence: per-user linking columns + `slack_run_messages` anchor

Serves human: "auto-matches each user by account email... manual Slack member-ID
override" (Decision Log, user-confirmed).

- Four columns added to `users` (migration `00044_slack.sql`, following the
  column-on-`users` precedent set by `default_model`/`autopilot_enabled`/`theme` — no
  `user_settings` table exists): `slack_member_id` (manual override), `slack_notify`
  (per-user kill switch, default `true`), `slack_resolved_id` (the effective linked id:
  the override if set, else the cached email-lookup result), `slack_link_confirmed_at`
  (`NULL` = unconfirmed, no content flows).
- **`users_slack_resolved_id_key`**, a partial unique index (`WHERE slack_resolved_id IS
  NOT NULL`): the structural backstop that makes "exactly one uzi user per Slack
  identity" true regardless of application-layer bugs, and what turns a colliding manual
  override into a `409` rather than a silent second mapping.
- **`slack_run_messages`** (`run_id` PK, cascades on run delete): one row per notified
  run — `channel_id`/`root_ts` anchor the DM thread, `gate_ts`/`gate_state` (`NULL |
  'open' | 'reject_pending'`) track a live approval gate for M4/M5's cross-surface
  idempotency.

## 145. Identity mapping is the authz primitive

Serves human: "no inbound Slack action can affect a run whose owner isn't the
confirmed-linked Slack user... an ambiguous or unconfirmed link refuses rather than
guesses" (PRD Success Criteria; security-review-driven, since uzi emails are unverified
at registration).

- Every inbound handler (Gatekeeper, Replier) resolves the actor from the Socket Mode
  envelope's **authenticated** user id (`callback.User.ID` / `ev.User`) — never a value
  read out of a forgeable payload blob — through `GetConfirmedUserBySlackID`, which
  additionally requires `slack_link_confirmed_at IS NOT NULL` and `is_active = true`. An
  unconfirmed link, a cleared link, or a deactivated account all resolve to zero rows,
  and the action is refused with an ephemeral notice rather than a best-guess match.
- **Confirmation round-trip, not the email match itself, is what makes the mapping
  trustworthy**: an auto-matched or overridden id sends a one-time Confirm/Not-me DM
  (Block Kit), and no run content flows until the target presses Confirm. Squatting a
  uzi account's email routes nothing anywhere until the *actual* Slack owner of that
  address confirms a DM that names the uzi account by label.
- **AI decision (defect fix in the minimal design)**: a manual override alone would have
  been a dead end without a paired confirmation DM (`SetUserSlackOverride` resets
  `slack_link_confirmed_at` unconditionally, and auto-match skips override'd users by
  design) — so setting an override also (re)sends the Confirm card to the new target.
- Every action that actually changes a run additionally rides `workersvc.SubmitInput`'s
  own ownership check (`GetRunByIDForUser`) as a second, independent gate — identity
  resolution authorizes *which uzi user* is acting; `SubmitInput` authorizes *that user
  against that specific run*.
- **Email auto-match runs as the manager's on-connected hook**, not per request: once per
  socket session it compares each user's email-resolved id and writes only on a change (an
  unchanged match never resets `slack_link_confirmed_at`), with a 10-minute cooldown, and
  stops early if Slack rate-limits — so a reconnect storm can't hammer `users.lookupByEmail`.

## 146. Notifier: content-minimized DMs, non-blocking fan-out, sweeper coverage

Serves human: "state transitions... arrive as Slack DMs... status + issue title + link
only" (Decision Log; content minimization is a security-review requirement).

- **`slacksvc.Notifier` implements `workersvc.Broadcaster`**, wired via
  `workersvc.MultiBroadcaster{liveHub, slackNotifier}` alongside the existing WS hub —
  `PublishState` enqueues to an internal channel and returns immediately (never blocks
  the request path, honoring the Broadcaster contract), with the run/owner load and the
  actual Slack calls happening on the notifier's own drain goroutine. A Slack failure is
  logged (redacted) and never affects the run.
- **One root message per run**, edited in place (`chat.update`) as status changes
  (▶ running, ⏸ needs you, ✅ MR link, ❌ failed: reason, 🚫 cancelled); every other event
  threads under it. No plan or diff content ever appears — status, issue title, MR URL,
  and failure reason only, each individually mrkdwn-escaped before interpolation (§149).
- **AI decision (fact-check finding, built into M3, not a later fix)**: the Broadcaster
  seam alone misses the sweeper's bulk timeout/requeue/stale-worker transitions, which
  run as row-count-only SQL and never touched the Broadcaster before this PRD — exactly
  the "failed" DMs users most need. The sweep queries were changed to `RETURNING id,
  status` (owner joined at publish time) and `Sweep` now publishes each transition
  through the same fan-out, which incidentally also fixed the web UI's board column
  getting stuck in "In Progress" forever on a sweeper-driven failure — judged a
  latent-gap bugfix within the PRD's own "publish each transition through the same
  fan-out" scope, reviewer-flagged as a broadening and lead-accepted.
- **Back-pressure + fan-out scope**: the enqueue is a non-blocking send to a 256-deep buffer
  that **drops-and-warns when full** (Slack lag can never stall the run path), and
  `Broadcaster.PublishMessage` (per-run-message events) is a deliberate **no-op** — only
  *state* transitions notify, reinforcing content minimization.

## 147. Approval gate from Slack

Serves human: "the `awaiting_approval` message carries Approve/Reject buttons... Reject
prompts for a threaded reason" (Decision Log, user-confirmed, refined after design
review).

- **Block Kit gate message**, no plan excerpt: Approve (primary, native confirm dialog),
  Reject, and an Open-in-uzi link button, each carrying the run id in its button value.
  The Socket Mode receive loop **ACKs the envelope before any processing** (Slack
  re-delivers an un-ACKed envelope in ~3s).
- **`SubmitInput` does not itself verify `awaiting_approval` before an approve**, so
  `Gatekeeper` reads `run.Status` itself first and answers a stale click with "already
  handled in {state}" rather than letting a second Approve resubmit.
- **Reject enters a `reject_pending` state** (recorded on `slack_run_messages`) instead
  of submitting immediately, since `reject_plan` is terminal and feedback can't follow
  it: the buttons swap for a "reply in this thread with the reason, or Reject without
  reason" card (webui parity — the webui's own reject also takes a reason).
- **Cross-surface idempotency**: resolving the gate from *either* surface (a Slack
  button, or the webui) edits the Slack message to a button-free terminal state and
  clears `gate_ts`/`gate_state`, so the other surface's later action on the same gate is
  a no-op ephemeral, never a double-submit.
- Inbound dispatch runs through an **`InboundMux` with disjoint action-id namespaces**
  (`slack_link_*` for link-confirmation vs `slack_gate_*` for approve/reject), so the
  Gatekeeper and the confirmation handler can never cross-fire on a stray payload.
  Ephemerals use **`chat.postEphemeral`** (channel+user), not a stored `response_url`, so no
  per-message reply URL has to be persisted or expired.
- A double-click on Approve is a **benign micro-race**: both envelopes can pass the
  pre-submit `run.Status` read and enqueue an `approve_plan`, but the worker consumes only
  the first as the verdict — the second is a dead input, not a second approval.

## 148. Reply-from-Slack

Serves human: "Threaded replies on a live (non-gate) run become `follow_up` inputs"
(Decision Log, user-confirmed).

- A thread reply re-resolves the confirmed author (§145) and the run anchored at the
  thread (`channel_id`+`root_ts` — effectively unique per run, since each run posts its
  own root message), then re-checks **ownership** of that run via the same
  `PlanGateSubmitter.GetRun` the gatekeeper uses — never trusting the thread anchor or
  `gate_state` alone. Routing: a reply while `reject_pending` **is** the reasoned reject;
  a bare reply during an *open* (not yet reject-pending) gate is nudged, not submitted,
  since the worker does not consume a `follow_up` mid-gate (verified against
  `agent/src/steering.ts`, per the PRD's own M4 caveat); otherwise it becomes a
  `follow_up` on a live run, or a coalesced "already finished" notice on a terminal one.
- Every accepted reply gets a ✅ (`reactions.add`) ack; unlinked-author and
  already-finished notices are **coalesced** (at most once per 10 minutes per author+kind)
  so a burst of replies is answered once, not per event.
- **Two independent rate limits, not one**: a per-Slack-user flood window
  (20 events/minute) on the inbound `message.im` stream, plus (a fast-follow promoted out
  of M5 into an M3 blocking fix after audit) a dedicated per-user `SLACK_DM_RATE_LIMIT_MAX`/
  `_WINDOW` limiter (default 6/minute) on the two outbound-DM-triggering `/me/slack`
  endpoints (`override`, `test-dm`), plus a 30-second per-target DM cooldown in
  `slacksvc.Linker` — bounding both an arbitrary-member spam primitive and the endpoints'
  accepted-residual member-id enumeration oracle (a 409 or a 200-vs-502 is deliberate,
  PRD-specified UX, so only the *rate*, not the *existence*, of the oracle is closed).
- **Stale-reply guard**: the reject-pending branch additionally re-checks
  `run.Status == 'awaiting_approval'` (never `gate_state` alone) — a reply arriving after the
  gate already resolved elsewhere falls through to the `follow_up`/finished paths instead of
  forcing a wrongful `running → failed` via the server-side reject. Inbound reply text is
  trimmed → `ScrubSecrets` → **capped at 2000 runes**, worker-bound only, never echoed back
  to Slack.

## 149. mrkdwn injection hardening + outbound secret scrub

Serves human: best-practice (audit finding: forge/worker-controlled text must not
smuggle Slack markup into a trusted message).

- **AI decision (audit finding, fixed same-milestone, not deferred)**: every dynamic
  field that could carry forge- or worker-controlled text — issue title, repo path,
  failure reason, MR URL, the link-confirmation DM's account label — is passed through
  `EscapeMrkdwn` (Slack's own control-character escaper) **individually**, never applied
  to the whole rendered string (which would also break the trusted `<url|label>` deep-link
  markup the notifier constructs itself). `failure_reason` is additionally bounded to 500
  runes before it reaches a message.
- **`ScrubSecrets`**, orthogonal to the escaping above, strips `sk-ant-`/`glpat-`/
  `xoxb-`/`xapp-` patterns from every outbound string as defense-in-depth, on top of a
  separate `Redact` (used only for logging) that additionally scrubs the Socket Mode
  `wss://…?ticket=…` connection URL.
- This scrub discipline is standing policy, not a one-off patch: every later outbound
  Slack surface (the M4 gate messages, the M5 reply acks/ephemerals) is built on the same
  two functions rather than reinventing escaping per call site.

## 150. Web UI + configuration

Serves human: "webui field renders greyed out... a 'send test DM' button" (Decision Log,
user-confirmed).

- **Admin → Instance settings → Slack card**: enable toggle, write-only bot/app token
  fields (`configured ✓` when a value is already stored, never re-displaying it), public
  base URL, and a live status chip; any field whose value comes from the environment
  renders disabled with a "set from environment" hint, matching the server-enforced `409`.
- **Settings → Notifications** (own-user only, via `mw.UserFromContext` throughout — no
  user can touch another's mapping): link status (unlinked / pending confirmation /
  confirmed), the per-user notify toggle, the manual member-ID override field, and a
  Send-test-DM button.
- `GetMySlack`/`PutMySlackNotify`/`PutMySlackOverride`/`PostMySlackTestDM` under
  `/api/me/slack`; `GetAdminSlackStatus` under `/api/admin/slack/status` as a cheap
  separate poll target so the status chip doesn't have to re-fetch the whole settings
  blob every few seconds.
- **AI decision**: `GET /me/slack` stays pure-DB (no live `users.info` call for a display
  name), so a settings page load never degrades when Slack itself is down; a
  resolvable display name is deferred as a follow-up.
- Configuration surface: `SLACK_BOT_TOKEN`/`SLACK_APP_TOKEN`/`UZI_PUBLIC_BASE_URL` (ENV
  overlay, §143), `SLACK_HTTP_TIMEOUT` (default `15s`), `SLACK_DM_RATE_LIMIT_MAX`/
  `_WINDOW` (default `6`/`1m`, §148) — extends the existing configuration doc/table
  pattern (§13/§25/§131), no new mechanism.

## 151. App manifest scopes verified against the implemented code (M7)

Serves human: best-practice — a paste-ready manifest should grant exactly what the bot
uses, not what a draft guessed it would need.

- The PRD's M7 milestone text listed `chat:write`, `im:write`, `im:history`, `users:read`,
  `users:read.email` as the bot scopes. Checked against every `slack-go` call actually
  made (`poster.go`, `linker.go`, `validate.go`): `users:read` is unused (only
  `users.lookupByEmail`, scoped by `users:read.email` alone, is called — no
  `users.info`/`users.list`), while `reactions:write` (`reactions.add`, the M5 ✅ ack) was
  missing from every scope list in the PRD.
- **Correction (2026-07-10, found configuring a real workspace)**: dropping `users:read`
  was wrong at the *manifest* level. Slack rejects a manifest requesting
  `users:read.email` without `users:read` ("Missing bot extension scopes" —
  `users:read.email` is an extension scope that must be requested together with
  `users:read`, per its scope reference page). API-call-level analysis was correct but
  irrelevant: the constraint is OAuth-install-time, not call-time. `docs/slack.md`'s
  manifest carries the working set: `chat:write`, `im:write`, `im:history`, `users:read`,
  `users:read.email`, `reactions:write`.
- Out of scope (PRD, deliberate): channel/broadcast notifications, an Events API/public-URL
  mode, slash commands, multiple workspaces, non-Slack notification providers (the
  notifier sits behind the `Broadcaster` seam, so one slots in later without rework), and
  email verification at registration (would strengthen auto-match, but the confirmation
  round-trip already makes it safe without it).

## 152. Testing (M6)

Serves human: best-practice — the authz + seal invariants above must hold against a real
Postgres, and the whole integration must be a strict no-op when unconfigured.

- The **live-DB store integration tests** (`run-store-it.sh`) carry the authz + seal proofs:
  exactly-one-user link resolution and its collision refusal, the `is_active` inbound refusal,
  and a **seal-at-rest raw-`SELECT` byte-check** that `slack_bot_token`'s stored value is
  ciphertext, never the plaintext token.
- The byte-check lives in the **store IT, not an HTTP e2e**, because the `PUT` live-validates
  the token against Slack `auth.test`, which an offline gate can't pass; the complementary
  "admin GET never returns token bytes" half is asserted at the unit level instead.
- **e2e stays unchanged-green with Slack disabled** (the default) — the strict-no-op
  acceptance criterion: an unconfigured instance must behave exactly as it did before the
  feature.

## 153. Deferred / follow-up candidates (recorded, not implemented)

- Live Slack **display-name** resolution in `GET /me/slack` (kept pure-DB, §150).
- **Backoff-reset-on-flap gating** (an M2 reviewer nit): a socket that connects then drops
  immediately still resets its backoff.
- Requeue-swept runs currently render as **"queued"** in DMs (a "↻ requeued" affordance or
  suppression is the follow-up).
- A **redundant `OpenDM`** on non-first transitions, and a rare **duplicate root message** if
  the `slack_run_messages` anchor upsert fails after the post — both benign.
- The **webui steering-input body is uncapped/unscrubbed** (pre-existing; the Slack reply path
  is the stricter one, §148) — out of this PRD's scope.
- **Worker-side `failure_reason` sanitization at source** (today the notifier bounds it to
  500 runes and escapes it, §149).
- A **tighter dedicated DM budget** (~5/min) for the two `/me/slack` DM routes, below the
  shared forge limiter — a Low follow-up from the DM-abuse audit (§148).

# PRD #18 — Worker templates (git-curated) + devbox tool tiers + agent template scopes

Serves human plan.md lines 44/64/81 (per-user/per-repo toolchains; "command not
found" surfacing; global/user agent scoping with allocation). Three tracks on one
mental model (the PRD #16 scope+allocation pattern): curated worker image templates
in git, per-repo devbox tool tiers, and agent-template scopes+allocation. Section
numbers continue past PRD #25's #153. Builds on PRD #4 (worker runtime, claim
payload, guardrails), #16 (skills scope+allocation shapes reused here), and #17
(claim plumbing, decoupled builtins). Migrations landed `00045`–`00049` (renumbered above main's `00044_slack`; prior live head
was `00043`). Full rationale in `prds/18-worker-templates-and-agent-scopes.md`;
user guides in `docs/worker-setup.md` + `docs/worker-tools.md` + `docs/agent-templates.md`.

## 154. Worker image templates in git (agent + compose)

Serves human: "one might need node tools, other might need java tools" (plan.md 81).

- **`agent/templates/<name>/Dockerfile`**, each self-contained (not a shared base
  stage — the M3 provisioning stack is MIRRORED across every template, kept in
  lockstep by a test). `base` (node22+git+bash) and `jvm` (base + JDK) ship. Variants
  exist ONLY for heavy/system deps devbox/nix handles poorly; per-repo CLI tools are
  Track 2's job (the two tracks never solve the same problem twice).
- **`WORKER_TEMPLATE` build arg** selects `agent/templates/<name>/Dockerfile` (compose
  `agent.build.dockerfile`); unset ⇒ `base`. A bare name only (interpolated into the
  path; `/`/`..`/absolute would escape `agent/templates/`).
- **Image identity is baked, distinct from the build arg.** Each Dockerfile bakes its
  own name as a fixed literal `UZI_WORKER_TEMPLATE` (NOT the `WORKER_TEMPLATE` build
  var), and the worker reports THAT at register. So the reported value is the image's
  own identity — it flags a real mismatch when you build one template but declared
  another at token issuance.
- **`workers.template_declared` (UI, at join-token issuance) + `workers.template_reported`
  (register)** (migration `00045`). Register payload gains optional `template`
  (`agent/src/client.ts` / `WorkerRegister`); the decode struct widened BEFORE any
  worker sends it (DecodeJSON rejects unknown fields). **A malformed/blank reported
  template drops to NULL** rather than persisting junk.
- **Soft verification only** (Decision 5): declared-vs-reported drift is surfaced
  (admin + owner), never rejected — a hostile worker can lie, and legitimate local
  builds must not break. The join token stays the sole authn boundary.

## 155. Devbox provisioning engine in the worker (M3)

Serves human: per-repo toolchains; "command not found" surfacing (plan.md 44/64).

- **Alpine/musl base KEPT** (nix works on it — tester-verified). **Build-time PINNED
  installs only**, no floating `curl|bash`, no runtime download: Determinate
  nix-installer (single-user, `--init none`, `chown -R uzi /nix`, `NIX_REMOTE=""`) +
  devbox release binary at mode 0755 (the launcher's 0711 root-owned blocked non-owner
  exec). `/nix` stays at its DEFAULT path (relocating forfeits cache.nixos.org
  substitution) and is persisted by the `agentnix:/nix` compose volume; devbox/nix
  per-user metadata lands HOME-derived under `/data` (`agentdata`).
- **`ARG TARGETARCH`** drives arch (amd64→x86_64, arm64→aarch64). **Devbox verified
  against the release `checksums.txt`** (tag-pinned artifact, not a hardcoded sha —
  lead-accepted tradeoff): tarball saved under its EXACT release filename so
  `sha256sum -c` finds it, and the `grep|sha256sum` pipe runs under **`set -o pipefail`**
  (BusyBox `sha256sum -c -` exits 0 on empty stdin, so a no-match grep would else skip
  verification silently — audit L2).
- **Provisioning at claim time, before the SDK**: resolve manifest by precedence →
  synthesize `devbox.json` in a per-run dir OUTSIDE the clone → `devbox install` in a
  **secret-scrubbed subprocess** (no PAT, Anthropic token, or join-token path —
  Decision 3, mirroring `buildSdkEnv`) → export `devbox shellenv` filtered through an
  **explicit variable allowlist** (`PATH` prepend + nix TLS/locale vars only, never a
  blind merge) into the SDK env. **Provision failure FAILS the run** (missing package,
  allowlist reject) — never silent degradation.
- **New egress**: nix substituters (cache.nixos.org). ARCHITECTURE.md's "outbound-only
  to api" worker description amended accordingly.

## 156. Tool profiles + admin allowlist (M4)

Serves human: allowlist-bounded per-repo tools.

- **Tables** (`00046`): `tool_allowlist` (admin CRUD: exact package base name (an
  allowlist map key matched by lookup, no globs) + optional pinned-version policy;
  seeded with the M3 default package set) and `repo_tool_profiles`
  (user_id, repo_id, packages JSONB, unique per pair). **Tier 1 stores a package LIST,
  not a `devbox.json`** (Decision 2): a raw manifest permits `shell.init_hook`/`scripts`
  (arbitrary provision-time shell); a declarative allowlist-validated list is safe to
  offer non-admins.
- **`api/internal/toolprofile` is the single validation seam**: pure over a `Rules` map;
  `RulesFromRows` is the ONE loader used at BOTH write time (handler) and claim time
  (workersvc) — allowlist may have shrunk since save, so both re-validate. Includes a
  **credential-CLI DENYLIST** (Decision 6: a pre-authenticated `glab`/kubeconfig would
  bypass "worker holds the PAT, agent doesn't") and a **128-char cap**.
- **Claim payload** gains `tool_packages []string` (resolved tier-1 list) + `repo_devbox_opt_in bool`.
- **Web**: repo **Tools** panel (Boards page) = allowlist-backed package picker;
  **Admin → Tool allowlist** page for the allowlist CRUD.

## 157. Tier-2 repo `devbox.json` opt-in (M5)

Serves human: repo-carried toolchains, safely.

- **`repos.repo_devbox_opt_in BOOLEAN NOT NULL DEFAULT false`** (`00047`); per-repo
  trust toggle in the Tools panel. **Packages-only extraction** (`agent/src/repo-tools.ts`):
  only the `packages` field is read, shape-validated (name / name@version); `init_hook`,
  `scripts`, flake refs, and every other key are ignored and NEVER executed — pure JSON
  parse, no `devbox` invocation on the repo file. A **1 MiB stat-guard** rejects an
  oversized manifest before reading it (the length/count caps only bite post-read —
  audit L1).
- **Union-merge with tier-1 precedence** (`mergeToolPackages`): tier-1 wins a version
  conflict on the same base name; two tier-2 versions of one base dedup to the first
  (audit ride-along).
- **Tier-2 bypasses BOTH the allowlist AND the credential-CLI denylist** — the PRD
  posture, **audit-ACCEPTED** (probed, no concrete escalation): bounded by opt-in +
  packages-only, and the actual security control is Decision 3's secret-scrubbed
  provisioning env (a freshly nix-installed CLI holds no credentials; toolEnv folds only
  into the SDK env, never the worker's PAT-bearing process). NOT re-hardened.

## 158. Agent template scopes migration + user CRUD (M6)

Serves human plan.md 44 (global/user agents), extended from skills to the templates.

- **`agent_templates` grows `scope` (builtin|global|user) + `user_id`** (`00048`),
  mirroring skills §99: backfill `scope='builtin' where is_builtin`; drop the flat
  `UNIQUE(name)`; add the two partial uniques (`uq_agent_templates_shared_name (name)
  WHERE scope<>'user'`, `uq_agent_templates_user_name (user_id,name) WHERE scope='user'`).
- **`is_builtin` KEPT as a compat column** (Decision 9): rather than churn every
  is_builtin consumer, a CHECK `is_builtin = (scope='builtin')` ties the two so they
  can never drift; builtin seeding + ResetAgentTemplate behavior unchanged.
- **Reconciler conflict-target fix in the SAME migration commit**: the builtin seed's
  `ON CONFLICT (name) DO NOTHING` is INVALID against the partial index — changed to
  `ON CONFLICT (name) WHERE scope<>'user'` (+ sets `scope='builtin'`), so a user's
  same-name template can never block a builtin seed at boot. The shadow-warning
  read-back is likewise scoped (`GetSharedAgentTemplateByName`, `WHERE name=$1 AND
  scope<>'user'`) so a user row can't win the QueryRow and emit a false "shadowed" warn.
- **Handlers mirror the skills authz matrix**: viewer-scoped list/get (builtin∪global∪own;
  admins all), per-row write authz (builtin/global admin-only, user owner-only; a
  non-owner non-admin gets 404 to hide existence). User CRUD for `scope='user'` templates
  leaves the blanket `RequireAdmin` group.
- **Reserved-name check extended to global creates, not just user** (deliberate
  extension of Decision 8): the binding invariant is the worker-side "a claim never
  carries two LEAD_NAME_RE matches" pin, which only holds if NO API-created template
  (global or user) may take a lead name — the seeded builtin `lead` is the sole
  legitimate one. Enforced at create (name is immutable, so no rename path exists).
- **Blank scope on create defaults to `global`** (back-compat): the pre-M6 admin create
  sent no scope field; the "my agents" flow sends `scope='user'` explicitly.

## 159. Template allocation + claim filtering (M7)

Serves human: per-agent allocation (which templates ride which runs).

- **`agent_template_allocations`** (`00049`), same shape as `agent_skill_allocations`
  but the overlay carries an **`enabled` flag** (a user must be able to both ADD a
  template and REMOVE a global default from their own runs — skills only ever add).
  Row identity `uq (template_id, COALESCE(user_id,'0000…'))`; `user_id NULL` = global
  default (admin, always enabled=true), non-NULL = the user's signed overlay.
- **No empty-means-all cliff** (review m5): the migration seeds a global-default row
  for every existing builtin/global; the reconciler seeds a builtin's row the FIRST
  time it inserts that builtin (gated on the insert, so an admin's later removal is
  never re-added on the next boot); the global-create handler seeds a new global's row
  in-tx. Absence of a shared row is thus always a deliberate removal; absence of a user
  overlay means "follow the global default set".
- **Claim resolution** (`ListClaimAgentTemplates`, replaces the all-templates read):
  visible to the run owner AND resolved-as-allocated — a user overlay (enabled) wins,
  else the global default, else dropped. Delivers builtin∪global defaults ± the owner's
  overlay + the owner's own allocated user templates; NEVER another user's row (the M6
  audit's confidentiality criterion).
- **Shared-precedence claim drop for name collisions** (M7 audit acceptance criterion,
  deliberate divergence from skills' user>global>builtin BODY precedence): a user
  template whose name exists in the shared namespace is DROPPED from the claim entirely.
  The worker keys subagents by name with no scope tiebreak, so the curated shared
  subagent must survive (lead routing delegates to `coder` etc. by name); SHARED always
  wins. With this drop + the two partial uniques, every delivered claim name is unique,
  so the worker never sees a collision. Surfaced in the UI as a `shadowed` badge.
- **Lead toggle-off is graceful** (Decision, lead-accepted): the lead is a normal
  template in the allocation set; a user CAN disable it. The worker degrades to the
  hardcoded `LEAD_GUARDRAIL_APPEND` (guardrails intact, just no custom lead body) —
  never a broken run, no pin needed.
- **Endpoints**: `GET/PUT /agent-templates/allocations` — the per-template toggle view;
  the PUT is a replace-set with an admin global-default half + the caller's overlay
  half (per-half authz). Worker needs no routing changes (`assembleAgents` consumes what
  the claim delivers). Claim WIRE shape unchanged (only which templates ride it), so the
  goldens stand.

---

# PRD #33 — Board–Run follow-ups: MR-state surfacing, deliberate-stop signal, multi-user hardening, e2e guard

Serves human Feature #12 (board–run lifecycle: "run badges + MR link on the card") and
Feature #14 (run-status language unified across all surfaces), plus the best-practice
bar (multi-user least-disclosure; e2e robustness). **The single user-stated decision
here (2026-07-10) is to consolidate the four self-contained follow-ups issue #15
recorded while shipping PRD #12 into ONE PRD / ONE MR** — the four items themselves were
AI-surfaced, not user-requested, so `specs/human.md` gains no new requirement. Status:
designed + implemented on `feature/prd-33-board-run-followups`; pre-reviewed by
design-review + fact-check (blocking findings folded: the transactional stamp §161 and
the terminal guard §162). Updates §72 (owner_name / run_count), supersedes §74's client
string heuristic (→ server-stamped `stop_kind`), and realizes §92's noted-but-unbuilt
`mr_state`-on-the-runs-API chip. Migration landed `00050` (renumbered above main's new
head `00049` at land, goose convention).

## 160. MR-state surfacing — display-only, best-effort chip (api handler + web)

Serves human: Feature #12 board card MR signal; Feature #14 unified surfaces. Realizes
§92's deferred nice-to-have without touching PRD #24's watcher.

- **`runs.mr_state` (PRD #24's watcher column, §89) is *display-only* surfacing — no new
  polling, no watcher/candidate-set widening.** The watcher maintains `mr_state` only
  for its watch candidates (the latest completed run while in Human Review or under
  reopen-watch, §89/§91); widening that set for cosmetic freshness would break PRD #24's
  cost bound (§91). The chip therefore treats `mr_state` as a *hint*: NULL renders
  exactly today's plain `MR !N`.
- **Freshness holds only for the board card** (the issue's latest run, kept watched).
  Per-run history rows (issue view, runs list, run view, dashboard) render each run's
  *frozen* `mr_state` "as of last sync": once a rework run supersedes a completed run the
  watcher never revisits the old run, so a superseded run's chip can read a stale
  `closed`. Only stale `closed` misleads; `merged` is terminal. The chip `title` says "as
  of last sync"; docs scope the freshness claim to the board card and do not oversell
  `merged` (a merge usually closes the issue and drops it from candidates before `merged`
  is ever observed).
- **Derived-status enum, never raw forge strings** (multica's PR-status pattern, the
  prior art §92 named): one pure web helper `mrChipState(mr_state) → open | merged |
  closed`, where unknown / `opened` / `locked` / NULL all collapse to `open`,
  unit-tested like the rest of `runBadge.ts`. `merged` → ok-toned "MR !N merged";
  `closed` → muted/struck "MR !N closed"; `open` → today's chip. Applied at **all five**
  MR-chip surfaces (board card, issue-view run history, runs list, dashboard, run view)
  so no surface ever renders a raw forge state string.
- **Plumbing**: `mr_state` added to the latest-run DTO (`handler/board.go`) and the
  per-run DTO (`runToDTO`, `handler/workers.go`), sourced from the run-returning queries
  (sqlc regenerated) and mirrored into the web `LatestRun` / run-row types. Store
  unchanged — the column already exists (migration `00029`).

## 161. Deliberate-stop signal — server-stamped `runs.stop_kind`, not a new status

Serves human: Feature #14 ("a deliberate human stop is not breakage" — neutral, never
rose, across all surfaces). Supersedes §74's client string heuristic.

- **New nullable column `runs.stop_kind` (`cancelled` | `plan_rejected`), NOT a new run
  status.** A new status would touch the state machine, the sweeper, the claim gate,
  terminal-status checks, and every status switch in api/web/agent for what is purely
  presentation semantics; the status stays `failed` / `cancelled` exactly as before. The
  column records the *server's intent*, independent of whatever `failure_reason` string
  the worker later reports.
- **Stamped server-side at the moment the server knows the verdict** (all in
  `workersvc`):
  - **The live-poller reject/cancel path has no status write** — it enqueues the verdict
    via the shared `CreateRunInput` insert. The stamp MUST land in the *same statement*
    as that insert: a dedicated `CreateStopVerdictInput` CTE (unconditional UPDATE +
    INSERT, one statement) used only by the live cancel/reject branch. **Non-negotiable
    (blocking review finding): a second, non-transactional statement whose loss would
    reintroduce the failed-vs-stopped bug is forbidden.** `CreateRunInput` stays a plain
    INSERT for approve/follow_up (no runs-row lock on the hot path).
  - **The server-side no-poller reject** stamps `plan_rejected` alongside its existing
    `FailureReason` write; the **existing server-side cancel path** (status `cancelled`)
    stamps `cancelled` for uniformity (the client already treats status `cancelled` as
    stopped).
  - **Sweeper timeout/requeue failures never stamp it** — a timeout is not a deliberate
    human stop. Worker-reported `SetRunFailed` never stamps it either; if the run had a
    pending stop verdict, the verdict site already did.
- **Agent unchanged**: no worker-protocol change; the worker keeps reporting whatever
  `failure_reason` it has and the server signal is authoritative.

## 162. Client stopped-vs-failed reclassification, backfill, and the terminal guard

Serves human: the same Feature #14 neutral-stop styling, now server-driven end to end.

- **`isStoppedRun(status, stopKind) = status === 'cancelled' || (status === 'failed' &&
  stopKind != null)`** replaces §74's `STOPPED_FAILURE_REASONS` string set entirely
  (deleted, including the `runBadge.ts` known-limitation comment block) — issue #15's
  stated bar. The previously-uncatchable live-poller plan reject carrying the user's
  *verbatim* reason now renders a neutral "stopped".
- **The `failed` guard is load-bearing (blocking review finding), not defensive
  boilerplate.** On the live path `stop_kind` is stamped at verdict *enqueue* while the
  run is still `awaiting_approval` / `running`, and a reject-then-approve race (latest
  verdict wins) can even *complete* a run that carries `stop_kind`. Without the terminal
  guard those would render "stopped" prematurely or suppress a completed run's MR chip.
  Two dedicated web tests pin it (stamped-but-still-running renders by status;
  stamped-but-completed renders the MR chip).
- **Backfill covers only the two exact literals**: `stop_kind` is set where
  `failure_reason IN ('run cancelled','plan rejected')` (the only literals a deliberate
  stop ever persisted). Historical rejects that carried a verbatim reason are
  indistinguishable from real failures after the fact and **stay "failed" — accepted**;
  the set is small and shrinking. Server-side cancels write status `cancelled` with no
  `failure_reason` and are deliberately not backfilled (`isStoppedRun`'s `cancelled`
  branch already covers them).
- **Accepted edge (reviewer, non-blocking): `stop_kind` is never cleared on requeue.** A
  stamped-then-requeued run that later genuinely fails renders "stopped"; accepted as
  semantically fine — the user did ask to stop it.

## 163. Multi-user board hardening — no email leak, owner-gated failure_reason, issue-scoped run_count

Serves human: best-practice / least-disclosure. Latent today (per-connection repos are
self-owned) but real once boards are shared. Supersedes §72's email-fallback revisit
item.

- **`owner_name` never contains an email.** The fallback chain becomes display-name →
  empty string (the web already renders a no-owner badge for empty). No sanitized email
  local-part — a handle derived from the email still leaks the identifier it was meant to
  hide. Email is **dropped from the board queries entirely** (never fetched), not merely
  omitted from the DTO. `owner_email` stays admin-list-only where it already is
  (`handler/runs.go`).
- **`failure_reason` on the board latest-run DTO is owner-gated** (auditor pre-flag,
  lead-accepted 2026-07-10): it can carry a user's verbatim typed reject reason or a raw
  agent error, so a non-owner viewer of a shared board gets `null` (owners keep it for
  the failed-badge tooltip). `stop_kind` — a non-sensitive enum — stays exposed to
  everyone, so stopped-vs-failed classification never needs the free-text field.
- **`run_count` stays issue-scoped (counts all users' runs), documented, not
  per-viewer.** The "×N" hint answers "how many times has this issue been run" — a
  board-level fact a shared board legitimately shows, exposing only a count and no
  identity. Per-viewer scoping would make the same card show different histories to
  different users and complicate the `DISTINCT ON` query for no confidentiality gain.
  Rationale commented at the window definition (`forge.sql`); revisit only if a real
  multi-tenant deployment objects.

## 164. e2e compose-project-name guard (`e2e/run-e2e.sh`)

Serves human: best-practice (the harness must fail loudly up front, not mid-run).

- **Validate the resolved `UZI_E2E_COMPOSE_PROJECT` unconditionally against compose's
  project-name rule** `^[a-z0-9][a-z0-9_-]*$` (lowercase alphanumerics, `-`, `_`, must
  start alphanumeric; verified against Compose v5.1.2's own error message) and exit 2
  with the offending value + the rule **before any scratch-dir / compose work**. A
  slashed explicit value (e.g. a branch-like `feature/prd-33`) otherwise makes docker
  reject the name mid-run, after setup has begun.
- **Reject, never rewrite.** An explicit export is user intent; silently sanitizing it
  would hide the mismatch from logs, teardown hints, and any concurrently running stack
  that would then reference a name the user never set. No provenance check needed — the
  PID default `uzi-e2e-$$` always passes the rule.

# PRD-less — Slack settings startup seed (`UZI_SEED_SLACK_*`)

Serves human: user-requested (2026-07-10, while configuring a real workspace): "seed
the DB from .env like we do for other stuff" — chose the seed option over extending
the ENV overlay or replacing it.

## 165. Create-only Slack seed alongside (not replacing) the ENV overlay

- **Third config path** for Slack, complementing §143's two: `UZI_SEED_SLACK_BOT_TOKEN`
  + `UZI_SEED_SLACK_APP_TOKEN` (+ optional `UZI_SEED_PUBLIC_BASE_URL`) write the
  app_settings rows at boot — tokens sealed via the SAME write-side seam the settings
  PUT uses (`settings.ValueForStorage`), `slack_enabled` flipped `"true"` in the same
  pass, `updated_by` NULL (no user performed the write). §143's "ENV wins" overlay is
  untouched: a seeded value is an ordinary DB row, UI-rotatable, later `.env` edits
  ignored while the row exists.
- **Create-only PER KEY** (`seed.SlackSettings`, `api/internal/seed/slack.go`): a row
  that exists is never overwritten — so an admin's UI rotation survives restarts, and
  crucially an admin's later `slack_enabled=false` flip is never re-enabled by the seed.
  Only a fresh `down -v` re-seeds. Runs in main after the Anthropic seed, before the
  socket-manager goroutine starts; the settings cache is invalidated right after.
- **Seed × overlay per key is boot-fatal** (config.Load, `loadSeedSlack`): the overlay
  wins over the DB on every read, so a seeded row under an overlay would be dead weight
  silently diverging from what runs — loud misconfiguration beats silent shadowing.
  Also boot-fatal: a half pair (both tokens or neither) and a wrong prefix
  (`xoxb-`/`xapp-` — cheapest catch for swapped tokens). Error text names variables
  only, never token bytes.
- **No live Slack validation at seed time** (mirrors the Anthropic seed's no-network
  stance, diverges from the settings PUT's `auth.test`/handshake): a network call must
  not gate boot; a bad seeded token surfaces exactly like a bad UI-saved one — the
  manager's failed connect on the admin status chip.
- Unlike the other seeds, requires no `UZI_SEED_EMAIL`: app_settings is instance-wide,
  not user-owned.

---

## 166. As-built validation — PRD #32 vault (tests & e2e)

Serves human: the success criteria are demonstrable, not just asserted.

- **Unit tests** (`api/internal/vault/vault_test.go`) cover wrap/unwrap roundtrip, wrong
  password ⇒ GCM auth failure ⇒ `ErrWrongPassword`, the master key cannot open a DEK-sealed
  row, `UnlockExisting` returns `ErrNoVault` (never creates), and the first-unlock race
  (concurrent creators converge on one persisted DEK). Handler tests assert the identical
  403 on wrong-password vs no-vault.
- **e2e** (`e2e/run-e2e.sh`) runs four vault scenarios end to end against the isolated
  stack: (1) the seeded token is DEK-sealed at boot and `/api/me` reports unlocked; a
  handler save also writes `sealed_with='dek'`; (2) lock → a new run stays `queued` across
  several poll cycles (never claimed, never failed) → unlock → it claims and reaches the
  plan gate; (3) restart the API → the seed admin is boot-unlocked while a normal user is
  locked (both JWTs survive; only the DEK cache is lost); (4) that user unlocks without
  re-login → lazy rewrap flips their staged legacy `'master'` row to `'dek'` and the admin
  migration count returns to 0.
- **Test seam for the legacy-row fixture**: scenario 4 stages a `sealed_with='master'` row
  sealed under the API's *actual* `UZI_SECRET_KEY`. Because Compose ranks the developer's
  shell env above `--env-file` (the repo's documented smoke hazard), the harness reads the
  key back out of the running api container via `docker inspect ... .Config.Env` rather than
  trusting the env it thinks it set, then AES-256-GCM-seals the fixture with it.

## 167. Socket Mode inbound was dead-on-arrival: the empty-envelope ack (found live 2026-07-10)

Serves human: PRD #25's Confirm/gate buttons and thread replies — none had ever worked
against real Slack (unit tests fake the dialer; e2e stubs Slack entirely).

- **Symptom**: outbound DMs fine, every inbound click/DM lost with a ⚠ in Slack; manager
  chip stuck `connected`; zero error logs. **Cause**: the receive loop's catch-all "ack
  any other envelope" also acked *hello* (its `Request` is non-nil but `EnvelopeID` is
  empty), sending `{"envelope_id":""}` — protocol garbage after which Slack never pinged
  the connection and dropped it ~10s later, forever. Every inbound event raced a dying
  socket. Fix: only ack envelopes with a non-empty `EnvelopeID` (`socket.go`).
- **Debugging affordances added** (all INFO, payload-free): inbound interactive receipt
  (callback type + action ids, never values), events-api receipt, `ErrorBadMessage`
  cause (redacted), unhandled envelope types, and `SLACK_SOCKET_DEBUG=1` (env-gated
  slack-go wire logging; leaks the wss `?ticket=` — diagnostic only, never in a shared
  deployment). The silent-receive design made this bug indistinguishable from "Slack
  sends nothing": three config-side red herrings (interactivity toggle, app_home
  messages tab, event subscriptions) were chased first.
- **Diagnostic ladder that found it** (for the next socket mystery): DB row (click
  persisted?) → admin chip (in-memory state) → receipt logs (envelope arrived?) → raw
  Node WebSocket probe on the same xapp token with the api stopped (Slack delivers at
  all?) → slack-go wire debug (the probe acked only non-empty envelope ids and lived;
  the api acked hello and died every 10s — a controlled A/B).
- **slack-go bumped v0.26.0 → v0.27.0** while diagnosing (socketmode identical; kept for
  the newer payload structs). The manifest in docs/slack.md also gained
  `features.app_home.messages_tab_enabled: true` + `messages_tab_read_only_enabled:
  false` — without them the DM shows "Sending messages to this app has been turned off"
  and M5 thread replies are impossible to type.
- **Open follow-up**: the manager trusts slack-go's RunContext to notice a dead link; a
  zombie connection (e.g. after laptop sleep) can leave the chip `connected` with no
  socket — consider a liveness probe (last-hello age) surfaced on the admin DTO.

---

# PRD-less — On-demand worker spawn: always-on for compose, spawn-on-demand deferred to k8s (design decision, 2026-07-10)

Serves human: discussed while designing a future in-app chat agent (chat rides the
run machinery, so it needs a live worker to answer; a user chatting in the web UI
should never have to know a worker exists). No PRD yet — recorded so the k8s phase
inherits the decision.

## 168. Worker availability: always-on on compose; launcher-spawned pods on k8s

- **Compose (now): the worker runs always-on; nothing spawns containers.** An idle
  worker is a Node process on a 3s poll — tens of MB RAM, zero Anthropic spend — so
  demand-spawning buys nothing on a laptop. Features that need a live worker (chat,
  runs) surface worker liveness (heartbeats already track it) as an explicit
  "worker offline" state instead of silently queueing forever.
- **Rejected: `api` spawning workers via `docker.sock`.** The socket is effectively
  root on the host; mounting it into `api` would wreck its posture (distroless,
  no shell, sole holder of `UZI_SECRET_KEY`/`JWT_SECRET`). `api` must never hold
  container-runtime credentials — this constraint carries to every deployment shape.
- **Rejected for compose: a dedicated launcher sidecar** (a small always-on service
  holding `docker.sock`, watching for queued work and starting/stopping the agent
  container). It isolates the privilege correctly but means building an orchestrator
  (join-token provisioning, template/image selection, lifecycle, crash handling) to
  avoid ~50MB of idle RAM, and the implementation is throwaway once workers move
  off compose.
- **k8s / remote-worker phase (the deferred design): the launcher shape, as an
  operator.** A dedicated controller with cluster-API access (never `api` itself)
  spawns per-user worker pods on demand — triggered by queued work (a run or chat
  session appearing) — and reaps idle ones. This composes with the
  [docs/proc-hardening.md](../docs/proc-hardening.md) remote-worker design (pod-level
  uid split worker/agent, TLS-terminated ingress in front of `api` for the join-token
  hop): the pods the operator spawns are exactly those two-container pods. Join-token
  issuance for spawned workers becomes the operator's job (machine-issued, not
  pasted-once-by-a-human), which is new design work to do then.
- Cross-references: ARCHITECTURE.md "Not yet in scope" (API-spawned workers) and
  specs/human.md "Deferred" both point here.
