# ADR-3607: Compact policy-hold M1 isolation

**Status:** Accepted narrow M1 repair contract; partial checkpoint through
d7b03dae276ebc2c54681a47a5205c2903654140. Initial R1/R2 reviewed; R3 INCOMPLETE.
Final merged validation, integration review and exact-head CI are pending.
**Date:** 2026-10-31
**Issue:** [#3607](https://github.com/vtmocanu/uzi/issues/3607)
**PRD:** [Compact policy-refusal hold](../prds/3607-compact-policy-refusal-hold.md#decision-log)

## Context and authority

The isolated-Outbox reproduction after the earlier M1 checkpoint demonstrated a
mandatory isolation/latch defect. Historical successful local checks and CI remain
successful for their tested tree; their readiness inference missed this case.
This blocker is M1, distinct from F5's deferred M2 retention-flag cleanup.

The owner's explicit server-contract approval amends the PRD's former absolute
“Do not add a second capability, readiness overlay or wire-version scheme.”
The narrow exception is the generic protocol capability
`pending_outcome_scoped_claim_v1`. It is independent of the single unreleased policy
contract `provider_policy_hold_v1`; it neither replaces that contract nor advertises
policy readiness. The API still omits the policy feature from its advertisements.
Implementation judgment was not a prior waiver.

The owner/server-supplied APPROVED plan-tool reports at baseline 4faaec1d record the
architect/tester/reviewer/auditor design critique and revision before implementation:
correct F6's baseline premise, remove the proposed bodyless overflow guard, startup
rewrite and durable placeholder, and approve this narrow protocol exception.
These supplied reports are design history, not locally rerun design tests.
Implementation reviews of immutable R1/R2 ranges and their focused executions are
separately inventoried in the PRD. No new runtime architecture proof is claimed by
this documentation turn.

## Decision: isolate retained sources without inventing authority

### A bounded live catalog and an existing journal

In `agent/src/outbox.ts`, `admitPolicyMetadata`, `reservePolicyFlight` and
`reserveInspection` share 1,024 simultaneous slots across selected policy/UNKNOWN
metadata, producer reservations and inspection/held physical scopes. Ordinary
terminal metadata is outside this tier. The 1,024-byte check bounds the encoded
identity metadata for a slot; it is not a JavaScript heap or Map-overhead bound.
Slots measure live occupancy, so sequential reuse is not a lifetime 1,024-event
limit. An unused producer reservation is not a pending terminal outcome.

The runner determines the eligible producer envelope at claim entry, before provider
selection/work, and reserves its slot before entering that flight. This captures
the negotiated eligible lane for that producer; delivery later rechecks live support.
`selectPolicyFlight` synchronously transfers the reservation to an outcome identity
and selection token before capture or persistence yields. Selection is tied to that
flight/generation, not authority acquired by a later capability refresh. Its object/token
epoch is rechecked after asynchronous authentication so an older read cannot overwrite
a newer synchronous selection. The captured
report body is frozen, and journal retries retain its selected identity, payload hash
and message fence. A competing file or disappearance cannot substitute another
outcome for the selected one.

A valid `policy_hold` is an existing terminal journal variant under
`MAC_DOMAIN_TERMINAL`, not a new MAC domain, durable placeholder or deletion
protocol. Retained bodies still have configured run/total content budgets. A
memory-only selection protects the running process but can be lost on crash before
durable installation; it is not crash-durable evidence. Policy producers can remain
backpressured permanently when the live catalog is full of unresolved evidence.
Destroyed or unprovable identities cannot be safely evicted or treated as settled
to recover capacity. Unrelated unnegotiated ordinary work retains its existing
exact-target admission and unjournaled fallback where that target is positively safe.

### Bounded demand and recovery, without a startup rewrite

Boot content budgets and per-record authentication remain. A record marked internally
`budgetDeferred` (budget-deferred evidence) can be authenticated by a later bounded
on-demand read rather than being permanently classified oversized because the
aggregate boot budget ran out. The selected identity still wins over competing
files, mutation or disappearance. Inspection capacity is reserved before interpreting
an additional candidate, including before boot/runtime raw directory reads.
Authentication joins descriptor close before ordinary promotion or slot release,
then rechecks the selected identity and current metadata object. Failed close or
unknown evidence retains protection. The 76bc3680 and 2df3641f repair reviews and
regressions are inventoried in the PRD; these changes do not establish a whole-startup
heap bound. Opaque/provisional evidence prevents a claim of complete
inventory; safe known source bindings supply identity coverage, not authentication
or settlement authority.

The targeted scanner and persistent recovery pager cooperate after at most 256
visited entries per slice or a one-second elapsed-time checkpoint. Ignored entries
consume visits. The deadline is cooperative, not preemptive I/O cancellation: an
already awaited filesystem call can outlast it. Mutation retries retain the demand's
consumed byte budgets. Skipped, exhausted or mutated inventory is not evidence of
coverage. Existing root enumeration, telemetry, temporary-file cleanup and key census
are not covered by a new bounded-startup claim.

The combined source getter retains installed metadata and known physical UNKNOWN
sources until registry wire tuple deduplication. A generic safe-leaf/source binding
does not become an ordinary journal, fabricated source generation or deletion proof.
The exact known source can contribute coverage; an opaque older scope remains
unavailable even after EOF or disappearance. Existing MAC-rejection cleanup may
discharge its exact captured physical scope only after its existing custody,
fingerprint, unlink and directory-fsync checks. Independent selected or opaque
evidence stays protected.

### A fresh, transaction-local scoped claim exception

An active snapshot may carry:

```json
{
  "pending_policy_protection": [
    {"run_id": "<run UUID>", "owner_generation": 9}
  ]
}
```

The group relates retained source tuples in complete, untruncated `pending_all` to
a current owner generation. Source generations stay in `pending_all`; they are not
rewritten to 9. UNKNOWN evidence with active=[] is valid input when complete source
coverage and independently known ownership are available. It does not require a
fabricated current-generation row lease.

The client uses the field after two-sided negotiation: the API advertises the generic
feature with active snapshots enabled and the worker's registration report contains
the capability. `WorkerClient.supportsScopedPendingClaim` checks both. At claim time,
the server uses the capability on the freshly locked worker row, not a stale handler
copy. Feature loss/downgrade stops the exception; selected evidence, local source
protection and existing server overflow/lease protection stay active.

In the snapshot-carrying transactional claim lane, the existing nonce/epoch checks
must accept and freshly apply the enclosing snapshot. The lock order remains
worker → ordered parents → ordered frozen targets. After
`LockFrozenWorkerSnapshotRuns`, `scopedPendingGroups` checks each group against the
actual locked target set and a fresh row: same user/worker, positive current owner
generation, unreleased claim, status claimed/running. Frozen kind/target/parent
association fences remain part of target and identity writes.

The absolute wire ceiling is `ActiveSnapshotMaxEntries` (default 256), independently
of the real admission cap `WorkerOutboxMaxPending`. A valid group costs one unit;
each ordinary tuple outside groups costs one unit. The exception requires this sum
to fit the actual configured pending cap, not merely the wire ceiling. Invalid groups
disable the exception as a whole while independent valid snapshot evidence can persist.

Historical grouped source generations legitimately produce zero-row identity upserts:
their current owner/frozen proof accounts for the group without creating a historical
lease. Each ordinary tuple must independently produce its exact attributable identity
write through `UpsertFrozenWorkerPendingOutcomeIdentity`; a zero-row ordinary write
disables the exception. Active terminal-pending entries must match a source tuple or
the proven group's current owner generation.

This is a trusted worker evidence attestation about grouping and completeness.
The API cannot cryptographically prove filesystem completeness or prove that a worker
did not label an ordinary journal as a policy source. It verifies the bounded
transactional ownership/accounting contract, not the worker's filesystem.

The exception is consumed in that claim transaction after successful snapshot
application. Register, heartbeat and the compatibility replacement entrypoint do not
grant it. No exemption is persisted. `SetWorkerSnapshotState` still persists overflow;
unchanged candidate SQL still excludes work attributed to an overflowed worker.
An unrelated unassigned candidate may be claimed under the fresh exception without
making that worker's attributed candidates available. Bodyless ordinary claims,
ephemeral claims and no-transaction compatibility paths are unchanged; there is no
new overflow guard on those paths.

### Cancellation and permanent outcome precedence

In `ReportPolicyHold`, an already requested owner cancel uses exactly one
`CancelRunByWorker` mutation. The service reads the updated owned row in the same
transaction using the locked current worker, checks ownership/lane and cancelled
status, then commits. It then performs the five existing best-effort follow-ups
from authoritative metadata: state broadcast, terminal notification, scope-input
settlement, checkpoint retention/deletion and ephemeral teardown. Failed readback
or commit does not run the hooks. No second state mutation or completed-custody
release is introduced; custody guards remain.

On the single-run detail route, a recognized current-owner permanent blocked reason
takes precedence. The transient `codex_policy_hold_pending` overlay is set only
when `dto.OutcomePending == nil`. Confirmed pending-outcome discard and its typed
race/auth/confirmation responses retain their existing contract, including successful
POST 202 followed by GET 200 cancelled. Display telemetry does not grant disposition
or cleanup authority.

### Consumer retention and retry history

R1 repaired source projection and G1/G2 finalize retry bookkeeping introduced during
the repair. `finalizeRecordIdentity` anchors retry state directly, so temporary
ineligibility is not mistaken for record removal. History and fairness survive
policy exclusion; candidate custody and deletion guards remain unchanged. Source
retention, delivery ACK/settlement, current ownership/lease and physical retirement
are separate facts. An ACK can settle admission without authorizing clone, journal
or custody disposal.

## Consequences and delivery boundary

[ADR-1393 D11/D13](1393-worker-outbox-outcomes.md) and
[ADR-2009](2009-pending-outcome-identities.md) receive scoped amendments for this
claim-only exception. Their earlier decisions remain historical records.

M2/M3/I1 remain deferred under the formal owner partial decision. Policy Resume
and Stop are unavailable. F5's eventual flag clearing belongs to M2: clear solely
successful policy-admission retention while preserving independent vetoes, including
`retainedEpisode` and `predecessorCapture`. Optional m3 historic-head hardening stays
deferred: current report-path 40/64-hex validation and React escaping do not validate
arbitrary historic database values.

Initial R1/R2 delivery and later enrollment/descriptor-close repairs retain their
candidate-bound reviews and focused evidence. The required 114-file agent selection
at 2df3641f failed two fixture assertions. Test-only repairs through d7b03dae pass
the lead's and independent reviewer/tester 16-case selections. A bounded auditor
review found no specific security defect in the supplied complete two-file diff;
it did not execute tests, scanners or independent repository verification. Final
integration review and the merged rerun are pending. The PRD inventories failures, repairs, review limits and
historical successes without treating them as final validation.

Current-main integration remains required. Public REST observation on 2026-10-31
at 08:32:14 UTC found main at 01d624859f34c0fff969081fbc9eb50100776416; that commit
object is absent locally. Its migration prefix 00386 collides with the feature's
compact-policy migration. The worker must supply the fetched current tip and object;
the agent cannot fetch over network Git. Integrate with git merge, inspect conflicts,
then use task migration:renumber above the fetched live head and fix reported
references. Changed runtime, API, SQL and package inputs require renewed validation;
no history rewrite, manual dependency installation or object reconstruction is allowed.

The [PRD validation record](../prds/3607-compact-policy-refusal-hold.md#deferred-scope-and-final-validation)
records the successful current-branch strict agent, Codex M4 and complete seven-package
LiveDB gates at 5b8a14f8, including the shared claim-route fixture. The API component
inputs remain unchanged from its recorded successful gate. The expanded 125-file
affected-agent selection also passed at f6240187. A small matched common-check
timing pair is recorded separately from new conformance-case costs, without a CI
performance inference. The canonical task gate:repo at
855378d5bd1febe0ba0dad411405ed92a6fa83f1 failed with EXIT=201 after 1,332 seconds
(log .uzi/scratch/gate-log.qJcAkA). Its first 41 top-level targets passed, then
test:ci-safe-paths failed at scripts/ci-safe-paths.test.rb:225, “failed Brew proof
remains a gate failure”: expected 1, got 3. The subsequent 35 top-level targets
were unexecuted in that invocation. A separate serial diagnostic at cf52af62
attempted all 35: 34 passed and check:changelog-entry failed because the inherited
fragment was edited without a new entry against the published baseline. Its
EXIT=1 and 1,092-second elapsed time are recorded in .uzi/scratch/gate-log.Ho3GiD.
The repair adds changelog.d/47d1a848-2195-4347-8be6-b3a518219524.md alongside the
required existing fragment to satisfy that coverage rule, with focused validation
in the run receipt. This is an in-scope release-evidence addition, not a capability
or readiness change; name it in the MR description. All suffix targets were
attempted, but separate diagnostics cannot establish a full canonical gate pass.
The final gate remains FAILED, not waived.

At cf52af62 the actual secret scan passed with zero tracked findings and both
canaries detected; 182 untracked scratch findings were separately reported as
non-gating. Semgrep passed with its canary detected and zero tree findings/errors.
These are candidate-bound results, not proof of future merged or published inputs.

The narrow watcher probe in .uzi/scratch/r3-ci-safe-brew-reproducer.json records
exit 3 and stderr “release-watch: not in a git checkout”. The direct diagnostic
using git -c core.fsmonitor=false was denied by the worker Git guardrail because
modifying remote/core/http/credential Git config is not permitted. Do not retry
or bypass that denial. Baseline reproduction has not executed; no passing baseline
or unrelated/pre-existing classification is established.

F7 thread reply 4426391039 and resolution of PRRT_kwDOUGFljs6yEQQM both returned
success; those thread actions are complete. Default integration remains open:
the missing live-main object prevents the approved merge, migration 00386 collision
repair and merged-input revalidation. Changed merge inputs require renewed validation.
VCI remains externally pending; final published-head agent CI and comparable CI
timing are separately pending. This is a
partial checkpoint, not a completion claim or waiver of R3. Historical successful
checks remain successful for their tested candidates. The final MR description
must preserve F5/m3 and M2/M3/I1 deferrals and report only observed results.
