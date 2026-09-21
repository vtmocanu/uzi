package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOWallParkFields pins the widened run-detail DTO for a wall park (PRD #1497 M1): a paused
// budget_exhausted run carries budget_total_seconds (INCLUDING the finalize term), a
// budget_used_seconds FROZEN at status_since (not drifting from now), budget_finalize_seconds, and a
// can_stop_at_wall computed by its FULL gate. worker_name is wired separately by the handler
// (runs_lifecycle.go via WorkerNameForRun / the list-row column), not by runToDTO, so it is covered
// by that path rather than here.
func TestRunToDTOWallParkFields(t *testing.T) {
	// started 2h before dtoTestNow, parked (status_since) 1h before it → 1h of active time, frozen.
	started := dtoTestNow.Add(-2 * time.Hour)
	parked := dtoTestNow.Add(-1 * time.Hour)

	base := func() store.Run {
		return store.Run{
			ID:                     uuid.New(),
			Status:                 "paused",
			Kind:                   "issue",
			Interactive:            false,
			HoldReason:             pgtype.Text{String: "budget_exhausted", Valid: true},
			StartedAt:              pgtype.Timestamptz{Time: started, Valid: true},
			StatusSince:            pgtype.Timestamptz{Time: parked, Valid: true},
			BudgetWallSeconds:      pgtype.Int4{Int32: 3600, Valid: true},
			BudgetExtensionSeconds: 1800,
			BudgetFinalizeSeconds:  0,
			BudgetPausedSeconds:    0,
			MilestonesCompleted:    []byte(`["m1"]`),
		}
	}

	t.Run("budget total/used/finalize and can_stop_at_wall true", func(t *testing.T) {
		dto := runToDTO(base(), "normal", 0, 0, 0, dtoTestNow)
		if dto.BudgetTotalSeconds == nil || *dto.BudgetTotalSeconds != 3600+1800+0 {
			t.Fatalf("budget_total_seconds = %v, want 5400 (wall+extension+finalize)", dto.BudgetTotalSeconds)
		}
		// FROZEN at status_since: 1h of active time, NOT the 2h that now-started would give.
		if dto.BudgetUsedSeconds == nil || *dto.BudgetUsedSeconds != 3600 {
			t.Fatalf("budget_used_seconds = %v, want 3600 (frozen at status_since, not drifting from now)", dto.BudgetUsedSeconds)
		}
		if dto.BudgetFinalizeSeconds != 0 {
			t.Fatalf("budget_finalize_seconds = %d, want 0", dto.BudgetFinalizeSeconds)
		}
		if !dto.CanStopAtWall {
			t.Fatal("can_stop_at_wall should be true for a budget_exhausted milestone issue run with a completed milestone and no finalize grant")
		}
	})

	t.Run("budget_total includes the finalize term once granted", func(t *testing.T) {
		r := base()
		r.BudgetFinalizeSeconds = 1800
		dto := runToDTO(r, "normal", 0, 0, 0, dtoTestNow)
		if dto.BudgetTotalSeconds == nil || *dto.BudgetTotalSeconds != 3600+1800+1800 {
			t.Fatalf("budget_total_seconds = %v, want 7200 (finalize term included)", dto.BudgetTotalSeconds)
		}
	})

	// can_stop_at_wall full gate: each condition falsified individually must flip it to false.
	t.Run("can_stop_at_wall full gate", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(r *store.Run)
		}{
			{"not paused", func(r *store.Run) { r.Status = "running" }},
			{"not budget_exhausted", func(r *store.Run) { r.HoldReason = pgtype.Text{String: "completion_blocked", Valid: true} }},
			{"not an issue kind", func(r *store.Run) { r.Kind = "prompt" }},
			{"interactive", func(r *store.Run) { r.Interactive = true }},
			{"finalize allowance already used", func(r *store.Run) { r.BudgetFinalizeSeconds = 1800 }},
			{"zero completed milestones", func(r *store.Run) { r.MilestonesCompleted = []byte(`[]`) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := base()
				tc.mutate(&r)
				dto := runToDTO(r, "normal", 0, 0, 0, dtoTestNow)
				if dto.CanStopAtWall {
					t.Fatalf("can_stop_at_wall should be FALSE when %s", tc.name)
				}
			})
		}
	})
}
