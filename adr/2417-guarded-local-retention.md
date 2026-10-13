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

Issue #2854 qualifies this order while an exact terminal/finalize report is pending,
or outbox initialization failed: canonical local bundles may be unlinked after the
covering ACK and execution/quarantine checks, before step 1. ACK journals and pins
remain until report retirement (or a healthy store proves no pending report).
The retained ACK preserves the acknowledged-inventory report clause below.
Redundancy claims wait with the report; server `local_replica_worker_id` retention
and the absence of acknowledged-bundle readers make this byte-only deletion safe.
See [ADR-2652](2652-report-only-retirement.md#consequences-and-limits).

Outside that pending-report exception, cleanup proceeds in this order:

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
server archive remains available under its existing retention policy (qualified by
[Amendment 2026-10-11](#amendment-2026-10-11--published-redundancy-early-expiry-2625): a proven-redundant archive may expire early).

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
configured ready-retention window and clears `local_replica_worker_id` (a
published-redundancy early expiry, #2625, also clears it). Local
ACK cleanup leaves that marker and expiry unchanged, even after its local
bundles and journals are gone. No server archive retention policy is changed by this decision; #2625 later
adds one narrow early-expiry path ([Amendment 2026-10-11](#amendment-2026-10-11--published-redundancy-early-expiry-2625)).

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
#2545 and #2506 remain outside this change. (2026-10-12: #2545 is now
implemented by [ADR-2545](2545-failed-run-held-publication.md); see the
amendment below. #2544 and #2506 remain excluded.)

## Amendment 2026-10-11 — published-redundancy early expiry (#2625)

**Status**: Implemented in API and worker, per maintainer decision #2625.
This records implemented behavior, not live or deployment acceptance. It
qualifies the statements above that the selected server archive "remains
available under its existing retention policy" and that "No server archive
retention policy is changed": a completed run's guarded archive can now expire
early, but only on a complete server-side proof. Everything without one keeps
its existing retention.

### The proof

`POST /api/worker/runs/{id}/archives/{captureID}/redundancy`
(`WorkerRecoveryArchiveRedundancy`; prover `archive_redundancy.go` in
`api/internal/workersvc`; protocol token `recovery_archive_redundancy_v1`)
takes a worker claim but trusts nothing it says about publication. The API
proves through the forge, against the live head of the run's published branch
(the anchor from `proveCompletedPublication`, the #2507/#2598 proof), that
every root, the captured head and every server-recorded prerequisite (the
thin-bundle closure) is an ancestor of that head. For parked WIP, the captured
head is replaced by a witness commit with the same tree plus every parent of
the captured head.

The claim is bound to the exact hold, generation, `coverage_digest` and
`source_sha`, and requires a released, completed, identity-stamped hold (a
completion identity that carries the MR). On success the API expires the
`available` capture with reason `published_redundant`, records a
`redundancy_proof` and deletes its payload chunks. A refusal keeps the archive
on its existing retention with one bounded reason; a forge-evidence refusal is
memoized for 1 hour per capture and digest.

The ADR-2417 capture guard (migration `00322_recovery_archive_redundancy.sql`)
skips its protected-final raise, the local-replica or unexpired-window
protection, only for a proof matching the capture, its released hold and the
completed run. Without a proof the original protection is unchanged.

### Worker claims

Both guarded cleanup paths submit claims: receipt cleanup
(`forgetCompletedGeneration`) and covering-FINAL-ACK cleanup
(`forgetGenerationWithinCycle`). The worker builds the claims under the
journal lock, from the records read for cleanup and before any destructive
step, and submits them after the lock is released, sequentially under one
30-second deadline. Results are logged and never gate cleanup. Only uploaded,
guarded, non-empty, non-settled records claim.

The WIP witness exists on the receipt path only and is exactly the receipt's
delivered `final_head`. When `final_head` already contains the captured head, the claim is sent with
no witness. Only when it does not (divergent) is the witness attached, and
only if its tree equals the archive tree and every parent of the captured head
is its ancestor; if those checks fail or ancestry is unknown, no claim is sent
(fail-closed, with no broader search). The ACK path sends no witness.
Cleanup usually removes the local source in the same pass, so each archive
effectively gets one attempt; a missed or failed claim leaves the normal TTL.

### Durability policy

Publication is proved against the live branch head at proof time. **Accepted
risk**: after early expiry, later deletion or force-rewrite of the published
branch (or the MR source branch) is not protected by uzi. Forge-side MR refs
may still hold the commits, but that is not guaranteed. This is the same
post-publication risk #2598 accepts for custody release. A missing branch, a
head mismatch or any inconclusive answer keeps the TTL, as does anything
without complete proof.

### Quota and storage

Early expiry frees payload bytes, so the per-owner ready payload and instance
byte quotas recover. The capture stays `expired`, and since #2744 the per-owner
count (`MaxCapturesPerOwner`, `UZI_RECOVERY_MAX_CAPTURES_PER_OWNER`) counts every
state except `discarded` and `expired`, so an early-expired capture also frees its
count slot. The per-claim ceiling still counts it.
PostgreSQL can reuse the freed pages; no immediate reduction in database file
size is promised.

### Boundaries

Explicit discard semantics are unchanged, as is failed, cancelled and parked
run handling. Only the completed, identity-stamped, released-hold case above
can expire early.

## Amendment 2026-10-12 — held-publication FINAL as covering acknowledgment (#2545)

**Status**: Implemented in worker and API. See
[ADR-2545](2545-failed-run-held-publication.md) for the publication design.

A failed run's journaled `held_publication` FINAL, once ACKed, is a covering
final acknowledgment for the local retirement rules above. Its retained copy is
the forge ref `refs/uzi-held/<run-id>/<generation>`, which the API listed at the
declared tip before release. It therefore does not need the self-contained,
prerequisite-free bundle that an archive FINAL needs. Every other covering check
still applies: the inventory is inactive, every unresolved root and retained
source head is an ancestor of the FINAL's source SHA, clone heads are verified,
and quarantine blocks physical destruction. The API releases only when the
request exactly matches the created publication row (publication id, source SHA,
coverage digest), and on replay the hold's stored receipt. The worker sends the
request verbatim from its journal and checks the ACK's run, generation and
release flags before the
record is marked acknowledged and the generation is retired. A retained or
cleared held FINAL grants no destruction authority, and the worker keeps the
clone, pins and open hold. A transient retention keeps the journaled request
for replay. A definitive one clears it, and the worker captures an archive on
the same record, which then follows the archive rules above. #2544 and #2506
remain excluded.
