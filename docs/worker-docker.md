---
title: Docker inside a worker
order: 63
audience: user
---

# Docker inside a worker

Every worker ships the `docker` CLI (`docker`/`docker compose`/`docker buildx`,
see [Worker templates](./worker-setup.md#worker-templates)) on `PATH`, so a
repo whose tests or build call them doesn't fail with "command not found".
But the CLI alone can't run anything: there's no daemon behind it, and the
[guardrail](../ARCHITECTURE.md#guardrail-layers-the-primary-directive) denies
every `docker`/`docker compose` command outright until one is wired up.

To actually run containers, use a **docker-capable worker**: an ordinary
worker plus a rootless Docker-in-Docker (DinD) sidecar that supplies the
daemon.

## Trust: rootless, isolated, no host access

The sidecar is its own container (compose) or its own sidecar container in
the worker's pod (hosted/k8s) — never the host's Docker. It:

- runs **rootless** (a breakout lands as an unprivileged, userns-remapped
  uid, never host root);
- shares **only its daemon socket** with the worker, over its own mount
  namespace — it mounts none of the worker's join token, `/data`, or `/nix`,
  so a container the agent launches (`docker run -v ...`) can bind-mount
  none of the worker's own files;
- there is no host `docker.sock` anywhere in the picture, on either track.

This mount-namespace separation, not the guardrail, is what actually stops a
hijacked agent from reading your credentials through a container mount — see
[ARCHITECTURE.md](../ARCHITECTURE.md#agent-runtime-workers-runs-live-view)
for the full design.

## Bring one up: compose

```sh
UZI_DIND_SOCKET=/run/dind/docker.sock docker compose --profile agent --profile agent-docker up
```

This starts the ordinary `agent` service plus a `dind` sidecar; the worker
waits for the daemon to answer before it reports docker as available. Leave
`UZI_DIND_SOCKET` unset (the plain `--profile agent` default) and nothing
changes from an ordinary worker — no sidecar, no wait, docker stays absent.

## Bring one up: hosted (k8s)

On a hosted instance, tick **Docker-capable** on the
[hosted worker provision card](./hosted-workers.md#provision-one) — it needs
an instance whose admin has turned the docker tier on. The cluster runs the
daemon as a native sidecar in a dedicated, isolated namespace; there's
nothing to configure yourself.

## Cost

A docker sidecar budgets roughly **1-2 GiB memory and 1 CPU** on top of the
worker's own, plus its own storage for pulled images and build cache — see
[Worker setup](./worker-setup.md#resource-stats-and-sizing) for the numbers
next to a plain worker's, and for reclaiming that storage.
