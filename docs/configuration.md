# Configuration

All configuration is via environment variables, set in `.env` (copied from `.env.example`) and passed into the `api` container by `docker-compose.yml`. `POSTGRES_*` also configures the `db` service directly.

| Var | Default | Notes |
|---|---|---|
| `JWT_SECRET` | — (required) | HS256 signing key for session JWTs. The API refuses to start if it is missing, empty, shorter than 16 characters, or a known placeholder (`change-me`, `secret`, `password`, etc. — see `api/internal/config/config.go`). Generate with `openssl rand -hex 64`. |
| `POSTGRES_PASSWORD` | — (required) | Password for the bundled Postgres role. Generate with `openssl rand -hex 24`. Compose refuses to start without it. |
| `POSTGRES_USER` | `uzi` | Postgres role name. |
| `POSTGRES_DB` | `uzi` | Postgres database name. |
| `AUTH_TOKEN_TTL` | `168h` (7 days) | Session JWT lifetime and cookie `MaxAge`, as a Go duration. Rolling refresh (see [auth-design.md](auth-design.md)) slides this on every authenticated request past the halfway point, so active users are never logged out mid-session. |
| `FRONTEND_ORIGIN` | `http://127.0.0.1:8080` | User-facing origin. Its scheme decides the cookie `Secure` flag: `https://` makes cookies `Secure`, anything else does not. Use an `https://` origin behind TLS in production. |
| `RATE_LIMIT_MAX` | `10` | Max requests per window, per (route, client IP), for `/api/auth/register` and `/api/auth/login`. |
| `RATE_LIMIT_WINDOW` | `1m` | Fixed window for the rate limiter, as a Go duration. |
| `TRUSTED_PROXIES` | `10.0.0.0/8,172.16.0.0/12,10.244.0.0/16,127.0.0.1/32` | CIDRs whose direct connections are trusted to speak for a real client via `X-Forwarded-For`. Only requests whose `RemoteAddr` falls in one of these ranges get their `X-Forwarded-For` header honored; everyone else's is ignored. The compose default trusts the private compose network (the nginx hop) — see [auth-design.md](auth-design.md) for why this is safe here. Leave empty to never trust `X-Forwarded-For` and rely on `RemoteAddr` only. |
| `DATABASE_URL` | set by compose | pgx connection string, built from `POSTGRES_*` and the `db` service name. Not meant to be set directly when using compose. |
| `API_ADDR` | `:8080` | Address the `api` binary listens on inside its container. Set by compose; no need to change it. |

Invalid values for `AUTH_TOKEN_TTL`, `RATE_LIMIT_MAX`, `RATE_LIMIT_WINDOW` or an unparseable `TRUSTED_PROXIES` entry fall back to their defaults rather than failing boot (the last one is logged as a warning); only a bad `JWT_SECRET` or missing `DATABASE_URL` refuses to start.

There is no CORS configuration to make, by design: nginx serves the SPA and proxies `/api/*` to the API on the same origin (see [ARCHITECTURE.md](../ARCHITECTURE.md)), so the browser never makes a cross-origin request.

## Forge integration

See [gitlab-bot-setup.md](gitlab-bot-setup.md) for the bot-account procedure these variables support.

| Var | Default | Notes |
|---|---|---|
| `UZI_SECRET_KEY` | — (required) | base64-encoded 32-byte AES-256 master key that encrypts bot PATs at rest (`api/internal/secretbox`). The API refuses to start if it is missing, not valid base64, not exactly 32 bytes decoded, or a low-entropy placeholder (e.g. all-zero). Generate with `openssl rand -base64 32`. **Rotating this key invalidates every stored bot token** — every user must reconnect their PAT in Settings → Forge; there is no re-encrypt path in this MVP. |
| `FORGE_ALLOWED_BASE_URLS` | `https://gitlab.example.com` | Comma-separated SSRF allowlist: the only forge base URLs a connection may target. Every entry must be an absolute `https://` URL; boot fails if the list is empty or any entry is malformed or non-`https`. The Settings → Forge base-URL dropdown offers exactly this set. |
| `FORGE_POLL_INTERVAL` | `60s` (`1m`) | Per-enabled-repo incremental sync cadence (Go duration). An invalid or non-positive value falls back to the default. See "Freshness contract" below. |
| `FORGE_RECONCILE_EVERY` | `10` | Every Nth incremental poll is a full reconcile instead (fetches the complete `PRD`-labeled issue set with no `updated_after` bound, diffs, and evicts cache rows the forge no longer returns). A non-positive or unparseable value falls back to the default, same as the other numeric/duration vars here (the poller itself additionally clamps any value `< 1` to `1` — every poll becomes a full reconcile — as defense in depth, but that path isn't reachable through this env var). The very first poll after a repo is enabled is always a full reconcile regardless of this setting, since it has to seed the cache. |
| `FORGE_HTTP_TIMEOUT` | `15s` | Hard per-call timeout on every outbound HTTP request to the forge (connect, projects, labels, issues, label updates). Closes the untimeouted-`http.DefaultClient` wart the `multica` inspiration shipped with. |
| `FORGE_RATE_LIMIT_MAX` / `FORGE_RATE_LIMIT_WINDOW` | `30` / `1m` | Per-user request budget (not per-IP) on the forge-*proxying* endpoints only — connection verify, project listing, issue move, manual sync — so one user's connection can't hammer the upstream forge. Separate limiter and separate budget from `RATE_LIMIT_MAX`/`RATE_LIMIT_WINDOW`, which only cover `/api/auth/register` and `/api/auth/login`. |
| `UZI_SEED_EMAIL` | — (optional) | Setting this provisions an admin user automatically at startup, after migrations, if no user with that email already exists yet (an existing user's password is never touched — the seed is idempotent and safe to leave set across restarts). Must be a syntactically valid address or boot fails. Leave unset to disable seeding entirely (the normal first-registration-becomes-admin flow still applies). |
| `UZI_SEED_PASSWORD` | — (required if `UZI_SEED_EMAIL` is set) | Password for the seeded admin, hashed with the same argon2id parameters as normal registration. Must satisfy the same length policy as registration (12–1024 characters) or boot refuses to start — a set-but-invalid seed is a loud misconfiguration, not a silent skip. |
| `UZI_SEED_NAME` | — (optional) | Display name for the seeded admin. |
| `UZI_SEED_FORGE_PAT` | — (optional) | Setting this seeds a forge connection (owned by the seed admin) at startup. Requires `UZI_SEED_EMAIL` to be set, or boot fails. At boot, if the seed admin has no connection for the target base URL yet, the API verifies this bot PAT against the forge, stores it encrypted (same path as the Settings → Forge connect flow), lists the bot's projects, and enables the repos named in `UZI_SEED_FORGE_REPOS`. An **already-present** connection is left entirely untouched (never re-verified, never overwritten) — safe to leave set across restarts. A **forge outage** at seed time (network error, 401) is non-fatal: it is logged and skipped, and seeding retries on the next boot; only a bad *static* config (PAT without email, non-allowlisted base URL) refuses to start. The PAT is never logged. |
| `UZI_SEED_FORGE_BASE_URL` | first entry of `FORGE_ALLOWED_BASE_URLS` | Forge base URL the seeded connection targets. Must be one of `FORGE_ALLOWED_BASE_URLS` or boot fails (same SSRF allowlist as user-created connections). |
| `UZI_SEED_FORGE_REPOS` | — (optional) | Comma-separated `path_with_namespace` list (e.g. `vtmocanu/uzi,vtmocanu/foo`) to enable on the seeded connection. Entries the bot cannot see are logged as a warning and skipped, not fatal. Empty means the connection is created but no repo is enabled. |

### Default column labels are created on the forge

The first time a repo's board is opened, uzi ensures three labels exist on that GitLab project — `In Progress`, `Upcoming`, `Later` (`forgesvc.DefaultColumns`) — creating any that are missing via the forge's label-create API. This is a deliberate, visible side effect: those labels then show up in GitLab's own label list and board for that project, not just inside uzi. Board columns are reconfigurable afterward to any of the project's labels; the default set is only what's seeded on first open.

### Freshness contract

- Content edits and label adds/removes made at normal human cadence (spaced further apart than GitLab's `updated_at` bump throttle — see below) are picked up within one `FORGE_POLL_INTERVAL`.
- GitLab throttles how often it bumps an issue's `updated_at` to roughly once per ~60-second window, regardless of whether the triggering change is a label add or a label remove (verified against gitlab.example.com — see the PRD's Sync engine section for the full finding). Multiple edits landing inside the same throttle window collapse to a single bump, so only the latest of them is guaranteed to be caught incrementally; earlier ones in that window are caught by the next reconcile pass instead. `FORGE_POLL_INTERVAL`'s default (`60s`) is the same order of magnitude as the throttle window, so normal editing cadence is still caught incrementally almost all the time.
- De-labeling, issue deletion, and any edit whose `updated_at` bump the incremental filter missed are only guaranteed to be visible within one `FORGE_RECONCILE_EVERY`-th poll (the full reconcile), because eviction — noticing a previously-cached issue is now absent from the forge's current set — is structurally impossible for an `updated_after`-filtered incremental query to do.
