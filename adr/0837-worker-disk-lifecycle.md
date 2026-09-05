# ADR-837: Disk usage is observed on the heartbeat; recycle is delete-and-remint, kept safe by an await-gone gate

**Status**: Accepted (PRD #837, M1-M5 landed)
**Date**: 2026-09-05
**Deciders**: Vlad + agent team (risk/correctness and testability review waves screened the PRD before implementation; the user overrode their default-OFF recommendation on D9, see below)
**PRD**: [prds/done/837-worker-disk-observability-and-pressure.md](../prds/done/837-worker-disk-observability-and-pressure.md) (GitHub issue [vtmocanu/uzi#837](https://github.com/vtmocanu/uzi/issues/837)) — the PRD carries the milestones, the verified anchors, and the full Decision Log (D1-D9); this ADR carries the durable design shape and the rationale a future reader most needs and will not re-derive from the diff.

## Decision (summary)

A worker now reports its `/nix` and `/data` filesystem usage on the existing heartbeat (mirroring the PRD #49 CPU/memory plumbing), the web renders it as a Disk bar beside CPU/Memory, and the controller gained **two new drift arms** that both resolve to the same mechanism: delete the Deployment and the target PVC(s), then let the existing create-gate re-mint them at the current size. One arm fires when a worker's observed `/nix` PVC is smaller than the current `preset.nixSize` constant (the direct root-cause fix for the incident this PRD documents); the other fires when the api derives sustained disk pressure (>=90% used, debounced, fresh) and flags it on the controller poll wire. Both arms drain a busy worker first, exclude ephemeral (run-bound, PRD #529) workers, and share one `recycleWorkerVolumes` helper and an explicit multi-phase state machine that only recreates the Deployment once the target PVC is observed **live** at the desired size — never over a PVC still `Terminating`.

## Context

The controller's own code already anticipated this remedy: `preset.go`'s header comment on `nixSize` states the nix store has no auto-GC and that "v1's remedy is delete + reprovision." The incident that prompted this PRD was exactly that gap left open — a worker's `/nix` PVC was provisioned at an old 4Gi default before `nixSize` was bumped to 20Gi (PRD #87), and nothing ever reconciled it, so it ran to 100% and failed tool provisioning with "No space left on device." Three things were missing: disk was invisible (no metric, unlike CPU/memory), nothing self-healed a full volume, and nothing reconciled a stale/undersized PVC against the current preset. This PRD closes all three.

## The decisions

### D1 — Recycle means delete-and-remint the PVC, never a pod roll

Verified in `controller/internal/kube/materializer.go`'s `patchFor`: a worker image roll is a JSON merge-patch over the Deployment's **pod template only**, so the same PVCs re-attach after the `Recreate` strategy cycles the pod — the code states this directly ("PVC specs are near-immutable, so a size change is delete + reprovision, not a live edit"). A pod roll therefore reclaims zero disk. Both the size-drift arm (M3) and the pressure arm (M4) delete the PVC and rely on the create-gate to re-mint it; neither is realized as a pod roll, and no amount of pod-template patching could substitute for one.

### D2 — Teardown-in-place, not drop-the-row

There is no autoscaler, no `WorkerSet`, no desired-replica owner in this system: the controller's desired set is literally the `workers` rows with `kind='hosted'`, and a **persistent** worker's row is created and deleted by a user, not by the controller. Dropping the row to force reprovision would permanently destroy a persistent worker the user never asked to delete. The safe realization keeps the worker **row + uuid + join-token Secret** and tears down only the kube volume objects (Deployment + target PVC(s)) — the same property that already makes an ordinary image roll safe, reused rather than reinvented. `Materializer.teardown` (used when a worker id is observed but absent from the desired set) is the sibling code path; recycle reuses its object-deletion primitives but must never touch the Secret or the row.

### D3 — The api derives `disk_pressure`; the controller only actuates

The controller has minimal RBAC — no pod exec, no metrics-server access — and its only view of a worker is the poll response. So the disk signal has to ride the poll wire as a boolean, exactly like `busy` and `draining_since` already do. Putting the threshold (`UZI_DISK_PRESSURE_THRESHOLD`, default `0.90`) in the api, next to the `stats_disk_*` columns it derives from, keeps the comparison in one place and keeps the wire a single field (`DesiredWorker.DiskPressure`, deliberately **not** `omitempty` — it must serialize even `false`, so the golden `controller_poll_wire.json` proves the round-trip rather than the zero-value default). The controller's job stays pure lifecycle: it never computes a threshold, it only reacts to the flag.

### D4 — The multi-phase recycle state machine, the Terminating-PVC observe fix, and the await-gone gate

`observeNamespace` originally set `HasNixPVC`/`HasDataPVC` on a name match without ever inspecting `deletionTimestamp` — but a PVC mid-deletion keeps `deletionTimestamp != nil` and is still returned by `List` for the entire pod-termination window. A naive recycle would read `HasNixPVC=true` at the OLD (undersized) size while `HasDeployment` flips false instantly, and the very next tick would recreate the Deployment over a doomed, still-terminating PVC.

The fix has two parts, both load-bearing:

- **`HasNixPVC` stays exists-in-ANY-state** (unchanged from before this PRD) — it still gates the create-side `AlreadyExists`/quota safety the code already relied on (upstream [kubernetes#119593](https://github.com/kubernetes/kubernetes/issues/119593): Kubernetes does **not** decrement `used.requests.storage` on a create rejected as `AlreadyExists`, so a create-gate that only fires on `HasNixPVC=false` avoids double-charging quota against a PVC that already exists in any state).
- **`ObservedWorker.NixPVCSize` (a `*resource.Quantity`) is sourced only from a LIVE (non-Terminating) nix PVC**, nil while the PVC is absent or Terminating. The size-drift comparison (`NixPVCSize.Cmp(spec.NixSize) < 0`) and the Deployment-recreate guard both key off this liveness-carrying field, not the exists-in-any-state boolean.

`recycleWorkerVolumes` is therefore realized as an explicit, idempotent multi-phase machine rather than two bare deletes in one arm: drain if busy, delete the Deployment (releases the RWO mount so the `pvc-protection` finalizer allows the PVC delete), delete the target PVC(s), **await the PVC actually gone** (absent from `List`, not merely `deletionTimestamp != nil`), let the create-gate re-mint it at the desired size, then recreate the Deployment. That last step is gated by `nixLiveAtSize` (`obs.NixPVCSize != nil && obs.NixPVCSize.Cmp(spec.NixSize) >= 0`) rather than merely `!HasDeployment` — the **await-gone gate** — so a tick landing mid-recycle can never recreate the Deployment over a PVC that is present-but-wrong-size or still Terminating. Each phase is a no-op if already satisfied, so a tick that lands mid-recycle resumes cleanly rather than restarting.

### D5 — Thrash cooldown is read statelessly, and reads as a capacity signal

The reconcile loop is stateless, so rather than persist "last recycled", the pressure arm's thrash guard reads the target PVC's own `creationTimestamp` (the controller already lists PVCs) and skips a recycle within `UZI_WORKER_DISK_RECYCLE_COOLDOWN` (default `1h`) of it, emitting the fixed, test-asserted log token `disk-recycle-skipped-cooldown worker=<id>`. A worker back at pressure within the cooldown is not a bug to suppress quietly — it is a **capacity** signal: the worker's actual working set exceeds `nixSize`, and the fix is an operator raising the preset size, not another recycle.

**Intended interaction worth naming explicitly**: after an M3 size-drift recycle re-mints `/nix` at the current `nixSize`, the fresh PVC's `creationTimestamp` puts the worker inside the M4 cooldown window, so a disk-pressure recycle on `/data` that would otherwise fire right afterward instead defers for the cooldown. This is intended, not a bug interaction to fix — a worker that was just intervened on is exactly the case the capacity signal is for, and deferring avoids stacking two teardowns back to back on the same worker.

### D6 — No auto-rollback on a stranded worker; this is detection, not prevention

The multi-phase machine deletes the old PVC **before** the new one is re-minted. If the new PVC cannot bind (storage class unavailable, quota exhausted, no schedulable node), the old data is already gone and there is nothing to roll back to — the worker is left with no volume, a permanent outage plus loss of whatever the recycled volume held. This is inherent to reclaim-by-delete and cannot be made atomic without keeping two PVCs' worth of quota reserved per worker, which the ceiling/quota model (`ValidatePVCCeilings`) does not budget for. The mitigation is **detection, not prevention**: a re-mint stuck `Pending` past a bound timeout must surface loudly (a stable log token, the display-only report path) so an operator intervenes. Do **not** attempt an automatic rollback — there is no prior state to restore, and papering over that with a "retry from scratch" would hide the real failure (an exhausted storage class or quota) behind a busy-loop instead of surfacing it.

### D7 — Both recycle arms exclude ephemeral workers and share one helper

`DesiredWorker.Ephemeral` (new wire field, both `controller/internal/protocol/protocol.go` and `api/internal/hostedsvc/protocol.go`) marks a run-bound throwaway worker (PRD #529): auto-provisioned for one run, torn down when it ends. Recycling one is pointless (it is about to be deleted anyway) and force-recycling it past the drain deadline would wipe its run's workspace mid-run. Both the size-drift arm and the pressure arm check `!Ephemeral` before recycling. They also call the same `recycleWorkerVolumes(worker, obs, ns, volumes)` helper and reuse the PRD #422 busy-guard/cordon/`DrainDeadline`/`ForceRoll` machinery verbatim for the drain-first step — deliberately one mechanism with a `{nix}` vs `{nix, data}` volume-set parameter, not two parallel implementations that could drift.

### D8 — The 90% actuation threshold and the 85% UI danger tone are intentionally different

`UZI_DISK_PRESSURE_THRESHOLD` (api, default `0.90`) drives the destructive recycle action; the web `MeterTrack`'s `danger` tone threshold (`>=85%`) drives only a color change in a gauge. These are read from two different places for a reason: the UI should warn an operator *before* the system is willing to act on its own, not at the same instant. Do not "align" the two numbers — a reviewer instinct to unify them would remove the deliberate lead time between "this looks concerning" and "the controller will tear it down."

### D9 — Recycle defaults ON, over recorded reviewer dissent

`UZI_WORKER_DISK_RECYCLE_ENABLED` defaults `true`, gating both drift arms with a single master toggle. **The risk/correctness and testability review waves unanimously recommended default-OFF, plus splitting observe (ship the `DiskPressure` wire flag) from actuate (the recycle arm) as separate rollout phases**, so pressure could be observed in production before any automated teardown was trusted against real hosted-worker data. The user chose default-ON with one toggle and no observe/actuate split, judging that self-healing the exact incident this PRD documents is the point, and that the guards already specified are sufficient. The dissent is recorded here deliberately rather than dropped. The mitigation is that every safety guard matters *more* precisely because the default is ON: freshness (only a fresh heartbeat can hold pressure true), a >=2-consecutive-heartbeat debounce (`stats_disk_pressure_streak`, so a single spike never fires it), reset-on-action (`RegisterWorker` zeroes the streak unconditionally on every fresh pod incarnation — a rolled/restarted worker cannot inherit a stale streak), the PVC-age cooldown (D5), drain-before-recycle (D7), and the ephemeral exclusion (D7) are all retained precisely because there is no staged rollout to fall back on.

## Known gap

The docker-tier `dind-data` PVC (the build/image cache on a docker-capable hosted worker) is neither metered nor recycled in v1 — M1 samples only `/nix` and `/data`. It is the volume most likely to fill on a docker-tier worker, and `disk_pressure` can never fire on it. Documented as a deliberate v1 boundary and fast-follow (meter it in a later pass, then the existing M4 arm can act on it without further design work), not a regression.

## Consequences

- Disk joins CPU/memory as a first-class, display-only worker metric; the `stats_disk_*` columns must stay out of every scheduling/claim/assignment/sweeper query, exactly like the existing `stats_*` columns.
- A hosted worker's `/nix`/`/data` volumes are no longer a one-way ratchet toward full — both an undersized-PVC drift and a genuinely full volume now self-heal without an operator manually deleting and reprovisioning the worker.
- The await-gone gate and the Terminating-PVC observe fix are the two properties that make any *future* delete-and-remint drift arm on this codebase safe; a new arm that skips either reintroduces the quota-churn and doomed-Deployment failure modes this ADR exists to avoid.
- The default-ON decision (D9) means an operator who wants to observe before trusting automated teardown must explicitly set `UZI_WORKER_DISK_RECYCLE_ENABLED=false` rather than relying on a shipped-off default.
