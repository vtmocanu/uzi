package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Attrib attributes the pre-rollup on-disk footprint to runs. Per-run bytes are the sum over
// that run's rows of the tuple header (24) and line pointer (4) plus pg_column_size of every
// column (payload counted at its stored, possibly compressed, size), plus the run's share of
// all index bytes proportional to its row count. It ignores alignment padding and TOAST chunk
// headers, so RowBytesVsRelation reports how far the model is from the real heap + TOAST size.
type Attrib struct {
	Model              string  `json:"model"`
	ArchivedPre        int64   `json:"archived_runs_pre_rollup_footprint_bytes"`
	LivePre            int64   `json:"never_rolled_runs_pre_rollup_footprint_bytes"`
	RowBytes           int64   `json:"row_bytes_total"`
	HeapPlusToast      int64   `json:"heap_plus_toast_bytes"`
	IndexBytes         int64   `json:"index_bytes"`
	RowBytesVsRelation float64 `json:"row_bytes_over_heap_plus_toast"`
	ArchivedRows       int64   `json:"archived_rows"`
	TotalRows          int64   `json:"total_rows"`
	FreeSpaceBefore    int64   `json:"free_space_heap_plus_toast_before"`
}

func computeAttrib(ctx context.Context, pool *pgxpool.Pool, pre RelReport) (Attrib, error) {
	a := Attrib{Model: "per run: sum(24 + 4 + pg_column_size of each column) over its rows + index_bytes * rows_run / rows_total"}
	var archBytes, liveBytes int64
	err := pool.QueryRow(ctx, `SELECT
		coalesce(sum(b) FILTER (WHERE t), 0)::bigint, coalesce(sum(b) FILTER (WHERE NOT t), 0)::bigint,
		coalesce(sum(n) FILTER (WHERE t), 0)::bigint, coalesce(sum(n), 0)::bigint FROM (
		SELECT m.run_id, r.status IN ('completed','failed','cancelled') AS t, count(*) AS n,
		  sum(28 + pg_column_size(m.id) + pg_column_size(m.run_id) + pg_column_size(m.seq) + pg_column_size(m.kind)
		      + coalesce(pg_column_size(m.agent), 0) + pg_column_size(m.payload) + pg_column_size(m.created_at)
		      + coalesce(pg_column_size(m.agent_instance), 0) + coalesce(pg_column_size(m.agent_label), 0)
		      + coalesce(pg_column_size(m.claim_generation), 0)) AS b
		FROM run_messages m JOIN runs r ON r.id = m.run_id GROUP BY m.run_id, r.status) s`).Scan(&archBytes, &liveBytes, &a.ArchivedRows, &a.TotalRows)
	if err != nil {
		return a, err
	}
	a.IndexBytes = pre.IndexSize
	a.RowBytes = archBytes + liveBytes
	a.HeapPlusToast = pre.Heap.TableLen + pre.ToastSize
	if a.TotalRows > 0 {
		a.ArchivedPre = archBytes + a.IndexBytes*a.ArchivedRows/a.TotalRows
		a.LivePre = liveBytes + a.IndexBytes*(a.TotalRows-a.ArchivedRows)/a.TotalRows
	}
	if a.HeapPlusToast > 0 {
		a.RowBytesVsRelation = float64(a.RowBytes) / float64(a.HeapPlusToast)
	}
	a.FreeSpaceBefore = pre.Heap.FreeSpace
	if pre.Toast != nil {
		a.FreeSpaceBefore += pre.Toast.FreeSpace
	}
	return a, nil
}

// RoundTripReport is the every-field check of every rolled-up run after the backfill.
type RoundTripReport struct {
	Runs              int `json:"runs_checked"`
	Rows              int `json:"rows_compared"`
	Mismatches        int `json:"runs_with_a_mismatch"`
	Errors            int `json:"read_errors"`
	OversizedVerified int `json:"messages_larger_than_the_chunk_byte_bound_verified"`
}

// RewriteReport is VACUUM FULL of run_messages after the rollup.
type RewriteReport struct {
	TotalBefore int64   `json:"run_messages_total_before_bytes"`
	TotalAfter  int64   `json:"run_messages_total_after_bytes"`
	Headroom    int64   `json:"headroom_needed_for_new_copy_bytes"`
	WAL         WALStat `json:"wal"`
	DurationSec float64 `json:"duration_s"`
}

// LatencyResults groups a config's archived matrices.
type LatencyResults struct {
	PreVacuum  []Cell     `json:"archived_before_vacuum"`
	PostVacuum []Cell     `json:"archived_after_vacuum"`
	Chat       []ChatCell `json:"chat_list"`
}

// DiskRun is run (ii): rollup with a plain VACUUM every K runs.
type DiskRun struct {
	VacuumEvery int            `json:"vacuum_every_runs"`
	Backfill    BackfillReport `json:"backfill"`
}

// ConfigResult is everything measured for one materialised configuration.
type ConfigResult struct {
	Name         string             `json:"name"`
	Codec        string             `json:"codec"`
	ByteBoundKiB int                `json:"byte_bound_kib"`
	CountCeil    int                `json:"count_ceiling"`
	Why          []string           `json:"selected_because"`
	Backfill     BackfillReport     `json:"backfill_worst_case_no_vacuum"`
	VacuumPhase  WALStat            `json:"wal_explicit_vacuum_phase"`
	VacuumSecs   float64            `json:"vacuum_phase_s"`
	WALTotal     int64              `json:"wal_total_lsn_bytes_rollup_plus_vacuum"`
	VacuumRun    *DiskRun           `json:"backfill_with_periodic_vacuum,omitempty"`
	Sizes        map[string]any     `json:"sizes"`
	Latency      LatencyResults     `json:"latency"`
	Digests      DigestCheck        `json:"reader_results_vs_unrolled_corpus"`
	RoundTrip    RoundTripReport    `json:"roundtrip"`
	Concurrency  *ConcurrencyReport `json:"concurrency,omitempty"`
	Rewrite      RewriteReport      `json:"rewrite_headroom"`
	Criteria     []Criterion        `json:"criteria"`
	Overall      string             `json:"overall"`
	archivedPost int64
}

func (e *Env) runConfig(ctx context.Context, idx int, sel Selection, withConcurrency bool, attrib *Attrib) (*ConfigResult, error) {
	cfg := sel.Config
	res := &ConfigResult{Name: cfg.Name(), Codec: cfg.Spec.Name(), ByteBoundKiB: cfg.Bounds.ByteBound >> 10, CountCeil: cfg.Bounds.CountCeiling, Why: sel.Why, Sizes: map[string]any{}}
	dbName := fmt.Sprintf("m0_cfg%d", idx)
	e.Lg.f("config %s: cloning the corpus", res.Name)
	if err := e.createDB(ctx, dbName, "m0_corpus"); err != nil {
		return nil, err
	}
	defer func() { _ = e.dropDB(context.WithoutCancel(ctx), dbName) }()
	pool, _, err := e.openDB(ctx, dbName, e.Opt.maxReaders()+8, nil)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if err := createChunkTables(ctx, pool); err != nil {
		return nil, err
	}
	arch := &ArchReader{Pool: pool, Eng: e.Eng}

	bo := backfillOpts{cfg: cfg, sleep: e.Opt.BackfillSleep, withLoad: true}
	var cc *concurrencyCheck
	if withConcurrency {
		cc = e.newConcurrencyCheck(pool, arch)
		bo.hooks = cc.hooks()
		bo.pause = 150 * time.Millisecond
	}
	e.Lg.f("config %s: backfill (worst case, no vacuum)", res.Name)
	if res.Backfill, err = e.backfill(ctx, pool, bo); err != nil {
		return res, err
	}
	if cc != nil {
		rep, err := cc.finish(ctx, cfg)
		if err != nil {
			return res, err
		}
		res.Concurrency = &rep
	}
	if _, err := pool.Exec(ctx, `ANALYZE run_messages`); err != nil {
		return res, err
	}
	if _, err := pool.Exec(ctx, `ANALYZE `+chunkTable); err != nil {
		return res, err
	}
	if res.Sizes["after_rollup_run_messages"], err = measureRel(ctx, pool, "run_messages", "after rollup, before vacuum"); err != nil {
		return res, err
	}
	if res.Sizes["after_rollup_chunks"], err = measureRel(ctx, pool, chunkTable, "chunk table after rollup"); err != nil {
		return res, err
	}

	e.Lg.f("config %s: verifying readers and round trip", res.Name)
	res.Digests = e.checkDigests(ctx, arch, pool, withConcurrency)
	res.RoundTrip = e.roundTripAll(ctx, arch, cfg)

	mo := MatrixOpts{Backend: "archived/" + res.Name, Readers: e.Opt.Readers, Iterations: e.Opt.Iterations, PeakAt: e.Opt.maxReaders(), Seed: e.Opt.Seed + uint64(idx)*101} //nolint:gosec // G115: small config index.
	e.Lg.f("config %s: latency before vacuum", res.Name)
	if res.Latency.PreVacuum, err = e.measureMatrix(ctx, arch, mo); err != nil {
		return res, err
	}
	res.Latency.Chat = append(res.Latency.Chat, e.measureChat(ctx, pool, "archived/"+res.Name+"/before_vacuum", true, true, critReaders(e.Opt.Readers), e.Opt.Iterations))

	e.Lg.f("config %s: explicit VACUUM (ANALYZE)", res.Name)
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		return res, err
	}
	wm, err := markWAL(ctx, pool)
	if err != nil {
		return res, err
	}
	t0 := time.Now()
	for _, t := range []string{"run_messages", chunkTable} {
		if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) `+t); err != nil {
			return res, err
		}
	}
	res.VacuumSecs = time.Since(t0).Seconds()
	if res.VacuumPhase, err = endWAL(ctx, pool, wm); err != nil {
		return res, err
	}
	res.WALTotal = res.Backfill.WAL.LSNBytes + res.VacuumPhase.LSNBytes
	if res.Sizes["after_vacuum_run_messages"], err = measureRel(ctx, pool, "run_messages", "after plain VACUUM"); err != nil {
		return res, err
	}
	if res.Sizes["after_vacuum_chunks"], err = measureRel(ctx, pool, chunkTable, "chunk table after VACUUM"); err != nil {
		return res, err
	}

	e.Lg.f("config %s: latency after vacuum", res.Name)
	post := mo
	post.Backend = "archived_after_vacuum/" + res.Name
	post.Readers = []int{critReaders(e.Opt.Readers)}
	post.PeakAt = 0
	if res.Latency.PostVacuum, err = e.measureMatrix(ctx, arch, post); err != nil {
		return res, err
	}
	res.Latency.Chat = append(res.Latency.Chat, e.measureChat(ctx, pool, "archived/"+res.Name+"/after_vacuum", true, true, critReaders(e.Opt.Readers), e.Opt.Iterations))

	e.Lg.f("config %s: VACUUM FULL (rewrite headroom)", res.Name)
	if res.Rewrite.TotalBefore, err = relTotal(ctx, pool, "run_messages"); err != nil {
		return res, err
	}
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		return res, err
	}
	wm, err = markWAL(ctx, pool)
	if err != nil {
		return res, err
	}
	t0 = time.Now()
	if _, err := pool.Exec(ctx, `VACUUM FULL run_messages`); err != nil {
		return res, err
	}
	res.Rewrite.DurationSec = time.Since(t0).Seconds()
	if res.Rewrite.WAL, err = endWAL(ctx, pool, wm); err != nil {
		return res, err
	}
	if res.Rewrite.TotalAfter, err = relTotal(ctx, pool, "run_messages"); err != nil {
		return res, err
	}
	res.Rewrite.Headroom = res.Rewrite.TotalAfter
	if res.Sizes["after_vacuum_full_run_messages"], err = measureRel(ctx, pool, "run_messages", "after VACUUM FULL"); err != nil {
		return res, err
	}
	if res.Sizes["after_vacuum_full_chunks"], err = measureRel(ctx, pool, chunkTable, "chunk table at the end"); err != nil {
		return res, err
	}
	chunksTotal, err := relTotal(ctx, pool, chunkTable)
	if err != nil {
		return res, err
	}
	res.archivedPost = max(0, res.Rewrite.TotalAfter+chunksTotal-attrib.LivePre)
	pool.Close()
	return res, nil
}

func relTotal(ctx context.Context, pool *pgxpool.Pool, rel string) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `SELECT pg_total_relation_size($1::regclass)`, rel).Scan(&n)
	return n, err
}

// roundTripAll replays every terminal run from its chunks and compares every field with the
// digest taken before the rollup. Rows above the run's original last seq (stragglers) are ignored.
func (e *Env) roundTripAll(ctx context.Context, arch *ArchReader, cfg RollupConfig) RoundTripReport {
	var rep RoundTripReport
	for _, in := range e.terminalInfos() {
		out, _, err := arch.Read(ctx, &Shape{}, in.ID, ReadSpec{})
		if err != nil {
			rep.Errors++
			continue
		}
		out = trimStragglers(out, in.MaxSeq)
		rep.Runs++
		rep.Rows += len(out)
		if digestRecords(out) != e.RunDigest[in.ID] {
			rep.Mismatches++
		}
		for i := range out {
			if encodedSize(&out[i]) > cfg.Bounds.ByteBound {
				rep.OversizedVerified++
			}
		}
	}
	return rep
}

// runVacuumEvery is run (ii): the same rollup with a plain VACUUM after every K runs.
func (e *Env) runVacuumEvery(ctx context.Context, idx int, cfg RollupConfig) (*DiskRun, error) {
	dbName := fmt.Sprintf("m0_cfg%dv", idx)
	if err := e.createDB(ctx, dbName, "m0_corpus"); err != nil {
		return nil, err
	}
	defer func() { _ = e.dropDB(context.WithoutCancel(ctx), dbName) }()
	pool, _, err := e.openDB(ctx, dbName, 8, nil)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if err := createChunkTables(ctx, pool); err != nil {
		return nil, err
	}
	rep, err := e.backfill(ctx, pool, backfillOpts{cfg: cfg, sleep: e.Opt.BackfillSleep, vacuumEvery: e.Opt.VacuumEvery})
	return &DiskRun{VacuumEvery: e.Opt.VacuumEvery, Backfill: rep}, err
}
