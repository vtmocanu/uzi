# ADR-1759: dind-data is metered by a sidecar and pruned by the worker under a claim-gate exclusion

**Status**: Accepted (issue #1759, M1-M4 landed)
**Date**: 2026-09-27
**Deciders**: agent team (issue #1759 milestones + review waves)
**Policy update**: issue #1760; maintainer decision 2026-10-04 (AI-synced 2026-10-05). D7-D9 supersede the original cache-only scope below.
**Amended by**: [ADR-3153](3153-runner-shared-pvc-subpath.md) (dind now mounts the `runner-shared` subPath of `data` at `/data/runner`, so Decision 3's "mounts none of its own crown jewels" is narrowed to the mount view; DinD-only maintenance keeps `/data/runner`)
**Related**: [ADR-837](0837-worker-disk-lifecycle.md) (the `/nix`/`/data` disk-observability and pressure-recycle design this one narrows the "Known gap" of)

## Decision (summary)

A docker-tier hosted worker now gets a second, unprivileged native sidecar, `dind-meter`, that mounts the `dind-data` PVC **read-only** and publishes a periodic statfs sample (bytes + inodes) for the worker to read and display as a "Disk dind" gauge. The worker itself never mounts `dind-data`. Separately, the worker watches that sample and, only when the volume is under sustained pressure, it is provably idle, and the api's heartbeat confirms it holds no unpublished work, runs a short allowlisted sequence of `docker image prune`/`docker builder prune` commands against the daemon. The legacy `disk_pressure` boolean remains `/nix`+`/data`-only. Separate per-volume wire flags now drive a DinD-only pressure-recycle arm under the stricter terminal-run maintenance gate (D7-D9), including optional anonymous-volume pruning.

## Context (original #1759 scope)

ADR-837 shipped disk observability and a delete-and-remint recycle for `/nix` and `/data`, but named the docker-tier `dind-data` PVC (the inner daemon's image/layer/build-cache store) as a deliberate v1 gap: it is neither metered nor recycled, and `disk_pressure` can never fire on it. It is also the volume most likely to fill on a docker-tier worker, since nothing there ever shrinks on its own (`render_dind.go`'s `dindDataDefaultSize` comment: "the cache PERSISTS across pod rolls ... with nothing garbage-collecting it"). This issue closes the metering half of that gap and adds a bounded, safe reclaim path that does not need the recycle machinery's delete-and-remint shape at all — the daemon can shed its own build cache without losing the PVC.

## The decisions

D1-D6 record the original #1759 metering/cache-prune design. D7-D9 define the current #1760 maintenance policy; the cache-prune allowlist and running-container check do not constrain the separate maintenance path.

### D1 — Meter with a separate, unprivileged sidecar, not by mounting into the worker or the privileged daemon

The daemon container (`dindContainer`) is either a userns-remapped rootless process or, on nodes that cannot run rootless DinD, real root (`prds/done/89-optional-nonrootless-dind.md`'s non-rootless posture) — Decision 3 of `prds/done/83-docker-capable-worker.md` (the separate-container mount namespace) is that the worker mounts none of its own crown jewels into that container's reach (amended by ADR-3153: the one exception is the `runner-shared` subPath of `data`, mounted at `/data/runner` in both containers), and the inverse holds too: the worker must not gain a new mount into the privileged daemon's own data root either. So metering is a **third, purpose-built container** (`dindMeterContainer`): the worker's own uid, no privilege escalation, a read-only root filesystem, every capability dropped, `RuntimeDefault` seccomp, and exactly two mounts — `dind-data` read-only, and its own private emptyDir it writes its sample into. It has no startup probe, so it never gates the worker's own readiness; a missing or broken meter degrades to "no disk-dind gauge, no prune," never to a stalled pod.

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
and join Secret survive. Deployment replacement keeps `/data/runner` (a data-PVC subPath since ADR-3153; it formerly lost the run-workdir emptyDir). A replacement must have a Bound PVC, a ready pod and fresh
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

- `dind-data` has a gauge and two reclaim paths: the original bounded cache prune and the stricter terminal-only maintenance path. Maintenance may delete the entire DinD PVC, while preserving `/nix`, `/data`, worker UUID and join Secret; Deployment replacement keeps `/data/runner` (ADR-3153).
- **Residual: in the non-rootless posture, node root (an accepted residual of `prds/done/89-optional-nonrootless-dind.md`) can write the meter emptyDir's node-side backing directory, so it can, in principle, forge the meter's sample.** Mount-namespace separation only keeps the daemon's own mount namespace, and any container it runs, from reaching that file directly; it does not bound node root itself. What bounds the damage is the reader's strict validation (D2), D4's frozen allowlist, and D5's idle/custody gate together: a forged sample cannot bypass the strict maintenance gate; it can affect maintenance intent as well as the original cache-prune sequence. This is not a guarantee that forged pressure cannot cause scratch loss once the gate is satisfied.
- **Residual: docker compose has no equivalent meter or automatic prune.** A compose docker worker's `dinddata` volume is reclaimed only by the existing manual path (`docker compose down -v`, or `docker volume rm`).
- **Residual: a filesystem reporting zero total inodes yields no valid sample** — btrfs commonly reports `files=0` this way — since the reader's statfs-relation check requires `files > 0`; the gauge and the prune both silently degrade to "no sample" there (no sample, no prune, no gauge), not a crash or a false trigger.
- **Destructive default on upgrade**: #1760 adds automated DinD-only recycling for eligible persistent hosted Docker workers. Named volumes are not preserved (`/data/runner` is kept since ADR-3153); store deliverables in git/checkpoints/capture. Operators must explicitly opt out before stop with `UZI_WORKER_DISK_RECYCLE_ENABLED=false`; a stop already started finishes safely even if disabled.
- Flipping `UZI_WORKER_DISK_RECYCLE_ENABLED` now has one more visible effect beyond ADR-837's own two arms: it also gates whether a docker worker's DinD cache self-prunes, and toggling it rolls the **non-ephemeral** docker-tier fleet the same way toggling any other pod-template field does (an ephemeral worker's rendered value never changes, so its pod does not roll on the toggle alone).

## Investigation checkpoint — #3653 M1 (2026-10-30)

**Outcome:** retain the existing custody checks. This records M1 investigation,
not a runtime amendment. D4/D5 and D7-D9 remain operational policy verbatim.
At the M1 checkpoint, M2 worker documentation/mirror work remained the next
milestone; any runtime amendment needs a separate gate. The matrix recommends a next plan, not authorization.

### Command matrix and source boundary

| Frozen argv (literal) | Resource set | Decision | Evidence and recommendation |
| --- | --- | --- | --- |
| `builder prune -f` | Unused/dangling build cache; without `--all`, the pinned BuildKit source excludes internal, frontend and shared records. | **DEFER** | Credible candidate for a separately reviewed builder-only amendment, not a universal no-go: native BuildKit protects acquired cache references. Future reuse of unused cache, cache mounts, intermediates and unavailable inputs remains at risk under D7. No held-custody exception is authorized. |
| `image prune -f` | Dangling images unreferenced by containers. | **DEFER** | Existing container references survived the tests. No held-custody exception proposed: the pinned containerd implementation deletes pull-labelled leases; an image inventory does not enumerate future dependencies. |
| `image prune -a -f --filter until=24h` | Unused images unreferenced by containers whose `Created` timestamp is older than 24h. | **DEFER** | Same refusal. Positive aged-image deletion was demonstrated. `Created` is build time, not pull or last-use time. |

Current true or unknown custody refuses **all three commands**. Future intended
reuse by image ID or tag is not a container reference. Pruning can lose an image
needed later when its registry or source image is unavailable; D7 treats that as
scratch dependency loss, not protected-source loss. A future amendment must
explicitly accept this dependency-loss boundary.

The lead inspected Moby module v2.0.0-beta.23 origin
`3ce5872b7950c63ba2ffbc5123101019ff3e6682`, matching Docker 29.8;
its [go.mod line 66](https://github.com/moby/moby/blob/3ce5872b7950c63ba2ffbc5123101019ff3e6682/go.mod#L66)
pins BuildKit v0.33.0, origin `dddd5621af04ea57823085c93a063383f71d3173`.
Public Go proxy archive fetches returned HTTP 200 / exit 0; the BuildKit
module checksum matched Moby's go.sum. This is not independent sumdb authentication.

BuildKit's [cache selection/deletion](https://github.com/moby/buildkit/blob/dddd5621af04ea57823085c93a063383f71d3173/cache/manager.go#L1124-L1235)
holds `cm.mu` and record `cr.mu`, selects `len(cr.refs) == 0`, and marks
selected records dead before unlocking. [getRecord](https://github.com/moby/buildkit/blob/dddd5621af04ea57823085c93a063383f71d3173/cache/manager.go#L416-L420)
rejects dead records. Without `--all`, internal/frontend/shared records are
excluded. These boundaries protect acquired references, not a future acquisition
of currently unreferenced cache. The [buildCount check](https://github.com/moby/buildkit/blob/dddd5621af04ea57823085c93a063383f71d3173/control/control.go#L242-L245)
controls `CancelCacheLeases`; it is not an admission fence.

Moby's [containerd image prune](https://github.com/moby/moby/blob/3ce5872b7950c63ba2ffbc5123101019ff3e6682/daemon/containerd/image_prune.go#L67-L80)
deletes leases carrying the [pull lease label](https://github.com/moby/moby/blob/3ce5872b7950c63ba2ffbc5123101019ff3e6682/daemon/containerd/leases.go#L11-L35).
The classic/vfs experiment below does not validate that containerd path or hosted
rootless behavior. Native cache safety and daemon execution exclusion are separate.
No protected-source loss was observed. D5's coarse global refusal incidentally
preserves held scratch dependencies; it supplies neither a source inventory nor
proof of quiescence. No unrelated-resource ownership guarantee or inventory is proposed.

### Current local authorization map

The inspected paths establish the following boundaries; they do not establish
daemon-wide exclusion:

- Custody counts open, owner-scoped holds naming the live worker
  ([query](../api/internal/store/queries/recovery.sql), lines 298-299;
  [service](../api/internal/workersvc/service.go), line 3126 onward).
  [Heartbeat handler](../api/internal/handler/worker_protocol.go), line 596 onward,
  reports true on lookup error; [client](../agent/src/client.ts), line 1903,
  accepts a boolean or returns unknown. [Worker](../agent/src/worker.ts),
  line 1119, records heartbeat send time.
- [Cache prune](../agent/src/dind-prune.ts), line 460 onward, requires fresh false
  custody after activity. It rechecks before the awaited `ps -q`, but not after
  that await or before each prune step. [Worker isIdle](../agent/src/worker.ts),
  line 184, checks run/chat/cross-check active sets and the active-run registry.
- Gate owners are `cache`, `maintenance`, `retained_ops` and
  `terminal_maintenance` ([gate](../agent/src/dind-prune.ts), line 194;
  shared [wiring](../agent/src/main.ts), line 676).
  Run/cross-check admission brackets claims at worker lines 1383/1483;
  chat does so at 1561/1586, registering active work before releasing the claim.
- [Maintenance](../agent/src/dind-maintenance.ts), line 180, defers cache-owner
  actuation, but its readiness `safe()` predicate at line 132 does not itself
  test the cache owner. [Retained-source discard](../agent/src/ops/retained-source-server.ts),
  line 482, refuses terminal maintenance before joining the gate; it does not
  make the gate a mutex among owners. [Terminal maintenance](../agent/src/maintenance-exclusion.ts),
  line 49 onward, pins sole ownership and generation.

Cache outcomes do not settle custody: [dind-prune.ts](../agent/src/dind-prune.ts),
`attempt()` at line 490 onward, releases only `gate.open()` in `finally`
(the default owner is `cache`, lines 227-235). Its options and execution path
have no custody-settlement callback or API call. [worker.ts](../agent/src/worker.ts),
line 1119 onward, feeds heartbeat custody independently to cache prune and
maintenance. [dind-maintenance.ts](../agent/src/dind-maintenance.ts), `safe()`
at line 132 and `acknowledgement()` at line 159 onward, require maintenance's
own matching fenced operation/registration identity, fresh false custody after
activity, and a matching disposition for ACK, rather than a cache outcome.
Its `tick()` at line 180 excludes an active cache owner before actuation.

Existing [dind-maintenance.test.ts](../agent/test/dind-maintenance.test.ts),
line 266 onward, checks unknown/null/malformed/stale binding and custody fail
closed; line 323 onward checks cache running-container refusal, failure and
backoff are not maintenance prerequisites: independently eligible maintenance
is allowed. The precise regression gap is the absence of a single end-to-end
API-custody persistence test across cache refusal/failure. The code boundary
and focused controller tests are evidence, not whole-#2257 acceptance or an
exclusion fix.

These are execution-authorization gaps, not demonstrated cache-driven source
loss. A proposed next plan needs serialization of conflicting local operations
and revalidation across awaits/steps, under separate review.

### Isolated daemon methods and observations

Synthetic fixtures ran in pinned
`docker:29-dind@sha256:5efed980cba3fc126cf54e21a5a6ff8849d05b6e0623d6e7612f48e9cd6cd17e`:
Docker 29.8.0 / API 1.56, rootful, classic/vfs, with
`dockerd --host=unix:///var/run/docker.sock --storage-driver=vfs`.
There were no host binds, host socket or published ports. Contexts and
runner/source/recovery sentinels were synthetic inside the disposable apparatus.
The three literal prune argv above targeted that inner daemon.

| First proof (exit 0) | Removed fixture payload | Separate Docker estimate |
| --- | ---: | ---: |
| `builder prune -f` | 4,194,304 bytes across four files | 4.201MB |
| `image prune -f` | 1,048,576 bytes | 1.049MB |
| `image prune -a -f --filter until=24h` | 1,048,576 bytes | 1.049MB |

These are disappearing-file logical lengths and summed `st_blocks * 512`,
not unique physical or filesystem free-space bytes. The aged image was loaded
with actual `Created=2020-01-01T00:00:00Z`, read back and checked eligible;
the young tagged control survived. An accepted unfinished build overlapped
`builder prune -f`: estimate 0B, live payloads retained.
Before/after snapshots matched container/reference identities, complete
state/config/mounts, named and three anonymous volumes (including an orphan),
and sentinel contents. First proof including cleanup took 14.984s;
the pinned apparatus pull separately took 8s.

Supplemental acceptance barriers gated a registry layer body after HTTP headers,
plugin `POST /VolumeDriver.Create` after image resolution, and an executing
Dockerfile RUN's HTTP request. They established daemon acceptance, not merely
client launch. Both image prunes succeeded during the executed pull/create
barriers, reporting 0B. The create image already had container references.

| Supplemental attempt | Overall result / wall time | Bounded observation |
| --- | --- | --- |
| 1 | FAIL, exit 1 / 28.267s | Accepted pull/prune/inner exit 143 evidence usable. Create acceptance/overlap/inner exit usable, but untyped inspect returned volume metadata and raised `KeyError: 'Image'` before build setup. Its create-completion claim is invalid; build unrun. |
| 2 | FAIL, exit 1 / 10.230s | Pull completed across overlapping prunes, inner exit 0. Subsequent create failed with `no command specified`; planned completed-old-image/reference prune unrun. |
| Final | PASS within scope, exit 0 / 13.857s | `docker inspect --type container <fixture>` proved create completed after actual inner client exit 143. Accepted buildx exited 130 with request cancellation observed, then `builder prune -f` reported 10.43kB; protected snapshots matched. Pull unrun in this attempt. |

Exact owned-container/data-root cleanup passed for the first proof and each
supplemental attempt. Offline recovery verification exited 0: nine executed
phase snapshot equalities and provenance checked, without a costly daemon rerun.
Earlier leftover snapshots were excluded from newer attempts; phase successes
do not turn either failed attempt green. The first proof's outer transport exit
does not establish inner build cancellation.

### Limits and next gate

Pull cancellation/eventual outcome after inner exit 143 remains unproved.
No pull/create prune ran after client exit but before barrier release; no newly
unreferenced old-image create was proved. One observed build cancellation is
not universal quiescence. Rootless/overlay/containerd equivalence, hosted behavior
and net physical reclamation remain unrun. The accepted-create residual in
[ADR-1783](1783-run-quiescence-and-attempt-clone-paths.md) remains.
[PRD-1860 M0](../prds/1860-pod-epoch-writer-exclusion.md) complete / conditional-go
is historical context, not newly shipped universal proof; current
[main.ts](../agent/src/main.ts) also invokes the GET-only pod-epoch shadow reader.

Lead evidence records 71 prune/maintenance tests plus four selected A-docker
teardown tests passing, no failures/cancellations/skips, coarse combined wall
2s. These were the existing `dind-prune.test.ts`, `dind-maintenance.test.ts`
and `run-quiescence.test.ts` suites under agent/test; the selected
`A-docker: the teardown never claims quiescence` suite includes fake-daemon
late-create coverage. No positive-exception red/green, runtime change or full
suite result is claimed. An E2E literal sweep found no matching prune
outcome/flag hits in e2e/phases, e2e/lib.sh, e2e/lib.test.sh or e2e/forge-fake;
no runtime contract changed and no E2E phases ran. Subsequent existing logs
record agent dependency/lint/dead-code/type checks passing in 18s, the Go
`api/internal/uzidocs` check passing in 14s wall time, docs validation passing
for 80 pages with 48 existing style warnings, and changelog validation passing.
These are the original recorded results; repair validation is reported separately.
No positive held-custody exception red/green was performed because no
runtime fix or exception was implemented.

### Evidence inventory

Durable code/test evidence is the linked custody/heartbeat/gate paths above,
[prune tests](../agent/test/dind-prune.test.ts),
[maintenance tests](../agent/test/dind-maintenance.test.ts), and
[quiescence tests](../agent/test/run-quiescence.test.ts); pinned public upstream
links are in the source boundary above. Local reports and logs below are
**ephemeral, nonpublished evidence**, not durable links or published raw scratch.
Paths identify this run's evidence only and may disappear with the worker.

| Local evidence path (under `.uzi/scratch/`) | Recorded scope/result |
| --- | --- |
| `m1-report.md` | First isolated positive deletion/protection proof and its limits. |
| `m1-race-recovery-report.md`, `m1-race-recovery-verification.log` | Recovered race evidence and offline consistency verification, exit 0; failed attempts remain failed. |
| `gate-log.aCIiZf` | 71 + 4 focused tests, exit 0, coarse combined wall 2s. |
| `gate-log.UMup34` | Agent static/dependency checks, exit 0, 18s. |
| `gate-log.ogTDqU` | Go docs check, exit 0, 14s wall time. |
| `gate-log.bss262` | Docs: 80 pages, 48 existing style warnings, exit 0. |
| `gate-log.lWV4HO` | Changelog check, exit 0; recorded branch exemption, not a new fragment. |

### Separate nonclosing gate

The architect judged a candidate next-gate invariant, not approved policy:
exclude tracked executions/in-flight claims, require fresh known custody after
activity, and serialize conflicting local operations. Accepted daemon work may
still overlap; native BuildKit protects acquired references but unused cache
can disappear before future acquisition. An explicit owner decision must accept
that narrower invariant before a builder-only amendment; D7 alone is not approval.
The proposed next plan continues refusing unknown, stale, missing, future or
straddling custody. Current code has not been changed to implement that plan.
Any runtime amendment remains a separate, nonclosing gate; this checkpoint
leaves #3653 open and does not establish whole-#2257 acceptance.
#2257 terminal/custody/volume/PVC authority remains unchanged. No threshold tuning
is proposed; the historical 85-versus-90 cause is unknown. This checkpoint promises
neither busy-worker reclaim nor hosted reclaimed bytes.
