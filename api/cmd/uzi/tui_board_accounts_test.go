package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestBoardAccountDefaultPresentation(t *testing.T) {
	for _, tc := range []struct {
		name, harness, label, want string
		count                      int
		deleted, admin             bool
	}{
		{"Claude default", "claude", "default", "claude", 1, false, false},
		{"legacy Claude harness", "", "default", "claude", 1, false, false},
		{"Codex default", "codex", "default", "codex", 1, false, false},
		{"multiple", "codex", "default", "default", 2, false, false},
		{"zero count", "codex", "default", "default", 0, false, false},
		{"deleted", "codex", "default", "default", 1, true, false},
		{"admin", "codex", "default", "default", 1, false, true},
		{"custom", "codex", "acct-a", "acct-a", 1, false, false},
		{"case sensitive", "codex", "Default", "Default", 1, false, false},
		{"raw equality", "codex", "default\u202e", "default", 1, false, false},
		{"hostile", "codex", "\x1b[2J\u202e\x07界界", "[2J界界", 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.board.admin = tc.admin
			m.tokenCount, m.codexCredentialCount = tc.count, tc.count
			r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Harness: tc.harness, AnthropicSecretLabel: sp(tc.label), CodexSecretLabel: sp(tc.label)}}
			if !tc.deleted {
				r.AnthropicSecretID, r.CodexSecretID = sp("claude-id"), sp("codex-id")
			}
			if got := strings.TrimSpace(stripANSI(m.boardCredSeg(r, nil, boardCredWidth))); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBoardAccountFrameWidths(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width = 160
	m.tokenCount, m.codexCredentialCount = 1, 1
	rows := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa", Harness: "claude", Status: "running", IssueTitle: "title-a", AnthropicSecretLabel: sp("default"), AnthropicSecretID: sp("claude-id"), Usage: &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 9}}},
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb", Harness: "codex", Status: "completed", IssueTitle: "title-b", CodexSecretLabel: sp("default"), CodexSecretID: sp("codex-id"), Usage: &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 10}}},
		{RunDTO: apitypes.RunDTO{ID: "cccccccc", Harness: "claude", Status: "queued", IssueTitle: "title-c"}},
	}
	cols := m.boardAccountCols(rows)
	if cols.credW != 6 || cols.costW != 3 {
		t.Fatalf("measured widths = %+v, want 6/3", cols)
	}
	titleAt := -1
	for i, r := range rows {
		row := stripANSI(m.boardRow(r, false, boardMarkerCols{}, cols))
		at := visualWidth(row[:strings.Index(row, r.IssueTitle)])
		if i == 0 {
			titleAt = at
		} else if at != titleAt {
			t.Fatalf("title offset = %d, want %d: %q", at, titleAt, row)
		}
		if i == 0 && !strings.Contains(row, "claude   $9  title-a") {
			t.Fatalf("account/cost gap did not compact: %q", row)
		}
		if i == 1 && !strings.Contains(row, "codex   $10  title-b") {
			t.Fatalf("cost digits did not align: %q", row)
		}
	}
	m.board.runs = rows
	view := stripANSI(m.View().Content)
	for _, r := range rows {
		found := false
		for _, line := range strings.Split(view, "\n") {
			if at := strings.Index(line, r.IssueTitle); at >= 0 {
				found = true
				if got := visualWidth(line[:at]); got != titleAt {
					t.Fatalf("production frame title offset = %d, want %d: %q", got, titleAt, line)
				}
			}
		}
		if !found {
			t.Fatalf("production frame omitted %s", r.IssueTitle)
		}
	}

	for _, wide := range []struct {
		label string
		width int
	}{
		{"日本語", 6},
		{"\u202e界界界界界界", 9},
	} {
		rows[0].AnthropicSecretLabel = sp(wide.label)
		cols = m.boardAccountCols(rows)
		if cols.credW != wide.width {
			t.Fatalf("wide label display width = %d, want %d", cols.credW, wide.width)
		}
		offset := -1
		for _, r := range rows {
			row := stripANSI(m.boardRow(r, false, boardMarkerCols{}, cols))
			at := visualWidth(row[:strings.Index(row, r.IssueTitle)])
			if offset < 0 {
				offset = at
			} else if at != offset {
				t.Errorf("wide label %q shifted title: %d != %d: %q", wide.label, at, offset, row)
			}
		}
	}
	rows[0].AnthropicSecretLabel = nil
	rows[0].Usage = nil
	rows[1].CodexSecretLabel = nil
	rows[1].Usage = nil
	if cols = m.boardAccountCols(rows); cols.credW != 0 || cols.costW != 0 {
		t.Fatalf("blank frame reserves content widths: %+v", cols)
	}
}

func TestBoardAccountConservativeThresholds(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.tokenCount, m.codexCredentialCount = 1, 1
	for _, tc := range []struct {
		width            int
		cost, mile, prog bool
		worker           int
	}{
		{83, false, false, false, 0}, {94, false, false, false, 0},
		{95, true, false, false, 0}, {100, true, false, false, 0},
		{110, true, false, false, 0}, {111, true, true, true, 0},
		{119, true, true, true, 0}, {120, true, true, true, 18},
	} {
		m.width = tc.width
		if m.boardShowCost() != tc.cost || m.boardShowMile() != tc.mile {
			t.Fatalf("width %d: cost/mile = %t/%t", tc.width, m.boardShowCost(), m.boardShowMile())
		}
		if got := m.boardShowProg(); got != tc.prog {
			t.Errorf("width %d: percentage visible = %t, want %t", tc.width, got, tc.prog)
		}
		if got := m.boardWorkerWidth(); got != tc.worker {
			t.Errorf("width %d: worker columns = %d, want %d", tc.width, got, tc.worker)
		}
	}
	// On the personal board milestones are the stricter limit. The admin's
	// owner column makes the title-room floor bind: 128 leaves 24 columns,
	// while 129 leaves the required 25, with milestones already visible.
	m.board.admin = true
	for _, tc := range []struct {
		width int
		prog  bool
	}{{128, false}, {129, true}} {
		m.width = tc.width
		if !m.boardShowMile() {
			t.Fatalf("admin width %d must show milestones", tc.width)
		}
		if got := m.boardShowProg(); got != tc.prog {
			t.Errorf("admin width %d: percentage visible = %t, want %t", tc.width, got, tc.prog)
		}
	}
}

func TestBoardAccountWidthsFollowRenderedWindow(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.tokenCount, m.codexCredentialCount = 140, 1, 1
	for _, entry := range []struct {
		id, title, label string
		cost             float64
	}{
		{"aaaaaaaa", "window-a", "a", 3},
		{"bbbbbbbb", "window-b", "b", 9},
		{"cccccccc", "window-c", "long-label", 9999},
	} {
		m.board.runs = append(m.board.runs, apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
			ID: entry.id, Harness: "claude", Status: "running", IssueTitle: entry.title,
			AnthropicSecretLabel: sp(entry.label), Usage: &apitypes.UsageDTO{CostStatus: "metered", CostUSD: entry.cost},
		}})
	}
	before := stripANSI(m.renderBoardBody(4, false))
	if strings.Contains(before, "window-c") || !strings.Contains(before, "a  $3  window-a") {
		t.Fatalf("off-screen widest account/cost affected first window: %q", before)
	}
	titleOffset := func(view, title string) int {
		t.Helper()
		for _, line := range strings.Split(view, "\n") {
			if at := strings.Index(line, title); at >= 0 {
				return visualWidth(line[:at])
			}
		}
		t.Fatalf("missing %s in frame %q", title, view)
		return 0
	}
	firstOffset := titleOffset(before, "window-b")
	m.board.cursor, m.board.scroll = 2, 2
	after := stripANSI(m.renderBoardBody(4, false))
	if !strings.Contains(after, "long-label  $9999  window-c") {
		t.Fatalf("wider scrolled row did not fit: %q", after)
	}
	if got := titleOffset(after, "window-b"); got <= firstOffset {
		t.Fatalf("title did not move with wider visible values: %d <= %d", got, firstOffset)
	}
}

func TestBoardLongTitleFillsJudgeAndWorkerBudget(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.tokenCount, m.codexCredentialCount = 1, 1
	iid := int64(777)
	r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
		ID: "aaaaaaaa", Harness: "claude", Status: "running", IssueIID: &iid,
		IssueTitle: strings.Repeat("X", 400), AnthropicSecretLabel: sp("default"), AnthropicSecretID: sp("claude-id"),
		Usage: &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 3},
	}, WorkerName: sp("worker-a"), JudgeVerdict: sp("issues"), JudgeTodoCount: 2}
	rows := []apitypes.RunListItemDTO{r}
	mc := m.boardMarkerCols(rows)
	marker := stripANSI(m.verdictMarker("issues", 2, mc.verdictW, mc.countW, false, nil))
	for _, width := range []int{120, 200} {
		m.width = width
		row := stripANSI(m.boardRow(r, false, mc, m.boardAccountCols(rows)))
		markerAt := strings.Index(row, marker)
		titleAt := strings.Index(row, "XXXX")
		if markerAt < 0 || titleAt < 0 {
			t.Fatalf("width %d: missing title or judge marker: %q", width, row)
		}
		titleEnd := visualWidth(strings.TrimRight(row[:markerAt], " "))
		wantEnd := width - 18 - mc.fullW - 2
		if titleEnd != wantEnd {
			t.Errorf("width %d: title ends at %d, want marker budget boundary %d", width, titleEnd, wantEnd)
		}
		titleWidth := titleEnd - visualWidth(row[:titleAt])
		if width == 200 && titleWidth <= 60 {
			t.Errorf("wide title still capped: rendered %d columns", titleWidth)
		}
		if visualWidth(row[:markerAt]) != width-18-mc.fullW {
			t.Errorf("width %d: judge marker moved", width)
		}
		workerAt := strings.Index(row, "worker-a")
		if workerAt < 0 || visualWidth(row[:workerAt]) != width-16 {
			t.Errorf("width %d: worker column moved: %q", width, row)
		}
		if visualWidth(row) != width {
			t.Errorf("width %d: row overflowed or underfilled: %d", width, visualWidth(row))
		}
	}
}
