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
		Coverage: "partial", CoverageReasons: []string{"leg_not_closed", "brand_new\x1b[31m\u202ereason"},
	}
	out := renderDetail(t, apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "failed",
		Usage:              &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 4.20, InputTokens: 100},
		UsageEstimatedTail: priced,
	})
	for _, want := range []string{"COST", "$4.20", "EST. TAIL", "~$12.34 estimated (anthropic-standard-2026-10-03)", "1.20M in/34.0k out", "partial: the last session was cut off before it reported its total; brand_new"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "COST") && (strings.Contains(line, "12.34") || strings.Contains(line, "estimated")) {
			t.Errorf("tail merged into the COST row: %q", line)
		}
	}
	if strings.ContainsAny(out, "\x1b\u202e") {
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

// TestUsageTailReasonSentences pins the plain-words sentence for all seven coverage reasons;
// web lib/usageTail.ts and docs/run-cost.md carry the identical wording.
func TestUsageTailReasonSentences(t *testing.T) {
	want := map[string]string{
		"leg_not_closed":       "the last session was cut off before it reported its total",
		"ordinal_gap":          "some model calls were never received",
		"output_not_final":     "output tokens of the last call were still streaming",
		"superseded_uncertain": "it is unclear whether a later total already counted some calls",
		"records_dropped":      "some usage records were dropped",
		"record_cap_reached":   "the per-run usage record limit was reached",
		"unresolved":           "coverage could not be determined",
	}
	if len(usageTailReasonText) != len(want) {
		t.Errorf("reason table has %d entries, want %d", len(usageTailReasonText), len(want))
	}
	for code, sentence := range want {
		cell := usageTailCell(apitypes.UsageTailDTO{OutputTokens: 1, CostStatus: "unpriced", Coverage: "partial", CoverageReasons: []string{code}})
		if !strings.HasSuffix(cell, "partial: "+sentence) {
			t.Errorf("%s: cell %q does not end with %q", code, cell, sentence)
		}
	}
}

// TestUsageTailCellCostForms: an "estimated" status with a nil cost is cost unknown, never
// "~$0.00"; an unpriced tail drops the price-table version; a sub-cent cost reads "<$0.01".
func TestUsageTailCellCostForms(t *testing.T) {
	cell := usageTailCell(apitypes.UsageTailDTO{OutputTokens: 1, CostStatus: "estimated", PriceTableVersion: "anthropic-v1", Coverage: "complete"})
	if !strings.HasPrefix(cell, "cost unknown") || strings.Contains(cell, "$") {
		t.Errorf("estimated with nil cost: %q", cell)
	}
	cell = usageTailCell(apitypes.UsageTailDTO{OutputTokens: 1, CostStatus: "unpriced", PriceTableVersion: "anthropic-v1", Coverage: "complete"})
	if !strings.HasPrefix(cell, "cost unknown (unpriced) \u00b7") || strings.Contains(cell, "anthropic-v1") {
		t.Errorf("unpriced must drop the version: %q", cell)
	}
	cell = usageTailCell(apitypes.UsageTailDTO{OutputTokens: 1, CostStatus: "estimated", CostUSD: f64(0.001), PriceTableVersion: "v", Coverage: "complete"})
	if !strings.HasPrefix(cell, "<$0.01 estimated (v)") {
		t.Errorf("tiny cost: %q", cell)
	}
	cell = usageTailCell(apitypes.UsageTailDTO{OutputTokens: 1, CostStatus: "estimated", CostUSD: f64(0), PriceTableVersion: "v", Coverage: "complete"})
	if !strings.HasPrefix(cell, "~$0.00 estimated") {
		t.Errorf("zero cost: %q", cell)
	}
}

// TestUsageTailCellSanitizesHostileText: a hostile reason and a hostile price-table version
// reach the cell only through cellText, so ESC and bidi characters never survive.
func TestUsageTailCellSanitizesHostileText(t *testing.T) {
	cell := usageTailCell(apitypes.UsageTailDTO{
		OutputTokens: 1, CostUSD: f64(1), CostStatus: "estimated",
		PriceTableVersion: "v\x1b[31m\u202e1",
		Coverage:          "partial", CoverageReasons: []string{"bad\x1b[2J\u202ereason"},
	})
	if strings.ContainsAny(cell, "\x1b\u202e") {
		t.Errorf("control or bidi characters leaked: %q", cell)
	}
	if !strings.Contains(cell, "badreason") && !strings.Contains(cell, "bad") {
		t.Errorf("reason lost: %q", cell)
	}
}
