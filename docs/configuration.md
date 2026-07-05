---
title: Configuration
audience: operator
---

# Configuration

All configuration is via environment variables, set in `.env` (copied from `.env.example`) and passed into the `api` container by `docker-compose.yml`. `POSTGRES_*` also configures the `db` service directly.

| Var | Default | Notes |
|---|---|---|
| `JWT_SECRET` | — (required) | HS256 signing key for session JWTs. The API refuses to start if it is missing, empty, shorter than 16 characters, or a known placeholder (`change-me`, `secret`, `password`, etc. — see `api/internal/config/config.go`). Generate with `openssl rand -hex 64`. |
| `UZI_SECRET_KEY` | — (required) | Base64-encoded 32-byte master key for AES-256-GCM. The API refuses to start if it is missing, not valid base64, not exactly 32 bytes decoded, or a low-entropy placeholder such as an all-zero key (same boot-guard stance as `JWT_SECRET`). Generate with `openssl rand -base64 32`. It is the platform's one shared encryption-at-rest key: it seals every stored secret kind (today the per-user Anthropic token and forge bot PATs; any future kind) before it reaches Postgres, so a DB dump alone never recovers a plaintext secret. Rotating this key invalidates **all** stored secrets across every kind at once, not just one feature's: every affected user has to reconnect or re-paste theirs. There is no re-encrypt path. |
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

Invalid values for `AUTH_TOKEN_TTL`, `RATE_LIMIT_MAX`, `RATE_LIMIT_WINDOW` or an unparseable `TRUSTED_PROXIES` entry fall back to their defaults rather than failing boot (the last one is logged as a warning); only a bad `JWT_SECRET`, a bad `UZI_SECRET_KEY`, or a missing `DATABASE_URL` refuses to start.

There is no CORS configuration to make, by design: nginx serves the SPA and proxies `/api/*` to the API on the same origin (see [ARCHITECTURE.md](../ARCHITECTURE.md)), so the browser never makes a cross-origin request.

## Forge integration

See [gitlab-bot-setup.md](gitlab-bot-setup.md) for the bot-account procedure these variables support.

These extend the core table above; `UZI_SECRET_KEY` (which also encrypts forge bot PATs) is documented there.

| Var | Default | Notes |
|---|---|---|
| `FORGE_ALLOWED_BASE_URLS` | `https://gitlab.example.com` | Comma-separated SSRF allowlist: the only forge base URLs a connection may target. Every entry must be an absolute `https://` URL; boot fails if the list is empty or any entry is malformed or non-`https`. The Forge tab (under Settings) offers exactly this set in its base-URL dropdown. |
| `FORGE_POLL_INTERVAL` | `60s` (`1m`) | Per-enabled-repo incremental sync cadence (Go duration). An invalid or non-positive value falls back to the default. See "Freshness contract" below. |
| `FORGE_RECONCILE_EVERY` | `10` | Every Nth incremental poll is a full reconcile instead (fetches the complete `PRD`-labeled issue set with no `updated_after` bound, diffs, and evicts cache rows the forge no longer returns). A non-positive or unparseable value falls back to the default, same as the other numeric/duration vars here (the poller itself additionally clamps any value `< 1` to `1` — every poll becomes a full reconcile — as defense in depth, but that path isn't reachable through this env var). The very first poll after a repo is enabled is always a full reconcile regardless of this setting, since it has to seed the cache. |
| `FORGE_HTTP_TIMEOUT` | `15s` | Hard per-call timeout on every outbound HTTP request to the forge (connect, projects, labels, issues, label updates). Closes the untimeouted-`http.DefaultClient` wart the `multica` inspiration shipped with. |
| `FORGE_RATE_LIMIT_MAX` / `FORGE_RATE_LIMIT_WINDOW` | `30` / `1m` | Per-user request budget (not per-IP) on the forge-*proxying* endpoints only — connection verify, project listing, issue move, manual sync — so one user's connection can't hammer the upstream forge. Separate limiter and separate budget from `RATE_LIMIT_MAX`/`RATE_LIMIT_WINDOW`, which only cover `/api/auth/register` and `/api/auth/login`. |
| `UZI_SEED_EMAIL` | — (optional) | Setting this provisions an admin user automatically at startup, after migrations, if no user with that email already exists yet (an existing user's password is never touched — the seed is idempotent and safe to leave set across restarts). Must be a syntactically valid address or boot fails. Leave unset to disable seeding entirely (the normal first-registration-becomes-admin flow still applies). |
| `UZI_SEED_PASSWORD` | — (required if `UZI_SEED_EMAIL` is set) | Password for the seeded admin, hashed with the same argon2id parameters as normal registration. Must satisfy the same length policy as registration (12–1024 characters) or boot refuses to start — a set-but-invalid seed is a loud misconfiguration, not a silent skip. |
| `UZI_SEED_NAME` | — (optional) | Display name for the seeded admin. |
| `UZI_SEED_FORGE_PAT` | — (optional) | Setting this seeds a forge connection (owned by the seed admin) at startup. Requires `UZI_SEED_EMAIL` to be set, or boot fails. At boot, if the seed admin has no connection for the target base URL yet, the API verifies this bot PAT against the forge, stores it encrypted (same path as the Forge tab's connect flow), lists the bot's projects, and enables the repos named in `UZI_SEED_FORGE_REPOS`. An **already-present** connection is left entirely untouched (never re-verified, never overwritten) — safe to leave set across restarts. A **forge outage** at seed time (network error, 401) is non-fatal: it is logged and skipped, and seeding retries on the next boot; only a bad *static* config (PAT without email, non-allowlisted base URL) refuses to start. The PAT is never logged. |
| `UZI_SEED_FORGE_BASE_URL` | first entry of `FORGE_ALLOWED_BASE_URLS` | Forge base URL the seeded connection targets. Must be one of `FORGE_ALLOWED_BASE_URLS` or boot fails (same SSRF allowlist as user-created connections). |
| `UZI_SEED_FORGE_REPOS` | — (optional) | Comma-separated `path_with_namespace` list (e.g. `vtmocanu/uzi,vtmocanu/foo`) to enable on the seeded connection. Entries the bot cannot see are logged as a warning and skipped, not fatal. Empty means the connection is created but no repo is enabled. |
| `UZI_SEED_ANTHROPIC_TOKEN` | — (optional, dev convenience) | Setting this seeds the seed admin's [Anthropic token](anthropic-token.md) at startup, create-only: an existing token (UI-pasted, or already seeded) is left untouched. Requires `UZI_SEED_EMAIL` to be set (the token belongs to that user) or boot fails. Only format-checked at seed time, never verified against Anthropic and never logged. Exists so a local `docker compose down -v` doesn't force re-pasting your own token; leave unset in any shared deployment. |

### Default column labels are created on the forge

The first time a repo's board is opened, uzi ensures three labels exist on that GitLab project — `In Progress`, `Upcoming`, `Later` (`forgesvc.DefaultColumns`) — creating any that are missing via the forge's label-create API. This is a deliberate, visible side effect: those labels then show up in GitLab's own label list and board for that project, not just inside uzi. Board columns are reconfigurable afterward to any of the project's labels; the default set is only what's seeded on first open.

### Freshness contract

- Content edits and label adds/removes made at normal human cadence (spaced further apart than GitLab's `updated_at` bump throttle — see below) are picked up within one `FORGE_POLL_INTERVAL`.
- GitLab throttles how often it bumps an issue's `updated_at` to roughly once per ~60-second window, regardless of whether the triggering change is a label add or a label remove (verified against gitlab.example.com — see the PRD's Sync engine section for the full finding). Multiple edits landing inside the same throttle window collapse to a single bump, so only the latest of them is guaranteed to be caught incrementally; earlier ones in that window are caught by the next reconcile pass instead. `FORGE_POLL_INTERVAL`'s default (`60s`) is the same order of magnitude as the throttle window, so normal editing cadence is still caught incrementally almost all the time.
- De-labeling, issue deletion, and any edit whose `updated_at` bump the incremental filter missed are only guaranteed to be visible within one `FORGE_RECONCILE_EVERY`-th poll (the full reconcile), because eviction — noticing a previously-cached issue is now absent from the forge's current set — is structurally impossible for an `updated_after`-filtered incremental query to do.

## Agent runtime (PRD #4)

See [ARCHITECTURE.md](../ARCHITECTURE.md#agent-runtime-workers-runs-live-view) for how these are used (run lifecycle, sweeper, claim affinity) and [worker-setup.md](worker-setup.md) for the operator procedure. `UZI_SEED_ANTHROPIC_TOKEN` (dev convenience: boot-seeds an existing Anthropic token for the seed admin) is documented above, in the Forge integration seed table, alongside the other startup seeds.

The run view in the web UI shows a terse, one-line-per-event feed (tool calls with argument summaries, results folded under their call, durations, an in-flight spinner), never raw JSON. When you need the complete raw frames for debugging, set `UZI_LOG_LEVEL=debug` and read them from the worker's `docker logs` (see the `UZI_LOG_LEVEL` row below).

### Server (`api`)

| Var | Default | Notes |
|---|---|---|
| `RUN_TIMEOUT` | `2h` | Wall-clock cap on a `running` run; the sweeper fails it past this. Also sent to the worker in the claim payload (`RunTimeoutSeconds`) for its own reference; the server's own sweeper is the actual enforcement. |
| `RUN_IDLE_TIMEOUT` | `10m` | No-SDK-message idle cap, enforced **worker-side**; read here and shipped in the claim payload (`IdleTimeoutSeconds`). |
| `RUN_MAX_ITERATIONS` | `5` | Cap on the implement ⇄ review loop count, enforced worker-side; read here and shipped in the claim payload. |
| `RUN_MAX_REQUEUES` | `1` | How many times the sweeper may re-queue a run whose worker went stale before failing it instead. `0` means fail immediately on worker death (no re-queue). |
| `WORKER_HEARTBEAT_STALE` | `45s` | No heartbeat past this and the sweeper marks a worker offline and re-queues its non-terminal runs. |
| `WORKER_AFFINITY_GRACE` | `2m` | How long a re-queued run is claimable only by the worker that was already running it, before any of the user's other workers may claim it (gives a resume a chance to land back on the disk that still holds the session + git worktree). |

Invalid values for any of the above fall back to their defaults (the same lenient-parse behavior as the core table); none of them is a boot guard. All six are wired into the `api` service's `environment:` block in `docker-compose.yml` and documented in `.env.example`, so setting them in `.env` takes effect on the bundled stack.

`config.Load` also parses `WORKER_HEARTBEAT_INTERVAL` and `WORKER_POLL_INTERVAL` into the server's `Config` struct, but nothing on the API side reads those two fields back out (the sweeper acts on `WORKER_HEARTBEAT_STALE`, not on them) — they are not server knobs despite being parsed here. The values that actually matter are the **worker's own copy** of the same-named variables, consumed by the `agent` binary and wired to the `agent` compose service; see the Worker container table below.

### Worker container (`agent`)

Set on the `agent` compose service (profile `agent`) or on a standalone `docker run` — see [worker-setup.md](worker-setup.md).

| Var | Default | Notes |
|---|---|---|
| `UZI_API_URL` | — (required) | Base URL the worker uses to reach `api`. The bundled compose profile sets `http://api:8080` (the compose network); a remote/standalone worker points this at wherever `api` is actually reachable. |
| `UZI_WORKER_TOKEN` | — (required, unless `_FILE` is set) | The join token from Settings → Workers, sent as a Bearer credential on every worker call. |
| `UZI_WORKER_TOKEN_FILE` | — (optional, preferred) | Path to a file containing the join token; read once at startup, then the file is unlinked (best-effort — a read-only secret mount can't be). Takes precedence over `UZI_WORKER_TOKEN` when set. Keeps the token out of the worker's `/proc/<pid>/environ`; see [proc-hardening.md](proc-hardening.md). The bundled compose profile mounts this as a compose file secret. |
| `UZI_DATA_DIR` | `/data` | Persistent storage: the bare-clone cache, per-run git worktrees, and the pinned Claude Agent SDK session directory. Must be a durable volume for resume-after-restart to work. In the bundled compose profile this is **hardcoded** to `/data` (matching the `agentdata` volume mount), not read from `.env`; it's only a real knob for a standalone `docker run` or a compose override. |
| `UZI_WORKER_NAME` | container hostname | Display name shown in Settings → Workers / Admin. |
| `UZI_AGENT_VERSION` | `0.1.0-m4` | Version string the worker reports on `register`/`heartbeat`; informational only. |
| `UZI_EXECUTOR` | `sdk` | `sdk` runs real Claude Agent SDK turns (the product path); `stub` is a no-AI executor the project's own tests use — leave at `sdk` for real use. |
| `UZI_STUB_PLAN_GATE` | `false` | Only meaningful with `UZI_EXECUTOR=stub`: makes the stub drive the M4 plan-approval gate before committing, instead of committing immediately. Test/harness use only. |
| `WORKER_HEARTBEAT_INTERVAL` | `15s` | How often the worker posts a heartbeat. |
| `WORKER_POLL_INTERVAL` | `3s` | How often an idle worker asks the server for a run to claim. |
| `WORKER_MESSAGE_BATCH_INTERVAL` | `500ms` | How long the worker accumulates SDK output before a batched `POST .../messages` call. |
| `WORKER_HTTP_TIMEOUT` | `30s` | Per-request timeout on the worker's own control-plane HTTP calls to `api`. |
| `WORKER_PLAN_APPROVAL_TIMEOUT` | `24h` | How long a run may sit at `awaiting_approval` before the worker fails it: generous so a human has time, finite so an abandoned plan never wedges the (single-run-at-a-time) worker. |
| `UZI_LOG_LEVEL` | `info` | Worker log verbosity: `debug`/`info`/`warn`/`error`. At `debug` the worker also writes every raw run event (each `tool_use`, `tool_result`, status, etc.) to its stdout as it is emitted, so `docker logs uzi-agent-1` becomes the full-frame debug surface. Secrets are redacted before logging. `info` stays terse (no per-event lines). |

Duration values accept the same Go-style strings used server-side (`15s`, `3s`, `500ms`, `2h`) or a bare integer read as milliseconds.
