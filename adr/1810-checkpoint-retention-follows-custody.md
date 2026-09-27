# ADR-1810: A published checkpoint outlives its custody hold; supersession moves it, never deletes it

**Status**: Accepted (PRD #1810 M1-M4 implemented; M5 in progress)
**Date**: 2026-09-27
**Deciders**: agent team, per PRD #1810
**PRD**: [PRD #1810](../prds/1810-retain-failed-run-checkpoint-ref.md)

## Decision (summary)

> A published checkpoint outlives its custody hold, and supersession moves
> it, never deletes it.

The api no longer deletes a terminal run's `refs/uzi-checkpoints/<branch>`
ref best-effort at each of the three writers that reach it (PRD #1030 M4):
the worker-reported terminal state report, and the two server-side
cancel/reject paths. Instead, at each of those writers it retains the ref
while any custody hold of that run is open, and settles it
(compare-and-swap delete on the recorded tip) only once the run's last
hold is released or discarded. When a new run on the same branch is
blocked because a retained ref still occupies the slot (a
`not_descendant` publish refusal), the api **supersedes** the old run's
record: it moves the tip to `refs/uzi-recovery/<run-id>` and frees the
branch ref, rather than deleting the tip or force-updating anything.

Every forge write this invariant requires is CAS, never forced, consistent
with the push broker's existing rule (ADR-0122).

## Context

PRD #1030 M4 deletes a run's checkpoint ref, best-effort, from three writers
in `api/internal/workersvc` — the worker-reported terminal state report and
the two server-side cancel/reject paths — so a stale ref never blocks a
later run's publish with `not_descendant`. A sweeper-driven terminal
transition, such as failing a stale worker over its cap, never called that
delete. That is correct for a completed run with no open hold. It is
wrong for a failed or cancelled run: the same code path deliberately
keeps custody holds open for those runs "for capture or explicit
discard", while deleting the one off-worker copy of the work the hold
exists to protect. On 2026-09-27, issue #1798's run published two
checkpoints, failed (a full data volume, PRD #1809), and both checkpoint
publishes were then deleted while the run's custody holds were still open
with no captures — the committed work survived only in the worker's own
working clone.

A completed run with an open custody hold from an *older* generation of the
same run is a related case: freeing the branch ref for the new generation's
publish must not destroy the older generation's only off-worker copy either.

## The mechanism

### Retention (M1)

At each of the three terminal writers that used to delete the ref (and
later, through the sweeper backfill, for every other terminal writer) the
api inserts a `checkpoint_retentions` row
(one per run, migration 00259) recording the run's checkpoint branch, the
published tip, and the ref that currently carries it. A run with an open
custody hold gets state `retained` and no forge call. A run with no open
hold (a completed run whose hold already settled, or a failed/cancelled run
on a worker without the recovery capability) gets `settling`, and a
background settle deletes the ref, CAS on the recorded tip, under the run's
retention lock.

### Per-run session advisory lock, not a row lock (amends the PRD's D2)

The PRD's D2 originally described supersession and settlement as
serializing on the old run's row lock. That is insufficient: the operation
spans several forge round-trips (list, create, delete), and a Postgres row
lock is released the instant its transaction commits — it cannot hold
across an intent commit *and* the forge writes that follow it, which is
exactly the ordering the invariant needs (the intent must be durable
**before** any forge write, so a crash mid-operation leaves the tip
discoverable from the persisted state alone).

Instead every retention forge write — settle's delete, supersession's
create and branch-delete, a stuck supersession's exit, and the
post-settlement audit's delete — runs under one **per-run PostgreSQL
session advisory lock**, held for the whole operation including every forge
call, taken with try semantics (a busy lock is a no-op: the caller leaves
the ref alone and a later trigger or the sweeper retries). Because the lock
is session-scoped, it is pinned to one dedicated pool connection for the
operation's duration.

A session advisory lock alone is not sufficient either: a lock can be lost
mid-operation if its backend is terminated or its connection drops, silently
leaving the caller without exclusivity it believes it still holds. So every
forge write is preceded by a **fence**: a re-check, on the same pinned
session, that `pg_locks` still shows the lock granted to this backend. A
lock lost between acquisition and the write is caught by the fence and the
write is skipped, rather than racing another writer that has since taken
over the record.

Even the lock-plus-fence pair leaves one narrow residual window (below), so
the sweeper also runs a **post-settlement audit** arm: 10 minutes after a
recovery ref's deletion is recorded, it re-lists that ref on origin and
removes it if it is still there at the recorded tip and no hold has
reopened — closing the gap where a create landed on origin after the fence
passed but the session was lost before the delete recorded.

### Supersession (M3)

Triggered only by a *live* run's own publish being refused `not_descendant`
against a retained ref from a different run — a terminal run's worker has no
path to trigger it. In practice this means an issue run whose branch (keyed
on the issue) a later run on the same issue reuses; a self-improve run's
checkpoint branch is keyed on its own run id (`uzi/self-improve/<run-id>`),
so no two self-improve runs ever share a branch and supersession never
triggers for that kind. Under the old run's retention lock:

1. **persist the intent first**: `retained` → `superseding`, recording the
   recovery ref name `refs/uzi-recovery/<old-run-id>` and the expected tip,
   before any forge write;
2. create the recovery ref at that tip, CAS with old = zero;
3. CAS-delete the branch ref at that tip;
4. mark the record `superseded` (a hold is still open) or drive straight to
   `settling` (none is), in one statement.

Every step is idempotent, so a crash at any point leaves the tip
discoverable under at least one recorded ref, and the sweeper re-drives an
interrupted `superseding` record from step 2.

### Empty pack, not "pack-less" (amends the PRD's D2)

The PRD's D2 describes the recovery-ref create as "pack-less", since the
object is already on the remote and nothing new needs to be sent. In
practice `git receive-pack` requires a pack for any non-delete command, even
an empty one (a zero-object pack is the smallest legal payload), so the
push broker's new CAS ref-create primitive (M2) sends an empty pack rather
than omitting one.

### Settlement and the sweeper (M4)

The recovery ref (or the branch ref, if supersession never triggered) is
deleted, CAS on its recorded tip, once the run's last custody hold is
released or discarded. Every release and discard writer in
`api/internal/store/queries/recovery.sql` triggers the settle check after
its transaction commits. Because that trigger can be lost (a busy lock, a
full concurrency slot, a crash) or never fire at all (several terminal
writers besides the original three call sites never touched retention —
see the PRD's amended Problem statement), the sweeper's
`ReconcileCheckpointRetentions` pass is the backstop: it retries failed
deletes with exponential backoff, drives due `superseding` records, settles
records whose holds have since cleared, backfills a record for any terminal
run that published a checkpoint but has none (a watermarked scan bounded by
migration 00260's index), and runs the post-settlement audit described
above.

## Consequences

- **A checkpoint ref can now persist well past a run's terminal
  transition**, for as long as its custody holds stay open. This is the
  point: it is the fix for the data-loss mode PRD #1810 exists to close.
- **The api's forge-write surface for checkpoints grows**: a create-ref
  primitive alongside the existing publish/delete (ADR-0122), still CAS,
  still never forced.
- **A per-run advisory lock plus a fence plus a scheduled audit is more
  machinery than a row lock**, but it is what the "intent commits before
  any forge write, and forge writes never race a lost lock" invariant
  actually requires; see the PRD's D2 amendment above for why the simpler
  design does not work.
- **Three known residuals remain, by design, rather than closed with a
  primitive the push broker does not have:**
  1. If the old run's own worker publishes again in the brief window
     between the recovery ref's create and the branch ref's delete, the
     supersession stays open (the branch ref still carries the old run's
     later tip, which the record does not name) and the new run's
     checkpoints are skipped until the old run's hold is released or
     discarded. Moving the ref instead would need a CAS-*update* primitive
     the push broker does not have.
  2. If that late publish becomes visible on origin before its tip is
     persisted server-side, the branch ref can be left untracked by any
     record; this is logged as a warning and a human must delete it.
  3. In the tip-lag case — the record's recorded tip lags what the run
     itself later published, and that later tip is not the run's own
     persisted `checkpoint_tip` either — a stuck supersession's exit leaves
     the branch ref for a human, for the same reason: nothing in the
     record can prove ownership of a tip it never recorded.
- **The live-settle ancestry proof (PRD #1349/#1751,
  `ReleasePredecessorCustodyHoldByLiveAncestry`) is unchanged.** It only
  runs against a *live* run's checkpoint or branch head, and a retention
  record is only ever created for a *terminal* run, so the two do not in
  practice meet — the PRD's D3 test expecting the live-settle reader to
  accept a recovery ref was dropped as dormant rather than implemented.

## References

- [PRD #1810](../prds/1810-retain-failed-run-checkpoint-ref.md)
- [ADR-0122](0122-checkpoint-push-broker.md) — the push broker's CAS,
  never-forced invariant this ADR extends to a second ref namespace.
