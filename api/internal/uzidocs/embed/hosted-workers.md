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

**Size** picks how much CPU, memory, and disk the worker gets: **S**, **M**,
or **L** (the default), with larger presets providing more capacity. The provision
form shows the exact numbers next to each option when you pick — they live
there, not here, so they can't say something different from what you'll
actually get. Every size also gets the same 20Gi tools cache (`/nix`),
regardless of size or type — the one number that doesn't change with your
choice.

An ephemeral (run-bound) worker, the kind uzi provisions for a single run and
removes once that run finishes and its idle lease ends (see [Ephemeral worker
lease](#ephemeral-worker-lease)), is the exception on `/data`: its `/data` is
its own setting, 20Gi by default and set by the operator, whatever size the
worker runs at. Its CPU and memory still follow its size.

New ephemeral hosted workers default to **M** (1 CPU / 8Gi memory
requested, 4 CPU / 12Gi memory limits), saving 6Gi of memory reservation per
worker compared with **L**. New persistent workers still preselect **L**
(1 CPU / 14Gi requested, 4 CPU / 20Gi limits) for more memory headroom.
Both **M** and **L** now allocate 25Gi of persistent `/data`; ephemeral data
retains its independent 20Gi default. Existing persistent **M** data volumes
stay at 10Gi until reprovisioned: the controller does not resize existing PVCs.
Existing workers keep their stored size; existing **M** workers receive the
1-CPU request and 4-CPU limit when the controller next rolls their pod spec.
Operators set `workers.ephemeralDefaultSize` in the chart (`s`, `m`, or `l`);
invalid values fail rendering, and so does setting
`UZI_EPHEMERAL_DEFAULT_SIZE` in `api.config` or `api.secretEnv` (move it to
`workers.ephemeralDefaultSize`). Non-chart deployments can set
`UZI_EPHEMERAL_DEFAULT_SIZE`, which defaults and falls back to `m`.
Users can choose another size when provisioning a persistent worker.

**Every size costs you the same, 1 of your quota below**; there's
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

The Workers page's **Ephemeral workers → Docker-capable** preference adds
Docker when auto-provisioning for repositories your admin allows; see
[the triggers, scope and warm-reuse policy](./scheduling.md#auto-provisioning-a-worker-for-an-unmet-capability).

## Ephemeral worker lease

When an ephemeral worker finishes its run it is not removed at once. It stays
idle for a **lease** so a follow-up run can reuse the clone, build caches and
tools it already warmed up, instead of waiting for a new worker to provision
and download everything again.

- **What is reused.** The same worker, with its working clone and caches. Nothing
  about the earlier run's result or credentials carries over.
- **Who can take it.** Only a run with the same owner, the same repository and
  the same branch. A run for any other repository, branch or owner never
  claims a leased worker. A leased worker does not count as a free slot for
  those runs.
- **Which runs.** Issue runs, `mr_rework` runs and `ci_fix` runs that continue
  the same branch (a `ci_fix` run's branch is the ref of the failure it fixes). Other kinds (chat, task, job, prompt, judge and self-improve runs), a
  run without a repository, a run on an egress profile, and a worker on the
  isolated research lane never reuse a lease.
- **How long.** 2 hours by default, which is also the maximum: the operator sets
  `UZI_EPHEMERAL_LEASE` (chart `workers.ephemeralLease`) to anything up to
  `2h`. `0` turns the lease off, so an ephemeral worker is removed when its run
  finishes, as before. A value above 2h or a malformed one refuses to boot the
  api.
- **At the cap.** A leased worker still counts toward your per-user ephemeral
  cap. If a new run needs a slot and you are exactly at the cap, the oldest
  releasable leased worker (one that is not busy and holds no custody hold) is
  evicted first. An owner over the cap is refused instead.
- **What ends it early.** A cordon, a roll to a new worker image, a restart of
  the worker, or the owner deleting it. A cordoned or restarted worker never
  keeps a lease. When the lease runs out the worker is removed by the normal
  cleanup.

`uzi worker list` shows a leased worker as `online (ephemeral) (leased, 1h12m
left)`, with the time remaining. `uzi worker list --json` (and the admin
fleet list) carries `ephemeral_lease_expires_at`, present only while the lease
is live. The lease branch is not shown.

## My Codex run says "no Codex-capable worker is online"

A hosted worker only claims a Codex run once it can actually run one, so an
unmet precondition leaves the run queued rather than claimed and then failing
partway through. This applies to a full Codex run and to Codex judge/review
advice alike. Four things can hold a worker back:

- The fleet does not have the opt-in uid-split profile Codex needs turned on.
  Ask your admin to enable it (`UZI_WORKER_UID_SPLIT`).
- The fleet opted into Landlock (command sandbox `required`) and a node's
  kernel lacks it. Either move the fleet onto Landlock-capable nodes, or set
  the command sandbox back to its default `off`, or to `best-effort`
  (`UZI_CODEX_COMMAND_SANDBOX`).
- The fleet opted into Landlock (`required` or `best-effort`) and the Landlock
  probe fails outright: it could not run, or the kernel returned an unexpected
  result. This is distinct from a clean "unavailable" and is fatal in both
  modes: check the node's kernel/Landlock support and that the command-sandbox
  binary is present on the worker image, or set the mode back to `off`.
- The worker's Codex installer receipt is not intact (the Codex runtime is
  missing or corrupt on the image). Re-provision or rebuild the hosted worker
  so the runtime is reinstalled; an operator knob does not fix this.

The [operator knobs](./configuration.md#controller) that control the first two
are `UZI_WORKER_UID_SPLIT` and `UZI_CODEX_COMMAND_SANDBOX`.

## My Codex run says "no worker supporting custom Codex models is online"

This is a narrower version of the message above, and only shows up when the
run's effective Codex model is a [custom worker-root
ID](./worker-model.md#custom-codex-model) rather than a curated one. A
curated Codex model (`gpt-6-astra`, `gpt-5.6-sol`, `gpt-6-sol`) needs only
the ordinary Codex capability covered above; a custom root model additionally
needs a worker advertising `codex_custom_model_v1`, which only ships with a
newer worker image.

- Ask your admin to upgrade or roll the hosted worker fleet to an image that
  advertises `codex_custom_model_v1`. An old worker never claims the run and
  never silently substitutes `gpt-6-astra`; it stays queued instead.
- Or switch to one of the curated Codex models in [Settings → Run
  defaults](./worker-model.md#harness-and-worker-models) if you don't need
  the custom ID right now.

A resumed run can stay tied to its previous worker until the configured
affinity ceiling (two hours by default), even if another capable worker
comes online. This preserves unpublished work on the previous worker's
volume before a cold move is allowed.

## Disk self-heal

A hosted worker also reports its disk usage now — the same CPU/memory gauges
in **Settings → Workers** and the Dashboard's "Worker load" card gain a Disk
bar per volume (`/nix`, `/data`); see [Resource stats and
sizing](./worker-setup.md#resource-stats-and-sizing).

Two things now self-heal without you deleting and re-provisioning by hand:

- **A tools cache smaller than the current size** (provisioned before a size
  bump, like the one above) gets reconciled to the current size automatically:
  only the `/nix` cache PVC is recycled, and the `/data` PVC is preserved.
  On Docker workers, Deployment replacement still loses run-workdir emptyDir.
- **A volume that fills up** (at or above 90% used, sustained across a couple
  of heartbeats) gets recycled: the worker is drained first if it's busy —
  same cordon behavior as above — then **both** its volumes are deleted and
  re-provisioned fresh: the `/nix` tools cache re-downloads, and everything on
  the `/data` workspace is permanently lost. A worker recycled this way shows
  the same draining/cordoned pills while it happens.

A **persistent Docker-capable worker** also has a separate DinD-only
pressure recycle. It preserves the `/nix` and `/data` PVCs, worker UUID,
and join Secret, but **loses all DinD state, including named Docker volumes,
and the shared run-workdir emptyDir** when the Deployment is replaced.
Deliverables belong in git, published checkpoints, or captured work.
**This destructive behavior defaults on for existing installs on upgrade.**

DinD maintenance waits for every run to finish (`completed`, `failed`, or
`cancelled`); parked, paused, approval, input, and follow-up waits block
cleanup. A pending drain refuses new run/chat work while letting this
worker's own parked runs resume to finish. It then fences claims and
requires fresh custody clearance and zero local activity before gated
anonymous-volume pruning or stop. No timer, forced park, or force-roll
override bypasses this DinD gate. The legacy `/nix`+ `/data` recycle and
ordinary rolls retain their current drain-deadline/force override behavior.

If legacy and DinD pressure coincide, the legacy recycle goes first; the
pending DinD intent resumes on a later tick before another legacy recycle.
The legacy cooldown does not delay DinD cleanup, whose cooldown reads its
own PVC's creation time. Older workers are report-only; ephemeral Docker
workers report metering/admin pending state but use terminal teardown
instead of prune/recycle. See
[Docker inside a worker](./worker-docker.md#dind-scratch-and-pressure-recycle).

These are cluster-driven; there is no self-heal button. An admin can set
`UZI_WORKER_DISK_RECYCLE_ENABLED=false` to opt out before DinD stop;
an operation already stopping finishes safely. Upgrade the chart's Docker
controller Role with the controller: replacement readiness requires
`apps/replicasets` `list` in the Docker namespace only, with no Secret
read permission. An observation limit or list failure leaves maintenance
pending/fenced rather than granting cleanup; see
[ADR-1759](../adr/1759-dind-data-metering-and-prune.md#d9--fresh-bounded-observation-and-replacement-readiness).

The legacy nix/data full-volume recycle is the **last resort**. Before that data volume gets anywhere
near it, a run's own build caches are bounded and reclaimed on their own —
see [Worker disk safety](./worker-setup.md#worker-disk-safety-prd-1809) for
the cache drop, in-run cache cap, periodic reclaim, admission stop, and the
bounded `data_volume_full` park that exist to make this recycle rare.

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

A restart in the narrower window before that journal exists is also covered, once in the initial recovery episode (issue #1742). Right after the agent's final result, and before the worker starts finalizing (fetching the branch back, pushing, opening the MR), the worker writes a small authenticated finalize-pending record; it is not an outcome. If the worker container restarts while finalizing, it reports that record on register, and the api re-queues the run once even when that episode's re-queue budget is already spent, provided `RUN_MAX_REQUEUES > 0` and the lifetime finalize-resume marker is unused. Owner-started worker-death recovery episodes get no extra allowance. The run resumes through the ordinary claim path and completes only through the normal completion path at a later claim generation (the next claim, or the one after it when that claim first captures retained work and parks) (the claim-generation fences, plus the completion permit where the run is interlocked). A crash before that record is durable, or a slow restart without timely attested proof, uses ordinary disposition: requeue under the episode cap; at exhaustion, hold for the owner when recorded recovery evidence or uncertainty exists, otherwise fail `worker_lost` (not proof that no unrecorded worker work survives). The extra resume is once per run, never renewed by owner Resume, and off at `RUN_MAX_REQUEUES=0`, and `uzi run recovery` reports a source it could not archive as retained `source_only` custody rather than offering an export. See [ADR-1742](../adr/1742-finalize-resume-allowance.md).

## How this differs from running your own worker

A worker you run yourself is a container on hardware you control: you copy a
join token, start it, and keep it running. A hosted worker is the same
protocol against a container the cluster manages for you — provisioning and
deleting from the UI stand in for starting and stopping it yourself. Both
kinds work identically once online: same claim/run behavior, same resource
gauges, same [concurrency](./worker-setup.md#concurrent-runs) rules.

## Current Codex worker baseline

The current Codex worker uses runtime 0.159.3 and defaults to `gpt-6.1-sol`
when no worker-model override is set. Explicit model preferences remain
unchanged. Both harnesses inherit medium reasoning effort; Claude and Codex
keep separate overrides. See [Reasoning effort](worker-effort.md).

Every Codex claim requires `codex_runtime_v2`, so an older worker cannot
silently use the old model/catalog or ignore the requested effort. Codex runs
queue until the worker image rolls, on hosted workers and compose alike.
