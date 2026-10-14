# Worker memory foundation reconciliation — issue #2396

## Resumed reconciliation — 2026-10-14, validation pending

This run preserves published floor `0bc24b959f85d8ef5adcf322f4c3240a6bf2eec3` and merges default `ebbe088d01959ec74e0826a0887af3e3e49ce3b5` without rewriting either history. The earlier sections below remain historical evidence for their named revisions, not acceptance of this merge.

Conflict resolutions combine the captured memory capability with upstream summary settlement, Read scheduling, retained-source admission and shutdown. Worker construction keeps upstream's gate/channel arguments and adds the memory runtime afterward; the lifecycle fixture now verifies that the memory-enabled worker starts and joins the retained-source channel once. Runner failure/release paths keep both unresolved-completion and memory-custody protections. The credential-free settlement helper retains the memory cancellation purpose while settling upstream plan summaries under the same remaining deadline. Test doubles supply the bounded supervisor observation interface.

The implementation delegation ended without a report; retained edits were inspected by the lead. A completed SQL generation run used `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate` (log `gate-log.ZFFLat`, EXIT=0); no additional generated drift resulted. A combined retained test log (`gate-log.MiFJfB`, EXIT=1) failed the late native park successor-admission case; its isolated rerun (`gate-log.2hn1fy`, EXIT=0) does not establish a baseline failure or close the combined-run concern. Current committed-range review and integration checks remain pending.

A read-only current-worker sample returned `available: false, reason: absent`; no load, usable headroom or sizing recommendation follows. The provisioned agent SDK/knip and web knip still match the pre-merge manifests, not upstream's updated versions. They were not reinstalled. Tests in that dependency environment cannot establish final matched-lockfile acceptance. M2 required real-process lane enrollment, M3 documentation/surfaces and M4 real-cgroup/CI calibration remain unfinished. Production enablement stays disabled.


This record supersedes the current-state statements in `worker-memory-validation.md`; that earlier record remains historical evidence for its named revisions and commands. M1 implementation and required local validation are **complete** at the fixture-isolation candidate below. M2–M5 have not been implemented in this resumed run. Real adapter integration and private-cgroup calibration remain unverified; production enablement remains disabled.

## Source and contract seam

Published floor `ee0d8077e82013413e820e0a2d8a843c8ae6d8de` was preserved. Merge `2abb091f55a3ca133f009d28644b66c18010bc10` integrates default `cbcf12b7f9d70b304671fc3dbce4ea2bf242d288` without rewriting history. Fixture repair `f8003a14f58dca1f7f1b3bfefcc3190fc265b7dc` follows it.

The issue migration is now `00334_worker_memory_interventions.sql`; upstream migrations through 00333 remain intact. sqlc output was regenerated. Current plan- and code-checker parent eligibility and owner-resume locks are reconciled with default. Claim decoding carries the persisted memory episode, intervention count and policy. Separate typed `memory_hold` detail/summary projections expose the memory allowance without substituting requeue counters; unknown policy yields unknown maximum/remaining values. API JSON fixtures and the web contract type match that projection.

The merged default contains #2512's bounded retained-source recovery mechanism. This is source evidence, not deployed-runtime or incident-causation evidence. The repository memory reader reported unavailable/absent in this worker; no current-worker load or headroom recommendation follows.

## Executed checks

Logs below are run-local `.uzi/scratch/` artifacts, not portable published artifacts. Commands ran in the foreground. Agent and web dependencies were provisioned by the worker and were not reinstalled. Canonical recipes kept their existing flags, settings and concurrency. `UZI_TEST_DATABASE_URL` was unset for `task gate:api`.

| Revision | Command / check | Observed result | Log |
|---|---|---|---|
| merge candidate | `task check:migration-numbering check:migration-additive typecheck:agent` | PASS; highest migration 334, next 335 | `gate-log.HkJNFA` |
| `2abb091f` | `task gate:api` | PASS, including sqlc drift; ordinary LiveDB skips do not prove live contracts | `gate-log.simwK0` |
| merge candidate | Go API contract fixture marshal/strict-decode tests | PASS | `gate-log.mNowAI` |
| `f8003a14` | `task test:codex-supervisor typecheck:web` | PASS | `gate-log.kTlgM7` |
| `f8003a14` export | Focused memory LiveDB diagnostic, workersvc `^TestMemory.*LiveDB$` | PASS: 22 top-level tests, zero skips; package 23.877s | `gate-log.ANfnmy` |
| `f8003a14` export | Named registration timeout diagnostic | PASS: zero skips, package 7.698s | `gate-log.d7irjx` |
| default export | Same named registration diagnostic | PASS: zero skips, package 8.623s | `gate-log.yXDdEQ` |
| default export | Unchanged `./e2e/run-store-it.sh` | PASS: 2,779 passed, zero skips; elapsed 1,767s, workersvc 756.007s | `gate-log.3CAM8S` |
| `f8003a14` | Unchanged `./e2e/run-store-it.sh` | FAIL: 2,614 top-level passes, zero skips before workersvc's 900s package deadline; elapsed 1,759s | `gate-log.KIGQrj` |
| `26f92823` export | Focused memory and pending-health LiveDB diagnostic | PASS: 27 tests, zero skips, including the new isolation assertion; elapsed 347s | `gate-log.MMWnkR` |
| `26f92823` | `task gate:api`, database URL unset | PASS; ordinary LiveDB skips are expected | `gate-log.TloOdJ` |
| `26f92823` | Unchanged `./e2e/run-store-it.sh` | PASS: 2,808 tests, zero skips; elapsed 1,702s, workersvc 717.050s | `gate-log.of370n` |

The live harness owns its PostgreSQL lifecycle and database URL. `TMPDIR` pointed to this worktree's scratch directory solely to contain temporary artifacts. Focused diagnostic exports used scratch-only harness copies with a narrowed test selector/package list; they were not replacements for the unchanged required sweep. Each export was removed after its run.

The first unchanged candidate sweep (`gate-log.Frtggw`) failed two owned fixtures and later reached the workersvc aggregate deadline. Task-shaped checker fixtures needed a branch; extension fixtures needed the current maximum wall budget input. Both were repaired. The rerun had no preceding named assertion failure and timed out during `TestReconciliationForcedParkAndRetryLiveDB`, with that test only one second old. It passed store, handler, forgesvc and schedsvc; recovery and poller were not reached. The timeout cannot be dismissed as preexisting: the unchanged default sweep passed, while the isolated registration comparison did not reproduce the earlier timeout.

Named required cases positively executed in the repaired unchanged candidate sweep include `TestMemoryCodeCheckerAuthorityLiveDB`, `TestMemoryHoldOwnerOnlyReleaseLiveDB`, `TestMemoryHoldNonOwnerGuardsLiveDB`, `TestMemoryHoldResumeClaimIncarnationLiveDB`, `TestMemoryOrdinaryRedispatchPreservesAllowanceLiveDB`, `TestMemoryReservationConcurrentLastSlotLiveDB`, `TestMemoryReservationOwnerResumeRaceLiveDB`, `TestMemoryReservationNonceRotationRaceLiveDB`, `TestMemoryReservationHistoricalRetryAfterNewClaimLiveDB`, `TestMemoryReservationNoSignalNoRefundLiveDB` and `TestMemoryOutcomeAndFeedDeduplicationLiveDB`. None skipped. This executes the contract seam, but does not turn the failed full sweep into a pass or prove runner preservation.

## Fixture-isolation repair and review evidence

Reviewer, tester and auditor reviewed immutable saved/default-to-merge ranges at `2abb091f`. The mandatory scratch-fixture finding was repaired at `f8003a14`; a scoped reviewer/tester/auditor wave reported clean closure. The supervisor fixtures now create their scratch parent independently. The unrelated-event memory-hold matrix now seeds a nonzero episode/count and policy so reset regressions are observable. Focused live execution confirmed the repaired matrix.

Auditor at `f8003a14` left one optional, unverified transaction-clock note: a lock wait might cross a deadline while SQL uses transaction-start `now()`. No demonstrated violation or graded security finding was established; existing default eligibility uses the same clock convention. This note remains deferred, rather than represented as proven safe.

The final unchanged candidate sweep passed after fixture isolation; the earlier timeouts remain recorded failures, not reclassified baseline defects. This establishes passing validation on the final M1 code candidate, not exact causation for all observed timing differences. Full final-component gates and all subsequent implementation/calibration proofs remain mandatory later work. Local calibration duration and bounded worst-case evidence belong to the worker; post-publication before/after CI step and dependency-path timing remains pending lander-owned acceptance before merge.


## Final M1 candidate and scope

Fixture-isolation commit `26f92823b4f3e9d227372f4dee4ce2989c07f4e4` removes only fresh fixture-owner data before pool closure. It registers reassigned owners, removes cross-check attempts before run cascades, and explicitly handles custody hold bindings. The new `TestMemoryFixtureIsolationLiveDB` exercises issue/chat/plan/code and foreign-owner cases through the real health query, verifies absence after cleanup and retains a sibling fixture as a control. The handler's resumed run is also cleaned up by its exact fixture owner. No production policy, harness package list, deadline, test flag or concurrency setting changed.

Reviewer, tester and auditor found no blocking introduced defect in `90ed4d8a..26f92823`. Their no-database checks proved compilation/skip behavior only; the lead's focused and full live executions supply database proof. Tester and auditor each noted that the discard UPDATE lacks `release_evidence='owner_discard'` and can be suppressed for guarded holds. The following scoped DELETE still removes the current capture-free fixture holds, and live checks passed. This shared optional note remains deferred; fixtures that add restrictive capture/publication references will need corresponding cleanup. Tester's generic coverage-threshold and pre-commit-hook suggestions remain optional deferred tooling work.

The final full sweep positively executed all seven packages: store 398.323s, handler 331.510s, forgesvc 12.771s, schedsvc 9.959s, workersvc 717.050s, recovery 24.232s and poller 3.137s. It included the named reservation, owner-hold and checker cases above, plus `TestMemoryCustodyV1ReleaseRunBeforeHoldLiveDB`, `TestMemoryCustodyWorkerFinalReleaseRefusedLiveDB` and `TestMemoryCustodyLegacyWorkerReleaseRefusedLiveDB`. Required-case skips were zero. Source-only deployment evidence and unverified M2–M5 claims remain unchanged.
