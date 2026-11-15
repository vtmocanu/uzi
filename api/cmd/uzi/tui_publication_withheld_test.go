package main

import (
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Issue #4257: a running run whose health is publication_withheld is a WARN state on the TUI token
// and the board counter. The token word is the short "pub withheld" (one spare cell in the board's
// status column); the CLI HEALTH row keeps the long displayHealth label.
func TestTUIPublicationWithheldIsAWarnToken(t *testing.T) {
	p := newPalette(true)
	run := apitypes.RunDTO{ID: "r1", Status: "running", Health: healthPublicationWithheld}

	glyph, word := stateGlyphWord(run.Status, run.Health, false, false, "", "")
	if glyph != "▲" || word != "pub withheld" {
		t.Fatalf("stateGlyphWord = (%q, %q), want (▲, pub withheld)", glyph, word)
	}
	tok := p.runStateToken(run, false)
	if tok.glyph != "▲" || tok.word != "pub withheld" {
		t.Fatalf("runStateToken = (%q, %q), want (▲, pub withheld), not running", tok.glyph, tok.word)
	}
	if tok.color != p.stall || p.stateColor(run.Status, run.Health, false, false, "") != p.stall {
		t.Errorf("colour = %v, want the stall colour %v", tok.color, p.stall)
	}
	if n := lipgloss.Width(tok.word); n >= boardStatusWordWidth {
		t.Errorf("word %q is %d cells, needs one spare in %d", tok.word, n, boardStatusWordWidth)
	}
	if got := runStateWord(apitypes.RunListItemDTO{RunDTO: run}); got != "pub withheld" {
		t.Errorf("filter word = %q, want pub withheld", got)
	}
	if g, w, tone := checkRunState(run, false); g != "" || w != "" || tone != "" {
		t.Errorf("checkRunState drew over the health warning: (%q, %q, %q)", g, w, tone)
	}
	// The lane dot stays calm: this is a publication fact, not evidence the speaker is unhealthy.
	if laneStalledHealth[healthPublicationWithheld] {
		t.Error("publication_withheld must not turn the active lane amber")
	}
}

func TestTUIPublicationWithheldCountsInBoardSummary(t *testing.T) {
	m := tuiModel{}
	m.pal = newPalette(true)
	m.board.runs = []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "r1", Status: "running", Health: healthPublicationWithheld}},
		{RunDTO: apitypes.RunDTO{ID: "r2", Status: "running", Health: "ok"}},
	}
	if got := stripANSI(m.boardSummary()); !strings.Contains(got, "▲ 1") {
		t.Fatalf("boardSummary = %q, want the ▲ warn counter at 1", got)
	}
}

func TestTUIPublicationWithheldCLIHealthRowKeepsLongLabel(t *testing.T) {
	if got := displayHealth(healthPublicationWithheld); got != "publication withheld" {
		t.Fatalf("displayHealth = %q, want the long label", got)
	}
	rows, _ := detailRows(t, apitypes.RunDTO{Status: "running", Health: healthPublicationWithheld})
	if got := rows["HEALTH"]; len(got) != 1 || !strings.HasPrefix(got[0], "publication withheld") {
		t.Fatalf("HEALTH rows = %q", got)
	}
}
