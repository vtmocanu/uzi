package factoryui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The demo model is driven in-process (no PTY): handleKey takes a key string and returns a
// model, Body() returns the rendered string. This proves navigation + rendering work headless
// — the same seam the shipped TUI's tests use.

func drive(t *testing.T, m Model, k string) Model {
	t.Helper()
	next, _ := m.handleKey(k)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("handleKey(%q) returned %T, not factoryui.Model", k, next)
	}
	return nm
}

func TestDemoBoardOpensDetailAndBanner(t *testing.T) {
	m := New()
	m.width, m.height = 120, 40

	board := m.Body()
	for _, want := range []string{"factory floor", "running", "awaiting_approval", "needs you"} {
		if !strings.Contains(board, want) {
			t.Errorf("board does not render %q", want)
		}
	}

	// cursor starts on the running run; move to the awaiting_approval run and open it.
	m = drive(t, m, "j")
	if m.cursor != 1 {
		t.Fatalf("j did not move the cursor (got %d)", m.cursor)
	}
	m = drive(t, m, "enter")
	if m.view != viewDetail {
		t.Fatal("enter did not open the detail view")
	}
	detail := m.Body()
	if !strings.Contains(detail, "PLAN GATE") {
		t.Errorf("the awaiting_approval run's detail is missing the PLAN GATE banner:\n%s", detail)
	}
	if !strings.Contains(detail, "approve") {
		t.Error("the plan gate does not promote the approve action")
	}
	m = drive(t, m, "esc")
	if m.view != viewBoard {
		t.Fatal("esc did not return to the board")
	}
}

// The detail view has two focusable panes: left/right (and tab) move focus; up/down act
// WITHIN the focused pane — moving between agents on the rail, scrolling on the transcript.
func TestDemoDetailFocusPanes(t *testing.T) {
	m := New()
	m.width, m.height = 120, 40
	m = drive(t, m, "enter") // open the running run (cursor 0)
	if m.view != viewDetail {
		t.Fatal("enter did not open detail")
	}
	if m.focus != FocusRail {
		t.Fatalf("detail should open focused on the crew rail, got %d", m.focus)
	}

	// down while the RAIL is focused moves between agents (the default pane).
	lane := m.selLane
	m = drive(t, m, "j")
	if m.selLane != lane+1 {
		t.Errorf("down on the focused rail did not move to the next agent (got %d)", m.selLane)
	}

	// right focuses the transcript; now down scrolls instead of changing the agent.
	m = drive(t, m, "right")
	if m.focus != FocusTranscript {
		t.Fatal("right did not focus the transcript")
	}
	lane = m.selLane
	m = drive(t, m, "j")
	if m.selLane != lane {
		t.Error("down on the transcript pane changed the selected agent; it should scroll")
	}

	// left returns focus to the crew rail.
	m = drive(t, m, "left")
	if m.focus != FocusRail {
		t.Errorf("left did not return focus to the crew rail (got %d)", m.focus)
	}

	// the focused pane title carries the focus marker.
	if !strings.Contains(m.Body(), "▎") {
		t.Error("no focus indicator (▎) rendered on the focused pane")
	}
}

// Follow-live: a live run auto-tails; scrolling up detaches (PAUSED); `g` re-attaches.
func TestDemoFollowLive(t *testing.T) {
	m := New()
	m.width, m.height = 120, 24
	m = drive(t, m, "enter") // running run
	if !m.follow {
		t.Fatal("a live run should open following")
	}
	if !strings.Contains(m.Body(), "FOLLOWING") {
		t.Error("the FOLLOWING indicator is not shown while tailing a live run")
	}

	// a live tick appends output.
	before := len(m.detail.Transcript)
	next, _ := m.Update(tickMsg{})
	m = next.(Model)
	if len(m.detail.Transcript) <= before {
		t.Fatal("a live tick did not append output to follow")
	}

	// focus the transcript, then scrolling up detaches follow.
	m = drive(t, m, "right")
	m = drive(t, m, "k")
	if m.follow {
		t.Error("scrolling up did not detach follow")
	}
	if !strings.Contains(m.Body(), "PAUSED") {
		t.Error("the PAUSED indicator is not shown after detaching")
	}

	// g re-attaches and jumps to the newest output.
	m = drive(t, m, "g")
	if !m.follow || m.scroll != 0 {
		t.Errorf("g did not re-attach follow (follow=%v scroll=%d)", m.follow, m.scroll)
	}
}

func TestDemoReviewOverlay(t *testing.T) {
	m := New()
	m.width, m.height = 120, 40
	m = drive(t, m, "enter")
	m = drive(t, m, "v")
	if !m.reviewOpen {
		t.Fatal("v did not open the review overlay")
	}
	if got := m.Body(); !strings.Contains(got, "issues") {
		t.Errorf("review overlay does not render the verdict:\n%s", got)
	}
	m = drive(t, m, "esc")
	if m.reviewOpen {
		t.Error("esc did not close the review overlay")
	}
}

func TestDemoFilterAndQuit(t *testing.T) {
	m := New()
	m.width, m.height = 120, 40
	m = drive(t, m, "/")
	for _, k := range []string{"f", "l", "a", "k", "y"} {
		m = drive(t, m, k)
	}
	if n := len(m.visible()); n != 1 {
		t.Fatalf("filter %q matched %d rows, want 1", m.filter, n)
	}
	m = drive(t, m, "esc")
	if m.filter != "" || len(m.visible()) != len(m.runs) {
		t.Errorf("esc did not clear the filter (filter=%q)", m.filter)
	}
	_, cmd := m.handleKey("q")
	if cmd == nil {
		t.Error("q did not return a quit command")
	}
}

func TestDemoWindowSize(t *testing.T) {
	m := New()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)
	if m.width != 80 || m.height != 24 {
		t.Errorf("WindowSizeMsg not applied: %dx%d", m.width, m.height)
	}
}
