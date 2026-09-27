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
worker plus a Docker-in-Docker (DinD) sidecar that supplies the daemon
(rootless by default; see below).

## Trust boundaries and Docker tiers

The sidecar is its own container (compose) or its own sidecar container in
the worker's pod (hosted/k8s) — never the host's Docker. It:

- runs **rootless** by default (a breakout lands as an unprivileged,
  userns-remapped uid, never host root). A hosted cluster whose nodes cannot
  run rootless DinD may opt into a privileged, non-rootless sidecar instead.
  There a container breakout lands as root on the node and can read the
  node's and co-scheduled pods' secrets, including other users'
  credentials, so the chart requires an explicit acknowledgement
  (`acknowledgeNonRootlessNodeRoot`) to enable it;
- runs in its own mount namespace and mounts none of the worker's join
  token, `/data` cache, or `/nix`, so a container the agent launches
  (`docker run -v ...`) cannot bind-mount your credentials or those volumes;
- there is no host `docker.sock` anywhere in the picture, on either track.

**One shared directory on hosted workers.** On a hosted (k8s) worker the
sidecar also mounts the shared run working directory (`/data/runner`), so
builds can see the checkout. That directory holds the clone of *every* run
the worker is executing at the time, so a container launched from one run
can read or write another concurrent run's clone. Hosted workers only
ever run their owner's runs, so the exposure stays within your own runs;
it is an accepted part of the docker tier. Under docker compose the sidecar
shares only its daemon socket and its own data, not the run directory.

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

## dind-data metering and automatic pruning

On a **hosted (k8s)** docker-capable worker, the daemon's images and build
cache live on their own PVC (`dind-data`), which the worker itself never
mounts — only the daemon and a small, unprivileged `dind-meter` sidecar can
see it. `dind-meter` samples the volume's fill level (bytes and inodes) every
30s and publishes it read-only for the worker to display as the **Disk
dind** gauge (see [Worker setup](./worker-setup.md#resource-stats-and-sizing)).

The worker also watches that sample and, when it stays at or above 85% of
bytes or inodes across two fresh samples, runs a short, fixed sequence of
cache prunes against the daemon: `docker image prune -f`, then
`docker builder prune -f`, and — only if the volume is still over the
threshold — `docker image prune -a -f --filter until=24h` (any unused image
built more than a day ago). It only does this once the worker is provably
idle: no active run, chat, or claim in flight, and the api has confirmed on a
fresh heartbeat that the worker holds no unpublished work; any doubt skips
the attempt rather than risking one. Look for `dind-prune-completed`,
`dind-prune-failed`, `dind-prune-insufficient`, or `dind-prune-deferred` in
the worker's logs to see what it did.

**What this never touches**: docker volumes, running or stopped containers,
and the `/nix`/`/data` disk-pressure recycle (which only ever deletes and
re-provisions those two, never `dind-data`; see
[ADR-837](../adr/0837-worker-disk-lifecycle.md)). If the cache is still over
the threshold after a prune (`dind-prune-insufficient`), there is currently
no automatic recycle of `dind-data` — the gauge stays elevated and the only
way to reclaim the volume is the manual fallback below.

**Manual fallback.** Delete the worker and reprovision it (**Settings →
Workers**, or `uzi worker delete` + re-provision): this tears down the pod
and its `dind-data` PVC and re-creates it empty, the same reclaim path a
plain worker's `/nix`/`/data` volumes get automatically. A compose worker has
no meter or automatic prune (`dind-meter` is a hosted-only sidecar); reclaim
its `dinddata` volume the same way as `agentnix` — see
[Worker setup](./worker-setup.md#resource-stats-and-sizing).

## Cost

A docker sidecar budgets roughly **1-2 GiB memory and 1 CPU** on top of the
worker's own, plus its own storage for pulled images and build cache — see
[Worker setup](./worker-setup.md#resource-stats-and-sizing) for the numbers
next to a plain worker's, and for reclaiming that storage.
