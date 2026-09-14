import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { CodexUsageAccountant, deriveCodexRunCost, type CodexModelUsageEntry } from "../src/codex/token-accounting.js";
import type { CodexThreadTokenUsage, CodexUsageBreakdown } from "../src/codex/transport.js";

// PRD #1332 (M5A, C4a) — the per-thread/per-model token accountant, exercised directly (no
// harness, no transport). These are the reconciliation proofs D5 demands: a duplicate, stale
// resume replay or out-of-order note cannot increase usage; a missed update is recovered from
// the cumulative; an unknown thread is never attributed to the root; a mixed-model child
// aggregates per actual model; redelivery is idempotent. Every emitted entry is `unreported`
// (C4a prices nothing) and carries NO costUSD.

const MODEL_ROOT = "gpt-6-astra";
const MODEL_CHILD = "gpt-5.6-sol";
const ROOT = "th-root";

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

/** Like {@link usage} but carries the m3 (CodeRabbit 4004800884) pricing-evidence flag. `usage`
 *  itself omits it (undefined = lenient/complete, the "present-but-partial is usable" default). */
function usageEvidence(
  total: Partial<CodexUsageBreakdown>,
  last: Partial<CodexUsageBreakdown>,
  pricingEvidenceComplete: boolean,
): CodexThreadTokenUsage {
  return { total: bd(total), last: bd(last), pricingEvidenceComplete };
}

/** Assert an emitted entry equals the given token buckets and is an unreported, price-free
 *  record. Uses a bare-object comparison so a stray `costUSD` (which C4a must NOT emit) fails. */
function assertEntry(
  actual: CodexModelUsageEntry | undefined,
  want: { input: number; output: number; cacheRead: number; cacheCreation: number; reasoning: number },
): void {
  assert.deepEqual(actual, {
    inputTokens: want.input,
    outputTokens: want.output,
    cacheReadInputTokens: want.cacheRead,
    cacheCreationInputTokens: want.cacheCreation,
    reasoningOutputTokens: want.reasoning,
    costStatus: "unreported",
  });
  // Belt-and-braces: the closed marker is present and no dollar figure rode along.
  assert.ok(actual !== undefined && !("costUSD" in actual), "no costUSD in a C4a entry");
}

describe("CodexUsageAccountant: a fresh thread charges its whole cumulative", () => {
  it("maps the six wire buckets, deriving uncached input and keeping reasoning as a subset", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    // input is the TOTAL input; uncached = 1000 - 600 - 100 = 300. output already includes
    // the 50 reasoning tokens (carried, never re-added).
    acct.record(ROOT, usage(
      { inputTokens: 1000, cachedInputTokens: 600, cacheWriteInputTokens: 100, outputTokens: 200, reasoningOutputTokens: 50, totalTokens: 1200 },
      { inputTokens: 1000, cachedInputTokens: 600, cacheWriteInputTokens: 100, outputTokens: 200, reasoningOutputTokens: 50, totalTokens: 1200 },
    ));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 300, output: 200, cacheRead: 600, cacheCreation: 100, reasoning: 50 });
  });

  it("charges the full cumulative across several growing notes (no summing of `last`)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    acct.record(ROOT, usage({ inputTokens: 250, outputTokens: 150, totalTokens: 400 }, { inputTokens: 150, outputTokens: 90, totalTokens: 240 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    // Cumulative wins: 250 input / 150 output, NOT 100+250 or 60+150 (which summing `last`/`total` would give).
    assertEntry(agg[MODEL_ROOT], { input: 250, output: 150, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });
});

describe("CodexUsageAccountant: duplicate / out-of-order / missed-update", () => {
  it("a duplicate note cannot increase usage", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    const note = usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 });
    acct.record(ROOT, note);
    acct.record(ROOT, note); // exact redelivery
    acct.record(ROOT, note); // and again
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 100, output: 60, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("an out-of-order (older, lower cumulative) note cannot increase usage", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 180, totalTokens: 480 }, { inputTokens: 300, outputTokens: 180, totalTokens: 480 }));
    // A stale note arriving late with a LOWER cumulative — must be ignored, not add to totals.
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 300, output: 180, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("a stale note cannot inflate a single bucket even if that bucket is higher", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 100, totalTokens: 600 }, { inputTokens: 500, outputTokens: 100, totalTokens: 600 }));
    // A stale/lower-magnitude note whose OUTPUT bucket alone is higher — adopting it per-bucket
    // would over-count output. Magnitude gating keeps the whole newer breakdown.
    acct.record(ROOT, usage({ inputTokens: 50, outputTokens: 400, totalTokens: 450 }, { inputTokens: 50, outputTokens: 400, totalTokens: 450 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 500, output: 100, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("an EQUAL-magnitude note with a DIFFERENT bucket composition is REJECTED (strictly exceeds, not >=)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    // First note: magnitude = max(500, 300+200) = 500.
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 }));
    // Second note: SAME magnitude (max(500, 100+400) = 500) but buckets reallocated (input↓,
    // output↑). Adoption is gated on `m > maxMagnitude`, so an EQUAL magnitude must NOT replace
    // the breakdown; the first note's values are retained. A `>=` gate would wrongly adopt this,
    // reporting 100/400. This pins the strictly-exceeds boundary no duplicate/redelivery test can.
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 400, totalTokens: 500 }, { inputTokens: 100, outputTokens: 400, totalTokens: 500 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 300, output: 200, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("a missed intermediate update is recovered from the later cumulative", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    // We see the first note, MISS the second, then see the third — the cumulative captures the gap.
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    // (intermediate total=300/… never delivered)
    acct.record(ROOT, usage({ inputTokens: 700, outputTokens: 500, totalTokens: 1200 }, { inputTokens: 400, outputTokens: 300, totalTokens: 700 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 700, output: 500, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });
});

describe("CodexUsageAccountant: resumed threads baseline at the full restored cumulative", () => {
  it("baselines at the FULL restored `total` so the prior leg's final response is NOT double-counted (real resume shape: initial replay carries a non-zero `last`)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true); // resumed
    // REAL resume behavior (pinned app-server token_usage_replay.rs): the FIRST note on resume is
    // the replayed snapshot of the restored TokenUsageInfo — total = the prior cumulative AND
    // `last` = the prior leg's FINAL response (NON-ZERO, a subset already in `total`). The baseline
    // must be the full `total` ({in:500, out:300}); a `total - last` baseline ({in:400, out:250})
    // would sit below the prior cumulative and re-charge the prior leg's last response.
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }));
    // The first genuinely-new response after resume adds {in:200, out:100}.
    acct.record(ROOT, usage({ inputTokens: 700, outputTokens: 400, totalTokens: 1100 }, { inputTokens: 200, outputTokens: 100, totalTokens: 300 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    // Only this leg's genuinely-new work: 700-500 input, 400-300 output. The old `total - last`
    // rule would report 300/150 here — double-counting the prior leg's 100/50 last response.
    assertEntry(agg[MODEL_ROOT], { input: 200, output: 100, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("a stale resume replay after the baseline cannot increase usage", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }));
    acct.record(ROOT, usage({ inputTokens: 700, outputTokens: 400, totalTokens: 1100 }, { inputTokens: 200, outputTokens: 100, totalTokens: 300 }));
    // A resume replay redelivers an EARLIER cumulative snapshot — must not increase usage.
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 200, output: 100, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("baselines at `total` irrespective of `last` (a hypothetical last=0 snapshot charges the same)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    // The resume baseline is the full `total` regardless of `last`, so a last=0 first note (were
    // one ever emitted) baselines at total = {in:400, out:250} — the same rule as the non-zero case.
    acct.record(ROOT, usage({ inputTokens: 400, outputTokens: 250, totalTokens: 650 }, {}));
    // First new response after resume.
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 100, output: 50, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });
});

describe("CodexUsageAccountant: unknown-thread attribution", () => {
  it("an unknown/unregistered thread is dropped and never added to the root model", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    // A note for a thread the accountant never registered — a stray/foreign/stale id.
    acct.record("th-unknown", usage({ inputTokens: 9999, outputTokens: 9999, totalTokens: 19998 }, { inputTokens: 9999, outputTokens: 9999, totalTokens: 19998 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    // The root model reflects ONLY its own note; the unknown thread contributes nothing and
    // no phantom model appears.
    assert.deepEqual(Object.keys(agg), [MODEL_ROOT]);
    assertEntry(agg[MODEL_ROOT], { input: 100, output: 60, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });
});

describe("CodexUsageAccountant: aggregation by actual configured model", () => {
  it("a mixed-model child is charged to its own model, not the root's", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.registerThread("th-child", MODEL_CHILD, false);
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 }));
    acct.record("th-child", usage({ inputTokens: 80, outputTokens: 40, totalTokens: 120 }, { inputTokens: 80, outputTokens: 40, totalTokens: 120 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assert.deepEqual(new Set(Object.keys(agg)), new Set([MODEL_ROOT, MODEL_CHILD]));
    assertEntry(agg[MODEL_ROOT], { input: 300, output: 200, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
    assertEntry(agg[MODEL_CHILD], { input: 80, output: 40, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("two threads on the SAME model sum into one entry", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.registerThread("th-child", MODEL_ROOT, false); // child also on the root model
    acct.record(ROOT, usage({ inputTokens: 300, cachedInputTokens: 100, outputTokens: 200, totalTokens: 600 }, { inputTokens: 300, cachedInputTokens: 100, outputTokens: 200, totalTokens: 600 }));
    acct.record("th-child", usage({ inputTokens: 120, cachedInputTokens: 20, outputTokens: 40, totalTokens: 180 }, { inputTokens: 120, cachedInputTokens: 20, outputTokens: 40, totalTokens: 180 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assert.deepEqual(Object.keys(agg), [MODEL_ROOT]);
    // uncached: (300-100) + (120-20) = 300; cacheRead: 100+20 = 120; output: 200+40 = 240.
    assertEntry(agg[MODEL_ROOT], { input: 300, output: 240, cacheRead: 120, cacheCreation: 0, reasoning: 0 });
  });
});

describe("CodexUsageAccountant: redelivery and empties", () => {
  it("aggregate is idempotent and a redelivered final snapshot changes nothing", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    acct.record(ROOT, usage({ inputTokens: 250, outputTokens: 150, totalTokens: 400 }, { inputTokens: 150, outputTokens: 90, totalTokens: 240 }));
    const first = acct.aggregateByModel();
    // The result frame is redelivered: the final usage snapshot arrives again.
    acct.record(ROOT, usage({ inputTokens: 250, outputTokens: 150, totalTokens: 400 }, { inputTokens: 150, outputTokens: 90, totalTokens: 240 }));
    const second = acct.aggregateByModel();
    const third = acct.aggregateByModel();
    assert.deepEqual(first, second);
    assert.deepEqual(second, third);
  });

  it("no notes at all yields undefined (so the terminal omits modelUsage entirely)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    assert.equal(acct.aggregateByModel(), undefined);
  });

  it("a registered thread whose reconciled delta is entirely zero is omitted", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    // Only an initial resume snapshot with last=0 — baseline = total, so charged is zero.
    acct.record(ROOT, usage({ inputTokens: 400, outputTokens: 250, totalTokens: 650 }, {}));
    assert.equal(acct.aggregateByModel(), undefined);
  });

  it("the thread->model map is immutable: a repeat registration cannot rebind the model", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.registerThread(ROOT, MODEL_CHILD, false); // ignored
    acct.record(ROOT, usage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }));
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assert.deepEqual(Object.keys(agg), [MODEL_ROOT]);
  });
});

describe("CodexUsageAccountant: m3 fail-closed pricing on malformed required evidence (CodeRabbit 4004800884)", () => {
  // MODEL_ROOT = "gpt-6-astra" is a Standard-priced model, so a well-shaped note meters. These
  // pin the m3 gate: a note flagged pricingEvidenceComplete:false degrades cost to `unreported`
  // even when the numeric reconciliation matches, while token totals still survive; and a flag
  // that is absent (undefined) or true keeps the existing lenient/metered behavior.
  const API_KEY = { authMode: "api_key" as const, now: new Date("2026-01-01T00:00:00Z") };
  // input 300 / output 200 on gpt-6-astra: 300*10 + 200*50 = 13000 µ$ when metered.
  const B = { inputTokens: 300, outputTokens: 200, totalTokens: 500 };

  it("BASELINE: the same well-shaped, reconciling note meters when evidence is COMPLETE", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usageEvidence(B, B, true));
    const agg = acct.aggregateByModel(API_KEY);
    assert.ok(agg);
    const astra = agg[MODEL_ROOT]!;
    assert.equal(astra.costStatus, "metered", "a complete, reconciling api-key note meters");
    assert.equal(Math.round(astra.costUSD! * 1e6), 13000, "300*10 + 200*50 µ$");
  });

  it("malformed required evidence → costStatus 'unreported' EVEN THOUGH the numeric reconciliation matches; tokens survive", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    // Identical buckets to the baseline (so sum(last) === charged delta — reconciliation PASSES),
    // but a pricing-required bucket was present-but-malformed on the wire (flag false). The gate
    // must fail cost closed anyway — the CodeRabbit-4004800884 understated-metered bug.
    acct.record(ROOT, usageEvidence(B, B, false));
    const agg = acct.aggregateByModel(API_KEY);
    assert.ok(agg);
    const astra = agg[MODEL_ROOT]!;
    // Cost fails closed.
    assert.equal(astra.costStatus, "unreported", "malformed evidence degrades cost to unreported despite a matching reconciliation");
    assert.ok(!("costUSD" in astra), "no dollar figure on an unreported entry");
    // Token totals are RETAINED (no token-accounting regression).
    assert.equal(astra.inputTokens, 300, "uncached input retained");
    assert.equal(astra.outputTokens, 200, "output retained");
    // Run-level rollup: an unreported entry makes the api-key run unreported.
    assert.deepEqual(deriveCodexRunCost(agg, "api_key"), { kind: "unreported" });
  });

  it("a subscription run is unaffected by the evidence flag (still subscription, tokens retained)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usageEvidence(B, B, false));
    const astra = acct.aggregateByModel({ authMode: "subscription", now: API_KEY.now })![MODEL_ROOT]!;
    assert.equal(astra.costStatus, "subscription", "the evidence gate only applies to the api-key pricing branch");
    assert.equal(astra.inputTokens, 300);
  });

  it("a trusted pre-decoded usage object without the optional evidence flag stays backward-compatible", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    // The direct helper fills every numeric bucket and omits only the optional decoder flag. Real
    // transport frames always set that flag, including false when any required bucket is absent.
    acct.record(ROOT, usage(B, B));
    const astra = acct.aggregateByModel(API_KEY)![MODEL_ROOT]!;
    assert.equal(astra.costStatus, "metered", "an absent flag defaults to complete → metered");
    assert.equal(Math.round(astra.costUSD! * 1e6), 13000);
  });

  it("dominance: one tainted thread degrades its whole model; a clean thread on ANOTHER model still meters", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.registerThread("th-clean", MODEL_CHILD, false);
    acct.record(ROOT, usageEvidence(B, B, false)); // tainted astra
    acct.record("th-clean", usageEvidence({ inputTokens: 100, outputTokens: 100, totalTokens: 200 }, { inputTokens: 100, outputTokens: 100, totalTokens: 200 }, true));
    const agg = acct.aggregateByModel(API_KEY);
    assert.ok(agg);
    assert.equal(agg[MODEL_ROOT]!.costStatus, "unreported", "the tainted model is unreported");
    assert.equal(agg[MODEL_CHILD]!.costStatus, "metered", "an independent clean model still meters");
    assert.equal(agg[MODEL_ROOT]!.inputTokens, 300, "tainted model still retains its tokens");
  });

  it("a tainting note that is NOT adopted (stale/out-of-order) does not degrade a clean thread", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    // Adopt a clean, higher-magnitude note first.
    acct.record(ROOT, usageEvidence(B, B, true));
    // A stale, LOWER-magnitude malformed note: not adopted, so it must not taint pricing.
    acct.record(ROOT, usageEvidence({ inputTokens: 10, outputTokens: 5, totalTokens: 15 }, { inputTokens: 10, outputTokens: 5, totalTokens: 15 }, false));
    const astra = acct.aggregateByModel(API_KEY)![MODEL_ROOT]!;
    assert.equal(astra.costStatus, "metered", "a stale (unadopted) malformed note cannot taint the thread");
    assert.equal(Math.round(astra.costUSD! * 1e6), 13000);
  });
});
