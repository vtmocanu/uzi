package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M6: owner health for a run waiting on a waking worker. Skipped without
// UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

func (f *hibFx) agedWakingRun() (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	w := f.sleepingWorker()
	run := f.queuedRun()
	f.env.exec(`UPDATE runs SET status_since = now() - interval '20 minutes' WHERE id = $1`, run)
	if woke, err := f.demand(f.svc, run, w); err != nil || !woke {
		f.t.Fatalf("DemandWake = %v, %v", woke, err)
	}
	f.want(w, "waking")
	return run, w
}

func (f *hibFx) healthOf(run uuid.UUID) (string, string) {
	f.t.Helper()
	th := healthThresholds{queued: time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true}
	return f.svc.healthTargetFor(f.env.ctx, time.Now(), healthRowForRun(f.t, f.env, run), th)
}

func TestQueuedHealthWakingFromHibernationLiveDB(t *testing.T) {
	t.Run("an aged queued run with a waking worker reads as waking, not as an alarm", func(t *testing.T) {
		f := newHibFx(t)
		run, _ := f.agedWakingRun()
		flag, reason := f.healthOf(run)
		if flag != healthWaitingWorker || reason != "your worker is waking from hibernation" {
			t.Fatalf("health = (%q, %q)", flag, reason)
		}
	})
	t.Run("a failed wake falls through to the ordinary reasons", func(t *testing.T) {
		f := newHibFx(t)
		run, w := f.agedWakingRun()
		f.env.exec(`UPDATE workers SET wake_error = 'pod never ready' WHERE id = $1`, w)
		if _, reason := f.healthOf(run); reason == reasonWakingFromHibernation || reason == "" {
			t.Fatalf("reason = %q, want an ordinary queued reason", reason)
		}
	})
	t.Run("a run with no wake row never reads as waking", func(t *testing.T) {
		f := newHibFx(t)
		f.sleepingWorker()
		run := f.queuedRun()
		f.env.exec(`UPDATE runs SET status_since = now() - interval '20 minutes' WHERE id = $1`, run)
		if _, reason := f.healthOf(run); reason == reasonWakingFromHibernation {
			t.Fatalf("reason = %q without a wake row", reason)
		}
	})
}

func TestFleetCapacityIgnoresWakingWorkerLiveDB(t *testing.T) {
	waitingRows := func(f *hibFx, run uuid.UUID) int {
		f.t.Helper()
		rows, err := f.env.q.ListOwnersWaitingNoCapacity(f.env.ctx, store.ListOwnersWaitingNoCapacityParams{
			HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
			RollReason:      ReasonWorkersUpgrading,
			WakeTimeout:     pgtype.Interval{Microseconds: WorkerWakeTimeout.Microseconds(), Valid: true},
		})
		if err != nil {
			f.t.Fatalf("ListOwnersWaitingNoCapacity: %v", err)
		}
		n := 0
		for _, r := range rows {
			if r.RunID == run {
				n++
			}
		}
		return n
	}
	f := newHibFx(t)
	run, w := f.agedWakingRun()
	f.env.exec(`UPDATE runs SET health = 'waiting_worker', health_since = now() - interval '20 minutes',
	            health_reason = $2 WHERE id = $1`, run, "no online worker")
	if got := waitingRows(f, run); got != 0 {
		t.Fatalf("a run whose worker is waking inside the timeout counted as no-capacity (%d rows)", got)
	}
	f.env.exec(`UPDATE hibernation_wakes SET first_woken_at = now() - interval '16 minutes' WHERE run_id = $1`, run)
	if got := waitingRows(f, run); got != 1 {
		t.Fatalf("after the wake timeout the run must count again, got %d rows", got)
	}
	f.env.exec(`UPDATE hibernation_wakes SET first_woken_at = now() WHERE run_id = $1`, run)
	if got := waitingRows(f, run); got != 0 {
		t.Fatalf("precondition: fresh wake should be excluded, got %d", got)
	}
	f.env.exec(`UPDATE workers SET wake_error = 'pod never ready' WHERE id = $1`, w)
	if got := waitingRows(f, run); got != 1 {
		t.Fatalf("a failed wake must count again, got %d rows", got)
	}
}
