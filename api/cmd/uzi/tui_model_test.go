package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// stripANSI removes SGR escapes so a test can assert on visual text that the renderer splits
// across colour spans (e.g. the milestone micro-bar's tungsten ▰ and faint ▱).
func stripANSI(s string) string { return ansi.Strip(s) }

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

// toolUseMsg builds a tool_use MessageDTO whose payload carries a (possibly hostile) tool `name`,
// for the transcript's `⚙ <name>` path (toolFrameName → buildTranscriptLines). msgDTO only builds
// a {"text":…} payload, which never reaches that branch.
func toolUseMsg(seq int32, agent, instance, name string, at time.Time) apitypes.MessageDTO {
	m := apitypes.MessageDTO{Seq: seq, Kind: "tool_use", CreatedAt: at,
		Payload: json.RawMessage(`{"name":` + quoteJSON(name) + `}`)}
	if agent != "" {
		m.Agent = &agent
	}
	if instance != "" {
		m.AgentInstance = &instance
	}
	return m
}

func TestTUIBoardRendersRunsAndMoves(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running", IssueTitle: "first issue"}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-1111-2222-3333-444444444444", Kind: "ci_fix", Status: "completed", IssueTitle: "second issue"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)

	out := m.View().Content
	// Status renders as the human word now: completed → "done".
	for _, want := range []string{"aaaaaaaa", "bbbbbbbb", "first issue", "second issue", "running", "done"} {
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
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)

	m = press(t, m, keyAdmin)
	if !m.board.admin {
		t.Fatal("[a] did not turn the admin view on")
	}
	// The server refuses a uzc_ token on the admin surface.
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, admin: true, err: uzicli.Exitf(uzicli.ExitAuth, "admin access required")})
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
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, admin: true, runs: []apitypes.RunListItemDTO{
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
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
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

// q quits immediately (user preference), while ctrl+c still routes through the confirm modal
// so a stray ctrl+c cannot drop a watched run; a second ctrl+c is the escape hatch.
func TestTUIQuitKeys(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")

	// q quits at once: it returns the quit command and never opens the modal.
	next, cmd := m.handleKey(keyQuit)
	m = next.(tuiModel)
	if cmd == nil {
		t.Error("q did not return a quit command")
	}
	if m.quitting {
		t.Error("q opened the confirm modal; it should quit immediately")
	}
	if strings.Contains(m.View().Content, "Quit uzi tui?") {
		t.Error("q rendered the quit confirmation; it should quit immediately")
	}

	// ctrl+c opens the confirm modal rather than quitting outright.
	next, cmd = m.handleKey(keyCtrlC)
	m = next.(tuiModel)
	if !m.quitting {
		t.Error("ctrl+c did not route through the confirm modal")
	}
	if cmd != nil {
		t.Error("the first ctrl+c returned a command; it must only open the modal")
	}
	// Any other key cancels the modal.
	m = press(t, m, "x")
	if m.quitting {
		t.Error("a non-confirming key did not dismiss the quit modal")
	}
	// The second ctrl+c quits immediately — the way out when the modal is the problem.
	m = press(t, m, keyCtrlC)
	if _, cmd = m.handleKey(keyCtrlC); cmd == nil {
		t.Error("a second ctrl+c did not quit; there must be an escape hatch that does not depend on the modal")
	}
}

// M3: the plan gate shows the amber PLAN GATE banner and, for the OWNER, promoted
// approve/reject keycaps; the detail header carries a semantic status chip.
func TestTUIDetailPlanGateBanner(t *testing.T) {
	runID := "pg-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_approval"}, nil)
	next, _ := m.Update(runInputsMsg{runID: runID}) // err nil → owner → steerAllowed
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
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_input"}, nil)
	next, _ := m.Update(runInputsMsg{runID: runID}) // owner
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
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_approval"}, nil)
	// RunInputs 404 → steerNotOwner (an admin observing another user's run).
	next, _ := m.Update(runInputsMsg{runID: runID, err: uzicli.Exitf(uzicli.ExitNotFound, "not found")})
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

// PRD #517: awaiting_followup gets its own attention band. The owner's banner names the
// follow-up park and the `f` hint; the non-owner's names the park but OMITS "with f" (the
// `f` steer key is owner-only, so a viewer is pointed at web/Slack, not an inert key); and
// the band fits the 100-col reference frame without truncating.
func TestTUIDetailFollowupBanner(t *testing.T) {
	runID := "fu-1"

	// Owner: nil-err runInputs → steerAllowed.
	mo := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	mo = applyDetail(mo, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_followup"}, nil)
	next, _ := mo.Update(runInputsMsg{runID: runID})
	mo = next.(tuiModel)
	mo.width = 100 // the reference frame width the TUI-UX validator measured against
	ob := mo.detailBanner()
	if !strings.Contains(ob, "AWAITING FOLLOW-UP") {
		t.Errorf("owner follow-up banner missing the head\n%q", ob)
	}
	if !strings.Contains(ob, "with f") {
		t.Errorf("owner follow-up banner should offer the `f` hint\n%q", ob)
	}
	if strings.Contains(ob, "…") {
		t.Errorf("follow-up banner truncated at the 100-col frame (too long)\n%q", ob)
	}
	if w := visualWidth(ob); w > mo.width {
		t.Errorf("follow-up banner width %d exceeds the %d-col frame", w, mo.width)
	}

	// Non-owner: 404 runInputs → steerNotOwner. Same band, but no inert `f` hint.
	mn := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	mn = applyDetail(mn, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "awaiting_followup"}, nil)
	next, _ = mn.Update(runInputsMsg{runID: runID, err: uzicli.Exitf(uzicli.ExitNotFound, "not found")})
	mn = next.(tuiModel)
	mn.width = 100
	nb := mn.detailBanner()
	if !strings.Contains(nb, "AWAITING FOLLOW-UP") {
		t.Errorf("non-owner follow-up banner missing the head\n%q", nb)
	}
	if strings.Contains(nb, "with f") {
		t.Errorf("non-owner banner points a read-only viewer at the owner-only `f` key\n%q", nb)
	}
}

// M6: the review overlay colours the verdict WORD by SEVERITY (issues → alarm red, ideal →
// faint) via the shared verdictColor. No chip fill now — it is a bold coloured word — so this
// asserts through the foreground SGR, and that the two verdicts resolve to different colours.
func TestTUIReviewVerdictSeverityColour(t *testing.T) {
	render := func(verdict string) string {
		runID := "rv-" + verdict
		m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "completed"}, nil)
		m = press(t, m, "v")
		next, _ := m.Update(reviewLoadedMsg{runID: runID, review: &apitypes.ReviewDTO{Verdict: verdict}})
		m = next.(tuiModel)
		return m.View().Content
	}
	pal := newPalette(true)
	issuesFg := fgSGR(pal.verdictColor("issues"))
	idealFg := fgSGR(pal.verdictColor("ideal"))
	if issuesFg == idealFg {
		t.Fatal("issues and ideal resolve to the same colour; the severity test cannot distinguish them")
	}
	// Assert on the verdict WORD's OWN rendered span (bold + its severity colour on the literal
	// word), NOT on a bare foreground SGR. faint (verdictColor's ideal/default) is the ubiquitous
	// chrome colour, so Contains(out, idealFg) is trivially true regardless of the verdict colour —
	// it proved nothing. The overlay renders the word as Foreground(verdictColor).Bold(true), so
	// reconstruct exactly that span here.
	issuesWord := lipgloss.NewStyle().Foreground(pal.verdictColor("issues")).Bold(true).Render("issues")
	idealWord := lipgloss.NewStyle().Foreground(pal.verdictColor("ideal")).Bold(true).Render("ideal")
	if out := render("issues"); !strings.Contains(out, issuesWord) {
		t.Errorf("the issues verdict word is not rendered bold in the alarm (red) severity colour %q\n%s", issuesFg, out)
	}
	if out := render("ideal"); !strings.Contains(out, idealWord) {
		t.Errorf("the ideal verdict word is not rendered bold in the faint severity colour %q\n%s", idealFg, out)
	}
}

// bgFillSGR / fgSGR are the truecolor background / foreground SGR lipgloss emits for a colour.
func bgFillSGR(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("48;2;%d;%d;%d", r>>8, g>>8, b>>8)
}

func fgSGR(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("38;2;%d;%d;%d", r>>8, g>>8, b>>8)
}

// The detail view: replay builds lanes, and a live frame extends them.
func TestTUIDetailBuildsLanesFromReplayThenLiveFrames(t *testing.T) {
	now := time.Now()
	runID := "dddddddd-1111-2222-3333-444444444444"
	fake := &uzicli.FakeClient{}
	m := tuiTestModel(t, fake, runID)

	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", Health: "ok"},
		[]apitypes.MessageDTO{
			msgDTO(1, "text", "lead", "", "", "planning", now.Add(-2*time.Minute)),
			msgDTO(2, "text", "coder", "toolu_aaa111", "write the tests", "writing", now.Add(-time.Minute)),
		})

	// 2 real lanes (lead, coder) plus the prepended aggregated "all agents" lane = 3.
	if len(m.detail.lanes) != 3 {
		t.Fatalf("replay produced %d lanes, want 3", len(m.detail.lanes))
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
	next, _ := m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{{
		Type: uzicli.RunEventTypeMessage, Seq: 3, Kind: "text",
		Agent: &agent, AgentInstance: &inst, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"testing"}`),
	}}})
	m = next.(tuiModel)
	// 3 real lanes now (lead, coder, tester) plus the aggregated "all agents" lane = 4.
	if len(m.detail.lanes) != 4 {
		t.Fatalf("a live frame for a new invocation produced %d lanes, want 4", len(m.detail.lanes))
	}
}

// A frame that arrives over BOTH transports must not be counted twice: a reconnect
// replays from the last seq seen and the socket can deliver the same frame.
func TestTUIDetailDedupesBySeqAcrossTransports(t *testing.T) {
	now := time.Now()
	runID := "eeeeeeee-1111"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"},
		[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "hello", now)})

	agent, at := "lead", now
	next, _ := m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{{
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
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"}, nil)

	next, _ := m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{
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
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"}, nil)

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
	m = applyDetail(m, apitypes.RunDTO{ID: "run-current", Status: "running"}, nil)

	agent, at := "coder", time.Now()
	next, _ := m.Update(streamEventsMsg{runID: "run-OTHER", events: []apitypes.RunEventDTO{{
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
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"},
		[]apitypes.MessageDTO{
			msgDTO(1, "text", "lead", "", "", "a", now),
			msgDTO(2, "text", "coder", "toolu_a", "", "b", now),
			msgDTO(3, "text", "tester", "toolu_b", "", "c", now),
		})

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

	// → focuses the transcript; now ↑/↓ drive the transcript and do NOT change the agent.
	// (Scroll amount is M5's concern and needs a transcript taller than the viewport; here
	// the point is only that the agent selection no longer responds to ↑/↓.)
	m = press(t, m, keyRight)
	if m.detail.focus != focusTranscript {
		t.Fatal("→ did not focus the transcript")
	}
	lane := m.detail.laneIdx
	m = press(t, m, "k")
	if m.detail.laneIdx != lane {
		t.Error("↑ on the transcript changed the selected agent; it should scroll the transcript")
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
	// 4 lanes now: the prepended "all agents" lane at 0, then lead/coder/tester, so the last is 3.
	if m.detail.laneIdx != 3 {
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
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}, nil)
	next, _ := m.Update(runInputsMsg{runID: runID}) // owner → steerAllowed
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

// M5: a live run's transcript follows (tail -f, bottom-anchored) with ⇣ following; a new
// frame auto-tails; scrolling up detaches to ⏸ with an "N new" count; g re-attaches.
func TestTUIDetailFollowLive(t *testing.T) {
	now := time.Now()
	runID := "live-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m.height = 16 // small viewport (vp = height-11 = 5) so a handful of frames overflow

	var msgs []apitypes.MessageDTO
	for i := int32(1); i <= 8; i++ {
		msgs = append(msgs, msgDTO(i, "text", "lead", "", "", fmt.Sprintf("frame %d body", i), now))
	}
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}, msgs)
	m = press(t, m, keyRight) // focus the transcript so ↑/↓ scroll it

	// A live run opens following, bottom-anchored: the newest frame shows, the oldest does not.
	if !m.detail.follow {
		t.Fatal("a live run should open following")
	}
	out := m.View().Content
	if !strings.Contains(out, "following") {
		t.Errorf("the following indicator is not shown while tailing\n%s", out)
	}
	if !strings.Contains(out, "frame 8") || strings.Contains(out, "frame 1 body") {
		t.Errorf("following is not bottom-anchored (newest visible, oldest hidden)\n%s", out)
	}
	// F-M5b: the window shows EXACTLY the viewport's worth of lines — a whole-frame
	// bottom-anchored check alone would miss an end-side ±1 in the window height.
	trLines := strings.Split(m.renderTranscript(), "\n")
	if body := len(trLines) - 1; body != m.transcriptViewport() { // line 0 is the pane title
		t.Errorf("transcript window has %d lines, want the viewport %d", body, m.transcriptViewport())
	}

	// A new frame while following auto-tails to the newest.
	agent, at := "lead", now
	next, _ := m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{{
		Type: uzicli.RunEventTypeMessage, Seq: 9, Kind: "text", Agent: &agent, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"frame 9 body"}`),
	}}})
	m = next.(tuiModel)
	if !m.detail.follow || !strings.Contains(m.View().Content, "frame 9") {
		t.Error("a new frame while following did not auto-tail to the newest")
	}

	// Scrolling up detaches to the paused badge (⏸), reporting the lines below the fold.
	m = press(t, m, "k")
	if m.detail.follow {
		t.Error("scrolling up did not detach follow")
	}
	paused := m.View().Content
	if !strings.Contains(paused, "⏸") {
		t.Errorf("the paused indicator (⏸) is not shown after scrolling up\n%s", paused)
	}
	if !strings.Contains(paused, "1 new") {
		t.Errorf("the paused badge does not report one line below the fold\n%s", paused)
	}

	// g re-attaches follow and jumps to the newest.
	m = press(t, m, keyGoLive)
	if !m.detail.follow {
		t.Error("g did not re-attach follow")
	}
	if !strings.Contains(m.View().Content, "following") {
		t.Error("g did not restore the following indicator")
	}
}

// F-M5a: a paused scroll that a resize leaves ABOVE the new maxTop must not re-arm follow
// on the next UP. Repro: pause near the bottom (scroll = maxTop-1), resize TALLER (bigger
// viewport → smaller maxTop, so the stored scroll is now stale), then UP — follow must stay
// detached and the window must scroll toward OLDER output, not jump to the live tail.
func TestTUIDetailPausedScrollSurvivesResize(t *testing.T) {
	now := time.Now()
	runID := "resize-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m.height = 15 // vp = 5 (header is one row now, issue #666)
	var msgs []apitypes.MessageDTO
	for i := int32(1); i <= 8; i++ {
		msgs = append(msgs, msgDTO(i, "text", "lead", "", "", fmt.Sprintf("frame %d body", i), now))
	}
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}, msgs)
	m = press(t, m, keyRight) // focus the transcript

	m = press(t, m, "k") // one scroll up → paused, scroll = maxTop-1
	if m.detail.follow {
		t.Fatal("scrolling up did not detach follow")
	}
	pausedScroll := m.detail.scroll

	// Resize taller: the viewport grows, so maxTop shrinks below the stored scroll.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 19})
	m = next.(tuiModel)

	m = press(t, m, "k") // UP: scroll toward older output, stay paused
	if m.detail.follow {
		t.Errorf("UP after resizing taller wrongly re-armed follow; the stale paused scroll %d was not reclamped to the new maxTop", pausedScroll)
	}
	if m.detail.scroll >= pausedScroll {
		t.Errorf("UP did not scroll toward older output after the resize (scroll %d, was %d)", m.detail.scroll, pausedScroll)
	}
}

// esc returns to the board and CLOSES the stream — otherwise every run a user opens
// leaks a socket and a goroutine for the life of the session.
func TestTUIDetailEscReturnsToBoard(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-1")
	m = applyDetail(m, apitypes.RunDTO{ID: "run-1", Status: "running"}, nil)

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

	// A hostile AnthropicSecretLabel exercises the board credential cell (boardCredSeg) and the
	// detail rail ACCOUNTS label (railRateMeters, PRD #623 — the header credential tag is gone),
	// both drawn through renderer.Plain. *string, so bind a local.
	hostileLabel := nasty
	// A hostile IssueWebURL exercises the OSC-8 issue-link path (issueLink → oscLink): the
	// forge-authored URL is the link target, so its control bytes must be stripped before
	// they reach the frame. IssueIID is non-nil so the link path actually runs; the default
	// test profile is TrueColor, so the link is emitted (not degraded). *string, so bind a local.
	// The trailing newline+tab+text would forge a whole extra row in the frame (#169 class) if
	// oscLink used sanitizeTTY (which spares \n/\t) instead of its OSC-8-strict strip.
	hostileURL := nasty + "\n  FORGED-ROW\t"
	hostileIID := int64(519)
	// A hostile current_activity on the SELECTED row exercises the board's second "now" line
	// (boardSecondLine, PRD #1064 D4): its Agent (role) and AgentLabel/Detail (task label) are
	// model-authored, unsanitized on the wire, and drawn through renderer.Plain. Its milestone
	// (hostile title) drives the `▸ <id> <title>` prefix too.
	hostileActivity := apitypes.RunActivity{Agent: nasty, AgentLabel: nasty, Tool: "Edit", Detail: nasty}
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: nasty, AnthropicSecretLabel: &hostileLabel,
			Milestones: []apitypes.Milestone{{ID: "m1", Title: nasty}}, MilestonesInProgress: []string{"m1"},
			CurrentActivity: &hostileActivity}},
		{RunDTO: apitypes.RunDTO{ID: "77777777-2222", Kind: "issue", Status: "running", IssueTitle: nasty,
			IssueIID: &hostileIID, IssueWebURL: &hostileURL}},
	}}
	board := tuiTestModel(t, fake, "")
	next, _ := board.Update(boardRunsMsg{reqID: board.board.waitID, runs: fake.Runs})
	board = next.(tuiModel)
	// >1 token so the own board draws the credential cell (the boardCredSeg path).
	next, _ = board.Update(secretsMsg{count: 2})
	board = next.(tuiModel)
	// A hostile rate-limit token Label exercises the factory-floor rate-limit strip
	// (boardRateLimitStrip → rateWindowCell), drawn through renderer.Plain (D7). IsDefault so it
	// clears the sidebar selection and actually renders; status "ok" so it is readable.
	next, _ = board.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{
		{SecretID: "sec-nasty", Label: nasty, IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 40}, SevenDay: &apitypes.RateLimitWindow{Pct: 70}}},
		{SecretID: "sec-second", Label: "second", Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 10}, SevenDay: &apitypes.RateLimitWindow{Pct: 20}}},
	}})
	board = next.(tuiModel)
	// Two readable tokens ⇒ showLabel true ⇒ the hostile Label is actually drawn.
	next, _ = board.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: []string{"sec-second"}}})
	board = next.(tuiModel)
	boardOut := board.View().Content
	assertNoRawControls(t, "board", boardOut)
	// Belt-and-braces beyond assertNoRawControls: the raw control bytes from the hostile
	// IssueWebURL must not survive verbatim into the frame (the OSC-8 target is sanitized).
	for _, ctrl := range []string{"\x1b[2J", "\x07", "\x01"} {
		if strings.Contains(boardOut, ctrl) {
			t.Errorf("board frame contains a raw control byte %q from the hostile IssueWebURL", ctrl)
		}
	}
	// The hostile URL's trailing "\n  FORGED-ROW\t" must not survive into the frame as a real
	// line break followed by attacker text — that is the forged-row (#169) class oscLink's
	// OSC-8-strict strip exists to prevent. A regression to sanitizeTTY (which spares \n/\t)
	// would also trip assertNoRawControls above via Fix 2 — defense in depth.
	if strings.Contains(boardOut, "\n  FORGED-ROW") {
		t.Errorf("board frame gained a forged line from the hostile IssueWebURL newline\n%s", boardOut)
	}

	detail := tuiTestModel(t, fake, runID)
	secretID := "sec-hostile"
	// The rail label needs a marker DISTINCT from the title's "safe": the detail title (nasty)
	// also sanitizes to "safe" and is drawn by detailHeaderLines() independently of the rail, so
	// asserting on "safe" would pass via the title even if the rail never drew the label (vacuous).
	// "credsafe" carries the same hostile control/bidi bytes but a tail that only the rail's
	// account-label render can put into the frame.
	detailCredLabel := "\x1b[2J\u202E\x07\x01credsafe" //nolint:gosec // G101: not a credential - a hostile control/bidi-byte sanitization fixture whose display label happens to contain "cred"; the test asserts it is stripped, never a secret.
	// A hostile milestone title exercises renderMilestones' crew-rail draw (D7): the
	// in-progress id makes the row render its title through renderer.Plain. The hostile
	// AnthropicSecretLabel exercises the detail rail ACCOUNTS label (railRateMeters →
	// renderer.Plain, PRD #623 — the header credential tag was removed). AnthropicSecretID
	// is set so the label force-shows in the rail; with no rateLimits seeded this drives the
	// synthesis path (a label-only entry from AnthropicSecretLabel).
	detail = applyDetail(detail, apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: nasty,
		AnthropicSecretID: &secretID, AnthropicSecretLabel: &detailCredLabel,
		Milestones:           []apitypes.Milestone{{ID: "m1", Title: nasty}},
		MilestonesInProgress: []string{"m1"}},
		[]apitypes.MessageDTO{
			msgDTO(1, "text", nasty, "toolu_"+nasty, nasty, nasty, now),
			// A hostile tool_use frame drives the crew rail's now line (renderMilestones →
			// railNowLines, PRD #1064 D4): the role (Agent) and the italic task label
			// (AgentLabel / the Bash description Detail) are model-authored, unsanitized on the
			// wire, and must be drawn through renderer.Plain.
			{Seq: 2, Kind: "tool_use", Agent: ptr(nasty), AgentLabel: ptr(nasty), CreatedAt: now,
				Payload: json.RawMessage(`{"name":"Bash","input":{"description":` + quoteJSON(nasty) + `}}`)},
		})
	detailOut := detail.View().Content
	assertNoRawControls(t, "detail", detailOut)
	// The hostile AnthropicSecretLabel sanitizes to "credsafe", which can ONLY reach detailOut via
	// the rail's render of the account label — the title produces "safe", not a superstring of
	// "credsafe". Its presence proves the rail ACCOUNTS render path ran (PRD #623) and, paired with
	// assertNoRawControls above, that it sanitized the hostile label. The marker must differ from
	// the title's "safe" or this assertion is vacuous (the title alone would satisfy it).
	if !strings.Contains(detailOut, "credsafe") {
		t.Fatalf("the detail rail is not drawing AnthropicSecretLabel, so this test is not exercising that render path\n%s", detailOut)
	}

	// The ADMIN board is the only view that draws OwnerEmail (PRD #325 M2, B1). A hostile
	// OwnerEmail must not emit control bytes into the frame. The clean-fixture screenshots
	// cannot catch this, so this is the guard. OwnerEmail is *string, so bind a local.
	owner := nasty
	adminRuns := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: nasty}, OwnerEmail: &owner},
	}
	adm := tuiTestModel(t, &uzicli.FakeClient{}, "")
	adm = press(t, adm, keyAdmin)
	next, _ = adm.Update(boardRunsMsg{reqID: adm.board.waitID, admin: true, runs: adminRuns})
	adm = next.(tuiModel)
	admOut := adm.View().Content
	// The hostile OwnerEmail is nasty + "safe"; after sanitizing, "safe" survives. Its presence
	// proves the OwnerEmail render path ran (the redesign dropped the column HEADER, so there is
	// no "OWNER" label to look for — the email cell itself is the signal).
	if !strings.Contains(admOut, "safe") {
		t.Fatalf("the admin board is not drawing OwnerEmail, so this test is not exercising that render path\n%s", admOut)
	}
	assertNoRawControls(t, "admin board", admOut)

	// The board footer's always-on version readout (issue #687) draws the SERVER's build
	// version, which is attacker-controlled via GET /api/version. cellText must strip its
	// control bytes (and the bidi override) before the readout embeds it. The controls are
	// interspersed AMONG the digits so the sanitized result is a valid "0.63.0" — a stamped CLI
	// behind that server renders the compared server number, proving the sanitized digits
	// survive while the raw ESC/BEL/CR/bidi bytes never reach the frame. A stamped CLI is
	// required: a dev CLI would show its own version alone and never exercise the server
	// sanitizer. (Controls placed BEFORE the digits, e.g. an ESC "[2J" prefix, would leave the
	// printable "[2J" after ESC-stripping, fail semver, and drop the readout to client-only —
	// which does not exercise the render-time sanitizer, so they are interspersed here.)
	withVersion(t, "v0.14.0")
	board.showVersion = true
	const hostileVer = "0.6\x1b3\u202e.0\x07\r"
	next, _ = board.Update(buildInfoMsg{version: hostileVer})
	board = next.(tuiModel)
	footerOut := board.View().Content
	assertNoRawControls(t, "footer", footerOut)
	// Check the SPECIFIC hostile ESC ("\x1b3", the ESC+'3' this fixture embeds) rather than a
	// bare ESC, which would also match the frame's legitimate ESC[…m SGR colour sequences.
	for _, ctrl := range []string{"\r", "\x1b3", "\x07"} {
		if strings.Contains(footerOut, ctrl) {
			t.Errorf("board footer contains a raw control byte %q from the hostile server version", ctrl)
		}
	}
	if !strings.Contains(stripANSI(footerOut), "0.63.0") {
		t.Fatalf("the sanitized server version should render in the footer readout\n%s", stripANSI(footerOut))
	}
}

// The board credential cell (boardCredSeg) shows just the token LABEL, muted and WITHOUT a dot,
// whatever the select reason (PRD #295 — the reason/mode lives on the detail and `uzi run <id>`,
// not the board). A run with no recorded credential draws a blank cell.
func TestTUIBoardCredentialCell(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	meta, personal := "meta", "personal"

	// The label shows, without a ● dot, for every reason — neutral and fallback alike.
	for _, reason := range []string{"auto", "default", "pool_stale", "best_of_pool", ""} {
		reason := reason
		r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{AnthropicSecretLabel: &personal}}
		if reason != "" {
			r.AnthropicSelectReason = &reason
		}
		if s := stripANSI(m.boardCredSeg(r, nil)); !strings.Contains(s, "personal") || strings.Contains(s, "●") {
			t.Errorf("credential cell (reason %q): want the label without a dot, got %q", reason, s)
		}
	}

	// It is the label, not a constant.
	if s := stripANSI(m.boardCredSeg(apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{AnthropicSecretLabel: &meta}}, nil)); !strings.Contains(s, "meta") {
		t.Errorf("credential cell: want the meta label, got %q", s)
	}

	// No recorded credential (pre-#111 or unclaimed): a blank cell, never a guessed placeholder.
	if s := strings.TrimSpace(stripANSI(m.boardCredSeg(apitypes.RunListItemDTO{}, nil))); s != "" {
		t.Errorf("no-credential cell must be blank, got %q", s)
	}
}

// → (right) on the board opens the selected run (mirroring enter); ← (left) from the run detail's
// leftmost pane backs out to the board (mirroring esc) — the board↔detail drill in / out pair.
func TestTUIBoardArrowNavigation(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111", Kind: "issue", Status: "running", IssueTitle: "a run"}},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: runs}, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)

	// → opens the selected run.
	m = press(t, m, keyRight)
	if m.view != viewDetail || m.detail.runID != "aaaaaaaa-1111" {
		t.Fatalf("→ on the board should open the selected run; view=%v runID=%q", m.view, m.detail.runID)
	}

	// ← from the detail (focus opens on the rail, the leftmost pane) returns to the board.
	m = press(t, m, keyLeft)
	if m.view != viewBoard {
		t.Fatalf("← from the run detail's leftmost pane should return to the board; view=%v", m.view)
	}
}

// ← moves pane focus first (transcript → rail) and backs out only on the SECOND press, at the left
// boundary — it must NOT exit while the transcript pane is focused.
func TestTUIDetailLeftExitsAtBoundaryNotBefore(t *testing.T) {
	runID := "bbbbbbbb-1111"
	now := time.Now()
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "multi-lane"},
		[]apitypes.MessageDTO{
			msgDTO(1, "text", "lead", "toolu_a", "impl", "hi", now),
			msgDTO(2, "text", "coder", "toolu_b", "impl", "yo", now),
		})
	m.detail.focus = focusTranscript

	// First ← focuses the rail, still inside the detail view.
	m = press(t, m, keyLeft)
	if m.view != viewDetail || m.detail.focus != focusRail {
		t.Fatalf("first ← should focus the rail, not exit; view=%v focus=%d", m.view, m.detail.focus)
	}
	// Second ← at the rail boundary returns to the board.
	m = press(t, m, keyLeft)
	if m.view != viewBoard {
		t.Fatalf("second ← at the rail boundary should return to the board; view=%v", m.view)
	}
}

// The credential column is GATED exactly as the web RunsList (PRD #295): the own board shows it
// only when the viewer holds more than one Anthropic token; the admin factory board always does.
func TestTUIBoardCredentialGate(t *testing.T) {
	meta := "meta"
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111", Kind: "issue", Status: "running", IssueTitle: "cred run", AnthropicSecretLabel: &meta}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	drive := func(m tuiModel, msg tea.Msg) tuiModel {
		t.Helper()
		next, _ := m.Update(msg)
		return next.(tuiModel)
	}

	// Own board, ONE token → no credential column.
	one := drive(drive(tuiTestModel(t, fake, ""), boardRunsMsg{reqID: 1, runs: runs}), secretsMsg{count: 1})
	if out := stripANSI(one.View().Content); strings.Contains(out, "meta") {
		t.Errorf("own board with one token must not show the credential\n%s", out)
	}

	// Own board, TWO tokens → the credential shows.
	two := drive(drive(tuiTestModel(t, fake, ""), boardRunsMsg{reqID: 1, runs: runs}), secretsMsg{count: 2})
	if out := stripANSI(two.View().Content); !strings.Contains(out, "meta") {
		t.Errorf("own board with two tokens must show the credential\n%s", out)
	}

	// Admin factory board → always shows, without any token probe.
	adm := press(t, tuiTestModel(t, &uzicli.FakeClient{}, ""), keyAdmin)
	adm = drive(adm, boardRunsMsg{reqID: adm.board.waitID, admin: true, runs: runs})
	if out := stripANSI(adm.View().Content); !strings.Contains(out, "meta") {
		t.Errorf("admin factory board must always show the credential\n%s", out)
	}
}

// The detail header is ALWAYS exactly one physical row (issue #666): the title takes the middle width
// and ellipsizes with "…" when it does not fit, while the breadcrumb (left) and the status + transport
// block (right) are never the field cut. transcriptViewport counts len(detailHeaderLines), so the
// footer accounting follows (#379).
func TestTUIDetailHeaderIsAlwaysOneRow(t *testing.T) {
	runID := "cccccccc-1111"
	const longTitle = "A very long issue title that will not fit beside the breadcrumb and the status token on a narrow terminal"
	load := func(w int, title string) tuiModel {
		m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		m.width = w
		run := apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: title}
		return applyDetail(m, run,
			[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "hi", time.Now())})
	}

	// Wide + short title: one row carrying the full title AND the status word.
	wide := load(160, "Short title").detailHeaderLines()
	if len(wide) != 1 {
		t.Fatalf("wide terminal: want 1 header row, got %d: %q", len(wide), wide)
	}
	if s := stripANSI(wide[0]); !strings.Contains(s, "Short title") || !strings.Contains(s, "running") {
		t.Errorf("the wide row must carry both the full title and the status: %q", s)
	}

	// Wide + long title: still one row; the title is ellipsized but the status word survives in full.
	wl := load(120, longTitle).detailHeaderLines()
	if len(wl) != 1 {
		t.Fatalf("wide+long: want 1 header row, got %d: %q", len(wl), wl)
	}
	if s := stripANSI(wl[0]); !strings.Contains(s, "…") {
		t.Errorf("a long title must be ellipsized with …: %q", s)
	} else if !strings.Contains(s, "A very long issue title") {
		t.Errorf("the ellipsized title must still show its leading prefix: %q", s)
	} else if !strings.Contains(s, "running") {
		t.Errorf("the status word must never be the field cut: %q", s)
	} else if strings.Contains(s, "…●") {
		t.Errorf("a truncated title's … must not abut the status glyph — keep a separating gap: %q", s)
	}

	// Narrow: still exactly one row, with the status word still present in full.
	narrow := load(90, longTitle).detailHeaderLines()
	if len(narrow) != 1 {
		t.Fatalf("narrow terminal: want 1 header row, got %d: %q", len(narrow), narrow)
	}
	if s := stripANSI(narrow[0]); !strings.Contains(s, "running") {
		t.Errorf("the narrow row must still carry the status word in full: %q", s)
	}
}

// D7 regression guard for the transcript's TOOL-FRAME path. buildTranscriptLines → toolFrameName
// pulls a tool_use frame's `name` from its (SDK/agent-authored, untrusted) payload and draws
// `⚙ <name>`. It is routed through renderer.Plain today, but the hostile-render test above drives
// only a Kind:"text" frame, so this branch is never exercised — a regression to a raw draw of the
// tool name would pass every clean-fixture screenshot AND that test. This drives a tool_use frame
// with control bytes in `name` and asserts the rendered transcript carries none.
func TestTUITranscriptStripsControlBytesFromToolName(t *testing.T) {
	now := time.Now()
	// Hostile bytes at the FRONT, so Plain's 24-rune truncation cannot produce a false green by
	// cutting them off the tail.
	const nasty = "\x1b[2J\u202E\x07\x01safe"
	runID := "77777777-2222"

	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"},
		[]apitypes.MessageDTO{
			// A benign text frame opens the lane; the tool_use frame (same instance) is what the
			// toolFrameName path compresses to `⚙ <name>`.
			msgDTO(1, "text", "coder", "toolu_aaa111", "impl", "hello", now),
			toolUseMsg(2, "coder", "toolu_aaa111", nasty, now),
		})

	out := m.View().Content
	// The sanitized tool name's "safe" tail proves the `⚙ <name>` path ran — otherwise this test
	// would assert cleanliness over a render that never drew the field.
	if !strings.Contains(out, "safe") {
		t.Fatalf("the transcript is not drawing the tool-frame name, so this test is not exercising toolFrameName\n%s", out)
	}
	assertNoRawControls(t, "transcript tool frame", out)
}

// The crew rail draws a milestone-structured run's progress (renderMilestones): the
// `{done}/{total}` summary, a per-milestone glyph (✓ done, ◐ in progress, ○ not started),
// and the titles. A run with no frozen milestone list must draw NO milestone block, and a
// run that has reported nothing shows `–/N` rather than `0/N`. Driven through the real
// Update/View seam so it gates the wiring, not just the helper.
func TestTUIDetailMilestoneBlock(t *testing.T) {
	now := time.Now()
	milestoneRun := apitypes.RunDTO{ID: "77777777-1111", Status: "running",
		IssueTitle: "structured run",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"},
			{ID: "m3", Title: "Gamma"}, {ID: "m4", Title: "Delta"},
		},
		MilestonesCompleted:  []string{"m1", "m2"},
		MilestonesInProgress: []string{"m3"},
	}
	load := func(run apitypes.RunDTO) string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		return applyDetail(m, run,
			[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "planning", now)}).View().Content
	}

	out := load(milestoneRun)
	// PRD #1064 D4: the in-progress row's mark is the blinking cell (▰/▱ in the wait colour),
	// NOT the old ◐, and the eyebrow gains a `· <id>` suffix naming the in-progress milestone.
	for _, want := range []string{"MILESTONES", "2/4", "✓", "○", "Alpha", "Gamma", "· m3"} {
		if !strings.Contains(out, want) {
			t.Errorf("milestone block missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "◐") {
		t.Errorf("the in-progress row must use the blinking cell, not the retired ◐ glyph\n%s", out)
	}
	// blinkOn defaults false, so the static frame shows the in-progress cell as ▱ in the wait
	// colour — the same span the eyebrow micro-bar's in-progress cell uses.
	waitCell := paintSeg(newPalette(true).wait, nil, false, "▱")
	if !strings.Contains(out, waitCell) {
		t.Errorf("the in-progress cell (▱ in the wait colour) is not rendered in the static frame\n%s", out)
	}

	// A run with no frozen list draws no block at all (back-compat: pre-#122 runs unchanged).
	plain := load(apitypes.RunDTO{ID: "66666666-1111", Status: "running", IssueTitle: "no milestones"})
	if strings.Contains(plain, "MILESTONES") {
		t.Errorf("a non-milestone run drew a MILESTONES block\n%s", plain)
	}

	// Nothing reported → "–/N", never "0/N" (which reads as failure).
	fresh := milestoneRun
	fresh.MilestonesCompleted, fresh.MilestonesInProgress = nil, nil
	if got := load(fresh); !strings.Contains(got, "–/4") {
		t.Errorf("a run that reported nothing should show –/4, not 0/4\n%s", got)
	}

	// A milestone run with NO activity yet (queued / just-claimed → no lanes) must STILL show
	// the block: the crew rail's no-lanes path appends it too. Regression guard — a duplicated
	// early return once made that branch dead, so a milestone run showed no block before its
	// first frame. `load` seeds a frame, so this case loads with none.
	noAct := tuiTestModel(t, &uzicli.FakeClient{}, milestoneRun.ID)
	na := applyDetail(noAct, milestoneRun, nil).View().Content
	if !strings.Contains(na, "MILESTONES") || !strings.Contains(na, "no activity yet") {
		t.Errorf("a milestone run with no activity yet should show the block AND '(no activity yet)'\n%s", na)
	}
}

// The run viewer fills the full terminal height (issue #379): the two-pane body and the pane
// divider beside it reach the footer even when the transcript is shorter than the viewport, so
// a tall terminal shows no dead space below the content. Rendered rows == the terminal height,
// and the divider extends down most of the frame rather than stopping at the last content line.
func TestTUIDetailFillsHeight(t *testing.T) {
	now := time.Now()
	runID := "55555555-1111"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m.width, m.height = 100, 40
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "short"},
		[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "one short line", now)})
	rows := strings.Split(m.View().Content, "\n")
	if len(rows) != 40 {
		t.Fatalf("detail view rendered %d rows, want exactly the terminal height 40\n%s", len(rows), strings.Join(rows, "\n"))
	}
	div := 0
	for _, r := range rows {
		if strings.Contains(r, "▏") {
			div++
		}
	}
	if div < 25 {
		t.Errorf("pane divider on only %d rows; it should extend down the full body, not stop at the content\n%s", div, strings.Join(rows, "\n"))
	}
}

// A tall crew rail (many lanes + the milestone block) must NOT push the footer off-screen
// (issue #379 tui-ux finding 1): the two-pane body is clamped to the viewport, so the rail
// truncates rather than overflowing past the footer, which carries the pane/esc/? controls.
func TestTUIDetailFooterSurvivesTallRail(t *testing.T) {
	now := time.Now()
	runID := "44444444-1111"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m.width, m.height = 100, 20
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "many lanes",
		Milestones: []apitypes.Milestone{{ID: "m1", Title: "a"}, {ID: "m2", Title: "b"},
			{ID: "m3", Title: "c"}, {ID: "m4", Title: "d"}},
		MilestonesCompleted: []string{"m1"}, MilestonesInProgress: []string{"m2"}},
		[]apitypes.MessageDTO{
			msgDTO(1, "text", "lead", "", "", "planning", now),
			msgDTO(2, "text", "coder", "toolu_a", "impl", "a", now),
			msgDTO(3, "text", "tester", "toolu_b", "sweep", "b", now),
			msgDTO(4, "text", "reviewer", "toolu_c", "review", "c", now),
			msgDTO(5, "text", "auditor", "toolu_d", "audit", "d", now),
		})
	out := m.View().Content
	if rows := strings.Split(out, "\n"); len(rows) > 20 {
		t.Fatalf("detail rendered %d rows at height 20; a tall rail must clamp, not overflow\n%s", len(rows), out)
	}
	// The footer labels are per-token SGR spans, so "esc back" is not one substring; " back"
	// and " keys" each sit inside a single faint span and are footer-only here.
	if !strings.Contains(out, "back") || !strings.Contains(out, "keys") {
		t.Errorf("footer (esc back / ? keys) missing — a tall crew rail pushed it off-screen\n%s", out)
	}
}

// At a narrow board width the milestone micro-bar column is dropped and no row overflows the
// terminal edge, so the trailing judge marker keeps its full text instead of being clipped
// (issue #379 tui-ux finding 2: the micro-bar tax pushed marker rows past the edge at 80 cols).
func TestTUIBoardRowsFitNarrowWidth(t *testing.T) {
	issues := "issues"
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "completed",
			IssueTitle: "Migrate per-user secrets into the vault hierarchy"}, JudgeVerdict: &issues, JudgeTodoCount: 3},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "running", IssueTitle: "structured",
			Milestones:          []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}, {ID: "m4"}},
			MilestonesCompleted: []string{"m1", "m2"}}},
	}}
	m := tuiTestModel(t, fake, "")
	m.width, m.height = 80, 34
	out := func() string {
		next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
		return next.(tuiModel).View().Content
	}()
	for _, r := range strings.Split(out, "\n") {
		if w := visualWidth(r); w > 80 {
			t.Errorf("board row %d cols wide at width 80 (overflows the edge): %q", w, r)
		}
	}
	if !strings.Contains(out, "issues · 3") {
		t.Errorf("judge marker 'issues · 3' was clipped at width 80\n%s", out)
	}
	if strings.Contains(out, "▰") {
		t.Errorf("milestone micro-bar should be hidden below boardMileMinWidth (width 80)\n%s", out)
	}
	// At a wide width the micro-bar returns.
	m.width = 120
	if wide := func() string {
		next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
		return stripANSI(next.(tuiModel).View().Content)
	}(); !strings.Contains(wide, "▰▰▱▱") {
		t.Errorf("milestone micro-bar should show at width 120\n%s", wide)
	}
}

// The factory floor shows a milestone micro-bar on a milestone-structured run (milestoneMarker,
// the web MilestoneBadge twin) — ▰ per done, ▱ per remaining — and NOTHING on a run with no
// frozen list. A run that reported nothing has done=0, so it draws an all-empty ▱ bar (graphical 0/N).
func TestTUIBoardMilestoneBadge(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "structured",
			Milestones:          []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}, {ID: "m4"}},
			MilestonesCompleted: []string{"m1", "m2"}}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "running", IssueTitle: "plain"}},
		{RunDTO: apitypes.RunDTO{ID: "cccccccc-3", Kind: "issue", Status: "running", IssueTitle: "unreported",
			Milestones: []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}}}}, // nil completed ⇒ never reported
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	out := stripANSI(next.(tuiModel).View().Content) // the ▰/▱ split across colour spans

	// 2 of 4 done → ▰▰▱▱ on the "structured" row.
	if !strings.Contains(out, "▰▰▱▱") {
		t.Errorf("board missing 2-of-4 milestone micro-bar ▰▰▱▱\n%s", out)
	}
	// Nothing reported → the "unreported" row draws an all-empty ▱▱ bar (graphical 0/2), never
	// –/2 text. Anchor on that run's own line so the reported run's trailing ▱▱ can't stand in.
	var unrep string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "unreported") {
			unrep = line
			break
		}
	}
	if unrep == "" {
		t.Fatalf("no board row for the unreported run\n%s", out)
	}
	if !strings.Contains(unrep, "▱▱") || strings.Contains(unrep, "▰") {
		t.Errorf("unreported run should draw an all-empty ▱▱ bar\n%q", unrep)
	}
	if strings.Contains(unrep, "–/") {
		t.Errorf("unreported run should draw a graphical 0/2 bar, not –/N text\n%q", unrep)
	}
	// A run with no frozen list draws no micro-bar cell at all: the only micro-bar is
	// aaaaaaaa-1's, so exactly one ▰▰▱▱ pattern exists (the plain run carries none).
	if strings.Count(out, "▰▰▱▱") != 1 {
		t.Errorf("expected exactly one ▰▰▱▱ micro-bar (the plain run must carry none)\n%s", out)
	}
}

// The micro-bar caps at boardMileCap (9) cells: a 9-milestone run draws the full bar, while a
// 10-milestone run falls back to N/M text (the bar would overflow the boardMileWidth column).
// A text cell has no bar to read as 0, so an UNREPORTED over-cap run keeps the neutral –/N there,
// not 0/N (the cross-surface convention the empty bar only overrides where a bar can be drawn).
func TestTUIBoardMilestoneBadgeCap(t *testing.T) {
	mile := func(n int) []apitypes.Milestone {
		ms := make([]apitypes.Milestone, n)
		for i := range ms {
			ms[i] = apitypes.Milestone{ID: "m" + itoa(i+1)}
		}
		return ms
	}
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "nine",
			Milestones: mile(9), MilestonesCompleted: []string{"m1"}}}, // 1/9 → full 9-cell bar
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "running", IssueTitle: "ten",
			Milestones: mile(10), MilestonesCompleted: []string{"m1", "m2", "m3"}}}, // 3/10 → text
		{RunDTO: apitypes.RunDTO{ID: "cccccccc-3", Kind: "issue", Status: "running", IssueTitle: "unreported-ten",
			Milestones: mile(10)}}, // nil completed, over cap → –/10 text, never 0/10
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	out := stripANSI(next.(tuiModel).View().Content)

	// 9 milestones sit at the cap → the full bar renders (1 done, 8 remaining), never "1/9" text.
	if !strings.Contains(out, "▰▱▱▱▱▱▱▱▱") {
		t.Errorf("a 9-milestone run should draw a 9-cell bar, not text\n%s", out)
	}
	if strings.Contains(out, "1/9") {
		t.Errorf("a 9-milestone run must not fall back to N/M text\n%s", out)
	}
	// 10 milestones exceed the cap → N/M text, never a bar.
	if !strings.Contains(out, "3/10") {
		t.Errorf("a 10-milestone run should fall back to 3/10 text\n%s", out)
	}
	// Over cap AND nothing reported → neutral –/10 text, never 0/10 (which reads as failure).
	var unrep string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "unreported-ten") {
			unrep = line
			break
		}
	}
	if unrep == "" {
		t.Fatalf("no board row for the unreported over-cap run\n%s", out)
	}
	if !strings.Contains(unrep, "–/10") || strings.Contains(unrep, "0/10") {
		t.Errorf("an unreported over-cap run should show –/10 text, not 0/10\n%q", unrep)
	}
}

// The board COST cell (boardCostSeg, PRD #650) renders whole dollars with three distinct states:
// a real cost as "$N" (no decimal), a $0-with-tokens subscription run as "—" (never "$0"), a
// sub-dollar real cost as "<$1", and a nil-Usage run as a blank cell (the boardCredSeg convention).
func TestTUIBoardCostCell(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")

	// A real cost renders whole dollars, no decimal.
	real := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 9.4, InputTokens: 100}}}
	if s := stripANSI(m.boardCostSeg(real, nil)); !strings.Contains(s, "$9") || strings.Contains(s, ".") {
		t.Errorf("cost cell: want whole-dollar $9 with no decimal, got %q", s)
	}

	// $0 with tokens is a subscription-auth run the SDK prices at $0 → "—", never "$0".
	sub := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 0, InputTokens: 100}}}
	if s := stripANSI(m.boardCostSeg(sub, nil)); !strings.Contains(s, "—") || strings.Contains(s, "$0") {
		t.Errorf("cost cell: a $0-with-tokens run must render — not $0, got %q", s)
	}

	// A sub-dollar real cost renders "<$1" (so a real cost never shows as $0, keeping — unambiguous).
	subdollar := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 0.3, InputTokens: 100}}}
	if s := stripANSI(m.boardCostSeg(subdollar, nil)); !strings.Contains(s, "<$1") {
		t.Errorf("cost cell: a sub-dollar cost must render <$1, got %q", s)
	}

	// nil Usage (a pre-#40 or unclaimed run) draws a blank cell.
	if s := strings.TrimSpace(stripANSI(m.boardCostSeg(apitypes.RunListItemDTO{}, nil))); s != "" {
		t.Errorf("nil-Usage cost cell must be blank, got %q", s)
	}

	// A pathological high cost must still fit the fixed COST cell width: $100000 would
	// render "$100000" (7 > boardCostWidth), which without the board-cell cap would blow
	// the column; fmtCostBoard abbreviates it so the cell stays exactly boardCostWidth.
	huge := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 100000, InputTokens: 100}}}
	hs := stripANSI(m.boardCostSeg(huge, nil))
	if w := visualWidth(hs); w != boardCostWidth {
		t.Errorf("cost cell for a huge cost must be exactly boardCostWidth=%d, got width %d (%q)", boardCostWidth, w, hs)
	}
	if !strings.Contains(hs, "k") {
		t.Errorf("cost cell for $100000 must abbreviate (expect a k suffix), got %q", hs)
	}

	// Even an absurd value above the $9999G abbreviation ceiling stays within the cell:
	// the overflow marker is fixed-width, so the invariant holds for ALL inputs.
	absurd := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 1e13, InputTokens: 100}}}
	as := stripANSI(m.boardCostSeg(absurd, nil))
	if w := visualWidth(as); w != boardCostWidth {
		t.Errorf("cost cell for an absurd cost must be exactly boardCostWidth=%d, got width %d (%q)", boardCostWidth, w, as)
	}
	// Assert the exact marker, not just the width — a width-only check passes for any
	// six-cell string, so a regression to a wrong marker would go undetected.
	if as != ">$999G" {
		t.Errorf("cost cell for an absurd cost must use the overflow marker %q, got %q", ">$999G", as)
	}
}

// The own-board summary carries a floor total (PRD #650): the rounded RAW CostUSD sum over
// usage-bearing runs, dropped when 0 or when no run carries Usage.
func TestTUIBoardCostSummaryTotal(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "one",
			Usage: &apitypes.UsageDTO{CostUSD: 1.6, InputTokens: 100}}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "running", IssueTitle: "two",
			Usage: &apitypes.UsageDTO{CostUSD: 1.6, InputTokens: 100}}},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: runs}, "")
	m.width = 120
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	// round(1.6 + 1.6) = round(3.2) = 3 → "$3"; summing the rounded per-row cells would give
	// 2 + 2 = 4. The presence of "$3" proves the total is computed from the raw sum.
	content := m.View().Content
	if out := stripANSI(content); !strings.Contains(out, "$3") {
		t.Errorf("summary floor total should show the rounded raw sum $3\n%s", out)
	}
	// The floor total wears the tungsten accent (tui-ux P1), not the faint chrome around it, so
	// the money figure is findable in the cluster and matches the detail SPEND total's weight. A
	// colour a screenshot cannot gate, so pin the SGR here.
	if wantTung := lipgloss.NewStyle().Foreground(m.pal.tungsten).Render("$3"); !strings.Contains(content, wantTung) {
		t.Errorf("floor total should carry the tungsten accent SGR, got:\n%q", content)
	}
	if faint := m.pal.faint.Render("$3"); strings.Contains(content, faint) {
		t.Errorf("floor total must not use the faint chrome tone (it should be tungsten)")
	}

	// A board whose runs all carry nil Usage shows no "$" total segment at all.
	nilRuns := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "cccccccc-3", Kind: "issue", Status: "running", IssueTitle: "no usage"}},
	}
	m2 := tuiTestModel(t, &uzicli.FakeClient{Runs: nilRuns}, "")
	next, _ = m2.Update(boardRunsMsg{reqID: m2.board.waitID, runs: nilRuns})
	m2 = next.(tuiModel)
	if out := stripANSI(m2.View().Content); strings.Contains(out, "$") {
		t.Errorf("a board with no usage-bearing runs must show no cost total\n%s", out)
	}
}

// The COST column obeys the #379 drop order (PRD #650 M2) as a PROPERTY, not a pixel pin: the
// milestone micro-bar drops FIRST on a narrowing terminal, then COST, and the title is never
// squeezed past the edge. Asserted over a width sweep plus a fit check in the cost-only band and
// below both columns.
func TestTUIBoardCostColumnDropOrder(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running",
			IssueTitle:          "a milestone-and-cost run on the floor",
			Milestones:          []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}, {ID: "m4"}},
			MilestonesCompleted: []string{"m1", "m2"},
			Usage:               &apitypes.UsageDTO{CostUSD: 9.4, InputTokens: 100}}},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: runs}, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	// One token → no credential column, isolating the mile↔cost interdependency.
	next, _ = m.Update(secretsMsg{count: 1})
	m = next.(tuiModel)

	sawCostOnly, sawNeither := false, false
	for w := 60; w <= 130; w++ {
		m.width = w
		// The mile bar is never shown without COST (COST has the higher retention priority).
		if m.boardShowMile() && !m.boardShowCost() {
			t.Fatalf("width %d: milestone bar shown without COST (violates the #379 drop order)", w)
		}
		if m.boardShowCost() && !m.boardShowMile() {
			sawCostOnly = true // COST outlives the mile bar
		}
		if !m.boardShowCost() && !m.boardShowMile() {
			sawNeither = true // narrow enough that both drop
		}
	}
	if !sawCostOnly {
		t.Errorf("expected a width band where COST outlives the milestone bar")
	}
	if !sawNeither {
		t.Errorf("expected a narrow width where both COST and the milestone bar drop")
	}

	// The title is never squeezed past the edge: no rendered row exceeds the width, in the
	// cost-only band (90) and below both columns (70).
	for _, w := range []int{90, 70} {
		m.width = w
		for _, line := range strings.Split(m.View().Content, "\n") {
			if vw := visualWidth(line); vw > w {
				t.Errorf("width %d: board line %d cols wide overflows the edge: %q", w, vw, line)
			}
		}
	}
}

// The admin factory board never shows the COST column or the floor total (PRD #650): AdminListRuns
// attaches no Usage, the same way it carries no judge marker — and even a run that DID carry Usage
// draws no cost cell there, because the gate keys off the admin flag, not the data.
func TestTUIBoardCostAdminHidden(t *testing.T) {
	owner := "dev@example.com"
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "admin run",
			Usage: &apitypes.UsageDTO{CostUSD: 12.0, InputTokens: 100}}, OwnerEmail: &owner},
	}
	adm := press(t, tuiTestModel(t, &uzicli.FakeClient{}, ""), keyAdmin)
	next, _ := adm.Update(boardRunsMsg{reqID: adm.board.waitID, admin: true, runs: runs})
	adm = next.(tuiModel)
	adm.width = 120

	if adm.boardShowCost() {
		t.Errorf("the admin board must never show the COST column, even at width 120")
	}
	// Defensive: even the usage-bearing admin row draws no cost cell, and no floor total appears.
	if out := stripANSI(adm.View().Content); strings.Contains(out, "$") {
		t.Errorf("the admin board must show no cost cell or total, even for a usage-bearing run\n%s", out)
	}
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
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)
	out := m.View().Content

	// The summary cluster: ⚑ N (awaiting_approval) and ▲ N (warn health — stalled AND looping
	// both fold into the ▲ axis via the health override), plus "N runs". A looping run's WORD
	// also surfaces in its status slot (the health override renders ▲ + the word). The judge
	// marker carries the todo count ("issues · 2") when JudgeTodoCount > 0.
	for _, want := range []string{"5 runs", "⚑ 1", "▲ 2", "looping", "issues · 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("board missing %q\n%s", want, out)
		}
	}
	// The one filled surface on the board — the warm selection bar on the cursor row — is a
	// truecolor background SGR (48;2;…). Everything else is foreground ink.
	if !strings.Contains(out, "\x1b[48;2;") {
		t.Errorf("board has no background-fill SGR; the warm selection bar is missing\n%s", out)
	}
	// The NO_COLOR-safe state glyph for a running run survives independent of colour.
	if g, _ := stateGlyphWord("running", "", false, false); !strings.Contains(out, g) {
		t.Errorf("running state glyph %q absent from the board\n%s", g, out)
	}
}

// TestTUIBoardSummaryCountsFollowupPark pins that an awaiting_followup run (PRD #517) is
// counted in the board summary's needs-you cluster — it is in the NEEDS YOU band, so it
// must not be invisible in the summary line. Mutation that reddens this: dropping the
// awaiting_followup case from boardSummary → no "➤ 1" segment appears.
func TestTUIBoardSummaryCountsFollowupPark(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "one"}},
		{RunDTO: apitypes.RunDTO{ID: "ffffffff-2", Kind: "issue", Status: "awaiting_followup", IssueTitle: "parked"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)
	if got := m.boardSummary(); !strings.Contains(got, "➤ 1") {
		t.Errorf("board summary omits the follow-up park (want a ➤ 1 segment): %q", got)
	}
}

// TestTUIBoardSummaryExcludesRevisingApproval pins issue #750 in the summary cluster: a run
// mid-"revise" replan keeps status == awaiting_approval but is NOT the user's turn, so it must
// not inflate the ⚑ counter. With one genuine plan-gate run and one revising run the cluster
// must read "⚑ 1" (not "⚑ 2"), matching the NEEDS YOU band — where the revising run does NOT
// belong. Mutation that reddens this: dropping the !r.IsRevising gate in boardSummary.
func TestTUIBoardSummaryExcludesRevisingApproval(t *testing.T) {
	genuine := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "awaiting_approval", IssueTitle: "plan gate"}}
	revising := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "awaiting_approval", IssueTitle: "re-planning"}, IsRevising: true}
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{genuine, revising}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)
	got := m.boardSummary()
	if !strings.Contains(got, "⚑ 1") || strings.Contains(got, "⚑ 2") {
		t.Errorf("board summary must count only the genuine plan gate (want ⚑ 1, not ⚑ 2): %q", got)
	}
	// The revising run drops to ON THE FLOOR; only the genuine plan gate sits in NEEDS YOU.
	if b := runBand(revising.Status, revising.IsRevising); b == bandNeedsYou {
		t.Errorf("revising awaiting_approval run must not be in NEEDS YOU, got band %d", b)
	}
	if b := runBand(genuine.Status, genuine.IsRevising); b != bandNeedsYou {
		t.Errorf("genuine awaiting_approval run must be in NEEDS YOU, got band %d", b)
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
// the frames contain ONLY SGR and OSC-8 hyperlink delimiters. OSC-8 hyperlink
// delimiters ARE now allowed (named reason: the clickable #<iid> issue link, m2), via
// osc8SequenceEnd — the skip covers ONLY the two well-formed delimiters (ESC ] 8 ; ;
// … ESC \), not arbitrary OSC, and the styled text between them is still scanned. Any
// OTHER escape than SGR or a well-formed OSC-8 delimiter is still a finding. Widen the
// allowlist further only with a named reason — never restore the blanket skip, which is
// what made it blind.
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
			if end, ok := osc8SequenceEnd(rs, i); ok {
				i = end // a legitimate OSC-8 hyperlink delimiter; skip past it
				continue
			}
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

// osc8SequenceEnd reports the index of the final rune of a well-formed OSC-8
// hyperlink DELIMITER starting at i (ESC ] 8 ; ; <params/url> ESC \), and whether
// one is there. Each of the open and close delimiters matches this shape; the
// enclosed styled text between them is scanned normally (its SGR is handled by
// sgrSequenceEnd). It REJECTS (returns 0, false) a delimiter whose URL param carries
// a raw control byte before the ST — so a control byte smuggled into the OSC-8 target
// is flagged by assertNoRawControls independently, not skipped over on the assumption
// that emission was sanitized.
func osc8SequenceEnd(rs []rune, i int) (int, bool) {
	// ESC ] 8 ; ;
	if i+4 >= len(rs) || rs[i+1] != ']' || rs[i+2] != '8' || rs[i+3] != ';' || rs[i+4] != ';' {
		return 0, false
	}
	for j := i + 5; j < len(rs); j++ {
		c := rs[j]
		if c == 0x1b { // ST is ESC \
			if j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 1, true
			}
			return 0, false
		}
		if unicode.IsControl(c) {
			// A raw control byte inside the URL param — not a well-formed delimiter.
			// Fall through so assertNoRawControls flags the escape instead of skipping it.
			return 0, false
		}
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

// The [h] toggle hides terminal runs (completed/failed/cancelled) and keeps the active +
// needs-you set, without a refetch. The header shows the mode and the footer key flips
// label; pressing h again restores the full board.
func TestTUIBoardHideDoneToggle(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "live one"}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "awaiting_approval", IssueTitle: "needs you"}},
		{RunDTO: apitypes.RunDTO{ID: "cccccccc-3", Kind: "issue", Status: "completed", IssueTitle: "done one"}},
		{RunDTO: apitypes.RunDTO{ID: "dddddddd-4", Kind: "issue", Status: "failed", IssueTitle: "done two"}},
		{RunDTO: apitypes.RunDTO{ID: "eeeeeeee-5", Kind: "issue", Status: "cancelled", IssueTitle: "done three"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)

	if n := len(m.board.visible()); n != 5 {
		t.Fatalf("expected 5 runs visible before the toggle, got %d", n)
	}

	m = press(t, m, keyHideDone)
	if !m.board.hideDone {
		t.Fatal("h did not set hideDone")
	}
	vis := m.board.visible()
	if len(vis) != 2 {
		t.Fatalf("hideDone should keep 2 (running + awaiting_approval), got %d", len(vis))
	}
	for _, r := range vis {
		if terminalRunStatuses[r.Status] {
			t.Errorf("terminal run %s (%s) still visible under hideDone", r.ID, r.Status)
		}
	}
	out := m.View().Content
	for _, want := range []string{"active only", "show done"} {
		if !strings.Contains(out, want) {
			t.Errorf("board view missing %q under hideDone\n%s", want, out)
		}
	}

	m = press(t, m, keyHideDone)
	if m.board.hideDone || len(m.board.visible()) != 5 {
		t.Errorf("second h did not restore the full board (hideDone=%v, %d visible)", m.board.hideDone, len(m.board.visible()))
	}
	if !strings.Contains(m.View().Content, "fold done") {
		t.Errorf("footer did not return to 'fold done'\n%s", m.View().Content)
	}
}

// On the admin board [h] is a no-op — AdminListRuns returns non-terminal runs only — so it
// must not flip the header label or the footer hint (a visible change with no row change
// reads as a broken toggle). The "hide done" key hint is dropped there entirely.
func TestTUIBoardHideDoneInertOnAdminBoard(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = press(t, m, keyAdmin)
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, admin: true, runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "cccccccc-1", Kind: "issue", Status: "running", IssueTitle: "live"}},
	}})
	m = next.(tuiModel)

	if strings.Contains(m.View().Content, "fold done") {
		t.Error("admin board footer should not offer the fold-done toggle")
	}
	m = press(t, m, keyHideDone)
	out := m.View().Content
	if strings.Contains(out, "active only") {
		t.Errorf("h flipped the header label on the admin board, where it is a no-op\n%s", out)
	}
	if strings.Contains(out, "show done") {
		t.Errorf("h flipped the footer hint on the admin board, where it is a no-op\n%s", out)
	}
}

// When [h] hides every run because they are all terminal, the empty state says so rather
// than the misleading "No runs yet".
func TestTUIBoardHideDoneEmptyState(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "completed", IssueTitle: "done one"}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "failed", IssueTitle: "done two"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = press(t, next.(tuiModel), keyHideDone)

	out := m.View().Content
	if strings.Contains(out, "No runs yet") {
		t.Errorf("all-terminal board under hideDone shows the misleading 'No runs yet'\n%s", out)
	}
	if !strings.Contains(out, "finished runs are folded") {
		t.Errorf("empty state should explain the folded finished runs\n%s", out)
	}
}

// The judge marker is a right-aligned column: markers of different widths end at the same
// column, so every verdict-carrying row has the same total visual width. The pre-fix board
// trailed the marker straight after the title, so a shorter marker produced a narrower row.
func TestTUIBoardJudgeMarkerRightAligned(t *testing.T) {
	issues, okVerdict := "issues", "ok"
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "completed", IssueTitle: "alpha"}, JudgeVerdict: &issues, JudgeTodoCount: 2}, // "⚖ issues · 2" — widest
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "completed", IssueTitle: "beta"}, JudgeVerdict: &okVerdict},                  // "⚖ ok" — narrowest, no count
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	out := next.(tuiModel).View().Content

	var widths []int
	for _, r := range strings.Split(out, "\n") {
		if strings.Contains(r, "⚖") {
			widths = append(widths, visualWidth(r))
		}
	}
	if len(widths) != 2 {
		t.Fatalf("expected 2 judge-marker rows, got %d\n%s", len(widths), out)
	}
	if widths[0] != widths[1] {
		t.Errorf("judge markers not right-aligned: verdict rows have unequal width %d vs %d\n%s", widths[0], widths[1], out)
	}
}

// With more runs than fit the terminal height, the board windows the list so the header and
// footer stay on screen (the footer carries the key legend — it must never scroll off), a
// position readout shows where you are, and scrolling keeps the cursor in view.
func TestTUIBoardWindowsToHeightAndKeepsFooter(t *testing.T) {
	runs := make([]apitypes.RunListItemDTO, 0, 100)
	for i := 0; i < 100; i++ {
		runs = append(runs, apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
			ID: fmt.Sprintf("%08d-1111-2222-3333-444444444444", i), Kind: "issue", Status: "running",
			IssueTitle: fmt.Sprintf("run %d", i)}})
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")
	m.width, m.height = 120, 24
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)

	linesFit := func(out string) {
		t.Helper()
		if n := len(strings.Split(out, "\n")); n > m.height {
			t.Errorf("board rendered %d lines at height %d — it must window to fit", n, m.height)
		}
	}
	footerShown := func(out string) {
		t.Helper()
		// The key letters are tungsten and the labels faint, so "q" and "quit" sit in separate
		// SGR spans — assert on the label alone.
		if !strings.Contains(out, "quit") {
			t.Errorf("footer key legend missing — it scrolled off the bottom\n%s", out)
		}
	}

	out := m.View().Content
	linesFit(out)
	footerShown(out)
	if !strings.Contains(out, "100 runs") {
		t.Errorf("windowed board should show the total in the summary cluster (\"100 runs\")\n%s", out)
	}
	if !strings.Contains(out, "–") {
		t.Errorf("windowed board should show a position readout (lo–hi)\n%s", out)
	}

	// Drive the cursor far down; the footer and height cap must still hold, and the selected
	// run must be on screen.
	for i := 0; i < 60; i++ {
		m = press(t, m, "j")
	}
	out = m.View().Content
	linesFit(out)
	footerShown(out)
	sel, _ := m.board.selected()
	if !strings.Contains(out, shortRunID(sel.ID)) {
		t.Errorf("cursor scrolled out of the window: selected %s not rendered\n%s", sel.ID, out)
	}
}

// The strip's meters/settings must refresh on their own 60s ticker, not just at Init and on
// the manual refresh key — otherwise the boardRateLimitStrip freezes at its launch value while
// the 2s boardTickMsg re-fetches only runs. This is the mutation guard: it reddens if the
// stripTickMsg handler stops re-fetching the meters.
func TestTUIStripTickRefetchesMeters(t *testing.T) {
	// Shrink the strip cadence so the re-arm (tea.Tick) fires promptly instead of in 60s.
	orig := rateLimitPollInterval
	rateLimitPollInterval = time.Millisecond
	t.Cleanup(func() { rateLimitPollInterval = orig })

	fake := &uzicli.FakeClient{SelfMeters: []apitypes.TokenRateLimitDTO{
		{SecretID: "sec-x", Label: "tok", IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 42}}},
	}}
	m := tuiTestModel(t, fake, "")

	_, cmd := m.Update(stripTickMsg{})
	if cmd == nil {
		t.Fatal("stripTickMsg returned no command; the strip will never refresh on its own ticker")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("stripTickMsg command yielded %T, want a tea.BatchMsg", msg)
	}

	var sawMeters, sawRearm bool
	for _, inner := range batch {
		if inner == nil {
			continue
		}
		switch im := inner().(type) {
		case rateLimitsMsg:
			// Non-vacuous: assert the fake's meter actually reached the message, not merely
			// that the batch is non-empty.
			if len(im.tokens) != 1 {
				t.Fatalf("rateLimitsMsg carried %d tokens, want 1 (the fake's meter)", len(im.tokens))
			}
			if got := im.tokens[0]; got.SecretID != "sec-x" || got.Limits.FiveHour == nil || got.Limits.FiveHour.Pct != 42 {
				t.Errorf("rateLimitsMsg did not carry the fake's meter: %+v", got)
			}
			sawMeters = true
		case stripTickMsg:
			sawRearm = true
		}
	}
	if !sawMeters {
		t.Error("the stripTickMsg batch did not re-fetch the meters; the strip would freeze at its launch value")
	}
	if !sawRearm {
		t.Error("the stripTickMsg batch did not re-arm the 60s strip ticker; it would fire only once")
	}
}

// Init must start the strip's own 60s ticker, so the rate-limit strip refreshes without the
// user pressing the manual refresh key.
func TestTUIInitStartsStripTicker(t *testing.T) {
	orig := rateLimitPollInterval
	rateLimitPollInterval = time.Millisecond
	t.Cleanup(func() { rateLimitPollInterval = orig })

	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init command yielded %T, want a tea.BatchMsg", msg)
	}

	var sawRearm bool
	for _, inner := range batch {
		if inner == nil {
			continue
		}
		if _, ok := inner().(stripTickMsg); ok {
			sawRearm = true
		}
	}
	if !sawRearm {
		t.Error("Init did not start the strip's 60s ticker; the strip would only refresh on the manual key")
	}
}

// spendUsage is the shared PRD #650 M3 usage fixture: a real-shaped run at $9.55 with a
// heavily-cached token profile (in 2.4M / out 88.4k / cache 14.2M).
func spendUsage() *apitypes.UsageDTO {
	return &apitypes.UsageDTO{
		CostUSD: 9.55, InputTokens: 2_400_000, CacheReadTokens: 14_200_000,
		CacheCreationTokens: 0, OutputTokens: 88_400,
	}
}

// spendModel seeds a detail model at a comfortable 100x40 with the given usage, so the run-view
// headline cost and the crew-rail SPEND block both have a run to draw. No frames are seeded, so
// renderLaneRail takes the no-lanes path.
func spendModel(t *testing.T, usage *apitypes.UsageDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	m.width, m.height = 100, 40
	return applyDetail(m, apitypes.RunDTO{
		ID: "run-detail", Status: "running", IssueTitle: "cost run", Usage: usage,
	}, nil)
}

// TestTUIDetailHeadlineCost — PRD #650 M3 Part A: the run-view status tag carries the run's
// rolled-up cost, faint, beside the duration. A subscription-auth $0 run renders "—" (never
// "$0.00"), and a pre-#40 nil-Usage run appends no cost token at all.
func TestTUIDetailHeadlineCost(t *testing.T) {
	header := func(m tuiModel) string { return stripANSI(strings.Join(m.detailHeaderLines(), "\n")) }

	if h := header(spendModel(t, spendUsage())); !strings.Contains(h, "$9.55") {
		t.Errorf("headline status tag missing the cost $9.55:\n%s", h)
	}

	// Subscription-auth $0 (real token usage, zero cost) → "—", never "$0.00".
	zero := header(spendModel(t, &apitypes.UsageDTO{CostUSD: 0, InputTokens: 100}))
	if !strings.Contains(zero, "—") {
		t.Errorf("a $0 usage run should render the em-dash cost:\n%s", zero)
	}
	if strings.Contains(zero, "$0.00") || strings.Contains(zero, "$") {
		t.Errorf("a $0 usage run must not render a dollar cost:\n%s", zero)
	}

	// nil Usage (unclaimed / pre-#40) → no cost token in the header at all.
	nilU := header(spendModel(t, nil))
	if strings.Contains(nilU, "$") {
		t.Errorf("a nil-Usage run must append no cost token to the header:\n%s", nilU)
	}
}

// TestTUIDetailSpendBlock — PRD #650 M3 Part B: the crew rail draws a SPEND block with the cost
// headline over an in/out/cache token breakdown, sitting ABOVE the ACCOUNTS block. A nil-Usage
// run draws no SPEND block.
func TestTUIDetailSpendBlock(t *testing.T) {
	// Seed a rate meter + the run's account so ACCOUNTS renders, then confirm SPEND sits above it.
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(tuiModel)
	next, _ = m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{
		okMeter("sec-run", "runacct", true, 33, 61),
	}})
	m = next.(tuiModel)
	sid, lbl := "sec-run", "runacct"
	m = applyDetail(m, apitypes.RunDTO{
		ID: "run-detail", Status: "running", Health: "ok", IssueTitle: "cost run",
		AnthropicSecretID: &sid, AnthropicSecretLabel: &lbl, Usage: spendUsage(),
	}, nil)

	out := stripANSI(m.renderLaneRail())
	pct := cacheDisplayPct(2_400_000, 14_200_000, 0)
	for _, want := range []string{"SPEND", "$9.55", "in 2.40M", "out 88.4k", "cache 14.20M", itoa(pct) + "%"} {
		if !strings.Contains(out, want) {
			t.Errorf("SPEND block missing %q:\n%s", want, out)
		}
	}

	// SPEND sits directly ABOVE the ACCOUNTS block (both present).
	si, ai := strings.Index(out, "SPEND"), strings.Index(out, "ACCOUNTS")
	if si < 0 || ai < 0 {
		t.Fatalf("expected both SPEND and ACCOUNTS in the rail:\n%s", out)
	}
	if si > ai {
		t.Errorf("SPEND should render above ACCOUNTS, got SPEND@%d ACCOUNTS@%d:\n%s", si, ai, out)
	}

	// A nil-Usage run draws no SPEND block.
	nilU := stripANSI(spendModel(t, nil).renderLaneRail())
	if strings.Contains(nilU, "SPEND") {
		t.Errorf("a nil-Usage run must not draw a SPEND block:\n%s", nilU)
	}
}

// TestTUIDetailSpendDropsWhole — the SPEND block is whole-block-or-nothing: when the remaining
// rail height cannot hold all three lines, renderSpend returns "" rather than a half-drawn block
// (header with no cache line), because joinColumns clamps the rail by dropping its bottom lines.
func TestTUIDetailSpendDropsWhole(t *testing.T) {
	m := spendModel(t, spendUsage())

	// A large usedRows leaves no room: renderSpend returns "" (never a partial block).
	if got := m.renderSpend(m.transcriptViewport()); got != "" {
		t.Errorf("renderSpend must be empty when the rail height is exhausted, got %q", got)
	}
	// With generous room it renders the whole block.
	full := stripANSI(m.renderSpend(0))
	for _, want := range []string{"SPEND", "cache"} {
		if !strings.Contains(full, want) {
			t.Errorf("renderSpend at generous height missing %q:\n%s", want, full)
		}
	}

	// End to end at a short height the block is absent entirely — and if SPEND is gone, so is its
	// own "cache" line (no half-drawn block).
	short := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	short.width, short.height = 100, 12
	nx := applyDetail(short, apitypes.RunDTO{
		ID: "run-detail", Status: "running", IssueTitle: "cost run",
		Milestones:          []apitypes.Milestone{{ID: "m1", Title: "a"}, {ID: "m2", Title: "b"}, {ID: "m3", Title: "c"}},
		MilestonesCompleted: []string{"m1"},
		Usage:               spendUsage(),
	}, nil)
	rail := stripANSI(nx.renderLaneRail())
	if strings.Contains(rail, "SPEND") {
		t.Fatalf("SPEND should be dropped whole at a short height:\n%s", rail)
	}
	if strings.Contains(rail, "cache 14.20M") {
		t.Errorf("SPEND dropped but its cache line survived — a half-drawn block:\n%s", rail)
	}
}

// TestTUIDetailSpendZeroCost — a $0 run WITH real token usage shows the "—" total in the SPEND
// block while the in/out/cache lines still render (the token breakdown does not depend on cost).
func TestTUIDetailSpendZeroCost(t *testing.T) {
	m := spendModel(t, &apitypes.UsageDTO{CostUSD: 0, InputTokens: 1000, OutputTokens: 50})
	out := stripANSI(m.renderSpend(0))
	if !strings.Contains(out, "—") {
		t.Errorf("a $0 SPEND total should render the em-dash:\n%s", out)
	}
	for _, want := range []string{"in 1.0k", "out 50", "cache 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("SPEND token line missing %q on a $0 run:\n%s", want, out)
		}
	}
}

// TestTUIBoardCostAsciiSurvives — PRD #650 M4: under an Ascii (NO_COLOR) colorprofile downgrade
// the board's plain-text cost cues survive. The "$", digits, and "—" are derived numerics, not
// coloured chrome — only the tungsten/faint accent is stripped downstream at flush — so cost is
// never signalled by colour alone. Mirrors TestBoardRateLimitStripAsciiSignalSurvives. NOTE: the
// colorprofile Writer strips SGR downstream at flush, which the in-frame text does not show — so
// this asserts the derived text cues are present regardless of colour.
func TestTUIBoardCostAsciiSurvives(t *testing.T) {
	// Two tokens so the credential gate clears (secretsMsg{count:2}); at width 120 the mile
	// threshold (111) is under the terminal so every column, COST included, renders.
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "a real cost",
			Usage: &apitypes.UsageDTO{CostUSD: 12.0, InputTokens: 100}}}, // → "$12"
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "running", IssueTitle: "a subscription run",
			Usage: &apitypes.UsageDTO{CostUSD: 0, InputTokens: 100}}}, // → "—"
	}
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: runs}, "")
	m.width = 120
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	next, _ = m.Update(secretsMsg{count: 2})
	m = next.(tuiModel)

	// Downgrade to the Ascii (NO_COLOR) profile, then render.
	next, _ = m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	out := stripANSI(m.View().Content)

	// The per-row "$12" cell, the "—" subscription cue, and the rounded floor total (round(12+0) =
	// "$12") all survive with colour gone.
	for _, want := range []string{"$12", "—"} {
		if !strings.Contains(out, want) {
			t.Errorf("Ascii-profile board dropped the plain cost cue %q:\n%s", want, out)
		}
	}
}

// TestTUIDetailCostAsciiSurvives — PRD #650 M4: under an Ascii (NO_COLOR) colorprofile downgrade
// the run-view headline cost and the crew-rail SPEND block keep their plain-text figures. Only the
// faint/tungsten accent is colour and it is stripped downstream at flush; the "$", digits, and the
// token breakdown are derived numerics that survive. Mirrors TestRailRateMetersAsciiSignalSurvives.
func TestTUIDetailCostAsciiSurvives(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	m.width, m.height = 100, 40
	m = applyDetail(m, apitypes.RunDTO{
		ID: "run-detail", Status: "running", Health: "ok", IssueTitle: "cost run",
		Usage: &apitypes.UsageDTO{
			CostUSD: 9.55, InputTokens: 2_400_000, CacheReadTokens: 14_200_000,
			CacheCreationTokens: 0, OutputTokens: 88_400,
		},
	}, nil)

	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)

	// The header carries the faint headline cost even with colour gone.
	if h := stripANSI(strings.Join(m.detailHeaderLines(), "\n")); !strings.Contains(h, "$9.55") {
		t.Errorf("Ascii-profile header dropped the headline cost $9.55:\n%s", h)
	}

	// The SPEND block keeps its total and its in/out/cache breakdown (fmtTokens M-tier is two-decimal
	// → 2.40M / 14.20M) plus the cacheDisplayPct string.
	rail := stripANSI(m.renderLaneRail())
	pct := cacheDisplayPct(2_400_000, 14_200_000, 0)
	for _, want := range []string{"SPEND", "$9.55", "in 2.40M", "out 88.4k", "cache 14.20M", itoa(pct) + "%"} {
		if !strings.Contains(rail, want) {
			t.Errorf("Ascii-profile SPEND block dropped the plain cue %q:\n%s", want, rail)
		}
	}
}

// A detail load that resolves AFTER the user has left the run must be dropped, not applied:
// exitToBoard resets m.detail to its zero value (nil `seen` map), so a late applyRun/applyTailPage
// would write the nil map and panic (observed in the field). The runID guard drops it.
func TestTUIDetailLateLoadAfterExitIsDropped(t *testing.T) {
	now := time.Now()
	runID := "late-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	// Leave the run before its load resolves.
	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatalf("esc should return to the board, got view %v", m.view)
	}
	// The in-flight load lands now, for the run just left. It must be dropped, and must not
	// panic against the torn-down (nil-seen) detail.
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", Health: "ok"},
		[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "hello", now)})
	if m.detail.runLoaded {
		t.Error("a load for a run the user has left must not populate the detail")
	}
	if len(m.detail.frames) != 0 {
		t.Errorf("stale load spliced %d frames into a torn-down detail", len(m.detail.frames))
	}
}

// A load that resolves for a DIFFERENT run than the one now open must be dropped, so a slow
// run-A load cannot overwrite the run-B transcript the user drilled into next.
func TestTUIDetailStaleLoadForOtherRunIsDropped(t *testing.T) {
	now := time.Now()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-b")
	m = applyDetail(m, apitypes.RunDTO{ID: "run-a", Status: "running", Health: "ok"},
		[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "from A", now)})
	if m.detail.run.ID == "run-a" {
		t.Error("a load for run-a overwrote the open run-b detail")
	}
	if len(m.detail.frames) != 0 {
		t.Errorf("run-a load spliced %d frames into run-b", len(m.detail.frames))
	}
}

// addFrame must be total on a zero-value detailState: newDetailState seeds `seen`, but a
// constructor-less detail (the zero value exitToBoard assigns) has a nil map, and the
// field-observed crash was exactly a write to it. Lazy-init keeps it from panicking.
func TestDetailAddFrameTotalOnZeroValue(t *testing.T) {
	var d detailState // zero value: seen is nil
	d.addFrame(laneFrame{Seq: 1})
	if len(d.frames) != 1 {
		t.Fatalf("addFrame on a zero-value detailState dropped the frame: got %d", len(d.frames))
	}
	// A second frame with the same seq is deduped, which only works if seen was initialized
	// and written — so this also proves the lazy map is live, not just non-nil.
	d.addFrame(laneFrame{Seq: 1})
	if len(d.frames) != 1 {
		t.Errorf("dedup did not hold; seen map not initialized: got %d frames", len(d.frames))
	}
}

// TestTUIBoardSelectedFloorTitleHasExplicitForeground pins issue #938 fix 2: a SELECTED
// floor-band row's title carries the explicit tungsten foreground, so it stays legible on
// the warm selection bar (on a light terminal the default-ink title would otherwise be
// dark-on-dark). Unselected floor titles keep the default ink (nil fg). Mutation that
// reddens this: reverting the floor-band `default` case to leave titleC nil.
func TestTUIBoardSelectedFloorTitleHasExplicitForeground(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running", IssueTitle: "selectedtitle"}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-2", Kind: "issue", Status: "running", IssueTitle: "othertitle"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)
	out := m.View().Content

	// The selected (cursor-0) floor title is drawn exactly as boardRow draws it: the tungsten
	// fg over the warm selection bg. Reconstruct that span and require it present — this is the
	// span that disappears if the floor-band default case is dropped.
	selTitle := paintSeg(m.pal.tungsten, m.pal.selBg, false, "selectedtitle")
	if !strings.Contains(out, selTitle) {
		t.Errorf("selected floor title is not painted with the tungsten fg over the selection bar\nwant span %q\n%s", selTitle, out)
	}
	// Control (non-vacuous): the UNSELECTED floor title must NOT carry the tungsten fg — it
	// keeps the default ink (nil fg). If this span appeared, the assertion above could pass for
	// the wrong reason (every floor title tungsten-painted regardless of selection).
	otherTungsten := paintSeg(m.pal.tungsten, nil, false, "othertitle")
	if strings.Contains(out, otherTungsten) {
		t.Errorf("unselected floor title must keep default ink, not the tungsten fg\ngot span %q\n%s", otherTungsten, out)
	}
}

// TestTUIInitRequestsBackgroundColor pins issue #938 fix 1: Init issues
// tea.RequestBackgroundColor at startup so the terminal reports its background and the
// BackgroundColorMsg path can flip the theme. We inspect initCmds without executing any Cmd
// (the tick cmds block on tea.Tick), comparing function pointers.
func TestTUIInitRequestsBackgroundColor(t *testing.T) {
	fake := &uzicli.FakeClient{}
	m := tuiTestModel(t, fake, "")
	want := reflect.ValueOf(tea.RequestBackgroundColor).Pointer()
	found := false
	for _, c := range m.initCmds() {
		if c != nil && reflect.ValueOf(c).Pointer() == want {
			found = true
		}
	}
	if !found {
		t.Errorf("initCmds does not include tea.RequestBackgroundColor; the theme probe will never fire")
	}
}

// TestTUIBackgroundColorMsgFlipsPalette pins that the BackgroundColorMsg handler flips
// m.dark and rebuilds the palette in BOTH directions — the path fix 1 makes actually fire.
func TestTUIBackgroundColorMsgFlipsPalette(t *testing.T) {
	fake := &uzicli.FakeClient{}
	m := tuiTestModel(t, fake, "") // dark=true by default
	darkSelBg := m.pal.selBg

	// A light background reported by the terminal → light theme.
	next, _ := m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")})
	m = next.(tuiModel)
	if m.dark {
		t.Errorf("a light background must set m.dark=false")
	}
	if fgSGR(m.pal.selBg) == fgSGR(darkSelBg) {
		t.Errorf("selBg did not change when flipping to the light theme")
	}
	if fgSGR(m.pal.selBg) != fgSGR(newPalette(false).selBg) {
		t.Errorf("flipped selBg %v does not match newPalette(false).selBg %v", m.pal.selBg, newPalette(false).selBg)
	}

	// The converse: a dark background reported → dark theme again.
	next, _ = m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#000000")})
	m = next.(tuiModel)
	if !m.dark {
		t.Errorf("a dark background must set m.dark=true")
	}
	if fgSGR(m.pal.selBg) != fgSGR(newPalette(true).selBg) {
		t.Errorf("flipped-back selBg %v does not match newPalette(true).selBg %v", m.pal.selBg, newPalette(true).selBg)
	}
}

var _ tea.Model = tuiModel{}
