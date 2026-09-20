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
worker's — a heartbeat, nothing pod-level. If one never comes online, that's
one of the checks an admin can now see cross-user in **Admin → Health**
(`fleet.roll`, see [Admin health](admin-health.md)) — it names the blocking
container and reason for a stuck roll, across every owner's fleet, without
anyone needing to check the pod by hand. (A worker that came online once and
then failed an upgrade also shows in your own **Settings → Workers** — see
[Worker versions and upgrades](worker-upgrades.md).)

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

## My Codex run says "no Codex-capable worker is online"

A hosted worker only claims a Codex run once it can actually run one. Two
things can leave it unable to, and both mean the run stays queued rather than
being claimed and then failing partway through: the fleet may not have the
opt-in uid-split profile Codex needs turned on, or a node's kernel may lack
Landlock while the fleet requires it. This applies to a full Codex run and
to Codex judge/review advice alike — ask your admin to check the
[operator knobs](./configuration.md#controller) (`UZI_WORKER_UID_SPLIT`,
`UZI_CODEX_COMMAND_SANDBOX`) that control this.

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

## Message outbox

A hosted worker gets the same [message outbox](./worker-setup.md#message-outbox) as
any worker: an api outage of any length no longer costs a run its message feed,
since the worker spills to a durable on-disk outbox after a sustained outage and
replays it in order once the api is back — as long as the retained data stays
within the outbox's configured quotas (`WORKER_OUTBOX_RUN_MAX_BYTES`,
`WORKER_OUTBOX_MAX_BYTES`, `WORKER_OUTBOX_SPILL_BUFFER_BYTES`); beyond them, some
message frames are dropped and replayed as contiguous per-seq gap markers rather
than the original messages. One caveat is worth restating here rather
than at length, and which profile the worker runs decides it. On the default
single-uid profile a hosted worker runs the `#58` posture, so the model process
shares the worker's uid and could read, forge, truncate, or delete its own outbox —
there the outbox protects against the outage, not against a hostile model. On the
opt-in uid-split Codex profile the model runs under a distinct uid while the outbox
tree stays worker-owned and `0700`-private, so a hostile model can no longer read or
tamper with it (see ["no Codex-capable worker is online"](#my-codex-run-says-no-codex-capable-worker-is-online)
for that profile). See [worker-setup.md](./worker-setup.md#message-outbox) for the
full caveat and the tunable quotas.

## Surviving an api restart mid-run

The message outbox above is about the run's message feed surviving an
outage; this is about the run's own status. A hosted worker reports, on
every heartbeat, which run-lane attempts it's actually executing and in
which phase — running, or waiting at a plan-approval or a clarifying-question
gate. Once the api is back up (after a boot grace during which it holds off
declaring workers stale, so a worker that only lost the api and not its own
health isn't wrongly re-queued), it uses that report to restore a run the
outage flipped to `queued` back to its exact phase within one heartbeat,
without spending the run's re-queue budget or opening a new custody hold. A
run genuinely re-queued during the outage has that charge refunded once it's
back. See [configuration.md](./configuration.md#server-api) for
`SWEEPER_BOOT_GRACE` and the related knobs.

The same write-ahead protection covers a run's actual outcome, not just its
status. If a run-lane attempt (an issue run, a judge, or a review — chat is
excluded) finishes `completed` or `failed` while the api can't be reached,
the worker journals that outcome to disk before it ever tries to send it,
in the same durable tree the message outbox above uses. Once the api is
back, the journal replays after the run's own messages have caught up, so
a run that finished during the outage still lands its MR and its final
state exactly once — never redone, even if a duplicate claim attempt races
it. If the worker container itself restarts mid-outage, it resolves every
journaled outcome before claiming anything new, so the outcome still lands
even though the process that produced it is gone. If the api permanently
refuses a journaled outcome, it's never silently dropped: the run shows it
as a held outcome, and only you can clear it, by cancelling the run and
confirming you want to discard what's held. See
[worker-setup.md](./worker-setup.md#message-outbox) for the outbox
mechanics and the same-uid caveat, which applies to a journaled outcome
exactly as it does to the message feed.

## How this differs from running your own worker

A worker you run yourself is a container on hardware you control: you copy a
join token, start it, and keep it running. A hosted worker is the same
protocol against a container the cluster manages for you — provisioning and
deleting from the UI stand in for starting and stopping it yourself. Both
kinds work identically once online: same claim/run behavior, same resource
gauges, same [concurrency](./worker-setup.md#concurrent-runs) rules.
