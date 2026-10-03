package anthropicprice

import "testing"

func i64(v int64) *int64 { return &v }

func TestPriceExactMicrodollars(t *testing.T) {
	// 1,000,000 of each token class on Sonnet 5.5 = 2 + 0.20 + 2.50 + 4 + 10 = $18.70 exactly
	// (the pricing page's per-MTok rates), i.e. 1,870,000,000 hundredths of a microdollar.
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
	if g := got.HundredthMicroUSD(); g != "1870000000" {
		t.Fatalf("cost = %s hundredth-microdollars, want 1870000000", g)
	}
	if usd := got.USD(); usd != 18.7 {
		t.Fatalf("USD() = %v, want 18.7", usd)
	}
}

func TestPriceFractionalMicrodollarIsExact(t *testing.T) {
	// One Fable 5.1 cache-hit token is 0.25 microdollars: not representable in whole
	// microdollars, exact in hundredths.
	got, ok := Price(Usage{Model: "claude-fable-5-1", CacheReadInputTokens: 1})
	if !ok || got.HundredthMicroUSD() != "25" {
		t.Fatalf("one Fable cache-read token = %s ok=%v, want 25 true", got.HundredthMicroUSD(), ok)
	}
}

func TestPriceEveryRowMatchesTheFetchedTable(t *testing.T) {
	// 1M input, 1M output, 1M cache read, 1M 5m write, 1M 1h write per model, in USD/MTok.
	want := map[string]float64{
		"claude-fable-5-1":          10 + 50 + 0.25 + 12.5 + 20,
		"claude-opus-5-5":           4 + 20 + 0.20 + 5 + 8,
		"claude-opus-5":             5 + 25 + 0.50 + 6.25 + 10,
		"claude-opus-4-8":           5 + 25 + 0.50 + 6.25 + 10,
		"claude-sonnet-5-5":         2 + 10 + 0.20 + 2.5 + 4,
		"claude-haiku-4-5":          1 + 5 + 0.10 + 1.25 + 2,
		"claude-haiku-4-5-20251001": 1 + 5 + 0.10 + 1.25 + 2,
	}
	if len(table) != len(want) {
		t.Fatalf("table has %d rows, test expects %d", len(table), len(want))
	}
	for model, usd := range want {
		got, ok := Price(Usage{
			Model: model, InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadInputTokens: 1_000_000,
			CacheCreationInputTokens: 2_000_000, CacheCreation5mTokens: i64(1_000_000), CacheCreation1hTokens: i64(1_000_000),
		})
		if !ok || got.USD() != usd {
			t.Errorf("%s: USD = %v ok=%v, want %v", model, got.USD(), ok, usd)
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
			t.Errorf("%s: priced (%s), want unpriced", name, c.HundredthMicroUSD())
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
	if c, ok := Price(u2); !ok || c.HundredthMicroUSD() != "0" {
		t.Fatalf("zero split = %s ok=%v, want 0 true", c.HundredthMicroUSD(), ok)
	}
}

func TestPriceHostileTokenCountsDoNotOverflow(t *testing.T) {
	const huge = int64(1) << 62
	c, ok := Price(Usage{Model: "claude-fable-5-1", InputTokens: huge, OutputTokens: huge})
	if !ok || c.HundredthMicroUSD() == "" || c.v.Sign() <= 0 {
		t.Fatalf("huge counts must price exactly, got %s ok=%v", c.HundredthMicroUSD(), ok)
	}
}

func TestProvenanceConstants(t *testing.T) {
	if AnthropicPriceTableVersion == "" || AnthropicPriceSourceURL == "" || AnthropicPriceFetchedAt != "2026-10-03" {
		t.Fatal("price provenance constants must be set")
	}
}
