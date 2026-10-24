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
| `exp-volumes-build.sh` | Q3 | plain versus bind-option `local` volumes; a build with the tree as context leaves no reference and a cache that outlives the tree |
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
  - `SCANNED pids=<n> mountns=<n>`.
  Limits: a mount namespace with no process in it is unreachable; only inodes
  recorded while the tree existed are matched after deletion. Positive control:
  a running container bound to the tree is seen as a strict mount hit in the
  task's mount namespace and as an `FD` hit on the task's cwd, with at least two
  mount namespaces scanned; a deleted directory held by a labelled cwd holder
  process is seen; `exp-volumes-build.sh` shows a bind-option volume's mount in
  dockerd's own mount namespace. The scanner is also run after removing the
  control container to show it then reports zero.
- **Inode retention** (`exp-stopped.sh`). A root process whose cwd is the tree
  keeps its inode allocated after `rm -rf`, because freed inode numbers are
  reused at once (observed). Without it "the recreated directory has a new
  inode" is not decidable from inode numbers.
- **Events window** (`exp-restore.sh`). `docker events --since <first-response
  SystemTime>` for 20 s from the worker container; supplementary, paired with a
  control that runs a container and requires at least 3 events from the same form
  of command. Container states are also re-inspected at the end and compared
  with the first-response inspect.
- **Markers** (`exp-readonly.sh`). Events are ordered, so a marker volume created
  before the snapshot and the control operations after it bound the query phase
  exactly; no sleep stands in for absence.
- **Restart detection** (`exp-restore.sh`). `time_to_eof_ms` is measured from just
  before the trigger command to the exit of the local client (20 ms polling) and
  includes `docker exec`/`docker restart` start-up (about 100 ms). The rootless
  socket is hard-linked before the trigger; the held inode stays allocated, so a
  socket file recreated by the next start must show a different inode.

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
  After every daemon start the harness checks (read-only, via `/proc/net/tcp*`)
  that nothing listens on a non-loopback TCP port and fails otherwise.
- The rootful posture omits production's MTU cap and the `--config-file`
  default-network-opts; it uses the image defaults.
- Rootless mounts a tmpfs at `/run/dind` (uid/gid 1000, `XDG_RUNTIME_DIR`: pid
  files, rootlesskit state) in place of the pod `emptyDir`; it is cleared on a
  container restart. The socket is `/run/dind-sock/docker.sock` on a separate
  harness volume shared with the worker container (production: one `dind-sock`
  emptyDir for both; the socket persists across a sidecar restart, the rest of
  `XDG_RUNTIME_DIR` does not here).
- Rootless: a labelled one-shot helper container chowns the two fresh volumes to
  1000:1000 (in a pod, `fsGroup`/subPath setup does this).
- Rootful runs `dockerd --host=tcp://127.0.0.1:2375 --tls=false --storage-driver=vfs`.
- The worker container does not mount `/data/runner`; act on the tree with
  `pe_inner_sh` (as the daemon uid) or `pe_root_sh`. The worker is a docker CLI
  client only (it runs the pinned DinD image's CLI with `sleep` as its command).
- The container restart and replace stand in for a sidecar restart and a new
  pod; kubelet, cgroup and pod-lifecycle behaviour is not reproduced. A
  `docker restart` keeps the container's writable layer; kubelet would create a
  fresh container instance (`pe_daemon_container_recreate`).
