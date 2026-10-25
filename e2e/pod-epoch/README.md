# Pod-epoch real-daemon harness (PRD #1860, M0)

Controlled Docker-in-Docker daemons for experiments on what a sidecar restart
or a successor pod does to a daemon's state. Runs **outside all gates**: not in
`task gate`, CI or any Taskfile recipe. Run it by hand.

```sh
bash e2e/pod-epoch/smoke.sh all        # or: rootless | rootful
```

Prints `SMOKE <posture> PASS` per posture and exits nonzero on any failure.

Experiments (each `bash e2e/pod-epoch/exp-<name>.sh <rootless|rootful|all>`):

| script | PRD question | what it shows |
|---|---|---|
| `exp-restore.sh` | Q2 | what a persisted data root starts before the first API response (`always`, `unless-stopped`, `on-failure`, `no`), after a sidecar restart and after a successor pod; whether anything changes afterwards; the barrier predicate; how a daemon restart is detected |
| `exp-stopped.sh` | Q3 | a stopped container holds no mount or file reference; what `docker start` and `docker cp` do when the bind source has been deleted |
| `exp-volumes-build.sh` | Q3 | plain versus bind-option `local` volumes; a build with the tree as context leaves no mount, fd or mapping reference and a cache that outlives the tree |
| `exp-termination.sh` | Q1 | when the DinD sidecar container is ended the way kubelet ends a container (outer `docker kill -s KILL`; outer `docker stop --time 10`), every writer inside it (a container's main loop, a backgrounded child, a `docker exec`'d loop, all in the inner container's own nested pid namespace) has stopped by the time the outer runtime reports the container `exited`, read from a separate read-only helper container on the same volume; and the successor pod's restored writer container (policy `no`) is exited and its file does not grow |
| `exp-readonly.sh` | Q4/Q6 | the reclaim predicate's read-only queries change nothing and emit no event |

Each posture runs in its own subshell. Output is machine-greppable:
`RESULT <exp> <posture> <key>=<value>` lines, then
`EXP <exp> <posture> PASS|FAIL|INCOMPLETE`, then `DONE <exp> <which> rc=<n>`.
Exit status: 0 all passed, 1 any FAIL or harness error (`EXP ... FAIL harness-error`),
3 some INCOMPLETE and none failed. `check.<name>=PASS|FAIL` lines are assertions;
`control.<name>=PASS` lines are positive controls (the instrument seeing the thing it
asserts absent); a failed control prints `instrument.<name>=unavailable reason=...`
and makes the experiment INCOMPLETE, never PASS. Everything else is a recorded
observation, not a pass criterion. Bounded observation windows are labelled
`supplementary` and are always paired with a control.

## Requirements

- `docker` CLI reaching an outer daemon that permits `--privileged` containers
  (`DOCKER_HOST`, e.g. `tcp://127.0.0.1:2375`), and `timeout` (coreutils).
- Network to pull the pinned images on a cache miss.
- Verified against Docker Engine 29.8.0 (go1.26.8, API 1.56) in both postures.

## Library (`lib.sh`, sourced)

`pe_init <posture>`, `pe_volumes_create`, `pe_daemon_create_start`,
`pe_inner <cap> <docker args>` (docker CLI in the daemon container),
`pe_client <cap> <docker args>` (docker CLI in the worker container),
`pe_inner_sh` / `pe_worker_sh` / `pe_root_sh <cap> <script>`,
`pe_daemon_restart`, `pe_daemon_container_recreate`, `pe_daemon_replace`
(see Pod model), `pe_events_start <file>` / `pe_events_stop`
(exit status in `PE_EVENTS_RC`), `pe_server_info`, and an EXIT-trap cleanup
that preserves the original exit status. Every experiment call has a cap within
a shared deadline (`PE_DEADLINE_SECONDS`, `^[1-9][0-9]{0,4}$`, default 900 per
posture for `smoke.sh`, 1500 for the restore, stopped and volumes-build
experiments, 900 for readonly); cleanup and events teardown use fixed caps
outside it. Reporting helpers: `pe_exp_begin`, `pe_res`, `pe_check`,
`pe_control`, `pe_incomplete`, `pe_mark_fail`, `pe_exp_finish`, `pe_exp_main`.

`PE_EVENTS_RC` is the raw exit status of the local exec; `PE_EVENTS_END` says
why: `stopped` (we ended it), `ended` (it ended itself, e.g. daemon restart) or
`deadline` (status 124). The stream is started with `--since` taken inside the
worker container. `pe_events_stop` then, whenever the worker container is still
up, finds every process in it whose cmdline contains ` events `, terminates it
(then SIGKILL after 10 s), requires that none remain and that the pid file is
gone, and returns nonzero otherwise. This runs on the `stopped`, `ended` and
`deadline` paths alike: a `deadline` or a lost local exec leaves the in-container
client running, so it is the in-container check, not the local exit, that
establishes "gone". A worker container that is removed or stopped takes its
processes with it (nothing to verify). A second start while one is active fails.
`pe_events_wait_end <seconds>` polls (20 ms) until the stream's client exits and
sets `PE_EVENTS_EOF_US`.

`pe_first_response_start <file>` / `pe_first_response_wait` run, in the worker
container, a poller that first observes the daemon failing, then takes the first
successful `docker info` as the first API response and immediately records
`docker ps -a` (`GET /containers/json?all=1`), `docker inspect` of every
container and `docker plugin ls`. Start it before triggering the restart or
replace.

The library requires the caller's `set -euo pipefail` and one posture per
(sub)shell; `pe_init` fails if called twice in the same shell.

## Pod model (changed from the first smoke harness)

The harness is a three-container pod, so that a client survives what it observes:

- `pe1860-<posture>-<token>-pause` owns the network namespace and outlives a
  daemon container restart, as a pod sandbox does.
- `pe1860-<posture>-<token>-worker` is the uzi-worker stand-in: the docker CLI
  client (`pe_client`, `pe_worker_sh`, events stream, first-response poller). It is
  a separate container and pid namespace on the pod network (and, rootless, the
  socket volume). Previously the events client lived in the daemon container and
  died with it, so "stream ended on restart" could not distinguish the daemon
  closing the stream from the client being killed; it is no longer used.
- `pe1860-<posture>-<token>` is the DinD sidecar (`pe_inner`, `pe_inner_sh`;
  `pe_root_sh` is the same container as uid 0, for instruments only).

`pe_daemon_restart` is `docker restart` of the daemon container (same container,
same writable layer). `pe_daemon_container_recreate` is a new daemon container in
the same pod (what kubelet does when a sidecar dies: fresh root filesystem, same
netns and volumes). `pe_daemon_replace` is a successor pod: all three containers
are removed (the daemon dies by `docker rm -f`, SIGKILL) and recreated on the same
volumes.

## Instruments

All run in the daemon container as root (`--privileged`, every capability) so they
can read per-process proc files of every process, in both postures. `which nsenter`
exists in the image, but no `nsenter` is needed: dockerd, containerd, the shims
and every container task share the daemon container's pid namespace (rootlesskit
does not create a pid namespace; its child's mount and user namespaces are
visible through the per-pid `ns/mnt` links), so the proc files of
`docker inspect` `State.Pid` and of dockerd are readable directly.

- **Tree-reference scanner** (`pe_tree_ids`, `pe_tree_scan`, `pe_scan_refs`).
  `pe_tree_ids <tree>` records `st_dev:st_ino` of the tree and of everything in
  it. `pe_tree_scan <tree> <ids>` reports:
  - `MNT-STRICT`: a mount in any reachable mount namespace whose mountinfo
    `major:minor` equals the `/data/runner` mount's and whose root field is that
    mount's root plus the tree path (or inside it). Mountinfo is read once per
    distinct mount namespace (`ns/mnt` link), from the first readable process.
  - `MNT-LOOSE`: any other mountinfo line mentioning `/<tree-relative-path>`.
  - `FD`: a process `cwd`, `root`, `exe` or open `fd` whose `stat -L`
    (resolved by the kernel, so independent of the process's own mount namespace
    and root) has a recorded `st_dev:st_ino`, including inodes of since-deleted
    files held open.
  - `MAP`: a memory mapping of a recorded inode on the same device.
  - `NS-SEEN ns=<id>` per distinct mount namespace some live process was in,
    `NS-READ ns=<id> pid=<p>` per mount namespace whose mountinfo was read, and
    `SCAN-UNREADABLE pid=<p> what=<item>` per item that could not be read
    (`ns-mnt`, `mountinfo`, `fd-dir`, `cwd`, `root`, `exe`, `fd/<n>`, `maps`)
    while the process still existed, then
    `SCANNED pids=<n> mountns=<n> unreadable=<n> vanished=<n>`. A process that
    exited (or is a zombie) by the time a failure is examined counts once in
    `vanished`, and an fd that closed between the listing and the stat is
    ignored; neither is an unreadable item. The script exits nonzero on an
    internal error (no `/data/runner` mount, empty id list, a failing awk).
  A zombie thread-group leader whose other threads still run counts as alive
  (its unreadable items are reported, not "vanished"), and `NS-SEEN` is emitted
  for every thread's mount namespace (`task/*/ns/mnt`), so a control namespace is
  "gone" only if no task holds it.
  Limits: a mount namespace with no task in it is unreachable (a namespace held
  only by an nsfs fd or bind mount, with no task, is invisible and would count
  as gone); only inodes recorded while the tree existed are matched after deletion. Positive control:
  a running container bound to the tree is seen as a strict mount hit in the
  task's own mount namespace and as an `FD` hit on the task's cwd, with the
  task's and dockerd's namespaces read and at least two mount namespaces
  scanned; a deleted directory held by a labelled cwd holder process is seen;
  `exp-volumes-build.sh` shows a bind-option volume's mount in dockerd's own
  mount namespace, and a process executing a file in a tree gives the `MAP`
  control (if that fixture fails, MAP is reported `uncontrolled` and the
  experiment is INCOMPLETE). The scanner is also run after removing the control
  container to show it then reports zero. Every zero-reference conclusion in
  `exp-stopped.sh` and `exp-volumes-build.sh` (`pe_check_clear`) requires a
  complete scan (`pe_scan_complete`): `unreadable=0`, dockerd's current mount
  namespace among the `NS-READ` lines, and every mount namespace the control
  scan read either read again or no longer observed (`NS-SEEN`) by any live
  process (recorded as `ns_gone`). Otherwise the line is
  `<label>.scan=incomplete reason=...`, the check is `INCOMPLETE scan=incomplete`
  and the experiment is INCOMPLETE (exit 3); any reference found is still a FAIL.
  Positive control that `SCAN-UNREADABLE` fires (`pe_unreadable_control`): the
  same scan as uid 65534 cannot follow root-owned processes' per-pid links, so it
  must report lines equal to its `unreadable=` count, while the root scan of the
  same tree reports 0. Limit: the `ns/mnt` readlink shares one ptrace-access
  gate with `fd`, `cwd`, `root`, `exe` and `maps`, so permissions can only
  trigger the `ns-mnt` item; the reporting paths of the other items are not
  separately triggered (only the vanished-process accounting is seen in
  ordinary runs). Synthetic checks of the instrument (not real-daemon results),
  run on the worker host with the scan body extracted from `lib.sh`: (1) with a
  `stat` stub that always fails (empty stat output) the pre-fix awk
  (`NR == FNR`) reported only the 7 permission-denied `ns-mnt` items and none
  of `cwd`, `root`, `exe`, `fd/<n>`; the fixed awk (`FILENAME == ARGV[1]`)
  reported `SCANNED pids=39 mountns=1 unreadable=406 vanished=2` including
  `what=cwd` and `what=root` lines. (2) A python process whose main thread exited
  via `pthread_exit` while a thread sleeps (leader `State: Z`, 2 tasks): the
  pre-fix `alive()` answered no, the fixed one answers yes.
- **Inode retention** (`exp-stopped.sh`). A root process whose cwd is the tree
  keeps its inode allocated after `rm -rf`, because freed inode numbers are
  reused at once (observed). Without it "the recreated directory has a new
  inode" is not decidable from inode numbers. The same holder is the handle for
  listing the deleted directory (`ls -A` on the holder's per-pid `cwd` link, as
  root): its listing right after the deletion is the baseline, and
  `obs.heldA.listing_unchanged` / `obs.heldB.listing_unchanged` record whether it
  is unchanged after `docker cp` and `docker start`. They are observations, not
  checks: the kernel refuses to create entries in an unlinked directory, so an
  unchanged listing cannot show that a request avoided writing there. The
  meaningful checks are `check.start-idle.container-binds-recreated-inode` and
  `check.start-cbind.container-binds-recreated-inode`: after a successful start,
  `stat -c %i /w` run inside the container must equal the recreated tree's inode
  and differ from every held deleted inode (a failed start is an allowed outcome,
  recorded by `<name>.rc`); the recreated idle tree's inode is held too, so the
  later `start-cbind` cannot meet a reused number. The tree-M section asserts
  `check.start-c-mount.fails`, `check.cp-into-c-mount.fails`, and for `start-c-vf`
  re-resolution plus the same container-inode check (tree-M's original inode is
  held by a cwd holder).
  `check.cp-into-stopped.reresolution` and `check.start-idle.reresolution` pass
  only if the request failed or the tree was recreated on an inode different from
  every held deleted inode; the recreated tree of an idle start must be empty.
- **Writer termination** (`exp-termination.sh`). In the posture's daemon a
  writer container (`-v /data/runner/tree-W-<case>:/w`, restart `no`) appends a
  counter every 100 ms from its main loop, a backgrounded child and a
  `docker exec`'d loop (`<n>`, `c<k>`, `x<k>` lines). Positive controls: the file
  grows over about 1 s and all three writers have written (read through the
  helper), and the writer container's pid namespace differs from the daemon
  container's. The daemon container is then ended from the outer daemon (case
  `kill`: `docker kill -s KILL`; case `stop`: `docker stop --time 10`),
  `docker inspect` is polled (20 ms) for `exited`, and the file is read at once
  through a labelled helper container (read-only mount of the run volume)
  started beforehand so that the first read is a single exec; after 5 s
  (supplementary) it is read again, and once more from a fresh labelled helper
  container. `check.<case>.writers-stopped-by-exit` requires the three readings
  identical (size, line count, last lines). The pod is then recreated on the same
  volumes and the restored writer must be `exited`, policy `no`, the file
  unchanged after 3 s (`check.<case>.restored.log-does-not-grow`; the exited
  state and the absent writer processes of the new pod are observations,
  `obs.<case>.restored.*`, as they cannot see an old-epoch survivor). Besides
  `writers-stopped-by-exit`, `check.<case>.writers-active-until-signal` requires
  the size at exit to exceed a reading taken just before the signal, and
  `check.<case>.last-write-not-after-exit` requires the log's last modification
  time (`stat -c %y` in the busybox helper, which has no sub-second `%Y` and no
  `date +%N`) to be not after the outer `docker inspect` `State.FinishedAt` of the
  daemon container (same host kernel clock); both values and their difference are
  recorded (`last_write_mtime`, `finished_at`, `finished_minus_last_write_ms`).
  Recorded per case:
  delivery, time to `exited`, size at exit, size after the window, last counters.
  Stand-in: this exercises the outer Docker 29.8.0 engine's containerd/runc stop
  path in place of kubelet plus CRI containerd. They share the shim/runc family;
  this is not the CRI plugin, so the CRI's own timeouts, signalling and status
  reporting are not exercised.
- **Events window** (`exp-restore.sh`). `docker events --since <first-response
  SystemTime>` for 20 s from the worker container (killed by `timeout`);
  supplementary. It is valid only if its exit status equals the timeout-kill
  status observed in its control, its stderr is empty and it ran at least 19 s;
  otherwise `window.instrument-ok` FAILs (`window-instrument-failed`). The
  control uses the same command shape and the same `--since` form (RFC3339Nano
  `docker info --format {{.SystemTime}}`, taken just before it runs a container)
  and requires at least 3 events. Container states are also re-inspected at the
  end and compared with the first-response inspect.
- **Markers** (`exp-readonly.sh`). Events are ordered, so a marker volume created
  before the snapshot and the control operations after it bound the query phase
  exactly; no sleep stands in for absence.
- **Restart detection** (`exp-restore.sh`). `supp.time_to_eof_ms` (supplementary)
  is measured from just before the trigger command to the exit of the local
  client (20 ms polling) and includes `docker exec`/`docker restart` start-up
  (about 100 ms). The control holds the idle stream open for 5 s and requires it
  still open. The rootless socket is hard-linked before the trigger; the held
  inode stays allocated, so a socket file recreated by the next start must show
  a different inode; both inodes must read as numbers or the check FAILs.
  `kill9` records the dockerd pids (with start times) and the daemon
  container's pid and start time before the kill and requires the target gone
  afterwards (`kill-delivered`, else `kill-not-delivered`). Rootful delivers it
  with `docker kill --signal KILL` from the outer daemon (the kill must not
  depend on dockerd's place in the container's pid namespace; measured here,
  `docker-init` is PID 1 and dockerd is not); rootless kills the recorded pids from inside (dockerd is rootlesskit's
  child). The delivery used is a RESULT line. The rootless recovery key is
  `recovery.harness_tmpfs_runtime_dir` because rootless `XDG_RUNTIME_DIR` is a
  per-start tmpfs here (production: a pod-lifetime `emptyDir`).

## Safety rules

The outer daemon is shared with other runs.

- Never prune, never list-and-remove, never glob.
- Only names `pe1860-<posture>-<token>[-suffix]`, never the `uzi-` prefix.
- Every container and volume carries `pe1860.owner=<token>`; removal is by
  exact name, only after reading the label back and matching the token.
- Check for leftovers with `docker ps -a --filter label=pe1860.owner` and
  `docker volume ls --filter label=pe1860.owner` (read-only).

## Wiring versus production

Mirrors `controller/internal/kube/render_dind.go` (`dindContainer`):
a harness-owned volume at the data root (the persisted DinD PVC) and another at
`/data/runner` (the shared run workdir).

| | rootless | rootful |
|---|---|---|
| image | `docker:29-dind-rootless@sha256:e17fa54c...` | `docker:29-dind@sha256:5efed980...` |
| flags | `--privileged --user 1000:1000`, `XDG_RUNTIME_DIR=/run/dind`, `DOCKER_TLS_CERTDIR=` | `--privileged` |
| data root | `/home/rootless/.local/share/docker` | `/var/lib/docker` |
| endpoint | `unix:///run/dind-sock/docker.sock` | `tcp://127.0.0.1:2375` |

Deviations:

- Rootless runs the explicit command
  `dockerd --host=unix:///run/dind-sock/docker.sock --storage-driver=vfs` (nested
  overlay is not assumed). Flags only would make the image entrypoint add
  `--host=tcp://0.0.0.0:2375` and publish it on the shared bridge with TLS off.
  Production rootless keeps `/certs` TLS on 2376; the harness binds unix only.
  After every daemon start, restart and first response, and after the Swarm
  control's init (which binds `--listen-addr 127.0.0.1:2377`, `--data-path-addr
  127.0.0.1`), the harness checks (read-only, via `/proc/net/tcp*`) that nothing
  listens on a non-loopback TCP port and fails otherwise. (A raw `docker restart`
  inside an experiment is covered where the script calls the check; the
  `restore` experiment calls it after each restart, kill and recreate.)
- Image `VOLUME` paths not mounted by a harness volume (`/var/lib/docker`, and
  rootless also `/home/rootless/.local/share/docker`) get a tmpfs in the pause,
  worker, helper and (where applicable) daemon containers, so no unlabelled
  anonymous volume is created; `pe_assert_no_anon_volumes` inspects the pod's
  containers read-only after each start and fails on any other volume.
- The rootful posture omits production's MTU cap and the `--config-file`
  default-network-opts; it uses the image defaults.
- Rootless mounts a tmpfs at `/run/dind` (uid/gid 1000, `XDG_RUNTIME_DIR`: pid
  files, rootlesskit state) in place of the pod `emptyDir`; it is cleared on a
  container restart. The socket is `/run/dind-sock/docker.sock` on a separate
  harness volume shared with the worker container (production: one `dind-sock`
  emptyDir for both; the socket persists across a sidecar restart, the rest of
  `XDG_RUNTIME_DIR` does not here).
- Rootless: a labelled one-shot helper container chowns the three fresh volumes
  (data root, `/data/runner`, socket) to 1000:1000 (in a pod, `fsGroup`/subPath setup does this).
- Rootful runs `dockerd --host=tcp://127.0.0.1:2375 --tls=false --storage-driver=vfs`.
- The worker container does not mount `/data/runner`; act on the tree with
  `pe_inner_sh` (as the daemon uid) or `pe_root_sh`. The worker is a docker CLI
  client only (it runs the pinned DinD image's CLI with `sleep` as its command).
- The container restart and replace stand in for a sidecar restart and a new
  pod; kubelet, cgroup and pod-lifecycle behaviour is not reproduced. A
  `docker restart` keeps the container's writable layer; kubelet would create a
  fresh container instance (`pe_daemon_container_recreate`).
