package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func checkPtr(s string) *string { return &s }

// Match each state on its own run row. A whole-board substring search for "checking"
// would also match "xchecking" and would pass after the checking state disappeared.
var demoCheckRows = []struct{ id, glyph, word string }{
	{"c0de0005", "‖", "compl blocked"},
	{"b2c3d4e5", "⚑", "xchk revise"},
	{"c0de0006", "⚑", "xchk blocked"},
	{"c0de0007", "⚑", "xchk n/a"},
	{"d0e1f2a3", "◈", "xchecking"},
	{"c0de0003", "◎", "checking"},
	{"c0de0004", "⊕", "reworking"},
	{"c0de0001", "◈", "xchecking"},
}

func assertDemoCheckRows(t *testing.T, board string) {
	t.Helper()
	for _, want := range demoCheckRows {
		found := false
		for _, line := range strings.Split(board, "\n") {
			fields := strings.Fields(line)
			for i, field := range fields {
				if field != want.id {
					continue
				}
				found = true
				words := strings.Fields(want.word)
				if i == 0 || !strings.Contains(fields[i-1], want.glyph) || i+len(words) >= len(fields) ||
					strings.Join(fields[i+1:i+1+len(words)], " ") != want.word {
					t.Errorf("row %s lacks exact glyph+word %q %q: %q", want.id, want.glyph, want.word, line)
				}
			}
		}
		if !found {
			t.Errorf("demo board lacks run %s:\n%s", want.id, board)
		}
	}
}

func TestTUICompletionAndCrossCheckTokens(t *testing.T) {
	p := newPalette(true)
	cases := []struct {
		name, glyph, word, tone string
		r                       apitypes.RunDTO
	}{
		{"checking", "◎", "checking", "check", apitypes.RunDTO{Status: "running", CompletionPhase: "checking"}},
		{"reworking", "⊕", "reworking", "warn", apitypes.RunDTO{Status: "running", CompletionPhase: "reworking"}},
		{"blocked", "‖", "compl blocked", "danger", apitypes.RunDTO{Status: statusPaused, CompletionPhase: "blocked"}},
		{"plan live", "◈", "xchecking", "check", apitypes.RunDTO{Status: "running", CrossCheckActive: checkPtr("plan")}},
		{"code live", "◈", "xchecking", "check", apitypes.RunDTO{Status: "running", CrossCheckActive: checkPtr("code")}},
		{"plan revision", "⚑", "xchk revise", "warn", apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("revise")}},
		{"exhausted", "⚑", "xchk revise", "warn", apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("revisions_exhausted")}},
		{"plan blocked", "⚑", "xchk blocked", "danger", apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("block")}},
		{"plan unavailable", "⚑", "xchk n/a", "warn", apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("worker_unsupported")}},
		{"completion beats code", "◎", "checking", "check", apitypes.RunDTO{Status: "running", CompletionPhase: "checking", CrossCheckActive: checkPtr("code")}},
		{"gate beats active", "⚑", "xchk revise", "warn", apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("revise"), CrossCheckActive: checkPtr("plan")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := p.runStateToken(tc.r, tc.r.IsRevising)
			if tok.glyph != tc.glyph || tok.word != tc.word {
				t.Fatalf("token = (%q, %q), want (%q, %q)", tok.glyph, tok.word, tc.glyph, tc.word)
			}
			want := p.indigo
			switch tc.tone {
			case "warn":
				want = p.amber
			case "danger":
				want = p.alarm
			}
			if tok.color != want {
				t.Errorf("tone = %v, want %v", tok.color, want)
			}
			if got := runStateWord(apitypes.RunListItemDTO{RunDTO: tc.r}); got != tc.word {
				t.Errorf("filter word = %q, want %q", got, tc.word)
			}
			if n := lipgloss.Width(tok.glyph); n != 1 {
				t.Errorf("glyph %q is %d cells, want one", tok.glyph, n)
			}
			if n := lipgloss.Width(tok.word); n >= boardStatusWordWidth {
				t.Errorf("word %q is %d cells, needs one spare in %d", tok.word, n, boardStatusWordWidth)
			}
		})
	}
}

func TestTUICompletionAndCrossCheckPrecedence(t *testing.T) {
	p := newPalette(true)
	for _, tc := range []struct {
		name, want string
		r          apitypes.RunDTO
	}{
		{"human stop", "stopped", apitypes.RunDTO{Status: "failed", StopKind: checkPtr("plan_rejected"), CompletionPhase: "blocked"}},
		{"stalled health", "stalled", apitypes.RunDTO{Status: "running", Health: "stalled", CompletionPhase: "checking"}},
		{"account cooldown", "account cooldown", apitypes.RunDTO{Status: "running", Health: "waiting_account_cooldown", CompletionPhase: "checking"}},
		{"revision in flight", "revising", apitypes.RunDTO{Status: "awaiting_approval", IsRevising: true, PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("revise")}},
		{"unbacked gate reason", "plan gate", apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckGateReason: checkPtr("revise")}},
		{"completion question stays input", "needs input", apitypes.RunDTO{Status: "awaiting_input", CompletionPhase: "blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.runStateToken(tc.r, tc.r.IsRevising).word; got != tc.want {
				t.Errorf("word = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTUICompletionAndCrossCheckViewTones(t *testing.T) {
	now := time.Now()
	rows := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "neutral-1", Kind: "issue", Status: "running", CreatedAt: now}},
		{RunDTO: apitypes.RunDTO{ID: "gate-1", Kind: "issue", Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("revise"), CreatedAt: now.Add(-time.Second)}},
		{RunDTO: apitypes.RunDTO{ID: "gate-2", Kind: "issue", Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("block"), CreatedAt: now.Add(-2 * time.Second)}},
		{RunDTO: apitypes.RunDTO{ID: "gate-3", Kind: "issue", Status: "awaiting_approval", PlanCrossCheckRequired: true, PlanCrossCheckGateReason: checkPtr("worker_unsupported"), CreatedAt: now.Add(-3 * time.Second)}},
		{RunDTO: apitypes.RunDTO{ID: "xcheck-1", Kind: "issue", Status: "running", CrossCheckActive: checkPtr("plan"), CreatedAt: now.Add(-4 * time.Second)}},
		{RunDTO: apitypes.RunDTO{ID: "check-1", Kind: "issue", Status: "running", CompletionPhase: "checking", CreatedAt: now.Add(-time.Minute)}},
		{RunDTO: apitypes.RunDTO{ID: "rework-1", Kind: "issue", Status: "running", CompletionPhase: "reworking", CreatedAt: now.Add(-2 * time.Minute)}},
		{RunDTO: apitypes.RunDTO{ID: "block-1", Kind: "issue", Status: statusPaused, CompletionPhase: "blocked", CreatedAt: now.Add(-3 * time.Minute)}},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: rows}, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: rows})
	m = next.(tuiModel)
	view := m.View().Content
	for _, tc := range []struct {
		glyph string
		ink   string
	}{
		{"◎", paintSeg(m.pal.indigo, nil, false, "◎")},
		{"◈", paintSeg(m.pal.indigo, nil, false, "◈")},
		{"⊕", paintSeg(m.pal.amber, nil, false, "⊕")},
		{"‖", paintSeg(m.pal.alarm, nil, false, "‖")},
		{"⚑ revise/unavailable", paintSeg(m.pal.amber, nil, false, "⚑")},
		{"⚑ blocked", paintSeg(m.pal.alarm, nil, false, "⚑")},
	} {
		if !strings.Contains(view, tc.ink) {
			t.Errorf("View lacks the %q glyph in its expected SGR tone:\n%q", tc.glyph, view)
		}
	}
}

func TestTUICompletionBlockedWordFitsNarrowBoard(t *testing.T) {
	row := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
		ID: "blocked-1", Kind: "issue", Status: statusPaused, CompletionPhase: "blocked", IssueTitle: "A completion hold",
	}}
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{row}}, "")
	m.width, m.height = 60, 20
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{row}})
	m = next.(tuiModel)
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "compl blocked") {
		t.Fatalf("13-cell status word was truncated on a narrow board:\n%s", view)
	}
}

func TestTUICompletionBlockedNeedsYouBandAndSummary(t *testing.T) {
	blocked := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
		ID: "blocked-1", Kind: "issue", Status: statusPaused, CompletionPhase: "blocked",
		HoldReason: checkPtr("completion_blocked"), IssueTitle: "Owner decision needed",
	}}
	ordinary := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
		ID: "paused-1", Kind: "issue", Status: statusPaused, IssueTitle: "Owner paused it",
	}}
	if got := runBandOf(blocked); got != bandNeedsYou {
		t.Errorf("completion-blocked band = %d, want NEEDS YOU", got)
	}
	if got := runBandOf(ordinary); got != bandFloor {
		t.Errorf("ordinary paused band = %d, want ON THE FLOOR", got)
	}
	rows := []apitypes.RunListItemDTO{blocked, ordinary}
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: rows}, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: rows})
	m = next.(tuiModel)
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "NEEDS YOU · 1") || !strings.Contains(view, "ON THE FLOOR · 1") {
		t.Errorf("band counts did not split the owner decision from an ordinary pause:\n%s", view)
	}
	if summary := stripANSI(m.boardSummary()); !strings.Contains(summary, "‖ 1") || strings.Contains(summary, "⚑ 1") {
		t.Errorf("board summary = %q, want a distinct completion-block count", summary)
	}
}

func TestTUICompletionAndCrossCheckAsciiBoardAndDetail(t *testing.T) {
	run := apitypes.RunDTO{ID: "check-001", Kind: "issue", Status: "running", CompletionPhase: "reworking",
		CrossCheckActive: checkPtr("code"), CompletionUnmet: []string{"m3\x1b[2J\u202e"}, CompletionAttempts: 2}
	client := newDemoClient()
	m := tuiTestModel(t, client, "")
	m.height = 80
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: client.Runs})
	m = next.(tuiModel)
	next, _ = m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	board := stripANSI(m.renderBoard())
	assertDemoCheckRows(t, board)

	d := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	d = applyDetail(d, run, nil)
	rawDetail := d.renderDetail()
	detail := stripANSI(rawDetail)
	for _, want := range []string{"Reworking unmet milestones", "Code cross-check in progress", "attempt 2"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(rawDetail, "\x1b[2J") || strings.Contains(rawDetail, "\u202e") {
		t.Errorf("hostile milestone id reached the detail:\n%q", rawDetail)
	}
	if len(d.checkDetailLines()) != 2 {
		t.Errorf("detail fact lines = %d, want completion plus code", len(d.checkDetailLines()))
	}
	d.width = 80
	d.detail.run.CompletionUnmet = []string{strings.Repeat("x", 100), strings.Repeat("y", 100)}
	if !strings.Contains(stripANSI(d.checkDetailLines()[0]), "attempt 2") {
		t.Fatal("long untrusted unmet ids hid the attempt count on a narrow detail")
	}

	// A metadata refresh replaces the live presence field; it does not reuse a stale board row.
	run.CrossCheckActive = nil
	d.detail.applyMeta(run)
	if strings.Contains(stripANSI(d.renderDetail()), "Code cross-check in progress") {
		t.Fatal("settled code cross-check persisted after detail metadata refresh")
	}
	if got := renderDetail(t, apitypes.RunDTO{ID: run.ID, CrossCheckActive: checkPtr("plan")}); !strings.Contains(got, "CROSS_CHECK") || !strings.Contains(got, "Plan cross-check in progress") {
		t.Errorf("uzi run get omits live plan check:\n%s", got)
	}
}

func TestTUIDemoHasEveryCheckState(t *testing.T) {
	client := newDemoClient()
	m := tuiTestModel(t, client, "")
	m.height = 80
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: client.Runs})
	m = next.(tuiModel)
	board := stripANSI(m.renderBoard())
	assertDemoCheckRows(t, board)
	for _, r := range client.Runs {
		if r.CompletionPhase == "" && r.CrossCheckActive == nil && r.PlanCrossCheckGateReason == nil {
			continue
		}
		d := tuiTestModel(t, client, r.ID)
		d = applyDetail(d, client.RunByID[r.ID], nil)
		out := stripANSI(d.renderDetail())
		if r.CompletionPhase != "" && !strings.Contains(out, completionPhaseLabel(r.CompletionPhase)) {
			t.Errorf("demo detail %s lacks completion phase:\n%s", r.ID, out)
		}
		if label := crossCheckActiveLabel(r.CrossCheckActive); label != "" && !strings.Contains(out, label) {
			t.Errorf("demo detail %s lacks active cross-check:\n%s", r.ID, out)
		}
		if label := planCrossCheckGateText(r.RunDTO); label != "" && !strings.Contains(out, label) {
			t.Errorf("demo detail %s lacks gate reason:\n%s", r.ID, out)
		}
	}
}
