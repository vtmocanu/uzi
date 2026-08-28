package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// row is a tiny constructor keeping the table rows readable.
func planRow(runID uuid.UUID, seq int32, kind string) store.ListPlanRevisionStateForRunsRow {
	return store.ListPlanRevisionStateForRunsRow{RunID: runID, Seq: seq, Kind: kind}
}

func TestPlanRevisingSet(t *testing.T) {
	runA := uuid.New()
	runB := uuid.New()

	tests := []struct {
		name string
		rows []store.ListPlanRevisionStateForRunsRow
		want map[uuid.UUID]bool // only the true entries; absent ⇒ false
	}{
		{
			name: "latest plan-ish is plan_revising ⇒ revising",
			rows: []store.ListPlanRevisionStateForRunsRow{
				planRow(runA, 3, "plan"),
				planRow(runA, 7, "plan_revising"),
			},
			want: map[uuid.UUID]bool{runA: true},
		},
		{
			name: "a newer plan after a plan_revising ⇒ not revising",
			rows: []store.ListPlanRevisionStateForRunsRow{
				planRow(runA, 3, "plan"),
				planRow(runA, 7, "plan_revising"),
				planRow(runA, 12, "plan"),
			},
			want: map[uuid.UUID]bool{},
		},
		{
			name: "only a plan, never revised ⇒ absent (not revising)",
			rows: []store.ListPlanRevisionStateForRunsRow{
				planRow(runA, 5, "plan"),
			},
			want: map[uuid.UUID]bool{},
		},
		{
			name: "no plan-ish rows at all ⇒ empty set",
			rows: nil,
			want: map[uuid.UUID]bool{},
		},
		{
			name: "multi-run, interleaved by seq ⇒ correct per-run grouping",
			rows: []store.ListPlanRevisionStateForRunsRow{
				// Deliberately interleaved and not globally seq-ordered, to prove the
				// fold takes the max PER run rather than trusting arrival order.
				planRow(runA, 1, "plan"),
				planRow(runB, 2, "plan"),
				planRow(runB, 9, "plan_revising"), // runB's latest ⇒ revising
				planRow(runA, 4, "plan_revising"),
				planRow(runA, 8, "plan"), // runA's latest ⇒ not revising
			},
			want: map[uuid.UUID]bool{runB: true},
		},
		{
			name: "out-of-order rows for one run, plan_revising wins on max seq",
			rows: []store.ListPlanRevisionStateForRunsRow{
				planRow(runA, 10, "plan_revising"),
				planRow(runA, 2, "plan"),
			},
			want: map[uuid.UUID]bool{runA: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planRevisingSet(tt.rows)
			if len(got) != len(tt.want) {
				t.Fatalf("set size = %d %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for id, wantTrue := range tt.want {
				if got[id] != wantTrue {
					t.Errorf("run %s: got %v, want %v", id, got[id], wantTrue)
				}
			}
			// A revising run must be present with value true; a non-revising run must be
			// absent (so the caller's m[id] yields false), never present with false.
			for id, v := range got {
				if !v {
					t.Errorf("run %s stored with value false; non-revising runs must be absent", id)
				}
			}
		})
	}
}
