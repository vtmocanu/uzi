package main

import (
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// toFixed formats a non-negative x to d decimal places the way JavaScript's
// Number.prototype.toFixed does: round half UP (ties toward +inf → the larger n)
// on the EXACT value of the double. This is the parity primitive the token/cost
// formatters need because the web helpers (web/src/lib/formatTokens.ts) use
// toFixed, and Go's own rounding diverges from it on halves — in BOTH directions:
//
//   - fmt "%.*f" rounds half to EVEN, so an exact tie like 0.125 formats "0.12"
//     where toFixed gives "0.13".
//   - a math.Round(x*10^d)/10^d pre-round corrupts a NEAR-tie: 2.675 is really
//     2.67499999…, which toFixed renders "2.67", but 2.675*100 rounds UP to
//     exactly 267.5 in float64, so math.Round then yields "2.68".
//
// Operating on the exact rational value of the double (math/big) sidesteps both:
// there is no intermediate float multiply to round, and the tie-break is half-up.
// Negatives clamp to 0 (callers already reject NaN/Inf/negative before scaling).
func toFixed(x float64, d int) string {
	if x < 0 {
		x = 0
	}
	r := new(big.Rat).SetFloat64(x)
	if r == nil { // NaN/Inf guard; callers clamp these, but stay total.
		r = new(big.Rat)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil)
	num := new(big.Int).Mul(r.Num(), scale) // numerator of x*10^d (denom = r.Denom)
	den := r.Denom()
	// n = floor(x*10^d + 1/2) = floor((2*num + den) / (2*den)); num,den >= 0.
	twoNum := new(big.Int).Lsh(num, 1)
	twoNum.Add(twoNum, den)
	n := new(big.Int).Div(twoNum, new(big.Int).Lsh(den, 1))
	if d <= 0 {
		return n.String()
	}
	intPart := new(big.Int)
	frac := new(big.Int)
	intPart.DivMod(n, scale, frac)
	return intPart.String() + "." + fmt.Sprintf("%0*d", d, frac)
}

// Shared cost/token presentation layer for the TUI (PRD #650 M1). These are the
// pure formatters — the callers (board and detail, in later milestones) own the
// "—" (subscription $0) and blank (nil Usage) decisions, so the board and run
// view can differ. Each formatter mirrors a web helper so the terminal and the
// web UI read a figure the same way; the parity source is named on each.

// fmtCostCents renders a USD cost the way web formatCost does
// (web/src/lib/formatTokens.ts, formatCost): "$1.87". A cost of $1000 or more
// drops the cents and renders as whole dollars ("$1119") — the cents are noise
// at that scale — while anything below keeps the two-decimal form. No thousands
// separator either way. NaN/Inf/negative clamp to 0.
func fmtCostCents(usd float64) string {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		usd = 0
	}
	if usd >= 1000 {
		return "$" + strconv.FormatInt(int64(math.Round(usd)), 10)
	}
	// toFixed matches web formatCost's JS toFixed (half-up on the exact double),
	// where fmt "%.2f" would round half-to-even and diverge on an exact half.
	return "$" + toFixed(usd, 2)
}

// fmtCostWhole is the factory-floor formatter: whole dollars only, to keep the
// board's COST column width stable (PRD #650 Decision: no decimals on the board).
// A real sub-dollar cost must never show as "$0", so 0 < usd < 0.5 renders "<$1";
// everything else rounds to "$" + round(usd) (so exactly 0 → "$0", and callers
// decide whether to even call this on a 0). NaN/negative clamp to 0.
func fmtCostWhole(usd float64) string {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		usd = 0
	}
	if usd > 0 && usd < 0.5 {
		return "<$1"
	}
	return "$" + strconv.FormatInt(int64(math.Round(usd)), 10)
}

// fmtTokens renders a token count the way web formatTokens does
// (web/src/lib/formatTokens.ts, formatTokens): an adaptive ladder — bare integer
// under 1k, "k" with one decimal under 1M, then "M"/"B"/"T" with two decimals.
// Negatives clamp to 0. The tier is chosen from the raw value, matching the web
// helper (including its known top-edge rounding wart).
func fmtTokens(n int64) string {
	if n < 0 {
		n = 0
	}
	f := float64(n)
	// Format each decimal tier through toFixed so ties round half-UP the way the
	// web helper's JS toFixed does; Go's fmt would round half-to-even and diverge
	// on an exact half (e.g. 1250 → web "1.3k" but fmt "1.2k"). The scaling divide
	// mirrors the web helper (it divides in JS doubles too), and the tier is still
	// selected from the raw n, unchanged.
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return toFixed(f/1000.0, 1) + "k"
	case n < 1_000_000_000:
		return toFixed(f/1e6, 2) + "M"
	case n < 1_000_000_000_000:
		return toFixed(f/1e9, 2) + "B"
	default:
		return toFixed(f/1e12, 2) + "T"
	}
}

// cacheDisplayPct is the integer "% from cache" the SPEND block renders, mirroring
// web cacheDisplayPct (web/src/lib/runUsage.ts:257-274). fresh = input +
// cacheCreation, cached = cacheRead. It is stated as a property, not a plain round:
// a real run sits at 97-99.6% cache, and a plain round would show "100%" beside
// plainly-present fresh tokens. So when both parts are positive the result is
// clamped into [1,99] — 100 renders only when fresh is 0, and 0 only when there
// are no cache reads.
func cacheDisplayPct(input, cacheRead, cacheCreation int64) int {
	fresh := input + cacheCreation
	cached := cacheRead
	if cached <= 0 {
		return 0
	}
	if fresh <= 0 {
		return 100
	}
	pct := int(math.Round(float64(cached) / float64(fresh+cached) * 100))
	if pct < 1 {
		pct = 1
	}
	if pct > 99 {
		pct = 99
	}
	return pct
}

// boardCostTotal is the factory-floor total (PRD #650 M1): the sum of the raw
// CostUSD over every run whose Usage is non-nil, rounded for display as
// "$" + round(sum). The sum is over raw values, NOT the rounded per-row cells, so
// the aggregate is accurate even though individual rounded rows may not visibly
// add up to it (documented dashboard behaviour). Returns ("", false) when the
// summed total is 0 (or no usage-bearing runs), so the caller drops the segment.
func boardCostTotal(runs []apitypes.RunListItemDTO) (string, bool) {
	var sum float64
	for _, r := range runs {
		if r.Usage == nil {
			continue
		}
		// Skip an invalid per-row cost the same way fmtCostWhole clamps it, so one
		// bad row cannot poison the whole total: a lone NaN would make the sum NaN
		// and drop the segment, and a negative would silently shrink the aggregate.
		cost := r.Usage.CostUSD
		if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
			continue
		}
		sum += cost
	}
	rounded := math.Round(sum)
	// sum is now always finite and >= 0 (invalid rows were skipped above), so the
	// segment is dropped only when there is genuinely nothing to show: no
	// usage-bearing runs, or a raw sum that rounds to 0.
	if rounded <= 0 {
		return "", false
	}
	return "$" + strconv.FormatInt(int64(rounded), 10), true
}

// fmtCostBoard formats a run's cost for the FIXED-WIDTH board COST cell: fmtCostWhole
// for anything that fits boardCostWidth, and a k/M/G abbreviation above that so a
// pathological cost (>= $100000 renders "$100000", 7 > the 6-cell column) cannot blow
// the row width. Only the board cell is width-constrained; the detail view calls
// fmtCostWhole directly (full number, no cap), so the abbreviation lives at this call
// site rather than in the shared fmtCostWhole.
func fmtCostBoard(usd float64) string {
	s := fmtCostWhole(usd)
	if visualWidth(s) <= boardCostWidth {
		return s
	}
	// Above $9999G even the abbreviation ($<n>G) would exceed boardCostWidth, so cap it
	// with a fixed-width overflow marker. No real run approaches even $1; this makes the
	// width invariant TOTAL rather than merely usual (CodeRabbit re-review).
	if usd >= 1e13 {
		return ">$999G"
	}
	switch {
	case usd < 1e6:
		return "$" + strconv.FormatInt(int64(usd/1e3), 10) + "k"
	case usd < 1e9:
		return "$" + strconv.FormatInt(int64(usd/1e6), 10) + "M"
	default:
		return "$" + strconv.FormatInt(int64(usd/1e9), 10) + "G"
	}
}
