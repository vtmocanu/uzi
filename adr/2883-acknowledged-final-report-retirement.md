# ADR-2883: acknowledged FINAL permits report retirement without source cleanup

**Status**: Implemented
**Date**: 2026-10-13
**Issue**: [vtmocanu/uzi#2883](https://github.com/vtmocanu/uzi/issues/2883)

## Context

After restart loses the runner clones, a completed run can retain both a
covering archive FINAL ACK and a redundant unacknowledged publication-settled
FINAL. Its pending terminal or finalize report needs a separate read-only
retirement proof without resolving that remaining recovery obligation.

[ADR-2417](2417-guarded-local-retention.md)'s generic pending-inventory custody
path still requires exact discard. [ADR-2652](2652-report-only-retirement.md)'s
physical archive path still requires strict actual clean content and refuses
missing clones. This additive decision preserves both contracts; missing
physical sources qualify only through the separate proof below.

## Decision

### Exact ACK coverage and fresh custody authorize the report alone

`RecoveryCoordinator.retireAcknowledgedFinalReport` in `agent/src/recovery.ts`
is separate from `retireArchivedReport`. For positive safe-integer generation
G, bounded authenticated journal reads must find exactly one current guarded
archive FINAL ACK and exactly one unacknowledged FINAL. The latter must belong
to G, be `settled` with `publication` evidence, have the empty inventory digest
and no original roots, and match the archive's coverage context. Snapshot-change
or quiescence-breach evidence refuses proof.

The ACK record must be an eligible uploaded archive with bound capture id,
checksum, positive byte size, original/current heads and frozen roots. Its
resolved bare repository must match the coverage context. A fresh status read
must prove an available, manifest-bound, unexpired archive matching that record,
without a failure reason. Relevant journal heads and owed roots must be covered
by archive ancestry; owed contexts must match frozen root contexts. Frozen heads
remain requirements after pins disappear. Earlier covering-ACK generations and
positively attributed later generations retain their existing coverage treatment.

`WorkerClient.withAcknowledgedFinalReportCustody` in `agent/src/client.ts`
requires stable registered worker identity and both `recovery_inventory_v1`
and `terminal_rejection_report`. Fresh ownership must be `completed`, exactly
G, with `inventory_guarded === true`. The fresh, runtime-validated custody
response must match this run, worker and G, be `settled`, complete for exact
and sibling sets, and contain a nonempty exact set with matching counts.
Each exact hold must be released or discarded, with at least one released;
no open sibling hold on this worker/run may remain. Discard-only custody does
not qualify for this separate path.

### Positive missing paths are distinct from unknown attribution

`GitCache.readAcknowledgedReportSourcesUnderLock` in `agent/src/git.ts` reads
a bounded, stable worker-owned config snapshot without includes, validates
recovery attribution, protected descriptors, attempt ledgers and stopped
markers, and probes attributed paths. Positive `ENOENT` on the permitted clone
path chain can establish missing sources. Permission errors, unsafe paths,
malformed/conflicting metadata, changed identities and overflow are unknown
and refuse proof. Missing paths do not prove physical disposal or permit
metadata cleanup.

Present relevant clones still require covered heads and the existing strict
physical clean-content proof. On Linux, process observations bracket each of
two content inspections; observed writers refuse rather than being killed.
Source snapshots are compared after each pass and re-read after fresh custody,
before conditional unlink under the same bare lock and cumulative metadata
budget. Recovery journals and owed roots are also rechecked for changes.

The exact installed authenticated report is conditionally unlinked under
C(skip busy) → S(refuse busy) → J → B → O. Execution, residue quarantine,
undrained messages, held terminal resolution, cancellation and changed report
identity retain the report. The proof uses the earlier of the one-second
budget and the caller deadline; restart and ABA replacement require reproof.
Finalize unlink also refuses while a companion terminal remains pending.

### Result authority cannot detach publication bindings

`Runner.retireRecoveryReport` returns ephemeral exact-call authority
`acknowledged_final_report_only`, distinct from `existing_archive`. The
protected terminal replay caller and worker finalize retry caller return on
that new result before completion notification. They skip
`notifyPublicationCompletionRetired`, `releasePublicationCompletion` and
`releaseRetiredPublicationCompletion`, including when a receipt exists.

Report-only unlink neither detaches nor excludes publication bindings, tickets
or reservations. It removes no source, pin, journal, request, attempt or
settlement evidence and sends no cleanup/release RPC. Ownership, archive and
custody reads remain necessary. Later release or detachment requires existing
independent authority; SettlementJournal machinery is unchanged.

## Verification and integration boundaries

`agent/test/runner-acknowledged-final-restart.test.ts` uses real local
capture/release history and runner sweeps with testing-server seams. It exercises terminal replay and actual worker
finalize sweep, retained publication state and downstream live/due/fresh-boot
recovery retries. Finalize noninterference is exercised without a companion
terminal because Outbox refuses finalize unlink while terminal is pending.
The approved later independent physical detachment control is limited to the
initial terminal-replay history's retained companion finalize report; it is
not claimed for every caller or variant. The fixture also exercises existing
completed/live settlement replay rather than introducing settlement machinery.

Local restart fixtures do not establish hosted Docker/Kubernetes acceptance.
PR #2874 owns the live-pass pending-report filter and finalize-authority-first
ordering. Integration must preserve that filter before sort/slice, and must
keep this ACK-only branch reachable when generic finalize authority refuses.
Neither ordering change adds retirement or cleanup authority.
