# codex-m3b live acceptance — the subscription-only real-provider run (PRD #1106 M3b)

This is the maintainer handoff for the **live** branch of the codex-m3b lifecycle harness: the same
packaged `CodexExecutor` proof that runs credential-free against a loopback fake provider, pointed at
the **real** ChatGPT/OpenAI provider using a maintainer-injected subscription login. It is strictly
opt-in via env; with the env unset every leg is byte-for-byte the credential-free offline run.

The live branch drives the **subscription** lifecycle only, and adds a two-concurrent-refresh
**check-b** that proves the coordinated single-exchange refresh against the real `/codex/refresh`
route.

## 🔴 Read first — a real refresh rotates the seat

A real coordinated refresh performs a real oauth exchange at the provider and **ROTATES the seat's
refresh-token family**. This harness performs *several* real refreshes in one run: the executor's own
in-run refresh and finalize reconcile, the sequential advance+replay probe, and the two concurrent
check-b refreshes. Each rotates the stored refresh token.

- **Use a DEDICATED test seat, never your primary working `codex login`.** If a bug — or an
  interrupted durable commit — leaves the seat's stored refresh token out of sync with the provider,
  the seat can require a fresh `codex login` re-auth.
- The injected credential stays **ENV-ONLY**. It is never written to a file (the gitleaks gate
  forbids a credential literal in the tree); the sealed login lives only in the throwaway Postgres
  and process memory, and is destroyed at teardown.

## Env contract

The maintainer injects (all three are required for `--live`):

| Env | What |
|---|---|
| `CODEX_M3B_LIVE=1` | Turns live mode on everywhere. `live.sh --live` sets it for you; it is inert/absent for every automated run. |
| `CODEX_M3B_LIVE_LOGIN_JSON` | The real subscription login blob (see below). |
| `CODEX_M3B_LIVE_BASE_URL` | The real provider Responses base URL, e.g. `https://api.openai.com/v1`. |

Optional tunables (sane defaults; real-provider latency makes larger values advisable):

| Env | Default | What |
|---|---|---|
| `UZI_M3B_TEST_TIMEOUT` | `600` | Outer watchdog **seconds**. Raise it for a live run (a real model turn is slow); e.g. `1800`. |
| `UZI_M3B_LIVE_TEST_TIMEOUT_MS` | `600000` | Per-test `node --test` cap in ms (live only). |
| `CODEX_M3B_LIVE_RUN_TIMEOUT_MS` | `300000` | Inner cap on the real executor run in ms. |
| `CODEX_M3B_LIVE_SKIP_EXEC` | (unset) | `1` skips the full real-model executor run and runs only the credential-lifecycle + check-b proof (cheaper/faster; the deterministic gate still holds). |
| `UZI_M3B_SKIP_BUILD` | (unset) | `1` reuses an existing `uzi-agent-m3b:base` image instead of building it. |

### The login blob shape

`CODEX_M3B_LIVE_LOGIN_JSON` is exactly the two fields, from a `codex login` (the same shape uzi seals
for a subscription account):

```json
{"access_token":"<the login access token>","refresh_token":"<the login refresh token>"}
```

**Only those two fields.** The account **identity is auto-discovered**: the Go test server calls the
real, nonrotating `codexauth.DiscoverIdentity` with the access token to resolve the canonical
`(provider_user_id, workspace_account_id)` tuple, so **no account id is injected**. An expired or
invalid access token fails loudly at seed time (the server `log.Fatal`s before any run is seeded)
rather than seeding a broken account.

## The exact run (on a docker-capable / dind pod, e.g. meta-manager)

```sh
# 1. Get the tree. Until the `feat/codex-m3b-live-subscription` branch is merged, this glue lives
#    only on that branch: either check it out, or clone `main` and copy the 5 changed harness files
#    over it (e.g. `kubectl cp` them into the pod) — `e2e/codex-m3b/{live.sh,run-lifecycle.sh,
#    lifecycle.test.ts,LIVE-ACCEPTANCE.md}` and `api/cmd/codexm3btestserver/main.go`.
git clone https://github.com/vtmocanu/uzi.git && cd uzi

# 2. Pull the packaged worker base image and retag it to the harness's expected tag.
#    (agent-base carries the baked /app/src CodexExecutor + supervisor + openat2 fileop.)
docker pull ghcr.io/vtmocanu/uzi/agent-base:0.82.0
docker tag  ghcr.io/vtmocanu/uzi/agent-base:0.82.0 uzi-agent-m3b:base

# 3. Run the live subscription acceptance. UZI_M3B_SKIP_BUILD=1 reuses the pulled image.
UZI_M3B_SKIP_BUILD=1 \
CODEX_M3B_LIVE=1 \
CODEX_M3B_LIVE_LOGIN_JSON='{"access_token":"...","refresh_token":"..."}' \
CODEX_M3B_LIVE_BASE_URL='https://api.openai.com/v1' \
UZI_M3B_TEST_TIMEOUT=1800 \
  ./e2e/codex-m3b/live.sh --live
```

`live.sh --live` refuses (non-zero) unless both `CODEX_M3B_LIVE_LOGIN_JSON` and
`CODEX_M3B_LIVE_BASE_URL` are present, then `exec`s `run-lifecycle.sh` with live mode on. In live mode
`run-lifecycle.sh`:

- creates the docker network **without** `--internal` (egress-capable), so the worker and test-server
  reach the real provider and the real ChatGPT auth hosts;
- passes the live env into the **test-server** container (its Go seeds discover the identity, seal the
  real login, and wire the real `codexauth` client into the coordinated refresher) and into the
  **lifecycle** container (so the suite uses the real base URL, subscription-only);
- runs the same Block A canary-boundary proof (injected fakes, unchanged), then the live subscription
  leg, and asserts a reduced live count summary (`CODEX_M3B_LIVE_SUB_COUNTS`).

## What the four maintainer live-acceptance checks (PRD #1171 §"Maintainer-only live acceptance") get from this

| # | Check | Status in this harness |
|---|-------|------------------------|
| 1 | One production run-harness turn **and** one advice-harness pass on a subscription login, then repeat on an OpenAI API key; no fallback | **Partial.** The live subscription leg drives one real production run-harness turn against the real provider (reported as `ran`; non-gating — a real model's tool choices against the minimal test server are maintainer-verified, and it can be skipped with `CODEX_M3B_LIVE_SKIP_EXEC=1`). The **advice-harness pass** and the **API-key repeat** are **NOT** exercised here (the api_key leg is skipped in live mode) — still manual. |
| 2 | Two processes on the same subscription state, force an expired generation, observe **exactly one** coordinated refresh and both consuming the one durable generation | **Exercised (check-b).** Two concurrent `/codex/refresh` calls at the same observed generation, distinct operation ids, over the **real** coordinated-refresh route: the harness asserts exactly one `advanced`, both converging on the same access token + generation, and the committed generation advancing by **exactly one** step. Nuance: it uses two concurrent HTTP calls from **one** process (both hit the same server-side coordinated-refresh state machine, which is the serialization point being proven), not two OS processes. |
| 3 | Recreate the process/root, authenticate from the API's committed state, then cancel + final boundary cleanup with every registered root settled | **Partial.** The live run exercises release-from-committed-state, the finalize boundary reconcile and the terminal dispose (roots reaped/settled). **Cancel** is proven by Block A (injected fakes), not on the live path. |
| 4 | No secret, capability, account id, private endpoint, session/thread id or raw auth artifact in logs, git, image layers or the public evidence | **Partial.** The live leg captures every plaintext token the executor + probes released and asserts none — and the capability — appears in an emitted message or a log line. The broader sweep (account id, endpoints, session/thread ids, git history, image layers, the recorded evidence) remains a **manual** maintainer step. |

Treat the deterministic gate here as the credential lifecycle: a real release, the coordinated
advance+replay, and the check-b concurrency convergence. The full real-model run and the wider
no-leak sweep are maintainer-verified.

## Teardown

`run-lifecycle.sh` tears down **by exact name** in a `trap cleanup EXIT` — every throwaway resource is
named **outside** the `uzi-` namespace (`codex-m3b-*`, `cdr-m3b-*`). It never uses a `uzi-*` glob,
never `docker compose down`, never `-p uzi`, never `-v`.

If the outer watchdog kills the run and orphans a resource, remove it by its exact name (the `$$` is
the `run-lifecycle.sh` PID from the run):

```sh
docker rm -f codex-m3b-lifecycle-<pid> codex-m3b-controls-<pid> cdr-m3b-api-<pid> cdr-m3b-pg-<pid>
docker network rm cdr-m3b-net-<pid>
docker image  rm -f cdr-m3b-api-img-<pid>:local
```

Do **not** glob `uzi-` and do **not** touch the real dev stack (`uzi-db-1`, `uzi-api-1`, …): those
share the daemon and, per the repo's destructive-ops rules, a `uzi-` glob cannot tell a throwaway from
the real database.

## Files

- `live.sh` — the maintainer-only entrypoint. `--dry-run` = credential-free loopback; `--live` =
  real provider (requires the injected login + base URL); no arg / no login = refuses.
- `run-lifecycle.sh` — the orchestrator. Live mode: egress-capable network + live env into the
  test-server and lifecycle containers + a reduced live count summary.
- `api/cmd/codexm3btestserver` — the throwaway Bearer-route server. Live mode: seals the real
  env-injected login, auto-discovers identity, wires the real `codexauth` client, subscription-only.
- `lifecycle.test.ts` — Block A (injected fakes, always) + the real-launch suite; its subscription
  leg branches to the live body under `CODEX_M3B_LIVE=1`, and adds check-b.
