package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerCrossCheckCapacityMapping(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		cap  pgtype.Int4
	}{
		{"legacy", pgtype.Int4{}},
		{"disabled", pgtype.Int4{Int32: 0, Valid: true}},
		{"enabled", pgtype.Int4{Int32: 16, Valid: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := store.Worker{MaxCrossCheckSlots: tc.cap}
			bare := workerDTOFromWorker(worker, 2, true, "", "", "", now, now)
			owner := workerDTOFromRow(store.ListWorkersByUserRow{MaxCrossCheckSlots: tc.cap, ActiveRuns: 2, ActiveCrossChecks: 3}, "", "", now, now)
			admin := adminWorkerDTOFromRow(store.ListAllWorkersRow{Worker: worker, ActiveRuns: 2, ActiveCrossChecks: 3}, "", "", now, now, time.Minute).WorkerDTO
			for name, dto := range map[string]apitypes.WorkerDTO{"bare": bare, "owner": owner, "admin": admin} {
				if (dto.MaxCrossCheckSlots != nil) != tc.cap.Valid {
					t.Fatalf("%s cap nullability: %+v", name, dto.MaxCrossCheckSlots)
				}
				if tc.cap.Valid && *dto.MaxCrossCheckSlots != int(tc.cap.Int32) {
					t.Fatalf("%s cap = %d", name, *dto.MaxCrossCheckSlots)
				}
				want := 3
				if name == "bare" {
					want = 0
				}
				if dto.ActiveCrossChecks != want || dto.ActiveRuns != 2 {
					t.Fatalf("%s counts runs/checks = %d/%d, want 2/%d", name, dto.ActiveRuns, dto.ActiveCrossChecks, want)
				}
			}
		})
	}
}
