import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CodexUsageAccountant,
  deriveCodexRunCost,
  type CodexModelUsageEntry,
  type CodexPricingContext,
} from "../src/codex/token-accounting.js";
import type { CodexThreadTokenUsage, CodexUsageBreakdown } from "../src/codex/transport.js";

// PRD #1332 (M5A, C4b) — the cost projection D5 layers over C4a's token accounting, exercised
// through the accountant. Both auth modes use API-equivalent per-response prices.
// Incomplete evidence and reconciliation gaps retain tokens as unreported.

const ASTRA = "gpt-6-astra";
const SOL = "gpt-5.6-sol";
const UNKNOWN = "gpt-mystery-0";
const ROOT = "th-root";
const CHILD = "th-child";

const API_KEY_EARLY: CodexPricingContext = { authMode: "api_key", now: new Date("2026-01-01T00:00:00Z") };
const SUBSCRIPTION: CodexPricingContext = { authMode: "subscription", now: new Date("2026-01-01T00:00:00Z") };

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

function usage(total: Partial<CodexUsageBreakdown>, last: Partial<CodexUsageBreakdown>): CodexThreadTokenUsage {
  return { total: bd(total), last: bd(last) };
}

/** Microdollars, exact integer. */
function micro(usd: number | undefined): number | undefined {
  return usd === undefined ? undefined : Math.round(usd * 1e6);
}

/** The named model's entry off an aggregate, extracted with a presence assertion (the aggregate's
 *  indexed access is `T | undefined` under noUncheckedIndexedAccess). */
function get(agg: Record<string, CodexModelUsageEntry> | undefined, model: string): CodexModelUsageEntry {
  assert.ok(agg, "aggregate present");
  const entry = agg[model];
  assert.ok(entry, `entry for ${model}`);
  return entry;
}

/** Assert the token buckets (D5-derived uncached input, cache buckets, output, reasoning). */
function assertTokens(
  entry: CodexModelUsageEntry,
  want: { input: number; output: number; cacheRead: number; cacheCreation: number; reasoning: number },
): void {
  assert.equal(entry.inputTokens, want.input);
  assert.equal(entry.outputTokens, want.output);
  assert.equal(entry.cacheReadInputTokens, want.cacheRead);
  assert.equal(entry.cacheCreationInputTokens, want.cacheCreation);
  assert.equal(entry.reasoningOutputTokens, want.reasoning);
}

describe("subscription API-equivalent pricing", () => {
  it("gpt-6.1-sol identical responses have independent literal dollar parity with per-response tiers", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, "gpt-6.1-sol", false);
    const first = { inputTokens: 200000, cachedInputTokens: 60000, cacheWriteInputTokens: 10000, outputTokens: 200, reasoningOutputTokens: 50, totalTokens: 200200 };
    const second = { inputTokens: 300000, cachedInputTokens: 60000, cacheWriteInputTokens: 10000, outputTokens: 200, reasoningOutputTokens: 50, totalTokens: 300200 };
    acct.record(ROOT, usage(first, first));
    acct.record(ROOT, usage({ inputTokens: 500000, cachedInputTokens: 120000, cacheWriteInputTokens: 20000, outputTokens: 400, reasoningOutputTokens: 100, totalTokens: 500400 }, second));
    // Low: (130000*2 + 60000*.1 + 10000*2.5 + 200*10)/1e6 = .293.
    // High: (230000*4 + 60000*.2 + 10000*5 + 200*15)/1e6 = .985.
    for (const pricing of [API_KEY_EARLY, SUBSCRIPTION]) {
      const agg = acct.aggregateByModel(pricing);
      const entry = get(agg, "gpt-6.1-sol");
      assert.equal(entry.costStatus, "metered");
      assert.equal(micro(entry.costUSD), 1278000);
      assert.deepEqual(deriveCodexRunCost(agg), { kind: "metered", usd: 1.278, source: "price_table" });
    }
    assert.deepEqual(acct.aggregateByModel(SUBSCRIPTION), acct.aggregateByModel(API_KEY_EARLY));
    assert.equal(get(acct.aggregateByModel(), "gpt-6.1-sol").costStatus, "unreported");
    assert.deepEqual(deriveCodexRunCost(undefined), { kind: "unreported" });
  });

  for (const authMode of ["subscription", "api_key"] as const) {
    for (const scenario of ["unknown", "stale", "malformed", "cache split", "missed", "dominance", "incomplete resume"] as const) {
      it(`${authMode}: ${scenario} fails pricing closed`, () => {
        const acct = new CodexUsageAccountant();
        const model = scenario === "unknown" ? "gpt-5.5" : scenario === "stale" ? SOL : "gpt-6.1-sol";
        acct.registerThread(ROOT, model, scenario === "incomplete resume");
        const b = { inputTokens: 100, cachedInputTokens: scenario === "cache split" ? 101 : 0, outputTokens: 60, totalTokens: 160 };
        acct.record(ROOT, { ...usage(b, scenario === "missed" ? { inputTokens: 50, outputTokens: 30, totalTokens: 80 } : b), pricingEvidenceComplete: scenario !== "malformed" });
        if (scenario === "dominance") {
          acct.registerThread(CHILD, model, false);
          acct.record(CHILD, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 50, outputTokens: 30, totalTokens: 80 }));
        }
        const agg = acct.aggregateByModel({ authMode, now: new Date("2026-11-21T00:00:00Z") });
        if (scenario === "incomplete resume") assert.equal(agg, undefined);
        else {
          const entry = get(agg, model);
          assert.equal(entry.costStatus, "unreported");
          assert.ok(!("costUSD" in entry));
          assert.equal(entry.outputTokens, scenario === "dominance" ? 120 : 60);
        }
        assert.deepEqual(deriveCodexRunCost(agg), { kind: "unreported" });
      });
    }
  }
});

describe("CodexUsageAccountant cost: a fully-observed api-key run is metered at the Standard price", () => {
  it("prices a single reconciled response, deriving uncached input and keeping reasoning a subset", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    // input 1000 (cached 600, cacheWrite 100 → uncached 300), output 200 (incl. 50 reasoning).
    const b = { inputTokens: 1000, cachedInputTokens: 600, cacheWriteInputTokens: 100, outputTokens: 200, reasoningOutputTokens: 50, totalTokens: 1200 };
    acct.record(ROOT, usage(b, b));
    const agg = acct.aggregateByModel(API_KEY_EARLY);
    const astra = get(agg, ASTRA);
    assertTokens(astra, { input: 300, output: 200, cacheRead: 600, cacheCreation: 100, reasoning: 50 });
    assert.equal(astra.costStatus, "metered");
    // 300*10 + 600*1 + 100*12.5 + 200*50 = 14850 µ$. Reasoning (50) NOT added to output.
    assert.equal(micro(astra.costUSD), 14850);
    assert.deepEqual(deriveCodexRunCost(agg), { kind: "metered", usd: astra.costUSD!, source: "price_table" });
  });

  it("sums per-response prices across growing notes (cost is Σ last, tokens are the cumulative delta)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    acct.record(ROOT, usage({ inputTokens: 250, outputTokens: 150, totalTokens: 400 }, { inputTokens: 150, outputTokens: 90, totalTokens: 240 }));
    const astra = get(acct.aggregateByModel(API_KEY_EARLY), ASTRA);
    assertTokens(astra, { input: 250, output: 150, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
    assert.equal(astra.costStatus, "metered");
    // r1 = 100*10 + 60*50 = 4000; r2 = 150*10 + 90*50 = 6000 → 10000 µ$.
    assert.equal(micro(astra.costUSD), 10000);
  });

  it("prices a resumed thread from its post-baseline responses only", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, true); // resumed
    // Explicitly proved historical replay (total 500/300, last 100/50) establishes the baseline, no charge.
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }), true);
    // First genuinely-new post-resume response.
    acct.record(ROOT, usage({ inputTokens: 700, outputTokens: 400, totalTokens: 1100 }, { inputTokens: 200, outputTokens: 100, totalTokens: 300 }));
    const astra = get(acct.aggregateByModel(API_KEY_EARLY), ASTRA);
    assertTokens(astra, { input: 200, output: 100, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
    assert.equal(astra.costStatus, "metered");
    // Only the post-baseline response: 200*10 + 100*50 = 7000 µ$ (the prior leg is not re-charged).
    assert.equal(micro(astra.costUSD), 7000);
  });
});

describe("CodexUsageAccountant cost: a duplicate/redelivery never double-counts cost", () => {
  it("an exactly-redelivered note is neither charged nor priced twice (still reconciles, metered)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 })); // exact duplicate
    acct.record(ROOT, usage({ inputTokens: 250, outputTokens: 150, totalTokens: 400 }, { inputTokens: 150, outputTokens: 90, totalTokens: 240 }));
    const astra = get(acct.aggregateByModel(API_KEY_EARLY), ASTRA);
    assert.equal(astra.costStatus, "metered");
    // Only two responses priced (100/60 and 150/90) = 4000 + 6000 = 10000 µ$. A counted duplicate
    // would sum a third {100,60} response (extra 4000) AND break reconciliation to unreported.
    assert.equal(micro(astra.costUSD), 10000);
  });
});

describe("CodexUsageAccountant cost: an unreconciled gap keeps tokens but reports no cost", () => {
  it("a missed intermediate update degrades cost to unreported while retaining the cumulative tokens", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    // (intermediate note dropped) — the cumulative jumps but the responses cannot account for it.
    acct.record(ROOT, usage({ inputTokens: 700, outputTokens: 500, totalTokens: 1200 }, { inputTokens: 400, outputTokens: 300, totalTokens: 700 }));
    const agg = acct.aggregateByModel(API_KEY_EARLY);
    const astra = get(agg, ASTRA);
    // Tokens RETAINED from the cumulative delta (700 in / 500 out) — never fabricated or dropped.
    assertTokens(astra, { input: 700, output: 500, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
    assert.equal(astra.costStatus, "unreported");
    assert.ok(!("costUSD" in astra), "no costUSD on an unreconciled entry");
    assert.deepEqual(deriveCodexRunCost(agg), { kind: "unreported" });
  });

  it("an unknown model on an api-key run is unreported with tokens retained", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, UNKNOWN, false);
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    const unknown = get(acct.aggregateByModel(API_KEY_EARLY), UNKNOWN);
    assertTokens(unknown, { input: 100, output: 60, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
    assert.equal(unknown.costStatus, "unreported");
    assert.ok(!("costUSD" in unknown));
  });
});

describe("CodexUsageAccountant cost: mixed status across models in one run", () => {
  it("a priceable model is metered and an unknown model is unreported, side by side", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    acct.registerThread(CHILD, UNKNOWN, false);
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 500, outputTokens: 300, totalTokens: 800 }));
    acct.record(CHILD, usage({ inputTokens: 80, outputTokens: 40, totalTokens: 120 }, { inputTokens: 80, outputTokens: 40, totalTokens: 120 }));
    const agg = acct.aggregateByModel(API_KEY_EARLY);
    const astra = get(agg, ASTRA);
    const unknown = get(agg, UNKNOWN);
    assert.equal(astra.costStatus, "metered");
    assert.equal(micro(astra.costUSD), 500 * 10 + 300 * 50); // 20000 µ$
    assert.equal(unknown.costStatus, "unreported");
    assert.ok(!("costUSD" in unknown));
    assertTokens(unknown, { input: 80, output: 40, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
    // Run-level dominance: any unreported entry makes the whole run unreported.
    assert.deepEqual(deriveCodexRunCost(agg), { kind: "unreported" });
  });
});

describe("CodexUsageAccountant cost: the Sol review date flips only Sol", () => {
  function twoModelRun(): CodexUsageAccountant {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    acct.registerThread(CHILD, SOL, false);
    acct.record(ROOT, usage({ inputTokens: 1000, outputTokens: 200, totalTokens: 1200 }, { inputTokens: 1000, outputTokens: 200, totalTokens: 1200 }));
    acct.record(CHILD, usage({ inputTokens: 1000, outputTokens: 200, totalTokens: 1200 }, { inputTokens: 1000, outputTokens: 200, totalTokens: 1200 }));
    return acct;
  }

  it("2026-11-20: both gpt-6-astra and gpt-5.6-sol are metered", () => {
    const agg = twoModelRun().aggregateByModel({ authMode: "api_key", now: new Date("2026-11-20T23:59:59Z") });
    assert.equal(get(agg, ASTRA).costStatus, "metered");
    assert.equal(micro(get(agg, ASTRA).costUSD), 1000 * 10 + 200 * 50); // 20000 µ$
    assert.equal(get(agg, SOL).costStatus, "metered");
    assert.equal(micro(get(agg, SOL).costUSD), 1000 * 4 + 200 * 20); // 8000 µ$
    assert.equal(deriveCodexRunCost(agg).kind, "metered");
  });

  it("2026-11-21: gpt-5.6-sol becomes unreported (tokens retained), gpt-6-astra stays metered", () => {
    const agg = twoModelRun().aggregateByModel({ authMode: "api_key", now: new Date("2026-11-21T00:00:00Z") });
    const astra = get(agg, ASTRA);
    const sol = get(agg, SOL);
    assert.equal(astra.costStatus, "metered");
    assert.equal(micro(astra.costUSD), 20000);
    assert.equal(sol.costStatus, "unreported");
    assert.ok(!("costUSD" in sol));
    assertTokens(sol, { input: 1000, output: 200, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
    // One unreported entry dominates the run rollup.
    assert.deepEqual(deriveCodexRunCost(agg), { kind: "unreported" });
  });
});

describe("CodexUsageAccountant cost: conservative dominance across threads on the same model", () => {
  it("two reconciled threads on one model sum into one metered cost", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    acct.registerThread(CHILD, ASTRA, false);
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 }));
    acct.record(CHILD, usage({ inputTokens: 120, cachedInputTokens: 20, outputTokens: 40, totalTokens: 180 }, { inputTokens: 120, cachedInputTokens: 20, outputTokens: 40, totalTokens: 180 }));
    const agg = acct.aggregateByModel(API_KEY_EARLY);
    assert.ok(agg);
    assert.deepEqual(Object.keys(agg), [ASTRA]);
    const astra = get(agg, ASTRA);
    // tokens: uncached (300 + 100) = 400, cacheRead 20, output 240.
    assertTokens(astra, { input: 400, output: 240, cacheRead: 20, cacheCreation: 0, reasoning: 0 });
    assert.equal(astra.costStatus, "metered");
    // root 300*10 + 200*50 = 13000; child 100*10 + 20*1 + 40*50 = 3020 → 16020 µ$.
    assert.equal(micro(astra.costUSD), 16020);
  });

  it("if ANY thread on a model fails to reconcile, the whole model entry is unreported with tokens retained", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    acct.registerThread(CHILD, ASTRA, false);
    // A cleanly reconciled thread…
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 }));
    // …and a second thread on the SAME model with a dropped intermediate note.
    acct.record(CHILD, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    acct.record(CHILD, usage({ inputTokens: 700, outputTokens: 500, totalTokens: 1200 }, { inputTokens: 400, outputTokens: 300, totalTokens: 700 }));
    const astra = get(acct.aggregateByModel(API_KEY_EARLY), ASTRA);
    assert.equal(astra.costStatus, "unreported");
    assert.ok(!("costUSD" in astra));
    // Tokens still summed and retained: uncached 300 + 700 = 1000, output 200 + 500 = 700.
    assertTokens(astra, { input: 1000, output: 700, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });
});

describe("CodexUsageAccountant cost: per-response dominance within ONE reconciled thread", () => {
  it("a single individually-unpriceable response on an otherwise-reconciling thread taints the whole model to unreported, tokens retained", () => {
    // Distinct from the two-thread dominance block above and from every whole-model-unpriceable
    // case (unknown model / Sol flip): here ONE thread has TWO adopted responses whose SUMMED
    // cumulative reconciles the priced buckets, but response 2 is INDIVIDUALLY unpriceable — its own
    // cache detail buckets exceed its own input (40 + 30 > 50), the impossible split
    // priceCodexResponse refuses. Per-response conservative dominance must taint the WHOLE model to
    // `unreported`; it must NOT silently price only response 1 and emit a too-low `metered` figure.
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, ASTRA, false);
    // Response 1: cleanly priceable (uncached-only).
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    // Response 2: the cumulative grows so the note is ADOPTED (magnitude 250 > 160) and Σ last
    // reconciles the delta on all four priced buckets (input 150, cached 40, cacheWrite 30, output
    // 100) — yet last2 ALONE is unpriceable (cached 40 + cacheWrite 30 > input 50).
    acct.record(
      ROOT,
      usage(
        { inputTokens: 150, cachedInputTokens: 40, cacheWriteInputTokens: 30, outputTokens: 100, totalTokens: 260 },
        { inputTokens: 50, cachedInputTokens: 40, cacheWriteInputTokens: 30, outputTokens: 40, totalTokens: 120 },
      ),
    );
    const agg = acct.aggregateByModel(API_KEY_EARLY);
    const astra = get(agg, ASTRA);
    assert.equal(astra.costStatus, "unreported", "one individually-unpriceable response taints the whole model");
    assert.ok(!("costUSD" in astra), "no costUSD on an unreported entry — never a too-low metered $ from pricing only response 1");
    // Tokens RETAINED from the cumulative delta: uncached 150-40-30 = 80, cacheRead 40, cacheCreation 30, output 100.
    assertTokens(astra, { input: 80, output: 100, cacheRead: 40, cacheCreation: 30, reasoning: 0 });
    // Run-level rollup: the unreported entry dominates.
    assert.deepEqual(deriveCodexRunCost(agg), { kind: "unreported" });
  });
});

describe("deriveCodexRunCost: api-key rollup edges", () => {
  it("no usage on an api-key run is unreported (never a metered $0)", () => {
    assert.deepEqual(deriveCodexRunCost(undefined), { kind: "unreported" });
  });

  it("all-metered entries sum into one metered run cost", () => {
    const entries: Record<string, CodexModelUsageEntry> = {
      [ASTRA]: { inputTokens: 1, outputTokens: 1, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "metered", costUSD: 0.5 },
      [SOL]: { inputTokens: 1, outputTokens: 1, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "metered", costUSD: 0.25 },
    };
    // 0.5 + 0.25 = 0.75 is exact in binary float, so an equality assert is safe here.
    assert.deepEqual(deriveCodexRunCost(entries), { kind: "metered", usd: 0.75, source: "price_table" });
  });

  it("any unreported entry makes the run unreported even alongside metered entries", () => {
    const entries: Record<string, CodexModelUsageEntry> = {
      [ASTRA]: { inputTokens: 1, outputTokens: 1, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "metered", costUSD: 0.5 },
      [SOL]: { inputTokens: 1, outputTokens: 1, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "unreported" },
    };
    assert.deepEqual(deriveCodexRunCost(entries), { kind: "unreported" });
  });
});
