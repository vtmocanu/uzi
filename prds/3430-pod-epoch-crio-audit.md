# CRI-O 1.36.2 + crun + conmon: termination-evidence audit for PRD #3430

Supporting evidence for `prds/3430-pod-epoch-lock-shadow.md` (runtime gate value `crio-crun`) and a follow-up to PRD #1860 M0 Q1. Read 2026-10-24 by a source-reading agent; reviewed by the lead and buddy. Claims carry READ / INFERRED / NOT READ markings.


Extended 2026-10-29 with a source audit of crun 1.28 and its CentOS Stream 10 source RPM.

Read-only source audit. Markings: **READ** = read in source at the pinned tag; **INFERRED** = reasoned from read code or general kernel/libc knowledge, not traced line by line; **NOT READ** = not examined.

## Scope and pinned sources

| Component | Tag / ref read | Notes |
|---|---|---|
| CRI-O | `v1.36.2` (commit `7ec673a2519ec5531073b9e9acc63cc5399c664a`) | incl. vendored `github.com/opencontainers/runtime-tools v0.9.1-0.20251205004911-5e639034dcdc` and `github.com/containerd/nri` |
| conmon | `v2.2.1` primary; function-level comparison `v2.1.0`, `v2.1.8`, `v2.1.10`, `v2.1.12`, `v2.1.13`, `v2.2.0` | |
| crun | `1.30.1` primary; `1.28` namespace/state-output re-read; function-level comparison `1.26`, `1.27`, `1.28`, `1.29.1` | 1.28 commit `54f16ffbefcd022bf032af768b5c5ce075c18bfc`; 1.30.1 commit `079ff6a7a16d029460af882f9ea44674e6a510b6` |
| kubelet | `k8s.io/kubernetes v1.35.3` (`pkg/kubelet`, `pkg/features`) | |
| Linux | upstream `v6.12`: `kernel/exit.c`, `kernel/pid_namespace.c`, `fs/proc/base.c`, `fs/proc/array.c`, `fs/seq_file.c` | the el10 `6.12.0-*` backport tree was NOT READ |
| CentOS Stream 10 dist-git | `gitlab.com/redhat/centos-stream/rpms/crun` branch `c10s` (head `5471d4c`, 2026-09-25); `.../rpms/conmon` branch `c10s` (head `37f945c`, 2026-05-29) | |

**Versions on a target cluster's nodes: not determined.**
- CentOS Stream 10 AppStream currently carries `conmon-2.2.1-{1..4}.el10` and `crun-1.30.1-1.el10` (older `crun-1.26`…`1.29.1` also listed). READ from the `mirror.stream.centos.org/10-stream/AppStream/x86_64/os/Packages/` listing.
- Which RPMs the OKD 4.22 SCOS node image actually pins is NOT determined. OKD may also layer packages from its own repos.
- Check on a node with `oc debug node/<n> -- chroot /host rpm -q cri-o conmon crun`, plus `findmnt -no OPTIONS /proc` (for item 3).
- crun 1.30.1 is dated 2026-09-25, later than mid-2026. It was chosen because it is what CS10 ships.

**Runtime type** (READ):
- `pkg/config/config_linux.go`: `DefaultRuntimeType = "oci"`.
- `pkg/config/config.go` (around 1689 and 2066) treats `runtime_type = ""` as `"oci"`.
- `internal/oci/oci.go:newRuntimeImpl` maps `"vm"` to runtimeVM, `"pod"` to conmon-rs (`runtimePod`), and anything else to `newRuntimeOCI`, which is classic conmon.
- So with `runtime_type` unset, conmon-rs is not used. conmon-rs and runtimeVM are NOT READ.

---

## Q1. Normal exit path

**Conclusion:**
1. conmon is the init's subreaper parent. It `waitpid`s init and writes the exit code to `<persist-dir>/exit` and `<exits_dir>/<id>`.
2. CRI-O learns of the exit through fsnotify on `exits_dir`. It then runs `crun state`, and only on "stopped" (or a `crun state` error) does it set Stopped. It reads the code from the persist-dir exit file, and `ContainerStatus` reports `CONTAINER_EXITED`.
3. crun does no kill-all for a private pid namespace; it relies on the kernel.
4. The kernel empties the pid namespace before init even becomes a zombie.

**conmon v2.2.1** (READ):
- `src/conmon.c:main` calls `set_subreaper(true)` ("so we can wait for container process and return its exit code").
- `src/ctr_exit.c:check_child_processes` runs `waitpid(-1, &status, WNOHANG)`. It is called on SIGCHLD through signalfd (`on_signalfd_cb`) and dispatches `container_exit_cb` for `container_pid`.
- `container_exit_cb` sets `container_status` and quits the main loop.
- Back in `main`, `exit_status = get_exit_status(container_status)` gives `WEXITSTATUS`, or `128+WTERMSIG`, else -1.
- `main` then writes `opt_persist_path/exit` and `opt_exit_dir/<cid>` with `g_file_set_contents`.
- SIGTERM/SIGINT/SIGQUIT to conmon are forwarded to the container (`on_sig_exit`); conmon does not exit on them.
- `WNOHANG` without `WUNTRACED`/`WCONTINUED` only returns terminated children (INFERRED from waitpid(2) semantics).

**CRI-O** (READ):
1. `server/server.go:StartExitMonitor/monitorExits/handleExit` reacts to fsnotify Create.
2. `postStopCleanup` (`server/container_stop.go`) calls `ContainerStateToDisk` (`internal/lib/container_server.go:640`), which calls `runtimeOCI.UpdateContainerStatus` (`internal/oci/runtime_oci.go:1167`).
3. That function runs `crun state`:
   - If the status is not stopped, it copies the running state and returns.
   - If stopped, it runs `waitForExitFile` (`:1286`, backoff 500 ms ×1.2 ×6 steps, about 5 s), then `updateContainerStatusFromExitFile` (`:1138`). The file read is `c.dir/exit`, the persist-dir file.
4. `server/container_status.go:ContainerStatus`:
   - Stopped maps to `CONTAINER_EXITED`. A nil `ExitCode` is reported as **-1**.
   - Reason: `OOMKilled` if the `oom` file was seen, `seccomp killed`, `Completed` if exit is 0, else `Error`.

**kubelet** (READ):
- `kuberuntime_container.go:toKubeContainerStatus` copies Reason, Message, ExitCode and FinishedAt unchanged.
- `kubelet_pods.go:convertToAPIContainerStatuses` sets `state.terminated` with the containerID.

**crun 1.30.1** (READ):
- `src/libcrun/container.c:container_delete_internal(force)` sends SIGKILL to init only when `has_new_pid_namespace` ("it is enough to kill the first process"). `libcrun_cgroup_killall` runs only without a private pid namespace.
- `libcrun_container_kill` signals init only (`libcrun_kill_linux`).
- CRI-O sends SIGKILL to all processes (`signalContainer(..., all=true)`) only for `nodeLevelPIDNamespace()` (`runtime_oci.go:UpdateContainerStatus`).

**Kernel v6.12** (READ):
- `kernel/exit.c:exit_notify` calls `forget_original_parent`, then `find_child_reaper`. When the exiting task is the namespace's `child_reaper` and no other thread is alive, `find_child_reaper` calls `zap_pid_ns_processes(pid_ns)`. Only *after* that does `exit_notify` set `tsk->exit_state = EXIT_ZOMBIE`.
- `kernel/pid_namespace.c:zap_pid_ns_processes` disables pid allocation, sends SIGKILL to every pid in the namespace's idr (descendant-namespace tasks have a pid there too), reaps, then sleeps until `pid_allocated == init_pids`.
- **So once init is a zombie or gone, every other process of that pid namespace and its descendants is gone.** This does not depend on conmon having reaped it.
- That the el10 backport keeps this is INFERRED.

## Q1-follow-up (item 1). Private pid namespace for pods without hostPID or shareProcessNamespace: proven

**Chain** (READ unless marked):
1. **kubelet v1.35.3** `pkg/kubelet/kuberuntime/util/util.go:PidNamespaceForPod` returns `NODE` if `HostPID`, `POD` if `ShareProcessNamespace` is true, else **`NamespaceMode_CONTAINER`**. `NamespacesForPod` feeds that into `NamespaceOption.Pid`.
2. **CRI-O** `server/container_create.go:479` calls `container.New()`, i.e. `internal/factory/container/container.go:New`, which calls `generate.New(runtime.GOOS)`.
3. **Vendored runtime-tools** `vendor/github.com/opencontainers/runtime-tools/generate/generate.go:New`: under `switch os { case "linux": ... }` (line 99), `config.Linux.Namespaces` defaults to `{Type:"pid"}, {Type:"network"}, {Type:"ipc"}, {Type:"uts"}, {Type:"mount"}`, with **no `Path`** (lines 229-245).
4. **CRI-O** `server/container_create.go:784` calls `ctr.SpecAddNamespaces(...)`, which is `internal/factory/container/namespaces_linux.go:SpecAddNamespaces`:
   - `ConfigureGeneratorGivenNamespacePaths(sb.NamespacePaths(), ...)` only maps `IPCNS`, `NETNS`, `UTSNS`, `USERNS` (its `typeToSpec` map). `internal/lib/sandbox/namespaces.go:NamespacePaths` likewise returns only ipc, net, uts and user. The pid entry is untouched.
   - `switch sc.GetNamespaceOptions().GetPid()` has cases only for `NODE` (remove the pid ns), `POD` (set the infra's pidns path) and `TARGET` (set the target container's pidns path). **`CONTAINER` falls through, so the default pathless `pid` entry stays.**
   - No other non-vendor, non-test CRI-O code writes the pid namespace (`rg PIDNamespace` hits only these lines, `sandbox_run_linux.go:957` for the infra, and the `nodeLevelPIDNamespace` reader).
5. **crun 1.28 and 1.30.1** `src/libcrun/linux.c`: `configure_init_status` (1.28:4194; 1.30.1:5482) adds a namespace with `path == NULL` to `namespaces_to_unshare` ("Unsharing namespace"). The ordinary no-user-namespace path creates `CLONE_NEWPID` in the first clone. If user/time namespace ordering requires a later fork, `libcrun_run_linux_container` (1.28:5114; 1.30.1:6447) retains `CLONE_NEWPID` for unshare and sets `must_fork`; `init_container` (1.28:4854; 1.30.1:6177) forks into that namespace. In either path, init is PID 1 of a new pid namespace. `has_new_pid_namespace` (`container.c`, 1.28:1818; 1.30.1:1532) uses the same `path == NULL` test.

**Residual: an NRI plugin.**
- NRI is enabled by default in CRI-O 1.36.2 (`internal/config/nri/nri.go:New`, `Enabled: true`).
- `server/nri-api.go` applies plugin adjustments through `nrigen.SpecGenerator(...).Adjust(adjust)`. The vendored `containerd/nri/pkg/runtime-tools/generate/generate.go:AdjustNamespaces` "adds or replaces namespaces in the OCI Spec" (READ).
- So an installed NRI plugin could replace the pid namespace. That needs an operator-installed node plugin, which falls under the PRD's out-of-band/operator residual.
- Whether any NRI plugin is installed on a target cluster's nodes is NOT READ; check `/opt/nri/plugins` and NRI registrations in the crio log.

**Ephemeral containers** (INFERRED from the namespaces code above):
- With `--target` they join the target's pid namespace (`TARGET`), so the target init's death kills them.
- Without a target they get their own pid namespace and are **not** covered by `containerStatuses`/`initContainerStatuses`. The evidence should also require `ephemeralContainerStatuses` to be empty or all terminated under the same allowlist.

---

## Q2. Every CRI-O path that sets Stopped or an exit code

Exhaustive search of non-vendor, non-test CRI-O for `Status = ContainerStateStopped` and `ExitCode =` (READ). Only three places set Stopped: `runtime_oci.go:941`, `:1194` and `:1734`. Only these write `ExitCode`: `:1161` (from the exit file), `:1204`/`:1208` (255), `:1735` (0, checkpoint), `:1802` (nil, restore), and `runtime_vm.go:878` (VM runtime, not used).

| # | Path | Stopped set by | Exit code / CRI reason | Proves init exited? |
|---|---|---|---|---|
| A | `StopContainer` with `c.Spoofed()` (`runtime_oci.go:936`) | directly | none | n/a: spoofed infra only (`drop_infra_ctr=true`), not a pod container status (READ) |
| B | `UpdateContainerStatus`: `crun state` **errors** (`:1194`) | directly | from the exit file if present after about 5 s, else **255** (`:1204`, `:1208`); reason Error/Completed | only via exit-file provenance (see Joint guarantee) |
| C | `UpdateContainerStatus`: `crun state` == "stopped", exit file present | copies `crun state` | file value | yes (see Joint guarantee) |
| D | `UpdateContainerStatus`: `crun state` == "stopped", no exit file after about 5 s | copies `crun state`; `ExitCode` stays nil | **-1, Error** (`container_status.go`, nil maps to -1) | no proof by itself (see item 3) |
| E | `CheckpointContainer(leaveRunning=false)` (`:1734`) | directly | **0**, Completed | no reap. **Unreachable via CRI**: `server/container_checkpoint.go` passes `KeepRunning: true` (READ) |
| F | `StopContainer` / `StopLoopForContainer` (`:936`, `:974`) | never directly | n/a | Sends the stop signal via `crun kill`, then retries `crun kill KILL` with backoff until `Living()` fails. Status comes later via `postStopCleanup`, which goes to B/C/D. If the kubelet ctx expires, `WaitOnStopTimeout` returns, `crun state` still says running, and the status stays RUNNING (READ) |
| G | `StartContainer` failure (`server/container_start.go`) | n/a | n/a | `SetStartFailed` plus `removeContainerInPod`: the container leaves CRI-O, so the kubelet sees no runtime status (READ) |
| H | CRI-O restart (`internal/lib/container_server.go` `LoadContainer` / `LoadSandbox`) | via `FromDisk`, then `ContainerStateToDisk`, then `UpdateContainerStatus` | same as B/C/D; an `ExitCode` already persisted is kept (`UpdateContainerStatus` skips when `ExitCode != nil && !Finished.IsZero()`) | same as the path that originally set it |
| I | OOM: an `oom` file in the bundle | after the stopped path | reason `OOMKilled` | rejected by the allowlist |
| J | Dead conmon (Q3) | ends in D | -1, Error | see Q3 |

**Exit-file writers** (READ):
- Only the create-time conmon writes `c.dir/exit` or `exits_dir/<id>`. It gets `--exit-dir` and `--persist-dir` in `runtime_oci.go:CreateContainer` (around 148-154).
- ExecSync conmon gets neither flag (`ExecSyncContainer` arguments). Streaming exec runs the runtime directly (`ExecContainer`, `exec.CommandContext(runtimePath, ...)`).
- CRI-O never writes the exit file for the OCI runtime. `runtime_vm.go:351` writes one for the VM runtime only.

**conmon exit-file values** (READ, v2.2.1):

| Value | Origin |
|---|---|
| `WEXITSTATUS` or `128+sig` | `container_exit_cb` after `waitpid` returned `container_pid` |
| **0** | v2.2.x only: `check_child_processes`, ECHILD case, when `kill(container_pid,0)` gives `ESRCH` ("We can't get the real exit status") |
| **-1** | ECHILD while `container_pid <= 0`, or `kill` failing with an errno other than ESRCH. The main loop quits with `container_status == -1`, and `get_exit_status(-1)` is -1 (the glibc `WIFEXITED`/`WIFSIGNALED` decode of -1 is INFERRED). Also `timed_out` (`TimedOut`), but CRI-O passes no `-T` for create (`CreateContainer` arguments, READ) |

---

## Q2-follow-up (item 2). Joint guarantee for EXITED with exit code >= 0 and reason Completed/Error

**Key fact** (READ):
- On the OCI path, a non-negative `ExitCode` for a pod container can come only from `updateContainerStatusFromExitFile` (route E is unreachable via CRI, and 255 is the synthetic value).
- `crun state` never supplies an exit code: `src/libcrun/container.c:libcrun_container_state` (1.28:3499; 1.30.1:3230) emits only `ociVersion`, `id`, `pid`, `status`, `bundle`, `rootfs`, `created`, `systemd-scope`, `owner` and `annotations`. CRI-O decodes that into `ContainerState`, whose `exitCode` key it never contains.
- So for every accepted status (code >= 0, not 255), the guarantee rests on **exit-file provenance**. The runtime-state evidence only decides *when* EXITED is reported.

| Route | Runtime-state evidence | Exit-file provenance | Init exit |
|---|---|---|---|
| C1 | `crun state` "stopped" (pid ESRCH, zombie or `X`, start-time mismatch, or the item-3 cases) | conmon `waitpid` returned `container_pid`; value is `WEXITSTATUS` or `128+sig` (≥ 0) | **Proven (READ):** waitpid only returns a terminated child, which is past `exit_notify`, so `zap_pid_ns_processes` has already run |
| C2 | `crun state` "stopped" | conmon v2.2.x ESRCH probe, value **0** | **Proven (READ + INFERRED):** ESRCH means no task holds that pid, so init was released, which follows `exit_notify`. That `release_task` frees the pid is INFERRED (not re-read). That `container_pid` is a host-pidns pid (crun's pidfile, read by conmon in the host pid ns) is INFERRED |
| B1 | `crun state` **errors** (state dir missing, `kill` EPERM, `/proc` open error, etc.) | as C1 | **Proven (READ)**, same as C1. The `crun state` error is irrelevant to the proof |
| B2 | `crun state` errors | as C2 | **Proven**, same as C2 |
| C3 / B3 | either | value **-1** (ECHILD with no pid, other kill errno, timeout) | **Not proven.** Under conmon v2.1.x ECHILD can quit while init is alive. Refuse all negatives |
| B4 | `crun state` errors | no file, so **255** | **Not proven.** Refuse 255 |
| D | `crun state` "stopped" | no file, so **-1** | **Not proven** (see item 3). Refuse negatives |

**Stale or foreign file:** the persist dir `c.dir` is per container ID. A kubelet restart of a container creates a new container ID. No other writer exists (READ above).

---

## Q3. Dead monitor (conmon dies while the container runs)

**What CRI-O does** (READ):
- It does **not** synthesize an exit. `runtime_oci.go:ProbeMonitor`, driven every probe interval by `internal/lib/container_server.go` (around line 1071), only logs "Conmon for container %s is stopped, although the container is running", increments `MetricContainersStoppedMonitorCount` and clears `monitorProcess`.
- The container stays RUNNING while `crun state` says running.

**What happens to init:**
- CRI-O does not set a subreaper (READ: no subreaper call in non-vendor CRI-O). conmon `setsid`s and double-forks.
- So an orphaned init is reaped by host PID 1 (INFERRED). Nobody writes an exit file.
- When init later exits (for example after the kubelet's `StopContainer`), `crun state` says stopped and `waitForExitFile` times out. The result is **exit -1, reason `Error`, with a containerID** (route D). The allowlist must refuse it.

**Codes other than 137/255 without a reap:**
- -1 (routes C3, B3, D).
- 0 from the conmon v2.2.x ESRCH probe, but init is gone in that case (C2).
- No path found that yields a positive code without conmon's `waitpid`.
- CRI-O never synthesizes 137. All 137s on CRI-O come from conmon's `128+SIGKILL` after a reap (C1/B1), or from kubelet synthesis with reason `ContainerStatusUnknown`/`RestartingAllContainers` and no containerID.

---

## Q3-follow-up (item 3). Unreadable `/proc/<pid>/stat`, resolved

**crun 1.30.1 `src/libcrun/status.c`** (READ):
- `libcrun_is_container_running`:
  - Calls `kill(pid, 0)`. ESRCH means stopped. Any other errno is a **crun error**, so `crun state` fails and CRI-O takes route B.
  - On success it calls `libcrun_check_pid_valid`.
- `libcrun_check_pid_valid`: if there is no saved start time, the container is treated as running. Otherwise it calls `read_pid_stat` and reports stopped if the start time differs or the state is `Z`/`X`.
- `read_pid_stat`:
  - `open("/proc/<pid>/stat")` failing with **ENOENT or ESRCH** zeroes the struct and returns 0. The zero start time then mismatches, so this means **stopped**. Any **other open errno** (EACCES, EPERM, ENOMEM, EMFILE, …) returns a **crun error**, so route B.
  - `read_all_fd` failing with **any errno** sets state `'X'` and returns 0, so **stopped**. `read_all_fd_with_size_hint` (`utils.c`) retries EINTR (`TEMP_FAILURE_RETRY`) and returns other errors.
  - Parse failures return a crun error (route B).

**When can each happen while the pid is alive?** crun runs as root in the host pid and mount namespace, spawned by CRI-O or conmon, reading the host `/proc` (INFERRED).
- **open, ENOENT:** `fs/proc/base.c:proc_pid_lookup` and `proc_pid_permission` (v6.12, READ) return ENOENT for a live pid only when the procfs mount has `hidepid=invisible` (2) or `hidepid=ptraceable` (4) and `has_pid_permissions` fails, i.e. `ptrace_may_access(task, PTRACE_MODE_READ_FSCREDS)` is denied.
  - Root with CAP_SYS_PTRACE in the initial user namespace passes the capability part. An LSM (SELinux `file:read` between the crun domain and the container domain) could still deny (INFERRED).
  - The default host `/proc` has no `hidepid` (INFERRED). The node mount options are NOT READ; check `findmnt -no OPTIONS /proc`.
  - A different pid namespace does not apply: crun runs in the host pid namespace and the status pid is the host pid (INFERRED).
  - **If `hidepid=2/4` is set and access is denied, crun reports a live init as stopped.**
- **open, EPERM:** `hidepid=noaccess` (1) with access denied. That is an error, not stopped (route B).
- **open, ENOMEM/EMFILE:** an error, not stopped (route B).
- **read errors:** `proc_single_show` returns **ESRCH** only when `get_pid_task` finds no task, i.e. the task was released (genuinely gone). `do_task_stat` (`fs/proc/array.c`) has no error return (READ). `fs/seq_file.c:seq_read_iter` can return **ENOMEM** when `seq_buf_alloc` fails (READ).
  - **So a read-time ENOMEM on a live pid makes crun report "stopped".** That is plausible only under severe memory pressure, or if the crun process itself is the OOM victim (INFERRED likelihood).
- **cgroup branch** (`container.c:libcrun_get_container_state_string`, READ): with the pid alive, an ENOENT/ENODEV reading `cgroup.freeze` means **stopped**.
  - On cgroup v2 a populated cgroup cannot be removed, so this needs init to have migrated out of its cgroup, which takes a privileged actor (INFERRED; kernel cgroup rmdir NOT READ).
  - crun 1.29.1 and earlier also short-circuit an empty cgroup path to "not paused". 1.30.1 instead normalizes an empty status path to NULL (commits `d150477d`, `d43a070f`; INFERRED equivalent).

**Plainly:** yes, `crun state` can report a live init as "stopped", via proc ENOMEM, via `hidepid=2/4` with ptrace denied, or via a cgroup migrated out.

**Impact on the guarantee:**
- In each case conmon is still waiting on init, so **no exit file exists**. CRI-O takes **route D: exit -1, Error**. If `crun state` instead errored, it takes **route B4: 255**.
- Both are refused once the allowlist rejects negatives and 255.
- If init does exit during the roughly 5 s `waitForExitFile` window, the file is genuine (C1/C2).
- So the false-stopped cases cannot produce an accepted status.
- A false-stopped also makes the kubelet see the container as exited while it is alive. With -1 refused the epoch defers, which fails closed. A later `ContainerStatus` re-runs `UpdateContainerStatus` (Stopped with nil `ExitCode`) and can flip it back to RUNNING (READ `container_status.go`).

---

## Q4. kubelet 1.35.3 synthesized terminated statuses

These are all the places in non-test `pkg/kubelet` that build `ContainerStateTerminated{` (READ):
- `kubelet_pods.go:convertToAPIContainerStatuses`, around line 2285: the CRI EXITED pass-through, with a containerID.
- Around line 2308: Unknown state after a previous Running. Reason `ContainerStatusUnknown`, or `RestartingAllContainers` if the alpha gate `RestartAllContainersOnContainerExits` is on and the pod is restarting. ExitCode 137, no containerID. The gate is alpha and default false in 1.35 (`pkg/features/kube_features.go:1720-1722`).
- Around lines 2507 and 2547: `LastTerminationState` only (`RestartingAllContainers` / `ContainerStatusUnknown`, 137). State stays waiting.
- `status/status_manager.go:TerminatePod`, lines 593-615: `ContainerStatusUnknown`/137, no containerID, for both containers and init containers.
- **No "init container `Completed` without a containerID" in 1.35.3.** If 1.37.1 has one, it is newer.
- A previous terminated status is reused when the runtime no longer lists the container (around lines 2460-2470), keeping the original containerID and reason. Benign.
- `SyncTerminatingPod`'s post-kill running-container recheck (`kubelet.go` around 2255) and `mergePodStatus` with `PodCouldHaveRunningContainers` (`status_manager.go:1062`) are present, matching 1.37.1 M0 items 1 and 3.

## Q5. Native-sidecar stop order and grace (kubelet 1.35.3)

**Confirmed** (READ):
- `kuberuntime_termination_order.go:newTerminationOrdering` walks `InitContainers` in reverse. Each restartable init container waits for every main container and for the sidecar declared after it.
- `waitForTurn` waits at most the container's own grace period and returns the elapsed seconds.
- `kuberuntime_container.go:killContainer` does `gracePeriod -= int64(waitForTurn)` (truncated), then raises anything below `minimumGracePeriodInSeconds = 2` (`kuberuntime_manager.go:84`) to 2.
- `containerTerminated` (which releases waiters) runs only after `StopContainer` succeeds.
- `setTerminationGracePeriod` uses `DeletionGracePeriodSeconds`, else `terminationGracePeriodSeconds`. `controller/internal/kube/render.go` sets neither (`git grep`), so the default of 30 s is INFERRED from API defaulting.

**Effect today** (INFERRED, matches the measurement):
1. `dind-meter` is declared after `dind`, so it is stopped first, gets about 30 s, ignores SIGTERM and is SIGKILLed. conmon reports a genuine **137**.
2. `dind`'s wait also runs out at about 30 s, so its grace becomes the **2 s** minimum. dockerd's roughly 1 s shutdown fits only narrowly.
3. Fixing `dind-meter`'s TERM handling restores about 30 s for `dind`.

On CRI-O, the stop path for each container is `StopLoopForContainer`: the stop signal via `crun kill`, then repeated `crun kill KILL` after the timeout (READ).

---

## Q6-follow-up (item 4). Version bounds

**crun.** Function bodies were extracted per tag and compared byte for byte (READ):

| Function | 1.26 | 1.27 | 1.28 | 1.29.1 | vs 1.30.1 |
|---|---|---|---|---|---|
| `read_pid_stat` | = | = | = | = | identical |
| `libcrun_check_pid_valid` | = | = | = | = | identical |
| `libcrun_is_container_running` | = | = | = | = | identical |
| `libcrun_get_container_state_string` | = | = | = | = | identical |
| `has_new_pid_namespace` | = | = | = | = | identical |
| `container_delete_internal` | 1 line differs | = | = | = | 1.26: a `libcrun_cgroup_killall` failure is ignored (`return 0`), on the no-private-pidns branch only; irrelevant here |
| `libcrun_cgroup_is_container_paused` | differs | differs | differs | differs | ≤1.29.1 also treat an empty cgroup path as "no cgroup"; 1.30.1 normalizes it earlier (commit titles, INFERRED equivalent) |

### crun 1.28 extension (2026-10-29, READ)

The remaining namespace-creation and state-output paths were compared at upstream
1.28 (`54f16ffbefcd022bf032af768b5c5ce075c18bfc`) and 1.30.1
(`079ff6a7a16d029460af882f9ea44674e6a510b6`). This extends the complete audited
crun bound to **1.28 or 1.30.1**, not every intervening version.

- **Namespace creation:** [1.28 linux.c:4194](https://github.com/containers/crun/blob/54f16ffbefcd022bf032af768b5c5ce075c18bfc/src/libcrun/linux.c#L4194), `configure_init_status`, is byte-identical to 1.30.1. [linux.c:5114](https://github.com/containers/crun/blob/54f16ffbefcd022bf032af768b5c5ce075c18bfc/src/libcrun/linux.c#L5114), `libcrun_run_linux_container`, and `init_container` at linux.c:4854 preserve both PID-namespace creation routes described in item 1. The differences delay mount-namespace creation, adjust mount preparation and add a null mount-type guard on an existing-namespace join path; they do not turn a pathless PID namespace into a join or omit its creation.
- **State output:** [1.28 container.c:3499](https://github.com/containers/crun/blob/54f16ffbefcd022bf032af768b5c5ce075c18bfc/src/libcrun/container.c#L3499), `libcrun_container_state`, emits the same top-level keys as 1.30.1, with no exit code. 1.30.1 factors config loading and annotation generation into helpers and checks individual JSON writes. These differences do not supply CRI-O an exit code; accepted nonnegative codes still depend on conmon's exit-file provenance.
- **Supporting paths:** `container.c:3439` state classification, `container.c:1818` private-PID detection, `container.c:1834` force-delete, `container.c:1945` kill, `linux.c:6028/6056` kill helpers, `status.c:177/775/795` proc/PID checks, `status.c:406` status loading and `utils.c:951` fd reading are byte-identical between the two tags. The earlier table's cgroup-path normalization inference remains marked INFERRED.
- **Distribution source:** the [crun-1.28-1.el10 SRPM](https://kojihub.stream.centos.org/kojifiles/packages/crun/1.28/1.el10/src/crun-1.28-1.el10.src.rpm) contains only the upstream release archive and its spec. The spec declares Version 1.28, Release 1, no patches, `%autosetup -Sgit`, and the ordinary autogen/configure/make build. It matches [CentOS c10s packaging commit 60ddb496](https://gitlab.com/redhat/centos-stream/rpms/crun/-/blob/60ddb496436a0d21c8f2a5e5fe3a942ea14b352f/crun.spec).
- **Archive identity:** the SRPM SHA-256 is `77e90ce4c8621a11c336916151f2b92051a0d06ec09016be3e1d4c0ed1896459`; its source archive SHA-512 is `9a2ee5c53169e166f415bcd7713b03e0ce8ec007a6ec6409aa72a1339881efa0155338c1147dcf39fbce100888f3217cb6bab51f45d5a2656f7eef34dc4d1eef`, matching the historical dist-git sources manifest.

The actual SRPM archive's relevant files are byte-identical to upstream 1.28:

| File under src/libcrun | SHA-256 |
|---|---|
| linux.c | `76bf4648d05d988a16f597009d2df4800790a632c5d54612ee56ed762fbaa706` |
| container.c | `100b530310e68b4f906384173e266d72d0e9ca37b23f22423bf3a795c4b3c137` |
| status.c | `9e02df5b502a800484761fbb1230fb3bac4d5347283c3effdccefeba1b04d3b8` |
| utils.c | `be1b7daf2f537a3d357c7df8a8e4dbfdb4d0b206a59b9d8322c37823ea08a6b2` |

This is source equivalence for the audited properties, not a reproducible binary
build, an el10 kernel-backport audit, or target-cluster storage acceptance.

**conmon** (READ):
- `src/ctr_exit.c` is byte-identical v2.1.10 to v2.1.13 (`git diff --stat` empty). From v2.1.8, the only difference is `reset_oom_adjust()` in `do_exit_command`, not on the cited path.
- v2.2.0 and v2.2.1 are identical in `ctr_exit.c` and `conmon.c`.
- v2.1.13 to v2.2.x adds the ECHILD `kill(container_pid,0)` probe (commit `90d6038`, "Fix container exit detection in systemd scope environments"), first in v2.2.0. Before it, ECHILD quits with `container_status = -1`, giving an exit file of -1.
- The post-loop exit-file section of `conmon.c`, from `int exit_status = -1;` to `return exit_status;`, is **identical v2.1.10 to v2.2.1**. v2.1.0 differs only in signalfd cleanup and the sync-fd helper.
- `set_subreaper(true)` is present at v2.1.13 and v2.2.1 (v2.1.0 not checked for the subreaper call).

**CentOS Stream 10 dist-git** (READ):
- `crun.spec` is `Version: 1.30.1`, `Release: 1`, with `Source0` the upstream release tarball, **zero `Patch` lines**, and `%autosetup -Sgit`.
- `conmon.spec` is `Version: 2.2.1`, `Release: 4`, with `Source0` the upstream tag archive and **zero `Patch` lines**. The changelog for -2 to -4 covers spec macros, hardening flags and TMT tests only.
- For the 1.30.1 and conmon release archives, equality to the git tag for the cited files is INFERRED. The four relevant 1.28 SRPM source files were compared directly above (READ).

**The conclusion holds for:**
- **conmon v2.1.10 to v2.2.1** (ctr_exit.c also from v2.1.8). Under v2.1.x the ECHILD path yields -1 rather than the v2.2.x ESRCH 0; both are handled by refusing negatives.
- **crun 1.28 or 1.30.1** for the complete namespace/state-output proof. The broader function comparisons above do not extend the declared bound to other versions.
- **CRI-O v1.36.2 only**: other CRI-O versions are NOT READ.
- The OKD 4.22 image's actual RPMs must fall inside these ranges (NOT verified).

---

## Q6. Verdict

**The current allowlist (Completed/Error, containerID set, exit not in {255, 137}) is not sufficient on CRI-O.** It accepts **-1 / Error** (routes C3, B3, D, and the dead conmon of Q3).

**With all negative exit codes and 255 also refused:**
- Every accepted CRI-O status is route C1, C2, B1 or B2.
- Each proves init exited through conmon's `waitpid` or its ESRCH probe.
- By `exit_notify` ordering, the kernel had then already killed and waited out every process in the pid namespace and its descendants (READ in upstream v6.12).

This holds even when `crun state` misreports a live init as stopped (item 3), because no exit file exists in that case.

**Remaining residuals:**
1. An **NRI plugin** replacing the pid namespace (operator-installed). Defer if any NRI plugin is present, or treat it as an operator residual.
2. **Ephemeral containers** without a target: add an `ephemeralContainerStatuses` check.
3. A **deliberate escape** or a privileged cgroup migration: an accepted PRD residual. It also cannot yield a file of 0 or more without init exiting.
4. **el10 kernel backport and actual node RPM versions**: NOT verified.
5. **conmon-rs, runtimeVM, the runc handler under CRI-O, and CRI-O versions other than v1.36.2**: NOT READ. Those must defer.

**Allowlist changes needed for CRI-O + crun:**
- Refuse `exitCode < 0` in addition to 255.
- Keep 137 refused for containerd parity; CRI-O alone does not need it.
- CRI-O reasons are only `Completed`, `Error`, `OOMKilled` and `seccomp killed`, so allowing only Completed/Error is correct.
- Add the `ephemeralContainerStatuses` check.
- Gate on a declared runtime of "CRI-O 1.36.2, `runtime_type` oci (conmon v2.1.10 to v2.2.1), crun 1.28 or 1.30.1 default handler, kubelet 1.35.3, no `runtimeClassName`, no NRI plugin that adjusts namespaces".

## Not read / not determined
- The exact conmon, crun and cri-o RPMs in the OKD 4.22 SCOS node image, and the node `/proc` mount options (hidepid).
- The el10 kernel source (only upstream v6.12 was read). Kernel cgroup v2 rmdir and `release_task` pid freeing were not re-read.
- The glibc `WIF*` decode of -1.
- crun namespace creation and state output at versions other than 1.28 and 1.30.1.
- conmon `conmon.c` before v2.1.10 beyond the exit section, and the v2.1.0 subreaper call.
- conmon-rs, the CRI-O runtimeVM path, runc behind CRI-O, and any installed NRI plugins.
- No live-node verification was performed.
