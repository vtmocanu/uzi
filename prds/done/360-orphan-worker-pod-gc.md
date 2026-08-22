# PRD #360: Garbage-collect orphaned hosted-worker pods (bound worker Deployment ReplicaSet history)

**Issue**: [#360](https://github.com/vtmocanu/uzi/issues/360)
**Status**: Complete (landed on branch agent/issue-360; live-cluster confirmation on meta-dev-02 is a maintainer follow-up)
**Priority**: Medium (reliability / quota hygiene; no user-facing outage today, but an unbounded leak of stuck pods holding docker-tier ResourceQuota)

> This PRD is self-contained for an offline worker: it needs no live cluster and no open-web access. Every fact below was gathered from this repo's source (cited by symbol + path) or from a one-time live observation on meta-dev-02 that is recorded here as a resolved fact. Acceptance is by controller unit tests, not a cluster deploy.

## Problem

Hosted-worker Deployments (`uzi-hw-<run-uuid>`) accumulate stuck pods that are never garbage-collected. Observed live on meta-dev-02 (2026-08-18), namespace `uzi-workers-docker`:

- Two worker pods stuck in `Init:1/2` for **36h** and **2.5 days**. In each, the `seed-nix` init container had Completed (exit 0) but the `dind` native sidecar was wedged at `waiting: PodInitializing` (never started, RESTARTS=0).
- Each stuck pod was owned by a ReplicaSet **scaled to `replicas=0`**, yet the pod had **no `deletionTimestamp`** - the RS never issued the delete, so the pod lingered indefinitely.
- Each of those worker Deployments had roughly **11 ReplicaSets** (one per agent-image roll, ~daily), all old ones scaled to 0, matching the k8s default `revisionHistoryLimit` of 10.
- The stuck pods reserved docker-tier `ResourceQuota` (`uzi-hosted-workers-docker`, 8 CPU / 32Gi). At the time the tier read 4/8 CPU, 16/32Gi with 5 pods (3 healthy current + 2 stuck orphans); left unchecked, an accumulating leak eventually starves new workers.

The current worker pods were healthy (2/2 Running); only superseded-RS pods were wedged. So the confirmed, reproducible failure is **orphan accumulation**, not a live-worker outage.

## Background: current controller behaviour (code facts)

Citations are by symbol + path at `main` (line numbers approximate; cite the symbol if the tree has moved).

- **Worker Deployment is rendered in Go**, not from a chart template: `RenderDeployment` (`controller/internal/kube/render.go`, ~line 768). It sets exactly four `DeploymentSpec` fields: `Replicas: 1`, `Selector` (worker-id label, immutable), `Strategy: Recreate` (deliberate - a RollingUpdate surge pod would Multi-Attach-deadlock on the worker's RWO PVCs), and `Template`.
- **`RevisionHistoryLimit` is NOT set** anywhere in `controller/`, `api/`, or `deploy/` (repo-wide grep is clean). So the k8s default of **10** applies: up to 10 superseded ReplicaSets are retained per worker Deployment.
- **A new ReplicaSet is created on every pod-template patch.** `Materializer.reconcileWorker` (`controller/internal/kube/materializer.go`, ~line 447) patches `spec.template` when `obs.Generation != w.Generation` OR `obs.SpecHash != wantHash`. The spec-hash (`specHash`, `render.go`) is a sha256 over the whole rendered pod template, which **includes the agent container image** (`spec.Image`); the controller resolves that image from its own config, so each agent-image republish rolls the fleet -> new spec-hash -> patch -> new RS. That is the ~1-RS/day driver.
- **The `dind` container is a native sidecar**: an init container with `RestartPolicy: Always` (`dindContainer`, `render.go`, ~line 1119), with a StartupProbe (`docker info`, `PeriodSeconds: 2`, `FailureThreshold: 30`, ~60s). The observed wedge is `dind` **never starting** (`PodInitializing`) after `seed-nix` completes; the startupProbe only governs `dind` once it has started, so it does not bound this.
- **There is no garbage collection of worker pods or ReplicaSets.** The only delete path is `Materializer.teardown` (`materializer.go`, ~line 618), which fires only when the api drops a worker from the desired set (de-provision) and deletes the Deployment/Secret/PVCs (background cascade). The controller never deletes a pod directly, never scales or deletes a ReplicaSet, and never prunes anything.
- **Stuck pods are DETECTED but the signal is display-only.** `deriveRollHealth` (`controller/internal/kube/rollhealth.go`, ~line 175) classifies pods `rolling`/`stuck`/`settled` (including a 10-minute `stuckAge` arm), but `RollHealth` feeds only the Workers-UI status report POSTed to the api; `reconcile.Tick` reads nothing from it for any create/patch/teardown decision, and the report is error-swallowed so it cannot affect reconciliation. Note `PodInitializing` is not in `blockingReasons`, so a stuck-Init pod is flagged only via the age arm - and nothing acts on it either way.
- **Hosted workers are long-lived, not per-run.** They are provisioned once via Settings -> Workers (`ProvisionHostedWorker`, `api/internal/handler/hosted_workers.go`), and `ListHostedWorkersForController` (`api/internal/store/queries/hosted_workers.sql`) selects the whole fleet with no run-state filter, so each Deployment stays `DESIRED=1` for the worker's whole life and is reused across runs. A worker leaves the desired set only when its `workers` row is deleted (gated 409 while it holds a non-terminal run).

## Root cause

The worker Deployment is rendered without a `RevisionHistoryLimit`, so k8s keeps up to 10 superseded ReplicaSets per worker. When a pod from one of those superseded RSes wedges during init (the `dind`-never-starts case), nothing reaps it: the RS is scaled to 0 but retained, the controller has no pod/RS GC, and the roll-health detector that could notice is display-only. The pod therefore survives until the worker itself is de-provisioned, holding ResourceQuota the whole time.

## Solution

Bound the worker Deployment's ReplicaSet history so k8s prunes superseded ReplicaSets and cascades deletion of their pods (including wedged ones). Deleting a ReplicaSet with the default background propagation deletes the pods that carry it in `ownerReferences`, so the orphan pods are reaped even though the earlier scale-down left them behind.

The field must be set in **two** places, because the controller reconciles an existing worker with a template-only merge patch:
- `RenderDeployment` (the `!obs.HasDeployment` Create path) sets it for **newly created / reprovisioned** workers.
- `patchFor` (the drift path in `reconcileWorker`) currently sends `{spec: {template: ...}}` only, and `RevisionHistoryLimit` is a `spec`-level sibling of `template` (not inside it), so a merge patch never writes it to an already-provisioned Deployment. Extend that patch to also carry `spec.revisionHistoryLimit`, so an **existing** long-lived worker receives the field on its next drift-roll (the ~daily agent-image roll), after which k8s prunes its accumulated RSes and their wedged pods. Without this second edit the field reaches existing workers only on reprovision, and the specific orphans this PRD was filed against - which are on long-lived, reused workers - would persist.

This is a small, safe change: it only affects **superseded** ReplicaSets, never the current RS or the live worker pod, so it cannot disrupt a running worker or an in-flight run. A maintainer may optionally `kubectl patch` the field onto the existing worker Deployments to reap the current orphans immediately rather than waiting for each worker's next roll.

## Milestones

### M1 - Bound worker Deployment ReplicaSet history (core fix)
- [x] In `RenderDeployment` (`controller/internal/kube/render.go`), set `RevisionHistoryLimit` on the worker `DeploymentSpec` to a small value. **Recommended: `1`** (keeps the immediately-prior RS for post-mortem while pruning everything older; `0` is a defensible alternative for these Recreate/single-replica workers, which never use rollout rollback). Take the `*int32` with the controller's OWN local pointer helper - `ptr(int32(1))` (the generic `ptr[T any]` in `render.go`, already used as `ptr(int32(0o440))`). Do **not** introduce `k8s.io/utils/ptr`: it is an indirect-only dependency and is imported nowhere in `controller/`, so `ptr.To[int32](1)` would add a new direct import.
- [x] Extend the drift patch so **existing** workers get the field too. `patchFor` (`controller/internal/kube/materializer.go`) sends a merge patch of `{spec: {template: ...}}` only; add `revisionHistoryLimit` at the `spec` level of that patch (a `spec`-level sibling of `template`, not inside it). Without this, `reconcileWorker`'s patch path never writes the field to an already-provisioned Deployment, so long-lived workers (the incident case) keep the k8s default of 10 forever; the field is idempotent, re-asserted on every roll.
- [x] Add a brief comment beside both edits explaining *why* (bounds superseded-RS retention so wedged init pods from old rolls are GC'd; ties back to this PRD).
- **Acceptance**: the rendered worker Deployment carries the field AND the drift merge patch carries `spec.revisionHistoryLimit`; no other rendered field changes.

### M2 - Tests (offline, no cluster)
- [x] Extend the render test (`controller/internal/kube/render_test.go`, near `TestRenderedPodPosture`) to assert `RevisionHistoryLimit` is set to the chosen value on the rendered Deployment. One assertion suffices: the field is set unconditionally at `DeploymentSpec` level, outside the plain-vs-docker branching (which lives entirely in `podTemplate`), so it does not vary by tier.
- [x] Add a materializer test (`controller/internal/kube/materializer_test.go`) asserting the **drift patch carries `spec.revisionHistoryLimit`** (the M1 patch-path change - this is the meaningful guard, since the render test already covers the Create path). Confirm `TestNoDriftMeansNoPatch` stays green unchanged: the field is not in the pod template, so it does not move the spec-hash and cannot introduce spurious drift.
- **Acceptance**: new assertions pass; `task gate:controller` is green. Note the `lint:controller` ratchet is `whole-files: true`, so any pre-existing golangci-lint finding anywhere in a touched file (e.g. `render.go`) gates - run `task lint:controller` and be ready for whole-file findings, not only the diff.

### M3 - Documentation + validation notes
- [x] Record the worker-Deployment GC behaviour where the controller's worker lifecycle is described (a short note in `ARCHITECTURE.md`'s Agent-runtime/hosted-workers section, or a docstring on `RenderDeployment`), so the next reader knows the limit is deliberate and why.
- [x] Note in the PRD Decision Log (below) that live-cluster confirmation - watching the superseded RSes and their wedged pods disappear on meta-dev-02 after the controller rolls with the new limit - is a **maintainer follow-up**, out of scope for an offline worker.

## Success criteria

1. The rendered worker Deployment sets `RevisionHistoryLimit` to the chosen small value (Create path) AND the drift merge patch carries `spec.revisionHistoryLimit` (existing-worker path), both asserted by unit tests.
2. `task gate:controller` passes (format, lint ratchet, dead-code, tests) with the change.
3. No behavioural change to worker create/patch/teardown beyond the new field: an unchanged worker still no-op reconciles (no spurious RS churn introduced by the field).
4. The change is documented so the limit reads as deliberate.

## Out of scope (explicitly)

- **Controller-side stuck-pod GC / self-healing.** A controller sweep that deletes worker pods stuck in Init (e.g. wiring the existing `deriveRollHealth` "stuck" signal to a delete, or deleting pods not owned by the current RS) would additionally cover a *current*-RS pod wedging (a live-worker availability gap the observed incident did NOT hit - the current pods were healthy). It is deliberately deferred: it is riskier (the controller deleting worker pods based on a health heuristic could disrupt a live run if the heuristic misfires), and M1 already reaps the confirmed orphan case. If pursued later, it must never delete the current ReplicaSet's pod and must be gated on a generous deadline. Track separately.
- **Fixing *why* `dind` wedges at `PodInitializing`.** That is a kubelet/containerd/node-level init-startup stall, not something the controller renders; M1 makes the wedged pod get cleaned up rather than preventing the wedge. Diagnosing the node cause is a separate investigation.
- **`ProgressDeadlineSeconds`.** Setting it explicitly does not help here: the controller does not read the Deployment's `Progressing`/`ProgressDeadlineExceeded` condition for any action, so it would be an inert field. Excluded to keep the change minimal.

## Risks and mitigations

- **Risk**: choosing `0` loses all rollout history, making a post-mortem of a bad roll harder. **Mitigation**: recommend `1` (keeps the immediately-prior RS); the value is a one-line change if a different retention is wanted.
- **Risk**: the new field perturbs the pod-template spec-hash and causes a one-time roll of every worker on deploy. **Mitigation**: `RevisionHistoryLimit` is a `DeploymentSpec` field, not part of the pod `Template`, and the spec-hash is computed over the template only, so it does not affect the hash. M2's no-drift test guards this explicitly.
- **Risk**: retroactive pruning deletes an RS whose pod is somehow still wanted. **Mitigation**: only *superseded* (scaled-to-0) RSes are pruned; the current RS and its live pod are never touched.

## Decision log

- **2026-08-18**: Filed from a live meta-dev-02 observation (2 worker pods stuck in Init for 36h/2.5d, orphaned on scaled-to-0 ReplicaSets, holding docker-tier quota) made while switching the cluster to the public GHCR chart. Root cause traced to the worker Deployment rendering without `RevisionHistoryLimit` (k8s default 10) plus the absence of any controller pod/RS GC.
- **2026-08-18**: Scoped to the single high-confidence, low-risk fix (bound the RS history) so it is safe for an auto-approved Night-Shift sweep to implement. Controller-side stuck-pod self-healing and the `dind`-wedge root cause are explicitly deferred (see Out of scope). Live-cluster confirmation is a maintainer follow-up because the implementing worker runs offline.
- **2026-08-18** (review correction): An initial draft claimed setting the field would retroactively clean up existing orphans on the controller's next render. That was false as first scoped: `reconcileWorker` patches an existing Deployment with a template-only merge patch (`patchFor` sends `{spec: {template: ...}}`), so a `spec`-level `RevisionHistoryLimit` never reaches an already-provisioned worker - it would apply only to newly created ones, leaving the incident's long-lived workers untouched. M1 was extended to also add the field to that merge patch, so existing workers receive it on their next drift-roll (with an optional maintainer `kubectl patch` to reap the current orphans immediately). The pointer idiom was corrected from `ptr.To[int32]` (a new indirect-dep import) to the controller's own `ptr(int32(...))` helper. Verified against the code by an independent PRD review.
- **2026-08-20** (implemented): M1-M3 landed on branch `agent/issue-360`. `RenderDeployment` sets `RevisionHistoryLimit: ptr(int32(1))` and `patchFor` carries `spec.revisionHistoryLimit` (both in `controller/internal/kube/`), guarded by a render-test assertion and a new `TestDriftPatchCarriesRevisionHistoryLimit`; `TestNoDriftMeansNoPatch` stays green (the field is `DeploymentSpec`-level, not in the pod template, so the spec-hash is unchanged). `task gate:controller` is green. Documented in `ARCHITECTURE.md`'s worker-roll section. **Live-cluster confirmation** - watching the superseded RSes and their wedged pods disappear on meta-dev-02 after the controller rolls with the new limit - remains a **maintainer follow-up**, out of scope for this offline worker (a maintainer may `kubectl patch revisionHistoryLimit=1` onto the existing `uzi-hw-*` Deployments to reap the current orphans immediately).
