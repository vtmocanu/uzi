---
title: Installation
order: 10
audience: operator
---

# Installation

## Prerequisites

- Docker + Docker Compose v2 (`docker compose`, not the standalone `docker-compose` v1).
- `openssl` (to generate secrets).

Everything else (Go, Node, PostgreSQL) runs inside containers; nothing needs to be installed locally.

## Clone

```sh
git clone <repo-url> uzi
cd uzi
```

## Configure

```sh
./scripts/init-env.sh
```

This generates the three required secrets and writes them to `.env` (starting from
`.env.example`, so every other option stays documented and editable):

- `JWT_SECRET`: session signing key (`openssl rand -hex 64`)
- `UZI_SECRET_KEY`: base64 master key that encrypts secrets at rest (`openssl rand -base64 32`)
- `POSTGRES_PASSWORD`: bundled Postgres password (`openssl rand -hex 24`)

All three are refused at boot / by compose if left empty. The script is
**generate-once**: it writes `.env` only if it is absent and never regenerates,
because `UZI_SECRET_KEY` (rotating it makes stored tokens undecryptable) and
`POSTGRES_PASSWORD` (only used to init the `pgdata` volume on first run) must stay
stable. The rest have sane defaults for local use; see
[configuration.md](configuration.md) for the full list.

Prefer to do it by hand? `cp .env.example .env` and fill those three values in.
With `task` installed, `task init` runs the same script.

## Run

```sh
docker compose up
```

This builds and starts three services (`db`, `api`, `web`) in dependency order: `api` waits for `db`'s healthcheck, `web` waits for `api`'s. The API runs its goose migrations against `db` before it starts serving. Once `web` is healthy, open <http://127.0.0.1:8080>.

Register the first account — it becomes admin automatically. Subsequent registrations are regular users.

## Codex account admission upgrades

Deploy the **API first, then workers**. The API applies the additive
`00360_codex_account_admission_throttle.sql` and
`00361_codex_admission_projection.sql` schema before serving. The supported
Helm API deployment remains a singleton with `Recreate`
([deployment template](../deploy/chart/templates/api-deployment.yaml));
mixed old/new API replicas outside that topology have no enforcement promise.

Workers negotiate `codex_account_admission_signal_v1` before reporting
provider evidence. Older workers still obey the server's claim gate but
supply less evidence. Missing or refused reporting drops telemetry without
changing run lifecycle behavior. See the
[user contract](rate-limits.md#temporary-account-admission-waits).

Reporting is bounded best effort and does not block the provider or lifecycle
path. Each observation gets at most two HTTP attempts, at most three seconds
each, within a ten-second monotonic budget. The current implementation starts
that budget at observation, including queue time, stricter than starting at
first send. The pending queue holds 32 observations including the active
send; overflow drops signals rather than inventing evidence.

A worker client retains closed/generation markers for up to **1,024 distinct
run IDs over that client's lifetime**. At that limit it conservatively refuses
telemetry for new run IDs; existing tracked IDs remain eligible to report.
This can reduce pressure evidence on a long-lived worker. It does **not**
limit claims or runs. State, release and park paths seal pending reporting
and abort unsent retries; preservation does not wait for a reporting ACK.
The signal retains typed evidence rather than raw provider text and does not
use Retry-After.

An ordinary API restart reconstructs admission from persisted database state.
Application rollback leaves the additive tables and view in place: an old
API does not read this state and admits without this account gate.
Roll-forward reapplies still-live finite state; expired rows follow the
quiet-reset policy when qualifying evidence next arrives. Do not run the
schema Down migrations as an application rollback.

The M4 reconstruction test creates a separate database pool and service
instance; it does not exercise a binary or Kubernetes restart. An optional
operator rollout smoke check can observe a queued run across an API restart
and confirm admission after eligibility; it is separate from required gates.
Before merge, the maintainer/CI must complete **full agent suite: exact-head PR CI**:
successful required `validate-agent`, both `test-agent-shard` legs and the
worker-UID lane on the exact published head. The worker cannot trigger hosted
jobs before publication; these checks remain external and unverified here.
See [ADR-1106](../adr/1106-codex-harness.md#amendment-temporary-account-admission-3530-2026-10-27)
for the statement-snapshot boundary and critical invariants.

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
cd api && ../scripts/sqlc.sh v1.31.1 generate
```

The wrapper downloads and caches the pinned, sha256-verified sqlc release for Linux amd64 or macOS arm64, so no local `sqlc` install or source build is needed (the same command is commented atop `api/sqlc.yaml`). A cold cache needs network access; cached archives are verified before use. Never hand-edit the generated files under `api/internal/store/`.

## Persistence

Postgres data lives in the named volume `pgdata` (declared in `docker-compose.yml`), not a bind mount.

- `docker compose down` then `docker compose up` — the volume is untouched; users, sessions and admin state survive.
- `docker compose down -v` — deletes the volume along with all data. Use this only when you want a clean slate (e.g. before running `scripts/smoke.sh`).

## Ports

Only `web` publishes a port, and only to loopback: `127.0.0.1:8080`. `db` and `api` are reachable exclusively over the internal compose network — nothing on the laptop's network interfaces can reach them directly. See [ARCHITECTURE.md](../ARCHITECTURE.md) for the full trust-boundary picture.
