# ADR-2417: guarded local recovery retirement requires covering final acknowledgment

**Status**: Implemented
**Date**: 2026-10-07
**Issue**: [vtmocanu/uzi#2417](https://github.com/vtmocanu/uzi/issues/2417)

## Context

A guarded generation can close custody with a durable API disposition while
its local recovery pins, bundles and journals remain on the worker. Those
bytes need a bounded cleanup path that preserves the proof needed after a
partial failure. Separately, retiring a terminal or finalize report must not
mistake a terminal run, a missing journal or a superseded claim for permission
to destroy recovery inventory.

The claim-scoped custody and immutable archive model remains as described in
[ADR-1296](1296-durable-run-recovery.md). This decision separates local inventory
cleanup authority from report retirement authority. The original archive path
adds no persisted local receipt or tombstone; the 2026-10-09 amendment below
adds completed-publication receipt authority inside existing recovery records.

## Decision

### Covering FINAL ACK authorizes exact-generation inventory cleanup

`RecoveryCoordinator.forgetGeneration` in `agent/src/recovery.ts` serializes
cleanup through the generation capture cycle and run journal lock. For a
guarded generation, its complete authenticated physical journal must establish
a covering FINAL ACK: the final request and coverage digest are authenticated,
and the acknowledged source covers the relevant original roots and source
heads by locally verified ancestry. An earlier generation with its own covering
ACK is already settled for this coverage check; a later generation is outside
this cleanup's deletion scope.

Unknown, unreadable, malformed, foreign or uncovered inventory retains local
custody. A pending final request or invalid source attribution cannot be bypassed
by an ACK. Bundle paths must be canonical files belonging to the selected
generation; the resolved bare repository must match the authenticated context.
Execution or residue quarantine blocks destruction, with those guards rechecked
at destructive steps.

`GitCache.cleanupRecoveryGeneration` in `agent/src/git.ts` checks physical refs
and their attribution under the bare lock. Recovery and coverage pins must be
known and covered; exact-generation owed candidates must match the authenticated
roots. A shared owed pin survives when another context still consumes it.
Sibling generations remain intact. Lightweight context metadata remains when
referenced by sibling candidates, tracking refs, owner stamps or publication
receipts/markers; an indeterminate consumer retains that context.

Cleanup proceeds in this order:

1. Delete the selected generation's recovery and coverage refs and unshared
   owed refs using their expected SHAs, then read back to verify deletion.
   Remove the eligible candidate and unconsumed context metadata.
2. Remove that generation's local bundles.
3. Remove non-ACK source journals, then any other ACK journals.
4. Remove the covering ACK journal last.
5. Attempt nonrecursive removal of the empty run directory, best-effort.

Failure before step 4 retains the covering ACK for a later bounded boot or live
pass. Successful earlier deletions are idempotent, including already-absent
files. The ACK is retry authority, not a new durable local receipt. After its
deletion, the API's durable disposition remains authoritative and the selected
server archive remains available under its existing retention policy.

Archive coverage is not remote publication proof. Local recovery cleanup does
not reconcile a head as forge-published or erase referenced publication identity.
Rejected credentials stop network recovery work in the retry pass but do not
prevent bounded local cleanup using a persisted authenticated ACK, subject to
execution and quarantine guards. Boot discovery does not rematerialize journals
for closed or discarded holds.

### Terminal and finalize report retirement has separate authority

`Runner.recoveryInventoryPending` in `agent/src/runner.ts` protects report
retirement. A covering authenticated ACK or a persisted exact-generation
completed-publication receipt clears the exact custody check. Worker-driven finalize
retirement additionally requires either acceptance of the exact original record offered during
registration, temporary authority for captured lower originals during accepted registration cleanup,
or fresh runtime-validated terminal ownership (`completed`, `failed` or `cancelled`,
nonnegative safe-integer generation at least the record's, boolean `inventory_guarded` when present).
Original offered identities and existing lower records are frozen before the request; lower
generations gain no saved handoff and same-key replacements gain no authority. Registration
attempts the frozen set in waves of at most 16 within the five-second pass deadline. At most 256 accepted identities
remain for the process lifetime, even while terminal journals hide them. Boot and live terminal
resolution apply the same predicate. Ownership and custody share the existing 16 unresolved-key
quarantine, one-second candidate and five-second pass bounds; late results cannot delete.
Deletion rechecks the original identity, cancellation, liveness and pending terminals under the
run lock. The runner's normal durable-outcome retirement remains unchanged. Otherwise
`WorkerClient.hasRecoveryRetirementAuthority` in `agent/src/client.ts` requires
fresh evidence; it authorizes retiring the report, not deleting recovery sources.

For the exact custody path, the registered worker ID must be validated. The
fresh ownership response must belong to that worker, have status `completed`,
`failed` or `cancelled`, and carry a positive safe-integer `claim_generation`
greater than or equal to the requested generation. The custody response must
match the run, worker and requested generation, be `settled`, complete for both
exact and sibling sets, and contain a nonempty exact hold set with matching
counts. No open sibling hold on that worker may remain. The negotiated
`terminal_rejection_report` feature must still be available and registration
must still identify the same worker after the read.

- With pending authenticated guarded inventory, every exact hold must be
  `discarded`. Mixed `released`/`discarded` holds do not suffice.
- With absent recovery journals, every exact hold may be `released` or
  `discarded`, provided the exact authority checks above pass.

Ownership 404, ownership transfer, unsupported protocol, unavailable reads or
invalid/incomplete evidence retains the report. An absent journal alone supplies
no authority, including in a fresh client process. Positive legacy classification
is either an authenticated legacy journal (subject to known guarded membership
and negotiated hold checks), or runtime-validated terminal ownership for exactly
the requested generation with `inventory_guarded === false`. Known guarded
membership blocks downgrade to legacy, including a guarded claim previously
observed by the client and subsequently settled.

A `stale_claim` response supersedes the report only. It neither settles custody
nor deletes inventory. Normal protected report retirement checks recovery
authority separately. Exact owner discard can authorize report retirement alone; it is not
permission to delete physical clones, recovery sources or guarded journals.
Physical source retirement continues to require the existing quiescence and
retention rules. Guarded local recovery journal deletion requires a covering
FINAL ACK or the distinct persisted completed-publication authority below.

### Server archive retention does not follow local ACK cleanup

Migration `00305_recovery_inventory.sql` protects a selected final capture from
expiry while `OLD.local_replica_worker_id` is nonnull, or its existing expiry is
still in the future. The trigger raises before an expiry update can remove the
selected archive, rolling back the statement's chunk deletion too.

Physical worker deletion is the operation that renews the selected capture's
configured ready-retention window and clears `local_replica_worker_id`. Local
ACK cleanup leaves that marker and expiry unchanged, even after its local
bundles and journals are gone. No server archive retention policy is changed.

## Consequences and boundaries

Archive-backed local storage can be reclaimed without keeping a second
disposition record on disk, while partial cleanup retains authenticated retry
authority. Completed-publication cleanup uses a receipt in an existing record. Conservative
attribution and ownership checks can leave bytes or reports retained when a
reader might otherwise infer that a closed hold or terminal status is enough.

The [bad-MAC terminal-record policy](../docs/run-recovery.md#when-a-terminal-record-fails-authentication-after-restart)
remains distinct and unchanged. Clone-parent discovery (#2433) and server archive
retention are outside this change. This decision adds no upload or release
exception during quarantine and does not close the existing quiescence gaps.

The decisions in [specs/human.md](../specs/human.md) remain in force: exact owner
discard warns about a possible only copy; a published checkpoint ref is
removed after the last hold releases or is discarded; physical clone cleanup
requires the existing confirmed-quiescence boundary, including its disclosed
gaps; quarantine retains evidence. The existing completed custody release
exception includes the guarded triad below, without narrowing legacy release.

## Amendment 2026-10-09 — completed-publication receipt authority

**Status**: Implemented in API and worker, per maintainer decision #2507.
This records implemented behavior, not live or deployment acceptance.
[ADR-1296](1296-durable-run-recovery.md#amendment-2026-10-09--2507-synchronous-completed-publication-release)
defines the negotiated proof and exact completing-generation release.

### Persisted completion receipt is distinct from archive coverage

`RecoveryCoordinator.persistCompletionReceipt` writes the validated receipt
only into an existing exact-generation guarded recovery record for
`issue`, `mr_rework` or `self_improve`, then authenticates its readback.
It neither creates a post-release journal nor introduces a standalone receipt
or tombstone. A receipt authorizes completion cleanup separately from
`finalAcknowledged` / `finalRequest` / `coverageDigest`; it does not claim
archive ancestry or expand an archive's coverage.

A successful receipt skips terminal inventory preparation even when an
available, thin or needs-action capture already exists. Protected terminal and
finalize retirement must preserve the fixed original completion until the
receipt is persisted and read back. Failure to persist retains source and
terminal evidence; server release alone does not substitute for the local
record. Without a valid receipt, the original capture fallback remains.
A lost ACK after release followed by capture refusal grants no destruction
authority and cannot turn completion into failure or reopen it. Original
completion replay recovers the exact stored receipt. Attempted-send guards
also protect no-outbox and reserve-failure paths from false failure, without
inventing durable storage on those paths.

### Destruction requires a separate physical boundary

`RecoveryCoordinator.forgetCompletedGeneration` reads the complete
authenticated physical journal under the generation cycle and journal lock.
It selects the exact receipt and bound completion source, validates consistent
branch, kind, bare identity, source attribution and canonical bundle paths,
and preflights refs before physical source retirement. The runner's
`withCompletionSourceBoundary` reserves execution and checks quiescence and
credential isolation. Execution/adoption exclusion and the bare lock remain
held through destruction, with guards rechecked at destructive steps.

`GitCache.retireRunnerClone` validates the exact canonical owned clone or
recorded attempt, expected source SHA and attribution. Eligible dirty or
untracked leftovers of that completing source may be discarded without
requiring WIP capture or a clean worktree. This exception does not dispose of
sibling generations, shared owed pins, metadata with consumers, foreign or
unknown inventory, or quarantined residue. Unknown attribution and changed
ownership retain evidence.

Completion retirement finishes source metadata only after actual physical
disposal. Metadata completion has one bounded EIO retry. An existing durable
`retired` attempt-ledger entry can prove disposal and authorize clearing only
the matching old recovery journal when the source is absent and no successor
owns it. Permanently lost retirement proof retains remaining artifacts;
absence alone is not authority and no new crash-discovery mechanism is added.

Eligible recovery/coverage and unshared owed refs are deleted using expected
SHAs under the bare lock. Bundles and other exact-generation journals follow;
the receipt-bearing recovery record is removed last, preserving retry authority
on partial failure. Empty-directory removal remains nonrecursive and
best-effort. Server archives retain their existing policy.

### Boundaries

This authority applies only to guarded COMPLETED `issue`, `mr_rework` and
`self_improve` completing generations with the negotiated receipt. Older or
successor holds gain no authority, and unstamped older completions are not
backfilled. Failed/cancelled/parked runs and other kinds keep their prior rules.
Quarantine can permit the existing custody release exception but blocks
physical destruction. Bad-MAC terminal cleanup remains its separate policy.
No intent, endpoint, discovery, driver or E2E mechanism is added; #2544,
#2545 and #2506 remain outside this change.
