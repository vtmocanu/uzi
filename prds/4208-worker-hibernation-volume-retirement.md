# PRD #4208: Worker hibernation: retire a long-hibernated worker's volumes

**Epic**: #4204. **Status**: M1 implemented (dry run, no deletion; outcomes conditional); M2 pending. **Blocked by**: PRD #4205, merged. **Activation**: needs PRD #4207 live, since without automatic hibernation only manually hibernated workers qualify.

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

- The M1 dry run reports a reclaim-policy proxy: the StorageClass `reclaimPolicy` of each claim, read through the controller's existing storageclass read (no PV read, no new RBAC). It is shown as "class policy <X>" beside "would retire only after M2 verifies: actual PV reclaim policy; …", so the actual PV policy is explicitly unverified; Retain and unknown (no class, an unrecognised policy, or the read refused) are flagged. It never authorizes deletion: every retire result is conditional on verifying the actual PV policy, which M2 resolves (with the Retain guard below) before any deletion. A new cluster-scoped PV read needs owner approval first.
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

**M1: Inventory at hibernation, plus a dry-run retirement report.** Blocked by: none within this PRD. **Done** (#4208 M1 run; non-closing delivery, live dry-run observation pending under D3).

Covers the inventory in the handshake, the disposition check, and Admin health listing "would retire only after M2 verifies …" / "blocked by X", with **no deletion**.

M1 samples pod presence between controller observations, so its best outcome is conditional (`conditional_retire`); M2 must establish actual exclusion, or obtain a fresh inventory under proven exclusion, before any deletion. A fresh pod list alone does not certify historical validity.

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
- **D6. M1 only, non-closing (owner, 2026-11-14).** The first run implements M1 alone; this PRD stays in `prds/` and #4208 stays open. M2 is dispatched separately, after the live M1 dry run has been observed (D3).
- **D7. Reclaim policy from the StorageClass, as a proxy.** The controller already reads the claim's StorageClass, so M1 reports its `reclaimPolicy` as the class policy, with the actual PV policy listed as unverified (Retain and unknown flagged) instead of adding a cluster-scoped PV read and new RBAC. The proxy never authorizes deletion; M2 must verify the actual PV policy first.
- **D8. Writer exclusion per lane.** Only the DinD lane (`w.Docker && !w.Isolated`) holds the pod-epoch lock, so only it has an exclusion path. Order: close admission (a hibernation latch on a writer registry, plus a `hibernation_inventory` owner on the claim gate); settle in-flight writers within 60 s through their trackers (in-flight claims, worker idle, outbox drain, and registry counters for every other writer class; a class with no tracker is reported uncovered, which makes the inventory incomplete; a timeout reports busy); prove quiescence; upgrade the epoch lock from shared to exclusive (flock conversion is non-atomic, so it is a close-and-reopen; on failure shared is re-taken and `exclusion_failed` reported); then take the census. Exclusive is kept until process exit; when the api's hold signal disappears or names another op, the worker re-takes shared (refusing a replaced lock inode) or exits non-zero. Plain and isolated workers have no mechanism and are retained (`lane_without_writer_exclusion`).
- **D9. The inventory never blocks hibernation.** The controller waits (`await`) only while the row is `awaiting`; every locked status (accepted, a worker-reported status, `timed_out` after 3 minutes, `no_capability`, `closed_by_scale_down`) permits, all inside the 10-minute preparing deadline. Acceptance is a guarded update under the worker row lock, valid only while the worker is preparing and no scale-down is recorded. When the controller sees no worker pod or no claim, it ignores an `observe` gate (there is nothing to inventory, and the missing row keeps the volumes as `no_inventory`) but still holds under `await`. A gate value the controller does not recognise permits, which only skips the inventory and so keeps the volumes; a future holding gate value therefore needs a controller upgrade.
- **D10. Final attestation bound to the process.** On shutdown, with exclusion held, the worker re-walks the volume (fingerprint only, within 10 s of the 40 s grace) and posts it with the accepted census digest, its register nonce and pod. The api accepts it only from the bound nonce and pod; a different fingerprint marks the inventory written after inventory, and a missing attestation blocks as `exclusion_until_termination_unproven`. The fingerprint hashes path, inode, size, mtime and ctime of every node, so a same-size, same-tip rewrite is caught.
- **D11. Whole-volume census and an index-independent worktree proof.** The census walks the whole data volume (no symlinks, no mount crossing, 500,000 nodes, 30 s) with an explicit classification table; anything unlisted is unknown and blocks. Worktrees count only through a blob-hash proof of every tracked file that refuses skip-worktree and assume-valid flags (no `git status`); ignored or otherwise unproven bytes block. Known consequence, observed by review: the authority store under `.uzi-authority/` (boundary records written at every start) and, in the split uid layout, the DinD prune's leftover config directory in the worker's TMPDIR are classified unknown, so a real worker will usually show as blocked in the M1 dry run. M2 must define explicit rules for them, not widen the census silently.
- **D12. Post-exit evidence is sampled.** Presence-preserving pod sets (an empty set and an absent set are distinct), the Deployment generation fence pinned at the scale-down patch, the PVC-user scan over the namespace pod list and a per-tick `held` chain with fail-closed gaps (`observation_gap`, `hold_chain_stale`) detect what the controller observes. These are sampled: a pod whose whole lifetime falls between two observations is not detectable in M1. M1 therefore never emits an unconditional retire; its best outcome is `conditional_retire` listing `pv_reclaim_policy` and `intervening_pod_exclusion` as unverified. M2 must establish actual exclusion, or obtain a fresh inventory under proven exclusion, before any deletion. A fresh pod list alone does not certify historical validity.
- **D13. The evaluation bound is visible.** The report evaluates at most 50 candidates, oldest hibernation first, but counts all of them; above 50 the check warns and its summary says "evaluated 50 of N (oldest first)", so it never reads ok while unevaluated workers exist.

- **Merge integration with #4206.** Cleanup settles before the controller observes the inventory and before the worker is offered it. The inventory uses its own local claim-gate owner (`hibernation_inventory`), so it cannot release the cleanup hold or mistake another gate owner for writer exclusion.

- **D14. Pressure-only builder pruning is uncovered in M1 (review, 2026-11-15).** A configured builder leg marks `dind_builder` uncovered for the worker process, so its inventory is incomplete. A registry lease around `tick` would end before an unsettled child closes, and even that local close does not prove daemon-side settlement. Rather than inventing settlement authority or changing the accepted pressure behavior, M1 keeps the volume protected until a supported exclusion mechanism exists. Disabled or unwired builder legs add no uncovered writer.

- **D15. Exclusion reports require delivery acknowledgement (review, 2026-11-15).** Reading a lost-exclusion report never consumes it. It repeats until the exact report was included in a successful heartbeat (original body or ACK-free resync, not an omitted or stripped report). A receipt for another op or another report cannot clear it. Inventory-heartbeat errors fail the request after the independent liveness write, preserving an unknown observation and the existing exclusion rather than returning a misleading absent member.

- **D16. Builder-prune settlement: named gap, a docs-only negative result (#4362, owner decision 2026-11-15).**
  - **Decision.** At base 2ced6fa50 this run has not established a supported, daemon/submission-bound epoch baseline for the current worker population, including retained daemon data and in-pod worker restarts. It therefore takes the sanctioned named-gap branch and keeps `dind_builder` uncovered. This is a negative result for the mechanisms evaluated here, not proof that a colocated ledger or a fresh PV is universally necessary. No runtime, test or config change. The existing configured #4291 pressure-only leg remains uncovered and no new coverage is granted.
  - **Owner decision.** Bounded deferral of a builder pressure prune was allowed only while the worker is idle (no nonterminal run), in `hibernation_state` preparing, and the inventory holds writer exclusion, with a finite pressure-deferral cap and escape triggers. It is not implemented, because coverage also requires the epoch baseline below.
  - **Source-inspection negative findings.** These come from the owner's read-only source study of the chart-pinned Engine 29.8.0 / BuildKit v0.33.0 / Buildx 0.37.0. No daemon experiment was run. In the inspected interfaces:
    - there is no submission-bound join or completion lookup for an earlier prune after client death or a lost reply;
    - cancellation is not settlement: the current batch finishes before the cancel check, and containerd GC adds a separately scheduled tail;
    - `/info.ID` persists across daemon restarts;
    - BuildKit reconciles pending deletes at startup;
    - `du` and `/system/df` are point-in-time samples.
  - **Source-established limitations at 2ced6fa50** (containment facts, not falsifiers):
    - The builder leg's state is in memory only (`agent/src/dind-builder-prune.ts`, the `unsettled` latch), and nothing persists builder-prune attempts.
    - The leg is on by default. The master `UZI_DIND_PRUNE_ENABLED` must be "true" and Docker wired; the controller renders `UZI_DIND_BUILDER_PRUNE_ENABLED` true by default; unset also means on. So existing workers carry unrecorded builder-prune history.
    - The worker container does not mount `dind-data` (only the daemon, plus the meter read-only). The current worker-facing mechanisms establish no positively proved storage epoch or submission-history baseline; absence of a direct mount alone does not rule out a future externally attested binding.
    - Positive facts: #3431 pod-epoch evidence (`agent/src/pod-epoch-evidence.ts`, `controller/internal/kube/pod_epoch_evidence.go`) records the predecessor pod's containers and init containers, including the DinD native sidecar, as observed Terminated (container ID, exit code, restart count); the DinD claim name, uid and volume name; and a maintenance remint (old to new claim UID). Its "grants no epoch or deletion authority" comment describes its current scope, not an impossibility.
    - No code reads the live DinD container's restart signal or `docker info` as an epoch.
  - **Unverified composition (the missing contract).** Through supported runtime validation, that Terminated evidence and the claim bindings would have to be bound to (1) the relevant old daemon/storage writer set, (2) the startup-reconciliation/GC tail, and (3) worker-only restart and submission-history continuity within a pod. It was neither fully traced nor falsified, so it is UNVERIFIED and grants nothing.
  - **Pod replacement.** A positively observed termination of the attempt's daemon containers can supply stop evidence under the existing rules. Recreate, pod disappearance, or a replacement pod UID alone supplies no such proof ([ADR-1470](../adr/1470-paced-hosted-worker-rolls.md) for Recreate and terminal phase; the #4362 owner rule for the rest).
  - **Rule kept.** No baseline may be inferred from an empty ledger, a registration or a pod UID alone.
  - **Cap and release/finalization: candidate ingredients identified, not implemented or proven.** This delivery selected and validated no finite cap and no release/finalization mechanism. Inputs to future planning: the api's 10-minute `HibernationPreparingDeadline` (`api/internal/workersvc/hibernation.go`), which the worker never receives; a possible local timer; and the existing D10/D15 checks (release clears the held state; `finalizeAtShutdown` sends nothing unless the exclusion is held, accepted and intact; retirement blocks with `exclusion_until_termination_unproven` without a final report). Open items: alignment with a preparing interval that starts before EX, shutdown grace, pressure escape, final-report races (including same-op reacquisition), and API loss. The owner's partition regression guards a release-then-resume path this delivery does not introduce, so it was not added.
  - **Considered and set aside.** A late regular prune shrinks only disposable build cache, and the D10 fingerprint walks only the data volume. The owner made the epoch rule the coverage condition, so this is not a coverage route.
  - **Still open.** Native BuildKit GC and other Docker writers stay separate obligations ([ADR-1759](../adr/1759-dind-data-metering-and-prune.md)). `dind_builder` remains a #4208 M2 blocker on Docker workers with the leg enabled.
  - **Residual, tracked separately in #4369.** The idle cache prune runs `builder prune -f` inside the covered writer class `dind_prune`, and its lease ends at client close. D14's "disabled or unwired builder legs add no uncovered writer" likewise treats those workers as covered without an epoch proof. This is an M1 coverage over-claim, not evidence of an actual PVC deletion: M1 is conditional and read-only, and the other retirement gates remain.
  - **Reopening conditions.** A supported submission-bound daemon join, or a positively proven daemon/storage-writer epoch and subsequent submission history, including restart continuity and attributable cleanup tails. Neither fresh physical bytes nor one particular ledger design is prescribed.
