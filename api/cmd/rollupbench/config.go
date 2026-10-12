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

// modelRowBytes is the row-byte model of Attrib over the rows that are in run_messages now:
// bytes and row counts of terminal runs' rows and of the other runs' rows.
func modelRowBytes(ctx context.Context, pool *pgxpool.Pool) (archBytes, liveBytes, archRows, totalRows int64, err error) {
	err = pool.QueryRow(ctx, `SELECT
		coalesce(sum(b) FILTER (WHERE t), 0)::bigint, coalesce(sum(b) FILTER (WHERE NOT t), 0)::bigint,
		coalesce(sum(n) FILTER (WHERE t), 0)::bigint, coalesce(sum(n), 0)::bigint FROM (
		SELECT m.run_id, r.status IN ('completed','failed','cancelled') AS t, count(*) AS n,
		  sum(28 + pg_column_size(m.id) + pg_column_size(m.run_id) + pg_column_size(m.seq) + pg_column_size(m.kind)
		      + coalesce(pg_column_size(m.agent), 0) + pg_column_size(m.payload) + pg_column_size(m.created_at)
		      + coalesce(pg_column_size(m.agent_instance), 0) + coalesce(pg_column_size(m.agent_label), 0)
		      + coalesce(pg_column_size(m.claim_generation), 0)) AS b
		FROM run_messages m JOIN runs r ON r.id = m.run_id GROUP BY m.run_id, r.status) s`).Scan(&archBytes, &liveBytes, &archRows, &totalRows)
	return archBytes, liveBytes, archRows, totalRows, err
}

func computeAttrib(ctx context.Context, pool *pgxpool.Pool, pre RelReport) (Attrib, error) {
	a := Attrib{Model: "per run: sum(24 + 4 + pg_column_size of each column) over its rows + index_bytes * rows_run / rows_total"}
	archBytes, liveBytes, archRows, totalRows, err := modelRowBytes(ctx, pool)
	if err != nil {
		return a, err
	}
	a.ArchivedRows, a.TotalRows = archRows, totalRows
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

// C1Basis is how the footprint bar was evaluated. The verdict never mixes a model on one side
// with a measurement on the other: it is real bytes on both sides (the pre-rollup size of the
// archived runs is a VACUUM FULL of a fresh corpus clone minus a VACUUM FULL of this config's
// run_messages once only never-rolled runs are left in it) or, when that baseline is
// unavailable, the row-byte model on both sides (chunk and watermark tables stay real bytes
// there, because a chunk table has no row model).
type C1Basis struct {
	Basis                  string `json:"basis_of_the_verdict"`
	Pre                    int64  `json:"pre_rollup_archived_runs_bytes"`
	Post                   int64  `json:"post_rollup_archived_runs_bytes"`
	ChunkTableBytes        int64  `json:"chunk_table_total_bytes"`
	WatermarkBytes         int64  `json:"watermark_table_total_bytes"`
	LeftoverRows           int64  `json:"archived_runs_rows_still_in_run_messages"`
	RealPre                int64  `json:"real_bytes_pre_vacuum_full_baseline_minus_live_only,omitempty"`
	ModelPre               int64  `json:"model_pre_bytes"`
	ModelPostLeftoverBytes int64  `json:"model_bytes_of_leftover_archived_rows"`
	Note                   string `json:"note,omitempty"`
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
	WALGap       int64              `json:"wal_lsn_bytes_between_backfill_end_and_vacuum_phase"`
	WALTotal     int64              `json:"wal_total_lsn_bytes_rollup_gap_vacuum"`
	VacuumRun    *DiskRun           `json:"backfill_with_periodic_vacuum,omitempty"`
	Sizes        map[string]any     `json:"sizes"`
	Latency      LatencyResults     `json:"latency"`
	Digests      DigestCheck        `json:"reader_results_vs_unrolled_corpus"`
	RoundTrip    RoundTripReport    `json:"roundtrip"`
	Concurrency  *ConcurrencyReport `json:"concurrency,omitempty"`
	Rewrite      RewriteReport      `json:"rewrite_headroom"`
	C1           C1Basis            `json:"c1_footprint_basis"`
	Explain      map[string]string  `json:"explain_analyze_buffers_chunk_fetch"`
	Criteria     []Criterion        `json:"criteria"`
	Overall      string             `json:"overall"`
}

func (e *Env) runConfig(ctx context.Context, idx int, sel Selection, attrib *Attrib) (*ConfigResult, error) {
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

	// This clone carries no concurrency hooks: its backfill is the clean one the cost bars (C5)
	// are read from. The reader and straggler checks run on their own clone (runCorrectness).
	e.Lg.f("config %s: backfill (worst case, no vacuum)", res.Name)
	if res.Backfill, err = e.backfill(ctx, pool, backfillOpts{cfg: cfg, sleep: e.Opt.BackfillSleep, withLoad: true}); err != nil {
		return res, err
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
	res.Digests = e.checkDigests(ctx, arch, pool, false)
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
	// Everything between the end of the backfill window and this one (ANALYZE, the size and
	// correctness reads, the latency runs, the checkpoint) is WAL the backfill window did not see.
	if res.WALGap, err = lsnGap(ctx, pool, res.Backfill.WAL.endLSN, wm.lsn); err != nil {
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
	res.WALTotal = res.Backfill.WAL.LSNBytes + res.WALGap + res.VacuumPhase.LSNBytes
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

	res.Explain = e.explainChunkFetch(ctx, pool)

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
	if res.C1, err = e.footprintBasis(ctx, pool, res.Rewrite.TotalAfter, attrib); err != nil {
		return res, err
	}
	pool.Close()
	return res, nil
}

// footprintBasis settles the C1 inputs after the VACUUM FULL of run_messages (see C1Basis).
func (e *Env) footprintBasis(ctx context.Context, pool *pgxpool.Pool, liveOnlyTotal int64, attrib *Attrib) (C1Basis, error) {
	var b C1Basis
	var err error
	if b.ChunkTableBytes, err = relTotal(ctx, pool, chunkTable); err != nil {
		return b, err
	}
	if b.WatermarkBytes, err = relTotal(ctx, pool, archiveTable); err != nil {
		return b, err
	}
	archBytes, _, archRows, totalRows, err := modelRowBytes(ctx, pool)
	if err != nil {
		return b, err
	}
	b.LeftoverRows = archRows
	if totalRows > 0 {
		var idx int64
		if err := pool.QueryRow(ctx, `SELECT pg_indexes_size('run_messages')`).Scan(&idx); err != nil {
			return b, err
		}
		b.ModelPostLeftoverBytes = archBytes + idx*archRows/totalRows
	}
	b.ModelPre = attrib.ArchivedPre
	realPost := b.ChunkTableBytes + b.WatermarkBytes
	switch {
	case e.CompactTotal > 0 && liveOnlyTotal > 0 && archRows == 0:
		b.Basis = "real_bytes"
		b.RealPre = max(0, e.CompactTotal-liveOnlyTotal)
		b.Pre, b.Post = b.RealPre, realPost
	default:
		b.Basis = "row_byte_model"
		b.Pre, b.Post = b.ModelPre, realPost+b.ModelPostLeftoverBytes
		b.Note = "real-bytes baseline unavailable or terminal rows were left in run_messages; the pre side is the row-byte model"
	}
	return b, nil
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

// runCorrectness is the reader and straggler check on its own clone, so its pauses, readers and
// late writers cannot distort the cost numbers of the clean clone. It also repeats the
// every-field round trip and the reader digests on the state the concurrent backfill left.
func (e *Env) runCorrectness(ctx context.Context, idx int, sel Selection) (*ConcurrencyReport, error) {
	cfg := sel.Config
	dbName := fmt.Sprintf("m0_cfg%dc", idx)
	e.Lg.f("config %s: concurrency check on its own clone", cfg.Name())
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
	cc := e.newConcurrencyCheck(pool, arch)
	bo := backfillOpts{cfg: cfg, sleep: e.Opt.BackfillSleep, hooks: cc.hooks(), pause: 150 * time.Millisecond, inflight: cc.fl}
	bf, err := e.backfill(ctx, pool, bo)
	cc.rep.BackfillRoundTripFail = bf.RoundTripFails
	if err != nil {
		return &cc.rep, err
	}
	rep, err := cc.finish(ctx, cfg)
	if err != nil {
		return &rep, err
	}
	rt := e.roundTripAll(ctx, arch, cfg)
	dg := e.checkDigests(ctx, arch, pool, true)
	rep.RoundTrip, rep.Digests = &rt, &dg
	return &rep, nil
}
