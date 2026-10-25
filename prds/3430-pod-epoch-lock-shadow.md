# PRD #3430: Pod-epoch lock and shadow reclaim verdicts on Docker-wired workers

**Status**: Draft (after two architecture reviews, a final pass and lead/buddy review). PRD A of two for #3329's disposal with the writer-exclusion proof PRD #1860 M0 recommended (`prds/1860-pod-epoch-writer-exclusion.md`). This PRD deletes **nothing**: it ships the epoch lock, the controller handoff and a shadow verdict. PRD B (#3431) adds deletion once this PRD's shadow acceptance passes. **Blocked by** #3421 (`dind-meter` SIGTERM). The CRI-O source audit is committed beside it in `prds/3430-pod-epoch-crio-audit.md`.

## Problem

On a Docker-wired hosted worker, attempt trees under `/data/runner` stay on the data volume after their runs end and their custody holds are discarded or released. Nothing deletes them automatically, because nothing proves that no Docker-side writer can still reach a tree (#1858, #1860). On busy workers they fill `/data` until runs park or fail on disk. #3329 recorded 10 settled trees, about 12.5 GiB, on one 25 GiB volume, plus recurrences on three more workers. Today an operator can discard eligible sources through the operator channel (#1786). Anything else must be preserved and the volume grown.

## Outcome

When a Docker-wired worker pod is replaced (a worker-pin roll, a template-drift rollout or a DinD maintenance recycle), the new pod logs one `pod_epoch_reclaim` verdict before it takes any run. The verdict says whether PRD B would be allowed to delete settled attempt trees at that epoch, and lists exactly which trees it would delete. Nothing is deleted.

The verdict is `eligible` only when all three of these hold:
1. The controller positively observed every container of the immediately preceding pod terminate, on a runtime stack the operator declared and this PRD audited.
2. No other pod used the volumes in between.
3. No other worker process holds the data volume's epoch lock.

Every Docker-wired worker takes part in the lock from this PRD on, so PRD B's deletion is excluded from the moment it ships.

**What the proof covers.** The immediate predecessor, any pod in between, and any concurrent worker process. It does **not** exclude every historical writer. The owner accepted one residual on 2026-10-24: a process of an older pod that survived its own termination (for example through a runtime dead-shim cleanup failure) and still writes into a settled tree after at least one later evidenced epoch. Races in the reclaim interval are **not** part of that residual; the epoch lock closes them.

Acceptance examples:
1. A worker holds three settled trees (run terminal, every hold discarded or released) and one tree with an open hold. A worker-pin roll replaces its pod gracefully on a declared runtime. The new pod logs `pod_epoch_reclaim` `eligible`, with a would-delete list naming exactly the three settled trees and their sizes. It deletes nothing, then claims.
2. The same worker's pod is force-deleted (grace 0). The controller patches a negative document. The new pod logs `deferred` (`no_predecessor_evidence`) without waiting, and claims as usual.
3. `workers.docker.podEpochReclaim.runtime` is unset. Every Docker pod logs `deferred` (`runtime_not_declared`) without waiting and claims as today. It still holds the shared epoch lock.

## Out of scope

- **Any deletion.** That is PRD B (#3431), together with the ADR amendments it requires.
- An on-demand "recycle pod, keep volumes" operation (M0 Q7). Parked for a later PRD.
- Reclaim triggered by idle time or disk pressure (#3329's original trigger). M0 Q7 replaces it, and those paths never delete attempt trees.
- Cross-worker reclaim of moved runs (#2823).
- Removing stopped Docker containers. The verdict pass sends the daemon only GET requests; container ownership is #3387's.
- Compose and non-Docker lanes. Runtime stacks other than the audited ones. Any `runtimeClassName`. Ephemeral containers.
- Storage without attach enforcement (CephFS, NFS and other shared filesystems).
- A node agent, cluster-scoped RBAC, or any threat-model relaxation beyond the owner-accepted residual.

## Modules and seams

### 1. Epoch lock: lifetime shared ownership, exclusive for reclaim

This module closes the reclaim interval. Both architecture reviews found the earlier startup-only lock unsound: T could take and release it, admit work, and then let S reclaim.

- **Lock file.** `/data/.uzi-epoch.lock` lives on the data-volume root, outside the `runner-shared` subPath, so DinD (which mounts only that subPath) cannot reach it.
  - `seed-nix` (root under uid-split) creates it if missing, `chown -h`s it to the worker uid and gid (10001:10001) and sets mode 0600, before it locks it. It is never unlinked or replaced. Like `runnerInitScript` (`render.go`), it refuses a symlink (`[ -L ]`) or non-regular file at the lock or record path; on refusal it skips the record, and the worker fails closed (operator repair, as in ADR-3153).
  - The worker never creates it. It opens it with `O_NOFOLLOW` and checks that it is a regular file the worker owns. A missing file, or any other state, makes the worker fail closed (exit non-zero, no admission), because an attacker-swapped or broken lock file on a supported volume cannot be told apart from a reclaimer's.
- **Participation and fail-open.** Locking is keyed on a render-time env var, `UZI_EPOCH_LOCK=1`, set only for `withDinD` pods. It is not keyed on runtime Docker wiring, which degrades to unwired when the daemon is slow (`main.ts:374-385`). Compose and other lanes never lock. Participation is skipped (fail-open) **only** for conditions identical for every pod that can overlap on the volume, which make it impossible for any pod to have taken EX:
  - the filesystem type fails the allowlist, checked by `statfs` before any lock call (the pods share the volume).

  Then no pod can reclaim, so skipping is safe. The worker image **ships** the required `flock` (util-linux), and the worker invokes it by a trusted absolute path. Every other failure fails **closed**: a missing or failing `flock`, a non-`EWOULDBLOCK` error on an allowlisted filesystem, or an unverified ownership or type check on the lock file. One process's capability result never implies volume-wide exclusion. The worker does not touch protected state and does not open admission; it exits non-zero and retries on restart. `seed-nix` skips its record update. Default-off workers may skip deletion, but never participation.
- **Every participating worker holds `LOCK_SH` for its whole process lifetime.** That includes a pod with the runtime value unset, a pod without evidence and a pod whose one-shot is already consumed. The lock is taken at the top of `main()`, before the first data-volume writers (`ensureRunnerTmpdir`, `main.ts:364`, under `/data/runner`; `outbox.init()`, `main.ts:508`) and before startup reaping (`inspectRetireScratchAtStartup`, `reclaimStrandedRunHomes`, `reapStaleWorkspaces`, `reapCodexOrphansAtStartup` and `startupReapVerdict`), before any other access to `/data/runner` or the bare repos, and before any Docker request or `worker.run()`. Startup reaping and reclaim both run under that ownership. The wait is bounded and the bound exceeds the pod grace period (proposed 120 s), so an ordinary overlap costs no restart. On timeout the worker exits non-zero without opening admission.
- **Candidate S takes a tentative `LOCK_EX|LOCK_NB` at the top of `main()` instead of SH.** S is a candidate when the runtime env var is set, the projection is mounted, the storage gate passes and the one-shot is unconsumed. `flock(2)` conversion is not atomic, and a failed non-blocking conversion drops the shared lock, so S chooses its first lock mode up front.
  - S holds EX through startup reaping, evidence validation and the bounded read-only verdict pass.
  - If EX is busy, S defers (`lock_busy`) and takes SH (bounded) with admission still closed.
  - If the evidence is absent, negative or invalid, S closes the descriptor and takes SH (bounded) with admission still closed.
  - Every other worker takes SH.
- **Under EX**, S re-reads the pod-entry record (module 2) and requires `current == S` and `previous == X`. It checks this before the predicate and again before each tree. Then it runs the predicate and the read-only verdict pass. Once that pass has settled, S closes EX and takes SH (bounded) with admission still closed, and only then opens admission. Settlement of destructive children under EX is PRD B's requirement.
- **Mechanism from Node.** Node has no `flock` API, and libuv opens descriptors close-on-exec. Node opens the file, then runs the image's `flock` utility on that descriptor passed in an extra stdio slot (`flock -s|-x [-n] 3`). The lock lands on Node's open file description and outlives the helper. A-M1 ships util-linux `flock` in the worker image (util-linux is already the source of `setpriv`) and calls it by absolute path. There is no wait option, so `-n` is polled within the bound.
- **Release semantics.** The lock is released only when the last descriptor on that open file description closes. This PRD runs no destructive work under EX, so the only holders are the worker process itself (and, under EX, its read-only verdict pass). Passing the descriptor to destructive child processes, and the definition of unsettled cleanup, belong to PRD B.
- **Storage gate.** The worker checks `statfs('/data').type` against an allowlist (ext4, xfs) and defers reclaim otherwise. Declaring the runtime value also attests that the data storage enforces attach exclusivity (the RWO Multi-Attach refusal) or is node-local. Only then do overlapping pods share one kernel's view of the filesystem.
- **How the cases come out.**
  - **X→S→T, S acquires first.** T's `seed-nix` finds the lock busy and skips the record update, leaving `current = S`. T's DinD can only restore containers, which the barrier covers. T's worker SH blocks while S holds EX, so T never admits work during S's pass (in PRD B, during deletion). Once S is SIGTERMed, its read-only pass stops, so T waits at most about the grace period.
  - **T acquires first.** T never releases SH, so S's EX attempt fails and S defers.
  - **T's DinD before T's lock.** T's DinD sees only `runner-shared` and the DinD volume. Without a client it can only restore containers, and restore starts only containers whose restart policy is not `no`. S's predicate already proved there are none, against a daemon restored from the same persisted root with no client since. Its only clients before the lock are the kubelet's `docker info` probe and the connect-only wiring probe, both read-only. A render test pins that no init container touches attempt trees.
- **Mixed versions.** Pods that overlap on one volume come from the same ReplicaSet, so they run the same image. Graceful deletes are replaced from the same ReplicaSet, and Recreate's `oldPodsRunning` stops overlap across templates. Shipping SH in the same image as deletion, in this PRD before PRD B enables deletion, is therefore enough. An old controller renders no projection, so the worker defers immediately (module 4).

### 2. Pod-entry record (volume binding; corroboration, never authority)

- The first init container (`seed-nix`, `controller/internal/kube/render.go:1195`, before `dind`) takes `flock -x -n` on the same lock file and updates `/data/.uzi-pod-entry` under it, so the read-modify-write is serialized.
- If the lock is busy, because an overlapping predecessor still holds SH or EX, `seed-nix` skips the update and continues without waiting. An overlap therefore adds no startup serialization. It costs at most the next epoch: the next pod sees `previous` different from its own predecessor, and defers. The pod's DinD stays covered by the restart-policy barrier, and its worker by SH.
- The record holds two slots, `current` and `previous` pod UID. It is replaced atomically (write to a temp file, fsync it, rename, fsync the directory).
- Re-entry with the same UID (an init-container restart) leaves the record unchanged.
- An unreadable, torn or schema-invalid record makes reclaim defer, and the record never supplies authority. The record file is mode 0644.
- The update runs only for Docker workers (`withDinD`). The non-Docker render stays unchanged, as `render.go:1191` requires. It uses a new downward-API env var `UZI_POD_UID` in that container.

### 3. Controller: a non-transferable predecessor-termination handoff

- **Observe.** The controller watches pods in the **Docker** worker namespace only. This adds the `watch` and `patch` verbs on pods to the docker Role in `deploy/chart/templates/worker-rbac.yaml`. The restricted and isolated Roles are unchanged, and none of these becomes a ClusterRole. The comment there ("`list` ONLY") and `TestObserveIssuesListOnlyOnPodsAndActuallyListsThem` are updated together. Today's 10 s `list` (`materializer.go`) is unchanged.
- **Session.** A session starts from a full list and then a watch from the list's resourceVersion. It must be synced (list plus watch established) before the materializer's first mutating tick after a controller start, so a roll the controller applies is observed. That wait is bounded (60 s); after the bound the controller proceeds. Epochs in the unsynced window get no handoff, so they defer: verdicts fail closed and rolls fail open. Events are handled serially in stream order; resourceVersions are opaque and never compared. A reconnect that resumes from the last delivered resourceVersion, and that the server accepts, continues the session. A `410 Gone`, a relist, a decode error or a controller restart ends it. Pods present in the initial list are never treated as newly added successors.
- **Signal for pod X.**
  - One pod UID with `status.phase ∈ {Failed, Succeeded}`.
  - Container and init-container status **names** equal the spec's name sets exactly, with nothing missing and nothing extra.
  - No ephemeral container is declared or reported.
  - Every entry is `state.terminated` with reason `Completed` or `Error`, a non-empty `containerID`, and an exit code that is ≥ 0 and not 137 or 255.

  M0's refusals are kept. None of these is evidence: a tombstone, a pod that disappeared without an observed terminal status, or podgc's `Failed` patch.
- **Pods that count.** These count as pods of that worker:
  - every pod with the worker's labels;
  - **any pod whose `spec.volumes` names the worker's data or DinD claim** (M0, Binding).
- **Handoff rules.** Within one session, all of these must hold:
  1. X's terminal status was observed satisfying the signal.
  2. S was then observed added: a new UID, owned by the worker Deployment's current ReplicaSet (Deployment → ReplicaSet → Pod ancestry verified), and mounting the same data claim name.
  3. Over X's lifetime (from X's addition, or from session start if X was in the initial list) up to S's addition, every other counted pod seen non-terminal showed the signal before X's terminal status. No counted pod was deleted without the signal in that window.
  4. X's deletion after its observed terminal status is fine.
  5. Identical duplicate events are idempotent.
  6. **DinD claim identity.** It is unchanged, except where the session recorded an ADR-1759 maintenance recycle for this worker (`maintenance.go`) that reminted the DinD claim. Then the old and new claim UIDs are bound to that operation. An unexplained DinD claim change voids the handoff.

  Any failed rule means no handoff, and S defers.
- **Negative document.** When S's addition fails the handoff, the controller patches S with a negative document (`{"handoff":"none","reason":…}`) through the same patch path, so S defers at once instead of waiting out the cap.
- **Deliver.** The handoff is one bounded JSON document. The controller writes it only onto S's pod as the annotation `uzi.dev/pod-epoch-evidence`, through a UID-preconditioned patch whose body carries only `metadata.uid`, `metadata.resourceVersion` and that annotation. A test asserts the body. There is no PVC persist step: the annotation on S is durable, and a controller restart before delivery simply makes S defer. The PVC carries `annotationRoll`, which `mirrorRoll` patches under resourceVersion guard, so adding a second writer there would block rolls.

  The document holds:
  - schema version and session id;
  - X's UID, with per-container `containerID`, exit code and `restartCount`;
  - S's UID;
  - data and DinD claim UIDs and `spec.volumeName`, and any recycle binding;
  - the declared runtime value;
  - times.

  The controller verifies S's mounted claim UIDs itself, from S's `spec.volumes` and the PVC list.
- **Projection.** S's render gains a `downwardAPI` volume, mounted directly (no `subPath`), with one item: `fieldRef: metadata.annotations['uzi.dev/pod-epoch-evidence']`. The worker re-opens the path on each read. An empty or missing file means "pending".
- **Declaration.** The runtime value is rendered as an env var into the Docker pod template, so changing it rolls the workers through Recreate. That roll is itself an evidenced epoch. The controller adds nothing to the document unless the value is set.
- **Bounds.** The document is controller-built and never taken from worker or user input. It is at most 8 KiB, and at most one patch is made per epoch.
- **Tests.** Controller unit tests drive event sequences through the session handler:
  - graceful Recreate; X deleted after its terminal status;
  - eviction; force delete; podgc;
  - a sidecar in `waiting`; a status-name mismatch; an ephemeral container;
  - exit codes 137, 255 and -1; `ContainerStatusUnknown`;
  - reconnect with resume; `410` and relist;
  - a controller restart before delivery; duplicate events;
  - A→B→C with B unobserved; A→B→C with B unevidenced;
  - a non-terminal W next to X; a foreign pod naming the claim;
  - a stale replay to a later pod;
  - a DinD recycle with and without a recorded operation;
  - the patch body containing only the three fields.

### 4. Worker: once-per-pod shadow verdict before claim admission opens

- **Where.** In `agent/src/main.ts`, under the module 1 lock: lock first, then the existing startup reaping and `startupReapVerdict`, then the verdict pass, then `worker.run()`. The ordering test asserts this sequence. It also asserts that no data-volume write precedes the lock, and that the only reads before it are `statfs`, the one-shot marker and the lock file itself.
- **One-shot, consumed on every path.** `/data/.uzi-epoch-consumed` is one atomically replaced file holding the consuming pod UID: write a `mktemp`-named temp file, fsync it, rename it, fsync the directory. The first worker process of a pod writes it **before admission opens, on every path**, including every deferral. A present marker carrying this pod's UID means this pod never evaluates again. So "defer, run work, OOM restart, late evidence" cannot produce a verdict, or in PRD B a deletion, after the pod issued Docker requests. An uncertain marker state refuses.
- **Immediate deferral.** The worker defers without waiting when the runtime env var or the projection mount is absent (`runtime_not_declared`, `no_projection`), when the storage gate fails, or when the document is negative.
- **Evidence read.** Otherwise the worker re-reads the projected path until a cap (proposed 120 s; the shadow acceptance measures it). Empty means pending. Late arrival after a deferral is ignored. The worker accepts a document only when all of these hold:
  - its schema is valid;
  - S equals `UZI_POD_UID`;
  - the runtime value equals the rendered env;
  - module 2's record shows `current == S` and `previous == X`. This is the worker-side volume binding, independent of the controller.
- **Daemon predicate.** Only GET requests are sent: `/_ping`, `/info`, `/containers/json?all=1`, `/containers/{id}/json`, `/volumes`, `/plugins` and `/events`.
  - Conditions:
    - the daemon served its first response;
    - no container is running, restarting or paused;
    - every container's restart policy is `no`;
    - swarm is inactive, no plugin is enabled, and live-restore is off;
    - every volume is `local` with empty `Options`;
    - an `/events` stream is held from before the inventory and confirmed live, and any event aborts the pass.
  - Bounds:
    - at most 500 containers, 500 volumes and 50 plugins (defer above);
    - inspect concurrency is 4;
    - a truncated or unparseable inventory defers;
    - events are buffered only up to the first event, which aborts the pass.
  - A test-only fake daemon records the methods it receives; the test asserts the pass sends only GET requests.
- **Read-only authority evaluation** (the would-delete list). For each attempt tree, the worker evaluates #3329's authority **without deleting**:
  - holds through `listRecoveryHolds` (as `ops/retained-source-server.ts` `readHolds` does);
  - lifecycle from the ownership probe (`GET /worker/runs/{id}/ownership`); a tree is listed only if the run is terminal;
  - the retained-source disposition through the existing inspection path (`inspectRetainedSources`, which uses `evaluateRetained`, `agent/src/git.ts:8309`), which must be `local_discard` or `local_protected_discard`;
  - the tree's size.

  Hold currency rests on "read before admission ever opened in this pod, under EX". T shares the worker id, so T's claims could add holds, and EX excludes T.
- **Aggregate bounds** (proposed; the shadow acceptance measures them):
  - at most 200 trees per epoch;
  - total wall time at most 10 min, covering discovery, hold and ownership reads, inspection and size measurement;
  - the remaining deadline is passed into each operation, not checked only between trees.

  On SIGTERM or deadline, the pass stops and the verdict is `partial`.
- **Verdict.** One `pod_epoch_reclaim` log line carrying:
  - `eligible`, `deferred` or `partial`, with a reason code;
  - counts of trees examined, would-delete and kept;
  - the would-delete list (bounded: ids plus sizes; an overflow count past 200);
  - bytes, marked `measured`, `partial` or `unknown`;
  - the evidence-wait and pass durations.
- **Untrusted inputs.** Daemon responses have bounded bodies (as `dockerRequest`, `run-quiescence.ts:2038`) and are bounded by the counts above. The projected file is at most 8 KiB under a strict schema.

### 5. Runtime gate (chart value)

`workers.docker.podEpochReclaim.runtime`. Unset (the default) disables reclaim. Accepted values are exactly the audited stacks:

- `containerd-runc` (M0): containerd 2.3.4, runc 1.5.1, kubelet 1.37.1 read.
- `crio-crun` (CRI-O audit):
  - CRI-O **1.36.2** with `runtime_type` `oci` (classic conmon) and crun as the default handler;
  - **crun 1.30.1** (namespace creation and state output were read only at that tag);
  - conmon v2.1.10 to v2.2.1;
  - kubelet 1.35.3 read;
  - no namespace-adjusting NRI plugin (an unsupported configuration).

Declaring a value attests that every node running worker pods uses that stack within those bounds, with attach-enforced or node-local data storage. A version outside the bounds needs a new audit, not a declaration. A pod with any `runtimeClassName` defers.

### CRI-O + crun audit: summary

The full audit, with READ / INFERRED / NOT READ markings and source references, is `prds/3430-pod-epoch-crio-audit.md`.

- **Private pid namespace (READ).** Kubelet `PidNamespaceForPod` returns `CONTAINER`. CRI-O uses the runtime-tools `generate.New` defaults, which include a pathless `pid` namespace; `SpecAddNamespaces` maps only NODE, POD and TARGET. crun unshares a pathless namespace.
- **A non-negative exit code comes only from conmon's exit file (READ).** conmon writes ≥ 0 only after `waitpid` returns init's pid, or (v2.2.x) after a kill probe gets `ESRCH`. The kernel's `exit_notify` runs `zap_pid_ns_processes` before init becomes a zombie.
- **Synthetic codes.**
  - A missing exit file gives -1.
  - A `crun state` error with no file gives 255.
  - A dead conmon later surfaces as -1 / `Error` with a `containerID`. Hence exit codes `< 0` are refused.
- **Fail-closed cases.** `crun state` can report a live init as stopped on a procfs `ENOMEM`, under `hidepid`, or when the freeze file is missing. conmon is still waiting in all three, so the exit code is -1 and refused.
- **Verified live on one maintainer cluster (2026-10-24):** nodes report `cri-o://1.36.2`, `default_runtime = "crun"`, `runtime_type` unset, kubelet `v1.35.3`.
- **Not verified:**
  - installed conmon and crun packages, `/proc` options, NRI plugins, the storage class's attach enforcement;
  - the audit's inference that conmon's `container_pid` probe runs in the host pid namespace;
  - the el10 kernel backport.

  These are acceptance prerequisites, not residuals.

## Risks

- **Startup latency.** The verdict pass runs before registration, so a roll's canary waits up to 120 s plus the pass longer to count as usable (`roll_pacing.go:593/771`, which needs Registered, PodUID and a heartbeat). The shadow acceptance measures it.
- **Restart-loop flag.** Fail-closed exits count toward `stuckRestartThreshold = 3` (`rollhealth.go:106`). Repeated lock timeouts would mark the worker stuck, which is visible and fails closed.
- **Wedged holder.** A wedged holder of the lock delays a successor indefinitely through bounded retries.
- **File overlap.** Overlaps with #3103 (`roll_pacing.go`, `materializer.go`) and #3153 (`render.go`). Sequence against them.
- **Controller release roll.** This PRD's controller release rolls every Docker worker once (capacity at zero for a few minutes, #1470).
- **DinD sharing.** Two dockerd processes sharing one DinD root during an overlap is a hazard that exists today, and this PRD does not change it.

## Testing decisions

- **Unit tests** cover the seams above, including Linux two-process flock tests in CI. These must fail, not skip, on Linux:
  - S first (T's worker SH waits until S's process is gone);
  - T first;
  - a swapped lock file caught by the record tripwire;
  - the default-off path still holds SH;
  - a storage type outside the allowlist.
- **Shared fixture.** A contract fixture under `fixtures/` is read by the Go controller test and the agent test (the cross-module `fixtures/api-contract/` pattern).
- **No KinD lane.** The KinD smoke runs no controller and no workers, its local-path volumes would fail the storage gate, and extending it means a workflow edit a uzi run cannot push. In-run validation is the controller event-fixture tests and the worker two-process tests. Watch capture, projection latency and real `eligible` verdicts move to a live shadow acceptance, which is safe because this PRD deletes nothing. `e2e/pod-epoch/` keeps covering daemon semantics only. A KinD hosted-worker smoke, if wanted, is a separate maintainer issue.
- **Pending-create regression.** The #1858 pending-create control (`agent/test/clone-attempt-paths.test.ts`, which has an open flake, #2578) keeps its sentinel-preservation assertion.

## Milestones (one run, one PR)

- **M1: Lifetime epoch lock, pod-entry record and one-shot.** Modules 1 and 2 and the one-shot. Contents:
  - `UZI_EPOCH_LOCK`, the tentative EX and SH;
  - util-linux `flock` shipped in the worker image and invoked by absolute path;
  - lock-file ownership and symlink safety;
  - the non-blocking init record;
  - the consumed marker written before admission on every path;
  - the storage gate and the fail-closed boundary;
  - lock, render and ordering tests.

  The tentative EX path is not taken yet, because no evidence is read. Behaviour change: an overlapping successor's worker waits only while a predecessor holds EX, and in M1 nothing holds EX, so ordinary overlap is unchanged; `seed-nix` never waits. A persistent lock error on a supported volume now stops that worker's starts (owner decision 2026-10-24). Blocked by: none.
- **M2: Controller handoff and shadow verdict.** Module 3 (the negative document and the bounded startup barrier), module 4's evidence read, daemon predicate and read-only authority evaluation under tentative EX, and module 5. Also:
  - the `pod_epoch_reclaim` verdict;
  - controller event-fixture tests;
  - docs on the runtime value and what declaring it attests.

  Blocked by: M1.

### Delivery, acceptance and closure

The PR references #3329 and #1860 and closes neither.

A linked `acceptance` issue, owned by the user, covers:
- **Before declaring:** node packages, `/proc` options, NRI plugins, storage attach enforcement, the conmon `container_pid` namespace check, and the image's `flock`.
- **In shadow, over several graceful rolls:**
  - handoff capture rate;
  - projection latency;
  - pass time;
  - verdicts and would-delete lists, checked by hand against the trees and holds.

  This acceptance unblocks PRD B (#3431).

## Decision Log

| Date | Decision | Why | Rejected |
|---|---|---|---|
| 2026-10-24 | Implement #3329's disposal at pod epochs only, in two PRDs: this one (lock plus shadow verdicts) and PRD B (deletion, blocked by this PRD's shadow acceptance and by #2852 C, which #3329's dispatch gate names). | Owner-approved split. The caps can only be measured live; #2852 C gates only deletion; shadow evidence is independently valuable. | One PRD and one PR for all of it. |
| 2026-10-24 | Immediate predecessor + intervening pods + concurrent workers proved; older-pod survivor accepted as a residual. | Owner decision. It relieves today's inventory at the next evidenced epoch. The ADR states the limit. | Strict proven history from a trusted genesis (buddy's preference): it reclaims nothing on existing workers. |
| 2026-10-24 | A readers-writer `flock` on the data volume: SH for every worker's lifetime, EX non-blocking for reclaim, record re-check under EX. | Both architecture reviews: a startup-only lock fails when T acquires first. This is the smallest mechanism that also covers S's helpers and default-off workers. | A startup-only lock; ReadWriteOncePod (needs new claims and a data migration for existing workers); EX for every worker's lifetime, or a blocking init EX (both serialize every overlap). |
| 2026-10-24 | Consume the one-shot before admission on every path. | Otherwise defer, then run, then restart, then late evidence would reclaim after Docker use. | Consuming it only before the first reclaim action. |
| 2026-10-24 | Handoff on S's pod annotation only; no PVC persist. | A second PVC writer conflicts with `mirrorRoll`'s resourceVersion-guarded `annotationRoll`; the pod annotation is durable enough. | Persisting on the PVC. |
| 2026-10-24 | A single-session watch, stream-ordered, counting claim-mounting pods, with ReplicaSet ancestry and a DinD-recycle binding. | Closes A→B→C, stale replay and foreign pods; M0's Binding rules. | Counting by labels only; comparing resourceVersions. |
| 2026-10-24 | The runtime declaration is a pod-template env var; defer immediately when it is absent. | Gives the worker an input the evidence document does not supply, and means default-off costs no wait. | Comparing the document only against itself. |
| 2026-10-24 | crun 1.30.1 only; storage restricted to ext4/xfs with attach enforcement. | Namespace creation was read only at 1.30.1. A lock fences nothing on shared filesystems. | Accepting the range 1.26-1.30.1; accepting any RWO claim. |
| 2026-10-24 | Refuse exit codes below 0, 137 and 255; exact status-name sets; defer on ephemeral containers; fix `dind-meter` first (#3421). | M0's dead-shim follow-up; the CRI-O dead-conmon -1 case; a count alone can hide a missing entry. | A node agent; a threat-model amendment. |
| 2026-10-24 | Support `crio-crun` beside `containerd-runc`. | Source audit, and the maintainers' cluster runs CRI-O 1.36.2 with crun. | containerd only. |
| 2026-10-24 | Stopped containers are not removed. | M0 Q3; #3387. | Removing bound containers. |
| 2026-10-24 | Skip participation only for a filesystem type outside the allowlist, which every pod sharing the volume sees the same way. A missing or failing `flock` and any other lock failure fail closed; the image ships `flock`. `seed-nix` locks non-blocking and skips the record when busy. | A local "reclaim disabled" cannot stop a reclaimer that is already active, so failing open is safe only when no pod could have taken EX. A blocking init lock would serialize every overlap. | Failing open on any lock error (buddy BLOCK: T admits while S deletes); failing closed on unsupported storage (breaks today's starts). |
| 2026-10-24 | Non-closing PR; #3329 and #1860 close after PRD B's acceptance. | Real runtimes and attested versions can only be verified live. | Auto-closing. |
