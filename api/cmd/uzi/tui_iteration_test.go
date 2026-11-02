package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestIterationTUIBoardAttention(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	r := iterationFixture()
	r.Health = "slow"
	row := apitypes.RunListItemDTO{RunDTO: r}
	for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii} {
		m.profile = profile
		token := m.pal.runStateToken(r, false)
		if runBandOf(row) != bandNeedsYou || runStateWord(row) != "iter limit" || token.word != "iter limit" || token.color != m.pal.amber {
			t.Fatal("iteration must have typed warning and attention band")
		}
		if !m.boardShowSecondLine(row) {
			t.Fatal("iteration actions hidden")
		}
		for _, width := range []int{12, 40, 100} {
			m.width = width
			out := stripANSI(m.boardSecondLine(row))
			if visualWidth(out) > width || strings.ContainsAny(out, "\r\n") {
				t.Fatalf("unsafe width %d: %q", width, out)
			}
		}
		m.width = 100
		requireWorkerCopy(t, stripANSI(m.boardSecondLine(row)), "Resume/Cancel", "Iteration limit reached")
	}
	m.board.runs = []apitypes.RunListItemDTO{row}
	summary := stripANSI(m.boardSummary())
	if !strings.Contains(summary, "⊘ 1") || strings.Contains(summary, "▲") {
		t.Fatalf("wrong attention summary: %q", summary)
	}
	r.Status = "queued"
	if m.pal.runStateToken(r, false).word == "iter limit" {
		t.Fatal("retained reason rendered as active hold")
	}
}

func TestIterationTUIDetail(t *testing.T) {
	for _, spent := range []bool{false, true} {
		r := iterationFixture()
		r.BudgetTotalSeconds = intPtr(3600)
		r.BudgetUsedSeconds = intPtr(20)
		if spent {
			r.BudgetUsedSeconds = intPtr(3600)
		}
		m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
		m = applyDetail(m, r, nil)
		m.width = 100
		out := stripANSI(m.renderDetail())
		requireWorkerCopy(t, out, "Iteration limit reached", "Resume: uzi run resume r1", "Cancel: uzi run cancel r1")
		if strings.Contains(out, "paused by you") || strings.Contains(out, "uzi run scope") || strings.Contains(out, "uzi run stop") {
			t.Fatalf("wrong iteration action: %s", out)
		}
		if strings.Contains(out, "uzi run extend") != spent {
			t.Fatalf("wrong wall guidance: %s", out)
		}
		// Extra wrapped rows must reduce the transcript's available height.
		wide := m.transcriptBudgetWithoutCard()
		m.width = 24
		if m.transcriptBudgetWithoutCard() >= wide {
			t.Fatal("wrapped iteration copy not charged to transcript")
		}
	}
}
