package handler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
			dto := workerDTOFromWorker(store.Worker{DockerEnabled: tc.col}, 0, false)
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
