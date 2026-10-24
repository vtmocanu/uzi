# Pod-epoch real-daemon harness (PRD #1860, M0)

Controlled Docker-in-Docker daemons for experiments on what a sidecar restart
or a successor pod does to a daemon's state. Runs **outside all gates**: not in
`task gate`, CI or any Taskfile recipe. Run it by hand.

```sh
bash e2e/pod-epoch/smoke.sh all        # or: rootless | rootful
```

Prints `SMOKE <posture> PASS` per posture and exits nonzero on any failure.

## Requirements

- `docker` CLI reaching an outer daemon that permits `--privileged` containers
  (`DOCKER_HOST`, e.g. `tcp://127.0.0.1:2375`), and `timeout` (coreutils).
- Network to pull the pinned images on a cache miss.
- Verified against Docker Engine 29.8.0 (go1.26.8, API 1.56) in both postures.

## Library (`lib.sh`, sourced)

`pe_init <posture>`, `pe_volumes_create`, `pe_daemon_create_start`,
`pe_inner <cap> <docker args>`, `pe_inner_sh <cap> <script>`,
`pe_daemon_restart` (same container, sidecar restart),
`pe_daemon_replace` (remove, then create a new container on the same two
volumes: successor pod epoch), `pe_events_start <file>` / `pe_events_stop`
(exit status in `PE_EVENTS_RC`), `pe_server_info`, and an EXIT-trap cleanup
that preserves the original exit status. Every experiment call has a cap within
a shared deadline (`PE_DEADLINE_SECONDS`, `^[1-9][0-9]{0,4}$`, default 900 per
posture); cleanup and events teardown use fixed caps outside it.

`PE_EVENTS_RC` is the raw exit status of the local exec; `PE_EVENTS_END` says
why: `stopped` (we ended it), `ended` (it ended itself, e.g. daemon restart) or
`deadline` (status 124). The stream is started with `--since` taken inside the
daemon container, and the in-container `docker events` pid is killed and
verified gone on stop. A second start while one is active fails.

The library requires the caller's `set -euo pipefail` and one posture per
(sub)shell; `pe_init` fails if called twice in the same shell.

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
| endpoint | `unix:///run/dind/docker.sock` | `tcp://127.0.0.1:2375` |

Deviations:

- Rootless runs the explicit command
  `dockerd --host=unix:///run/dind/docker.sock --storage-driver=vfs` (nested
  overlay is not assumed). Flags only would make the image entrypoint add
  `--host=tcp://0.0.0.0:2375` and publish it on the shared bridge with TLS off.
  Production rootless keeps `/certs` TLS on 2376; the harness binds unix only.
  After every daemon start the harness checks (read-only, via `/proc/net/tcp*`)
  that nothing listens on a non-loopback TCP port and fails otherwise.
- The rootful posture omits production's MTU cap and the `--config-file`
  default-network-opts; it uses the image defaults.
- Rootless mounts a tmpfs at `/run/dind` (uid/gid 1000) in place of the pod
  `emptyDir`; it is cleared on a container restart.
- Rootless: a labelled one-shot helper container chowns the two fresh volumes to
  1000:1000 (in a pod, `fsGroup`/subPath setup does this).
- Rootful runs `dockerd --host=tcp://127.0.0.1:2375 --tls=false --storage-driver=vfs`.
- Both daemons live in one container; production has a worker container sharing
  `/data/runner` with the DinD sidecar. Use `pe_inner_sh` to act on that tree
  as the daemon uid.
- The container restart and replace stand in for a sidecar restart and a new
  pod; kubelet, cgroup and pod-lifecycle behaviour is not reproduced.
