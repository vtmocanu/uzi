package workersvc

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIdleStallBackoffAndStopLiveDB(t *testing.T) {
	for _, stop := range []bool{false, true} {
		e := setupInterlockLiveDB(t)
		svc := e.diskParkService(t, 3)
		svc.p.RunCodexIdleStallMaxParks = 3
		w := e.seedWorker(t, nil)
		id := e.fpSeedRunning(t, w, 1)
		e.exec(t, "UPDATE runs SET recovery_wait_count=5, idle_stall_park_count=1, provider_park_count=4, provider_park_streak_started_at=now() WHERE id=$1", id)
		if stop {
			e.exec(t, "UPDATE runs SET stop_kind='cancelled' WHERE id=$1", id)
		}
		start := time.Now()
		r, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, idleReport(1, i32Ptr(2)))
		if err != nil || !ok {
			t.Fatalf("park %v %v", ok, err)
		}
		if stop {
			if r.Status != "cancelled" || r.IdleStallParkCount != 1 || r.RecoveryWaitCount != 5 || r.ProviderParkCount != 4 {
				t.Fatalf("stop lost precedence %+v", r)
			}
		} else {
			delay := r.RecoveryRetryNotBefore.Time.Sub(start)
			if r.IdleStallParkCount != 1 || r.RecoveryWaitCount != 6 || r.ProviderParkCount != 0 ||
				!r.RecoveryRetryNotBefore.Valid || delay < 30*time.Minute+recoveryParkJitterMin-time.Second || delay > 30*time.Minute+recoveryParkJitterMax+time.Second {
				t.Fatalf("backoff must use prior ordinary count: %+v delay=%s", r, delay)
			}
		}
	}
}

func TestIdleStallMaintenanceTagLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	for _, held := range []bool{false, true} {
		id := e.fpSeedRunning(t, w, 1)
		e.exec(t, "UPDATE runs SET status='recovery_wait', status_since='1900-01-01', recovery_wait_cause='codex_idle_stall', recovery_retry_not_before=CASE WHEN $2::boolean THEN NULL ELSE now()+interval '1 hour' END WHERE id=$1", id, held)
		rows, err := e.q.ListMaintenanceHeldRuns(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rows {
			if r.RunID == id {
				found = true
				want := "progressing"
				if held {
					want = "needs_owner"
				}
				if r.Tag != want {
					t.Fatalf("held=%v tag=%s", held, r.Tag)
				}
			}
		}
		if !found {
			t.Fatal("idle run missing from maintenance")
		}
	}
}
