# ADR-3076: Codex command-cache calibration — incomplete

**Status**: Evidence/documentation partial; M0 incomplete. No scanner limits were selected from the calibration below, and that record changed no production code. M1–M5 are deferred to follow-up work by the recorded owner partial decision. The calibration record stays incomplete; a separate decision, [rotate, never refuse (#3675)](#decision-rotate-never-refuse-3675), is implemented by issue #3675 and supersedes the refusal proposed here.
**Date**: 2026-10-30
**Related**: issue #3076; issue #3675; [ADR-1598](1598-codex-command-storage.md); [ADR-1809](1809-per-run-cache-bounds.md).

## Decision state

This is an interim measurement record, not a shipped cache governor. The approved proposal is an admission/disposal soft byte budget, without interrupting a running command. M1–M4 have not started. ADR-1598's current lifecycle and ADR-1809's separate Claude-cache policy remain unchanged.

M0's first-scan/warm work was authorized while cold evidence remained unavailable. That continuation did not waive cold evidence or complete M0. Production or provisional entry, depth and wall limits cannot be selected from this incomplete matrix.

**cold populated-tree scan: pending, needs a qualifying environment (VM or privileged host)**

A container restart does not clear kernel caches; replacing an emptyDir-backed pod loses the populated cache. Neither supplies the missing evidence. Cold latency and cold-induced refusal frequency are unvalidated. A future exceeded bound must yield unknown and a recoverable `codex_command_cache_budget` refusal: that protects safety, not availability. This behavior is proposed, not implemented here (superseded by the rotate, never refuse decision below).

## Evidence-only delivery and recovery provenance

The maintainer's 2026-10-30 steering requests an evidence/documentation partial, with no further experiment or production changes. This record preserves the measurements obtained so far and their unmet criteria; it does not complete M0. The requested follow-up covers M1–M5 and must be designed from these results. The formal disposition is recorded in the run's frozen completion contract: revision 2 kept M0 and deferred M1–M5, and revision 3 accepted the unmet criterion `M0.c1` as-is for evidence/documentation-only, non-closing delivery. M0 remains incomplete.

The recovered history contains the measurement report in commit `19a5b7e8` and the prerequisite/tooling follow-up in `033a0b54`. Comparing the recovered HEAD `ba2144a9` with task baseline `3de7d05d` shows only this ADR added. The recovery coverage commits have no file changes. In this session the scratch directory was empty; the scanner, driver, raw series, timing logs and safety logs described below were unavailable. Numerical results, exits and cleanup claims below are therefore **inherited reports**, not newly executed or independently verified measurements. Source hashes identify historical artifacts but cannot reconstruct them or prove their contents. No new representative experiment was attempted for this delivery.

## Corpus and method

The experiment ran as the real/effective command uid on Linux amd64, using Go 1.27.3. The filesystem reported `ext2/ext3` through `stat -f -c %T`. The experiment filesystem and the worker data volume were the same device; this is not hosted node-wide storage evidence.

Go build/module caches, HOME, GOPATH and temporary output were private to the owned calibration directory. Preparation used `GOMAXPROCS=2`, `GOTOOLCHAIN=local`, and `GOFLAGS="-p=2 -buildvcs=false"`, with these successful producer steps:

| Step | Duration | Resulting aggregate charged bytes |
| --- | ---: | ---: |
| API `go build ./...` | 126.646 s | 924,479,488 |
| API `go vet ./...` | 188.465 s | 1,658,134,528 |
| Controller `go build ./...` | 71.857 s | 2,259,877,888 |

Each producer exited 0. This is authentic build-derived output, without padding, another run's caches, or hosted/model workloads. Module directory modes remained those produced by Go; the measurement walks did not chmod them. No directory-mode histogram was retained.

The measured active cache was approximately **2.105 GiB**:

| Inventory | Apparent bytes | Allocated/charged bytes | Files | Directories |
| --- | ---: | ---: | ---: | ---: |
| Build cache, excluding its root inode | 1,698,059,815 | 1,726,070,784 | 7,837 | 256 |
| Module cache, excluding its root inode | 472,414,030 | 533,794,816 | 22,927 | 3,034 |
| Whole generation, including directory overhead | 2,170,486,133 | 2,259,877,888 | 30,764 | 3,293 |

There were **34,057 examined non-dot entries**, maximum depth **12**, and no symlinks in this corpus. Three directory inodes account for the difference between component and whole-generation totals. The aggregate cache-root inode is excluded consistently.

This corpus is substantially smaller than the approved plan's historical 10.1/13.7/10.0 GiB run-cache samples. Those samples are not peaks and have no comparable file/entry inventory in this evidence; byte ratios do not establish comparable metadata distributions or scan latency.

An earlier preparation attempt additionally ran controller vet, producing 2,458,677,248 charged bytes (35,258 entries). Four uniform trees of that size would exceed the 8 GiB corpus cap, so that attempt performed no timing matrix. A mixed cohort of three approximately 2.105 GiB active caches and an approximately 1.544 GiB retained cache would arithmetically fit at about 7.86 GiB. That mixed layout was proposed for preparation, **not populated or measured**.

The scratch Go prototype used fd-relative, no-follow traversal with owner/device/identity checks, bounded getdents buffers/descriptors, nondumpable confirmation and fixed-shape output. Charges were the sum of `max(st_size, st_blocks * 512)`, including directories and symlink inodes, counting hardlinks per entry. A separate Python fd/scandir inventory matched the complete one-cache result and byte categories. Fixture checks covered exact charging, sparse/allocated files, hardlinks and symlinks.

The finite diagnostic caps were 2,000,000 entries, depth 96, 97 directory descriptors, 10 seconds and 10,000,000 directory reads. These protected the prototype experiment; **they are not production or provisional defaults**. Observed peak directory descriptors were 12. A complete live scan is a non-atomic observation, not cleanup proof or a strict byte ceiling.

The executed scanner source SHA-256 was `58b456be06456840048f88f85460db4014d350c6ce2a17ac067c63b8e6a4d838`. The final driver source SHA-256 was `53c614842887e5b2c8942d445c9db8ae2423882281a59a4f167faedb34beaf00`. Scratch sources/logs are run-local evidence, not published implementation.

## One-cache scan measurements

Preparation, copying and inventorying warm metadata. The initial observation is labelled exactly **first scan after preparation**, not cold: its response was **290.169 ms**, with traversal **282.879 ms**. Launch-to-main was **2.333 ms** and launch-to-traversal **2.618 ms**.

After an explicit warm-up, all **100/100 warm scans completed**, with zero incomplete reasons and exact inventory agreement:

| Metric | Median / nearest-rank p50 | p95 | Maximum |
| --- | ---: | ---: | ---: |
| Prototype invocation response | 286.384 ms | 371.424 ms | 471.703 ms |
| Traversal | 273.110 ms | 347.217 ms | 426.566 ms |
| Setup within helper after main entry | 0.128 ms | 0.297 ms | 0.460 ms |
| Process CPU, user + system | 223.312 ms | 231.666 ms | 267.289 ms |
| Reported process peak RSS | 32,112 KiB | 32,112 KiB | 32,112 KiB |

Response p99 was 426.981 ms. Response includes owned helper launch/settlement and bounded logging; it is not a production broker/admission measurement. RSS is process-level evidence under this launcher, not an isolated Go heap measurement. The full warm-series external launch/startup distribution was not retained; the setup row excludes runtime startup before main.

This one quiet occupancy case meets the one-second response target. It supplies no aggregate-layout, contention, cold, or fleet probability guarantee. It cannot establish the mandated margins for production scan limits.

## Partial command-sequence comparison

The proxy repeated cached API `go build ./...` against its own generation, comparing equivalent baseline and scanned commands. Scanned commands took a fresh sample before launch and after actual process-group drainage. Baseline/scanned order alternated. No future byte-budget policy or cache rotation was present, and rebuild/download costs of that policy are not included.

The driver requested 100 command pairs, yielding 200 normal scan observations if finished. M0 requires at least 100 normal scan observations per concurrency layout; that driver choice was more work than the minimum. Preparation plus this sequence did not fit the bounded experiment.

The last fully recorded timing prefix contained **40 command pairs**:

| Baseline | With pre/post scans | Added elapsed time |
| ---: | ---: | ---: |
| 313.610 s | 340.363 s | 26.752 s / **8.530%** |

This partial sequence is above the proposed 5% overhead criterion; it is not a completed-series or CI timing result. Additional commands ran before the stop, but their final paired timing aggregate was not retained. **88/88 normal-sequence scan observations completed**, with zero incomplete reasons; this remains below the required 100. Only concurrency one was attempted, so no concurrent-writer completeness conclusion follows.

## Safety and shutdown evidence

The experiment used the supplied deployment-specific pressure configuration. Fresh projected headroom checks covered both filesystems and cumulative remaining reservations. An independent monitor targeted 250 ms sampling, with fail-closed unknown readings and a latched stop. Opaque Go build/vet invocations were supervised producer steps in their own process groups.

Monitoring reduces risk; it guarantees neither the reserve nor absence of ENOSPC. Writes can continue during sampling/scheduling delay, the five-second SIGTERM grace, and kill/drain latency. Writer throughput is unbounded, so there is no numerical overshoot bound. The planned artifact reservations are not kernel quotas or proof of instantaneous spare node capacity.

For the final attempt, the monitor recorded:

- 4,528 samples, maximum observed gap **306.523 ms**;
- minimum available bytes **20,258,443,264** and maximum observed used fraction **0.562854**;
- no pressure/free-space or unknown-reading stop; the **experiment deadline** latched the stop;
- supervised execution through drainage **1,140.044 s**, reserving the last minute of the 20-minute bound for shutdown/cleanup; the final log write was at 10:20:08 UTC after a 10:01:06 UTC start.

No new producer or scan leg started after the latch. Owned groups were terminated/joined as needed; group disappearance and final ECHILD were confirmed before cleanup. No kill escalation was recorded in the final drainage records. The corpus/temporary trees were removed (34,071 owned entries); final enumeration found no remaining directories in the owned experiment root. Bounded logs and the prototype binary remain. The runner exited **2**, meaning incomplete calibration, not a passing gate. The interrupted Go command's individual exit status was not retained; success must not be inferred from drainage.

Guard validation: 25 deterministic/tiny-filesystem tests passed without skips, including headroom loss between allocations, mid-scan stop, data pressure, unknown readings, cumulative reservations, irreversible stop, TERM/KILL timing and join-before-cleanup. Process-control assertions in those tests were injected; the actual experiment separately exercised healthy and deadline drainage. The scratch scanner Go tests passed after correcting newly created fixture modes: a setgid scratch parent had caused `mkdir(0700)` to inherit mode `02700`. Clearing inherited bits on our new private roots preserved the scanner's exact `0700` check.

## Remaining M0 work and gate

Cold evidence is **not** M0's only unmet requirement. Still unvalidated:

- two/three independently populated active-cache layouts and retained/unregistered overlap layouts, each with its required warm series and inventory;
- at least 100 normal observations for each concurrency layout, concurrent writer/admission activity, queue effects and cleanup completing during scanning;
- the full comparable sequence overhead requirement and per-operation observation/logging overhead;
- complete retained startup/queue timing evidence and the interrupted-leg exit record;
- selection and verification of entry/depth/time margins against the complete matrix, followed later by matching the production scanner to the calibrated implementation;
- cold populated-tree latency and resulting refusal frequency.

M0 remains incomplete. No entry/depth/wall constants have been selected. M1–M4 remain gated, M5 hosted acceptance remains unperformed, and delivery remains non-closing. A new calibration approach or acceptance change needs the owner decision required by the run; this timeout is not permission to enlarge the bound, weaken coverage, reuse a successful sample for later admissions, or implement policy.

## Unmet acceptance criteria at evidence-only delivery

| M0 criterion | Evidence and limitation |
| --- | --- |
| Representative inventory | One authentic 2.105 GiB generation was reported with an independent inventory match. No raw oracle output survives recovery; the full occupancy matrix was not obtained. |
| Cold populated-tree latency | Unmeasured. No qualifying reproducible isolated cold preparation was established. Warm results cannot replace it. |
| Warm latency and five-second admission answer | One-cache prototype response p95 was 371.424 ms and maximum 471.703 ms; its 100 warm scans completed. This is below five seconds for that sample, but no production admission transaction, queue, cold, multi-cache or contention deadline proof exists. The prototype's ten-second diagnostic cap is not compliant production deadline evidence. |
| Sequence overhead ≤5% | Not met by the recorded 40-pair prefix: 8.530% added elapsed time. The full requested 100 pairs did not finish. Exemption/disposal policy overhead was not measured. |
| Completeness under normal concurrency | Only 88 normal-sequence scans were reported, all complete, below the minimum 100 per concurrency layout. Two-/three-run layouts, retained/unregistered overlap and cleanup-during-scan are unmeasured. |
| Whole-experiment deadline | Deadline stop/drain was reported at 1,140.044 s, with final logging within 20 minutes. Calibration exited 2, incomplete; the interrupted command's individual exit was lost. |
| Headroom and pressure-stop safety | Historical minimum available bytes were 20,258,443,264 with maximum observed used fraction 0.562854; the later single prerequisite reading was 21,513,170,944 available bytes. The configured stop floor was 0.80 and reserve 4 GiB. These sampled observations are neither continuous guarantees nor current headroom. |
| Bounded own-producer allocations | Not established. Opaque Go build/vet processes can write beyond reservations between samples and during shutdown. The earlier attempt's monitoring did not satisfy admission before each bounded allocation; the rebuilt driver was left disabled. |
| Complete reproducible timing evidence | Startup/queue distributions, final paired aggregate, raw logs and an interrupted-leg exit are missing. Historical safety test results do not prove full-driver integration. |
| Production bound selection and parity | Not performed. No entry/depth/time limits meet the prescribed selection rule without the missing matrix and cold evidence; no production scanner exists to compare. |

No M0 criterion is waived by this table. No production budget, rotation, recoverable model refusal, trusted-operation exemption or hosted acceptance is delivered. M0 must remain incomplete; the recorded non-closing partial leaves the remaining measurement/design and M1–M5 work for a formally scoped follow-up. Issues #1598 and #2013 remain outside this delivery's completion claims.

## Recovery follow-up: tools rebuilt, fresh experiment not started

The recovered branch contained this historical report but no scratch sources, binary, corpus or raw logs from that experiment. The results above are inherited records, not measurements repeated or independently revalidated in this session. New scratch tooling was built on 2026-10-30; none of its tests constitutes the requested fresh Go calibration attempt.

The configuration inputs supplied for the follow-up are an API pressure threshold of 0.90 and the default soft margin of 0.10, giving an experiment pressure stop floor of 0.80. The default hard margin is 0.03; its 0.87 threshold does not replace that earlier experiment stop. Pressure uses the higher valid byte/inode used fraction. Byte usage is `(blocks - bfree) / blocks`; allocation headroom separately uses `bavail * frsize`.

At **2026-10-30 11:46:49 UTC**, the experiment filesystem and data filesystem each reported **52,521,566,208 total bytes**, **21,513,170,944 available bytes**, **0.538965 byte used fraction** and **0.130256 inode used fraction**. This is one prerequisite observation, not continuous monitoring or a reservation.

The new tooling remains scratch-only and is not production implementation:

| Building block | Executed check | Observed result |
| --- | --- | --- |
| Go fd-relative scanner | Focused Go tests; format check; build; tiny CLI fixtures | Final commands exited 0; 10 top-level Go test cases passed. No representative throughput result. |
| Python guard and independent inventory | AST syntax checks, 25 unit tests, eight isolated mutation probes | Exit 0; no skips; all eight mutations failed their intended assertions. Cleanup authorization and monitor-gap tests use injected conditions. |
| Linux owned-process backend | Nine actual subprocess tests | Exit 0; 4.313 s for this tiny suite. Cases include adopted descendants, cancellation, TERM/KILL, bounded capture, multiple roots, and withheld cleanup proof for an escaped child. |

The backend uses a dedicated child owner and Linux subreaper. Cleanup proof requires actual waits, output closure, no pending children, and an observed ECHILD. An escaped-child fixture first returned uncertainty without cleanup permission; the test subsequently released and joined its own child. Its retention marker remains. These tests do not establish full-driver monitor integration, producer write bounds or experiment performance.

For identifying these run-local artifacts, the new source SHA-256 values are:

| Source | SHA-256 |
| --- | --- |
| Scanner | `ba693e57d78173fffd3257221fb9be0ec464bdd7d5efd875b5917a089498eecf` |
| Guard and inventory | `e0955560a198a59a2ea1844acbd41150c629e06546de217a9ad31a5f1fe2585b` |
| Disabled protocol manifest driver | `afb88e4c35a9a4d4acee07085a9761248ab13d2be6f6a52df2ceebbc08b40ffc` |
| Owned-process backend | `d2fdc0d649b7ed867a2b72b30f2402f0c0d2992c6caef0ba698be99542bc71f9` |

The scanner binary SHA-256 is `1278fce9107b4ff11c101b299c379a3ef3ea3784709bbb030cc064240fcaffa3`. New logs record their exits in the owned scratch directories. Scratch artifacts are not a published or durable test harness.

### Own-producer allocation conflict

The approved experiment requires “at most four approximately 2 GiB cache trees / 8 GiB corpus allocation” and fresh admission “before each allocation”, including the cumulative remaining planned allocation and the 4 GiB reserve. The authorized Go build/vet commands produce authentic caches, but an opaque command's finite runtime, reservation and periodic monitoring do not establish a bound on its writes.

For example, with outstanding reservations R and R + 4 GiB available, own producers can write R + ε before the next sample. Later cancellation and actual descendant joins cannot undo that reserve breach. The approved warning about other runs consuming the reserve does not resolve this own-producer case. A hard kernel quota is not prescribed, but an enforceable own-allocation bound is still needed. It must cover cache writes, module downloads/extraction, build temporary output, bounded logs and writes during shutdown.

Independent safety review identified this conflict before enabling any producer. Read-only capability checks did not establish a suitable existing bounded-write facility or cold environment. FUSE/KVM devices and the checked quota/VM tools were absent; the available Docker overlayfs driver alone does not demonstrate an enforceable per-experiment storage bound. No new infrastructure, privilege or producer was provisioned.

The full scheduler, live monitor synchronization and complete observation matrix remain worker implementation obligations. The current manifest driver is deliberately disabled; its refusal is not a measured calibration result. Before an actual attempt, the producer boundary needs either an available bounded-write environment or an explicit owner amendment permitting sampled own-producer overshoot. No such amendment is assumed.

**Follow-up outcome:** zero new representative scans, zero Go command pairs and no corpus allocation. No experiment producer or representative scan leg was launched; no producer cleanup was necessary. The small tooling tests completed; no live test child was left outstanding by those tests. Cold validation, full layout coverage, overhead/completeness targets and scanner-limit selection remain unperformed. M0 is incomplete and M1–M4 remain gated.

## Decision: rotate, never refuse (#3675)

**Status**: Implemented by issue #3675. Supersedes this record's proposed `codex_command_cache_budget` refusal: no command is ever refused for cache size. The M0 calibration above stays incomplete and is not reopened by this decision; the walk bounds below are provisional because of it.

### Mechanism

- **Measure.** The supervisor has a `--measure-cache` mode: a read-only, no-follow, fd-relative walk of `<cache root>/<token>` as the command uid. It takes no lock and never chmods, unlinks or renames. It charges `max(st_size, st_blocks*512)` per entry and prints one `cache_measure` line.
- **Schedule.** After a cached command's roots dispose, the worker schedules a measurement of the run's current cache generation. The command does not await it, but when the worker-wide limiter slot is free its helper is spawned before the disposing caller resumes (otherwise it waits in the queue) (the dispose-to-return cost under Measured overhead). At most one is outstanding per generation (requests coalesce) and at most one starts per 30 s per generation (`minIntervalMs`, default 30 000). A worker-wide FIFO limiter runs one measure helper at a time.
- **Rotate.** A result at or above the budget, or an unknown result (incomplete, error, timeout), marks the generation rotation-pending. Commands keep being admitted to it until a replacement holder is ready; admission then switches atomically. A per-launch reservation is taken before any cache-bearing spec is built, and a prepared launch keeps its pinned generation and never reacquires. The old generation is closed and never reopened.
- **Holder loss** still means uncached commands, as before. Terminal eviction, the capture and reaper bypasses, Landlock and the private HOME/TMPDIR are unchanged. The startup reaper is not run mid-run, and only the run's own tokens are touched.

### Finite retirement and cleanup judgment

- A retiring generation is removed only through the existing drain proof (all roots that used it disposed cleanly, poisoned registries proven), with zero reservations and no unresolved measurement. Removal is by holder release, or `--remove-cache` after holder loss.
- If it cannot be proven drained mid-run it is left for terminal settle; there is no early distrust.
- At most one retiring generation exists per run. While one is not removed (pending, failed or deferred), no further rotation happens: growth limiting is sacrificed rather than letting generations accumulate.
- A failed replacement start latches replacement off for the run. An abandoned replacement (ready but no longer adoptable) blocks until its own settlement and latches if unclean. A replacement that never spawned a process does not distrust, and a capture-admission refusal before any spawn re-arms rotation.
- Cleanup judgment is ownership-specific: an adopted holder's close is joined but judged only by the existing release/remove path. Holder loss plus verified drainage plus successful removal therefore keeps today's clean lifetime outcome.

### Helper settlement

The helper's delivery bound is 30 s (`DEFAULT_MEASURE_TIMEOUT_MS`). At timeout or abort it is SIGKILLed as the command uid, only while no exit has been observed. The slot and the generation's unresolved measurement settle at the helper's `close`, or at `exit` plus a 2 s stdout drain (`MEASURE_STDOUT_DRAIN_MS`).

### Setting and default

`UZI_CODEX_COMMAND_CACHE_RUN_BYTES` is plain decimal bytes. Unset or empty means 4294967296 (4 GiB); `0` disables live measurement and rotation; anything else (sign, suffix, hex, leading zero, above 9007199254740991) refuses to start the worker and is a boot error in the controller. The chart value is `workers.codex.commandCacheRunBytes` (default empty, forwarded only when non-empty, so a default install's pod is unchanged). On compose the agent service has no `env_file` and does not pass the variable through, so it must be added to the agent service's `environment` through a compose override; a uid-split compose worker then reads it directly. Worker images older than this change ignore it.

4 GiB is above the roughly 2.1 GiB single-build corpus measured above and below the 10-14 GiB historical run-cache samples (#3076). It is a candidate, not a validated fleet default.

### Provisional walk bounds, undercount, advisory status

The walk is incomplete (and so counts as unknown, which triggers rotation) on a foreign owner, device or swap, a missing owner r+x, an I/O error, byte-total saturation (`bytes`), or the provisional uncalibrated bounds of 500,000 entries, depth 64 and 5 s wall. These bounds were not selected from the incomplete M0 matrix. Entries or directories that vanish mid-walk are skipped, so a result can undercount. A measurement is advisory and never cleanup authority.

### Exposure

Rotation is best-effort and promises no numeric bound.

- Measurement lags several admissions (30 s spacing, the helper queue).
- Overlapping commands keep writing a closed generation until they finish.
- A failed or stalled removal retains bytes until the startup reaper.
- Per run there can be one current generation, one retiring generation and overshoot. As the issue's hypothetical capacity example, two ordinary slots plus one checker would be roughly three current-generation targets plus retiring or retained caches on node ephemeral storage; that is not a default (the hosted default `maxConcurrentRuns` is 1, `deploy/chart/values.yaml`).

Residual risks:

- If the kill of a stopped helper cannot be delivered (`timeout_unkilled`), the worker-wide measurement slot stays held and rotation stops for every run on that worker. How to force this is not demonstrated.
- A same-uid peer that repeatedly stops helpers can delay other runs' measurements by up to 30 s each and force rotations (bounded).
- A kill racing libuv's reap can target a free pid; reuse would need the pid space to wrap.
- After the 2 s drain a peer may still hold the helper's stdout pipe (it gains no cache access).

### Measured overhead

Measured 2026-10-30 from the committed test, 3 runs. Environment: Intel Xeon Gold 5120, 4 CPUs available to the process (nproc/cgroup; the host reports 8), node v24.20.0, go1.27.3, ext4-family scratch filesystem, single process. The test is `agent/test/codex-command-cache-overhead.test.ts`. Its two scenarios are gated independently: (a) runs with `UZI_CACHE_OVERHEAD=1`, (b) with `UZI_CACHE_OVERHEAD_SUPERVISOR=<absolute path to a built supervisor binary>`; each is skipped otherwise. Not measured: the production `setpriv`/uid split, real Go/npm caches, large corpora and cold caches. No latency claim is made beyond these figures.

(a) Synthetic scheduler overhead (fake helpers, 50 ms fake measure, 20 ms fake holder start, 200 commands, 5 ms think time, minInterval 0):

- elapsed 1141-1144 ms with no rotation options, 1089-1373 ms with budget 0, and 1090-1241 ms with rotation enabled; run-to-run noise exceeds the difference;
- reserve-to-launch and dispose-to-return medians 0.022-0.029 ms, worst p95 0.196 ms, worst max 3.1 ms;
- 18-20 measurements and 7 rotations per enabled run.

(b) Integrated real helper. In the helper process layer the only substitution is the spawn (the built binary is exec'd directly, without `setpriv`, with the test uid and an owned 0700 fixture root) plus mapping the holder's ready path back to the production root text; the holder, measure, limiter, remove and rotation code are production. In addition the scenario launches fake roots (no real Codex command runs; the test writes the files itself) and uses a no-op capture-admission check. 100 commands per cache, each fake command's test code writing 20 x 4 KiB into the generation's `gocache/`; 2 MiB budget; minInterval 20 ms (a stress setting far more frequent than the 30 s default); 10 ms think time.

- Single cache: command sequence 1190-1235 ms off vs 1364-1539 ms on (+15-25%; per run +24.6%, +14.7%, +15.2%). Dispose-to-return median 0.02-0.03 ms off vs 0.26-1.5 ms on, p95 4.8-7.2 ms, max 18.7 ms: the measurement is not awaited, but with a free limiter slot its helper is forked/exec'd before the disposing caller resumes. Helper spawn-to-close median 7.1-8.7 ms, p95 10.8-23.4 ms, max 30.7 ms; queue wait median about 0.2 ms, max 1.4 ms. 50-53 measurements and 3 rotations per run, 3 generations retired mid-run with no `--remove-cache` needed, the fixture root empty after settle, and all lifetimes sealed.
- Two caches sharing the worker-wide limiter: sequence 1179-1227 ms off vs 1317-1367 ms on (+11-14%). Dispose-to-return median 0.04-0.11 ms, p95 3.3-3.8 ms, max 8.3 ms. Helper spawn-to-close median 6.7-9.0 ms, p95 11.6-18.1 ms, max 19.8 ms. Queue wait median 0.2-2.0 ms, p95 6.1-10.3 ms, max 15.2 ms. 6 rotations per run; fixture root empty after settle.
- The burst-on and two-cache-on runs always, and one of the three single-cache-on runs, end with one in-flight measurement aborted by terminal settle (reported `unknown`/aborted with a "measurement helper unresolved" warning); two of the three two-cache runs also had one queued measurement cancelled. This is expected teardown behaviour.
- The burst scenarios (0 ms think time) are not a helper-work measurement: only one measurement starts, at the first command, and it cannot finish until the loop yields.

(c) Isolated Go walk: `BenchmarkMeasure` 6.6 ms per walk of a 2,040-entry tree.

At the 30 s production interval a run triggers at most about two measurements per minute per generation, so the per-measurement spawn cost is amortised. This frequency argument is arithmetic, not a measurement.

### Not yet done

Hosted observation of a long Go-heavy run after release is pending (owner, #3675 m5). The 4 GiB default, the walk bounds and the hosted effect on node ephemeral storage are unvalidated until then.
