# ADR-3890: Persistent worker data and DinD claims converge upward, size-only and non-destructively

**Status**: Accepted (issue #3890, implemented in issue #3890; maintainer hosted acceptance pending)
**Date**: 2026-11-04
**Deciders**: maintainer (owner decision 2026-11-04) and the agent team (issue #3890 milestones and review waves)
**Related**: #2013 (`l` data 50Gi), #224 (DinD data PVC), #3383 (`l-highmem`), #3852 (DinD maintenance fences), #3153 (runner-shared subPath), #3103; [ADR-837](0837-worker-disk-lifecycle.md) (delete-and-remint recycle, where PVC sizes never changed in place), [ADR-1759](1759-dind-data-metering-and-prune.md) (the DinD data volume), [ADR-3153](3153-runner-shared-pvc-subpath.md) (the `data` PVC also carries `/data/runner`), [ADR-1484](1484-in-app-health-boundary.md) (no cluster-scope API reach), [ADR-2240](2240-hosted-worker-default-size.md) (preset sizes)

## Decision (summary)

Owner decision 2026-11-04: Docker-tier DinD data-root claims are sized per preset (`s`/`m` 20Gi, `l`/`l-highmem` 50Gi), and the controller converges an **existing** `/data` or DinD claim's storage **request** upward to the render size where the claim's StorageClass permits expansion. It never shrinks, deletes, recreates or recycles a claim to do so. The mechanism is one `uid` + `resourceVersion` fenced JSON merge patch of `spec.resources.requests.storage` per claim, gated by the same roll, maintenance and reservation fences that protect the claim today. A pod template carries no PVC size, so a size-only change rolls nothing. Reporting is logs-only; no API or wire contract changes.

## Context

Until #3890 a claim was created at the render size and never patched. ADR-837 D1 records why: PVC specs were treated as near-immutable, so a size change meant delete and reprovision. That left two gaps. `l` data grew to 50Gi (#2013) and every existing `l` claim stayed at its old request forever, and the DinD data root was a flat 20Gi that a busy `l` worker outgrew while its `/data` was 50Gi. Kubernetes can grow a claim in place when its StorageClass sets `allowVolumeExpansion` (see [Expanding PersistentVolumeClaims](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims)); an operator could do it by hand, but nothing converged a fleet.

## The decisions

### D1 — Per-preset DinD size, with an override that always wins

`preset.Size.DinDDataSize` is 20Gi for `s` and `m` and 50Gi for `l` and `l-highmem`. `dindDataSize(spec)` in `controller/internal/kube/render_dind.go` returns the cluster override (`UZI_WORKER_DIND_DATA_SIZE`, chart `workers.docker.dindDataSize`) when it is set, otherwise the preset value. The chart default for `dindDataSize` is now empty. A non-empty value is a cluster-wide override at every preset until it is removed: a retained 20Gi, an explicit 30Gi, or a value kept by `helm upgrade --reuse-values` keeps applying, and a 30Gi override does not become 50Gi. An operator adopts the per-preset defaults by clearing the value.

### D2 — Convergence is monotonic and request-only

`expandClaims` (`controller/internal/kube/pvc_expand.go`) runs from the ordinary reconcile path after the PVC create loop, per worker per tick. For each of `<worker>-data` and the DinD data claim it patches only when the rendered size is strictly larger than the claim's current request. Never a shrink, and `/nix` is not touched (its size drift stays the ADR-837 delete and reprovision). The patch body carries `metadata.uid`, `metadata.resourceVersion` and `spec.resources.requests.storage` and nothing else, so a claim replaced or modified between observe and patch is refused by the API server (`pvc-expand-failed reason=conflict`) instead of being resized. A claim that is not `Bound`, is terminating, lacks a uid or resourceVersion, or does not carry this worker's ownership stamp and expected name and namespace is skipped (`pvc-expand-skipped`). The DinD-maintenance reconcile path does not run expansion.

### D3 — One snapshot per decision

For `/data` of a worker that has a roll entry, the roll snapshot's claim (`e.claim`, kept current by `mirrorRoll`) is the sole source of the claim's identity, request and annotation. Observe's claim only has to agree with it on UID; a UID change defers the claim as `claim-identity-changed`. The patch result replaces `e.claim` so later roll writes in the same tick fence on the new `resourceVersion`. DinD, and `/data` of a worker with no roll entry, use Observe's claim. A completed canary or cohort marker in the reservation annotation does not block expansion, so history from a finished roll cannot hold a claim back forever; an open or unreadable marker does.

### D4 — One gate shared by the mutator and the reporter

`claimGate` returns the first blocking reason, in this order: `maintenance`, `roll-uncertain`, `roll-admission`, and for `/data` only `roll-reservation`. Both `expandClaims` and the read-only `reportClaimStates` call it, so the log cannot name a deferral the mutator does not honour or the reverse. Reasons map to the pacer as follows (`controller/internal/kube/roll_pacing.go`):

- **Admission.** A persistent, non-drifted worker with no roll in flight is admitted by the `!active` branch of `planPool` regardless of whether it is busy, so the ordinary case expands on the next tick. Anything that leaves a pool entry unadmitted defers expansion as `roll-admission`: an active roll in the worker's pool; a blocked entry (snapshot failure, unknown target version, a missing Deployment without first-provision evidence, a reservation or owner mismatch, missing API coverage evidence except the API-skew admission); migration-held drift; a pool blocker such as a stuck canary or an old incarnation during an active roll.
- **`roll-uncertain`.** Coverage the pacer cannot prove (uncertain, or migration-fenced coverage).
- **`maintenance`.** An active DinD maintenance episode for the worker.
- **`roll-reservation`.** An open or unreadable roll reservation on the `/data` claim. DinD is not held by this fence, because the reservation marker lives on `/data` only.

Every deferral logs `pvc-expand-deferred reason=<reason>` once per change and expansion proceeds on a later tick when the gate opens.

### D4a — Size-only rollout neutrality

Sizes never enter the pod template or the spec hash. Expansion never touches the Deployment, a cordon or drain, a roll reservation, the `uzi.dev/image-roll` annotation or any pod. A larger claim is therefore invisible to the roll machinery by construction, and a preset or override change rolls no worker.

### D5 — Reading the StorageClass needs one narrow cluster-scoped exception

Expansion must know whether the claim's class sets `allowVolumeExpansion` before patching. A StorageClass is cluster-scoped, and a namespaced Role cannot authorize reading one. The chart therefore adds one ClusterRole, `<fullname>-controller-storageclass-read`, with `get` on `storage.k8s.io` `storageclasses` and no other verb or resource, bound to the controller's ServiceAccount (`deploy/chart/templates/worker-rbac.yaml`, gated by `workers.storageClassRead.create`, default `true`). The controller looks up the class by the name on the claim, so it never lists classes.

This is an owner-authorized exception to the chart's "never a ClusterRole" posture. [ADR-1484](1484-in-app-health-boundary.md) records that posture (no `ClusterRole` in the chart), and `prds/1860-pod-epoch-writer-exclusion.md` recorded the owner choosing an operator chart value over a ClusterRole on 2026-10-24. Both were correct for their scope; this decision narrows the rule to "one get-only `storageclasses` ClusterRole".

Without it (`workers.storageClassRead.create=false`, custom RBAC, or a chart older than this change) expansion fails closed: the lookup is refused, the controller logs `pvc-expand-skipped reason=class-lookup-forbidden` and leaves every claim untouched. Creation, recycling and rolls are unaffected. A lookup that fails for any other reason logs `class-lookup-failed`, and a class without `allowVolumeExpansion: true` logs `class-not-expandable`. A claim with no `storageClassName` is skipped as `no-storage-class`: the controller never guesses the cluster default class. The verdict is memoised per tick.

### D6 — Reporting is honest about what an accepted patch means

An accepted patch grows the request, not the filesystem. `pvc-expand-requested reason=desired-larger from=<old> to=<new>` records only that the API accepted it. Resize progress is read from the claim's status: `pvc-resize-pending` when capacity is below the request, when the `FileSystemResizePending` or `Resizing` condition is true, or when an `allocatedResourceStatuses` entry is `ControllerResizeInProgress`, `NodeResizePending` or `NodeResizeInProgress`; `pvc-resize-failed` for a `ControllerResizeError` or `NodeResizeError` condition or an allocated status ending in `Failed` or `Infeasible`. The controller never forces a restart. A driver that supports online expansion finishes without one. Otherwise it stays pending until the next independently authorized idle roll, which remounts the volume (see [Hosted workers](../docs/hosted-workers.md): a cordoned idle worker rolls under the ordinary pacing policy; there is no owner restart action, and no bypass of the drain is introduced). Reporting is logs-only. There is no API, wire or UI change.

Log tokens, all one line per state change per claim (dedup state is process-local, so a controller restart re-logs once):

| Token | Reasons |
|---|---|
| `pvc-expand-requested` | `desired-larger` (with `from=` and `to=`) |
| `pvc-expand-deferred` | `maintenance`, `roll-uncertain`, `roll-admission`, `roll-reservation`, `claim-identity-changed` |
| `pvc-expand-skipped` | `not-bound`, `terminating`, `no-identity`, `foreign`, `no-storage-class`, `class-lookup-forbidden`, `class-lookup-failed`, `class-not-expandable` |
| `pvc-expand-failed` | `conflict`, `invalid`, `forbidden`, `error` (with a truncated `error=`) |
| `pvc-resize-pending` | `capacity-below-request`, `condition-FileSystemResizePending`, `condition-Resizing`, `status-<allocated status>` |
| `pvc-resize-failed` | `condition-ControllerResizeError`, `condition-NodeResizeError`, `status-<allocated status>` |

## Consequences

- **Quota cost.** The default Docker tier quota is `10 x (50 + 20 + 50) = 1200Gi` (`workers.docker.quota.requestsStorage`, raised from 900Gi). An operator quota override kept at 900Gi can refuse the growth once the tier's claims reach the quota (a full 10-worker `l` fleet needs 1200Gi); the refusal is logged as `pvc-expand-failed reason=forbidden` and the claim stays at its old request.
- **LimitRange limitation.** The per-claim ceiling (`limitRange.maxPVCStorage`) is enforced by the controller's boot check (`ValidatePVCCeilings`) and the chart guards in `worker-invariants.yaml`. A request above the namespace LimitRange `maxPVCStorage` is expected to be refused at admission on update (logged `pvc-expand-failed reason=forbidden`); this is not verified on a live cluster. The controller's boot check and the chart guards keep targets within the ceiling.
- **Driver limits.** Expansion needs a StorageClass with `allowVolumeExpansion: true` and a CSI driver that supports it. Some drivers rate-limit resizes or require the volume to be detached; the controller reports what the claim's status says and does nothing else.
- **No shrink, ever.** A lowered preset or override leaves existing claims at their larger request. Reclaiming space remains the ADR-837 and ADR-1759 mechanisms.
- **Manual procedure remains.** Operators may still expand a claim by hand ([deploy/README.md](../deploy/README.md#expanding-existing-worker-data-volumes)); the controller's later tick sees the larger request and does nothing.

## Not shipped, and acceptance

Maintainer hosted acceptance is pending and not verified: request, capacity and filesystem readings on a real claim, claim UIDs unchanged across the expansion, a sentinel file surviving it, an online-capable driver finishing without a restart and a non-online driver finishing at an idle remount, and the denied cases (`class-not-expandable`, `class-lookup-forbidden`, quota `forbidden`, a request above the LimitRange `maxPVCStorage`). The LimitRange behaviour above is likewise unverified. Everything above is proven only by the controller unit tests and the chart render check (`task render:pvc-expansion-check`).
