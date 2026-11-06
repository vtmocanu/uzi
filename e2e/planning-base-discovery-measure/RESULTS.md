# Results: issue #3543 base-discovery measurement

Measured on 2026-11-06, 12:08–12:29 UTC, with `run.sh` and the harness at commit `58d4d79f`. Each configuration ran once; this dataset has no repeats.

**Environment:**
- **Fixtures:** `REPO_SHA=5ef702b2`.
- **Commands:**
  1. 1536m: two invocations with `MEM_CONFIGS="1536m:1:full,control,cancel"`:
     - `FIXTURES="repo synthetic-10000 synthetic-50000 chain-d64 chain-d65 fanout2-d8 fanout2-d12"`;
     - `FIXTURES="fanout2-d16 fanout2-d20 fanout2-d24 fanout2-d64 wide-1024x1024 wide-4096x4096 wide-16384x16384"`.
  2. 4g: one invocation, with `MEM_CONFIGS="4g:2:full"`.
- **Records:** 56 measurement records in one `OUT`:
  - 14 fixtures × `full` / `control` / `cancel` at 1536m;
  - 14 fixtures × `full` at 4g.
- **Termination:** every job exited 0, with no OOM and no harness kill.

| item | value |
|---|---|
| image | `uzi-agent-home-split:base`, `sha256:d7518ec4b5fc5c6de0fbc6ea19a8885b0d7b2e2a75d98020b414c5f7b5f98092`, built locally 2026-11-03 (image revision label `2528ee77`): node v26.12.1, git 2.54.0. `run.sh` sets `/sbin/tini` as the entrypoint (PID 1) |
| limits | 1536 MiB / 1 CPU (compose per-slot budget) and 4 GiB / 2 CPU (compose whole-worker default), memory-swap = memory, network none. Compose itself enforces only the shared whole-worker limit: 1536 MiB is a per-run budget, not a per-run cgroup, and one OOM there kills every run in the container |
| production bounds | unchanged: 2 MiB per commit/tree object, depth 64, helper deadline 28 s inside the 30 s outer timeout, 5 s per git command, 20 ms yields, 2000 ms kill grace, default `PLANNING_LIMITS`. Those allowances are added on top of the base's own discovered totals (`agent/src/git.ts`, the `ENTRY_LIMIT = E + 1 + entryAllowance` family), so none of those allowances bounds discovery itself; discovery is bounded by the depth limit, the per-object cap and the deadlines |
| host | one shared worker host: kernel 6.12, cgroup v2, a Docker-in-Docker daemon reporting 8 CPUs and 67,420,241,920 bytes (62.8 GiB), possibly with other runs active. Absolute times are host-specific |

## Observations (measurement only; no recommendation)

1. **Representative bases complete inside the bounds.**
   - This repository at `5ef702b2` (333 trees, 8953 leaves):
     - discovery 3.3 s at 1 CPU and 2.4 s at 2 CPU;
     - whole capture 8.5 s and 5.9 s;
     - container peak 167 MiB and 150 MiB.
   - 50k unique files (501 trees):
     - discovery 6.2 s and 4.8 s;
     - capture 18.4 s and 13.0 s;
     - peak 175 MiB and 220 MiB.
     - The 1 CPU capture used 18.4 s of the 28 s helper deadline (66%).
2. **Each tree occurrence costs two git spawns.**
   - Counted: two git processes per tree occurrence during discovery (for example 7,989 spawns for 3,992 occurrences). Whole-capture ratios are higher, for example 3.0 for repo. On the binary DAGs this came to 7.0–9.1 ms per occurrence at 1 CPU and 7.0–8.0 ms at 2 CPU. The cost was not profiled, so attributing it to the spawns themselves is an inference. Per occurrence at 1 CPU, repo took about 10 ms and synthetic-50000 about 12 ms (7.3 and 9.5 ms at 2 CPU).
   - About 3,100–4,000 tree occurrences fit before the 28 s helper deadline, wherever the DAG shape allows.
3. **Exponential shared DAGs (binary, depth 12 to 64) never finish discovery.**
   - Every run stops at about 28.1 s with the helper deadline (`git_deadline`), after 3,100–4,000 of 8,191 to about 3.7×10^19 expected occurrences.
   - The container peak stays flat at 122–131 MiB at 1 CPU and 151–156 MiB at 2 CPU.
   - Depth beyond what fits in the deadline changes nothing measured.
4. **Wide shared trees (extra, beyond the spec) show the memory dimension.**
   - One leaf tree with W entries, referenced F times, adds W path entries per occurrence. Memory then grows with the work done before the deadline:

   | fixture | tree entries before stop, `full` (1 CPU / 2 CPU) | container peak, `full` (1 CPU / 2 CPU) | uninstrumented `control` at 1 CPU | outcome, `full` |
   |---|---|---|---|---|
   | wide-16384x16384 | 2.3M / 2.9M | 648 MiB / 782 MiB | 663 MiB, `capture_deadline`, 28.2 s | `capture_deadline` |
   | wide-4096x4096 | 2.0M / 2.4M | 482 MiB / 742 MiB | **610 MiB, `capture_deadline`, 29.5 s** | `git_deadline` |
   | wide-1024x1024 | all 1.05M | 362 MiB / 452 MiB | 367 MiB, `git_deadline`, 28.1 s | `git_deadline` after discovery |

   - "Tree entries" is the helper's total tree-entry count, which includes the root's entries.
   - At 1 CPU, the uninstrumented `control` run (the production code path) peaked at or above `full` on every wide fixture. Two of the gaps are small (+2.3% and +1.2%, one run each); on wide-4096x4096 it was 27% higher (610 vs 482 MiB) and stopped on a different deadline. The 1 CPU `full` column can therefore understate the production-path peak. `control` was not run at 2 CPU.
   - wide-1024x1024 *completed* discovery, at 20.8 s and 22.5 s. Its later refusal came in the post-discovery capture. In these `--no-checkout` fixtures every base path diffs as a deletion, so that phase is inferred to be shaped by the harness setup, not by production.
   - None of the 56 runs ran out of memory at either limit. Note that 1536 MiB is a budget, not an enforced per-run limit, under compose (see the limits row).
   - Memory grew with the work done before the deadline stopped it. Discovery has no aggregate ceiling, so the peak is not bounded structurally (the helper comment reads "no aggregate base ceiling or cache; the shared deadline bounds discovery"). The 4 GiB / 2 CPU configuration changed memory and CPU together, so its higher peaks cannot be attributed to CPU alone. On wide-1024x1024 it did the same work more slowly and still peaked higher.
   - **The deadline is soft.** The helper deadline is checked only at `check()` points and per git command. Measured by the harness from just before the capture call, so including helper startup, captures took up to 29.5 s (wide-4096x4096 `control`) and 28.9 s (wide-16384x16384 `full`) against the 28 s helper deadline. The wide-16384x16384 helper's own last snapshot came 28.6 s after its start. 29.5 s is 0.5 s short of the 30 s outer timeout. Only peaks were recorded, so whether memory was still growing at the stop is not measured.
5. **Cancellation and cleanup stayed inside the grace.** All 14 cancel runs settled 13–128 ms after the abort, with no surviving process group and no leftover `uzi-planning-*` temp tree, against the 2000 ms grace.
   - What was exercised: only the mid-discovery trigger (the abort landed after ≥50 tree occurrences). The post-discovery trigger did not fire in this matrix.
   - In every run the helper exited 143 on SIGTERM, and TickSpawner sent its group SIGKILL 12–125 ms later. The grace itself was never consumed.
   - The cancel rows read `cancelled unclassified` because the abort message ("permit-held git output collection aborted: boundary deadline exceeded") carries no refusal token. That is expected, not a classifier failure.
6. **The depth bound behaves as specified.** `chain-d64` completes, and `chain-d65` is refused with `directory_cap`.
7. **Instrumentation overhead (`full` minus `control`, one run each; run-to-run noise was not measured):**
   - Wall time, 1 CPU:
     - repo −0.4 s (8.5 vs 8.9 s);
     - fanout2-d8 −0.7 s (8.4 vs 9.1 s);
     - synthetic-10000 +0.4 s, +11.5% (3.9 vs 3.5 s);
     - synthetic-50000 +1.2 s, +6.8% (18.4 vs 17.2 s).
   - Container peak, wide trees at 1 CPU:
     - wide-16384x16384 −15 MiB (648 vs 663);
     - wide-1024x1024 −4 MiB (362 vs 367);
     - wide-4096x4096 −128 MiB (482 vs 610), with a different deadline token (see 4).

   The preamble's effect on the helper's own maxRSS is not separately quantified.
8. **Six rows read `refused unclassified`.** They are fanout2-d12 / d16 / wide-1024x1024 at 4g and fanout2-d20 / d24 / d64 `control`. All are `git_deadline` refusals whose token was hidden by the classifier:
   - Near the deadline, the helper's per-command timer `Math.min(5000, deadline - Date.now())` went negative.
   - Node then printed a `TimeoutNegativeWarning` ahead of the `UZI-PLANNING-REFUSAL git_deadline` line, as the raw messages show.
   - It happens in uninstrumented control runs too, so it is production behaviour. It is recorded as an incidental finding and left unchanged here.

**Caveats:**
- The adversarial clones are `--no-checkout`. Production would first have to check out the base.
- Tree RSS double-counts shared pages and is sampled every 10 ms, so it can miss short peaks.
- The parent-observed "disc" values (tree RSS and cgroup peak) were read at the parent's first 10 ms poll after the helper's boundary snapshot. The lag is up to one 10 ms poll plus parent scheduling delay, so they can include early post-discovery work such as the first capture spawn. The cgroup peak is monotonic, so it is an upper bound for discovery; the discovery tree-RSS peak is approximate and may be above the true peak (polling lag) or below it (a short peak missed by the 10 ms sampling). The helper maxRSS "disc" value is taken synchronously at the boundary. The container peak includes the harness parent (about 60 MiB) and page cache.
- There is no UID split or sandbox.
- Everything was measured on one host. See README.md.

## Instrumentation validation (m1 smoke and matrix cross-checks)

- **Counts against the analytic manifests:**
  - completed discovery matches the manifest exactly: fanout2-d8 (511 trees / 766 entries), chain-d64 (65), synthetic-10000 (101 / 10,100), synthetic-50000 (501 / 50,500), repo (333 / 9,285) and wide-1024x1024 (1,025 / 1,049,600);
  - d12 and deeper did not finish within the production 28 s deadline on this host: 3,100–4,000 occurrences were reached, against the 8,191 that d12 needs. So a full comparison was not possible. Instead, every instrumented binary-DAG run (fanout2-d12 to d64 at both limits, plus an earlier d12 smoke) matched the exact DFS-prefix entry count for its observed occurrences. The last spawned tree read may be unclosed at the stop.
- **Controlled faults (m1 smoke, harness at m1):**
  - a deadline injected between the sizing batch close and the index open reported discovery not completed, with `capture_deadline`;
  - wide-4096x4096 at 256 MiB reported `oom` (oom_kill delta 1, Docker OOMKilled) with `≥` / `n/a` peaks;
  - a short `START_TIMEOUT` reported `killed_by_harness`, exit 137, not `oom`.
- **Raw records:** `results-2026-11-06.jsonl` beside this file holds the 56 per-job result records with their termination records. It is the source of the table below.

## Table (verbatim `results/results.md`)

Legend:
- Outcomes:
  - `completed`;
  - `refused <token>`, a deadline or bound refusal whose token names the bound;
  - `cancelled`;
  - `oom`;
  - kill classes, as defined in README.md.
- `control` rows are uninstrumented, so their occurrence, byte and helper-RSS cells are `n/a`.
- `≥` marks a lower bound.
- Discovery is "done" only when the helper opened the clone index.
- Rows sort by job label string: `fixture__mode__mem` in this dataset. The harness now appends `-<cpus>cpu` to the label, so two configurations sharing a memory limit cannot collide.
- `cancelled` rows also carry a token, `unclassified` (see 5); Observation 8 explains the `refused unclassified` rows.

| fixture | mode | mem/cpus | outcome | discovery done | discovery ms | capture ms | tree occ (obs / expected) | tree entries | discovery object bytes | readSync bytes | helper maxRSS MiB (disc / final) | tree RSS peak MiB (disc / all) | cgroup peak MiB (disc / end) | cancel: settle ms, cleanup |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| chain-d64 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 613 | 51 / 65 | 50 | 1990 | 0 | n/a / 62.8 | 67.0 / 67.0 | n/a / 77.4 | 15, ok (mid_discovery:treeOccurrences>=50) |
| chain-d64 | control | 1536m/1 | completed | n/a | n/a | 751 | n/a / 65 | n/a | n/a | n/a | n/a | n/a / 69.0 | n/a / 78.3 |  |
| chain-d64 | full | 1536m/1 | completed | yes | 594 | 768 | 65 / 65 | 65 | 2571 | 240 | 67.1 / 67.9 | 77.3 / 77.3 | 83.2 / 83.6 |  |
| chain-d64 | full | 4g/2 | completed | yes | 548 | 677 | 65 / 65 | 65 | 2571 | 240 | 65.3 / 65.9 | 67.8 / 67.8 | 80.6 / 80.9 |  |
| chain-d65 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 726 | 56 / 66 | 56 | 2198 | 0 | n/a / 62.7 | 67.5 / 67.5 | n/a / 78.6 | 21, ok (mid_discovery:treeOccurrences>=50) |
| chain-d65 | control | 1536m/1 | refused `directory_cap` | n/a | n/a | 714 | n/a / 66 | n/a | n/a | n/a | n/a | n/a / 68.3 | n/a / 80.8 |  |
| chain-d65 | full | 1536m/1 | refused `directory_cap` | no | n/a | 710 | 65 / 66 | 65 | 2522 | 0 | n/a / 65.1 | 69.8 / 69.8 | n/a / 79.5 |  |
| chain-d65 | full | 4g/2 | refused `directory_cap` | no | n/a | 589 | 65 / 66 | 65 | 2522 | 0 | n/a / 63.3 | 66.1 / 66.1 | n/a / 79.5 |  |
| fanout2-d12 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 607 | 59 / 8191 | 92 | 3254 | 0 | n/a / 64.3 | 67.3 / 67.3 | n/a / 80.8 | 15, ok (mid_discovery:treeOccurrences>=50) |
| fanout2-d12 | control | 1536m/1 | refused `git_deadline` | n/a | n/a | 28107 | n/a / 8191 | n/a | n/a | n/a | n/a | n/a / 109.7 | n/a / 125.5 |  |
| fanout2-d12 | full | 1536m/1 | refused `git_deadline` | no | n/a | 28130 | 3992 / 8191 | 5990 | 201840 | 0 | n/a / 103.9 | 110.5 / 110.5 | n/a / 127.0 |  |
| fanout2-d12 | full | 4g/2 | refused `unclassified` | no | n/a | 28110 | 3915 / 8191 | 5874 | 197928 | 0 | n/a / 131.8 | 138.5 / 138.5 | n/a / 152.8 |  |
| fanout2-d16 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 735 | 62 / 131071 | 98 | 3446 | 0 | n/a / 62.9 | 65.9 / 65.9 | n/a / 78.2 | 20, ok (mid_discovery:treeOccurrences>=50) |
| fanout2-d16 | control | 1536m/1 | refused `git_deadline` | n/a | n/a | 28109 | n/a / 131071 | n/a | n/a | n/a | n/a | n/a / 111.6 | n/a / 124.4 |  |
| fanout2-d16 | full | 1536m/1 | refused `git_deadline` | no | n/a | 28149 | 3104 / 131071 | 4660 | 157040 | 0 | n/a / 105.7 | 111.9 / 111.9 | n/a / 124.7 |  |
| fanout2-d16 | full | 4g/2 | refused `unclassified` | no | n/a | 28133 | 3813 / 131071 | 5723 | 192833 | 0 | n/a / 131.0 | 138.4 / 138.4 | n/a / 153.7 |  |
| fanout2-d20 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 617 | 55 / 2097151 | 91 | 3189 | 0 | n/a / 64.6 | 67.5 / 67.5 | n/a / 79.6 | 22, ok (mid_discovery:treeOccurrences>=50) |
| fanout2-d20 | control | 1536m/1 | refused `unclassified` | n/a | n/a | 28139 | n/a / 2097151 | n/a | n/a | n/a | n/a | n/a / 110.5 | n/a / 122.6 |  |
| fanout2-d20 | full | 1536m/1 | refused `git_deadline` | no | n/a | 28131 | 3885 / 2097151 | 5834 | 196550 | 0 | n/a / 105.0 | 111.4 / 111.4 | n/a / 126.2 |  |
| fanout2-d20 | full | 4g/2 | refused `git_deadline` | no | n/a | 28115 | 3988 / 2097151 | 5988 | 201738 | 0 | n/a / 134.3 | 143.1 / 143.1 | n/a / 155.3 |  |
| fanout2-d24 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 612 | 57 / 33554431 | 95 | 3315 | 0 | n/a / 64.7 | 69.9 / 69.9 | n/a / 79.6 | 13, ok (mid_discovery:treeOccurrences>=50) |
| fanout2-d24 | control | 1536m/1 | refused `unclassified` | n/a | n/a | 28120 | n/a / 33554431 | n/a | n/a | n/a | n/a | n/a / 113.4 | n/a / 130.4 |  |
| fanout2-d24 | full | 1536m/1 | refused `git_deadline` | no | n/a | 28117 | 3689 / 33554431 | 5542 | 186706 | 0 | n/a / 105.6 | 112.6 / 112.6 | n/a / 128.2 |  |
| fanout2-d24 | full | 4g/2 | refused `git_deadline` | no | n/a | 28117 | 3627 / 33554431 | 5450 | 183602 | 0 | n/a / 132.0 | 139.4 / 139.4 | n/a / 153.3 |  |
| fanout2-d64 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 642 | 62 / 36893488147419103231 | 124 | 4150 | 0 | n/a / 63.1 | 68.6 / 68.6 | n/a / 77.0 | 42, ok (mid_discovery:treeOccurrences>=50) |
| fanout2-d64 | control | 1536m/1 | refused `unclassified` | n/a | n/a | 28121 | n/a / 36893488147419103231 | n/a | n/a | n/a | n/a | n/a / 109.8 | n/a / 128.5 |  |
| fanout2-d64 | full | 1536m/1 | refused `git_deadline` | no | n/a | 28125 | 3970 / 36893488147419103231 | 5984 | 201458 | 0 | n/a / 103.6 | 110.3 / 110.3 | n/a / 125.8 |  |
| fanout2-d64 | full | 4g/2 | refused `git_deadline` | no | n/a | 28107 | 3523 / 36893488147419103231 | 5314 | 178890 | 0 | n/a / 132.2 | 139.5 / 139.5 | n/a / 151.7 |  |
| fanout2-d8 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 613 | 55 / 511 | 85 | 3027 | 0 | n/a / 62.9 | 67.7 / 67.7 | n/a / 78.2 | 19, ok (mid_discovery:treeOccurrences>=50) |
| fanout2-d8 | control | 1536m/1 | completed | n/a | n/a | 9106 | n/a / 511 | n/a | n/a | n/a | n/a | n/a / 90.1 | n/a / 108.0 |  |
| fanout2-d8 | full | 1536m/1 | completed | yes | 4289 | 8440 | 511 / 511 | 766 | 26022 | 240 | 80.8 / 83.3 | 83.2 / 90.5 | 98.5 / 113.5 |  |
| fanout2-d8 | full | 4g/2 | completed | yes | 3677 | 7526 | 511 / 511 | 766 | 26022 | 240 | 79.0 / 83.6 | 83.6 / 90.2 | 99.0 / 102.9 |  |
| repo | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 729 | 53 / 333 | 1059 | 49998 | 0 | n/a / 66.6 | 68.6 / 68.6 | n/a / 81.5 | 26, ok (mid_discovery:treeOccurrences>=50) |
| repo | control | 1536m/1 | completed | n/a | n/a | 8944 | n/a / 333 | n/a | n/a | n/a | n/a | n/a / 124.9 | n/a / 143.8 |  |
| repo | full | 1536m/1 | completed | yes | 3331 | 8508 | 333 / 333 | 9285 | 936236 | 163002634 | 91.4 / 143.8 | 129.9 / 143.5 | 110.8 / 167.2 |  |
| repo | full | 4g/2 | completed | yes | 2435 | 5948 | 333 / 333 | 9285 | 936236 | 163002634 | 92.1 / 128.1 | 129.5 / 129.5 | 114.5 / 150.4 |  |
| synthetic-10000 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 708 | 54 / 101 | 5400 | 168322 | 0 | n/a / 70.7 | 77.2 / 77.2 | n/a / 78.5 | 14, ok (mid_discovery:treeOccurrences>=50) |
| synthetic-10000 | control | 1536m/1 | completed | n/a | n/a | 3531 | n/a / 101 | n/a | n/a | n/a | n/a | n/a / 107.8 | n/a / 123.2 |  |
| synthetic-10000 | full | 1536m/1 | completed | yes | 1379 | 3938 | 101 / 101 | 10100 | 821695 | 21287571 | 87.7 / 108.0 | 85.2 / 108.0 | 99.9 / 124.2 |  |
| synthetic-10000 | full | 4g/2 | completed | yes | 960 | 2644 | 101 / 101 | 10100 | 821695 | 21287571 | 85.6 / 118.7 | 87.0 / 117.2 | 99.5 / 136.6 |  |
| synthetic-50000 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 715 | 53 / 501 | 5700 | 178413 | 0 | n/a / 72.4 | 78.0 / 78.0 | n/a / 81.1 | 21, ok (mid_discovery:treeOccurrences>=50) |
| synthetic-50000 | control | 1536m/1 | completed | n/a | n/a | 17181 | n/a / 501 | n/a | n/a | n/a | n/a | n/a / 151.3 | n/a / 175.5 |  |
| synthetic-50000 | full | 1536m/1 | completed | yes | 6155 | 18353 | 501 / 501 | 50500 | 4107233 | 106394115 | 129.7 / 150.0 | 129.5 / 150.0 | 149.4 / 175.0 |  |
| synthetic-50000 | full | 4g/2 | completed | yes | 4756 | 13008 | 501 / 501 | 50500 | 4107233 | 106394115 | 136.7 / 200.6 | 134.8 / 200.6 | 155.1 / 220.0 |  |
| wide-1024x1024 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 1330 | 54 / 1025 | 55296 | 1935112 | 0 | n/a / 93.4 | 93.4 / 93.4 | n/a / 105.7 | 25, ok (mid_discovery:treeOccurrences>=50) |
| wide-1024x1024 | control | 1536m/1 | refused `git_deadline` | n/a | n/a | 28138 | n/a / 1025 | n/a | n/a | n/a | n/a | n/a / 365.7 | n/a / 366.5 |  |
| wide-1024x1024 | full | 1536m/1 | refused `git_deadline` | yes | 20801 | 28123 | 1025 / 1025 | 1049600 | 36746481 | 240 | 292.4 / 339.1 | 319.5 / 364.6 | 315.4 / 362.3 |  |
| wide-1024x1024 | full | 4g/2 | refused `unclassified` | yes | 22459 | 28182 | 1025 / 1025 | 1049600 | 36746481 | 240 | 349.4 / 427.4 | 387.2 / 486.8 | 372.8 / 452.2 |  |
| wide-16384x16384 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 9719 | 50 / 16385 | 819200 | 28656398 | 0 | n/a / 281.4 | 310.5 / 310.5 | n/a / 309.0 | 128, ok (mid_discovery:treeOccurrences>=50) |
| wide-16384x16384 | control | 1536m/1 | refused `capture_deadline` | n/a | n/a | 28220 | n/a / 16385 | n/a | n/a | n/a | n/a | n/a / 642.9 | n/a / 663.0 |  |
| wide-16384x16384 | full | 1536m/1 | refused `capture_deadline` | no | n/a | 28863 | 143 / 16385 | 2342912 | 81987434 | 0 | n/a / 619.0 | 619.2 / 619.2 | n/a / 648.0 |  |
| wide-16384x16384 | full | 4g/2 | refused `capture_deadline` | no | n/a | 28212 | 176 / 16385 | 2883584 | 100911350 | 0 | n/a / 750.5 | 750.5 / 750.5 | n/a / 782.2 |  |
| wide-4096x4096 | cancel | 1536m/1 | cancelled `unclassified` | no | n/a | 3154 | 51 / 4097 | 208896 | 7308058 | 0 | n/a / 142.8 | 154.3 / 154.3 | n/a / 155.7 | 48, ok (mid_discovery:treeOccurrences>=50) |
| wide-4096x4096 | control | 1536m/1 | refused `capture_deadline` | n/a | n/a | 29508 | n/a / 4097 | n/a | n/a | n/a | n/a | n/a / 582.9 | n/a / 609.6 |  |
| wide-4096x4096 | full | 1536m/1 | refused `git_deadline` | no | n/a | 28185 | 496 / 4097 | 2027520 | 70965238 | 0 | n/a / 455.8 | 456.2 / 456.2 | n/a / 481.8 |  |
| wide-4096x4096 | full | 4g/2 | refused `git_deadline` | no | n/a | 28171 | 574 / 4097 | 2351104 | 82291614 | 0 | n/a / 711.8 | 722.1 / 722.1 | n/a / 742.2 |  |
