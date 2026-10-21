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

   A Docker-capable worker claims repo-bearing runs only for repos on the Docker worker repo allowlist,
   which only an admin edits at **Admin → Instance → Docker worker repo allowlist**. A run on any other
   repo stays queued unless another eligible (for example non-Docker) worker can claim it; repo-less
   judge runs are not affected. See [Run health](./run-health.md#what-the-flags-mean).

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
its own setting, 40Gi by default and set by the operator, whatever size the
worker runs at. Its CPU and memory still follow its size.

New ephemeral hosted workers default to **M** (1 CPU / 8Gi memory
requested, 4 CPU / 12Gi memory limits), saving 6Gi of memory reservation per
worker compared with **L**. New persistent workers still preselect **L**
(1 CPU / 14Gi requested, 4 CPU / 20Gi limits) for more memory headroom.
New persistent **M** workers allocate 25Gi of `/data`, and **L** workers allocate
50Gi. Ephemeral data uses its independent 40Gi default. Existing data volumes
keep their current capacity: the controller does not resize existing PVCs.
Operators can [expand them in place](https://github.com/vtmocanu/uzi/blob/main/deploy/README.md#expanding-existing-worker-data-volumes)
on a StorageClass that supports volume expansion.
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
owner restart action in this version. Read the blocking diagnosis before deciding
whether deletion/re-provisioning is appropriate: it destroys the existing disks
and does not repair a shortage of node CPU or memory.

## Status

A hosted worker's online/offline/busy status works exactly like any other
worker's — a heartbeat, nothing pod-level. If one never comes online, that's
one of the checks an admin can now see cross-user in **Admin → Health**
(`fleet.roll`, see [Admin health](admin-health.md)) — it names the blocking
container and reason for a stuck roll, across every owner's fleet, without
anyone needing to check the pod by hand. Your own **Settings → Workers** also
shows roll failures, first-provision progress/failure, and **waiting for capacity**
when the scheduler reports a verified CPU/memory shortage. Capacity blockage
requires waiting, a smaller worker, or added capacity; Pending claims alone do
not establish storage health. See [Worker versions and upgrades](worker-upgrades.md).

## Draining and cordoned

Persistent hosted workers roll one canary first in each owner's placement and
capability pool. Workers waiting for their turn keep claiming runs on the old
image. A stuck replacement holds further replacements in that pool; it does
not trigger an automatic rollback. Size and worker type do not divide pools.

When a busy worker is admitted for replacement, the cluster **cordons** (drains)
it. It may finish its own pinned continuations and checkers, but refuses unrelated
new roots, unassigned children and new judge intake. An already-claimed judge
finishes under its existing budget. Actual spec-hash/generation drift
(image pin, template or size) and associated recycle wait **indefinitely for genuine
idle**, including run-bound workers outside pacing and calls with no pacing entry
or a plain drain reason. Any assigned nonterminal run blocks, including queued,
paused and all waits/gates. Idle guards still apply; custody remains separate even
when there are no assigned blockers.

An admitted busy drainer occupies pool concurrency (default one), supplies no
replacement coverage, canary proof or post-promotion proof, and can hold the pool
indefinitely. Healthy peers waiting for admission stay uncordoned and claimable;
legacy recycle cannot evade that policy. Separately fenced DinD maintenance remains
independent. A pool with one worker can have an outage during its roll. Waiting
indefinitely can delay capacity turnover and security fixes indefinitely.

`workers.drainDeadline` / `UZI_WORKER_DRAIN_DEADLINE` retains its `24h` default and
positive-only parsing, now only for independent **nondrifted** pressure/nix-size
maintenance. That maintenance can interrupt busy runs and delete /data unless
custody-held. Shortening the deadline no longer forces image upgrades; no knob is
added. An unchanged retained zero-replica Deployment can still be restored while
busy under its existing guard.

An operator can set `workers.forceRoll: true` as a **global emergency override**.
It bypasses pacing, holds, incarnation proof and busy deferral for drift, plus busy
deferral for independent maintenance. It may interrupt runs and lose all capacity;
it is neither targeted nor a recovery guarantee. Custody-held data and independent
DinD readiness/identity/pod-absence guards remain authoritative; DinD has no deadline
or ForceRoll bypass. Restore `false` after the emergency.

A cordoned worker shows a dashed **draining** or **cordoned** pill next to its
status in the Workers list — **draining** while it's still finishing runs,
**cordoned** once it's idle and just holding until the restart. `uzi worker
list` shows the same thing folded into the STATUS column, e.g. `online
(draining)` or `offline (draining)`.

This is what explains an otherwise confusing moment: a worker that's online
and shows a free run slot, but isn't picking up a run sitting in the queue.
That's expected while it's cordoned — it isn't a bug, and it isn't stuck. It
resumes claiming when the roll-owned drain can safely be released. Independent
maintenance or recovery fences can keep it cordoned. No timer automatically
uncordons it. There is no user-facing
manual cordon action; the cluster drives this process. Operators can follow the
[hosted roll verification runbook](./hosted-worker-roll-verification.md).

### See which assigned runs block an upgrade

Existing worker `upgrade_detail` and health evidence use API assignments, not the
worker's `reported_runs`. Worker detail shows up to five owner-safe assigned
nonterminal blockers with full run UUID, status, kind and continuation actions,
plus omissions. **Unavailable** evidence differs from **no assigned blockers**;
neither establishes custody clearance. Health retains at most five waiting-run
rows, each with one bounded blocker and omission information. These are read-only
diagnostics, not a new instance alarm. The existing owner wait/drain overlap reaches
danger at 24h; CLI danger exit `8` is unchanged and does not authorize replacement.

For a `pool_wait` blocker, the wait is for a spendable credential in the owner's
Anthropic token pool. The release wording is: "a pooled token becomes spendable, or the owner adds or selects a usable token / runs `uzi run resume-now`".
This action enables continuation; upgrade release still requires the assigned run to become terminal
or be unassigned. See [Run recovery](./run-recovery.md) for custody held separately.

The Workers page's **Ephemeral workers → Docker-capable** preference adds
Docker when auto-provisioning for repositories your admin allows; see
[the triggers, scope and warm-reuse policy](./scheduling.md#auto-provisioning-a-worker-for-an-unmet-capability).

An eligible worker can finish its own pinned plan checker while cordoned,
subject to maintenance fences. Plan checkers have separate capacity; see
[Cross-check slots](./worker-setup.md#cross-check-slots). An ephemeral worker
can check its bound lead on the same pod without provisioning an extra pod
when the lane is eligible and has space. Active children block lease entry,
teardown and reaping; checking the parent preserves its binding, including
legacy run-slot fallback.

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
  On Docker workers, this recycle keeps the DinD data volume (images, named
  volumes and build cache survive) and, on a worker already on the #3153
  layout, `/data/runner` (a subPath of the `/data` PVC); running DinD
  containers stop with the pod.
- **A volume that fills up** (at or above 90% used, sustained across a couple
  of heartbeats) gets recycled: the worker is drained first if it's busy —
  same cordon behavior as above — then **both** its volumes are deleted and
  re-provisioned fresh: the `/nix` tools cache re-downloads, and everything on
  the `/data` workspace is permanently lost. A worker recycled this way shows
  the same draining/cordoned pills while it happens.

A **persistent Docker-capable worker** also has a separate DinD-only
pressure recycle. It preserves the `/nix` and `/data` PVCs, worker UUID,
and join Secret, but **loses all DinD state, including named Docker volumes,**
when the Deployment is replaced; the shared run directory
`/data/runner` is on the `/data` PVC and is kept.
Deliverables belong in git, published checkpoints, or captured work.
**This destructive behavior defaults on for existing installs on upgrade.**

DinD maintenance waits for every run to finish (`completed`, `failed`, or
`cancelled`); parked, paused, approval, input, and follow-up waits block
cleanup. A pending drain refuses unrelated new run, chat and cross-check
work while letting this worker's own parked runs and pinned plan checkers
finish; maintenance fencing still closes claims. It then fences claims and
requires fresh custody clearance and zero local activity before gated
anonymous-volume pruning or stop. No timer, forced park, or force-roll
override bypasses this DinD gate. Legacy `/nix` + `/data` recycle associated with
spec-hash/generation drift also waits indefinitely for idle. Only independent
nondrifted pressure/nix-size maintenance retains the compatible deadline; global
ForceRoll can bypass its busy deferral, while custody-held data remains protected.

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

### Docker workers: run directory on the data volume

On a Docker-capable worker `/data/runner` is a subPath of the `/data` PVC, so
replacing the pod keeps the run clones (a reschedule to another node also
depends on the data StorageClass supporting reattach, which has not been
verified); deleting or recycling the PVC itself (the full-volume recycle
above, or removing an ephemeral worker) still deletes them. A rootless dind
container cannot write into worker-created run checkouts through bind
mounts by default; that predates this layout in both uid modes. In single-uid
mode it also lost writes at the `/data/runner` and `TMPDIR` roots (owner
decision 2026-10-21: accepted as a documented regression; code analysis only,
no real rootless container run; see
[Docker inside a worker](./worker-docker.md#run-directory-on-the-data-volume)).
**Run Docker workers in uid-split mode** for a different reason:
it closes the single-uid subPath re-resolution residual. Upgrade order, the
custody migration guard (surfaced as the fleet health warning
`Worker roll held at <image>: layout_migration_custody`), rollback and the disk-full
residual are in [Docker inside a
worker](./worker-docker.md#run-directory-on-the-data-volume) and
[ADR-3153](../adr/3153-runner-shared-pvc-subpath.md).

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

### run_not_owned: an outcome this reporting worker cannot replay

If the api answers a worker's report for a run with `run_not_owned` (the run is attributed to another worker or to none, or no longer exists), this reporting worker cannot replay that outcome at its recorded claim generation; another worker may own the run. It keeps the journal, marks it **blocked** with reason `run_not_owned`, and stops sending it; it never deletes or retries it, and the block survives a restart. Only that exact structured answer does this: any other 404 keeps the ordinary retry. The worker logs once: `outbox: terminal journal blocked...` with the run id, claim generation and `terminal-<G>.json`.

The blocked journal no longer pauses claims or appears in the worker's active snapshot lease, since the api cannot lease a run it does not attribute to that worker. Its local files are still protected. It shows as `outbox_blocked` (the reason) and `outbox_blocked_run_id` on the worker: in the TUI worker detail, `uzi worker list --json` and the admin workers JSON; the table cell shows only `blocked`. It does not appear on the run page.

This is an **operator-assisted interim path**. Hosted-worker owners normally have no pod or volume access and should contact their operator; self-hosted owners with volume access are their own operator. A server-mediated owner discard is planned follow-up work. Every step below is a gate: stop on any mismatch or missing evidence.

1. **Quiescence.** Stop the worker (scale the workload to 0, or stop the container). Verify with the platform's process listing that no worker process and no process using the run's checkout remains (pod gone, or `fuser`/`lsof` from a debug pod mounting the volume shows nothing). Confirm with `uzi run get <run>` that the run is not attributed to this worker, and that it is absent from the worker's last active snapshot. If the operator's token cannot read the run (404 or permission error), that is not evidence: get the attribution from the run's owner (their `uzi run get <run>`) or an admin-scoped read. Never operate on a running worker.
2. **Exact attribution.** Open `outbox/<run>/terminal-<G>.json` (plaintext JSON with a MAC). Require `run_id` equal to the reported `outbox_blocked_run_id` and to the directory name, `claim_generation` equal to `G` from the filename, `blocked` true and `blocked_reason` `run_not_owned`. Unreadable or mismatched: stop.
3. **Siblings.** List `outbox/<run>/` (`finalize-<G>.json`, other generations), the run's clone and recovery-store directories, and the database holds via `uzi run recovery <run>` (or have the owner or an admin list them). Leave them all in place; never delete a finalize record or recovery data.
4. **Evidence, or an informed decision.** Copy the journal off the volume and capture commits from the stopped checkout with `git bundle create <run>.bundle --all`. Preserve uncommitted work as follows:
   - Require **GNU tar** for the command below (`tar --version` must identify GNU tar; use an already installed `gtar` where that is its name). Do not substitute a newline-delimited file list.
   - Save `git status --porcelain=v1 -z --untracked-files=all` as a binary status record outside the checkout. Parse its NUL-delimited records with a byte-oriented parser: each record starts with two status bytes and one space, followed by the literal path. A rename or copy has a second NUL-delimited path (the original path); preserve both names and the status in a separate metadata record. Do not feed the status record directly to tar, split paths on whitespace/newlines, use shell command substitution for them, or interpret them as shell commands.
   - Capture every dirty tracked path regardless of directory name, including under `dist/`, `target/` and `node_modules/`. Record deletions and renames in the metadata record. Produce a separate binary `existing-paths.nul` containing only existing paths to archive, one literal repository-relative path followed by NUL; missing/deleted paths belong only in metadata. Preserve symlinks as links, including dangling links. Reject absolute paths or paths outside the stopped checkout.
   - Include every untracked path unless it is a build or dependency artifact verified to hold no unique work: wholly regenerable from committed inputs (for example a committed lockfile or build definition), with no hand-edited or downloaded-only content. If that cannot be shown, include it. Record each exclusion and its reason separately.
   - From the stopped checkout, use `tar --create --file <run>-uncommitted.tar --null --verbatim-files-from --files-from existing-paths.nul`. Both file-list options precede `--files-from`, so spaces, newlines and leading hyphens are literal filename bytes, never tar options. Quote the archive/list arguments if their paths require it. Keep the binary status record, deletion/rename metadata, exclusion record and a path-safe checksum manifest alongside the archive; process manifest paths as literal arguments, never space-joined shell text.
   - Verify before deleting anything: `git bundle verify`, then a clone from the bundle into a fresh directory succeeds with the expected refs and tips; extract the tar into a fresh directory and compare each path from `existing-paths.nul` with the stopped checkout, including file contents and symlink targets. Check the separate deletion/rename metadata too. Any missing or differing path stops the procedure. A matching checksum manifest can verify regular-file contents, but `tar -tf` alone is not enough. The copied journal must parse with matching `run_id` and `claim_generation`.

   Alternatively, the run's owner records an explicit informed decision to discard, naming what is lost: the outcome body's status, branch and MR fields, unbundled commits, and every listed uncommitted path. Without verified evidence or that decision, stop.
5. **Delete and restart.** Delete only `terminal-<G>.json`, restart the worker, and confirm `outbox_blocked_run_id` no longer names the run.

Deleting the journal releases only its own local physical protection. It does not release database custody or recovery holds: those release only through a server-side disposition, such as settlement on publication or archive evidence, the worker's release and settle endpoints, or the owner's `uzi run discard` / **Discard held work** (see [run-recovery.md](./run-recovery.md)). The worker's other local retention is unaffected. Design rationale: [ADR-1393](../adr/1393-worker-outbox-outcomes.md).

## How this differs from running your own worker

A worker you run yourself is a container on hardware you control: you copy a
join token, start it, and keep it running. A hosted worker is the same
protocol against a container the cluster manages for you — provisioning and
deleting from the UI stand in for starting and stopping it yourself. Both
kinds work identically once online: same claim/run behavior, same resource
gauges, same [concurrency](./worker-setup.md#concurrent-runs) rules.

## Current Codex worker baseline

The current Codex worker uses runtime 0.160.1 and defaults to `gpt-6.1-sol`
when no worker-model override is set. Explicit model preferences remain
unchanged. Both harnesses inherit medium reasoning effort; Claude and Codex
keep separate overrides. See [Reasoning effort](worker-effort.md).

Every Codex claim requires `codex_runtime_v2`, so an older worker cannot
silently use the old model/catalog or ignore the requested effort. Codex runs
queue until the worker image rolls, on hosted workers and compose alike.
