package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func txt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
func nullTxt() pgtype.Text     { return pgtype.Text{} }
func i8(v int64) pgtype.Int8   { return pgtype.Int8{Int64: v, Valid: true} }
func tstamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func TestMapLatestRun(t *testing.T) {
	viewer := uuid.New()
	runID := uuid.New()
	created := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	updated := created.Add(5 * time.Minute)

	t.Run("owner's run maps all fields and is mine", func(t *testing.T) {
		dto := mapLatestRun(runID, viewer, "completed", i8(7), txt("boom"),
			txt("Vlad"), txt("vlad@example.com"), txt("laptop"), tstamp(created), tstamp(updated), viewer)
		if dto.ID != runID.String() || dto.Status != "completed" {
			t.Fatalf("id/status wrong: %+v", dto)
		}
		if dto.MrIID == nil || *dto.MrIID != 7 {
			t.Fatalf("mr_iid should be 7, got %v", dto.MrIID)
		}
		if dto.FailureReason == nil || *dto.FailureReason != "boom" {
			t.Fatalf("failure_reason not carried: %v", dto.FailureReason)
		}
		if dto.WorkerName == nil || *dto.WorkerName != "laptop" {
			t.Fatalf("worker_name not carried: %v", dto.WorkerName)
		}
		if dto.OwnerName != "Vlad" {
			t.Fatalf("owner_name should prefer display name, got %q", dto.OwnerName)
		}
		if !dto.IsMine {
			t.Fatal("a run owned by the viewer must be is_mine")
		}
		if !dto.CreatedAt.Equal(created) || !dto.UpdatedAt.Equal(updated) {
			t.Fatalf("timestamps wrong: %+v", dto)
		}
	})

	t.Run("another owner's run is not mine, still shows owner name", func(t *testing.T) {
		otherOwner := uuid.New()
		dto := mapLatestRun(runID, otherOwner, "running", pgtype.Int8{}, nullTxt(),
			nullTxt(), txt("someone@example.com"), nullTxt(), tstamp(created), tstamp(updated), viewer)
		if dto.IsMine {
			t.Fatal("a run owned by someone else must not be is_mine")
		}
		// display name absent → fall back to email so "started by X" is never blank.
		if dto.OwnerName != "someone@example.com" {
			t.Fatalf("owner_name should fall back to email, got %q", dto.OwnerName)
		}
		if dto.MrIID != nil {
			t.Fatalf("null mr_iid should map to nil, got %v", *dto.MrIID)
		}
		if dto.FailureReason != nil || dto.WorkerName != nil {
			t.Fatalf("null failure_reason/worker_name should map to nil: %+v", dto)
		}
	})

	t.Run("blank display name and email leave owner name empty", func(t *testing.T) {
		dto := mapLatestRun(runID, viewer, "queued", pgtype.Int8{}, nullTxt(),
			txt(""), nullTxt(), nullTxt(), tstamp(created), tstamp(updated), viewer)
		if dto.OwnerName != "" {
			t.Fatalf("owner_name should be empty when no name/email, got %q", dto.OwnerName)
		}
	})
}
