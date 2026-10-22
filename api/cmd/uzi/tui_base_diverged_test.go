package main

import (
	"bytes"
	"fmt"
	"image/color"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Shared fixtures keep the review scenes on the tested shipped render paths.
func baseDivergedRows() []apitypes.RunListItemDTO {
	rows := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "reject01", Status: "failed", FailOrigin: sp("base_diverged"), IssueTitle: "Base advanced before implementation", IssueIID: ip(3200), Kind: "issue"}},
		{RunDTO: apitypes.RunDTO{ID: "reject02", Status: "failed", FailOrigin: sp("base_diverged"), LandingState: "needs_landing", IssueTitle: "Base advanced with saved commits", Kind: "issue"}},
		{RunDTO: apitypes.RunDTO{ID: "failure1", Status: "failed", FailOrigin: sp("agent_failure"), IssueTitle: "Implementation failed", Kind: "issue"}},
		{RunDTO: apitypes.RunDTO{ID: "stopped1", Status: "failed", FailOrigin: sp("base_diverged"), StopKind: sp("cancelled"), LandingState: "needs_landing", IssueTitle: "Owner cancelled implementation", Kind: "issue"}},
		{RunDTO: apitypes.RunDTO{ID: "stopped2", Status: "failed", FailOrigin: sp("base_diverged"), StopKind: sp("plan_rejected"), LandingState: "needs_landing", IssueTitle: "Owner declined plan", Kind: "issue"}},
	}
	return rows
}

func baseDivergedBoardModel(dark, selected bool) tuiModel {
	m := uxModel(&uzicli.FakeClient{}, "", dark)
	rows := append([]apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "active01", Status: "running", IssueTitle: "Active work"}}}, baseDivergedRows()...)
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: rows})
	if selected {
		m.board.cursor = 1
	}
	return m
}

func baseDivergedWorkerModel(dark bool) tuiModel {
	m := workerRenderModel(dark)
	m.height = 40
	m.board.runs = baseDivergedRows()
	m.board.runsAdmin = m.board.admin
	for _, r := range m.board.runs {
		m.workers.rows[0].w.ReportedRuns = append(m.workers.rows[0].w.ReportedRuns, apitypes.WorkerReportedRunDTO{RunID: r.ID, Phase: "running", ClaimGeneration: 1})
	}
	return m
}

func TestTUIBaseDivergedTokens(t *testing.T) {
	for _, dark := range []bool{false, true} {
		p := newPalette(dark)
		for i, r := range baseDivergedRows() {
			t.Run(fmt.Sprintf("dark=%t/%s", dark, r.ID), func(t *testing.T) {
				word, glyph, ink, failed := "rejected", "·", p.faintC, false
				if i == 2 {
					word, glyph, ink, failed = "failed", "✗", p.alarm, true
				}
				if i >= 3 {
					word = "stopped"
				}
				tok := p.runStateToken(r.RunDTO, r.IsRevising)
				if tok.word != word || tok.glyph != glyph || !reflect.DeepEqual(tok.color, ink) {
					t.Errorf("token = %+v, want %s %s color %v", tok, glyph, word, ink)
				}
				if got := runStateWord(r); got != word {
					t.Errorf("runStateWord = %q, want %q", got, word)
				}
				if got := doneRunFailed(r); got != failed {
					t.Errorf("doneRunFailed = %t, want %t", got, failed)
				}
			})
		}
		for _, origin := range []*string{nil, sp("")} {
			r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Status: "failed", FailOrigin: origin}}
			if tok := p.runStateToken(r.RunDTO, false); tok.word != "failed" || !reflect.DeepEqual(tok.color, p.alarm) || !doneRunFailed(r) {
				t.Errorf("missing origin must remain failed: %+v", tok)
			}
		}
		for _, status := range []string{"queued", "running", "completed", "cancelled"} {
			r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Status: status, FailOrigin: sp("base_diverged")}}
			if p.runStateToken(r.RunDTO, false).word == "rejected" || runStateWord(r) == "rejected" || doneRunFailed(r) {
				t.Errorf("nonfailed %s classified as rejected/failure", status)
			}
		}
	}
	if n := utf8.RuneCountInString("rejected"); n != 8 || n > boardStatusWordWidth || boardStatusWordWidth != 14 {
		t.Fatalf("rejected must fit the 14-cell word: %d/%d", n, boardStatusWordWidth)
	}
}

func TestTUIBaseDivergedBoard(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("dark=%t/selected=%t", dark, selected), func(t *testing.T) {
				m := baseDivergedBoardModel(dark, selected)
				out := m.View().Content
				line := boardAgeTestRow(t, out, "reject01")
				var bg color.Color
				if selected {
					bg = m.pal.selBg
				}
				for _, span := range []string{
					paintSeg(m.pal.faintC, bg, false, "▌"),
					paintSeg(m.pal.faintC, bg, false, "·"),
					paintSeg(m.pal.faintC, bg, false, padCell("rejected", boardStatusWordWidth)),
				} {
					if !strings.Contains(line, span) {
						t.Errorf("rejected row missing faint span %q in %q", span, line)
					}
				}
				if !strings.Contains(stripANSI(out), "DONE · 5 · 1 failed") {
					t.Error("DONE must count only agent failure")
				}
				m = key(m, "/")
				for _, ch := range "rejected" {
					m = key(m, string(ch))
				}
				visible := m.board.visible()
				if len(visible) != 2 || visible[0].ID != "reject01" || visible[1].ID != "reject02" {
					t.Errorf("/ rejected must match both rejection rows; got %d rows", len(visible))
				}
				m.board.filter = "failed"
				if got := len(m.board.visible()); got != 5 {
					t.Errorf("raw failed filter must still match all five: %d", got)
				}
			})
		}
	}
}

func TestTUIBaseDivergedDetailAndWorker(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for i, r := range baseDivergedRows() {
			m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
			m.dark, m.pal = dark, newPalette(dark)
			m = applyDetail(m, r.RunDTO, nil)
			want := "· rejected"
			if i == 2 {
				want = "✗ failed"
			}
			if i == 3 {
				want = "· stopped (cancelled)"
			}
			if i == 4 {
				want = "· stopped (plan rejected)"
			}
			if got := stripANSI(strings.Join(m.detailHeaderLines(), "\n")); !strings.Contains(got, want) {
				t.Errorf("%s detail missing %q: %s", r.ID, want, got)
			}
		}
		m := baseDivergedWorkerModel(dark)
		out := stripANSI(m.View().Content)
		for _, want := range []string{"run stage rejected", "run stage failed", "run stage stopped"} {
			if !strings.Contains(out, want) {
				t.Errorf("cached worker missing %q", want)
			}
		}
		m.board.runsAdmin = !m.board.admin
		out = stripANSI(m.View().Content)
		if strings.Contains(out, "run stage rejected") || !strings.Contains(out, "run stage ? (not cached)") {
			t.Error("worker used cache with wrong provenance")
		}
	}
}

func TestTUIBaseDivergedAscii(t *testing.T) {
	m := baseDivergedBoardModel(true, false)
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
		t.Fatalf("Ascii retained ANSI: %q", out)
	}
	if stripANSI(out) != stripANSI(truecolor) {
		t.Error("downgrade changed text")
	}
	out = stripANSI(out)
	for _, want := range []string{"▌· reject01  rejected", "▌· stopped1  stopped", "▌✗ failure1  failed", "DONE · 5 · 1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("Ascii missing distinct outcome %q", want)
		}
	}
}
