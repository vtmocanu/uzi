# #2494 reproduction evidence

This report lives with the test fixtures; it is not part of the top-level embedded CLI documentation corpus.

M1 observation base: `ce36a79f` (includes #3058, `0c4c7287`). M1 changed tests only, plus this evidence report; its observations used unchanged product code. The M3 repair and its validation are recorded below. #3101 was still open when checked; no matching landing commit was present in this checkout.

The two historical incidents were **not reproduced in the bounded fixtures below**. This does not establish the cause of the historical snapshots or rule out an untested interleaving. The permanent-refusal reconciliation cases have independent failing regressions. The approved plan requires retaining that distinction.

## Incident controls

The real Runner, protected Outbox, authenticated on-disk journals and real GitCache are exercised in `agent/test/runner-issue-2494-m1.test.ts`. RPC answers and quiescence observations are test doubles; no live API or compose environment was exercised. These Runner source-boundary controls use the existing HELD_LINUX_ONLY guard because the production physical-source proof refuses non-Linux hosts. The coordinator-only protocol controls remain platform-independent. Recorded Runner results are Linux results; no non-Linux execution is claimed.

| Scenario | Observed result on unchanged product code |
| --- | --- |
| Outage, replay, restart; boot materialization; receipt / reason / neither ACK | All three pass. |
| Outage, replay, restart; spaced live materialization; receipt / reason / neither ACK | All three pass. |
| Completed ci_fix; heartbeat after execution exits / attempted before execution exits | Both pass; one original upload and one archive FINAL ACK after tail release. |
| Completed ci_fix; admitted maintenance reservation | Raw Runner execution predicate skips while reservation holds a tail; subsequent spaced pass settles after release. Included in the preceding two tests. |
| Cancelled gated issue; archive fallback / held publication | Both pass through Runner.resumeLiveRecoveries, without resumePending. |
| Cancelled gated issue; exact authority becomes unknown after freeze | Passes its retention control: no FINAL ACK, original exact-bound record retained. |
| Completed ci_fix; report retirement owns C before reserving a tail | Focused candidate control passes; qualifications below. |

The outage fixture commits work, drops terminal/capture RPC transport, preserves the signed completed body, spills an ambiguously acknowledged feed frame, replays the fenced feed and completion in the original process, and restarts before any recovery sweep. Fresh GitCache/Outbox/Runner objects then exercise boot or live recovery and repeated terminal resolution.

The journaled completed body carries the exact `completion_final_head`. All six variants assert immutable replay. Direct instrumentation records that `prepareCompletionFallback` ran only while API transport was down. That is distinct from the later archive reconstruction.

The receipt variant returns an authenticated completed-publication receipt and retires its terminal before a sweep, with zero archive uploads. The reason variant returns `completed_publication_reason=mr_missing`; the mixed-version variant returns neither receipt nor reason. Those two retain the protected terminal after replay, reconstruct one archive, and receive one archive FINAL ACK. Reconstructed records assert absent `exactHoldBinding` and `heldPolicy`.

Discovery returns an exact positive generation/kind/default identity and an owed candidate for the completing head. During outage the hold/ownership reads fail; after transport recovery they succeed. Neither boot nor live reconstruction rejects this context. No missing reconstruction authority or skipping predicate for the historical outage record has been established. Its discovery context was absent from the historical snapshot. These positive controls therefore do not authorize an incident-specific predicate repair or a new discovery producer.

The ci_fix fixture first observes `inventory_source_not_quiescent:execution_tail_present` on its uploaded self-contained archive. Actual execute-tail and maintenance-reservation states are recorded separately. Once those tails clear, heartbeat recovery ACKs the same capture/source/coverage identity with one total upload. The cancelled controls distinguish the existing held-first route, typed held refusal with archive fallback, and unknown exact custody after a valid freeze. Ownership and exact-hold observations both report cancelled after the terminal ACK. The tests assert no second publication/FINAL; unknown custody preserves the original capture identity, exact binding, unacknowledged state and installed protected terminal. Unknown custody is not treated as an ACK; archive upload itself is not categorically forbidden.

## Contention and timing qualifications

The report-before-reservation variant obtains the real authenticated `expectedIdentity` from Outbox.readTerminalJournalForRetirement and calls real Runner.retireRecoveryReport. A barrier before the bound withReportSourceReservation call holds C while no execution tail or inventory reservation exists. The live candidate rotates without FINAL; the exact terminal stays installed while C is held. The barrier releases in finally, the report proof completes through existing authority, and the original archive ACKs on the next +30-second live pass without another upload.

During this busy candidate pass only, the fake hold list is empty so discovery defers materialization. With unrestricted discovery, materializeBootInventory can enter freezeInventory's wait-mode C acquisition before the candidate skip loop. Awaiting that live pass while manually withholding the report barrier created a test deadlock and timed out. That artificial barrier timeout is **not a reproduced product stall**. The focused control proves candidate C(skip) behavior with discovery deferred; it does not prove unrestricted discovery skips C or reproduce the historical concurrent-heartbeat ordering.

A separate foreground-C control records that a busy generation rotates without entering its status read. Maintenance and report controls preserve the real source boundary; they add no FINAL or deletion exemption.

Existing backlog controls remain green: production spacing reaches the unchanged archive at 150 seconds, stub spacing at 5 seconds. They remain timing controls for #3054, not a new product regression or justification for faster product pacing. The original incident's backlog and precise contention order remain unknown. No separate scheduler, tail-lifetime, or discovery change is established by M1.

## Permanent-refusal regression results

`agent/test/recovery-inventory.test.ts` adds 28 focused cases: 16 controls pass and 12 regression assertions fail on unchanged product code. Two pre-existing backlog controls also pass. These are intentional intermediate reds for the later approved repair milestone, not a claim of a passing suite.

| Group | Red assertions | Already-correct controls |
| --- | --- | --- |
| R2987-05(a) | 403 and 409 saved binding-less FINALs resend at ordinary cadence; two diagnostics lack bounded structured refusal/capture fields; refused peer does not wait its own delay. Five reds. | Network, 429 and 503 retries follow bounded backoff. A new healthy peer can ACK. |
| R2987-05(b) | Fresh bound discard/different receipt with unhealthy or throwing capture status never persists exact closure. Four reds. Healthy status persists closure and suppresses resend, but repeats status/coverage work. Two cost reds. | Matching receipts recover the actual ACK for all health variants. Open/unknown/wrong-binding controls preserve existing behavior; unknown/mismatch does not persist closure. |
| R2987-05(c) | Authenticated persisted discarded closure still repeats expensive coverage/status work. One red. | No resend, replacement FINAL, or fabricated ACK. |

At elapsed milliseconds [0, 30000, 60000, 90000, 180000, 420000, 900000, 1800000, 2700000], binding-less 403/409 resend counts are [1,2,3,4,5,6,7,8,9], rather than the capped-backoff expectation [1,1,2,2,3,4,5,6,7]. The saved FINAL, archive bytes and one-upload identity remain protected. Binding-less suppression remains outside this issue, assigned to #3105.

The saved request in the differing-receipt cases is archive(capture/source/digest); the server observation is held_publication(publication ID/source/digest) with matching held_publication release evidence. Open controls contain neither release evidence nor a receipt, preserving the real client validation contract. The differing disposition is explicit, not inferred digest recomputation. No holdSettlement is preseeded in the fresh-read matrix. Healthy-bound cases already suppress FINAL resend; their red is repeated work. The persisted-observation case begins with a MAC-authenticated discard observation and still increments status counts on later passes.

Relevant unchanged base mechanisms: recovery.ts finalizeInventoryWithinBoundary checks capture status before heldOperationState; unhealthy or failed reads enter reconcileInventory first. Differing reconciliation receipts are rejected without persisting exact closure. Persisted observations are reached after avoidable source/status checks. The 403/409 catch emits raw error text and classifies the result without per-record backoff. These observations justify the approved M3 investigation/repair, not expanded suppression or cleanup authority.

## Executed validation and handoff

All logs are run-local under `.uzi/scratch/`; no logs or scratch data are tracked. Log results were read after execution. New fixture changes were checked in focused selections rather than repeatedly running the full suite.

| Check | Recorded result | Log |
| --- | --- | --- |
| Final outage selection, six cases | EXIT=0, six pass, 27.362s | gate-log.m1bDq7 |
| Final cancellation controls with cancelled ownership and retention assertions | EXIT=0, three pass, 12.106s | gate-log.vcEd8Z |
| ci_fix after/during-exit controls with reservation barriers | Both pass in a larger run; that run exited 1 for the subsequently corrected cancellation fixture | gate-log.KrOX1D |
| Final report-before-reservation control | EXIT=0, one pass, 7.332s | gate-log.9fp64P |
| Final inventory selection plus backlog controls, valid proof shapes and bounded diagnostic assertions | EXIT=1, 18 pass / 12 expected regression failures, no cancellations, 6.410s | gate-log.T5YwvL |
| npm --prefix agent run lint; npm --prefix agent run typecheck; git diff --check HEAD | EXIT=0; initial test typing errors corrected | gate-log.JcBOlC |

Focused Node invocations ran from `agent/`, with `TMPDIR` pointing to the worktree's `.uzi/scratch/`, using:

```sh
node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 --test-name-pattern='<selection>' <test-file>
```

Selections: `M1 completion outage restart before sweep`; `M1 cancelled gated issue LIVE`; `M1 completed ci_fix uploaded archive live report-before-reservation`; and `M1 R2987|M1 C\(skip\)|handoff receipt pacing`. The initial ci_fix evidence used the whole runner fixture file before the added report variant. All cases are bounded by injected clocks/barriers and existing per-test timeout; no production timer changes were made.

Review of 373bd30d identified invalid differing/open proof fixture shapes and top-level documentation placement; both were corrected. Reviewer/auditor assertion notes were addressed with hostile/oversized diagnostic payload checks across every emitted warning, plus explicit unknown-custody record/report retention assertions. A lead review also corrected the inherited failed ownership response to cancelled. The focused selections in the table were rerun after those test fixes. The tester mentioned the absence of a coverage-instrumentation gate; adding instrumentation/dependencies is deferred under the approved focused-evidence scope, not presented as executed coverage measurement.

No full gate, compose phase, LiveDB, hosted acceptance, CI run, or full-suite before/after runtime measurement is claimed in M1. Final gates and delivery validation remain M4 obligations. Test type narrowing and the non-Linux guard were added after the recorded Linux controls; their Linux assertion bodies are unchanged. Lint/typecheck were repeated after the guard change. The reused guard evaluates false on the Linux host, so it does not turn these recorded Linux cases into skips. M2 applicability must preserve the independent non-reproduction decisions; M3 must turn the named refusal reds green without weakening immutable FINAL, authority, source-boundary or cleanup constraints.

## M2 disposition

No incident-specific product repair is justified by M1's bounded non-reproductions. M2's conditional repair deliverables remain unclaimed; no pending-publication state, discovery producer, candidate-selection change, scheduler or source-boundary exemption was introduced. The original outage discovery authority and historical heartbeat interleaving remain unproved. This disposition does not cancel the independently reproduced M3 defects or claim to close the historical incidents. #3101 remained open at the pre-repair recheck, with no matching landing commit in this branch.

## M3 refusal repair and compatibility evidence

The automatic saved-FINAL path now reads validated exact custody before capture-health/coverage work. Authenticated persisted nonmatching closure short-circuits that work. A fresh bound observation uses the existing exact-hold client validation; matching receipts still replay the original FINAL and recover an actual ACK. Closure alone persists `inventory_hold_settled`, never `finalAcknowledged`, and grants no source or report deletion authority.

Binding-less and unresolved refusals keep their immutable FINAL indefinitely eligible with capped per-record process-local backoff. Restart permits one fresh attempt and then re-establishes that delay. The retry identity includes authenticated capture identity, binding and the saved request; disappearance or identity change resets the process-local entry. Credential quarantine and cancellation budgets keep their existing authority. One diagnostic per refusal transition names the run/generation/capture and bound hold when available, structured protocol reason/status, refusing predicate and verified disposition or unknown. Raw response bodies are excluded. The named retryable retained reasons are `final_retry_refused`, `final_retry_unknown` and `inventory_hold_settled`; the last denotes retained sources under a nonmatching exact closure, not successful preservation.

All twelve unchanged M1 regression assertions pass with the repair (included in the focused 90-case run and the 404-case inventory file run below). The baseline red evidence remains `gate-log.T5YwvL` at the M1 product base. The compatibility assertions now permit only the intended `reason` addition to the prior full record, and the wrong-ACK control asserts immediate deferral before advancing its injected clock to the retry deadline. Neither change relaxes immutable FINAL, no-ACK or no-replacement assertions.

| Check | Recorded result | Log |
| --- | --- | --- |
| Focused M1/M3 refusal, settlement and cancellation controls | EXIT=0, 90 pass | gate-log.sZD40E |
| Legacy scheduling and credential controls | EXIT=0, nine pass | gate-log.nkMQOs |
| Compatibility failures before expectation corrections | EXIT=1, 20 fail; reason-only delta or newly paced immediate retry | gate-log.YVVvpZ |
| Same compatibility selection after explicit reason/pacing assertions | EXIT=0, 20 pass, 2.992s | gate-log.JDdExK |
| Entire recovery-inventory test file | EXIT=0, 404 pass, no skips/cancellations, 27.920s | gate-log.FJnSfa |
| Real Runner/outbox/GitCache incident fixture file after repair | EXIT=0, 12 pass | gate-log.RWengX |
| Agent lint, typecheck and git diff --check HEAD | EXIT=0 | gate-log.8iE9G2 |
| Dispatch-base and current authenticated reader control | EXIT=0 | gate-log.QIKoIW |
| Bounded E2E diagnostic reason projections and unknown-text exclusion | EXIT=0, four pass, 0.571s | gate-log.vqGt3s |

The old-reader control extracted `ce36a79f` under scratch, used its actual RecoveryCoordinator alongside the candidate reader, and authenticated a current-writer record carrying the unchanged `holdSettlement` and `reason` representation. Both readers preserve the same saved FINAL, report cleanup as pending and leave fixture bundle bytes intact. This is a parser/MAC and cleanup-authority control, not a claim that old workers inherit new pacing. No record field, sidecar, API or backup schema was added. Generic signing and parser representation are unchanged; backup stopped-marker validation concerns another record type.

The required literal audit (`gate-log.nSHiBs`) found no stale expectations for the changed FINAL log messages or new retained reasons in e2e/phases, lib.sh, lib.test.sh or forge-fake. The separately audited bounded settlement diagnostic has an explicit reason allowlist, updated and tested for the three new names without admitting arbitrary text. Full E2E library/fake checks and integration gates remain M4 obligations. Binding-less closed-custody suppression remains deferred to #3105 as approved; unknown authority is never relabelled settled.

Independent reviewer/tester/auditor reviewed `aa516e7d..db96740d`. The reviewer identified a diagnostic transition-cache defect: a stable FINAL refusal plus a reconciliation failure alternated the single cache entry and repeated both warnings on each deadline. Although labelled nonblocking, this violated the stated diagnostic behavior and was repaired. The cache now remembers the latest transition separately for each of the four fixed retained predicates, still pruned by authenticated identity and record disappearance. A new regression was observed red before this correction (`gate-log.pUBH84`, EXIT=1, four warnings where two were expected), then green with seven adjacent diagnostic/reset/unknown-authority controls (`gate-log.ajAAuW`, EXIT=0, eight pass). It also checks genuine protocol transitions, bounded hostile-text exclusion, exact FINAL identity, unchanged archive bytes, no new upload, no ACK and no deletion. Lint (`gate-log.vozJPo`), typecheck (`gate-log.4iy5em`) and diff check (`gate-log.pteJeE`) each exited zero after the fix.

The tester's optional request for per-run revision/runtime/invocation metadata is deferred; the evidence report links revisions, commands and logs, but older individual logs do not contain every item inline. No missing CI/runtime evidence is claimed as measured. The auditor recorded a pre-existing diagnostic directory-enumeration issue separately for triage; this repair does not widen into that unrelated change.

## M4 delivery validation

Independent reviewer, tester and auditor reviewed the M3 unit `aa516e7d..db96740d`. The diagnostic correction `db96740d..ffb1c29c` received clean scoped reviews from all three, including a bounded replacement tester review after two cancelled attempts returned no verdict. No cancelled review is counted as approval. The final tester independently checked the red/green assertions, genuine protocol transitions and immutable-request/source-retention controls without duplicating execution. The source work is committed locally in `db96740d` and `ffb1c29c`; M1 fixture/evidence commits are `373bd30d`, `8b811446` and `aa516e7d`.

A serial local affected-file comparison used Node v24.20.0 and the same provisioned dependencies, hermetic loader, 120-second per-test limit and worktree scratch TMPDIR. The baseline ran from a `ce36a79f` archive while the command's working directory remained the real agent checkout; the invocation did not change into the export or run Git commands there. The candidate was `ffb1c29c`. No dependency installation or full baseline gate was run.

| Affected-file measurement | Result | Local duration | Log |
| --- | --- | --- | --- |
| Dispatch-base recovery-inventory file | EXIT=0, 368 pass, no skips | 29.823s | gate-log.ZTV7EQ |
| Candidate recovery-inventory file | EXIT=0, 405 pass, no skips | 39.591s | gate-log.wMCoLn |
| New real Runner incident-control file, candidate product | EXIT=0, 12 pass, no skips | 46.732s | gate-log.RWengX |

The inventory file adds 37 cases and 9.768 seconds in this single serial comparison. The Runner file is new, so its local 46.732-second run is additional test work rather than a measured CI wall-time increase. Existing global live-pass spacing and timeout settings, and CI jobs, were unchanged. These observations include ordinary local execution/cache effects; they do not isolate cache cost. CI queue time, reruns, shard placement, step duration and critical-path impact remain unmeasured until publication. No repeated samples or hosted timing claim is made.

The delivery metadata adds a UUID-v4 changelog fragment describing only the demonstrated refusal repair. The candidate must pass the serial `task gate:agent` and `task gate:repo` checks after metadata review. The repo gate includes `test:e2e-lib` and `test:forge-fake`, so their nested results supply the required harness validation without a duplicate invocation. Actual gate results are reported in the delivery record; this paragraph is not a claim that an unrun gate passed.

CI is required after worker publication and is not a locally observed result. The maintainer still owns focused/full compose verification. Unrun compose coverage includes phases 05/06 in both forge lanes, 52, 60, 70 handoff and 72, plus the #3054 phase-boundary check when present. No API file, workflow, migration, discovery producer, scheduler, tail-lifetime rule or deletion authority was changed. The issue remains open because the historical outage and heartbeat causes are unproved; M2's conditional incident repairs are not claimed. #3105 retains ownership of binding-less closed-custody suppression.

Deferred review notes: tester at `373bd30d`, one optional coverage-instrumentation gap; tester at `db96740d`, one optional inline execution-metadata improvement; auditor at `db96740d`, one pre-existing off-task directory-enumeration finding recorded separately for triage. None is presented as executed validation or silently folded into this repair.

### Advisory repair after delivery review

The three advisory findings were independently checked against `ffa09d30`. The lead ran nine new focused controls against an export of that unchanged product with the new tests (`gate-log.kIJ9g9`): five failed and four passed. The failures demonstrate local inactive/feature/coverage deferrals incorrectly creating a retry reason and delay, repeated writes of an unchanged settled reason, and a pacing write masking the original finalization exception. The four passing controls require failed, malformed, available-but-unreconciled and unavailable reconciliation to remain paced.

The repair restricts pacing to explicit failure outcomes after a normal finalization return. Local void preconditions retain their existing evidence and do not create or increase a retry delay. Saved-FINAL reconciliation uncertainty now has an explicit retry outcome; existing wire-refusal classification still takes precedence when a FINAL request actually failed. Unchanged retention reasons no longer rewrite the signed record. No source proof, immutable request, ACK or cleanup authority changes. An earlier real ACK-loss failure still warrants its existing delay even if a later feature check defers work; the prior Runner cleanup control covers that distinct case.

The final eleven-case selection also covers an already-due retry encountering a local gate and budget exhaustion inside finalization. The lead observed seven failures and four passing controls against unchanged `ffa09d30` (`gate-log.iv9e8L`, 3.754s), then eleven passes with the repair (`gate-log.nt9HJi`, 3.432s). Both entire affected inventory and Runner cleanup files passed, 439 cases with no skips (`gate-log.Vt3DNt`, 57.462s); lint and typecheck passed (`gate-log.b7eiNG`) after correcting a fixture-only interface type annotation. The added tests use injected clocks and no real-time waits. The initial unchanged-product nine-case selection took 4.432 seconds locally; these are not CI runtime measurements. Revision-linked repaired checks, independent scoped review and repeated serial integration gates are recorded in the delivery evidence. Historical incident non-reproduction and all previously recorded scope limits remain unchanged.

### First full gate and narrowed triage

The first `task gate:agent` at `ba946116` failed: task exit 201 (wrapper exit 1), 4,435 seconds elapsed, log `gate-log.z09lOf`. Dependency validation, lint, dead-code and typecheck completed. Unit results were 15,750 tests: 15,698 pass, 37 fail, 15 skipped, zero cancelled; unit duration was 4,414.370 seconds. The Codex M4 stage did not run after the unit failure. This is a recorded failed check, not an unrelated-failure claim based on inspection alone.

The four-case narrowed selection reproduced one pin-cleanup failure and three Runner cleanup failures on the candidate (`gate-log.7qNVz1`); the same selection on an isolated dispatch-base export reproduced the pin failure but passed all three Runner controls (`gate-log.nFR3Po`). The Runner tests expected an immediate replay after ACK loss and feature restoration. They now use the harness's existing injected `now` seam: the immediate attempt must retain one request with no ACK; after advancing 60 seconds, the exact saved request must receive its real ACK. The existing unreadable/uncovered sibling and terminal/source retirement assertions remain intact. The selected three pass (`gate-log.EV68LK`, 4.027s), as does the entire 23-case Runner cleanup file (`gate-log.UM4i8e`, 8.803s). Scoped lint, typecheck and diff checks exited zero (`MEI5Na`, `OluTG0`, `XyeHDT`). No production code or retry timing was changed by this test correction.

The inherited worker umask was `0002`. The legacy pin fixtures create directories with default modes; the existing physical-source boundary refuses metadata writable by the shared runner group. A controlled serial comparison ran the entire unchanged pin-cleanup file with only the creation mask changed:

| Revision and mask | Result | Log |
| --- | --- | --- |
| Dispatch base, `0002` | EXIT=1, 13 pass / 34 fail | gate-log.b6YbZW |
| Dispatch base, `0022` | EXIT=0, 47 pass | gate-log.XF2EKM |
| Candidate product, `0022` | EXIT=0, 47 pass | gate-log.BOIBvX |

All 34 base failure names match the full gate's pin failures. This establishes an environment-sensitive baseline fixture problem, not a new cleanup-authority defect. It is recorded separately for triage; this issue does not edit those fixtures or weaken the production `unsafe_metadata` refusal. The next serial gate run uses `umask 0022` so test-created private metadata is not shared-runner writable. The failed gate remains part of the evidence. A rerun is justified by the three corrected controls and the demonstrated creation-mask change, after their scoped review is clean. Repo gate and final agent-gate results remain delivery-record results, not assumed passes here.
