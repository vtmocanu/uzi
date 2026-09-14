package recovery

// convert_owner_test.go unit-tests the M5 owner attention derivation (PRD #1349 D6/D8) and the
// DTO mapping WITHOUT a database — the derivation is pure over a listing row, so the whole
// attention vocabulary, its precedence, and the DecisionNeeded predicate are pinned here.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestDeriveHoldAttention(t *testing.T) {
	row := func(state, captureState, runStatus string, hasAvailable bool) store.ListCustodyHoldsForOwnerRow {
		return store.ListCustodyHoldsForOwnerRow{
			State: state, CaptureState: captureState, RunStatus: runStatus, HasAvailableCapture: hasAvailable,
		}
	}
	cases := []struct {
		name string
		row  store.ListCustodyHoldsForOwnerRow
		want string
	}{
		{"discarded wins over everything", row("discarded", "preparing", "running", true), attentionDiscarded},
		{"released wins over everything", row("released", "available", "completed", true), attentionReleased},
		{"open + available archive", row("open", "available", "completed", true), attentionArchiveReady},
		{"open + preparing capture", row("open", "preparing", "running", false), attentionCapturing},
		{"open + uploading capture", row("open", "uploading", "running", false), attentionCapturing},
		{"open + needs_action capture", row("open", "needs_action", "failed", false), attentionNeedsAction},
		{"open + live running run, no capture", row("open", "", "running", false), attentionActive},
		{"open + queued run, no capture", row("open", "", "queued", false), attentionActive},
		{"open + paused run, no capture", row("open", "", "paused", false), attentionActive},
		{"open + completed run, no capture", row("open", "", "completed", false), attentionSourceOnly},
		{"open + failed run, no capture", row("open", "", "failed", false), attentionSourceOnly},
		{"open + cancelled run, no capture", row("open", "", "cancelled", false), attentionSourceOnly},
		{"open + gone/unknown run, no capture", row("open", "", "", false), attentionSourceOnly},
		// Precedence: a ready archive / in-flight capture outranks the run's terminality, so a
		// terminal run with a durable/in-progress capture is never a source_only decision.
		{"available archive beats a terminal run", row("open", "available", "failed", true), attentionArchiveReady},
		{"capturing beats a terminal run", row("open", "preparing", "completed", false), attentionCapturing},
		{"needs_action beats a live run", row("open", "needs_action", "running", false), attentionNeedsAction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveHoldAttention(tc.row); got != tc.want {
				t.Errorf("deriveHoldAttention(%+v) = %q, want %q", tc.row, got, tc.want)
			}
		})
	}
}

func TestIsDecisionAttention(t *testing.T) {
	// Only needs_action and source_only await an owner decision (D10). active protection and
	// self-releasing archive_ready/capturing rows, plus settled released/discarded, do not.
	decision := map[string]bool{
		attentionNeedsAction: true, attentionSourceOnly: true,
		attentionActive: false, attentionArchiveReady: false, attentionCapturing: false,
		attentionReleased: false, attentionDiscarded: false,
	}
	for a, want := range decision {
		if got := isDecisionAttention(a); got != want {
			t.Errorf("isDecisionAttention(%q) = %v, want %v", a, got, want)
		}
	}
}

func TestCustodyHoldToDTO(t *testing.T) {
	id, runID, workerID := uuid.New(), uuid.New(), uuid.New()
	created := time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC)

	// An OPEN, capture-less hold on a terminal run → source_only; ReleasedAt stays nil.
	open := store.ListCustodyHoldsForOwnerRow{
		ID: id, RunID: runID, Generation: 3, State: "open", OriginalWorkerID: workerID,
		WorkerName: "alpha", CaptureState: "", RunStatus: "failed",
		CreatedAt: pgtype.Timestamptz{Time: created, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: updated, Valid: true},
	}
	got := custodyHoldToDTO(open)
	if got.ID != id.String() || got.RunID != runID.String() || got.WorkerID != workerID.String() {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Generation != 3 || got.State != "open" || got.WorkerName != "alpha" {
		t.Errorf("scalar fields wrong: %+v", got)
	}
	if got.Attention != attentionSourceOnly {
		t.Errorf("attention = %q, want source_only", got.Attention)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) {
		t.Errorf("timestamps wrong: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
	if got.ReleasedAt != nil {
		t.Errorf("open hold ReleasedAt = %v, want nil", got.ReleasedAt)
	}

	// A RELEASED hold carries a non-nil ReleasedAt and attention=released.
	releasedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	rel := open
	rel.State = "released"
	rel.ReleasedAt = pgtype.Timestamptz{Time: releasedAt, Valid: true}
	relDTO := custodyHoldToDTO(rel)
	if relDTO.Attention != attentionReleased {
		t.Errorf("released attention = %q, want released", relDTO.Attention)
	}
	if relDTO.ReleasedAt == nil || !relDTO.ReleasedAt.Equal(releasedAt) {
		t.Errorf("released ReleasedAt = %v, want %v", relDTO.ReleasedAt, releasedAt)
	}
}
