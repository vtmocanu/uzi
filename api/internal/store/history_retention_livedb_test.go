package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// history_retention_livedb_test.go drives the session half of the history_retention pass (issue
// #4695) against PostgreSQL: the pinned lock, the cutoff statement and the four batch deletes.
// Each top-level test stands ONE isolated database (newHrFx, which uses newWcsFx), because a delete
// test must never see another top-level test's rows; its subtests share that database and each
// starts from emptied history tables (hrFx.run). Standing a database (create plus migrate) costs
// about four seconds, so one per table or per case dominated the package's LiveDB time. The tests
// do not use t.Parallel: the surrounding store LiveDB tests do not, and lockHolders counts only the
// lock of the fixture's own database (two-int form, current database), so another database's
// holder of the same key cannot confuse it, but two top-level tests in one database would.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run
// via ./e2e/run-store-it.sh. The session tests live in package store_test (not store) so they
// can use the isolated-database helpers; the unexported seams come from
// history_retention_export_test.go.

var hrTables = store.HistoryRetentionTables()

// hrExpiryColumn is the column each table expires on.
func hrExpiryColumn(table store.HistoryRetentionTable) string {
	if table == store.HistoryRetentionPRHistoryEvents {
		return "observed_at"
	}
	return "bucket_start"
}

// hrBase is a bucket-aligned instant (day-aligned, so aligned to the five-minute bin origin).
var hrBase = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

type hrFx struct {
	*wcsFx
	owner uuid.UUID
	repoQ uuid.UUID
	iid   int64
}

func newHrFx(t *testing.T) *hrFx {
	t.Helper()
	f := &hrFx{wcsFx: newWcsFx(t)}
	f.owner = f.user()
	f.repoQ = f.repo(f.owner)
	return f
}

// seed inserts n rows of table whose expiry column equals at. PR events are ordinary
// (first_merge = false).
func (f *hrFx) seed(table store.HistoryRetentionTable, n int, at time.Time) {
	f.t.Helper()
	switch table {
	case store.HistoryRetentionPRHistoryEvents:
		f.exec(`INSERT INTO pr_history_events (repo_id, mr_iid, to_state, time_source, observed_at, bootstrap, first_merge)
		        SELECT $1, $2::bigint + g, 'opened', 'observation_fallback', $3, true, false FROM generate_series(1, $4::int) g`,
			f.repoQ, f.nextIID(n), at, n)
	case store.HistoryRetentionAnthropicUsage:
		f.exec(`INSERT INTO anthropic_usage_samples (user_id, user_secret_id, observed_at, enablement_rev, source, seven_day_pct)
		        SELECT $1, gen_random_uuid(), $2, 1, 'usage_endpoint', 10 FROM generate_series(1, $3::int)`,
			f.owner, at, n)
	case store.HistoryRetentionCodexUsage:
		f.exec(`INSERT INTO codex_usage_samples (user_id, provider_account_id, observed_at, observed_generation, observed_credential_revision, buckets)
		        SELECT $1, gen_random_uuid(), $2, 1, 1, '[]'::jsonb FROM generate_series(1, $3::int)`,
			f.owner, at, n)
	case store.HistoryRetentionWorkerCapacitySample:
		f.exec(`INSERT INTO worker_capacity_samples (worker_id, bucket_start, user_id, observed_at, freshness, worker_kind, ephemeral, isolated_lane, attribution)
		        SELECT gen_random_uuid(), $2, $1, $2, 'online', 'external', false, false, 'unavailable' FROM generate_series(1, $3::int)`,
			f.owner, at, n)
	default:
		f.t.Fatalf("seed: unknown table %q", table)
	}
}

// seedFirstMerge inserts n first_merge events at at (each its own PR).
func (f *hrFx) seedFirstMerge(n int, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO pr_history_events (repo_id, mr_iid, to_state, time_source, observed_at, bootstrap, first_merge)
	        SELECT $1, $2::bigint + g, 'merged', 'observation_fallback', $3, true, true FROM generate_series(1, $4::int) g`,
		f.repoQ, f.nextIID(n), at, n)
}

// sub returns a view of the fixture bound to the subtest t, so fixture failures and cleanups
// belong to that subtest while the database, pool and rows stay shared. The per-subtest copies of
// iid and wcsFx.n are safe because the history tables are truncated per subtest and no HR subtest
// calls wcsFx.run.
func (f *hrFx) sub(t *testing.T) *hrFx {
	g := *f
	w := *f.wcsFx
	w.t = t
	g.wcsFx = &w
	return &g
}

// reset empties every table the retention pass reads or protects, so a subtest never sees another
// subtest's rows.
func (f *hrFx) reset() {
	f.t.Helper()
	f.exec(`TRUNCATE pr_history_events, pr_history_heads, anthropic_usage_samples, codex_usage_samples, worker_capacity_samples`)
}

// run runs fn as a subtest on a reset, subtest-bound view of the shared fixture.
func (f *hrFx) run(t *testing.T, name string, fn func(t *testing.T, f *hrFx)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		g := f.sub(t)
		g.reset()
		fn(t, g)
	})
}

func (f *hrFx) nextIID(n int) int64 {
	base := f.iid
	f.iid += int64(n)
	return base
}

func (f *hrFx) total(table store.HistoryRetentionTable) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM ` + string(table))
}

func (f *hrFx) countAt(table store.HistoryRetentionTable, at time.Time) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM `+string(table)+` WHERE `+hrExpiryColumn(table)+` = $1`, at)
}

// open opens the production session (statement and lock timeouts as given) and closes it on
// cleanup.
func (f *hrFx) open(stmt, lock time.Duration) *store.HistoryRetentionSession {
	f.t.Helper()
	s, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, f.pool, stmt, lock)
	if err != nil || !got {
		f.t.Fatalf("open session: got=%v err=%v, want the lock", got, err)
	}
	f.t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	return s
}

// lockRelation holds ACCESS EXCLUSIVE on table in a transaction on its own pool connection until
// release is called (or the test ends).
func (f *hrFx) lockRelation(table store.HistoryRetentionTable) (release func()) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	f.t.Cleanup(release)
	if _, err := tx.Exec(f.ctx, `LOCK TABLE `+string(table)+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		f.t.Fatal(err)
	}
	return release
}

// lockHolders counts backends holding the history retention advisory lock in the fixture database:
// the two-int form (objsubid 2) of (HistoryRetentionLockClass, 0), scoped to the current database.
func (f *hrFx) lockHolders() int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objid = 0 AND classid = $1::int8::oid
	        AND objsubid = 2 AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		int64(store.HistoryRetentionLockClass))
}

// heads snapshots pr_history_heads by whole row (every column, as jsonb) plus the row version
// (xmin), one string per row in key order, so a test can tell a changed, nulled, same-value
// rewritten or replaced row from an untouched one (a row count cannot).
func (f *hrFx) heads() []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT to_jsonb(h)::text || ' xmin=' || h.xmin::text
	        FROM pr_history_heads h ORDER BY repo_id, mr_iid`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// seedHeads inserts n pr_history_heads rows with a non-default value in every nullable or
// defaulted column the CHECK constraints allow, so a rewrite that nulls or resets any column is
// visible in heads().
func (f *hrFx) seedHeads(n int, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO pr_history_heads (repo_id, mr_iid, state, observed_at, last_provider_at, first_merged_at,
	            first_merged_source, last_changed, last_from_state, last_to_state, last_event_provider_at,
	            last_time_source, last_bootstrap, last_first_merge, created_at, updated_at)
	        SELECT $1, $2::bigint + g, 'merged', $3::timestamptz, $3 - interval '1 minute', $3 - interval '2 minutes',
	            'provider', true, 'opened', 'merged', $3 - interval '3 minutes',
	            'provider', true, true, $3 - interval '1 hour', $3 - interval '30 minutes'
	        FROM generate_series(1, $4::int) g`, f.repoQ, f.nextIID(n), at, n)
}

// hrHeadsEqual reports the first difference between two heads snapshots ("" when equal).
func hrHeadsEqual(a, b []string) string {
	if len(a) != len(b) {
		return fmt.Sprintf("row count %d -> %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("row %d: %q -> %q", i, a[i], b[i])
		}
	}
	return ""
}

func (f *hrFx) pidAlive(pid uint32) bool {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, int64(pid)) > 0
}

func hrMustDelete(t *testing.T, s *store.HistoryRetentionSession, table store.HistoryRetentionTable, cutoff time.Time, limit int32, want int64) {
	t.Helper()
	n, err := s.DeleteBatch(context.Background(), table, cutoff, limit)
	if err != nil || n != want {
		t.Fatalf("DeleteBatch(%s, %v, %d) = (%d, %v), want (%d, nil)", table, cutoff, limit, n, err, want)
	}
}

// TestHistoryRetentionDeleteLiveDB drives the delete half of the session: the strict boundaries,
// the exact cutoff, protected rows, batching, row-lock skipping, the server-side timeouts, the
// abandoned-statement path, the refusals and the plan shape. One database, one subtest per case.
func TestHistoryRetentionDeleteLiveDB(t *testing.T) {
	root := newHrFx(t)

	root.run(t, "boundary pr_history_events", func(t *testing.T, f *hrFx) {
		s := f.open(time.Second, time.Second)
		c := hrBase.Add(36*time.Hour + 123456*time.Microsecond)
		for _, at := range []time.Time{c.Add(-time.Microsecond), c, c.Add(time.Microsecond)} {
			f.seed(store.HistoryRetentionPRHistoryEvents, 1, at)
		}
		hrMustDelete(t, s, store.HistoryRetentionPRHistoryEvents, c, 1000, 1)
		if f.countAt(store.HistoryRetentionPRHistoryEvents, c.Add(-time.Microsecond)) != 0 {
			t.Error("the row 1µs before the cutoff was kept")
		}
		if f.countAt(store.HistoryRetentionPRHistoryEvents, c) != 1 {
			t.Error("the row exactly at the cutoff was deleted (the comparison must be strict)")
		}
		if f.countAt(store.HistoryRetentionPRHistoryEvents, c.Add(time.Microsecond)) != 1 {
			t.Error("the row 1µs after the cutoff was deleted")
		}
		hrMustDelete(t, s, store.HistoryRetentionPRHistoryEvents, c, 1000, 0)
	})

	for _, table := range hrTables[1:] {
		root.run(t, "boundary "+string(table), func(t *testing.T, f *hrFx) {
			s := f.open(time.Second, time.Second)
			b := hrBase.Add(10 * time.Hour) // a bucket boundary
			for _, at := range []time.Time{b.Add(-5 * time.Minute), b, b.Add(5 * time.Minute)} {
				f.seed(table, 1, at)
			}
			hrMustDelete(t, s, table, b, 1000, 1)
			if f.countAt(table, b.Add(-5*time.Minute)) != 0 || f.countAt(table, b) != 1 || f.countAt(table, b.Add(5*time.Minute)) != 1 {
				t.Fatalf("after cutoff B: want only B-5m deleted; total=%d", f.total(table))
			}
			hrMustDelete(t, s, table, b.Add(time.Microsecond), 1000, 1)
			if f.countAt(table, b) != 0 || f.countAt(table, b.Add(5*time.Minute)) != 1 {
				t.Fatalf("after cutoff B+1µs: want the row at B deleted and B+5m kept; total=%d", f.total(table))
			}
		})
	}

	root.run(t, "cutoff exact", func(t *testing.T, f *hrFx) {
		// The session runs on a probe pool whose socket delays traffic by d while the cutoff
		// statement is in flight (armed at its trace start, disarmed at its end): writes sleep before
		// reaching the server, reads sleep before returning to the client. A reference taken on the
		// database clock at execution therefore lies at least d after the client's "before" reading
		// and at least d before its "after" reading; a reference taken from the Go clock at call
		// time or at return time cannot satisfy both.
		const d = 300 * time.Millisecond
		p := hrDelayPool(t, f, d)
		s, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, p, time.Second, time.Second)
		if err != nil || !got {
			t.Fatalf("open session on the delay pool: got=%v err=%v", got, err)
		}
		t.Cleanup(func() { _, _ = s.Close(context.Background()) })
		retention := 720*time.Hour + 1234567*time.Microsecond
		before := time.Now()
		ref, cut, err := s.Cutoff(f.ctx, retention)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		if got := ref.Sub(cut); got != retention {
			t.Errorf("reference - cutoff = %v, want exactly %v", got, retention)
		}
		// The reference is the database clock; the test and the database share the host clock.
		// The tolerance (d/4) absorbs any client/server clock offset; a Go-clock reference misses by >= 3d/4.
		eps := d / 4
		if ref.Before(before.Add(d-eps)) || ref.After(after.Add(-d+eps)) {
			t.Errorf("reference %v is not inside [%v, %v] (client readings %v and %v, delay %v each way)",
				ref, before.Add(d-eps), after.Add(-d+eps), before, after, d)
		}
		for _, bad := range []time.Duration{0, -time.Hour} {
			if _, _, err := s.Cutoff(f.ctx, bad); err == nil {
				t.Errorf("Cutoff(%v) succeeded, want a refusal", bad)
			}
		}
	})

	root.run(t, "protected rows", func(t *testing.T, f *hrFx) {
		s := f.open(time.Second, time.Second)
		old := hrBase
		f.seed(store.HistoryRetentionPRHistoryEvents, 4, old)
		f.seedFirstMerge(3, old)
		f.seedHeads(5, old)
		headsBefore := f.heads()
		if len(headsBefore) != 5 {
			t.Fatalf("seeded %d heads, want 5", len(headsBefore))
		}
		hrMustDelete(t, s, store.HistoryRetentionPRHistoryEvents, old.Add(24*time.Hour), 1000, 4)
		if diff := hrHeadsEqual(headsBefore, f.heads()); diff != "" {
			t.Errorf("pr_history_heads changed by the delete: %s", diff)
		}
		if got := f.count(`SELECT count(*) FROM pr_history_events WHERE first_merge`); got != 3 {
			t.Errorf("first_merge events = %d, want all 3 kept", got)
		}
		if got := f.total(store.HistoryRetentionPRHistoryEvents); got != 3 {
			t.Errorf("pr_history_events rows = %d, want only the 3 protected ones", got)
		}
	})

	for _, table := range hrTables {
		root.run(t, "batching "+string(table), func(t *testing.T, f *hrFx) {
			s := f.open(5*time.Second, time.Second)
			f.seed(table, 1500, hrBase)
			cutoff := hrBase.Add(time.Hour)
			hrMustDelete(t, s, table, cutoff, 1000, 1000)
			if got := f.total(table); got != 500 {
				t.Fatalf("after the first batch %d rows remain, want 500", got)
			}
			hrMustDelete(t, s, table, cutoff, 1000, 500)
			hrMustDelete(t, s, table, cutoff, 1000, 0)
		})
	}

	// The inner SELECT orders by the expiry column (issue #4695), so a batch removes the oldest
	// rows first. The groups are inserted newest first, so heap order is the reverse of age: a
	// statement without the ORDER BY would take the newest group.
	for _, table := range hrTables {
		root.run(t, "oldest first "+string(table), func(t *testing.T, f *hrFx) {
			s := f.open(5*time.Second, time.Second)
			at := []time.Time{hrBase.Add(2 * time.Hour), hrBase.Add(time.Hour), hrBase}
			for _, a := range at {
				f.seed(table, 300, a)
			}
			cutoff := hrBase.Add(3 * time.Hour)
			hrMustDelete(t, s, table, cutoff, 300, 300)
			for i, want := range []int{300, 300, 0} {
				if got := f.countAt(table, at[i]); got != want {
					t.Errorf("after the first batch %d rows remain at %v, want %d (the oldest group goes first)", got, at[i], want)
				}
			}
			hrMustDelete(t, s, table, cutoff, 300, 300)
			if got := f.countAt(table, at[1]); got != 0 {
				t.Errorf("after the second batch %d rows remain at %v, want 0", got, at[1])
			}
		})
	}

	for _, table := range hrTables {
		root.run(t, "row lock skip "+string(table), func(t *testing.T, f *hrFx) {
			s := f.open(5*time.Second, 5*time.Second)
			f.seed(table, 10, hrBase)
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
			if _, err := tx.Exec(f.ctx, `SELECT 1 FROM `+string(table)+` WHERE ctid IN (SELECT ctid FROM `+string(table)+` LIMIT 3) FOR UPDATE`); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			hrMustDelete(t, s, table, hrBase.Add(time.Hour), 1000, 7)
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("batch took %v: it waited on the locked rows instead of skipping them", d)
			}
			if err := tx.Rollback(f.ctx); err != nil {
				t.Fatal(err)
			}
			hrMustDelete(t, s, table, hrBase.Add(time.Hour), 1000, 3)
		})
	}

	root.run(t, "relation lock", func(t *testing.T, f *hrFx) {
		s := f.open(300*time.Millisecond, 200*time.Millisecond)
		for _, table := range hrTables {
			f.seed(table, 5, hrBase)
		}
		locked := store.HistoryRetentionPRHistoryEvents
		release := f.lockRelation(locked)
		cutoff := hrBase.Add(time.Hour)

		start := time.Now()
		n, err := s.DeleteBatch(f.ctx, locked, cutoff, 1000)
		t.Logf("delete on a relation held ACCESS EXCLUSIVE: returned after %v (lock_timeout 200ms): n=%d err=%v", time.Since(start), n, err)
		var rb *store.HistoryRetentionRollbackError
		if !errors.As(err, &rb) || rb.Code != "55P03" || rb.StatementTimeout() || n != 0 {
			t.Fatalf("delete on the locked table = (%d, %v), want a known 55P03 rollback with 0 rows", n, err)
		}
		if !s.Idle() {
			t.Fatal("session is not idle after a known rollback")
		}
		for _, other := range hrTables[1:] {
			hrMustDelete(t, s, other, cutoff, 1000, 5)
		}
		release()
		if got := f.total(locked); got != 5 {
			t.Fatalf("%s count after release = %d, want the 5 rows unchanged", locked, got)
		}
		hrMustDelete(t, s, locked, cutoff, 1000, 5)
	})

	root.run(t, "server timeout", func(t *testing.T, f *hrFx) {
		// statement_timeout fires first: the lock wait is far longer than the statement bound.
		s := f.open(150*time.Millisecond, 10*time.Second)
		table := store.HistoryRetentionAnthropicUsage
		f.seed(table, 5, hrBase)
		release := f.lockRelation(table)
		start := time.Now()
		n, err := s.DeleteBatch(f.ctx, table, hrBase.Add(time.Hour), 1000)
		t.Logf("statement_timeout 150ms against a held relation lock: returned after %v", time.Since(start))
		var rb *store.HistoryRetentionRollbackError
		if !errors.As(err, &rb) || rb.Code != "57014" || !rb.StatementTimeout() || n != 0 {
			t.Fatalf("delete = (%d, %v), want a known 57014 rollback with 0 rows", n, err)
		}
		if !s.Idle() {
			t.Fatal("session is not idle after a statement timeout")
		}
		release()
		if got := f.total(table); got != 5 {
			t.Fatalf("count after the timeout = %d, want 5 unchanged", got)
		}
	})

	root.run(t, "cancel and reacquire", func(t *testing.T, f *hrFx) {
		table := store.HistoryRetentionPRHistoryEvents
		f.seed(table, 5, hrBase)
		s := f.open(5*time.Second, 5*time.Second)
		pid := s.PIDForTest()
		release := f.lockRelation(table)

		workCtx, cancelWork := context.WithCancel(f.ctx)
		defer cancelWork()
		type res struct {
			n   int64
			err error
			at  time.Duration
		}
		started := time.Now()
		done := make(chan res, 1)
		// Registered after f.open and lockRelation, so it runs BEFORE the session's own cleanup
		// Close even when an assertion below fails: the goroutine is cancelled and has returned
		// before anything else touches the shared session.
		var wg sync.WaitGroup
		t.Cleanup(func() {
			cancelWork()
			wg.Wait()
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.DeleteBatch(workCtx, table, hrBase.Add(time.Hour), 1000)
			done <- res{n, err, time.Since(started)}
		}()
		// Cancel only once the server shows the delete waiting on the relation lock; a 20s watchdog
		// bounds both the gate and the wait for the result.
		watchdog := time.NewTimer(20 * time.Second)
		defer watchdog.Stop()
		for f.count(`SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock' AND wait_event = 'relation'`, int64(pid)) != 1 {
			select {
			case r := <-done:
				t.Fatalf("DeleteBatch returned (%d, %v) before wait_event_type = Lock was observed", r.n, r.err)
			case <-watchdog.C:
				t.Fatal("watchdog: the delete never showed wait_event_type = Lock in pg_stat_activity")
			case <-time.After(10 * time.Millisecond):
			}
		}
		cancelWork()
		var r res
		select {
		case r = <-done:
		case <-watchdog.C:
			t.Fatal("watchdog: DeleteBatch did not return after the work context was cancelled")
		}
		t.Logf("work context cancelled: DeleteBatch returned after %v", r.at)
		var rb *store.HistoryRetentionRollbackError
		if r.err == nil || errors.As(r.err, &rb) || r.n != 0 {
			t.Fatalf("DeleteBatch = (%d, %v), want an UNKNOWN outcome (not a known rollback)", r.n, r.err)
		}
		if !strings.Contains(r.err.Error(), string(table)) {
			t.Errorf("unknown-outcome error %q does not name the table %s", r.err, table)
		}

		cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, time.Second)
		defer cancelCleanup()
		closeStart := time.Now()
		unlocked, cerr := s.Close(cleanupCtx)
		t.Logf("production Close(cleanupCtx) returned after %v (unlocked=%v err=%v)", time.Since(closeStart), unlocked, cerr)
		if unlocked {
			t.Error("Close claims a proven unlock on a session whose statement was abandoned")
		}

		exitStart := time.Now()
		for f.pidAlive(pid) {
			if time.Since(exitStart) > 3*time.Second {
				t.Fatalf("backend %d still alive 3s after the work context was cancelled and Close ran (idle_session_timeout is 5s, so only the client side can end it: when the context is cancelled pgx itself sends the cancel request and the terminate, and Close then finds a closed connection and does nothing)", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Logf("backend pid %d exited %v after Close", pid, time.Since(exitStart))

		reacquireStart := time.Now()
		for {
			s2, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, f.pool, time.Second, time.Second)
			if err == nil && got {
				t.Logf("second open acquired the lock %v after Close", time.Since(reacquireStart))
				_, _ = s2.Close(f.ctx)
				break
			}
			if time.Since(reacquireStart) > 3*time.Second {
				t.Fatalf("lock not reacquirable 3s after Close (got=%v err=%v)", got, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		release()
		if got := f.total(table); got != 5 {
			t.Fatalf("count after release = %d, want 5 unchanged", got)
		}
	})

	root.run(t, "session refusals", func(t *testing.T, f *hrFx) {
		for _, c := range [][2]time.Duration{{0, time.Second}, {time.Second, 0}, {999 * time.Microsecond, time.Second}} {
			if s, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, f.pool, c[0], c[1]); err == nil || got || s != nil {
				t.Fatalf("open with timeouts %v = (%v, %v, %v), want a refusal", c, s, got, err)
			}
		}
		if n := f.lockHolders(); n != 0 {
			t.Fatalf("%d lock holders after refused opens", n)
		}
		s := f.open(time.Second, time.Second)
		if _, err := s.DeleteBatch(f.ctx, store.HistoryRetentionPRHistoryEvents, hrBase, 0); err == nil {
			t.Error("DeleteBatch with limit 0 succeeded, want a refusal")
		}
		if _, err := s.DeleteBatch(f.ctx, "pr_history_heads", hrBase, 10); err == nil {
			t.Error("DeleteBatch on pr_history_heads succeeded, want it refused as an unknown table")
		}
		if unlocked, err := s.Close(f.ctx); err != nil || !unlocked {
			t.Fatalf("Close = (%v, %v), want a proven unlock", unlocked, err)
		}
		if _, err := s.DeleteBatch(f.ctx, store.HistoryRetentionPRHistoryEvents, hrBase, 10); err == nil {
			t.Error("DeleteBatch on a closed session succeeded")
		}
		if unlocked, err := s.Close(f.ctx); unlocked || err != nil {
			t.Errorf("second Close = (%v, %v), want (false, nil)", unlocked, err)
		}
	})

	root.run(t, "generic plan", hrGenericPlan)
}

// hrOpenOn opens a session on the probe pool (one tagged connection, so a leaked backend shows in
// pg_stat_activity) with the idle_session_timeout value chosen by the caller.
func hrOpenOn(p *wcsProbe, work, cleanup context.Context, idle string) (*store.HistoryRetentionSession, bool, error) {
	return store.OpenHistoryRetentionSessionWithIdleForTest(work, cleanup, p.pool, time.Second, time.Second, idle)
}

// hrWaitLockFree polls, within 1.5s, until no backend holds the history retention lock.
func hrWaitLockFree(t *testing.T, f *hrFx) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for f.lockHolders() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d backend(s) still hold the history retention lock 1.5s after the session was abandoned", f.lockHolders())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestHistoryRetentionFailedOpenLiveDB covers every way OpenHistoryRetentionSession can end
// without a usable session, and the close of a session abandoned inside a transaction. Each case
// asserts, BEFORE the probe pool is closed, that no tagged backend and no lock holder is left: the
// connection must have been closed by the session code itself, not merely returned to a pool
// whose own close would hide a leak.
func TestHistoryRetentionFailedOpenLiveDB(t *testing.T) {
	root := newHrFx(t)

	root.run(t, "contention", func(t *testing.T, f *hrFx) {
		_ = f.open(time.Second, time.Second) // the holder
		p := f.probePool(t)
		workCtx, cancelWork := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancelWork()
		cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, 6*time.Second)
		defer cancelCleanup()
		s, got, err := hrOpenOn(p, workCtx, cleanupCtx, "5000")
		if s != nil || got || err != nil {
			t.Fatalf("contended open = (%v, %v, %v), want (nil, false, nil)", s, got, err)
		}
		f.waitNoSessions(t, p.tag)
		if n := f.lockHolders(); n != 1 {
			t.Fatalf("%d sessions hold the lock, want only the original holder", n)
		}
	})

	root.run(t, "setup error", func(t *testing.T, f *hrFx) {
		p := f.probePool(t)
		workCtx, cancelWork := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancelWork()
		cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, 6*time.Second)
		defer cancelCleanup()
		// An invalid idle_session_timeout makes the settings statement fail on the real server.
		s, got, err := hrOpenOn(p, workCtx, cleanupCtx, "not-a-number")
		if s != nil || got || err == nil || !strings.Contains(err.Error(), "session setup") {
			t.Fatalf("setup-error open = (%v, %v, %v), want (nil, false, a session setup error)", s, got, err)
		}
		f.waitNoSessions(t, p.tag)
		if n := f.lockHolders(); n != 0 {
			t.Fatalf("%d sessions hold the lock after a failed setup, want 0", n)
		}
	})

	// The work context ends between the settings statement and the try-lock. pgx refuses to send
	// on a context that is already done, so the try-lock never reaches the server: this pins the
	// close under a separate, still-live cleanup context. Like the next case, the try-lock is never
	// sent, but here the connection stays intact until the session closes it.
	root.run(t, "deadline before try-lock", func(t *testing.T, f *hrFx) {
		p := f.probePool(t)
		workCtx, cancelWork := context.WithCancel(f.ctx)
		defer cancelWork()
		cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, 6*time.Second)
		defer cancelCleanup()
		p.mu.Lock()
		p.afterSetup = cancelWork
		p.mu.Unlock()
		s, got, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, p.pool, time.Second, time.Second)
		if s != nil || got || err == nil || !strings.Contains(err.Error(), "try lock") {
			t.Fatalf("open = (%v, %v, %v), want (nil, false, a try-lock error)", s, got, err)
		}
		f.waitNoSessions(t, p.tag)
		hrWaitLockFree(t, f)
	})

	// The connection is lost before the try-lock is sent, while both contexts are still live: the
	// client socket is closed from the TraceQueryStart hook, which pgx calls before any network I/O,
	// so pg_try_advisory_lock never reaches the server. The open returns a try-lock error; the case
	// asserts that no session and no lock remain afterwards.
	root.run(t, "connection lost before try-lock", func(t *testing.T, f *hrFx) {
		p := f.probePool(t)
		cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, 6*time.Second)
		defer cancelCleanup()
		p.mu.Lock()
		p.onQueryStart = func(sql string) {
			if strings.Contains(sql, "pg_try_advisory_lock") {
				p.killClient(t)
			}
		}
		p.mu.Unlock()
		workCtx, cancelWork := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancelWork()
		s, got, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, p.pool, time.Second, time.Second)
		if s != nil || got || err == nil || !strings.Contains(err.Error(), "try lock") {
			t.Fatalf("open = (%v, %v, %v), want (nil, false, a try-lock error)", s, got, err)
		}
		f.waitNoSessions(t, p.tag)
		hrWaitLockFree(t, f)
	})

	// The try-lock IS sent and the server acquires the lock, but its reply never reaches the client
	// and the work context is cancelled: the open cannot know whether it holds the lock. The probe
	// pool's connection is wrapped so that, once the try-lock statement has been executed on the
	// server, every client read blocks until its deadline or the close. The cancel is gated on the
	// observed state (lock held, hold armed, a read blocked), not on a timer. The open must return an
	// error, the session must still be ended by the client side (the server-side lock was really
	// taken, so a leaked backend would keep it), and all of this is asserted while the probe pool
	// is still open.
	root.run(t, "try-lock sent, reply lost, work context cancelled", func(t *testing.T, f *hrFx) {
		p, w := hrReplyLostPool(t, f)
		// Warm the probe pool (dial and handshake) before the work context exists: the wrapper only
		// arms on the try-lock write.
		warm, err := p.pool.Acquire(f.ctx)
		if err != nil {
			t.Fatalf("warm the probe pool: %v", err)
		}
		warm.Release()
		cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, 6*time.Second)
		defer cancelCleanup()
		workCtx, cancelWork := context.WithCancel(f.ctx)
		defer cancelWork()

		type res struct {
			s   *store.HistoryRetentionSession
			got bool
			err error
		}
		done := make(chan res, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, got, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, p.pool, time.Second, time.Second)
			done <- res{s, got, err}
		}()
		t.Cleanup(func() {
			cancelWork()
			wg.Wait()
		})

		// While the open is still waiting for the reply, the server must already hold the lock for
		// the probe's backend: this is what makes the outcome ambiguous for the client.
		var holderPID int64
		sawLock := false
		pollEnd := time.Now().Add(5 * time.Second)
		for !sawLock && time.Now().Before(pollEnd) {
			select {
			case r := <-done:
				t.Fatalf("open returned (%v, %v, %v) before the server-side lock was observed", r.s, r.got, r.err)
			default:
			}
			err := f.pool.QueryRow(f.ctx, `SELECT l.pid FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			        WHERE l.locktype = 'advisory' AND l.granted AND l.objid = 0 AND l.classid = $1::int8::oid
			          AND l.objsubid = 2 AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
			          AND a.application_name = $2`, int64(store.HistoryRetentionLockClass), p.tag).Scan(&holderPID)
			switch {
			case err == nil:
				sawLock = true
			case errors.Is(err, pgx.ErrNoRows):
				time.Sleep(5 * time.Millisecond)
			default:
				t.Fatalf("lock holder query: %v", err)
			}
		}
		if !sawLock {
			t.Fatalf("the server never showed an advisory lock held by the probe backend (reply hold armed=%v)", w.armed())
		}
		// The lock was held while the open was still pending: the open has not returned yet.
		select {
		case r := <-done:
			t.Fatalf("open returned (%v, %v, %v) while the server-side lock was held and the reply withheld", r.s, r.got, r.err)
		default:
		}
		// Cancel only once the reply is provably withheld: the wrapper saw the try-lock execute
		// (armed) and a client read is blocked on the lost reply.
		// The lock poll above already implies this (the server holds the lock only after processing
		// the try-lock, after which the client is necessarily waiting on the withheld reply), so this
		// loop is a redundant safety condition; removing the poll and this loop together fails the test.
		gate := time.NewTimer(20 * time.Second)
		defer gate.Stop()
		for !w.armed() || !w.blockedRead() {
			select {
			case r := <-done:
				t.Fatalf("open returned (%v, %v, %v) before the reply hold was armed and a read blocked", r.s, r.got, r.err)
			case <-gate.C:
				t.Fatalf("watchdog: the reply hold never armed and blocked a read (armed=%v blocked=%v)", w.armed(), w.blockedRead())
			case <-time.After(5 * time.Millisecond):
			}
		}
		cancelWork()

		var r res
		select {
		case r = <-done:
		case <-gate.C:
			t.Fatal("watchdog: the open did not return after the work context was cancelled")
		}
		if r.s != nil || r.got || r.err == nil || !strings.Contains(r.err.Error(), "try lock") {
			t.Fatalf("open = (%v, %v, %v), want (nil, false, a try-lock error)", r.s, r.got, r.err)
		}
		if !w.blockedRead() {
			t.Error("no client read was ever blocked: the reply was not lost")
		}
		// The probe pool is still open here. The backend-gone and lock-gone assertions below also hold
		// through pgx's own asynchronous close of the broken connection; the store-specific assertions
		// of this subtest are the "try lock" error, got=false and s=nil above.
		f.waitNoSessions(t, p.tag)
		if f.count(`SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, holderPID) > 0 {
			t.Errorf("backend %d that held the lock is still alive after the open returned", holderPID)
		}
		hrWaitLockFree(t, f)
		if n := f.lockHolders(); n != 0 {
			t.Errorf("%d lock holders after the open returned, want 0", n)
		}
		s2, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, f.pool, time.Second, time.Second)
		if err != nil || !got {
			t.Fatalf("fresh open on the main pool: got=%v err=%v, want the lock", got, err)
		}
		if unlocked, err := s2.Close(f.ctx); err != nil || !unlocked {
			t.Errorf("fresh session Close = (%v, %v), want a proven unlock", unlocked, err)
		}
	})

	// Close on a session whose connection is open but inside a transaction (open or failed) must
	// not run the unlock: it reports no proven unlock, closes the connection, and the lock ends
	// with the backend.
	for _, c := range []struct {
		name   string
		stmts  []string
		status byte
	}{
		{"open transaction", []string{"BEGIN"}, 'T'},
		{"failed transaction", []string{"BEGIN", "SELECT 1/0"}, 'E'},
	} {
		root.run(t, "close inside "+c.name, func(t *testing.T, f *hrFx) {
			p := f.probePool(t)
			s, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, p.pool, 10*time.Second, 5*time.Second)
			if err != nil || !got {
				t.Fatalf("open session: got=%v err=%v", got, err)
			}
			t.Cleanup(func() { _, _ = s.Close(context.Background()) })
			pid := s.PIDForTest()
			for _, st := range c.stmts {
				_ = s.ExecForTest(f.ctx, st) // the failing statement is expected to error
			}
			if s.TxStatusForTest() != c.status || s.Idle() {
				t.Fatalf("setup: tx status %q idle=%v, want %q and not idle", s.TxStatusForTest(), s.Idle(), c.status)
			}
			if n := f.lockHolders(); n != 1 {
				t.Fatalf("%d sessions hold the lock before Close, want the session's own", n)
			}
			unlocked, err := s.Close(f.ctx)
			if unlocked || err != nil {
				t.Errorf("Close on a non-idle session: unlocked=%v err=%v, want false/nil (no unlock attempted)", unlocked, err)
			}
			// No unlock round trip: the last statement the connection ran is the one the test
			// issued (an unlock inside the failed transaction would also have errored above).
			p.mu.Lock()
			last := p.lastSQL
			p.mu.Unlock()
			if strings.Contains(last, "pg_advisory_unlock") {
				t.Errorf("Close sent the unlock %q on a non-idle session", last)
			}
			if s.Idle() {
				t.Error("session still reports idle after Close")
			}
			f.waitNoSessions(t, p.tag)
			if f.pidAlive(pid) {
				t.Errorf("backend %d is still alive after Close", pid)
			}
			hrWaitLockFree(t, f)
			s2, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, f.pool, time.Second, time.Second)
			if err != nil || !got {
				t.Fatalf("fresh open after the close: got=%v err=%v, want the lock", got, err)
			}
			if unlocked, err := s2.Close(f.ctx); err != nil || !unlocked {
				t.Errorf("fresh session Close = (%v, %v), want a proven unlock", unlocked, err)
			}
		})
	}
}

// hrReplyLostConn wraps the probe's client socket. Writes pass through to the server. Once the
// try-lock statement has been executed (its Execute message follows the write that carried the
// SQL text, or arrives in the same write for the simple and unprepared protocols), every Read
// blocks until the read deadline passes or the connection is closed, so the client never sees
// the reply the server sends. Writes (the Terminate pgx sends when it gives up) still pass.
type hrReplyLostConn struct {
	net.Conn
	mu       sync.Mutex
	sawSQL   bool
	hold     bool
	blocked  bool
	released bool
	isClosed bool
	deadline time.Time
	wake     chan struct{}
}

// pgExecuteUnnamedPortal is the Execute message for the unnamed portal with no row limit.
var pgExecuteUnnamedPortal = []byte{'E', 0, 0, 0, 9, 0, 0, 0, 0, 0}

// pgTerminate is the Terminate message.
var pgTerminate = []byte{'X', 0, 0, 0, 4}

func (c *hrReplyLostConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if bytes.Equal(b, pgTerminate) {
		// A Terminate message: the client is ending the session. Release the read hold so pgx's
		// asynchronous close drain sees the server's EOF instead of waiting out its own deadline.
		c.released = true
		c.signal()
	}
	if !c.hold {
		if bytes.Contains(b, []byte("pg_try_advisory_lock")) {
			c.sawSQL = true
			if len(b) > 0 && b[0] == 'Q' { // simple protocol: this message executes the query
				c.hold = true
			}
		}
		if c.sawSQL && bytes.Contains(b, pgExecuteUnnamedPortal) {
			c.hold = true
		}
	}
	c.mu.Unlock()
	return c.Conn.Write(b)
}

func (c *hrReplyLostConn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *hrReplyLostConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	c.signal()
	return c.Conn.SetDeadline(t)
}

func (c *hrReplyLostConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	c.signal()
	return c.Conn.SetReadDeadline(t)
}

func (c *hrReplyLostConn) Read(b []byte) (int, error) {
	for {
		c.mu.Lock()
		if !c.hold || c.released || c.isClosed {
			closed := c.isClosed
			c.mu.Unlock()
			if closed {
				return 0, net.ErrClosed
			}
			return c.Conn.Read(b)
		}
		c.blocked = true
		dl := c.deadline
		c.mu.Unlock()
		var timer <-chan time.Time
		if !dl.IsZero() {
			if !time.Now().Before(dl) {
				return 0, hrTimeoutErr{}
			}
			tm := time.NewTimer(time.Until(dl))
			timer = tm.C
			defer tm.Stop()
		}
		select {
		case <-c.wake:
		case <-timer:
		}
	}
}

func (c *hrReplyLostConn) Close() error {
	c.mu.Lock()
	c.isClosed = true
	c.mu.Unlock()
	c.signal()
	return c.Conn.Close()
}

func (c *hrReplyLostConn) armed() bool       { c.mu.Lock(); defer c.mu.Unlock(); return c.hold }
func (c *hrReplyLostConn) blockedRead() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.blocked }

// hrTimeoutErr is the error a net.Conn read returns when its deadline passes.
type hrTimeoutErr struct{}

func (hrTimeoutErr) Error() string   { return "i/o timeout" }
func (hrTimeoutErr) Timeout() bool   { return true }
func (hrTimeoutErr) Temporary() bool { return true }

// hrReplyLostPool is a one-connection probe pool (tagged application_name, a single dial allowed so
// a client-side cancel request cannot reach the server) whose socket is the wrapper above.
func hrReplyLostPool(t *testing.T, f *hrFx) (*wcsProbe, *hrReplyLostConn) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	p := &wcsProbe{tag: "hr-lost-reply-" + uuid.NewString()}
	cfg.MaxConns = 1
	// Plaintext, so the SQL bytes the wrapper inspects are readable, and no fallback config, so
	// exactly one dial happens.
	cfg.ConnConfig.TLSConfig = nil
	cfg.ConnConfig.Fallbacks = nil
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = p.tag
	w := &hrReplyLostConn{wake: make(chan struct{}, 1)}
	var dials int
	var dmu sync.Mutex
	d := &net.Dialer{}
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dmu.Lock()
		defer dmu.Unlock()
		if dials > 0 {
			return nil, errors.New("reply-lost pool: only one dial is allowed")
		}
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		dials++
		w.Conn = c
		return w, nil
	}
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Close the wrapper too, so no drain goroutine or socket outlives the subtest.
	t.Cleanup(func() {
		if w.Conn != nil {
			_ = w.Close()
		}
	})
	p.pool = pool
	return p, w
}

// hrDelayConn wraps a client socket and, while armed is set, sleeps d before every write reaches
// the server and after every read, before returning to the client.
type hrDelayConn struct {
	net.Conn
	armed *atomic.Bool
	d     time.Duration
}

func (c *hrDelayConn) Write(b []byte) (int, error) {
	if c.armed.Load() {
		time.Sleep(c.d)
	}
	return c.Conn.Write(b)
}

func (c *hrDelayConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.armed.Load() {
		time.Sleep(c.d)
	}
	return n, err
}

// hrDelayTracer arms the delay for the duration of any statement that reads statement_timestamp.
type hrDelayTracer struct{ armed *atomic.Bool }

func (t hrDelayTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "statement_timestamp") {
		t.armed.Store(true)
	}
	return ctx
}

func (t hrDelayTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
	t.armed.Store(false)
}

// hrDelayPool is a pool on the fixture database whose sockets delay traffic by d each way while a
// statement mentioning statement_timestamp is in flight.
func hrDelayPool(t *testing.T, f *hrFx, d time.Duration) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	armed := &atomic.Bool{}
	cfg.ConnConfig.Tracer = hrDelayTracer{armed: armed}
	dialer := &net.Dialer{}
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &hrDelayConn{Conn: c, armed: armed, d: d}, nil
	}
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestHistoryRetentionLockHoldersScopingLiveDB pins the scope of lockHolders: it must count the
// two-int advisory lock (HistoryRetentionLockClass, 0) in the fixture's own database only. Two
// sentinels that match every other filter are held: the one-int form of the same numeric key in the
// fixture database (objsubid 1) and the two-int form in another database. Each is shown granted
// server-wide first, so a zero count cannot come from a lock that was never taken.
func TestHistoryRetentionLockHoldersScopingLiveDB(t *testing.T) {
	f := newHrFx(t)
	class := int64(store.HistoryRetentionLockClass)

	oneKey, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer oneKey.Release()
	if _, err := oneKey.Exec(f.ctx, `SELECT pg_advisory_lock($1::bigint)`, class<<32); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = oneKey.Exec(context.Background(), `SELECT pg_advisory_unlock($1::bigint)`, class<<32) }()

	otherCtx, otherDSN := standIsolatedDB(t, "hrsent_")
	other, err := pgx.Connect(otherCtx, otherDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close(context.Background()) }()
	if _, err := other.Exec(otherCtx, `SELECT pg_advisory_lock($1, 0)`, int32(class)); err != nil {
		t.Fatal(err)
	}

	var fixtureOID, otherOID uint32
	if err := f.pool.QueryRow(f.ctx, `SELECT oid FROM pg_database WHERE datname = current_database()`).Scan(&fixtureOID); err != nil {
		t.Fatal(err)
	}
	if err := other.QueryRow(otherCtx, `SELECT oid FROM pg_database WHERE datname = current_database()`).Scan(&otherOID); err != nil {
		t.Fatal(err)
	}
	if fixtureOID == otherOID {
		t.Fatalf("both sentinels are in database %d", fixtureOID)
	}
	serverWide := func(subid int16, db uint32) int {
		return f.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND classid = $1::int8::oid
		        AND objid = 0 AND objsubid = $2 AND database = $3::oid`, class, int32(subid), int64(db))
	}
	if n := serverWide(1, fixtureOID); n != 1 {
		t.Fatalf("one-key sentinel in the fixture database: %d granted locks, want 1", n)
	}
	if n := serverWide(2, otherOID); n != 1 {
		t.Fatalf("two-int sentinel in the other database: %d granted locks, want 1", n)
	}
	if n := f.lockHolders(); n != 0 {
		t.Errorf("lockHolders = %d with only a one-key lock here and a two-int lock in another database, want 0", n)
	}

	// Positive control: a real session in the fixture database is counted exactly once.
	_ = f.open(time.Second, time.Second)
	if n := f.lockHolders(); n != 1 {
		t.Errorf("lockHolders = %d with one real session, want 1", n)
	}
}

// hrPlan is the part of an EXPLAIN (FORMAT JSON) node this test reads.
type hrPlan struct {
	NodeType  string   `json:"Node Type"`
	Relation  string   `json:"Relation Name"`
	IndexName string   `json:"Index Name"`
	Plans     []hrPlan `json:"Plans"`
}

// findIndexScan reports whether the tree has an Index Scan or Index Only Scan node on relation
// that reads the named index.
func (p *hrPlan) findIndexScan(relation, index string) bool {
	if (p.NodeType == "Index Scan" || p.NodeType == "Index Only Scan") && p.Relation == relation && p.IndexName == index {
		return true
	}
	for i := range p.Plans {
		if p.Plans[i].findIndexScan(relation, index) {
			return true
		}
	}
	return false
}

// hrRetentionIndex is the time index migrations 00555 to 00558 create for each table.
var hrRetentionIndex = map[store.HistoryRetentionTable]string{
	store.HistoryRetentionPRHistoryEvents:      "pr_history_events_observed_at_idx",
	store.HistoryRetentionAnthropicUsage:       "anthropic_usage_samples_bucket_start_idx",
	store.HistoryRetentionCodexUsage:           "codex_usage_samples_bucket_start_idx",
	store.HistoryRetentionWorkerCapacitySample: "worker_capacity_samples_bucket_start_idx",
}

func (p *hrPlan) find(nodeType, relation string) bool {
	if p.NodeType == nodeType && (relation == "" || p.Relation == relation) {
		return true
	}
	for i := range p.Plans {
		if p.Plans[i].find(nodeType, relation) {
			return true
		}
	}
	return false
}

// hrGenericPlan pins the plan shape of the four delete statements under force_generic_plan: the
// outer DELETE must be a Tid Scan on the target table, the inner page read a LockRows over an
// Index Scan (or Index Only Scan) on the table's retention time index from migrations 00555 to
// 00558, which the ORDER BY on the expiry column lets the planner use. Under force_generic_plan the
// cutoff is a parameter, so the planner costs it with default selectivity and the fixture's expired
// fraction plays no part; what the test pins is the plan shape, the migrations' index names and the
// partial predicate of the pr_history_events index. The stale-statistics custom-plan flip (a Seq Scan
// once a drain leaves the statistics stale) is not covered here; only the evidence harness
// (history_retention_evidence_livedb_test.go) measures it. Production runs each delete once on a fresh
// hijacked connection, so it uses a custom plan; the generic plan checked here is a conservative
// safety pin (it is what a long-lived pooled connection would reach after five executions), not a
// path the pass takes. The plan is explained inside a rolled-back transaction.
func hrGenericPlan(t *testing.T, f *hrFx) {
	// The four migration indexes exist, are valid btrees on the expiry column, and only the
	// pr_history_events one is partial on NOT first_merge.
	for _, table := range hrTables {
		var def string
		var valid bool
		err := f.pool.QueryRow(f.ctx, `SELECT i.indexdef, x.indisvalid
		        FROM pg_indexes i
		        JOIN pg_index x ON x.indexrelid = format('%I.%I', i.schemaname, i.indexname)::regclass
		        WHERE i.schemaname = current_schema() AND i.tablename = $1 AND i.indexname = $2`,
			string(table), hrRetentionIndex[table]).Scan(&def, &valid)
		if err != nil {
			t.Fatalf("%s: index %s not found: %v", table, hrRetentionIndex[table], err)
		}
		if !valid {
			t.Errorf("%s: index %s is not valid", table, hrRetentionIndex[table])
		}
		if want := " USING btree (" + hrExpiryColumn(table) + ")"; !strings.Contains(def, want) {
			t.Errorf("%s: indexdef %q lacks %q", table, def, want)
		}
		hasPred := strings.Contains(def, " WHERE ")
		if table == store.HistoryRetentionPRHistoryEvents {
			if !strings.HasSuffix(def, " WHERE (NOT first_merge)") {
				t.Errorf("%s: indexdef %q is not partial on WHERE (NOT first_merge)", table, def)
			}
		} else if hasPred {
			t.Errorf("%s: indexdef %q carries an unexpected predicate", table, def)
		}
	}
	for _, table := range hrTables {
		f.seed(table, 2000, hrBase)
		f.seed(table, 38000, hrBase.Add(48*time.Hour))
		f.exec(`ANALYZE ` + string(table))
	}
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	simple := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(f.ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	simple(`SET plan_cache_mode = force_generic_plan`)
	// The connection returns to the pool: drop the prepared statements and the setting with it.
	defer func() {
		_, _ = conn.Exec(context.Background(), `RESET ALL`, pgx.QueryExecModeSimpleProtocol)
		_, _ = conn.Exec(context.Background(), `DEALLOCATE ALL`, pgx.QueryExecModeSimpleProtocol)
	}()
	for _, table := range hrTables {
		stmt := "hr_gp_" + string(table)
		simple("DEALLOCATE ALL")
		simple("PREPARE " + stmt + " AS " + store.HistoryRetentionDeleteSQLForTest(table))
		simple("BEGIN")
		var raw []byte
		err := conn.QueryRow(f.ctx, fmt.Sprintf(`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE %s('%s', 1000)`, stmt, hrBase.Add(time.Hour).Format(time.RFC3339Nano)),
			pgx.QueryExecModeSimpleProtocol).Scan(&raw)
		simple("ROLLBACK")
		if err != nil {
			t.Fatalf("%s: EXPLAIN EXECUTE: %v", table, err)
		}
		var generic int64
		if err := conn.QueryRow(f.ctx, `SELECT generic_plans FROM pg_prepared_statements WHERE name = $1`, pgx.QueryExecModeSimpleProtocol, stmt).Scan(&generic); err != nil || generic < 1 {
			t.Fatalf("%s: generic_plans = %d (%v); the EXPLAIN did not run the generic plan this test exists to check", table, generic, err)
		}
		var doc []struct {
			Plan hrPlan `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
			t.Fatalf("%s: decode EXPLAIN JSON (%d docs): %v", table, len(doc), err)
		}
		root := &doc[0].Plan
		if !root.find("Tid Scan", string(table)) {
			t.Errorf("%s: the outer DELETE is not a Tid Scan on the table:\n%s", table, raw)
		}
		if !root.find("LockRows", "") {
			t.Errorf("%s: the inner page read has no LockRows node:\n%s", table, raw)
		}
		if !root.findIndexScan(string(table), hrRetentionIndex[table]) {
			t.Errorf("%s: the inner page read is not an Index Scan on %s:\n%s", table, hrRetentionIndex[table], raw)
		}
	}
}
