# ADR-3076: Codex command-cache calibration — incomplete

**Status**: Evidence/documentation partial; M0 incomplete. No scanner limits selected or production code changed. M1–M5 are deferred to follow-up work by the recorded owner partial decision.
**Date**: 2026-10-30
**Related**: issue #3076; [ADR-1598](1598-codex-command-storage.md); [ADR-1809](1809-per-run-cache-bounds.md).

## Decision state

This is an interim measurement record, not a shipped cache governor. The approved proposal is an admission/disposal soft byte budget, without interrupting a running command. M1–M4 have not started. ADR-1598's current lifecycle and ADR-1809's separate Claude-cache policy remain unchanged.

M0's first-scan/warm work was authorized while cold evidence remained unavailable. That continuation did not waive cold evidence or complete M0. Production or provisional entry, depth and wall limits cannot be selected from this incomplete matrix.

**cold populated-tree scan: pending, needs a qualifying environment (VM or privileged host)**

A container restart does not clear kernel caches; replacing an emptyDir-backed pod loses the populated cache. Neither supplies the missing evidence. Cold latency and cold-induced refusal frequency are unvalidated. A future exceeded bound must yield unknown and a recoverable `codex_command_cache_budget` refusal: that protects safety, not availability. This behavior is proposed, not implemented here.

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
