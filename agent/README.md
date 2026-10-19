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

## Unit test shards

`npm test` launches a child Node test runner with tsx and the hermetic
process preload, a 120000 ms test timeout, and inherited environment and stdio.
`UZI_AGENT_TEST_CONCURRENCY` sets a positive integer concurrency;
`UZI_AGENT_TEST_JUNIT` adds spec output on stdout and JUnit at the named path.
With `UZI_AGENT_TEST_SHARD=1/2` or `2/2`, the launcher selects explicit
sorted top-level `test/*.test.ts` files. It never also passes native sharding.
With no shard, it runs the entire inventory and still validates the pins.

Ordinary additions and unpinned deletions need no pin edits. Unpinned paths use
the first SHA-256 digest byte of their UTF-8 agent-relative path: even goes to
1/2, odd to 2/2. Only a renamed unpinned file may move. The small
`test-shards.json` overrides heavy files; a pinned rename or deletion fails
closed until its entry is updated. Arrays must remain sorted and unique,
with exactly the two shard keys. This is not an inventory manifest.

Re-pin newly heavy tests from CI per-file timings: seed each shard with the
measured hashed remainder, add the M4 overhead to its shard after converting
wall seconds to load at the CI concurrency, then place heavy tests longest
first on the lighter shard. Keep unmeasured files hashed with unknown costs.
Compare the resulting load estimates and run focused selector/launcher tests.
Run both `task test:agent:shard` legs with matching JUnit destinations and M4
once in the shard-1 composition, then run `task check:agent-shards` over the
actual reports and success marker. This independently proves the tracked-file
union and disjointness before accepting new pins. See [ADR-3121](../adr/3121-stable-agent-test-shards.md)
for the initial timing provenance and decision.

Supported filters are `npm test -- --test-name-pattern '<regex>'`,
`--test-skip-pattern=<regex>`, and `--test-only`. Positional filenames,
native shard flags, timeout changes, and reporter overrides are rejected.

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
