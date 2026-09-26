package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOStatusSinceFromColumn pins issue #1727: runToDTO surfaces runs.status_since
// as status_since, NOT updated_at. A paused row whose updated_at moved on a later,
// unrelated write must still report the pause-entry instant, or every client aging the
// park from it reads the park as younger than it is.
func TestRunToDTOStatusSinceFromColumn(t *testing.T) {
	since := time.Date(2026, 9, 7, 11, 2, 0, 0, time.UTC)
	updated := since.Add(95 * time.Minute) // an unrelated write after the park
	dto := runToDTO(store.Run{
		ID:          uuid.New(),
		Status:      "paused",
		StatusSince: pgtype.Timestamptz{Time: since, Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: updated, Valid: true},
	}, "normal", 0, 0, 0, dtoTestNow)

	if dto.StatusSince == nil {
		t.Fatalf("StatusSince = nil, want %v", since)
	}
	if !dto.StatusSince.Equal(since) {
		t.Fatalf("StatusSince = %v, want the status_since column %v (not updated_at %v)", *dto.StatusSince, since, updated)
	}
	if !dto.UpdatedAt.Equal(updated) {
		t.Fatalf("UpdatedAt = %v, want %v (it keeps its row-changed meaning)", dto.UpdatedAt, updated)
	}
}

// TestRunToDTOStatusSinceNullWhenColumnInvalid pins the defensive null: an invalid column
// maps to a nil pointer rather than a zero time, so a client falls back to updated_at.
func TestRunToDTOStatusSinceNullWhenColumnInvalid(t *testing.T) {
	dto := runToDTO(store.Run{ID: uuid.New(), Status: "running"}, "normal", 0, 0, 0, dtoTestNow)
	if dto.StatusSince != nil {
		t.Fatalf("StatusSince = %v, want nil for an invalid column", *dto.StatusSince)
	}
}
