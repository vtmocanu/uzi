# PRD #2013: Bound a Claude run's Go cache growth inside a long implement turn

**Status**: Proposed (design run complete; implementation pending owner acceptance)
**Issue**: #2013
**Related**: #1809, #1829, #1783, #1830/#2072, #2206, #2471, #2964, #3076/#3675, #2969, #3329; epic #2026
**Priority**: High (high-priority architecture review pending)
**Builds on**: `adr/1809-per-run-cache-bounds.md` (per-run caches, named subtrees, descriptor-pinned removal, soft and hard layers), `adr/1783-run-quiescence-and-attempt-clone-paths.md`, `adr/2396-worker-memory-guard.md` (the guarded command lane), `adr/3076-codex-command-cache-budget.md` (the Codex rotation precedent).

This PRD is the output of a design-only run. It records measurements, evaluates the issue's four options, and chooses one for a separate implementation dispatch. It changes no runtime code.

## Problem

ADR-1809's soft layer trims a run's rebuildable caches (`.cache/go-build`, `go/pkg/mod`, `.npm/_cacache`) only at turn boundaries, because only there can it prove that no process is using them. A Claude lead can keep one implement turn open for hours. Inside that turn, every Go edit writes new build-cache entries and supersedes old ones, and nothing removes them. The volume reaches the hard layer, which parks the run, and repeated parks fail it.

Owner-reported evidence (from the issue body and comments; owner-reported, not verified by this run):

- Run `2dc4d842` (PRD #1909): 25 GiB volume, 2 concurrent runs, soft cap about 6.25 GiB per run. A hard stop at used_fraction 0.878 with cache_bytes 21,948,706,816; the run resumed in a fresh directory without context, and about 2 h after its caches were dropped it hit a second hard stop with cache_bytes 21,943,070,720. Those windows held 41 exports or worktrees and 58 `-race` invocations.
- 2026-10-03: changing only the export path added about 750 MB per `-race` build (api, go1.27.1); a same-path control added nothing. `-trimpath` restored reuse but broke `TestMirrorsWebPipelineBadge`. Stable export slots shipped (#2206).
- 2026-10-14, after #2206: run `84ed3050` grew from 3.7 to 17.7 GB in about 2 h with a single source root (about 60 explicit `go test`, 25 with `-race`, 10 source mutations and 7 SQL/sqlc mutation attempts in the first window). At the 23:48 trim go-build was 6.9 of 10.7 GB. The second park failed the run (retained recovery blocked, budget exhausted). A live run, `11e97b82`, held about 10 GB of caches: go-build about 7 GB, `go/pkg/mod` about 2.8 GB (including sqlc-dev 544 MB and the go1.27.2 toolchain 253 MB), `.npm` about 0.3 GB; about 70 package archives of 10 to 367 MB made up about 4.7 GB of go-build, with the same sizes recurring under different mtimes. Age-only trimming was reproduced as unsafe on go1.27.2.
- 2026-10-14, 40 GiB run-bound workers: the soft cap fired as designed for one run; no hard stops.
- 2026-10-16: Codex runs kept Go caches outside `/data`; since rotated (#3675).
- 2026-11-03: 49 GiB volumes; a Claude run's HOME build cache of about 14 GiB drove `/data` to 90% twice; two runs reached the park cap 3/3; owner-wide custody holds stood at 13 against a limit of 8. Which components grew there is owner-reported only as "mostly `.cache/go-build`".

## Required findings

**(a) This run is guard-bound (observed).** Its shell tool was `mcp__uzi_cmd__bash`, not native `Bash`. The mechanism, verified at HEAD 620fdc36:

| Where | Verified content |
|---|---|
| `agent/src/claude-command/grants.ts` line 10 | `export const GUARDED_COMMAND_TOOL = "mcp__uzi_cmd__bash";` |
| `agent/src/claude-harness.ts` line 262 | `options.toolAliases = { Bash: GUARDED_COMMAND_TOOL };` (line 263 adds `"Bash"` and `"Monitor"` to `disallowedTools`) |
| `agent/src/sdk-executor.ts` line 1735 | `const guarded = ctx.capturedMemory !== undefined;` (decided once per flight) |
| `agent/src/runner.ts` line 14957 | `capturedMemory: () => this.captureWorkerMemory(flight),` (passed only when the flight has a worker-memory binding) |
| `agent/src/config.ts` `memoryGuardConfig`, line 578 | `const flag = env.WORKER_MEMORY_GUARD_ENABLED?.trim().toLowerCase() ?? "";` (empty means disabled) |
| `deploy/chart/values.yaml` `memoryGuard:` | `enabled: false` (the chart default) |

So a run is guard-bound only on a worker that opted in to the memory guard and whose claim the api enrolled (`runner.ts` throws "Worker memory enrollment requires the full worker claim and registration" otherwise). The guard is off by default.

**(b) Whether the recurrence runs were guard-bound: not supplied.** The owner has not supplied whether `84ed3050`, `11e97b82` or the 2026-11-03 runs were guard-bound. Also not supplied: the fleet's effective cap configuration (`WORKER_MAX_CONCURRENT_RUNS`, `UZI_RUN_CACHE_CAP_FRACTION`) and the per-turn durations of the 2026-11-03 runs. This run cannot read worker logs, fleet configuration or the cluster. These remain owner inputs (Open questions).

## Measurements (M1)

### Method

- Environment: go1.27.3 linux/amd64, `nproc` 4, `GOMAXPROCS` unset, `GOTOOLCHAIN=auto` (no toolchain download occurred), `CGO_ENABLED=1`. The inherited `GOFLAGS` was empty; `-modcacherw` was appended so the scratch caches could be deleted.
- Confinement: every experiment used its own scratch directory under the run's untracked scratch area, with `GOCACHE`, `GOMODCACHE`, `GOTMPDIR`/`TMPDIR`, `GOLANGCI_LINT_CACHE` and `GOBIN` relocated into it (plus `UZI_GOLANGCI_LINT_DIR`, `UZI_SQLC_DIR` and `XDG_CACHE_HOME` for the tool step). Sizes are `du -sb` bytes per component before and after each step. One command ran at a time, in this run's guarded command lane, on a 40G `/data` volume (one 4-CPU worker).
- Edits were made in a scratch export of 620fdc36 at a stable path (path churn excluded, per #2206). Each edit step reran `go test [-race] -count=1 -run '^$' ./...` in `api/`. A **comment edit** appends one `//` line to a non-test `.go` file; a **compiled edit** appends one unexported function. A comment-only edit barely changes object code, so it cannot establish the cost of real source churn; the compiled edits model it. Reverse-dependency counts (packages whose deps include the edited one, test variants included, an overcount): runkind 61, store 105, handler 13 (non-test: 18, 32, 3).
- Disk bound: an 8 GiB (8,589,934,592 B) per-directory bound was **sampled by a 5 s watchdog, not enforced**. The guardrail denied a process-group kill inside a command, so the watchdog could only report. In experiment A the edit series was not stopped at the bound: seven steps ran to completion past it and the directory peaked at 8,885,864,947 B (about 0.3 GB over the bound). Those seven steps' growth was not individually recorded; they were re-measured in a second scratch directory (A2b). No further producing experiments were run after M1. A future bounded experiment in this lane must stop its own series in-script when the watchdog trips (the lane forbids process-group kill), rather than rely on a kill. In experiment B the last step (race compiled handler edit) crossed the bound at 8,731,432,994 B; its figure below is derived, not a sampled post-step delta. The lowest post-step `df` reading left 16,422,146,048 B available (about 56-59% of the volume used, with two earlier scratch directories of 8.9 and 5.3 GB still on disk). The last B step and mid-step peaks were not sampled by `df`, so the true minimum was lower (about 14.8 GB available by arithmetic, itself an upper bound on the minimum because the last step's figure is the watchdog reading at the bound crossing, not its peak).
- During a step, peak `du` exceeded the post-step total by about 1 GB (transient link and temporary output).
- Cleanup: the read-only `/proc` probe was not permitted, so cleanup relied on the lane's confirmed disposal of each producing command plus `kill -0` on the awaited pids. All five scratch directories were removed in separate later commands; none were retained.
- This run's own HOME caches (governor-visible; the lead's read-only `du`/`df` samples, not in the M1 log): go-build and `go/pkg/mod` absent at the start (14:31Z) and at the end of M1; `.npm/_cacache` 298,404,002 B at both points; `/data` used 1,745,502,208 B of 41,956,900,864 at the start and 1,748,852,736 after M1 (which was also the start of M3), and 1,757,282,304 at 16:10Z after M3 and M4, with go-build and `go/pkg/mod` still absent and `.npm/_cacache` still 298,404,002 B. M1 left the run's real caches untouched, and M3/M4 (document authoring and review) added no run caches.

### Cold working set, `api/`, non-race (experiment A)

| Step | Time | go-build delta | `go/pkg/mod` delta | Post-step total | Peak during step |
|---|---|---|---|---|---|
| 1a `go build ./...` | 73 s | +527,047,270 (0.53 GB) | +347,260,013 (0.35 GB) | 874,311,476 | 1,527,598,381 |
| 1b `go vet ./...` | 118 s | +794,305,289 (0.79 GB) | +0 | 1,668,616,765 | 2,469,551,968 |
| 1c `go test -count=1 -run '^$' ./...` | 99 s | +680,537,558 (0.68 GB) | +0 | 2,349,161,640 | 3,415,400,324 |
| 1c control (same command, same path) | 26 s | +0 | +0 | 2,349,168,957 | 2,423,191,511 |
| 2 test compile at a new (export) path, warm | 217 s | +1,611,535,494 (1.61 GB) | +0 | 4,115,297,748 | 5,721,716,055 |

Cumulative cold go-build after 1a to 1c: 2,001,890,117 B (2.0 GB). The control confirms full reuse at a stable path; step 2 qualitatively matches the owner's path-churn observation (a new path reused little); the magnitudes differ (owner: about 750 MB per `-race` build; here 1.61 GB for a non-race test compile).

### Cold working set, `api/` with `-race`, and `controller/`

| Step | Time | go-build delta | `go/pkg/mod` delta |
|---|---|---|---|
| B 1d `api` race test compile, cold (fresh cache, export path) | 395 s | +2,236,732,867 (2.24 GB) | +347,260,013 (0.35 GB) |
| B 1d control | 57 s | +0 | +0 |
| C c1 `controller` test, cold, non-race | 82 s | +790,105,000 (0.79 GB) | +171,128,398 (0.17 GB) |
| C c2 `controller` test, `-race` (same cache) | 104 s | +859,894,304 (0.86 GB) | +0 |
| C c3 race control | 10 s | +0 | +0 |

### Growth per edit, `api/` test compile (experiments A, A2b, B)

Each comment edit was repeated three times; the deltas repeated to within a few hundred bytes, so superseded entries accumulate linearly.

| Package (revdeps, test-incl) | Edit | Non-race delta per edit | `-race` delta per edit |
|---|---|---|---|
| `internal/runkind` (61) | comment | +329,786 (0.33 MB), 21-26 s | +386,592 (0.39 MB), 52-57 s |
| `internal/runkind` | compiled | +1,231,293,381 and +1,231,294,931 (1.23 GB), 158-164 s | +1,391,750,177 (1.39 GB), 260 s |
| `internal/store` (105) | comment | +43,290,738 (43 MB), 26-27 s | +57,552,209 / +57,552,034 (58 MB), 62 s |
| `internal/store` | compiled | +1,528,729,762 (1.53 GB), 191 s (A2b) | +1,725,414,898 (1.73 GB), 301 s |
| `internal/handler` (13) | comment | +311,221,978 / +311,221,803 (311 MB), 57-62 s (A2b) | +349,685,988 / +349,685,813 (350 MB), 119-125 s |
| `internal/handler` | compiled | +397,716,529 (0.40 GB), 68 s (A2b) | about +1,652,715,818 (1.65 GB), derived: directory size at the bound (8,731,432,994) minus the previous total (7,078,717,176); includes transient output, not comparable |

A2b ran in a second scratch directory seeded at the export path (+1,980,279,999 B go-build, +347,260,013 B `go/pkg/mod`, 259 s; the log does not record the seeding command beyond its label).

Reading, marked by kind:
- [Measured] A compiled edit to a widely imported package costs 1.2 to 1.7 GB of go-build per edit; a compiled edit to a top-level package costs about 0.4 GB. Comment edits cost 0.3 MB to 350 MB.
- [Inference, not verified] Comment-edit cost tracks the edited package's own archive size (a new archive entry is written although the object code barely changes), while compiled-edit cost tracks the number of importers whose test binaries must be relinked. The follow-up should classify go-build entries (package archives versus link outputs and executables) to confirm this.
- [Measured] Per edit, a `-race` rebuild costs about 12-13% more than the same non-race rebuild (runkind compiled 1.39 vs 1.23 GB; store compiled 1.73 vs 1.53 GB; handler comment 350 vs 311 MB). The cold figures are not a like-for-like pair (2.24 GB is a race test compile alone; 2.00 GB is non-race build, vet and test compile together).
- [Measured] Race and non-race builds write separate cache entries: `controller` `-race` on a cache already warm from the non-race build added 0.86 GB, more than the non-race build's own 0.79 GB. So a loop that runs both modes after each edit pays both costs (for example about 1.23 + 1.39 = 2.6 GB per runkind compiled edit). The owner-reported trace ran both (about 25 of 60 `go test` used `-race`).

### Tools

| Step | Time | go-build delta | `go/pkg/mod` delta | Other |
|---|---|---|---|---|
| `golangci-lint run` on `api/` (repo script, fresh cache) | 333 s | +2,006,213,058 (2.0 GB) | +347,260,013 | golangci-lint's own cache +1,693,691 (1.7 MB) |
| `golangci-lint` on `api/` again, unratcheted (warm cache) | 6 s, rc=1 (attributed by the tester to backlog findings; the log records only rc=1) | +0 | +0 | |
| sqlc via `scripts/sqlc.sh` (binary, `version`) | 5 s | +0 | +0 | binary +15,075,333 (15 MB) |
| sqlc via `go run` (not what the repo does) | 87 s | +395,136,341 (0.40 GB) | +324,173,625 (0.32 GB) | |
| `go install` govulncheck | 11 s | +33,590,920 | +18,243,939 | binary 16,055,363 |
| `go install` deadcode | 5 s | +23,295,181 | +15,593,531 | binary 17,416,497 |

`scripts/golangci-lint.sh` and `scripts/sqlc.sh` fetch pinned, sha256-verified release binaries; neither builds the tool with `go run`. The lint step's 2.0 GB go-build growth is the api build graph lint loads, not lint's own cache (1.7 MB). The owner's 544 MB sqlc-dev entry in `go/pkg/mod` is consistent with an earlier `go run` invocation; the current script fetches a binary.

Not measured: npm growth (only point-in-time `.npm/_cacache`, 298,404,002 B at start and end, unchanged; the worker's dependency install had already run, and a fresh install needs the network and would grow the governor-visible cache); web and agent caches; `lint:controller`; `sqlc generate` (writes files).

### Limits

- One 4-CPU worker; go1.27.3 only.
- Synthetic edits, not real source churn; three packages only.
- No hosted fleet data; owner evidence is quoted, not reproduced.
- npm growth not attributed (above).
- The disk bound was sampled, not enforced, with the overshoot described under Method.

### Attribution reading

- A cold `api` working set (build, vet and test compile) is about 2.0 GB go-build plus 0.35 GB `go/pkg/mod`; the `-race` test compile adds about 2.24 GB; `controller` adds about 1.65 GB plus 0.17 GB for both modes. The sum of every measured cold component plus `.npm/_cacache` is 6,705,414,701 B (6.2 GiB), excluding lint's overlap with the build cache (not measured). That fits comfortably under a 12 GiB cap (49 GiB volume / 2 runs x 0.5, the `UZI_RUN_CACHE_CAP_FRACTION` default). It is close to the 6.25 GiB cap of the 25 GiB / 2-run case.
- For the measured command set only, a warm working set stays well under a 12 GiB cap, and the observed growth is explained by accumulated superseded entries, which only a trim can reclaim. This does not bound real turns: npm growth, other components (web, agent, other tools) and lint's overlap with the build cache were not measured, so a larger working set in a real turn is not excluded.
- Consistency check, not proof: at the owner-reported cadence (about 60 explicit `go test` in about 2 h, with 10 source and 7 SQL mutations), a per-compiled-edit growth of 0.4 to 1.7 GB is enough to explain growth from 3.7 to 17.7 GB without any path churn.

## Options evaluated

1. **Guarded-lane quiet-point trim (hold-and-drain), triggered by the governor.** Chosen (D1). The guarded lane already routes every Bash call of the lead and its subagents through one per-run object (it admits up to 8 concurrent commands, `lane.ts` `MAX_CONCURRENT_COMMANDS`), so it can offer a quiet point: no command past launch, new commands waiting rather than refused (up to the concurrency cap; see the review constraints). The governor trims through the existing pinned helpers inside that window and reopens. The trim is oldest-mtime-approximate: Go refreshes an entry's mtime at most hourly, so a frequent trim can evict some hot entries; it keeps the working set unlike a wholesale drop or a rotation, provided the low-water mark exceeds the working set (review constraint 7).
2. **Rotation (the Codex #3675 variant).** Rejected (D2). The Codex design relies on per-command cache variables from a reservation, a per-generation drain proof and Landlock exact-directory grants (ADR-1598). The Claude lane builds per-command environment, but `agent/src/claude-command/env.ts` says "this is parity with native Bash, not isolation", and there is no Landlock: a later command can name an older generation by path, so per-generation isolation alone cannot prove that a retiring generation is unused. A variant that deletes retired generations only at the same full-lane quiet point as option 1 is not ruled out on safety grounds, but it adds generation bookkeeping on top of option 1's quiet point and discards the hot cache each rotation, for no safety gain over trimming at that quiet point. It would also require an ADR-1809 D2 amendment, because `isRunCacheSubtree` (`agent/src/rmtree.ts`) matches the exact names in `RUN_CACHE_SUBTREES`, the go-build lister hard-codes `.cache/go-build`, and the park drop, periodic reclaim and the `rmHomeSubtree` refusal key on the fixed list.
3. **Guidance, flag or layout remedy.** No guidance-only remedy was shown to be sufficient (D4); the measurements do not establish that none can be, since affected-package testing is unmeasured. One lever is real. Running `-race` only at the final gate rather than after every edit roughly halves per-edit growth for a loop that runs both modes, because race and non-race entries are separate; it is advisory and does not bound growth. Otherwise: per-edit `-race` overhead alone is only about 12-13%; compiled non-race edits still cost 0.4-1.5 GB each, so real churn is the driver and no flag removes it; stable paths (#2206) already shipped; sqlc and golangci-lint are already binaries. One candidate for later guidance, **unmeasured**: test only the affected packages rather than `./...`, which would reduce relinking per edit. It is advisory and does not bound growth.
4. **No safe point; shorter turns.** Rejected (D3). `max_iterations` limits the number of turns, not their length: `agent/src/sdk-executor.ts` line 1659 reads `let maxIterations = positive(ctx.config?.max_iterations, DEFAULT_MAX_ITERATIONS);` with `DEFAULT_MAX_ITERATIONS = 5`, scaled by the api up to 60 (`api/internal/workersvc/budget.go`, "run_max_iterations*milestoneBudgetCap = 60 iters / 24h on defaults"). The lead decides when a turn ends. Forcing turn ends would make `quiesce` abort live commands (a 40-minute gate would be lost). More boundaries also mean more boundary trims; but when a background process keeps blocking the trim, each blocked boundary still increments the soft-park streak (`cache-cap.ts`, `run.overStreak += 1` after a boundary that ends over the cap), so shorter turns reach the three-boundary preventive park sooner.

Not evaluated: a worker-owned `GOCACHEPROG` with per-process leases on cache entries (the issue lists it as optional and unverified). This run has no evidence for or against it.

## Chosen design (for the follow-up)

Facts it rests on, verified at HEAD 620fdc36:
- The soft layer runs only at turn boundaries: `agent/src/sdk-executor.ts` line 3322, `const capVerdict = await ctx.cacheCapBoundary?.(() => this.runProcessAlive());`. `runProcessAlive` (line 5490) is documented "Only meaningful between turns": mid-turn the live CLI makes it always true.
- The cap: `agent/src/cache-cap.ts` line 112, `return Math.floor((volumeTotalBytes * fraction) / Math.max(1, maxConcurrentRuns));`. The streak: `SOFT_PARK_BOUNDARIES = 3` (line 75), `run.overStreak += 1;` (line 394).
- One lane per guard-bound run, built once (`lane = new ClaudeCommandLane({` at `sdk-executor.ts` line 1876); `quiesce` aborts live commands (`agent/src/claude-command/lane.ts` line 278, `live.ac.abort(new Error("lane quiesce"))`); the live set is `private readonly live = new Map<string, LiveCommand>();` (line 197). The lane is foreground only (`agent/src/claude-command/server.ts` line 21, "Foreground only: run_in_background is refused …").
- The memory-pressure path already waits rather than refuses: `lane.ts` line 404, `commands.waitForPressure(context, { deadlineAt, phaseRevision, signal, now: this.now })`, then re-validates (line 409) and re-screens.
- Deletion allowlist and helpers: `agent/src/rmtree.ts` line 279, `RUN_CACHE_SUBTREES = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"]`; `trimRunCaches` in `agent/src/run-caches.ts` line 248.
- The background dependency install is joined before the implement roster (`depsJoined = true` in `sdk-executor.ts`), so it is live only during planning.

The contract, which the follow-up implements (names are illustrative; the review constraints 1-9 below amend it):

- **Lane quiet-point primitive.** The lane offers a quiet point; trim policy stays in the governor. Inputs: an arm deadline and the caller's trim function. Outcome: either "quiet" (admission closed, zero launched commands; the caller runs its trim, then the lane reopens) or "gave up" (nothing deleted). The primitive refuses to start when the lane is poisoned, the turn is not open, authority is lost, or a quiet point is already in progress.
- **Counter.** Quiet means a new `launched` counter is zero: commands past the launch gate. Not `live.size`, which also holds pressure waiters and hold waiters; a `live.size` predicate would deadlock.
- **Gate placement.** One gate immediately before `this.launch(` (`lane.ts` line 428), after the deny hooks, screening, admission and the pressure wait. `launched` is incremented synchronously there, so closing admission and the drain check are atomic on the event loop. Order: pressure wait first, then the hold gate, as one re-validated chain.
- **Waiters.** Waiters follow the pressure-wait pattern: the same abort sources, and on wake the same re-validation and re-screen. The wait is charged to the command's own deadline, as the pressure path does, because the transport bound leaves no slack: `worstCaseCallMs` for the 3,600,000 ms maximum against `GUARDED_SERVER_TIMEOUT_MS = MAX_COMMAND_TIMEOUT_MS + 60_000`.
- **Arming.** Opportunistic: the hold begins at the first moment `launched` is zero, within a bounded arm period; otherwise give up. Whether to close admission actively after the arm period is an open question.
- **Second proof inside the hold.** The lane proves only its own command trees drained. Inside the hold the trim also requires a process scan: the existing run-process scan, excluding only the exact Claude CLI root pids. Any other attributed process counts as alive, and an incomplete scan counts as alive. The scan is point-in-time: it does not stop the live CLI from starting a detached child after the scan, while listing or eviction runs. The follow-up must resolve this before implementation: either a barrier that holds for the whole listing-and-eviction interval, or mid-turn trimming stays disabled wherever that proof is unavailable. A second scan after eviction is not sufficient.
- **Reopen.** Reopen only after the trim helpers have exited. Under the uid split the worker cannot kill a blocked foreign-uid helper (`rmtree.ts`: "the worker cannot kill a blocked foreign-uid helper under the split"), and a timeout only rejects the promise. If exit is not confirmed within a bound, keep admission closed and take the existing preventive park (fail closed).
- **Turn close and run join.** `quiesce` joins an in-progress trim and never reopens mid-trim. The trim joins `joinOwnedOperations`, so a park's cache drop never overlaps it.
- **Trigger.** A mid-turn leg of the governor, beside the existing sampling (`agent/src/main.ts`: `onSample: (usedFraction, sampledAtMs) => diskGovernor.observe(...)`). It runs only for runs that declare the capability: guard-bound, implement phase, turn open, dependency install joined. It measures at a minimum interval (proposal: 2-5 min, to be measured) and requests a quiet point when over the cap. It trims under a mid-turn deadline (proposal: about 60 s, to be measured; today's `TRIM_DEADLINE_MS = 3 * 60_000` is too long against a 120 s default command deadline).
- **Serialization.** One single-flight lock per run across the boundary trim, the mid-turn trim and the hard layer's `reclaimInPlace`, which can already overlap the first implement turn (the approve-to-first-report window in `cache-cap.ts`).
- **Streak.** The mid-turn leg never writes `overStreak`; giving up is a log line only. After a successful mid-turn trim the next boundary resets the streak through its existing branches.
- **Rollout.** Worker-local only: no api, schema or protocol change, and no new capability. A mixed fleet is safe in either rollout order, because old workers keep boundary-only trim. The cross-flight helper-exit rule (F3b) relies on a resume on the same HOME running on the same worker (the HOME is worker-local); a resume elsewhere starts with a different HOME (inferred; F3b verifies).

Riskiest assumption: mid-turn on a drained guarded lane, the Claude CLI has no persistent attributed children (other than its own root). If that is false, the scan never passes, the mid-turn trim never fires, and the behaviour falls back to today's boundary-only trim. F1 validates it first.

Coverage limit: only guard-bound runs benefit, and the guard is off by default (D5).

### Constraints from the design review (for the follow-up)

1. **Counter bookkeeping.** The `launched` counter (rename it: `launched` is already a local in `lane.ts`) is incremented in the synchronous stretch between the last await (`waitForPressure`) and `this.launch(`, and decremented only on the confirmed-dispose path or the never-started path, never on abort. If pressure is held again after a hold wait, the command loops back to the pressure wait or is refused.
2. **Deadline.** The hold branch sets `deadlineAt = startedAt + timeoutMs` as the pressure branch does, so `supervise` gets only the remaining time; the worst case leaves about 13 s of slack under the MCP server timeout (3,660 s against 3,647 s), so an uncharged hold would overrun it. The tool result should report how long a command waited.
3. **Concurrency cap.** Hold waiters count toward the lane's 8-command cap (`live.size`), so arrivals beyond it are refused, not queued; F2 tests this and the follow-up decides whether to accept it.
4. **Helper exit across flights (mandatory).** "Reopen only after the trim helpers exit" must hold across park and resume, not only within this lane: until a started foreign-uid trim helper is confirmed exited, no later flight on the same HOME may admit commands (the park/resume path waits for, or refuses on, an unconfirmed helper, failing closed). Accepting this as a residual is not an option for the mid-turn leg. Today's boundary trim has the same exposure; the follow-up fixes it for both, or the mid-turn leg does not ship.
5. **Docker containers (mandatory).** The process scan covers the run's uid only, so it cannot see container processes. The design relies on the wiring instead, and the wiring does share part of `/data`: on the chart DinD tier the sidecar mounts the data PVC's `runner-shared` subPath at `/data/runner` (`controller/internal/kube/render_dind.go`, `runnerWorkdirMount`, "the ONLY data mount dind may have"), so containers can bind-mount anything under the runner tree. The run HOME's caches live outside that subtree (the agent home on the same PVC), so the proof needed is cache-file exclusion under this actual topology: no container can reach a HOME cache file through the shared subtree by any alias, including a symlink or hard link created from the worktree into HOME, or a cache variable pointed into the runner tree. F1 must establish this for every supported docker wiring. On any wiring where it is not proven, the mid-turn leg is disabled for docker-wired runs: they keep boundary-only trim. Recording the exposure as a residual is not sufficient.
6. **No mid-turn `go/pkg/mod` removal.** `rmHomeSubtree` deletes in place without a rename-aside, so a walk cut off by a short deadline could leave partial module directories (whether Go then fails rather than heals is unverified), and removal forces a network re-download mid-turn. The mid-turn leg runs only trim stages (a) and (b); stage (c) stays at the boundary.
7. **Thrash.** On a 25 GiB / 2-run worker the low-water mark (0.6 x 6.25 GiB = 3.75 GiB) is below the measured cold working set (about 6.2 GiB), so each mid-turn trim would evict hot entries. A minimum gap limits how often that happens but does not prevent it. When the measured working set exceeds the low-water mark, mid-turn trims are suppressed for that configuration (it keeps boundary-only trim); the minimum gap and logging evicted bytes against the working set remain (F3).
8. **Off switch.** The mid-turn leg obeys `UZI_RUN_CACHE_CAP_ENABLED` (`specs/human.md` records an off switch for the in-run cache cap) or gets its own switch.
9. **Hard-layer latency.** The single-flight lock can make the hard layer wait up to the mid-turn trim bound; record this small latency change in F4.

## Safety invariants

Carried verbatim from the issue:

**Safety invariants the design must not weaken:**
- Never delete cache entries while a process might be reading or writing them; an unknown state counts as alive (`agent/src/cache-cap.ts`).
- File age never establishes that concurrent deletion is safe. Age may still select entries after a proven quiet point.
- Keep the session drop on an attempt-path change (`agent/src/runner.ts`, `cwd_changed_attempt_path`).
- Keep the hard layer's parkable rule.
- Delete only the named subtrees, through the descriptor-pinned helpers.
- Any hold sits after the Bash deny hook. Do not relax the lane's poison behavior.
- The hard layer's in-place drop during revision or finalize is an accepted hazard (ADR-1809 Consequences), not a precedent for implement turns.

How the chosen design meets each:
1. **No deletion while possibly in use.** Deletion requires both the lane drain (`launched` zero, confirmed disposals) and a process scan in which any attributed process other than the CLI roots, or an incomplete scan, counts as alive. On wirings where F1 proves cache-file exclusion (the daemon shares only the runner subtree, `controller/internal/kube/render_dind.go`, and no alias from that subtree reaches a HOME cache file), containers cannot read the run HOME's caches; elsewhere, docker-wired runs keep boundary-only trim (review constraint 5). A trim helper must be confirmed exited before any flight on the HOME admits commands again (review constraint 4).
2. **File age.** mtime only orders the oldest-first trim after the quiet point is proven, as today.
3. **Session drop.** Untouched (`runner.ts` line 13768, `reason: "cwd_changed_attempt_path",`).
4. **Parkable rule.** The hard layer is unchanged and serialized with the mid-turn trim through the single-flight lock; a trim interrupted by a stop is joined before the park drop.
5. **Named subtrees.** The trim uses `trimRunCaches` (with a new option limiting it to stages (a) and (b) mid-turn, review constraint 6) and the existing pinned helpers on `RUN_CACHE_SUBTREES`; no D2 change.
6. **Deny hook and poison.** The gate is immediately before launch, after every deny: the PreToolUse hook screens before recording attribution (`if (screen.denied)`, line 293), and the command handler refuses unattributed calls and re-screens (line 360) before launch. A poisoned lane refuses the hold, and poison during a hold aborts the waiters.
7. **In-place drop.** The design never drops caches mid-command; it runs the existing trim behind a proof. The hard layer's existing in-place drop (`reclaimInPlace`), which can already overlap the first implement turn through the approve-to-first-report window, stays as today and is not covered or extended by this design; the single-flight lock only serializes it with the new trim.

## Rejected, with evidence

Carried verbatim from the issue (D6):

**Rejected, with evidence.** Revisit any of these only with new evidence and the maintainer's acknowledgement:
- `-trimpath`: it breaks tests that use `runtime.Caller`;
- file-age or age-only eviction: the reproduction deleted an archive a live build was using. Separately, Go refreshes an entry's mtime at most hourly, so an old mtime does not prove the entry is unused;
- a per-subagent `GOCACHE`: subagents share the CLI's environment;
- a stable `GOCACHE` symlink to a swapped generation: Go hands archive paths on as text;
- rewriting commands in the native-Bash PreToolUse hook: it is a guardrail layer;
- a `GOCACHE` outside HOME with its own bound: ADR-1598 forbids an emptyDir `sizeLimit`, and without one the growth moves to node storage, outside the `/data` governor;
- a shared cache: ADR-1809 D1;

Optional to evaluate: a worker-owned `GOCACHEPROG` with per-process leases on cache entries (unverified).

## Follow-up implementation milestones (separate dispatch)

- **F0: Required input, npm growth attribution (unmet here).** Requirement gap: this run did not attribute `.npm/_cacache` growth. The follow-up measures it (per install and per turn) to calibrate the mid-turn stage (b) (`.npm/_cacache`) and the cap discussion, or records why it cannot; it does not reopen constraint 6.
- **F1: Real-CLI scan fixture and docker-wiring check (riskiest assumptions first).** For every supported docker wiring (compose `dind` sidecar, chart DinD tier), confirm with a fixture that a container started from a lane command cannot reference the run HOME's cache files, given that the runner subtree is shared with the daemon: cover bind mounts of the runner tree and aliases into HOME (symlink, hard link, cache variables pointed into the runner tree) (review constraint 5); record any wiring where it cannot be confirmed. During an open implement turn with the guarded lane drained, scan the run's processes and prove that no persistent attributed CLI children exist beyond the exact CLI root pids. Also measure one governor measurement pass on a realistic cache (10 to 20 GB) to calibrate the interval. If the scan fails, stop and re-plan.
- **F2: Lane quiet-point primitive with unit tests.** The counter, gate placement, waiter deadline, the concurrency cap with waiters, quiesce join (joining a trim must not trip quiesce's own deadline-poison path), poison, abort and drain atomicity.
- **F3: Governor mid-turn trigger and wiring.** The capability declaration (guarded, implement phase, depsJoined), the scan variant, the single-flight lock, the mid-turn trim deadline, stages (a) and (b) only, a minimum gap between trims, suppression of mid-turn trims when the measured working set exceeds the low-water mark (with a test), the off switch, the join in `joinOwnedOperations`, and a measurement-cost fixture. F3 also carries a mandatory barrier fixture for D13: a detached child or an escaped native `Bash` path that starts after the scan attempts to read or write the cache during listing and during eviction; the test requires the cache use to be blocked for the whole interval, or mid-turn trimming to stay disabled for that wiring. F3 also carries the issue tests "runs not bound by the guard keep today's boundary-only trim" and "the `cwd_changed_attempt_path` session drop and the hard stop are unchanged".
- **F3b: Mandatory safety conditions, with tests.** (i) The mid-turn leg is disabled for docker-wired runs on any wiring F1 did not confirm; test: such a run keeps boundary-only trim. (ii) Cross-flight helper exit (review constraint 4): a started foreign-uid trim helper that is not confirmed exited blocks command admission for any later flight on the same HOME (boundary and mid-turn trims alike); tests: a resume while a helper is still running waits or refuses and fails closed, and a confirmed exit lets the resume proceed. This widens the follow-up into today's boundary trim and the park/resume path; the owner should see that scope.
- **F4: ADR-1809 amendment.** A decision record of the mid-turn leg and its proofs, explicitly amending D4's "between turns" quiet-point definition and adding the CLI-root-pid exclusion; no D2 change; records the docker-wiring condition (constraint 5), the cross-flight helper-exit rule (constraint 4) and the hard-layer latency change. The follow-up plan carries the hosted acceptance as Owner acceptance and `full agent suite: exact-head PR CI` as a landing obligation, with a non-closing delivery until hosted acceptance.

Tests, carried verbatim from the issue:

**The follow-up implementation (separate dispatch) carries tests for the mechanism the PRD chooses. If it chooses mid-turn trimming through a synchronization point, these tests apply:**
- caches exceed the cap during one implement iteration and are trimmed back under the low-water mark without a park and without ending the turn;
- a command arriving during a trim waits for it to finish;
- a command that never drains makes the trim give up without deleting anything, with the soft-park streak unchanged;
- a hold timeout or lane poison fails closed;
- runs not bound by the guard keep today's boundary-only trim;
- `.npm/_cacache` stays kept until the dependency install is joined;
- the `cwd_changed_attempt_path` session drop and the hard stop are unchanged.

Hosted acceptance, carried verbatim from the issue:

**The follow-up's hosted acceptance:** verify the chosen remedy on a guard-bound hosted run that exceeds the cap while mid-turn trimming is eligible: it records at least one successful mid-turn trim with its duration and the hold duration, and the turn continues without cache corruption. Zero cache-driven counted parks is reported as a separate outcome; a run where the cap never triggers, working-set suppression disables mid-turn trimming, or only boundary trims run does not satisfy acceptance. Its landing obligation is `full agent suite: exact-head PR CI`.

## Out of scope

- Codex command caches: the #3675 rotation already shipped (`adr/3076-codex-command-cache-budget.md`).
- Changing the hard layer (thresholds, the parkable rule, the counted stop).
- The cap fraction policy. Input for a later decision: the measured cold working set (about 6.2 GiB for api, race, controller and npm together) fits under a 12 GiB per-run cap, but sits at the 6.25 GiB cap of a 25 GiB / 2-run volume, where a mid-turn trim would thrash (review constraint 7).
- Enabling the memory guard by default (an open owner question below, not decided here).

## Open questions for the owner

1. **Coverage.** The guard is off by default (`deploy/chart/values.yaml`, `memoryGuard: enabled: false`), so on a default deployment the chosen remedy does nothing. Should the follow-up also enable the guard by default or for the hosted fleet, or does the owner accept boundary-only coverage for non-guarded runs?
2. **Escalation of the hold.** Opportunistic only (wait for `launched` to reach zero), or actively close admission after a bounded arm period, at the cost of waiting commands possibly hitting "deadline expired before launch"?
3. **Numbers.** The 2-5 min measurement interval and the about 60 s mid-turn trim deadline are proposals, not measurements; F1 and F3 measure them. Does the owner want other starting values?
4. **Hosted inputs.** Please supply which cache components grew (including `.npm/_cacache`), whether the recurrence runs were guard-bound, the fleet's `WORKER_MAX_CONCURRENT_RUNS` and `UZI_RUN_CACHE_CAP_FRACTION`, and the per-turn durations of the 2026-11-03 runs. A later revision of this PRD folds them in.

## Decision Log

- **D1 (2026-11-03): Choose a guarded-lane quiet-point trim, triggered by the governor's mid-turn leg.** Why: the lane is the one per-run choke point all guarded Bash calls pass through, the pressure path already shows that admission can wait instead of refusing, and an oldest-mtime trim keeps most of the working set when the low-water mark exceeds it (approximate: Go refreshes mtimes at most hourly). Evidence: for the measured command set, M1 shows accumulation of superseded entries, not the working set, exceeds the cap (real-turn working sets not bounded); the lane citations under Chosen design.
- **D2 (2026-11-03): Reject rotation for Claude runs.** Why: there is no Landlock-equivalent confinement (`claude-command/env.ts`, "parity with native Bash, not isolation"), so per-generation isolation alone cannot prove a retiring generation unused. Deleting retired generations only at option 1's full-lane quiet point is not ruled out on safety grounds; it is rejected on cost: it adds generation bookkeeping, needs an ADR-1809 D2 amendment (`isRunCacheSubtree` exact names, the fixed go-build lister), and discards the hot cache, for no safety gain over trimming at that same quiet point.
- **D3 (2026-11-03): Reject shorter turns via `max_iterations`.** Why: it bounds turn count, not turn length (`sdk-executor.ts` line 1659; up to 60 iterations in `budget.go`); forced turn ends abort live commands in `quiesce`; and with a blocked trim, more boundaries reach the three-boundary soft park sooner.
- **D4 (2026-11-03): No guidance-only remedy was shown to be sufficient.** The measurements do not establish that none can be: affected-package testing is unmeasured. Why: per M1, compiled non-race edits alone cost 0.4-1.5 GB each, and no measured guidance bounds that growth; #2206 already removed path churn; sqlc and golangci-lint are already binaries. Two advisory levers remain for the follow-up's guidance: run `-race` only at the final gate (measured: race and non-race entries are separate, so a both-modes loop pays both), and test only affected packages rather than `./...` (unmeasured).
- **D5 (2026-11-03): Boundary-only trim remains for runs that are not guard-bound. OPEN OWNER QUESTION.** Why: the mechanism needs the guarded lane, which exists only when the worker opts in and the api enrolls the claim; the chart default is off. Whether the follow-up should also enable the guard by default or for the fleet, or accept limited coverage, is for the owner (Open question 1).
- **D6 (2026-11-03): Carry the issue's rejected list forward unchanged.** Why: no new evidence was found against any item; M1 adds that per-edit `-race` overhead alone and tool choices (sqlc and golangci-lint are already binaries) are not levers; `-race` placement is an advisory lever (D4).
- **D7 (2026-11-03): Quiet means a new `launched` counter is zero, not `live.size`.** Why: `live` also holds pressure waiters and hold waiters, so a `live.size` predicate would deadlock the hold on its own waiters.
- **D8 (2026-11-03): One gate immediately before launch, after the deny hooks, screening, admission and the pressure wait; the wait is charged to the command's own deadline.** Why: the hold must sit after every deny (invariant 6), the increment must be atomic with the drain check, and the transport bound (`worstCaseCallMs` against `GUARDED_SERVER_TIMEOUT_MS`) leaves no room for an uncharged wait.
- **D9 (2026-11-03): Reopen only after the trim helpers have exited; otherwise keep admission closed and take the existing preventive park.** Why: under the uid split a blocked foreign-uid helper cannot be killed, and a timeout only rejects the promise; reopening early could let a command use a subtree a helper is still deleting.
- **D10 (2026-11-03): The mid-turn leg never writes the soft-park streak.** Why: giving up must not bring a park sooner (issue test list); the boundary already resets the streak when a trim succeeded.
- **D11 (2026-11-03): One single-flight lock per run across the boundary trim, the mid-turn trim and `reclaimInPlace`.** Why: `reclaimInPlace` already covers the approve-to-first-report window, which can overlap the first implement turn.
- **D12 (2026-11-03): The mid-turn leg runs only in the implement phase with the dependency install joined.** Why: the background install is live during planning, which is why `.npm/_cacache` is kept until `depsJoined`.
- **D13 (2026-11-03): Inside the hold, also require a process scan that excludes only the exact CLI root pids; an unknown or incomplete scan counts as alive.** Why: the lane proves only its own trees drained. Native `Bash` is kept out only by the alias plus `disallowedTools`; the lane's PreToolUse hook screens a native `Bash` call (`lane.ts` line 289) but does not route it, so a CLI regression that offered native `Bash` again would run commands outside the lane's drain accounting (inference, not tested). The routing must not be the only barrier. The scan catches such escaped processes only if they exist at scan time; it does not block one that starts during the trim. So the follow-up needs a barrier covering the whole listing-and-eviction interval, or keeps mid-turn trimming disabled; a second scan after eviction does not close the gap. F1 validates that the scan can pass mid-turn.
- **D14 (2026-11-03): The interval (2-5 min) and the mid-turn trim deadline (about 60 s) are proposals to be measured.** Why: the measurement cost on a 10-20 GB cache is extrapolated, not measured (`MEASURE_MAX_ENTRIES = 500_000` in `rmtree.ts`, `MEASURE_DEADLINE_MS = 60_000` in `cache-cap.ts`).
