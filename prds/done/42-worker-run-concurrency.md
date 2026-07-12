# PRD #42: Worker run concurrency — bounded multi-run workers

**GitLab Issue**: [vtmocanu/uzi#42](https://gitlab.example.com/vtmocanu/uzi/-/issues/42)
**Status**: Draft — reviewed 2026-07-10 by 3 agents (design, security, fact-check); no blocking findings; all major/medium findings folded in below (marked ↳review where the design changed). Done — merged to main via MR !42 (2026-07-12); all six milestones implemented, reviewed, and audited.
**Priority**: Medium
**Created**: 2026-07-10
**Depends on**: PRD #4 (worker runtime + run machinery, done). **Coupled to**: PRD #39 (chat agent) — its Decision 4 requires exactly the concurrency substrate this PRD builds; M1/M2 here are prerequisites of #39's chat lane and must not be duplicated there.
**Research inputs**: three-agent investigation 2026-07-10 — codebase assumption map, prior-art sweep (multica, bottega, dot-agent-deck), industry-practice survey (CI runners, queue workers, AI coding-agent platforms). Findings folded into [ADR-42](../adr/0042-worker-run-concurrency.md).

## Problem

"One run at a time per worker" is not a design decision today — it is an accident of implementation. The only thing enforcing it is the worker's serial claim loop (`agent/src/worker.ts:58-73` awaits `runner.execute` before re-polling). The server enforces nothing: `ClaimRun` (`api/internal/store/queries/runtime.sql:147-170`) will hand the same worker any number of runs, `runs.worker_id` is a plain non-unique indexed FK (`migrations/00020_workers_runs.sql:56`), and all active-run uniqueness lives at the issue level (`uq_runs_one_active_per_issue`, `00020_workers_runs.sql:63-65`), never the worker level.

This unstated invariant is now load-bearing in three conflicting ways:

1. **PRD #39 deliberately breaks it** — the chat lane adds a second concurrent claim loop in the same worker process. Without an explicit decision, #39 would be the first concurrent consumer of worker-process state that was never audited for concurrency.
2. **The worker process contains a security-critical concurrency hazard**: `SdkExecutor.spawnedPids` (`agent/src/sdk-executor.ts:115`) is a single instance-scoped set on the one shared executor instance (`main.ts:42`). Two concurrent runs would wipe and kill each other's subprocess trees — and that set is the B1 reap mechanism that kills agent children before the PAT-bearing push so nothing survives to read `/proc/environ`. Corrupting it is a guardrail regression, not just a bug. (Latent today; the serial loop hides it.)
3. **Throughput**: a user with several queued PRD issues and one worker gets strict serialization even when the runs touch different repos, while the laptop the worker runs on sits mostly idle during long model turns.

We need to decide, on purpose: may a worker execute multiple runs concurrently, and if so, under what bounds and isolation?

## Decision (summary)

**A worker may execute multiple runs concurrently, bounded by a slot semaphore with a low default cap of 1 (`WORKER_MAX_CONCURRENT_RUNS`), after the per-run isolation gaps in the worker process are closed. The server deliberately does NOT enforce 1:1 worker:run, but the worker advertises its cap at registration for observability (↳review). Scale-out (more workers per user) remains the primary parallelism mechanism; scale-up (N slots per worker) is a supported, opt-in configuration whose intra-user security residuals are documented honestly below — the full isolation story (uid-split / container-per-run) belongs to the future k8s-operator deployment, where each ephemeral pod is a single-slot worker.**

The full reasoning, alternatives, and evidence are in [ADR-42](../adr/0042-worker-run-concurrency.md) (summarized below).

---

## Architecture Decision Record

The full ADR — context (codebase assumption map, prior-art table, industry survey), the reconciling argument and its honest limits, options A/A′/B/C/D with rationale, decision, and consequences — lives at **[adr/0042-worker-run-concurrency.md](../adr/0042-worker-run-concurrency.md)** (ADR-42). The short version:

- **Chosen (Option A)**: bounded N-per-worker via a worker-side slot semaphore, default cap 1, cap advertised at registration for observability (never enforced server-side).
- **Rejected**: Option A′ (no cap advertisement — kills the saturation-visibility story), Option B (server-enforced 1:1 — blocks PRD #39, contradicts all prior art, encodes scaling policy in the schema), Option D (unbounded — bottega's model, no backpressure).
- **Deferred**: Option C (ephemeral container per run — the industry gold standard and the only real fix for the cap>1 residuals; belongs to the k8s-operator era, where each pod is a single-slot worker, i.e. Option A composes with it).
- **Accepted residuals at cap>1** (detailed in the ADR and the Accepted Residuals section below): sibling `/proc` PAT exposure during push windows; Bash cross-run worktree writes. Default 1 is byte-identical to today.

---

## Design Decisions

1. **Slot semaphore in the worker, slot-before-claim.** `worker.ts` replaces the serial loop with: acquire slot → claim → spawn execution (async) → release slot on terminal state. At capacity the loop sleeps the poll interval without calling claim (multica `daemon.go:2618`: "poll: at capacity") — and logs an at-capacity line so saturation is observable (↳review D-minor-12). This never manufactures claimed-but-waiting runs, so `SweepClaimedNeverStarted` semantics are untouched.
2. **A slot is held across `awaiting_approval` (↳review D-major-1).** A run parked at the plan gate blocks inside `runner.execute` (`worker.ts:65` → `runner.ts:275` `steering.awaitVerdict()`) for up to `WORKER_PLAN_APPROVAL_TIMEOUT` (default 24h), holding its slot the whole time. This is deliberate: releasing the slot at the gate would reintroduce exactly the claimed-but-over-capacity state that slot-before-claim exists to prevent, and the held cost is small (the planning SDK subprocess is already gone between turns; a worktree + a pending promise remain). Consequence stated honestly: with cap N, N unapproved plans pin all slots until approved, rejected, or timed out — the 24h timeout is the self-heal, and with the default cap of 1 this is precisely today's documented behavior. Approve your plans.
3. **`WORKER_MAX_CONCURRENT_RUNS`, default 1, advertised at registration.** Worker-side config (`agent/src/config.ts`), validated ≥ 1, with a warn log above a documented soft ceiling of 8 (↳review D-minor-11 — multica caps its whole daemon at 20; a fat-fingered value should shout before the OOM killer does). Reported in the register payload; stored in nullable `workers.max_concurrent_runs` (draft migration `00075`, landed as `00055`); never enforced server-side. Documented in `docs/worker-setup.md` with sizing guidance (each slot ≈ one SDK CLI + git ops + optional devbox provisioning) **and the cap>1 residuals from the ADR**.
4. **One `SdkExecutor` instance per execution; `killAgentTree` is runId-scoped end-to-end (↳review S-major-3, D-minor-5/7).** Pinned to the per-instance option (not a runId-keyed map): each `execute()` constructs its own executor, so `spawnedPids` is naturally per-run and #39's "per-instance executor state is safe" claim becomes true by construction (#39 D4's wording defers to this PRD — reconcile on whichever lands second). This ripples into `runner.ts`: the explicit pre-push `this.executor.killAgentTree?.()` calls (`runner.ts:152,157`) and the executor wiring in `RunRunner`'s constructor change from one shared instance to a per-run factory — named here, not discovered at implementation. Acceptance test (fixed): two concurrent executions with a fake injected kill — the pre-push reap kills exactly the pushing run's subprocess tree and clears exactly its pid set; the sibling's tree and set are untouched.
5. **Each run gets its own `HOME` (`agent-home/<runId>`) (↳review S-medium-4, D-major-3 — adopted as the plan, not a fallback).** The audit verified shared `$HOME/.claude` is not clean: session transcripts are safe (keyed by cwd-hash + session id, distinct per-run worktree cwds), but history/todos/shell-snapshots/`~/.claude.json` are process-global with concurrent-write corruption races and cross-run readability of prompts/actions. Per-run HOME eliminates all of it, is stable across resume (requeue keeps the run_id; affinity keeps the disk — the persisted SDK session_id resolves within the same HOME), and is cheaper than proving the shared dir safe. Terminal runs' HOME dirs are cleaned with the worktree.
6. **Concurrency audit of every collaborator shared across concurrent `execute()` calls (↳review scope widened).** Not a fixed list: `WorkerClient` (verified stateless-per-call — every method takes runId), `GitLabClient` (`runner.ts:175` shared `this.gitlab` — verify same), the `Logger`/`SecretRegistry`, `GitCache` (safe: per-path lock), and anything else `RunRunner` closes over. `StubExecutor` is already concurrency-safe (zero instance-level run state) — which also means the e2e stub path does NOT exercise the executor fix; M1's unit test is the real guard (stated in M5).
7. **`SecretRegistry` evicts a run's secrets on terminal state (↳review S-minor-6, D-minor-8 — upgraded from "document growth").** Verified leak-free (worst case over-redacts one run's secret out of another's log line; run-message streams use the per-run redactors, `runner.ts:79-80`, not this registry) — but a process-lifetime Set holding every completed run's PAT + token plaintext contradicts uzi's secret-minimization ethos and makes scrub() O(all-runs-ever) per log line. The registry becomes run-scoped (join token and worker-lifetime secrets stay process-scoped; run secrets are removed when the run reaches terminal state).
8. **No new claim SQL.** The existing invariants already provide multica's serialization guarantees at claim time: one active run per issue (`uq_runs_one_active_per_issue`), cross-kind same-branch exclusion, affinity grace. Concurrent slots in one worker claim disjoint runs by construction (`FOR UPDATE SKIP LOCKED`; independently confirmed by `specs/ai.md:788`).
9. **Resource limits sized for the whole worker, chat lanes included (↳review S-medium-5, D-major-4).** `docker-compose.yml` gains `cpus`/`mem_limit` on the agent service, sized as (run slots + #39 chat sessions) × per-slot budget + headroom — a single shared cgroup limit, stated plainly: there is no per-run fairness on compose (that is Option C's territory). Cross-run OOM fails **safe for `main`** (container death requeues all in-flight runs together) but not free for the innocent sibling: after `RUN_MAX_REQUEUES` (default 1, compose:97) it *fails* rather than requeues — the docs recommend raising `RUN_MAX_REQUEUES` alongside any cap>1. The shared `/nix` store under concurrent devbox provisioning relies on nix's own locking (correctness-safe, occasionally serializing; a lock failure fails a run — reliability note, not a leak). Coordination: #39 also edits the agent service (build-context switch) — merge order matters, noted in the parallelization table.
10. **UI: `busy` boolean → `active_runs`/`cap`.** `ListWorkersByUser`/`ListAllWorkers` return a count (`EXISTS` → `count(*)`, `runtime.sql:24-32,130-141`) plus the advertised cap; web renders "2/2 runs" when count > 1 or cap > 1. `busy` stays derivable so nothing else changes.
11. **Same-repo concurrency is allowed but serialized at the git layer.** The GitCache per-bare-path lock already queues clone/fetch/worktree ops for runs sharing a repo — correct, just slower. Documented; no attempt to parallelize git within a repo.
12. **Shared-token contention is accepted and documented.** N slots share the user's Anthropic token; the SDK's own retry/backoff is the handler (as in every prior-art system — none solves this). The docs state plainly that raising the cap multiplies 429 pressure on one token. A per-user token concurrency budget is out of scope (recorded enhancement).
13. **Docs/specs prose updated everywhere the invariant is stated**: `specs/ai.md` §877-878 ("One run at a time in M2" — ↳review fact-check: §669 is the one-*worker*-per-user invariant, which this PRD does NOT touch), `docs/worker-setup.md:75`, `agent/src/worker.ts:9-10`, `agent/src/config.ts:57`. specs/human.md is touched only if the user approves wording (it currently defers worker pools; this PRD doesn't contradict it).

## Technical Design

### Worker (agent/)

- `worker.ts`: slot semaphore (N-sized token pool, multica's pattern translated to a counter + wakeup in TS), claim loop acquires before `claimRun()`, executions run as tracked promises, slot released in `finally`, at-capacity log line. Graceful shutdown awaits all in-flight executions (signal handling extended from one awaited promise to a set).
- `sdk-executor.ts` + `runner.ts` + `main.ts`: per-execution executor construction (factory instead of the single `main.ts:42` instance), runId-scoped `killAgentTree` threading through the pre-push calls (`runner.ts:152,157`), per-run `homeDir` (`agent-home/<runId>`) replacing the single `main.ts:38` dir, cleanup on terminal.
- `log.ts`: run-scoped secret eviction (Decision 7).
- `config.ts`: `WORKER_MAX_CONCURRENT_RUNS` (int ≥ 1, default 1, warn > 8); register payload carries it.
- Tests (`agent/test/`): semaphore honors cap; two concurrent stub executions; the Decision 4 kill/reap acceptance test (fake executor with injected pids); per-run HOME isolation; shutdown drains all slots; secret eviction on terminal.

### API (api/)

- **Migration (draft `00075` — renumber at landing, landed as `00055`)**: `workers.max_concurrent_runs int` nullable. Nothing else — no claim SQL change, no constraint (ADR Options B/A′ rejected).
- Register handler/service: accept + store the advertised cap.
- `ListWorkersByUser` / `ListAllWorkers`: `EXISTS(...)` → `count(*)` of active runs; DTO gains `active_runs int` + `max_concurrent_runs *int`.

### Web (web/)

- `api.ts` worker type: `active_runs: number`, `max_concurrent_runs: number | null`; `WorkersSettings.tsx` / `RunsList.tsx` badge shows "N/M runs" when relevant.

### Compose / docs / specs

- `docker-compose.yml`: agent service `cpus`/`mem_limit` (Decision 9 sizing; coordinate with #39's build-context change).
- `docs/worker-setup.md`: the knob, sizing guidance, plan-gate slot holding, cap>1 residuals (ADR wording), `RUN_MAX_REQUEUES` recommendation, shared-token caveat, same-repo serialization note.
- `specs/ai.md`: §173 already references ADR-42 + this PRD (added at PRD creation); §877-878 prose updated at landing.
- `ARCHITECTURE.md`: run-lifecycle section notes the bounded-concurrency model and links this PRD.

## Milestones

- [x] **M1 — Per-run executor + HOME isolation (security-critical)**: per-execution `SdkExecutor` (factory wiring in `main.ts`/`runner.ts`), runId-scoped `killAgentTree` incl. the pre-push call sites, per-run HOME + terminal cleanup, SecretRegistry run-scoped eviction, shared-collaborator audit (incl. `GitLabClient`) with findings recorded; tests incl. the two-execution kill/reap acceptance test and HOME isolation. Validation: `cd agent && npm test` green; audit notes committed to this PRD.
- [x] **M2 — Bounded concurrent claim loop**: slot semaphore + `WORKER_MAX_CONCURRENT_RUNS` (default 1, warn > 8) + at-capacity log + graceful shutdown draining; behavior at default identical to today (existing tests unchanged ↳review — "byte-for-byte" retired, the loop structure does change); new tests for cap honored, at-capacity backoff, slot release on failure paths, slot held across a gated plan. Validation: worker with cap 2 executes two stub runs concurrently in `agent` tests.
- [x] **M3a — Cap advertisement + UI count (parallel-safe from day one)**: migration `00075` (workers.max_concurrent_runs), register payload + handler, list queries `EXISTS→count`, DTOs, web badge "N/M runs"; Go + vitest coverage. Validation: workers page shows "2/2 runs" against a cap-2 worker with two stub runs.
- [x] **M3b — Compose resource limits**: agent service `cpus`/`mem_limit` sized per Decision 9 (run + chat slots), `RUN_MAX_REQUEUES` guidance; coordinate the compose merge with #39. Validation: `docker compose config` renders limits; e2e stack still green.
- [x] **M4 — Docs + specs landing**: `specs/ai.md` §877-878 prose fix (§173 already links ADR-42), `docs/worker-setup.md` (frontmatter rules per `docs/README.md`) incl. the cap>1 residuals section, `ARCHITECTURE.md` note, `worker.ts`/`config.ts` comment updates, #39 D4 `spawnedPids` wording reconciled. Validation: `cd web && npm run build` (check-docs) green.
- [x] **M5 — E2E validation**: e2e scenario on the isolated stack (`./e2e/run-e2e.sh` extension): one worker, cap 2, two PRD issues on different repos → both runs progress concurrently (interleaved run_messages), both land MRs; a mid-run worker kill requeues both runs together. Stated limit (↳review): the stub executor is already concurrency-safe, so this exercises the loop/server/UI path, NOT the executor kill/reap fix — M1's unit test is the guard for that. Validation: e2e green.

## M1 audit notes (Decision 6 — shared-collaborator concurrency audit)

Everything `RunRunner` closes over that two concurrent `execute()` calls share, audited for run-vs-run:

- **The executor (was the primary hazard) — RESOLVED by construction.** Previously one shared `SdkExecutor` with one instance-scoped `spawnedPids` Set; two runs would wipe/kill each other's tree (the B1 reap). Now a per-run factory builds a fresh executor per `execute()`, so `spawnedPids` + `killAgentTree` are private per run. Proven by the two-concurrent kill/reap test (each run's pre-push reap touches only its own pids; the sibling's set is intact).
- **`this.gitlab` (GitLabClient) — SAFE, stateless per call.** Constructor sets only `fetchFn` + `httpTimeoutMs` (immutable); `createMergeRequest`/`findOpenMr`/`request` take every input as a parameter and hold no mutable per-call state — each call is an independent fetch. Same property #39 verified for WorkerClient; confirmed identical for run-vs-run. Two concurrent runs opening MRs is safe (the test exercises it).
- **`this.git` (GitCache) — SAFE, per-bare-path lock.** Only mutable state is `this.locks` (Map<barePath, chained Promise>). `withLock` does its get+set synchronously (no await between), so concurrent calls for the same key chain correctly; same-repo runs serialize their clone/fetch/worktree/push (Decision 11 — correct, just slower), different-repo runs run in parallel. Worktree dirs are per-run (`worktrees/<repo>/issue-<iid>`, distinct iids); the server's `uq_runs_one_active_per_issue` + cross-kind same-branch exclusion guarantee no two active runs share a branch/worktree key. No shared-state corruption.
- **`this.log` (Logger/SecretRegistry) — SAFE, re-audited for the new eviction path.** #39 verified add/scrub are synchronous-no-await. Decision 7 adds `removeSecret`; the registry is now a ref-counted Map, still all-synchronous, so no concurrent run observes a half-updated registry on Node's single thread. Ref-counting closes the specific run-vs-run hazard: a completed run's terminal eviction cannot un-scrub a secret a same-user sibling still holds. Child loggers share the registry by reference (unchanged).
- **`this.client` (WorkerClient) — SAFE, stateless per call** (every method takes runId; #39 verified; unchanged).
- **Immutable fields** (`batchMs`, `joinToken`, `pollMs`, `planApprovalTimeoutMs`) — primitives set once, never mutated. Trivially safe.
- **Per-execution collaborators built INSIDE `execute()`** (`runLog` child, `redact`/`redactText`, `MessageBatcher`, `cancel` AbortController, `SteeringChannel`, `observedSessionId`, `barePath`/`worktreePath`) — all per-run, nothing shared. #39's "each run builds its own batcher/redactor/steering" still holds.

**Conclusion**: with Decision 4 (per-run executor) + Decision 7 (ref-counted eviction) landed, no shared collaborator is unsafe for run-vs-run. The only shared mutable state is `GitCache.locks` (correct serialization) and the SecretRegistry Map (synchronous + ref-counted). `GitLabClient`/`WorkerClient` are stateless-per-call; all other run state is per-execution or immutable. The equivalent latent hazard on the chat lane — a single shared `ChatExecutor` whose instance-scoped `spawnedPids` would have been shared across concurrent chat sessions — was closed the same way: `a2c0a31` moved `ChatRunner` to a per-session `ChatExecutor` factory, so PRD #39's chat lane inherits the same per-execution isolation this audit establishes for runs.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched | Repo area |
|---|---|---|---|---|
| 1 (parallel) | M1 (agent executor/HOME/registry), M3a (api migration+queries+web badge) | — | `agent/src/{sdk-executor,runner,main,log}.ts`, `agent/test/` · migration `00075`, `runtime.sql` list queries, register handler, `web/src/lib/api.ts`, `WorkersSettings.tsx` | agent · api+web |
| 2 | M2 | M1 (executor must be concurrency-safe first) | `agent/src/worker.ts`, `agent/src/config.ts` | agent |
| 3 (parallel) | M3b, M4 | M2 (M3b sizing needs the final slot model; M4 documents it) | `docker-compose.yml` · `docs/`, `specs/`, `ARCHITECTURE.md`, `prds/39-chat-agent.md` (wording) | root+docs |
| 4 | M5 | M2+M3a | `e2e/` | e2e |

Coordination: PRD #39 Phase 2 touches `worker.ts`/`steering.ts` and its M2 edits the same compose agent service — the intended order is this PRD's M1/M2 first, then #39 builds its chat lane on the substrate and rebases its compose edit.

## Out of Scope

- Per-user Anthropic-token concurrency budget / cross-run 429 coordination (recorded enhancement; no prior art anywhere — Decision 12).
- Server-side enforcement of any worker cap (ADR Option B, rejected; the advertised cap is observability only).
- Container-per-run / ephemeral worker pods and the uid-split/`shareProcessNamespace` hardening that closes the cap>1 residuals (ADR Option C — the k8s-operator era, already deferred in specs/human.md).
- Raising the default above 1, or any autoscaling of workers.
- Parallelizing git operations within one repo (GitCache lock stays).
- Slot release/reacquire at the plan gate (Decision 2 holds the slot; revisit only if real usage shows gate-pinned slots are a problem the 24h timeout doesn't handle).
- PRD #39's chat lane itself (it consumes this substrate; it is specified there).

## Accepted residuals (named, per security review — all gated on cap>1; default 1 is today's behavior)

- **Sibling `/proc` PAT exposure during a push window**: a live concurrent agent can read the transient git child's environment (same uid), eroding layer 2 for the sibling run. Layer 1 (protected `main`, Developer role) is untouched. Fix belongs to the k8s uid-split/container-per-run era; until then it is documented at the knob.
- **Bash cross-run writes**: the deny-list screens push/proc/env/secrets but not out-of-worktree file writes; a prompt-injected run can poison a sibling's worktree or the shared bare cache. The human-merge backstop on protected `main` is what bounds the blast radius.
- **Cross-run availability**: a memory-ballooning run can OOM the shared container; all runs requeue together (safe for `main`), and an innocent sibling can *fail* after `RUN_MAX_REQUEUES` — mitigated by sizing guidance + the requeue recommendation, eliminated only by per-run cgroups (Option C).

## Success Criteria

- With `WORKER_MAX_CONCURRENT_RUNS` unset, behavior is identical to today at the observable level: one run at a time, all existing tests and e2e unchanged and green.
- With cap 2 and two queued runs on different repos, one worker executes both concurrently: interleaved live feeds, two MRs, no cross-talk in messages, logs, or subprocess handling.
- Killing one concurrent run's agent reaps exactly that run's subprocess tree (B1 preserved per-run); the other run completes unaffected (M1 unit test).
- A run parked at the plan gate holds exactly one slot; a cap-2 worker still executes a second run alongside it.
- Worker crash with two in-flight runs requeues both (existing sweeper semantics, now exercised at N=2).
- Workers UI shows a truthful `active_runs/cap`.
- ADR-42 lives at `adr/0042-worker-run-concurrency.md`, referenced from specs/ai.md §173 and this PRD; no doc still claims the one-run invariant as unconditional; `docs/worker-setup.md` documents the cap>1 residuals where the knob is documented.
