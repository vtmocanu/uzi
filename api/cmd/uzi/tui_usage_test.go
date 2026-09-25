package main

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func usageReply(t *testing.T, m tuiModel, msg selfUsageMsg) tuiModel {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(tuiModel)
}

func TestBoardUsageCost(t *testing.T) {
	tests := []struct {
		name  string
		usage apitypes.SelfUsageDTO
		want  string
		ok    bool
	}{
		{"metered", apitypes.SelfUsageDTO{Last7Days: apitypes.UsageDTO{CostUSD: 8.6}}, "$9 7d", true},
		{"rounded zero", apitypes.SelfUsageDTO{Last7Days: apitypes.UsageDTO{CostUSD: 0.49}}, "", false},
		{"subscription zero", apitypes.SelfUsageDTO{Last7SubscriptionRunCount: 1}, "$0+ 7d", true},
		{"unreported", apitypes.SelfUsageDTO{Last7Days: apitypes.UsageDTO{CostUSD: 3.2}, Last7UnreportedRunCount: 1}, "$3+ 7d", true},
		{"both", apitypes.SelfUsageDTO{Last7Days: apitypes.UsageDTO{CostUSD: 3.2}, Last7SubscriptionRunCount: 1, Last7UnreportedRunCount: 1}, "$3+ 7d", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := boardUsageCost(tc.usage)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("boardUsageCost = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestTUIBoardUsageSummaryServerValueAndStates(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	// The board's 200-row list has a deliberately divergent sum.
	m.board.runs = make([]apitypes.RunListItemDTO, 200)
	for i := range m.board.runs {
		m.board.runs[i].Usage = &apitypes.UsageDTO{CostUSD: 100}
	}
	if got := stripANSI(m.boardSummary()); strings.Contains(got, "$") {
		t.Fatalf("pending initial fetch showed cost: %q", got)
	}
	m = usageReply(t, m, selfUsageMsg{reqID: m.selfUsageReqID, usage: apitypes.SelfUsageDTO{
		Last7Days: apitypes.UsageDTO{CostUSD: 12.6},
	}})
	got := m.boardSummary()
	want := lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(m.renderer.Plain("$13 7d", 40))
	if !strings.Contains(got, want) || strings.Contains(stripANSI(got), "$20000") {
		t.Fatalf("summary should use server value in tungsten: %q", got)
	}
	m = usageReply(t, m, selfUsageMsg{reqID: m.selfUsageReqID, err: errors.New("offline")})
	if got := stripANSI(m.boardSummary()); strings.Contains(got, "$") {
		t.Fatalf("current fetch error should hide previous value: %q", got)
	}
	m = usageReply(t, m, selfUsageMsg{reqID: m.selfUsageReqID, usage: apitypes.SelfUsageDTO{
		Last7SubscriptionRunCount: 1,
	}})
	if got := stripANSI(m.boardSummary()); !strings.Contains(got, "$0+ 7d") {
		t.Fatalf("marked zero missing: %q", got)
	}
	if got := stripANSI(m.boardFooter()); !strings.Contains(got, "+ partial") {
		t.Fatalf("legend missing: %q", got)
	}
	m.board.admin = true
	if got := stripANSI(m.boardSummary()); strings.Contains(got, "$") {
		t.Fatalf("admin summary showed own usage: %q", got)
	}
}

func TestTUIBoardMarkedCostLegendRenderedAtOrdinaryWidths(t *testing.T) {
	withVersion(t, "v0.63.0")
	for _, tc := range []struct {
		name  string
		usage apitypes.SelfUsageDTO
	}{
		{"subscription", apitypes.SelfUsageDTO{Last7SubscriptionRunCount: 1}},
		{"unreported", apitypes.SelfUsageDTO{Last7UnreportedRunCount: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m = usageReply(t, m, selfUsageMsg{reqID: m.selfUsageReqID, usage: tc.usage})
			m.showVersion = true
			for _, width := range []int{80, 100, 120} {
				m.width = width
				footer := stripANSI(m.boardFooterLine())
				view := stripANSI(m.View().Content)
				if !strings.Contains(view, "$0+ 7d") {
					t.Errorf("width %d board did not show marked cost: %q", width, view)
				}
				renderedFooter := view[strings.LastIndex(view, "\n")+1:]
				for _, got := range []string{footer, renderedFooter} {
					if !strings.Contains(got, "+ partial") ||
						!strings.Contains(got, "enter/→ open") || !strings.Contains(got, "/ filter") ||
						!strings.Contains(got, "q quit") || !strings.Contains(got, "v0.63.0") {
						t.Errorf("width %d footer lost cost cue, key hint, or version: %q", width, got)
					}
					if !strings.Contains(got, "a factory") {
						t.Errorf("width %d footer lost factory hint: %q", width, got)
					}
					if width >= 100 && (!strings.Contains(got, "r refresh") || !strings.Contains(got, "? keys")) {
						t.Errorf("width %d footer lost refresh or help hint: %q", width, got)
					}
					if width >= 120 && !strings.Contains(got, "h fold done") {
						t.Errorf("width %d footer lost fold-done hint: %q", width, got)
					}
					if visualWidth(got) > width {
						t.Errorf("width %d footer overflowed: %q", width, got)
					}
				}
			}
		})
	}
}

func TestTUIBoardHelpExplainsMarkedCost(t *testing.T) {
	if got := strings.Join(helpLines(viewBoard), "\n"); !strings.Contains(got, "+ after 7d cost means subscription/unreported costs excluded") {
		t.Fatalf("board help missing marked-cost explanation: %q", got)
	}
}

func TestTUISelfUsageReplyOrdering(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	old := m.selfUsageReqID
	_ = (&m).startSelfUsageReq()
	current := m.selfUsageReqID
	m = usageReply(t, m, selfUsageMsg{reqID: current, usage: apitypes.SelfUsageDTO{Last7Days: apitypes.UsageDTO{CostUSD: 5}}})
	m = usageReply(t, m, selfUsageMsg{reqID: old, err: errors.New("stale")})
	if got := stripANSI(m.boardSummary()); !strings.Contains(got, "$5 7d") {
		t.Fatalf("stale error overwrote newer success: %q", got)
	}
	_ = (&m).startSelfUsageReq()
	current = m.selfUsageReqID
	m = usageReply(t, m, selfUsageMsg{reqID: current, err: errors.New("current")})
	m = usageReply(t, m, selfUsageMsg{reqID: current - 1, usage: apitypes.SelfUsageDTO{Last7Days: apitypes.UsageDTO{CostUSD: 99}}})
	if got := stripANSI(m.boardSummary()); strings.Contains(got, "$") {
		t.Fatalf("stale success overwrote newer error: %q", got)
	}
}

func TestTUISelfUsageCadence(t *testing.T) {
	fake := &uzicli.FakeClient{SelfUsageV: apitypes.SelfUsageDTO{Last7Days: apitypes.UsageDTO{CostUSD: 7}}}
	m := tuiTestModel(t, fake, "")
	assertFetch := func(cmd tea.Cmd, wantID uint64) {
		t.Helper()
		msg, ok := cmd().(selfUsageMsg)
		if !ok || msg.reqID != wantID || msg.usage.Last7Days.CostUSD != 7 {
			t.Fatalf("usage command yielded %#v, want request %d", msg, wantID)
		}
	}
	assertFetch(m.initCmds()[4], 1)
	// When idle, the 2s tick polls runs only.
	m.board.waitID = 0
	next, cmd := m.Update(boardTickMsg{gen: m.board.tickGen})
	m = next.(tuiModel)
	if _, ok := cmd().(boardRunsMsg); !ok {
		t.Fatalf("2s board tick command was not a runs fetch")
	}
	next, cmd = m.Update(stripTickMsg{})
	m = next.(tuiModel)
	if m.selfUsageReqID != 2 {
		t.Fatalf("strip id = %d, want 2", m.selfUsageReqID)
	}
	assertFetch(cmd().(tea.BatchMsg)[2], 2)
	next, cmd = m.handleKey(keyRefresh)
	m = next.(tuiModel)
	if m.selfUsageReqID != 3 {
		t.Fatalf("refresh id = %d, want 3", m.selfUsageReqID)
	}
	assertFetch(cmd().(tea.BatchMsg)[3], 3)
	next, cmd = m.handleKey(keyAdmin)
	m = next.(tuiModel)
	if m.selfUsageReqID != 4 {
		t.Fatalf("board toggle id = %d, want 4", m.selfUsageReqID)
	}
	assertFetch(cmd().(tea.BatchMsg)[1], 4)
}
