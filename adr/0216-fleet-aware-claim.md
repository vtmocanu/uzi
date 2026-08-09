# ADR-216: Where a queued run is placed across a worker fleet

**Status**: Proposed (the PRD is not fully complete — M5's real-fleet validation is still owed; see Consequences)
**Date**: 2026-08-09
**Deciders**: architect (M0 design gate); the 2026-08-03 one-pass architectural review recorded in the PRD (five blocking findings, all applied — two established against a real `postgres:17`); user decision of 2026-08-03 on the #216/#84 ordering (D5)
**PRD**: [prds/216-worker-load-balancing.md](../prds/216-worker-load-balancing.md) (GitLab issue [vtmocanu/uzi#216](https://gitlab.example.com/vtmocanu/uzi/-/issues/216)) — the PRD carries the twelve decisions, the milestones and the correction record; this ADR carries the placement decision and the seam it establishes, because both outlive #216: the seam is consumed by PRD #84, and the placement/enforcement boundary is the one most likely to be read as a contradiction of ADR-42.
**Related**: refines [ADR-42](0042-worker-run-concurrency.md) (run PLACEMENT, not cap enforcement — see Decision 4). Establishes the eligibility seam consumed by **PRD #84** (capability-aware scheduling, GitLab issue #84).

## Decision (summary)

Run placement across a fleet is decided **server-side, inside the `ClaimRun`
statement's own MVCC snapshot**. A worker holding `n` active run-lane runs
**defers** a queued run — declines to claim it, so a peer does on its next poll —
when a strictly-less-loaded, live, eligible peer with a free slot exists. Resume
affinity outranks the spread, a run older than a grace window bypasses it
entirely, and a minimum-loaded worker never defers. Per-(worker,run) eligibility
is expressed **once**, in `fn_worker_can_claim`, and applied identically to the
claiming worker and to every candidate peer — this function is a seam PRD #84
extends rather than copies. The spread reads a peer's advertised
`max_concurrent_runs` to **target** a deferral; it still does not **enforce** a
cap on the claiming worker, so ADR-42's decision stands unweakened.

Entry point: `ClaimRun` in `api/internal/store/queries/runtime.sql` (the spread
clause under the `WHERE id = (SELECT …)`); `fn_worker_can_claim` and the
`idx_runs_worker_active` partial index in
`api/internal/store/migrations/00113_fleet_aware_claim.sql`; the sole caller
`Service.Claim` in `api/internal/workersvc/service.go`; the observability reason
in `api/internal/workersvc/health.go` (`queuedReason` → `reasonAllWorkersBusy`).

## Context

The run is uzi's unit of work; the worker claims it pull-based over an
outbound-only protocol. Before this change, `ClaimRun` selected the oldest
claimable queued run ordered by resume affinity then `created_at ASC`, with no
knowledge of how many runs the claiming worker already held or whether a peer was
idle. Three independently-sensible mechanisms compose into pile-up: the claim is
fleet-blind; the advertised cap is observability-only and never enforced
server-side (ADR-42); and a worker that claims does not yield, so the first
worker to poll an idle fleet takes run 1 and is back for run 2 within one
round-trip while its peer is still inside its poll interval. The cost is CPU
contention between co-located runs sharing a 4-CPU limit against a 1-CPU request
(PRD Problem section; the clean signal is the same test suite at a constant tally
spreading 2.2–2.65×).

The PRD's opening evidence for the bug was **withdrawn as unsound** (the two runs
it cited were claimed against a single-worker fleet — there was no peer to choose
over). The mechanism argument stands from code; the *observation* does not yet
exist, which is why M5 owes a real-fleet control (see Decision 8).

The decisions below are the load-bearing ones. Their full derivation, the
measured `postgres:17` results, and the risk analysis (starvation, claim-path
cost, terminally-failing-peer sink) live in the PRD's decision log and are not
duplicated here.

## Decision

### 1. Server-side, decided inside the claim statement's snapshot (D1/D2)

The placement predicate lives **in the `ClaimRun` UPDATE**, not in a worker-side
post-claim sleep and not in a service-layer read-then-decide.

- A worker-side sleep (make `worker.ts` pause a poll after a successful claim) is
  timing luck, not placement: it cannot see the fleet, and it *slows* backfill on
  a single-worker fleet — which the design names as the common case. Rejected.
- A service-layer read-then-decide adds a round-trip and a read-then-act window.
  Putting the predicate in the statement means the peer-load counts come from the
  claim's own consistent snapshot, with no extra query.

**This does NOT provide cross-worker mutual exclusion, and that is safe.**
Verified by `EXPLAIN (ANALYZE)`: the peer-load aggregates evaluate in
`InitPlan`/`SubPlan` nodes **before** `LockRows`, so two concurrent claims
evaluate peers independently. The one direction that could bite — two workers
both deferring the same run to each other — cannot happen under a strict
comparison: the minimum-loaded set is never empty and a minimum-loaded worker
never defers. An uncommitted peer claim only makes that peer look *emptier*,
which can make *me* defer but can never cause a double-claim, because the run
row's `FOR UPDATE SKIP LOCKED` is unchanged and binds the locking query level
only. The corrected PRD retains the original (wrong) "it would livelock"
argument as D2 precisely so the correction is legible.

### 2. Eligibility is ONE expression — the seam PRD #84 extends (D5)

`fn_worker_can_claim(is_docker, allowlist, run_repo_id, run_kind)` (migration
00113) is the **single** per-(worker,run) claim-eligibility predicate. `ClaimRun`
calls it in two places against the same arguments-shape: once for the claiming
worker, once inside the `NOT EXISTS` peer scan for each candidate peer. This is
deliberate and load-bearing: two hand-written copies drift, and each drift
direction is silent — one defers to a peer that then cannot claim (a strand); the
other quietly stops spreading. Answering "could peer P claim run R?" **must** use
the identical filter the candidate scan applies to the claiming worker, or the
seam is a lie.

Record this as a **seam other code must respect, not merely a helper**:

> **PRD #84 (capability-aware scheduling, GitLab issue #84) adds its
> `required_capabilities ⊆ worker_caps` test by EXTENDING
> `fn_worker_can_claim`'s signature, not by writing a second predicate.** Per the
> user decision of 2026-08-03, #216 lands first and writes eligibility as one
> reusable expression that #84 extends; both features carry a mutual reference.
> #84's own "no eligible worker" pre-claim state is indistinguishable from this
> PRD's deferral, which is a second reason the two must share the predicate.

A caller-side consequence rides on this seam: `Service.Claim` fetches the docker
repo allowlist **unconditionally**, even for a non-docker claiming worker, so the
peer scan can evaluate whether a *docker* peer could claim a *repo* run. Without
the unconditional fetch, spreading a repo run to a docker peer would silently
stop. The fetch fails closed only for a docker claiming worker (whose own
eligibility depends on it); a non-docker worker degrades to "don't spread repo
runs to docker peers this cycle" and still claims itself.

Note a third, pre-existing copy of the run-lane active-count shape lives in the
UI queries (`ListWorkersByUser`, `ListAllWorkers` `active_runs`). The peer/my
active counts in the spread use the **same** status set and `kind <> 'chat'`
filter, so a placement is never contradicted by the "N/M runs" the UI displays.

### 3. Fail-open, structured as `NOT EXISTS` and bounded in time (D4/D7)

The peer test is a two-valued `NOT EXISTS`, **never** a scalar occupancy
comparison. `workers.max_concurrent_runs` is nullable by design (an older worker
image advertises no cap; out-of-band reports are dropped to NULL). A scalar
occupancy comparison propagates that NULL into three-valued logic and filters the
row out — measured on `postgres:17`, this strands both a NULL-cap peer *and, more
totally, a single-worker fleet* (`min()` over an empty peer set is NULL, and
`x <= NULL` is NULL, not false — so the row is never claimed, forever). Because
the single-worker fleet is the common case, the natural scalar shape breaks first
and breaks completely. `NOT EXISTS` makes fail-open **structural** rather than
something every `COALESCE` must remember: an empty peer set, a NULL-cap peer, and
a NULL claiming-worker cap all yield "no strictly-better peer" → I claim.

The spread is also bounded in **time**, not only by condition. A queued run older
than `@spread_cutoff` (`WORKER_SPREAD_GRACE`, default `3× WORKER_POLL_INTERVAL`)
bypasses the spread predicate entirely, mirroring the resume `@affinity_cutoff`.
No condition-list can cover the case where the state persistently and *correctly*
says "an idle eligible peer exists" and that peer never claims (a wedged loop, a
worker mid-roll, a peer whose every claim dies in assembly). The grace guarantees
**the spread can never make a run unclaimable** — which is what turns success
criterion 2 into a testable invariant ("every queued run is claimable within
`spread_grace`") instead of an unbounded "never stranded". Combined with "a
minimum-loaded worker never defers", claimability is structural.

### 4. Read a peer's cap for TARGETING, not ENFORCEMENT — and why this is not what ADR-42 rejected (D8)

The peer scan reads a candidate peer's advertised `max_concurrent_runs` to decide
whether it is a **plausible deferral target**: a peer qualifies only if
`peer.active < peer.cap`, and a **NULL cap makes a worker not a target** (never
defer work to capacity you cannot establish). Without this, worker A (cap 4, holds
2) defers to worker B (cap 1, holds 1) because B holds fewer; B's own semaphore
refuses; A defers again; repeat forever. On a heterogeneous fleet that is a
steady state, not a transient window — so reading the peer cap is required for
correctness, not an optimisation.

**This does not contradict ADR-42.** ADR-42 rejected the server *enforcing* a cap
on the *claiming worker's own* slots (Option B: a unique index / `NOT EXISTS`
guard that makes "one active run per worker" a schema truth), because the worker
is the sole claimant of its own slots and enforcement would block PRD #39's chat
lane and freeze a scaling policy into the hardest layer to change. Reading a
*peer's advertised* cap to decide where to *place* work is a different act on a
different subject:

- The claiming worker still self-bounds via its own worker-side semaphore; the
  server sets no ceiling on it. A worker at capacity never calls the claim path
  at all.
- Placement/affinity across workers is an invariant ADR-42 **already assigns to
  the server** (it lists affinity, per-issue uniqueness, per-branch exclusion as
  the cross-worker invariants the server does enforce). This decision adds
  balanced placement to that same list; it does not add cap enforcement to it.

The boundary, stated crisply: **the server may read any worker's advertised cap
to route work; it still may not use a cap to forbid a worker from claiming its own
slot.**

### 5. Heterogeneous-fleet tie-break by integer cross-multiplication (R3)

"Fewer active runs" is wrong under unequal caps: a cap-8 worker at 2/8 should take
work before a cap-2 worker at 1/2, and raw count gets that backwards. Once the
free-slot filter (Decision 4) is in place, load comparison is only a **tie-break**
among peers that already have room. Compute it as
`peer.active * my.cap < my.active * peer.cap` — integer cross-multiplication,
exact, no division, no float ties. A NULL `my.cap` makes the product NULL → the
peer row is excluded → I claim (fail-open, consistent with Decision 3). A zero
`my.active` makes the right-hand side zero → no peer qualifies → I always claim
(the minimum-loaded worker never defers). The `00055` migration has no CHECK on
the cap, so the comparison does not rely on any handler-side clamp holding in SQL.

### 6. Liveness via `last_heartbeat_at`, not `workers.status` (D6)

A peer counts as live iff `last_heartbeat_at >= @heartbeat_cutoff`
(`now() - WORKER_HEARTBEAT_STALE`, default 45s), passed as a param mirroring
`@affinity_cutoff`. `workers.status` only moves when the sweeper runs and lags by
up to one sweep, which would let a corpse look like a valid deferral target. No
corpse window exists with the heartbeat test: a stale worker's runs are requeued
by the same sweep that would stop it being a target, so it loses its `active_runs`
and its target-eligibility together.

### 7. Observability lives on the reason-resolver path, not the claim statement (M4)

A deferral and an idle queue were previously indistinguishable — both surface as
`ErrNoRows → nil,nil → 204`. Worse, the queued-health detector then
*mis-labelled* a deferral as `waiting_worker` ("waiting for a worker to pick up
this run"), which is guaranteed wrong when a worker asked and the server deferred:
`queuedReason` returned `reasonWaitingWorker` whenever any worker was online, and
in a deferral the peer is by definition online.

The fix is a new health reason, `reasonAllWorkersBusy` ("all your workers are
busy; this run is waiting for a free slot"), added to `queuedReason` in
`health.go`, backed by a new off-hot-path count query
(`CountOnlineWorkersWithFreeSlotForUser`). It resolves most-actionable-first:
vault locked → no worker online → **every online worker at cap** → plain wait.

**Why the reason path, not a reason column on the claim statement** (the PRD's
open question, resolved here): a reason column returned by `ClaimRun` would change
the shape and query-count of the **hot path** — the statement every idle worker
runs every poll — and would collide with PRD #84's own intended reshape of that
same statement. The reason-resolver path already runs only for a queued run past
its health threshold (off the hot path), needs no new column on `runs`
(`health_reason` is free text; only `runs.health` is CHECK-constrained, so no
migration is owed), and keeps the diagnostic cost off the claim entirely. The
placement decision stays a pure `WHERE`-clause concern; the *explanation* is
computed lazily where a human actually reads it.

### 8. Manual real-fleet validation is owed and unmet (M5)

The end-to-end two-worker behaviour **cannot be exercised in CI or a worktree**
(the store integration harness hardcodes a single worker; per-run eligibility only
diverges on a mixed docker/non-docker fleet, which the live fleet is not). It is
recorded here as an explicit debt, not as done. The exact procedure to run
against a real fleet:

- **(a) Pre-change control — the observation the Problem section lacks.** *Before*
  the predicate lands, queue two runs together against two genuinely-idle workers
  and confirm **both land on one worker**. Without this control, the PRD fixes a
  bug nobody has watched happen.
- **(b) Post-change one-per-worker.** After the predicate lands, the same two runs
  land **one per worker**, repeatably.
- **(c) Saturation.** Three runs on two cap-2 workers place **2+1 (or 1+2), never
  3+0, and never a strand** — proving both the spread and its fail-open grace.

Until run on the real fleet, success criteria 1, 7 and 8 are **unverified**. This
is why the ADR status is Proposed.

## Consequences

- Placement is now a server responsibility exercised inside `ClaimRun`; the worker
  side needs no change (a deferral is just a `204`/idle, and the worker re-polls).
- `WORKER_SPREAD_GRACE` (default `3× WORKER_POLL_INTERVAL`) joins
  `WORKER_AFFINITY_GRACE` and `WORKER_HEARTBEAT_STALE` as a claim-timing knob.
- The FIFO ordering guarantee is intentionally relaxed: a per-row predicate can
  skip an older run (deferred to a peer eligible for it) and take a younger one.
  Accepted (D3).
- `fn_worker_can_claim` is a **frozen seam**: PRD #84 extends its signature; no
  other package writes a parallel eligibility predicate. A future repo-less run
  kind fails *closed* (the judge exemption is scoped to `kind='judge'`), by
  design — a deliberate choice to add, not a default to inherit.
- Two documented residual hazards are bounded, not eliminated: a terminally
  -failing peer never accumulates `active_runs` and so would *systematically* win
  deferrals (a sink, not a race — the `@spread_cutoff` grace bounds each run's
  exposure but not the selection; treat as part of the eligibility predicate as
  #84 lands capability filtering); and the spread balances **slots**, while the
  motivation is **CPU** — a parked or judge run holds a slot and burns little CPU,
  and chat is excluded entirely (D9/D10). Slots remain the right proxy; a
  load-based scheduler is out of scope.
- **M5's real-fleet validation is unmet** (Decision 8). The ADR is Proposed and
  should not be read as recording a fully-validated outcome; it is accepted when
  #216 completes.
- The `WITH ADR-42` boundary must be cited together: anyone later tempted to make
  the server enforce the claiming worker's cap should read Decision 4 here and
  Option B in ADR-42 before doing so.

## Alternatives (rejected)

- **Worker-side post-claim sleep** — timing luck, fleet-blind, slows
  single-worker backfill (Decision 1).
- **Service-layer read-then-decide** — extra round-trip and a read-then-act
  window the in-statement snapshot avoids (Decision 1).
- **Scalar occupancy comparison** instead of `NOT EXISTS` — measured to strand
  NULL-cap peers and, totally, single-worker fleets via three-valued logic
  (Decision 3).
- **Raw "fewer active runs"** as the load metric — inverts placement under
  unequal caps (Decision 5).
- **A reason column on the claim statement** for M4 — changes the hot-path query
  shape and collides with #84's reshape; the off-hot-path reason resolver is
  cheaper and isolated (Decision 7).
