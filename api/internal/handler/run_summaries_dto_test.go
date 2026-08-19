package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOSummaries pins how runToDTO surfaces PRD #362 M1's three summary fields:
// the intent/plan text as pointers (nil when NULL), the deltas decoded from jsonb, and —
// Decision 6, tolerate-on-read — a malformed summary_deltas column degrading to nil ("no
// deltas") rather than panicking.
func TestRunToDTOSummaries(t *testing.T) {
	t.Run("all three fields travel", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:            uuid.New(),
			SummaryIntent: pgtype.Text{String: "builds the thing", Valid: true},
			SummaryPlan:   pgtype.Text{String: "the plan does X", Valid: true},
			SummaryDeltas: []byte(`[{"kind":"added","text":"a test"},{"kind":"dropped","text":"the cache"}]`),
		}, "normal")
		if dto.SummaryIntent == nil || *dto.SummaryIntent != "builds the thing" {
			t.Fatalf("SummaryIntent = %v, want the intent text", dto.SummaryIntent)
		}
		if dto.SummaryPlan == nil || *dto.SummaryPlan != "the plan does X" {
			t.Fatalf("SummaryPlan = %v, want the plan text", dto.SummaryPlan)
		}
		if len(dto.SummaryDeltas) != 2 || dto.SummaryDeltas[0].Kind != "added" || dto.SummaryDeltas[1].Text != "the cache" {
			t.Fatalf("SummaryDeltas = %+v, want the two decoded deltas", dto.SummaryDeltas)
		}
	})

	t.Run("absent summaries are nil (null on the wire)", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New()}, "normal")
		if dto.SummaryIntent != nil || dto.SummaryPlan != nil || dto.SummaryDeltas != nil {
			t.Fatalf("a run with no summaries must expose nils, got intent=%v plan=%v deltas=%+v",
				dto.SummaryIntent, dto.SummaryPlan, dto.SummaryDeltas)
		}
	})

	t.Run("malformed deltas degrade to nil, never panic", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New(), SummaryDeltas: []byte(`{not an array`)}, "normal")
		if dto.SummaryDeltas != nil {
			t.Fatalf("a malformed summary_deltas column must degrade to nil, got %+v", dto.SummaryDeltas)
		}
	})
}
