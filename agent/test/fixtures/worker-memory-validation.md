# Worker memory guard validation — issue #2396 (historical foundation)

This retained foundation record describes only its named source revisions, including dependency status and unfinished seams at that time. It does not state current integration or deployment status. [M1 reconciliation](worker-memory-m1-reconciliation.md) and [M4 acceptance](worker-memory-m4-validation.md) supersede its current-state language without reclassifying its failed or unexecuted checks.

## M1 source and limits

Default `bd3a46e0efd4cd054cb9e9d98c1f7b7b002e39a3` was merged, preserving published `1aef4d2a8aa95cfa25a4582ec9e0d7beb25aec5d`. Merge commit: `80109a568e4b9ee78c187f4a7b4a97552b7af112`. Drain repair: `cf3919ea8b45c2d5e9dc58f59e72f4ef17dd85a8`. Test correction and final validated production source: `6fa39c916d0ff79f3704e563fa05df48a66e9f64`.

This records M1 only. Codex enrollment, tool-result joining, claim binding, admission/preservation integration and their five required M2 red/green proofs remain unfinished. Real private-cgroup calibration is **unverified**. No enabled default or production headroom recommendation is established. #2512 remained open when checked; the retained-source recovery throw remained in runner.ts. Preservation/recovery integration requires verification of that dependency's fixed source and deployment.

Logs and patches below are run-local artifacts under `.uzi/scratch/`, not published files. Scratch is not portable across worker recovery. This report preserves names, source identities, commands and outcomes, not raw logs.

## Fix-absent and fix-present assertions

Every listed red is an observed behavioral assertion failure, not an import/build error, test-runner timeout or skip. Named assertions were retained between legs. All six selected launcher tests executed with zero skips in the final focused green run.

| Named regression | Fix-absent source and observed FAIL | Fix-present source and observed PASS |
|---|---|---|
| `memory drain evidence preserves process identities and reports enumeration failure without claiming clean disposal` | Export of 6fa39c91 with only the drain parser reconciliation reverted: numeric PID validation replaces identity validation; enumeration/oversize reasons and their empty-child guard removed. `gate-log.YqYATy`, EXIT=1: “valid negative evidence reaches the caller”, undefined versus unconfirmed. Earlier isolated reconciliation red `gate-log.mOv2ji` also failed this assertion. | 6fa39c91, `gate-log.UlUedp`, EXIT=0. Deadline/echild-contradicted identity objects retain exact decimal ticks; enumeration remains unconfirmed, no clean disposal. Negative malformed-evidence cases also passed in the integrated gate. |
| `bounded drain evidence remains unconfirmed without disabling disposal retries` | Same fix-reverted export, `gate-log.YqYATy`, EXIT=1: “bounded negative evidence reaches the caller”, undefined versus oversize. Strengthened identical retry assertion also used in `gate-log.6n37C3` with just oversize acceptance reverted. | 6fa39c91, `gate-log.UlUedp`, EXIT=0: two disposal requests, repeated unconfirmed outcomes, no parser failure or tree removal. |
| `keeps unavailable responses and timed-out late replies nondestructive and single-flight` | Unmodified published 1aef4d2a export, `gate-log.DhWXVV`, EXIT=1: unavailable reason timeout instead of unreadable. | 6fa39c91, `gate-log.UlUedp`, EXIT=0. |
| `preflights 16000 rows, escaped keys, nesting and malformed structure before parsing rows` | Same published export/log, FAIL: stale instead of oversize. | Same fixed source/log, PASS. |
| `fences 300 timed-out attempts and ignores invalid and oversized late IDs` | Same published export/log, FAIL: timeout instead of busy. | Same fixed source/log, PASS. |
| `respects real writable backpressure without queueing further observations` | Same published export/log, FAIL: control queue grows instead of remaining unchanged. | Same fixed source/log, PASS. |
| `TestDrainActualProducerWire/deadline`, `/echild-contradicted` | 80109a56 production plus test-only patch, `gate-log.MWvMrE`, EXIT=1: children [4243] instead of identity object with startTime 18446744073709551615. | Repair source represented by cf3919ea/6fa39c91, `gate-log.uqbnkr` EXIT=0 and final supervisor integrated gate PASS. Enumeration subcase passed both legs; no enumeration-producer fix claimed. |
| `TestDrainContinuousReapsDeadline` | Same pre-fix source/log, FAIL: 10001 waits then drained. | Same fixed source/log, PASS: four waits, unconfirmed deadline. |
| `TestDrainKillLoopDeadline` | Same pre-fix source/log, FAIL: three kills and one wait. | Same fixed source/log, PASS: one kill, zero waits. |
| `TestDrainDeadlineAfterEmptyVerification` | Same pre-fix source/log, FAIL: drained after deadline. | Same fixed source/log, PASS: unconfirmed deadline, no authority. |
| `TestDrainExpiredAuditorProbe` | Same pre-fix source/log, FAIL: one kill, 10001 waits. | Same fixed source/log, PASS: zero kills/waits. |
| `TestDrainCumulativeRetainedEvidenceBound/killed`, `/reaped` | Same pre-fix source/log, FAIL: retains 257 identities and reports drained. | Same fixed source/log, PASS: at most 256 retained PIDs, unconfirmed oversize, no authority. |

Go red test-only patch: `m1-regression-tests.patch`, SHA256 `ff10cd9aa7c8976a4f19f662b6d8e8145d3adfa4f0a2017e30d4aa7a35eece2f`. The added finite evidence tests avoid counting a timeout from the older infinite churn fixture as proof. Parser reversal: `m1-final-parser-reverted.patch` contains only the production predicate replacements described above; unchanged tracked tests execute against it. No reverted production code was committed.

The cleanup test fixture correction changes no production behavior: `TestDisposeCleanupPastDeadlineIsRetainedDeadline` now completes drainage at 950ms of a 1000ms budget, after the 900ms cleanup deadline. It preserves the retained-tree/retry assertions. Focused PASS: `gate-log.nf1Bgt`; final supervisor suite also PASS.

### Focused commands

All commands ran in the foreground with separate logs and recorded EXIT. Go used GOMAXPROCS=2 and -p=1; Node used UZI_AGENT_TEST_CONCURRENCY=1 and --test-concurrency=1. No dependency reinstall.

Launcher selector O:
`keeps unavailable responses and timed-out late replies|preflights 16000 rows|fences 300 timed-out attempts|respects real writable backpressure`

Launcher selector D:
`memory drain evidence preserves process identities|bounded drain evidence remains`

From the real checkout root, with a fresh archive export at the stated source and matching-lockfile borrowed dependencies:

```sh
UZI_AGENT_TEST_CONCURRENCY=1 node --import "$PWD/agent/node_modules/tsx/dist/loader.mjs" --test --test-concurrency=1 --test-name-pattern='<O or D>' "$snap/agent/test/codex-launcher.test.ts"
```

O on published export: DhWXVV; D on fix-reverted export: YqYATy. Final green command used the combined O|D selector and real `agent/test/codex-launcher.test.ts`: UlUedp. Fresh exports used pipefail + git archive extraction, no Git inside exports, and were removed after execution. A final focused rerun established exact command provenance; earlier matching red/green logs remain supplementary.

Go red (MWvMrE), from supervisor directory:

```sh
GOMAXPROCS=2 GOFLAGS='-p=1 -buildvcs=false' go test -count=1 -v -run 'TestDrain(ActualProducerWire|CumulativeRetainedEvidenceBound|ContinuousReapsDeadline|KillLoopDeadline|DeadlineAfterEmptyVerification|ExpiredAuditorProbe)$' .
```

Go green (uqbnkr), same directory/environment:
`go test -count=1 -v -run 'TestDrain|TestDisposeEvidence|TestAbnormalEvidence|TestObservation' .`

## Integrated M1 gate

6fa39c91: `gate-log.BALIxi`, EXIT=0. Bounded foreground script `m1-integration-gate.sh` executed these serial checks:

- `task check:migration-numbering`, `task check:migration-additive`, `task check:sqlc-drift`, `task typecheck:agent`: PASS. Migration 314 is the memory migration; default's 313 cross-check migration retained. Regeneration with sqlc v1.31.1 produced no drift.
- From agent: `node --import tsx --test --test-concurrency=1 --test-timeout=120000 test/codex-launcher.test.ts test/codex-startup-cleanup.test.ts test/worker-cross-check-lane.test.ts test/worker-memory-lifecycle.test.ts`: 173 PASS, zero FAIL, one UID-dependent skip. This is not image-backed UID proof.
- `go -C agent/codex/supervisor test -count=1 ./...`: PASS, including sandbox/fileop/safetree.
- `go -C controller test -race -count=1 ./internal/config ./internal/kube ./cmd/controller`: PASS.
- Live PostgreSQL 17, newly owned disposable fixture limited to 512MiB and one CPU, pinned digest `sha256:5c855ad7b85e68e48a62f34662853f38b57c1c1d80f3a927ab58034fd6d31c5e`, removed by the gate's EXIT trap. With its UZI_TEST_DATABASE_URL, the following executed through migration 314 with zero skips:

```sh
go -C api test -buildvcs=false -race -count=1 -v -p 1 -run 'TestMemory(Reservation|HoldOwnerOnlyRelease|HoldNonOwnerGuards|FreshAdmission|OrdinaryRedispatch|CrossCheckParent|AutomaticRound|OutcomeAndFeed|HoldResumeClaim|Policy).*LiveDB$|TestCrossCheckLane.*LiveDB$' ./internal/workersvc ./internal/handler
```

Reservation races/binding/no-refund, owner-only resume, allowance retention on redispatch, memory cross-check parent guards and dedicated cross-check lane tests PASS. This proves the database authority seam; it does not prove runner preservation or an enrolled Codex adapter.

Owned foreground gate resource observation: /usr/bin/time reported 13m30.57s elapsed and maximum resident set size 3,649,280 KiB. That metric is not total worker cgroup peak, an analyzer memory cap, or calibration. No retained incident telemetry was available to establish the earlier OOM's measured cause.

Earlier attempts: YfvBuD EXIT=201 stopped at duplicate-property TS2783; plgPgq EXIT=1 passed agent checks but stopped at the now-corrected expired-deadline fixture. Neither reached live DB. Intermediate export setup/build failures are not counted as red proofs.

## Review and remaining checks

Reviewer/tester/auditor inspected the saved issue delta and merge at 80109a56; mandatory wire/deadline findings were repaired and re-reviewed at cf3919ea. All three reported clean scoped closure at 6fa39c91; lead inspected patches and recorded gate outcomes. Secrets and Semgrep scans detected canaries with zero tracked findings; agent dependency audit reported zero vulnerabilities. Go vulnerability scanning remained unavailable. Web audit's two low-severity KaTeX/Mermaid entries predate this issue delta.

Optional tester suggestions for a new coverage threshold and broader hook checks were deferred beyond M1. The demonstrated missing retry assertion was fixed. Full component/repo gates, recovery UI/CLI, real adapter subprocess proof and calibration remain later milestones; this report does not claim them.
