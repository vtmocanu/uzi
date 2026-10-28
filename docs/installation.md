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
`00364_codex_account_admission_throttle.sql` and
`00365_codex_admission_projection.sql` schema, plus
`00369_codex_operation_admission.sql` (operation metadata) and
`00370_account_cooldown_projection.sql` (active owner projection), before
serving. The supported Helm API deployment remains a singleton with `Recreate`
([deployment template](../deploy/chart/templates/api-deployment.yaml));
mixed old/new API replicas outside that topology have no enforcement promise.

Workers negotiate `codex_account_admission_signal_v1` before reporting
provider evidence. Older workers still obey the server's claim gate but
supply less evidence. Missing or refused reporting drops telemetry without
changing run lifecycle behavior. See the
[user contract](rate-limits.md#temporary-account-admission-waits).

Workers independently negotiate `codex_account_operation_admission_v1` for
fresh supported model dispatches; evidence reporting and optional active
snapshots do not grant operation permission. A flight that has never negotiated
this feature retains legacy behavior. Once negotiated, it stays engaged through
feature loss, clear/re-registration and application rollback. **An admission-service
outage blocks the next dispatch of every negotiated subscription flight,
including accounts with no known cooldown.** Already dispatched turns and
preservation continue with existing controls, budgets and deadlines effective.
Transport errors, 5xx, malformed/untyped responses and an untyped missing-route
404 mean unavailable wait, not allow or a provider failure. Strict typed
`authority_lost` / `stale_source` refusals use the existing ownership/source routes.

Operation reads have a three-second client deadline and 2.5-second server
context, bounded to 4 KiB actual request bytes and 8 KiB actual response bytes,
including error bodies. One serialized read per run/generation shares the
unsuccessful-read schedule across siblings and replacement sources: churn
cannot bypass it. Cooldown rechecks wait 1–15 seconds using server time;
unavailability waits 15 seconds. Each later dispatch still needs a fresh read.
The server uses the existing episode/class policy and finite per-run spread;
this adds no provider backoff/streak policy. Missing optional active snapshots
make specialized owner visibility unknown without disabling enforcement.

Reporting is bounded best effort and does not block the provider or lifecycle
path. Each observation gets at most two HTTP attempts, at most three seconds
each, within a ten-second monotonic budget. The current implementation starts
that budget at observation, including queue time, stricter than starting at
first send. The pending queue holds 32 observations including the active
send; overflow drops signals rather than inventing evidence.

The worker client has separate bounds of **1,024 live reporters** and
**1,024 recent closed/refused generation markers**. Closing removes the live
reporter and frees capacity for future bindings. Concurrent live overflow drops the telemetry binding but
records its generation to fence older pending responses. Closed markers retain
recent generation high-water marks in FIFO order. Eviction rotates the claim
observation epoch to prevent pre-eviction responses from rebinding forgotten
old authority; unrelated pending telemetry bindings may also be dropped.
Claims and execution continue. A fresh server claim increments the generation;
retained closed handles stay inert.

Nonclosing running/awaiting-approval state sends, pause intent and completion
attempts seal queued work and unsent retries. Active HTTP may finish within
its existing three-second attempt and ten-second observation bounds under the
server's live-state fence. Ending authority permanently closes reporting and
aborts active HTTP, allowing loss of the last attempt. Lifecycle and preservation
do not wait for a reporting ACK.

Registration and temporarily missing feature support discard pending work and
suspend new reporting while support is absent. Fresh attempts on live reporters
resume when support is advertised; closed, refused or stale-generation reporters
stay closed. Claims started before registration or feature invalidation are
ignored for telemetry binding through the observation epoch, without changing
execution. The signal retains typed evidence rather than raw provider text and
does not use Retry-After.

An ordinary API restart reconstructs admission from persisted database state.
Application rollback leaves the additive tables and view in place: an old
API does not read this state and admits new claims without this account gate.
An already engaged operation flight remains closed to new dispatch until the
admission service returns or existing controls/deadlines settle it; rollback
is not an operation-admission bypass. Never-negotiated flights remain legacy.
Roll-forward reapplies still-live finite state; expired rows follow the
quiet-reset policy when qualifying evidence next arrives. Do not run the
schema Down migrations as an application rollback.

Automated reconstruction coverage creates a separate database pool and service
instance; it does not exercise a binary or Kubernetes restart. An optional
operator rollout smoke check can observe a queued run across an API restart
and confirm admission after eligibility; that smoke check is unperformed here
and does not replace required CI. M5 **full agent suite: exact-head PR CI**
remains pending maintainer/hosted-CI verification at the published head:
`validate-agent`, both `test-agent-shard` legs, `test-agent-worker-uid` and
`test-agent`. Hosted queue/cache/rerun and critical-path costs remain unmeasured.
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
