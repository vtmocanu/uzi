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

# Aggregate bound (issue #3543): M1 per-unit measurement and provisional thresholds

Measured on 2026-11-06 with `run.sh` and the harness at commit `86341fb8` (the long-path fixtures, the analytic charge model and basis labels; see README.md). The helper was the unchanged, **uncapped** `PLANNING_CAPTURE_HELPER` of base `1fca12dd`. Each configuration ran once.

- **Image:** `pbd-measure:1fca12dd`, built on the measuring host from `agent/templates/base/Dockerfile` at `1fca12dd` (`docker build`, 1559 s). It replaces the `uzi-agent-home-split:base` image of the section above, which was absent on this host. Baseline controls (repo, synthetic-50000, wide-1024x1024) were re-run in this image so the rows below compare with each other; they are not directly comparable with the earlier section's image.
- **Commands:** one invocation, `REPO_SHA=86341fb8 FIXTURES="repo synthetic-50000 wide-256x1024 wide-512x1024 wide-1024x1024 longpath-d60n250w2000f1 longpath-d60n250w2000f2 longpath-d60n250w2000f4 longpath-d64n250w0f64" MEM_CONFIGS="1536m:1:full,control 4g:2:full"`, 823 s, every job exited 0 with no OOM.
- **Raw records:** `results-2026-11-06-bound-m1.jsonl` beside this file (27 records).
- **Charged work** is the analytic model's (basis `analytic`), used only for rows whose discovery completed. On every such row the meter's fetched tree-entry total matched the model exactly (`meterMatch`). The one fixture whose discovery did not complete (`longpath-d64n250w0f64`, both `full` rows and its `control`, stopped by `git_deadline` after about 3,300–3,760 of 4,097 tree reads) is reported as a fetched upper bound and is not used below.

## Per-unit cost (container cgroup peak at the end of the capture, `control` = uninstrumented production path, 1536 MiB / 1 CPU)

| fixture | charged entries | charged path bytes | discovery (`full`, 1 CPU) | container peak, `control` 1 CPU | container peak, `full` 4g/2 CPU |
|---|---|---|---|---|---|
| repo (this repository at `86341fb8`) | 9,366 | 403,041 | 3.3 s, capture completed | 149.8 MiB | 152.7 MiB |
| synthetic-50000 | 50,500 | 503,000 | 6.7 s, capture completed | 175.8 MiB | 206.2 MiB |
| wide-256x1024 | 262,400 | 3,933,952 | 5.9 s | 193.8 MiB | 231.6 MiB |
| wide-512x1024 | 524,800 | 7,867,904 | 12.0 s | 254.4 MiB | 290.7 MiB |
| wide-1024x1024 | 1,049,600 | 15,735,808 | 24.1 s | 374.2 MiB | 437.3 MiB |
| longpath-d60n250w2000f1 | 2,060 | 31,079,270 | 0.9 s | 134.5 MiB | 139.1 MiB |
| longpath-d60n250w2000f2 | 4,120 | 62,158,540 | 1.9 s | 191.1 MiB | 175.7 MiB |
| longpath-d60n250w2000f4 | 8,240 | 124,317,080 | 3.9 s | 258.9 MiB | 263.9 MiB |

- The wide fixtures complete discovery and are then stopped by `git_deadline` in the post-discovery phase, which in these `--no-checkout` fixtures diffs every base path as a deletion (see the section above). The long-path fixtures complete discovery and are then refused with `errno:ENAMETOOLONG`, when the post-discovery walk reaches a path longer than the host's `PATH_MAX`. Peaks therefore cover discovery plus whatever post-discovery work ran.
- **Per entry occurrence** (wide, about 15 path bytes each): (374.2 − 193.8) MiB / 787,200 entries ≈ **240 bytes per entry**, intercept ≈ 134 MiB. This is an empirical estimate for base discovery on this host and image only; the 4g/2 CPU rows give about 274 bytes per entry.
- **Per charged path byte** (long-path, few entries): 1.91 (f1 → f2) and 1.14 (f2 → f4) bytes of container memory per path byte; ≈ 1.40 over the whole range (the instrumented `full` 1 CPU rows give 1.64 and 1.50). The largest, 1.91, is used below; it is noisy, driven by the f2 `control` peak sitting about 17.5 MiB above its `full` row. This is likewise an empirical estimate.

## Realistic trees (rule 5a)

Counted on 2026-11-06 from depth-1, blob-less bare clones of each repository's default branch with `git ls-tree -r -t -z --name-only HEAD`. Entries are files, directories and gitlinks (Chromium has 273 submodule entries; Linux none), and path bytes are the sum of every entry's full relative path, which is exactly what the caps charge.

| repository | commit | entries | path bytes | longest path |
|---|---|---|---|---|
| Linux (`github.com/torvalds/linux`) | `d4872371` | 103,697 | 4,018,329 | 110 |
| Chromium (`github.com/chromium/chromium`, the GitHub mirror of `chromium/src`; it excludes DEPS-fetched checkouts) | `ee70694d` | 563,282 | 42,616,138 | 200 |

## Provisional thresholds

- **`baseDiscoveryEntryCap` = 1,200,000** (2.13× Chromium's 563,282 entries).
- **`baseDiscoveryPathByteCap` = 100,663,296 (96 MiB)** (2.36× Chromium's 42,616,138 path bytes).
- **Estimated container peak with both caps reached:** 134 MiB + 1,200,000 × 240 B (≈ 275 MiB, already including about 15 path bytes per entry) + (96 MiB − 17 MiB) × 1.91 (≈ 151 MiB) ≈ **560 MiB**, inside the 1536 MiB per-slot budget. This is an estimate only; M3 measures the capped helper and decides acceptance.
- **Time:** at 1 CPU the wide fixtures charge about 44,000 entries/s, so a wide base reaches the entry cap after about 27.5 s (projected from wide-1024x1024), within about 0.5 s of the 28 s helper deadline. Which one binds depends on host speed (the 2 CPU rows charge 50,700–54,150 entries/s). The path-byte cap is reached in seconds (about 3 s for 96 MiB at the f4 rate).

# Aggregate bound (issue #3543): M3 measurement with the capped helper and acceptance

Measured on 2026-11-06/07 with the capped helper (`baseDiscoveryEntryCap` = 1,200,000, `baseDiscoveryPathByteCap` = 100,663,296) and the harness at commit `455673e7`, in the same `pbd-measure:1fca12dd` image as the M1 section. The image is the runtime only: `run.sh` bind-mounts this checkout's `agent/`, so the helper measured is the one at `455673e7`. Each configuration ran once unless stated. Every job exited 0, with no OOM, no harness kill and no `FAIL` flag.

- **Commands** (all `MEASURE_IMAGE=pbd-measure:1fca12dd`):
  1. production rows, 1127 s: `REPO_SHA=455673e7 FIXTURES="repo synthetic-50000 wide-1170x1024 wide-1171x1024 longpath-d60n250w2000f3 longpath-d60n250w2000f4 distinct-1170x1024n75 fatchain-d64" MEM_CONFIGS="1536m:1:full,control 4g:2:full"`;
  2. rerun and sizing-batch rows: the same with `FIXTURES="repo synthetic-50000 distinct-256x1024n75 distinct-512x1024n75"`;
  3. twelve validation probes, 1641 s: one invocation each with `MEM_CONFIGS="4g:2:full"`, `LIMITS_JSON` and `LIMITS_TAG` (below).
- **Raw records:** `results-2026-11-06-bound-m3.jsonl` beside this file. It holds 50 records, each tagged with its `run` (`m3-production`, `m3-rerun`, `m3-probes`, `m3-deadline-scaled`).
- **Disk:** each configuration's container and its anonymous volumes were removed with `docker rm -v` as soon as its results were on the mounted output directory. No measurement container remained after any invocation.

## Production rows (default limits)

| fixture | what it exercises | outcome, `control` 1536m/1 | container peak MiB, `control` 1536m/1 | outcome, `full` 4g/2 | container peak MiB, `full` 4g/2 |
|---|---|---|---|---|---|
| repo (`455673e7`, 9,368 entries) | clean control | completed | 146.4 | completed | 218.9 |
| synthetic-50000 (50,500 entries) | clean control | completed | 177.6 | **`capture_deadline`** (28.2 s, first run); completed in 13.9 s on the rerun | 295.0; rerun 210.5 |
| wide-1170x1024 (1,199,250 entries, under the entry cap) | entry cap, just under | `capture_deadline` before discovery finished | 442.2 | `capture_deadline` (1,098,898 entries fetched) | 440.1 |
| wide-1171x1024 (1,200,275 entries) | entry cap, just over | **`base_discovery_entry_cap`** (25.7 s) | 367.1 | `git_deadline` before the cap (1,185,939 entries fetched) | 447.1 |
| longpath-d60n250w2000f3 (93,237,810 path bytes) | path-byte cap, just under | discovery completed, then `errno:ENAMETOOLONG` (see the M1 section) | 226.1 | same | 226.3 |
| longpath-d60n250w2000f4 (124,317,080 path bytes) | path-byte cap, just over | **`base_discovery_path_byte_cap`** (3.7 s) | 229.9 | **`base_discovery_path_byte_cap`** | 230.7 |
| distinct-1170x1024n75 (1,199,250 entries, 1,198,080 distinct blobs, 99,448,830 path bytes) | both caps nearly reached, maximal sizing batch | `capture_deadline` before discovery finished | 449.3 | `git_deadline` (974,994 entries fetched) | 448.5 |
| fatchain-d64 (65 nested trees of up to 1.95 MiB, 60,000 filler files each) | pinned in-flight tree buffers | **`base_discovery_path_byte_cap`** (13.1 s) | **502.4** | **`base_discovery_path_byte_cap`** | **575.5** |

- **Charged work at the cap refusals**, labelled `derived-from-refusal` (7c) and confirmed by the probes below where noted:
  - wide-1171x1024 at 1536m/1: E = 1,200,001, P = 17,990,632. The P figure is not probe-confirmed at that index (see the probes).
  - longpath-d60n250w2000f4: E = 6,696, P = 100,678,440.
  - fatchain-d64: E = 830,983, P = 100,663,348. The fat chain's filler file names, charged under a 64-level prefix, reach the path-byte cap before the entry cap.
- **Discovery speed binds before the entry cap on this host.** No near-cap wide or distinct base finished discovery within the 28 s helper deadline at either configuration. The entry cap fired only on wide-1171x1024 at 1 CPU (`control` 25.7 s, `full` 24.9 s). On this host the deadline therefore stops most near-cap bases first. The caps bound memory on any host, however fast.
- **The synthetic-50000 `capture_deadline` at 4g/2** happened in the first production run, with a host load average of about 10 from other runs. Its discovery had completed (5,964 ms, 50,500 entries, `meterMatch`). The deadline fired in the post-discovery file walk, at 28.2 s, against 13.0–14.6 s in every other 4g/2 run of this fixture (#3991 13.0 s, M1 14.6 s, the rerun here 13.9 s). It is 24× below the entry cap and 200× below the path-byte cap, so the caps cannot cause it. The rerun completed at both configurations.

## Sizing-batch worst case (distinct blobs)

The auditor of M2 noted that every M1 fixture shared one blob, so the post-discovery sizing batch (`cat-file --batch-check` over the distinct blob ids) was never large. The distinct fixtures complete discovery, including the sizing batch, up to 512 directories:

| fixture | entries / distinct blobs | discovery (`full` 1536m/1) | container peak MiB, `control` 1536m/1 | container peak MiB, `full` 4g/2 |
|---|---|---|---|---|
| distinct-256x1024n75 | 262,400 / 262,144 | 10.9 s | 301.2 | 386.4 |
| distinct-512x1024n75 | 524,800 / 524,288 | 21.6 s | 543.4 | 505.1 |

- **Superseded by the M3 joint-cap measurement below** (validation, deadline lifted): a base just under both caps with the full sizing batch was then measured, and the extrapolation is kept only as context.
- **Extrapolated to the entry cap** (1,199,250 entries, all distinct). This is an extrapolation, not a measurement, because no such base completes discovery inside the deadline on this host.
  - Linear in the `control` rows: 301.2 MiB + 936,850 × 968 B ≈ **1,166 MiB**.
  - Linear in the `full` 4g/2 rows: ≈ 810 MiB.
  - Both rows include post-discovery work these `--no-checkout` fixtures add (every base path diffs as a deletion until `git_deadline`), whose share is bounded by time rather than size, so a two-point linear fit is an estimate, not a bound. No measured row reached both caps together.

## Near-both-caps peak (validation, deadline ×4, 1536m/1)

**Superseded by the M3 joint-cap measurement below** (validation, deadline lifted), which completed discovery and the full sizing batch at both caps; this partial lower bound is kept as context.

Measured on 2026-11-07 with the harness's `DEADLINE_SCALE` mode (README, "Deadline scale") at the shipped caps, so that a near-cap base can get past the 28 s helper deadline on this host. Only the helper's clock, the outer capture timeout and the harness timers are stretched ×4; every cap and memory bound stays at its production value. Each git command's timer is 5 s of real time, clamped to the deadline's remaining helper time (README, "Deadline scale"): the distinct row failed at about 9.7 s of helper time, under the plain 5 s clamp, while the wide row's post-discovery commands after 23 s of helper time ran under a shorter clamp. These rows measure memory at the caps, not production timing. Command, as actually run (one invocation, one container): `DEADLINE_SCALE=4 MEASURE_IMAGE=pbd-measure:1fca12dd FIXTURES="distinct-1170x1024n75 wide-1170x1024" MEM_CONFIGS="1536m:1:full,control"`, with the harness at `3258368a` (the helper is unchanged since the caps landed). Peaks are the container cgroup peak at the end of the capture, as in the earlier sections (the distinct row's sampled process-tree RSS peak was 788.3 MiB). Every job exited 0, with no OOM and no harness kill.

| fixture | outcome | discovery | container peak MiB (cgroup, end) | real capture ms |
|---|---|---|---|---|
| distinct-1170x1024n75 (1,199,250 entries = 99.94 % of the entry cap; 99,448,830 path bytes = 98.8 % of the path-byte cap; 1,198,080 distinct blobs) | `git_deadline` in the sizing batch (that `cat-file --batch-check` exceeded its own 5 s real-time timer) | all 1,199,250 entries fetched (meter); the sizing batch was partly read | **767.5** | 39,815 |
| wide-1170x1024 (1,199,250 entries; 17,979,390 path bytes) | discovery completed (charged 1,199,250 / 17,979,390, `analytic; meter match`), then a post-discovery `git_pipe_cap` from this `--no-checkout` fixture's all-deleted diff | completed, 28.8 s real (7.2 s helper clock) | 499.9 | 103,451 |

- The distinct row is the closest this host gets to both caps reached: discovery of every entry plus part of the maximal sizing batch (26,550,384 bytes of `--batch-check` output read for 1,198,080 ids at 48 or more bytes per line, so under half the batch), in `full` (instrumented) mode, which adds metering overhead. Its 767.5 MiB peak complements, and does not replace, the earlier two-point extrapolation for a completed sizing batch (about 1,166 MiB, in the sizing-batch section above): it is a partial measurement and only a lower bound for that base; the measured 767.5 MiB is half the 1536 MiB budget, while the completed-batch estimate leaves about 1.3× headroom. It is not a completed whole capture: the sizing batch's 5 s per-command timer fired first.
- The same invocation also ran `control` rows. `control` runs the unmodified helper without the metering preamble that slows the clock, so those rows ran at the production deadline under a stretched label; they were discarded (not in the jsonl). The full-mode code path is identical in the current harness, which, like `run.sh`, now refuses `DEADLINE_SCALE` for any mode other than `full` (so this exact command is no longer accepted; rerun it with `MEM_CONFIGS="1536m:1:full"`).

## Validation probes (7f, injected limits, 4g/2)

For each index i, the pair `(entryCap = i, pathByteCap = Pm(i) − 1)` must refuse with `base_discovery_path_byte_cap` (actual A(i) ≥ Pm(i)), and `(entryCap = i, pathByteCap = Pm(i))` must refuse with `base_discovery_entry_cap` (A(i) ≤ Pm(i)). Pm comes from the harness's analytic model.

| fixture | i | Pm(i) | path-token probe | entry-token probe | result |
|---|---|---|---|---|---|
| longpath-d60n250w2000f4 | 6,695 | 100,663,130 | path cap | entry cap | A = Pm |
| longpath-d60n250w2000f4 | 6,696 | 100,678,440 | path cap | entry cap | A = Pm |
| fatchain-d64 | 830,982 | 100,663,240 | path cap | entry cap | A = Pm |
| fatchain-d64 | 830,983 | 100,663,348 | path cap | entry cap | A = Pm |
| wide-8x1024 | 3,000 (inside a leaf tree buffer) | 44,976 | path cap | entry cap | A = Pm |
| wide-1171x1024 | 1,200,000 | 17,990,632 | `capture_deadline` | `capture_deadline` | inconclusive: the deadline fired before entry 1,200,000 |

- The pairs at k and k − 1 pin both production path-cap refusals (longpath-d60n250w2000f4 and fatchain-d64) to exactly k with the derived E and P: A(k − 1) = Pm(k − 1) ≤ 100,663,296 < Pm(k) = A(k).
- The probe meter's fetched counts (8,240 longpath; 3,900,064 fatchain; 3,080 wide-8x1024) exceed the charged counts. This shows why fetched counts are never reported as charges.

## Decision (M3 rule)

1. **Memory:** the acceptance basis is the joint-cap measurement (section below; validation, deadline lifted): a base at 99.94 % of the entry cap and 99.98 % of the path-byte cap, with the maximal sizing batch, peaked at 985.1 MiB (validation, deadline lifted, unmetered) on the 1536 MiB / 1 CPU slot and at 1,111.9 MiB (validation, deadline lifted, metered) at 4g/2. Both are below 1536 MiB. At production timing the largest measured whole-capture container peak is 575.5 MiB (fatchain-d64, 4g/2); on the 1536 MiB / 1 CPU slot it is 543.4 MiB (distinct-512x1024n75, `control`), and 502.4 MiB among the cap-refusal rows (fatchain-d64). The earlier ×4 partial row (767.5 MiB) and the two-point extrapolation of the sizing batch (about 1,166 MiB) are superseded context.
2. **Clean controls:** this repository and synthetic-50000 capture at the caps in the M2 tests (`git-planning-diff.test.ts`, "Base discovery aggregate caps (#3543)") and in the harness rerun at both configurations. The first production run's synthetic-50000 4g/2 row did not pass: it is a post-discovery `capture_deadline` under host load, not a cap (its discovery completed 24× below the entry cap). This acceptance counts the disclosed rerun; under a strict reading in which every M3 row must pass, that row is the exception.
   The later production rerun of both clean controls (joint-cap section below) completed at both configurations, synthetic-50000 included (21.0 s and 13.7 s).
3. **Realistic margin (rule 5a):** the caps stay at 2.13× (entries) and 2.36× (path bytes) Chromium's tree, as in the M1 section.

**Accepted:** `baseDiscoveryEntryCap` = 1,200,000 and `baseDiscoveryPathByteCap` = 100,663,296 (96 MiB). No lowering iteration was needed.

**Residual, not measured:**
- Compose enforces memory per worker, not per run, so concurrent runs in one worker share the 1536 MiB-per-slot arithmetic.
- In the lifted rows, discovery and the full sizing batch at both caps completed (metered row: discovery at 50,515 ms, sizing closed at 45,161 ms). The whole capture then ended on the fixture's post-discovery `git_pipe_cap`, an artifact of the `--no-checkout` fixture (every base path diffs as a deletion). Production timing was not exercised in those rows: the helper deadline and per-command timer were lifted, so whether a base this size finishes inside 28 s on a production host is not measured.
- The post-discovery phase of such a base (reading a real checkout) was not measured; the `--no-checkout` fixture cannot exercise it.
- Each configuration ran once.

# Aggregate bound (issue #3543): M3 joint-cap measurement (validation, deadline lifted)

Measured on 2026-11-07 with the capped helper (`baseDiscoveryEntryCap` = 1,200,000, `baseDiscoveryPathByteCap` = 100,663,296, the helper defaults) to answer the one question the earlier sections left open: the peak of a base just under both caps with the full sizing batch. The deadline-bound rows above never got there, because the 28 s helper deadline stops such a base first on this host. Rows are named "validation, deadline lifted, unmetered" (`control`, no preamble) and "validation, deadline lifted, metered" (`full`). None is a production-path control: the helper's wall-clock bounds differ from production (below), so these rows measure memory and charges at the caps, not production timing. The peaks are measured values for these fixtures, not upper bounds for every base under the caps.

- **Environment:** harness at `e613f033` (the measure code at `cf297d58` differs only in that the report skips `verify.json`); image `pbd-measure:73969ef3`, built locally 2026-11-07 from `agent/templates/base/Dockerfile` (node v26.12.1, git 2.54.0), runtime only (`agent/` bind-mounted). The helper has not changed since the caps landed (`agent/src` is untouched in this rework). Raw records: `results-2026-11-07-bound-m3-lift.jsonl` beside this file (10 records, each tagged `run`: `m3-lift` or `m3-lift-clean-controls`).
- **Deadline lift (`DEADLINE_LIFT=1`, harness only; README, "Deadline lift"):** the harness rewrites the helper source it spawns, failing closed unless each marker occurs exactly once. The helper deadline `Date.now() + 28000` becomes 1,800,000 ms and the per-git-command timer ceiling `Math.min(5000, ...)` becomes 1,800,000 ms. The outer capture timeout is 1,830 s, the harness scope timer 1,860 s and `run.sh`'s per-job wait 1,900 s. The per-command timer is lifted as well because the 28 s deadline clamps it, and the earlier ×4 row stopped on it at under half the sizing batch. Every cap and memory bound keeps its production value. No production flag or hook exists.
- **Phase evidence:** unmetered rows carry no preamble, so a parent-side `/proc` observer (no helper instrumentation) records when the sizing `cat-file --batch-check` was seen and gone, when the clone index was opened (best-effort) and when a later git command was spawned. A row proves the post-sizing window if it completed, or if it was refused after the sizing batch was seen and gone and a later spawn or index open followed.
- **Commands** (all `MEASURE_IMAGE=pbd-measure:73969ef3`, `OUT` reused per run):
  1. 1a: `DEADLINE_LIFT=1 FIXTURES="distinct-1170x1024n76" MEM_CONFIGS="1536m:1:control"`;
  2. 1b: the same with `FIXTURES="distinct-1171x1024n75 distinct-1170x1024n77"`;
  3. 2: `DEADLINE_LIFT=1` with the three fixtures and `MEM_CONFIGS="4g:2:full"`;
  4. 3, production (no lift): `REPO_SHA=e613f033 FIXTURES="repo synthetic-50000" MEM_CONFIGS="1536m:1:control 4g:2:full"`.
- **Termination:** every invocation exited 0; every job exited 0 with no OOM, no harness kill and no preserved container. Containers were removed with `docker rm -v` after the readability gate passed. Fixture generation took about 230-237 s each. The host load average was about 5-10 during run 3. Disk preflight floor 5 GiB (Docker store about 24.9 GiB free, the data filesystem about 35 GiB at the time).

## Fixtures

All blobs distinct (`distinct-<F>x<W>n<L>`; README, "Fixtures"). Percentages are of the production caps.

| fixture | entries | path bytes | distinct blobs | position |
|---|---|---|---|---|
| distinct-1170x1024n76 | 1,199,250 (99.94 % of the entry cap) | 100,646,910 (99.98 % of the path-byte cap) | 1,198,080 | just under both caps; maximal sizing batch |
| distinct-1171x1024n75 | 1,200,275 (over) | 99,533,829 | 1,199,104 | entry cap exceeded first |
| distinct-1170x1024n77 | 1,199,250 | 101,844,990 (over) | 1,198,080 | path-byte cap exceeded first |

## Rows

Container peak is the cgroup `memory.peak` at the end of the capture, as in the earlier sections. "Derived" charges are `derived-from-refusal (unverified until 7f probe)`.

| fixture | config | row | outcome | container peak MiB | real capture s |
|---|---|---|---|---|---|
| distinct-1170x1024n76 | 1536m / 1 CPU | validation, deadline lifted, unmetered | `git_pipe_cap` after discovery and sizing (phase evidence below) | **985.1** | 94.3 |
| distinct-1170x1024n76 | 4g / 2 CPU | validation, deadline lifted, metered | discovery completed at 50,515 ms (sizing closed at 45,161 ms; charged 1,199,250 entries / 100,646,910 path bytes, meter match), then `git_pipe_cap` | **1,111.9** (also at the boundary) | 89.1 |
| distinct-1171x1024n75 | 1536m / 1 CPU | validation, deadline lifted, unmetered | `base_discovery_entry_cap` (derived E = 1,200,001, P = 99,511,004) | 486.6 | 34.8 |
| distinct-1171x1024n75 | 4g / 2 CPU | validation, deadline lifted, metered | `base_discovery_entry_cap` (same derived charge) | 590.7 | 33.0 |
| distinct-1170x1024n77 | 1536m / 1 CPU | validation, deadline lifted, unmetered | `base_discovery_path_byte_cap` (derived E = 1,185,336, P = 100,663,314) | 479.3 | 35.1 |
| distinct-1170x1024n77 | 4g / 2 CPU | validation, deadline lifted, metered | `base_discovery_path_byte_cap` (same derived charge) | 645.0 | 33.4 |
| repo (`e613f033`) | 1536m / 1 CPU | production, unmetered | completed | 143.7 | 8.5 |
| repo (`e613f033`) | 4g / 2 CPU | production, metered | completed (9,371 entries / 403,307 path bytes, meter match) | 148.8 | 5.9 |
| synthetic-50000 | 1536m / 1 CPU | production, unmetered | completed | 188.1 | 21.0 |
| synthetic-50000 | 4g / 2 CPU | production, metered | completed (50,500 entries / 503,000 path bytes, meter match) | 224.8 | 13.7 |

- The helper's own maxRSS in the metered n76 row was 1,022,288 KiB at the discovery boundary and at exit.
- The metered n76 peak at the discovery boundary equals the peak at the end (1,111.9 MiB), so on this row the peak was reached by the end of discovery and the sizing batch; the post-discovery phase added nothing to it.
- The unmetered n76 row's sampled process-tree RSS peak was 950.0 MiB; its cgroup peak includes the harness parent (about 60 MiB) and page cache.
- Both n76 rows ended on `git_pipe_cap` in the post-discovery phase. In these `--no-checkout` fixtures every base path diffs as a deletion, so that phase is shaped by the fixture, not by production (as for `wide-1024x1024` above).
- The cap-refusal rows peak well below the joint-cap row because they stop at the first cap, before the sizing batch.

## Phases of the accepted row (n76, 1536m / 1 CPU, unmetered)

Observer times, ms from the job start:

| event | ms |
|---|---|
| sizing `cat-file --batch-check` seen | 36,842 |
| sizing batch gone | 49,072 |
| later git spawn seen | 61,186 |
| clone index open seen | not observed (best-effort) |

Phase evidence `post_sizing_observed`: the sizing batch was seen and gone, and a later git spawn followed it, all before the refusal. The metered n76 row independently recorded the discovery boundary at 50,515 ms and its first post-boundary spawn at 54,106 ms.

## Verification

A `verify` job (README, "Row verification") validated every cited row: container exit 0, no OOM, no harness kill, no failures, exact lift values on the lifted rows, and the expected outcome. Expectations: `phase-evidence` for the two n76 rows, `base_discovery_entry_cap` for n75, `base_discovery_path_byte_cap` for n77, `completed` for the four clean controls. Verdict: all 10 rows valid.

## Decision (M3 joint-cap rule)

Accepted. The joint-cap 1536 MiB / 1 CPU unmetered row is valid with phase evidence and peaks at 985.1 MiB, below 1536 MiB; its metered 4g/2 row is valid and peaks at 1,111.9 MiB, also below 1536 MiB. Both over-cap fixtures refuse on their own cap at both configurations. Both clean controls complete at both configurations. No lowering of either cap. The rule 5a margins are unchanged: 2.13x (entries) and 2.36x (path bytes) Chromium's tree.

Limits of this measurement, in addition to the residuals in the decision above: one host, one image, each row ran once; the lifted rows do not show that a base this size completes inside the production deadline; the headroom is about 1.6x unmetered (985.1 against 1536 MiB) and 1.4x metered (1,111.9), not the 2.7x of the production-timing rows (575.5 MiB).
