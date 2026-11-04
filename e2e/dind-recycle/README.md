# Anonymous volume prune proof

From the repository root:

```sh
task test:dind-volume-prune
```

Requires Bash, Docker access with permission to start privileged containers, and
GNU-compatible `timeout`. Missing Docker, setup errors, an API below 1.42, failed
assertions, and cleanup errors fail the run; there are no skips.

The pinned Docker 29 DinD image uses a fresh anonymous `/var/lib/docker` volume,
no host socket mount, and no published ports. All fixture and prune commands use
`docker exec`, an empty client environment, and the inner Unix socket.
Images are pulled on cache misses without passing credentials into DinD.
The inner Alpine image is pinned to the repository's existing digest.

The inner `dockerd` runs with `--debug`, so `docker logs` of the owned container
records every request path. The test prints the inner server's Version, GitCommit,
APIVersion and MinAPIVersion and the client APIVersion (so the engine build can be
compared with the sources the design cites), then makes one server API check
(>= 1.42).

The prune runs exactly as the worker's volume leg does: `docker volume prune -f`
with `DOCKER_API_VERSION=1.42` in an otherwise empty client environment. Before it,
a fail-closed probe runs the same command with one minor below the daemon minimum
(1.39 against minimum 1.40). It must exit non-zero with the daemon's "too old"
error, remove nothing (every fixture volume stays), and leave no volumes/prune
request in the daemon log other than the rejected one, so there is no retry at
another version. After the real prune the log must show `POST /v1.42/volumes/prune`
and no other volumes/prune request path.

Assertions: an orphan anonymous volume created immediately before the prune is
removed (no age delay), as is the earlier orphan. Retained: the named volume;
anonymous volumes of a running and a stopped container; a never-started
`docker create -v` holder's volume (reference first); and an anonymous volume with
local-driver options (`--mount ...volume-opt=type=tmpfs`, container removed without
`-v`, asserted anonymous-labelled before and after) because the engine excludes it.
An unrelated volume-less running container and the stopped container keep their
state. Removal first: after the prune, `run -v <removed-orphan-id>:/fixture` finds
`/fixture/proof` absent and the re-created volume has no anonymous label.

This test is sequential. Real concurrency (a create or start racing the prune)
rests on the cited engine source contract (reference acquisition and removal share
the per-name volume lock, and removal rechecks references under it), not on this
test; an accepted-but-not-yet-visible create is not a reference.

Docker calls have individual timeouts within a 600-second test deadline.
Readiness has at most 30 probes. Cleanup has separate 10/30-second caps, verifies
the ownership label, and removes only the exact `dind-prune-test-<random>`
container with `docker rm -fv`, including its anonymous DinD root.
Cleanup preserves an earlier failure status. No host prune or Compose teardown
is used. This checks Docker prune semantics, not the production maintenance gate or
Kubernetes lifecycle. Real-cluster acceptance remains maintainer-owned after release.
