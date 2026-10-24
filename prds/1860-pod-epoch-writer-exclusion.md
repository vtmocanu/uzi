# PRD #1860: Pod-epoch writer exclusion for settled attempt trees (M0 investigation)

**Status**: M0 in progress, 2026-10-24 (preflight recorded). Item 1 of #1860, re-scoped (see Decision Log). Investigation-only: the single milestone M0 produces evidence and a go/no-go recommendation. Implementation is a later PRD, written only after the owner reviews M0.

## Problem

On a Docker-wired hosted worker, a settled run's attempt tree under `/data/runner` can never be deleted automatically, because nothing proves that no writer can still reach it.

- Process-only quiescence is not proof (#1858, ADR-1783).
- An already-accepted container create can complete after its originating process exits and after clean container listings.
- `teardownDocker` (`agent/src/run-quiescence.ts`) only ever returns `not_wired`, `docker_unconfirmed` or `docker_error`.

So the retention sweep stays disabled (#1858, PR #3115). Settled trees fill the data volume until runs fail on disk (#3329). The documented options today are operator discard where a source is eligible, and otherwise preservation plus storage expansion.

## Candidate design under investigation: pod-epoch reclaim

Reclaim runs once per worker pod, at worker startup, before claim admission first opens. It also requires **positive evidence that the predecessor pod fully terminated**. A tree qualifies only if it is settled under #3329's authorization (the operator-channel disposition plus a positive terminal lifecycle).

Why this could be a real proof, to be established by M0:

- **Predecessor writers.** If the controller positively observed the predecessor pod terminate, every predecessor process is gone: the old dockerd, its shims and containers, and every agent process. In-flight or accepted-but-invisible creates of the old daemon cannot complete.
- **Successor writers.** In the new pod, no workload or maintenance request that could write or acquire a reference to the tree may run before reclaim completes. That includes worker-owned clients such as DinD prune. M0 must justify which observation requests are safe, including its own inventory queries and the existing readiness probes.
- **Persisted daemon state.** This is what remains: the DinD data root on its own PVC, holding stopped containers and volumes. Eligibility requires all of the following:
  - no running, restarting or paused container, checked after a proven restore barrier;
  - no volume with a non-local driver or non-empty `Options`;
  - stopped containers only where M0 shows they hold no writable reference to the tree. After deletion, a later `docker start` would re-resolve a bind by path.
- **Deletion in place.** Deletion uses the existing retire and delete helpers, with no rename. Readers of the recorded path, the ledger and `backup-runs.sh` are unchanged.

### Predecessor-termination guard (required, no fallback)

- **The evidence.** The controller supplies positive evidence that the predecessor pod terminated. The evidence is bound to:
  - the predecessor pod UID;
  - the successor pod UID (the worker's existing `UZI_POD_UID`);
  - the relevant PVC identities (data and DinD data).
- **Without it, defer.** Missing, ambiguous or stale evidence means **defer**. There is no fallback to "normally safe".
- **Why evidence is required.**
  - Recreate's terminate-before-create ordering holds only for Deployment rollouts. A pod deletion, even a graceful one, lets the ReplicaSet create the replacement while the old pod is still terminating.
  - ReadWriteOnce does not stop two pods on one node from using the volume.
- **Ineligible cases.** Force-deleted pods and non-graceful node loss produce no positive evidence, so they are simply ineligible. Out-of-band actions that violate the guard's assumptions are documented operator residuals.

### One-shot eligibility

Eligibility is keyed by `UZI_POD_UID` and consumed durably before any reclaim attempt starts. A worker container restart, an interrupted reclaim or any uncertainty never reopens it. The `dind-sock` emptyDir cannot hold the marker, because the rootful posture omits it.

### Threat model

- **Covered**: ordinary workloads: `docker run`, compose, testcontainers, exec, `docker cp` and builds.
- **Accepted residuals** (as in ADR-1783 R3 and ADR-3153): deliberate escapes, namely a container binding containerd's socket, a privileged container with shared mount propagation, `--pid=host`, and rootful device-level reach.
- **Not a residual**: an ordinary predecessor workload surviving pod replacement. The guard must exclude it.

## Outcome (of this PRD)

An evidence record appended to this PRD as `## M0 evidence`, plus a recommendation:

- **Go**: build the pod-epoch reclaim, with a concrete implementation PRD outline.
- **No-go**: stop and name the gap. The item-2 mediation design is the fallback.

## Out of scope

- Any production code change or production caller.
- Changes to operator discard. Automatic reclaim stays disabled until a later implementation PRD and #3329 land.
- The ADR-1786 amendment, which belongs to #3329.
- Container ownership and lifecycle, and in-place session resume (#1860 item 2). These go to a separate follow-up issue.
- The compose and unwired lanes. Compose runs DinD as a separate service, and unwired status is not durable proof (ADR-1783 follow-up 3). Both defer, which leaves those workers unreclaimed. The plain hosted lane (no daemon in the pod spec) is a separate, easier follow-up.
- Any `.github/workflows/**` change.

## M0: Establish the pod-epoch guarantees

Blocked by: none.

Deliverables: the evidence record, plus any real-daemon experiment scripts under `e2e/pod-epoch/`. The scripts run outside the gates and are reported as run or unrun.

### Preflight

Establish all of the following, or M0 stops incomplete:

- **Images to source.** The pinned images (the `docker:29-dind-rootless` and `docker:29-dind` digests in `deploy/chart/values.yaml`; `docker-compose.yml` pins only the rootless digest) are Engine **29.8.0**; record this from the image config. Map them to source:
  - `github.com/moby/moby/v2` (the Go proxy resolves the `docker-v29.8.0` tag as `v2.0.0-beta.23`);
  - BuildKit `v0.33.0`;
  - containerd `v2.3.4`;
  - Go `1.26.3`.
- **Runtime versions.** Record runc and rootlesskit from `--version` inside the image.
- **Source access.** Read source through the Go module proxy; the worker has no open web. A cold proxy query can time out, so retry it.
- **Controlled daemon.** Establish an instrumentation method for a controlled daemon, and the privileges to launch one in the run's environment.

### Questions

Answer each with cited source (module, version, file, function) and, where testable, a real-daemon result.

1. **Termination evidence.** What controller-observable state proves that all of the predecessor pod's containers have exited, including the DinD native sidecar and its shims?
   - The candidate evidence is a kubelet-reported status, seen by the controller before the delete event, showing every entry in both `containerStatuses` and `initContainerStatuses` (where the native sidecar reports) as `terminated`, plus a terminal phase where the Kubernetes version sets one.
   - None of these is evidence: a pod object disappearing (a force-delete and a kubelet-confirmed deletion produce the same watch DELETE), a `DeletedFinalStateUnknown` tombstone, or a controller restart or relist across the window. A cluster version outside the one M0 read is not evidence either.
   - Name the Kubernetes source version read (v1.37.x, matching the controller's `k8s.io/api v0.37.1`; `k8s.io/kubernetes` is on the Go proxy). State whether kubelet removes the pod object only after the pod cgroup and volumes are reclaimed.
   - This question is source-only: a worker cannot run a real cluster offline, so the absence of a real-cluster result does not make M0 incomplete.
   - Cover graceful deletion, rollout, eviction and preemption.
   - Could any other pod have acquired the PVCs in between?
   - Name the exact signal, and how the controller binds it to the predecessor UID, the successor UID and the PVC identities.
2. **Restore barrier.** When dockerd starts with a persisted data root, when is the container inventory complete and stable?
   - Cover restore, the restart manager and restart policies, containerd tasks, volume preparation and BuildKit state.
   - Is there an API-observable point after which no restored object can acquire or hold a writable reference to the tree without a new client request?
   - The barrier must hold through deletion on one daemon incarnation. A DinD sidecar restart re-runs restore, and restart-policy `always` containers start again. Show how the worker detects a daemon restart between the barrier and the end of deletion. For example, an `/events` stream held open across the sequence, where a disconnect or any container event aborts and defers. Otherwise prove the restart cannot happen unseen. (`GET /info` `ID` survives restarts and cannot detect one.)
   - Evaluate adding "no container with a restart policy other than `no`" to eligibility. That removes this class without relying on restore timing; report its cost in eligibility.
3. **Stopped state.** Can a stopped container, an image, a volume or a build-cache entry write to a deleted tree? Can it keep a reference to the tree's inode across a daemon restart? Show that a stopped container holds no mounts, and that a later `docker start` or `docker cp` re-resolves binds by path at request time, which needs a client (Q4).
4. **No-client interval.** Derive the worker-owned Docker clients from a full code sweep, not a hand-picked list:
   - Start from `git grep -l -E 'dockerRequest\(|execDocker\(|teardownDocker\(|dockerRunningContainers\(|DOCKER_HOST|dockerHost' -- agent/src`, which covers 12 files on 2026-10-24.
   - Include DinD maintenance (`volume prune`), DinD prune, the #3031 terminal-maintenance proof, startup orphan recovery inside `worker.run()` (which can reach `teardownDocker`), and the kubelet startup probe (`docker info`).
   - Map each call site to the startup order.
   - "No client" means no state-changing request: create, start, restart, exec, archive, unpause, volume create or build. List and justify the read-only probes, including M0's own inventory queries.
5. **Postures.** Answer every question above for both the rootless and the rootful hosted wiring. A posture not covered is unsupported and defers.
6. **Eligibility estimate.** Record read-only inventory counts per container state (counts only; no pod, namespace or node names), sampled by the lead on hosted workers before dispatch. Estimate how often a realistic idle worker would pass the eligibility check.
   - **Lead's sample (2026-10-24, read-only, 11 hosted Docker-lane workers, Engine 29.8.0 images):**
     - running containers: 0 on 10 of 11 (one worker was busy, with 2 running);
     - exited containers: present on 7 of 11 (1, 2, 2, 5, 5, 6 and 27 per worker), none on 4;
     - containers with a restart policy other than `no`: 0 on all 11;
     - volumes: 0 to 122 per worker, of which 0 with a non-local driver or non-empty `Options`;
     - attempt dirs under `/data/runner`: 0 to 22 per worker.

     At that instant, the candidate predicate (no running, restarting or paused containers, and only local volumes without options) passes on 10 of 11 workers. Adding "restart policy `no` for every container" also passes 10 of 11. A "zero containers in any state" rule would pass only 4 of 11.
7. **Cadence.** Compare how often an evidenced epoch occurs (rollouts, DinD maintenance recycles) with the fill rate recorded in #3329.
   - Note what is lost: a plain pod delete or an eviction can leave the old pod still terminating when the successor starts, so the one-shot is consumed and that epoch defers.
   - Answer whether an idle worker under pressure can request its own pod recycle through the existing server-fenced maintenance, to create an epoch on demand.
   - State what #3329's trigger text must become (its current text reclaims on an idle worker from the periodic pass or an admission refusal).

### Experiment hygiene

Destructive or race experiments run only on disposable trees and a controlled daemon. If an acquisition point cannot be instrumented, the evidence is incomplete. Sleeps and fake-daemon results are not a substitute.

### Acceptance

- Every question is answered with cited source and, where testable, a real-daemon result on Engine 29.8.0.
- The record ends with a go/no-go recommendation.
- On go, the record includes an implementation PRD outline covering:
  - the controller evidence field;
  - the worker one-shot;
  - the eligibility predicate;
  - the ADR-1783 and ADR-3153 amendments;
  - the amendment to #3329's trigger;
  - the test seams.
- No production code changes. No workflow changes.

## Decision Log

| Date | Decision | Reason | Rejected alternative |
|---|---|---|---|
| 2026-10-24 | Split #1860: this PRD covers writer exclusion only. Container ownership/lifecycle and in-place resume become a separate item-2 issue. | The maintainer chose the split and delegated the scope and simplicity decisions to lead and buddy. Deletion needs exclusion, not ownership. | Building ownership here, which needs full mediation now. |
| 2026-10-24 | No Docker API proxy or authorization plugin. | A proxy is a proof only if it is the agent's sole route to the daemon. That is false today: the socket is `0666`, the rootful posture uses loopback TCP, containers can mount the socket, and hosted runs are single-uid by default. An authorization plugin cannot revoke admitted work. | A per-attempt proxy; an authorization plugin. |
| 2026-10-24 | The rename fence plus inventory verdict is rejected. | A symlink-alias bind keeps writing after the rename and is reported under the alias. After the rename, process proofs lose in-tree processes (their cwd reads `/`). A deferred fence makes the tree look missing to every reader, and operator discard can then orphan it. Stopped leftovers would make it defer almost always. | Rename fence with path or inventory checks. |
| 2026-10-24 | Pod-epoch reclaim, with a positive controller-supplied predecessor-termination guard and no fallback without evidence. | In-flight creates on the old daemon die with the old pod by construction. Graceful pod deletion and same-node RWO sharing defeat "new pod implies old pod gone", so evidence is required. | A documentation-only residual for force-delete. |
| 2026-10-24 | Investigation-only PRD; implementation is a later PRD after owner review. | The guarantees depend on Kubernetes and daemon ordering, which must be source-backed before any deletion path exists. | Implementing with the experiments inline. |

## M0 evidence

Recorded by the #1860 M0 run, 2026-10-24. Real-daemon results come from the harness in `e2e/pod-epoch/`, which runs outside all gates; each result says whether it was run.

### Preflight

- **Images.** Both pins pulled by digest: `docker:29-dind-rootless@sha256:e17fa54c2ffd…84c0` (`deploy/chart/values.yaml:958`, `docker-compose.yml:514`) and `docker:29-dind@sha256:5efed980cba3…d17e` (`values.yaml:959`; compose does not pin the rootful image). Their image config has the same `DOCKER_VERSION=29.8.0`, `DOCKER_BUILDX_VERSION=0.37.0`, `DOCKER_COMPOSE_VERSION=5.5.1` and `DIND_COMMIT=8d9e3502…`. The rootless image runs as `User=rootless`.
- **Runtime versions** (`--version` inside each image): `dockerd` 29.8.0 build `3ce5872`; containerd `v2.3.4` (`db880954…`); runc `1.5.1` (spec 1.3.0, built with go1.26.8); rootlesskit `3.1.0`, rootless image only; tini `0.19.0` (`docker-init`). A running daemon reports `GoVersion go1.26.8` and API `1.56` in both postures.
- **Source.** All source was read through `proxy.golang.org`:
  - `github.com/moby/moby/v2@v2.0.0-beta.23`, whose origin hash `3ce5872b79…` matches the dockerd build, so the `docker-v29.8.0` mapping holds. Its `go.mod` declares `go 1.26.3`; the shipped binary was built with go1.26.8.
  - `github.com/moby/buildkit@v0.33.0`.
  - `github.com/containerd/containerd/v2@v2.3.4`, whose hash matches the binary.
  - `k8s.io/kubernetes@v1.37.1`, matching the controller's `k8s.io/api v0.37.1`.

  The first Kubernetes zip fetch returned a 302 redirect with an empty body; following the redirect worked.
- **Controlled daemon.** `e2e/pod-epoch/lib.sh` starts a privileged, digest-pinned DinD container per posture, on harness-owned volumes for the data root (the DinD PVC) and `/data/runner` (the shared workdir). Ownership is labelled and teardown removes by exact name only. A container restart stands in for a sidecar restart, and a remove-plus-recreate on the same volumes stands in for a successor pod. Wiring deviations: vfs storage, a tmpfs `/run/dind`, and a single container instead of separate worker and sidecar containers. They are listed in `e2e/pod-epoch/README.md`. Result: `bash e2e/pod-epoch/smoke.sh all` **run, exit 0**, with `SMOKE rootless PASS` and `SMOKE rootful PASS` on Engine 29.8.0. The worker's own outer daemon is shared with other runs, so it was never a test subject.
