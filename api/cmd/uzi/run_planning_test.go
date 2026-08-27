package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestEffectiveRunStatus pins the running→planning mapping (issue #321 M4): only a
// running run whose server-computed is_planning is set renders as "planning". Every
// other status is passed through unchanged, and is_planning on a non-running status is
// ignored (the predicate is meaningful only while running).
func TestEffectiveRunStatus(t *testing.T) {
	for _, tc := range []struct {
		status     string
		isPlanning bool
		want       string
	}{
		{"running", true, "planning"},
		{"running", false, "running"},
		{"completed", true, "completed"}, // only running maps
		{"queued", false, "queued"},
	} {
		if got := effectiveRunStatus(tc.status, tc.isPlanning); got != tc.want {
			t.Errorf("effectiveRunStatus(%q, %v) = %q, want %q", tc.status, tc.isPlanning, got, tc.want)
		}
	}
}

// TestPlanningStatusColour pins the ANDON planning colour: "planning" (running + is_planning)
// is a distinct indigo, not a fall-through to the faint default nor a reuse of the running sage.
// The stalled health override still wins over the planning bucket (health precedence unchanged).
func TestPlanningStatusColour(t *testing.T) {
	p := newPalette(true)

	planning := p.stateColor("running", "", true) // running + is_planning → planning
	running := p.stateColor("running", "", false)
	if bgFillSGR(planning) == bgFillSGR(running) {
		t.Error("planning and running resolve to the same colour; the planning phase is not visually distinct")
	}
	if bgFillSGR(planning) == bgFillSGR(p.faintC) {
		t.Error("planning resolved to the faint default; its own indigo entry is not being read")
	}
	// Health precedence: a stalled planning run is still the stall colour (triage wins).
	if bgFillSGR(p.stateColor("running", "stalled", true)) != bgFillSGR(p.stall) {
		t.Error("stalled health no longer overrides the planning bucket; the precedence rule regressed")
	}
}

// TestStatusGlyphPlanning pins the NO_COLOR-safe state glyph (D3): planning is a hollow circle
// ("nothing committed yet"), distinct from running's filled dot.
func TestStatusGlyphPlanning(t *testing.T) {
	if got, _ := stateGlyphWord("running", "", true); got != "○" {
		t.Errorf("planning glyph = %q, want %q", got, "○")
	}
	if got, _ := stateGlyphWord("running", "", false); got != "●" {
		t.Errorf("running glyph = %q, want %q", got, "●")
	}
}

// TestRenderRunDetailPlanningStatus proves `uzi run get` renders the effective word in
// its STATUS row: a running+is_planning run reads "planning", while a plain running run
// still reads "running". The raw status is not surfaced to a human here (issue #321 M4).
func TestRenderRunDetailPlanningStatus(t *testing.T) {
	render := func(isPlanning bool) string {
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		r := apitypes.RunDTO{
			ID:         "run-1",
			Kind:       "issue",
			Status:     "running",
			IsPlanning: isPlanning,
			IssueTitle: "do the thing",
			Health:     "ok",
		}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail(isPlanning=%v): %v", isPlanning, err)
		}
		return buf.String()
	}

	if out := render(true); !strings.Contains(out, "planning") {
		t.Errorf("a running+is_planning run must render STATUS \"planning\", got:\n%s", out)
	}
	if out := render(false); !strings.Contains(out, "running") {
		t.Errorf("a plain running run must render STATUS \"running\", got:\n%s", out)
	}
}

// TestRunListPlanningStatus proves the `uzi run list` table column renders the effective
// word: a planning run reads "planning" and a normal running run reads "running".
func TestRunListPlanningStatus(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "plan-1", Kind: "issue", Status: "running", IsPlanning: true, IssueTitle: "planning one"}},
		{RunDTO: apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "running", IssueTitle: "running two"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "planning") {
		t.Errorf("run list must render \"planning\" for a running+is_planning run, got:\n%s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("run list must still render \"running\" for a plain running run, got:\n%s", out)
	}
}

// TestAdminRunsPlanningStatus proves the `uzi admin runs` STATUS column renders the
// effective word: a planning run reads "planning" and a normal running run reads
// "running", so the admin table agrees with `run list` and the board.
func TestAdminRunsPlanningStatus(t *testing.T) {
	fc := &uzicli.FakeClient{AdminRuns: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "plan-1", Kind: "issue", Status: "running", IsPlanning: true, IssueTitle: "planning one"}, OwnerEmail: sp("a@example.com")},
		{RunDTO: apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "running", IssueTitle: "running two"}, OwnerEmail: sp("b@example.com")},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "runs")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "planning") {
		t.Errorf("admin runs must render \"planning\" for a running+is_planning run, got:\n%s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("admin runs must still render \"running\" for a plain running run, got:\n%s", out)
	}
}

// TestBoardFilterMatchesPlanning proves the board `/` filter matches the RENDERED status:
// a planning run (running + is_planning) matches the query "planning" and no longer
// matches "running", so the filter matches the word the user actually sees.
func TestBoardFilterMatchesPlanning(t *testing.T) {
	b := newBoardState()
	b.runs = []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "plan-1", Kind: "issue", Status: "running", IsPlanning: true, IssueTitle: "planning one"}},
		{RunDTO: apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "running", IssueTitle: "running two"}},
	}

	b.filter = "planning"
	planned := b.visible()
	if len(planned) != 1 || planned[0].ID != "plan-1" {
		t.Errorf("filter \"planning\" must match only the planning run, got %d rows: %+v", len(planned), planned)
	}

	b.filter = "running"
	runningOnly := b.visible()
	if len(runningOnly) != 1 || runningOnly[0].ID != "run-2" {
		t.Errorf("filter \"running\" must match only the plain running run (the planning run's haystack no longer holds \"running\"), got %d rows: %+v", len(runningOnly), runningOnly)
	}
}
