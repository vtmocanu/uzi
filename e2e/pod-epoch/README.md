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
that preserves the original exit status. Every call has a cap within a shared
deadline (`PE_DEADLINE_SECONDS`, default 900 per posture).

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

- Rootless passes `--storage-driver=vfs` to the image entrypoint (nested
  overlay is not assumed); override with `PE_ROOTLESS_DOCKERD_FLAGS`.
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
