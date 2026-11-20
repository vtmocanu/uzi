package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4488 (ADR-4007 T3): tx-R1 try-takes the repository's consumer gate exclusively, under
// server-side hold bounds set before it. These LiveDB tests pin the retire side of the gate: a busy
// gate keeps the ref, an unrelated repository does not matter, a stalled tx-R1 loses the gate to the
// server's own timeouts, a queued admission waits for one bounded hold, and the other lanes take no
// gate. The admission side is covered by terminal_consumer_race_livedb_test.go.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh). Run it
// against PostgreSQL 16 and 17: the stall test exercises transaction_timeout on 17 and the statement
// and idle fallback on 16.

// retireTestBounds keeps every bound out of the way of a paused tx-R1.
var retireTestBounds = terminalRetireBounds{transaction: "20s", statement: "20s", idle: "20s", lock: "2s"}

// holdGateShared takes the gate shared in a test transaction that the cleanup rolls back.
func holdGateShared(t *testing.T, f *sweepFix, repo uuid.UUID) pgx.Tx {
	t.Helper()
	tx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err = store.New(tx).LockTerminalConsumerGateShared(f.e.ctx, repo, time.Second); err != nil {
		t.Fatalf("shared gate: %v", err)
	}
	return tx
}

// gateLocks counts the pg_locks rows on the repository's gate key (two-int advisory, objsubid 2).
func gateLocks(t *testing.T, f *sweepFix, repo uuid.UUID, mode string, granted bool) int {
	t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype='advisory' AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=2 AND mode=$3 AND granted=$4`,
		int64(store.TerminalConsumerGateLockClass), int64(uint32(store.TerminalConsumerGateObjID(repo))), mode, granted).Scan(&n); err != nil { //nolint:gosec // reinterpret the int4 lock key as pg_locks' oid
		t.Fatal(err)
	}
	return n
}

// retireFix is a retire-enabled fixture whose live, terminal run is proven closed.
func retireFix(t *testing.T) *sweepFix {
	t.Helper()
	f := newSweepFix(t)
	f.enableRetire()
	f.live()
	f.forges.issue = forge.Issue{State: "closed"}
	return f
}

func TestTerminalRetireGateBusyLiveDB(t *testing.T) {
	start := time.Now()
	f := retireFix(t)
	holder := holdGateShared(t, f, f.e.repoID)

	if n := f.sweep(); n != 0 {
		t.Fatalf("changed %d rows while an admission held the gate", n)
	}
	p := f.mustPres("live")
	if p.LastError.String != "retire kept: consumer_gate_busy" || p.Attempts != 0 || f.deleteCount() != 0 {
		t.Fatalf("last_error = %q attempts = %d deletes = %d, want the gate-busy keep, no attempt, no delete", p.LastError.String, p.Attempts, f.deleteCount())
	}
	if !p.NextCheckAt.Time.After(f.dbNow().Add(4 * time.Minute)) {
		t.Fatalf("next_check_at = %v, want pushed out", p.NextCheckAt.Time)
	}

	// Recovery: once the admission is done the next pass retires normally.
	if err := holder.Commit(f.e.ctx); err != nil {
		t.Fatal(err)
	}
	f.due()
	if n := f.sweep(); n != 2 {
		t.Fatalf("changed %d rows after the gate was free, want 2", n)
	}
	f.mustPres("deleted")
	t.Logf("elapsed %v", time.Since(start))
}

func TestTerminalRetireGateOtherRepoLiveDB(t *testing.T) {
	f := retireFix(t)
	holdGateShared(t, f, uuid.New())
	if n := f.sweep(); n != 2 {
		t.Fatalf("changed %d rows, want 2: a gate holder on another repository must not block", n)
	}
	f.mustPres("deleted")
}

// A tx-R1 that stalls while holding the gate loses it to the server: transaction_timeout on 17 and
// later, the idle (and statement) timeout otherwise. The pause outlasts the bound; the gate must be
// gone while the process is still paused, nothing may be retired, and the pass must back the row off.
func TestTerminalRetireHoldBoundLiveDB(t *testing.T) {
	const slack = 1500 * time.Millisecond
	// transaction_timeout counts from its SET, so it must also cover tx-R1's own statements before the
	// pause point; 1s keeps a loaded runner from killing it before the stall begins.
	bounds := []struct {
		name   string
		b      terminalRetireBounds
		bound  time.Duration
		pg17up bool
	}{
		{"idle", terminalRetireBounds{transaction: "20s", statement: "20s", idle: "300ms", lock: "2s"}, 300 * time.Millisecond, false},
		{"transaction_timeout", terminalRetireBounds{transaction: "1s", statement: "20s", idle: "20s", lock: "2s"}, time.Second, true},
	}
	points := []struct {
		name string
		set  func(h *terminalHooks, pause func())
	}{
		{"after the gate", func(h *terminalHooks, pause func()) { h.afterRetireGate = pause }},
		{"before the retiring commit", func(h *terminalHooks, pause func()) { h.beforeRetiringCommit = pause }},
	}
	for _, bc := range bounds {
		for _, pt := range points {
			t.Run(bc.name+"/"+pt.name, func(t *testing.T) {
				start := time.Now()
				f := retireFix(t)
				var version int
				if err := f.e.pool.QueryRow(f.e.ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
					t.Fatal(err)
				}
				if bc.pg17up && version < terminalPG17 {
					t.Skipf("transaction_timeout needs PostgreSQL 17 (server_version_num %d)", version)
				}
				t.Logf("server_version_num %d", version)
				b := bc.b
				f.svc.terminal.retireBounds = &b
				g := newRaceGate(t)
				pt.set(&f.svc.terminal, g.pause)
				done := make(chan error, 1)
				go func() { done <- f.sweepErr() }()
				raceReached(t, g, "tx-R1 to stall")

				if got := gateLocks(t, f, f.e.repoID, "ExclusiveLock", true); got != 1 {
					g.release()
					t.Fatalf("exclusive gate holders at the stall = %d, want 1", got)
				}
				bound := bc.bound
				stalled := time.Now()
				for gateLocks(t, f, f.e.repoID, "ExclusiveLock", true) != 0 {
					if time.Since(stalled) > bound+slack {
						g.release()
						t.Fatalf("the gate was still held %v into the stall (bound %v)", time.Since(stalled), bound)
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Logf("gate released by the server after %v (bound %v), process still paused", time.Since(stalled), bound)

				g.release()
				if err := raceAwait(t, done, "the stalled pass to return"); err == nil {
					t.Fatal("the pass succeeded although its transaction was killed")
				}
				p := f.mustPres("live")
				if f.deleteCount() != 0 || p.Attempts != 1 || !p.NextCheckAt.Time.After(f.dbNow().Add(30*time.Second)) {
					t.Fatalf("deletes = %d attempts = %d next_check_at = %v, want nothing retired and a backoff", f.deleteCount(), p.Attempts, p.NextCheckAt.Time)
				}
				t.Logf("elapsed %v", time.Since(start))
			})
		}
	}
}

// Waiter priority at the sweep level: while tx-R1 is paused holding the gate, an admission queues
// shared behind it (and a plain try on the key fails); once tx-R1 ends the admission is granted and the
// preservation stays retiring or later.
func TestTerminalRetireGateWaiterLiveDB(t *testing.T) {
	start := time.Now()
	f := retireFix(t)
	b := retireTestBounds
	f.svc.terminal.retireBounds = &b
	g := newRaceGate(t)
	f.svc.terminal.beforeRetiringCommit = g.pause
	sweepDone := make(chan error, 1)
	go func() { sweepDone <- f.sweepErr() }()
	raceReached(t, g, "tx-R1 to pause")

	waiterTx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		g.release()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = waiterTx.Rollback(context.Background()) })
	waiter := make(chan error, 1)
	go func() {
		waiter <- store.New(waiterTx).LockTerminalConsumerGateShared(f.e.ctx, f.e.repoID, 10*time.Second)
	}()
	for deadline := time.Now().Add(2 * time.Second); gateLocks(t, f, f.e.repoID, "ShareLock", false) != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			g.release()
			t.Fatal("the admission never showed as a queued shared waiter in pg_locks")
		}
	}

	g.release()
	if err := raceAwait(t, sweepDone, "the retire pass"); err != nil {
		t.Fatalf("retire pass: %v", err)
	}
	if err := raceAwait(t, waiter, "the queued admission"); err != nil {
		t.Fatalf("queued admission: %v", err)
	}
	if p, ok := f.pres(); !ok || (p.State != "retiring" && p.State != "deleted") {
		t.Fatalf("preservation = %+v (found %v), want retiring or deleted, never reopened", p, ok)
	}
	t.Logf("elapsed %v", time.Since(start))
}

// The bound and reconcile lanes take no gate: a shared holder does not stop either transition.
func TestTerminalRetireGateOtherLanesLiveDB(t *testing.T) {
	t.Run("stale bound", func(t *testing.T) {
		f := newSweepFix(t)
		if _, err := f.e.q.InsertTerminalPreservation(f.e.ctx, store.InsertTerminalPreservationParams{
			RunID: f.run, UserID: f.e.userID, RepoID: f.e.repoID, Ref: pushbroker.TerminalRef(f.run), LifetimeAtBind: "issue",
		}); err != nil {
			t.Fatal(err)
		}
		release := holdGateExclusive(f.e.ctx, t, f.e.pool, f.e.repoID)
		f.shiftClock(terminalBoundGrace + time.Minute)
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1 while the exclusive holder remains", n)
		}
		f.mustPres("not_published")
		release()
	})
	t.Run("reconcile", func(t *testing.T) {
		f := newSweepFix(t)
		f.publishing("invoked")
		release := holdGateExclusive(f.e.ctx, t, f.e.pool, f.e.repoID)
		f.shiftClock(terminalReconcileGrace + time.Minute)
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1 while the exclusive holder remains", n)
		}
		f.mustPres("live")
		release()
	})
}

// The production pre-commit budgets stay below the admission gate wait, and the
// preservation row lock_timeout strictly below statement_timeout (so a contended row is busy, 55P03,
// not a statement timeout). A change to either side of the inequality must fail here.
func TestTerminalRetireBoundsInvariants(t *testing.T) {
	ms := func(s string) time.Duration {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			t.Fatalf("bound %q must be positive (parse error: %v)", s, err)
		}
		return d
	}
	pg17 := terminalRetireBoundsFor(terminalPG17)
	_ = ms(pg17.idle) // The transaction budget does not make a disabled idle timeout valid.
	if pg17.transaction == "" || ms(pg17.transaction) >= terminalConsumerGateWait {
		t.Fatalf("PG17 transaction bound %q must be set and below the %v gate wait", pg17.transaction, terminalConsumerGateWait)
	}
	if ms(pg17.lock) >= ms(pg17.statement) {
		t.Fatalf("PG17 lock %q must be below statement %q", pg17.lock, pg17.statement)
	}
	pg16 := terminalRetireBoundsFor(terminalPG17 - 1)
	if pg16.transaction != "" {
		t.Fatalf("pre-17 bounds must not set transaction_timeout (got %q)", pg16.transaction)
	}
	hold := ms(pg16.statement) + terminalRetirePG16RoundTrips*(ms(pg16.statement)+ms(pg16.idle))
	if hold >= terminalConsumerGateWait {
		t.Fatalf("pre-17 hold bound %v must be below the %v gate wait", hold, terminalConsumerGateWait)
	}
	if ms(pg16.lock) >= ms(pg16.statement) {
		t.Fatalf("pre-17 lock %q must be below statement %q", pg16.lock, pg16.statement)
	}
}
