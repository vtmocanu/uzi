---
title: Installation
order: 10
audience: operator
---

# Installation

For an existing VPS with Docker, a TLS reverse proxy, and other services, use the
[server Compose file](../deploy/compose/compose.yaml) with
[Run uzi on a VPS with Docker Compose](./vps-deployment.md). It covers signed release
images, HTTPS, administrator bootstrap, worker registration, and backups. Choose a
release containing that server file. The steps below are the local source-build path.

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
because `UZI_SECRET_KEY` (rotating it makes server-key-sealed tokens undecryptable) and
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

## First setup from a script

`/server setup` connects the forge and enables repos for the seed admin without the web UI. It is a subcommand of the API binary. You run it inside the deployment, so it needs no admin password and no new token. It applies the same checks as the web UI: the bot token must be least-privilege, a repo is enabled only if the default branch is protected, and an enabled repo gets its board columns.

Before you run it:

- The API has started once, so the database is migrated. Setup never migrates and refuses (exit 2) a database that is not at the API's schema version.
- `UZI_SEED_EMAIL` names an existing admin account. Setup acts for that user and never creates or promotes one. It does not need `UZI_SEED_PASSWORD`.
- The forge base URL is in `FORGE_ALLOWED_BASE_URLS`.
- Setup reads the API's own `DATABASE_URL` and `UZI_SECRET_KEY`, or their `_FILE` variants (a mounted secret file), with the same rules as the API: see [configuration.md](configuration.md). A `_FILE` path must name a regular file: setup refuses a FIFO or device instead of waiting on it.

Docker Compose, with the token on stdin:

```sh
printf '%s' "$PAT" | docker compose exec -T api /server setup \
  --forge-type github --forge-base-url https://github.com \
  --forge-token-file - --repos acme/app,acme/lib
```

Kubernetes (Helm chart). The API pod must have `UZI_SEED_EMAIL` set (see the `UZI_SEED_*` notes in the chart values). Use `-i` so stdin reaches the container:

```sh
printf '%s' "$PAT" | kubectl exec -i deploy/<release>-api -- /server setup \
  --forge-type github --forge-base-url https://github.com \
  --forge-token-file - --repos acme/app,acme/lib
```

Flags:

| Flag | Meaning |
|---|---|
| `--forge-type` | Required: `gitlab`, `forgejo` or `github`. No default, no detection. |
| `--forge-base-url` | Required: the forge URL; it must be in `FORGE_ALLOWED_BASE_URLS`. |
| `--forge-token-file PATH` | Required: a file holding the bot token, or `-` for stdin. The token is never read from a flag value or an environment variable. At most 16 KiB. |
| `--repos a/b,c/d` | Comma-separated `path_with_namespace` values to enable, spelled with the same case as on the forge. At most 200. |
| `--worker-name NAME` | Optional: create the first self-hosted worker with this name (at most 200 bytes). Without it setup skips the worker step and reports `not_requested`. |
| `--worker-template NAME` | Optional, needs `--worker-name`: the worker template. |

Setup validates every flag before it writes anything; invalid input exits 2.

### The first worker

With `--worker-name`, setup creates a self-hosted worker for the seed admin and prints its join token. The step is create-only. It runs only after the forge and every requested repo succeeded, so one refused repo means no worker and no token (`skipped_dependency`). If the seed admin already owns any worker, setup creates nothing and prints no token: a join token is stored as a hash and cannot be shown again, and setup never rotates one. The existence check and the creation run in one database transaction under a per-owner lock, so two setup runs at the same time cannot create two workers.

The join token appears once, in the `join_token` field of the `worker` entry of the stdout document, and nowhere else: not on stderr, not in a log. Capture stdout, extract the field and write it into `UZI_WORKER_TOKEN` in `.env`. If the document is lost, the token is lost; delete the worker in Settings → Workers and create a new one.

```sh
install -m 600 /dev/null setup.json   # private before it holds the join token
rc=0
printf '%s' "$PAT" | docker compose exec -T api /server setup \
  --forge-type github --forge-base-url https://github.com \
  --forge-token-file - --repos acme/app,acme/lib --worker-name vps-1 > setup.json || rc=$?
token=$(jq -r '.worker.join_token // empty' setup.json)
if [ "$rc" -eq 0 ] && [ -n "$token" ]; then
  printf 'UZI_WORKER_TOKEN=%s\n' "$token" >> .env
  docker compose --profile agent up -d
else
  echo "setup exited $rc and issued no join token; .env is unchanged" >&2
fi
```

The example writes `UZI_WORKER_TOKEN` only when setup exits `0` and the report carries a token. On exit `3` (a worker already exists) there is no token, and an empty `UZI_WORKER_TOKEN=` line would override the existing worker's token. `setup.json` holds the join token; delete it after you copy the token. See [configuration.md](configuration.md) for `UZI_WORKER_TOKEN` and `UZI_WORKER_TOKEN_FILE`.

On Kubernetes the primary worker path is hosted workers, which the controller creates; see [openshift.md](openshift.md) and the `workers.*` chart values. Setup creates join-token workers only, so on a hosted-worker install omit `--worker-name`: the step reports `not_requested` and exit codes work as before. Use `--worker-name` on Kubernetes only to start a self-hosted worker outside the cluster.

Setup writes one JSON document to stdout and progress notes and log lines to stderr. Neither stream carries the forge token: setup replaces it in every report field, note and log line. The one secret on stdout is the worker join token, printed once on purpose. A database connection error prints a fixed message, not the raw error, which can hold the password. The document has `schema_version`, an overall `status`, and one entry per step (`forge`, `discovery`, each repo in `repos`, `worker`). Step statuses:

| Status | Meaning |
|---|---|
| `connected` | The forge connection was created. |
| `present` | A connection for the same forge type and base URL already existed. Setup reuses it untouched: it does not re-verify it, and a different token never replaces it. |
| `done` | Discovery listed the forge's repos. |
| `enabled` | The repo is enabled and has its board columns. Re-running re-checks the guard and reports `enabled` again. |
| `refused` | The token is over-privileged, or the repo guard blocked the repo. `findings` lists the reasons, the same as the web UI shows. |
| `not_found` | The bot cannot see the repo on the forge. |
| `failed` | The forge or the database failed; `message` says which step. |
| `skipped_dependency` | An earlier step failed, so this one did not run. |
| `not_requested` | Not asked for (the `worker` entry without `--worker-name`). |
| `created` | The worker was created. The `worker` entry carries `worker_id`, `name` and the one-time `join_token`. |
| `exists` | The seed admin already owns a worker. No token is issued and none can be recovered. |

Exit codes: `0` every requested step is done or already present; `1` a step failed or was refused, and the report says which; `2` a usage, configuration, schema or owner precondition failed and nothing was written; `3` everything else succeeded but the worker already exists, so no token was issued (the report `status` is `worker_exists`). Exit `1` wins over exit `3`. Writes that completed stay in place. Fix the cause and run the same command again.

Setup runs in its own process, so it cannot wake the API's repo sync. The report sets `issue_sync` to `pending`: the poller imports the new repos' issues on its next interval (`FORGE_POLL_INTERVAL`).

## Codex account admission upgrades

Deploy the **API first, then workers**. The API applies the additive
`00364_codex_account_admission_throttle.sql` and
`00365_codex_admission_projection.sql` schema, plus
`00377_codex_operation_admission.sql` (operation metadata) and
`00378_account_cooldown_projection.sql` (active owner projection), before
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
