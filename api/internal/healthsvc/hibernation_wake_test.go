package healthsvc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestFleetHibernationWake(t *testing.T) {
	stamp := func(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }
	hosted := func(name string, mut func(*store.Worker)) store.ListAllWorkersRow {
		w := store.Worker{Kind: "hosted", Name: name, HibernationState: "none"}
		if mut != nil {
			mut(&w)
		}
		return store.ListAllWorkersRow{Worker: w}
	}
	waking := func(age time.Duration) func(*store.Worker) {
		return func(w *store.Worker) {
			w.HibernationState = "waking"
			w.WakeRequestedAt = stamp(fixedNow.Add(-age))
		}
	}
	svc := newSvc(&fakeStore{}, &fakeSettings{})

	t.Run("na without hosted workers", func(t *testing.T) {
		c := New(Config{}).checkFleetHibernationWake(fixedNow, []store.ListAllWorkersRow{{Worker: store.Worker{Kind: "docker"}}})
		if c.Severity != sevNA || c.Scope != "instance" || c.Group != groupWorkers {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("ok for a fresh wake and an awake worker", func(t *testing.T) {
		c := svc.checkFleetHibernationWake(fixedNow, []store.ListAllWorkersRow{
			hosted("a", waking(workersvc.WorkerWakeTimeout-time.Minute)), hosted("b", nil),
		})
		if c.Severity != sevOK || len(c.Evidence) != 0 {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("warn for a wake past the timeout", func(t *testing.T) {
		c := svc.checkFleetHibernationWake(fixedNow, []store.ListAllWorkersRow{hosted("slow", waking(workersvc.WorkerWakeTimeout+time.Minute))})
		if c.Severity != sevWarn || len(c.Evidence) != 2 || c.Evidence[0].Value != "1" || c.Evidence[1].Value != "slow" || c.Since == nil {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("warn for a wake error even when fresh", func(t *testing.T) {
		row := hosted("bad", waking(time.Minute))
		row.Worker.WakeError = pgtype.Text{String: "pod never ready", Valid: true}
		c := svc.checkFleetHibernationWake(fixedNow, []store.ListAllWorkersRow{row})
		if c.Severity != sevWarn || len(c.Evidence) != 2 {
			t.Fatalf("got %+v", c)
		}
		for _, e := range c.Evidence {
			if strings.Contains(e.Value, "pod never ready") || strings.Contains(c.Summary, "pod never ready") {
				t.Fatalf("wake_error text leaked: %+v", c)
			}
		}
	})
	t.Run("evidence is bounded and names are sanitized", func(t *testing.T) {
		var rows []store.ListAllWorkersRow
		for i := 0; i < 15; i++ {
			rows = append(rows, hosted(fmt.Sprintf("w%d\x1b[31m", i), waking(time.Hour)))
		}
		c := svc.checkFleetHibernationWake(fixedNow, rows)
		if len(c.Evidence) != 1+maxQuarantineEvidence || c.Evidence[0].Value != "15" {
			t.Fatalf("evidence = %+v", c.Evidence)
		}
		for _, e := range c.Evidence {
			if strings.ContainsRune(e.Value, 0x1b) {
				t.Fatalf("control byte in %q", e.Value)
			}
		}
	})
}

// TestFleetCapacityPassesWakeTimeout pins the wiring of the transient-wake window: the capacity
// query must receive workersvc.WorkerWakeTimeout as a valid interval, else a stale wake would
// never expire out of the "waking" exemption.
func TestFleetCapacityPassesWakeTimeout(t *testing.T) {
	f := &fakeStore{}
	svc := newSvc(f, &fakeSettings{})
	svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if len(f.capacityParams) != 1 {
		t.Fatalf("ListOwnersWaitingNoCapacity calls = %d, want 1", len(f.capacityParams))
	}
	got := f.capacityParams[0].WakeTimeout
	if !got.Valid || got.Microseconds != workersvc.WorkerWakeTimeout.Microseconds() {
		t.Fatalf("WakeTimeout = %+v, want valid %v", got, workersvc.WorkerWakeTimeout)
	}
}
