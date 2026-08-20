package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerDTODockerMapping pins the worker.docker_enabled → WorkerDTO.Docker
// convention (PRD #83 M3, PRD #76) without a database: the mapping is pure, so a
// bare store.Worker is enough to exercise every branch of boolPtrValue through the
// real DTO builder. A NULL column (an external worker, where DinD is not a concept)
// must map to JSON null; a valid column must carry the stored true/false through
// unchanged so the row badge reflects a hosted worker's actual sidecar state.
func TestWorkerDTODockerMapping(t *testing.T) {
	cases := []struct {
		name    string
		col     pgtype.Bool
		wantNil bool
		want    bool // only meaningful when wantNil is false
	}{
		{"external worker (NULL)", pgtype.Bool{Valid: false}, true, false},
		{"hosted docker-capable", pgtype.Bool{Bool: true, Valid: true}, false, true},
		{"hosted no sidecar", pgtype.Bool{Bool: false, Valid: true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dto := workerDTOFromWorker(store.Worker{DockerEnabled: tc.col}, 0, false, "", "", "", time.Now(), time.Now())
			if tc.wantNil {
				if dto.Docker != nil {
					t.Fatalf("Docker = %v, want nil for a NULL docker_enabled column", *dto.Docker)
				}
				return
			}
			if dto.Docker == nil {
				t.Fatalf("Docker = nil, want a non-nil *bool for a valid column")
			}
			if *dto.Docker != tc.want {
				t.Fatalf("*Docker = %v, want %v", *dto.Docker, tc.want)
			}
		})
	}
}

// TestWorkerDTOCarriesUpgradeClassification pins the wiring, not the rules: that BOTH
// DTO builders run the classifier and that they agree. The rules themselves are pinned
// in workersvc/upgrade_test.go, which is where a rule change belongs.
//
// Two builders exist because the list path has a credential-label join the bare-row
// path does not, and they have drifted apart before. A field added to one and forgotten
// in the other is invisible from the outside — the list endpoint would carry the badge
// while the register/heartbeat responses silently returned the zero value, so a worker
// would show up classified and then blank itself on its next beat.
func TestWorkerDTOCarriesUpgradeClassification(t *testing.T) {
	const cpVersion = "0.11.7"
	behind := pgtype.Text{String: "0.11.0", Valid: true}

	fromWorker := workerDTOFromWorker(store.Worker{Version: behind}, 0, false, "", cpVersion, "", time.Now(), time.Now())
	fromRow := workerDTOFromRow(store.ListWorkersByUserRow{Version: behind}, cpVersion, "", time.Now(), time.Now())

	for name, dto := range map[string]struct {
		status string
		detail *string
	}{
		"workerDTOFromWorker": {fromWorker.UpgradeStatus, fromWorker.UpgradeDetail},
		"workerDTOFromRow":    {fromRow.UpgradeStatus, fromRow.UpgradeDetail},
	} {
		if dto.status != workersvc.UpgradeStatusOutdated {
			t.Errorf("%s: UpgradeStatus = %q, want %q", name, dto.status, workersvc.UpgradeStatusOutdated)
		}
		if dto.detail == nil {
			t.Errorf("%s: UpgradeDetail = nil, want the sentence behind the badge", name)
			continue
		}
		if *dto.detail != "running 0.11.0, target 0.11.7" {
			t.Errorf("%s: UpgradeDetail = %q, want the running/target sentence", name, *dto.detail)
		}
	}

	// The steady state serializes as JSON null rather than an empty string, so the UI
	// renders no explanatory line at all instead of an empty one.
	current := workerDTOFromWorker(store.Worker{Version: pgtype.Text{String: cpVersion, Valid: true}}, 0, false, "", cpVersion, "", time.Now(), time.Now())
	if current.UpgradeStatus != workersvc.UpgradeStatusUpToDate {
		t.Errorf("UpgradeStatus = %q, want %q", current.UpgradeStatus, workersvc.UpgradeStatusUpToDate)
	}
	if current.UpgradeDetail != nil {
		t.Errorf("UpgradeDetail = %q, want nil for the up_to_date steady state", *current.UpgradeDetail)
	}

	// A NULL version column must not reach the classifier as a comparable value: the
	// pgtype zero value is the empty string, which is the unreported case.
	never := workerDTOFromWorker(store.Worker{Version: pgtype.Text{Valid: false}}, 0, false, "", cpVersion, "", time.Now(), time.Now())
	if never.UpgradeStatus != workersvc.UpgradeStatusUnknown {
		t.Errorf("UpgradeStatus = %q for a worker that never registered a version, want %q",
			never.UpgradeStatus, workersvc.UpgradeStatusUnknown)
	}
}
