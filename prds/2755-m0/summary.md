# Run message rollup M0 results

Corpus: synthetic, scale full, 8 CPUs, Postgres 17.11 (Debian 17.11-1.pgdg13+2). Completed all phases: true.

## Corpus fingerprint

488407 rows over 200 runs (169 terminal, 31 live). Heap 271 MiB, TOAST 171 MiB (39% of heap plus TOAST), indexes 54 MiB. Largest payload 3330366 bytes.

## Selected configs

- flate-9/512KiB/1000 ([best flate by ratio])
- zstd-9/512KiB/500 ([best zstd by ratio])
- flate-6/256KiB/1000 ([reference flate-6 256KiB/1000])
- zstd-3/256KiB/1000 ([reference zstd-3 256KiB/1000])

## Per-message lz4 reference

compression counts map[inline_uncompressed:416875 lz4:71532]; run_messages total is 1.06x the pglz baseline; TOAST share 43% (pglz 39%).

## Verdicts per config

| config | overall | C1 | C2 | C3 | C4 | C5 | C6 | C7 |
|---|---|---|---|---|---|---|---|---|
| flate-9/512KiB/1000 | fail | pass | pass | FAIL | pass | FAIL | FAIL | n/e |
| zstd-9/512KiB/500 | fail | pass | pass | FAIL | pass | FAIL | FAIL | n/e |
| flate-6/256KiB/1000 | fail | pass | FAIL | FAIL | pass | FAIL | FAIL | n/e |
| zstd-3/256KiB/1000 | fail | pass | pass | FAIL | pass | FAIL | FAIL | n/e |

C7 (reader inventory): not evaluated by harness.

### flate-9/512KiB/1000

- chunks: 1215, compressed 178 MiB from 559 MiB uncompressed; backfill 101s, 4225 rows/s; largest tx 1097 ms; rewrite headroom 63 MiB.
- WAL: rollup 872 MiB plus vacuum 87 MiB; peak extra disk 186 MiB; final chunk table 186 MiB.
- C2: 32 readers are reported in the latency cells only
- C7: C7: not evaluated by harness

### zstd-9/512KiB/500

- chunks: 1215, compressed 171 MiB from 559 MiB uncompressed; backfill 51s, 8385 rows/s; largest tx 438 ms; rewrite headroom 63 MiB.
- WAL: rollup 863 MiB plus vacuum 87 MiB; peak extra disk 178 MiB; final chunk table 178 MiB.
- C2: 32 readers are reported in the latency cells only
- C7: C7: not evaluated by harness

### flate-6/256KiB/1000

- chunks: 2348, compressed 186 MiB from 559 MiB uncompressed; backfill 88s, 4820 rows/s; largest tx 769 ms; rewrite headroom 63 MiB.
- WAL: rollup 881 MiB plus vacuum 87 MiB; peak extra disk 195 MiB; final chunk table 195 MiB.
- C2: 32 readers are reported in the latency cells only
- C4: concurrent reader and straggler checks run on the first config of each codec only
- C7: C7: not evaluated by harness

### zstd-3/256KiB/1000

- chunks: 2348, compressed 182 MiB from 559 MiB uncompressed; backfill 74s, 5761 rows/s; largest tx 627 ms; rewrite headroom 63 MiB.
- WAL: rollup 876 MiB plus vacuum 87 MiB; peak extra disk 190 MiB; final chunk table 190 MiB.
- C2: 32 readers are reported in the latency cells only
- C4: concurrent reader and straggler checks run on the first config of each codec only
- C7: C7: not evaluated by harness

Percentiles are nearest-rank; p99 is indicative. Full numbers, settings and per-cell latency are in results.json and settings.json.
- C7 (reader inventory) is written by the lead in the PRD: C7: not evaluated by harness
- pg_waldump --stats per phase is not run by the harness; the pg_current_wal_insert_lsn delta is the WAL evidence; pg_stat_wal figures (wal_bytes, wal_fpi, wal_records) are a cross-check, not exact: backends report their counters lazily, so they are a lower bound
- percentiles are nearest-rank; p99 over ~1000 samples is indicative only; heavy shapes ran fewer iterations (recorded per cell)
