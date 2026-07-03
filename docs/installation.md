# Installation

## Prerequisites

- Docker + Docker Compose v2 (`docker compose`, not the standalone `docker-compose` v1).
- `openssl` (to generate secrets).
- `git` (submodules).

Everything else (Go, Node, PostgreSQL) runs inside containers; nothing needs to be installed locally.

## Clone

```sh
git clone <repo-url> uzi
cd uzi
git submodule update --init
```

The submodule step pulls `inspiration/bottega`, `inspiration/multica` and `inspiration/dot-agent-deck` — reference projects the design was compared against (see [auth-design.md](auth-design.md)). They are not built or run; skip the step if you only want to run the stack.

## Configure

```sh
cp .env.example .env
```

Fill in the two required values (both refused at boot / by compose if left empty):

- `JWT_SECRET` — `openssl rand -hex 64`
- `POSTGRES_PASSWORD` — `openssl rand -hex 24`

The rest have sane defaults for local use. See [configuration.md](configuration.md) for the full list.

## Run

```sh
docker compose up
```

This builds and starts three services (`db`, `api`, `web`) in dependency order: `api` waits for `db`'s healthcheck, `web` waits for `api`'s. The API runs its goose migrations against `db` before it starts serving. Once `web` is healthy, open <http://127.0.0.1:8080>.

Register the first account — it becomes admin automatically. Subsequent registrations are regular users.

## Smoke test

`scripts/smoke.sh` drives the full journey against a running stack (registration race, login, admin list, deactivation/revocation, CSRF enforcement) and expects a **fresh, empty** `users` table:

```sh
docker compose down -v && docker compose up -d --build
./scripts/smoke.sh
```

Override the target with `BASE=http://127.0.0.1:8080 ./scripts/smoke.sh`.

`scripts/smoke-prd3.sh` covers agent templates and the per-user Anthropic token the same way (admin-only template writes, builtin protection, byte-stable render, metadata-only secret responses, a DB dump that holds only ciphertext); same fresh-stack expectation and `BASE`/`DB_DUMP` overrides (see the script header).

## Development

Editing a migration or a query file (`api/internal/store/migrations/`, `api/internal/store/queries/`) requires regenerating the `sqlc` code before it compiles:

```sh
cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate
```

Pinned via `go run`, so no local `sqlc` install is needed (the same command is commented atop `api/sqlc.yaml`). Never hand-edit the generated files under `api/internal/store/`.

## Persistence

Postgres data lives in the named volume `pgdata` (declared in `docker-compose.yml`), not a bind mount.

- `docker compose down` then `docker compose up` — the volume is untouched; users, sessions and admin state survive.
- `docker compose down -v` — deletes the volume along with all data. Use this only when you want a clean slate (e.g. before running `scripts/smoke.sh`).

## Ports

Only `web` publishes a port, and only to loopback: `127.0.0.1:8080`. `db` and `api` are reachable exclusively over the internal compose network — nothing on the laptop's network interfaces can reach them directly. See [ARCHITECTURE.md](../ARCHITECTURE.md) for the full trust-boundary picture.
