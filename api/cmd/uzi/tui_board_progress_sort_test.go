package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runprogress"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #3207: `p` orders ON THE FLOOR highest progress first.

func sortRun(id, status string, p *apitypes.RunProgress) apitypes.RunListItemDTO {
	return apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
		ID: id, Kind: "issue", Status: status, IssueTitle: "title-" + id, Progress: p,
	}}
}

func pctProg(pct int) *apitypes.RunProgress {
	return &apitypes.RunProgress{State: runprogress.StatePercent, Pct: &pct}
}

func runIDs(runs []apitypes.RunListItemDTO) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

func progressSortFixture() []apitypes.RunListItemDTO {
	return []apitypes.RunListItemDTO{
		sortRun("nil-progress", "running", nil),
		sortRun("pct-40", "running", pctProg(40)),
		sortRun("needs-1", "awaiting_input", &apitypes.RunProgress{State: runprogress.StateWaiting}),
		sortRun("queued", "queued", &apitypes.RunProgress{State: runprogress.StateQueued}),
		sortRun("pct-90-a", "running", pctProg(90)),
		sortRun("unknown-state", "running", &apitypes.RunProgress{State: "zzz-future"}),
		sortRun("done-1", "completed", nil),
		sortRun("percent-nil-pct", "running", &apitypes.RunProgress{State: runprogress.StatePercent}),
		sortRun("stalled", "running", &apitypes.RunProgress{State: runprogress.StateStalled, Pct: intPtr(80), MilestoneDone: 4, MilestoneTotal: 5}),
		sortRun("pct-90-b", "running", pctProg(90)),
		sortRun("pct-11", "running", pctProg(11)),
	}
}

func TestBandOrderFloorByProgress(t *testing.T) {
	runs := progressSortFixture()
	before := slices.Clone(runs)

	got := runIDs(bandOrder(runs, true))
	want := []string{
		"needs-1",
		// measured runs, highest first, server order on the 90 tie
		"pct-90-a", "pct-90-b", "pct-40", "pct-11",
		// no drawn percentage (nil, queued, unknown state, percent without pct, stalled):
		// below every measured run, in server order
		"nil-progress", "queued", "unknown-state", "percent-nil-pct", "stalled",
		"done-1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("byProgress order:\n got %v\nwant %v", got, want)
	}

	off := runIDs(bandOrder(runs, false))
	wantOff := []string{
		"needs-1",
		"nil-progress", "pct-40", "queued", "pct-90-a", "unknown-state", "percent-nil-pct", "stalled", "pct-90-b", "pct-11",
		"done-1",
	}
	if !slices.Equal(off, wantOff) {
		t.Errorf("default order:\n got %v\nwant %v", off, wantOff)
	}
	if !slices.EqualFunc(runs, before, func(a, b apitypes.RunListItemDTO) bool { return a.ID == b.ID }) {
		t.Error("bandOrder mutated the fetched runs")
	}
}

func progressSortModel(t *testing.T, runs []apitypes.RunListItemDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	return next.(tuiModel)
}

func selectRun(t *testing.T, m tuiModel, id string) tuiModel {
	t.Helper()
	i := slices.Index(runIDs(m.board.visible()), id)
	if i < 0 {
		t.Fatalf("run %s not visible", id)
	}
	m.board.cursor = i
	return m
}

func pollRuns(t *testing.T, m tuiModel, runs []apitypes.RunListItemDTO) tuiModel {
	t.Helper()
	m.board.waitID = 99
	next, _ := m.Update(boardRunsMsg{reqID: 99, runs: runs})
	return next.(tuiModel)
}

func TestTUIBoardProgressSortToggleKeepsSelection(t *testing.T) {
	m := selectRun(t, progressSortModel(t, progressSortFixture()), "pct-11")

	m = press(t, m, keyProgressSort)
	if !m.board.sortProgress {
		t.Fatal("p did not turn the progress sort on")
	}
	if got := m.board.selectedID(); got != "pct-11" {
		t.Errorf("toggle on moved the selection to %s", got)
	}
	if got := runIDs(m.board.visible()); !slices.Equal(got, runIDs(bandOrder(m.board.runs, true))) {
		t.Errorf("board not in progress order: %v", got)
	}
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "ON THE FLOOR · 9 · by progress") {
		t.Errorf("floor eyebrow lacks the by-progress marker\n%s", out)
	}
	if title := strings.SplitN(out, "\n", 2)[0]; strings.Contains(title, "by progress") {
		t.Errorf("by-progress marker belongs on the floor eyebrow, not the title: %q", title)
	}

	m = press(t, m, keyProgressSort)
	if m.board.sortProgress {
		t.Fatal("second p did not turn the sort off")
	}
	if got := m.board.selectedID(); got != "pct-11" {
		t.Errorf("toggle off moved the selection to %s", got)
	}
	if got := runIDs(m.board.visible()); !slices.Equal(got, runIDs(bandOrder(m.board.runs, false))) {
		t.Errorf("toggle off did not restore the default order: %v", got)
	}
	if out := stripANSI(m.View().Content); strings.Contains(out, "by progress") {
		t.Errorf("by-progress marker survived toggling off\n%s", out)
	}
}

// At 80 columns the sort marker must not crowd a filter being typed off the title line.
func TestTUIBoardProgressSortKeepsFilterCaretAt80(t *testing.T) {
	m := progressSortModel(t, progressSortFixture())
	m.width = 80
	m = press(t, m, keyHideDone)
	m = press(t, m, keyProgressSort)
	m = press(t, m, keyFilter)
	for _, r := range "scheduler-poll" {
		m = press(t, m, string(r))
	}
	title := strings.SplitN(stripANSI(m.View().Content), "\n", 2)[0]
	if !strings.Contains(title, "/scheduler-poll▌") {
		t.Errorf("typed filter or caret clipped at 80 columns: %q", title)
	}
}

func TestTUIBoardProgressSortPollFollowsSelection(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		sortRun("a", "running", pctProg(80)),
		sortRun("b", "running", pctProg(50)),
		sortRun("c", "running", pctProg(20)),
	}
	m := press(t, selectRun(t, progressSortModel(t, runs), "c"), keyProgressSort)

	// c overtakes both: it moves from the bottom of the floor to the top.
	m = pollRuns(t, m, []apitypes.RunListItemDTO{
		sortRun("a", "running", pctProg(80)),
		sortRun("b", "running", pctProg(50)),
		sortRun("c", "running", pctProg(95)),
	})
	if got := m.board.selectedID(); got != "c" || m.board.cursor != 0 {
		t.Errorf("after reorder: selected %s at %d, want c at 0", got, m.board.cursor)
	}

	// c moves band: it now waits on the owner, so it sits in NEEDS YOU.
	m = pollRuns(t, m, []apitypes.RunListItemDTO{
		sortRun("a", "running", pctProg(80)),
		sortRun("b", "running", pctProg(50)),
		sortRun("c", "awaiting_input", &apitypes.RunProgress{State: runprogress.StateWaiting}),
	})
	if got := m.board.selectedID(); got != "c" {
		t.Errorf("after band change: selected %s, want c", got)
	}

	// Select b (index 2: c, a, b), then b disappears: the previous index is clamped.
	m = selectRun(t, m, "b")
	m = pollRuns(t, m, []apitypes.RunListItemDTO{
		sortRun("a", "running", pctProg(80)),
		sortRun("c", "awaiting_input", &apitypes.RunProgress{State: runprogress.StateWaiting}),
	})
	if m.board.cursor != 1 || m.board.selectedID() != "a" {
		t.Errorf("after disappearance: cursor %d on %s, want 1 on a", m.board.cursor, m.board.selectedID())
	}
}

// Default-mode polls keep their existing index-based cursor.
func TestTUIBoardDefaultOrderPollKeepsIndex(t *testing.T) {
	m := selectRun(t, progressSortModel(t, []apitypes.RunListItemDTO{
		sortRun("a", "running", nil), sortRun("b", "running", nil),
	}), "b")
	m = pollRuns(t, m, []apitypes.RunListItemDTO{
		sortRun("b", "running", nil), sortRun("a", "running", nil),
	})
	if m.board.cursor != 1 || m.board.selectedID() != "a" {
		t.Errorf("default poll: cursor %d on %s, want index 1 (now a)", m.board.cursor, m.board.selectedID())
	}
}

// A reorder that jumps the selection past the window still renders the selected row.
func TestTUIBoardProgressSortPollScrollsToSelection(t *testing.T) {
	runs := make([]apitypes.RunListItemDTO, 40)
	for i := range runs {
		runs[i] = sortRun(fmt.Sprintf("run-%02d", i), "running", pctProg(50))
	}
	m := progressSortModel(t, runs)
	m.width, m.height = 100, 20
	m = press(t, m, keyProgressSort)
	for i := 0; i < 39; i++ {
		m = press(t, m, keyDown)
	}
	if m.board.selectedID() != "run-39" {
		t.Fatalf("selected %s, want run-39", m.board.selectedID())
	}
	moved := slices.Clone(runs)
	moved[39] = sortRun("run-39", "running", pctProg(99))
	m = pollRuns(t, m, moved)
	if m.board.cursor != 0 {
		t.Fatalf("run-39 should now be first, cursor %d", m.board.cursor)
	}
	if out := stripANSI(m.View().Content); !strings.Contains(out, "title-run-39") {
		t.Errorf("selected row not rendered after the reorder\n%s", out)
	}
}

func TestTUIBoardProgressSortComposesWithFilterAndHideDone(t *testing.T) {
	m := progressSortModel(t, progressSortFixture())

	// While filtering, p is filter text.
	m = press(t, m, keyFilter)
	m = press(t, m, keyProgressSort)
	if m.board.sortProgress || m.board.filter != "p" {
		t.Fatalf("p while filtering: sortProgress=%v filter=%q", m.board.sortProgress, m.board.filter)
	}
	m = press(t, m, keyEnter)
	m.board.filter = "pct-"
	m = press(t, m, keyHideDone)
	m = press(t, m, keyProgressSort)
	if !m.board.sortProgress {
		t.Fatal("p did not toggle after the filter closed")
	}
	want := []string{"pct-90-a", "pct-90-b", "pct-40", "pct-11"}
	if got := runIDs(m.board.visible()); !slices.Equal(got, want) {
		t.Errorf("filtered sorted board %v, want %v", got, want)
	}
}

func TestTUIBoardProgressSortAdminBoard(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = press(t, m, keyAdmin)
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, admin: true, runs: []apitypes.RunListItemDTO{
		sortRun("low", "running", pctProg(20)),
		sortRun("high", "running", pctProg(70)),
	}})
	m = press(t, next.(tuiModel), keyProgressSort)
	if got := runIDs(m.board.visible()); !slices.Equal(got, []string{"high", "low"}) {
		t.Errorf("admin board order %v, want [high low]", got)
	}
}

// In split mode p toggles only while the board pane has focus.
func TestTUIBoardProgressSortSplitFocus(t *testing.T) {
	m := progressSortModel(t, progressSortFixture())
	m = resizeSplit(m, 120, splitMinHeight+2)
	if !m.splitDrawn() || m.view != viewBoard {
		t.Fatalf("split not drawn with board focus (drawn=%v view=%v)", m.splitDrawn(), m.view)
	}
	for _, want := range []tuiView{viewWorkers, viewPulls, viewCI} {
		m = press(t, m, keyTab)
		if m.view != want {
			t.Fatalf("tab focused %v, want %v", m.view, want)
		}
		m = press(t, m, keyProgressSort)
		if m.board.sortProgress {
			t.Fatalf("p with %v focused toggled the board sort", want)
		}
	}
	m = press(t, m, keyTab)
	m = press(t, m, keyProgressSort)
	if m.view != viewBoard || !m.board.sortProgress {
		t.Fatalf("p with the board focused: view=%v sortProgress=%v", m.view, m.board.sortProgress)
	}
}

func TestTUIBoardProgressSortHelpLine(t *testing.T) {
	if !slices.ContainsFunc(helpLines(viewBoard), func(l string) bool { return strings.HasPrefix(l, "p   ") }) {
		t.Errorf("board help lacks the p line: %v", helpLines(viewBoard))
	}
}
