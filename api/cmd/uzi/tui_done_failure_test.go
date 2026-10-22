package main

import (
	"bytes"
	"fmt"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Shared with uxlab so the review scenes exercise the same terminal outcomes.
func doneFailureRows() []apitypes.RunListItemDTO {
	return []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "failed01", Kind: "issue", Status: "failed", IssueTitle: "Retry interrupted implementation", IssueIID: ip(42), IssueWebURL: sp("https://example.com/issues/42")}},
		{RunDTO: apitypes.RunDTO{ID: "landing1", Kind: "issue", Status: "failed", LandingState: "needs_landing", IssueTitle: "Land recovered commits", IssueIID: ip(43), IssueWebURL: sp("https://example.com/issues/43")}},
		{RunDTO: apitypes.RunDTO{ID: "stopped1", Kind: "issue", Status: "failed", StopKind: sp("plan_rejected"), Health: "stalled", LandingState: "needs_landing", IssueTitle: "Human rejected the plan", IssueIID: ip(44)}},
		{RunDTO: apitypes.RunDTO{ID: "done0001", Kind: "issue", Status: "completed", IssueTitle: "Completed implementation", IssueIID: ip(45)}},
		{RunDTO: apitypes.RunDTO{ID: "cancel01", Kind: "issue", Status: "cancelled", IssueTitle: "Cancelled implementation", IssueIID: ip(46)}},
	}
}

func TestBoardDoneFailureInk(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			for i, r := range doneFailureRows() {
				t.Run(fmt.Sprintf("dark=%t/selected=%t/%s", dark, selected, r.ID), func(t *testing.T) {
					rows := []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "active01", Status: "running"}}, r}
					m := tuiTestModel(t, &uzicli.FakeClient{}, "")
					m.dark, m.pal = dark, newPalette(dark)
					m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: rows})
					if selected {
						m.board.cursor = 1
					}
					out := m.View().Content
					line := boardAgeTestRow(t, out, r.ID)
					var bg color.Color
					idInk := m.pal.faintC
					if selected {
						bg, idInk = m.pal.selBg, m.pal.tungsten
					}
					stateInk, issueInk := m.pal.faintC, idInk
					word, glyph := "done", "✓"
					switch i {
					case 0, 1:
						stateInk = lipgloss.Color("#a04444")
						if dark {
							stateInk = lipgloss.Color("#cf8585")
						}
						issueInk = stateInk
						word, glyph = "failed", "✗"
						if i == 1 {
							word = "needs landing"
						}
					case 2, 4:
						word, glyph = "stopped", "·"
					}
					for label, span := range map[string]string{
						"spine":  paintSeg(stateInk, bg, false, "▌"),
						"glyph":  paintSeg(stateInk, bg, false, glyph),
						"status": paintSeg(stateInk, bg, false, padCell(word, boardStatusWordWidth)),
						"run id": paintSeg(idInk, bg, selected, padCell(r.ID, boardIDWidth)),
						"issue":  paintSeg(issueInk, bg, selected, "#"+itoa(int(*r.IssueIID))),
						"title":  paintSeg(m.pal.faintC, bg, false, r.IssueTitle),
					} {
						if !strings.Contains(line, span) {
							t.Errorf("%s missing expected SGR span %q in %q", label, span, line)
						}
					}
					if r.IssueWebURL != nil && !strings.Contains(line, "\x1b]8;;"+*r.IssueWebURL+"\x1b\\") {
						t.Error("failed row lost its OSC-8 issue link")
					}
				})
			}
		}
	}
}

func TestBoardDoneVerdictInk(t *testing.T) {
	rows := append([]apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "judgerun", Status: "running"}}}, doneFailureRows()...)
	for _, dark := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			for _, r := range rows {
				for _, verdict := range []string{"issues", "ideal", "ok"} {
					t.Run(fmt.Sprintf("dark=%t/selected=%t/%s/%s", dark, selected, r.ID, verdict), func(t *testing.T) {
						r.JudgeVerdict, r.JudgeTodoCount = sp(verdict), 2
						m := tuiTestModel(t, &uzicli.FakeClient{}, "")
						m.dark, m.pal = dark, newPalette(dark)
						m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{
							{RunDTO: apitypes.RunDTO{ID: "active01", Status: "running"}}, r,
						}})
						visible := m.board.visible()
						for i, row := range visible {
							if row.ID == r.ID {
								m.board.cursor = i
								if !selected {
									m.board.cursor = (i + 1) % len(visible)
								}
								break
							}
						}
						var bg color.Color
						if selected {
							bg = m.pal.selBg
						}
						ink := m.pal.faintC
						if verdict == "issues" {
							ink = m.pal.alarm
							if r.Status != "running" {
								ink = m.pal.doneAlarm
							}
						}
						line := boardAgeTestRow(t, m.View().Content, r.ID)
						span := paintSeg(ink, bg, false, "⚖ "+verdict+" · 2")
						if !strings.Contains(line, span) {
							t.Errorf("judge badge missing expected SGR span %q in %q", span, line)
						}
					})
				}
			}
		}
	}
}

func TestBoardDoneFailureEyebrow(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, tc := range []struct{ filter, want string }{
			{"", "DONE · 5 · 2 failed"},
			{"needs landing", "DONE · 1 · 1 failed"},
			{"Retry interrupted", "DONE · 1 · 1 failed"},
			{"Human rejected", "DONE · 1"},
			{"Completed implementation", "DONE · 1"},
		} {
			t.Run(fmt.Sprintf("dark=%t/filter=%s", dark, tc.filter), func(t *testing.T) {
				m := tuiTestModel(t, &uzicli.FakeClient{}, "")
				m.dark, m.pal = dark, newPalette(dark)
				m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: doneFailureRows()})
				m.board.filter = tc.filter
				out := m.View().Content
				var header string
				for _, line := range strings.Split(out, "\n") {
					if strings.Contains(stripANSI(line), "DONE ·") {
						header = line
						break
					}
				}
				if strings.TrimSpace(stripANSI(header)) != tc.want {
					t.Errorf("header = %q, want %q", stripANSI(header), tc.want)
				}
				if strings.Contains(tc.want, "failed") {
					suffix := strings.Split(tc.want, " · ")[2]
					if !strings.Contains(header, paintSeg(m.pal.alarm, nil, false, suffix)) {
						t.Errorf("failure suffix must have alarm SGR: %q", header)
					}
				} else if strings.Contains(header, "failed") {
					t.Errorf("zero failures must omit suffix: %q", header)
				}
			})
		}
	}
}

func TestBoardDoneFailureAscii(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	rows := doneFailureRows()
	rows[0].JudgeVerdict, rows[0].JudgeTodoCount = sp("issues"), 2
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: rows})
	truecolor := m.View().Content
	m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	var buf bytes.Buffer
	writer := colorprofile.NewWriter(&buf, nil)
	writer.Profile = colorprofile.Ascii
	if _, err := writer.Write([]byte(m.View().Content)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "38;") || strings.Contains(out, "48;") || strings.Contains(out, "\x1b]8;") {
		t.Fatalf("Ascii retained colour or OSC-8: %q", out)
	}
	if stripANSI(out) != stripANSI(truecolor) {
		t.Error("Ascii changed the board's text or glyphs")
	}
	out = stripANSI(out)
	for _, want := range []string{"DONE · 5 · 2 failed", "✗", "needs landing", "stopped", "✓", "⚖ issues · 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("Ascii lost %q", want)
		}
	}
}
