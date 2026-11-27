package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// history_retention_livedb_test.go drives the session half of the history_retention pass (issue
// #4695) against PostgreSQL: the pinned lock, the cutoff statement and the four batch deletes.
// Every test stands its own isolated database (newWcsFx), because a delete test must never see
// another test's rows. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run
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

// lockHolders counts backends holding the history retention advisory lock.
func (f *hrFx) lockHolders() int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objid = 0 AND classid = $1::int8::oid`,
		int64(store.HistoryRetentionLockClass))
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

func TestHistoryRetentionBoundaryPRHistoryEventsLiveDB(t *testing.T) {
	f := newHrFx(t)
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
}

func TestHistoryRetentionBoundarySampleTablesLiveDB(t *testing.T) {
	for _, table := range hrTables[1:] {
		t.Run(string(table), func(t *testing.T) {
			f := newHrFx(t)
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
}

func TestHistoryRetentionCutoffExactLiveDB(t *testing.T) {
	f := newHrFx(t)
	s := f.open(time.Second, time.Second)
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
	if ref.Before(before.Add(-time.Millisecond)) || ref.After(after.Add(time.Millisecond)) {
		t.Errorf("reference %v is not between the client readings %v and %v", ref, before, after)
	}
	for _, bad := range []time.Duration{0, -time.Hour} {
		if _, _, err := s.Cutoff(f.ctx, bad); err == nil {
			t.Errorf("Cutoff(%v) succeeded, want a refusal", bad)
		}
	}
}

func TestHistoryRetentionProtectedRowsLiveDB(t *testing.T) {
	f := newHrFx(t)
	s := f.open(time.Second, time.Second)
	old := hrBase
	f.seed(store.HistoryRetentionPRHistoryEvents, 4, old)
	f.seedFirstMerge(3, old)
	f.exec(`INSERT INTO pr_history_heads (repo_id, mr_iid, state, observed_at, last_to_state)
	        SELECT $1, $2::bigint + g, 'merged', $3, 'merged' FROM generate_series(1, 5) g`, f.repoQ, f.nextIID(5), old)
	hrMustDelete(t, s, store.HistoryRetentionPRHistoryEvents, old.Add(24*time.Hour), 1000, 4)
	if got := f.count(`SELECT count(*) FROM pr_history_events WHERE first_merge`); got != 3 {
		t.Errorf("first_merge events = %d, want all 3 kept", got)
	}
	if got := f.total(store.HistoryRetentionPRHistoryEvents); got != 3 {
		t.Errorf("pr_history_events rows = %d, want only the 3 protected ones", got)
	}
	if got := f.count(`SELECT count(*) FROM pr_history_heads`); got != 5 {
		t.Errorf("pr_history_heads rows = %d, want all 5 kept", got)
	}
}

func TestHistoryRetentionBatchingLiveDB(t *testing.T) {
	for _, table := range hrTables {
		t.Run(string(table), func(t *testing.T) {
			f := newHrFx(t)
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
}

func TestHistoryRetentionRowLockSkipLiveDB(t *testing.T) {
	for _, table := range hrTables {
		t.Run(string(table), func(t *testing.T) {
			f := newHrFx(t)
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
}

func TestHistoryRetentionRelationLockLiveDB(t *testing.T) {
	f := newHrFx(t)
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
}

func TestHistoryRetentionServerTimeoutLiveDB(t *testing.T) {
	f := newHrFx(t)
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
}

func TestHistoryRetentionCancelAndReacquireLiveDB(t *testing.T) {
	f := newHrFx(t)
	table := store.HistoryRetentionPRHistoryEvents
	f.seed(table, 5, hrBase)
	s := f.open(5*time.Second, 5*time.Second)
	pid := s.PIDForTest()
	release := f.lockRelation(table)

	workCtx, cancelWork := context.WithTimeout(f.ctx, 1500*time.Millisecond)
	defer cancelWork()
	type res struct {
		n   int64
		err error
		at  time.Duration
	}
	started := time.Now()
	done := make(chan res, 1)
	go func() {
		n, err := s.DeleteBatch(workCtx, table, hrBase.Add(time.Hour), 1000)
		done <- res{n, err, time.Since(started)}
	}()
	waiting := false
	for deadline := time.Now().Add(1200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if f.count(`SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock'`, int64(pid)) == 1 {
			waiting = true
			break
		}
	}
	if !waiting {
		t.Fatal("the delete never showed wait_event_type = Lock in pg_stat_activity")
	}
	r := <-done
	t.Logf("work context expired: DeleteBatch returned after %v", r.at)
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
			t.Fatalf("backend %d still alive 3s after Close (idle_session_timeout is 5s, so only the close can end it)", pid)
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
}

// hrOpenOn opens a session on the probe pool (one tagged connection, so a leaked backend shows in
// pg_stat_activity) with the idle_session_timeout value chosen by the caller.
func hrOpenOn(p *wcsProbe, work, cleanup context.Context, idle string) (*store.HistoryRetentionSession, bool, error) {
	return store.OpenHistoryRetentionSessionWithIdleForTest(work, cleanup, p.pool, time.Second, time.Second, idle)
}

func TestHistoryRetentionFailedOpenContentionLiveDB(t *testing.T) {
	f := newHrFx(t)
	holder := f.open(time.Second, time.Second)
	_ = holder
	p := f.probePool(t)
	workCtx, cancelWork := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancelWork()
	cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, 6*time.Second)
	defer cancelCleanup()
	s, got, err := hrOpenOn(p, workCtx, cleanupCtx, "5000")
	if s != nil || got || err != nil {
		t.Fatalf("contended open = (%v, %v, %v), want (nil, false, nil)", s, got, err)
	}
	p.pool.Close()
	f.waitNoSessions(t, p.tag)
	if n := f.lockHolders(); n != 1 {
		t.Fatalf("%d sessions hold the lock, want only the original holder", n)
	}
}

func TestHistoryRetentionFailedOpenSetupErrorLiveDB(t *testing.T) {
	f := newHrFx(t)
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
	p.pool.Close()
	f.waitNoSessions(t, p.tag)
	if n := f.lockHolders(); n != 0 {
		t.Fatalf("%d sessions hold the lock after a failed setup, want 0", n)
	}
}

func TestHistoryRetentionFailedOpenTryLockExpiringWorkLiveDB(t *testing.T) {
	f := newHrFx(t)
	p := f.probePool(t)
	workCtx, cancelWork := context.WithCancel(f.ctx)
	defer cancelWork()
	// The work context ends between the settings statement and the try-lock; the cleanup context
	// is a separate, still-live one, so the close is not shortened by the expired work deadline.
	cleanupCtx, cancelCleanup := context.WithTimeout(f.ctx, 6*time.Second)
	defer cancelCleanup()
	p.mu.Lock()
	p.afterSetup = cancelWork
	p.mu.Unlock()
	s, got, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, p.pool, time.Second, time.Second)
	if s != nil || got || err == nil || !strings.Contains(err.Error(), "try lock") {
		t.Fatalf("open = (%v, %v, %v), want (nil, false, a try-lock error)", s, got, err)
	}
	p.pool.Close()
	f.waitNoSessions(t, p.tag)
	if n := f.lockHolders(); n != 0 {
		t.Fatalf("%d sessions hold the lock after an interrupted try-lock, want 0", n)
	}
}

func TestHistoryRetentionSessionRefusalsLiveDB(t *testing.T) {
	f := newHrFx(t)
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
}

// hrPlan is the part of an EXPLAIN (FORMAT JSON) node this test reads.
type hrPlan struct {
	NodeType string   `json:"Node Type"`
	Relation string   `json:"Relation Name"`
	Plans    []hrPlan `json:"Plans"`
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

// TestHistoryRetentionGenericPlanLiveDB pins the plan shape of the four delete statements under
// force_generic_plan (what a long-lived pooled connection reaches after five executions): the
// outer DELETE must be a Tid Scan on the target table, the inner page read a LockRows over the
// table. The plan is explained inside a rolled-back transaction.
func TestHistoryRetentionGenericPlanLiveDB(t *testing.T) {
	f := newHrFx(t)
	for _, table := range hrTables {
		f.seed(table, 2000, hrBase)
		f.seed(table, 2000, hrBase.Add(48*time.Hour))
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
	defer func() { _, _ = conn.Exec(context.Background(), `RESET ALL`, pgx.QueryExecModeSimpleProtocol) }()
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
	}
}
