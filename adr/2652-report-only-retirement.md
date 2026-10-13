# ADR-2652: archive-backed report retirement grants no source cleanup authority

**Status**: Implemented
**Date**: 2026-10-10
**Issue**: [vtmocanu/uzi#2652](https://github.com/vtmocanu/uzi/issues/2652)

## Context

A cancelled run can have ended execution and uploaded a covering archive while
its authenticated terminal or finalize report remains installed. That report
can keep the worker advertising the run as active even though source custody
must remain open. The ACK/discard path in
[ADR-2417](2417-guarded-local-retention.md) separates reports from source cleanup,
but does not give a pending guarded inventory an archive-backed report path.

Archive byte integrity and source completeness are different proofs. An
available archive can omit newer heads or dirty working files. A thin archive
can cover the recorded heads but still depend on external commits. Neither its
checksum nor retiring a report establishes independent recovery or permits
archive-backed FINAL. The prerequisite-free FINAL requirement in
[ADR-1296's independent-recovery requirement](1296-durable-run-recovery.md#d5-independent-import-and-the-guarded-cached-dependency-exception) and the existing custody decisions in
[specs/human.md](../specs/human.md) remain in force.

## Decision

### Exact report, ownership and archive provenance

`Runner.retireRecoveryReport` in `agent/src/runner.ts` can conditionally remove
an installed authenticated terminal or finalize report for generation G after
real execution ends. G must be a positive safe integer. Fresh runtime-valid
ownership must report `completed`, `failed` or `cancelled` with
`claim_generation` exactly G; a later terminal generation is insufficient for
this archive-backed exception. `inventory_guarded`, when present in ownership,
must be boolean.

`RecoveryCoordinator.retireArchivedReport` in `agent/src/recovery.ts` requires
one eligible authenticated uploaded guarded journal for G. It binds the
server-minted capture id, source SHA, coverage digest/context, original and
current heads, original roots, checksum and positive byte size to the exact
reservation/upload provenance. The resolved worker bare repository must match
that context. Snapshot-change and quiescence-breach records do not qualify.

A fresh capture-status read must identify that capture as `available`, with a
bound manifest matching checksum and byte size, no failure reason, and a valid
future expiry. The existing API upload service in
`api/internal/recovery/service.go` verifies the actual streamed byte count and
checksum before binding the manifest, and checks a declared chunk count against
actual chunking. Report retirement consumes that evidence: it adds no local
bundle rehash or header read and no new aggregate verifier.

### Complete source coverage and actual clean content

Under the journal and bare locks, the worker authenticates the physical recovery
journals and enumerates owed roots and their contexts. Relevant journal heads
and owed roots must be ancestors of the selected aggregate; owed contexts must
match the authenticated frozen roots. Frozen original/current heads remain
requirements even after pins disappear. Earlier generations with their own
covering acknowledgment are settled for this check; positively attributed
later-generation sources are outside G's report scope. Unknown attribution
retains the report.

The runner discovers attributed clone heads and identities and checks coverage.
For relevant clones, Linux process observations bracket strict content
inspection, twice, with clone-inventory identity rechecked after each pass.
Report observations refuse writers rather than kill them.
`GitCache.credentialFreeCancelCleanHead` in `agent/src/git.ts` checks the real
HEAD tree from worker-owned objects, exact index, status, configuration and
actual file content. A clean-looking status alone is insufficient: hidden index
flags, unsafe filters, unexpected content or indeterminate reads refuse proof.
The existing worker scratch exclusion remains in force.

Dirty or untracked files and reset-soft WIP retain the report. A clean WIP
commit covered by the selected archive can qualify. Being a WIP commit is not
itself a refusal. Missing, unreadable, malformed, foreign or changed evidence,
unsupported process proof and bounded-input overflow retain the report.

### Proof and conditional unlink share one protected boundary

The operation holds C → S → J → B → O through conditional unlink:

- **C**, the exact generation capture cycle, uses `skip` when busy.
- **S**, the runner source reservation, refuses an existing execution tail,
  active run or snapshot. Only this proof's own tail is exempt from its
  eligibility check. A real flight or queued duplicate cannot borrow that
  exemption; a changed tail refuses deletion.
- **J**, the run recovery journal lock, protects authenticated source reads.
- **B**, the resolved bare lock, protects owed-root and ancestry observations.
- **O**, the outbox run lock, rechecks the exact installed report and performs
  its conditional unlink before the preceding boundaries release.

The terminal resolver in `agent/src/terminal-resolve.ts` captures the
authenticated payload and opaque lifetime identity before send, under O.
`Outbox.retireTerminalIfEligible` binds that identity to the canonical payload,
reauthenticates the installed record and refuses blocked records.
`Outbox.retireFinalizeIfEligible` rechecks the captured identity and authenticates
the finalize file; a pending terminal prevents finalize deletion. These paths
also recheck cancellation and synchronous eligibility. Undrained messages,
held terminal resolution, execution and residue quarantine prevent retirement.

Same-key replacement (including ABA replacement) does not inherit authority.
A restart reconstructs identities and requires fresh proof; no reusable archive
proof, persisted report receipt or new tombstone is introduced.

### Bounded proof, settled work

The proof deadline is the earlier of one second from entry and the caller's
deadline. `GitCache.withReportProofBudget` scopes deadline, cancellation and
bounded Git reads without changing concurrent capture/cleanup operations.
`reportBudget` in `agent/src/run-quiescence.ts` bounds observational process
proof. Recovery directory enumeration is capped at 256 entries; exceeding a
read/output/input cap or receiving an unknown result retains the report.

Cancellation or expiry before unlink prevents deletion. Once unlink starts,
its actual settlement holds O and the enclosing reservation even if the caller
budget expires. A failed unlink leaves the pending record for retry. Started
helper work likewise settles before the source/journal boundaries release;
the one-second authorization budget is not a promise that settling already
started work completes within one second.

Helper-owned temporary cleanup uses a fresh, independent bounded maintenance
budget so an aborted proof can still clean up its temporary directory. Cleanup
failure/expiry does not authorize retirement, and maintenance grants no source,
custody or report authority.

## Consequences and limits

A healthy covering thin capture may retire a report while custody remains open.
Once live execution has ended, the next accepted heartbeat omits the retired
report's terminal-pending active-run row. This changes the worker's pending-report
advertisement, not the run's terminal state or the archive's dependency guarantees. It grants no FINAL, custody
release, recovery-journal deletion, source/pin deletion, clone reclamation or
pod/PVC cleanup authority. Existing retention costs and owner recovery decisions
continue. The bad-MAC terminal policy and permanently blocked-report policy
remain distinct.

Rollout is evidence-based rather than version-based. Older workers keep their
existing report policy; older API responses or old journals missing the required
contract evidence cannot supply this new authority. Workers retain reports
until fresh supported evidence satisfies the full predicate. No API, schema,
workflow, custody or physical-cleanup change is introduced here. Ordinary report
retirement work in #2655 is outside this decision.

## Validation evidence

The desired regression was observed failing on the base and passing with the
fix. The named `TestHeartbeatRetiredCancelledReportOmissionLiveDB` executed and
passed in the actual LiveDB lane; integrated agent, repository and API gates
passed after implementation repairs. This documentation milestone does not
rerun the agent or API gates.
Hosted UID-split execution and Kubernetes timing verification are not established
by that evidence; neither is claimed here.
