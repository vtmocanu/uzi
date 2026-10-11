package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TupleStat is pgstattuple output for one heap relation.
type TupleStat struct {
	TableLen   int64   `json:"table_len"`
	Tuples     int64   `json:"tuple_count"`
	TupleLen   int64   `json:"tuple_len"`
	DeadTuples int64   `json:"dead_tuple_count"`
	DeadLen    int64   `json:"dead_tuple_len"`
	FreeSpace  int64   `json:"free_space"`
	FreePct    float64 `json:"free_percent"`
}

// IndexStat is pgstatindex output for one btree.
type IndexStat struct {
	Name         string  `json:"name"`
	Size         int64   `json:"index_size"`
	LeafPages    int64   `json:"leaf_pages"`
	DeletedPages int64   `json:"deleted_pages"`
	EmptyPages   int64   `json:"empty_pages"`
	AvgDensity   float64 `json:"avg_leaf_density"`
	Fragment     float64 `json:"leaf_fragmentation"`
}

// RelReport is the size picture of one table at one moment.
type RelReport struct {
	Label     string      `json:"label"`
	Total     int64       `json:"total_relation_size"`
	Heap      TupleStat   `json:"heap"`
	Toast     *TupleStat  `json:"toast,omitempty"`
	ToastSize int64       `json:"toast_relation_size"`
	IndexSize int64       `json:"indexes_size"`
	Indexes   []IndexStat `json:"indexes"`
}

func tupleStat(ctx context.Context, pool *pgxpool.Pool, rel string) (TupleStat, error) {
	var t TupleStat
	err := pool.QueryRow(ctx, `SELECT table_len, tuple_count, tuple_len, dead_tuple_count, dead_tuple_len, free_space, free_percent
		FROM pgstattuple($1::regclass)`, rel).Scan(&t.TableLen, &t.Tuples, &t.TupleLen, &t.DeadTuples, &t.DeadLen, &t.FreeSpace, &t.FreePct)
	return t, err
}

// measureRel reports total size, pgstattuple on the heap and on its TOAST relation separately,
// and pgstatindex for every btree on the table (the partial indexes included).
func measureRel(ctx context.Context, pool *pgxpool.Pool, rel, label string) (RelReport, error) {
	r := RelReport{Label: label}
	var toastOID *string
	if err := pool.QueryRow(ctx, `SELECT pg_total_relation_size($1::regclass), pg_indexes_size($1::regclass),
		(SELECT reltoastrelid::regclass::text FROM pg_class WHERE oid = $1::regclass AND reltoastrelid <> 0)`, rel).Scan(&r.Total, &r.IndexSize, &toastOID); err != nil {
		return r, err
	}
	var err error
	if r.Heap, err = tupleStat(ctx, pool, rel); err != nil {
		return r, fmt.Errorf("pgstattuple heap: %w", err)
	}
	if toastOID != nil {
		ts, err := tupleStat(ctx, pool, *toastOID)
		if err != nil {
			return r, fmt.Errorf("pgstattuple toast: %w", err)
		}
		r.Toast = &ts
		r.ToastSize = ts.TableLen
	}
	rows, err := pool.Query(ctx, `SELECT c.relname, s.index_size, s.leaf_pages, s.deleted_pages, s.empty_pages, s.avg_leaf_density, s.leaf_fragmentation
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid, LATERAL pgstatindex(c.oid::regclass) s
		WHERE i.indrelid = $1::regclass ORDER BY c.relname`, rel)
	if err != nil {
		return r, fmt.Errorf("pgstatindex: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s IndexStat
		if err := rows.Scan(&s.Name, &s.Size, &s.LeafPages, &s.DeletedPages, &s.EmptyPages, &s.AvgDensity, &s.Fragment); err != nil {
			return r, err
		}
		r.Indexes = append(r.Indexes, s)
	}
	return r, rows.Err()
}

// footprint is the combined on-disk size of the message table and the chunk table.
func footprint(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `SELECT pg_total_relation_size('run_messages') + coalesce(pg_total_relation_size('`+chunkTable+`'), 0)`).Scan(&n)
	return n, err
}

// WALStat is the WAL a phase generated. LSNBytes (insert-LSN delta) is authoritative; the
// pg_stat_wal counters are cross-checks and are read only after they stop moving.
type WALStat struct {
	LSNBytes      int64              `json:"lsn_bytes"`
	WALBytes      int64              `json:"pg_stat_wal_bytes"`
	WALFPI        int64              `json:"wal_fpi"`
	WALRecords    int64              `json:"wal_records"`
	StatsSettled  bool               `json:"pg_stat_wal_settled"`
	StatsPolls    int                `json:"pg_stat_wal_polls"`
	Checkpointer  map[string]float64 `json:"checkpointer_delta"`
	DurationSec   float64            `json:"duration_s"`
	WALBytesPerSc float64            `json:"wal_mb_per_s"`
	endLSN        string
}

type walMark struct {
	lsn   string
	t     time.Time
	ckpt  map[string]float64
	taken bool
}

func checkpointerStats(ctx context.Context, pool *pgxpool.Pool) (map[string]float64, error) {
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(c) FROM pg_stat_checkpointer c`).Scan(&raw); err != nil {
		return nil, err
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for k, v := range all {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out, nil
}

// statsSettleTimeout bounds how long endWAL waits for pg_stat_wal to stop moving.
const statsSettleTimeout = 15 * time.Second

// markWAL resets pg_stat_wal and notes the current insert LSN and checkpointer counters.
// Backends report their pending statistics when their connection closes, so the pool is reset
// first: counters a previous phase left unreported in an idle backend can then not land in the
// new window after the reset.
func markWAL(ctx context.Context, pool *pgxpool.Pool) (walMark, error) {
	m := walMark{t: time.Now(), taken: true}
	if err := resetAndWaitGone(ctx, pool); err != nil {
		return m, err
	}
	if _, err := pool.Exec(ctx, `SELECT pg_stat_reset_shared('wal')`); err != nil {
		return m, err
	}
	if err := pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&m.lsn); err != nil {
		return m, err
	}
	var err error
	m.ckpt, err = checkpointerStats(ctx, pool)
	return m, err
}

// pidsGoneTimeout bounds how long markWAL waits for the reset pool's old backends to exit.
const pidsGoneTimeout = 5 * time.Second

// resetAndWaitGone resets the pool and polls pg_stat_activity until the backends it held have
// exited, so their final stats report cannot land after the following pg_stat_reset_shared.
// It stops waiting after pidsGoneTimeout; the window is then a best effort.
func resetAndWaitGone(ctx context.Context, pool *pgxpool.Pool) error {
	var pids []int32
	for _, c := range pool.AcquireAllIdle(ctx) {
		pids = append(pids, int32(c.Conn().PgConn().PID())) //nolint:gosec // G115: a backend pid fits int32.
		c.Release()
	}
	pool.Reset()
	deadline := time.Now().Add(pidsGoneTimeout)
	for len(pids) > 0 && time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = ANY($1)`, pids).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// endWAL reads the counters since mark. The pool is reset so every backend that wrote reports
// its statistics as its connection closes, then pg_stat_wal is polled until two reads 300 ms
// apart agree (StatsSettled), or statsSettleTimeout passes (StatsSettled false: the
// pg_stat_wal figures are then a lower bound). The LSN delta does not depend on any of that.
func endWAL(ctx context.Context, pool *pgxpool.Pool, m walMark) (WALStat, error) {
	var w WALStat
	w.DurationSec = time.Since(m.t).Seconds()
	if err := pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text, pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint`, m.lsn).Scan(&w.endLSN, &w.LSNBytes); err != nil {
		return w, err
	}
	pool.Reset()
	deadline := time.Now().Add(statsSettleTimeout)
	prev := int64(-1)
	for {
		if err := pool.QueryRow(ctx, `SELECT wal_bytes::bigint, wal_fpi, wal_records FROM pg_stat_wal`).Scan(&w.WALBytes, &w.WALFPI, &w.WALRecords); err != nil {
			return w, err
		}
		w.StatsPolls++
		if w.WALBytes == prev {
			w.StatsSettled = true
			break
		}
		prev = w.WALBytes
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return w, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	after, err := checkpointerStats(ctx, pool)
	if err != nil {
		return w, err
	}
	w.Checkpointer = map[string]float64{}
	for k, v := range after {
		if d := v - m.ckpt[k]; d != 0 {
			w.Checkpointer[k] = d
		}
	}
	if w.DurationSec > 0 {
		w.WALBytesPerSc = float64(w.LSNBytes) / w.DurationSec / (1 << 20)
	}
	return w, nil
}

// lsnGap is the WAL between two insert LSNs.
func lsnGap(ctx context.Context, pool *pgxpool.Pool, from, to string) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff($2::pg_lsn, $1::pg_lsn)::bigint`, from, to).Scan(&n)
	return n, err
}

// Fingerprint is the shape of the loaded corpus, so a later run on a real export can be compared.
type Fingerprint struct {
	Label        string                      `json:"corpus"`
	Note         string                      `json:"note"`
	Rows         int64                       `json:"rows"`
	Runs         int                         `json:"runs"`
	TerminalRuns int                         `json:"terminal_runs"`
	LiveRuns     int                         `json:"live_runs"`
	KindMix      map[string]int64            `json:"kind_mix"`
	PayloadBytes map[string]map[string]int64 `json:"payload_bytes_by_kind"`
	HeapBytes    int64                       `json:"heap_bytes"`
	ToastBytes   int64                       `json:"toast_bytes"`
	IndexBytes   int64                       `json:"index_bytes"`
	ToastShare   float64                     `json:"toast_share_of_heap_plus_toast"`
	AvgRowWidth  float64                     `json:"avg_row_width_pg_stats"`
	RunIDCorr    *float64                    `json:"run_id_correlation"`
	LargestMsg   int64                       `json:"largest_message_payload_bytes"`
	LargestTermM int64                       `json:"largest_terminal_run_message_payload_bytes"`
	RunSizes     map[string]int64            `json:"run_message_count_percentiles"`
}

func fingerprint(ctx context.Context, pool *pgxpool.Pool, label, note string, infos []*RunInfo) (Fingerprint, error) {
	f := Fingerprint{Label: label, Note: note, KindMix: map[string]int64{}, PayloadBytes: map[string]map[string]int64{}, RunSizes: map[string]int64{}, Runs: len(infos)}
	for _, in := range infos {
		if in.Terminal {
			f.TerminalRuns++
		} else {
			f.LiveRuns++
		}
	}
	rows, err := pool.Query(ctx, `SELECT kind, count(*),
		percentile_disc(0.5) WITHIN GROUP (ORDER BY octet_length(payload::text)),
		percentile_disc(0.9) WITHIN GROUP (ORDER BY octet_length(payload::text)),
		percentile_disc(0.99) WITHIN GROUP (ORDER BY octet_length(payload::text)),
		max(octet_length(payload::text)) FROM run_messages GROUP BY kind ORDER BY kind`)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n, p50, p90, p99, mx int64
		if err := rows.Scan(&k, &n, &p50, &p90, &p99, &mx); err != nil {
			return f, err
		}
		f.KindMix[k] = n
		f.Rows += n
		f.PayloadBytes[k] = map[string]int64{"p50": p50, "p90": p90, "p99": p99, "max": mx}
		f.LargestMsg = max(f.LargestMsg, mx)
	}
	if err := rows.Err(); err != nil {
		return f, err
	}
	rows.Close()
	if err := pool.QueryRow(ctx, `SELECT pg_relation_size('run_messages'),
		coalesce(pg_relation_size((SELECT reltoastrelid FROM pg_class WHERE oid = 'run_messages'::regclass)), 0),
		pg_indexes_size('run_messages')`).Scan(&f.HeapBytes, &f.ToastBytes, &f.IndexBytes); err != nil {
		return f, err
	}
	if t := f.HeapBytes + f.ToastBytes; t > 0 {
		f.ToastShare = float64(f.ToastBytes) / float64(t)
	}
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(avg_width), 0)::float8 FROM pg_stats WHERE tablename = 'run_messages' AND schemaname = 'public'`).Scan(&f.AvgRowWidth); err != nil {
		return f, err
	}
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(octet_length(m.payload::text)), 0) FROM run_messages m JOIN runs r ON r.id = m.run_id
		WHERE r.status IN ('completed','failed','cancelled')`).Scan(&f.LargestTermM); err != nil {
		return f, err
	}
	var corr *float64
	if err := pool.QueryRow(ctx, `SELECT correlation::float8 FROM pg_stats WHERE tablename = 'run_messages' AND attname = 'run_id' AND schemaname = 'public'`).Scan(&corr); err == nil {
		f.RunIDCorr = corr
	}
	var p50, p90, p99, mx int64
	if err := pool.QueryRow(ctx, `SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY n), percentile_disc(0.9) WITHIN GROUP (ORDER BY n),
		percentile_disc(0.99) WITHIN GROUP (ORDER BY n), max(n) FROM (SELECT count(*) AS n FROM run_messages GROUP BY run_id) s`).Scan(&p50, &p90, &p99, &mx); err != nil {
		return f, err
	}
	f.RunSizes = map[string]int64{"p50": p50, "p90": p90, "p99": p99, "max": mx}
	return f, nil
}

// settingsAll returns SHOW ALL as name -> setting.
func settingsAll(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	rows, err := pool.Query(ctx, `SELECT name, setting FROM pg_settings ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, s string
		if err := rows.Scan(&n, &s); err != nil {
			return nil, err
		}
		out[n] = s
	}
	return out, rows.Err()
}
