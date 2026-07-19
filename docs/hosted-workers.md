---
title: Hosted workers
order: 62
audience: user
---

# Hosted workers

A **hosted worker** is a worker whose container the cluster runs for you —
same [worker](./worker-setup.md) in every other respect (it claims your runs,
shows up in **Settings → Workers**, reports online/offline/busy), except
there's no join token to copy and no container to start yourself. It's only
available if your admin has turned hosting on for this instance.

## Provision one

1. Open **Settings → Workers**. If hosting is available, a **Provision a
   hosted worker** card appears above your worker list.
2. Pick a **type** and a **size** (below), optionally tick **Docker-capable**
   (see [Docker inside a worker](./worker-docker.md) — needs an instance
   whose admin has turned the docker tier on), then **Provision**.
3. The new worker appears in your list right away and comes online on its
   own, usually within a few seconds — nothing to run, nothing to copy.

## Type and size

**Type** picks the worker image — the same choice as
[Worker templates](./worker-setup.md#worker-templates) for a self-run worker:
`base` (Node + git, most repos) or `jvm` (`base` plus a JDK).

**Size** picks how much CPU, memory, and disk the worker gets: **S**, **M**
(the default), or **L**, each step roughly doubling the last. The provision
form shows the exact numbers next to each option when you pick — they live
there, not here, so they can't say something different from what you'll
actually get. Every size also gets the same 4Gi tools cache (`/nix`),
regardless of size or type — the one number that doesn't change with your
choice.

**M** is the default because it matches what a self-run worker gets out of
the box. **Every size costs you the same, 1 of your quota below** — there's
no personal cost to picking bigger. Size for your actual workload anyway,
not to save quota: **L** for large projects (a big JVM test suite, a large
`go build`), **S** for light repos. A hosted worker's CPU, memory and disk
are real capacity on a shared cluster, so an oversized pick you don't need
is capacity someone else's worker doesn't get.

## Your quota

You may hold a limited number of hosted workers at once (an admin-set quota,
shown on the provision card as "N of M used"); provisioning past it is
refused until you delete one. See [Admin settings](./admin-settings.md#hosted-worker-quota)
for how an admin sets it — including turning self-service off entirely.

## Deleting one

**Settings → Workers → Delete**, same as any worker, but a hosted delete asks
you to confirm first: unlike deleting a worker you run yourself (which only
revokes its token — the container keeps running until you stop it), deleting
a hosted worker **permanently destroys its disks**, including its tools
cache, so a replacement re-downloads everything from scratch. There's no
restart in this version — if a hosted worker seems stuck, delete and
re-provision it.

## Status

A hosted worker's online/offline/busy status works exactly like any other
worker's — a heartbeat, nothing pod-level. If one never comes online, there's
no in-app diagnostic yet; ask an admin to check the pod directly
(`kubectl -n <worker namespace> get pods`).

## How this differs from running your own worker

A worker you run yourself is a container on hardware you control: you copy a
join token, start it, and keep it running. A hosted worker is the same
protocol against a container the cluster manages for you — provisioning and
deleting from the UI stand in for starting and stopping it yourself. Both
kinds work identically once online: same claim/run behavior, same resource
gauges, same [concurrency](./worker-setup.md#concurrent-runs) rules.
