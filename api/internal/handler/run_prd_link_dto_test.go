package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOPrdLink pins how runToDTO surfaces the two PRD-link reconciliation
// columns read-only: prd_done_path (the path the run declared it moved a PRD to) and
// prd_patch_settled_at (when that patch lifecycle settled, null while pending). Both
// map INDEPENDENTLY through the existing nullable helpers — the "declared a move, not
// yet reconciled" state is prd_done_path set with prd_patch_settled_at still null.
func TestRunToDTOPrdLink(t *testing.T) {
	settled := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	t.Run("both set", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:                uuid.New(),
			PrdDonePath:       pgtype.Text{String: "prds/done/72-x.md", Valid: true},
			PrdPatchSettledAt: pgtype.Timestamptz{Time: settled, Valid: true},
		})
		if dto.PrdDonePath == nil || *dto.PrdDonePath != "prds/done/72-x.md" {
			t.Fatalf("prd_done_path = %v, want prds/done/72-x.md", dto.PrdDonePath)
		}
		if dto.PrdPatchSettledAt == nil || !dto.PrdPatchSettledAt.Equal(settled) {
			t.Fatalf("prd_patch_settled_at = %v, want %v", dto.PrdPatchSettledAt, settled)
		}
	})

	t.Run("neither set: both nil", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New()})
		if dto.PrdDonePath != nil {
			t.Fatalf("prd_done_path = %v, want nil — a run that moved no PRD must not fabricate a path", *dto.PrdDonePath)
		}
		if dto.PrdPatchSettledAt != nil {
			t.Fatalf("prd_patch_settled_at = %v, want nil — an unsettled patch reads null", *dto.PrdPatchSettledAt)
		}
	})
}
