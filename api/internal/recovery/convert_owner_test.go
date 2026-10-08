package recovery

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The store custody admission LiveDB matrix owns classifier precedence. This test
// checks that DTO mapping preserves the canonical SQL attention field verbatim.
func TestCanonicalHoldAttentionMapping(t *testing.T) {
	for _, attention := range []string{"discarded", "released", "archive_ready", "capturing", "needs_action", "active", "source_only"} {
		t.Run(attention, func(t *testing.T) {
			got := custodyHoldToDTO(store.ListCustodyHoldsForOwnerRow{Attention: attention})
			if got.Attention != attention {
				t.Fatalf("attention = %q, want %q", got.Attention, attention)
			}
		})
	}
}

func TestCustodyHoldToDTO(t *testing.T) {
	id, runID, workerID := uuid.New(), uuid.New(), uuid.New()
	created := time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC)

	// An OPEN, capture-less hold on a terminal run → source_only; ReleasedAt stays nil.
	open := store.ListCustodyHoldsForOwnerRow{
		ID: id, RunID: runID, Generation: 3, State: "open", OriginalWorkerID: workerID,
		Attention: "source_only", WorkerName: "alpha", CaptureState: "", RunStatus: "failed",
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
	if got.Attention != "source_only" {
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
	rel.Attention = "released"
	rel.ReleasedAt = pgtype.Timestamptz{Time: releasedAt, Valid: true}
	relDTO := custodyHoldToDTO(rel)
	if relDTO.Attention != "released" {
		t.Errorf("released attention = %q, want released", relDTO.Attention)
	}
	if relDTO.ReleasedAt == nil || !relDTO.ReleasedAt.Equal(releasedAt) {
		t.Errorf("released ReleasedAt = %v, want %v", relDTO.ReleasedAt, releasedAt)
	}
}

// TestCaptureToDTOHoldID (issue #1417) proves the owner archive DTO names the custody hold the
// capture was reserved under, so `uzi run recovery --json` can list each hold's captures.
func TestCaptureToDTOHoldID(t *testing.T) {
	id, runID, holdID := uuid.New(), uuid.New(), uuid.New()
	got := captureToDTO(store.RecoveryCapture{ID: id, RunID: runID, HoldID: holdID, State: "available", SourceSha: "abc"})
	if got.ID != id.String() || got.RunID != runID.String() || got.HoldID != holdID.String() {
		t.Errorf("identity fields wrong: %+v", got)
	}
}

// SQL fields remain authoritative even when raw lifecycle labels disagree.
func TestOwnerHoldNeedsDecision(t *testing.T) {
	for _, decision := range []bool{false, true} {
		row := store.ListCustodyHoldsForOwnerRow{
			State: "open", RunStatus: "recovery_wait", RecoveryWaitCause: "worker_requeue_exhausted",
			CaptureState: "available", HasAvailableCapture: true, Attention: "active", DecisionNeeded: decision,
		}
		if got := OwnerHoldNeedsDecision(row); got != decision {
			t.Fatalf("decision = %v, want SQL value %v", got, decision)
		}
		if got := custodyHoldToDTO(row).Attention; got != "active" {
			t.Fatalf("attention = %q, want SQL value active", got)
		}
	}
}
