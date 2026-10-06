package healthsvc

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestFleetDiskDindAndPending(t *testing.T) {
	stamp := func(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }
	base := store.Worker{Kind: "hosted", Ephemeral: true, LastHeartbeatAt: stamp(fixedNow), DindMeterAt: stamp(fixedNow), DindPressureStreak: 2}
	cases := []struct {
		name              string
		change            func(*store.Worker)
		affected, pending bool
	}{
		{"report only", func(w *store.Worker) {}, true, true},
		{"legacy plus dind counted once", func(w *store.Worker) { w.StatsDiskPressureStreak = 2 }, true, true},
		{"single sample", func(w *store.Worker) { w.DindPressureStreak = 1 }, false, false},
		{"boundary", func(w *store.Worker) { w.DindMeterAt = stamp(fixedNow.Add(-45 * time.Second)) }, true, true},
		{"stale meter", func(w *store.Worker) { w.DindMeterAt = stamp(fixedNow.Add(-45*time.Second - time.Nanosecond)) }, false, false},
		{"invalid meter", func(w *store.Worker) { w.DindMeterAt.Valid = false }, false, false},
		{"future meter", func(w *store.Worker) { w.DindMeterAt = stamp(fixedNow.Add(time.Nanosecond)) }, false, false},
		{"stale heartbeat", func(w *store.Worker) { w.LastHeartbeatAt = stamp(fixedNow.Add(-time.Hour)) }, false, false},
		{"invalid heartbeat", func(w *store.Worker) { w.LastHeartbeatAt.Valid = false }, false, false},
		{"future heartbeat dind", func(w *store.Worker) { w.LastHeartbeatAt = stamp(fixedNow.Add(time.Second)) }, false, false},
		{"legacy future unchanged", func(w *store.Worker) {
			w.LastHeartbeatAt = stamp(fixedNow.Add(time.Second))
			w.StatsDiskPressureStreak = 2
		}, true, false},
	}
	for _, phase := range []string{"requested", "ready", "stopping", "recycling", "complete", "cancelled"} {
		cases = append(cases, struct {
			name              string
			change            func(*store.Worker)
			affected, pending bool
		}{
			"operation " + phase, func(w *store.Worker) {
				w.MaintenanceID.Valid = true
				w.MaintenancePhase = phase
				w.LastHeartbeatAt.Valid = false
				w.DindPressureStreak = 0
			},
			false, phase == "requested" || phase == "ready" || phase == "stopping" || phase == "recycling",
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := base
			tc.change(&w)
			c := newSvc(&fakeStore{}, &fakeSettings{}).checkFleetDisk(fixedNow, []store.ListAllWorkersRow{{Worker: w}})
			wantSev := sevOK
			if tc.affected || tc.pending {
				wantSev = sevWarn
			}
			if c.Severity != wantSev {
				t.Fatalf("severity %s want %s", c.Severity, wantSev)
			}
			wantSummary := "No worker is under sustained disk pressure."
			if tc.affected {
				wantSummary = "1 worker(s) are under sustained disk pressure."
			}
			if c.Summary != wantSummary {
				t.Fatalf("summary %q want %q", c.Summary, wantSummary)
			}
			found := false
			for _, e := range c.Evidence {
				if e.Label == "Cleanup pending" {
					found = true
					if e.Value != "1 worker(s)" {
						t.Fatalf("pending evidence %q", e.Value)
					}
				}
			}
			if found != tc.pending {
				t.Fatalf("pending evidence %+v", c.Evidence)
			}
			if tc.pending && (c.Action == nil || !strings.Contains(*c.Action, "waiting safely") || !strings.Contains(*c.Action, "report-only")) {
				t.Fatalf("missing safe waits: %+v", c)
			}
		})
	}
}
