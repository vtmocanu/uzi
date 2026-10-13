# Worker memory foundation reconciliation — issue #2396

This record supersedes the current-state statements in `worker-memory-validation.md`; that earlier record remains historical evidence for its named revisions and commands. M1 validation remains **incomplete**. M2–M5 have not been implemented in this resumed run. Real adapter integration and private-cgroup calibration remain unverified; production enablement remains disabled.

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

The live harness owns its PostgreSQL lifecycle and database URL. `TMPDIR` pointed to this worktree's scratch directory solely to contain temporary artifacts. Focused diagnostic exports used scratch-only harness copies with a narrowed test selector/package list; they were not replacements for the unchanged required sweep. Each export was removed after its run.

The first unchanged candidate sweep (`gate-log.Frtggw`) failed two owned fixtures and later reached the workersvc aggregate deadline. Task-shaped checker fixtures needed a branch; extension fixtures needed the current maximum wall budget input. Both were repaired. The rerun had no preceding named assertion failure and timed out during `TestReconciliationForcedParkAndRetryLiveDB`, with that test only one second old. It passed store, handler, forgesvc and schedsvc; recovery and poller were not reached. The timeout cannot be dismissed as preexisting: the unchanged default sweep passed, while the isolated registration comparison did not reproduce the earlier timeout.

Named required cases positively executed in the repaired unchanged candidate sweep include `TestMemoryCodeCheckerAuthorityLiveDB`, `TestMemoryHoldOwnerOnlyReleaseLiveDB`, `TestMemoryHoldNonOwnerGuardsLiveDB`, `TestMemoryHoldResumeClaimIncarnationLiveDB`, `TestMemoryOrdinaryRedispatchPreservesAllowanceLiveDB`, `TestMemoryReservationConcurrentLastSlotLiveDB`, `TestMemoryReservationOwnerResumeRaceLiveDB`, `TestMemoryReservationNonceRotationRaceLiveDB`, `TestMemoryReservationHistoricalRetryAfterNewClaimLiveDB`, `TestMemoryReservationNoSignalNoRefundLiveDB` and `TestMemoryOutcomeAndFeedDeduplicationLiveDB`. None skipped. This executes the contract seam, but does not turn the failed full sweep into a pass or prove runner preservation.

## Review and unresolved evidence

Reviewer, tester and auditor reviewed immutable saved/default-to-merge ranges at `2abb091f`. The mandatory scratch-fixture finding was repaired at `f8003a14`; a scoped reviewer/tester/auditor wave reported clean closure. The supervisor fixtures now create their scratch parent independently. The unrelated-event memory-hold matrix now seeds a nonzero episode/count and policy so reset regressions are observable. Focused live execution confirmed the repaired matrix.

Auditor at `f8003a14` left one optional, unverified transaction-clock note: a lock wait might cross a deadline while SQL uses transaction-start `now()`. No demonstrated violation or graded security finding was established; existing default eligibility uses the same clock convention. This note remains deferred, rather than represented as proven safe.

The cumulative candidate workersvc timeout remains unresolved and blocks M1 completion. Full component gates and all subsequent implementation/calibration proofs remain mandatory later work. Local calibration duration and bounded worst-case evidence belong to the worker; post-publication before/after CI step and dependency-path timing remains pending lander-owned acceptance before merge.
