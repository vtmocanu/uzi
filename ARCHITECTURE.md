# Architecture

uzi's current MVP is a single docker-compose stack: three services, one trust boundary at the edge, no external dependencies. This document describes that shape; see [docs/auth-design.md](docs/auth-design.md) for the security design of the auth surface it carries.

## Services

```
                  laptop (host network)
                          │
                   127.0.0.1:8080
                          │
┌─────────────────────────┼──────────────────────────────────┐
│ compose network         ▼                                  │
│                 ┌───────────────┐                           │
│                 │      web      │  nginx-unprivileged        │
│                 │  (React SPA)  │  serves static build,      │
│                 └───────┬───────┘  proxies /api/* same-origin│
│                         │                                    │
│                         ▼                                    │
│                 ┌───────────────┐                           │
│                 │      api      │  Go (chi), distroless      │
│                 │  (auth API)   │                             │
│                 └───────┬───────┘                            │
│                         │                                    │
│                         ▼                                    │
│                 ┌───────────────┐                           │
│                 │      db       │  postgres:17               │
│                 │               │  named volume: pgdata      │
│                 └───────────────┘                            │
└──────────────────────────────────────────────────────────────┘
```

- **`web`** (`web/Dockerfile`) — a Vite/React SPA built at image build time and served by `nginxinc/nginx-unprivileged`. Its `nginx.conf` does two jobs: serve the built static assets, and reverse-proxy `/api/*` to `api:8080`. The SPA and API therefore share one origin (`http://127.0.0.1:8080`) — no CORS configuration exists anywhere in the stack.
- **`api`** (`api/Dockerfile`) — a statically linked Go binary (`chi` router) on `gcr.io/distroless/static-debian12`, running as `nonroot`. Serves `/api/auth/*`, `/api/admin/*`, `/api/health`. Runs its own DB migrations at startup before it starts accepting traffic (see below).
- **`db`** — stock `postgres:17`, digest-pinned in `docker-compose.yml`. Data lives in the named volume `pgdata`; it is not a bind mount, so it survives container recreation but is bound to the Docker volume store on this host.

All three images are pulled/built by digest or pinned tag in `docker-compose.yml`, not floating `latest`.

## Trust boundaries

- **Only `web` publishes a port**, and only to loopback: `127.0.0.1:8080` in `docker-compose.yml`. `api` and `db` have no `ports:` mapping — they are reachable exclusively over the private compose network, by each other, and are invisible from the host's other network interfaces or from any other machine.
- **nginx is the sole entry point and the sole source of truth for the client's identity.** `web/nginx.conf` sets `X-Forwarded-For: $remote_addr` (not `$proxy_add_x_forwarded_for`), which *overwrites* any `X-Forwarded-For` a client sent rather than appending to it. `api` then only trusts that header when the direct connection comes from a `TRUSTED_PROXIES` CIDR (the compose network's private ranges, by default) — see [docs/auth-design.md](docs/auth-design.md#rate-limiting-and-the-x-forwarded-for-trust-model). Net effect: a client cannot spoof its apparent IP by sending its own `X-Forwarded-For`.
- **The session cookie is the only credential**, and it is `HttpOnly` — never readable by any JavaScript running in the SPA, including an XSS payload. See [docs/auth-design.md](docs/auth-design.md) for the full cookie/CSRF/revocation design.

## Request flow: an authenticated API call

1. Browser sends `GET /api/admin/users` with cookies `uzi_auth` (JWT) and `uzi_csrf` attached automatically (same-origin).
2. nginx (`web`) proxies the request to `api:8080/api/admin/users`, setting `X-Forwarded-For: $remote_addr`, `X-Real-IP`, `X-Forwarded-Proto`.
3. `chi` routes it through `RequireAuth` (`api/internal/middleware/auth.go`): validates the JWT signature/expiry, loads the user by ID from `db` (one PK read), rejects if inactive or if `token_version` has been bumped since the token was issued. Past the halfway point of the TTL, it also re-issues both cookies (rolling refresh).
4. Then through `RequireAdmin`: rejects unless `user.IsAdmin`.
5. The handler (`api/internal/handler/admin.go`) queries `db` via `sqlc`-generated, `pgx`-backed queries and returns JSON.

State-changing requests (POST/PATCH) additionally run CSRF validation inside `RequireAuth` before any handler logic executes.

## Startup and migrations

`api`'s entrypoint (`api/cmd/server/main.go`) does not assume the DB is ready or up to date:

1. Waits for `db` to accept connections (bounded retry loop; compose's `depends_on: condition: service_healthy` on `db`'s `pg_isready` healthcheck already gates container start, this is a second, in-process guard).
2. Runs all pending **goose** migrations, embedded in the binary via `go:embed` (`api/internal/store/migrate.go`, `api/internal/store/migrations/`) — no separate migration step or tool needed at deploy time.
3. Only then opens the `pgx` connection pool and starts the HTTP server.

`web` depends on `api`'s healthcheck (`GET /api/health`, which itself pings the DB pool), so `docker compose up` brings the stack up in the correct order: `db` healthy → `api` migrated and healthy → `web` serving.

## Forge integration

uzi's second surface connects each user to a git forge (GitLab now, via a forge-generic interface that keeps a later Forgejo driver from touching callers, schema, or UI) so the board has real work to show. It adds no new service — everything below lives inside `api` — but it does add a second trust boundary: `api` now makes authenticated *outbound* calls to a third party (the forge) on top of the inbound boundary at the nginx edge described above. See [docs/gitlab-bot-setup.md](docs/gitlab-bot-setup.md) for the operator/user procedure and the PRD (`prds/2-forge-integration-kanban.md`) for the full design rationale.

### Forge abstraction

`api/internal/forge` defines the `Forge` interface (`VerifyToken`, `ListProjects`, `ListLabels`, `EnsureLabels`, `ListIssues`, `UpdateIssueLabels`) and a neutral domain vocabulary (`BotIdentity`, `Project`, `Label`, `Issue`); `forge.New` selects a driver by `forge.Type` (`gitlab.go` today) so no other package ever imports a driver directly. Every driver call goes through an `*http.Client` bounded by `FORGE_HTTP_TIMEOUT` (`timeoutClient` in `forge.go`) — closing the untimeouted-`http.DefaultClient` wart the `multica` inspiration carries — and every returned error is passed through a `redactor` (`redact.go`) that scrubs the PAT and any `Authorization`/`PRIVATE-TOKEN` header value before the error can reach a log line or an HTTP response body.

### Bot PATs, encrypted at rest

`api/internal/secretbox` is a generic AES-256-GCM box (12-byte random nonce prepended to the ciphertext) — deliberately not PAT-specific, since the same utility is slated for per-user Anthropic OAuth tokens in the next PRD. `config.Load` calls `secretbox.LoadKey("UZI_SECRET_KEY")` at boot and refuses to start if it is missing, not valid base64, not exactly 32 bytes, or a low-entropy placeholder (e.g. all-zero) — the same refuse-to-start stance as `JWT_SECRET`. The resulting `*secretbox.Box` is constructed once in `main.go` and handed to `forgesvc.Service`, which seals a PAT into `forge_connections.token_ciphertext` on connect and opens it on every forge call thereafter. **Rotating `UZI_SECRET_KEY` invalidates every stored PAT** — there is no re-encrypt path in this MVP, so a rotation means every user reconnects.

### SSRF guard

The server making authenticated outbound calls to a `base_url` supplied at connection time is a classic SSRF surface (cloud metadata endpoints, internal services, loopback). `config.Config.ForgeAllowedBaseURLs` (from `FORGE_ALLOWED_BASE_URLS`, default `https://gitlab.example.com`) is the only set of base URLs a connection may target; `config.NormalizeForgeBaseURL` requires `https` and canonicalizes to `scheme://host[:port]` before every allowlist comparison, and boot fails if the parsed list is empty or any entry is malformed or non-`https`. The Settings → Forge UI offers only this set (`GET /api/forge/config`), so a user cannot even attempt a free-text URL.

### Data model: forge as source of truth

Migration `00002_forge.sql` adds four tables, all scoped down to `forge_connections.user_id` by FK cascade:

- **`forge_connections`** — one row per (user, forge_type, base_url); carries the encrypted PAT and the verified bot identity.
- **`repos`** — projects discovered via the bot's membership list, keyed by the forge's stable numeric project id (not the path, which can be renamed); upserted with `enabled=false` on every listing call so enable/disable always has a row to target.
- **`board_columns`** — ordered label names per repo; the implicit Open (no column label) and Closed (issue `state`) columns are never stored.
- **`issues`** — a *cache*, never authoritative. uzi's own board state is limited to column configuration; every other field is overwritten from the forge on each sync. `has_prd_link` is computed at fetch time from the issue description (regex match on a `prds/*.md` reference) and stored as a bool — the description itself is never persisted.

### Sync engine

`api/internal/forgesvc.Service` is shared by the HTTP handlers and the background poller: it builds a `Forge` driver from a stored (encrypted) connection, runs the PRD-link check, and implements `IncrementalSync`/`FullSync` against an `IssueStore` interface (narrowed for unit testing against a fake store and a mocked `Forge`, without a live database).

`api/internal/poller.Engine` (`main.go`) is started as a single background goroutine alongside the HTTP server. Each tick, for every enabled repo, it either runs an incremental pull (`ListIssues(labels=["PRD"], state=all, updated_after=HWM)`, high-water-mark = the max `updated_at` the forge itself returned, never the client clock) or, every `FORGE_RECONCILE_EVERY`-th tick (and always on a repo's first poll after being enabled), a full reconcile: fetch the complete `PRD`-labeled set with no lower bound, upsert everything, and evict cache rows the forge no longer returns. Eviction is the only way to observe de-labeling, closing-with-label-removed, or deletion, which an `updated_after`-filtered query structurally cannot report. Per-repo state (`hwm`, poll count) lives only in the `Engine`'s in-memory map — a disabled repo simply drops out and a re-enabled one starts over with a fresh full reconcile.

Writes are forge-first: a board move (`POST /api/repos/:id/issues/:iid/move`) calls `UpdateIssueLabels` before touching the cache, and only updates the cache on success — a failed forge write leaves the card where it was (snap-back), never an optimistically-moved card the forge disagrees with.

Shutdown ordering matters here specifically because of the poller: `main.go`'s `run()` calls `srv.Shutdown` to drain in-flight HTTP requests, cancels the root context (stopping the poller's next tick), then `pollerWG.Wait()`s for any in-flight sync to finish — all *before* the deferred `pool.Close()` runs, so a mid-tick database query never races the pool shutting down underneath it.

### Per-user rate limiting on forge-proxying endpoints

The endpoints that call out to the forge on a user's behalf (`.../verify`, `.../projects`, `.../issues/:iid/move`, `.../sync`) run behind a second instance of the same fixed-window `mw.Limiter` PRD #1 uses for auth endpoints, but keyed **per user** (`PerUserMiddleware`) rather than per client IP, and budgeted separately (`FORGE_RATE_LIMIT_MAX`/`FORGE_RATE_LIMIT_WINDOW`, default 30/minute) from `RATE_LIMIT_MAX`/`RATE_LIMIT_WINDOW`. This bounds how hard one uzi user's actions can hit the upstream forge, independent of the local-network IP-sharing caveat already documented for the auth limiter in [auth-design.md](docs/auth-design.md#accepted-limitations).

## Not yet in scope

Job submission and agent control are deliberately out of this MVP — see [plan.md](plan.md) and the forge integration PRD's Risks section ("this PRD ends at the synced board"). The kanban board reflects and writes back forge labels, but nothing in uzi yet acts on a card's issue; that is the next PRD. This document will grow new sections (additional services, data flows, trust boundaries) as those land.
