package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3250: runToDTO carries the start-time effort and the writers of the frozen model and
// effort; a NULL column is a null on the wire, not an empty string.
func TestRunToDTOStartChoice(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	dto := runToDTO(store.Run{
		ID: uuid.New(), Status: "queued",
		Model: txt("opus"), ModelSource: txt("run_override"), Effort: txt("xhigh"), EffortSource: txt("run_override"),
	}, "normal", 0, 0, 0, now, 0)
	if dto.Model == nil || *dto.Model != "opus" || dto.ModelSource == nil || *dto.ModelSource != "run_override" ||
		dto.Effort == nil || *dto.Effort != "xhigh" || dto.EffortSource == nil || *dto.EffortSource != "run_override" {
		t.Fatalf("start choice not mapped: model=%v/%v effort=%v/%v", dto.Model, dto.ModelSource, dto.Effort, dto.EffortSource)
	}

	// A legacy row: a model with no source stays a null source on the wire (the display reads it as schedule_pin).
	legacy := runToDTO(store.Run{ID: uuid.New(), Status: "queued", Model: txt("opus")}, "normal", 0, 0, 0, now, 0)
	if legacy.Model == nil || legacy.ModelSource != nil || legacy.Effort != nil || legacy.EffortSource != nil {
		t.Fatalf("legacy row: model=%v source=%v effort=%v/%v", legacy.Model, legacy.ModelSource, legacy.Effort, legacy.EffortSource)
	}
}
