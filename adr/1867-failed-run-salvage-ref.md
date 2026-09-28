# ADR-1867: A failed run's last published checkpoint gets a create-only, run-scoped salvage ref

**Status**: Accepted (PRD #1867 M1-M5 implemented; M6, the real-forge gate, is
maintainer-owned and pending)
**Date**: 2026-09-28
**Issue**: [vtmocanu/uzi#1867](https://github.com/vtmocanu/uzi/issues/1867)
**PRD**: [prds/1867-failed-run-salvage-ref.md](../prds/1867-failed-run-salvage-ref.md)

## Decision (summary)

> On a forge listed in `UZI_SALVAGE_FORGES` (default empty: off), a periodic
> sweep copies a failed, checkpoint-eligible run's last published checkpoint
> into a run-scoped `refs/uzi-salvage/<run-id>`, but only when the tip is
> still verified live under `refs/uzi-checkpoints/<branch>` or
> `refs/uzi-recovery/<run-id>`. The copy is create-only and expires after
> `UZI_RECOVERY_READY_RETENTION`. Salvage never deletes or moves a ref that
> [ADR-1810](1810-checkpoint-retention-follows-custody.md)'s custody
> retention manages; it only ever reads those refs as a source and only ever
> creates or CAS-deletes its own.

## Context

PRD #1867 was filed from the recovery of run `ec4eedfa` (issue #1856): a
failed run's only remote copy of its committed work, a checkpoint published
minutes earlier, was deleted by the terminal-cleanup path before anyone
could fetch it, and recovering it needed kube access to the worker's volume
and a hand-made git bundle. The original design (drafted before PR #1819
merged) was **promote-then-delete**: the sweep would create a salvage ref
and then delete the branch checkpoint ref itself, deferred off the terminal
path so a run's failure never raced the ref it needed.

While this PRD was in flight, [PRD #1810](../prds/1810-retain-failed-run-checkpoint-ref.md)
landed on `main` (PR #1819) and closed most of that same gap from a
different angle: a failed or cancelled run's checkpoint ref is now **kept**
(never deleted) while any custody hold of the run is open, and moved to
`refs/uzi-recovery/<run-id>` rather than lost when a sibling run needs the
branch slot. `#1810` also added `pushbroker.CreateRef`, a CAS, create-only
ref-create primitive, and `Service.Sweep`'s own `ReconcileCheckpointRetentions`
pass.

This redesign follows from that overlap. Salvage no longer owns any
delete-then-promote sequence over `#1810`'s refs at all: **#1810's state
machine owns the branch checkpoint ref and the recovery ref, end to end**,
including when either is deleted. Salvage's only remaining job is a
narrower one: an independent, create-only, expiring **archive copy**,
useful specifically because it does not depend on `#1810`'s custody state
ever settling, or on a pre-migration run ever having a `checkpoint_retentions`
row at all.

## Decision

### The namespace, and why it is outside `refs/uzi-checkpoints/`

`refs/uzi-salvage/<run-id>` (`pushbroker.SalvageRefPrefix`, `pushbroker.SalvageRef`)
is a third uzi-owned namespace, alongside `refs/uzi-checkpoints/<branch>`
(the live branch slot) and `refs/uzi-recovery/<run-id>` (#1810's
supersession ref). It is outside `refs/uzi-checkpoints/` for the same
reason #1810's recovery namespace is: every worker's bare mirror fetches
`+refs/uzi-checkpoints/*` and a new run seeds from
`refs/uzi-checkpoints/<branch>`, so a salvage ref living there would leak
into an unrelated run's seed. It is run-scoped, like the recovery ref, so
two runs on the same branch never collide over it, and a salvage ref
outlives the branch slot's own churn.

### Create-only on top of #1810's `CreateRef`, widened to `refs/uzi-salvage/`

Salvage reuses `pushbroker.CreateRef` and the generalized, ref-name-aware
`Delete` #1810 already added, rather than a parallel `Promote` primitive.
`CreateRef`'s allowed-prefix and `SourceRef` validation (`validateCreateRef`)
widened to accept `refs/uzi-salvage/*` as a target, with a `SourceRef` under
either `refs/uzi-checkpoints/` or `refs/uzi-recovery/`. This is the PRD's
own default (reuse `CreateRef` unless it cannot serve); it could, so no
deviation is recorded. `createSalvageRef` (`api/internal/workersvc/salvage.go`)
lists all three refs in one `ListRefTips` call, resolves a source (the
branch ref or the recovery ref, whichever is at the recorded tip), and
calls `CreateRef` with `Old = zero`, an empty pack, never forced, exactly
as #1810's recovery-ref create does. There is no `Promote` primitive in
the shipped code: the M1 design's combined create-then-delete operation
does not exist because salvage never deletes the source ref.

### Never deleting or moving a ref #1810 manages

Salvage's own `Delete` calls are scoped to `refs/uzi-salvage/<run-id>`
only, through `deleteSalvageRef` and `deleteUnrecordedSalvage`, both CAS on
the row's own recorded tip. Salvage only ever **reads** the branch
checkpoint ref and the recovery ref, as `createSalvageRef`'s source
candidates; it never writes, moves or deletes either. Nothing in
`salvage.go` calls `deleteCheckpointFn`; the source code comment at the top
of the file states this as an invariant, not an implementation detail:
"this file never deletes or moves them, never calls `deleteCheckpointFn`,
and never changes a custody hold." `pushbroker.Delete` itself is not
scoped to salvage's namespace — it also accepts a checkpoint or recovery
ref as `o.Ref` (that is how #1810's own callers use it) — so reuse alone
does not make the no-delete invariant mechanical. What does is that every
`Delete` call salvage makes passes `pushbroker.SalvageRef(row.RunID)` with
a non-empty `ExpectedOldTip`: a literal grep of `salvage.go`'s call sites,
not `Delete`'s own prefix check, is what a bug in this file could violate.
That invariant is pinned by an AST test,
`TestSalvageSourceNeverDeletesCheckpointRefs`
(`api/internal/workersvc/salvage_test.go`), which fails the build if a
future edit passes anything but `pushbroker.SalvageRef(...)` as `Ref` from
this file.

### Lifecycle and states

`run_salvage` (migration draft `00268`, one row per run, keyed on `run_id`,
no cascade from `runs`/repos/owners — the same ADR-1296 rule #1810 already
follows) tracks eight states: `pending` (recorded, not yet confirmed
created), `promoted` (a salvage copy exists — the name is historical, kept
so the DTO's `salvage_state` values needed no rename once the design
settled on create-only), `unavailable` (neither source ref was at the
recorded tip), `refused` (the salvage ref already existed at a different
tip: never overwritten), `failed` (no salvage ref recorded; gave up after
retries), `skipped_secret` (`fail_origin = push_secret_blocked`: never
salvaged), `expired` (the salvage ref was CAS-deleted, or confirmed
absent, or its delete gave up at the hard ceiling), and `disabled` (the forge left `UZI_SALVAGE_FORGES` before a
create landed). `enqueueSalvage` excludes `plan_rejected` failures (a
plan rejection is not a code failure worth archiving) and only enqueues on
a forge listed in `UZI_SALVAGE_FORGES`; a `push_secret_blocked` run is
recorded `skipped_secret` at enqueue time, never attempted.

### The RESTRICT live-pointer guard and FK-only serialization

`run_salvage.live_run_id` is a nullable `ON DELETE RESTRICT` foreign key to
`runs`, set for as long as a remote salvage ref may exist (`pending` or
`promoted`) and cleared once the row settles into any of `expired`,
`disabled`, `unavailable` or `refused` — the last two settle a row that
never had a confirmed create, so there is no ref left to hold the pointer
for. At `salvageHardCeiling` (30 attempts) `giveUpSalvage` clears the
pointer WITHOUT confirming the ref is gone: the cleanup that would confirm
it has itself failed every time, so the row is settled `failed` unverified
and `last_error` (plus an error log) names the salvage ref and tip that may
still remain on the forge for manual deletion. A due expiry whose delete
has failed that often is settled `expired` the same way
(`giveUpSalvageExpiry`). This is the same live-pointer
pattern ADR-1296 established for `recovery_custody_holds`. Deleting the run, or cascading through a repo or
forge-connection removal, fails on `run_salvage_live_run_id_fkey` (23503)
instead of silently dropping the only record of a public ref; the repo- and
connection-removal handlers (`api/internal/handler/forge.go`) turn that
23503, and a pre-emptive count, into an owner-facing 409 naming the live
salvage refs and pending runs.

Unlike #1810, which serializes its multi-step supersession and settlement
operations under a per-run PostgreSQL session advisory lock (a fence
against a lock lost mid-operation, plus a post-settlement audit — see
ADR-1810's "Per-run session advisory lock, not a row lock"), salvage takes
**no advisory lock at all**. Its operations do not
need one: enqueue is a single `INSERT` guarded by the RESTRICT FK itself (a
23503 on `run_salvage_live_run_id_fkey` at insert time means the run was
deleted between the candidate read and the insert — a benign, logged skip,
not a race to prevent), and every subsequent state transition is scoped to
one row by `run_id`. `RecordSalvageCreated`, `MarkSalvagePromoted` and
`SettleSalvage` are each idempotent against a re-run (a repeat call is a
no-op or keeps the first-recorded values); `RecordSalvageAttemptFailed` is
not idempotent — it always increments `attempts` and can move the row into
`failed` — but it is **re-drivable**: a re-run after a crash or a budget cut
finds the row's persisted state (its recorded salvage ref, its attempt
count) and picks up from there rather than repeating a stale decision. The sweeper runs `SweepSalvage` serially
from one goroutine, so even the in-process round-robin lead toggle
(below) needs no lock. This is deliberately less machinery than #1810's
lock-plus-fence-plus-audit: salvage's operations are single-row and
re-driveable from persisted state alone, so the FK is sufficient
serialization against the one race that matters (a concurrent run delete),
and the recorded tip plus the forge's own CAS is sufficient serialization
against every other salvage writer, because there is exactly one salvage
writer (the sweep) and it never contends with itself.

### The secret and plan-reject policy

A run whose `fail_origin` is `push_secret_blocked` is recorded
`skipped_secret` at enqueue and never reaches a broker call: the secret
that blocked its push is never given a second, unscanned publish target.
A `plan_rejected` failure is excluded from the enqueue query entirely
(`ListSalvageCandidates`'s `fail_origin IS DISTINCT FROM 'plan_rejected'`
filter) — rejecting a plan is not a run whose committed work is worth an
archive copy.

### The exposure tradeoff

[ADR-1597](1597-midturn-checkpoint-durability.md) documents that GitHub
milestone checkpoints, and park/shutdown/pause/capture checkpoints, are
published **without** the pre-publish secret scan. Keeping a salvage copy
adds discoverability of an already-published, possibly unscanned tip for
up to the retention period (`UZI_RECOVERY_READY_RETENTION`, default 168h)
— the same tradeoff #1810's retention already accepts for the branch and
recovery refs, now extended to a second, independently-expiring ref.
**Expiry counts from the salvage ref's own creation (`salvage_created_at`),
not from #1810's settlement of the source ref.** The two clocks are
unrelated: a salvage copy can expire and be removed while the source
branch checkpoint is still retained under an open custody hold, and it can
still be pending removal after the source ref has long since settled and
been deleted. `push_secret_blocked` has no carve-out on the source side —
every terminal path, secret-blocked or not, goes through #1810's
`retainOrDeleteCheckpoint` like any other. Salvage's only secret-related
policy is its own: a `push_secret_blocked` run is recorded `skipped_secret`
at enqueue and never copied, so the secret that blocked its push is never
given a second, unscanned publish target.

### Reliance on a single sweeper

`SweepSalvage` is one `sweeper.Pass`, run serially on the shared sweeper
tick, before the run-liveness sweep (`Service.Sweep`). There is no
detached goroutine, no per-run background worker, and no client-driven
promotion path: every state transition happens on a sweep tick, bounded by
`salvagePassBudgetDefault` (10s), including every broker call. A forge
outage therefore degrades to "this pass makes no progress on the item in
flight, and returns the rest for the next tick" rather than hanging the
sweeper or the run-liveness pass that follows it.

### The retry bounds

A pending row with no salvage ref recorded is retried up to
`salvageAttemptCap` (10) attempts; the attempt that would reach the cap
makes **no create call** — it only runs the orphan cleanup
(`deleteUnrecordedSalvage`, a CAS-delete of any salvage ref that landed on
the forge unrecorded) and settles `failed` only once that cleanup
succeeds, so the row's own create can never burn the exact budget its
capping cleanup needs. Past the cap, a row with no recorded ref backs off
to at most one retry per `salvageRetryBackoff` (1h), and gives up entirely
(settled `failed` with no further forge call) at `salvageHardCeiling`
(3× the cap, 30 attempts) — bounding a permanently dead remote (a revoked
PAT, a dropped allowlist entry, a deleted repo) to at most about 20 extra
hourly attempts before it stops holding the RESTRICT pointer or spending
forge calls. A failing expiry delete takes the same bound: past the cap it
backs off to one retry per hour, and at the ceiling it is settled
`expired` with no forge call, `last_error` naming the ref and tip left for
manual deletion. A row is promoted only below the cap, so an expiry always
gets at least one fast retry first. Both due lists are read up to 50 rows
(`salvagePendingScanLimit`, `salvageExpiryScanLimit`) and backed-off rows
are filtered out before the item budget. Each pass handles at most `salvageMaxItems` (5) broker items,
round-robin between due expiries and due pending rows, and which list
leads **alternates every pass** (`Service.salvageLeadPending`) so a
hanging item at the head of one list cannot starve the other list for more
than every other tick — the fix landed in review after the first cut of
M3 always led with expiry.

### The distinct value, the limits and the overlap with #1810

Once #1810 was on `main`, salvage's remaining distinct value is
**bounded, independently-expiring archival**, not custody or recovery in
its own right:

- **A salvage copy survives independently of custody state.** #1810 keeps
  the branch/recovery ref only while a custody hold is open; once the last
  hold settles, that ref is deleted. A promoted salvage copy has its own
  clock (`UZI_RECOVERY_READY_RETENTION` from its own creation) and needs no
  open hold to exist — useful once custody has already settled but an
  operator still wants the last checkpoint fetchable without a database
  export.
- **A salvage copy is a second, independent location.** It is a plain
  extra copy of a tip #1810 already keeps reachable while custody is open;
  it adds no new data #1810 did not already publish, only a second
  discoverable ref pointing at the same commit.
- **Salvage does not cover pre-#1810-migration runs specially.** A run
  that went terminal before migration 00266 (#1810's terminal-transition
  trigger) has no `checkpoint_retentions` row and so no branch/recovery ref
  guarantee from #1810 at all; if its branch ref happened to survive (the
  sweeper failure paths never deleted theirs before #1810), salvage can
  still archive it on a later sweep tick, same as any other eligible failed
  run. This is not a deliberate backfill feature, just the same enqueue
  query applied uniformly — and it is still bounded by the enqueue window:
  `ListSalvageCandidates` only reads runs finished within
  `max(UZI_RECOVERY_READY_RETENTION, salvageMinWindow)` (24h) of "now", so
  a pre-migration run that finished before that window is never enqueued
  at all, regardless of whether its branch ref survived.
- **The overlap is real and accepted, not eliminated.** For the common
  case — a failed run whose custody hold is still open — #1810 already
  keeps the branch or recovery ref reachable, and salvage's copy is
  redundant with it for as long as that hold stays open. The PRD's
  Decision Log records this explicitly (see the "Redesign after the
  #1810/#1819 heads-up" entry) rather than running two independent state
  machines side by side pretending the other does not exist: salvage's
  design is now **downstream** of #1810 (it only ever reads #1810's refs as
  a source, verifies against them, and never competes with them for a
  delete), so the redundancy is bounded (salvage's own retention window)
  and one-directional, not a second copy of #1810's own state machine.
- **No hold, no copy.** #1810 does not delete an unheld run's checkpoint
  ref synchronously "at" the terminal transition: the worker-reported
  terminal report, cancel and plan-reject paths dispatch
  `SettleRetainedCheckpoint` off the caller's own goroutine (a detached,
  best-effort background settle), and any record it could not settle there
  is picked up again by `ReconcileCheckpointRetentions`, #1810's own pass
  on the shared sweeper tick — which runs in `Service.Sweep`, AFTER
  `SweepSalvage` on that same tick. So on the tick right after the run goes
  terminal, salvage's pass still runs first; but the background settle
  dispatched at the terminal transition has almost always already deleted
  an unheld run's ref by the time that tick's `SweepSalvage` looks (it
  races only the DB write and a fast forge round-trip, not a full sweep
  interval), so salvage still **almost always** settles such a row
  `unavailable`. The exception is a terminal writer that dispatches no
  background settle and runs after the reconcile or outside the sweep
  (auto-stop, Codex account-wait failure): its
  `settling` record waits for the next tick's reconcile, which runs after
  that tick's `SweepSalvage`, so salvage usually copies such an unheld
  run's ref before #1810 deletes it. Otherwise salvage produces a copy only
  for held runs (and surviving pre-migration refs within the enqueue
  window above), and its
  own value is the window between the hold settling and the copy's expiry.
  A hold that outlives the retention leaves a copy that only duplicated
  exposure. The #1856-shaped loss is prevented by #1810's retention when
  the run held custody, not by salvage.

## Consequences

- **A failed run's last published checkpoint is fetchable from a stable,
  run-scoped ref, independent of custody settling, on any forge the
  maintainer has enabled.** `git fetch origin refs/uzi-salvage/<run-id>`
  works for as long as the row is `promoted` and unexpired.
- **Off by default, so merging this changes nothing for a stack with no
  existing rows.** `UZI_SALVAGE_FORGES` empty (the default) makes
  `enqueueSalvage` a no-op (it is gated on the setting directly), so a
  forge that was never listed behaves byte-for-byte as it did before this
  PRD. This is not unconditionally true across a rollback, though:
  `processSalvage` (the create/expire phase) runs regardless of the
  setting, so existing rows still progress. A `promoted` row's salvage ref
  still expires on schedule even after its forge is removed from the list,
  and a `pending` row with no recorded ref on a de-listed forge is
  CAS-deleted of any unrecorded copy and settled `disabled`. So "nothing
  changes" holds only where no `run_salvage` rows exist yet, not as a
  general invariant of the empty setting.
- **No new delete authority over #1810's refs.** Every `Delete` call
  salvage makes is scoped to its own namespace: `deleteSalvageRef` and
  `deleteUnrecordedSalvage` both pass `pushbroker.SalvageRef(row.RunID)`
  with a non-empty `ExpectedOldTip`, pinned by the AST test
  `TestSalvageSourceNeverDeletesCheckpointRefs`. `pushbroker.Delete` itself
  is not scoped to salvage — it also serves #1810's checkpoint and recovery
  refs — so the guarantee lives in salvage's own call sites, not in the
  broker refusing salvage a wider target.
- **A bounded, visible failure mode.** A permanently unreachable forge
  costs at most `salvageHardCeiling` attempts (with the 1h backoff past
  the cap) before the row gives up and releases the RESTRICT pointer,
  rather than blocking run/repo/connection deletion indefinitely. This
  holds for a pending row's create or cleanup and for a promoted row's
  expiry delete alike.
- **Redundant coverage with #1810 for the common case, by design.** A
  salvage copy of a still-custody-held run's checkpoint duplicates a ref
  #1810 already keeps reachable. This is accepted, not a bug: the
  redundancy is small (one extra ref, one extra retention clock) and buys
  independence from custody settling and from pre-#1810-migration
  coverage gaps.
- **`run_salvage` never cascades from `runs`, repos or owners**, matching
  ADR-1296's rule; a row can outlive the run it names once its ref is
  gone, but never disappears while a live salvage ref might still exist.

## References

- [PRD #1867](../prds/1867-failed-run-salvage-ref.md)
- [ADR-0122](0122-checkpoint-push-broker.md) — the push broker's CAS,
  never-forced invariant, reused unchanged.
- [ADR-1810](1810-checkpoint-retention-follows-custody.md) — owns the
  branch checkpoint ref and the recovery ref end to end; this ADR's
  `CreateRef` reuse and never-delete rule are downstream of it.
- [ADR-1597](1597-midturn-checkpoint-durability.md) — documents the
  unscanned-publish exposure this ADR's retention window inherits.
