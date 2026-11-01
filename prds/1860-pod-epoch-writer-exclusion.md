# PRD #1860: Pod-epoch writer exclusion for settled attempt trees (M0 investigation)

**Status**: M0 complete, 2026-10-24: evidence recorded under `## M0 evidence`, recommendation **go (conditional)**. Both postures ran their real-daemon experiments. Q1 is source-backed through the containerd CRI and runc path for exits on the normal shim paths. The enumerated synthetic `terminated` and EXITED statuses defer. One runtime-fault path, a dead shim, can still yield an accepted status without a proven reap. Refusing exit 137 as well as 255 closes it on the paths read, and the measured cost of that refusal is one sidecar that ignores SIGTERM (condition 4, *Dead-shim follow-up*). The kernel pid-namespace step is read in source (`zap_pid_ns_processes`) and corroborated by a real-daemon termination experiment. Awaiting owner review before an implementation PRD. Item 1 of #1860, re-scoped (see Decision Log). Investigation-only: the single milestone M0 produces evidence and a go/no-go recommendation. Implementation is a later PRD, written only after the owner reviews M0.

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
  - stopped containers only where M0 shows they hold no writable reference to the tree. After deletion, a later `docker start` would re-resolve a bind by path. (M0 shows they hold none; see Q3.)
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
| 2026-10-24 | M0 recommendation: go, conditional on controller termination observation and persisted evidence, evidence delivery before `worker.run()`, closing the dead-shim runtime-fault path, and a runtime gate (containerd with runc), with an allowlist of runtime-reported exits. (Revised the same day: see the next two rows.) | Kubernetes 1.37.1 exposes a positive all-terminated signal; dockerd 29.8.0 starts nothing on restore under the barrier predicate; stopped state holds no reference (real-daemon, both postures). A held `/events` stream is evidence only, not the barrier. | No-go now on the grounds that today's controller captures no termination evidence. |
| 2026-10-24 | The dead-shim path closes by refusing exit 137 as well as 255, plus a `dind-meter` SIGTERM fix; the runtime gate is an operator chart value, not node `get`. | On the paths read the dead shim reports only 137 or 255. Over 72 h, all 43 sampled graceful worker-pod terminations with DinD delivered SIGTERM to `dind` about 29.96 s after the worker exited (measured); `dind-meter` ignoring SIGTERM and being killed at grace expiry is the inferred cause, so without the fix refusing 137 would have rejected every sampled epoch. The owner chose the chart value over a ClusterRole. | A node agent's cgroup-empty signal; an owner threat-model amendment accepting the residual; cluster-scoped node `get`. |
| 2026-10-24 | Reclaim does not remove stopped containers. | A later start or copy reaches a new inode, never deleted bytes (Q3). Ancestor- and alias-bound container removal raises ownership questions moved to #3387. | Removing stopped containers bound into the tree (condition 3 of the first draft). |
| 2026-10-31 | Condition 5 = B (operator attestation of the runtime) is restored for hosted-Docker deletion only, per PRD #3431's option A. Compose and plain in-life reclaim need no outer-runtime attestation. | The namespaced controller API cannot establish the full audited runtime/storage tuple; observations can only contradict the declaration. See `prds/3431-pod-epoch-reclaim.md`. | Automatic proof from incomplete API facts. |

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
- **Controlled daemon.** `e2e/pod-epoch/lib.sh` models one worker pod per posture as three digest-pinned containers on the shared outer daemon:
  - a pause container that owns the network namespace;
  - a worker container that holds the `/events` stream and the first-response and window clients (fixture setup and the cp/start/read-only probe commands run the Docker CLI inside the daemon container, which does not change daemon API semantics);
  - a privileged DinD daemon container.

  Harness-owned, labelled volumes stand in for the data root (the DinD PVC), `/data/runner` (the shared workdir) and, rootless only, the socket directory. Teardown removes by exact name only, after a label check, and the image `VOLUME` paths get tmpfs mounts, so nothing unlabelled is left on the outer daemon.

  A `docker restart` of the daemon container stands in for a sidecar restart. Recreating the pod on the same volumes stands in for a successor pod.

  Wiring deviations from `controller/internal/kube/render_dind.go` (all listed in `e2e/pod-epoch/README.md`):
  - vfs storage in both postures;
  - rootless runs an explicit `dockerd --host=unix:///run/dind-sock/docker.sock`. Production keeps the image's `/certs` TLS listener on 2376; the harness binds the unix socket only and asserts that no TCP port listens beyond loopback;
  - rootless `XDG_RUNTIME_DIR` is a per-start tmpfs, where production uses a pod-lifetime `emptyDir`;
  - rootful drops production's MTU cap and its `--config-file` network defaults;
  - *inferred, not read:* `docker restart` keeps the container's writable layer, while a kubelet container restart creates a new container from the image.

  The first smoke result (harness `feb39ab6`) ran the events client inside the daemon container, so it could not tell the daemon closing the stream from the client dying. It is superseded by the official run below.

  The worker's own outer daemon is shared with other runs and was never a test subject.

### Q1. Termination evidence (source only)

Source: `k8s.io/kubernetes@v1.37.1`, `k8s.io/apiserver@v0.37.1`, `k8s.io/client-go@v0.37.1`. No real cluster was run; per the PRD, that does not make M0 incomplete.

**Kubelet ordering.**
1. `Kubelet.SyncTerminatingPod` (`pkg/kubelet/kubelet.go`) calls `killPod`. It then re-reads the runtime and fails if any container still runs. Only after that does it write a status built with `podIsTerminal=true`.
2. `getPhase` (`pkg/kubelet/kubelet_pods.go`) with `podIsTerminal` waits for regular and restartable (native sidecar) init containers to stop. The exception is a non-restartable init failure, which returns `Failed` early; item 3 still guards that case.
3. `status.mergePodStatus` (`pkg/kubelet/status/status_manager.go`) withholds a terminal phase while `PodCouldHaveRunningContainers` is true.
4. `Kubelet.SyncTerminatedPod` then waits for volume unmount (`volumeManager.WaitForUnmount`, `podVolumesExist`). It destroys the pod cgroup (`pcm.Destroy`) and finally calls `statusManager.TerminatePod`, which sets `podIsFinished`.
5. `status.syncPod` PATCHes that status, then `canBeDeleted` requires `DeletionTimestamp`, a terminal phase and `podIsFinished`. Only then does it send `DELETE` with grace 0 and a UID precondition.

So the kubelet's own final DELETE comes after the processes are gone and after the cgroup and volumes are reclaimed. The status with a terminal phase and every container terminated is written earlier, before unmount. It proves the processes are gone, not that the volumes are released.

**What is not evidence:**
- **Object disappearance.** `podStrategy.CheckGracefulDelete` (`pkg/registry/core/pod/strategy.go`) lets anyone delete a terminal-phase pod immediately. A force delete (grace 0) ends in the same store delete and the same watch DELETE (`apiserver registry/generic/registry/store.go`).
- **A terminal phase alone.** `PodGC.markFailedAndDeletePodWithCondition` (`pkg/controller/podgc/gc_controller.go`) PATCHes `Phase=Failed`, leaving container statuses as they were, possibly `Running`, and then force-deletes.
- **An informer's `DeletedFinalStateUnknown`.** It carries the last cached state (`client-go tools/cache/delta_fifo.go`), which can be stale.
- **A `Waiting` status.** `pkg/volume/util/util.go:IsPodTerminated` notes that `Waiting` does not prove the kubelet is not starting a container.

**Signal.** One observed pod object (one UID) that satisfies all of:
- `status.phase ∈ {Failed, Succeeded}`;
- `len(containerStatuses)` and `len(initContainerStatuses)` equal the spec's container counts;
- every entry has `state.terminated` set. Any `waiting` or `running` entry, including a sidecar mid-restart, disqualifies the pod.
- every terminated entry is runtime-reported. This is an **allowlist**: reason `Completed` or `Error`, the two reasons containerd's CRI gives a runtime-reported exit with no other reason (`internal/cri/server/container_status.go:toCRIContainerStatus`, `helpers.go`); a non-empty `state.terminated.containerID`; and an exit code other than 255 and 137 (137 added by the dead-shim follow-up below). Every other reason defers, including `ContainerStatusUnknown`, `Unknown`, `StartError`, `OOMKilled` and `RestartingAllContainers`; see the runtime link below.

A pod stopped before every container ever ran can show `waiting` and defers, which is safe.
- **Runtime link (containerd CRI + runc, read; added after a cross-check).** The chain from `terminated` to "no process of that container survives":
  1. **Kubelet.** `kuberuntime_container.go:killContainer` calls CRI `StopContainer` with the grace period. The runtime does SIGTERM, then SIGKILL. `kubelet_pods.go:convertToAPIContainerStatuses` sets `state.terminated` when CRI reports `CONTAINER_EXITED` (`kuberuntime/helpers.go`). It also *synthesizes* `terminated` without a runtime exit:
     - reason `ContainerStatusUnknown` (exit 137) for a previously running container the runtime no longer knows, in `convertToAPIContainerStatuses` and `status/status_manager.go:TerminatePod`;
     - reason `RestartingAllContainers` (exit 137, empty `state.terminated.containerID`) under pod restart rules, which the worker pods do not use;
     - reason `Completed` for an init container with no CRI status, which carries no `containerID`.

     The allowlist above rejects all of them.
  2. **Pod namespaces.** The pod shares no process namespace: `render.go` sets neither `ShareProcessNamespace` nor `HostPID`, and `kuberuntime/util/util.go:PidNamespaceForPod` defaults to a per-container namespace. So `dind` is init of its own pid namespace. The inner dockerd, containerd, shims and inner tasks are its descendants, inner tasks in nested namespaces.
  3. **containerd CRI** (`containerd/v2@v2.3.4`). EXITED means `FinishedAt != 0` (`internal/cri/store/container/status.go`). On the normal path it is set in `internal/cri/server/events.go:handleContainerExit`, on the shim's TaskExit event, after `task.Delete(…, WithProcessKill)`. It is also set *without* a reaped init:
     - `restart.go`, when after a containerd restart the task is missing or `Wait` fails: exit 255, reason `Unknown`;
     - `container_start.go`, on a start failure;
     - `container_stop.go:cleanupUnknownContainer` and the exit monitor's error path in `events.go`, which pass a locally made exit (exit 255).

     `container_stop.go:stopContainer` sends the stop signal, then SIGKILL on timeout, and waits for that stop. A start failure gets exit 128 and reason `StartError`, and it can leave live processes: the task delete is only logged, and the checkpoint-restore branch skips it. Those synthetic exits do not prove the container's processes are gone. The allowlist rejects every one of them: `Unknown`, `StartError`, and exit 255, which a locally made exit carries with reason `Error`. A genuine exit of 255 also defers, which fails closed.

     **Residual (runtime fault): dead shim.** When a container's shim dies, containerd's `core/runtime/v2/shim.go:cleanupAfterDeadShim` runs the shim's `delete`. That runs `runc delete --force`, only logs a failure (`cmd/containerd-shim-runc-v2/manager/manager_linux.go`), and reports exit 137 whenever the shim's `Stop` returns. On that 137 path the CRI records reason `Error`, which the allowlist accepts. (If `Stop` fails early, containerd records 255 instead and the pod defers.) Usually the exit monitor's own 255 exit is stored first (`events.go`, first exit wins) and the pod defers, but that ordering was not proven. On this path no shim reaps init, and if `runc delete --force` failed, init may still be alive. No pod-status field distinguishes it. It remains a named residual of the status-based evidence. The PRD threat model says an ordinary predecessor workload surviving pod replacement is *not* a residual. So this path cannot simply be accepted. The smaller fix is to refuse exit 137 as well as 255: on the paths read, the dead shim reports only 137 or 255, so the allowlist then rejects it without a node-side signal or a threat-model change (condition 4; its measured cost is under *Dead-shim follow-up*).
  4. **runc shim** (`cmd/containerd-shim-runc-v2/task/service.go:handleInitExit`). The shim is a subreaper (`pkg/shim/shim_linux.go`) and publishes init's TaskExit only after reaping init. For a *private* pid namespace, `runc/util.go:ShouldKillAllOnExit` is false, so it adds no kill-all.
  5. **runc** (`opencontainers/runc@v1.5.1`). `libcontainer/container_linux.go:Signal` and `state_linux.go:destroy` rely on the kernel to kill the rest of a private pid namespace when its init dies. They use `cgroup.kill` only for shared namespaces.
  6. **The kernel step.** When a pid namespace's init dies, the kernel SIGKILLs every process in that namespace and its descendant namespaces, and init is not reaped until they are gone. The SIGKILL of the namespace's processes is documented in pid_namespaces(7). That covers descendant-namespace processes by implication, since they are members of every ancestor namespace. The reaping order is not documented there; it is in the kernel source. Linux v6.18 `kernel/pid_namespace.c:zap_pid_ns_processes`, run when the namespace's init exits, disables pid allocation in the namespace, sends SIGKILL to every task in the namespace's pid table (which holds descendant-namespace tasks too, since they have a pid in every ancestor namespace), then waits until every other task is gone. Its comment states the invariant: "when the init process of a pid namespace is reaped all of the processes in the pid namespace are gone". runc's comments rely on the same behaviour, and the real-daemon `exp-termination` result below corroborates it on this stack, as a finite no-growth observation rather than proof of the ordering. The node kernel version on a target cluster was not checked; this behaviour predates v6.18.
- **Not covered by the runtime link.** CRI-O, Kata or gVisor runtime classes, and a process that escaped the `dind` pid namespace (a deliberate escape, as in the PRD's accepted residuals). A cluster on a runtime other than containerd with runc is outside this evidence and must defer.

**Per case.**

| Case | Positive evidence | Successor can start before the predecessor is terminal |
|---|---|---|
| Recreate rollout (rendered: `controller/internal/kube/render.go:982`) | yes, if observed | no: `pkg/controller/deployment/recreate.go:oldPodsRunning` ignores only `Failed`/`Succeeded` pods. The phase, though, precedes volume unmount. |
| Graceful pod delete | yes, if observed | **yes**: `IsPodActive` (`pkg/controller/controller_utils.go`) excludes a pod with `DeletionTimestamp`, so the ReplicaSet replaces it at once |
| API eviction (`pkg/registry/core/pod/storage/eviction.go`) | yes, if observed | **yes** (graceful delete) |
| Preemption (`pkg/scheduler/framework/preemption`) | yes, if observed | **yes** (graceful delete) |
| Kubelet node-pressure eviction | yes: the object stays `Failed` | no: phase is withheld until containers stop |
| Force delete, node loss, podgc | **no** | yes. These cases are ineligible |

**Other pods on the PVCs.** The claims are `ReadWriteOnce` (`render.go` `renderPVC`), not `ReadWriteOncePod`. Only RWOP is enforced at mount time (`pkg/volume/util/operationexecutor/operation_generator.go`). A second pod on the same node can therefore mount the data or DinD-data PV. Cross-node reuse waits for detach, through the attach/detach controller's Multi-Attach check (`pkg/controller/volume/attachdetach/reconciler/reconciler.go`); *inferred*, not traced to the end. An out-of-band pod naming the claims stays an operator residual.

**The uzi controller today.**
- It polls with a full LIST every 10 s (`controller/internal/config/config.go:329`, `CONTROLLER_POLL_INTERVAL`). It has `list`-only pod RBAC (`deploy/chart/templates/worker-rbac.yaml`), no watch and no in-memory pod state.
- It already counts terminating pods (`controller/internal/kube/materializer.go:421`) and reads the DinD PVC UID.
- It never reads an all-terminated status as proof. Its DinD recycle and volume recycle check *absence* only: `maintenance.go` waits for `WorkerPodCount==0`.
- The terminal-status window lasts from the end of `SyncTerminatingPod` to the kubelet DELETE. *Inferred, not measured:* it is often short, since unmount time is unbounded but usually small. A 10 s poll can miss it and then sees only absence, which is not evidence. Most epochs would therefore defer unless the controller observes termination more finely: a watch on the worker's pods or a shorter poll while a pod is terminating. A watch would need new RBAC.

**Binding.** The controller should durably record:
- the predecessor pod UID;
- per container, `containerID`, `restartCount` and `state.terminated`;
- for the data and DinD-data PVCs, `metadata.uid` and `spec.volumeName`, read from that pod's `spec.volumes` claim names.

It must persist that record *before* any successor can consume it. The precedent is the UID- and resourceVersion-guarded PVC annotation patch in `controller/internal/kube/roll_pacing.go` (`mirrorRoll`). It binds the record to the successor UID only after it lists the successor: a different UID, the same worker label and claim names, owned by the Deployment's current ReplicaSet. That successor UID is what the worker reads as `UZI_POD_UID` (`render.go:1016`).

The worker defers when:
- any field is missing;
- the PVC UID or PV name changed, except across a controller-planned DinD-PVC replacement (the ADR-1759 recycle), which the evidence must record as old and new DinD-PVC identity, the controller having already observed the predecessor's termination. Without that rule every DinD maintenance recycle would defer;
- the predecessor equals the successor;
- more than one live pod (a pod whose phase is not terminal and that is not the evidenced predecessor), or another pod naming the claims, was seen. This makes graceful delete, API eviction and preemption ineligible in practice, because their successor overlaps a non-terminal predecessor; under Recreate the terminal predecessor object may still exist when the successor appears, which this definition allows;
- a controller restart came between observation and persistence;
- the cluster runs a Kubernetes version other than 1.37.x.

**Not settled by source.** Runtimes other than containerd with runc, the target nodes' kernel versions, and the 255-versus-137 exit ordering on the dead-shim path (all above; the ordering no longer matters once both codes are refused), kubelet restart mid-termination, the target cluster's feature gates and admission, attachable vs node-local storage, and the transport of evidence to the worker (the worker has no Kubernetes RBAC).

### Q2 and Q3. Restore barrier and stopped state: source findings

Source: `github.com/moby/moby/v2@v2.0.0-beta.23`, `containerd/v2@v2.3.4`, `buildkit@v0.33.0`. Real-daemon results follow under "Real-daemon results".

**Startup order.** In `daemon/command/daemon.go` (`daemonCLI.start`):
1. API listeners are bound first (`loadListeners`).
2. A managed containerd child starts.
3. `daemon.NewDaemon` runs, in this order: the plugin manager, which restarts enabled managed plugins (`daemon/pkg/plugin/manager.go:reload`); the volume service; the engine ID (`daemon/id.go`, persisted `engine-id`); then `d.restore`.
4. Swarm restarts if state persists (`daemon/cluster/cluster.go:Start`). Then `RestartSwarmContainers` and BuildKit init run.
5. Only then does `httpServer.Serve` run.

So a connection is accepted into the backlog early, but the **first HTTP response comes only after restore and every restore-time start**. `/events` writes and flushes `200` just *before* it registers the subscription (`daemon/server/router/system/system_routes.go:getEvents`), so a received `200` does not by itself prove the subscription is live; a caller should use `since` or observe a first event.

**`restore`** (`daemon/daemon.go`):
- It kills any surviving containerd task, since live-restore is off by default (`daemon/command/config_unix.go`).
- It marks containers persisted as running `stopped`, keeping the task's real exit code, or 255 when containerd reports none.
- It removes `AutoRemove` leftovers.
- It synchronously starts every container with `cfg.AutoRestart && ShouldRestart() && HasBeenStartedBefore`, before the API serves. `ShouldRestart` (`daemon/internal/restartmanager/restartmanager.go`) is true for `always`; for `unless-stopped` unless manually stopped; for `on-failure` on a non-zero exit, which includes the 255 above, while under `MaximumRetryCount`. It is never true for `no`.
- After the barrier, the only runtime restart path is `daemon/monitor.go:handleContainerExit`, which needs a running container. Health checks run only for running containers (`daemon/health.go`).

**Barrier candidate.** After the first API response, if all of the following hold, nothing restored can start or take a mount without a new client request:
- no container is `running`, `restarting` or `paused`;
- every container's `HostConfig.RestartPolicy.Name` is `no`. This must be checked by inspect: `/containers/json` does not carry the policy. Restore migrates an empty name to `no`;
- `Swarm.LocalNodeState` is `inactive`;
- no plugin is enabled;
- `LiveRestoreEnabled` is false.

The restart-policy rule costs nothing on the lead's sample: 0 of 11 workers had a non-`no` policy. Without the rule, a restore-started container is running at the barrier and the first check alone defers it.

**Restart detection.** A sidecar restart re-runs `restore`, since `NewDaemon` runs it on every start. `GET /info` `ID` is persisted (`daemon/id.go`), and `SystemTime` is just the clock, so neither identifies an incarnation. Events live in an in-memory buffer (`daemon/events/events.go`), and `Shutdown` closes the event service. A stream opened before the inventory and held through deletion therefore ends on any daemon exit; a hard kill closes the socket. This is supporting evidence only (see "Restart detection is evidence, not a barrier" below).
- Rootless only: the socket inode changes on each start (not read in source; measured on the harness socket volume, see Real-daemon results).
- Controller-side only: the `dind` `containerID` and `restartCount` change.

**Stopped state (Q3).**
- **Stopped containers.** On exit, `containerExit` deletes the task, and `Cleanup` unmounts IPC, the rootfs and volumes (`daemon/monitor.go`, `daemon/start.go`). Binds live in the task's mount namespace and die with it. `docker cp` uses a private, discarded mount namespace (`daemon/containerfs_linux.go:openContainerFS`). A stopped container keeps only path strings, `MountPoint.Source` in `config.v2.json` (`daemon/volume/mounts/mounts.go`): no file descriptor and no mount.
- **Images and build cache.** Images reference no host path. BuildKit copies local context into its own snapshots under `<data-root>/buildkit` (`source/local/source.go`); no host-path reference was found in `solver`, `executor` or `cache`.
- **Local volumes.** A local volume without options is a directory under `<data-root>/volumes` (`daemon/volume/local/local.go:Mount` returns the path). With `type`/`o`/`device` options (`local_unix.go:validOpts`, `needsMount`), it bind-mounts a host path while referenced. The API's `Options` comes from the volume store's metadata (`daemon/volume/service/store.go`, `convert.go`), so "local driver, empty `Options`" excludes every such volume created through the API.
- *Residual:* a volume directory written into the data root out of band has no metadata.

**Re-resolution by path (important).** `containerStart → setupMounts → MountPoint.Setup` (`daemon/start.go`, `daemon/volumes_unix.go`) uses the persisted `Source` path at request time, and `docker cp` does the same through `openContainerFS`. When the source is missing:

| Mount form | Result |
|---|---|
| `-v`/`Binds` | `SkipMountpointCreation=false`, so `Setup` **recreates the whole missing path** (`MkdirAllAndChown`) |
| `--mount type=bind` without `CreateMountpoint` | fails |
| `--volumes-from` | copies the source string as the recreate variant |

So deleting a tree does not stop a later `docker start` or `docker cp` of a stopped `-v` container from creating an empty directory at the deleted path. That needs a client request (Q4), and the directory never shares the old inode, so nothing reaches the deleted data. Whether reclaim should also remove such stopped containers, to stop an empty path from reappearing, is a separate lifecycle decision (see the Recommendation); it is not needed for writer exclusion.

**Postures (Q5, source).**
- **Rootless:** ExecRoot and the pid files live in `$XDG_RUNTIME_DIR` (`daemon/config/config_linux.go`), which is the pod-lifetime `dind-sock` emptyDir in production. A stale pid file across a sidecar restart can cause a start failure (`pkg/pidfile/pidfile.go`), not a silent restart. rootlesskit 3.1.0 source was not read.
- **Rootful:** ExecRoot is in the container filesystem; the endpoint is loopback TCP; the chart passes no `--live-restore`.

Apart from these points, the source findings apply to both postures.

### Q4. No-client interval

Seed: `git grep -l -E 'dockerRequest\(|execDocker\(|teardownDocker\(|dockerRunningContainers\(|DOCKER_HOST|dockerHost' -- agent/src`, 12 files. The sweep followed imports to `maintenance-exclusion.ts`, `recovery.ts` and `ops/retained-source-server.ts`. It also grepped `agent/src`, `agent/templates/entrypoint.sh`, `agent/bin` and `agent/scripts` for `docker.sock`, `/run/dind`, `2375`, `socketPath`, `DOCKER_` and testcontainers. Code is at this branch's base `065a806a`.

**Startup order.**
1. `main.ts:367`: `resolveDockerWiring`. It only connects (`docker-wiring.ts` `defaultProbe`) and sends no bytes.
2. Construction of the runner, `DindPruneGate`, `MaintenanceWriterExclusion` (`main.ts:612`), `createDindPrune` (`:800`) and `DindMaintenanceController` (`:806`). No requests.
3. Signal handlers, filesystem sweeps and the process-only Codex orphan reap (`main.ts:980`, `startupReapVerdict`).
4. `worker.run()`. Inside it:
   1. toolchain preflight;
   2. `snapshotBootRecoveries` (`worker.ts:518`);
   3. `registerWithRetry` (`:528`), which starts the heartbeat;
   4. fire-and-forget `resumePendingRecoveries` (`:545`);
   5. `resolveBootTerminals`;
   6. **`worker.ts:595`**: settlement, DinD prune, DinD maintenance, disk reclaim, the retained-source server, and all claim loops start together. This is where claim admission opens.

**Clients.**

| Client | Requests | Class | Earliest |
|---|---|---|---|
| Wiring probe (`docker-wiring.ts`) | TCP/unix connect only | none | step 1 |
| Kubelet startup probe (`render_dind.go:336` rootful, `:353` rootless) | `docker info` (`/_ping`, `GET /info`) | read-only | before the worker container, and again on every `dind` restart |
| `terminalMaintenanceProof` (`run-quiescence.ts:810`, wired at `main.ts:615`) → `dockerRunningContainers` | `GET /containers/json` | read-only | after register, on recovery and terminal paths |
| `teardownDocker` (`run-quiescence.ts`) via `quiesceRunAttempt` | `GET /containers/json?all=1`, then `DELETE /containers/{id}?force=1&v=1` | listing, then **removal** | **before admission**: `resumePendingRecoveries` (`worker.ts:545`), heartbeat `resumeLiveRecoveries`, `forgetCompletedGeneration` (`recovery.ts`), only when a retained clone exists |
| Run-lane `quiesceRun` (`runner.ts`) and the retained-ops quiesce (`ops/retained-source-server.ts`) | as above | removal | after `worker.ts:595` |
| DinD prune (`dind-prune.ts:98-106`) | `ps -q`, `image prune -f`, `builder prune -f`, `image prune -a -f --filter until=24h` | read-only, then removal | after `:595`, plus at least two over-threshold meter samples |
| DinD maintenance (`dind-maintenance.ts:10`) | `GET /version`, `volume prune -f` | read-only, then removal | after `:595`, on a server-fenced `ready` |
| Run agents (`sdk-env.ts`, `codex/codex-executor.ts` set `DOCKER_HOST`; `guardrails.ts` allows docker) | anything (create, start, exec, cp, build, volume create) | **state-changing** | after a claim |

The `dind-init` and `dind-meter` sidecars make no Docker API calls (`render_dind.go`).

**Not worker-owned, but able to reach the daemon:**
- On rootful, every process in the pod network namespace can reach the daemon over loopback TCP (`render_dind.go`, `dindLoopbackTCP`). That includes run-agent children that outlive a worker *process* restart within the same container.
- Containers on the daemon can reach it too, with host networking or a mounted socket. Running containers are already excluded by the eligibility rule.
- On rootless, production keeps the image's TLS listener on 2376 (the harness deviation note above). *Inferred from the image entrypoint and `render_dind.go`'s mounts:* it requires client certificates that exist only in the `dind` container's filesystem, since the pod mounts no `/certs` volume, so no other pod container holds credentials for it.

**No-client interval.** Insert the once-per-pod reclaim in `main.ts` after the `startupReapVerdict` block and before `worker.run()`. Nothing worker-owned has sent a request there, and the only allowed requests are read-only: the reclaim's own `GET /_ping`, `GET /info`, `GET /containers/json?all=1`, `GET /containers/{id}/json`, `GET /volumes`, `GET /plugins` and `GET /events`; the kubelet `docker info` probe; and the connect-only wiring probe. The reclaim sends no write request to the daemon.

The cost of that placement is that it comes before registration and API contact, so controller evidence must reach the worker some other way. Options are a downward-API-projected annotation or a pre-registration API read; that is a design choice for the later PRD.

Placing the reclaim later is possible only before `registerWithRetry` (`worker.ts:528`). Any later placement needs a latch: on recovery (`resumePending` and `resumeLive` both reach `teardownDocker`), on the prune and maintenance loops, on the retained-source server, and on the claim loops.

**Process residual.** A worker *process* restart inside a live container keeps run-agent children, and with them potential clients. Under the *proposed* one-shot, keyed by `UZI_POD_UID` (no such marker exists today), only the first process of a pod would reclaim. A crash-restarted process would see the consumed marker and defer.

### Q7. Cadence (source and issue text)

**Fill rate (#3329).**
- First worker: a 25 GiB `/data` at 100 %. `/data/runner` held 23 GiB in 15 trees, of which 10 settled trees held about 12.5 GiB.
- Comments dated 2026-10-24 record recurrences on three more workers: one at 100 % on 25 GiB, and two with 50 GiB `/data` volumes, one holding 20 settled trees out of 23.
- Settled trees on one worker survived a pod replacement onto a newer agent image (#3329, 2026-10-24 comment).

*Inferred:* the issue gives no rate, but the recurrences suggest a busy worker fills its volume within days.

**Epochs.**

| Path | Driven by | Leaves `/data/runner` | uzi observes the predecessor today |
|---|---|---|---|
| Template or pin drift rollout (`materializer.go`, Recreate) | Deployment controller after a uzi patch | yes | no, only ordering |
| DinD-only maintenance recycle (`maintenance.go`, ADR-1759) | uzi, on DinD-volume pressure only | yes; the DinD PVC is new | absence only (`WorkerPodCount==0`) |
| Legacy data recycle and worker teardown | uzi | no: the data PVC is deleted | absence |
| Eviction, drain, manual delete | kubelet or operator | yes | no (outside the controller's control; `adr/3153-runner-shared-pvc-subpath.md` D7 calls them unguardable for its migration guard). An OOM kill restarts a container in the same pod, so it is not an epoch |

Worker-pin rollouts follow release cuts that change the agent runtime: 29 non-RC tags since 2026-08-24, and RCs almost daily in late October 2026 (`git tag`). The worker pin (`deploy/chart/values.yaml` `workers.image.tag`, which the controller stamps into the pod-spec hash) changed in 73 commits on 46 distinct days between 2026-08-27 (the start of the public history) and 2026-10-24 (`git log -L` on that line, excluding the file-creating initial commit), so a pin roll was available on roughly three days in four. *Inferred:* how soon a cluster applies a pin depends on its GitOps sync, which was not measured. That rate could match a days-scale fill rate, but only if the controller actually captures termination evidence at those rollouts, which today's 10 s absence-only poll does not do.

**On demand.** An idle worker **cannot** request its own pod recycle. Its heartbeat disk stats do feed `diskOverThreshold` (`api/internal/workersvc/service.go`), which drives the controller's legacy data recycle (`maintenance.go` `legacyRecycleDue`), but that recycle deletes the data PVC. `dind-maintenance.ts` only answers a server operation that the controller (`maintenance.go` `reconcileDinD`) starts on DinD-volume pressure, under server fences (`api/internal/workersvc/dind_maintenance.go`, ADR-1759 D7). Data-volume pressure enters only the legacy recycle, which deletes `/data/runner` and skips custody-held workers. An on-demand epoch therefore needs a new, fenced request type, such as "recycle pod, keep PVCs". That is the later PRD's choice.

**What is lost.** A graceful delete, an eviction or a preemption lets the successor start while the predecessor terminates. That pod's one-shot is consumed and it defers. A node drain does this to every worker it touches.

**#3329 trigger.** Today it reads: *"Only while the worker is idle, inside the existing `retained_ops` claim-gate close, drain and `isIdle()` sequence. It is triggered from the periodic D7 pass in `disk-reclaim.ts`, and when disk admission refuses a claim on an idle worker."* The proposed replacement:

> Reclaim runs only once per worker pod, at worker startup before any Docker client and before claim admission first opens, with controller-recorded predecessor-termination evidence bound to the predecessor pod UID, the successor `UZI_POD_UID` and the data and DinD-data PVC identities. It never runs from the periodic pass or from an admission refusal; those may at most request a fenced pod recycle (a new operation) that produces the next epoch.

### Real-daemon results (Q2, Q3, Q4 probes, Q5)

**Cross-check re-run.** A cross-check found three evidence gaps. The tree scan ignored unreadable reads. The re-resolution outcomes were not asserted. The `terminated` ⇒ dead-writers link was unread.

The harness was then hardened:
- **Scans fail closed.** Any unreadable namespace, mountinfo, cwd, root, exe, fd or maps read of a live process is `SCAN-UNREADABLE`, counted separately from processes that vanished.
- **Zero-reference conclusions need a complete scan:** `unreadable=0`, dockerd's mount namespace read, and the control's namespace set covered. Otherwise the experiment is `INCOMPLETE`.
- **The unreadable instrument is checked.** A positive control proves `SCAN-UNREADABLE` fires by scanning as uid 65534.
- **cp/start outcomes are checks**, and the experiments check the inode a container actually binds.
- **New experiment:** `exp-termination.sh`.

On a clean tree, Engine 29.8.0, `exp-stopped`, `exp-volumes-build` and `exp-termination` each exited 0 on `all`, with `EXP … PASS` in both postures. There was no `FAIL`, no `INCOMPLETE`, and every instrument scan read `unreadable=0`; only the deliberate uid-65534 control reports unreadable items. `exp-stopped` and `exp-volumes-build` ran at `4cf133d8`, and the only change since then is `exp-termination.sh`. Their lines are committed in `e2e/pod-epoch/results/m0-xcheck-016ba790.txt`. Where a Q3 statement below depends on a re-run check, it is supported by that file. Restore, read-only and smoke results come from the earlier run. Since then their scripts changed only by one comment line, and `lib.sh` changed only in the tree-scan functions they never call.

**Official run.** Run at harness commit `0658dee5`, clean tree, Engine 29.8.0, both postures, sequential on 2026-10-24.

- **Result:** `smoke.sh`, `exp-restore.sh`, `exp-stopped.sh`, `exp-volumes-build.sh` and `exp-readonly.sh` each ran on `all` and exited 0.
- **Verdicts:** every experiment prints `EXP … rootless PASS` and `EXP … rootful PASS`, and no `check.*` line is `FAIL`.
- **Raw lines:** the `RESULT`/`EXP` lines are committed in `e2e/pod-epoch/results/m0-0658dee5.txt` (verbatim apart from stripped trailing whitespace).
- **Cleanup:** no `pe1860.owner` container or volume remained, and the outer daemon's dangling-volume count was unchanged.
- **Reviewer and auditor:** this run's reviewer and auditor reviewed the scripts at that commit (review reports are in the run transcript, not the repo). No blocking or Medium findings remained; remaining notes are listed under "Limits of the experiments".
- **Earlier run:** at `7d343ce6`, after one ENOSPC failure on the shared outer daemon and a retry, the run passed too. Review then found checks that could pass vacuously (kill delivery, the events-window instrument, plugin-listing errors), which `0658dee5` fixed together with other fail-closed hardening, so only the `0658dee5` run is cited.

**Restore (Q2).** Seven fixtures on a persisted root, through a daemon restart and through a successor-pod replace.

At the first API response:
- **Already running:** `c-always`, `c-always-stopped` (stopped by hand, restarted anyway), `c-unless` and `c-onfail` (`running_at_first_response=4`).
- **Not started:** `c-unless-stopped`, `c-no` and `c-no-stopped`. The `check.*.c-no*.not-started` checks pass.

An events stream opened with `--since` set to the first-response time recorded **0 events** in a 20 s window (`window.events_after_first_response=0`), in both postures and both modes.
- That window is supplementary. Its instrument was checked against a control of the same form: the same `--since` form, the timeout-kill status and empty stderr.
- It agrees with the source finding that restore-time starts finish before the API serves.

**Barrier predicate (Q2).** The case: only stopped `no`-policy containers, then a restart. At the first response:
- no container is running, restarting or paused;
- `swarm=inactive`, `live_restore=false`, `plugins=0`;
- every policy is `no`.

All 7 `check.barrier.*` checks pass. Positive controls showed the same instruments seeing running containers, non-`no` policies, `swarm=active` (bound to loopback), an installed plugin and `live_restore=true`.

**Restart detection (Q2).** An `/events` stream was held from a separate client container on an idle daemon.

| Trigger | Stream ended | Supplementary time to EOF (rootless / rootful) |
|---|---|---|
| Container restart | yes | 123 ms / 78 ms |
| SIGKILL of dockerd | yes; the kill was asserted delivered (rootless in-container kill, rootful outer `docker kill`) | 215 ms / 102 ms |
| SIGTERM of dockerd | yes | 201 ms / 99 ms |

- **Negative control:** an untouched stream stayed open for 5 s.
- **`GET /info` ID:** identical before and after every restart.
- **Rootless socket inode:** a new socket inode was created on every start. This was measured against a hard link holding the old inode, because a plain inode comparison can reuse the number.
- **Different kill9 subjects per posture:** the kill9 row measures different things. Rootful kills the whole daemon container from outside; rootless signals only dockerd inside the container (the container then exits with it, exit 137). Each case's `delivery` line records which.
- **Rootful kill9 recovery failure (recorded as a finding):** after a rootful kill9, the *same-container* restart **failed**:
  - The error was `failed to save containerd pid to disk: process with PID 36 is still running`.
  - A stale `/var/run/docker/containerd/containerd.pid`, `exists=yes content=36`, survived in the container's writable layer.
  - The daemon then never served, which fails closed.
  - With a `--time 10` stop on the `restart` case, one development run hung with "containerd is still running" (a related stale-containerd state, not the same message); the `7d343ce6` official run with `--time 10` did not hang, so it is intermittent. *Suspected, not proven:* the fixtures ignore SIGTERM, so the short grace preempted dockerd's own shutdown. The official script's restore restart step uses `--time 30`, Kubernetes' default grace; its other restarts (no running fixtures) use `--time 10`, and one recovery restart of an already-dead daemon uses `--time 5`.
  - Whether production hits this depends on where the pid file lives:
    - Rootful: production keeps it in a writable layer that a kubelet container restart replaces (inferred).
    - Rootless: production keeps it in the pod-lifetime `dind-sock` `emptyDir`, which the harness's tmpfs does not reproduce, so the risk is **not exercised** there.
  - Either way, the outcome is a daemon that does not serve, never a silent restart.

**Stopped state (Q3).** A running `-v /data/runner/tree-B:/w` container was the positive control. The scanner saw:
- the bind in the task's mount namespace (`mnt_strict=1`);
- a cwd/fd in the tree (`fd=1`);
- and, from a separate harness cwd holder measured after `rm -rf`, a deleted-inode fd (`control.deleted_inode_fd=1`).

After `docker stop`, and again after a daemon restart, every scan found `refs=0` (mount, cwd, fd and memory-mapping references). Each of those scans proved it read dockerd's own mount namespace: `scan_complete=yes … dockerd_ns_read=yes` at `0658dee5`. At `4cf133d8` (the cross-check run of `exp-stopped`) each was also complete under the fail-closed gate (`unreadable=0`, namespace set covered).

After the tree was deleted (`after_rm exists=no`):

| Request on the stopped container | Rootless | Rootful |
|---|---|---|
| `docker cp` into a `-v` container | **recreates** the tree: new inode, owner `1000:1000`, file copied | **recreates**: owner `0:0` |
| `docker start` of an idle `-v` container | **recreates**: empty, new inode | **recreates**: empty, new inode |
| `docker start` of a `--mount type=bind` container | fails: `bind source path does not exist` | same |
| `docker cp` into that container | fails: `no such directory` | same |
| `docker start` of a `--volumes-from` copy of it | **recreates** the tree | same |

At `4cf133d8` these outcomes are checks. `cp`/`start` either fail or recreate the tree on an inode distinct from every held deleted inode, and the started container's `/w` is that recreated inode (`check.*.reresolution`, `check.*.container-binds-recreated-inode`). `--mount` start and cp fail (`check.start-c-mount.fails`, `check.cp-into-c-mount.fails`). Linux refuses to create entries in an unlinked directory, so the held deleted trees are recorded only as observations (`obs.held*.listing_*`). So nothing stopped can write to the deleted inode. A later *client request*, though, can recreate an empty directory at the deleted path. That makes Q4's client exclusion load-bearing during the reclaim; after it, a recreated path is a new empty inode (a lifecycle concern, not a writer reaching deleted data).

**Volumes and build cache (Q3).**
- `v-plain` reports `local` with `null` options.
- `v-bind` (`-o type=none -o o=bind -o device=/data/runner/tree-V`) reports its options. While in use it is bind-mounted in dockerd's own mount namespace (`strict_in_dockerd_ns=1`). It holds no reference after stop and after a restart.
- So the predicate "local driver, empty `Options`" does separate the two.
- A build with context `/data/runner/tree-K` and a cache mount left no mount, fd or mapping reference after the build, after the tree was deleted, or after a restart.
  - Its 8 build-cache records survived the tree's deletion.
  - During the build, the only holder seen was the buildx *client* (supplementary sampling).
  - The mapping category is controlled by a process running a binary from inside a tree (`map=5`).

**Read-only probes (Q4, Q6).** These were issued between two event markers:
- `info` and `version` (the CLI's own `/_ping` negotiation is included, not a separate command);
- `ps -a`, plus `inspect` on every container;
- `volume ls` / `inspect`, `plugin ls`, and a bounded `events` read.

Result: **0 events** and a byte-identical inventory of containers, volumes, images and networks. A positive control (`volume create` plus `start`) produced events and changed the inventory. The predicate's inventory queries are therefore read-only on Engine 29.8.0.

**Termination (Q1 runtime link, real daemon).** `exp-termination.sh` runs three writers inside an inner container's nested pid namespace: the main loop, a `docker exec` loop and a backgrounded child. Each appends to `/data/runner/tree-W-*/log` every 100 ms. The daemon container is ended from the outer engine, either with `docker kill -s KILL` or with `docker stop --time 10` (SIGTERM, then SIGKILL).

All of the following pass in both postures, for both cases:
- `writers-active-until-signal`: the log grew between the last pre-signal read and the exit.
- `writers-stopped-by-exit`: the log is identical at the moment the outer engine reports `exited`, after a 5 s supplementary window, and from a fresh reader.
- `last-write-not-after-exit`: the log's last mtime is not later than the container's `FinishedAt`, by 17 to 51 ms.
- `restored.log-does-not-grow` and `restored.restart-policy-no`: 3 s after the pod is recreated the log is unchanged, and the writer's policy is `no`. Observations, not checks: the restored writer is exited, and no writer process exists.

Two further observations:
- Under `docker stop`, which signals the daemon container's PID 1, all three writers kept writing for the full 10 s grace. The log grew 236→1393 bytes in rootless and 251→1402 in rootful. They stopped at the final SIGKILL (exit 137).
- This exercises the outer Docker 29.8.0 engine's containerd and runc stop path as a stand-in for kubelet plus CRI containerd: the same shim and runc family, but not the CRI plugin.

**Postures (Q5).** Every Q2, Q3 and Q4 experiment above ran and passed on both postures. Neither posture is UNRUN, but that does not close the production rootless restart shape: the pid file in the pod-lifetime `dind-sock` `emptyDir` across a sidecar restart remains **unvalidated** (see above).

**Limits of the experiments** (non-blocking review notes):
- The scans' remaining blind spot is a mount namespace held only by an nsfs fd or bind mount, with no task in it. The `SCAN-UNREADABLE` positive control can trigger only the namespace-read item by permission. The coder checked the other items' reporting on crafted input (an always-failing `stat`, a zombie thread-group leader with a live thread), as described in `e2e/pod-epoch/README.md`. Those checks are not committed tests and were not run on a real daemon.
- `last-write-not-after-exit` uses file mtime from the kernel's coarse clock, which can read up to one tick early.
- The termination experiment uses the Docker engine's containerd and runc stop path, not kubelet with the CRI plugin.
- The listener check reads TCP only, not UDP.
- The anonymous-volume check skips the one-shot chown helper, and its tmpfs path list is hard-coded from the pinned images.
- The rootful socket check reports PASS where there is no socket to measure (not applicable).
- These do not change any conclusion above; they bound the evidence.

### Restart detection is evidence, not a barrier

A held `/events` stream ending on every daemon restart is an **experiment observation**. By itself it is not a barrier that prevents writes during deletion. An EOF reports a restart after the fact, and a restore-time start fires before the new incarnation serves.

Exclusion during deletion rests on three things established separately:

1. **The no-client interval (Q4).** No worker-owned state-changing request can be issued until reclaim ends.
2. **The barrier predicate (Q2).** Every container has restart policy `no`, swarm is inactive, there are no enabled plugins and live-restore is off. Under this predicate a restore, including an *unseen* restart, starts nothing.
3. **Stopped state holds no reference (Q3).** Nothing stopped holds a mount, file descriptor or mapping into the tree.

An implementation may keep the stream as an extra abort-and-defer trigger, but it must not cite the stream as the exclusion.

### Q6. Eligibility estimate

- **The sample.** The lead's sample (11 workers, read-only, 2026-10-24) has 10 of 11 passing the candidate predicate at that instant. The one failure was a busy worker. Adding "restart policy `no` for every container" keeps 10 of 11.
- **At a pod epoch.** *Inferred from Q1 and Q2, subject to the Q1 runtime-link caveats (runtimes not read, dead-shim path closed only by refusing 137):* at a pod epoch every predecessor container is dead, and restore starts only non-`no` containers. None of the 11 sampled workers had one. So the running-container clause should pass on effectively every evidenced epoch, including the sampled busy worker.
- **Stopped containers.** These are present on 7 of 11 workers, up to 27 per worker. They do not block eligibility, and the reclaim leaves them in place (Q3).
- **Volumes.** No sampled volume had a non-local driver or non-empty `Options`.
- **What limits eligibility.** The predicate rarely does. Epoch evidence does (Q1, Q7).

### Dead-shim follow-up: refusing exit 137 (measured 2026-10-24)

The cheaper closure of the dead-shim path is to refuse exit 137 alongside 255, before any node-side machinery or threat-model change.

- **Paths read.** On the dead-shim path containerd records 137 (shim `Stop` returned) or 255 (early failure, or the exit monitor's own exit). The kubelet's synthesized `terminated` statuses also carry 137 and are already rejected by reason. So with 137 and 255 refused, no path read here yields an accepted status without a shim reap. Exit 137 is also the normal result of SIGKILL at the end of a grace period, which is what the refusal costs.
- **Measured cost.** Read-only, from the cluster's pod log store (72 h retention), over every docker-lane worker pod that terminated in that window: in all 43 graceful terminations of a pod running DinD, the worker logged `shutting down` on SIGTERM and exited 0 within about 20 ms, and `dind` logged `Processing signal 'terminated'` 29.95–30.12 s later, then `Daemon shutdown complete` about 1 s after that. Native sidecars stop in reverse order after the main container, so `dind-meter` (rendered after `dind`) is stopped first. Its script is a `/bin/sh -c` loop running as its namespace's PID 1 with no `TERM` trap, so SIGTERM is ignored, the container is SIGKILLed at grace expiry (exit 137, inferred: the pods were gone before their statuses were read), and only then does `dind` get its own SIGTERM. No live pod had a container restart with a recorded `lastState`.
- **Result (inference from that sample).** The 43 traces are measured; the `dind-meter` exit 137 is inferred from them and from its script. On that basis, refusing 137 without a fix would have rejected every one of the 43 sampled epochs. With `dind-meter` exiting on SIGTERM (a `trap` plus an interruptible `sleep`), those graceful terminations would plausibly have produced no 137, since the worker exited 0 within milliseconds and `dind` finished its shutdown in about 1 s. A 137 would then mean a container overran its grace, an OOM kill (already deferred by reason) or the dead-shim fault, and each defers. The sampled stops would also have been about 30 s shorter. The implementation PRD must validate the fixed meter's exit status and the resulting stop timing; neither is measured here.
- **Not covered.** Other runtimes' synthetic exit paths (CRI-O is unread) and node-kernel differences.

### Recommendation: **go**, conditional

M0 answers Q1–Q7. Q1 is source-backed (no real cluster, as the PRD allows). Its runtime link is read through containerd and runc, its kernel step is read in kernel source and corroborated by the real-daemon `exp-termination`, and its one runtime-fault path closes by refusing exit 137 (condition 4). Q2, Q3, Q5 and Q4's read-only probes have real-daemon results on Engine 29.8.0 in **both** postures (Q4's no-client interval is source-only), and neither posture is UNRUN.

The guarantees hold as follows:

- **Predecessor writers.** These are excluded when there is controller evidence that one pod UID reached terminal phase with every container and init-container status `terminated` (Q1).
  - That signal exists in Kubernetes 1.37.1.
  - With containerd CRI and runc, on the normal exit paths, `terminated` follows the shim reaping the container's init. The kernel's pid-namespace teardown then removes every descendant, including the inner dockerd, shims and nested-namespace tasks. The kernel step is read in source (`zap_pid_ns_processes`) and corroborated by the real-daemon `exp-termination`. Other runtimes defer. The exception is the dead-shim runtime-fault path (Q1 runtime link; condition 4).
  - The current controller never captures it. It polls every 10 s with `list` only and checks absence.
  - **Condition 1:** the controller must gain a termination observation, a watch or a fine-grained poll during termination, and persist the evidence before any successor can consume it.
- **Successor writers.** These are excluded by placing the reclaim before `worker.run()` (Q4), with only the listed read-only probes.
  - **Condition 2:** the evidence must reach the worker without the worker registering first, or the reclaim moves just before `registerWithRetry`.
- **Persisted daemon state.** This is safe under the barrier predicate (Q2, Q3).
  - **Not a condition: stopped containers.** Reclaim does not remove them. A later `docker start` or `docker cp` of a stopped `-v` container can recreate an empty directory at a deleted path, but that is a new inode and reaches no deleted data (Q3). Removing ancestor-bound or alias-bound containers raises ownership and deletion-authority questions this PRD moved to #3387; unresolved attribution must not authorize removal. (Condition 3 of an earlier draft is withdrawn; the numbering is kept.)
- **Runtime faults and runtime gate.** These close the gaps in the predecessor-writer evidence (Q1 runtime link).
  - **Condition 4:** the allowlist refuses exit 137 as well as 255, which rejects the dead-shim path on the paths read, with no node-side signal and no threat-model change. In the 72 h sample its cost would have been every epoch, because `dind-meter` ignored SIGTERM and (inferred) was killed at the end of each grace period; the implementation must make `dind-meter` exit on SIGTERM in the same change (*Dead-shim follow-up* below). A container that genuinely needs the full grace (a slow dockerd shutdown) then also exits 137 and the epoch defers, which fails closed.
  - **Condition 5:** the runtime gate (a declared runtime with a runc handler, no `runtimeClassName`) comes from an operator-declared chart value, with no cluster-scoped node `get`: the owner chose this over a ClusterRole (2026-10-24), keeping the chart's "never a ClusterRole" rule. Without the value, reclaim never runs. M0 read containerd only; CRI-O (the runtime of the maintainers' own cluster) is unread and must be read before it can be declared.
- **Cadence.** Epochs come from worker-pin rollouts (a pin change on 46 of the 59 days of public history) and, with the DinD-PVC binding rule under Q1, from DinD maintenance recycles. The evidence above is needed to make those count. On-demand epochs need a new server-fenced "recycle pod, keep PVCs" operation (Q7). That is optional for a first slice but likely needed for workers that fill between releases.

**Residuals** carried into the implementation PRD:
- **No evidence, no reclaim.** Force delete, node loss, podgc and out-of-band pods are ineligible.
- **Dead-shim runtime fault.** Closed on the paths read by refusing exit 137 and 255 (condition 4). Any other synthetic exit path found in a runtime not read here defers or blocks the gate for that runtime.
- **OOM kills waste an epoch.** *Inferred:* the cgroup v2 OOM count is hierarchical, so an earlier OOM kill inside DinD probably relabels a genuine 137 exit as `OOMKilled`, which defers. That fails closed but costs an epoch.
- **Ordinary deletes waste an epoch.** A graceful delete, eviction or preemption consumes the one-shot without evidence.
- **Rootless stale pid files are untested.** In the pod-lifetime `emptyDir`, a stale pid file could stop the daemon from starting after a sidecar restart. That fails closed, and it is not exercised by the harness.
- **Kernel pid-namespace teardown** is read in Linux v6.18 source and corroborated by `exp-termination`; target node kernels were not checked. **CRI-O and other runtime classes were not read**, and the evidence predicate must defer on them.
- **The PRD's accepted residuals remain:** deliberate escapes such as the containerd socket, privileged containers with shared propagation, `--pid=host`, and rootful device reach.

The go is conditional on conditions 1, 2, 4 and 5 being in the implementation PRD's scope. If the owner judges condition 1 (new controller watch or RBAC) unacceptable, the answer becomes no-go, with the gap named as "no positive termination evidence", and the item-2 mediation design is the fallback.

### Implementation PRD outline (on go)

1. **Controller evidence field.**
   - Observe worker pods with a watch, or with a sub-second poll while a pod is terminating. This is new RBAC: `watch` on pods.
   - Accept only the Q1 signal: terminal phase, matching container counts, and every entry `state.terminated`.
   - Accept it only when the operator-declared chart value names a read runtime with a runc default handler and the pod has no `runtimeClassName`; without the value, defer. Any other runtime defers, including the chart's documented sysbox path (`runtimeClassName: sysbox-runc`) (Q1 runtime link).
   - Accept only runtime-reported terminations, as an allowlist: reason `Completed` or `Error`, the two reasons containerd's CRI gives a runtime-reported exit with no other reason (`internal/cri/server/container_status.go:toCRIContainerStatus`, `helpers.go`); a non-empty `state.terminated.containerID`; and an exit code other than 255 and 137. Anything else defers.
   - Make `dind-meter` exit promptly on SIGTERM (condition 4), so a graceful termination ends with no 137.
   - Record a bounded evidence document containing:
     - the predecessor pod UID;
     - per-container `containerID` and `restartCount`;
     - data and DinD-data PVC `metadata.uid` plus `spec.volumeName`;
     - the observation time and controller generation.
   - Persist it as an annotation on the data PVC *object*, with the UID- and resourceVersion-guarded patch pattern of `mirrorRoll`. Then bind it to the successor UID once the successor is listed (same worker label and claims, current ReplicaSet).
   - Deliver it to the worker through a downward-API projection or a pre-registration API read. The PRD chooses one.
   - Defer on everything listed under Q1 "Binding".
2. **Worker one-shot.**
   - A marker *file* keyed by `UZI_POD_UID` on the data volume's filesystem, outside `/data/runner` and not in `dind-sock`, written and fsynced before the first reclaim action.
   - It is never reopened by a process restart, an interrupted reclaim or any uncertainty.
   - The reclaim sits in `main.ts` after `startupReapVerdict` and before `worker.run()`.
3. **Eligibility predicate.** Reclaim proceeds only when every one of these holds; anything else defers:
   - evidence matches `UZI_POD_UID` and the current PVC identities;
   - the daemon served its first API response;
   - no container is running, restarting or paused;
   - every container's restart policy is `no`;
   - swarm is inactive, no plugins are enabled and live-restore is off;
   - every volume uses the `local` driver with empty `Options`;
   - an `/events` stream is held from before the inventory, as an extra abort-and-defer trigger only, and confirmed live with `since` or a first event, because the daemon flushes `200` before it registers the subscription (Q2 source);
   - each tree is settled under #3329's authorization.

   For each eligible tree, delete in place with the existing retire and delete helpers. Stopped containers are left alone.
4. **ADR amendments.**
   - ADR-1783: pod-epoch exclusion as a writer-exclusion proof for settled attempt trees on Docker-wired hosted workers. Compose and unwired lanes stay deferred.
   - ADR-3153: an epoch-scoped exception to "automatic retention deletion stays off".
   - Update `specs/human.md:1306`, which says "#1860 separate daemon mediation", and `:1452`.
5. **#3329 trigger amendment.** Replace the idle periodic and admission-refusal triggers with the text proposed under Q7. Optionally add the fenced pod-recycle request.
6. **Test seams.**
   - Controller unit tests over pod-status fixtures: graceful, Recreate, eviction, force delete, podgc, a sidecar `waiting`, and a relist.
   - Worker tests for the one-shot, ordering before `worker.run()`, and every deferral.
   - A fake-daemon unit layer for predicate parsing.
   - The real-daemon harness `e2e/pod-epoch/` extended with a reclaim run: deletion under a held stream.
   - #1858's pending-create control (`agent/test/clone-attempt-paths.test.ts`) as the RED/GREEN control.
