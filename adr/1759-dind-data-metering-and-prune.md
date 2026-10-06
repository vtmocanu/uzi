# ADR-1759: dind-data is metered by a sidecar and pruned by the worker under a claim-gate exclusion

**Status**: Accepted (issue #1759, M1-M4 landed)
**Date**: 2026-09-27
**Deciders**: agent team (issue #1759 milestones + review waves)
**Policy update**: issue #1760; maintainer decision 2026-10-04 (AI-synced 2026-10-05). D7-D9 supersede the original cache-only scope below.
**Related**: [ADR-837](0837-worker-disk-lifecycle.md) (the `/nix`/`/data` disk-observability and pressure-recycle design this one narrows the "Known gap" of)

## Decision (summary)

A docker-tier hosted worker now gets a second, unprivileged native sidecar, `dind-meter`, that mounts the `dind-data` PVC **read-only** and publishes a periodic statfs sample (bytes + inodes) for the worker to read and display as a "Disk dind" gauge. The worker itself never mounts `dind-data`. Separately, the worker watches that sample and, only when the volume is under sustained pressure, it is provably idle, and the api's heartbeat confirms it holds no unpublished work, runs a short allowlisted sequence of `docker image prune`/`docker builder prune` commands against the daemon. The legacy `disk_pressure` boolean remains `/nix`+`/data`-only. Separate per-volume wire flags now drive a DinD-only pressure-recycle arm under the stricter terminal-run maintenance gate (D7-D9), including optional anonymous-volume pruning.

## Context (original #1759 scope)

ADR-837 shipped disk observability and a delete-and-remint recycle for `/nix` and `/data`, but named the docker-tier `dind-data` PVC (the inner daemon's image/layer/build-cache store) as a deliberate v1 gap: it is neither metered nor recycled, and `disk_pressure` can never fire on it. It is also the volume most likely to fill on a docker-tier worker, since nothing there ever shrinks on its own (`render_dind.go`'s `dindDataDefaultSize` comment: "the cache PERSISTS across pod rolls ... with nothing garbage-collecting it"). This issue closes the metering half of that gap and adds a bounded, safe reclaim path that does not need the recycle machinery's delete-and-remint shape at all — the daemon can shed its own build cache without losing the PVC.

## The decisions

D1-D6 record the original #1759 metering/cache-prune design. D7-D9 define the current #1760 maintenance policy; the cache-prune allowlist and running-container check do not constrain the separate maintenance path.

### D1 — Meter with a separate, unprivileged sidecar, not by mounting into the worker or the privileged daemon

The daemon container (`dindContainer`) is either a userns-remapped rootless process or, on nodes that cannot run rootless DinD, real root (`prds/done/89-optional-nonrootless-dind.md`'s non-rootless posture) — Decision 3 of `prds/done/83-docker-capable-worker.md` (the separate-container mount namespace) is that the worker mounts none of its own crown jewels into that container's reach, and the inverse holds too: the worker must not gain a new mount into the privileged daemon's own data root either. So metering is a **third, purpose-built container** (`dindMeterContainer`): the worker's own uid, no privilege escalation, a read-only root filesystem, every capability dropped, `RuntimeDefault` seccomp, and exactly two mounts — `dind-data` read-only, and its own private emptyDir it writes its sample into. It has no startup probe, so it never gates the worker's own readiness; a missing or broken meter degrades to "no disk-dind gauge, no prune," never to a stalled pod.

Rejected: mounting `dind-data` (even read-only) directly into the worker, which would put the worker container in the position of reading a volume it does not otherwise touch and remove the clean "worker never mounts dind-data" invariant `render_test.go` pins. Rejected: `docker system df` over the daemon socket, which would need the client to be trusted with a broader surface than a plain statfs and ties the sample to the daemon being responsive rather than to the filesystem directly.

### D2 — The sample is an untrusted, versioned, bounded contract; the worker validates every field

The sample crosses a privilege boundary the daemon posture makes explicit: in the non-rootless posture the daemon runs as real, privileged node root (`prds/done/89-optional-nonrootless-dind.md`'s accepted residual), and a container breakout there lands as node root, which **can** write to the meter emptyDir's node-side backing directory — mount-namespace separation only keeps the daemon's own mount namespace, and any container it runs, from reaching that file; it does nothing against node root reading or writing it directly (`agent/src/dind-meter.ts`'s header states this plainly). So the worker-side reader (`dind-meter.ts`) treats the file as fully untrusted input unconditionally, not as a defense of last resort: bounded read (no unbounded slurp), regular-file-only (`O_NOFOLLOW`, rejects a planted symlink or FIFO), one strict versioned line pattern (`v1 <epoch> <frsize> <blocks> <bfree> <files> <ffree>`), every integer exactly representable, statfs relations that must hold (`bfree <= blocks`, `ffree <= files`), and a freshness window (90s) plus a small future-skew allowance. Any violation returns "no sample," never a throw and never the raw line logged. What bounds the damage from even a successfully forged sample is this validation together with D4's frozen allowlist and D5's idle/custody gate: in the original cache-prune path, a forged sample can trigger or suppress only the allowlisted cache-prune sequence. Under the current policy it can also affect pressure-driven maintenance intent, but cannot bypass the server's terminal-run claim fence, identity checks, local quiescence, fresh custody clearance, or controller stop-before-PVC-delete gate (D7-D9).

### D3 — Prune is worker-local, gated on an exclusion claim, never dependent on the controller or the recycle machinery

Reclaiming build cache is an ordinary daemon operation (`docker image/builder prune`), not a volume-lifecycle operation, so it belongs to the worker process that already talks to the daemon over `DOCKER_HOST` — not to the controller, which has no daemon access and whose only lever is delete-and-remint (D1 of ADR-837). The worker's `DindPruneController` runs its own tick loop, reads the meter sample, and only acts once real exclusivity is established: it closes both claim lanes, drains claims already in flight (bounded wait), confirms no active run/chat/registry entry, and confirms `docker ps -q` lists nothing running. Most failures at these steps (a claim in flight, active work, custody not yet clear) release the gate immediately and retry on the next tick without spending the backoff — a busy worker is never penalized for being busy. `docker ps` failures (30s timeout each) also defer and retry next tick, except that the third consecutive one ends the attempt as "failed" and enters the hour-long backoff — a wedged daemon is not retried forever on every tick.

### D4 — The original cache-prune allowlist; nothing is removed by name

The original cache-prune module's allowlisted argv is the read-only `ps -q` idle check plus the prunes `image prune -f`, `builder prune -f`, and — only if still over threshold after those two — `image prune -a -f --filter until=24h`. `until` filters on the image's `Created` (build) timestamp, not when it was pulled or last used, so a freshly *pulled* image that was *built* more than a day ago **is** swept if nothing references it — the filter is about the image's age, not its provenance in this cache. A runtime guard re-validates every argv against the frozen table before exec: it rejects any argv token that exactly matches (case-insensitively) `volume`, `volumes`, `--volumes`, `system`, `container`, or `rm`, or that carries one of them as a `token=value` flag, and separately requires the whole argv to equal one of the frozen entries — that second, exact-allowlist check is what actually blocks every other docker subcommand, not the forbidden-token list alone (which only catches the tokens above, wherever they'd appear). The table itself is asserted against both checks at module load, so an edit that ever adds a forbidden command fails at import, not in production. Nothing here removes a container (stopped or running) or a volume by name: the run harness creates no containers this worker could prove it owns, so a stopped container may be a user's own deliberate state, and volumes are outside this cache-prune module's scope. The separate strict maintenance gate now allows exactly `docker volume prune -f` after a server API version check, and the controller can delete-and-remint the entire PVC (D7-D9).

### D5 — Custody is fail-closed, sourced from the same custody flag PRD #1296 already built, now also carried on the heartbeat response

"Provably no unpublished work" reuses `retaining_unpublished_work`, the same custody flag PRD #1296 introduced (on worker listing) for recovery holds, rather than inventing a second signal; this issue (#1759) is what adds it to the heartbeat response so the worker's own prune loop can read it. The worker trusts a flag it received on a **fresh** heartbeat (its *send* time, not its receipt time, within a small bound of heartbeat intervals) that was also sent **after** the worker's own record of its last activity ending — the straddling-heartbeat ordering exists because a run ending can open a hold the api reports only on a *later* heartbeat, so a flag sent before that boundary must not be read as clearance. Anything else — a missing flag, a stale one, a heartbeat send that raced the run's own end — skips the attempt. This is fail-closed by construction: every branch that cannot prove clearance defers, none defaults to proceeding.

### D6 — The prune gate is env-driven off the same toggle as the `/nix`+`/data` recycle, and it rolls the fleet once

`UZI_DIND_PRUNE_ENABLED` is `true` only when `UZI_WORKER_DISK_RECYCLE_ENABLED` (the ADR-837 D9 master toggle) is on **and** the worker is not ephemeral (a run-bound worker is torn down with its run regardless). Reusing the existing toggle means an operator who has already accepted ADR-837's self-heal default gets this reclaim path for free, and flipping that one toggle is still the single place that turns automated actuation off across both mechanisms. The env var is rendered on every docker worker's pod (an ephemeral worker always gets `"false"`), so flipping the toggle changes the rendered value, and therefore rolls the pod, only for **non-ephemeral** docker workers. Upgrading to this release is a separate cause with a wider blast radius: it adds the `dind-meter` sidecar and its emptyDir to the pod spec of **every** docker worker, ephemeral included, so the first upgrade rolls the whole docker-tier fleet regardless of the toggle's setting.

### D7 — DinD is scratch; readiness is terminal-only, not executor-idle

**Maintainer decision 2026-10-04 (AI-synced 2026-10-05), issue #1760.**
DinD containers, named/anonymous volumes, networks, images and build cache
are scratch. Deliverables belong in git, published checkpoints, or captured
work. There is no retirement/preservation inventory, migration, user
warning flow, nudge, cleanup timer, or forced park. The upgrade warning is
documentation of the destructive default, not a new user confirmation gate.

Pressure is bytes **or** inodes at the configured watermark across at least
two **distinct fresh meter epochs**, from the current registration floor,
with a 45s freshness bound; a gap resets the streak. Idle time and maximum
age are not triggers. The legacy `disk_pressure` boolean remains nix/data-only;
`disk_pressure_volumes` separately identifies per-volume pressure.
Old workers without `dind_maintenance_v1` remain report-only. Ephemeral
Docker workers report metering/admin pending state but disable prune/recycle
and use terminal teardown; plain workers do not enter DinD maintenance.
Compose manual cleanup is unchanged.

A pending maintenance drain refuses **new** run/chat claims but allows the
worker's own parked resumes to finish. Any status except `completed`,
`failed`, or `cancelled` is busy, including parked, paused, approval,
input, and follow-up waits. The API checks nonterminal claims and establishes
the all-claims fence atomically under the worker lock; see
`api/internal/workersvc/dind_maintenance.go` and
`api/internal/store/queries/dind_maintenance.sql`; the terminal-status check
and run/chat claim fences are in `api/internal/store/queries/runtime.sql`
and `api/internal/store/queries/chat.sql`. Readiness also requires
zero local executions/in-flight claims and fresh custody clearance sent
after local activity, bound to the matching operation, Deployment, PVC,
and registration identity (`agent/src/dind-maintenance.ts`). Neither
`ForceRoll` nor `DrainDeadline` bypasses DinD readiness; ordinary rolls and
legacy nix/data recycling keep their existing override behavior.

### D8 — Optional prune, then UID-bound stop and DinD-only remint

Eligible cache prune is tried first, but its skip, failure, timeout or
running-container deferral is not a prerequisite for maintenance. Within
the strict D7 gate, the separate worker maintenance path makes one read-only
`docker version --format {{.Server.APIVersion}}` check per bound identity.
A valid **server API >=1.42** permits exactly `docker volume prune -f`,
without `-a`, filters, container deletion or name-based removal. This
removes unreferenced anonymous volumes and keeps named volumes and volumes
referenced by any container, including stopped ones. This narrow prune
does not make named volumes durable: the subsequent PVC recycle loses them.

A newer fresh below-watermark sample can cancel pre-stop maintenance and
reopen admission without recycling. Failed, skipped, timed-out or
insufficient pruning proceeds toward recycle; missing samples are not
proof of recovery. Leaked running containers do not block this gate once
runs are terminal and fresh custody is clear.

The controller foreground-deletes the bound Deployment with a UID
precondition, then **observes the Deployment and old pods actually gone**,
including Terminating pods, before deleting the DinD PVC by its bound UID.
It waits for the old PVC, including any Terminating object, to disappear
before reminting. Only `dind-data` is recycled: nix/data PVCs, worker UUID
and join Secret survive. Deployment replacement also loses **run-workdir
emptyDir**. A replacement must have a Bound PVC, a ready pod and fresh
replacement-worker registration heartbeat; the controller publishes
readiness **before** completing the operation and opening the fence.
Failed binding stays fenced with no rollback.

Cooldown reads the DinD PVC's own creation timestamp. With simultaneous
legacy and DinD pressure, persist DinD intent, run legacy recycling first,
then continue DinD on a later tick before another legacy recycle. The
legacy cooldown allows immediate eligible DinD maintenance. Disabling
`UZI_WORKER_DISK_RECYCLE_ENABLED` opts out before stop; a started stop/rebuild
finishes safely even if disabled. See
`controller/internal/kube/maintenance.go` for the phase boundaries.

### D9 — Fresh bounded observation and replacement readiness

Before destructive transitions and replacement readiness, maintenance
uses fresh namespace reads with a **30s total observation deadline**,
**256 items per page**, **16 pages / 4096 objects per kind** (foreign
objects count too), and **8 MiB of actual decompressed HTTP response bytes
per response**. Oversized/incomplete lists, repeated continuation tokens,
timeouts, transport errors and list denial fail closed: cleanup/readiness
stays pending or fenced. These are operational response/list limits,
not a heap bound or an atomic Kubernetes snapshot; observations across
kinds remain separate reads. Legacy bulk `Observe` is unchanged.
No new configuration knobs are introduced.

The enforcing boundaries are
`controller/internal/kube/maintenance_observe.go`,
`controller/internal/kube/maintenance_pagination.go`, and
`controller/internal/kube/maintenance_transport.go`. Replacement readiness
checks real Deployment UID → ReplicaSet UID → Pod controller ancestry,
rather than trusting owner labels alone. Upgrade the chart's Docker
controller Role alongside the controller: only that Role adds
`apps/replicasets` `list` in
`deploy/chart/templates/worker-rbac.yaml`; default/isolated Roles retain
their grants, and no Secret reads are added. Missing ancestry permission
withholds readiness proof while preserving base rebuilding observations.

**Validation boundary:** real-cluster validation is assigned to the
maintainer after release and has **not run**. A focused throwaway DinD
check covers Docker prune semantics only, not the production gate; this
documentation update records no test result.
No new kind/SDK production E2E or paused-recycled-resumed result is claimed.

## Consequences

- `dind-data` has a gauge and two reclaim paths: the original bounded cache prune and the stricter terminal-only maintenance path. Maintenance may delete the entire DinD PVC, while preserving `/nix`, `/data`, worker UUID and join Secret; Deployment replacement also loses run-workdir.
- **Residual: in the non-rootless posture, node root (an accepted residual of `prds/done/89-optional-nonrootless-dind.md`) can write the meter emptyDir's node-side backing directory, so it can, in principle, forge the meter's sample.** Mount-namespace separation only keeps the daemon's own mount namespace, and any container it runs, from reaching that file directly; it does not bound node root itself. What bounds the damage is the reader's strict validation (D2), D4's frozen allowlist, and D5's idle/custody gate together: a forged sample cannot bypass the strict maintenance gate; it can affect maintenance intent as well as the original cache-prune sequence. This is not a guarantee that forged pressure cannot cause scratch loss once the gate is satisfied.
- **Residual: docker compose has no equivalent meter or automatic prune.** A compose docker worker's `dinddata` volume is reclaimed only by the existing manual path (`docker compose down -v`, or `docker volume rm`).
- **Residual: a filesystem reporting zero total inodes yields no valid sample** — btrfs commonly reports `files=0` this way — since the reader's statfs-relation check requires `files > 0`; the gauge and the prune both silently degrade to "no sample" there (no sample, no prune, no gauge), not a crash or a false trigger.
- **Destructive default on upgrade**: #1760 adds automated DinD-only recycling for eligible persistent hosted Docker workers. Named volumes and run-workdir are not preserved; store deliverables in git/checkpoints/capture. Operators must explicitly opt out before stop with `UZI_WORKER_DISK_RECYCLE_ENABLED=false`; a stop already started finishes safely even if disabled.
- Flipping `UZI_WORKER_DISK_RECYCLE_ENABLED` now has one more visible effect beyond ADR-837's own two arms: it also gates whether a docker worker's DinD cache self-prunes, and toggling it rolls the **non-ephemeral** docker-tier fleet the same way toggling any other pod-template field does (an ephemeral worker's rendered value never changes, so its pod does not roll on the toggle alone).
