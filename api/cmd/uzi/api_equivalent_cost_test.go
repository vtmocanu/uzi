package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestSpendBasisFitsRail(t *testing.T) {
	for _, cost := range []float64{0, 9.55, 1187} {
		out := stripANSI(spendModel(t, &apitypes.UsageDTO{CostStatus: "metered", CostUSD: cost, InputTokens: 300, OutputTokens: 200}).renderSpend(0))
		if !strings.Contains(out, fmtCostCents(cost)) || !strings.Contains(out, "API-equivalent") {
			t.Fatalf("missing cost/basis: %q", out)
		}
		for _, line := range strings.Split(out, "\n") {
			if visualWidth(line) > laneRailWidth {
				t.Errorf("spend exceeds %d columns: %q", laneRailWidth, line)
			}
		}
	}
	if got := adminUsageCost(0, 0, 0); got != "$0.00 (API-equivalent)" {
		t.Errorf("complete aggregate basis: %q", got)
	}
}

func TestCostReadersAPIEstimate(t *testing.T) {
	for _, status := range []string{"metered", "subscription", "unreported", "", "future-v2"} {
		t.Run(status, func(t *testing.T) {
			u := &apitypes.UsageDTO{CostStatus: status, CostUSD: 7.89, InputTokens: 300, OutputTokens: 200}
			m := spendModel(t, u)
			board := tuiTestModel(t, &uzicli.FakeClient{}, "")
			readers := map[string]string{
				"board":   stripANSI(board.boardCostSeg(apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Usage: u}}, nil)),
				"header":  stripANSI(strings.Join(m.detailHeaderLines(), "\n")),
				"spend":   stripANSI(m.renderSpend(0)),
				"run get": renderDetail(t, apitypes.RunDTO{ID: "r", Kind: "issue", Usage: u}),
			}
			for name, out := range readers {
				if strings.Contains(out, "$") != (status == "metered") {
					t.Errorf("%s status %q: %s", name, status, out)
				}
				if status == "subscription" {
					want := "n/a"
					if name == "run get" {
						want = "no estimate"
					}
					if !strings.Contains(out, want) {
						t.Errorf("%s missing %q: %s", name, want, out)
					}
				}
			}
			if status == "metered" {
				for _, name := range []string{"spend", "run get"} {
					if !strings.Contains(readers[name], "API-equivalent") {
						t.Errorf("%s lacks estimate basis: %s", name, readers[name])
					}
				}
			}
		})
	}
	u := &apitypes.UsageDTO{CostStatus: "metered"}
	m := spendModel(t, u)
	for _, out := range []string{renderDetail(t, apitypes.RunDTO{ID: "r", Usage: u}), stripANSI(m.renderSpend(0)), stripANSI(strings.Join(m.detailHeaderLines(), "\n")), stripANSI(m.boardCostSeg(apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{Usage: u}}, nil))} {
		if !strings.Contains(out, "$0") {
			t.Errorf("genuine metered zero lost dollars: %s", out)
		}
	}
}
