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

## Not yet in scope

Job submission, agent control, and any other "factory" surface are deliberately out of this MVP — see [plan.md](plan.md) and the PRD's Risks section. This document will grow new sections (additional services, data flows, trust boundaries) as those land.
