# ADR-2943: physical report retirement covers competing pending FINAL sources

**Status**: Implemented; shared-core evaluation pending
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

## Shared proof core

Evaluation follows resolution of the coverage regression. Extraction is not
authorized by this decision and no core is extracted in this change.
