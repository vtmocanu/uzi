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
- **`api`** (`api/Dockerfile`) — a statically linked Go binary (`chi` router) on `gcr.io/distroless/static-debian12`, running as `nonroot`. Serves `/api/auth/*`, `/api/admin/*`, `/api/agent-templates/*`, `/api/me/secrets/*`, `/api/health`. Runs its own DB migrations and builtin-template reconciliation at startup before it starts accepting traffic (see below).
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

1. Loads config (`config.Load`), including the `UZI_SECRET_KEY` boot guard (`secretbox.LoadKey`): boot fails here, before any DB connection is attempted, if the key is missing or malformed.
2. Waits for `db` to accept connections (bounded retry loop; compose's `depends_on: condition: service_healthy` on `db`'s `pg_isready` healthcheck already gates container start, this is a second, in-process guard).
3. Runs all pending **goose** migrations, embedded in the binary via `go:embed` (`api/internal/store/migrate.go`, `api/internal/store/migrations/`) — no separate migration step or tool needed at deploy time.
4. Opens the `pgx` connection pool, then reconciles the builtin agent templates through it (`store.ReconcileBuiltinTemplates`, see below): idempotent, so this is safe to run on every boot.
5. Builds the `secretbox.Box` from the already-validated key.
6. Only then starts the HTTP server.

`web` depends on `api`'s healthcheck (`GET /api/health`, which itself pings the DB pool), so `docker compose up` brings the stack up in the correct order: `db` healthy → `api` migrated and healthy → `web` serving.

## Agent templates

An agent template (`agent_templates` table) is the definition of one role an
agent can play: name, description, an optional model override, an optional
tools allowlist, and a prompt body. It is not itself an agent; it is the
recipe a later release renders into a running one.

- **Source of truth split.** The seven builtin roles (`coder`, `reviewer`,
  `auditor`, `tester`, `documenter`, `fact-checker`, `spec-keeper`) are
  Go-embedded from copies of this repo's own `.claude/agents/*.md` files
  (`api/internal/agenttmpl/builtins/`, parsed at package `init()`). At every
  boot, `store.ReconcileBuiltinTemplates` inserts any builtin row missing from
  the DB and never touches one that already exists, so an admin's edits to a
  builtin survive restarts, and future releases can add or upgrade builtins
  without a SQL seed that can't be re-run.
- **Read/write split.** Any authenticated user can list, view, and preview
  templates (`GET /api/agent-templates*`); only an admin can create, edit,
  delete, or reset one (`RequireAdmin` in `api/internal/handler/handler.go`).
  Templates are shared across all users, so this closes the hole where any
  user could rewrite the prompts everyone else's agents run with.
- **Renderer.** `api/internal/agenttmpl/render.go` turns a template into
  Claude Code's subagent Markdown (fixed-order YAML frontmatter, `tools` as an
  inline comma-separated string, `tools`/`model` omitted when they inherit).
  It is a pure function with no DB dependency, so golden-file tests pin a
  builtin's rendered output to byte-match the checked-in `.claude/agents/*.md`
  file. `GET /api/agent-templates/:id/rendered` serves this Markdown directly;
  nothing in this release writes it to a filesystem or spawns anything from it
  (that is a later release's job). See
  [docs/agent-templates.md](docs/agent-templates.md).

## Secrets: per-user credentials at rest

`user_secrets` is a generic, kind-keyed table (`kind` currently only
`anthropic_token`, `CHECK`-constrained so a new kind is one migration, not a
new table) holding one AES-256-GCM-sealed secret per `(user, kind)`. The
`secretbox` package (`api/internal/secretbox/`) wraps `Seal`/`Open` around a
single 32-byte key loaded from `UZI_SECRET_KEY` once at boot into one
`*secretbox.Box` shared by every handler; the API refuses to start without a
valid key (see [docs/configuration.md](docs/configuration.md)). This is the
platform's one shared secret-at-rest mechanism: any feature that needs to
store a per-user credential (starting with the Anthropic token this PRD adds)
seals it with the same key before it reaches Postgres, so a DB dump alone
never yields a plaintext secret, and rotating the key invalidates every
stored secret across every feature at once, not just one.

The API around it is deliberately minimal: `PUT /api/me/secrets/anthropic_token`
writes or rotates the caller's own secret, `DELETE` on the same path removes
it, and `GET /api/me/secrets` (and every other read) returns **metadata
only** (`kind`, `created_at`, `updated_at`); there is no reveal endpoint, and
no admin path to another user's secret value. See
[docs/anthropic-token.md](docs/anthropic-token.md) for the user-facing flow.

## Not yet in scope

Spawning agents from a template, connecting them to the server, decrypting
and injecting a user's Anthropic token into a running agent, and any other
"factory" execution surface are deliberately out of this MVP (see
[plan.md](plan.md) and the PRD's Risks section). This document will grow new
sections (additional services, data flows, trust boundaries) as those land.
