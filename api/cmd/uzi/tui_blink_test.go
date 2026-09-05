package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// milestoneRunItem builds a milestone-structured board run: `done` of len(ids) frozen
// milestones completed, one in progress. inProg is the frozen id reported in progress.
func milestoneRunItem(id, status string, done int, inProg string) apitypes.RunListItemDTO {
	ms := []apitypes.Milestone{
		{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"},
		{ID: "m3", Title: "Gamma"}, {ID: "m4", Title: "Delta"},
	}
	var completed []string
	for i := 0; i < done && i < len(ms); i++ {
		completed = append(completed, ms[i].ID)
	}
	r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
		ID: id, Kind: "issue", Status: status, IssueTitle: "structured run",
		Milestones: ms, MilestonesCompleted: completed,
	}}
	if inProg != "" {
		r.MilestonesInProgress = []string{inProg}
	}
	return r
}

// PRD #1064 D4: the blink tick is armed ONLY when the board holds a visible, non-terminal run
// with a non-empty MilestonesInProgress, is NEVER double-armed across a 2s board refresh
// (blinkArmed), and disarms — dropping to the static frame — when nothing is in progress.
func TestTUIBlinkArmOnlyWithInProgress(t *testing.T) {
	// The board reply now ALWAYS re-arms the board tick (PRD #1130: rescheduling moved to the
	// reply), so `cmd != nil` no longer distinguishes "blink armed" — detect the blinkTickMsg
	// itself. Shrink both cadences so draining the reply's ticks does not block on the real 2s/500ms.
	origBoard, origBlink := boardPollInterval, blinkInterval
	boardPollInterval, blinkInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { boardPollInterval, blinkInterval = origBoard, origBlink })

	fake := &uzicli.FakeClient{}
	m := tuiTestModel(t, fake, "")

	// A board with no in-progress run arms nothing.
	plain := []apitypes.RunListItemDTO{milestoneRunItem("aaaaaaaa-1", "running", 2, "")}
	next, cmd := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: plain})
	m = next.(tuiModel)
	if m.blinkArmed || hasMsg[blinkTickMsg](drainCmd(cmd)) {
		t.Fatalf("a board with no in-progress run must not arm the blink (armed=%v)", m.blinkArmed)
	}

	// A refresh that reveals an in-progress run arms exactly one tick.
	inprog := []apitypes.RunListItemDTO{milestoneRunItem("aaaaaaaa-1", "running", 1, "m2")}
	next, cmd = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: inprog})
	m = next.(tuiModel)
	if !m.blinkArmed || !hasMsg[blinkTickMsg](drainCmd(cmd)) {
		t.Fatalf("revealing an in-progress run must arm the blink (armed=%v)", m.blinkArmed)
	}

	// A SECOND refresh while already armed must NOT stack another blink tick (double renders).
	next, cmd = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: inprog})
	m = next.(tuiModel)
	if !m.blinkArmed || hasMsg[blinkTickMsg](drainCmd(cmd)) {
		t.Fatalf("a refresh while already armed must not re-arm the blink (armed=%v)", m.blinkArmed)
	}

	// The tick toggles the phase and re-arms itself while a run is still in progress.
	next, cmd = m.Update(blinkTickMsg{})
	m = next.(tuiModel)
	if !m.blinkOn || cmd == nil {
		t.Fatalf("a tick while in progress must toggle blinkOn and re-arm (on=%v cmd=%v)", m.blinkOn, cmd != nil)
	}

	// When nothing is in progress any more, the run row still exists but the tick lapses and the
	// phase resets to the static frame.
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: plain})
	m = next.(tuiModel)
	next, cmd = m.Update(blinkTickMsg{})
	m = next.(tuiModel)
	if m.blinkArmed || m.blinkOn || cmd != nil {
		t.Fatalf("with nothing in progress the tick must disarm and reset (armed=%v on=%v cmd=%v)", m.blinkArmed, m.blinkOn, cmd != nil)
	}
}

// UZI_TUI_NO_BLINK=1 (noBlink) pins the static frame: the tick is never armed and blinkOn stays
// false, even on a board with an in-progress run.
func TestTUIBlinkNoBlinkPinsStaticFrame(t *testing.T) {
	// The board reply always re-arms the board tick now (PRD #1130), so detect the blinkTickMsg
	// itself rather than a non-nil cmd; shrink the cadences so draining does not block.
	origBoard, origBlink := boardPollInterval, blinkInterval
	boardPollInterval, blinkInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { boardPollInterval, blinkInterval = origBoard, origBlink })

	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.noBlink = true
	inprog := []apitypes.RunListItemDTO{milestoneRunItem("aaaaaaaa-1", "running", 1, "m2")}
	next, cmd := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: inprog})
	m = next.(tuiModel)
	if m.blinkArmed || hasMsg[blinkTickMsg](drainCmd(cmd)) {
		t.Fatalf("UZI_TUI_NO_BLINK must never arm the blink (armed=%v)", m.blinkArmed)
	}
	// A stray tick cannot flip the phase on.
	next, _ = m.Update(blinkTickMsg{})
	m = next.(tuiModel)
	if m.blinkOn {
		t.Fatal("UZI_TUI_NO_BLINK must keep blinkOn false")
	}
}

// The board micro-bar's in-progress cell alternates by SHAPE (▰ vs ▱), so it survives an Ascii
// (NO_COLOR) profile that strips the wait tint. The done fill stays ▰ and the in-progress cell
// is the one that flips between the two phases.
func TestTUIBlinkAsciiShapeAlternates(t *testing.T) {
	inprog := []apitypes.RunListItemDTO{milestoneRunItem("aaaaaaaa-1", "running", 1, "m2")}
	render := func(on bool) string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.width = 120
		next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
		m = next.(tuiModel)
		next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: inprog})
		m = next.(tuiModel)
		m.blinkOn = on
		return stripANSI(m.View().Content)
	}
	off := render(false)
	onFrame := render(true)
	// 4 milestones, 1 done: the OFF frame reads ▰ (done) + ▱ (in-progress cell) + ▱▱ (remaining) =
	// ▰▱▱▱; the ON frame flips the in-progress cell to ▰ = ▰▰▱▱. The shapes differ with colour gone.
	if !strings.Contains(off, "▰▱▱▱") {
		t.Errorf("Ascii OFF frame is not ▰▱▱▱ (static in-progress cell ▱)\n%s", off)
	}
	if !strings.Contains(onFrame, "▰▰▱▱") {
		t.Errorf("Ascii ON frame is not ▰▰▱▱ (in-progress cell flipped to ▰)\n%s", onFrame)
	}
	if off == onFrame {
		t.Error("the in-progress cell did not alternate by shape under the Ascii profile")
	}
}

// PRD #1064 D5: a run with NO frozen milestones AND no current_activity renders byte-for-byte
// the same regardless of the blink phase or the noBlink pin — the blink machinery is inert on a
// null-milestone run. Asserted on both the board and the run-detail rail.
func TestTUIBlinkNullMilestoneByteIdentical(t *testing.T) {
	now := time.Now()
	plainRun := apitypes.RunDTO{ID: "aaaaaaaa-1111", Kind: "issue", Status: "running", IssueTitle: "no milestones"}

	// Board: same run, phase off vs on.
	boardFrame := func(on, noBlink bool) string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.noBlink = noBlink
		next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{{RunDTO: plainRun}}})
		m = next.(tuiModel)
		m.blinkOn = on
		return m.View().Content
	}
	base := boardFrame(false, false)
	for _, tc := range []struct {
		on, noBlink bool
	}{{true, false}, {false, true}, {true, true}} {
		if got := boardFrame(tc.on, tc.noBlink); got != base {
			t.Errorf("null-milestone board frame changed (blinkOn=%v noBlink=%v); D5 requires byte-identity", tc.on, tc.noBlink)
		}
	}

	// Detail rail: a plain run with a live frame, phase off vs on.
	detailFrame := func(on bool) string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, plainRun.ID)
		m = applyDetail(m, plainRun,
			[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "planning", now)})
		m.blinkOn = on
		return m.View().Content
	}
	if detailFrame(true) != detailFrame(false) {
		t.Error("null-milestone detail frame changed with the blink phase; D5 requires byte-identity")
	}
}

// The crew rail's now line is derived from the rail's OWN frames via runactivity.Latest (the
// same rule the server runs for the DTO), so it renders under the in-progress milestone: the
// role, an age, and the italic task label. A tool_use frame drives it.
func TestTUIRailNowLineFromFrames(t *testing.T) {
	now := time.Now()
	runID := "bbbbbbbb-1111"
	run := apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "structured",
		Milestones: []apitypes.Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"},
			{ID: "m3", Title: "Gamma"}},
		MilestonesCompleted: []string{"m1"}, MilestonesInProgress: []string{"m2"}}
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	// An Edit tool_use frame by a subagent: Detail is the file path, and the frame's agent_label
	// is the dispatch task label.
	editPayload := json.RawMessage(`{"name":"Edit","input":{"file_path":"api/internal/poller/ci_autofix.go"}}`)
	agent, label, at := "coder", "Decouple ci_fix detector from branch naming", now
	m = applyDetail(m, run, []apitypes.MessageDTO{
		{Seq: 1, Kind: "tool_use", Agent: &agent, AgentLabel: &label, CreatedAt: at, Payload: editPayload},
	})
	out := stripANSI(m.View().Content)
	// The `↳ <role> · <age>` line and the italic task label sit under the in-progress milestone.
	if !strings.Contains(out, "↳ coder") {
		t.Errorf("rail now line missing the ↳ role line\n%s", out)
	}
	if !strings.Contains(out, "Decouple ci_fix det") {
		t.Errorf("rail now line missing the italic task label\n%s", out)
	}
	// The eyebrow names the in-progress milestone.
	if !strings.Contains(out, "· m2") {
		t.Errorf("rail eyebrow missing the `· m2` in-progress suffix\n%s", out)
	}
}

// A milestone run with activity but NOTHING declared in progress renders an UNATTACHED now line
// directly under the eyebrow (PRD #1064 mock; #390 D7 — declared, not inferred), and no
// milestone row is marked in progress.
func TestTUIRailUnattachedNowLine(t *testing.T) {
	now := time.Now()
	runID := "cccccccc-1111"
	run := apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "structured",
		Milestones:          []apitypes.Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}},
		MilestonesCompleted: []string{"m1"}} // nothing in progress
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	agent, at := "lead", now
	m = applyDetail(m, run, []apitypes.MessageDTO{
		{Seq: 1, Kind: "tool_use", Agent: &agent, CreatedAt: at,
			Payload: json.RawMessage(`{"name":"Read","input":{"file_path":"api/internal/poller/mr_rework.go"}}`)},
	})
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "↳ lead") {
		t.Errorf("an unattached now line should show under the eyebrow when activity exists but nothing is declared\n%s", out)
	}
	// No `· <id>` suffix, because nothing is declared in progress (not inferred).
	if strings.Contains(out, "· m1") || strings.Contains(out, "· m2") {
		t.Errorf("nothing is in progress, so the eyebrow must carry no `· <id>` suffix\n%s", out)
	}
}

// activityFor is a current_activity fixture with a distinctive role/label per run.
func activityFor(role, label string, at time.Time) *apitypes.RunActivity {
	return &apitypes.RunActivity{Agent: role, AgentLabel: label, Tool: "Edit",
		Detail: "api/x.go", At: at}
}

// PRD #1064 D4: the SELECTED board row gains a second `▸ … · <role> <label> · <age>` line from
// current_activity — for the TOP row and the BOTTOM row — and the board's selection/scroll math
// tolerates that one variable-height row without overflowing the terminal height (D4: it is a
// DETAIL-rail precedent, not a board one, so the window must reserve its line).
func TestTUIBoardSecondLineTopAndBottom(t *testing.T) {
	now := time.Now()
	runs := []apitypes.RunListItemDTO{}
	// Enough rows to force windowing at a modest height.
	for i, role := range []string{"coder", "tester", "reviewer", "auditor", "lead", "coder2"} {
		r := milestoneRunItem("aaaaaaaa-"+itoa(i), "running", 1, "m2")
		r.CurrentActivity = activityFor(role, "task-"+role, now)
		runs = append(runs, r)
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 120, 16
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)

	fits := func(label string) {
		t.Helper()
		out := m.View().Content
		if rows := strings.Split(out, "\n"); len(rows) > m.height {
			t.Fatalf("%s: board rendered %d rows at height %d — the variable-height second line overflowed\n%s",
				label, len(rows), m.height, out)
		}
	}

	// TOP row selected: its second line shows under it, riding the selection bar.
	sel, _ := m.board.selected()
	topLine := stripANSI(m.boardSecondLine(sel))
	if !strings.Contains(topLine, "coder") || !strings.Contains(topLine, "task-coder") {
		t.Errorf("top selected row's second line missing its activity\n%q", topLine)
	}
	if !strings.Contains(m.View().Content, "task-coder") {
		t.Errorf("top row second line not in the frame\n%s", m.View().Content)
	}
	fits("top")

	// Move to the BOTTOM row: its second line shows, the previous top row's does not.
	for i := 0; i < len(runs)-1; i++ {
		m = press(t, m, "j")
	}
	botSel, _ := m.board.selected()
	if !strings.Contains(stripANSI(m.boardSecondLine(botSel)), "task-coder2") {
		t.Errorf("bottom selected row's second line missing its activity\n%q", stripANSI(m.boardSecondLine(botSel)))
	}
	out := m.View().Content
	if !strings.Contains(out, "task-coder2") {
		t.Errorf("bottom row second line not in the frame\n%s", out)
	}
	// Exactly one second line at a time (only the selected row has one).
	if n := strings.Count(stripANSI(out), " ▸ "); n != 1 {
		t.Errorf("expected exactly one selected-row second line, got %d\n%s", n, stripANSI(out))
	}
	fits("bottom")
}

// PRD #1064 D4: with the window FORCED (many runs, small height) and the BOTTOM row selected,
// boardCapacity's `chrome++` reservation for the selected row's second line is load-bearing —
// it shrinks the row window by one so the selected row PLUS its second physical line both fit
// without pushing the footer off the terminal. Unlike TestTUIBoardSecondLineTopAndBottom (6
// rows at height 16, where every item fits and the window is never engaged), this forces
// n > capacity so the reservation actually binds: delete the `chrome++` in boardCapacity and
// this test goes RED (frame is height+1 lines).
func TestTUIBoardSecondLineWindowReservesLine(t *testing.T) {
	now := time.Now()
	var runs []apitypes.RunListItemDTO
	// 20 running rows at height 16 forces windowing (21 display items incl. the band eyebrow,
	// far more than the ~12 that fit), and every row carries a milestone in progress + activity.
	for i := 0; i < 20; i++ {
		r := milestoneRunItem("aaaaaaaa-"+itoa(i), "running", 1, "m2")
		r.CurrentActivity = activityFor("coder", "task-"+itoa(i), now)
		runs = append(runs, r)
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 120, 16
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)

	// Select the BOTTOM row, whose second line rides the very bottom of the window — exactly the
	// row for which the reservation must have shrunk the window or the footer is pushed off.
	for i := 0; i < len(runs)-1; i++ {
		m = press(t, m, "j")
	}
	sel, ok := m.board.selected()
	if !ok || sel.ID != "aaaaaaaa-19" {
		t.Fatalf("expected the bottom row (aaaaaaaa-19) selected, got %q ok=%v", sel.ID, ok)
	}

	out := m.View().Content
	// The frame never overflows the terminal height: the window reserved one line for the
	// selected row's second line rather than letting it push the footer off screen.
	if rows := strings.Split(out, "\n"); len(rows) > m.height {
		t.Fatalf("board rendered %d physical lines at height %d — the reserved second line overflowed\n%s",
			len(rows), m.height, out)
	}
	// The selected bottom row AND its second line are both visible: the window shrank by one to
	// keep them on screen, not by dropping the second line.
	if !strings.Contains(out, "task-19") {
		t.Fatalf("selected bottom row's second line (task-19) not in the frame\n%s", out)
	}
	if n := strings.Count(stripANSI(out), " ▸ "); n != 1 {
		t.Fatalf("expected exactly one selected-row second line, got %d\n%s", n, stripANSI(out))
	}
}

// The selection can move across a run that GAINS or LOSES its activity between 2s polls: the
// second line appears/disappears and the window math stays sound (D4).
func TestTUIBoardSecondLineGainsAndLosesActivity(t *testing.T) {
	now := time.Now()
	withAct := []apitypes.RunListItemDTO{
		milestoneRunItem("aaaaaaaa-0", "running", 1, "m2"),
		milestoneRunItem("aaaaaaaa-1", "running", 1, "m2"),
	}
	withAct[1].CurrentActivity = activityFor("coder", "task-coder", now)
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 120, 20
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: withAct})
	m = next.(tuiModel)
	m = press(t, m, "j") // select the run WITH activity

	if !strings.Contains(m.View().Content, "task-coder") {
		t.Fatalf("selected run with activity should show its second line\n%s", m.View().Content)
	}
	baseRows := len(strings.Split(m.View().Content, "\n"))

	// Next poll: the SAME run loses its activity (the lane went quiet). The second line vanishes.
	lost := []apitypes.RunListItemDTO{
		milestoneRunItem("aaaaaaaa-0", "running", 1, "m2"),
		milestoneRunItem("aaaaaaaa-1", "running", 1, "m2"),
	}
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: lost})
	m = next.(tuiModel)
	if strings.Contains(m.View().Content, "task-coder") {
		t.Errorf("the selected run lost its activity but the second line is still drawn\n%s", m.View().Content)
	}
	if got := len(strings.Split(m.View().Content, "\n")); got > baseRows {
		t.Errorf("losing the second line grew the frame from %d to %d rows", baseRows, got)
	}

	// Next poll: it gains activity again. The second line returns.
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: withAct})
	m = next.(tuiModel)
	if !strings.Contains(m.View().Content, "task-coder") {
		t.Errorf("the selected run regained activity but the second line did not return\n%s", m.View().Content)
	}
}
