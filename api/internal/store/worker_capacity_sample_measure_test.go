package store_test

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// worker_capacity_sample_measure_test.go is the opt-in load measurement for issue #4693. It is NOT
// a LiveDB test (the store-IT sweep must not select it) and asserts nothing: it drives the real
// workersvc.CapacitySampler.Pass against an owned database holding 5,000 workers with three
// slot-holding runs each, and logs "MEASURE key=value" lines. Run it only against a throwaway
// Postgres:
//
//	UZI_CAPACITY_SAMPLE_MEASURE=1 UZI_TEST_DATABASE_URL=... go test -count=1 -v -timeout 30m \
//	  ./internal/store -run '^TestWorkerCapacitySampleLoadMeasurement$'

func TestWorkerCapacitySampleLoadMeasurement(t *testing.T) {
	if os.Getenv("UZI_CAPACITY_SAMPLE_MEASURE") != "1" {
		t.Skip("UZI_CAPACITY_SAMPLE_MEASURE=1 not set; this is an opt-in measurement")
	}
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	const (
		nWorkers    = 5000
		nUsers      = 50
		histBuckets = 7 * 288 // 7 days of five-minute buckets
		yearBuckets = 105120  // 365 days of five-minute buckets
		staleAfter  = 45*time.Second + 500*time.Millisecond
	)

	ctx, dsn := standIsolatedDB(t, "wcsmeas_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	logf := func(format string, a ...any) { t.Logf("MEASURE "+format, a...) }
	exec := func(sql string, args ...any) int64 {
		t.Helper()
		tag, err := pool.Exec(ctx, sql, args...)
		if err != nil {
			t.Fatalf("SQL: %v\n%s", err, sql)
		}
		return tag.RowsAffected()
	}
	scan := func(p *pgxpool.Pool, sql string, dest ...any) {
		t.Helper()
		if err := p.QueryRow(ctx, sql).Scan(dest...); err != nil {
			t.Fatalf("query: %v\n%s", err, sql)
		}
	}
	const binExpr = `date_bin('5 minutes', now(), TIMESTAMPTZ '2000-01-01 00:00:00+00')`

	// ---- environment ----
	var ver, sb, walLevel, maxWal, fsync, fpw, ckpt string
	scan(pool, `SELECT version()`, &ver)
	scan(pool, `SHOW shared_buffers`, &sb)
	scan(pool, `SHOW wal_level`, &walLevel)
	scan(pool, `SHOW max_wal_size`, &maxWal)
	scan(pool, `SHOW fsync`, &fsync)
	scan(pool, `SHOW full_page_writes`, &fpw)
	scan(pool, `SHOW checkpoint_timeout`, &ckpt)
	logf("env pg_version=%q shared_buffers=%s wal_level=%s max_wal_size=%s fsync=%s full_page_writes=%s checkpoint_timeout=%s",
		ver, sb, walLevel, maxWal, fsync, fpw, ckpt)

	// ---- workload: 50 users, 5,000 workers, 3 slot-holding runs per worker ----
	exec(fmt.Sprintf(`CREATE TABLE meas_users AS SELECT g AS n, gen_random_uuid() AS id FROM generate_series(1, %d) g`, nUsers))
	exec(`INSERT INTO users (id, email, password_hash) SELECT id, 'meas-' || n || '@e2e', 'x' FROM meas_users`)
	exec(`CREATE TABLE meas_repos AS SELECT n, id AS user_id, gen_random_uuid() AS conn_id, gen_random_uuid() AS id FROM meas_users`)
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
 SELECT conn_id, user_id, 'github', 'https://forge.e2e/' || n, 'bot', n, '\x01'::bytea FROM meas_repos`)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
 SELECT id, conn_id, n, 'g/meas' || n, 'https://forge.e2e/g/meas' || n, 'main', true FROM meas_repos`)
	exec(fmt.Sprintf(`CREATE TABLE meas_fleet AS SELECT i, gen_random_uuid() AS id, (i %% %d) + 1 AS un FROM generate_series(1, %d) i`, nUsers, nWorkers))
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, max_cross_check_slots)
 SELECT f.id, u.id, f.id::text, uuid_send(f.id), 'online', now() + interval '2 hours', 4, 1
 FROM meas_fleet f JOIN meas_users u ON u.n = f.un`)
	// one completed target run per user, for the cross-check runs
	exec(`INSERT INTO runs (id, user_id, kind, status, issue_title, issue_description, claim_generation, repo_id, issue_iid)
 SELECT gen_random_uuid(), r.user_id, 'issue', 'completed', 'meas', 'meas', 1, r.id, 1000000 + r.n FROM meas_repos r`)
	exec(`CREATE TABLE meas_targets AS SELECT DISTINCT ON (user_id) user_id, id FROM runs WHERE status = 'completed' ORDER BY user_id`)
	const runCols = `id, user_id, kind, status, worker_id, issue_title, issue_description, claim_generation`
	const runSel = `gen_random_uuid(), u.id, %s, %s, f.id, 'meas', 'meas', 1`
	join := `FROM meas_fleet f JOIN meas_users u ON u.n = f.un JOIN meas_repos r ON r.user_id = u.id`
	// 1: ordinary issue run for every worker
	exec(`INSERT INTO runs (` + runCols + `, repo_id, issue_iid) SELECT ` + fmt.Sprintf(runSel, `'issue'`, `'running'`) + `, r.id, f.i ` + join)
	// 2: chat on every tenth worker, otherwise a prompt run
	exec(`INSERT INTO runs (` + runCols + `) SELECT ` + fmt.Sprintf(runSel, `'chat'`, `'running'`) + ` ` + join + ` WHERE f.i % 10 = 0`)
	exec(`INSERT INTO runs (` + runCols + `, repo_id) SELECT ` + fmt.Sprintf(runSel, `'prompt'`, `'running'`) + `, r.id ` + join + ` WHERE f.i % 10 <> 0`)
	// 3: a cross-check run on the durable lane for every seventh worker, otherwise a task run
	exec(`INSERT INTO runs (` + runCols + `, repo_id, target_run_id, harness, report_only, budget_wall_seconds)
 SELECT ` + fmt.Sprintf(runSel, `'cross_check'`, `'claimed'`) + `, r.id, t.id, 'codex', true, 600 ` + join + `
 JOIN meas_targets t ON t.user_id = u.id WHERE f.i % 7 = 0`)
	exec(`UPDATE runs SET cross_check_lane = true WHERE kind = 'cross_check' AND status = 'claimed'`)
	exec(`INSERT INTO runs (` + runCols + `, repo_id, branch) SELECT ` + fmt.Sprintf(runSel, `'task'`, `'running'`) + `, r.id, 'uzi/task/' || f.i ` + join + ` WHERE f.i % 7 <> 0`)
	var nRuns, nLane, nChat int
	scan(pool, `SELECT count(*) FILTER (WHERE worker_id IS NOT NULL), count(*) FILTER (WHERE cross_check_lane), count(*) FILTER (WHERE kind='chat') FROM runs`, &nRuns, &nLane, &nChat)
	logf("workload workers=%d users=%d slot_runs=%d lane_runs=%d chat_runs=%d", nWorkers, nUsers, nRuns, nLane, nChat)
	if nRuns != 3*nWorkers {
		t.Fatalf("workload has %d worker-bound runs, want %d", nRuns, 3*nWorkers)
	}

	// ---- synthetic history: 7 days x 5,000 workers, inserted chronologically (bucket-major) ----
	var b0 time.Time
	scan(pool, `SELECT `+binExpr, &b0)
	start := time.Now()
	for hi := histBuckets; hi >= 1; hi -= 288 {
		lo := hi - 287
		if lo < 1 {
			lo = 1
		}
		exec(`INSERT INTO worker_capacity_samples (worker_id, bucket_start, user_id, observed_at, freshness, last_heartbeat_at,
 worker_kind, ephemeral, isolated_lane, max_concurrent_runs, max_cross_check_slots, attribution, ordinary_active, cross_check_lane_active, chat_active)
 SELECT w.id, $1::timestamptz - k * interval '5 minutes', w.user_id, $1::timestamptz - k * interval '5 minutes' + interval '90 seconds',
 'online', $1::timestamptz - k * interval '5 minutes' + interval '85 seconds', w.kind, false, false, 4, 1, 'owner', 2, 1, 0
 FROM generate_series($2::int, $3::int) k CROSS JOIN workers w ORDER BY k DESC, w.id`, b0, lo, hi)
	}
	logf("history_insert rows=%d seconds=%.1f", histBuckets*nWorkers, time.Since(start).Seconds())
	exec(`VACUUM (ANALYZE) worker_capacity_samples`)
	exec(`VACUUM (ANALYZE) runs`)
	exec(`ANALYZE workers`)

	remaining := func() time.Duration {
		var s float64
		scan(pool, `SELECT extract(epoch FROM (`+binExpr+` + interval '5 minutes' - clock_timestamp()))`, &s)
		return time.Duration(s * float64(time.Second))
	}
	waitSafe := func(min time.Duration) {
		for {
			r := remaining()
			if r >= min {
				return
			}
			time.Sleep(r + 1500*time.Millisecond)
		}
	}
	currentBucket := func() time.Time {
		var b time.Time
		scan(pool, `SELECT `+binExpr, &b)
		return b
	}
	sampler, err := workersvc.NewCapacitySampler(pool, staleAfter)
	if err != nil {
		t.Fatal(err)
	}
	pass := func() (time.Duration, int64) {
		t0 := time.Now()
		n, err := sampler.Pass(ctx)
		d := time.Since(t0)
		if err != nil {
			t.Fatalf("pass: %v", err)
		}
		return d, n
	}
	pct := func(ds []time.Duration, p float64) time.Duration {
		s := append([]time.Duration(nil), ds...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[int(math.Ceil(p*float64(len(s))))-1]
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

	// ---- passes: 2 to cover the fleet in one bucket, then 20 steady-state ----
	waitSafe(150 * time.Second)
	passBucket := currentBucket()
	var walls []time.Duration
	var coverWalls, steadyWalls []time.Duration
	var totalInserted int64
	for i := 1; i <= 22; i++ {
		d, n := pass()
		walls = append(walls, d)
		if i <= 2 {
			coverWalls = append(coverWalls, d)
		} else {
			steadyWalls = append(steadyWalls, d)
		}
		totalInserted += n
		logf("pass n=%d wall_ms=%.1f inserted=%d", i, ms(d), n)
	}
	if cb := currentBucket(); !cb.Equal(passBucket) {
		logf("WARN bucket rolled during the passes (%s -> %s); pass inserted counts after the roll are a second bucket", passBucket, cb)
	}
	logf("passes count=%d inserted_total=%d wall_max_ms=%.1f wall_p95_ms=%.1f wall_p50_ms=%.1f cover_max_ms=%.1f steady_max_ms=%.1f steady_p95_ms=%.1f",
		len(walls), totalInserted, ms(pct(walls, 1)), ms(pct(walls, 0.95)), ms(pct(walls, 0.5)),
		ms(pct(coverWalls, 1)), ms(pct(steadyWalls, 1)), ms(pct(steadyWalls, 0.95)))
	var ownerRows, unavailRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE attribution='owner'), count(*) FILTER (WHERE attribution='unavailable')
 FROM worker_capacity_samples WHERE bucket_start = $1`, passBucket).Scan(&ownerRows, &unavailRows); err != nil {
		t.Fatal(err)
	}
	logf("pass_bucket_rows attribution_owner=%d attribution_unavailable=%d", ownerRows, unavailRows)

	// ---- bytes per row ----
	exec(`VACUUM (ANALYZE) worker_capacity_samples`)
	var rows, heap, idxTotal, total int64
	scan(pool, `SELECT (SELECT count(*) FROM worker_capacity_samples), pg_relation_size('worker_capacity_samples'),
 pg_indexes_size('worker_capacity_samples'), pg_total_relation_size('worker_capacity_samples')`, &rows, &heap, &idxTotal, &total)
	logf("size rows=%d heap_bytes=%d indexes_bytes=%d total_bytes=%d toast_and_other_bytes=%d", rows, heap, idxTotal, total, total-heap-idxTotal)
	logf("size heap_bytes_per_row=%.2f indexes_bytes_per_row=%.2f total_bytes_per_row=%.2f",
		float64(heap)/float64(rows), float64(idxTotal)/float64(rows), float64(total)/float64(rows))
	irows, err := pool.Query(ctx, `SELECT c.relname, pg_relation_size(c.oid) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
 WHERE i.indrelid = 'worker_capacity_samples'::regclass ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	for irows.Next() {
		var name string
		var sz int64
		if err := irows.Scan(&name, &sz); err != nil {
			t.Fatal(err)
		}
		logf("size index=%s bytes=%d bytes_per_row=%.2f", name, sz, float64(sz)/float64(rows))
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		t.Fatal(err)
	}
	bytesPerRow := float64(total) / float64(rows)
	perWorkerYear := bytesPerRow * yearBuckets
	logf("projection bytes_per_row=%.2f rows_per_worker_year=%d per_worker_year_bytes=%.0f per_worker_year_mib=%.2f", bytesPerRow, yearBuckets, perWorkerYear, perWorkerYear/(1<<20))
	for _, n := range []int{50, 500} {
		logf("projection workers=%d per_year_bytes=%.0f per_year_mib=%.1f per_year_gib=%.3f", n, perWorkerYear*float64(n), perWorkerYear*float64(n)/(1<<20), perWorkerYear*float64(n)/(1<<30))
	}

	// ---- anti-join with most workers sampled: delete ~100 current-bucket rows, EXPLAIN ANALYZE, roll back ----
	waitSafe(60 * time.Second)
	for { // make sure the fleet is sampled in the (possibly new) current bucket
		if _, n := pass(); n == 0 {
			break
		}
	}
	src, err := os.ReadFile("worker_capacity_samples.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)const sampleWorkerCapacityBatch = `(.*?)`").FindSubmatch(src)
	if m == nil {
		t.Fatal("generated query constant not found")
	}
	stmt := strings.TrimSpace(string(m[1]))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cb := currentBucket()
	del, err := tx.Exec(ctx, `DELETE FROM worker_capacity_samples WHERE bucket_start = $1 AND worker_id IN
 (SELECT worker_id FROM worker_capacity_samples WHERE bucket_start = $1 ORDER BY worker_id LIMIT 100)`, cb)
	if err != nil {
		t.Fatal(err)
	}
	prows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+stmt,
		pgtype.Timestamptz{}, pgtype.Interval{Microseconds: staleAfter.Microseconds(), Valid: true}, int32(1000))
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for prows.Next() {
		var line string
		if err := prows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	logf("antijoin deleted_current_bucket_rows=%d workers=%d batch_size=1000 stale_ms=%d", del.RowsAffected(), nWorkers, staleAfter.Milliseconds())
	for _, l := range plan {
		t.Logf("MEASURE_PLAN %s", l)
	}
	if em := regexp.MustCompile(`Execution Time: ([0-9.]+) ms`).FindStringSubmatch(strings.Join(plan, "\n")); em != nil {
		logf("antijoin execution_ms=%s", em[1])
	}

	// ---- WAL per batch: remove the current bucket for the fleet, then let Pass re-insert it ----
	lsn := func() string {
		var s string
		scan(pool, `SELECT pg_current_wal_lsn()::text`, &s)
		return s
	}
	walSince := func(from string) int64 {
		var d float64
		if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)`, from).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return int64(d)
	}
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		logf("note CHECKPOINT not permitted or failed: %v", err)
	} else {
		logf("note CHECKPOINT executed before the WAL measurements")
	}
	for rep := 1; rep <= 2; rep++ {
		waitSafe(60 * time.Second)
		exec(`DELETE FROM worker_capacity_samples WHERE bucket_start >= ` + binExpr)
		before := lsn()
		var ins int64
		var batches int64
		for {
			_, n := pass()
			ins += n
			batches += (n + 999) / 1000
			if n == 0 {
				break
			}
		}
		w := walSince(before)
		logf("wal rep=%d (rep1 right after CHECKPOINT, rep2 steady) inserted=%d full_batches=%d wal_bytes=%d wal_bytes_per_batch=%.0f wal_bytes_per_row=%.1f",
			rep, ins, batches, w, float64(w)/float64(maxInt64(batches, 1)), float64(w)/float64(maxInt64(ins, 1)))
	}

	// ---- owner delete: one user, one worker, 105,120 sample rows at distinct buckets ----
	exec(`CREATE TABLE meas_victim AS SELECT gen_random_uuid() AS uid, gen_random_uuid() AS wid`)
	exec(`INSERT INTO users (id, email, password_hash) SELECT uid, 'meas-victim@e2e', 'x' FROM meas_victim`)
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, max_cross_check_slots)
 SELECT wid, uid, 'victim', uuid_send(wid), 'online', now(), 4, 1 FROM meas_victim`)
	exec(`INSERT INTO worker_capacity_samples (worker_id, bucket_start, user_id, observed_at, freshness, last_heartbeat_at,
 worker_kind, ephemeral, isolated_lane, max_concurrent_runs, max_cross_check_slots, attribution, ordinary_active, cross_check_lane_active, chat_active)
 SELECT v.wid, $1::timestamptz - k * interval '5 minutes', v.uid, $1::timestamptz - k * interval '5 minutes' + interval '90 seconds',
 'online', $1::timestamptz - k * interval '5 minutes' + interval '85 seconds', w.kind, false, false, 4, 1, 'owner', 2, 1, 0
 FROM meas_victim v JOIN workers w ON w.id = v.wid CROSS JOIN generate_series(1, $2::int) k`, b0.Add(-time.Duration(histBuckets)*5*time.Minute), yearBuckets)
	exec(`VACUUM (ANALYZE) worker_capacity_samples`)
	var victimRows int64
	scan(pool, `SELECT count(*) FROM worker_capacity_samples s JOIN meas_victim v ON v.uid = s.user_id`, &victimRows)
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		logf("note CHECKPOINT before owner delete not permitted or failed: %v", err)
	} else {
		logf("note CHECKPOINT executed before the owner delete")
	}
	before := lsn()
	t0 := time.Now()
	del2, err := pool.Exec(ctx, `DELETE FROM users WHERE id = (SELECT uid FROM meas_victim)`)
	dur := time.Since(t0)
	if err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	w := walSince(before)
	var left int64
	scan(pool, `SELECT count(*) FROM worker_capacity_samples s JOIN meas_victim v ON v.uid = s.user_id`, &left)
	logf("owner_delete users_deleted=%d sample_rows_before=%d sample_rows_after=%d wall_ms=%.1f wal_bytes=%d table_rows_total=%d",
		del2.RowsAffected(), victimRows, left, ms(dur), w, rows)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
