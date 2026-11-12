# PRD #4206: Worker hibernation: reclaim rebuildable disk caches at hibernation

**Epic**: #4204. **Status**: Draft. **Blocked by**: PRD #4205, merged.

## Problem

A worker that wakes from hibernation comes back with whatever filled its disk before it slept. Docker build cache and dangling images are rebuildable, but they are only pruned under disk pressure today. That is part of why runs park on `data_volume_full`. Hibernation is the safest moment to reclaim them: the worker has no assigned runs and no custody, and claims are fenced.

## Outcome

On the Docker lane, hibernating a worker first runs one guarded DinD cache prune. A prune that settles as pruned, or as refused before its command started, lets hibernation proceed; the outcome and reason are shown on the worker. A prune whose settlement is unknown, or whose evidence was lost, keeps the claim fence and blocks hibernation until it is resolved. Plain workers have nothing to prune in this PRD, see Out of scope.

Acceptance examples:

1. A Docker worker at 40% DinD usage is hibernated. The hibernation prune runs below the pressure threshold, removes build cache and dangling images, keeps every container and named volume, and only then does the worker scale to zero.
2. The prune is refused before its command starts, for a reason that does not touch sleep safety (the worker lacks the capability, or the daemon is unreachable). PRD #4205's independent admission evidence is still valid, so the worker hibernates, and its Workers row shows "cleanup skipped: <reason>".
   - When custody evidence is lost or stale, or there is evidence of active work (a non-exclusive claim gate), the refusal **withdraws** the hibernation instead. The worker goes to `waking` per PRD #4205, and claims reopen only on confirmed settlement.
3. Demand that arrives during the prune withdraws the hibernation, exactly as in PRD #4205. The prune is abandoned safely: the existing fenced maintenance phases already support cancellation.

## Out of scope

- **Settled attempt trees (#3329).** They are not cache. #3329 rejects in-life deletion without writer exclusion. Its chosen design is reclaim at a verified pod epoch, which is PRD #3431's startup path. A woken worker's new pod runs that existing path under its existing opt-ins and attestation. Nothing here deletes attempt trees.
- **Stopped containers and named volumes.** They may hold user state (`agent/src/dind-prune.ts:31-35`).
- **`/data` content, bare repos, `refs/uzi-runner/*`, `/data/recovery`.** Never touched.
- **`/nix`.** Rebuildable, but deleting it is volume retirement, a later child PRD of the epic.
- **The Codex command cache.** It is an `emptyDir` (`/var/cache/uzi-codex-cmd`, `render.go` ~1337) and is freed automatically when the pod stops. That is documented, with no code.

## Modules and seams

- **Two existing mechanisms, different commands.**
  - **Server-fenced DinD maintenance (#1760):** `agent/src/dind-maintenance.ts`, `api/internal/workersvc/dind_maintenance.go`, the poll field `dind_maintenance`, and the controller's `maintenance*.go`. Its command is anonymous-volume prune (`DIND_VOLUME_PRUNE_ARGV`), and its phases include stop/recycle.
  - **Pressure-triggered cache prune:** `agent/src/dind-prune.ts`, which prunes build cache and dangling images with frozen argv. Containers and volumes are never removed.
- **Hibernation cleanup reuses mechanisms, not commands.**
  - It reuses the maintenance path's fencing and identity mechanisms: the server-issued op id, custody evidence (`custody_clear`, `custody_checked_at`), exclusive claim gating (`local_claims` / `local_executions` = 0) and the settlement report.
  - It invokes **only** `dind-prune.ts`'s cache/image allowlist argv, with no pressure precondition.
  - It never runs volume prune, and never enters a stop or recycle phase.
  - Existing maintenance behaviour and the pressure loop are unchanged.
  - It is a distinct operation kind (`hibernation_cache_prune`), so the two can never be confused on the wire.
- **Prepare phase.** PRD #4205's `preparing` state gains a cleanup step.
  - Scale-down waits for a **confirmed settlement** of the prune: `pruned`, or `refused` with the command never started.
  - A command whose settlement is unknown (interrupted, timed out at 10 min, or worker lost mid-command) **keeps the claim fence**. The worker stays `preparing`, and an Admin health blocker is raised. It neither proceeds to scale-down nor reopens claims until settlement is confirmed. An admin wake, demand withdrawal or PRD #4205's `preparing` deadline may request cancellation of the prune, but claims stay fenced until its settlement is confirmed.
  - Settlement evidence that reveals custody or active work re-runs PRD #4205's admission check, which withdraws the hibernation on failure.
- **Records.** The outcome is recorded on the worker row (`hibernation_cleanup`: `pruned` / `skipped` / `failed`, plus a reason) and shown on Admin → Workers.
- **Protocol additions.** The trigger is a new value on an existing protocol field. The api/agent protocol mirrors and the poll-wire golden change together. The worker image must support it: gate it behind a new worker capability (`api/internal/capability`). An older worker skips cleanup with the reason `unsupported`.

## Testing decisions

- **Agent unit tests.**
  - The hibernation trigger prunes below threshold.
  - Containers and volumes are untouched (frozen argv asserted).
  - Stale custody evidence refuses.
  - Cancellation mid-prune.
  - Follow `agent/test/dind-maintenance*.test.ts`.
- **LiveDB.**
  - The prepare step waits for a confirmed settlement, then proceeds.
  - A settled refusal for a non-safety reason still hibernates and records the reason.
  - A refusal for lost custody evidence or active work withdraws the hibernation.
  - An admin wake during an unsettled prune keeps claims fenced until settlement.
  - An unknown settlement keeps the fence and raises a blocker.
  - New custody evidence re-runs admission.
  - Existing maintenance and pressure-prune behaviour is unchanged (existing tests stay green).
  - Demand during the prune withdraws the hibernation.
  - A worker without the capability skips the prune.
- **Contract.** The re-recorded golden.

## Milestones

**M1: Hibernating a Docker worker reclaims its DinD cache first.** Blocked by: none within this PRD.

Covers the trigger, prepare-phase wiring, the capability, the outcome record and display, docs (`docs/worker-docker.md`, `docs/hosted-workers.md`) and tests.

Acceptance: examples 1-3.

## Decision Log

- **D1. Reuse the server-fenced DinD maintenance path** rather than the pressure loop, which usually prunes nothing on a quiet worker. The other rejected alternative was a new prune command, which would duplicate the fencing.
- **D2. A settled refusal does not block hibernation; an unsettled operation does.** Hibernation's value, freed CPU and memory, stands without the prune. But scaling down, or reopening claims, while a destructive command's settlement is unknown would break the fence the prune relies on.
- **D3. No attempt-tree deletion.** #3329's writer-exclusion constraint stands. A woken pod uses the existing pod-epoch reclaim path.
