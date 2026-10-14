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

### Completed canonical validation (2026-10-14)

All completion checks below ran serially in the foreground on the clean frozen commit `e749ba45dafd8873842117f50129bae837b081ed`. Subsequent changes are verification documentation only. Each completed command has a separate scratch log with observed exit and elapsed nanoseconds. GOMAXPROCS=2, UZI_TEST_DATABASE_URL unset, absolute GOLANGCI_LINT_CACHE, and an owned mode-0700/non-setgid TMPDIR under absolute worktree `.uzi/scratch` were used. The LiveDB script also used that absolute private scratch subdirectory rather than the provisioned setgid root; this environment-only adjustment preserves the fixture ownership control above. No dependencies, workflows, test timeouts or production behavior changed.

The corrected `task gate:agent` attempt (`gate-log.pCa9ik`) reached the 60-minute command deadline and the runtime reaped its process tree. This is an **interrupted check, not a pass**; no shell EXIT footer was produced. Its unit stage completed with 14,369 passes, zero failures, 15 skips and zero real-procfs skips (unit duration 3,493,973.706576ms). The following M4 stage was interrupted. Following the user's deadline contingency, the full command was not repeated; every prerequisite and both CI shard legs were run once, serially, with their own logs:

| Command | Exit | Wall seconds | Scratch log suffix |
| --- | ---: | ---: | --- |
| task deps-check:agent | 0 | 1.876336803 | 3bZ9oA |
| task lint:agent | 0 | 0.865347141 | EuHWyD |
| task deadcode:agent | 0 | 5.565339409 | xGWOQs |
| task typecheck:agent | 0 | 7.557444954 | 2FvS7V |
| UZI_AGENT_TEST_SHARD=1/2 UZI_AGENT_TEST_JUNIT=shard-1.xml task test:agent:shard | 0 | 1955.174128653 | 6S44mq |
| UZI_AGENT_TEST_SHARD=2/2 UZI_AGENT_TEST_JUNIT=shard-2.xml task test:agent:shard | 0 | 1311.904290614 | 3JrIPl |
| task test:codex-m4 | 0 | 114.635468091 | qpHeGb |
| UZI_AGENT_SHARD_REPORT_DIR=<absolute scratch reports directory> task check:agent-shards | 0 | 0.445462131 | XxoCaJ |

Shard 1: 7,981 passes, zero failures, one skip. Shard 2: 6,388 passes, zero failures, 14 skips. Both report zero real-procfs skips. M4: 183 passes, zero failures/skips; completeness satisfies the required [claude/U, codex/U, codex/P] matrix. Both JUnit reports are retained in `.uzi/scratch/m4-composition-reports`; `shard-1.codex-m4` contains `passed` and was written only after M4 exit 0. The checker confirmed all 476 tracked agent test files ran exactly once across the two shards. The **serial composition passes**; it does not erase the interrupted full command or the earlier failed candidate recorded above. No suspected contention-flake exception was needed.

| Remaining canonical command | Exit | Wall seconds | Scratch log suffix |
| --- | ---: | ---: | --- |
| task gate:api (UZI_TEST_DATABASE_URL unset) | 0 | 976.426485217 | aKP4pZ |
| task gate:web, first attempt | 201 | 614.636765323 | 1boIsy |
| task gate:web, corrected Chromium environment | 0 | 533.276955605 | OCBM6Y |
| task gate:repo | 0 | 915.904196898 | LqV7RS |
| task check:changelog-entry | 0 | 0.286666602 | UC6fJd |
| ./e2e/run-store-it.sh | 0 | 1785.908526865 | mdyu58 |

The failed web gate reached browser launch but executed no browser tests: Chromium reported `FATAL: Socket path too long` for its Unix SingletonSocket under the long absolute TMPDIR. Narrow candidate/base controls from fresh archives (provisioned web/node_modules symlink, no installation) ran `npm run test:browser -- src/components/UsageCards.browser.test.tsx --reporter=verbose`: direct launch failed before tests on candidate e749ba45 (exit 1, wall 4s, CRZQS2) and task base ed2f53b7 (exit 1, wall 5s, LGMDB0). The selected test and browser config are unchanged between refs.

A scratch-only executable wrapper, selected through the existing UZI_WEB_BROWSER_CHROMIUM override, sets TMPDIR to the relative path from the browser's current directory to an owned scratch directory, then execs the exact `/opt/uzi-toolchain/bin/chromium "$@"`. All browser arguments and the provisioned binary remain unchanged; artifacts remain under worktree scratch. Wrapper controls passed the same four real browser tests with zero skips on candidate (exit 0, wall 10s, 3v1iJu) and base (exit 0, wall 9s, YM7Rzr). This observed environment correction justified the full web retry. Corrected gate:web passed 7,000 main tests and all 26 browser tests; the earlier failed check remains reported.

The API gate passed including pinned sqlc drift validation and API tests. The repository gate and changelog entry check passed. The integration script provisioned its throwaway Docker Postgres (`uzi-store-it-143445`) and ran the seven-package LiveDB sweep with race/p1/count1: store, handler, forgesvc, schedsvc, workersvc, recovery and poller. Script summary: **2,810 top-level LiveDB tests passed, zero skipped**. Independently counted verbose output: 7,306 RUN/PASS cases including subtests, zero SKIP and zero FAIL. Thus this is executed LiveDB evidence, not merely exit zero.

Local runtimes do not establish CI timing or execution; green required PR CI remains a merge prerequisite. No linked PRD exists. Final delivery retains the accepted rejected-enclosing-snapshot/fleet/ceiling limits from ADR 2009 and the deferred CI timing note from the reviewer of 6ddf16e0. All implementation and repair ranges were reviewed as recorded above; this final evidence-only range still requires its own review before completion/checkpoint.
