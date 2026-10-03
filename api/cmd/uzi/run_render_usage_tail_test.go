package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func f64(v float64) *float64 { return &v }

// TestRenderRunDetailEstTail pins the EST. TAIL row of `uzi run get` (issue #2014): it sits
// beside COST, never replaces it, and an unpriced tail reads cost unknown, never a dollar figure.
func TestRenderRunDetailEstTail(t *testing.T) {
	priced := &apitypes.UsageTailDTO{
		InputTokens: 200_000, CacheReadTokens: 1_000_000, OutputTokens: 34_000,
		CostUSD: f64(12.34), CostStatus: "estimated", PriceTableVersion: "anthropic-standard-2026-10-03",
		Coverage: "partial", CoverageReasons: []string{"leg_not_closed", "brand_new\x1b[31m‮reason"},
	}
	out := renderDetail(t, apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "failed",
		Usage:              &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 4.20, InputTokens: 100},
		UsageEstimatedTail: priced,
	})
	for _, want := range []string{"COST", "$4.20", "EST. TAIL", "~$12.34 estimated (anthropic-standard-2026-10-03)", "1.20M in/34.0k out", "partial: leg not closed, brand_new"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b‮") {
		t.Errorf("raw control or bidi characters leaked into the human view:\n%q", out)
	}

	unpriced := &apitypes.UsageTailDTO{InputTokens: 10, OutputTokens: 5, CostStatus: "unpriced", Coverage: "partial", CoverageReasons: []string{"ordinal_gap"}}
	out = renderDetail(t, apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "failed", UsageEstimatedTail: unpriced})
	if !strings.Contains(out, "cost unknown (unpriced)") || strings.Contains(out, "$") {
		t.Errorf("unpriced tail must read cost unknown with no dollar figure:\n%s", out)
	}
}

// TestRenderRunDetailEstTailHiddenWhenZeroComplete: the all-zero complete tail every finished
// run has renders no row; a zero-token partial one still does.
func TestRenderRunDetailEstTailHiddenWhenZeroComplete(t *testing.T) {
	zero := &apitypes.UsageTailDTO{CostUSD: f64(0), CostStatus: "estimated", PriceTableVersion: "v", Coverage: "complete"}
	out := renderDetail(t, apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "completed", UsageEstimatedTail: zero})
	if strings.Contains(out, "EST. TAIL") {
		t.Errorf("zero complete tail must be hidden:\n%s", out)
	}
	zero.Coverage, zero.CoverageReasons = "partial", []string{"unresolved"}
	out = renderDetail(t, apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "completed", UsageEstimatedTail: zero})
	if !strings.Contains(out, "EST. TAIL") {
		t.Errorf("zero-token partial tail must be shown:\n%s", out)
	}
}

// TestRunDTOJSONCarriesEstimatedTail: `uzi run get --json` emits the DTO as-is, so the
// estimated tail rides it under usage_estimated_tail with a null (not 0) unpriced cost.
func TestRunDTOJSONCarriesEstimatedTail(t *testing.T) {
	b, err := json.Marshal(apitypes.RunDTO{ID: "run-1", UsageEstimatedTail: &apitypes.UsageTailDTO{CostStatus: "unpriced", Coverage: "partial"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"usage_estimated_tail":{`) || !strings.Contains(s, `"cost_usd":null`) {
		t.Errorf("run JSON missing usage_estimated_tail with null cost: %s", s)
	}
}
