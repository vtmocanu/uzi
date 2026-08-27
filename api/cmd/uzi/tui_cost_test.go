package main

import (
	"math"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// These pin the exact web-parity reference values (web/src/lib/formatTokens.ts,
// web/src/lib/runUsage.ts) so the terminal and web read a figure identically.

func TestFmtTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{999, "999"},
		{48200, "48.2k"},
		{188000, "188.0k"},
		{1280000, "1.28M"},
		{5400000000, "5.40B"},
		{2300000000000, "2.30T"},
		{0, "0"},
		{-5, "0"},
		// Representable-half web-parity cases: JS toFixed rounds these exact ties
		// up, so fmtTokens' math/big toFixed helper must too (Go fmt's half-to-even
		// would give "1.2k"/"1.12M"). Verified against real web (node) output.
		{1250, "1.3k"},
		{1125000, "1.13M"},
	}
	for _, c := range cases {
		if got := fmtTokens(c.in); got != c.want {
			t.Errorf("fmtTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFmtCostCents(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1.87, "$1.87"},
		{1119.0, "$1119"},     // >= 1000 drops cents
		{999.999, "$1000.00"}, // below-1000 boundary keeps 2dp
		{-3.0, "$0.00"},
		// Web-parity: JS toFixed rounds this representable tie up; Go fmt
		// half-to-even would give "$0.12". Verified against real web (node) output.
		{0.125, "$0.13"},
		// Non-tie double-precision cases pinning toFixed's actual-double behavior.
		// 1.005 and 2.675 are stored as 1.00499…/2.67499…, so toFixed (operating on
		// the exact double) renders "$1.00"/"$2.67"; 0.005 is stored just above and
		// renders "$0.01". These are exactly the values where the rejected
		// math.Round(x*100)/100 approach diverges (2.675*100 lands on 267.5 → "$2.68"),
		// which is why fmtCostCents uses the exact-rational toFixed. Verified via node.
		{1.005, "$1.00"},
		{2.675, "$2.67"},
		{0.005, "$0.01"},
	}
	for _, c := range cases {
		if got := fmtCostCents(c.in); got != c.want {
			t.Errorf("fmtCostCents(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFmtCostWhole(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.3, "<$1"},
		{0.5, "$1"}, // rounds up, not <$1 since not < 0.5
		{0.49, "<$1"},
		{9.4, "$9"},
		{9.6, "$10"},
		{0, "$0"},
	}
	for _, c := range cases {
		if got := fmtCostWhole(c.in); got != c.want {
			t.Errorf("fmtCostWhole(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCacheDisplayPct(t *testing.T) {
	cases := []struct {
		name                          string
		input, cacheRead, cacheCreate int64
		want                          int
	}{
		{"99.6% clamps to 99 not 100", 4000, 996000, 0, 99},
		// fresh = input + cacheCreation must be 0 to hit the 100 branch, so both
		// input and cacheCreation are 0 with cacheRead > 0. (The PRD tuple
		// (0,500,500) would give fresh=500 and thus 50, not the intended 100.)
		{"fresh==0 is 100", 0, 500, 0, 100},
		{"cached==0 is 0", 1000, 0, 0, 0},
		{"tiny ratio clamps up to 1", 1_000_000, 100, 0, 1},
	}
	for _, c := range cases {
		if got := cacheDisplayPct(c.input, c.cacheRead, c.cacheCreate); got != c.want {
			t.Errorf("%s: cacheDisplayPct(%d,%d,%d) = %d, want %d",
				c.name, c.input, c.cacheRead, c.cacheCreate, got, c.want)
		}
	}
}

func TestBoardCostTotal(t *testing.T) {
	// round(1.6)+round(1.6) = 2+2 = 4, but round(1.6+1.6) = round(3.2) = 3.
	// Proves the total sums raw values, not the rounded per-row cells. The
	// nil-Usage run must be ignored.
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 1.6}}},
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 1.6}}},
		{RunDTO: apitypes.RunDTO{Usage: nil}},
	}
	if got, ok := boardCostTotal(runs); !ok || got != "$3" {
		t.Errorf("boardCostTotal(mixed) = (%q, %v), want (\"$3\", true)", got, ok)
	}

	// Empty slice → no usage-bearing runs.
	if got, ok := boardCostTotal(nil); ok || got != "" {
		t.Errorf("boardCostTotal(nil) = (%q, %v), want (\"\", false)", got, ok)
	}

	// All-nil slice → no usage-bearing runs.
	allNil := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{Usage: nil}},
		{RunDTO: apitypes.RunDTO{Usage: nil}},
	}
	if got, ok := boardCostTotal(allNil); ok || got != "" {
		t.Errorf("boardCostTotal(allNil) = (%q, %v), want (\"\", false)", got, ok)
	}

	// Raw sum that rounds to 0 (0.2 + 0.2 = 0.4 → round 0) → dropped.
	roundsToZero := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 0.2}}},
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 0.2}}},
	}
	if got, ok := boardCostTotal(roundsToZero); ok || got != "" {
		t.Errorf("boardCostTotal(roundsToZero) = (%q, %v), want (\"\", false)", got, ok)
	}

	// A NaN/Inf/negative row cost is skipped, not allowed to poison the total: $10
	// plus a NaN, an Inf, and a -5 must still total $10, not vanish or shrink.
	withInvalid := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: 10}}},
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: math.NaN()}}},
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: math.Inf(1)}}},
		{RunDTO: apitypes.RunDTO{Usage: &apitypes.UsageDTO{CostUSD: -5}}},
	}
	if got, ok := boardCostTotal(withInvalid); !ok || got != "$10" {
		t.Errorf("boardCostTotal(withInvalid) = (%q, %v), want (\"$10\", true)", got, ok)
	}
}
