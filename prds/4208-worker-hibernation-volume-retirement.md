# PRD #4208: Worker hibernation: retire a long-hibernated worker's volumes

**Epic**: #4204. **Status**: Draft. **Blocked by**: PRD #4205, merged. **Activation**: needs PRD #4207 live, since without automatic hibernation only manually hibernated workers qualify.

## Problem

A hibernated worker still holds its PVCs: data, nix, and on the Docker lane dind-data. Storage is provisioned, not used, so pruning files saves nothing. A worker asleep for weeks holds storage nobody is using.

## Outcome

When volume retirement is enabled, a worker that has been continuously hibernated for the retirement period has its PVCs deleted, but only when uzi can prove the volumes hold nothing protected. The default period is 21 days. The worker row, its configuration and its Secret stay. On its next wake it gets fresh volumes and starts like a new worker. When proof is missing, the volumes are kept and Admin health says why.

Retirement is **irreversible**, so it takes two switches, and both are required:

- the live admin setting `worker_hibernation_retire_enabled`, default false;
- a chart ceiling, `workers.hibernation.retirement.allowed`, default false.

Acceptance examples:

1. **Retire after 21 days, then wake fresh.** A worker hibernated 22 days ago, whose inventory shows only disposable sources, has its volumes deleted. Its Workers row shows "volumes retired". A run later wakes it onto fresh PVCs, and it registers and runs.
2. **Kept when work may be lost.** A worker whose data volume still holds the tracking ref of a failed run with no verified server-side copy keeps its volumes. Admin health names the worker and the source.
3. **Wake cancels retirement.** A wake requested before deletion commits cancels the retirement. A wake requested after deletion commits waits for the deletion to finish, then provisions fresh volumes. A concurrent wake's replacement volumes are never deleted.

## Out of scope

- Deleting server-side recovery captures.
- Deleting the worker row.
- Retiring ephemeral workers.
- Any user-facing control.

## Modules and seams

Safe-deletion invariant, required in full:

- a **complete, identity-bound inventory** of the volumes has no protected or unknown sources;
- **every server-side disposition** it relies on is still valid when re-checked just before deletion;
- **no writer** can change the inventoried volumes.

If current machinery cannot prove all three for the whole volume, uzi skips deletion and reports the blocker. Attempt-tree eligibility (PRD #3431) is not whole-volume deletion authority.

**Inventory at hibernation.** Two parties each prove what only they can see, and the api binds the two halves:

- **The controller** (it holds the kube credential) reports the PVC identities **before any scale-down mutation**, in a new pre-mutation transition `observed` on PRD #4205's route. It carries the names and UIDs of the data, nix and dind-data PVCs, the pod UID and the Deployment resourceVersion.
  - **Order with this PRD active:** `observed`, then the api puts the identity in the snapshot, then the worker inventories and echoes it, then the api persists the bound inventory, then the snapshot permits scale-down, and only then does the controller send `scaling_down`.
  - **Writer exclusion:** the inventory is taken under confirmed writer exclusion, held from before the inventory until the pod terminates.
    - **Candidate mechanism:** the existing pod-epoch lock (`prds/3430-pod-epoch-lock-shadow.md` section 1, lifetime shared ownership, exclusive for reclaim).
    - **Its limits today:** it is rendered only for DinD pods (`UZI_EPOCH_LOCK=1`), and the worker process itself holds it shared for its lifetime.
    - **M1 must establish the supported exclusion path per lane and record it in the Decision Log.** One example is the worker quiescing its own writers and upgrading to exclusive. Claims are already fenced by PRD #4205.
    - **Fail closed:** a lane with no proven mechanism never retires its volumes. Neither does a writer class the mechanism does not cover, or a failure to acquire or hold exclusion; in each case the inventory is invalid and the volumes are retained. Plain (non-DinD) workers are therefore retained until such a mechanism exists for them.
  - **Inventory validity:** the inventory stays valid only if, after it was persisted, the controller's next observations for that op are `scaling_down` and then `scaled_down` for the **same** pod UID, with no other pod in between, and the worker reported holding exclusion until termination. A wake, any other pod, or lost exclusion invalidates it, and retirement is refused.
- **The worker** has no kube credential and cannot see PVC UIDs or the DinD daemon's backing filesystem. In the prepare handshake, before scale-down, it reports a content inventory, bound to the same op id and its own pod UID:
  - attempt trees, canonical clones, journals, bare repositories and `refs/uzi-runner/*`, recovery staging, and unsettled operations, using the source classifications `agent/src/pod-epoch-reclaim.ts` already uses;
  - on the Docker lane, what the Docker API reports: containers (any state), named **and anonymous** volumes, and anything unclassified.
- **Binding.** The api puts the controller-observed identity into that worker's poll snapshot, as `DindMaintenance` does with `PVCUID` and `RegisterNonce`, and the worker echoes it in its inventory. The api accepts the inventory only if the echo matches the controller-reported PVC UIDs, the pod UID and the current register nonce for that op. The result is stored server-side.
- **Bounds.** The inventory is capped (for example 10,000 entries and 1 MiB). It is **complete** only if the worker positively reports completion. Hitting a cap, or any unreadable path, makes it incomplete, and an incomplete inventory keeps the volumes.
- **Workers with no inventory.** A worker that was already hibernated when this PRD deploys has none, and keeps its volumes.
- **Capability gate.** The inventory is a worker-side change, so it sits behind a new worker capability (`api/internal/capability`). The api rolls before the pinned worker fleet, so a worker without the capability reports no inventory and keeps its volumes.

**Disposition check.** Each retained source needs a positive disposition through its existing authority:

- **Source-specific receipts only.** Each source needs its own existing receipt, re-validated just before deletion. The receipt must cover that source's **latest bytes**: matching tip or tree, prerequisites present for thin bundles, and not expired.
  - A capture merely being `available` is not enough.
  - A run reaching a terminal state, a custody hold being released, or held publication existing does not, on its own, authorize every local source of that run.
- **Protected sources.** Sources that ADR-1786 and `agent/src/pod-epoch-reclaim.ts` classify as protected, unknown or unrecoverable keep the volume.
- **DinD state.** Any container (stopped included), any named or anonymous volume, and any unclassified state count as protected or unknown, and keep the dind-data PVC. In this PRD only an empty daemon inventory (images and build cache only) lets dind-data be retired.
- **`/nix`** is rebuildable, subject to the identity and writer guards.

**Retirement operation (api plus controller).**

- **Reserve.** Atomically reserve retirement against wake, claims and assignments, under the PRD #4205 row lock. Recheck: still hibernated with pods gone, 21 continuous days since `hibernated_at` (a wake resets eligibility), no non-terminal assigned run, no open custody hold, no roll or maintenance, no manual cordon. The inventory's dispositions are re-validated. Unknown evidence keeps the volumes.
- **Delete.** The controller deletes only the reserved PVC UIDs, with UID preconditions, after confirming no pod or writer exists. It acknowledges with the op id.
- **Retired storage is desired state.** `DesiredWorker` carries `storage: {state}`, where state is one of `live`, `retiring` or `retired`. This updates the golden and both protocol structs.
  - Its one reader is the materializer's PVC create and repair gating (`materializer.go:1080-1090`), which suppresses PVC creation while the worker is retired and asleep.
  - There is no separate generation counter: fresh PVCs already change the pod-epoch identity.
  - While a worker is `retiring` or `retired` **and** hibernated, the materializer suppresses its normal PVC creation and repair. Reconciliation never recreates a deleted PVC before a wake.
- **Commit boundary.** There are two durable states, so a reservation is never mistaken for an authorized deletion.
  - **Reserved** (cancellable): storage is `retiring`, with the op id and the reserved PVC UIDs. A wake, either switch turning off, or any failed recheck cancels it back to `live`. Nothing has been deleted.
  - **Committed** (irrevocable): the controller checks its own ceiling env, then sends a `delete_started` transition. The api records `deletion_committed_at` atomically with that transition, and only then may the first delete call be issued.
    - If the ceiling is off at that check, the controller sends `refused` and the reservation is cancelled.
    - From commit onward, the remaining reserved UIDs are deleted even if a switch is turned off. A partially deleted worker is less safe than a finished one.
- **Deletion.** The controller deletes the reserved UIDs one by one with UID preconditions, and reports each deletion through the authenticated transition route.
- **Ambiguous delete results.** A timeout or lost response is resolved by re-reading the PVC by name:
  - absent: confirmed;
  - same UID present: retry;
  - **different UID present: stop**, set `retirement_error`, raise the Admin health blocker, and never delete it. PVC creation is suppressed while retiring, so a different UID means an anomaly.
- **Retired.** Storage becomes `retired` (with `volumes_retired_at` set) only when every reserved UID is confirmed gone.
- **Restart.** After a restart, deletion resumes on the remaining **reserved** UIDs only, whatever the switches say once committed. It never deletes others.
- **Wake during retirement.** A wake while reserved cancels the retirement. A wake while committed waits for `retired`, then provisions fresh volumes.
- **Wake after retirement.**
  - On wake, the materializer creates fresh PVCs only after `retired` is recorded.
  - The worker becomes ready only after registration and readiness, per PRD #4205.
  - The token Secret is kept.
- **Disable.** Turning either switch off cancels reserved (uncommitted) retirements, and blocks new ones. Committed deletions finish, as above, and the worker wakes fresh. This is the one place the controller ceiling does not stop an in-flight operation, and the reason is recorded in the Decision Log.

**Settings.**

- `worker_hibernation_retire_enabled`: bool, default false.
- `worker_hibernation_retire_after`: duration, default `504h`, validated to [168h, 8760h].
- Both sit on the hibernation settings card.
- The chart value renders a controller and api env ceiling. The effective permission is setting AND ceiling.

**Extra refusals.**

- A recovery capture whose `local_replica_worker_id` is this worker (`recovery.sql:945,1001`).
- An active `run_publication_resume` custodian whose `worker_id` is this worker (`publication_resume.sql:86`).
- **Fencing during `retiring`:** claims and the wake pass treat `retiring` as fenced. A wake waits for `retired`.

**Reclaim policy.** Deleting a PVC whose PV has `reclaimPolicy: Retain` frees nothing; it only leaves a `Released` PV behind.

- The M1 dry run reports each PV's reclaim policy and flags Retain.
- M2 refuses Retain-backed volumes unless the operator has acknowledged them in the chart (`workers.hibernation.retirement.allowRetainPVs`, default false).

**Independent controller check.** The controller reads the chart ceiling from its own env. It refuses to **commit** a new retirement on its own when the ceiling is off, whatever the api asks. Committed operations complete (see *Commit boundary*).

**Admin health.** A `fleet.hibernation_retirement` entry lists workers past the period whose retirement is blocked, with the blocking source.

## Testing decisions

- **LiveDB.**
  - Every guard refuses on its own.
  - Wake-versus-delete races, both before and after commit.
  - Partial deletion followed by an api or controller restart.
  - Changed PVC UIDs: a concurrent replacement is never deleted.
  - A failed-run tracking ref with incomplete capture coverage keeps the volumes.
  - Non-cache DinD state keeps dind-data.
  - Disabling cancels an uncommitted retirement.
- **Controller.**
  - UID-preconditioned deletion and the pod-absence check.
  - No PVC is recreated while storage is `retiring` or `retired` and the worker is asleep.
  - Partial-deletion resume deletes only reserved UIDs.
- **Agent.**
  - Inventory completeness against the existing source classifications.
  - The same pod writing protected bytes after inventory (exclusion lost or bypassed) makes retirement refuse.
  - Failing to acquire the exclusive lock invalidates the inventory.
  - The caps make the inventory incomplete.
  - Anonymous volumes and stopped containers are reported.
- **LiveDB, inventory binding.**
  - An inventory whose pod UID does not match is rejected.
  - A worker hibernated before deploy has no inventory and keeps its volumes.
  - A terminal run or released hold without a source-specific receipt keeps the volumes.

## Milestones

**M1: Inventory at hibernation, plus a dry-run retirement report.** Blocked by: none within this PRD.

Covers the inventory in the handshake, the disposition check, and Admin health listing "would retire" / "blocked by X", with **no deletion**.

Acceptance: examples 2 and 3 (blocked and cancelled cases) are visible in the dry-run report.

**M2: Retire, then wake fresh.** Blocked by: M1.

Covers both switches, the reservation, controller deletion, storage state, wake onto fresh volumes, disable semantics, docs and tests.

Acceptance: examples 1-3.

## Owner acceptance

A live check of one retirement and a fresh wake on hosted k8s is filed as a linked `acceptance` issue.

## Decision Log

- **D1. Two switches.** An irreversible action needs a deploy-time ceiling as well as the live setting, and neither alone enables deletion.
- **D2. Inventory plus re-validated dispositions, not elapsed time.** Time alone cannot show the volumes hold nothing protected.
- **D3. A dry-run milestone first,** so the inventory and blockers are observable on a live cluster before anything is deleted.
- **D4. Keep the worker row and Secret.** The rejected alternative was dropping the worker from the snapshot, which tears down everything and loses its identity.
- **D5. Committed deletions complete even after disable.** Stopping halfway leaves a worker with some volumes gone and some kept, which is harder to wake safely than a finished retirement. The cancellable reservation is the point where disabling still stops everything.
