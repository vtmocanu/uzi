# ADR-2943: physical report retirement covers competing pending FINAL sources

**Status**: Implemented; assessment complete, duplication retained
**Date**: 2026-10-14
**Issue**: [vtmocanu/uzi#2943](https://github.com/vtmocanu/uzi/issues/2943)

## Reachable history

The physical proof's omitted competing FINAL source is reachable. The regression
in `agent/test/physical-final-source-coverage.test.ts` uses production-written
MAC journals, real Git objects and archive bytes, actual successor admission,
and restarted Outbox conditional unlink. Server replies, process observations
and the wall-budget clock are fixture seams; this is not live database,
process-table or hosted Docker/Kubernetes acceptance.

Generation 1 commits H1, retains an unconfirmed checkpoint, then commits H2 and
parks at its wall budget. Boot recovery freezes and uploads an aggregate over
its frozen heads and owed H1/H2. The archive FINAL RPC fails, leaving its
synthetic source Kold and authenticated pending FINAL intact. The testing owner
transition models the supported paused-budget extension/resume route
(`ExtendAndResumeWallPark` in `api/internal/store/queries/runtime.sql`), followed
by an actual generation-2 runner admission.

Admission confirms a real checkpoint pack containing H2. Production checkpoint
reconciliation prunes the H1/H2 owed contexts before freezing a new local
aggregate Knew. Its current and original source are H2 and its roots are empty;
its synthetic tip differs from Kold. Reserve fails transiently, so this verified
local capture is adopted without becoming a second eligible uploaded archive.
The actual successor SDK commits H3. Its failed terminal and subsequent recovery
produce the sole eligible generation-2 uploaded archive. That archive covers
all frozen heads, roots, owed contexts and clean attributed clone heads, but
Kold is not its ancestor. The old FINAL remains pending, so the earlier-generation
covering-ACK exclusion does not settle it. Coverage pins are separate from owed
refs and do not independently require this old wrapper tip.

Baseline `ed2f53b7` lacked this source requirement. The committed characterization
at `7bcbb110` demonstrated actual terminal unlink with `existing_archive` authority.
Running the desired-refusal assertion with `UZI_EXPECT_FINAL_SOURCE_REFUSAL=1`
failed with actual `{ retired: true, authority: "existing_archive" }` versus
expected `{ retired: false }` (exit 1). The command was the agent Node test lane:

```sh
node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 test/physical-final-source-coverage.test.ts
```

The test verifies the old journal authenticates unchanged after coordinator and
Outbox restart, current archive cardinality is one, the old tip is divergent,
every frozen head/root is covered, physical inventory and clean-content checks
succeed, and archive import restores the committed work. Retirement leaves
sources, bundles, pins, journals and custody untouched.

## Producer constraints and decision

Empty settled inventory is not the divergent-source producer: `freezeInventory`
sets both its source and inventory-current SHA to the current input. The earlier
hand-written ACK-path settled journal is not evidence of that history. A failed
held-publication predecessor cannot use ordinary resume to become a successor;
failed ownership is refused by that route. Neither rejected scenario disproves
the paused archive history above. FINAL dispositions are minted by the settled,
archive and held-publication producers in `agent/src/recovery.ts`; replay retains
the frozen request and acknowledgment records settlement rather than inventing
a different source.

For each relevant coverage-digest journal, `retireArchivedReport` now additionally
requires archive ancestry of `sourceSha` exactly when it has a FINAL request and
that request is not acknowledged. This matches the acknowledged-FINAL proof's
existing source rule. Earlier non-FINAL aggregate tips remain checked through
frozen original/current heads and roots. Later and settled-earlier exclusions,
archive and ownership eligibility, custody requirements, physical presence and
strict cleanliness, C → S → J → B → O lock order, deadline, report identity and
cleanup authority retain their existing rules. This narrowly qualifies the
historical unchanged-physical-path statement in ADR-2883.

## Resolution checks and limits

The permanent regression retains the actual generation-2 terminal after restart
when Kold is uncovered. A positive control merges the old FINAL wrapper through
real Git operations during the successor SDK flight and retires both terminal
and finalize reports. Another control makes ownership unavailable after the
real predecessor upload, preventing production FINAL minting; that divergent
non-FINAL wrapper still permits both report retirements. Finalize is tested
after terminal retirement because a pending terminal independently prohibits
finalize unlink. The negative history therefore verifies terminal retention,
not an independent finalize refusal.

The three focused histories passed with no skips in 38.65 seconds locally;
the original single characterization took approximately 13–16 seconds. The
two controls add roughly 23 seconds to this test file in this local run, without
added wall-clock waits. Hosted critical-path timing, queue and cache costs are
unmeasured. Validation uses umask 027 to keep test Git directories free of group
write while preserving the session store's explicit 0750/0640 modes. No live
server transitions or kernel process exclusion are claimed by these fixtures.

The landed correction at `573014c6` passed `task gate:agent`: 14,364 unit
tests passed, 15 were skipped, none failed/cancelled; all 183 Codex M4 tests
passed and the required conformance matrix completed. The main focused run
passed 41 affected tests, and the independent tester reproduced the expected
baseline refusal failure at `7bcbb110` before confirming the corrected cases.
Docs, spec numbering, changelog, token-literal, lint, dead-code and type checks
also passed. The full gate was not repeated for this final documentation-only
assessment because agent source and tests remain identical to the gated commit.

Comparable local unsharded unit-stage logs with concurrency 4 recorded 2,429.53
seconds at `7bcbb110` and 2,978.53 seconds at `573014c6`; Codex M4 recorded
108.94 and 110.67 seconds respectively. Unit tests dominate the local test
stage. These uncontrolled runs overlapped read-only review and are not a causal
estimate of the two controls' cost. Cache, scheduling/queueing and review
contention were not individually measured; earlier failed/terminated gate
attempts are rerun costs, not included in these passing-run timings. Hosted
affected-job and overall critical-path measurements remain deferred.

## Shared proof core

### Recommendation and accepted maintenance risk

Retain the two bodies for this correction. The demonstrated coverage drift is
fixed and independently regressed. Either method delegating to the other would
change authority: the physical path admits failed/cancelled ownership but
requires present sources, whereas the ACK path permits positively missing
sources only with completed guarded ownership and fresh released custody.
Keeping duplication accepts the risk of future coverage fixes diverging;
changes to either coverage loop should be reviewed against both paths.

A private parameterized core could reduce that risk, but moving awaits and
eligibility predicates is itself a data-integrity change. No extraction is
recommended for this delivery. A later extraction would require a separately
approved concrete design and differential tests, including the matrix below.
No new helper, export, public signature or runtime behavior is introduced by
this assessment.

### Explicit policy dimensions at the landed fix

The following map is scoped to the two methods in `agent/src/recovery.ts` and
their runner callbacks, rather than every recovery operation.

| Dimension | Acknowledged-FINAL proof | Physical archive proof |
| --- | --- | --- |
| Candidate admission | Exactly one current guarded archive FINAL ACK before uploaded-manifest eligibility filtering. | Exactly one eligible current guarded uploaded coverage record; no ACK requirement. |
| Unresolved admission | Exactly one unresolved FINAL across the snapshot, current-generation guarded publication-settled, empty digest/roots and matching archive context. | No analogous disposition/cardinality restriction; relevant competing journals must pass coverage. |
| Ownership | Completed, exact generation, guard exactly true. | Completed/failed/cancelled, exact generation; guard true, false or absent allowed, defined non-boolean refused. |
| Competing coverage context | Canonical equality with selected archive required for relevant coverage records. | No per-source context-equality check; selected archive's resolved bare must still match its own context. |
| Competing FINAL source | Both now require the relevant unacknowledged FINAL's own source; non-FINAL aggregates use frozen original/current heads and roots. | Same rule after this correction. |
| Final eligibility | Rechecks observed archive health and uses fresh custody callback eligibility. | Own reservation-tail eligibility before removal; runner/Outbox budget and identity checks still apply. |
| Custody | Complete nonempty exact released/discarded set, at least one released hold, zero siblings; registered worker/features remain valid. | No additional acknowledged-FINAL custody wrapper. |
| Source callback | Verified positive absence allowed; present clones require strict physical inspection; extra source reread after custody await. | Attributed clones must be present; no custody await or corresponding extra reread. |
| Result and callers | `acknowledged_final_report_only` makes terminal/finalize callers return before publication-completion notification. | `existing_archive` keeps existing independently constrained notification behavior. |

Mechanism anchors: `retireAcknowledgedFinalReport` contains
`if (ackRecords.length !== 1) return`, the `unresolved` filter and
`ownership.inventory_guarded !== true`; `retireArchivedReport` contains
`["completed", "failed", "cancelled"]` and the optional-boolean guard.
The ACK-only source-context check is
`canonicalJson(source.coverageContext) !== canonicalJson(record.coverageContext)`.
Both head lists now use
`source.finalRequest && !source.finalAcknowledged ? [source.sourceSha] : []`.
`WorkerClient.withAcknowledgedFinalReportCustody` in `agent/src/client.ts`
owns the fresh observational custody callback.

### Possible private seam, not an approved extraction

A future seam could centralize later-head classification, checked ancestry,
relevant source/root iteration, frozen-head checks and journal/owed snapshot
rechecks inside `RecoveryCoordinator`. The two public wrappers would retain
their signatures and explicit candidate, ownership, context and final-eligibility
policies. Prefer a closed two-mode policy over arbitrary caller-supplied
predicates; do not expose a switch that disables the now-common pending-FINAL
source requirement. Whether the private method encloses the complete coordinator
lifecycle or only the locked coverage section needs separate design review.

Preserve C(skip busy) → S(refuse busy) → J → B → O and await every reservation,
inspection, custody and removal callback before its owning boundary releases.
The snapshot rechecks occur after source inspection; ACK custody follows them
under the same bare lock, then runner rechecks the source identity before Outbox
unlink. Moving that extra reread before the custody await would lose its purpose.
ACK final eligibility also rechecks observed archive expiry; the physical path's
existing eligibility policy must remain explicit.

Keep source-reader choice and the removal closure in
`Runner.retireRecoveryReport`. Its existing bindings are the only production
call sites of these two coordinator methods (exhaustive tracked enumeration
with `git grep` over `agent/src`). ACK is attempted first, and physical fallback
shares the same remaining budget. Preserve the one-second proof deadline capped
by the caller, cancellation propagation, cumulative metadata accounting and
settlement ownership for already-started operations. A private seam must not
return reusable proof authority or bypass exact installed-report identity.

Keeping the seam in `recovery.ts` needs no new import: it already depends on
client/Git contracts. Do not import runner or Outbox back into recovery or export
a proof token. Runner supplies those lifetime-sensitive callbacks and remains
the sole producer of the ephemeral authority result. The protected terminal
caller in `agent/src/runner.ts` and finalize retry caller in
`agent/src/worker.ts` must retain their ACK-only early returns and publication
binding/reservation non-detachment.

### Differential regression obligations for any future extraction

Parity means preserving each path's outcome, including intentional differences,
rather than forcing both paths to agree. Tests must invoke both coordinator
paths and the runner so fallback cannot conceal a changed policy.

- Completed/failed/cancelled ownership with guard true/false/absent/malformed,
  wrong generation and nonterminal status: ACK accepts only completed/true;
  physical accepts the three terminal statuses with boolean or absent guard.
- Zero/one/multiple eligible archives; zero/one/multiple archive ACKs before
  eligibility filtering; zero/one/multiple unresolved FINALs; settled versus
  archive/held dispositions and generation/evidence/digest/root mismatches:
  preserve the ACK admission restrictions without imposing them on physical.
- Selected bare/context mismatch refuses both; a competing coverage-context
  mismatch distinguishes the ACK policy from physical coverage.
- Covered and uncovered pending FINAL tips, acknowledged earlier settlement,
  later-only attribution and divergent non-FINAL aggregate tips: preserve the
  shared corrected source rule and the existing exclusions.
- Complete released custody, mixed released/discarded, discard-only, open
  exact/sibling, empty/incomplete/malformed sets and changed worker/features:
  preserve ACK eligibility without adding its custody gate to physical.
- Covered clean present sources, dirty/untracked/reset-soft content, positively
  missing paths, missing Git content in a present clone and unknown attribution:
  only positive missing may qualify through ACK; present content remains strict.
- Changed journals, owed contexts and source identities after inspection;
  ACK source change during custody; archive expiry during final eligibility;
  cancellation/deadline and queued execution/lock waiters: retain at the current
  boundaries while allowing already-started unlink to settle under reservation.
- Terminal/finalize exact identity, identical-byte ABA replacement and restart,
  pending companion terminal, post-unlink faults and publication bindings:
  preserve actual Outbox result authority and caller non-detachment.

Starting points for this future matrix are
`agent/test/acknowledged-final-source-coverage.test.ts`,
`agent/test/runner-acknowledged-final-restart.test.ts`, the new physical-history
test, and existing report-retirement safety, locks, identity and Git tests.
This is not a claim of complete existing differential coverage. In particular,
duplicate/disposition/context admission and discard-only custody need explicit
paired cases before extraction.
