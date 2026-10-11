# PRD #2755: Compress terminal runs' messages into chunked archives (lossless, off by default)

**Issues:** [#2755](https://github.com/vtmocanu/uzi/issues/2755) (part of [#80](https://github.com/vtmocanu/uzi/issues/80)).
**Status:** Parked after M0: the pass criteria fixed before measuring are not all met; awaiting the owner's go/no-go decision (see M0 results).
**Execution:** M0 delivered by the #2755 run; M1–M4 run as a separate gated run after an owner go decision.

## Progress

- [x] M0 measured (synthetic), #2755 run, harness 5e1b30d6
- [ ] Owner go/no-go recorded
- [ ] M1
- [ ] M2
- [ ] M3
- [ ] M4

## Why

`run_messages` grows without bound and was the largest relation when a hosted instance's Postgres volume filled (#2623). On a 189-run sample, rolling a terminal run's messages into compressed chunks cut the table's physical size by **63%** (425 MB to 157 MB) in a real PostgreSQL 17; per-message compression saved only 11.5% because TOAST already compresses large payloads. Measurements and method: [#80 comment](https://github.com/vtmocanu/uzi/issues/80#issuecomment-6110889256).

This PRD is the lossless, DB-only step under #80. Retention (trimming or deleting content) stays parked. An object-storage tier stays planned for later: the chunk this PRD defines must be the unit that tier would store, but this PRD builds no storage backend abstraction.

## Outcome

A terminal run's messages, once eligible, are stored as compressed chunks instead of one row per message. Every reader returns exactly what it returned before, a late delivery for an archived sequence is never inserted twice or rebroadcast, and the feature ships **disabled by default** in both the Helm chart and compose.

## Requirements

1. **Lossless.** A rolled-up run reads back byte-identical to its rows: every field (`id`, `seq`, `kind`, `agent`, `agent_instance`, `agent_label`, `payload`, `created_at`, `claim_generation`), and the gapless `seq` contract. A run is never deleted from `run_messages` until its chunks have been verified to round-trip.
2. **Chunk format.** Byte-bounded chunks with a message-count ceiling (the measurement used 256 KiB uncompressed and 1000 messages; the plan picks values from M0's data). A single oversized message is stored whole, never truncated. Each chunk carries a format version, its seq range, a decoded-size bound checked before decompression, and an integrity check.
3. **Ingest stays idempotent after rollup.** Late worker deliveries can reach a terminal run (`usage_fold.go` documents this; `InsertRunMessage` fences on claim and generation, not on status, and not every terminal transition is proven to release the claim). Deleting rows removes today's `(run_id, seq)` dedup, so an archived run gets a watermark that ingest checks **atomically** with the insert: a delivery at or below it is answered as a duplicate, not inserted and not rebroadcast. `last_seq`, gap and contiguity semantics, duplicate-vs-fence-rejection reporting, and usage attribution stay unchanged.
4. **Every reader keeps working, through one shared seam.** The direct SQL readers today are in `api/internal/store/queries/` `runtime.sql`, `chat.sql`, `judge.sql`, `jobs.sql`, `slack.sql` and `codex_binding.sql`, with consumers including `workersvc/usage_refold.go`, `usage_fold.go`, `milestone_lanes.go`, `plan_revising.go`, `run_activity.go`, `judge_trace.go`, `chat.go` and `handler/runs_lifecycle.go`. The plan re-derives this inventory at its base commit. In particular:
   - forward, backward/tail and filtered paging apply their limit **after** the filter and span chunks as needed, without decoding a whole run per page and without an unbounded cache;
   - the judge's filtered tool-trace read and its paginated full trace both work across archived and live messages;
   - the jobs read keeps its owner and product scope and its text-only projection when filtering moves out of SQL.
5. **Eligibility.** A run becomes eligible a grace period after reaching `completed`, `failed` or `cancelled` (initially 7 days, configurable; a rollout policy, not a measured optimum) and only when no work that can append to it is active. The plan defines that rule precisely; it must not be so broad that a run can stay ineligible forever.
6. **Backfill.** Existing history is rolled up by a resumable, throttled background job, never inside a boot migration. Transactions are bounded and an interrupted rollup leaves the run fully readable. The plan budgets disk, WAL and temporary duplication.
7. **Off by default.** Disabled in the chart and in compose. Enabling starts rollups; disabling stops new rollups while archived runs stay readable. Disabling does **not** make archived runs readable by an older API binary. Hosted k8s is verified as first-class.
8. **Rollback.** Once rows are deleted, an older binary reads incomplete logs. Provide a verified hydration path (archive back to rows, with its time and disk cost stated) and name the oldest version that reads archives in the changelog.
9. **Reclaiming disk is an operator step.** Deleting rows makes space reusable after vacuuming. Returning space to the filesystem may need an operator-controlled relation rewrite (`VACUUM FULL` or `pg_repack`), with enough temporary disk and its locking constraints; neither shrinks the provisioned volume. Document it; never run it automatically.
10. **Atomic publication, exactly-once reads.** Publishing a run's chunks and deleting their source rows is atomic. A concurrent reader sees every message exactly once, including a request that spans archived chunks and live rows (one snapshot, not separate archive and live queries). Messages above the watermark stay readable and eligible for a later rollup.

## Milestones

- **M0, measure (go/no-go).** In a throwaway PostgreSQL with production-format rows, measure physical size, first-page, tail-page and judge-scan latency for live vs archived runs, concurrent readers, and backfill disk/WAL cost. The plan states the pass criteria up front. **After M0 the run stops for an owner decision** before any further milestone; an unfavourable result parks the run with its evidence.
- **M1, format and read seam.** Chunk storage and the shared read seam for every reader in requirement 4. No deletion yet.
- **M2, ingest watermark.** Requirement 3.
- **M3, rollup and backfill job.** Requirements 1, 5, 6 and 10, behind the disabled-by-default setting. The first milestone allowed to delete rows; M1 and M2 must be complete.
- **M4, rollback, docs, ADR.** Hydration, operator docs (enable, disable, hydrate, reclaim disk), changelog entry, and an ADR for the durable invariants (chunk format and its versioning, the watermark contract).

## Acceptance

- [ ] M0 results posted on this issue and the owner's go decision recorded before M1 starts.
- [ ] Live-DB regressions, each observed failing on the unfixed seam and passing with the fix: duplicate redelivery after rollup (not inserted, not rebroadcast); an archive-vs-ingest race; a reader racing chunk publication (every message exactly once); forward, tail and filtered paging across chunk boundaries; usage refold over an archived run; a judge read spanning archived and live messages; an interrupted backfill leaving the run readable.
- [ ] Round-trip equality for every field on representative runs, including an oversized single message.
- [ ] Disabled by default in chart and compose; enable and disable verified on hosted k8s.
- [ ] Hydration verified; docs cover enable, disable, hydrate and disk reclaim.
- [ ] ADR written; `docs:sync` run for any `docs/*.md` change.

M0 evidence is in the M0 results section below; posting it on #2755 and recording the owner decision are maintainer steps; the hosted-k8s enable/disable runbook belongs to M3.

## Out of scope

The object-storage tier, retention or trimming, and turning the feature on by default (a follow-up after hosted measurement). No `.github/workflows/**` change.

Part of #80.

## M0 pass criteria (fixed before measuring)

The criteria below were written before any measurement and are not changed after it. Percentiles are p95 unless stated.

- **C1 size.** For the chosen configuration (per codec), the footprint of the archived runs after rollup (chunk + watermark tables) is <= 50% of their pre-rollup footprint (VACUUM FULL baseline), and reusable free space after plain VACUUM is >= 40% of it. (#80 saw a 63% reduction.)
- **C2 pages.** 200-row first, tail and backward pages: archived p95 <= 50 ms and <= 3x live (transaction-wrapped live baseline) at 8 readers in every size stratum; 1000-row pages and the full replay: archived p95 <= 3x live; 32 readers reported only.
- **C3 judge, usage, jobs, chat list.** Judge signal scan archived p95 <= 500 ms on the large stratum; judge trace page, jobs read and usage scan <= 3x live; `ListChatRunsForUser` at 50 runs <= 2x live.
- **C4 lossless.** 100% every-field round trip for both codecs including the oversized message; zero duplicate, missing or lost seqs in the reader and straggler-writer checks; all corruption and bound unit tests pass for both codecs.
- **C5 backfill** (worst case: checkpoint immediately before, VACUUM included). WAL <= 2.5x the pre-rollup bytes of the rolled-up runs (rate reported); peak extra disk <= 1.1x final chunk-table size + 64 MiB and final chunk table <= 35% of the archived runs' pre-rollup footprint; rewrite headroom reported (no bar); largest transaction <= 30 s; live-reader p95 during the throttled backfill <= 1.5x idle.
- **C6 decode cost.** Per-request allocation <= 4x the bytes returned plus one chunk bound; peak heap at 32 readers <= 32 x (chunk bound + largest message) + 64 MiB.
- **C7 inventory.** Every `run_messages` reader is classified with caller evidence; none is left without a chunk mapping or a recorded proof that terminal runs never reach it.

Verdict rule: go if C1–C7 all pass for at least one configuration of either codec; otherwise no-go/parked with evidence. The owner decides either way and picks the codec.

## M0 results (synthetic corpus)

**All numbers in this section come from a SYNTHETIC corpus.** Compressibility, kind mix and message sizes may differ from production. Raw evidence: `prds/2755-m0/results.json`, `prds/2755-m0/settings.json` and `prds/2755-m0/summary.md`, produced by `scripts/measure-run-message-rollups.sh --scale full` at harness commit 5e1b30d6 (exit 0, all phases completed, 3202 s).

**Verdict: no configuration passes C1–C7.** All four materialised configurations are `fail` overall. C1 and C4 pass everywhere; C2 passes on three of four; C3, C5 and C6 fail on all four.

### Method

- Throwaway `postgres:17` container (17.11) with stock settings (the harness changes no server setting, only per-table `autovacuum_enabled=false` on `run_messages` and the chunk table), api migrations at head.
- Deterministic synthetic corpus (seed 1): 488,407 rows over 200 runs (169 terminal, 31 live); heap 284,688,384 bytes (271 MiB), TOAST 179,396,608 bytes (171 MiB), indexes 57,212,928 bytes (54 MiB); TOAST is 38.7% of heap plus TOAST; largest payload 3,330,366 bytes. Kind mix: text 159,668; tool_use 131,019; tool_result 124,862; status 21,860; progress_note 14,466; error 10,995; thinking 10,933; plan 7,352; summary_usage 7,252. Large `tool_result` bodies are slices of this repository's tracked text files; rows are interleaved across concurrently active runs.
- Size strata of terminal runs: small 63–207 messages (20 runs), median 416–929 (20 runs), large 21,025–29,973 (5 runs). 425,420 rows belong to the 169 archived (terminal) runs, whose pre-rollup footprint (VACUUM FULL baseline) is 443,170,816 bytes.
- Prototype chunk format: header with magic, version, codec and level, seq range, row count, uncompressed length (checked before decode) and CRC-32C; length-prefixed records of every field. The chunk table stores per-chunk `kinds` and `last_created_at` metadata beside the data (`data` column `STORAGE EXTERNAL`).
- One `REPEATABLE READ READ ONLY` snapshot per archived read, and the same for the live baseline. Rollup transaction: lock the runs `FOR UPDATE`, insert chunks, verify, delete the source rows bounded by seq range, write the watermark; at most 8 chunks per transaction.
- Codecs: stdlib `compress/flate` (pooled writers and readers) and `github.com/klauspost/compress/zstd` v1.20.1. Recorded zstd settings: requested level `n` is passed through `zstd.EncoderLevelFromZstd(n)` (the harness does not record the resolved encoder level separately); one shared encoder per requested level using `EncodeAll`, encoder concurrency 8, window 1,048,576 bytes; pooled streaming decoders (concurrency 1, max memory 67,108,864 bytes) read through an `io.LimitReader` of the declared length + 1.
- Environment: Go 1.27.2, `GOMAXPROCS` 4 on 8 CPUs, warm database cache (untimed warm-up per cell), no application cache on the archived path, backfill throttle 50 ms between runs, 1000 iterations (heavy shapes fewer, recorded per cell), readers 1, 8 and 32, pgx pool of 40 connections.

### Offline size grid and the materialised configurations

The offline size grid ran over a sample of the corpus, not the whole of it: `grid_sample` is 21 runs, 35,617 rows and 50,620,369 uncompressed bytes (cap 48 MiB). It covers 6 codec levels (flate-1/6/9, zstd-1/3/9) x byte bounds 64/128/256/512 KiB x count ceilings 500/1000/2000, 72 cells. Uncompressed over compressed on the sample ranged from 2.60 (flate-1, 64 KiB/500) to 3.28 (zstd-9, 512 KiB/500).

Four configurations were materialised in full: the best flate and best zstd by ratio (flate-9/512KiB/1000, zstd-9/512KiB/500), and the two references flate-6/256KiB/1000 and zstd-3/256KiB/1000 (the 256 KiB / 1000 shape used in the #80 comparison).

### Verdicts per configuration

| config | overall | C1 | C2 | C3 | C4 | C5 | C6 | C7 |
|---|---|---|---|---|---|---|---|---|
| flate-9/512KiB/1000 | fail | pass | pass | FAIL | pass | FAIL | FAIL | not evaluated by harness |
| zstd-9/512KiB/500 | fail | pass | pass | FAIL | pass | FAIL | FAIL | not evaluated by harness |
| flate-6/256KiB/1000 | fail | pass | FAIL | FAIL | pass | FAIL | FAIL | not evaluated by harness |
| zstd-3/256KiB/1000 | fail | pass | pass | FAIL | pass | FAIL | FAIL | not evaluated by harness |

C7 is judged in this PRD, not by the harness: see the C7 section below.

### Size, backfill and cost per configuration

| config | chunks | compressed / uncompressed | uncompressed over compressed | chunk table / archived pre (C1, C5) | WAL over archived pre | backfill rows/s | largest tx | peak extra disk | live p95 during / idle |
|---|---|---|---|---|---|---|---|---|---|
| flate-9/512KiB/1000 | 1215 | 178.8 / 559.2 MiB | 3.13 | 0.442 | 2.27 | 4,225 | 1,097 ms | 186.6 MiB | 1.16 |
| zstd-9/512KiB/500 | 1215 | 171.0 / 559.2 MiB | 3.27 | 0.423 | 2.25 | 8,385 | 438 ms | 178.6 MiB | 1.17 |
| flate-6/256KiB/1000 | 2348 | 186.2 / 559.2 MiB | 3.00 | 0.462 | 2.29 | 4,820 | 769 ms | 195.3 MiB | 1.33 |
| zstd-3/256KiB/1000 | 2348 | 182.2 / 559.2 MiB | 3.07 | 0.452 | 2.28 | 5,761 | 627 ms | 191.0 MiB | 0.66 |

C1 and C5 measure almost the same quantity, so they share one column. C1's post-rollup footprint adds the watermark table (56-80 KiB) and reads the chunk table after plain VACUUM, 16 KiB above C5's after-rollup reading: post/pre is 0.4418, 0.4227, 0.4623 and 0.4520 against C5's 0.4416, 0.4225, 0.4621 and 0.4518 (72-96 KiB apart). The column shows C5's values.

Per criterion:

- **C1 (pass, all four).** Post-rollup footprint over pre-rollup is 0.423–0.462 against the 50% bar. Reusable free space after plain VACUUM is 0.859 of the pre-rollup footprint against the 40% bar. After the rollup alone `run_messages` is still 523,804,672 bytes; plain VACUUM makes its space reusable (heap 86.9% free, TOAST 87.6% free) without shrinking the file, and `VACUUM FULL` brings it to 66,748,416 bytes (63.7 MiB), which is also the rewrite headroom needed for a new copy (reported, no bar). On flate-9 the rewrite took 0.77 s and 62,769,520 bytes of WAL.
- **C2 (pass on flate-9, zstd-9, zstd-3; FAIL on flate-6/256KiB/1000).** Worst archived p95 for 200-row first, tail and backward pages at 8 readers: 38.3 ms (flate-9), 27.1 ms (zstd-9), 53.5 ms (flate-6), 27.6 ms (zstd-3). flate-6/256KiB/1000 fails only the first_page_200 small cell: 53.5 ms against the 50 ms bound (live 24.9 ms, ratio 2.15). That is a single after-VACUUM measurement (p95 53.5 ms, p50 16.1 ms); the same cell before VACUUM was p95 23.8 ms (p50 8.5 ms) and the other three configurations measured 11.9-20.4 ms after VACUUM. The FAIL verdict stands, but this looks like measurement spread, so it should not be read as flate-6 being inherently slower. The worst 200-row ratio to live is 2.70 (flate-9, tail_200 median), against the 3x bar. 1000-row page ratios to live are at most 1.41, 0.81, 1.03 and 1.27 respectively. Full replay of the largest run at 8 readers is faster than live: archived p95 985.5, 391.2, 1,115.6 and 441.3 ms against 1,805.1 ms live in a transaction. 32 readers are reported in the latency cells only.
- **C3 (FAIL, all four).** Judge signal scan on the large stratum passes with room: p95 359.5, 125.6, 363.1 and 113.1 ms against 500 ms (worse of the before-vacuum and after-vacuum cells). The judge trace page passes everywhere. `ListChatRunsForUser` at 50 runs passes: worst ratio to live 0.83, 1.11, 0.88 and 0.84 against 2x (live p95 1,345.9 ms autocommit, 1,466.6 ms in a transaction). The failures are the jobs read (`jobs_text_200`) and the usage scan (`usage_frames_scan`) over 3x live:
  - flate-9: jobs small 4.29x, median 4.79x, large 4.94x; usage small 3.26x, median 4.31x, large 3.73x (948.7 ms against 254.4 ms live).
  - zstd-9: jobs median 3.79x (33.6 ms against 8.9 ms) only.
  - flate-6: jobs small 3.05x, median 4.21x, large 6.07x; usage small 5.40x, median 5.31x, large 3.83x (974.9 ms against 254.4 ms live).
  - zstd-3: jobs large 3.06x (26.0 ms against 8.5 ms) and usage median 3.64x (30.6 ms against 8.4 ms).
- **C4 (pass, all four).** Round trip: 169 runs and 425,420 rows compared, 0 mismatches, 0 read errors, 2 messages larger than the chunk byte bound verified; on flate-9, reader results matched the unrolled corpus on 407 shape-run combinations with 0 mismatches. Concurrent reader and straggler-writer checks run on the first configuration of each codec only (flate-9 and zstd-9); both found 0 order or duplicate violations, 0 missing seqs, 0 unexpected seqs, 0 read errors and 0 lost or duplicated stragglers:

  | check | flate-9/512KiB/1000 | zstd-9/512KiB/500 |
  |---|---|---|
  | reader full replays | 1,741 | 2,222 |
  | runs whose rollup took more than one transaction | 31 | 31 |
  | mixed-state reads of those runs (archived and live source rows in one read) | 49 | 50 |
  | straggler rows inserted | 2,051 | 2,150 |
  | straggler inserts issued while that run's rollup transaction was open | 309 | 294 |
  | of those, blocked by the open rollup transaction | 301 | 292 |
  | straggler insert wait p95 / max | 481.3 / 1,382.2 ms | 246.6 / 750.4 ms |
  | stragglers that ended archived / live / lost or duplicated | 2,051 / 0 / 0 | 2,150 / 0 / 0 |

  The corruption and bound unit tests pass for both codecs: `task gate:api` passed (exit 0) at harness commit 5e1b30d6, including `ok github.com/vtmocanu/uzi/api/cmd/rollupbench`. They live in `api/cmd/rollupbench/chunk_test.go` (for example `TestDecompressionBombRejectedWithoutAllocatingBeyondBound`, `TestDeclaredLengthLieRejectedBothDirections` and `TestZstdFrameWithoutContentSizeBoundedByDeclaredLength`); `results.json` itself does not record their outcome.
- **C5 (FAIL, all four, on one sub-bar).** WAL passes: rollup plus gap plus vacuum is 2.25–2.29x the archived runs' pre-rollup bytes against 2.5x (rollup phase 863.2–881.1 MiB at 8.7–17.0 MB/s, vacuum phase 87.7–87.8 MiB). Peak extra disk is within 16 KiB of the final chunk table (186.6, 178.6, 195.3 and 191.0 MiB) and sits under its bound (1.1x final + 64 MiB, which is 260.4–278.8 MiB across the configurations); no disk amplification beyond the chunks. Largest transaction 0.44–1.1 s against 30 s. Live-reader ratio 0.66–1.33 against 1.5. What fails is final chunk table over archived pre-rollup footprint: 0.423, 0.442, 0.452 and 0.462 against 0.35 (the 35% bar would need a chunk table of about 147.9 MiB for this corpus; the smallest measured is 178.6 MiB).
- **C6 (FAIL, all four).** Allocation (measured at 1 reader) exceeds 4x bytes returned plus one chunk bound in 4 cells for flate-9 (`jobs_text_200` median and large, `usage_frames_scan` median and large), 6 cells for zstd-9 (those four plus `judge_trace_page_200` large and `mid_backward_200` large), and 6 cells each for flate-6 and zstd-3 (jobs and usage in all three strata). Worst cell over its bound: 16.8x (flate-9), 22.8x (zstd-9), 18.7x (flate-6), 24.6x (zstd-3), always `usage_frames_scan` on the large stratum. Peak heap over the cell's own baseline at 32 readers is 2.16, 2.69, 1.97 and 2.98 GiB against bounds of 181.6, 181.6, 173.6 and 173.6 MiB (190,457,792 bytes for the 512 KiB configurations, 182,069,184 for the 256 KiB ones).

### Analysis

These are computations from `results.json`; they describe the synthetic corpus and are inputs to the owner's decision, not decisions.

1. **Compression on this corpus is about 3.0–3.3x.** Uncompressed over compressed is 3.00–3.27 (559 MiB uncompressed to 171–186 MiB of chunk data), below the owner's real-data comparison (next section). The C5 35% chunk-share bar fails because of that ratio, while C1's 50% bar passes (0.423–0.462). The bar does not convert to 1/0.35 = 2.9x: TOAST already compresses large payloads, so the 559.2 MiB uncompressed (586,350,949 bytes) exceeds the 422.6 MiB pre-rollup footprint (443,170,816 bytes). Meeting 0.35 means a chunk table of 147.9 MiB, which is 3.78x against the uncompressed bytes. Measured the same way (uncompressed bytes over final chunk-table size) the four configurations reach 3.13x (zstd-9), 3.00x (flate-9), 2.93x (zstd-3) and 2.86x (flate-6).
2. **The kinds metadata filter spared no fetched chunk.** In every cell `chunks_fetched_per_request` equals `chunks_matched_per_request` (a matched chunk emitted at least one row after filtering), including the filtered shapes: every chunk fetched for a status or tool-kind filter held a matching row, which fits a kind mix where status frames are 4.5% of rows spread through every run. The usage scan on the large stratum therefore decodes the whole run: 70 chunks and 75,085,071 bytes allocated to return 692,088 bytes (zstd-9, 1 reader; 55,211,856 bytes for flate-9; 140 chunks for the 256 KiB configurations). `results.json` does not record how many chunks the run holds, so "pruning excluded none before the fetch" is an inference. This whole-run decode applies to `usage_frames_scan` only, and drives its C6 allocation failures and C3 ratio failures. In the failing cells other than the large usage scan, archived p95 is 13.9–51.6 ms against live 4.6–8.9 ms (13 cells); the large usage-scan cell is the exception, at 948.7 ms (flate-9) and 974.9 ms (flate-6) against 254.4 ms live, while the zstd configurations pass it (381.4 ms for zstd-9, 319.2 ms for zstd-3).

   `jobs_text_200` has a different cause. It fetches only 1.05–2 chunks per request on the 512 KiB configurations and 1.2–3 on the 256 KiB ones (flate-9 large: 2 chunks per request, 5,185 decoded bytes per returned row, 112,464.8 bytes returned per request), so it is not decoding the whole run. The cost is chunk granularity against a narrow text-only projection: whole 256/512 KiB chunks, including large `tool_result` payloads, are decoded to return up to 200 text rows (about 53 on the small stratum, 195 on the median, 200 on the large), while live SQL projects only the text.
3. **The C6 heap bar is not comparable with the live path.** The live baseline itself peaks 2,049.5 MiB over its baseline at 32 readers on the full replay of the largest run (39,519,176 bytes returned per request), and 343.5 MiB on its worst non-replay shape (`judge_signal_scan_4000`, large). Archived full-replay peaks are 1.82–2.98 GiB (flate-9 2,215.5 MiB, zstd-9 2,756.2 MiB, flate-6 1,865.0 MiB, zstd-3 3,049.2 MiB). The bar (32 x (chunk bound + largest message) + 64 MiB, 173.6–181.6 MiB) does not account for bytes returned, so even the live path exceeds it. The meaningful regression against live is on non-replay shapes: archived `usage_frames_scan` large peaks 1.80–2.29 GiB (1,843.3–2,340.3 MiB) against 159.2 MiB live, and archived `judge_signal_scan_4000` large 544.5–672.6 MiB against 343.5 MiB live. The returned-bytes effect shows on the page shapes too: at 32 readers on the large stratum, archived `tail_1000` peaks (before-VACUUM cells; no 32-reader cell was measured after VACUUM) at 188.3, 203.5, 192.3 and 180.1 MiB (flate-9, zstd-9, flate-6, zstd-3) and archived `first_page_1000` at 185.2, 179.2, 182.1 and 225.7 MiB, so all eight cells except zstd-9 `first_page_1000` exceed their bar (181.6 MiB for the 512 KiB configurations, 173.6 MiB for the 256 KiB ones); live `tail_1000` large peaks 193.6 MiB (live `first_page_1000` 191.0 MiB) in the transaction baseline, above both bars. This supports reading the C6 heap bar as not accounting for returned bytes.
4. **What passes.** Lossless round trip and the concurrency checks (C4) on both codecs; page latency (C2) on three of four configurations, with archived full replay and many 1000-row pages faster than live; judge signal scan well under 500 ms; WAL 2.25–2.29x; no disk amplification beyond the chunks; largest transaction 0.44–1.1 s; live readers during the throttled backfill at most 1.33x idle.
5. **M1 design inputs, if the owner decides to continue.** Filtered full-run scans (the usage scan) need per-kind segregation (for example separate chunk streams, or a small side index for status and usage frames) or the `kinds` metadata does not reduce decoding; narrow projections (the jobs read) need a projection-friendly layout or side columns, since chunk granularity makes them decode whole chunks for a few bytes per row; the usage-fold counters need per-chunk metadata; `ListChatRunsForUser` needs `last_created_at` metadata (the prototype stored it per chunk); the C6 heap bar should be restated relative to the live path.

### Caveats

- Synthetic corpus: compressibility and kind mix may differ from production, and the owner's real-data ratios are higher.
- Warm cache only; no cold-cache runs.
- The live autocommit `full_replay` of the largest run at 32 readers had 12 errors over 52 iterations: `could not resize shared memory segment ... No space left on device (SQLSTATE 53100)`, which points to container `/dev/shm` exhaustion. Verdicts use the transaction-wrapped live baseline, which had 0 errors; the 32-reader autocommit live figures are degraded.
- `settings.json` lists container-internal Postgres paths only (for example `/var/lib/postgresql/data`).
- Stock `postgres:17` settings, not the hosted CNPG configuration: `wal_log_hints`, `data_checksums` and `wal_compression` may differ (here all three are off), so C5 WAL may read low against CNPG.
- WAL is the `pg_current_wal_insert_lsn` delta; `pg_stat_wal` is an approximate cross-check (backends report counters lazily, so it is a lower bound). `pg_waldump` was not run.
- 8 CPUs visible, harness `GOMAXPROCS` 4, Postgres on the same host; percentiles are nearest-rank and p99 over about 1000 samples is indicative only.
- The C4 concurrent reader and straggler checks ran on one configuration per codec (flate-9 and zstd-9).
- Not timed: `ListPlanRevisionStateForRuns` (terminal-reachable, but its result is display-only for `awaiting_approval`, so M1 maps it to a caller filter with no archive read) and `LatestToolUseForRuns` (never passed terminal ids). The jobs read was timed on the `run_messages` part only (the runs and job-origins joins were omitted on both sides of the comparison).
- The C5 live-reader ratio is a single measurement per configuration (4 readers over 28 live runs); the 0.66 for zstd-3 comes from a higher idle p95 (19.8 ms) than the other configurations (9.1–10.9 ms).

## Comparison point (real data, different implementation)

The #80 comment's measurement: a 189-run sample went from 425 MB to 157 MB (63% reduction) in PostgreSQL 17; per-message compression saved 11.5%. The four ratios below are not in that comment; they come from the owner's plan review on #2755 (2026-10-11), measured on the #80 sample re-chunked at 256 KiB / 1000 messages with Python zlib and zstd bindings: flate-1 3.14x, flate-6 3.71x, zstd-3 4.20x and zstd-9 4.54x.

These figures come from a different implementation and a real corpus. They are comparison evidence, not expected Go results: on the synthetic corpus the same shapes measured 3.00x (flate-6/256KiB/1000) and 3.07x (zstd-3/256KiB/1000), and zstd-9 at 512 KiB / 500 measured 3.27x. Source for the 63%, 425 to 157 MB and 11.5% figures: https://github.com/vtmocanu/uzi/issues/80#issuecomment-6110889256

## C7 reader inventory

**C7: pass.** Every `run_messages` reader is classified below with caller evidence and a chunk mapping, or a recorded proof that terminal runs never reach it. Two rows are marked POSSIBLE (`CountRunMessagesThrough`, `RunMessageGaps`): a retried terminal report on an already-terminal run is not proven never to reach them, so M2 must treat archived seqs as present for both.

Reader inventory at commit 5e1b30d6 (base 2c6b3393; no production code changed). Source: `for f in api/internal/store/queries/*.sql; do awk -v F=$f '/^-- name:/{n=$0} /run_messages/{print F": "n}' $f; done` plus every Go caller read. Line numbers are the `-- name:` line. Excluded: slack.sql queries on `slack_run_messages` (a different table: UpsertSlackRunMessage, GetSlackRunMessage, GetSlackRunMessageByRoot, SetSlackRunGate, SetSlackRunGateGen, SetSlackRunMilestoneNotified, SetSlackRunGateIf, SetSlackRunLimitPause, ClearSlackRunLimitPause, SetSlackRunQuestion, InsertSlackChatAnchor; GetSlackRunContext mentions run_messages only in a comment); InsertRunMessage (writer); UpdateRunLastSeq (comment only); UpsertRunUsage (comment-only mention, `runtime.sql`). No production Go has inline SQL on run_messages; no view or trigger reads it.

| Query | File:line | Scope | Terminal-reachable (evidence) | Chunk mapping |
|---|---|---|---|---|
| ListRunMessagesAfter | runtime.sql:7488 | single | YES: ListRunMessagesForViewer, owner/admin check only, no status guard | seam: forward read, chunks then live rows |
| ListRunMessagesAfterPage | runtime.sql:7504 | single | YES: ListRunMessagesForViewerPage (handler/runs_lifecycle.go) | seam: forward page |
| ListRunMessagesBeforePage | runtime.sql:7517 | single | YES: ListRunMessagesForViewerBefore (tail / before) | seam: backward page |
| ListRunMessagesForWorkerPage | runtime.sql:8151 | single | YES: worker read tool (workersvc/chat.go) and JudgeTrace (judge targets are completed/failed) | seam: forward page |
| ListToolTraceForRun | judge.sql:93 | single | YES: judgeSignal at judge claim on a terminal target | seam: filtered scan, kinds tool_use/tool_result |
| ListRunUsageFrames | runtime.sql:8055 | single | YES: RefoldRunUsage over terminal pre-migration runs (cmd/server/main.go) | seam: filtered scan, kinds status/error/progress_note/summary_usage |
| CountRunInitFramesBefore | runtime.sql:7625 | single | YES: foldUsageFrames via appendMessages; "No terminal-status guard" (usage_fold.go) | needs per-chunk init counts or a side index; fold must answer the same before and after rollup |
| CountRunLineageRestartsBefore | runtime.sql:7639 | single | YES, same path | same, plus first-init seq |
| ListJobMessagesForCaller | jobs.sql:205 | single | YES: ListJobMessages, no status guard (handler/v1_jobs.go) | seam: filtered forward page kinds text/status/error, text projection; keep owner/product scope |
| ListChatRunsForUser | chat.sql:43 | multi (up to 200 chats) | YES: kind='chat', no status filter | metadata: last_created_at per run or chunk, no decode |
| SweepIdleChatRuns | chat.sql:152 | multi | NO: SQL `r.status IN ('claimed','running')` | none (live only) |
| ListPlanRevisionStateForRuns | runtime.sql:683 | multi (page of runs) | YES: PlanRevisingForRuns called with unfiltered ids from ListRuns (handler/runs.go) and the board (handler/board.go) | caller passes non-terminal ids only, or latest-plan-kind metadata; the result is display-only for awaiting_approval, so terminal runs need no archive read |
| LatestToolUseForRuns | runtime.sql:721 | multi | NO: all callers pass activeIDs built from !IsTerminalRunStatus (handler/runs.go, runs_lifecycle.go) | live only; Go-side guard |
| LiveLaneFramesForRun | runtime.sql:735 | single | NO: MilestonesLiveForRun guarded by !IsTerminalRunStatus | live only |
| LeadAgentDispatchFramesForRun | runtime.sql:746 | single | NO: same caller and guard | live only |
| LeadDispatchCompletionIDsForRun | runtime.sql:758 | single | NO: same caller and guard | live only |
| LatestPlanSeqForRun | runtime.sql:694 | single | NO: claim assembly, awaiting_approval resume only | watermark-aware max if ever archived |
| LatestPersistedPlanFrameAtForRun | runtime.sql:703 | single | NO: claim assembly | live only |
| GetLatestProgressNote | runtime.sql:11021 | single | NO: overlayNowNote requires progress state percent (non-terminal) | live only |
| ListRunToolWindow | runtime.sql:9286 | single | NO: health pass on running runs | live only |
| ListRunLeadToolWindow | runtime.sql:9299 | single | NO: same | live only |
| CountCredentialSwitchMessages | runtime.sql:7400 | single | NO: claim path | live only |
| MaxRunMessageSeq | runtime.sql:7411 | single | NO: claim path; but ignores last_seq | must become GREATEST(last_seq, watermark) if a rolled run can ever be claimed |
| NextPlanCrossCheckMessageSeq | runtime.sql:10757 | single | NO: SQL status IN ('claimed','running') | safe while last_seq stays authoritative |
| InsertCodexReadmitRunMessage | codex_binding.sql:246 | single (INSERT with MAX(seq) subselect) | NO: status='recovery_wait' | safe while last_seq stays authoritative |
| CountRunMessagesThrough | runtime.sql:7420 | single | POSSIBLE: terminal fence in setState; a retried terminal report on an already-terminal run is not proven never to reach it | watermark: archived seqs count as present |
| RunMessageGaps | runtime.sql:7430 | single | POSSIBLE: fence is claim_released_at IS NULL + generation, and terminal transitions do not release the claim | watermark: no gaps at or below it |
| CountRunPlanMessages | slack.sql:335 | single | NO: handleGate only when status awaiting_approval | live only |
| GetLatestRunQuestion | slack.sql:344 | single | NO: both callers require awaiting_input | live only |
| GetLatestCredentialSwitchSince | slack.sql:360 | single | NO: handleLimitResume inside case "running" | live only |

`ListPlanRevisionStateForRuns`' mapping ("caller passes non-terminal ids") would change the raw `is_revising` JSON a terminal run gets (true to false). The in-repo consumers read for this document all gate on `awaiting_approval` (`api/cmd/uzi/render.go` `effectiveRunStatus`, `api/cmd/uzi/tui_board.go` `runBand`, `web/src/lib/runBadge.ts` `isRevisingRun`), so no display changes, but it is an API-visible change that M1 must state.

M1/M2 must change: the four paging readers and JudgeTrace (seam); ListToolTraceForRun, ListRunUsageFrames, ListJobMessagesForCaller (filtered seam); CountRunInitFramesBefore and CountRunLineageRestartsBefore (usage fold must be invariant under rollup); ListChatRunsForUser (metadata); ListPlanRevisionStateForRuns (caller filter or metadata); CountRunMessagesThrough and RunMessageGaps (watermark as present); MaxRunMessageSeq (GREATEST with last_seq/watermark); and InsertRunMessage's ON CONFLICT dedup is lost for archived seqs (requirement 3, M2). The live-only rows are safe because of Go-side status guards; M1 should keep or assert them.

## Rerunning M0

Synthetic: `scripts/measure-run-message-rollups.sh --scale full OUT_DIR`. `OUT_DIR` is required and must be outside the repo or git-ignored. Needs docker and Go. The measured run took 3202 s (about 53 minutes) with 8 CPUs visible and harness `GOMAXPROCS` 4; the script header budgets about 90 minutes on 4 CPUs. Environment: `UZI_M0_BACKFILL_SLEEP` (default 50ms) and `UZI_ROLLUP_M0_PG_IMAGE` (default `postgres:17`).

Real data via a local export: for each terminal run, `uzi run logs <run> --json > DIR/<name>.ndjson` (one MessageDTO per line: seq, kind, agent, agent_instance, agent_label, payload, created_at; no id or claim_generation, so the loader assigns id by created_at order and leaves claim_generation NULL), then `scripts/measure-run-message-rollups.sh --scale full --corpus-ndjson DIR OUT_DIR`. With `--corpus-ndjson` every run is loaded as terminal, so there are no live runs and C5's live-reader sub-bar cannot be evaluated; C5 is reported as not met when its live-reader sub-bar cannot be evaluated. (That rule, and failing C4 when no mixed archived-and-live read of a multi-transaction run is observed, landed in the harness after the full run; neither changes a verdict above, because the synthetic corpus has live runs and both concurrency-checked configs observed mixed-state reads.)

WARNING: the export holds real payloads; keep it and `OUT_DIR` out of the repo. Results never record run ids, DSNs or payload content. A restored snapshot can be read with `api/cmd/rollupbench` `--source-dsn` (read-only), against a throwaway target server:

```
cd api && go run ./cmd/rollupbench --dsn <throwaway target DSN> --i-own-this-db --source-dsn <restored snapshot DSN> --corpus snapshot --scale full --backfill-sleep 50ms --out <dir>
```

The snapshot corpus copies terminal runs only, so the same C5 live-reader limitation applies. The wrapper script defaults (`--scale` tiny unless given, 50ms throttle via `UZI_M0_BACKFILL_SLEEP`) differ from the binary's own defaults (`--scale tiny`, `--backfill-sleep 0s`), so pass `--scale full` and `--backfill-sleep 50ms` explicitly when calling the binary.

## Decision Log

Dated 2026-10-11.

| Decision | Reason | Rejected alternative |
|---|---|---|
| D1. This run delivers M0 only; M1–M4 run as a separate gated run after an owner go decision on this evidence. | The owner's answer during planning. Posting the results on #2755 is a maintainer step; the hosted-k8s enable/disable runbook is delivered in M3; those acceptance boxes stay open. | Running M0 through M4 in one run before the owner has seen the evidence. |
| D2. Both stdlib flate and klauspost zstd were measured; the codec choice is left to the owner's go decision and M1, and flate is not adopted. | Per materialised config (ratio / backfill rows/s / judge scan p95 / full replay p95): flate-9/512KiB/1000 3.13x / 4,225 / 359.5 ms / 985.5 ms; zstd-9/512KiB/500 3.27x / 8,385 / 125.6 ms / 391.2 ms; flate-6/256KiB/1000 3.00x / 4,820 / 363.1 ms / 1,115.6 ms; zstd-3/256KiB/1000 3.07x / 5,761 / 113.1 ms / 441.3 ms. `github.com/klauspost/compress` v1.20.1 is pinned in `api/go.mod` for measurement only: it is imported only by `api/cmd/rollupbench`, which is not built into any image. M1 decides whether it stays. | Adopting a codec before the owner's go/no-go. |
| D3. No chunk parameters are adopted. | No configuration met all criteria. Among the materialised configurations C1 passed everywhere and C2 passed on all but flate-6/256KiB/1000 (first_page_200 small, 53.5 ms against the 50 ms bound; a single after-VACUUM measurement with p50 16.1 ms, against 23.8 ms p95 for the same cell before VACUUM and 11.9-20.4 ms for the other configurations after VACUUM, so it looks like measurement spread rather than flate-6 being inherently slower); zstd-9/512KiB/500 had the smallest chunk table (0.423 of the archived pre-rollup footprint) and the fastest backfill and full replay. | Picking zstd-9/512KiB/500 as the winner despite failing C3, C5 and C6. |
| D4. Criteria review is an owner decision. | C5's 35% chunk-share bar is tighter than C1's 50% bar, and C6's heap bar is tighter than the live path itself (see Analysis 3). Three facts for the review: (a) C1 post/pre and C5 chunk/pre measure almost the same quantity (they differ by 72-96 KiB, the watermark table plus 16 KiB of chunk-table growth across the plain VACUUM, for example flate-9 0.4418 against 0.4416), so C5's 35% bar subsumes C1's 50% bar; (b) the #80 real-data result (157/425 = 0.369 remaining) would itself fail a 35% bar; (c) because pglz/TOAST already compresses large payloads, 559.2 MiB uncompressed exceeds the 422.6 MiB pre-rollup footprint, so meeting 0.35 needs about 3.78x chunk compression against the uncompressed bytes, not 2.9x (Analysis 1). The criteria were fixed before measuring and are not changed after it; restating them is for the owner. | Relaxing the bars in this document so a configuration passes. |
