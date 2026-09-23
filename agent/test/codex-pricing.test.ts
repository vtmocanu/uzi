import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CODEX_INPUT_TIER_THRESHOLD_TOKENS,
  CODEX_PRICE_TABLE_VERSION,
  SOL_PROMO_REVIEW_DATE,
  priceCodexResponse,
} from "../src/codex/codex-pricing.js";
import type { CodexUsageBreakdown } from "../src/codex/transport.js";

// PRD #1332 (M5A, C4b) — the pure versioned Standard price table (D5), exercised directly. These
// pin: the 8 published rates via each cache bucket priced alone for BOTH models; the per-response
// >272K tier boundary DIRECTION (`> 272K` → high, exactly 272K / just under → low); reasoning not
// double-counted in the output charge; the impossible cache split failing to unpriceable; unknown
// model unpriceable; and the gpt-5.6-sol promotional review-date flip (metered at 2026-11-20,
// unreported at 2026-11-21) with gpt-6-astra unaffected — all on an INJECTED clock.

const ASTRA = "gpt-6-astra";
const SOL = "gpt-5.6-sol";
const SOL6 = "gpt-6-sol";
// A clock well before any promotional review boundary — the "priceable" default for these tests.
const BEFORE_SOL_REVIEW = new Date("2026-01-01T00:00:00Z");

function bd(overrides: Partial<CodexUsageBreakdown> = {}): CodexUsageBreakdown {
  return {
    inputTokens: 0,
    cachedInputTokens: 0,
    cacheWriteInputTokens: 0,
    outputTokens: 0,
    reasoningOutputTokens: 0,
    totalTokens: 0,
    ...overrides,
  };
}

/** Microdollars — an EXACT integer comparison free of binary-float noise (the server quantizes to
 *  microdollars too). */
function micro(usd: number | undefined): number | undefined {
  return usd === undefined ? undefined : Math.round(usd * 1e6);
}

describe("codex-pricing: the version id and boundary constants are the pinned D5 values", () => {
  it("records the table version and the Sol review boundary with the table", () => {
    assert.equal(CODEX_PRICE_TABLE_VERSION, "openai-standard-2026-09-23");
    assert.equal(SOL_PROMO_REVIEW_DATE, "2026-11-21");
    assert.equal(CODEX_INPUT_TIER_THRESHOLD_TOKENS, 272_000);
  });
});

describe("codex-pricing: each cache bucket is priced at its own rate (low tier)", () => {
  // gpt-6-astra low: uncached 10.00 / cached 1.00 / cacheWrite 12.50 / output 50.00 per 1e6.
  it("gpt-6-astra prices uncached, cached, cacheWrite and output independently", () => {
    assert.equal(micro(priceCodexResponse(ASTRA, bd({ inputTokens: 100 }), BEFORE_SOL_REVIEW)), 1000); // 100 * 10.00
    assert.equal(
      micro(priceCodexResponse(ASTRA, bd({ inputTokens: 500, cachedInputTokens: 500 }), BEFORE_SOL_REVIEW)),
      500, // 500 * 1.00, uncached = 0
    );
    assert.equal(
      micro(priceCodexResponse(ASTRA, bd({ inputTokens: 400, cacheWriteInputTokens: 400 }), BEFORE_SOL_REVIEW)),
      5000, // 400 * 12.50, uncached = 0
    );
    assert.equal(micro(priceCodexResponse(ASTRA, bd({ outputTokens: 1000 }), BEFORE_SOL_REVIEW)), 50000); // 1000 * 50.00
  });

  // gpt-5.6-sol low: uncached 4.00 / cached 0.40 / cacheWrite 5.00 / output 20.00 per 1e6.
  it("gpt-5.6-sol prices uncached, cached, cacheWrite and output independently", () => {
    assert.equal(micro(priceCodexResponse(SOL, bd({ inputTokens: 100 }), BEFORE_SOL_REVIEW)), 400); // 100 * 4.00
    assert.equal(
      micro(priceCodexResponse(SOL, bd({ inputTokens: 500, cachedInputTokens: 500 }), BEFORE_SOL_REVIEW)),
      200, // 500 * 0.40
    );
    assert.equal(
      micro(priceCodexResponse(SOL, bd({ inputTokens: 400, cacheWriteInputTokens: 400 }), BEFORE_SOL_REVIEW)),
      2000, // 400 * 5.00
    );
    assert.equal(micro(priceCodexResponse(SOL, bd({ outputTokens: 1000 }), BEFORE_SOL_REVIEW)), 20000); // 1000 * 20.00
  });

  // gpt-6-sol low: uncached 2.00 / cached 0.20 / cacheWrite 2.50 / output 10.00 per 1e6 (official
  // model page, 2026-09-23; not promotional).
  it("gpt-6-sol prices uncached, cached, cacheWrite and output independently", () => {
    assert.equal(micro(priceCodexResponse(SOL6, bd({ inputTokens: 100 }), BEFORE_SOL_REVIEW)), 200); // 100 * 2.00
    assert.equal(
      micro(priceCodexResponse(SOL6, bd({ inputTokens: 500, cachedInputTokens: 500 }), BEFORE_SOL_REVIEW)),
      100, // 500 * 0.20
    );
    assert.equal(
      micro(priceCodexResponse(SOL6, bd({ inputTokens: 400, cacheWriteInputTokens: 400 }), BEFORE_SOL_REVIEW)),
      1000, // 400 * 2.50
    );
    assert.equal(micro(priceCodexResponse(SOL6, bd({ outputTokens: 1000 }), BEFORE_SOL_REVIEW)), 10000); // 1000 * 10.00
  });

  it("a full mixed breakdown sums the four buckets (uncached = input - cached - cacheWrite)", () => {
    // input 1000, cached 600, cacheWrite 100 → uncached 300. astra low:
    // 300*10 + 600*1 + 100*12.5 + 200*50 = 3000 + 600 + 1250 + 10000 = 14850 µ$.
    const last = bd({ inputTokens: 1000, cachedInputTokens: 600, cacheWriteInputTokens: 100, outputTokens: 200, reasoningOutputTokens: 50 });
    assert.equal(micro(priceCodexResponse(ASTRA, last, BEFORE_SOL_REVIEW)), 14850);
  });
});

describe("codex-pricing: the >272K tier boundary is per-response and strictly greater-than", () => {
  it("exactly 272K input uses the LOW rate; 272K+1 uses the HIGH rate; just under stays LOW", () => {
    const at = bd({ inputTokens: CODEX_INPUT_TIER_THRESHOLD_TOKENS, outputTokens: 100 });
    const under = bd({ inputTokens: CODEX_INPUT_TIER_THRESHOLD_TOKENS - 1, outputTokens: 100 });
    const over = bd({ inputTokens: CODEX_INPUT_TIER_THRESHOLD_TOKENS + 1, outputTokens: 100 });
    // astra low: uncached*10 + output*50. astra high: uncached*20 + output*75.
    assert.equal(micro(priceCodexResponse(ASTRA, at, BEFORE_SOL_REVIEW)), 272000 * 10 + 100 * 50); // 2_725_000
    assert.equal(micro(priceCodexResponse(ASTRA, under, BEFORE_SOL_REVIEW)), 271999 * 10 + 100 * 50); // 2_724_990
    assert.equal(micro(priceCodexResponse(ASTRA, over, BEFORE_SOL_REVIEW)), 272001 * 20 + 100 * 75); // 5_447_520
  });

  it("the high tier applies to gpt-5.6-sol too", () => {
    const over = bd({ inputTokens: CODEX_INPUT_TIER_THRESHOLD_TOKENS + 1, outputTokens: 100 });
    // sol high: uncached*8 + output*30.
    assert.equal(micro(priceCodexResponse(SOL, over, BEFORE_SOL_REVIEW)), 272001 * 8 + 100 * 30); // 2_176_008 + 3000
  });

  it("the high tier applies to gpt-6-sol: 2x input and cache, 1.5x output", () => {
    const over = bd({ inputTokens: CODEX_INPUT_TIER_THRESHOLD_TOKENS + 1, cachedInputTokens: 1000, cacheWriteInputTokens: 1000, outputTokens: 100 });
    // sol6 high: uncached*4 + cached*0.4 + cacheWrite*5 + output*15.
    assert.equal(micro(priceCodexResponse(SOL6, over, BEFORE_SOL_REVIEW)), 270001 * 4 + 1000 * 0.4 + 1000 * 5 + 100 * 15);
  });
});

describe("codex-pricing: reasoning is inside output, never charged twice", () => {
  it("output-only response prices outputTokens once, ignoring reasoningOutputTokens", () => {
    // astra low: 200 output at 50.00 = 10000 µ$. If reasoning (50) were added, it would be 12500.
    const last = bd({ outputTokens: 200, reasoningOutputTokens: 50 });
    assert.equal(micro(priceCodexResponse(ASTRA, last, BEFORE_SOL_REVIEW)), 10000);
  });
});

describe("codex-pricing: unpriceable responses return undefined (never priced as Standard)", () => {
  it("an unknown model is not priceable", () => {
    assert.equal(priceCodexResponse("gpt-unknown-9", bd({ inputTokens: 1000, outputTokens: 200 }), BEFORE_SOL_REVIEW), undefined);
  });

  it("a cache split exceeding total input is not priceable (impossible/hostile)", () => {
    // cached 80 + cacheWrite 40 = 120 > input 100 → undefined, never a negative uncached.
    assert.equal(priceCodexResponse(ASTRA, bd({ inputTokens: 100, cachedInputTokens: 80, cacheWriteInputTokens: 40 }), BEFORE_SOL_REVIEW), undefined);
  });

  it("a cache split exactly equal to input is priceable (uncached = 0), not rejected", () => {
    // cached 60 + cacheWrite 40 = 100 == input 100 → uncached 0, still priced.
    // astra low: 60*1 + 40*12.5 = 60 + 500 = 560 µ$.
    assert.equal(micro(priceCodexResponse(ASTRA, bd({ inputTokens: 100, cachedInputTokens: 60, cacheWriteInputTokens: 40 }), BEFORE_SOL_REVIEW)), 560);
  });
});

describe("codex-pricing: the gpt-5.6-sol promotional review boundary flips on the injected clock", () => {
  const solResponse = bd({ inputTokens: 1000, outputTokens: 200 });
  const astraResponse = bd({ inputTokens: 1000, outputTokens: 200 });

  it("gpt-5.6-sol is priced the day BEFORE the review date (2026-11-20)", () => {
    // sol low, uncached 1000: 1000*4 + 200*20 = 4000 + 4000 = 8000 µ$.
    assert.equal(micro(priceCodexResponse(SOL, solResponse, new Date("2026-11-20T23:59:59Z"))), 8000);
  });

  it("gpt-5.6-sol is UNPRICEABLE on and after the review date (2026-11-21)", () => {
    assert.equal(priceCodexResponse(SOL, solResponse, new Date("2026-11-21T00:00:00Z")), undefined);
    assert.equal(priceCodexResponse(SOL, solResponse, new Date("2026-12-01T12:00:00Z")), undefined);
  });

  it("gpt-6-astra is UNAFFECTED by the Sol review date — still priced on 2026-11-21", () => {
    // astra low, uncached 1000: 1000*10 + 200*50 = 10000 + 10000 = 20000 µ$.
    assert.equal(micro(priceCodexResponse(ASTRA, astraResponse, new Date("2026-11-21T00:00:00Z"))), 20000);
  });
});

describe("codex-pricing: gpt-6-sol has no promotional review boundary", () => {
  it("gpt-6-sol stays priced on and after the gpt-5.6-sol review date", () => {
    const onReview = new Date(`${SOL_PROMO_REVIEW_DATE}T00:00:00Z`);
    assert.equal(micro(priceCodexResponse(SOL6, bd({ inputTokens: 100 }), onReview)), 200);
    assert.equal(micro(priceCodexResponse(SOL6, bd({ inputTokens: 100 }), new Date("2027-06-01T00:00:00Z"))), 200);
  });
});
