# Worker memory foundation reconciliation — issue #2396

## Current integration — 2026-10-15, M1 complete under approved deviations

This section supersedes the current-state language in the older dated sections below; those sections remain historical evidence at their named revisions. M1 implementation, review and required local checks are **complete** on tested source `164ef8b86be831489242315c762f8395f71dc20e`, under the human-approved merge-target and LiveDB deviations below. The subsequent evidence-only edit changes no gate inputs. M4, M5a and M5b are not complete.

### Integrated candidate and approved deviation

Published floor `a7f721c0fba78725a70563979b363bccc9a296a9` remains an ancestor. Merge `40eed30ecda8a7f00c845234d28e9baf9b7596f5` integrates worker-provisioned default `4103fad73896f103bf7cebd743f009b0b6bde671`, also an ancestor. The human explicitly approved this target because the worker refreshes default refs only at claim/finalize and could not supply `f8a3338d` mid-run. This branch **lacks PR #3039**; the lander must merge current main including it before landing. No network Git or history rewrite was used. Relative to merged default, `.github/workflows/ci.yml`, `e2e/run-store-it.sh` and `e2e/run-store-it.test.sh` are unchanged.

Offline tool renumbering in `fa3b99d4b2bd451a3742777dd09572f0cd0f008c` moves only the memory migration above default's 00338 to **00339_worker_memory_interventions.sql**. SQL source conflicts were resolved before sqlc v1.31.1 regeneration; generated query/model conflicts were not hand-merged. Current rollback wording names 00339. References to 00334 in older dated evidence remain historical. The lander must recheck numbering against landing main.

The integrated command seam retains runtime/tool identity, callback origin, modelCommand and long_running, captured memory context and the effective long wall bound. Pressure/abort wins a ready deadline while disposal/outcome joins are pending; root-only long-command restrictions remain. Heartbeat feature shrink revokes cancellation authority immediately; growth alone cannot seed or resurrect registration authority. Already-started captured feedback, preservation intent and sink receipts remain owned by the original flight after revocation. Production exclusion relies on broker enrollment and exact supervisor/descendant identity validation, not an assumed production caller of protect(). Physical command-UID execution is still M5a work.

The new LiveDB admission parity case executes guarded/legacy memory holds with available archives and failed latest captures, checking canonical count, membership/reason and attention projections. Fixture-only repair `eced6a26b212f6da0519f60d8283cf75439f825d` regenerates the shared CI-repair claim golden with the three existing memory defaults; it does not change runtime behavior.

### Reviews and focused execution

Reviewer, tester and auditor completed immutable reviews through `fa3b99d4`, including both merge parents, retained delta against 4103fad7 and earlier evidence range 974d4992..a7f721c0. No mandatory source finding remained. Tester executed 235 focused tests with zero failures/skips/cancellations and an exported typecheck; a compiled heartbeat-revocation mutation failed the intended authority assertion. This proves that regression's sensitivity, not universal mutation coverage. Reviewer separately reviewed the three-field golden repair `fa3b99d4..eced6a26` with no findings.

Reviewer deferred one lander-owned CI timing note at fa3b99d4. Tester identified absent separate coverage/pre-commit slots without proposing a patch requirement; no new gate was added. Auditor reported no graded security finding. Secrets canaries were detected with zero tracked findings; scoped Semgrep found zero findings/errors; agent/web npm audits reported zero vulnerabilities in the installed trees. These pre-reconciliation scans do not establish complete static coverage or vulnerability status of the subsequently updated installed packages. govulncheck was unavailable and was not run.

### Observed checks on this integration

All logs are run-local scratch artifacts, not published artifacts. Gates used pinned Task 3.54.0; long root gates used the 120-minute command bound. Heavy jobs ran serially. Earlier failed and interrupted checks remain evidence, not green results.

| Tested revision | Executed check | Actual result | Scratch log |
|---|---|---|---|
| merge / fa3b99d4 | sqlc v1.31.1 generation before/after offline renumber | PASS, EXIT=0 each | gate-log.5H3z4q / gate-log.9X5vKb |
| fa3b99d4 | check:migration-numbering check:migration-additive | PASS, EXIT=0; head 339 | gate-log.8vNJVe |
| integrated source | npm run typecheck after fake observation/test narrowing repairs | PASS, EXIT=0; initial typecheck failure retained | gate-log.ssGDlX / gate-log.cRANeb |
| fa3b99d4 | gate:agent | FAIL, EXIT=201 at dependency preflight; no complete agent gate | gate-log.dNvHT0 |
| integrated source | deps-check:web before reconciliation | FAIL, EXIT=201 at dependency preflight | gate-log.4cy8xZ |
| 164ef8b8 | deps-check:agent deps-check:web before reconciliation | FAIL, EXIT=201 at agent preflight; later web target not reached | gate-log.aImdx2 |
| 164ef8b8 | approved incremental install, agent then web | PASS, EXIT=0 each; 4s / 2s; tracked manifests/locks unchanged | gate-log.oQWJvg / gate-log.PCOK5Z |
| 164ef8b8 | deps-check:agent deps-check:web after reconciliation | PASS, EXIT=0; both lockfiles matched | gate-log.xZFd5A |
| 164ef8b8 | gate:agent, root 120-minute command bound | PASS, EXIT=0; full unit and Codex conformance stages completed | gate-log.D0BFiu |
| fa3b99d4 | gate:api | FAIL, EXIT=201; shared CI-repair golden omitted memory defaults | gate-log.wBCMRE |
| fa3b99d4 / default export | narrowed CI-repair wire producer comparison | candidate FAIL, base PASS; actual merge defect repaired | gate-log.oZISri / gate-log.D0KfT8 |
| eced6a26 | gate:api after golden repair/review | PASS, EXIT=0; fmt/vet/build/sqlc/lint/deadcode and race/count tests completed | gate-log.XifDao |
| integrated source | test:codex-supervisor | PASS, EXIT=0 | gate-log.mHeAod |
| integrated source | test:e2e-lib / test:forge-fake | PASS, EXIT=0 each | gate-log.2ZPOIj / gate-log.wecFCw |
| eced6a26 | unchanged e2e/run-store-it.sh | FAIL, EXIT=1 solely at workersvc 15-minute package timeout | gate-log.JXAmc2 |
| eced6a26 | human-authorized disposable LiveDB continuation and combined guards | PASS, EXIT=0; sweep-equivalent coverage under the deviation below | gate-log.is170A |
| default 4103fad7 export | local workersvc-only LiveDB timing reference, identical Go flags/20m budget | PASS, EXIT=0; 1,290 passed, zero skipped | gate-log.oQs4jm |

### Provisioning deviation and completed agent gate

Before reconciliation, the supplied agent oxlint/native binding was 1.86.0 versus merged lockfile 1.87.0; web additionally had @babel/parser 8.0.6 versus 8.0.7. The human confirmed worker provisioning had finished at run start and explicitly authorized bounded incremental reconciliation after the merge. This run had no outstanding installation command; it did not inspect the process table. Tracked package.json/package-lock.json status was clean before and after installation.

Exactly once per package, serially, the lead ran `timeout --kill-after=30s 600 npm install --ignore-scripts --no-audit --no-fund` from agent/ and then web/. Agent ran 14:54:09–14:54:13 UTC (4s, EXIT=0); web ran 14:54:18–14:54:20 UTC (2s, EXIT=0). Neither node_modules directory was deleted; npm ci was not used. No tracked manifest or lockfile changed, so no installation change needed restoring. Both subsequent canonical dependency checks passed. Earlier failed dependency checks remain recorded.

The unchanged canonical gate:agent then ran from 14:54:41 to 16:03:08 UTC (4,107s), EXIT=0. Lint reported zero warnings/errors; deadcode and typecheck completed. Units reported 15,154 tests, 15,139 passed, zero failures/cancellations, 15 skips, and **zero real-procfs skips**. Reported skip categories include packaged-profile/worker-UID/root prerequisites, missing ssh-agent, a live credential opt-in and non-Linux counterpart cases; they do not prove those environments. Codex conformance reported 183 passed with zero failures/cancellations/skips; its compiler and completeness checker passed (37 catalogued clauses, required claude/U, codex/U and codex/P matrix satisfied).

The full unit log positively records both historical concerns passing: `new report proof transport failure preserves existing physical archive authority within the same budget` and `runner memory close preserves original feedback intent/custody with durable tail=true`. This accepts the current integrated candidate; it does not reclassify the old failed/interrupted observations as pre-existing or prove their original cause.

### LiveDB sweep-equivalent coverage: explicit deviation

The unchanged sweep ran locally from 13:54:52 to 14:28:45 UTC on 2026-10-15. Store (372.798s), handler (573.475s), forgesvc (13.543s) and schedsvc (10.112s) passed. Workersvc reached its 15-minute package timeout (900.096s); no earlier failed test, race or skip was found. Recovery, poller and final guards were not reached. This original sweep remains **failed**, not retroactively passing.

The human authorized one fresh disposable execution if that was the sole failure. A scratch-only copy reused the original database setup, exact LiveDB selection and race/count/serial flags. Only workersvc's package timeout became 20m; recovery/poller retained 10m. The successful first-four package output was supplied to the original complete seven-package skip/completeness checks. The shipping script was not edited.

That continuation ran locally from 14:30:22 to 14:46:20 UTC: workersvc **882.581s**, recovery **45.379s**, poller **10.352s**, EXIT=0. The combined original skip guard reported **2,921 passed, zero skipped**; all seven plain package-result checks passed. Both disposable databases were owned and cleaned by their harness traps. This establishes sweep-equivalent LiveDB coverage across two executions under the approved deviation, not a single successful unchanged sweep. Optional Codex lost-reply and heartbeat composed E2E opt-ins were unset and were not executed.

Named live cases include memory admission parity, archive custody, owner-only Resume/Cancel/non-owner behavior, reservation concurrency/replay/identity/no-refund, allowance redispatch, checker authority, revision races, feedback deduplication and retained custody. Ordinary gate:api LiveDB skips are not the evidence for these claims; actual database execution above is.

### Local workersvc timing reference

A default `4103fad73896f103bf7cebd743f009b0b6bde671` archive supplied the local before-change reference. A scratch-only harness copy retained its disposable database setup and skip/plain-package checks, selected workersvc only, and used the same exact Go flags and 20m package budget as the successful candidate execution. Git identity was resolved in the real checkout; Go VCS stamping was disabled for the exported benchmark. API go.mod/go.sum were byte-identical to the candidate. The default reference ran **after** the candidate, serially, from 16:04:10 to 16:17:59 UTC (829s harness wall time including setup/build); the cached postgres:17 image required no provisioning. Workersvc itself passed in **707.393s**, with 1,290 passing cases and zero skips. The candidate continuation used a fresh disposable database and cached image too, reporting **882.581s** for workersvc (958s harness wall time for workersvc plus recovery/poller/setup).

The package-duration delta is **+175.188s (+24.77%)**, candidate minus default. Both successful executions used `-buildvcs=false -count=1 -v -race -p 1 -timeout 20m -run 'LiveDB$'`, equivalent tooling and the same host, one heavy job at a time. Source/test inventories differ; each is a single local observation, the baseline was collected later, and build-cache/host variation was not controlled. This does not isolate a causal runtime regression or establish hosted performance. The earlier failed 900.096s candidate run is not the successful baseline. The scratch timing reference is not another full seven-package default sweep.

Worker-UID before/after timing and final M5b acceptance remain unfinished. Post-publication CI step/dependency-path timing remains lander-owned; no CI timing claim is made.

### Remaining acceptance

M1 is complete under the approved deviations. The failed original sweep and initial API/dependency failures remain above with their actual outcomes and follow-up evidence. A tracked literal consumer inspection found no new memory-result expectation in general e2e/phases, lib or forge-fake; existing requeue-exhaustion assertions describe their guard-off scenarios. Full Compose phases remain unrun. Real command-tree lane enrollment/execution, isolated 1Gi/2Gi calibration, complete bound stdout reports, owner-surface visuals/documentation and final serial gates remain later milestone work. Production remains default-off; no hosted sizing recommendation or real-kernel PID-recycling proof is claimed.


## Current resumed integration — 2026-10-14, M1 still in progress

The approved default merge is committed at `dddc535b84eabe690dd00525c3d117ec993b2567`, with parents `3fcd6a210a264defd0ec189b0a17266623ca8609` and `ff48bc1b8ee5cfc7aede08a25f68ee9f74cd220c`. No published history was rewritten. The only textual conflict was CHANGELOG; both entries were retained. Upstream report-only checkpoint evidence, bounded terminal-error evidence, retirement coverage, serving/custody separation, CPU advisory and storage sizing changes are preserved.

Comment-only commit `974d49921b253c03b0310e2ec374d66883bfc5ba` replaces the obsolete claim that runner reconciliation is a future seam. A tracked search for that runner-reconciliation claim found only this site; the runner already reconciles captured receipts before release. Reviewer independently confirmed the minimal correction with no findings. No executable guard behavior changed in that commit.

This section is the current-state record. The earlier sections below are historical records at their own named revisions. **M1 is not complete:** the full agent gate was interrupted, two observed failures remain unclassified, the unchanged LiveDB sweep ran and failed at the workersvc cumulative timeout, and the real command-only subprocess proof has not executed in this resumed integration. M4 and M5 remain unstarted.

### Follow-up live sweep — M1 remains incomplete

At evidence-only candidate `6842646ff1dde3cbfdc27e75dca799e8ab4be8f9`, the lead ran unchanged `./e2e/run-store-it.sh` in the foreground. It returned **FAIL, EXIT=1** (`gate-log.ZaOR5m`). Store, handler, forgesvc and schedsvc completed with plain package PASS lines. Named memory archive custody, owner-only Cancel/Release, reservation concurrency/binding/replay/no-refund, claim incarnation, allowance preservation, revision and feed-deduplication cases executed and passed. This is fresh live proof of those named cases, not a passing full sweep.

Workersvc reached its existing cumulative 15-minute package timeout (900.174 seconds); the panic named `TestWorkerExhaustionEpisodeAccountingLiveDB/ordinary/owner_ordinary` with zero seconds reported for the currently running case. This does not establish a deadlock or failure in that case, nor establish a pre-existing failure. The sweep stopped before recovery/poller and the final skip/completeness guard. No test timeout, filter or shipping script was changed. Next triage must use the smallest useful named candidate/base comparison before repeating the full sweep; remaining cumulative-runtime uncertainty must be reported.

The read-only documentation delegation for `974d4992..6842646f` failed in transport and supplied no completed review. That attempt is not review coverage; retry the combined evidence-only range. Earlier completed source reviews remain as recorded below. M1 is still in progress: full agent gate, classified failure evidence, successful unchanged full live sweep and the real command-only supervisor proof remain outstanding.

### Observed checks and environment

Worker-provisioned agent/web dependency trees matched the seed lockfiles but differed from the merged transitive versions. Initial `task deps-check:agent deps-check:web` failed (`gate-log.oWI02q`, EXIT=201). Incremental `npm install --ignore-scripts --no-audit --no-fund` reconciled agent (`gate-log.52BkzC`, EXIT=0) and web (`gate-log.W9FMu0`, EXIT=0), without deleting node_modules or changing tracked manifests/lockfiles. The intermediate check passed agent and failed web (`gate-log.7RHco9`, EXIT=201). Subsequent canonical checks matched both complete installed trees. Task 3.54.0 was provisioned under worker scratch; no shipping dependency or gate recipe changed.

| Tested revision | Actual command/check | Result and limits | Retained scratch evidence |
|---|---|---|---|
| `dddc535b` | `task deps-check:agent deps-check:web check:migration-numbering check:migration-additive typecheck:agent typecheck:web test:codex-supervisor` using pinned Task | PASS, EXIT=0; migration head 334, next 335; supervisor packages executed | `gate-log.Hy5JZk` |
| `dddc535b` | `task gate:api` using pinned Task | PASS, EXIT=0; SQLc drift clean, lint/dead code clean, race/count gate passed; ordinary LiveDB skips do not establish live contracts | `gate-log.gMUFlA` |
| `dddc535b` export | Tester two focused Node selections covering memory lifecycle/authority and upstream recovery | PASS, EXIT=0 each: 178 + 175 tests, zero skips/cancellations; installed dependencies independently verified | `gate-log.VwY8n6`, `gate-log.Vb6aUU`, `tester-1-review.md` |
| `974d4992` | `task gate:agent` using pinned Task, unchanged flags/environment/concurrency | INTERRUPTED by harness 60-minute deadline; command tree reaped, shell EXIT not recorded. Dependencies/lint/dead code/typecheck completed; unit stage incomplete, Codex M4 stage not reached. This is not a pass | `gate-log.OKS9eW` |
| `dddc535b` export | Cancelled auditor's focused guard/reader/runner selection | FAIL, EXIT=1: 124 tests, 123 pass, one durable-tail assertion failure; no skips/cancellations in the test process. The cancelled delegation supplied no completed audit report | `gate-log.eLdxnu` |
| `974d4992` and default `ff48bc1b` exports | Named archive fallback test via Node, tsx, hermetic-proc, 120000-ms timeout | PASS, EXIT=0 on each; exactly one case, zero skips. Neither reproduced the original suite failure | `gate-log.c5Y0HS`, `gate-log.0fPg8F` |
| `974d4992` and published-floor `3fcd6a21` exports | Named durable memory tail test, same Node flags | PASS, EXIT=0 on each; exactly one case, zero skips. Shared candidate dependencies; floor lockfile differs. Neither reproduced the original focused-selection failure | `gate-log.nwSwti`, `gate-log.lDPOK0` |
| `974d4992` frozen checkout/export | Completed replacement auditor secrets/npm/scoped static checks | Secrets PASS with both canaries detected and zero tracked findings; agent/web npm audit PASS with zero vulnerabilities; explicit Semgrep export scan covered 33 Codex files/eight applicable rules with zero findings | `gate-log.RlslGR`, `gate-log.QL2zLT`; explicit scan reported by auditor |
| `974d4992` | Disposable command-only fixture image provisioning, not a test | INTERRUPTED at harness 60-minute deadline while `apk add --no-cache bash git` was running. Node/Go image layers downloaded, but final fixture image absent; no subprocess proof ran | `gate-log.XBe8x0` |

The agent wrapper reported Node 24.20.0, libuv 1.52.1, availableParallelism=4, cpu.max=400000 100000 and effective test concurrency three from node-default. No concurrency override was supplied. The interrupted log contains 12,744 passing test-or-suite lines and one named failure; these are partial observations, not a complete unit result. The failure is `new report proof transport failure preserves existing physical archive authority within the same budget` in `runner-acknowledged-final-restart.test.ts:849`; it lacks a final assertion stack/unit summary because the outer command was interrupted.

The cancelled auditor's actual failed test is `runner memory close preserves original feedback intent/custody with durable tail=true`, with false versus true at `runner-worker-memory.test.ts:623` (awaited tail-preservation result). Its 200-ms cleanup fixture budget and the archive test's shared 1000-ms proof budget can refuse work, but elapsed fixture times do not establish the cause. Both candidate/base narrow comparisons were green and therefore **inconclusive** for the original failures. Neither failure is classified as pre-existing, unrelated, or a timing flake. No production repair is justified by those comparisons alone. Full validation remains required after justified triage or environment/diagnostic changes.

### State reader/writer/surface inventory

The tracked direct-token enumeration used memory_episode, memory_intervention_count, memory_policy, worker_memory_pressure, memory_hold and capturedMemory, then expanded with MemoryEpisode, MemoryInterventionCount, MemoryPolicy, MemoryHold, bindWorkerMemory and reconcileWorkerMemory. The resulting direct file population is exhaustive for those tokens and stated exclusions, not an absence proof for indirect control flow. Tests/docs were not treated as production consumers. Generated SQL readers and followed indirect consumers were inspected separately by the fact-checker.

- Persisted accounting: migration 00334 constrains episode/count/policy; queries/memory.sql freezes policy with COALESCE and increments count only after winning insertion. workersvc/memory.go binds/fences and freezes denied reservations without spending another slot. claim_assembly.go, chat.go, job_claim.go and judge.go project persisted fields; generated models.go/memory.sql.go and other generated claim/update rows carry them. Runner seeds count from claims and retains the maximum observed spend across retirement.
- Reservation/outcome: workersvc/memory.go locks/re-reads worker and tenant run/parent authority; binding includes intervention/run/user/worker/nonce/generation/episode and immutable policy. memory.sql conflict insertion does not double-charge; first outcome is immutable and outcome ACK is non-authorizing. WorkerClient strictly decodes binding, policy and allowance arithmetic.
- Incarnation/phase: WorkerClient invalidates before registration IO and refuses stale registration completion. WorkerMemoryCommands checks context/flight/revision identity and phase/pressure latches. RunRunner closes admission before non-running state writes and sequence-checks Running ACKs; a consumed human revision can reopen only its exact phase. Runtime/worker shutdown revokes authority.
- Owner state/custody: memory park releases the captured claim and has no scheduled retry. Generic recovery promotions exclude worker_memory_pressure. handler/resume.go uses guarded ResumeMemoryEpisode, resetting count/policy and advancing episode; ordinary cancellation remains owner-scoped. recovery/final_inventory.go and recovery.sql preserve independent custody guards. Runner retains source/session intent before awaited capture/report work and reconciles original receipts.
- Wire/UI: handler/worker_memory.go and route registration enforce worker/path/body binding; runs_dto.go projects held-only memory allowance, with null unknown maximum/remaining. apitypes summary conversions preserve MemoryHold; runStream/useRunStream refresh detail on state messages. RunView/runBadge render owner holds and separate memory counts; API types and fixtures match. Mock resume/cancel clears the held projection. CLI memory_hold.go, run_get/render/steer/wait, worker_exhaustion and TUI board/detail/transcript consumers preserve owner guidance, unknown allowance, default-wait termination and final log drain.
- Model attribution/tool result: main/worker/runtime own one monitor. Broker metadata is object-bound; default spawner/delegation consume captured authority. memory-command.ts rechecks exact supervisor/primary identity before registered reap. codex-harness/delegation/projection serialize the structured result. Reader total/stat/PSI observations and guard freshness/selection are separate from approximate command RSS.

The fact-checker's source inspection demonstrated wiring, not live database correctness or physical process enforcement. The followed indirect paths include handler/handler.go, handler/worker_memory.go, workersvc/submit.go, main.ts, codex/memory-command/codex-harness/projection and web/CLI stream/steering consumers beyond direct token matches.

### Completed reviews and remaining work

Reviewer and tester reported no mandatory merge defect over `3fcd6a21..dddc535b`, including `ff48bc1b..dddc535b`. Reviewer's Node execution was unavailable when dependencies were still stale; its 17 supervisor cases passed. Tester independently verified repaired dependencies and passed 353 focused cases. Replacement auditor completed both merge-to-`974d4992` comparisons with no demonstrated graded findings; its explicit scoped Semgrep coverage excluded earlier zero-target outputs. Go vulnerability scanning was unavailable (govulncheck absent), not green. Reviewer separately found the comment-only `dddc535b..974d4992` correction accurate with no findings.

Reviewer's one optional note about upstream hosted CI/critical-path timing remains lander-owned acceptance. Tester's standalone memory-command CI trigger gap remains assigned to M5's required lane wiring, not declared solved here. Existing earlier optional notes below remain recorded.

Next M1 work: classify or meaningfully recheck the two observed failures, obtain a complete canonical agent gate, execute the unchanged LiveDB sweep with required memory cases actually running, and finish the bounded command-only real-supervisor proof. Fixture provisioning must avoid repeating the unchanged stalled package-install step; a fixture-only prerequisite/environment remedy is not production proof or a change to shipped gate settings. Complete and review subsequent evidence/repair commits before declaring M1 complete or starting M4. Production enablement remains disabled; no calibration, hosted rollout, future CI success or production headroom is claimed.


## Resumed reconciliation — 2026-10-14, scoped M1 validation complete

This run preserves published floor `0bc24b959f85d8ef5adcf322f4c3240a6bf2eec3` and merges default `ebbe088d01959ec74e0826a0887af3e3e49ce3b5` without rewriting either history. The earlier sections below remain historical evidence for their named revisions, not acceptance of this merge.

Conflict resolutions combine the captured memory capability with upstream summary settlement, Read scheduling, retained-source admission and shutdown. Worker construction keeps upstream's gate/channel arguments and adds the memory runtime afterward; the lifecycle fixture now verifies that the memory-enabled worker starts and joins the retained-source channel once. Runner failure/release paths keep both unresolved-completion and memory-custody protections. The credential-free settlement helper retains the memory cancellation purpose while settling upstream plan summaries under the same remaining deadline. Test doubles supply the bounded supervisor observation interface.

The implementation delegation ended without a report; retained edits were inspected by the lead. A completed SQL generation run used `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate` (log `gate-log.ZFFLat`, EXIT=0); no additional generated drift resulted. A combined retained test log (`gate-log.MiFJfB`, EXIT=1) failed the late native park successor-admission case; its isolated rerun (`gate-log.2hn1fy`, EXIT=0) does not establish a baseline failure or close the combined-run concern. That earlier failed result remains recorded; the resumed checks and repair below supersede its pending-validation state.

### Resumed checks and deadline-fixture repair

Merge commit `564bf2332000041c628b1709055cdc6ad169584a` preserves both parents. Reviewer, tester and auditor reviewed the reconciliation against both published floor and default with no demonstrated mandatory finding. The first implementation delegation was cancelled; two auditor attempts and one tester attempt returned transport failures and provide no review. Completed retry reports and observed log results supply the evidence.

The lead's seven-file combined selection at `564bf233` failed three elapsed-bound assertions (`gate-log.V9ZfKb`, EXIT=1). The equivalent command against a published-floor export passed 202 tests, zero skips (`gate-log.glCnh0`, EXIT=0), so these candidate failures are not classified as base failures. The isolated late-park case also passed on both floor and candidate (`gate-log.xpJkYV` / `gate-log.6gZD4b`, EXIT=0); that comparison alone was inconclusive for the combined failure.

Repair `7d3532a9e275472d312ec2c2c2ca3ca0e1a31314` changes only the runner memory fixture. Elapsed clocks now start at the trusted accessor or pressure trigger, after runner preflight; the owner budgets remain 200 ms and all three elapsed bounds remain 1500 ms. Blocked-port rescues start when those ports enter, not before preflight. The successor's readiness wait uses its existing 5000 ms capture window; the park rescue allows a bounded 10 seconds for the original stop plus successor capture. Controlled-release, unsettled-operation, custody and successor-fencing assertions remain. No production code, gate flags or concurrency setting changed in this repair.

Reviewer, tester and auditor reviewed `564bf233..7d3532a9` with no mandatory finding. The tester typechecked an isolated control making the effective 200 ms production owner deadline 5000 ms, then observed all three intended elapsed assertions fail. This establishes sensitivity to an overlong deadline, not independent sensitivity to every custody or successor-corruption defect.

| Revision | Executed check | Observed result | Run-local log |
|---|---|---|---|
| `564bf233` | `task check:migration-numbering check:migration-additive typecheck:agent typecheck:web test:codex-supervisor` | PASS; EXIT=0 | `gate-log.wNsGuh` |
| `564bf233` | Scratch-only narrowed copy of `e2e/run-store-it.sh`: handler owner-only release plus workersvc reservation cases, unchanged race/count/package-serial flags | PASS; 10 top-level tests, zero skips; applied schema through 334; EXIT=0 | `gate-log.AZdaPy` |
| `564bf233` | `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/uzi -run '^TestMemoryHold' -v` | PASS; six top-level tests; EXIT=0 | `gate-log.BipKh3` |
| `7d3532a9` | Named repaired exhaustion/late-park/delayed-feedback cases | PASS; three tests, zero skips; EXIT=0 | `gate-log.hmsZUe` |
| `7d3532a9` | Seven-file Node selection: guard, reader, commands, lifecycle, runner memory, Codex memory command and runner plan-gate | PASS; 214 tests, zero skips/cancellations; EXIT=0; process 114.24 seconds | `gate-log.6R9jEw` |
| `564bf233` | Auditor `task scan:secrets`, inspected by lead | PASS; canaries detected, zero tracked findings; EXIT=0 | `gate-log.3Aff6N` |

The scratch-only live-DB diagnostic selected `TestMemoryReservation.*LiveDB` and `TestMemoryHoldOwnerOnlyReleaseLiveDB`; it is contract-seam proof, not the final unchanged full live-DB sweep. The source/supervisor/API checks remain usable after the test-only repair because their production inputs are unchanged. Reviewer/tester focused export checks also passed; their counts do not substitute for the lead's integration checks.

Both tester reports (`564bf233` and `7d3532a9`) ran `task gate:agent` despite the focused dispatch; both failed at the known dependency preflight (EXIT=201). These remain failed checks, not passing gates or demonstrated merge regressions. Their optional c8 coverage-threshold suggestions, one in each report, are deferred without changing repository gate recipes. Matched-lockfile acceptance and full final component gates remain M4 work.

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
