# Worker setup

A **worker** is the `uzi-agent` container: it connects to your uzi server, claims your queued runs, and drives them with the Claude Agent SDK. One worker per user is the normal setup; it runs anywhere that can reach the server outbound (your laptop, a VM, a CI runner), since it never needs an inbound port.

## 1. Generate a join token

In uzi, open **Settings → Workers** and register a worker (give it a name, e.g. `laptop`). uzi shows the **join token once**: copy it now, since only its hash is stored server-side and there is no way to retrieve it again later (generate a new worker if you lose it).

## 2. Anthropic credential

The worker runs agents against **your own** Anthropic credential, which must already be saved in uzi before you start a run: see [anthropic-token.md](anthropic-token.md). The recommended credential is an OAuth token from `claude setup-token`; a Console API key also works. The token is decrypted server-side and handed to the worker only inside a run's claim response, never stored on the worker beyond that run.

## 3. Run the worker

### Compose profile (bundled with the stack)

The worker is an opt-in compose service behind the `agent` profile, so the base stack (`db`/`api`/`web`) runs without it:

```sh
# in your .env (see docs/configuration.md for the full list):
#   UZI_WORKER_TOKEN=<the join token from step 1>
docker compose --profile agent up
```

This builds and starts the `agent` service from `agent/Dockerfile` (Node 22, git, bash, non-root, `cap_drop: ALL`, `no-new-privileges`), pointed at `UZI_API_URL=http://api:8080` (the compose network) by default, with its data directory on the named volume `agentdata`. The join token is delivered as a compose **file secret** (`worker_token`, mounted at `/run/secrets/worker_token`), not a plain environment variable: see [proc-hardening.md](proc-hardening.md) for why. Once it registers, **Settings → Workers** shows it as **online**.

### Standalone (`docker run`)

To run a worker on a different host, pointed at a remote uzi server, build the `agent` image yourself (there is no published image yet; this is a local demo) and run it directly:

```sh
docker build -t uzi-agent ./agent
docker run -d \
  -e UZI_API_URL=https://uzi.example.com \
  -e UZI_WORKER_TOKEN=<the join token> \
  -e UZI_WORKER_NAME=my-worker \
  -v uzi-agent-data:/data \
  --cap-drop ALL --security-opt no-new-privileges:true \
  uzi-agent
```

`UZI_API_URL` should point at wherever `api` is reachable for that worker; the local compose default is only correct when the worker runs on the same compose network. Note that **TLS on this hop is not provided by the MVP itself** (`api` listens plain HTTP on the compose network): put a TLS-terminating proxy in front of it for a worker reached over an untrusted network, same as you would for any other client of `api`.

## Configuration

The worker's environment variables are documented in full, with defaults, in [configuration.md](configuration.md#worker-container-agent). The ones that matter for a first run:

- **`UZI_API_URL`** (required): where the worker reaches the server.
- **`UZI_WORKER_TOKEN`** / **`UZI_WORKER_TOKEN_FILE`** (one required): the join token from step 1, as an env var or (preferred, and what the bundled compose profile uses) a file path. The file form keeps the token out of the worker's `/proc/<pid>/environ` (see [proc-hardening.md](proc-hardening.md)).
- **`UZI_DATA_DIR`** (default `/data`): persistent storage for the bare-clone cache, per-run git worktrees, and the pinned Claude Agent SDK session directory. This must survive `docker compose down && up` for resume-after-restart to work; the bundled compose profile already mounts it on the `agentdata` named volume, matching the `db`/`pgdata` pattern.
- **`UZI_EXECUTOR`** (default `sdk`): `sdk` runs real Claude Agent SDK turns (the product path); `stub` is a no-AI executor the project's own tests use. Leave it at `sdk` for real use.

## Online, offline, busy

- **online**: the worker's last heartbeat (`WORKER_HEARTBEAT_INTERVAL`, default 15s) arrived within the server's staleness window (`WORKER_HEARTBEAT_STALE`, default 45s).
- **offline**: no heartbeat within that window; the server marks it offline and re-queues any run it was holding (up to `RUN_MAX_REQUEUES` times, see [configuration.md](configuration.md)).
- **busy**: derived at read time, not stored: a worker is busy exactly when it currently holds a non-terminal run (`claimed`/`running`/`awaiting_approval`). A worker claims at most one run at a time.

## Multiple workers, one user

You can register more than one worker (e.g. `laptop` and `ci-runner-1`); each gets its own join token and claims independently from your queue. There is no work-splitting logic beyond "whichever idle worker of yours polls next claims the oldest queued run" (see [ARCHITECTURE.md](../ARCHITECTURE.md#run-lifecycle) for the claim semantics, including the affinity a re-queued run has toward the worker that was already running it).

## Removing a worker

**Settings → Workers → Delete** removes a worker's registration; the server refuses to delete one that still holds a non-terminal run (finish or cancel it first). Deleting does not revoke anything worker-side beyond the registration: stop the container yourself.
