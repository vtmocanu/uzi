// Command rollupbench is the M0 measurement harness for issue #2755: it decides, with numbers,
// whether a TERMINAL run's run_messages rows can be rolled up into compressed chunks without
// loss and without a regression the readers would notice. It is a measurement tool, not
// production code: it owns a throwaway Postgres (refusing to touch anything without
// --i-own-this-db), builds a corpus, measures size, read latency, correctness under concurrency
// and backfill cost for several chunk configurations, and writes results.json, settings.json
// and summary.md. Run it through scripts/measure-run-message-rollups.sh.
//
// Nothing it writes contains a DSN, a run id or a payload: run ids become ordinals and every
// text is scrubbed of uuid- and DSN-shaped strings before it reaches disk.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

// Options are the parsed flags plus the scale-derived corpus parameters.
type Options struct {
	DSN            string
	IOwn           bool
	Out            string
	Corpus         string
	NDJSONDir      string
	SourceDSN      string
	Seed           uint64
	Scale          string
	Iterations     int
	Readers        []int
	BackfillSleep  time.Duration
	VacuumEvery    int
	MaxChunksPerTx int
	LargeMin       int
	TextRoot       string

	synth synthParams
	grid  int
}

func (o Options) maxReaders() int {
	m := 1
	for _, r := range o.Readers {
		m = max(m, r)
	}
	return m
}

func parseOptions(args []string) (Options, error) {
	var o Options
	var readers, sleep, scale string
	fs := flag.NewFlagSet("rollupbench", flag.ContinueOnError)
	fs.StringVar(&o.DSN, "dsn", "", "target Postgres DSN (a throwaway server this tool may create and drop databases on)")
	fs.BoolVar(&o.IOwn, "i-own-this-db", false, "required: confirms --dsn is a disposable server")
	fs.StringVar(&o.Out, "out", "", "output directory (required)")
	fs.StringVar(&o.Corpus, "corpus", "synthetic", "synthetic | ndjson | snapshot")
	fs.StringVar(&o.NDJSONDir, "corpus-ndjson", "", "directory of one MessageDTO ndjson file per run (implies --corpus ndjson)")
	fs.StringVar(&o.SourceDSN, "source-dsn", "", "restored snapshot to copy terminal runs from, READ ONLY (implies --corpus snapshot)")
	seed := fs.Uint64("seed", 1, "corpus seed")
	fs.StringVar(&scale, "scale", "tiny", "tiny (a minute or two) | full (about 200 runs, a few hundred MB)")
	fs.IntVar(&o.Iterations, "iterations", 0, "requests per latency cell (default 1000 full, 100 tiny; heavy shapes run fewer, recorded)")
	fs.StringVar(&readers, "readers", "1,8,32", "concurrent reader counts")
	fs.StringVar(&sleep, "backfill-sleep", "0s", "pause between runs during the backfill (throttle)")
	fs.IntVar(&o.VacuumEvery, "vacuum-every", 10, "run (ii): plain VACUUM after every K runs (0 skips run (ii))")
	fs.IntVar(&o.MaxChunksPerTx, "max-chunks-per-tx", 0, "most chunks one rollup transaction publishes; a bigger run takes several (default 8 full, 1 tiny)")
	fs.IntVar(&o.LargeMin, "large-min", 0, "messages for the large stratum (default 20000 full, 1500 tiny)")
	fs.StringVar(&o.TextRoot, "text-root", ".", "git checkout whose tracked text files feed the synthetic tool results")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.Seed, o.Scale = *seed, scale
	if o.NDJSONDir != "" {
		o.Corpus = "ndjson"
	}
	if o.SourceDSN != "" {
		o.Corpus = "snapshot"
	}
	switch {
	case o.DSN == "":
		return o, errors.New("--dsn is required")
	case !o.IOwn:
		return o, errors.New("refusing to run: pass --i-own-this-db to confirm --dsn is a disposable server (the harness creates and drops databases)")
	case o.Out == "":
		return o, errors.New("--out is required")
	case o.Corpus == "ndjson" && o.NDJSONDir == "":
		return o, errors.New("--corpus ndjson needs --corpus-ndjson DIR")
	case o.Corpus == "snapshot" && o.SourceDSN == "":
		return o, errors.New("--corpus snapshot needs --source-dsn")
	case o.Corpus != "synthetic" && o.Corpus != "ndjson" && o.Corpus != "snapshot":
		return o, errors.New("--corpus must be synthetic, ndjson or snapshot")
	}
	var err error
	if o.BackfillSleep, err = time.ParseDuration(sleep); err != nil {
		return o, errors.New("bad --backfill-sleep")
	}
	for _, f := range strings.Split(readers, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			return o, errors.New("bad --readers")
		}
		o.Readers = append(o.Readers, n)
	}
	switch scale {
	case "tiny":
		o.synth = synthParams{Runs: 60, Large: 2, LargeMin: 1500, TargetMiB: 40, Window: 8, BigMiB: 3}
		o.grid = 8
		o.Iterations = cmpDefault(o.Iterations, 100)
		o.MaxChunksPerTx = cmpDefault(o.MaxChunksPerTx, 1)
	case "full":
		o.synth = synthParams{Runs: 200, Large: 6, LargeMin: 20000, TargetMiB: 700, Window: 20, BigMiB: 3}
		o.grid = 48
		o.Iterations = cmpDefault(o.Iterations, 1000)
		o.MaxChunksPerTx = cmpDefault(o.MaxChunksPerTx, 8)
	default:
		return o, errors.New("--scale must be tiny or full")
	}
	if o.LargeMin == 0 {
		o.LargeMin = o.synth.LargeMin
	}
	o.synth.LargeMin = o.LargeMin
	o.synth.Seed, o.synth.TextRoot = o.Seed, o.TextRoot
	return o, nil
}

func cmpDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func main() {
	opt, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "rollupbench:", err)
		os.Exit(2)
	}
	goose.SetLogger(goose.NopLogger()) // 321 per-migration lines per throwaway database drown the progress lines
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, opt); err != nil {
		fmt.Fprintln(os.Stderr, "rollupbench:", scrub(err.Error()))
		os.Exit(1)
	}
}

// Results is results.json.
type Results struct {
	Meta         map[string]any   `json:"meta"`
	Fingerprint  *Fingerprint     `json:"corpus_fingerprint,omitempty"`
	Strata       map[string]any   `json:"strata,omitempty"`
	Attribution  *Attrib          `json:"footprint_attribution,omitempty"`
	Baseline     map[string]any   `json:"baseline_sizes,omitempty"`
	Lz4          *Lz4Report       `json:"per_message_lz4_reference,omitempty"`
	GridSample   *GridSample      `json:"grid_sample,omitempty"`
	Grid         []GridCell       `json:"size_grid,omitempty"`
	Selected     []map[string]any `json:"selected_configs,omitempty"`
	Live         *LiveBaseline    `json:"live_baseline,omitempty"`
	Configs      []*ConfigResult  `json:"configs,omitempty"`
	Verdicts     map[string]any   `json:"verdicts,omitempty"`
	Notes        []string         `json:"notes,omitempty"`
	LiveDigest   *DigestCheck     `json:"live_digest_selfcheck,omitempty"`
	CompletedAll bool             `json:"completed_all_phases"`
}

func (r *Results) write(dir string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "results.json"), []byte(scrub(string(b))+"\n"), 0o600)
}

func writeJSON(dir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(scrub(string(b))+"\n"), 0o600)
}

// Lz4Report is the per-message lz4 TOAST reference.
type Lz4Report struct {
	Supported      bool             `json:"supported"`
	Error          string           `json:"error,omitempty"`
	Compression    map[string]int64 `json:"payload_compression_counts"`
	Size           *RelReport       `json:"run_messages"`
	TotalVsPglz    float64          `json:"total_size_over_pglz_baseline"`
	ToastShare     float64          `json:"toast_share"`
	PglzToastShare float64          `json:"pglz_baseline_toast_share"`
	LoadSeconds    float64          `json:"load_s"`
}

func run(ctx context.Context, opt Options) error {
	if err := os.MkdirAll(opt.Out, 0o750); err != nil {
		return errors.New("cannot create --out")
	}
	lg := &logger{start: time.Now()}
	adminCfg, err := pgxpool.ParseConfig(opt.DSN)
	if err != nil {
		return errors.New("cannot parse --dsn")
	}
	adminCfg.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		return errors.New("cannot connect to --dsn")
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		return errors.New("cannot reach --dsn")
	}

	eng, err := NewEngine(64<<20, runtime.NumCPU())
	if err != nil {
		return err
	}
	defer eng.Close()
	env := &Env{Opt: opt, Admin: admin, BaseDSN: opt.DSN, Eng: eng, Lg: lg}
	res := &Results{Meta: map[string]any{}}

	var pgVersion string
	_ = admin.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	settings, err := settingsAll(ctx, admin)
	if err != nil {
		return err
	}
	res.Meta = map[string]any{
		"purpose": "issue #2755 M0: rollup of terminal run_messages into compressed chunks",
		"scale":   opt.Scale, "corpus": opt.Corpus, "seed": opt.Seed, "iterations": opt.Iterations, "readers": opt.Readers,
		"num_cpu": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "go": runtime.Version(), "postgres": pgVersion,
		"backfill_sleep": opt.BackfillSleep.String(), "vacuum_every_runs": opt.VacuumEvery, "max_chunks_per_tx": opt.MaxChunksPerTx,
		"started_utc": time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSON(opt.Out, "settings.json", map[string]any{
		"postgres_show_all": settings,
		"harness": map[string]any{
			"postgres_tuning":   "stock: the harness changes no server setting (only per-table autovacuum_enabled=false on run_messages and the chunk table)",
			"pgx_exec_mode":     "pgx default (QueryExecModeCacheStatement) on every pool, live and archived alike",
			"pool_max_conns":    opt.maxReaders() + 8,
			"engine":            eng.PoolingNote(),
			"zstd_levels":       "requested level n is passed through zstd.EncoderLevelFromZstd(n)",
			"cache":             "no application cache on the archived path; warm database cache (untimed warm-up per cell)",
			"chunk_table":       chunkTable + " (data bytea, ALTER COLUMN data SET STORAGE EXTERNAL, PRIMARY KEY (run_id, first_seq)), watermark table " + archiveTable,
			"read_transaction":  "REPEATABLE READ READ ONLY for every archived read and for the tx-wrapped live baseline",
			"database_clone":    "CREATE DATABASE ... TEMPLATE m0_corpus STRATEGY FILE_COPY; the cost clone and the concurrency-check clone are separate databases",
			"num_cpu":           runtime.NumCPU(),
			"go_version":        runtime.Version(),
			"scale_parameters":  opt.synth,
			"max_decoded_chunk": 64 << 20,
		},
	}); err != nil {
		return err
	}

	// ---- corpus ----
	switch opt.Corpus {
	case "synthetic":
		env.Src = newSynthSource(opt.synth)
	case "ndjson":
		if env.Src, err = newNDJSONSource(opt.NDJSONDir, opt.LargeMin); err != nil {
			return err
		}
	default:
		if env.Src, err = newSnapshotSource(ctx, opt.SourceDSN, opt.LargeMin); err != nil {
			return err
		}
	}
	lg.f("corpus %s: %d runs", env.Src.Label(), len(env.Src.Runs()))
	corpus, err := env.newMigratedDB(ctx, "m0_corpus", opt.maxReaders()+8, nil)
	if err != nil {
		return err
	}
	defer func() { _ = env.dropDB(context.WithoutCancel(ctx), "m0_corpus") }()
	if _, err := corpus.Exec(ctx, `ALTER TABLE run_messages SET (autovacuum_enabled = false, toast.autovacuum_enabled = false)`); err != nil {
		return err
	}
	t0 := time.Now()
	if err := loadCorpus(ctx, corpus, env.Src); err != nil {
		return err
	}
	loadSecs := time.Since(t0).Seconds()
	lg.f("corpus loaded in %.1fs", loadSecs)
	if err := corpus.QueryRow(ctx, `SELECT user_id FROM runs LIMIT 1`).Scan(&env.User); err != nil {
		return err
	}
	if env.Infos, err = loadInfos(ctx, corpus); err != nil {
		return err
	}
	env.Strata = buildStrata(env.Infos, opt.LargeMin)
	res.Strata = env.Strata.Describe()
	fp, err := fingerprint(ctx, corpus, env.Src.Label(), env.Src.Note(), env.Infos)
	if err != nil {
		return err
	}
	res.Fingerprint = &fp
	pre, err := measureRel(ctx, corpus, "run_messages", "before rollup")
	if err != nil {
		return err
	}
	res.Baseline = map[string]any{"run_messages_before_rollup": pre, "corpus_load_s": loadSecs}
	attrib, err := computeAttrib(ctx, corpus, pre)
	if err != nil {
		return err
	}
	res.Attribution = &attrib
	if env.Strata.Note != "" {
		res.Notes = append(res.Notes, env.Strata.Note)
	}
	_ = res.write(opt.Out)

	lg.f("capturing reference digests on the unrolled corpus")
	if err := env.captureDigests(ctx, corpus); err != nil {
		return err
	}

	// ---- offline grid ----
	lg.f("size grid")
	sample, gs, err := sampleForGrid(ctx, corpus, env.Infos, opt.Seed, opt.grid)
	if err != nil {
		return err
	}
	res.GridSample = &gs
	if res.Grid, err = runGrid(ctx, eng, sample, lg); err != nil {
		return err
	}
	selected := pickConfigs(res.Grid, opt.MaxChunksPerTx)
	for _, s := range selected {
		res.Selected = append(res.Selected, map[string]any{"config": s.Config.Name(), "because": s.Why})
	}
	_ = res.write(opt.Out)

	// ---- live baselines ----
	lg.f("live baselines")
	live := &LiveBaseline{}
	mo := MatrixOpts{Readers: opt.Readers, Iterations: opt.Iterations, Seed: opt.Seed, PeakAt: opt.maxReaders()}
	mo.Backend = "live_autocommit"
	if live.Auto, err = env.measureMatrix(ctx, &LiveReader{Pool: corpus}, mo); err != nil {
		return err
	}
	mo.Backend = "live_in_tx"
	if live.Tx, err = env.measureMatrix(ctx, &LiveReader{Pool: corpus, InTx: true}, mo); err != nil {
		return err
	}
	live.ChatAuto = env.measureChat(ctx, corpus, "live_autocommit", false, false, critReaders(opt.Readers), opt.Iterations)
	live.ChatTx = env.measureChat(ctx, corpus, "live_in_tx", false, true, critReaders(opt.Readers), opt.Iterations)
	res.Live = live
	_ = res.write(opt.Out)

	// ---- per-message lz4 reference ----
	lg.f("per-message lz4 reference")
	res.Lz4 = env.lz4Reference(ctx, pre)
	_ = res.write(opt.Out)

	corpus.Close()

	lg.f("VACUUM FULL baseline of the whole corpus (real pre-rollup size)")
	if env.CompactTotal, err = env.compactBaseline(ctx); err != nil {
		res.Notes = append(res.Notes, "real-bytes pre-rollup baseline failed, C1 falls back to the row-byte model: "+scrub(err.Error()))
	}
	res.Baseline["run_messages_total_after_vacuum_full_of_whole_corpus"] = env.CompactTotal
	_ = res.write(opt.Out)

	// ---- configs ----
	seenCodec := map[CodecID]bool{}
	for i, sel := range selected {
		first := !seenCodec[sel.Config.Spec.ID]
		cr, err := env.runConfig(ctx, i, sel, &attrib)
		if cr != nil {
			if err != nil {
				cr.Overall = "incomplete: " + scrub(err.Error())
			}
			res.Configs = append(res.Configs, cr)
		}
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("config %s did not complete: %s", sel.Config.Name(), scrub(err.Error())))
			_ = res.write(opt.Out)
			continue
		}
		if first {
			seenCodec[sel.Config.Spec.ID] = true // only a config that completed counts as this codec's check
			rep, cerr := env.runCorrectness(ctx, i, sel)
			if rep == nil {
				rep = &ConcurrencyReport{}
			}
			if cerr != nil {
				rep.Failure = scrub(cerr.Error())
				res.Notes = append(res.Notes, fmt.Sprintf("config %s concurrency check did not complete: %s", cr.Name, rep.Failure))
			}
			cr.Concurrency = rep
		}
		if opt.VacuumEvery > 0 {
			lg.f("config %s: backfill with vacuum every %d runs", cr.Name, opt.VacuumEvery)
			if cr.VacuumRun, err = env.runVacuumEvery(ctx, i, sel.Config); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("config %s periodic-vacuum run failed: %s", cr.Name, scrub(err.Error())))
			}
		}
		evaluate(cr, live, &attrib, opt.Readers, fp.LargestMsg, fp.LargestTermM)
		_ = res.write(opt.Out)
	}
	res.Verdicts = map[string]any{}
	for _, c := range res.Configs {
		res.Verdicts[c.Name] = c.Overall
	}
	res.Notes = append(res.Notes,
		"C7 (reader inventory) is written by the lead in the PRD: C7: not evaluated by harness",
		"pg_waldump --stats per phase is not run by the harness; the pg_current_wal_insert_lsn delta is the WAL evidence; pg_stat_wal figures (wal_bytes, wal_fpi, wal_records) are a cross-check, not exact: backends report their counters lazily, so they are a lower bound",
		"percentiles are nearest-rank; p99 over ~1000 samples is indicative only; heavy shapes ran fewer iterations (recorded per cell)")
	res.CompletedAll = true
	res.Meta["finished_utc"] = time.Now().UTC().Format(time.RFC3339)
	res.Meta["duration_s"] = time.Since(lg.start).Seconds()
	if err := res.write(opt.Out); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(opt.Out, "summary.md"), []byte(scrub(summaryMD(res))), 0o600); err != nil {
		return err
	}
	lg.f("done")
	return nil
}

// lz4Reference loads the same corpus into a fresh database with default_toast_compression=lz4
// set before the load, so every payload is compressed per message by lz4 instead of pglz.
func (e *Env) lz4Reference(ctx context.Context, pglz RelReport) *Lz4Report {
	rep := &Lz4Report{Compression: map[string]int64{}}
	pool, err := e.newMigratedDB(ctx, "m0_lz4", 4, map[string]string{"default_toast_compression": "lz4"})
	if err != nil {
		rep.Error = scrub(err.Error())
		return rep
	}
	defer func() { _ = e.dropDB(context.WithoutCancel(ctx), "m0_lz4") }()
	defer pool.Close()
	t0 := time.Now()
	if err := loadCorpus(ctx, pool, e.Src); err != nil {
		rep.Error = scrub(err.Error())
		return rep
	}
	rep.LoadSeconds = time.Since(t0).Seconds()
	rows, err := pool.Query(ctx, `SELECT coalesce(pg_column_compression(payload)::text, 'inline_uncompressed'), count(*) FROM run_messages GROUP BY 1`)
	if err != nil {
		rep.Error = scrub(err.Error())
		return rep
	}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			rep.Error = scrub(err.Error())
			rows.Close()
			return rep
		}
		rep.Compression[k] = n
	}
	rows.Close()
	rep.Supported = rep.Compression["lz4"] > 0 && rep.Compression["pglz"] == 0
	sz, err := measureRel(ctx, pool, "run_messages", "per-message lz4")
	if err != nil {
		rep.Error = scrub(err.Error())
		return rep
	}
	rep.Size = &sz
	if pglz.Total > 0 {
		rep.TotalVsPglz = float64(sz.Total) / float64(pglz.Total)
		if t := pglz.Heap.TableLen + pglz.ToastSize; t > 0 {
			rep.PglzToastShare = float64(pglz.ToastSize) / float64(t)
		}
	}
	if t := sz.Heap.TableLen + sz.ToastSize; t > 0 {
		rep.ToastShare = float64(sz.ToastSize) / float64(t)
	}
	return rep
}

func summaryMD(r *Results) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("# Run message rollup M0 results")
	w("")
	w("Corpus: %v, scale %v, %v CPUs, Postgres %v. Completed all phases: %v.", r.Meta["corpus"], r.Meta["scale"], r.Meta["num_cpu"], r.Meta["postgres"], r.CompletedAll)
	if f := r.Fingerprint; f != nil {
		w("")
		w("## Corpus fingerprint")
		w("")
		w("%d rows over %d runs (%d terminal, %d live). Heap %d MiB, TOAST %d MiB (%.0f%% of heap plus TOAST), indexes %d MiB. Largest payload %d bytes.",
			f.Rows, f.Runs, f.TerminalRuns, f.LiveRuns, f.HeapBytes>>20, f.ToastBytes>>20, f.ToastShare*100, f.IndexBytes>>20, f.LargestMsg)
	}
	if len(r.Grid) > 0 {
		w("")
		w("## Selected configs")
		w("")
		for _, s := range r.Selected {
			w("- %v (%v)", s["config"], s["because"])
		}
	}
	if l := r.Lz4; l != nil {
		w("")
		w("## Per-message lz4 reference")
		w("")
		if l.Error != "" {
			w("Not measured: %s", l.Error)
		} else {
			w("compression counts %v; run_messages total is %.2fx the pglz baseline; TOAST share %.0f%% (pglz %.0f%%).", l.Compression, l.TotalVsPglz, l.ToastShare*100, l.PglzToastShare*100)
		}
	}
	w("")
	w("## Verdicts per config")
	w("")
	w("| config | overall | C1 | C2 | C3 | C4 | C5 | C6 | C7 |")
	w("|---|---|---|---|---|---|---|---|---|")
	for _, c := range r.Configs {
		cells := make([]string, 0, 7)
		for _, k := range c.Criteria {
			switch {
			case k.Pass == nil:
				cells = append(cells, "n/e")
			case *k.Pass:
				cells = append(cells, "pass")
			default:
				cells = append(cells, "FAIL")
			}
		}
		for len(cells) < 7 {
			cells = append(cells, "-")
		}
		w("| %s | %s | %s |", c.Name, c.Overall, strings.Join(cells, " | "))
	}
	w("")
	w("C7 (reader inventory): not evaluated by harness.")
	for _, c := range r.Configs {
		w("")
		w("### %s", c.Name)
		w("")
		bf := c.Backfill
		w("- chunks: %d, compressed %d MiB from %d MiB uncompressed; backfill %.0fs, %.0f rows/s; largest tx %.0f ms; rewrite headroom %d MiB.",
			bf.Chunks, bf.CompBytes>>20, bf.RawBytes>>20, bf.DurationSec, bf.RowsPerSec, bf.LargestTx.DurationMs, c.Rewrite.Headroom>>20)
		w("- WAL: rollup %d MiB plus vacuum %d MiB; peak extra disk %d MiB; final chunk table %d MiB.", bf.WAL.LSNBytes>>20, c.VacuumPhase.LSNBytes>>20, bf.Footprint.PeakExtra>>20, bf.Footprint.FinalChunks>>20)
		for _, k := range c.Criteria {
			if k.Note != "" {
				w("- %s: %s", k.ID, k.Note)
			}
		}
	}
	w("")
	w("Percentiles are nearest-rank; p99 is indicative. Full numbers, settings and per-cell latency are in results.json and settings.json.")
	for _, n := range r.Notes {
		if n != "" {
			w("- %s", n)
		}
	}
	return b.String()
}
