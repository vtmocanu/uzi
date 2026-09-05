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

1. Open **Settings → Workers**, then the **Add a worker** tab. If hosting is
   available, the **Hosted workers** card is the first card there, above
   **Register your own worker**.
2. Pick a **type** and a **size** (below), optionally tick **Docker-capable**
   (see [Docker inside a worker](./worker-docker.md) — needs an instance
   whose admin has turned the docker tier on), then **Provision**.
3. The new worker appears under **Your workers** right away and comes online
   on its own, usually within a few seconds — nothing to run, nothing to copy.

A freshly provisioned hosted worker derives which [Anthropic
token](./anthropic-token.md) it spends the same way any other new worker
does: **Auto-select from the pool** if you have at least one pooled token,
else your default token. There's no pin option on this form — bind it to a
particular token afterwards from **Settings → Workers** if you want one.

## Type and size

**Type** picks the worker image — the same choice as
[Worker templates](./worker-setup.md#worker-templates) for a self-run worker:
`base` (Node + git, most repos) or `jvm` (`base` plus a JDK).

**Size** picks how much CPU, memory, and disk the worker gets: **S**, **M**
(the default), or **L**, each step roughly doubling the last. The provision
form shows the exact numbers next to each option when you pick — they live
there, not here, so they can't say something different from what you'll
actually get. Every size also gets the same 20Gi tools cache (`/nix`),
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
refused until you delete one. At quota, the card's "delete one to provision
another" text is a link back to **Your workers**, so you don't have to hunt
for the tab yourself. See [Admin settings](./admin-settings.md#hosted-worker-quota)
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
no in-app diagnostic yet; ask an admin to check the pod directly. (A worker that came online
once and then failed an upgrade DOES have one now — see
[Worker versions and upgrades](worker-upgrades.md).)
(`kubectl -n <worker namespace> get pods`).

## Draining and cordoned

When the cluster needs to roll a hosted worker that is still busy, it doesn't
kill it out from under your runs. Instead it **cordons** (drains) the worker:
the worker keeps finishing the runs it already has, but stops claiming new
ones, then the cluster restarts it once it's idle.

A cordoned worker shows a dashed **draining** or **cordoned** pill next to its
status in the Workers list — **draining** while it's still finishing runs,
**cordoned** once it's idle and just holding until the restart. `uzi worker
list` shows the same thing folded into the STATUS column, e.g. `online
(draining)` or `offline (draining)`.

This is what explains an otherwise confusing moment: a worker that's online
and shows a free run slot, but isn't picking up a run sitting in the queue.
That's expected while it's cordoned — it isn't a bug, and it isn't stuck. It
resumes claiming runs on its own once the roll finishes. There's no manual
way to cordon a worker yourself; it's driven entirely by the cluster.

## Disk self-heal

A hosted worker also reports its disk usage now — the same CPU/memory gauges
in **Settings → Workers** and the Dashboard's "Worker load" card gain a Disk
bar per volume (`/nix`, `/data`); see [Resource stats and
sizing](./worker-setup.md#resource-stats-and-sizing).

Two things now self-heal without you deleting and re-provisioning by hand:

- **A tools cache smaller than the current size** (provisioned before a size
  bump, like the one above) gets reconciled to the current size automatically:
  only the `/nix` cache is recycled, and the `/data` workspace is preserved.
- **A volume that fills up** (at or above 90% used, sustained across a couple
  of heartbeats) gets recycled: the worker is drained first if it's busy —
  same cordon behavior as above — then **both** its volumes are deleted and
  re-provisioned fresh: the `/nix` tools cache re-downloads, and everything on
  the `/data` workspace is permanently lost. A worker recycled this way shows
  the same draining/cordoned pills while it happens.

Both are cluster-driven, like cordoning; there's no button for either. An
admin can turn the self-heal off entirely via chart config if it's ever not
wanted.

## How this differs from running your own worker

A worker you run yourself is a container on hardware you control: you copy a
join token, start it, and keep it running. A hosted worker is the same
protocol against a container the cluster manages for you — provisioning and
deleting from the UI stand in for starting and stopping it yourself. Both
kinds work identically once online: same claim/run behavior, same resource
gauges, same [concurrency](./worker-setup.md#concurrent-runs) rules.
