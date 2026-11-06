package anthropicprice

import (
	"math"
	"strconv"
	"testing"
)

func i64(v int64) *int64 { return &v }

func TestPriceExactMicrodollars(t *testing.T) {
	// 1,000,000 of each token class on Sonnet 5.5 = 2 + 0.10 + 2.50 + 4 + 10 = $18.60 exactly
	// (the pricing page's per-MTok rates), i.e. 18,600,000,000 nanodollars.
	got, ok := Price(Usage{
		Model:                    "claude-sonnet-5-5",
		InputTokens:              1_000_000,
		CacheReadInputTokens:     1_000_000,
		CacheCreationInputTokens: 2_000_000,
		CacheCreation5mTokens:    i64(1_000_000),
		CacheCreation1hTokens:    i64(1_000_000),
		OutputTokens:             1_000_000,
	})
	if !ok {
		t.Fatal("standard Sonnet 5.5 usage must be priced")
	}
	if g := got.NanoUSD(); g != "18600000000" {
		t.Fatalf("cost = %s nanodollars, want 18600000000", g)
	}
	if usd := got.USD(); usd != 18.6 {
		t.Fatalf("USD() = %v, want 18.6", usd)
	}
}

func TestPriceFractionalMicrodollarIsExact(t *testing.T) {
	// One Fable 5.1 cache-hit token is 0.25 microdollars: not representable in whole
	// microdollars, exact in nanodollars.
	got, ok := Price(Usage{Model: "claude-fable-5-1", CacheReadInputTokens: 1})
	if !ok || got.NanoUSD() != "250" {
		t.Fatalf("one Fable cache-read token = %s ok=%v, want 250 true", got.NanoUSD(), ok)
	}
}

func TestPriceEveryRowMatchesTheFetchedTable(t *testing.T) {
	// The fetched table, USD per MTok, typed in by hand per column (pricing page "Model pricing").
	// Each column is priced alone, so a swapped or mistyped column fails by name rather than
	// cancelling inside a sum. Prompt columns use 1M tokens, or the row's basePromptLimit when it
	// has one (Haiku 5.5's base tier ends at 100,000); output always uses 1M.
	type row struct{ input, cacheRead, write5m, write1h, output float64 }
	want := map[string]row{
		"claude-fable-5-1":          {10, 0.25, 12.5, 20, 50},
		"claude-opus-5-5":           {4, 0.20, 5, 8, 20},
		"claude-opus-5":             {5, 0.50, 6.25, 10, 25},
		"claude-opus-4-8":           {5, 0.50, 6.25, 10, 25},
		"claude-sonnet-5-5":         {2, 0.10, 2.5, 4, 10},
		"claude-haiku-5-5":          {0.10, 0.01, 0.125, 0.20, 0.50},
		"claude-haiku-4-5":          {1, 0.10, 1.25, 2, 5},
		"claude-haiku-4-5-20251001": {1, 0.10, 1.25, 2, 5},
	}
	if len(table) != len(want) {
		t.Fatalf("table has %d rows, test expects %d", len(table), len(want))
	}
	const m = int64(1_000_000)
	for model, w := range want {
		p := m
		if limit := table[model].basePromptLimit; limit > 0 {
			p = limit
		}
		cols := []struct {
			name   string
			u      Usage
			tokens int64
			usd    float64
		}{
			{"input", Usage{Model: model, InputTokens: p}, p, w.input},
			{"cache read", Usage{Model: model, CacheReadInputTokens: p}, p, w.cacheRead},
			{"5m write", Usage{Model: model, CacheCreationInputTokens: p, CacheCreation5mTokens: i64(p)}, p, w.write5m},
			{"1h write", Usage{Model: model, CacheCreationInputTokens: p, CacheCreation1hTokens: i64(p)}, p, w.write1h},
			{"output", Usage{Model: model, OutputTokens: m}, m, w.output},
		}
		for _, c := range cols {
			// USD per MTok is microdollars per token, so x1000 is nanodollars per token; the
			// hand-typed rates have at most three decimals, so the rounding is exact.
			wantNano := strconv.FormatInt(int64(math.Round(c.usd*1000))*c.tokens, 10)
			got, ok := Price(c.u)
			if !ok || got.NanoUSD() != wantNano {
				t.Errorf("%s %s: %s nanodollars for %d tokens ok=%v, want %s (USD %v per MTok)", model, c.name, got.NanoUSD(), c.tokens, ok, wantNano, c.usd)
			}
		}
	}
}

func TestPriceUnpricedNeverZero(t *testing.T) {
	base := Usage{Model: "claude-sonnet-5-5", InputTokens: 10, OutputTokens: 5}
	cases := map[string]Usage{
		"unknown model":        {Model: "claude-sonnet-4", InputTokens: 10},
		"empty model":          {Model: "", InputTokens: 10},
		"suffixed model":       {Model: "claude-sonnet-5-5[1m]", InputTokens: 10},
		"priority tier":        func() Usage { u := base; u.ServiceTier = "priority"; return u }(),
		"batch tier":           func() Usage { u := base; u.ServiceTier = "batch"; return u }(),
		"fast speed":           func() Usage { u := base; u.Speed = "fast"; return u }(),
		"us inference geo":     func() Usage { u := base; u.InferenceGeo = "us"; return u }(),
		"cache write no split": func() Usage { u := base; u.CacheCreationInputTokens = 7; return u }(),
		"split sum mismatch": func() Usage {
			u := base
			u.CacheCreationInputTokens = 7
			u.CacheCreation5mTokens = i64(3)
			u.CacheCreation1hTokens = i64(3)
			return u
		}(),
		"negative tokens": {Model: "claude-sonnet-5-5", InputTokens: -1},
	}
	for name, u := range cases {
		if c, ok := Price(u); ok {
			t.Errorf("%s: priced (%s), want unpriced", name, c.NanoUSD())
		}
	}
}

func TestPriceStandardMarkersArePriced(t *testing.T) {
	u := Usage{Model: "claude-haiku-4-5", InputTokens: 1000, ServiceTier: "standard", Speed: "standard", InferenceGeo: "global"}
	if _, ok := Price(u); !ok {
		t.Fatal("explicit standard markers must price")
	}
	// A split of zeros with zero total is priced (no cache write happened).
	u2 := Usage{Model: "claude-haiku-4-5", CacheCreation5mTokens: i64(0), CacheCreation1hTokens: i64(0)}
	if c, ok := Price(u2); !ok || c.NanoUSD() != "0" {
		t.Fatalf("zero split = %s ok=%v, want 0 true", c.NanoUSD(), ok)
	}
}

func TestPriceNotAvailableInferenceGeoUsesStandardRates(t *testing.T) {
	u := Usage{Model: "claude-haiku-4-5-20251001", InputTokens: 10,
		CacheReadInputTokens: 12326, CacheCreationInputTokens: 5572,
		CacheCreation5mTokens: i64(0), CacheCreation1hTokens: i64(5572),
		OutputTokens: 1, ServiceTier: "standard"}
	want, ok := Price(u)
	if !ok {
		t.Fatal("absent geography must price")
	}
	u.InferenceGeo = "not_available"
	got, ok := Price(u)
	if !ok || got.NanoUSD() != want.NanoUSD() {
		t.Fatalf("not_available = %s ok=%v, want %s true", got.NanoUSD(), ok, want.NanoUSD())
	}
	for _, geo := range []string{"us", "unknown"} {
		u.InferenceGeo = geo
		if _, ok := Price(u); ok {
			t.Errorf("%s geography must remain unpriced", geo)
		}
	}
	u.Model = "claude-haiku-4-5"
	u.InferenceGeo = "not_available"
	if c, ok := Price(u); !ok || c.NanoUSD() != want.NanoUSD() {
		t.Fatal("the undated Haiku 4.5 alias must use the same standard rates")
	}
}

func TestPriceNotAvailableInferenceGeoOnCapableModelsStaysUnpriced(t *testing.T) {
	for _, model := range []string{"claude-fable-5-1", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		t.Run(model, func(t *testing.T) {
			if _, ok := Price(Usage{Model: model, InputTokens: 100, InferenceGeo: "not_available"}); ok {
				t.Fatal("missing geography evidence on a geo-capable model must stay unpriced")
			}
		})
	}
}

func TestPriceSonnet55CacheReadUsesFivePercentOfInput(t *testing.T) {
	c, ok := Price(Usage{Model: "claude-sonnet-5-5", CacheReadInputTokens: 1_000_000})
	if !ok || c.NanoUSD() != "100000000" {
		t.Fatalf("Sonnet 5.5 cache read = %s ok=%v, want $0.10", c.NanoUSD(), ok)
	}
}

func TestPriceHostileTokenCountsDoNotOverflow(t *testing.T) {
	const huge = int64(1) << 62
	c, ok := Price(Usage{Model: "claude-fable-5-1", InputTokens: huge, OutputTokens: huge})
	if !ok || c.NanoUSD() == "" || c.v.Sign() <= 0 {
		t.Fatalf("huge counts must price exactly, got %s ok=%v", c.NanoUSD(), ok)
	}
}

func TestProvenanceConstants(t *testing.T) {
	if AnthropicPriceTableVersion == "" || AnthropicPriceSourceURL == "" || AnthropicPriceFetchedAt != "2026-11-04" {
		t.Fatal("price provenance constants must be set")
	}
}

// Issue #3920: Haiku 5.5 is priced per request by prompt length (input + cache reads + cache
// writes); up to 100,000 tokens pays the base rates. The two production side passes that went
// unreported (a summary pass and a Now note) had prompts of ~10.7k and ~22.3k tokens.
func TestPriceHaiku55UpTo100kPromptUsesBaseRates(t *testing.T) {
	// Each column alone at 100,000 tokens (the largest prompt the base tier covers), so a
	// swapped column fails by name. USD per MTok: 0.10 / 0.01 / 0.125 / 0.20 / 0.50.
	const n = int64(100_000)
	cols := []struct {
		name string
		u    Usage
		want string // nanodollars
	}{
		{"input", Usage{Model: "claude-haiku-5-5", InputTokens: n}, "10000000"},
		{"cache read", Usage{Model: "claude-haiku-5-5", CacheReadInputTokens: n}, "1000000"},
		{"5m write", Usage{Model: "claude-haiku-5-5", CacheCreationInputTokens: n, CacheCreation5mTokens: i64(n)}, "12500000"},
		{"1h write", Usage{Model: "claude-haiku-5-5", CacheCreationInputTokens: n, CacheCreation1hTokens: i64(n)}, "20000000"},
		{"output", Usage{Model: "claude-haiku-5-5", OutputTokens: 1_000_000}, "500000000"},
	}
	for _, c := range cols {
		got, ok := Price(c.u)
		if !ok || got.NanoUSD() != c.want {
			t.Errorf("haiku-5-5 %s = %s ok=%v, want %s true", c.name, got.NanoUSD(), ok, c.want)
		}
	}
	// The production summary-pass shape: 2 input, 10,716 cache read, 2 output.
	got, ok := Price(Usage{Model: "claude-haiku-5-5", InputTokens: 2, CacheReadInputTokens: 10716, OutputTokens: 2,
		CacheCreation5mTokens: i64(0), CacheCreation1hTokens: i64(0)})
	if !ok || got.NanoUSD() != "108360" {
		t.Fatalf("summary-pass shape = %s ok=%v, want 108360 true", got.NanoUSD(), ok)
	}
	// One 5m cache-write token is 0.125 microdollars: below the old hundredth-microdollar unit,
	// exact in nanodollars.
	if c, ok := Price(Usage{Model: "claude-haiku-5-5", CacheCreationInputTokens: 1, CacheCreation5mTokens: i64(1)}); !ok || c.NanoUSD() != "125" {
		t.Fatalf("one Haiku 5.5 5m-write token = %s ok=%v, want 125 true", c.NanoUSD(), ok)
	}
	// A summed side pass of several requests whose prompts total exactly the limit is priced.
	if _, ok := Price(Usage{Model: "claude-haiku-5-5", InputTokens: 40_000, CacheReadInputTokens: 60_000}); !ok {
		t.Fatal("a prompt total of exactly 100,000 must price")
	}
}

func TestPriceHaiku55Over100kPromptStaysUnpriced(t *testing.T) {
	// One token over the threshold, in each prompt component: a summed record over 100k cannot
	// prove every request stayed in the base tier, and a single message over it pays the higher
	// tier this table does not carry. Output tokens do not count toward the prompt.
	cases := map[string]Usage{
		"input":      {Model: "claude-haiku-5-5", InputTokens: 100_001},
		"cache read": {Model: "claude-haiku-5-5", InputTokens: 1, CacheReadInputTokens: 100_000},
		"cache write": {Model: "claude-haiku-5-5", InputTokens: 1, CacheCreationInputTokens: 100_000,
			CacheCreation5mTokens: i64(100_000)},
	}
	for name, u := range cases {
		if c, ok := Price(u); ok {
			t.Errorf("%s over 100k: priced (%s), want unpriced", name, c.NanoUSD())
		}
	}
	if _, ok := Price(Usage{Model: "claude-haiku-5-5", InputTokens: 100_000, OutputTokens: 500_000}); !ok {
		t.Error("output tokens must not count toward the prompt threshold")
	}
	// Hostile counts must not wrap the prompt sum back under the limit.
	const huge = int64(1) << 62
	if _, ok := Price(Usage{Model: "claude-haiku-5-5", InputTokens: huge, CacheReadInputTokens: huge,
		CacheCreationInputTokens: huge, CacheCreation5mTokens: i64(huge)}); ok {
		t.Error("an overflowing prompt sum must stay unpriced")
	}
}
