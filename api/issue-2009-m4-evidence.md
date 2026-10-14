# Issue 2009 M4 acceptance evidence

The M4 tests add exact persisted-age checks across six accepted heartbeat/claim renewals at cap 0 and cap 2, boot and same-generation register continuity, independently cleared overflow, foreign/released/stale-generation/terminal/expired evidence, fresh legacy minimum and expired reappearance, complete-list replacement and unavailable/invalid nonrenewal. A captured real-Outbox duplicate-active fixture is rejected in all four API paths without insertion or renewal. Identity-only recovery tests use existing frozen-drift and parent-lock-wait seams and verify identities grant no lifecycle pending lease.

Lead focused selection on the unchanged M2/M3 production tree plus these tests: `bash .uzi/scratch/m4-lead-focused.sh`; fresh session-owned Postgres 17 Docker fixture, race enabled, serial packages, `go test -buildvcs=false -count=1 -v -race -p 1 -timeout 240s ./internal/workersvc ./internal/handler -run '^(TestIssue2009M4.*LiveDB|TestPendingAll.*|TestIssue2009PendingAggregateRoutesLiveDB|TestHasLivePollerPendingOutcomeLeaseLiveDB|TestCancelRunServerSideWithPendingOutcomeLiveDB|TestSubmitInputPendingOutcomeCancelLiveDB|TestWorkerRecoveryFreezeDriftLiveDB|TestWorkerRecoveryCrossedServicesLiveDB|TestRegisterCarriedSnapshotPreservesLeaseLiveDB)$'`. Exit 0; zero skips; log `gate-log.Hrn72q`; wall 204.374645371 seconds including build, package test steps 20.212s workersvc and 2.431s handler. This is local evidence, not CI timing.

Earlier test-development failures were setup errors, not production defects: unused import (`gate-log.TSgbDx`), invalid string escape (`gate-log.IxJZ0C`), then invalid chat-run shape (`gate-log.zm19Ua`, exit 1). The fixture was corrected; the narrow six-renewal selection passed (`gate-log.qxBfpv`) before the successful selection above. Failed logs remain in scratch.

The literal UUID and generation-zero/int64-maximum assertion suggested by the M2 tester is now included in optional decoding coverage. Handler coverage adds equal-count/different-membership rows and ages, empty listed rows with zero confirmed plus fresh overflow, overflow expiry independent of raw flags, both list scopes and patch response.

Matched worker baseline runtime and genuine assertion failures are recorded in `agent/issue-2009-m4-evidence.md`. Full canonical gates and generated-SQL behavioral mutation controls remain required before M4 completion; this acceptance record alone does not assert they ran.

## Lead continuation on 2026-10-14: committed repairs and validation still incomplete

M4 remains in progress. No delivery signal has been sent. The primary tree is committed through `c9c8dcb2`; production code was not changed in this continuation. Scratch artifacts are local and may be absent after a fresh worker reseed; the observations below preserve commands, revisions, outcomes and limitations rather than asserting that an old log is still present.

### Committed test repairs and review

- `fd6591df` strengthens the six-renewal test: boot insertion is checked before heartbeat; each heartbeat/claim cycle and same-generation register shortens prior expiry to 30 seconds, captures it, and requires renewal to advance it while preserving exact first-seen. Lead command: `go test -buildvcs=false -count=1 -v -race -p 1 -timeout 240s ./internal/workersvc -run '^TestIssue2009M4SixRenewalsPreserveExactAgeLiveDB$'`. Fresh owned Postgres 17, exit 0, cap0/cap2 pass, zero skips, package 9.319s, wall 340.326914831s (including compilation/migration), log `gate-log.K0vdbH`. Reviewer, tester and auditor returned clean scoped reviews of `b808d2b8..fd6591df`.
- `f48e1157` adds `TestIssue2009M4IdentitySQLWorkerContextLiveDB`: both worker mutexes are held in UUID order, a real owner ledger and ordered parent/target locks are obtained, a matching-owner write succeeds, and changing only the reporting worker must write zero rows. Lead named-test command has the same flags above; exit 0, zero skips, package 5.789s, wall 115.178669848s, log `gate-log.3Jc9k5`. Reviewer, tester and auditor returned clean scoped reviews of `fd6591df..f48e1157`. One tester delegation was cancelled; its replacement completed the static review. A tester's additional API gate stopped at a relative lint-cache setup error after preliminary checks; it was not a complete gate and is not canonical validation.
- Reviewer also closed historical M3 cleanup coverage for `a8d01d69..ef47589b`: removal of redundant JSON round-trip assertions retained explicit zero/false/omission type checks. No findings.
- `c9c8dcb2` fixes the #1742 restart test's new-worker snapshot expectation by comparing against `{ ...fixture, pending_all: [] }`. The shared older-worker fixture retains omission of the optional field. Lead `node --import tsx --test --test-timeout=120000 test/issue-1742-restart.test.ts` with private TMPDIR: exit 0, 3 pass, 0 cancelled/skipped, wall 5.563652193s, log `gate-log.YWeDl3`. Reviewer returned a clean scoped review of `f48e1157..c9c8dcb2`.

Earlier M4 reviewer at `6ddf16e0` raised two notes: weak renewal/boot assertions are addressed by `fd6591df`; CI critical-path/queue/cache timing remains unmeasured and is deferred. No CI timing or execution is claimed.

### Generated-SQL behavioral mutation controls

Lead exported fixed commits under an owned scratch slot, changed only the named generated SQL constant in each fresh export, compiled with `go test -race -p 1 -run '^$' ./internal/workersvc`, then executed `go test -race -p 1 -count=1 -v ./internal/workersvc -run <selection>`. Environment: `GOFLAGS=-buildvcs=false`, `GOMAXPROCS=2`, fresh session-owned Postgres 17 per bounded batch, synthetic DB/user `uzi`, trust auth, loopback-only random port and test-database marker. No Git ran in exports and no primary SQL was edited.

| Control | Source | Compile exit | Assertion exit | Compile / test wall seconds | Test log |
|---|---|---:|---:|---|---|
| aggregate-owner | fd6591df | 0 | 1 | 134.827 / 5.600 | .uzi/scratch/gate-log.xspn0fq0 |
| aggregate-generation | fd6591df | 0 | 1 | 131.246 / 5.398 | .uzi/scratch/gate-log.o1o2yq46 |
| aggregate-unreleased | fd6591df | 0 | 1 | 134.759 / 6.586 | .uzi/scratch/gate-log.dsxerp_2 |
| aggregate-freshness | fd6591df | 0 | 1 | 126.458 / 5.269 | .uzi/scratch/gate-log.ohyc9807 |
| aggregate-terminal | fd6591df | 0 | 1 | 129.550 / 10.286 | .uzi/scratch/gate-log.301g62j8 |
| health-owner | fd6591df | 0 | 1 | 130.386 / 5.175 | .uzi/scratch/gate-log.cnbq8xpv |
| health-generation | fd6591df | 0 | 1 | 136.306 / 5.957 | .uzi/scratch/gate-log.g4bgbjrx |
| health-unreleased | fd6591df | 0 | 1 | 162.255 / 6.096 | .uzi/scratch/gate-log.xnbciwor |
| health-freshness | fd6591df | 0 | 1 | 131.299 / 5.662 | .uzi/scratch/gate-log.m6b49sco |
| continuity-freshness | fd6591df | 0 | 1 | 149.214 / 18.983 | .uzi/scratch/gate-log.3k81mqw6 |
| replacement-generation | fd6591df | 0 | 1 | 148.803 / 5.702 | .uzi/scratch/gate-log.ru098quq |
| write-owner | f48e1157 | 0 | 1 | 120.620 / 10.226 | .uzi/scratch/gate-log.44itnfhh |
| write-generation | fd6591df | 0 | 1 | 124.013 / 4.929 | .uzi/scratch/gate-log.wds51z10 |
| write-unreleased | fd6591df | 0 | 1 | 121.258 / 5.094 | .uzi/scratch/gate-log.v8izv7_d |

Aggregate mutations fail with count 1 where count 0 is required; health mutations fail with “ineligible stored evidence raised outcome health”. Continuity and replacement controls fail their persisted-age/tuple-membership assertions. Write-generation and write-release controls fail actual insertion/renewal assertions.

The first route-only write-owner probe at `fd6591df` survived (compile 0, test 0), retained in `m4-lead-mutation-results.json`. Recovery capture already excludes foreign-owned rows, so it masked that one SQL predicate. This was a test-coverage gap, not a production-owner bypass. The new query-contract test at `f48e1157` isolates the predicate: removing `r.worker_id = $1` produces exit 1 at “foreign frozen owner accepted in reporter context: rows=1 err=<nil>” (`gate-log.44itnfhh`). Production SQL did not change.

After restoration, `go test -race -p 1 -count=1 -v ./internal/workersvc -run '^TestIssue2009M4|^TestPendingAll'` passed at `f48e1157`, zero skips, package 18.838s, wall 194.268s, log `.uzi/scratch/gate-log.wu2szcn5`. Fourteen required SQL controls are killed by behavioral assertions; compilation errors are not counted. Three additional optional wire-mutation recipes in the scratch helper were not run; their ordinary validation/enclosing-rejection tests passed in the pristine selection.

### Matched original M1 API/CLI runtime

Current production archive `f48e1157cbf82f605db8c12cb57ba7da0bb1bfaa`; pinned regression archive `25d59524d33d2b74156cad143956d04d9ca509e8`. Both had identical api/go.mod and go.sum. The pinned archive received original `19c7e1eb` worker_test.go and issue_2009_pending_health_livedb_test.go; no production/helper adaptation. The selected current CLI function bodies and complete health regression file were byte-identical to those original tests. No dependency installation.

Both used the same toolchain, `GOMAXPROCS=2`, flags, absolute scratch TMPDIR and separate fresh owned Postgres 17 fixtures. Warm-up runs of `TestHeartbeatStoresValidSnapshotLiveDB` and `TestReportedRunsCell` migrated each database and compiled each package before measurement; all four warm-ups passed. Warm-up wall times were current health 150.403s / CLI 79.653s, pinned health 137.420s / CLI 48.052s, excluded from the comparison.

Exact measured commands from each archive's api directory:
~~~sh
go test -buildvcs=false -count=1 -v -race -p 1 ./internal/workersvc -run '^(TestIssue2009PendingAllSustainsHealthAcrossRotationLiveDB|TestPendingSinceStableAcrossRenewalsAndFlagsLiveDB|TestPendingSinceResetsLiveDB|TestPendingSincePreservedByRegisterLiveDB|TestPendingOutcomeHealthRequiresCurrentOwnerGenerationLeaseLiveDB|TestHeartbeatStoresValidSnapshotLiveDB|TestHeartbeatIgnoresInvalidSnapshotLiveDB)$'
go test -buildvcs=false -count=1 -v -race -p 1 ./cmd/uzi -run '^(TestIssue2009PendingAggregateJSON|TestReportedRunsCell|TestWorkerListShowsReportedRuns)$'
~~~

| Selection | Exit | Wall seconds | Package seconds | Log |
|---|---:|---:|---:|---|
| Current health | 0 | 8.116865177 | 2.813 | gate-log.mJbipK |
| Pinned health | 1 | 8.164161844 | 2.616 | gate-log.mhRz3Z |
| Current CLI | 0 | 4.997832062 | 1.298 | gate-log.SLtEBu |
| Pinned CLI | 1 | 4.496836351 | 0.388 | gate-log.pM8nzK |

Pinned exits are expected pre-fix assertion failures, not compilation/setup failures. Current selections pass. These are single local samples with warm compiler/package caches; failed baseline assertion/reporting paths differ. They do not establish CI runtime, critical-path impact, queue/cache/rerun cost or a general performance gain/loss. Worker matched measurements remain recorded separately in agent/issue-2009-m4-evidence.md.

### Canonical agent gate failure and triage

An earlier release delegation was cancelled before reporting a result; its partial `gate-log.Qgg5In` has no final exit status and is unverified.

Lead `task gate:agent` at `f48e1157` returned **201**, wall **3358.688908193s** (55m58.689s), log `gate-log.pWGAio`. It did **not** reach the 60-minute command deadline, so the conditional shard-composition fallback has not been invoked. Unit summary: 14,384 tests, 14,231 pass, 138 fail, 0 cancelled, 15 skipped; the wrapper recorded unit 3337s/exit 1 and zero real-procfs skips. Codex M4 was not reached. Failed files: issue-1742-restart, issue-2021-recovery-pin-cleanup, issue-2263-recovery-pin-cleanup, recovery-inventory-clone, recovery-inventory and terminal-rejection-retention.

TMPDIR was initially the provisioned scratch root (mode 2770, different group from command processes). Git recovery fixtures inherit that setgid group; `agent/src/git.ts`'s ownership checks reject unsafe group-writable bare directories. Narrow candidate/base command:
~~~sh
TMPDIR=<layout> node --import tsx --test --test-timeout=120000 --test-name-pattern='^a later valid journal supersedes an earlier empty value$' test/recovery-inventory-clone.test.ts
~~~
At current `f48e1157` and task base `ed2f53b75052422fa6cb91fb855e23bdaeb35152`, inherited-layout runs fail (exit 1), private-layout runs pass (exit 0), with real assertions and zero skips. Logs: candidate inherited ik3Fh0/private D2XaXH; base inherited TvKXFC/private 9VaUWX. The test and Git implementation are unchanged by this issue.

Private layout is created inside scratch with `mktemp -d .uzi/scratch/gate-tmp.XXXXXX`, `chmod 0700` and `chmod g-s` on that owned subdirectory only. The provisioned scratch root and production safety guards are unchanged. Full six-file focused comparison with `--test-concurrency=1`:
~~~sh
node --import tsx --test --test-timeout=120000 --test-concurrency=1 test/issue-1742-restart.test.ts test/issue-2021-recovery-pin-cleanup.test.ts test/issue-2263-recovery-pin-cleanup.test.ts test/recovery-inventory-clone.test.ts test/recovery-inventory.test.ts test/terminal-rejection-retention.test.ts
~~~
Base private: 489 pass/0 fail/0 skip, exit 0, wall 65.655428372s (`gate-log.W9dMGB`). Candidate private: 488 pass/1 fail/0 skip, exit 1, wall 69.765246888s (`gate-log.lgxrIk`). The remaining failure was the changed-wire #1742 equality expectation, fixed and focused-tested in `c9c8dcb2` above. The original failed full gate remains a reported failed check with its base/environment evidence; it is not silently converted to green.

### Next validation work

Repeat `task gate:agent` over the committed frozen tree using a private non-setgid scratch TMPDIR, then serially run `task gate:api` with UZI_TEST_DATABASE_URL unset and an **absolute** private GOLANGCI_LINT_CACHE, `task gate:web`, `task gate:repo`, `task check:changelog-entry`, and `./e2e/run-store-it.sh` with absolute scratch TMPDIR. Each foreground command needs its own mktemp scratch log, observed exit and elapsed time; assert the LiveDB sweep ran with zero skips. All these completion results remain pending.

If the next full agent attempt reaches the command deadline, do not repeat it whole: use every gate prerequisite, both serial CI shard legs with matching JUnit names, `task test:codex-m4`, and `task check:agent-shards`. Preserve both JUnit files and success-only `shard-1.codex-m4` containing `passed` (CI ci.yml:660–670; checker requires this marker). No test, timeout, dependency, workflow or production change. Apply the user's isolated candidate/base unchanged-code failure rule and report an incomplete composition accurately if applicable.

After final gates, update this record to actual results, review any new committed evidence/fix ranges, mark M4 complete, checkpoint as instructed, and only then signal delivery. No linked PRD exists. Include accepted rejected-enclosing-snapshot/fleet/ceiling limits from ADR 2009 and deferred CI timing note in final PR summary.
