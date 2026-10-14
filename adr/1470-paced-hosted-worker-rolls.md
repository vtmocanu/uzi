# ADR-1470: Paced hosted worker rolls use singleton admission and incarnation proof

**Status**: Accepted; maintainer hosted verification remains open
**Date**: 2026-10-12
**Deciders**: approved issue #1470 plan, PRD Decision Log 1–17
**Related**: [PRD #1470](../prds/1470-paced-hosted-worker-rolls.md), [ADR-422](0422-decouple-worker-version.md), [ADR-1720](1720-agent-runtime-base.md), [ADR-1759](1759-dind-data-metering-and-prune.md)

## Context

A deliberate worker image/template change previously let idle workers roll together
and busy workers become cordoned before replacement capacity was proven. A bad
canary could therefore remove the rest of an owner's usable capacity, including
when a fleet-wide wait outlasted the drain deadline. Worker Deployments use
Recreate with RWO volumes; overlapping pods are not the availability mechanism.

## Decision (summary)

Keep a hard singleton controller. Admit one canary per owner-specific coverage
pool, require current-incarnation registration and heartbeat proof, then admit
bounded replacements while retaining usable capacity. Holding is not rollback.
Use bounded Kubernetes metadata and observed committed Deployment state to
reconstruct progress, without an API journal, tickets, epochs or operation store.
Worker images and the release-pin guards remain unchanged.

## The decisions

### D1 — Singleton topology is a correctness boundary

The enabled chart accepts `workers.controller.replicaCount` of `0` or `1` and
uses Recreate. Disabled hosting/controller remains supported, and zero replicas
stops reconciliation. Extra controllers outside the chart are unsupported:
optimistic writes are fencing for a singleton's delayed work, not leader election
or multi-replica coordination. Worker Deployments also retain Recreate.

The existing `ck_workers_isolated_lane` constraint (migration 00274) requires
isolated workers to be hosted and ephemeral. Those workers retain their existing
provisioning health reasons and are excluded from pacing; this change does not
relax that isolation invariant to create a persistent isolated pool.

### D2 — Freeze owner-specific coverage, not resource shape

A pool key is owner, placement lane, effective registered scheduling capability
set, protocol capability set and two checker-eligibility booleans. The wire name
`ordinary_coverage` means legacy checker run-slot fallback, **not** ordinary
primary-job support; `checker_coverage` means dedicated checker-slot coverage.
Nullable checker slots and protocol determine these booleans. Effective Docker
placement contributes Docker scheduling coverage even if self-report omitted it.
Size and template do not split pools. Ephemeral workers are outside pacing.

`workers.rollConcurrency` defaults to `1` and must be a positive integer. Before
promotion there is one canary, regardless of this limit. After promotion, busy
draining reservations and pending/unknown writes count as occupied slots.

Admission protects physical usable capacity across overlapping pools belonging
to the same owner/lane. In a multi-worker pool, another healthy provider must
individually cover the frozen full capability/eligibility set; a union of partial
workers is insufficient. Capacity is not borrowed from another owner or lane.
A single-worker pool can roll with an outage, subject to the wider owner/lane
last-provider check. Healthy old busy workers can remain usable providers:
being busy is not being offline, and no free-slot guarantee is made.

### D3 — Deployment metadata reconstructs admission and completion

`uzi.dev/image-roll` is an at-most-8192-byte metadata annotation outside the pod
template/hash. It freezes target hash, image, exact version, coverage, prior
registration nonce/version, canary and completed history. Target Deployment
state plus a confirmed metadata reservation describe an in-flight turn.
Completed canary history survives restart and partial cleanup; the original
cohort closes before a later pin adopts current registered coverage. Completed
pacing is omitted from authoritative fleet outcomes, so independent maintenance
does not manufacture a new image canary.

Reservation, cancellation and template patches are synchronous and carry
observed UID/resourceVersion preconditions. Unconfirmed writes suspend sibling
work until fresh observation rather than freeing a slot. Cancellation advances
resourceVersion to fence delayed writes from an obsolete pin. A changed desired
pin does not erase unresolved replacement evidence.

Legacy volume recycling retains a zero-replica Deployment while old pods and
selected volumes stop/rebuild. ADR-1759's fenced DinD lifecycle instead requires
deleting the Deployment and replacing it with a new UID. During that necessary
gap, mirror the **same** reservation temporarily on the retained /data PVC,
through bounded existing PVC lists and UID/resourceVersion-conditioned patch
and cancellation. No PVC get or Secret read is added. A complete fresh list can
confirm an identical non-nil marker without an artificial no-op revision advance;
changed markers and cancellation require confirmed revision advance. This
handoff is not an additional journal and preserves legacy /data self-heal.

### D4 — Promotion proves the current incarnation

The promotion boundary in `controller/internal/kube/roll_pacing.go` requires:

- Current Deployment UID → ReplicaSet UID → pod controller ownership, with one
  current nonterminating pod and the target template hash.
- A running worker container on the target image in both pod spec and status.
- API registration bound to that pod UID and the **exact** expected version.
- A nonempty registration nonce changed from the reservation's prior nonce,
  followed by a heartbeat proven for that current nonce, fresh at API evaluation.
- Frozen coverage still supplied, with no conflicting maintenance or pressure
  fence and no stuck roll observation.

Custody protects sources rather than negating positive serving evidence (#2959).
Old and current providers can supply coverage while holding custody only when
uncordoned; no free-slot guarantee is added. Custody still independently fences
data-PVC destruction, drain release and duplicate-canary cancellation. A promoted
peer's completed Deployment pacing annotation can be retired while custody stays
held: this metadata-only cleanup neither releases source custody nor changes its
data PVC, and must not prevent the next desired generation from rolling.

Ready alone is insufficient. A registration nonce identifies a registration,
not a pod: an old process can re-register at the same version and change nonce.
Likewise A→B→A pin changes can make an old UID/version look current. These are
ambiguous counterexamples without ancestry/current-UID heartbeat proof. Missing
proof holds admission and requires explicit recovery; it never silently enables
unpaced replacement.

Existing worker registration Name transport carries
`UZI_WORKER_NAME=uzi-pod:$(UZI_POD_UID)`, with `UZI_POD_UID` from the downward API.
The API separates this hosted identity hint from the user-chosen name; worker
images and that label remain unchanged. Ordinary API heartbeat liveness remains
independent of nonce-proven promotion evidence. It supports healthy retained old
providers and fresh confirmation of an old worker's drain, including the first
same-image template roll introducing pod UID transport.

### D5 — Target version is independently verified release identity

Published charts carry `workers.rollTargetVersion` from the pinned image's
release-pipeline-verified baked stamp, including `+g<commit>` (#2906). Packaging
resolves each template tag once, verifies its exact release-workflow signature
and reads its config at that same digest; all templates must agree. The tracked
default stays empty because when the pin is the release being cut, its stamp
names the commit that contains the chart. A per-cluster override still wins; custom worker pins need their own verified
matching override. ADR-1720's published bare SemVer tag cannot identify
that complete stamp. Do not infer the target from the replacement, substitute
the independently advancing API version, or strip build metadata. A missing
mapping holds before cordoning. The controller's fully specified SemVer-tag
mapping is not a way to recover metadata absent from a bare published tag.

Registration retains its existing 64-byte display sanitization. Exact versions
arriving unchanged within that bound can prove promotion. Sanitized or longer
versions withhold incarnation binding: different builds sharing a truncated
prefix cannot promote each other. Explicit force-roll is the escape hatch for
unsupported proof, not an automatic relaxation of exact matching.

### D6 — Holds and deadlines preserve independent fences

Any unresolved stuck participant halts further admissions in its pool, including
a failure after canary promotion. Release clears only an `image_roll`-owned drain
on a healthy retained old worker or a proven healthy current replacement.
Independent maintenance, pressure and custody fences remain authoritative.
There is no automatic revert, timeout override of a hold, or rollback.

ADR-422's `workers.drainDeadline` (default `24h`) applies to an **admitted busy
image turn**. Before mutation, a bounded fresh re-cordon/poll confirms desired
state, independent fences, ordinary API liveness and the **current** expired
drain timestamp at API evaluation. A stale expired timestamp cannot authorize a
clear/re-cordon that reset the clock. Independent maintenance deadlines retain
their meaning. `workers.forceRoll: true` explicitly bypasses pacing, holds,
evidence and busy waits for drifted workers; it can remove remaining capacity.
Custody and independent maintenance protections still apply. Restore `false`
after the emergency; no automatic rollback follows it.

### D7 — Deploy API first; diagnostics do not control admission

Deploy API, then the new singleton controller/chart with the worker pin steady,
verify poll evidence/report support, then advance the pin and exact version.
Introducing UID environment changes the template and causes a paced roll at
that steady pin. New-controller/old-API missing proof fails closed; an old
controller with the new API remains unpaced. Downgrading an active roll is unsafe.

The API holds no Kubernetes credential. Diagnostics remain read-only and report
publication errors do not govern claims or controller admission. Reports distinguish authoritative `Kind: fleet` reports from partial
`Kind: readiness` receipts, preserves pacing with an API receipt TTL independent
of pod phase, and retains suitable owner/lane/capability/binding health checks.
Healthy retained busy workers remain busy; stale evidence falls back to existing
health logic. Local implementation and execution evidence are recorded in the PRD.

Preserve `workersvc.ReasonWorkersUpgrading` as the exact drain-only queue-wait
exemption with finite eligible timestamps. Held/offline new waits are genuine
queue waits; pacing state alone does not exempt them. Existing capacity overlap
and queue severity rules remain as recorded in the human spec.

### D8 — Poll bounds reject incomplete evidence atomically

The controller decoder bounds the total poll at 512 workers and independently
at 4 MiB compressed **and** decompressed, accepts one gzip member, and bounds
fields before typed worker allocation. Rejection fails the entire poll without
truncation or token-bearing payload diagnostics. The chart controller memory
limit is 256Mi by default. The PRD records a near-limit allocation measurement
of 31,900,920 bytes (about 32 MB); this is not a maximum live-heap guarantee.

## Verification boundary

The PRD records local fake-client, race and PostgreSQL evidence. Real
API-server ordering/conflict rejection and hosted retained-fleet availability
remain **open**. Local health/report persistence and retained-N-1 claim replay
checks do not establish real API-server behavior or production availability.
The existing KinD smoke disables the controller and covers the envelope only.
The
[maintainer runbook](../docs/hosted-worker-roll-verification.md) defines the open
bad-tag/good-tag, restart, delayed-write, nonce and recovery checks.
