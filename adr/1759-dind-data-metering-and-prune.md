# ADR-1759: dind-data is metered by a sidecar and pruned by the worker under a claim-gate exclusion

**Status**: Accepted (issue #1759, M1-M4 landed)
**Date**: 2026-09-27
**Deciders**: agent team (issue #1759 milestones + review waves)
**Related**: [ADR-837](0837-worker-disk-lifecycle.md) (the `/nix`/`/data` disk-observability and pressure-recycle design this one narrows the "Known gap" of)

## Decision (summary)

A docker-tier hosted worker now gets a second, unprivileged native sidecar, `dind-meter`, that mounts the `dind-data` PVC **read-only** and publishes a periodic statfs sample (bytes + inodes) for the worker to read and display as a "Disk dind" gauge. The worker itself never mounts `dind-data`. Separately, the worker watches that sample and, only when the volume is under sustained pressure, it is provably idle, and the api's heartbeat confirms it holds no unpublished work, runs a short allowlisted sequence of `docker image prune`/`docker builder prune` commands against the daemon. `dind-data` still never feeds `disk_pressure` and is never touched by the `/nix`+`/data` recycle in ADR-837; there is no automated recycle of the volume itself.

## Context

ADR-837 shipped disk observability and a delete-and-remint recycle for `/nix` and `/data`, but named the docker-tier `dind-data` PVC (the inner daemon's image/layer/build-cache store) as a deliberate v1 gap: it is neither metered nor recycled, and `disk_pressure` can never fire on it. It is also the volume most likely to fill on a docker-tier worker, since nothing there ever shrinks on its own (`render_dind.go`'s `dindDataDefaultSize` comment: "the cache PERSISTS across pod rolls ... with nothing garbage-collecting it"). This issue closes the metering half of that gap and adds a bounded, safe reclaim path that does not need the recycle machinery's delete-and-remint shape at all — the daemon can shed its own build cache without losing the PVC.

## The decisions

### D1 — Meter with a separate, unprivileged sidecar, not by mounting into the worker or the privileged daemon

The daemon container (`dindContainer`) is either a userns-remapped rootless process or, on nodes that cannot run rootless DinD, real root (ADR-89's non-rootless posture) — Decision 3 of that design is that the worker mounts none of its own crown jewels into that container's reach, and the inverse holds too: the worker must not gain a new mount into the privileged daemon's own data root either. So metering is a **third, purpose-built container** (`dindMeterContainer`): the worker's own uid, no privilege escalation, a read-only root filesystem, every capability dropped, `RuntimeDefault` seccomp, and exactly two mounts — `dind-data` read-only, and its own private emptyDir it writes its sample into. It has no startup probe, so it never gates the worker's own readiness; a missing or broken meter degrades to "no disk-dind gauge, no prune," never to a stalled pod.

Rejected: mounting `dind-data` (even read-only) directly into the worker, which would put the worker container in the position of reading a volume it does not otherwise touch and remove the clean "worker never mounts dind-data" invariant `render_test.go` pins. Rejected: `docker system df` over the daemon socket, which would need the client to be trusted with a broader surface than a plain statfs and ties the sample to the daemon being responsive rather than to the filesystem directly.

### D2 — The sample is an untrusted, versioned, bounded contract; the worker validates every field

The sample crosses a privilege boundary the daemon posture makes explicit: in the non-rootless posture the daemon runs as real node root, so a node-root actor could in principle forge the meter's output file if it ever shared a writer. It does not — the meter is the sole writer, on its own read-only root filesystem, in its own emptyDir the worker mounts read-only — but the worker-side reader (`dind-meter.ts`) treats the file as fully untrusted input regardless: bounded read (no unbounded slurp), regular-file-only (`O_NOFOLLOW`, rejects a planted symlink or FIFO), one strict versioned line pattern (`v1 <epoch> <frsize> <blocks> <bfree> <files> <ffree>`), every integer exactly representable, statfs relations that must hold (`bfree <= blocks`, `ffree <= files`), and a freshness window (90s) plus a small future-skew allowance. Any violation returns "no sample," never a throw and never the raw line logged. A forged or stale sample can only ever suppress or trigger the prune (D5's own gates bound the blast radius further); it cannot make the worker touch anything beyond the frozen prune allowlist.

### D3 — Prune is worker-local, gated on an exclusion claim, never dependent on the controller or the recycle machinery

Reclaiming build cache is an ordinary daemon operation (`docker image/builder prune`), not a volume-lifecycle operation, so it belongs to the worker process that already talks to the daemon over `DOCKER_HOST` — not to the controller, which has no daemon access and whose only lever is delete-and-remint (D1 of ADR-837). The worker's `DindPruneController` runs its own tick loop, reads the meter sample, and only acts once real exclusivity is established: it closes both claim lanes, drains claims already in flight (bounded wait), confirms no active run/chat/registry entry, and confirms `docker ps -q` lists nothing running. Any failure at any of those steps releases the gate immediately and retries on the next tick without spending the backoff — a busy worker is never penalized for being busy.

### D4 — A frozen, exec-time-enforced allowlist; nothing is ever removed by name

The only argv this module will ever run is `image prune -f`, `builder prune -f`, and — only if still over threshold after those two — `image prune -a -f --filter until=24h` (Created-timestamp based, so a recently pulled-but-unused image is not swept just because it is idle). A runtime guard re-validates every argv against the frozen table and rejects any token containing `volume`, `system`, `container`, or `rm` before exec, and the table itself is asserted against that guard at module load, so an edit that ever adds a forbidden command fails at import, not in production. Nothing here removes a container (stopped or running) or a volume by name: the run harness creates no containers this worker could prove it owns, so a stopped container may be a user's own deliberate state, and volumes are out of scope entirely — a full `dind-data` reclaim is delete-and-remint, which this module deliberately does not attempt (see Consequences).

### D5 — Custody is fail-closed, sourced from the same heartbeat channel PRD #1296 already built

"Provably no unpublished work" reuses `retaining_unpublished_work`, the same custody flag PRD #1296 introduced for recovery holds, rather than inventing a second signal. The controller trusts a flag it received on a **fresh** heartbeat (within two heartbeat intervals) that was also sent **after** the worker's own record of its last activity ending — the straddling-heartbeat ordering exists because a run ending can open a hold the api reports only on a *later* heartbeat, so a flag sent before that boundary must not be read as clearance. Anything else — a missing flag, a stale one, a heartbeat send that raced the run's own end — skips the attempt. This is fail-closed by construction: every branch that cannot prove clearance defers, none defaults to proceeding.

### D6 — The prune gate is env-driven off the same toggle as the `/nix`+`/data` recycle, and it rolls the fleet once

`UZI_DIND_PRUNE_ENABLED` is `true` only when `UZI_WORKER_DISK_RECYCLE_ENABLED` (the ADR-837 D9 master toggle) is on **and** the worker is not ephemeral (a run-bound worker is torn down with its run regardless). Reusing the existing toggle means an operator who has already accepted ADR-837's self-heal default gets this reclaim path for free, and flipping that one toggle is still the single place that turns automated actuation off across both mechanisms. The env var changes with the toggle, so flipping it rolls every non-ephemeral docker worker's pod once; upgrading to this release does the same the first time, since it is a new sidecar in the pod spec.

## Consequences

- `dind-data` gains a display-only gauge and a bounded, low-risk self-heal path (cache prune) without touching ADR-837's recycle mechanism at all — the PVC itself is never deleted by this code.
- **Residual: the non-rootless posture's node-root daemon could, in principle, forge the meter's sample** if it could ever write to the meter's own emptyDir — it cannot, by mount topology, but the reader's full validation (D2) is the belt to that mount-topology suspender. Even a forged sample is bounded to triggering or suppressing the allowlisted prune; it cannot reach outside D4's frozen argv.
- **Residual: docker compose has no equivalent meter or automatic prune.** A compose docker worker's `dinddata` volume is reclaimed only by the existing manual path (`docker compose down -v`, or `docker volume rm`).
- **Residual: a filesystem reporting zero total inodes (some btrfs configurations report `files=0`) yields no valid sample**, since the reader's statfs-relation check requires `files > 0`; the gauge and the prune both silently degrade to "no sample" there, not a crash or a false trigger.
- **Residual, matching ADR-837's own D6**: if the prune still leaves the volume over threshold (`dind-prune-insufficient`), there is no automated recycle of `dind-data` — an operator's only lever today is the same manual delete-and-reprovision fallback a plain worker's `/nix`/`/data` had before ADR-837. A DinD-only recycle arm is tracked as a follow-up (issue #1760), not built here.
- Flipping `UZI_WORKER_DISK_RECYCLE_ENABLED` now has one more visible effect beyond ADR-837's own two arms: it also gates whether a docker worker's DinD cache self-prunes, and toggling it rolls the docker-tier fleet the same way toggling any other pod-template field does.
