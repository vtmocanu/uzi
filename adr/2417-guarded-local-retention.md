# ADR-2417: guarded local recovery retirement requires covering final acknowledgment

**Status**: Implemented
**Date**: 2026-10-07
**Issue**: [vtmocanu/uzi#2417](https://github.com/vtmocanu/uzi/issues/2417)

## Amendment 2026-11-04: operator resolution of guarded local records (#3885)

**Status**: Accepted and implemented for Case A. Case B was accepted and
implemented afterwards, see the
[2026-11-10 Case B amendment](#amendment-2026-11-10-case-b-local-record-disposition-3885).

A worker-local operator command now retires a guarded local recovery
generation whose closed hold is backed by this record's own receipt or
acknowledgement. It adds no replacement request, no second release decision, no
new upload, no new endpoint, schema, privilege or authority. The autonomous
FINAL ACK and completion-receipt paths are unchanged. The surface is two new ops
on the retained-source channel (`local-records`, `resolve-local-record`) in
`agent/src/ops/retained-source-server.ts`, served by
`RecoveryCoordinator.describeLocalRecords` and `resolveLocalRecord` in
`agent/src/recovery.ts`. Operator steps are in
[Recovery docs](../docs/run-recovery.md).

**Case A (this amendment as first accepted).**

- **A1**: the FINAL ACK is already persisted and the exact closed hold was
  released with a receipt matching the saved FINAL request (an ACK with a
  discarded hold is refused). The existing
  `forgetGenerationWithinCycle` cleanup runs on operator request.
- **A2**: the saved immutable FINAL's exact closed hold was released with a
  receipt matching that request, but the local ACK never landed. The existing
  finalizer replays the identical request once. The ACK it persists makes the
  generation A1, and the same cleanup then runs.

**Binding.** Describe prints, and resolve must be given, a digest over the run,
generation, case, hold and repo ids, `serverCaptureId`, the exact closed-hold
observation (state, release evidence, receipt, run status), the bare
repository's `dev:ino` and refs, the record-directory names, the settlement
names and a hash of each record's content. Resolve recomputes it under the
journal lock and refuses `descriptor_changed` on any drift. The closed-hold
observation is a fresh read through the existing original-worker-scoped
exact-hold API. Missing evidence is a named refusal (`exact_evidence_unavailable`,
`holds_unknown`, `evidence_deadline`) that grants nothing.

**Order.**

- A1: channel claim gate, authority A, capture cycle C, journal J, bare B.
- A2: gate, A, C, a J pre-check (released), the inventory source boundary
  (execution tail, reservation, source-identity and process-quiescence proofs),
  the finalizer, the release POST, the ACK, then forget (J, B).

Client cancellation is honoured only before the first mutation (the A1 cleanup,
or the A2 POST). Afterwards only the deadline and worker shutdown can interrupt,
and they end the request `partial`.

**Retention exclusions.** An unbound `*.tmp` file or orphan bundle in the run's
record directory refuses as `unbound_artifact:<name>` and is retained.
Terminal protection and any settlement-root entry refuse. Cleanup deletes only
artifacts bound to the retired authenticated generation; pins of other
generations or runs are untouched.

**Failure model.** Abrupt process kill only. This is verified by real `SIGKILL`
of a child worker process at six points (before the POST, after the POST before
the ACK write, after the ACK temp write before its rename, after the ACK rename
before cleanup, after the first shared-bare deletion was registered (before it ran), and after the bundle
unlinks before the record deletes), each followed by a fresh process that
rediscovers the state (`agent/test/recovery-local-record-restart.test.ts`).
Behaviour under storage or power loss is unspecified: record writes are
rename-atomic, not fsynced.

**Cooperative deadline contract.** The 60 s request deadline is checked only
between operations. Owned filesystem I/O and child processes are awaited, so
locks and the closed claim gate can outlast the deadline and the client's
120 s idle timeout. The client then reports the outcome unknown and exits
non-zero. The server still logs and records the final result after the client
has left (`inspect` lists it in `lastDiscards` with attempt `local-record`, except
admission refusals `shutting_down`, `busy` and `no_claim_gate`, which return
before the remember step; the list is in-process and bounded).
The claim gate reopens only once the owned work has settled. The operator
re-inspects before retrying; the digest changes after any mutation. The 5 s
channel close/shutdown wait neither establishes settlement nor releases
ownership.

## Amendment (#2969): predecessor record cleanup deferral

Predecessor settlement (see the [ADR-1296 amendment](1296-durable-run-recovery.md#amendment-2969-predecessor-guarded-hold-settlement-is-built-and-disabled-in-production)) adds no cleanup authority. If a predecessor FINAL is ever ACKed while a later generation executes, the older generation's local records are not retired in that pass: they are retired by the ordinary `resumeInventory` / `forgetGenerationWithinCycle` path after the flight, and while the run executes the record is protected as `source_protected`. The covering-ACK rule above and the C1b/C2 pin boundaries ([ADR-3591](3591-prerequisite-closure.md)) are unchanged, and a pin still authorizes no local retirement. Custody accounting ([ADR-2445](2445-custody-admission-accounting.md)) and merged-predecessor discard ([ADR-2506](2506-merged-predecessor-discard.md)) are untouched. The pass is disabled in production, so no predecessor record is currently affected.

## Amendment 2026-11-10: Case B local-record disposition (#3885)

**Status**: Accepted and implemented in the worker. This records implemented
behavior, not live or deployment acceptance. Decisions on
[#3885](https://github.com/vtmocanu/uzi/issues/3885):
[2026-11-05](https://github.com/vtmocanu/uzi/issues/3885#issuecomment-6665697763),
[2026-11-06](https://github.com/vtmocanu/uzi/issues/3885#issuecomment-6692111919),
and the
[2026-11-10 reconfirmation](https://github.com/vtmocanu/uzi/issues/3885#issuecomment-6762744472).
This replaces the earlier "proposed, not accepted" section. Operator steps are
in [Recovery docs](../docs/run-recovery.md#resolving-guarded-local-records).

Case B is a generation where no record has `finalAcknowledged` and the closed
hold is no preservation proof for the record: the hold was discarded, or it was
released without a receipt matching the saved FINAL (or no FINAL was saved).
The operator command can now retire that generation's **local records and
bundles only**, on an explicit per-request confirmation. It adds no endpoint,
schema, privilege or server authority, never synthesizes `finalAcknowledged`,
and never converts abandonment into archive evidence.

### Classification and eligibility

`local-records` classifies a Case B generation as `case_b_owner_discard` (hold
discarded, `release_evidence` `owner_discard`) or `case_b_released_mismatch`
(released without a receipt matching the saved FINAL, or with no saved FINAL).
Only a generation in which no record has `finalAcknowledged` qualifies, in one
of three shapes: (a) one saved FINAL with a persisted non-matching settlement;
(b) one saved FINAL whose fresh exact read is discarded or released-mismatched;
(c) no saved FINAL and exactly one guarded frozen aggregate. A replay-refused
anchor (`final_retry_refused`, `credential_rejected`) qualifies only with a
persisted non-matching settlement; otherwise `replay_refused_persisted` stays.
The Case A names are unchanged except that `case_b_pending_owner_review` no
longer exists: an acknowledged generation whose exact hold is discarded or
mismatched is now `ack_evidence_inconsistent`.

### Confirmation contract

The digest binds a distinct Case B domain (`retained-local-record-case-b-v1`),
the run, generation, class, hold and repo ids, the anchor and server capture
ids, the immutable fields of the fresh exact observation (state, release
evidence, receipt, completed-publication receipt, guarded flag, run status,
original worker identity), the bare repository's
`dev:ino` and refs, the record-directory names, the settlement names, a hash of
**every** authenticated record of the run (not only this generation's), the
source-evidence descriptor and the `laterCleanup` list. Resolve recomputes it
under the journal lock and refuses `descriptor_changed` on any drift.

Each request must carry `--case-b` (`action: "case_b"`). A Case B digest
without it is refused `case_b_disposition_required`; the flag on a Case A
generation is refused `case_b_action_mismatch`; any other action value is
`invalid_request`. A worker without this change answers `invalid_request`.

### Owner binding (amends "exact owner")

The earlier proposal required a disposition "bound to the exact owner". The
local socket proves pod access, not owner identity, and the worker-side
`RecoveryExactHold` has no owner field. The binding is therefore: the run id,
plus the server's owner- and run-authorized, original-worker-scoped exact read
of the hold (the existing `getExactRecoveryHold` with the persisted
`exactHoldBinding`). The deployment operator confirms each request on the
owner's behalf. A server `owner_discard` event alone, or a stale digest, never
authorizes anything. For an owner-discarded source the owner has accepted
abandonment of dirty or unpublished local work; for a released mismatch no bytes
are treated as abandoned. Case B removes no tree bytes either way.

### Refusals

Case B adds these refusals on top of the shared ones.

- `run_hold_open`: any open hold of the run at any generation, including a
  run-wide `run_open_holds` count above zero. Case A keeps its own-generation
  `hold_open` check. `holds_unknown` also covers an absent or malformed
  run-wide count: absent is never zero.
- `settlement_evidence_inconsistent`: a persisted settlement disagrees with the
  fresh read on the immutable fields.
- `policy_discard_retained`: a `merged_predecessor_discard` outcome, even
  confirmed. `case_b_discard_evidence_unsupported`: any other discard evidence.
- Source evidence (next section), and
  `inventory_source_not_quiescent:<check>` from the source boundary.

### Source-evidence gate

Case B needs two reads: the clone inventory with per-clone identity, and the
worker-wide retained-source inspection filtered to this run and bare.
Authority is conservative: the inventory must be verified; the inspection must
report `partial === false` and `unreadable === 0`. `partialCauses` is
diagnostic only and is never consulted for eligibility or the descriptor. A
malformed shape is `source_evidence_incomplete`; no evidence reader is
`source_evidence_unavailable`; no clone and no retained source for the run is
`source_evidence_empty`; a source or part not present is `source_missing`;
inventory and inspection naming different clone sets is
`source_enumeration_mismatch`; any `blocker:` consumer is
`source_not_exclusive:<blocker>`. The run's own local records are never a
blocker. The descriptor (clone path, branch, head, `dev:ino`, retained-source
shape, consumers) is bound into the digest.

### Order and failure model

Order: channel claim gate, authority A, capture cycle C (`skip`), journal
pre-check J (released), the inventory source boundary S in record-disposition
mode, J again with a fresh full re-assessment and digest equality, the source
proof, then the first mutation. The proof keeps the execution tail and
reservation, the live-attempt path check, quiescence before and after, and the
clone identity (path, branch, HEAD, `dev:ino`) equal to what describe bound. It
skips only the worktree cleanliness check, because the tree is left in place.

Deletion order: canonical bundles, records without a binding, other
binding-carrying records, the anchor last, then a nonrecursive `rmdir` if the
run directory is empty. Quarantine and executing state, a pending generation
report and terminal protection are re-read before every unlink. Client
cancellation is honoured only before the first unlink; after it, failures
return `partial`. It never deletes tree bytes, refs, pins, server archives,
other generations' data or unbound files, and never releases, uploads or ACKs.

Failure model: abrupt process kill only, verified by real `SIGKILL` of a child
worker at each distinct on-disk state (every unlink) of three Case B
owner-discard worlds
(`agent/test/recovery-local-record-restart.test.ts`). Every surviving state
re-describes as the same class; the digest changes once any file was removed
(the old digest is then refused `descriptor_changed`) and is unchanged if
nothing was removed; `not_found` once the anchor is gone. Storage or power loss is unspecified (rename-atomic, not
fsynced). There is no autonomous retry: a fresh describe, a fresh confirmation
and `--case-b` are required.

### Retained classes

These stay refused even when confirmed: merged-predecessor discard
(`policy_discard_retained`); any generation (including a released mismatch) whose anchor has no
persisted hold binding, or whose saved FINAL has an archive disposition with a
missing or mismatched `serverCaptureId` (`binding_missing`, raised by the
classification shared with Case A);
`source_evidence_empty`; and a replay-refused anchor without a persisted
settlement (`replay_refused_persisted`). A record that fails its MAC refuses the
whole run (`record_unverified`).

### Consequences

- **Later-generation cleanup.** Retiring generation N can change a later
  generation M's ACK-cleanup state (for example `pending` to `acknowledged`).
  Describe lists this as `laterCleanup: [{generation, before, after}]`, and the
  list is in the digest. The request never retires M. The existing autonomous ACK
  cleanup, or a Case A resolve, may retire M afterwards.
- **Per-pass RPC.** Pins stay, so each later maintenance pass may spend one
  holds-list call on N's leftover owed contexts. This cost is expected from the
  design and has not been measured.
- **Follow-on source disposal.** After retirement `inspect` may show
  `local_discard`: the existing epoch path reclaims only if its own predicates
  pass, and that path runs only on the one opted-in canary worker, if any. Or it may show
  `local_protected_discard`, which the epoch deletion recheck refuses; the
  ordinary operator `discard --protected` runs its own checks. Record
  retirement guarantees neither outcome and grants no source disposal.
- **Known limit.** The completeness inspection covers every repository on the
  worker within the inspect budget. A large or unreadable unrelated retained
  source can make every Case B request refuse `source_evidence_incomplete`.

### Boundaries

This does not implement #4007's automatic reclamation, and it does not clear
Git journals or recovery-owner metadata. Terminal protection, settlement-root
entries and pins stay. The Case A behavior above is unchanged.

## Amendment 2026-10-29 — C1b pins do not authorize local retirement (#3591)

[ADR-3591](3591-prerequisite-closure.md) adds API prerequisite pin retention
without changing covering FINAL ACK authority. A pinned thin capture still
cannot authorize archive FINAL, inventory retirement or physical source
disposal. C2 release and its proof/permission work remain separate.

C1a's available-entry/query guard and its rollback boundary above are unchanged.
S7 prerequisite cleanup requires confirmed expired/discarded capture state,
frozen binding, expected-tip CAS and confirmed absence. Available pins survive
hold settlement by another route. Unknown creation is listing-only indefinitely;
eight exhausted cleanup attempts retain the live pointer and removal blocker,
then list with backoff capped at one hour. No manual delete, owner shortcut,
database override, rebind or attempt reset is granted. The active-capture limit
does not bound leaked pins or audit tombstones. Local lifecycle tests are
documented in ADR-3591; hosted acceptance and timing remain open.

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

- With pending authenticated guarded inventory, the generic custody path
  requires every exact hold to be `discarded`. Mixed `released`/`discarded`
  holds do not suffice for that path.
  [ADR-2883](2883-acknowledged-final-report-retirement.md) adds a separate
  ACK-final report-only exception for exact completed guarded ownership and
  covering archive ACK, with fresh complete custody containing at least one
  released exact hold. It permits positively missing attributed clone paths,
  retains unresolved sources and publication reservations, and grants no
  cleanup authority. [ADR-2652](2652-report-only-retirement.md)'s existing
  physical archive proof, including its missing-clone refusal, is unchanged.
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

## Amendment 2026-10-28 — available capture UPDATE entry rule (#3590)

**Status**: Implemented in API/store at `991df8b3`; exact-head CI/lander
acceptance remains pending. This is C1a of
[PRD-2506](../prds/2506-prerequisite-closure.md#c1a-execution-and-evidence-2026-10-28-child-3590),
not completion of #2506 or prerequisite-closure release.

### The invariant

An UPDATE of `recovery_captures` into `available` is permitted by the
state-entry branch only when `OLD.state` is `uploading` or `available`
(same-state). Migration `00374_recovery_available_guard.sql` replaces
`recovery_inventory_capture_guard` with the preceding 00322 Up function body
plus this refusal:

```sql
IF NEW.state = 'available' AND OLD.state NOT IN ('uploading', 'available') THEN
    RAISE EXCEPTION 'recovery capture must be uploading before becoming available' USING ERRCODE = '55000';
END IF;
```

Raw UPDATE and parameterized `MarkCaptureState` requests entering available
from expired, discarded, preparing or needs_action raise SQLSTATE 55000.
The branch precedes the identity guard's silent `RETURN NULL`, so a combined
identity/state change receives that exception too. This is a selected entry
rule; it does not establish that expired or discarded can never leave those
states through arbitrary SQL or a sequence of other transitions.

The `MarkCaptureReady` query separately requires `manifest_bound = true`
AND `state = 'uploading'` for the selected capture ID. If no row matches, it
returns exactly `pgx.ErrNoRows` through sqlc, rather than relying on a trigger
exception. This also rejects same-state available calls through that query;
the trigger permits same-state available metadata changes subject to its
existing proof, expiry and identity checks.

### Retained authority and rollback

The trigger remains BEFORE UPDATE; INSERT authority is unchanged. Existing
redundancy-proof immutability and validation, protected-final expiry and
identity rules are retained. An uploading-to-available transition or
same-available metadata update gains no exemption from those rules.

Down restores the preceding `00322_recovery_archive_redundancy.sql` Up
function body exactly. The 373→374→373→374 LiveDB round trip checks the
preceding, Up, Down and reapplied guards and exact captured
`pg_get_functiondef` restoration. This rollback removes C1a's guard branch;
it grants no future C1b rollback authority.

The migration replaces the function in a normal goose transaction and
rewrites no capture data. Function replacement/catalog-lock contention is
unmeasured. Focused test timings are recorded in the PRD; representative
production latency, mixed-version operation, comparable full-job cost and
CI critical-path impact have not been demonstrated.

This amendment adds no pin, custody-release, cleanup, capability or TTL
authority. C1b remains blocked until C1a is merged AND exact-head CI/lander
validation passes. C2 release, #2969 integration and producer enablement
remain separate work.

## Amendment (2026-11-01, #3727 C2): guarded retention releases a thin archive on a pinned closure

**Status**: Implemented by C2 of [PRD #2506](../prds/2506-prerequisite-closure.md).

The prerequisite-free requirement for a guarded archive FINAL gains one exact exception: the capture lists prerequisites, is bound and unexpired, and its closure is pinned and re-listed by the API at release time ([ADR-1296](1296-durable-run-recovery.md), 2026-11-01 amendment). The database guard `recovery_inventory_hold_guard` gains a branch that requires such a pinned, absence-free closure whose checksum and prerequisites match the capture, and the release-evidence CHECK admits `prerequisite_closure` only together with `final_closure_id`. The C1a available-capture UPDATE entry rule is unchanged. The proof is archive-only; retention of unpublished sources, pins and journals otherwise follows the existing rules.

Mixed-version limits, all fail closed:

- C1b API with a C2 worker: the API does not advertise `recovery_prerequisite_closure_v1`, so the worker never mints a thin archive FINAL and the hold stays protected.
- C2 API rolled back to an older binary without running Down: the older code cannot observe a closure-released hold or classify it across runs, and fails closed on both. A replay of an archive FINAL still ACKs by kind, capture, source and digest.
- C1b binary against the C2 schema: it never writes the new `prerequisite_closure` evidence or `final_closure_id`.

Accepted residuals: a thin FINAL stays stuck and is retried while the pin or the feature is unavailable; available-pin audits run at about 24 hours, so an external deletion can go unnoticed for up to a day before release-time listing records the absence and refuses; an external writer deleting or rewriting the pin after release defeats restore (the external-writer residual above is unchanged). A closure-released hold remains not history-safe for forge placement (`fn_forge_source_history_safe` is unchanged).

Deploy-window cost: the schema migration (00393, renumbered at landing from draft 00389) takes `ACCESS EXCLUSIVE` on `recovery_custody_holds` for the combined `ALTER`. Measured on synthetic data it is about 0.4 s per 500k holds (about 0.8 s per million), plus up to 5 s of `lock_timeout` waiting; live row counts and a cold cache are unmeasured. Its Down refuses (SQLSTATE 55000) while closure-backed rows exist.

Coordination with #2884: it also rebuilds the `recovery_inventory_hold_guard` function and the release-evidence CHECK. Whichever lands second must rebuild from the other's live body rather than from its own copy; neither migration may drop the other's branch or constraint value.
