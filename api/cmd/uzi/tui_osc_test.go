package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// oscLink must emit the exact OSC-8 delimiter bytes around the text, pass the bare text
// through when there is no url to link to, and strip control bytes from the url (so a
// hostile forge URL cannot inject its own escape sequence).
func TestOscLink(t *testing.T) {
	if got, want := oscLink("https://example.com/x", "#42"), "\x1b]8;;https://example.com/x\x1b\\#42\x1b]8;;\x1b\\"; got != want {
		t.Errorf("oscLink normal: got %q, want %q", got, want)
	}
	if got := oscLink("", "#42"); got != "#42" {
		t.Errorf("oscLink empty url: got %q, want bare %q", got, "#42")
	}
	// Control bytes in the url are stripped (sanitizeTTY), so the emitted target is clean and
	// the ST terminator cannot be forged. Here only the ESC and BEL are removed, leaving a
	// clean "https://ex.com" as the link target.
	got := oscLink("https://e\x1b\x07x.com", "#7")
	if strings.Contains(got, "\x07") {
		t.Errorf("oscLink did not strip BEL from url: %q", got)
	}
	// The only ESC bytes left must be the two OSC-8 delimiters, never one from the url.
	if !strings.HasPrefix(got, "\x1b]8;;https://ex.com\x1b\\") {
		t.Errorf("oscLink target not sanitized to https://ex.com: %q", got)
	}

	// A URL is single-line printable by construction, so the OSC-8-strict filter drops
	// \n and \t too (which sanitizeTTY spares) — a raw newline in the target would forge
	// a row in the frame. The visible text is untouched.
	nl := oscLink("https://x\n\tY\x07", "#1")
	if strings.Contains(nl, "\n") {
		t.Errorf("oscLink did not strip the newline from the url: %q", nl)
	}
	if strings.Contains(nl, "\t") {
		t.Errorf("oscLink did not strip the tab from the url: %q", nl)
	}
	if strings.Contains(nl, "\x07") {
		t.Errorf("oscLink did not strip the BEL from the url: %q", nl)
	}
	if !strings.Contains(nl, "#1") {
		t.Errorf("oscLink dropped the visible text: %q", nl)
	}
}

// The board's #<iid> link degrades to plain text when the terminal profile can't render
// OSC-8 (Ascii / NO_COLOR), and emits the hyperlink at ANSI or richer.
func TestIssueLinkDegradesByProfile(t *testing.T) {
	iid := int64(519)
	url := "https://github.com/vtmocanu/uzi/issues/519"
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "abcd1234-1111", Kind: "issue", Status: "running",
			IssueTitle: "Wire the issue link", IssueIID: &iid, IssueWebURL: &url}},
	}
	fake := &uzicli.FakeClient{Runs: runs}

	// Default test profile is TrueColor → link emitted, with the issue URL as target.
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	out := m.View().Content
	if !strings.Contains(out, "\x1b]8") {
		t.Errorf("default (TrueColor) board should emit an OSC-8 link; frame had none:\n%s", out)
	}
	if !strings.Contains(out, url) {
		t.Errorf("default board OSC-8 link should carry the issue URL %q; frame:\n%s", url, out)
	}

	// Ascii profile → no OSC-8 escape at all (plain #519 text instead).
	next, _ = m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	degraded := m.View().Content
	if strings.Contains(degraded, "\x1b]8") {
		t.Errorf("Ascii board must NOT emit an OSC-8 link; frame:\n%s", degraded)
	}
	if !strings.Contains(degraded, "#519") {
		t.Errorf("Ascii board should still show the plain #519 id; frame:\n%s", degraded)
	}

	// Back to TrueColor via an explicit message → link re-emitted.
	next, _ = m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m = next.(tuiModel)
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	if !strings.Contains(m.View().Content, "\x1b]8") {
		t.Errorf("TrueColor board should re-emit the OSC-8 link after degrade")
	}
}

// A run with an IssueIID renders #<iid> in both the board row and the detail header; a
// run with a nil IssueIID renders no #.
func TestIssueIDRendersInBoardAndDetail(t *testing.T) {
	iid := int64(463)
	url := "https://github.com/vtmocanu/uzi/issues/463"
	withIssue := apitypes.RunDTO{ID: "aaaa1111-2222", Kind: "issue", Status: "running",
		IssueTitle: "Port the judge", IssueIID: &iid, IssueWebURL: &url}
	noIssue := apitypes.RunDTO{ID: "bbbb3333-4444", Kind: "ci_fix", Status: "running",
		IssueTitle: "Fix flaky pipeline"}
	runs := []apitypes.RunListItemDTO{{RunDTO: withIssue}, {RunDTO: noIssue}}
	fake := &uzicli.FakeClient{Runs: runs}

	board := tuiTestModel(t, fake, "")
	next, _ := board.Update(boardRunsMsg{reqID: board.board.waitID, runs: runs})
	board = next.(tuiModel)
	out := board.View().Content
	if !strings.Contains(out, "#463") {
		t.Errorf("board should render #463 for the issue run; frame:\n%s", out)
	}

	// Detail header for the issue run shows #463.
	detail := tuiTestModel(t, fake, withIssue.ID)
	detail = applyDetail(detail, withIssue, nil)
	if dout := detail.View().Content; !strings.Contains(dout, "#463") {
		t.Errorf("detail header should render #463 for the issue run; frame:\n%s", dout)
	}

	// Detail header for the nil-issue run shows no #.
	nd := tuiTestModel(t, fake, noIssue.ID)
	nd = applyDetail(nd, noIssue, nil)
	if dout := nd.View().Content; strings.Contains(dout, "#") {
		t.Errorf("nil-issue detail header must not render a #; frame:\n%s", dout)
	}
}
