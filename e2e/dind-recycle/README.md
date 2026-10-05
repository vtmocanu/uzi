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

After one server API version check, the test runs exactly `docker volume prune -f`.
It proves that an unreferenced anonymous volume disappears while an unreferenced
named volume and anonymous volumes referenced by running and stopped containers
survive. Anonymous IDs, labels, pre-prune existence, and holder states are checked.

Docker calls have individual timeouts within a 360-second test deadline.
Readiness has at most 30 probes. Cleanup has separate 10/30-second caps, verifies
the ownership label, and removes only the exact `dind-prune-test-<random>`
container with `docker rm -fv`, including its anonymous DinD root.
Cleanup preserves an earlier failure status. No host prune or Compose teardown
is used. This checks Docker prune semantics, not the production maintenance gate or
Kubernetes lifecycle. Real-cluster acceptance remains maintainer-owned after release.
