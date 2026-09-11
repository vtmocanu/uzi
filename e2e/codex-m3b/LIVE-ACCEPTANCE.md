# codex-m3b live acceptance — the subscription-only real-provider run (PRD #1106 M3b)

This is the maintainer handoff for the **live** branch of the codex-m3b lifecycle harness: the same
packaged `CodexExecutor` proof that runs credential-free against a loopback fake provider, pointed at
the **real** ChatGPT/OpenAI provider using a maintainer-injected subscription login. It is strictly
opt-in via env; with the env unset every leg is byte-for-byte the credential-free offline run.

The live branch drives the **subscription** lifecycle only. Its default gate now includes one real
production advice-harness pass, a deterministic cancel after the app-server accepts `turn/start`,
registered provider/action-root settlement, and an in-process no-leak oracle. The sequential refresh
and two-concurrent-refresh **check-b** are available only through the explicit
`CODEX_M3B_LIVE_REFRESH_PROBES=1` opt-in.

## Read first: a real refresh rotates the seat

A real coordinated refresh performs a real OAuth exchange at the provider and **rotates the seat's
refresh-token family**. The default live run does **not** execute the explicit sequential or check-b
refresh probes. The app-server may still request a refresh if the injected access token expires, and
the default complete executor run normally refreshes during its finalize reconcile (set
`CODEX_M3B_LIVE_SKIP_EXEC=1` to omit that non-gating run). Setting
`CODEX_M3B_LIVE_REFRESH_PROBES=1` additionally runs the sequential advance/replay and concurrent
check-b probes, which deliberately rotate the stored refresh token.

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
| `UZI_M3B_TEST_TIMEOUT` | `1200` in live mode | Outer watchdog **seconds**. The credential-free default remains `600`; use `1800` when provider latency warrants more room. |
| `UZI_M3B_LIVE_TEST_TIMEOUT_MS` | `1000000` | Per-test `node --test` cap in ms (live only), sized above the advice, cancel, and optional complete-run inner caps. |
| `CODEX_M3B_LIVE_RUN_TIMEOUT_MS` | `300000` | Inner cap on the optional complete real executor run in ms. |
| `CODEX_M3B_LIVE_ADVICE_TIMEOUT_MS` | `180000` | Inner cap on the gated real advice pass in ms. |
| `CODEX_M3B_LIVE_SKIP_EXEC` | (unset) | `1` skips only the non-gating complete real-model run. Advice, cancel/root settlement, release/fail-closed, and no-leak gates still run. |
| `CODEX_M3B_LIVE_REFRESH_PROBES` | (unset) | `1` explicitly enables the token-rotating sequential advance/replay and concurrent check-b probes. Default live acceptance does not execute or require them. |
| `CODEX_M3B_LIVE_RELAX_TIMEOUTS` | (unset) | `1` enables diagnostic-only timeout relaxation: the worker client uses 30 seconds instead of the production 8-second budget, and the live test server removes its explicit identity and refresh limits. A run with this set is not production-equivalent acceptance. |
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
#    only on that branch: either check it out, or clone `main` and copy the changed harness files
#    over it: `e2e/codex-m3b/{live.sh,run-lifecycle.sh,lifecycle.test.ts,packaged-modules.ts,
#    LIVE-ACCEPTANCE.md}` and `api/cmd/codexm3btestserver/main.go`.
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

That default does not run the explicit sequential/check-b refresh rotation. A maintainer who
intentionally wants to reproduce acceptance check 2 adds
`CODEX_M3B_LIVE_REFRESH_PROBES=1` to the same command and uses a dedicated test seat.

`live.sh --live` refuses (non-zero) unless both `CODEX_M3B_LIVE_LOGIN_JSON` and
`CODEX_M3B_LIVE_BASE_URL` are present, then `exec`s `run-lifecycle.sh` with live mode on. In live mode
`run-lifecycle.sh`:

- skips the separate packaging-controls container; `--dry-run` keeps that container and its behavior
  unchanged;
- creates the docker network **without** `--internal` (egress-capable), so the worker and test-server
  reach the real provider and the real ChatGPT auth hosts;
- passes the live env into the **test-server** container so it discovers identity, seals the real
  login, and wires the real `codexauth` clients; initial identity establishment is outside the callback
  budget and gets a separate bounded 15-second client, while the refresh callback retains production's
  2.5-second single-call cap. The lifecycle process receives the login blob only so its no-leak oracle
  can scan the actual access and refresh values, while the launched production root still receives
  neither value through its environment;
- runs the unchanged Block A injected-fake proof, then the real subscription advice and cancellation
  passes, the optional complete run, and only when enabled the explicit refresh probes;
- parses `CODEX_M3B_LIVE_SUB_COUNTS` as counts and booleans only. That summary prints no token,
  capability, account, endpoint, session, thread, or turn value.

## What the four maintainer live-acceptance checks (PRD #1171 §"Maintainer-only live acceptance") get from this

| # | Check | Status in this harness |
|---|-------|------------------------|
| 1 | One production run-harness turn **and** one advice-harness pass on a subscription login, then repeat on an OpenAI API key; no fallback | **Subscription half exercised.** The complete production run remains reported as `ran` and content-non-gating. A separate real advice pass uses `makeCodexAdviceHarness` with a test-only adapter over the packaged production `launchCodexRoot` and `createCodexTransport`; it gates one fresh release, one successful terminal/policy callback, no action surface, clean root disposal, and a denied-release negative that launches no root. The real API-key repeat is intentionally **user-deferred to parent M6**. This harness does not add or claim it. |
| 2 | Two processes on the same subscription state, force an expired generation, observe **exactly one** coordinated refresh and both consuming the one durable generation | **Opt-in, default off.** `CODEX_M3B_LIVE_REFRESH_PROBES=1` runs the accepted sequential advance/replay and concurrent check-b assertions over the real route. The default live pass neither executes nor requires them, avoiding routine refresh-family rotation. The concurrency probe still uses two concurrent HTTP calls from one process, not two OS processes. |
| 3 | Recreate the process/root, authenticate from the API's committed state, then cancel + final boundary cleanup with every registered root settled | **Subscription cancellation exercised.** A fresh committed token authenticates a real root. The test observes a successful `turn/start`, aborts before the harness consumes a terminal, requires the exact owner-cancel outcome, then requires `safety.dispose` to report `disposed` with every tracked provider and command/file action root settled and no disposal failure. |
| 4 | No secret, capability, account id, private endpoint, session/thread id or raw auth artifact in logs, git, image layers or the public evidence | **In-process portion exercised.** The suite checks the injected access and refresh tokens, every released/refreshed access token, capability, account id, provider and worker endpoint URL/host, and every observed session/thread/turn id across emitted/advice messages, scrubbed logs, and non-auth request evidence. Required routing ids and login fields are allowed only in their exact internal RPC slots; the emitted summary contains booleans/counts only, and failures print only a static category/surface. Git history and image-layer inspection remain external maintainer checks. |

The default deterministic gate is now fresh release plus advice terminal/policy/fail-closed
construction, post-turn-start cancel, provider/action-root settlement, and the in-process no-leak
oracle. The complete real-model run and advice text remain content-non-gating. Check-b runs only
when explicitly enabled. The real API-key repeat remains deferred to M6.

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
- `run-lifecycle.sh`: the orchestrator. Live mode skips only the packaging-controls container, uses
  an egress-capable network, threads the no-leak inputs into the lifecycle process, and parses the
  boolean/count-only live summary. Dry-run behavior is unchanged.
- `api/cmd/codexm3btestserver`: the throwaway Bearer-route server. Live mode seals the real
  env-injected login, auto-discovers identity, wires the real `codexauth` client, and remains
  subscription-only.
- `packaged-modules.ts`: resolves the image-baked launcher and transport factories used by the
  test-only advice/cancel adapter.
- `lifecycle.test.ts`: Block A (injected fakes, always) plus the real-launch suite. Its live body gates
  subscription advice, cancellation, cleanup, and no-leak evidence; check-b is explicit opt-in.
