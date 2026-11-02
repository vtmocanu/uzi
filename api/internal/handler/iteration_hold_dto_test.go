package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIterationHoldDTOProjection(t *testing.T) {
	for _, kind := range []string{"issue", "mr_rework", "ci_fix", "task", "prompt", "self_improve"} {
		for _, shape := range []string{"none", "zero completed", "completed"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				done := []byte(`[]`)
				if shape == "completed" {
					done = []byte(`["m1"]`)
				}
				r := store.Run{ID: uuid.New(), Kind: kind, Status: "paused",
					HoldReason:                pgtype.Text{String: "iteration_budget_exhausted", Valid: true},
					CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
					MilestonesFrozen:          []byte(`[{"id":"m1","title":"first"}]`), MilestonesCompleted: done,
					StartedAt:   pgtype.Timestamptz{Time: dtoTestNow.Add(-time.Hour), Valid: true},
					StatusSince: pgtype.Timestamptz{Time: dtoTestNow, Valid: true},
				}
				if shape == "none" {
					r.MilestonesFrozen = []byte(`[]`)
				}
				dto := runToDTO(r, "normal", 2*time.Hour, 0, 0, dtoTestNow.Add(time.Hour), 0)
				board := apitypes.RunSummaryOf(apitypes.RunListItemDTO{RunDTO: dto}).ListItem()
				for _, projected := range []apitypes.RunDTO{dto, board.RunDTO} {
					if projected.Status != "paused" || projected.HoldReason == nil || *projected.HoldReason != "iteration_budget_exhausted" {
						t.Fatalf("iteration hold lost: %+v", projected)
					}
					if projected.CompletionPhase != "" || projected.HoldContext != nil || projected.CanStopAtWall || projected.CanStopAtProviderHold {
						t.Fatalf("iteration became completion or Stop hold: %+v", projected)
					}
					if projected.BudgetTotalSeconds == nil || *projected.BudgetTotalSeconds != 7200 || projected.BudgetUsedSeconds == nil || *projected.BudgetUsedSeconds != 3600 {
						t.Fatal("frozen wall projection lost")
					}
				}
			})
		}
	}
	// Positive controls ensure the projection still exposes distinct existing holds.
	for _, reason := range []string{"completion_blocked", "budget_exhausted"} {
		r := store.Run{ID: uuid.New(), Kind: "issue", Status: "paused", HoldReason: pgtype.Text{String: reason, Valid: true}, CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true}, MilestonesCompleted: []byte(`["m1"]`)}
		dto := runToDTO(r, "normal", 0, 0, 0, dtoTestNow, 0)
		if (dto.CompletionPhase == "blocked") != (reason == "completion_blocked") || dto.CanStopAtWall != (reason == "budget_exhausted") {
			t.Fatalf("existing hold control changed: %+v", dto)
		}
	}
}
