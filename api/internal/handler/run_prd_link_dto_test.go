package handler

import (
	"encoding/json"
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
		}, "normal")
		if dto.PrdDonePath == nil || *dto.PrdDonePath != "prds/done/72-x.md" {
			t.Fatalf("prd_done_path = %v, want prds/done/72-x.md", dto.PrdDonePath)
		}
		if dto.PrdPatchSettledAt == nil || !dto.PrdPatchSettledAt.Equal(settled) {
			t.Fatalf("prd_patch_settled_at = %v, want %v", dto.PrdPatchSettledAt, settled)
		}
	})

	t.Run("neither set: both nil", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New()}, "normal")
		if dto.PrdDonePath != nil {
			t.Fatalf("prd_done_path = %v, want nil — a run that moved no PRD must not fabricate a path", *dto.PrdDonePath)
		}
		if dto.PrdPatchSettledAt != nil {
			t.Fatalf("prd_patch_settled_at = %v, want nil — an unsettled patch reads null", *dto.PrdPatchSettledAt)
		}
	})
}

// TestRunToDTOHasPRDLink pins that the runs DTO exposes server-computed PRD presence
// (PRD #764 M2): an ISSUE-BACKED run whose snapshotted issue description links a
// prds/*.md serializes has_prd_link:true, and one whose description has no such link
// serializes false. The label-independent detection is computed from IssueDescription
// via the same forgesvc.HasPRDLink detector the board card uses — but only for
// issue-backed runs (IssueIid set): the badge reads "this run's ISSUE links a PRD", so
// an issue-less run (chat / self-improve) is always false even if its prompt happens to
// mention a prds/*.md path. This asserts the JSON field itself (present and correct);
// pre-M2 the field did not exist, so the true case fails on pre-change code.
func TestRunToDTOHasPRDLink(t *testing.T) {
	t.Run("issue-backed description with a prds link: has_prd_link true", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:               uuid.New(),
			IssueIid:         pgtype.Int8{Int64: 764, Valid: true},
			IssueDescription: "Fixes the thing.\n\nSpec: prds/764-uzi-eligibility-label.md",
		}, "normal")
		if !dto.HasPRDLink {
			t.Fatalf("HasPRDLink = false, want true for a description linking prds/764-uzi-eligibility-label.md")
		}
		b, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		raw, ok := got["has_prd_link"]
		if !ok {
			t.Fatalf("has_prd_link absent from runs DTO JSON, want present")
		}
		if string(raw) != "true" {
			t.Fatalf("has_prd_link = %s, want true", raw)
		}
	})

	t.Run("description with no prds link: has_prd_link false", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:               uuid.New(),
			IssueDescription: "A chat run with no issue and no prds reference at all.",
		}, "normal")
		if dto.HasPRDLink {
			t.Fatalf("HasPRDLink = true, want false for a description with no prds link")
		}
		b, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		raw, ok := got["has_prd_link"]
		if !ok {
			t.Fatalf("has_prd_link absent from runs DTO JSON, want present")
		}
		if string(raw) != "false" {
			t.Fatalf("has_prd_link = %s, want false", raw)
		}
	})

	t.Run("issue-less run whose prompt mentions a prds link: has_prd_link false", func(t *testing.T) {
		// The #764 guard: a chat / self-improve run has no issue (IssueIid unset), so
		// even a prompt that references prds/764-uzi-eligibility-label.md must NOT light
		// the "this run's issue links a PRD" badge.
		dto := runToDTO(store.Run{
			ID:               uuid.New(),
			IssueDescription: "Please review prds/764-uzi-eligibility-label.md and improve it.",
		}, "normal")
		if dto.HasPRDLink {
			t.Fatalf("HasPRDLink = true, want false for an issue-less run even though its prompt links a prds/*.md")
		}
	})
}
