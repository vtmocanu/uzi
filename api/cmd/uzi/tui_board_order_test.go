package main

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
			got := bandOrder(tc.runs, false)
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

// Issue #2965: DONE order and the rendered AGE cell must use the same anchor.
func TestBoardDoneAgeMatchesFinishOrder(t *testing.T) {
	now := time.Now()
	// Future dates pin the newer age to 0s; the older ages sit midway through
	// their hour/day buckets, avoiding a clock-boundary flake.
	newer := now.Add(time.Hour)
	older := now.Add(-6*time.Hour - 30*time.Minute)
	for _, status := range []string{"completed", "failed", "cancelled"} {
		for _, fallback := range []bool{false, true} {
			t.Run(status+fmt.Sprintf("/fallback=%t", fallback), func(t *testing.T) {
				runs := []apitypes.RunListItemDTO{
					{RunDTO: apitypes.RunDTO{ID: "oldfinish-1", Status: status,
						CreatedAt: now.Add(-12*time.Hour - 30*time.Minute), UpdatedAt: older, FinishedAt: &older}},
					{RunDTO: apitypes.RunDTO{ID: "newfinish-2", Status: status,
						CreatedAt: now.Add(-48*time.Hour - 30*time.Minute), UpdatedAt: newer}},
				}
				if !fallback {
					runs[1].FinishedAt = &newer
					// A later metadata update must not override a recorded finish.
					runs[0].UpdatedAt = newer.Add(time.Hour)
				}
				ordered := bandOrder(runs, false)
				if ordered[0].ID != "newfinish-2" {
					t.Fatalf("newest finish must sort first: %v", ordered)
				}
				m := tuiTestModel(t, &uzicli.FakeClient{Runs: runs}, "")
				m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs})
				var buf bytes.Buffer
				writer := colorprofile.NewWriter(&buf, nil)
				writer.Profile = colorprofile.Ascii
				if _, err := writer.Write([]byte(m.View().Content)); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(buf.String(), "38;") || strings.Contains(buf.String(), "48;") {
					t.Fatal("Ascii writer retained colors")
				}
				out := stripANSI(buf.String())
				for id, age := range map[string]string{"newfinish-2": "0s", "oldfinish-1": "6h"} {
					line := boardAgeTestRow(t, out, id)
					if !strings.Contains(line, " "+age+" ") {
						t.Errorf("DONE row %s must show finish-anchor age %s: %s", id, age, line)
					}
				}
			})
		}
	}
}

func TestBoardLiveAgeKeepsCreationTime(t *testing.T) {
	now := time.Now()
	for _, status := range []string{"queued", "running", "awaiting_approval"} {
		t.Run(status, func(t *testing.T) {
			r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "liveage-3", Status: status,
				CreatedAt: now.Add(-48*time.Hour - 30*time.Minute), UpdatedAt: now.Add(time.Hour)}}
			m := tuiTestModel(t, &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{r}}, "")
			m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{r}})
			line := boardAgeTestRow(t, stripANSI(m.View().Content), r.ID)
			if !strings.Contains(line, " 2d ") {
				t.Errorf("live row must keep its creation age: %s", line)
			}
		})
	}
}

func boardAgeTestRow(t *testing.T, out, id string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, shortRunID(id)) {
			return line
		}
	}
	t.Fatalf("row %s missing from rendered board:\n%s", id, out)
	return ""
}
