# ADR-3607: Compact policy-hold M1 isolation

**Status:** Accepted narrow M1 repair contract; partial checkpoint through
d7b03dae276ebc2c54681a47a5205c2903654140. Initial R1/R2 reviewed; R3 INCOMPLETE.
Superseded in part (2026-11-07): M2/M3 deliver owner Resume (see Consent and
continuity below); Policy Stop remains omitted. Final merged validation,
integration review and exact-head CI are pending.
**Date:** 2026-10-31
**Issue:** [#3607](https://github.com/vtmocanu/uzi/issues/3607)
**PRD:** [Compact policy-refusal hold](../prds/3607-compact-policy-refusal-hold.md#decision-log)

## Context and authority

The isolated-Outbox reproduction after the earlier M1 checkpoint demonstrated a
mandatory isolation/latch defect. Historical successful local checks and CI remain
successful for their tested tree; their readiness inference missed this case.
This blocker is M1, distinct from F5's deferred M2 retention-flag cleanup.

Run 8d3e58c5 had no separate owner approval of the server-contract change.
Cross-check round 1 requested revision at sequence 1302; automatic revision
followed at 1304. Round 2 approved at 1844 but warned that the new capability
conflicted with the PRD and required explicit approval. The plan posted at 1845
and implementation began at 1846 under auto-approval. That automation and the
supplied APPROVED plan-tool reports are design history, not owner consent.

On 2026-10-31, lead and buddy accepted the narrow generic protocol capability
`pending_outcome_scoped_claim_v1` for landing with the user's consent to their
recommendation. This dated acceptance amends the former absolute prohibition on
a second capability; it does not invent a prior owner decision or waive
validation. The owner consented to this acceptance on 2026-10-31, conditional on
buddy agreement, which was given. It is independent of the single unreleased
policy contract
`provider_policy_hold_v1`, which remains unadvertised by the API.

*Note (2026-11-07): since #3607 M2/M3 the API advertises
`provider_policy_hold_v1`; the sentence above records the M1 decision.*

A worker-only relaxation cannot supply the server's scoped overflow exception;
reusing policy readiness would couple generic pending-source protection to a
disabled policy feature. Both sides negotiate the narrow capability. In the
claim transaction the server revalidates worker registration, frozen run locks,
current ownership/generation, release state and bounded pending accounting.
Source classification and completeness remain worker attestations, not facts
independently authenticated from worker disk. Missing capability or invalid,
incomplete or uncertain evidence retains attributed-candidate protection. The
separate incomplete-inventory intake exception below never asserts coverage.

The historical critique corrected F6's baseline premise and removed the proposed
bodyless overflow guard, startup rewrite and durable placeholder. Implementation
reviews of immutable R1/R2 ranges and focused executions are separately
inventoried in the PRD; neither those reports nor this correction claims a new
runtime architecture proof or a prior owner waiver.

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

### Local claim capacity and physical coverage

Physical inventory coverage does not by itself pause the local ordinary claim
loop. A fresh boot, a symlinked run directory, or a mutation between inventory
pages can leave coverage unavailable with zero known pending outcomes. In that
case both legacy and scoped local claim gates stay open. Snapshots still omit
`pending_all` and retain `pending_overflow`, preserving conservative protection
for worker-attributed runs without falsely claiming a complete inventory.
Request emission alone did not establish API admission in the initial D1 repair:
the older claimant-wide overflow guard still returned idle. The marker below
repairs that server-side gap; the shared agent/API regression proves an actual
claim, not just a fake client's invocation.
Actual pending capacity and the existing legacy unknown-source gate remain.
Scoped capacity counts only positively observed owner generations, matching
`pending_policy_protection`; unknown ownership supplies no invented lease.

### Uninspectable-root recovery boundary

A disabled store is not proof that a target has no earlier first writer. An
inaccessible or symlinked root can hide an existing policy or UNKNOWN journal
after restart or downgrade. Keep exact-target deferral unless absence is
positively established; process-local enrollment, a first-claim assumption,
missing server policy fields, or absent feature negotiation cannot replace
that proof. A durable never-written witness would be a separate M2 design.

The operator repair must preserve the original key and every retained journal.
Repair the configured path or permissions, or move that intact store while the
worker is stopped, then restart so the evidence is re-arbitrated. Replacing it
with an empty directory or minting a new key to regain progress is not this
recovery procedure and grants no discard authority.

Restart re-arbitrates durable artifacts only. It does not make a memory-only
selection crash-durable or prove it delivered or settled. Preserve recoverable
work and diagnostic evidence before stopping; the existing loss boundary for an
intent that could never be journaled remains explicit.

The existing pre-factory admission refusal can leave the server row `claimed`.
Snapshot-absence requeue targets running/awaiting states; claimed-never-started
requeue is separately suppressed while heartbeat overflow protection remains
live. A continuously inaccessible root can therefore retain the claimed row
until storage repair. There is no automatic release or requeue guarantee for
already-claimed or running targets; these remain operator-held.

The user approved this intake change and diagnostic on 2026-10-31 after lead
and buddy recommended it. When initialization positively finds a symlinked or
unreadable outbox root, the
worker stops all new run, cross-check and chat claims in both negotiation modes.
This intake stop is distinct from healthy but incomplete inventory coverage and
from key failures on an otherwise readable root. Heartbeats and drains continue.
The optional `heartbeat_outbox_root_unusable` API feature negotiates the
`outbox_root_unusable` heartbeat/worker DTO diagnostic. It
reports only `symlink` or `unreadable`, independently of per-run `outbox_blocked`.
It uses the existing bounded, ten-minute in-memory outbox tracker, clears on a
healthy/absent report or registration, and is visible in `uzi worker list`.
Browser rendering remains deferred. Repair preserves the original `.key` and
all journals, then restarts the worker; an empty replacement is never recovery.

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

Deferral fields belong to the enrolled filename: an alias observation cannot
stamp its version/eligibility onto another file or clear that file's eligibility.
The existing version and selected-identity checks still control authentication.
Target, global and page scanner placeholders bind that observed version before
remaining deferred. Failed stats and individually oversized files revoke deferred
authentication eligibility; an existing binding cannot be refreshed to a different
version. First-sight authentication of valid unchanged bytes remains supported.
Runtime displacement of a rejected UNKNOWN entry by an authenticated alias is
deferred, retaining the baseline fail-closed boundary. No new rejection cache,
ranking policy or lifetime/version-cache subsystem is introduced by this repair.

Registration does not wait for ownership GETs. It publishes the conservative
pending snapshot first and starts heartbeats before the existing boot, drain and
sweep reconciliation reads. Those reads skip authenticated server-settled sources.
Unknown ownership still grants no lease or claim authority, and incomplete
coverage still cannot produce a complete `pending_all`. Each post-registration
ownership pass makes at most 16 requests sharing one second, including response
bodies through the existing HTTP deadline. Global passes rotate their starting
candidate, and expired responses supply no ownership proof. This bounds ownership
observation's added delay before boot admission and terminal replay independently
of retained-run count; existing delivery and admission guards remain unchanged.

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

#### Incomplete inventory: intake without complete-coverage authority

When physical coverage is unavailable, a negotiated snapshot may carry
`pending_inventory_incomplete: true` with `pending_overflow: true`, omitting
`pending_all` and `pending_policy_protection`. Known sources retain existing
bounded active projection; every attributed candidate remains protected by the
persisted worker overflow closure, including sources hidden behind skipped paths.
This marker supplies no complete inventory, per-run lease, release or deletion
authority. Actual local pending/protection capacity gates remain closed when full.

Only a freshly applied snapshot in the claim transaction, with the capability on
the locked worker row and positive pending capacity, may bypass claimant-wide idle
using this marker. Wrong types, false/null markers, stale nonce/epoch and a marker
combined with any non-null complete/group list grant no new exception. ClaimRun's
existing SQL still excludes all candidates attributed to any overflowed worker;
only otherwise eligible unrelated work can be returned. Register and heartbeat
cannot persist or grant the exception.

Roll out API support before capable workers. An API without capability support
causes the client to strip the marker. For protocol skew, an earlier API that
knows the capability but not this marker
ignores the nested field and conservatively returns idle. No partial list is ever
sent as `pending_all`, so it cannot be mistaken for complete empty coverage.
Outer worker bodies reject unknown fields; the isolated snapshot's custom decoder
accepts unknown nested fields, covered on both claim and heartbeat routes.
No migration or capability version is added. Coverage-specific operator diagnostics
remain deferred; the existing overflow display is not a new diagnostic.

#### Complete inventory: existing bounded ownership accounting

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

M2/M3/I1 were deferred under the formal owner partial decision at M1; M2/M3 have
since delivered Policy Resume (2026-11-07), and Policy Stop remains omitted. F5's eventual flag clearing belongs to M2: clear solely
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

At the worker checkpoint, current-main integration remained required. Public REST observation on 2026-10-31
at 08:32:14 UTC found main at 01d624859f34c0fff969081fbc9eb50100776416; that commit
object was absent from that worker checkout. Its migration prefix 00386 collided with the feature's
compact-policy migration. The worker must supply the fetched current tip and object;
the agent cannot fetch over network Git. Integrate with git merge, inspect conflicts,
then use task migration:renumber above the fetched live head and fix reported
references. Changed runtime, API, SQL and package inputs require renewed validation;
no history rewrite, manual dependency installation or object reconstruction is allowed.

The [PRD validation record](../prds/3607-compact-policy-refusal-hold.md#delivery-status-recovery-continuation-2026-11-07)
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
success; those thread actions are complete. At that worker checkpoint, default integration remained open:
the missing live-main object prevented the approved merge, migration 00386 collision
repair and merged-input revalidation. Changed merge inputs require renewed validation.
VCI remains externally pending; final published-head agent CI and comparable CI
timing are separately pending. This is a
partial checkpoint, not a completion claim or waiver of R3. Historical successful
checks remain successful for their tested candidates. The final MR description
must preserve F5/m3 and M2/M3/I1 deferrals and report only observed results.

Local integration on 2026-10-31 fetched main at
6007301a07e67d4733d66558ac8c795e97628ace and merged it into the recovered
275b2e9671258c205dd4da9bff3f91ab9ad43599 checkpoint as
14cb165d5618fae37b1cc3e5bfe0f3199e9880c4. The capability vocabulary preserves
both repair capabilities and main's prerequisite-closure entry. After review
repairs, the single coordinated re-anchor merged pinned main
bfb0fef80046eb7081a37ef0daebb6fb41f389ab as
9302c3a1e42e4353ce8c7d88feebae9d5b2b6d5b with no textual conflicts. The policy
migration is renumbered above the pinned main head at integration, and its tests
track that predecessor and the new policy version. Earlier 00386/00388 references
describe historical numbers. Local targeted validation passed as inventoried in
the PRD. Published-head review and complete exact-head CI remain required;
no worker-provisioning, full-suite or LiveDB result follows from a local merge.

## Consent and continuity (M2/M3)

M2/M3 deliver the owner Resume path for a `codex_policy_hold`. The user-facing
behaviour is in [Held on a Codex policy refusal](../docs/run-pause.md#held-on-a-codex-policy-refusal)
and [Codex policy hold](../docs/cli.md#codex-policy-hold). This section records the
decisions and what was rejected. The M1 deferral of Resume above is superseded for
the modes below; Policy Stop and revise-turn admission at `awaiting_approval`
remain approved omissions.

### Episode-bound consent, not grant arithmetic

Resume names its mode and the exact episode (`expected_episode_id`). The server
compares that episode with the current hold; an exact replay returns 200 with
`replay: true`, different guidance for the same episode is a 409 and is not
recorded, and consent for an older episode cannot resolve a newer one. `set-token`
cannot consent. Rejected: `ClaimRun` grant arithmetic and generation-wide grants
(the latter deferred to a follow-up). Binding every action to
`expected_episode_id` is what stops a replay from authorizing a later refusal;
later ordinary pickups are the chosen baseline behaviour, not reuse of old
consent. Only the optional compatible-lane one-claim Fresh uses a pending intent,
below.

### R/VG/VH continuity: the required head moves only on verified publication

A recorded required saved-work head is superseded only by a published report whose
checkpoint contains the latest head, at the same claim generation the worker
verified. Before any provider work the worker proves the recovered clone contains
the required head; otherwise it re-holds `saved_work_missing` with nothing started.
After `fresh_from_saved_work` the requirement persists until the claiming worker
verifies it at the current claim generation. Residual: clearing relies on the
claiming worker's local proof, not on publication.

### One-claim Fresh is consumed in ClaimRun

`fresh_one_claim` is consumed inside the `ClaimRun` transaction and restored only
on server-known non-delivery. A start that is never confirmed re-holds as
`fresh_start_uncertain` and is never repeated, because a provider conversation may
already exist. A released worker image that advertises
`provider_policy_hold_v1` but predates this support would consume the grant and
re-hold as `session_missing` without starting a conversation, so the API rolls
first and capable workers follow.

### Guarded Downs (migrations 00414 and 00413, "Shape A")

00413 adds the recorded hold cause and the required-head and recovery-verification
columns, 00414 the one-claim Fresh generation columns and a `NOT VALID` shape check, and 00415 validates it. The
Downs are guarded and run one version at a time from 00415: 00414 and 00413 refuse
live or orphan rows and convert paused or queued active rows to failed, and refuse
any other live row (claimed, running, `recovery_wait`, `limit_wait`, `pool_wait`,
`awaiting_*`) that still carries an active policy requirement; live rows without
policy state do not block. 00413 also refuses any live row where
`policy_preserve_required AND policy_resolved_at IS NOT NULL`, even with no head
(the normal state after a `preserve` or `fresh_one_claim` resume). Each is one transaction, so a `RAISE` rolls back fully;
each holds `ACCESS EXCLUSIVE` on `runs`. 00413's Down is destructive for the hold
cause and recovery identity columns of every row. Rollback is drain-only; the
operator procedure is in
[Rolling back](../docs/run-pause.md#rolling-back). Rejected: quiescing workers
alone, which leaves an old API able to resume rows it does not understand.

### Mcv not added

A separate validation migration for the 00413 check (an "Mcv") was not added. The
dba judged it optional: the API is a single Recreate-strategy instance, and the
check cannot fail on the new all-NULL columns.

### Residuals

- Server enforcement covers applied refusals; local pending evidence protects only
  its surviving worker.
- An absent worker or journal beyond the existing pending lease, or the
  wall-release race before delivery, can leave another claim unconstrained.
- Retained custody still counts toward the per-owner custody admission limit
  (default 8); no force-clear or capacity exception exists.
- Held assigned workers can defer idle-only rolls indefinitely (#3103).
- Policy journals survive a worker process crash, not storage or power loss.

### #3153 map cell

The runner-shared PVC subPath mount (controller render test
`TestDockerWorkerSharesTheRunWorkdirWithDindButNotSecrets`) and the agent
replacement test (`agent/test/runner-policy-saved-work.test.ts`) cover this cell: a
replacement worker over the same data volume recovers present saved work and
re-holds when it is absent.
