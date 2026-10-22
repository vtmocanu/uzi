package main

import (
	"bytes"
	"fmt"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Shared with uxlab: titles deliberately omit status words so search tests need the token.
func rejectedRows() []apitypes.RunListItemDTO {
	return []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "reject01", Kind: "issue", Status: "failed", FailOrigin: sp("base_diverged"), Health: "stalled", LandingState: "needs_landing", IssueIID: ip(51), IssueTitle: "Base moved during planning"}},
		{RunDTO: apitypes.RunDTO{ID: "failure1", Kind: "issue", Status: "failed", FailOrigin: sp("agent_failure"), IssueIID: ip(52), IssueTitle: "Implementation crashed"}},
		{RunDTO: apitypes.RunDTO{ID: "human001", Kind: "issue", Status: "failed", FailOrigin: sp("base_diverged"), StopKind: sp("plan_rejected"), Health: "stalled", LandingState: "needs_landing", IssueIID: ip(53), IssueTitle: "Owner declined the plan"}},
	}
}

func TestTUIRejectedTokenMatrix(t *testing.T) {
	for _, dark := range []bool{false, true} {
		p := newPalette(dark)
		for _, tc := range []struct {
			name, status                       string
			origin                             *string
			stop, health, landing, glyph, word string
			alarm, failed                      bool
		}{
			{"rejected", "failed", sp("base_diverged"), "", "", "", "·", "rejected", false, false},
			{"stale-health-and-landing", "failed", sp("base_diverged"), "", "stalled", "needs_landing", "·", "rejected", false, false},
			{"stale-health", "failed", sp("base_diverged"), "", "stalled", "", "·", "rejected", false, false},
			{"stale-landing", "failed", sp("base_diverged"), "", "", "needs_landing", "·", "rejected", false, false},
			{"agent-failure", "failed", sp("agent_failure"), "", "", "", "✗", "failed", true, true},
			{"missing-origin", "failed", nil, "", "", "", "✗", "failed", true, true},
			{"empty-origin", "failed", sp(""), "", "", "", "✗", "failed", true, true},
			{"other-origin", "failed", sp("future_origin"), "", "", "", "✗", "failed", true, true},
			{"human-cancel", "failed", sp("base_diverged"), "cancelled", "stalled", "needs_landing", "·", "stopped", false, false},
			{"human-plan", "failed", sp("base_diverged"), "plan_rejected", "stalled", "needs_landing", "·", "stopped", false, false},
			{"cancelled-status", "cancelled", sp("base_diverged"), "", "stalled", "needs_landing", "·", "stopped", false, false},
			{"completed-guard", "completed", sp("base_diverged"), "", "", "", "✓", "done", false, false},
			{"queued-guard", "queued", sp("base_diverged"), "", "", "", "·", "queued", false, false},
		} {
			t.Run(fmt.Sprintf("dark=%t/%s", dark, tc.name), func(t *testing.T) {
				r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Status: tc.status, FailOrigin: tc.origin, Health: tc.health, LandingState: tc.landing}}
				if tc.stop != "" {
					r.StopKind = sp(tc.stop)
				}
				tok := p.runStateToken(r.RunDTO, r.IsRevising)
				ink := p.faintC
				if tc.alarm {
					ink = p.alarm
				}
				if tok.glyph != tc.glyph || tok.word != tc.word || tok.color != ink {
					t.Errorf("token = %+v, want %s/%s/%v", tok, tc.glyph, tc.word, ink)
				}
				if word := runStateWord(r); word != tc.word || word != tok.word {
					t.Errorf("runStateWord = %q, want %q and token word %q", word, tc.word, tok.word)
				}
				if got := doneRunFailed(r); got != tc.failed {
					t.Errorf("doneRunFailed = %t, want %t", got, tc.failed)
				}
			})
		}
		// A stale fail origin must not turn active work into a rejection.
		r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Status: "running", FailOrigin: sp("base_diverged")}}
		tok := p.runStateToken(r.RunDTO, false)
		if tok.word != "running" || tok.glyph != "●" || tok.color != p.sage || runStateWord(r) != tok.word || doneRunFailed(r) {
			t.Errorf("active guard: token=%+v word=%q failed=%t", tok, runStateWord(r), doneRunFailed(r))
		}
	}
}

func TestTUIRejectedBoardInk(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("dark=%t/selected=%t", dark, selected), func(t *testing.T) {
				m := tuiTestModel(t, &uzicli.FakeClient{}, "")
				m.dark, m.pal = dark, newPalette(dark)
				rows := append([]apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "active01", Status: "running"}}}, rejectedRows()...)
				m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: rows})
				if selected {
					m.board.cursor = 1
				}
				row := boardAgeTestRow(t, m.View().Content, "reject01")
				var bg color.Color
				issueInk := m.pal.faintC
				if selected {
					bg, issueInk = m.pal.selBg, m.pal.tungsten
				}
				for label, span := range map[string]string{
					"spine": paintSeg(m.pal.faintC, bg, false, "▌"),
					"glyph": paintSeg(m.pal.faintC, bg, false, "·"),
					"state": paintSeg(m.pal.faintC, bg, false, padCell("rejected", boardStatusWordWidth)),
					"issue": paintSeg(issueInk, bg, selected, "#51"),
				} {
					if !strings.Contains(row, span) {
						t.Errorf("%s missing SGR span %q in %q", label, span, row)
					}
				}
			})
		}
	}
}

func TestTUIRejectedBoardEyebrowAndFilter(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.dark, m.pal = dark, newPalette(dark)
		m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: rejectedRows()})
		if header := strings.TrimSpace(lineWith(t, stripANSI(m.View().Content), "DONE ·")); header != "DONE · 3 · 1 failed" {
			t.Errorf("DONE eyebrow = %q", header)
		}
		m = key(m, "/")
		for _, ch := range "rejected" {
			m = key(m, string(ch))
		}
		rows := m.board.visible()
		if len(rows) != 1 || rows[0].ID != "reject01" {
			t.Fatalf("rejected filter did not isolate base divergence: %v", rows)
		}
		if header := strings.TrimSpace(lineWith(t, stripANSI(m.View().Content), "DONE ·")); header != "DONE · 1" {
			t.Errorf("filtered DONE eyebrow = %q", header)
		}
	}
}

func TestTUIRejectedDetailAndCachedWorker(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for i, r := range rejectedRows() {
			t.Run(fmt.Sprintf("dark=%t/%s", dark, r.ID), func(t *testing.T) {
				word, glyph := []string{"rejected", "failed", "stopped"}[i], []string{"·", "✗", "·"}[i]
				label := word
				if i == 2 {
					label = "stopped (plan rejected)"
				}
				m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
				m.dark, m.pal = dark, newPalette(dark)
				m = applyDetail(m, r.RunDTO, nil)
				ink := m.pal.faintC
				if i == 1 {
					ink = m.pal.alarm
				}
				header := strings.Split(m.View().Content, "\n")[0]
				if span := paintSeg(ink, nil, false, glyph+" "+label); !strings.Contains(header, span) {
					t.Errorf("detail header missing %q: %q", span, header)
				}
				w := workerRenderModel(dark)
				w.board.runs, w.board.runsAdmin = []apitypes.RunListItemDTO{r}, w.board.admin
				w.workers.rows[0].w.ReportedRuns = []apitypes.WorkerReportedRunDTO{{RunID: r.ID, Phase: "running"}}
				stage := lineWith(t, stripANSI(w.View().Content), "run stage")
				if !strings.Contains(stage, "worker phase running · run stage "+word+" · gen") {
					t.Errorf("matching cached run stage: %q", stage)
				}
			})
		}
	}
}

func TestTUIRejectedProfileDowngrade(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, dark := range []bool{false, true} {
		for _, profile := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.NoTTY} {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.dark, m.pal = dark, newPalette(dark)
			m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: rejectedRows()})
			m = step(m, tea.ColorProfileMsg{Profile: profile})
			var b bytes.Buffer
			w := colorprofile.NewWriter(&b, []string{"NO_COLOR=1"})
			w.Profile = profile
			if _, err := w.Write([]byte(m.View().Content)); err != nil {
				t.Fatal(err)
			}
			out := b.String()
			if strings.Contains(out, "38;") || strings.Contains(out, "48;") {
				t.Errorf("dark=%t profile=%v retained color: %q", dark, profile, out)
			}
			for _, tc := range []struct{ id, word string }{{"reject01", "rejected"}, {"failure1", "failed"}, {"human001", "stopped"}} {
				if row := boardAgeTestRow(t, stripANSI(out), tc.id); !strings.Contains(row, tc.word) {
					t.Errorf("dark=%t profile=%v lost %q: %q", dark, profile, tc.word, row)
				}
			}
		}
	}
}
