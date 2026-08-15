package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// The model is driven IN PROCESS, with no PTY: Update takes a message and returns a
// model, and View returns a string. That is a design constraint, not a testing
// convenience — a model that can only be exercised through a terminal cannot be
// asserted on in the ordinary `go test ./...` gate, which is where this has to run.

func tuiTestModel(t *testing.T, c uzicli.Client, startRun string) tuiModel {
	t.Helper()
	m := newTUIModel(context.Background(), c, startRun)
	m.width, m.height = 120, 40
	return m
}

// press drives one key through the real key path and returns the updated model.
func press(t *testing.T, m tuiModel, k string) tuiModel {
	t.Helper()
	next, _ := m.handleKey(k)
	tm, ok := next.(tuiModel)
	if !ok {
		t.Fatalf("handleKey(%q) returned %T, not tuiModel", k, next)
	}
	return tm
}

func msgDTO(seq int32, kind, agent, instance, label, text string, at time.Time) apitypes.MessageDTO {
	m := apitypes.MessageDTO{Seq: seq, Kind: kind, CreatedAt: at,
		Payload: json.RawMessage(`{"text":` + quoteJSON(text) + `}`)}
	if agent != "" {
		m.Agent = &agent
	}
	if instance != "" {
		m.AgentInstance = &instance
	}
	if label != "" {
		m.AgentLabel = &label
	}
	return m
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestTUIBoardRendersRunsAndMoves(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running", IssueTitle: "first issue"}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-1111-2222-3333-444444444444", Kind: "ci_fix", Status: "completed", IssueTitle: "second issue"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{runs: fake.Runs})
	m = next.(tuiModel)

	out := m.View().Content
	for _, want := range []string{"aaaaaaaa", "bbbbbbbb", "first issue", "second issue", "running", "completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("board does not render %q\n%s", want, out)
		}
	}
	// The cursor starts on the first row and moves with both j and the arrow.
	if got, _ := m.board.selected(); got.ID != fake.Runs[0].ID {
		t.Fatalf("cursor starts on %q, want the first run", got.ID)
	}
	m = press(t, m, "j")
	if got, _ := m.board.selected(); got.ID != fake.Runs[1].ID {
		t.Errorf("j did not move the cursor to the second run")
	}
	m = press(t, m, keyUp)
	if got, _ := m.board.selected(); got.ID != fake.Runs[0].ID {
		t.Errorf("↑ did not move the cursor back to the first run")
	}
}

// The `[a]` admin toggle is refused CLEANLY with a non-admin token, not crashed (D8),
// and the board falls back to the caller's own runs.
func TestTUIBoardAdminToggleIsRefusedCleanly(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111", Kind: "issue", Status: "running"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{runs: fake.Runs})
	m = next.(tuiModel)

	m = press(t, m, keyAdmin)
	if !m.board.admin {
		t.Fatal("[a] did not turn the admin view on")
	}
	// The server refuses a uzc_ token on the admin surface.
	next, _ = m.Update(boardRunsMsg{admin: true, err: uzicli.Exitf(uzicli.ExitAuth, "admin access required")})
	m = next.(tuiModel)

	if m.board.admin {
		t.Error("a refused admin list left the board in admin mode; the toggle must fall back to the caller's own runs")
	}
	if !m.board.adminDenied {
		t.Error("the refusal was not recorded, so it cannot be explained on screen")
	}
	out := m.View().Content
	if !strings.Contains(out, "admin") {
		t.Errorf("the refusal is not explained on screen; D8 requires a clear message, not a silent revert\n%s", out)
	}
	// And the caller's own runs are still there — a refused toggle must not blank the
	// board it was toggled away from.
	if len(m.board.runs) != 1 {
		t.Errorf("the own-runs list was lost on a refused admin toggle (%d rows)", len(m.board.runs))
	}
}

// The admin board is labelled "active runs", because AdminListRuns returns
// non-terminal runs only. Promising completed rows there is a claim the API cannot
// satisfy.
func TestTUIAdminBoardIsLabelledActiveRuns(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = press(t, m, keyAdmin)
	next, _ := m.Update(boardRunsMsg{admin: true, runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "cccccccc-1111", Kind: "issue", Status: "running"}},
	}})
	m = next.(tuiModel)
	out := m.View().Content
	if !strings.Contains(out, "active runs") {
		t.Errorf("the admin board must be labelled \"active runs\" — AdminListRuns returns non-terminal runs only, so a plain \"runs\" header promises completed rows the API never returns\n%s", out)
	}
}

func TestTUIBoardFilter(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "add the widget"}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "ci_fix", Status: "running", IssueTitle: "fix the pipeline"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{runs: fake.Runs})
	m = next.(tuiModel)

	m = press(t, m, keyFilter)
	for _, k := range []string{"p", "i", "p", "e"} {
		m = press(t, m, k)
	}
	if n := len(m.board.visible()); n != 1 {
		t.Fatalf("filter %q matched %d rows, want 1", m.board.filter, n)
	}
	// While filtering, an ordinary letter TYPES rather than triggering its binding —
	// otherwise "a" would flip to the admin board mid-search.
	m = press(t, m, "a")
	if m.board.admin {
		t.Error("typing \"a\" into the filter toggled the admin board; filter input must swallow ordinary keys")
	}
	m = press(t, m, keyEsc)
	if m.board.filter != "" || len(m.board.visible()) != 2 {
		t.Errorf("esc did not clear the filter (filter=%q, %d visible)", m.board.filter, len(m.board.visible()))
	}
}

// Quit is confirmed, and BOTH q and ctrl+c route through the modal — a stray key must
// not drop a watched run. A second ctrl+c is the escape hatch.
func TestTUIQuitIsConfirmed(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")

	m = press(t, m, keyQuit)
	if !m.quitting {
		t.Fatal("q did not open the quit confirmation")
	}
	if !strings.Contains(m.View().Content, "Quit") {
		t.Error("the quit confirmation is not rendered")
	}
	// Any other key cancels.
	m = press(t, m, "x")
	if m.quitting {
		t.Error("a non-confirming key did not dismiss the quit modal")
	}

	// ctrl+c opens the same modal rather than quitting outright.
	next, cmd := m.handleKey(keyCtrlC)
	m = next.(tuiModel)
	if !m.quitting {
		t.Error("ctrl+c did not route through the confirm modal")
	}
	if cmd != nil {
		t.Error("the first ctrl+c returned a command; it must only open the modal")
	}
	// The second ctrl+c quits immediately — the way out when the modal is the problem.
	if _, cmd = m.handleKey(keyCtrlC); cmd == nil {
		t.Error("a second ctrl+c did not quit; there must be an escape hatch that does not depend on the modal")
	}
}

// M3: the plan gate shows the amber PLAN GATE banner and, for the OWNER, promoted
// approve/reject keycaps; the detail header carries a semantic status chip.
func TestTUIDetailPlanGateBanner(t *testing.T) {
	runID := "pg-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_approval"}})
	m = next.(tuiModel)
	next, _ = m.Update(runInputsMsg{runID: runID}) // err nil → owner → steerAllowed
	m = next.(tuiModel)
	out := m.View().Content

	if !strings.Contains(out, "PLAN GATE") {
		t.Errorf("plan-gate banner missing\n%s", out)
	}
	// The owner's footer leads with approve/reject at the gate (M4 one-line footer).
	for _, want := range []string{"approve", "reject"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan gate does not offer %q for the owner\n%s", want, out)
		}
	}
	// The header status chip is a solid colour fill (a truecolor background SGR).
	if !strings.Contains(out, "\x1b[48;2;") {
		t.Errorf("detail header has no status chip fill\n%s", out)
	}
}

// M3 S3: awaiting_input gets a DISTINCT banner and NEVER offers y/n — those keys do
// nothing at a clarification park (which is answered off-TUI).
func TestTUIDetailInputBannerIsDistinctAndHasNoYesNo(t *testing.T) {
	runID := "in-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_input"}})
	m = next.(tuiModel)
	next, _ = m.Update(runInputsMsg{runID: runID}) // owner
	m = next.(tuiModel)
	out := m.View().Content

	if !strings.Contains(out, "NEEDS INPUT") {
		t.Errorf("awaiting_input banner missing\n%s", out)
	}
	if strings.Contains(out, "PLAN GATE") {
		t.Errorf("awaiting_input must NOT show the plan-gate banner\n%s", out)
	}
	if strings.Contains(out, "[y]") || strings.Contains(out, "approve") {
		t.Errorf("awaiting_input offered approve/reject, which does nothing at a clarification park\n%s", out)
	}
}

// M3 N1: the plan-gate banner shows for a NON-OWNER (informational), but the promoted
// approve/reject keys are ownership-gated and must not appear.
func TestTUIDetailPlanGateBannerNonOwnerHasNoKeys(t *testing.T) {
	runID := "pg-2"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_approval"}})
	m = next.(tuiModel)
	// RunInputs 404 → steerNotOwner (an admin observing another user's run).
	next, _ = m.Update(runInputsMsg{runID: runID, err: uzicli.Exitf(uzicli.ExitNotFound, "not found")})
	m = next.(tuiModel)
	out := m.View().Content

	if !strings.Contains(out, "PLAN GATE") {
		t.Errorf("the plan-gate banner must show even for a non-owner\n%s", out)
	}
	if strings.Contains(out, "[y]") || strings.Contains(out, "approve") {
		t.Errorf("a non-owner must not be offered approve/reject keys\n%s", out)
	}
	if !strings.Contains(out, "read-only") {
		t.Errorf("the non-owner steer bar should explain it is read-only\n%s", out)
	}
}

// M6: the review overlay colours the verdict chip by SEVERITY (issues red, ideal teal) via
// the shared verdictColor, not the old uniform brand-blue. Asserted through the chip's
// background-fill SGR, and that the two verdicts resolve to different colours.
func TestTUIReviewVerdictSeverityColour(t *testing.T) {
	render := func(verdict string) string {
		runID := "rv-" + verdict
		m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Status: "completed"}})
		m = next.(tuiModel)
		m = press(t, m, "v")
		next, _ = m.Update(reviewLoadedMsg{runID: runID, review: &apitypes.ReviewDTO{Verdict: verdict}})
		m = next.(tuiModel)
		return m.View().Content
	}
	pal := newPalette(true)
	issuesBg := bgFillSGR(pal.verdictColor("issues"))
	idealBg := bgFillSGR(pal.verdictColor("ideal"))
	if issuesBg == idealBg {
		t.Fatal("issues and ideal resolve to the same colour; the severity test cannot distinguish them")
	}
	if out := render("issues"); !strings.Contains(out, issuesBg) {
		t.Errorf("the issues verdict chip is not the failed (red) colour %q\n%s", issuesBg, out)
	}
	if out := render("ideal"); !strings.Contains(out, idealBg) {
		t.Errorf("the ideal verdict chip is not the completed (teal) colour %q\n%s", idealBg, out)
	}
}

// bgFillSGR is the truecolor background SGR lipgloss emits for a chip's fill.
func bgFillSGR(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("48;2;%d;%d;%d", r>>8, g>>8, b>>8)
}

// The detail view: replay builds lanes, and a live frame extends them.
func TestTUIDetailBuildsLanesFromReplayThenLiveFrames(t *testing.T) {
	now := time.Now()
	runID := "dddddddd-1111-2222-3333-444444444444"
	fake := &uzicli.FakeClient{}
	m := tuiTestModel(t, fake, runID)

	next, _ := m.Update(detailLoadedMsg{
		run: apitypes.RunDTO{ID: runID, Status: "running", Health: "ok"},
		msgs: []apitypes.MessageDTO{
			msgDTO(1, "text", "lead", "", "", "planning", now.Add(-2*time.Minute)),
			msgDTO(2, "text", "coder", "toolu_aaa111", "write the tests", "writing", now.Add(-time.Minute)),
		},
	})
	m = next.(tuiModel)

	if len(m.detail.lanes) != 2 {
		t.Fatalf("replay produced %d lanes, want 2", len(m.detail.lanes))
	}
	out := m.View().Content
	for _, want := range []string{"lead", "coder", "write the tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail view does not render %q\n%s", want, out)
		}
	}

	// A live frame for a NEW invocation opens a third lane.
	inst, agent := "toolu_bbb222", "tester"
	at := now
	next, _ = m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{{
		Type: uzicli.RunEventTypeMessage, Seq: 3, Kind: "text",
		Agent: &agent, AgentInstance: &inst, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"testing"}`),
	}}})
	m = next.(tuiModel)
	if len(m.detail.lanes) != 3 {
		t.Fatalf("a live frame for a new invocation produced %d lanes, want 3", len(m.detail.lanes))
	}
}

// A frame that arrives over BOTH transports must not be counted twice: a reconnect
// replays from the last seq seen and the socket can deliver the same frame.
func TestTUIDetailDedupesBySeqAcrossTransports(t *testing.T) {
	now := time.Now()
	runID := "eeeeeeee-1111"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	next, _ := m.Update(detailLoadedMsg{
		run:  apitypes.RunDTO{ID: runID, Status: "running"},
		msgs: []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "hello", now)},
	})
	m = next.(tuiModel)

	agent, at := "lead", now
	next, _ = m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{{
		Type: uzicli.RunEventTypeMessage, Seq: 1, Kind: "text", Agent: &agent, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"hello"}`),
	}}})
	m = next.(tuiModel)

	if n := len(m.detail.frames); n != 1 {
		t.Errorf("seq 1 arrived over replay AND the socket and was kept %d times; a duplicate doubles a lane's contribution", n)
	}
}

// A state frame is authoritative — including the SYNTHETIC one StreamRun's reconcile
// emits when a terminal frame was dropped. The view must not need to tell them apart.
func TestTUIDetailAppliesStateFrames(t *testing.T) {
	runID := "ffffffff-1111"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)

	next, _ = m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{
		{Type: uzicli.RunEventTypeState, Status: "completed"},
	}})
	m = next.(tuiModel)
	if m.detail.run.Status != "completed" {
		t.Errorf("run status = %q after a state frame, want completed", m.detail.run.Status)
	}
	// And every lane then reads `done`, which is rung 1 of the ladder.
	if !strings.Contains(m.View().Content, laneDot(crewDone)) && len(m.detail.lanes) > 0 {
		t.Error("lanes did not fall to the done dot after the run reached a terminal state")
	}
}

// D8: an unusable socket degrades to the REST poll WITH A VISIBLE REASON, never a
// crash and never a silently stale pane.
func TestTUIDetailDegradesWhenTheStreamCannotOpen(t *testing.T) {
	runID := "99999999-1111"
	fake := &uzicli.FakeClient{
		StreamErr: uzicli.Exitf(uzicli.ExitUnreachable, "cannot open the run stream"),
		RunByID:   map[string]apitypes.RunDTO{runID: {ID: runID, Status: "running"}},
	}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)

	next, cmd := m.Update(streamReadyMsg{runID: runID, err: fake.StreamErr})
	m = next.(tuiModel)
	if !m.detail.polling {
		t.Fatal("a failed stream did not fall back to polling")
	}
	if cmd == nil {
		t.Error("the fallback returned no command, so nothing will ever refresh the pane")
	}
	out := m.View().Content
	if !strings.Contains(out, "live stream unavailable") {
		t.Errorf("the degradation is not visible on screen; a user looking at a stale pane must be able to see WHY\n%s", out)
	}
}

// A late reply for a run the user has already left must be ignored, or it would
// overwrite the run they are now looking at.
func TestTUIDetailIgnoresRepliesForAnotherRun(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-current")
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: "run-current", Status: "running"}})
	m = next.(tuiModel)

	agent, at := "coder", time.Now()
	next, _ = m.Update(streamEventsMsg{runID: "run-OTHER", events: []apitypes.RunEventDTO{{
		Type: uzicli.RunEventTypeMessage, Seq: 9, Kind: "text", Agent: &agent, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"from another run"}`),
	}}})
	m = next.(tuiModel)

	if len(m.detail.frames) != 0 {
		t.Error("a stream batch for a DIFFERENT run was applied to the current one")
	}
}

// M4: the detail view has two focusable panes. ←/→ (and tab) select the pane; ↑/↓ act
// WITHIN it — moving between agents on the rail, scrolling the transcript. Default focus is
// the crew rail; the focused pane title carries the ▎ marker.
func TestTUIDetailFocusPaneNavigation(t *testing.T) {
	now := time.Now()
	runID := "77777777-1111"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{
		run: apitypes.RunDTO{ID: runID, Status: "running"},
		msgs: []apitypes.MessageDTO{
			msgDTO(1, "text", "lead", "", "", "a", now),
			msgDTO(2, "text", "coder", "toolu_a", "", "b", now),
			msgDTO(3, "text", "tester", "toolu_b", "", "c", now),
		},
	})
	m = next.(tuiModel)

	// Detail opens focused on the crew rail.
	if m.detail.focus != focusRail {
		t.Fatalf("detail opened focused on pane %d, want the crew rail", m.detail.focus)
	}
	// With the rail focused, j moves BETWEEN agents (it does not scroll).
	m = press(t, m, "j")
	if m.detail.laneIdx != 1 {
		t.Errorf("j on the focused rail moved to lane %d, want 1", m.detail.laneIdx)
	}
	if m.detail.scroll != 0 {
		t.Errorf("j on the rail scrolled the transcript (scroll=%d); it should move agents", m.detail.scroll)
	}

	// → focuses the transcript; now j scrolls and does NOT change the agent.
	m = press(t, m, keyRight)
	if m.detail.focus != focusTranscript {
		t.Fatal("→ did not focus the transcript")
	}
	lane := m.detail.laneIdx
	m = press(t, m, "j")
	if m.detail.scroll == 0 {
		t.Error("j on the focused transcript did not scroll")
	}
	if m.detail.laneIdx != lane {
		t.Error("j on the transcript changed the selected agent; it should scroll")
	}

	// ← returns focus to the rail; moving agents resets the scroll and wraps.
	m = press(t, m, keyLeft)
	if m.detail.focus != focusRail {
		t.Fatal("← did not focus the crew rail")
	}
	m = press(t, m, "k")
	if m.detail.laneIdx != 0 {
		t.Errorf("k on the rail moved to lane %d, want 0", m.detail.laneIdx)
	}
	if m.detail.scroll != 0 {
		t.Error("moving agents did not reset the scroll")
	}
	m = press(t, m, "k") // wrap backwards past the first lane
	if m.detail.laneIdx != 2 {
		t.Errorf("k did not wrap to the last lane, got %d", m.detail.laneIdx)
	}

	// tab cycles focus, and the focused pane title carries the ▎ marker.
	m = press(t, m, keyTab)
	if m.detail.focus != focusTranscript {
		t.Errorf("tab did not cycle focus to the transcript (got %d)", m.detail.focus)
	}
	if !strings.Contains(m.View().Content, "▎") {
		t.Error("no focus indicator (▎) rendered on the focused pane")
	}
}

// M4: the detail keymap is a SINGLE line — navigation and the owner's actions combined,
// not the pre-M4 two-line (steer key list + separate nav footer) region.
func TestTUIDetailFooterIsOneLine(t *testing.T) {
	runID := "foot-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}})
	m = next.(tuiModel)
	next, _ = m.Update(runInputsMsg{runID: runID}) // owner → steerAllowed
	m = next.(tuiModel)

	lines := strings.Split(strings.TrimRight(m.View().Content, "\n"), "\n")
	last := lines[len(lines)-1]
	// Navigation ("pane"/"move") and an action ("follow-up") share the ONE footer line.
	for _, want := range []string{"pane", "move", "follow-up"} {
		if !strings.Contains(last, want) {
			t.Errorf("the detail footer is not one combined line (missing %q)\nlast line: %q", want, last)
		}
	}
}

// esc returns to the board and CLOSES the stream — otherwise every run a user opens
// leaks a socket and a goroutine for the life of the session.
func TestTUIDetailEscReturnsToBoard(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-1")
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: "run-1", Status: "running"}})
	m = next.(tuiModel)

	stream := uzicli.NewRunStream(context.Background(), nil)
	m.detail.stream = stream

	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatal("esc did not return to the board")
	}
	select {
	case <-stream.Events():
	case <-time.After(2 * time.Second):
		t.Error("esc left the run's stream open; each opened run would leak a socket and a goroutine")
	}
}

// The whole untrusted surface goes through the sanitizer. This drives REAL model
// state rather than calling the helpers directly, so it covers the wiring too.
func TestTUIViewsStripControlBytesFromUntrustedText(t *testing.T) {
	now := time.Now()
	// Hostile bytes at the FRONT: capCell/Plain truncate to 8-60 runes, so a payload at
	// the tail can be legitimately cut and produce a false green. shortInstanceID keeps
	// only 8 runes, which is the tightest budget any of these fixtures meets.
	const nasty = "\x1b[2J\u202E\x07\x01safe"
	runID := "88888888-1111"

	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: nasty}},
	}}
	board := tuiTestModel(t, fake, "")
	next, _ := board.Update(boardRunsMsg{runs: fake.Runs})
	board = next.(tuiModel)
	assertNoRawControls(t, "board", board.View().Content)

	detail := tuiTestModel(t, fake, runID)
	next, _ = detail.Update(detailLoadedMsg{
		run: apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: nasty},
		msgs: []apitypes.MessageDTO{
			msgDTO(1, "text", nasty, "toolu_"+nasty, nasty, nasty, now),
		},
	})
	detail = next.(tuiModel)
	assertNoRawControls(t, "detail", detail.View().Content)

	// The ADMIN board is the only view that draws OwnerEmail (PRD #325 M2, B1). A hostile
	// OwnerEmail must not emit control bytes into the frame. The clean-fixture screenshots
	// cannot catch this, so this is the guard. OwnerEmail is *string, so bind a local.
	owner := nasty
	adminRuns := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: nasty}, OwnerEmail: &owner},
	}
	adm := tuiTestModel(t, &uzicli.FakeClient{}, "")
	adm = press(t, adm, keyAdmin)
	next, _ = adm.Update(boardRunsMsg{admin: true, runs: adminRuns})
	adm = next.(tuiModel)
	admOut := adm.View().Content
	if !strings.Contains(admOut, "OWNER") {
		t.Fatalf("the admin board is not showing the OWNER column, so this test is not exercising the OwnerEmail render path\n%s", admOut)
	}
	assertNoRawControls(t, "admin board", admOut)
}

// M2: the board encodes run status as a colour chip + spine and a summary bar. Automatable
// via the View() substring / SGR seam so CI gates the semantics per milestone (B2).
func TestTUIBoardSemanticStatusAndSummary(t *testing.T) {
	issues := "issues"
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "one"}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "ci_fix", Status: "awaiting_approval", IssueTitle: "two"}},
		{RunDTO: apitypes.RunDTO{ID: "cccccccc-3", Kind: "issue", Status: "running", Health: "stalled", IssueTitle: "three"}},
		{RunDTO: apitypes.RunDTO{ID: "dddddddd-4", Kind: "issue", Status: "running", Health: "looping", IssueTitle: "four"}},
		{RunDTO: apitypes.RunDTO{ID: "eeeeeeee-5", Kind: "issue", Status: "completed", IssueTitle: "five"}, JudgeVerdict: &issues, JudgeTodoCount: 2},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{runs: fake.Runs})
	m = next.(tuiModel)
	out := m.View().Content

	// "looping" is NOT stalled, so it is not counted as stalled — but its WORD must show in
	// the HEALTH column (restored, not reduced to a faint marker). F4: the judge marker
	// carries the todo count ("issues · 2") when JudgeTodoCount > 0.
	for _, want := range []string{"5 runs", "1 needs you", "1 stalled", "looping", "issues · 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("board missing %q\n%s", want, out)
		}
	}
	// The status chip + spine are solid colour fills: a truecolor background SGR (48;2;…).
	// Plain status text alone (the old board) would carry no background fill.
	if !strings.Contains(out, "\x1b[48;2;") {
		t.Errorf("board has no background-fill SGR; the status chip/spine colour is missing\n%s", out)
	}
	// The NO_COLOR-safe spine glyph for a running run survives independent of colour.
	if !strings.Contains(out, statusGlyph("running")) {
		t.Errorf("running spine glyph %q absent from the board\n%s", statusGlyph("running"), out)
	}
}

// assertNoRawControls fails on anything in a rendered frame that is not printable text
// or a legitimate SGR colour sequence.
//
// IT USED TO BE BLIND TO MOST OF ITS OWN CLASS, and the fixture it shipped with was in
// the blind spot. Measured against the old loop, 11 of 14 hostile inputs went unflagged:
// ESC[2J, ESC[H, ESCc, ESC[2K, U+202A, U+2066, U+200F, U+00AD, U+202E-after-ESC[, and
// bare 0x01 / 0x0b. Only a lone U+202E, a lone BEL, and an OSC's trailing BEL were seen.
// The test passed for a real reason — sanitizeTTY genuinely strips these — but the
// assertion could not fail on them, so it was not what held the property. It caught the
// unsanitized-instance-id defect via U+202E and BEL, the two things it could see.
//
// Four causes, all fixed here:
//
//  1. It skipped from ESC to the first ASCII letter, which is a blanket amnesty: J, H,
//     K and c are letters, so every cursor/erase/reset sequence was consumed as though
//     it were lipgloss styling. Now only ESC [ [0-9;:]* m — an SGR sequence — is
//     allowed, and everything else is a finding. That turns "skip to a letter" (a
//     pattern) into "only SGR may appear" (an identity).
//  2. The format-character arm was a three-codepoint list while production strips all
//     of unicode.Cf. Now the same predicate. This is not circular: it asserts on the
//     rendered FRAME, not on sanitizeTTY's return value, so it stays end to end.
//  3. The control arm never tested r < 0x20 generally, so most of C0 was unflagged.
//  4. The escape-state check ran BEFORE the control/Cf arms, so anything between ESC
//     and a letter was sheltered. Order is now reversed.
//
// CAVEAT, and treat a red here as information rather than a defect: nobody has proven
// the frames contain ONLY SGR. If lipgloss v2 starts emitting OSC 8 hyperlinks, this
// goes red on legitimate output. Widen the allowlist with a named reason — never
// restore the blanket skip, which is what made it blind.
//
// RESIDUAL, stated rather than papered over: the steer bar, help overlay and quit modal
// are not driven by any caller of this, so their draws are unasserted. That decay is
// bounded and visible (a view nobody drove), unlike a guard that silently stops
// matching. And this says nothing about structural markdown spoofing — "# VERDICT:
// APPROVED" is valid markdown, not a hostile byte; provenanceBox handles that and is
// tested separately.
func assertNoRawControls(t *testing.T, where, out string) {
	t.Helper()
	rs := []rune(out)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == 0x1b {
			if end, ok := sgrSequenceEnd(rs, i); ok {
				i = end // a legitimate colour sequence; skip past it
				continue
			}
			t.Errorf("%s view emitted a NON-SGR escape sequence at rune %d (%q…) — only ESC[…m colour sequences are legitimate here; a cursor-move, erase, reset or OSC sequence in a rendered frame is either a bug or an injection",
				where, i, string(rs[i:min(i+8, len(rs))]))
			continue
		}
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			t.Errorf("%s view emitted a raw control character %U from untrusted text", where, r)
		}
		if unicode.In(r, unicode.Cf) {
			t.Errorf("%s view emitted a format character %U from untrusted text (a bidi override can make a label read as something it is not, and zero-width runes steal column budget while drawing nothing)", where, r)
		}
	}
}

// sgrSequenceEnd reports the index of the final rune of a legitimate SGR sequence
// starting at i (ESC [ [0-9;:]* m), and whether one is there at all.
func sgrSequenceEnd(rs []rune, i int) (int, bool) {
	j := i + 1
	if j >= len(rs) || rs[j] != '[' {
		return 0, false
	}
	for j++; j < len(rs); j++ {
		c := rs[j]
		if c == ';' || c == ':' || (c >= '0' && c <= '9') {
			continue
		}
		if c == 'm' {
			return j, true
		}
		return 0, false
	}
	return 0, false
}

// The stream reader hands over a BATCH, so a burst of frames costs one re-render
// rather than one per frame — the SSH latency requirement.
func TestReadStreamCmdBatchesQueuedEvents(t *testing.T) {
	events := make([]apitypes.RunEventDTO, 0, 5)
	for i := int32(1); i <= 5; i++ {
		events = append(events, apitypes.RunEventDTO{Type: uzicli.RunEventTypeMessage, Seq: i, Kind: "text"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := uzicli.NewRunStream(ctx, events)
	defer s.Close()

	// The first read blocks for one event; whatever else has queued behind it comes in
	// the same message. The fake emits as fast as the reader drains, so the batch is
	// timing-dependent in SIZE but must never exceed what was sent, and repeated reads
	// must together deliver everything exactly once.
	var got []int32
	deadline := time.After(5 * time.Second)
	for len(got) < len(events) {
		select {
		case <-deadline:
			t.Fatalf("only %d of %d events arrived: %v", len(got), len(events), got)
		default:
		}
		msg, ok := readStreamCmd("r", s)().(streamEventsMsg)
		if !ok {
			t.Fatal("readStreamCmd returned the wrong message type")
		}
		for _, ev := range msg.events {
			got = append(got, ev.Seq)
		}
		if msg.closed {
			break
		}
	}
	if len(got) != len(events) {
		t.Fatalf("got seqs %v, want %d events exactly once", got, len(events))
	}
	for i, seq := range got {
		if seq != int32(i+1) {
			t.Fatalf("events arrived out of order: %v", got)
		}
	}
}

var _ tea.Model = tuiModel{}
