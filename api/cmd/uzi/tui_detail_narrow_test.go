package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// assertNarrowFrame checks every line of the rendered frame fits width columns and that the
// CREW header row is present and still carries the rail divider (issue #2591).
func assertNarrowFrame(t *testing.T, label, content string, width int) {
	t.Helper()
	crew := false
	for _, ln := range strings.Split(content, "\n") {
		if w := visualWidth(ln); w > width {
			t.Errorf("%s: line is %d cols (> %d): %q", label, w, width, stripANSI(ln))
		}
		if plain := stripANSI(ln); strings.Contains(plain, "CREW") {
			crew = true
			if !strings.Contains(plain, "▏") {
				t.Errorf("%s: CREW row lost its divider: %q", label, plain)
			}
		}
	}
	if !crew {
		t.Errorf("%s: no CREW row rendered (test would be vacuous):\n%s", label, stripANSI(content))
	}
}

func TestRunDetailRowsFitNarrowTerminal(t *testing.T) {
	run, msgs := autoFoldRun(true, true)
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 20
	m = applyDetail(m, run, msgs)
	m = answerUpdate(t, m, runInputsMsg{runID: m.detail.runID, gen: m.detail.gen})
	m = answerUpdate(t, m, tea.WindowSizeMsg{Width: 30, Height: 8})
	assertNarrowThemes(t, "detail", m, 30)
}

func TestAnswerViewRowsFitNarrowTerminal(t *testing.T) {
	m, _ := answerFlowModel(t, `{"question_id":"q","questions":[{"header":"H","question":"ok?","options":[{"label":"y"}]}]}`)
	m = answerKeys(t, m, "i")
	m = answerUpdate(t, m, tea.WindowSizeMsg{Width: 30, Height: 16})
	assertNarrowThemes(t, "answer", m, 30)
}

// assertNarrowThemes renders m with both palettes, as-is and downgraded through an Ascii
// colorprofile.Writer (what a NO_COLOR terminal receives), and checks each frame.
func assertNarrowThemes(t *testing.T, view string, m tuiModel, width int) {
	t.Helper()
	for _, dark := range []bool{false, true} {
		mm := m
		mm.pal = newPalette(dark)
		raw := mm.View().Content
		assertNarrowFrame(t, fmt.Sprintf("%s dark=%t", view, dark), raw, width)
		var buf bytes.Buffer
		w := colorprofile.NewWriter(&buf, nil)
		w.Profile = colorprofile.Ascii
		if _, err := w.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if out := buf.String(); strings.Contains(out, "38;") || strings.Contains(out, "48;") {
			t.Fatalf("%s dark=%t: Ascii writer retained color SGR", view, dark)
		}
		assertNarrowFrame(t, fmt.Sprintf("%s dark=%t ascii", view, dark), buf.String(), width)
	}
}
