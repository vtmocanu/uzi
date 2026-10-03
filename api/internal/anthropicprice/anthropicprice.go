// Package anthropicprice prices one Anthropic Messages API usage record from a recorded public
// price table (issue #2014, ADR-2014 D6). The result is an ESTIMATE shown apart from the metered
// total; it is never billed money and never merged into run_usage_totals.
//
// Provenance. Every rate below was transcribed from Anthropic's official pricing page, fetched
// during the run that introduced this package (see AnthropicPriceSourceURL and
// AnthropicPriceFetchedAt), never from memory. A model id is priced only when a fetched page
// confirmed it (the models overview, or the fast-mode page for the two ids that page names); the
// pricing page lists more models than appear here, and those stay unpriced on purpose. Matching
// is exact on the model id string: an unknown, dated-variant or suffixed id is unpriced, and
// unpriced is never rendered as zero.
//
// Units. Anthropic quotes USD per million tokens, which is exactly microdollars per token. Some
// rates carry cents (12.50, 0.25), so a rate is held as an integer count of hundredths of a
// microdollar per token, and a cost as an integer count of hundredths of a microdollar
// (1e-8 USD). All arithmetic is integer (math/big, so a hostile token count cannot overflow the COST
// computation; token COUNTS summed elsewhere are the caller's to saturate); nothing is float64
// until USD converts the final exact figure for display.
package anthropicprice

import (
	"math/big"
)

// AnthropicPriceTableVersion identifies this table. It changes whenever a rate, a model row or a
// pricing rule changes, so a stored-nothing estimate can say which table produced it.
const AnthropicPriceTableVersion = "anthropic-standard-2026-10-03"

// AnthropicPriceSourceURL is the official pricing page the rates were transcribed from.
const AnthropicPriceSourceURL = "https://platform.claude.com/docs/en/about-claude/pricing"

// AnthropicPriceFetchedAt is the date the pricing page (and the model-id pages named in
// pricing-source notes: https://platform.claude.com/docs/en/about-claude/models/overview and
// https://platform.claude.com/docs/en/build-with-claude/fast-mode) were fetched.
const AnthropicPriceFetchedAt = "2026-10-03"

// Standard-pricing markers a usage record must carry (or omit) to be priced from this table.
const (
	standardServiceTier  = "standard"
	standardSpeed        = "standard"
	standardInferenceGeo = "global"
)

// rates is one model's price in hundredths of a microdollar per token (USD per MTok x 100).
type rates struct {
	input, cacheRead, cacheWrite5m, cacheWrite1h, output int64
}

// table maps an exact API model id to its rates. USD per MTok, from the pricing page "Model
// pricing" table: base input / 5m cache write / 1h cache write / cache hits / output.
//
//	Claude Fable 5.1   10   / 12.50 / 20   / 0.25 / 50
//	Claude Opus 5.5     4   /  5    /  8   / 0.20 / 20
//	Claude Opus 5       5   /  6.25 / 10   / 0.50 / 25
//	Claude Opus 4.8     5   /  6.25 / 10   / 0.50 / 25
//	Claude Sonnet 5.5   2   /  2.50 /  4   / 0.20 / 10
//	Claude Haiku 4.5    1   /  1.25 /  2   / 0.10 /  5
//
// No long-context premium row exists: the fetched pages state the 1M context window is at
// standard pricing for the 4.6-and-later models listed, and Haiku 4.5's window is 200K.
var table = map[string]rates{
	"claude-fable-5-1":          {input: 1000, cacheRead: 25, cacheWrite5m: 1250, cacheWrite1h: 2000, output: 5000},
	"claude-opus-5-5":           {input: 400, cacheRead: 20, cacheWrite5m: 500, cacheWrite1h: 800, output: 2000},
	"claude-opus-5":             {input: 500, cacheRead: 50, cacheWrite5m: 625, cacheWrite1h: 1000, output: 2500},
	"claude-opus-4-8":           {input: 500, cacheRead: 50, cacheWrite5m: 625, cacheWrite1h: 1000, output: 2500},
	"claude-sonnet-5-5":         {input: 200, cacheRead: 20, cacheWrite5m: 250, cacheWrite1h: 400, output: 1000},
	"claude-haiku-4-5-20251001": {input: 100, cacheRead: 10, cacheWrite5m: 125, cacheWrite1h: 200, output: 500},
	"claude-haiku-4-5":          {input: 100, cacheRead: 10, cacheWrite5m: 125, cacheWrite1h: 200, output: 500},
}

// Usage is one message's token usage and the response markers that decide whether it is priced
// at the standard rates. Empty ServiceTier, Speed and InferenceGeo mean the field was absent.
type Usage struct {
	Model                    string
	InputTokens              int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	// CacheCreation5mTokens / CacheCreation1hTokens are the ephemeral cache-write split, nil
	// when the response carried none.
	CacheCreation5mTokens *int64
	CacheCreation1hTokens *int64
	OutputTokens          int64
	ServiceTier           string
	Speed                 string
	InferenceGeo          string
}

// Cost is an exact price in hundredths of a microdollar (1e-8 USD). The zero Cost is zero
// dollars; use it only as the starting point of a sum of priced messages.
type Cost struct {
	v big.Int
}

// Add adds o to c.
func (c *Cost) Add(o Cost) { c.v.Add(&c.v, &o.v) }

// HundredthMicroUSD returns the exact figure as a decimal string, for tests and logs.
func (c Cost) HundredthMicroUSD() string { return c.v.String() }

// USD converts the exact figure to dollars for display: one correctly rounded division of two
// exactly representable operands for any realistic total (below 2^53 hundredth-microdollars,
// about 90 million dollars).
func (c Cost) USD() float64 {
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(&c.v), big.NewFloat(1e8)).Float64()
	return f
}

// Price prices one message. ok is false, and the Cost meaningless, when the message cannot be
// priced from this table: the model is not a row, service_tier / speed / inference_geo is present
// and not the standard value, cache-creation tokens arrive without a consistent 5m/1h split, or a
// token count is negative. Callers must treat !ok as "cost unknown", never as zero.
func Price(u Usage) (Cost, bool) {
	r, found := table[u.Model]
	if !found {
		return Cost{}, false
	}
	if u.ServiceTier != "" && u.ServiceTier != standardServiceTier {
		return Cost{}, false
	}
	if u.Speed != "" && u.Speed != standardSpeed {
		return Cost{}, false
	}
	if u.InferenceGeo != "" && u.InferenceGeo != standardInferenceGeo {
		return Cost{}, false
	}
	if u.InputTokens < 0 || u.CacheReadInputTokens < 0 || u.CacheCreationInputTokens < 0 || u.OutputTokens < 0 {
		return Cost{}, false
	}
	var w5m, w1h int64
	switch {
	case u.CacheCreation5mTokens != nil || u.CacheCreation1hTokens != nil:
		// A split is present: price by it, but only when it accounts for exactly the recorded
		// cache-creation total. A missing half counts as zero; a mismatch means the recorded
		// figures disagree and the write cost is unknown.
		if u.CacheCreation5mTokens != nil {
			w5m = *u.CacheCreation5mTokens
		}
		if u.CacheCreation1hTokens != nil {
			w1h = *u.CacheCreation1hTokens
		}
		if w5m < 0 || w1h < 0 || w5m+w1h != u.CacheCreationInputTokens {
			return Cost{}, false
		}
	case u.CacheCreationInputTokens > 0:
		// Cache writes with no 5m/1h split: the two write rates differ, so the cost is unknown.
		return Cost{}, false
	}
	var c Cost
	add := func(tokens, rate int64) {
		var t big.Int
		t.Mul(big.NewInt(tokens), big.NewInt(rate))
		c.v.Add(&c.v, &t)
	}
	add(u.InputTokens, r.input)
	add(u.CacheReadInputTokens, r.cacheRead)
	add(w5m, r.cacheWrite5m)
	add(w1h, r.cacheWrite1h)
	add(u.OutputTokens, r.output)
	return c, true
}
