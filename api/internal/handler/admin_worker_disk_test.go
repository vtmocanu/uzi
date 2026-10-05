package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAdminWorkersDiskResponse(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	st := &runsStore{allWorkers: []store.ListAllWorkersRow{
		{Worker: store.Worker{Name: "report-only", Ephemeral: true, LastHeartbeatAt: pgtype.Timestamptz{Time: now, Valid: true}, DindMeterAt: pgtype.Timestamptz{Time: now, Valid: true}, DindPressureStreak: 2}, OwnerEmail: "owner@example.com"},
		{Worker: store.Worker{Name: "waiting", MaintenanceID: pgtype.UUID{Valid: true}, MaintenancePhase: "stopping"}},
		{Worker: store.Worker{Name: "clear"}},
	}}
	h := newRunsHandler(t, st)
	h.now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	h.AdminListWorkers(rec, httptest.NewRequest(http.MethodGet, "/api/admin/workers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Workers []map[string]json.RawMessage `json:"workers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Workers) != 3 {
		t.Fatalf("workers: %s", rec.Body.String())
	}
	for i, want := range []struct{ volumes, pending string }{{`["dind"]`, "true"}, {`[]`, "true"}, {`[]`, "false"}} {
		w := body.Workers[i]
		if string(w["disk_pressure_volumes"]) != want.volumes || string(w["cleanup_pending"]) != want.pending {
			t.Fatalf("worker %d: %s", i, rec.Body.String())
		}
	}
}

func TestAdminWorkerDiskEvidence(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	stamp := func(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }
	base := store.Worker{Kind: "hosted", LastHeartbeatAt: stamp(now), DindMeterAt: stamp(now), DindPressureStreak: 2}
	cases := []struct {
		name    string
		change  func(*store.Worker)
		volumes []string
		pending bool
	}{
		{"dind", func(w *store.Worker) {}, []string{"dind"}, true},
		{"ephemeral report only", func(w *store.Worker) { w.Ephemeral = true }, []string{"dind"}, true},
		{"toggle off report only", func(w *store.Worker) { w.DockerEnabled = pgtype.Bool{Bool: false, Valid: true} }, []string{"dind"}, true},
		{"stable order", func(w *store.Worker) { w.StatsDiskPressureStreak = 2; w.NixPressure = true; w.DataPressure = true }, []string{"nix", "data", "dind"}, true},
		{"legacy only", func(w *store.Worker) {
			w.DindPressureStreak = 0
			w.StatsDiskPressureStreak = 2
			w.NixPressure = true
			w.DataPressure = true
		}, []string{"nix", "data"}, false},
		{"legacy debounce", func(w *store.Worker) {
			w.DindPressureStreak = 0
			w.StatsDiskPressureStreak = 1
			w.NixPressure = true
			w.DataPressure = true
		}, []string{}, false},
		{"legacy no component", func(w *store.Worker) { w.DindPressureStreak = 0; w.StatsDiskPressureStreak = 2 }, []string{}, false},
		{"single sample", func(w *store.Worker) { w.DindPressureStreak = 1 }, []string{}, false},
		{"meter boundary", func(w *store.Worker) { w.DindMeterAt = stamp(now.Add(-45 * time.Second)) }, []string{"dind"}, true},
		{"stale meter", func(w *store.Worker) { w.DindMeterAt = stamp(now.Add(-45*time.Second - time.Nanosecond)) }, []string{}, false},
		{"invalid meter", func(w *store.Worker) { w.DindMeterAt.Valid = false }, []string{}, false},
		{"future meter", func(w *store.Worker) { w.DindMeterAt = stamp(now.Add(time.Nanosecond)) }, []string{}, false},
		{"stale heartbeat", func(w *store.Worker) { w.LastHeartbeatAt = stamp(now.Add(-time.Minute)) }, []string{}, false},
		{"invalid heartbeat", func(w *store.Worker) { w.LastHeartbeatAt.Valid = false }, []string{}, false},
		{"future heartbeat", func(w *store.Worker) { w.LastHeartbeatAt = stamp(now.Add(time.Nanosecond)) }, []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := base
			tc.change(&w)
			dto := adminWorkerDTOFromRow(store.ListAllWorkersRow{Worker: w, OwnerEmail: "owner@example.com"}, "", "", now, now, 45*time.Second)
			if !reflect.DeepEqual(dto.DiskPressureVolumes, tc.volumes) || dto.CleanupPending != tc.pending {
				t.Fatalf("volumes=%v pending=%v, want %v %v", dto.DiskPressureVolumes, dto.CleanupPending, tc.volumes, tc.pending)
			}
			raw, err := json.Marshal(dto.WorkerDTO)
			if err != nil {
				t.Fatal(err)
			}
			var owner map[string]json.RawMessage
			if err := json.Unmarshal(raw, &owner); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"owner_email", "disk_pressure_volumes", "cleanup_pending"} {
				if _, ok := owner[key]; ok {
					t.Fatalf("owner DTO leaked %s", key)
				}
			}
		})
	}
	for _, phase := range []string{"requested", "ready", "stopping", "recycling", "complete", "cancelled", ""} {
		t.Run("operation "+phase, func(t *testing.T) {
			w := base
			w.LastHeartbeatAt.Valid = false
			w.DindMeterAt.Valid = false
			w.MaintenanceID.Valid = true
			w.MaintenancePhase = phase
			want := phase == "requested" || phase == "ready" || phase == "stopping" || phase == "recycling"
			dto := adminWorkerDTOFromRow(store.ListAllWorkersRow{Worker: w}, "", "", now, now, time.Minute)
			if dto.CleanupPending != want || len(dto.DiskPressureVolumes) != 0 {
				t.Fatalf("phase %s: %+v", phase, dto)
			}
			w.MaintenanceID.Valid = false
			dto = adminWorkerDTOFromRow(store.ListAllWorkersRow{Worker: w}, "", "", now, now, time.Minute)
			if dto.CleanupPending {
				t.Fatal("phase without operation marked pending")
			}
		})
	}
}
