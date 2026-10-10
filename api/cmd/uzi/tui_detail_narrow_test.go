package main

import (
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
	for _, dark := range []bool{false, true} {
		for name, prof := range map[string]colorprofile.Profile{"default": colorprofile.TrueColor, "ascii": colorprofile.Ascii} {
			mm := m
			mm.pal = newPalette(dark)
			if name == "ascii" {
				mm = answerUpdate(t, mm, tea.ColorProfileMsg{Profile: prof})
			}
			assertNarrowFrame(t, name, mm.View().Content, 30)
		}
	}
}

func TestAnswerViewRowsFitNarrowTerminal(t *testing.T) {
	m, _ := answerFlowModel(t, `{"question_id":"q","questions":[{"header":"H","question":"ok?","options":[{"label":"y"}]}]}`)
	m = answerKeys(t, m, "i")
	m = answerUpdate(t, m, tea.WindowSizeMsg{Width: 30, Height: 16})
	for _, dark := range []bool{false, true} {
		for _, ascii := range []bool{false, true} {
			mm := m
			mm.pal = newPalette(dark)
			if ascii {
				mm = answerUpdate(t, mm, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			}
			assertNarrowFrame(t, "answer", mm.View().Content, 30)
		}
	}
}
