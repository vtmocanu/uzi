package main

import (
	"slices"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Issue #2098: the DONE band shares the web archive's finish-time ordering.
func TestBandOrderDoneHistory(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	row := func(id, status string, created, updated time.Time, finished *time.Time) apitypes.RunListItemDTO {
		return apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
			ID: id, Status: status, CreatedAt: created, UpdatedAt: updated, FinishedAt: finished,
		}}
	}
	earlier := now.Add(-time.Hour)
	later := now.Add(500 * time.Millisecond)
	cases := []struct {
		name string
		runs []apitypes.RunListItemDTO
		want []string
	}{
		{
			name: "older creation but newer finish comes first",
			runs: []apitypes.RunListItemDTO{
				row("created-newer", "completed", now, later, &now),
				row("finished-newer", "completed", earlier, earlier, &later),
			},
			want: []string{"finished-newer", "created-newer"},
		},
		{
			name: "nil finish uses updated time",
			runs: []apitypes.RunListItemDTO{
				row("finished", "failed", now, later, &now),
				row("fallback", "completed", earlier, later, nil),
			},
			want: []string{"fallback", "finished"},
		},
		{
			name: "equal instants rank failed cancelled completed",
			runs: []apitypes.RunListItemDTO{
				row("done", "completed", now, now, &now),
				row("cancelled", "cancelled", now, now, nil),
				row("failed", "failed", now, now, &now),
			},
			want: []string{"failed", "cancelled", "done"},
		},
		{
			name: "equal instant and status preserve server order",
			runs: []apitypes.RunListItemDTO{
				row("z-first", "completed", now, now, &now),
				row("a-second", "completed", earlier, now, nil),
			},
			want: []string{"z-first", "a-second"},
		},
		{
			name: "active bands preserve server order",
			runs: []apitypes.RunListItemDTO{
				row("floor-first", "queued", now, earlier, nil),
				row("needs-first", "awaiting_input", now, earlier, nil),
				row("done-first", "completed", now, now, &now),
				row("floor-second", "running", earlier, later, nil),
				row("needs-second", "awaiting_approval", earlier, later, nil),
				row("done-second", "failed", earlier, later, &later),
			},
			want: []string{"needs-first", "needs-second", "floor-first", "floor-second", "done-second", "done-first"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.runs)
			got := bandOrder(tc.runs)
			ids := make([]string, len(got))
			for i, r := range got {
				ids[i] = r.ID
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("bandOrder IDs = %v, want %v", ids, tc.want)
			}
			for i := range tc.runs {
				if tc.runs[i].ID != before[i].ID {
					t.Fatal("bandOrder reordered its input")
				}
			}
		})
	}
}

func TestCompareDoneRunsUnknownStatusLast(t *testing.T) {
	// Unknown statuses currently belong to ON THE FLOOR, so exercise the DONE
	// comparator directly to pin its defensive rank without changing band membership.
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	unknown := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Status: "future-status", UpdatedAt: at}}
	for _, status := range []string{"failed", "cancelled", "completed"} {
		known := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Status: status, UpdatedAt: at}}
		if compareDoneRuns(known, unknown) >= 0 || compareDoneRuns(unknown, known) <= 0 {
			t.Errorf("%s must precede unknown status at the same instant", status)
		}
	}
	if compareDoneRuns(unknown, unknown) != 0 {
		t.Error("equal unknown statuses must retain server order")
	}
}
