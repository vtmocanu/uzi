package main

import (
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// The crew|transcript divider must sit at ONE fixed column on every row: joinColumns
// clamps every left cell to laneRailWidth before padding, so a label longer than the
// rail budget is truncated (with an ellipsis) rather than shoving the │ right. Without
// the clamp, a long-label row reports a larger pre-│ visual width and the divider
// zigzags.
func TestTUIDetailSeparatorColumnIsConstant(t *testing.T) {
	now := time.Now()
	runID := "sep-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	// Distinct agent + instance per lane so each frame forms its own lane; labels vary in
	// length, including two longer than laneRailWidth, a short one, and a lane with none.
	// No string carries a │ rune, so the only │ in the frame is the column separator.
	long1 := "Sweep terraform occurrences + seed mapping"
	long2 := "Reconcile the judge-model drift across every sibling branch"
	next, _ := m.Update(detailLoadedMsg{
		run: apitypes.RunDTO{ID: runID, Status: "running", Health: "ok"},
		msgs: []apitypes.MessageDTO{
			msgDTO(1, "text", "lead", "toolu_a", long1, "planning", now.Add(-4*time.Minute)),
			msgDTO(2, "text", "coder", "toolu_b", long2, "writing", now.Add(-3*time.Minute)),
			msgDTO(3, "text", "tester", "toolu_c", "short", "testing", now.Add(-2*time.Minute)),
			msgDTO(4, "text", "judge", "toolu_d", "", "reviewing", now.Add(-time.Minute)),
		},
	})
	m = next.(tuiModel)

	out := m.View().Content

	var widths []int
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, "│")
		if idx < 0 {
			continue
		}
		widths = append(widths, visualWidth(line[:idx]))
	}
	if len(widths) < 3 {
		t.Fatalf("only %d separator rows found; the test is not exercising the join", len(widths))
	}
	// The left column is padded to laneRailWidth (26) plus the single space before the │.
	const wantWidth = laneRailWidth + 1
	if wantWidth != 27 {
		t.Fatalf("expected divider column %d, but laneRailWidth+1 = %d", 27, wantWidth)
	}
	for i, w := range widths {
		if w != wantWidth {
			t.Errorf("separator row %d has pre-│ width %d, want a constant %d — the divider zigzags", i, w, wantWidth)
		}
	}

	// A label longer than the rail budget is truncated with an ellipsis.
	if !strings.Contains(out, "…") {
		t.Errorf("a long label was not truncated with an ellipsis; the clamp did not fire\n%s", out)
	}
}
