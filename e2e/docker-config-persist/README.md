# docker-config-persist

Issue #4416 M3 proof: a `docker login` in one sandboxed Codex command persists for the run's later
commands, through the per-run `<runHome>/.docker` directory that `uzi-codex-command-sandbox`
creates, forces to `0700` and Landlock-grants (`--docker-config`, see ADR-1598).

```
./e2e/docker-config-persist/run.sh
```

It needs a Docker daemon reachable through `DOCKER_HOST` (the worker's DinD sidecar) and egress to
pull the pinned images. It builds the real worker image from `agent/templates/base/Dockerfile` at the
current tree, adds a thin layer carrying `agent/test/codex-docker-config-linux.test.ts` (a bind
mount source would resolve on the daemon host, so no repo file is bind-mounted), starts a
password-protected `registry` published on `127.0.0.1:<port>` of the daemon host, seeds a small
private image into it, and runs the suite as the worker uid (10001) through the real entrypoint with
`--network host`, so the worker container, the daemon and the registry share one loopback.

Per mode (`off`, plus `required` when `uzi-codex-command-sandbox --probe` exits 0):

| step | proves |
| --- | --- |
| a | `docker pull` of the private image is refused before login |
| b | `docker login` in one command succeeds; `.docker` ends uid 10003, mode 0700 |
| c | a later command's `docker pull` succeeds |
| d | a later command's `docker build` with `FROM <private image>` succeeds after `docker rmi` |
| e | two concurrent first commands in a fresh run home both succeed |
| f | a planted `.docker` owned by another uid degrades: the command runs, stderr has the degrade line, `DOCKER_CONFIG` is gone |
| g | (required only) another run's `.docker` is not writable from this run's sandbox |

A Claude leg runs the same login-then-pull as `runner` (10002) with `DOCKER_CONFIG` set explicitly;
the value `buildSdkEnv` sets; that wiring is pinned by `agent/test/sdk-env.test.ts`, not by this leg. When Landlock is unavailable the script prints
`required: NOT PROVEN` and does not count it as passing.

Isolation: every container and image is named `dcp-test-<hex>-*`, containers are labeled
`dcp-test.owner=<hex>`, and the EXIT trap removes only those by exact name after checking the label.
It never prunes and never touches resources it did not create. Credentials are generated at runtime
and never printed. Knobs: `DCP_WORKER_IMAGE` (reuse an image), `DCP_OUT`, `DCP_REGISTRY_PORT`,
`DCP_TEST_TIMEOUT`, `DCP_BUILD_TIMEOUT`.
