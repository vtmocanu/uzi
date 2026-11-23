package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These proofs model process restart by constructing fresh services against committed PostgreSQL
// state. They do not model storage loss, claim admission, or exactly-once delivery.
type codexProviderParkFix struct {
	*provFix
	id     uuid.UUID
	worker store.Worker
	keyRev int64
}

func newCodexProviderParkFix(t *testing.T) *codexProviderParkFix {
	t.Helper()
	fx := newProvFix(t)
	seedCodexSource(t, fx)
	// Distinct non-default revisions make accidental replacement visible.
	fx.env.exec("UPDATE user_secrets SET enablement_rev=7 WHERE id=$1", fx.key)
	run, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	f := &codexProviderParkFix{provFix: fx, id: run.ID, worker: wkrRow(t, fx.env, fx.workerID), keyRev: 7}
	// This park-only fixture seeds an already-running flight's ownership.
	// Actual service claim admission is exercised separately; no guard is bypassed here.
	fx.env.exec(`UPDATE runs SET status='running', worker_id=$2, claim_generation=11,
 wait_on_limit=true, started_at=now(), budget_wall_seconds=3600, session_id='m3-provider-session'
 WHERE id=$1`, f.id, f.worker.ID)
	f.restart(time.Now().UTC().Truncate(time.Second))
	f.check(t, "running", 0, 0)
	return f
}

func (f *codexProviderParkFix) restart(now time.Time) {
	// Do not copy the old Service: all in-memory run state must be discarded.
	s := New(store.New(f.env.pool), f.env.box, testParams())
	s.SetTxBeginner(f.env.pool)
	s.SetVault(f.env.vlt)
	s.p.ProviderAllowedBaseURLs = []string{pvBase}
	s.now = func() time.Time { return now }
	f.svc = s
}

func (f *codexProviderParkFix) check(t *testing.T, status string, streak, waits int32) store.Run {
	t.Helper()
	r := mustRun(t, f.env, f.id)
	wantCodexBinding(t, f.provFix, r, 1, f.keyRev)
	if r.ID != f.id || r.Status != status || r.ModelProviderLimitStreak != streak || r.LimitWaitCount != waits {
		t.Fatalf("persisted run: id=%s status=%s streak=%d waits=%d; want %s/%s/%d/%d", r.ID, r.Status, r.ModelProviderLimitStreak, r.LimitWaitCount, f.id, status, streak, waits)
	}
	return r
}

func (f *codexProviderParkFix) park(t *testing.T, gen int64) int64 {
	t.Helper()
	r := mustRun(t, f.env, f.id)
	// Real service policy + generation/status-fenced runtime write + actual transaction commit.
	rows, err := f.svc.setLimitWait(f.env.ctx, r, f.worker, StateRequest{State: "limit_wait", ClaimGeneration: &gen}, pgtype.Text{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (f *codexProviderParkFix) nextRunningFixture(t *testing.T) int64 {
	t.Helper()
	// Simulate the next admitted running flight only; this proof does not claim to test Claim.
	f.env.exec(`UPDATE runs SET status='running', worker_id=$2, claim_generation=claim_generation+1,
 claim_released_at=NULL, started_at=now(), status_since=now() WHERE id=$1 AND status='queued'`, f.id, f.worker.ID)
	r := mustRun(t, f.env, f.id)
	if r.Status != "running" {
		t.Fatalf("next running fixture: %s", r.Status)
	}
	return r.ClaimGeneration
}

func TestCodexProviderParkRestartScheduleLiveDB(t *testing.T) {
	f := newCodexProviderParkFix(t)
	now := time.Now().UTC().Truncate(time.Second)
	gen := int64(11)
	for i, delay := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 0} {
		f.svc.now = func() time.Time { return now }
		if rows := f.park(t, gen); rows != 1 {
			t.Fatalf("park %d rows=%d", i+1, rows)
		}
		f.restart(now)
		r := f.check(t, "limit_wait", int32(i+1), int32(i+1))
		if !r.RateLimitType.Valid || r.RateLimitType.String != RateLimitTypeProviderGatewayLimit || r.LimitResetsAt.Valid {
			t.Fatalf("park %d: type=%v resets=%v", i+1, r.RateLimitType, r.LimitResetsAt)
		}
		if delay == 0 {
			if r.RetryNotBefore.Valid {
				t.Fatalf("park 4 retry=%v; want NULL", r.RetryNotBefore)
			}
			f.svc.now = func() time.Time { return now.Add(24 * time.Hour) }
			if _, err := f.svc.Sweep(f.env.ctx); err != nil {
				t.Fatal(err)
			}
			f.check(t, "limit_wait", 4, 4)
			break
		}
		due := now.Add(delay)
		if !r.RetryNotBefore.Valid || !r.RetryNotBefore.Time.Equal(due) {
			t.Fatalf("park %d retry=%v want %v", i+1, r.RetryNotBefore, due)
		}
		f.svc.now = func() time.Time { return due.Add(-time.Microsecond) }
		if _, err := f.svc.Sweep(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		f.check(t, "limit_wait", int32(i+1), int32(i+1))
		f.svc.now = func() time.Time { return due }
		if _, err := f.svc.Sweep(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		f.check(t, "queued", int32(i+1), int32(i+1))
		gen = f.nextRunningFixture(t)
		now = due
	}
	// Same owner Resume query used by the existing gateway-limit fixture.
	rows, err := f.env.q.ResumeProviderGatewayLimitWait(f.env.ctx, store.ResumeProviderGatewayLimitWaitParams{ID: f.id, UserID: f.userID})
	if err != nil || rows != 1 {
		t.Fatalf("owner resume rows=%d err=%v", rows, err)
	}
	f.restart(now)
	r := f.check(t, "queued", 0, 4)
	if r.RetryNotBefore.Valid || r.StartedAt.Valid {
		t.Fatalf("resume retry=%v started=%v", r.RetryNotBefore, r.StartedAt)
	}
	gen = f.nextRunningFixture(t)
	if rows := f.park(t, gen); rows != 1 {
		t.Fatalf("post-resume park rows=%d", rows)
	}
	f.restart(now)
	r = f.check(t, "limit_wait", 1, 5)
	if !r.RetryNotBefore.Valid || !r.RetryNotBefore.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("post-resume retry=%v want +1m", r.RetryNotBefore)
	}
}

func TestCodexProviderParkCommitBoundaryLiveDB(t *testing.T) {
	f := newCodexProviderParkFix(t)
	now := time.Now().UTC().Truncate(time.Second)
	f.restart(now)
	gen := int64(11)
	// Stop immediately before commit: execute the actual park policy and runtime statement
	// on the production claim-mutation transaction, then roll back to model lost process work.
	locked, current, tx, err := f.svc.beginClaimMutation(f.env.ctx, f.worker, f.id, &gen)
	if err != nil {
		t.Fatal(err)
	}
	if tx == nil {
		t.Fatal("park boundary requires a real transaction")
	}
	defer func() { _ = tx.Rollback(f.env.ctx) }()
	rows, err := locked.setLimitWaitLocked(f.env.ctx, current, f.worker, StateRequest{State: "limit_wait", ClaimGeneration: &gen}, pgtype.Text{})
	if err != nil || rows != 1 {
		t.Fatalf("uncommitted park rows=%d err=%v", rows, err)
	}
	inside, err := store.New(tx).GetRunByID(f.env.ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	wantCodexBinding(t, f.provFix, inside, 1, 7)
	if inside.Status != "limit_wait" || inside.ModelProviderLimitStreak != 1 || inside.LimitWaitCount != 1 || !inside.RetryNotBefore.Valid || !inside.RetryNotBefore.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("uncommitted park status=%s streak=%d waits=%d retry=%v", inside.Status, inside.ModelProviderLimitStreak, inside.LimitWaitCount, inside.RetryNotBefore)
	}
	if err := tx.Rollback(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	f.restart(now)
	r := f.check(t, "running", 0, 0)
	if r.RetryNotBefore.Valid {
		t.Fatalf("rollback left retry=%v", r.RetryNotBefore)
	}
	// Wrong generation while status is still running must not increment either counter.
	if rows := f.park(t, gen-1); rows != 0 {
		t.Fatalf("stale generation park rows=%d", rows)
	}
	f.check(t, "running", 0, 0)
	// Normal service seam commits the same park, then the fresh service sees it.
	if rows := f.park(t, gen); rows != 1 {
		t.Fatalf("committed park rows=%d", rows)
	}
	f.restart(now)
	r = f.check(t, "limit_wait", 1, 1)
	if !r.RetryNotBefore.Valid || !r.RetryNotBefore.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("committed retry=%v", r.RetryNotBefore)
	}
	// Redelivery at the current generation is rejected by status, not double counted.
	if rows := f.park(t, gen); rows != 0 {
		t.Fatalf("duplicate park rows=%d", rows)
	}
	r = f.check(t, "limit_wait", 1, 1)
	if !r.RetryNotBefore.Valid || !r.RetryNotBefore.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("duplicate changed retry=%v", r.RetryNotBefore)
	}
}
