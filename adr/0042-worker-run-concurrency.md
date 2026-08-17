# ADR-42: Worker ↔ run concurrency model

**Status**: Proposed (accepted when PRD #42 is approved)
**Date**: 2026-07-10
**Deciders**: Vlad + three-agent research panel (codebase map, prior art, industry practice); adversarially reviewed by a second three-agent panel (design, security, fact-check)
**PRD**: [prds/done/42-worker-run-concurrency.md](../prds/done/42-worker-run-concurrency.md) (GitLab issue [vtmocanu/uzi#42](https://github.com/vtmocanu/uzi/issues/42)) — the PRD carries the implementation design and milestones; this ADR carries the decision and its rationale.

## Decision (summary)

A worker may execute multiple runs concurrently, bounded by a worker-side slot semaphore with a low default cap of 1 (`WORKER_MAX_CONCURRENT_RUNS`), after the per-run isolation gaps in the worker process are closed. The server deliberately does NOT enforce 1:1 worker:run, but the worker advertises its cap at registration for observability. Scale-out (more workers per user) remains the primary parallelism mechanism; scale-up (N slots per worker) is a supported, opt-in configuration whose intra-user security residuals are documented honestly below — the full isolation story (uid-split / container-per-run) belongs to the future k8s-operator deployment, where each ephemeral pod is a single-slot worker.

## Context

The run is uzi's unit of work: clone a repo, run a Claude agent for minutes to hours, mutate a large working tree, push a branch and open an MR. The worker is an opt-in, per-user container that claims runs over an outbound-only protocol. "One run at a time per worker" is enforced nowhere server-side — it exists only as the worker's serial claim loop (`agent/src/worker.ts:58-73`); `ClaimRun` (`api/internal/store/queries/runtime.sql:147-170`) will hand the same worker any number of runs, and `runs.worker_id` is a plain non-unique indexed FK (`migrations/00020_workers_runs.sql:56`). The question is whether the worker process is a single-run executor (CI-runner style), a multi-run host (queue-worker style), or something in between — and whether the server should enforce the answer. PRD #39 (chat agent) forces the question: its chat lane adds a second concurrent claim loop in the same worker process.

Three research threads informed this decision:

### (1) What the codebase already assumes

The server side turns out to be essentially concurrency-ready with zero changes:

| Area | Keying | Verdict under N runs/worker |
|---|---|---|
| Sweeper / worker-loss recovery (`FailWorkerRunsOverCap`, `RequeueWorkerRuns`, stale-worker sweeps — `service.go:264-286`, `runtime.sql:314-356`) | all of a worker's runs as a set, per-run `requeue_count` budgets | correct as-is: process death orphans all its runs together, batch requeue is the right semantics |
| Heartbeat (`runtime.sql:48-54`, `worker.ts:47-56`) | per-worker | correct: the process is the liveness unit; one stale heartbeat requeues all its runs at once |
| Workspace (`git.ts:106-127`, `runner.ts:227-238`) | per-run worktree (`worktrees/<repo>/issue-<iid>`), collision-proof via `uq_runs_one_active_per_issue` + cross-kind same-branch exclusion | correct: two active runs can never share a branch/worktree path |
| Git PAT scoping (`git.ts:277-297`) | per-subprocess env (`GIT_CONFIG_KEY/VALUE` on the child only) | correct: no process-global credential state |
| Guardrail hooks (`guardrails.ts`, rebuilt per run at `sdk-executor.ts:239-248`) | per-run closures over the run's worktree path | correct: no mutable singleton |
| Steering (`steering.ts`, `SteeringChannel` per runId, `/runs/:id/inputs`) | per-run | correct: the "one poller per run" invariant survives concurrency |
| `run_messages` seq / WS (`runtime.sql:360-365`, hub keyed by runID) | per-run | correct |
| Vault gate on claim (`service.go:308`) | per-user | correct |
| Deletion guard (`CountWorkerNonTerminalRuns`, `runtime.sql:59-68`) | `count(*) > 0` | correct for N |
| GitCache bare-clone lock (`git.ts:36,233-240`) | per bare repo path | correct but serializing: concurrent runs on the *same* repo queue their git ops (slower, never wrong) |

The breaks are all inside the worker process: the shared `SdkExecutor.spawnedPids` set (the B1 reap mechanism — security-critical, latent today because the serial loop hides it), the shared `$HOME/.claude` under concurrent SDK CLIs (verified: session transcripts are isolated by cwd-hash + session id, but history/todos/`~/.claude.json`/credentials state is process-global with concurrent-write races), the process-wide add-only `SecretRegistry` in the logger (`log.ts:30-38,84` — verified leak-free: worst case is over-redaction, but it holds every run's secrets in the heap for the worker's life and scrubbing is O(secrets) per line), and the absence of any CPU/memory limits on the agent service in `docker-compose.yml` (agent service lines 147-190, verified). The `busy` flag in the workers UI is a boolean, not a count (`runtime.sql:24-32`, `web/src/lib/api.ts:450`).

### (2) What the prior art does

All three inspiration systems multiplex many runs per host process; none isolates a run in its own worker process or container:

| System | Model | Cap | Per-job isolation |
|---|---|---|---|
| multica (the system uzi's queue semantics are ported from) | one Go daemon, one goroutine per task, child process per agent CLI | slot semaphore: daemon-wide `MaxConcurrentTasks` default **20** (`daemon/config.go:57`), per-agent `max_concurrent_tasks` **server-side**, default **6** (`migrations/023`); **slot acquired before claim** (`daemon.go:2476,2572`) so claimed tasks never pile up waiting for capacity | git worktree per task from a bare-clone cache; in-place `local_directory` tasks take a per-path lock (`waiting_local_directory` status); claim SQL serializes per (issue, agent) (`agent.sql:349-388`) |
| bottega | one Node process, one SDK subprocess per query | **none** (unbounded in-memory session map; only per-task 409 and per-conversation busy guards) | worktree per task + per-task dev port |
| dot-agent-deck | one Rust daemon, one child process + PTY per session | none global (per-scheduled-task `max_per_run` only) | separate process/PTY; worktree only for issue dispatch |

Notable: none of the three handles Anthropic-API 429 contention across concurrent sessions — each delegates backoff to the child SDK/CLI. Shared-token contention under concurrency has no prior art to copy.

### (3) What the industry does

The industry splits cleanly on one axis — the nature of the job:

- **CI runners** (jobs like ours: long, filesystem-heavy, untrusted code) default to one job per runner and scale horizontally. GitHub Actions: strictly one job per runner process, ephemeral runners + ARC as the recommended scaling model. GitLab Runner: global `concurrent` defaults to **1**; N>1 is realized as separate sub-processes with separate build directories, never shared in-process state. Buildkite: `--spawn N` = N independent logical agents.
- **AI coding-agent platforms are unanimous**: Devin, OpenHands, Google Jules, Cursor cloud agents, and Claude Code cloud all use one fresh VM/container per session, torn down afterwards. OpenHands explicitly describes cross-session sandbox reuse as a state-leakage hazard.
- **Queue workers** (Sidekiq default 5 threads, Celery default = CPU count, Temporal task slots, BullMQ `concurrency`) run N per process — because their jobs are short, homogeneous, in-memory, and mutually trusted. None of those properties holds for uzi runs.
- Where the industry does allow N>1 per worker, it always pairs it with: a hard low-default cap, a workspace per job, and process/container isolation over shared memory.

## The reconciling argument — and its honest limits

The industry's "one sandbox per job" answers **cross-tenant trust**: a shared runner lets job A poison the environment job B (a different customer, different credentials) will run in. Uzi's worker is already per-user: every run it can ever claim belongs to the same user, the same forge connection(s), the same Anthropic token. Within that single trust domain, most of the safeguards the industry demands for N>1 already exist per-run: worktree, guardrail-hook closures, per-subprocess credential scoping. What must not be shared is per-run mutable state inside the process — the `spawnedPids` fix and per-run HOME in PRD #42.

The security review found the original draft of this argument too strong. Two intra-user residuals are **opened by cap>1 and cannot be closed same-uid on compose** — they are accepted, documented, and gate the sizing guidance, and they are why the default stays 1:

- **B1 narrows from process-wide to per-run.** B1's guarantee today is "no agent process is alive during the PAT-bearing push" — the reap covers the whole container because there is only one run. Under cap>1, the *sibling* run's agent is a live same-uid process during run A's push window and can read run A's transient git child's `/proc/<pid>/environ` (the documented string-guard bypass, `docs/proc-hardening.md:104-108`), obtaining that connection's PAT. Layer 1 (GitLab protected branch + Developer role) still holds — `main` remains untouchable — but layer 2 ("the agent holds no push credential") erodes for the sibling. There is no cheap in-container fix (reaping the sibling would kill its run); the real fix is the uid-split / `shareProcessNamespace: false` / container-per-run endgame that `docs/proc-hardening.md` already defers to.
- **Bash is not path-jailed; cap>1 makes it a cross-run integrity channel.** The path guard covers only the file tools (`sdk-executor.ts:243-244` matches Read/Edit/Write/MultiEdit/NotebookEdit/Glob/Grep); Bash gets the deny-list only (`:241`), which screens push/proc/env/secrets/force but has no out-of-worktree write check (`guardrails.ts:283-292`). All runs' worktrees and bare clones are same-uid siblings on one data volume (`git.ts:38-41,108-109`). A prompt-injected run A can therefore shell-write into run B's worktree or the shared bare repo and poison content B pushes *after* B's own plan gate. The human-merge backstop (protected `main`, MR review) is what keeps this non-blocking; it still defeats "each run is independently gated" between concurrent runs. Same endgame as above; not fixable same-uid.

Neither residual weakens any layer at the **default cap of 1**, where behavior is unchanged. The ADR therefore does not claim "same residual as single-run" for cap>1 — it claims: `main` stays protected by layers that don't depend on intra-container isolation, and cap>1 is an informed opt-in whose documentation says exactly this.

## Options considered

### Option A — bounded N-per-worker via slot semaphore, default 1 (CHOSEN)

Multica's slot model, applied to a codebase whose server side already supports it — with one deliberate divergence from multica, stated plainly: multica keeps the per-agent cap **server-side** (`migrations/023`), while uzi's cap is **worker-side config**, because capacity is a property of the machine the worker runs on, which only the worker's operator knows. To keep the observability multica gets from the server-side cap, the worker **advertises** its cap at registration (one nullable column; the server never enforces it) so the UI can show `active/cap` instead of a bare count.

- ✅ Unblocks PRD #39 — which reuses the semaphore *mechanism* and the isolation audit with its own independent chat cap (`WORKER_CHAT_SESSIONS`), not this cap.
- ✅ Matches all three prior-art systems; the required safeguards are small and enumerable.
- ✅ Forces the `spawnedPids` and per-run-HOME fixes, which #39 needs anyway.
- ⚠️ Cap>1 opens the two intra-user residuals above and shares one container's resources and one user token across runs (mitigated by resource limits, the low default, and honest docs).

### Option A′ — Option A without cap advertisement (REJECTED)

Saves one migration, but the UI can then only show "2 runs", never "2/2 — saturated, queue growing", which guts the sizing-guidance story and the operator's ability to reason about capacity. One nullable int column is cheap.

### Option B — enforce 1:1 server-side (REJECTED)

A partial unique index on active `worker_id` + a `NOT EXISTS` guard in `ClaimRun`. Technically tiny and it would make the invariant real. Rejected:

- ❌ Directly blocks PRD #39's chat lane — a DB constraint saying "one active run per worker" is incompatible with "a run and N chat sessions concurrently".
- ❌ Contradicts every prior-art system and buys nothing the low-default cap doesn't: the *worker* is the only claimant of its own slots, so a worker configured with 1 slot never violates 1:1 anyway.
- ❌ Encodes a scaling policy (horizontal only) into the schema, the hardest layer to change.

### Option C — one ephemeral container/VM per run (DEFERRED to the k8s-operator era)

The Devin/Jules/ARC model — the industry gold standard for untrusted code, and the right endgame; it is also the only real fix for both accepted residuals above. But it requires an orchestrator that can spawn containers (docker socket access or a k8s operator), which contradicts the current opt-in, user-started, outbound-only worker on a laptop. specs/human.md already defers exactly this (operator-spawned worker pods). Deferred, not rejected: under the operator, each pod is a single-slot worker, i.e. Option A with cap 1 and an external spawner — the designs compose rather than conflict, and the residuals evaporate there because "sibling run" stops sharing a container.

### Option D — unbounded concurrency (REJECTED)

Bottega's model. No backpressure, unbounded memory/CPU/token contention on a laptop-class host. Bottega gets away with it as a single-user reference server; uzi's worker is a long-lived daemon.

## Decision

Option A with cap advertisement. No server-side cap *enforcement* (the worker is the sole claimant of its own slots; the server keeps enforcing the invariants that are actually cross-worker: per-issue uniqueness, per-branch exclusion, affinity). Option C is recorded as the k8s-era target shape, reachable from A without rework.

**Refined by [ADR-216](0216-fleet-aware-claim.md) (run placement).** ADR-216 makes the server balance load across the fleet inside `ClaimRun`, reading a *peer's* advertised cap to pick a deferral target — this is placement/affinity, an invariant this ADR already assigns to the server, and NOT the cap *enforcement* on the claiming worker that Option B rejected here. This decision is unchanged: the server still sets no ceiling on a worker's own slots.

## Consequences

- The "one run at a time" prose in specs/docs becomes "bounded by `WORKER_MAX_CONCURRENT_RUNS`, default 1".
- The worker process is audited as a concurrent host once (PRD #42 M1/M2); PRD #39 then inherits a clean substrate instead of doing its own audit.
- **Cap>1 carries two documented intra-user residuals** (sibling `/proc` read of a push-window PAT; Bash cross-run worktree writes) whose real fix is the k8s uid-split/container-per-run era. `docs/worker-setup.md` must state them where the knob is documented — raising the cap is an informed decision, not a free speedup.
- A run parked at the plan-approval gate holds its slot (PRD #42 Decision 2) — with cap 1 that is exactly today's behavior ("wedges the worker", `config.ts:57`).
- Shared-token 429 handling stays delegated to the SDK's own backoff (as in all prior art); a per-user token concurrency budget is a recorded future enhancement.
- The workers UI shows `active_runs/cap`; `busy` stays derivable (count ≥ 1).
- One trivial migration (nullable `workers.max_concurrent_runs`, draft `00075` — renumber at landing per CLAUDE.md).
