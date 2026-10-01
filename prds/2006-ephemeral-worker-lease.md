# PRD #2006: Ephemeral worker lease: keep a finished run's worker warm for same-branch follow-up work

**Status**: Implemented (M1, M2); hosted k8s acceptance pending.

## Problem

A user who has opted in to ephemeral (run-bound) hosted workers gets a fresh worker for every run. When that run reaches a terminal state, the api deletes the worker row at once (`maybeTeardownEphemeral` in `api/internal/workersvc/service.go` calls `DeleteEphemeralWorkerForRun`; `ReapEphemeralWorkers` is the sweeper backstop), and the controller removes the pod and PVCs on a subsequent successful reconciliation. The follow-up work a PR typically needs (an `mr_rework`, a `ci_fix`, an on-demand rework, a re-run of the issue on its branch) then pays the whole bootstrap again: pod scheduling, PVC and `seed-nix` initialization, registration and a cold bare clone.

PRD #1214 designs a lease for this as its optional M9, but with a default of zero, gated on M8 measurements and bound to that PRD's PR-session lineage, which is parked. Nothing ships it.

## Outcome

A finished ephemeral worker stays available for a bounded lease and picks up follow-up work on the same branch, so that work starts on a warm worker instead of a cold one. The lease is an operator setting with a non-zero default; `0` restores today's behaviour exactly.

Acceptance examples:
1. Lease 2h. Issue run R on branch `agent/issue-7` completes on ephemeral worker W. Its PR gets review comments; the `mr_rework` run for that PR (branch identity from `pipeline_ref`) is queued 20 minutes later. W claims it; no new worker is provisioned.
2. Same W, still leased. A run of the same owner on another branch, another repo, or another owner's run is queued. W never claims it; normal placement (persistent worker or a new ephemeral worker) handles it.
3. The owner is at the ephemeral cap, all of it held by leased-idle workers, and a run that none of them may claim is queued. The oldest leased-idle worker with no custody hold is released and a new worker is provisioned. If none is releasable, provisioning is refused exactly as today.
4. Lease `0`. Teardown timing and the reaper's selected rows are identical to today.

## Out of scope

- Retained transcript or session context across runs (PRD #1214 core). The lease reuses the worker (pod, PVCs, Nix store, bare repository), not the agent's conversation.
- Reuse by isolated-lane workers (PRD #1906) and by repo-less runs (`job`, `judge`, `chat`): the lane's one-run-per-worker invariant stays, so no file survives into another run. These workers keep today's immediate teardown at any lease value.
- Ending a lease on node disk pressure. The controller already excludes ephemeral workers from its disk-pressure roll, and a kubelet eviction alone does not remove the worker row; no new signal is added here.

## Modules and seams

- **Lease setting** (`api/internal/config`): one duration, `UZI_EPHEMERAL_LEASE`, default `2h`, maximum `2h`, `0` disables. The api refuses to boot on a malformed, negative or over-maximum value. Rendered from the Helm value `workers.ephemeralLease` into the api Deployment env, and settable in `docker-compose.yml` for compose. No separate admin-mutable setting.
- **Effective branch identity** (`api/internal/workersvc`, one function shared by every consumer below): a server-derived, non-empty branch per run kind. `mr_rework` uses `pipeline_ref` (its `runs.branch` is NULL, see `claim_assembly.go`); `ci_fix` uses its failure-snapshot ref; an issue run uses `runs.branch` when set and otherwise the canonical branch derived from its validated `issue_iid` by the existing `agentIssueBranch` convention (`api/internal/workersvc/ci_fix.go`), since a queued issue run normally has `branch` NULL. A kind or row with no derivable non-empty branch has no identity and is never lease-eligible: fail closed, never treat two NULL or empty branches as equal.
- **Lease transitions** (`api/internal/store/queries/hosted_workers.sql`, `runtime.sql`, a migration adding the lease columns to `workers`): a worker enters a lease only from the terminal transition of the run it actually served (its bound run, on that worker, ordinary repo-backed, not isolated). Lease entry, claim-and-rebind, expiry, eviction, drain and deletion serialize on the worker row so that one leased worker is rebound by at most one run, a claim never reaches a deleted or expired worker, and the one-worker-per-bound-run constraint (`uq_workers_ephemeral_run`) holds throughout. A claim inside the lease rebinds the worker to the new run and clears the lease; that run's terminal starts a new lease. Busy and custody-hold guards on deletion are unchanged.
- **Claim and placement** (`runtime.sql`): during its lease a worker is eligible only for a queued run with the same owner, same repository id and the same effective branch identity as its last bound run, with every existing authorization, capability, tier, drain and branch-serialization check intact. The same predicate is mirrored wherever ephemeral eligibility is computed today: the claimant clause, spread-peer eligibility, `CountOnlineWorkersClaimableForRun`, and the provisioning queries `ListUnplaceableQueuedRunsForEphemeral`, `ListSaturationQueuedRunsForEphemeral` and `ListIsolatedQueuedRunsForEphemeral`, so a run an eligible warm worker can take is not provisioned for.
- **Quota and eviction** (`api/internal/hostedsvc/ephemeral.go`, `CountEphemeralHostedWorkersForUser`): leased-idle workers count against `UZI_EPHEMERAL_MAX_PER_USER`. The provisioning queries currently drop at-cap owners before `provisionOne`; they change so an at-cap owner whose cap is held only partly by busy workers can still reach provisioning, which first releases the oldest leased-idle worker that is neither busy nor custody-held. If none is releasable, provisioning is refused as today.
- **Visibility** (worker DTO, `uzi worker list`, web Workers page): a leased-idle worker shows as idle with its remaining lease; it counts in the fleet and ephemeral totals.
- **Controller**: no lifecycle change. A leased worker's row stays present, so the controller keeps the Deployment; it is removed when the row is. Drain and image roll end a lease (a leased-idle worker is idle, so it rolls or is released immediately, never held for its lease).

## Testing decisions

- LiveDB equivalence at lease `0`: terminal teardown and the reaper's selected row set match the pre-feature behaviour for the same fixtures (follow the existing `DeleteEphemeralWorkerForRun` / `ReapEphemeralWorkers` tests).
- LiveDB claim tests: same-branch follow-up claimed by the leased worker for each supported kind (issue re-run, `mr_rework`, `ci_fix`); other branch, repo, owner, a run with no derivable effective branch identity, and an unknown kind never claimed (a queued issue run with a valid `issue_iid` stays eligible through its canonical branch); isolated-lane and repo-less workers never leased.
- Forced-interleaving regression tests (two transactions, deterministic ordering, as in existing claim-race tests): two follow-ups racing for one leased worker (exactly one wins), claim versus expiry, claim versus eviction, claim versus drain, claim versus deletion. Each with a mutation that removes the serialization and reddens it.
- Quota tests: eviction picks the oldest releasable leased-idle worker; busy and custody-held workers are never evicted; nothing releasable means provisioning refused.
- Placement mirror test: for a run a leased worker can take, the provisioning queries do not select it and `CountOnlineWorkersClaimableForRun` counts the leased worker.
- Issue re-run identity: a re-run created through the real create service (no hand-populated `branch`) is claimed by the leased worker of the run that served that issue's branch; a malformed or unsupported identity is never eligible.
- Config: boot validation for malformed, negative and over-maximum values. Chart: a new offline render assertion, `scripts/assert-ephemeral-lease-render.sh` behind a `render:ephemeral-lease-check` Taskfile target following `render:drain-knobs-check` (needs helm + yq; like the other `render:*` checks it is not part of `gate:repo`), proving `workers.ephemeralLease` reaches the api env and its default renders `2h`.
- Controller: a focused test that a leased-idle worker during an image roll is rolled or released at once, never held for its lease, even though controller production code is not expected to change (the existing row-presence tests do not prove this interaction).

## Milestones

- [x] **M1: A finished ephemeral worker is leased and takes a same-branch follow-up.** Lease setting (config, chart value, compose, boot validation), migration, effective branch identity, lease entry on terminal, lease-aware teardown and reaper, claim predicate with all placement mirrors and provisioning queries, atomic transitions, quota eviction, worker DTO plus `uzi worker list` display, `docs/hosted-workers.md` and the chart values docs, CHANGELOG. Tests per the Testing decisions. Blocked by: none. Correct the stale comment at `api/internal/hostedsvc/protocol.go` (the `Ephemeral` field says a disk-pressured ephemeral worker is torn down, while `controller/internal/kube/materializer.go` excludes ephemeral workers from pressure recycling). Gates: `task gate:api`, `task gate:controller`, relevant LiveDB tests via `./e2e/run-store-it.sh`, `task gate:repo`, `task render:ephemeral-lease-check`.
- [x] **M2: The web Workers page shows leased-idle workers.** Lease state and remaining time on the Workers page, counted in fleet totals; mock-mode fixture. Blocked by: M1. Gate: `task gate:web`.

No `.github/workflows/**` change in implementation or validation.

## Acceptance (hosted k8s, maintainer-owned)

Feature completion requires hosted k8s evidence, tracked in the linked `acceptance` issue #2008: warm same-branch follow-up reuse, lease expiry teardown, quota eviction of a leased-idle worker, and drain/roll of a leased-idle worker. Implementation may merge before this acceptance completes; the PRD moves to `prds/done/` only after it.

## Decision Log

- 2026-10-01: Default lease is 2h, maximum 2h, chart-configurable, `0` disables. The maintainer asked for at least 30 minutes and preferred 2h; a 2h window is the maintainer-selected value, not a measured review-cycle length. Raising the maximum needs a concrete use case. This supersedes PRD #1214 M9's "default zero" for the lease; #1214's Decision Log points here.
- 2026-10-01: Bind reuse to owner + repository id + server-derived effective branch rather than #1214's PR-session lineage, so the lease ships without the parked session core. This gives bounded worker reuse, not transcript lineage. Rejected: waiting for #1214 M8/M9.
- 2026-10-01: Isolated-lane and repo-less workers are excluded; widening the lane's one-run-per-worker invariant needs its own design.
- 2026-10-01: No disk-pressure lease ending: there is no signal that removes the worker row on kubelet eviction, and the controller already skips ephemeral workers in its disk-pressure roll.
- 2026-10-01: Hosted acceptance is required for completion (not optional), because this changes scheduling and lifecycle on the primary runtime; merge does not wait for it.
- 2026-10-01: Reviewed with a Codex buddy (four rounds on the issue draft; findings on branch identity (incl. issue re-runs via `agentIssueBranch`), chart render checks, lane exclusion, placement mirrors and quota prefilters, atomic transitions, and lifecycle wording folded in).
- 2026-10-01: The effective branch identity is one SQL function, `fn_run_lease_branch` (migration `00282_ephemeral_worker_lease.sql`), not a Go function, because every consumer is a SQL predicate. A Go/SQL parity LiveDB test pins its issue and `ci_fix` arms to the Go sources (`agentIssueBranch`, `claimPipelineFromSnapshot`); the `mr_rework` arm mirrors `pipeline_ref`, the source `claim_assembly.go` uses. An issue run's identity is server-derived: `agent/issue-<iid>` only when its `branch` is NULL, empty or equal to that, otherwise no identity.
- 2026-10-01: Drain and roll end a lease by clearing it in `CordonHostedWorker` and `RegisterWorker`; a rolled or restarted pod re-registers and the reaper releases the row. The controller is unchanged. A crash-restart also ends a lease (fail closed).
- 2026-10-01: Deliberate narrowing of "any terminal": a lease starts only when a worker-reported `completed` or `failed` commits in a transaction holding the worker lock (the `setState` generation fence tx, or the `completeRunWithPermitLease` permit tx) and no custody hold is left open. A worker-reported `cancelled`, a failed report routed to cancelled, a nil-generation report (on both the fence and the permit path), and sweeper, cancel and server-side failures keep immediate teardown, because the worker's state is unknown. A plan-rejected run ends `failed` and may lease. Lease entry, the completed run's custody release and the terminal write commit together, so the reaper sees either a non-terminal run or a live lease.
- 2026-10-01: Known limitation: the display-only health rungs (`CountOnlineWorkersWithFreeSlotForUser`, `CountOnlineWorkersSatisfying*`) are not lease-aware, so a run a leased-idle worker could take may briefly show "all workers busy".
- 2026-10-01: Fresh clock: claim admission reads `clock_timestamp()` immediately before `ClaimRun` (after the worker lock, run locks, snapshot replace and claimant guards), the rebind re-checks the lease against its own `clock_timestamp()`, and a refused rebind rolls the claim back to a savepoint and reports idle. The same discipline applies on the no-snapshot claim path.
- 2026-10-01: Deadlocks (40P01) are retried by the caller, not server-side: the agent's state report retries 5xx (`agent/src/client.ts` `reportStateOnce`), a claim error is retried on the next claim poll, a provision error on the next provisioner pass, and a repo delete is re-issued by the user. A claim racing a provision of another ephemeral worker for the same run ends in a unique violation (claim reports idle) or a 40P01 on one side; no state is corrupted.
- 2026-10-01: At-cap eviction removes the oldest releasable (not busy, no custody hold) leased worker only for an owner exactly at the cap with the lease on; an owner over the cap (cap lowered across a restart) or with the lease at `0` is refused without eviction. The web Workers page's manual hosted quota count now excludes ephemeral workers, matching the server's `CountHostedWorkersForUser`.
