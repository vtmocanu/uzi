package healthsvc

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestFleetDiskDockerDataRecovery(t *testing.T) {
	const generic = "Free disk on the affected worker(s) or increase the worker volume size."
	const pending = "DinD cleanup pending can be report-only or waiting safely for active work and retained unpublished work to clear. An active operation can outlast fresh telemetry. Inspect uzi admin workers; wait for safe cleanup, or increase volume capacity."
	stamp := func(at time.Time) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: at, Valid: true}
	}
	base := store.Worker{
		Kind: "hosted", DockerEnabled: pgtype.Bool{Bool: true, Valid: true},
		DataPressure: true, StatsDiskPressureStreak: 2, LastHeartbeatAt: stamp(fixedNow),
	}
	plain := base
	plain.DockerEnabled.Bool = false
	tests := []struct {
		name     string
		change   func(*store.Worker)
		prefix   []store.Worker
		affected int
		pending  int
		guidance bool
	}{
		{name: "Docker data pressure", affected: 1, guidance: true},
		{name: "qualifying Docker worker later in mixed fleet", prefix: []store.Worker{plain}, affected: 2, guidance: true},
		{name: "data and fresh DinD pressure", change: func(w *store.Worker) {
			w.DindMeterAt = stamp(fixedNow)
			w.DindPressureStreak = 2
		}, affected: 1, pending: 1, guidance: true},
		{name: "data and pending operation", change: func(w *store.Worker) {
			w.MaintenanceID.Valid = true
			w.MaintenancePhase = "requested"
		}, affected: 1, pending: 1, guidance: true},
		{name: "pending operation earlier in fleet", prefix: []store.Worker{{
			MaintenanceID: pgtype.UUID{Valid: true}, MaintenancePhase: "recycling",
		}}, affected: 1, pending: 1, guidance: true},
		{name: "legacy future heartbeat", change: func(w *store.Worker) {
			w.LastHeartbeatAt = stamp(fixedNow.Add(time.Second))
		}, affected: 1, guidance: true},
		{name: "nix only", change: func(w *store.Worker) {
			w.DataPressure = false
			w.NixPressure = true
		}, affected: 1},
		{name: "plain data pressure", change: func(w *store.Worker) { w.DockerEnabled.Bool = false }, affected: 1},
		{name: "nonhosted data pressure", change: func(w *store.Worker) { w.Kind = "external" }, affected: 1},
		{name: "absent kind", change: func(w *store.Worker) { w.Kind = "" }, affected: 1},
		{name: "stale heartbeat", change: func(w *store.Worker) {
			w.LastHeartbeatAt = stamp(fixedNow.Add(-time.Hour))
		}},
		{name: "invalid heartbeat", change: func(w *store.Worker) { w.LastHeartbeatAt.Valid = false }},
		{name: "single sample", change: func(w *store.Worker) { w.StatsDiskPressureStreak = 1 }},
		{name: "no sustained pressure", change: func(w *store.Worker) { w.StatsDiskPressureStreak = 0 }},
		{name: "invalid Docker config with true value", change: func(w *store.Worker) {
			w.DockerEnabled.Valid = false
		}, affected: 1},
		{name: "absent Docker config", change: func(w *store.Worker) {
			w.DockerEnabled = pgtype.Bool{}
		}, affected: 1},
		{name: "DinD alone does not qualify data sample", change: func(w *store.Worker) {
			w.StatsDiskPressureStreak = 1
			w.DindMeterAt = stamp(fixedNow)
			w.DindPressureStreak = 2
		}, affected: 1, pending: 1},
		{name: "stale data with pending operation", change: func(w *store.Worker) {
			w.LastHeartbeatAt = stamp(fixedNow.Add(-time.Hour))
			w.MaintenanceID.Valid = true
			w.MaintenancePhase = "ready"
		}, pending: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := base
			if tt.change != nil {
				tt.change(&w)
			}
			var rows []store.ListAllWorkersRow
			for _, earlier := range tt.prefix {
				rows = append(rows, store.ListAllWorkersRow{Worker: earlier})
			}
			rows = append(rows, store.ListAllWorkersRow{Worker: w})
			c := newSvc(&fakeStore{}, &fakeSettings{}).checkFleetDisk(fixedNow, rows)
			wantSeverity := sevOK
			wantSummary := "No worker is under sustained disk pressure."
			wantEvidence := []apitypes.HealthEvidenceDTO{}
			wantAction := ""
			if tt.affected > 0 {
				wantSeverity = sevWarn
				wantSummary = fmt.Sprintf("%d worker(s) are under sustained disk pressure.", tt.affected)
				wantEvidence = append(wantEvidence, apitypes.HealthEvidenceDTO{Label: "Workers", Value: fmt.Sprint(tt.affected)})
				wantAction = generic
			}
			if tt.pending > 0 {
				wantSeverity = sevWarn
				wantEvidence = append(wantEvidence, apitypes.HealthEvidenceDTO{Label: "Cleanup pending", Value: fmt.Sprintf("%d worker(s)", tt.pending)})
				wantAction = pending
			}
			if c.Severity != wantSeverity || c.Summary != wantSummary || !reflect.DeepEqual(c.Evidence, wantEvidence) {
				t.Fatalf("legacy health changed: severity=%q summary=%q evidence=%+v; want %q, %q, %+v",
					c.Severity, c.Summary, c.Evidence, wantSeverity, wantSummary, wantEvidence)
			}
			if !tt.guidance {
				if wantAction == "" {
					if c.Action != nil {
						t.Fatalf("unexpected action %q", *c.Action)
					}
				} else if c.Action == nil || *c.Action != wantAction {
					t.Fatalf("legacy action changed: got %v, want %q", c.Action, wantAction)
				}
				return
			}
			if c.Action == nil {
				t.Fatal("missing recovery action")
			}
			suffix, ok := strings.CutPrefix(*c.Action, wantAction+" ")
			if !ok {
				t.Fatalf("action must preserve exact selected advice %q: %q", wantAction, *c.Action)
			}
			for _, procedure := range []string{
				"Automatic Docker data-PVC pressure recycling is disabled",
				"protection requires the fixed controller deployment",
				"Preserve work first",
				"uzi run recovery to identify open holds",
				"uzi run export <run-id> --output FILE for available archives",
				"For retained sources with no open hold, use worker-local inspect first",
				"docs/run-recovery.md, Retained sources with no open hold (worker-local inspect and discard)",
				"Stop if inspect is refused, source coverage is unproven, or removal fails with not permitted",
				"Discard only through supported hold discard (uzi run discard <run-id> --hold <hold-id> --yes) or worker-local discard after its checks",
				"Reviewing and resolving held work in docs/run-recovery.md",
				"Enlarge the PVC where the StorageClass allows it",
				"Do not delete the PVC or pod by hand to free space",
			} {
				if !strings.Contains(suffix, procedure) {
					t.Errorf("missing procedure %q in appended action %q", procedure, suffix)
				}
			}
			if strings.Count(suffix, "Automatic Docker data-PVC pressure recycling") != 1 {
				t.Errorf("recovery guidance must be appended once: %q", suffix)
			}
		})
	}
}
