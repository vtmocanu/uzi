package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FootPoint is the combined footprint after one transaction.
type FootPoint struct {
	T     float64 `json:"t_s"`
	Bytes int64   `json:"bytes"`
}

// FootprintReport summarises the disk model of one backfill.
type FootprintReport struct {
	PreBytes     int64       `json:"pre_footprint_bytes"`
	PeakCombined int64       `json:"peak_combined_bytes"`
	PeakExtra    int64       `json:"peak_extra_bytes"`
	FinalChunks  int64       `json:"final_chunk_table_bytes"`
	FinalRM      int64       `json:"final_run_messages_bytes"`
	Samples      int         `json:"samples"`
	Series       []FootPoint `json:"series_downsampled"`
}

// TxSummary is the largest rollup transaction.
type TxSummary struct {
	Rows       int     `json:"rows"`
	RawBytes   int64   `json:"uncompressed_bytes"`
	CompBytes  int64   `json:"compressed_bytes"`
	DurationMs float64 `json:"duration_ms"`
	LockWaitMs float64 `json:"lock_wait_ms"`
}

// ProbeReport is the lock wait a concurrent writer-style probe saw on the runs row.
type ProbeReport struct {
	Probes int     `json:"probes"`
	P95Ms  float64 `json:"p95_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// LiveLoadReport compares live-reader latency idle and during the backfill.
type LiveLoadReport struct {
	Runs       int     `json:"live_runs_read"`
	Readers    int     `json:"readers"`
	IdleP95Ms  float64 `json:"idle_p95_ms"`
	DuringP95  float64 `json:"during_backfill_p95_ms"`
	Ratio      float64 `json:"ratio"`
	IdleN      int     `json:"idle_samples"`
	DuringN    int     `json:"during_samples"`
	NotMeasure string  `json:"not_measured,omitempty"`
}

// BackfillReport is the cost of one rollup pass over all terminal runs.
type BackfillReport struct {
	RunsRolled     int             `json:"runs_rolled"`
	RunsSkipped    int             `json:"runs_skipped_not_terminal"`
	Rows           int64           `json:"rows"`
	Txs            int             `json:"transactions"`
	MultiTxRuns    int             `json:"runs_published_in_multiple_txs"`
	RawBytes       int64           `json:"uncompressed_bytes"`
	CompBytes      int64           `json:"compressed_chunk_bytes"`
	Chunks         int             `json:"chunks"`
	DurationSec    float64         `json:"duration_s"`
	RowsPerSec     float64         `json:"rows_per_s"`
	RawMBPerSec    float64         `json:"uncompressed_mb_per_s"`
	LargestTx      TxSummary       `json:"largest_tx_by_duration"`
	RoundTripFails int             `json:"roundtrip_failures_in_tx"`
	WAL            WALStat         `json:"wal_rollup_phase"`
	Footprint      FootprintReport `json:"footprint"`
	Probe          ProbeReport     `json:"runs_row_lock_probe"`
	LiveLoad       LiveLoadReport  `json:"live_readers"`
	Throttle       string          `json:"throttle"`
}

type backfillOpts struct {
	cfg         RollupConfig
	sleep       time.Duration
	vacuumEvery int
	pause       time.Duration // between txs of one run (correctness config only)
	withLoad    bool          // idle/during live-reader comparison
	hooks       *correctnessHooks
}

// correctnessHooks run alongside the backfill.
type correctnessHooks struct {
	start func(ctx context.Context)
	stop  func()
}

func (e *Env) terminalInfos() []*RunInfo {
	var out []*RunInfo
	for _, in := range e.Infos {
		if in.Terminal {
			out = append(out, in)
		}
	}
	return out
}

// backfill rolls up every terminal run once and measures the cost.
func (e *Env) backfill(ctx context.Context, pool *pgxpool.Pool, bo backfillOpts) (BackfillReport, error) {
	var rep BackfillReport
	rep.Throttle = fmt.Sprintf("sleep %s between runs; vacuum every %d runs", bo.sleep, bo.vacuumEvery)
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		return rep, err
	}
	pre, err := footprint(ctx, pool)
	if err != nil {
		return rep, err
	}
	rep.Footprint.PreBytes, rep.Footprint.PeakCombined = pre, pre
	var series []FootPoint
	t0 := time.Now()
	sample := func() error {
		fp, err := footprint(ctx, pool)
		if err != nil {
			return err
		}
		rep.Footprint.PeakCombined = max(rep.Footprint.PeakCombined, fp)
		series = append(series, FootPoint{T: time.Since(t0).Seconds(), Bytes: fp})
		return nil
	}
	if bo.withLoad {
		rep.LiveLoad = e.idleLive(ctx, pool)
	}
	loadStop := e.startLiveLoad(ctx, pool, bo.withLoad)
	probeStop := startProbe(ctx, pool, e.terminalInfos())
	if bo.hooks != nil {
		bo.hooks.start(ctx)
	}
	wm, err := markWAL(ctx, pool)
	if err != nil {
		return rep, err
	}
	t0 = time.Now()
	var lockWaits []time.Duration
	runs := e.terminalInfos()
	for i, in := range runs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		txs := 0
		skipped, err := rollupRun(ctx, pool, e.Eng, bo.cfg, in.ID, bo.pause, func(st TxStat) {
			txs++
			rep.Txs++
			rep.Rows += int64(st.Rows)
			rep.RawBytes += st.RawBytes
			rep.CompBytes += st.CompBytes
			rep.Chunks += st.Chunks
			lockWaits = append(lockWaits, st.LockWait)
			if st.Duration > time.Duration(rep.LargestTx.DurationMs*float64(time.Millisecond)) {
				rep.LargestTx = TxSummary{Rows: st.Rows, RawBytes: st.RawBytes, CompBytes: st.CompBytes, DurationMs: ms(st.Duration), LockWaitMs: ms(st.LockWait)}
			}
			_ = sample()
		})
		switch {
		case errors.Is(err, errRoundTrip):
			rep.RoundTripFails++
			continue
		case err != nil:
			return rep, fmt.Errorf("rollup: %w", err)
		case skipped:
			rep.RunsSkipped++
			continue
		}
		rep.RunsRolled++
		if txs > 1 {
			rep.MultiTxRuns++
		}
		if bo.vacuumEvery > 0 && (i+1)%bo.vacuumEvery == 0 {
			if _, err := pool.Exec(ctx, `VACUUM run_messages`); err != nil {
				return rep, err
			}
			_ = sample()
		}
		if bo.sleep > 0 {
			time.Sleep(bo.sleep)
		}
		if (i+1)%25 == 0 {
			e.Lg.f("backfill %d/%d runs, %d tx", i+1, len(runs), rep.Txs)
		}
	}
	rep.DurationSec = time.Since(t0).Seconds()
	if bo.hooks != nil {
		bo.hooks.stop()
	}
	rep.Probe = probeStop()
	if bo.withLoad {
		during := loadStop()
		rep.LiveLoad.DuringN = len(during)
		if len(during) > 0 {
			rep.LiveLoad.DuringP95 = summarize(during).P95Ms
		}
		if rep.LiveLoad.IdleP95Ms > 0 {
			rep.LiveLoad.Ratio = rep.LiveLoad.DuringP95 / rep.LiveLoad.IdleP95Ms
		}
	} else {
		loadStop()
	}
	if rep.WAL, err = endWAL(ctx, pool, wm); err != nil {
		return rep, err
	}
	if err := sample(); err != nil {
		return rep, err
	}
	if rep.DurationSec > 0 {
		rep.RowsPerSec = float64(rep.Rows) / rep.DurationSec
		rep.RawMBPerSec = float64(rep.RawBytes) / rep.DurationSec / (1 << 20)
	}
	rep.Footprint.PeakExtra = rep.Footprint.PeakCombined - pre
	rep.Footprint.Samples = len(series)
	rep.Footprint.Series = downsample(series, 200)
	_ = pool.QueryRow(ctx, `SELECT pg_total_relation_size('`+chunkTable+`'), pg_total_relation_size('run_messages')`).Scan(&rep.Footprint.FinalChunks, &rep.Footprint.FinalRM)
	return rep, nil
}

func downsample(s []FootPoint, n int) []FootPoint {
	if len(s) <= n {
		return s
	}
	out := make([]FootPoint, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[i*(len(s)-1)/(n-1)])
	}
	return out
}

// idleLive measures live-reader p95 with nothing else running.
func (e *Env) idleLive(ctx context.Context, pool *pgxpool.Pool) LiveLoadReport {
	rep := LiveLoadReport{Readers: 4}
	runs := e.liveRuns()
	rep.Runs = len(runs)
	if len(runs) == 0 {
		rep.NotMeasure = "the corpus has no non-terminal runs (every run is rolled), so there is no untouched live run to read"
		return rep
	}
	sh := &readerShapes()[0]
	rd := &LiveReader{Pool: pool}
	durs, _, _ := loopWorkers(ctx, rep.Readers, 800, func(w, i int) error {
		r := runs[(w*13+i)%len(runs)]
		_, _, err := rd.Read(ctx, sh, r.ID, sh.Spec(r.MaxSeq))
		return err
	})
	rep.IdleN = len(durs)
	rep.IdleP95Ms = summarize(durs).P95Ms
	return rep
}

func (e *Env) liveRuns() []*RunInfo {
	var out []*RunInfo
	for _, in := range e.Infos {
		if !in.Terminal && in.Count >= 200 {
			out = append(out, in)
		}
	}
	return out
}

// startLiveLoad reads live (non-terminal) runs continuously until the returned stop is called,
// which returns every latency sample.
func (e *Env) startLiveLoad(ctx context.Context, pool *pgxpool.Pool, enabled bool) func() []time.Duration {
	runs := e.liveRuns()
	if !enabled || len(runs) == 0 {
		return func() []time.Duration { return nil }
	}
	sh := &readerShapes()[0]
	rd := &LiveReader{Pool: pool}
	stop := make(chan struct{})
	var mu sync.Mutex
	var all []time.Duration
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				default:
				}
				r := runs[(w*13+i)%len(runs)]
				t0 := time.Now()
				if _, _, err := rd.Read(ctx, sh, r.ID, sh.Spec(r.MaxSeq)); err == nil {
					mu.Lock()
					all = append(all, time.Since(t0))
					mu.Unlock()
				}
			}
		}()
	}
	return func() []time.Duration {
		close(stop)
		wg.Wait()
		return all
	}
}

// startProbe takes FOR KEY SHARE on a random terminal run's row, which is the lock a
// run_messages insert takes through its foreign key and which the rollup's FOR UPDATE conflicts
// with. The time that takes is the wait a concurrent writer would see.
func startProbe(ctx context.Context, pool *pgxpool.Pool, runs []*RunInfo) func() ProbeReport {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var durs []time.Duration
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := newRNG(5, 5)
		for len(runs) > 0 {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Millisecond):
			}
			r := runs[rng.IntN(len(runs))]
			t0 := time.Now()
			tx, err := pool.Begin(ctx)
			if err != nil {
				continue
			}
			var id uuid.UUID
			err = tx.QueryRow(ctx, `SELECT id FROM runs WHERE id = $1 FOR KEY SHARE`, r.ID).Scan(&id)
			_ = tx.Commit(ctx)
			if err == nil {
				durs = append(durs, time.Since(t0))
			}
		}
	}()
	return func() ProbeReport {
		close(stop)
		wg.Wait()
		s := summarize(durs)
		return ProbeReport{Probes: s.N, P95Ms: s.P95Ms, MaxMs: s.MaxMs}
	}
}

// ---- correctness under concurrency -----------------------------------------------------------

// ConcurrencyReport is the outcome of the concurrent reader and straggler checks.
type ConcurrencyReport struct {
	Reads                 int64    `json:"reader_full_replays"`
	Violations            int64    `json:"order_or_duplicate_violations"`
	MissingSeqs           int64    `json:"missing_source_seqs"`
	UnexpectedSeqs        int64    `json:"unexpected_seqs"`
	ReadErrors            int64    `json:"read_errors"`
	MixedStateObserved    int64    `json:"reads_that_saw_archived_and_live_source_rows"`
	MixedOnMultiTxRun     int64    `json:"mixed_state_reads_of_multi_tx_runs"`
	StragglerInserts      int64    `json:"straggler_rows_inserted"`
	StragglerInsertErrors int64    `json:"straggler_insert_errors"`
	StragglerWaitP95Ms    float64  `json:"straggler_insert_wait_p95_ms"`
	StragglerWaitMaxMs    float64  `json:"straggler_insert_wait_max_ms"`
	StragglersArchived    int64    `json:"stragglers_ended_archived"`
	StragglersLive        int64    `json:"stragglers_ended_live"`
	StragglersLost        int64    `json:"stragglers_lost_or_duplicated"`
	Samples               []string `json:"first_problems,omitempty"`
}

type concurrencyCheck struct {
	e      *Env
	pool   *pgxpool.Pool
	arch   *ArchReader
	rep    ConcurrencyReport
	mu     sync.Mutex
	intent map[uuid.UUID]map[int]bool
	next   map[uuid.UUID]int
	waits  []time.Duration
	cancel context.CancelFunc
	wg     sync.WaitGroup
	multi  map[uuid.UUID]bool
}

func (e *Env) newConcurrencyCheck(pool *pgxpool.Pool, arch *ArchReader) *concurrencyCheck {
	c := &concurrencyCheck{e: e, pool: pool, arch: arch, intent: map[uuid.UUID]map[int]bool{}, next: map[uuid.UUID]int{}, multi: map[uuid.UUID]bool{}}
	for _, in := range e.terminalInfos() {
		c.next[in.ID] = in.MaxSeq + 1
		if in.Count > 0 && in.Count >= e.Strata.LargeMin {
			c.multi[in.ID] = true
		}
	}
	return c
}

func (c *concurrencyCheck) problem(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rep.Samples) < 5 {
		c.rep.Samples = append(c.rep.Samples, scrub(s))
	}
}

func (c *concurrencyCheck) hooks() *correctnessHooks {
	return &correctnessHooks{
		start: func(ctx context.Context) {
			cctx, cancel := context.WithCancel(ctx)
			c.cancel = cancel
			for w := 0; w < 4; w++ {
				c.wg.Add(1)
				go c.readerLoop(cctx, w)
			}
			c.wg.Add(1)
			go c.stragglerLoop(cctx)
		},
		stop: func() {
			c.cancel()
			c.wg.Wait()
		},
	}
}

func (c *concurrencyCheck) readerLoop(ctx context.Context, w int) {
	defer c.wg.Done()
	terminal := c.e.terminalInfos()
	var large []*RunInfo
	for _, in := range terminal {
		if c.multi[in.ID] {
			large = append(large, in)
		}
	}
	rng := newRNG(11, uint64(w)) //nolint:gosec // G115: small loop index.
	full := &Shape{}
	for ctx.Err() == nil {
		var in *RunInfo
		if len(large) > 0 && rng.IntN(2) == 0 {
			in = large[rng.IntN(len(large))]
		} else {
			in = terminal[rng.IntN(len(terminal))]
		}
		out, st, err := c.arch.Read(ctx, full, in.ID, ReadSpec{})
		if err != nil {
			if ctx.Err() == nil {
				c.mu.Lock()
				c.rep.ReadErrors++
				c.mu.Unlock()
				c.problem("read error: " + err.Error())
			}
			continue
		}
		c.check(in, out, st)
	}
}

// check validates one full replay against the source seq set.
func (c *concurrencyCheck) check(in *RunInfo, out []Record, st ReadStats) {
	src := c.e.Seqs[in.ID]
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rep.Reads++
	have := make(map[int]bool, len(out))
	prev := 0
	extras := 0
	for i := range out {
		s := out[i].Seq
		if s <= prev || have[s] {
			c.rep.Violations++
			if len(c.rep.Samples) < 5 {
				c.rep.Samples = append(c.rep.Samples, "seq order or duplicate violation")
			}
		}
		prev = s
		have[s] = true
	}
	inSrc := make(map[int]bool, len(src))
	for _, s := range src {
		inSrc[s] = true
		if !have[s] {
			c.rep.MissingSeqs++
		}
	}
	for s := range have {
		if !inSrc[s] {
			extras++
			if !c.intent[in.ID][s] {
				c.rep.UnexpectedSeqs++
			}
		}
	}
	if st.ChunksFetched > 0 && st.LiveRows-extras > 0 {
		c.rep.MixedStateObserved++
		if c.multi[in.ID] {
			c.rep.MixedOnMultiTxRun++
		}
	}
}

func (c *concurrencyCheck) stragglerLoop(ctx context.Context) {
	defer c.wg.Done()
	terminal := c.e.terminalInfos()
	rng := newRNG(13, 13)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
		in := terminal[rng.IntN(len(terminal))]
		c.mu.Lock()
		seq := c.next[in.ID]
		c.next[in.ID]++
		if c.intent[in.ID] == nil {
			c.intent[in.ID] = map[int]bool{}
		}
		c.intent[in.ID][seq] = true // recorded before the insert, so a reader can never see an unexplained row
		c.mu.Unlock()
		t0 := time.Now()
		_, err := c.pool.Exec(ctx, `INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, $2, 'text', '{"text":"late"}')`, in.ID, seq)
		d := time.Since(t0)
		c.mu.Lock()
		if err != nil {
			if ctx.Err() == nil {
				c.rep.StragglerInsertErrors++
				delete(c.intent[in.ID], seq)
			}
		} else {
			c.rep.StragglerInserts++
			c.waits = append(c.waits, d)
		}
		c.mu.Unlock()
	}
}

// finish re-rolls runs that took stragglers, then proves each straggler is archived or live
// exactly once.
func (c *concurrencyCheck) finish(ctx context.Context, cfg RollupConfig) (ConcurrencyReport, error) {
	s := summarize(c.waits)
	c.rep.StragglerWaitP95Ms, c.rep.StragglerWaitMaxMs = s.P95Ms, s.MaxMs
	var runs []uuid.UUID
	for id := range c.intent {
		runs = append(runs, id)
	}
	for _, id := range runs {
		if _, err := rollupRun(ctx, c.pool, c.e.Eng, cfg, id, 0, nil); err != nil && !errors.Is(err, errRoundTrip) {
			return c.rep, err
		}
	}
	for _, id := range runs {
		out, _, err := c.arch.Read(ctx, &Shape{}, id, ReadSpec{})
		if err != nil {
			return c.rep, err
		}
		count := map[int]int{}
		for i := range out {
			count[out[i].Seq]++
		}
		for seq := range c.intent[id] {
			switch count[seq] {
			case 1:
				var live bool
				if err := c.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_messages WHERE run_id = $1 AND seq = $2)`, id, seq).Scan(&live); err != nil {
					return c.rep, err
				}
				if live {
					c.rep.StragglersLive++
				} else {
					c.rep.StragglersArchived++
				}
			default:
				c.rep.StragglersLost++
			}
		}
	}
	return c.rep, nil
}
