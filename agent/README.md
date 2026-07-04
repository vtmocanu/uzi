# uzi-agent (worker)

Outbound-only worker for the uzi "AI dark factory" (PRD #4). One worker runs per
user, connects to the uzi API with a join token, claims queued runs, works each
in an isolated git worktree, and reports state + messages back. No inbound ports.

This is the **M2 skeleton**: the protocol client, the bare-clone + worktree git
layer, and the claim → worktree → work → report state machine, driven by a
**stub executor** (writes a marker file and makes a local commit). The Claude
Agent SDK executor, guardrails, plan gate, and MR push land in M3/M4.

## Layout

| Path | What |
|---|---|
| `src/config.ts` | Env parsing (`UZI_API_URL`, `UZI_WORKER_TOKEN`, `UZI_DATA_DIR`, intervals) |
| `src/protocol.ts` | Worker↔API wire contract (the M1 server side is built in parallel) |
| `src/client.ts` | HTTP client: register / heartbeat / claim / messages / state / inputs |
| `src/git.ts` | Bare-clone cache + `agent/issue-{iid}` worktree lifecycle + PAT-scoped auth |
| `src/executor.ts` | `Executor` interface + `StubExecutor` (M3 drops in the SDK executor) |
| `src/batcher.ts` | 500ms seq-numbered message batching, gapless from `last_seq` |
| `src/runner.ts` | Per-run state machine |
| `src/worker.ts` | Register + heartbeat + claim-poll loops |
| `src/main.ts` | Entry point |
| `test/` | `node:test` suites against an in-process fake API + on-disk fixture repos |

## Develop

```sh
npm install
npm run typecheck   # tsc --noEmit
npm test            # node --test via the tsx loader (no extra test deps)
npm start           # tsx src/main.ts  (needs the env vars below)
```

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `UZI_API_URL` | — (required) | Base URL of the uzi API (e.g. `http://api:8080`) |
| `UZI_WORKER_TOKEN` | — (required) | Join token issued from Settings → Workers |
| `UZI_DATA_DIR` | `/data` | Persistent volume: bare clones, worktrees, (M3) SDK sessions |
| `UZI_WORKER_NAME` | hostname | Display name shown in the workers list |
| `WORKER_HEARTBEAT_INTERVAL` | `15s` | Heartbeat cadence |
| `WORKER_POLL_INTERVAL` | `3s` | Claim poll cadence |
| `UZI_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |

Run it via compose: `docker compose --profile agent up`.
