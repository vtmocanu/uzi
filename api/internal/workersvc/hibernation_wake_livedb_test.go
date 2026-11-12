package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M5: DemandWake over a real database. Skipped without UZI_TEST_DATABASE_URL (run via
// ./e2e/run-store-it.sh).

// sleep drives an online worker to hibernated.
func (f *hibFx) sleep(id, pod uuid.UUID) {
	f.t.Helper()
	f.hibernate(id)
	f.mustApply(id, TransitionScalingDown, pod)
	f.mustApply(id, TransitionScaledDown, uuid.Nil)
	f.want(id, "hibernated")
}

// sleepingWorker seeds a worker and puts it to sleep.
func (f *hibFx) sleepingWorker() uuid.UUID {
	f.t.Helper()
	id, pod := f.worker()
	f.sleep(id, pod)
	return id
}

func (f *hibFx) wakeRows(run uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM hibernation_wakes WHERE run_id = $1`, run).Scan(&n); err != nil {
		f.t.Fatalf("count wakes: %v", err)
	}
	return n
}

func (f *hibFx) wakeWorker(run uuid.UUID) uuid.UUID {
	f.t.Helper()
	var w uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT worker_id FROM hibernation_wakes WHERE run_id = $1`, run).Scan(&w); err != nil {
		f.t.Fatalf("wake worker: %v", err)
	}
	return w
}

// demand reports whether the call recorded a wake row (woke or attached).
func (f *hibFx) demand(svc *Service, run, worker uuid.UUID) (bool, error) {
	res, err := svc.DemandWake(f.env.ctx, run, worker)
	return res != DemandWakeNone, err
}

// faultBeginner wraps every transaction it opens so a test can fail the commit in a chosen way.
type faultBeginner struct {
	pool   *pgxpool.Pool
	commit func(ctx context.Context, tx pgx.Tx, pid int32) error
}

type faultTx struct {
	pgx.Tx
	pid    int32
	commit func(ctx context.Context, tx pgx.Tx, pid int32) error
}

func (t *faultTx) Commit(ctx context.Context) error { return t.commit(ctx, t.Tx, t.pid) }

func (b faultBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return &faultTx{Tx: tx, pid: pid, commit: b.commit}, nil
}

func (f *hibFx) faultySvc(commit func(ctx context.Context, tx pgx.Tx, pid int32) error) *Service {
	s := New(f.env.q, f.env.box, testParams())
	s.SetTxBeginner(faultBeginner{pool: f.env.pool, commit: commit})
	s.SetHibernationSettings(f.set)
	return s
}

var errDemandWakeInjected = errors.New("injected failure before commit")

func TestDemandWakeLiveDB(t *testing.T) {
	t.Run("a hibernated worker wakes once per run: fresh op, one row", func(t *testing.T) {
		f := newHibFx(t)
		w := f.sleepingWorker()
		run := f.queuedRun()
		before := f.op(w)
		woke, err := f.demand(f.svc, run, w)
		if err != nil || !woke {
			t.Fatalf("DemandWake = %v, %v; want true, nil", woke, err)
		}
		f.want(w, "waking")
		if f.op(w) == before {
			t.Fatalf("a wake must mint a fresh op id; still %s", before)
		}
		if f.wakeRows(run) != 1 || f.wakeWorker(run) != w {
			t.Fatalf("want exactly one wake row naming %s", w)
		}
		if reason := f.row(w).HibernationReason.String; reason != "demand: run "+run.String() {
			t.Fatalf("reason = %q", reason)
		}
	})

	t.Run("a preparing worker with demand is withdrawn to waking", func(t *testing.T) {
		f := newHibFx(t)
		w, _ := f.worker()
		f.hibernate(w)
		f.want(w, "preparing")
		run := f.queuedRun()
		woke, err := f.demand(f.svc, run, w)
		if err != nil || !woke {
			t.Fatalf("DemandWake = %v, %v", woke, err)
		}
		f.want(w, "waking")
		if f.wakeRows(run) != 1 {
			t.Fatal("no wake row recorded for the withdrawn worker")
		}
	})

	t.Run("repeated passes reuse the row and never wake a sibling", func(t *testing.T) {
		f := newHibFx(t)
		a, b := f.sleepingWorker(), f.sleepingWorker()
		run := f.queuedRun()
		if woke, err := f.demand(f.svc, run, a); err != nil || !woke {
			t.Fatalf("first = %v, %v", woke, err)
		}
		op := f.op(a)
		for i := 0; i < 3; i++ {
			for _, cand := range []uuid.UUID{a, b} {
				if woke, err := f.demand(f.svc, run, cand); err != nil || woke {
					t.Fatalf("repeat for %s = %v, %v; want false, nil", cand, woke, err)
				}
			}
		}
		if f.op(a) != op {
			t.Fatal("a repeat minted a new op id")
		}
		f.want(b, "hibernated")
		if f.wakeRows(run) != 1 || f.wakeWorker(run) != a {
			t.Fatal("the run's wake row changed")
		}
	})

	t.Run("one woken worker serves several runs", func(t *testing.T) {
		f := newHibFx(t)
		a := f.sleepingWorker()
		r1, r2, r3 := f.queuedRun(), f.queuedRun(), f.queuedRun()
		if woke, err := f.demand(f.svc, r1, a); err != nil || !woke {
			t.Fatalf("r1 = %v, %v", woke, err)
		}
		op := f.op(a)
		for _, r := range []uuid.UUID{r2, r3} {
			if woke, err := f.demand(f.svc, r, a); err != nil || !woke {
				t.Fatalf("run %s = %v, %v; want a new row on the already-waking worker", r, woke, err)
			}
		}
		if f.op(a) != op {
			t.Fatal("an already-waking worker must keep its op id")
		}
		for _, r := range []uuid.UUID{r1, r2, r3} {
			if f.wakeRows(r) != 1 || f.wakeWorker(r) != a {
				t.Fatalf("run %s has no row naming %s", r, a)
			}
		}
	})

	t.Run("not a candidate: awake worker, other owner, run no longer queued", func(t *testing.T) {
		f := newHibFx(t)
		run := f.queuedRun()
		awake, _ := f.worker()
		if woke, err := f.demand(f.svc, run, awake); err != nil || woke {
			t.Fatalf("awake worker: %v, %v", woke, err)
		}
		sleeper := f.sleepingWorker()
		other := f.env.seedUser(t)
		t.Cleanup(func() { f.env.exec(`DELETE FROM users WHERE id = $1`, other) })
		f.env.exec(`UPDATE runs SET user_id = $2 WHERE id = $1`, run, other)
		if woke, err := f.demand(f.svc, run, sleeper); err != nil || woke {
			t.Fatalf("other owner: %v, %v", woke, err)
		}
		f.env.exec(`UPDATE runs SET user_id = $2, status = 'cancelled' WHERE id = $1`, run, f.user)
		if woke, err := f.demand(f.svc, run, sleeper); err != nil || woke {
			t.Fatalf("terminal run: %v, %v", woke, err)
		}
		f.want(sleeper, "hibernated")
		if f.wakeRows(run) != 0 {
			t.Fatal("a refused demand left a row")
		}
	})

	t.Run("injected error after the insert and before commit: no row, worker unchanged, next pass wakes", func(t *testing.T) {
		f := newHibFx(t)
		w := f.sleepingWorker()
		run := f.queuedRun()
		op := f.op(w)
		bad := f.faultySvc(func(ctx context.Context, tx pgx.Tx, _ int32) error {
			_ = tx.Rollback(ctx)
			return errDemandWakeInjected
		})
		if _, err := f.demand(bad, run, w); !errors.Is(err, errDemandWakeInjected) {
			t.Fatalf("err = %v, want the injected failure", err)
		}
		if f.wakeRows(run) != 0 || f.state(w) != "hibernated" || f.op(w) != op {
			t.Fatalf("the failed transaction left state behind: rows=%d state=%s", f.wakeRows(run), f.state(w))
		}
		if woke, err := f.demand(f.svc, run, w); err != nil || !woke {
			t.Fatalf("next pass = %v, %v; want it to wake", woke, err)
		}
		f.want(w, "waking")
	})

	t.Run("lost ack: the commit lands, the pass sees an error, the next pass reuses it", func(t *testing.T) {
		f := newHibFx(t)
		a, b := f.sleepingWorker(), f.sleepingWorker()
		run := f.queuedRun()
		lost := f.faultySvc(func(ctx context.Context, tx pgx.Tx, _ int32) error {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return errDemandWakeInjected
		})
		if _, err := f.demand(lost, run, a); err == nil {
			t.Fatal("a lost commit acknowledgement must surface as an error")
		}
		f.want(a, "waking")
		op := f.op(a)
		if f.wakeRows(run) != 1 {
			t.Fatal("the commit that landed left no row")
		}
		// The next pass may pick either candidate; both must reuse the recorded wake.
		for _, cand := range []uuid.UUID{a, b} {
			if woke, err := f.demand(f.svc, run, cand); err != nil || woke {
				t.Fatalf("retry for %s = %v, %v; want a reuse", cand, woke, err)
			}
		}
		if f.op(a) != op {
			t.Fatal("the retry minted a new op id")
		}
		f.want(b, "hibernated")
		suppressed, err := f.env.q.HibernationWakeSuppresses(f.env.ctx, store.HibernationWakeSuppressesParams{
			CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
			CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
			RunID:                    run,
			WakeTimeout:              pgtype.Interval{Microseconds: WorkerWakeTimeout.Microseconds(), Valid: true},
		})
		if err != nil || !suppressed {
			t.Fatalf("the run must stay suppressed after the lost ack: %v, %v", suppressed, err)
		}
	})

	t.Run("backend terminated after the insert: rollback", func(t *testing.T) {
		f := newHibFx(t)
		w := f.sleepingWorker()
		run := f.queuedRun()
		op := f.op(w)
		killer := f.faultySvc(func(ctx context.Context, tx pgx.Tx, pid int32) error {
			var ok bool
			if err := f.env.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&ok); err != nil || !ok {
				t.Errorf("pg_terminate_backend(%d) = %v, %v", pid, ok, err)
			}
			return tx.Commit(ctx)
		})
		if _, err := f.demand(killer, run, w); err == nil {
			t.Fatal("a terminated backend must fail the demand")
		}
		if f.wakeRows(run) != 0 || f.state(w) != "hibernated" || f.op(w) != op {
			t.Fatalf("termination was not a rollback: rows=%d state=%s", f.wakeRows(run), f.state(w))
		}
		if woke, err := f.demand(f.svc, run, w); err != nil || !woke {
			t.Fatalf("next pass = %v, %v", woke, err)
		}
	})

	t.Run("concurrent passes on one run: exactly one row and one waking worker", func(t *testing.T) {
		f := newHibFx(t)
		a, b := f.sleepingWorker(), f.sleepingWorker()
		run := f.queuedRun()
		var wg sync.WaitGroup
		var mu sync.Mutex
		recorded := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			cand := a
			if i%2 == 1 {
				cand = b
			}
			go func() {
				defer wg.Done()
				woke, err := f.demand(f.svc, run, cand)
				if err != nil {
					t.Errorf("DemandWake: %v", err)
					return
				}
				if woke {
					mu.Lock()
					recorded++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if recorded != 1 || f.wakeRows(run) != 1 {
			t.Fatalf("recorded=%d rows=%d, want exactly one", recorded, f.wakeRows(run))
		}
		waking := 0
		for _, id := range []uuid.UUID{a, b} {
			if f.state(id) == "waking" {
				waking++
			}
		}
		if waking != 1 {
			t.Fatalf("%d workers waking, want exactly 1", waking)
		}
	})

	t.Run("a row whose worker settled and was re-hibernated is kept: no second wake, first_woken_at unchanged", func(t *testing.T) {
		f := newHibFx(t)
		a, b := f.sleepingWorker(), f.sleepingWorker()
		run := f.queuedRun()
		if res, err := f.svc.DemandWake(f.env.ctx, run, a); err != nil || res != DemandWakeWoke {
			t.Fatalf("first wake: %v, %v", res, err)
		}
		var first time.Time
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT first_woken_at FROM hibernation_wakes WHERE run_id = $1`, run).Scan(&first); err != nil {
			t.Fatal(err)
		}
		// The woken worker settles awake and is hibernated again, with the row still naming it.
		pod := uuid.New()
		f.mustApply(a, TransitionReady, pod)
		f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, a, pod)
		f.heartbeatNow(a)
		f.register(a, &pod)
		f.want(a, "none")
		// HibernateWorker now refuses this sequence (pending_wake_demand), so write the state that a
		// pre-refusal api, or a row that outlived its guard, could have left, to prove DemandWake's own
		// one-wake-per-run guard still holds.
		f.env.exec(`UPDATE workers SET hibernation_state = 'hibernated', hibernation_op_id = gen_random_uuid(), hibernated_at = now() WHERE id = $1`, a)
		f.want(a, "hibernated")
		opA, opB := f.op(a), f.op(b)
		for _, cand := range []uuid.UUID{a, b} {
			if res, err := f.svc.DemandWake(f.env.ctx, run, cand); err != nil || res != DemandWakeNone {
				t.Fatalf("second demand for %s = %v, %v; want none", cand, res, err)
			}
		}
		// Release also leaves it: the run is still queued.
		if n, err := f.svc.ReleaseStaleWake(f.env.ctx, run); err != nil || n {
			t.Fatalf("release deleted a row of a still-queued run: %v, %v", n, err)
		}
		f.want(a, "hibernated")
		f.want(b, "hibernated")
		if f.op(a) != opA || f.op(b) != opB {
			t.Fatal("a second wake minted an op id")
		}
		var after time.Time
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT first_woken_at FROM hibernation_wakes WHERE run_id = $1`, run).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if f.wakeRows(run) != 1 || f.wakeWorker(run) != a || !after.Equal(first) {
			t.Fatalf("the row changed: rows=%d worker=%s first %v -> %v", f.wakeRows(run), f.wakeWorker(run), first, after)
		}
	})

	t.Run("a run claimed between listing and DemandWake is not woken for", func(t *testing.T) {
		f := newHibFx(t)
		w := f.sleepingWorker()
		run := f.queuedRun()
		// A claim in flight: the run row is updated but not yet committed.
		claim, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = claim.Rollback(f.env.ctx) }()
		if _, err := claim.Exec(f.env.ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, run); err != nil {
			t.Fatal(err)
		}
		type out struct {
			res DemandWakeResult
			err error
		}
		done := make(chan out, 1)
		go func() {
			res, err := f.svc.DemandWake(f.env.ctx, run, w)
			done <- out{res, err}
		}()
		select {
		case o := <-done:
			t.Fatalf("DemandWake did not wait for the run lock: %v, %v", o.res, o.err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := claim.Commit(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		o := <-done
		if o.err != nil || o.res != DemandWakeNone {
			t.Fatalf("DemandWake = %v, %v; want none", o.res, o.err)
		}
		f.want(w, "hibernated")
		if f.wakeRows(run) != 0 {
			t.Fatal("a row was recorded for a claimed run")
		}
	})

	t.Run("a waking worker whose wake failed does not capture demand", func(t *testing.T) {
		f := newHibFx(t)
		w := f.sleepingWorker()
		first := f.queuedRun()
		if res, err := f.svc.DemandWake(f.env.ctx, first, w); err != nil || res != DemandWakeWoke {
			t.Fatalf("wake: %v, %v", res, err)
		}
		f.env.exec(`UPDATE workers SET wake_error = 'scale up failed' WHERE id = $1`, w)
		second := f.queuedRun()
		if res, err := f.svc.DemandWake(f.env.ctx, second, w); err != nil || res != DemandWakeNone {
			t.Fatalf("a failed-wake worker captured demand: %v, %v", res, err)
		}
		if f.wakeRows(second) != 0 {
			t.Fatal("a row was recorded against a failed wake")
		}
	})

	t.Run("ReleaseStaleWake removes only a row whose run left the queue", func(t *testing.T) {
		f := newHibFx(t)
		a := f.sleepingWorker()
		live, gone := f.queuedRun(), f.queuedRun()
		for _, r := range []uuid.UUID{live, gone} {
			if woke, err := f.demand(f.svc, r, a); err != nil || !woke {
				t.Fatalf("wake: %v, %v", woke, err)
			}
		}
		if n, err := f.svc.ReleaseStaleWake(f.env.ctx, live); err != nil || n {
			t.Fatalf("a live wake was released: %v, %v", n, err)
		}
		f.env.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, gone)
		if n, err := f.svc.ReleaseStaleWake(f.env.ctx, gone); err != nil || !n {
			t.Fatalf("a wake for a run that left the queue was kept: %v, %v", n, err)
		}
		f.env.exec(`UPDATE workers SET hibernation_state = 'hibernated', hibernated_at = now() WHERE id = $1`, a)
		if n, err := f.svc.ReleaseStaleWake(f.env.ctx, live); err != nil || n {
			t.Fatalf("a wake whose worker stopped waking was released while its run is queued: %v, %v", n, err)
		}
		if f.wakeRows(live) != 1 || f.wakeRows(gone) != 0 {
			t.Fatalf("rows: live=%d gone=%d", f.wakeRows(live), f.wakeRows(gone))
		}
	})
}

func TestHibernateRefusedWhileAQueuedRunWokeTheWorkerLiveDB(t *testing.T) {
	f := newHibFx(t)
	w := f.sleepingWorker()
	run := f.queuedRun()
	if res, err := f.svc.DemandWake(f.env.ctx, run, w); err != nil || res != DemandWakeWoke {
		t.Fatalf("wake: %v, %v", res, err)
	}
	// Settle the worker awake with the run still queued and still holding its wake row.
	pod := uuid.New()
	f.mustApply(w, TransitionReady, pod)
	f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, w, pod)
	f.heartbeatNow(w)
	f.register(w, &pod)
	f.want(w, "none")

	_, err := f.svc.HibernateWorker(f.env.ctx, f.actor, w, "")
	if got := refusalCode(err); got != RefusalPendingWakeDemand {
		t.Fatalf("hibernate with a queued run that woke the worker = %q (%v), want %q", got, err, RefusalPendingWakeDemand)
	}
	f.want(w, "none")
	if f.wakeRows(run) != 1 {
		t.Fatal("the refusal released the wake row")
	}

	// The run leaves the queue (claimed away): the row is no longer pending demand.
	f.env.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, run)
	if _, err := f.svc.HibernateWorker(f.env.ctx, f.actor, w, ""); err != nil {
		t.Fatalf("hibernate after the run left the queue: %v", err)
	}
	f.want(w, "preparing")
}
