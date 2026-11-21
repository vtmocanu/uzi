import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { CodexUsageAccountant, deriveCodexRunCost, type CodexModelUsageEntry } from "../src/codex/token-accounting.js";
import type { CodexThreadTokenUsage, CodexUsageBreakdown } from "../src/codex/transport.js";

// PRD #1332 (M5A, C4a) — the per-thread/per-model token accountant, exercised directly (no
// harness, no transport). These are the reconciliation proofs D5 demands: a duplicate, stale
// resume replay or out-of-order note cannot increase usage; a missed update is recovered from
// the cumulative; an unknown thread is never attributed to the root; a mixed-model child
// aggregates per actual model; redelivery is idempotent. Token-only cases (no pricing
// context) emit `unreported` entries with NO costUSD; the pricing-aware controls also assert
// metered entries.

const MODEL_ROOT = "gpt-6-astra";
const MODEL_CHILD = "gpt-5.6-sol";
const ROOT = "th-root";

describe("closed usage diagnostic formatting", () => {
  const prefix = "Codex token accounting is incomplete: invalid raw token usage. The proved baseline is retained; proven usage remains available. Cost is unreported for the affected model. Execution continues.";
  it("appends a typed diagnostic and retains the first notice and claim latch", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.markIncomplete(ROOT, "invalid_raw_usage", false,
      { predicate: "missing bucket", breakdown: "last", bucket: "outputTokens" });
    assert.equal(acct.incompleteNoticeText, prefix + " Diagnostic: missing bucket (last.outputTokens).");
    assert.equal(acct.takeIncompleteNotice(), true);
    acct.markIncomplete(ROOT, "invalid_raw_usage", false, { predicate: "missing container" });
    assert.equal(acct.incompleteNoticeText, prefix + " Diagnostic: missing bucket (last.outputTokens).");
    assert.equal(acct.takeIncompleteNotice(), false);
  });
  it("selects only fixed literals even when an untyped caller supplies arbitrary fields", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    const diagnostic = { predicate: "missing bucket", breakdown: "provider-breakdown-sentinel",
      bucket: "provider-key-sentinel", id: "provider-id-sentinel", value: "provider-value-sentinel" };
    acct.markIncomplete(ROOT, "invalid_raw_usage", false,
      diagnostic as unknown as NonNullable<Parameters<CodexUsageAccountant["markIncomplete"]>[3]>);
    assert.equal(acct.incompleteNoticeText, prefix + " Diagnostic: missing bucket.");
    const other = new CodexUsageAccountant();
    other.registerThread(ROOT, MODEL_ROOT, false);
    other.markIncomplete(ROOT, "invalid_raw_usage", false,
      { predicate: "provider-predicate-sentinel" } as unknown as NonNullable<Parameters<CodexUsageAccountant["markIncomplete"]>[3]>);
    assert.equal(other.incompleteNoticeText, prefix);
  });
  it("ignores diagnostics for other causes", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.markIncomplete(ROOT, "invalid_identity", false, { predicate: "missing container" });
    assert.equal(acct.incompleteNoticeText, prefix.replace("invalid raw token usage", "invalid token usage identity"));
  });
});

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

// #2878 M2: guard counter loss reproducibly; no historical incident raw frame is available.
// Resume checks live in e2e/codex-m4/token-resume.test.ts and large-resume.test.ts:
// replay must precede turn/started. Neither proves compaction caused the incident.
describe("#2878 M2 accountant", () => {
  const pricing = { authMode: "api_key" as const, now: new Date("2026-10-13") };
  const note = (inputTokens: number, outputTokens: number) =>
    usage({ inputTokens, outputTokens, totalTokens: inputTokens + outputTokens },
      { inputTokens, outputTokens, totalTokens: inputTokens + outputTokens });
  const entry = (inputTokens: number, outputTokens: number) => ({
    inputTokens, outputTokens, cacheReadInputTokens: 0, cacheCreationInputTokens: 0,
    reasoningOutputTokens: 0, costStatus: "unreported",
  });

  for (const sameModel of [false, true]) {
    it(`regression retained child recorded before root incomplete: ${sameModel ? "same model" : "distinct model"}`, () => {
      const acct = new CodexUsageAccountant();
      acct.registerThread(ROOT, MODEL_ROOT, true);
      const childModel = sameModel ? MODEL_ROOT : "gpt-6.1-sol";
      acct.registerThread("child", childModel, false);
      acct.record("child", note(200, 100)); // FIRST: already proved child work must survive.
      acct.record(ROOT, note(600, 180)); // resumed root has no proved baseline
      const expected = sameModel
        ? { [MODEL_ROOT]: entry(200, 100) }
        : { [MODEL_ROOT]: entry(0, 0), [childModel]: {
          ...entry(200, 100), costStatus: "metered", costUSD: 0.0014,
        } };
      assert.deepEqual(acct.aggregateByModel(pricing), expected,
        "incomplete root retains child counters and only taints its own model");
      assert.equal(acct.takeIncompleteNotice(), true);
      acct.markIncomplete(ROOT, "invalid_raw_usage");
      assert.equal(acct.takeIncompleteNotice(), false, "notice remains claim-wide once-only");
    });
  }

  it("regression advancing snapshot with decreasing output rejects whole snapshot", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    acct.record(ROOT, note(1000, 200), true);
    acct.record(ROOT, usage({ inputTokens: 1100, outputTokens: 220, totalTokens: 1320 },
      { inputTokens: 100, outputTokens: 20, totalTokens: 120 }));
    const rejected = acct.record(ROOT, usage({ inputTokens: 1120, outputTokens: 210, totalTokens: 1330 },
      { inputTokens: 20, outputTokens: 0, totalTokens: 20 }));
    assert.equal(rejected, undefined, "regressing advancing snapshot cannot become a priced response");
    assert.equal(acct.usageIncomplete, true);
    assert.deepEqual(acct.aggregateByModel(pricing), { [MODEL_ROOT]: entry(100, 20) });
    // A later valid snapshot advances from the last proved counters, not the rejected snapshot.
    acct.record(ROOT, usage({ inputTokens: 1130, outputTokens: 230, totalTokens: 1360 },
      { inputTokens: 30, outputTokens: 10, totalTokens: 40 }));
    assert.deepEqual(acct.aggregateByModel(pricing), { [MODEL_ROOT]: entry(130, 30) });
  });

  for (const cacheBucket of ["cachedInputTokens", "cacheWriteInputTokens"] as const) {
    for (const alreadyIncomplete of [false, true]) {
      it(`regression advancing uncached input decrease rejects whole snapshot: ${cacheBucket}, ${alreadyIncomplete ? "already incomplete" : "initially complete"}`, () => {
        const acct = new CodexUsageAccountant();
        acct.registerThread(ROOT, MODEL_ROOT, true);
        acct.record(ROOT, usage(
          { inputTokens: 1000, cachedInputTokens: 200, cacheWriteInputTokens: 100, outputTokens: 200, totalTokens: 1200 },
          { inputTokens: 1000, cachedInputTokens: 200, cacheWriteInputTokens: 100, outputTokens: 200, totalTokens: 1200 },
        ), true);
        acct.record(ROOT, usage(
          { inputTokens: 1100, cachedInputTokens: 220, cacheWriteInputTokens: 110, outputTokens: 220, totalTokens: 1320 },
          { inputTokens: 100, cachedInputTokens: 20, cacheWriteInputTokens: 10, outputTokens: 20, totalTokens: 120 },
        ));
        assert.equal(acct.usageIncomplete, false);
        const before = acct.aggregateByModel();
        if (alreadyIncomplete) acct.markIncomplete(ROOT, "invalid_raw_usage");
        const rejected = acct.record(ROOT, usage(
          { inputTokens: 1120, cachedInputTokens: 220, cacheWriteInputTokens: 110, outputTokens: 230,
            totalTokens: 1350, [cacheBucket]: cacheBucket === "cachedInputTokens" ? 300 : 190 },
          { inputTokens: 20, cachedInputTokens: 0, cacheWriteInputTokens: 0, outputTokens: 10,
            totalTokens: 30, [cacheBucket]: 10 },
        ));
        assert.equal(rejected, undefined, "advancing uncached input decrease cannot become a priced response");
        assert.equal(acct.usageIncomplete, true);
        assertEntry(acct.aggregateByModel(pricing)?.[MODEL_ROOT],
          { input: 70, cacheRead: 20, cacheCreation: 10, output: 20, reasoning: 0 });
        assert.deepEqual(acct.aggregateByModel(), before, "accepted lower bound survives rejection");
        const adopted = acct.record(ROOT, usage(
          { inputTokens: 1130, cachedInputTokens: 225, cacheWriteInputTokens: 115, outputTokens: 240, totalTokens: 1370 },
          { inputTokens: 30, cachedInputTokens: 5, cacheWriteInputTokens: 5, outputTokens: 20, totalTokens: 50 },
        ));
        assert.ok(adopted, "valid continuation advances from preserved accepted snapshot");
        assertEntry(acct.aggregateByModel(pricing)?.[MODEL_ROOT],
          { input: 90, cacheRead: 25, cacheCreation: 15, output: 40, reasoning: 0 });
        assert.equal(acct.usageIncomplete, true);
      });
    }

    for (const equalMagnitude of [true, false]) {
      it(`compatibility uncached input decrease in ${equalMagnitude ? "equal magnitude" : "low magnitude reset"}: ${cacheBucket}`, () => {
        const acct = new CodexUsageAccountant();
        acct.registerThread(ROOT, MODEL_ROOT, true);
        acct.record(ROOT, usage(
          { inputTokens: 1000, cachedInputTokens: 200, cacheWriteInputTokens: 100, outputTokens: 200, totalTokens: 1200 },
          { inputTokens: 1000, cachedInputTokens: 200, cacheWriteInputTokens: 100, outputTokens: 200, totalTokens: 1200 },
        ), true);
        acct.record(ROOT, usage(
          { inputTokens: 1100, cachedInputTokens: 220, cacheWriteInputTokens: 110, outputTokens: 220, totalTokens: 1320 },
          { inputTokens: 100, cachedInputTokens: 20, cacheWriteInputTokens: 10, outputTokens: 20, totalTokens: 120 },
        ));
        const before = acct.aggregateByModel(pricing);
        assert.equal(acct.usageIncomplete, false);
        const total = equalMagnitude
          ? { inputTokens: 1120, cachedInputTokens: 220, cacheWriteInputTokens: 110, outputTokens: 200,
            totalTokens: 1320, [cacheBucket]: cacheBucket === "cachedInputTokens" ? 300 : 190 }
          : { inputTokens: 20, cachedInputTokens: 5, cacheWriteInputTokens: 5, outputTokens: 2, totalTokens: 22 };
        assert.equal(acct.record(ROOT, usage(total, total)), undefined);
        assert.deepEqual(acct.aggregateByModel(pricing), before, "nonadvancing note preserves counters and cost status");
        assert.equal(acct.usageIncomplete, false);
      });
    }
  }

  it("regression missing live baseline quarantines attempted work despite later replay", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    acct.record(ROOT, note(600, 180));
    acct.record(ROOT, note(600, 180), true);
    assert.equal(acct.hasBaseline(ROOT), false, "later replay cannot baseline away attempted work");
    acct.record(ROOT, note(700, 210));
    assert.deepEqual(acct.aggregateByModel(pricing), { [MODEL_ROOT]: entry(0, 0) });
  });

  it("regression historical malformation can recover proved baseline but never reported cost", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    acct.markIncomplete(ROOT, "invalid_raw_usage"); // invalid historical note rejected by harness, before attempted work
    acct.record(ROOT, note(1000, 200), true);
    assert.equal(acct.hasBaseline(ROOT), true);
    acct.record(ROOT, usage({ inputTokens: 1100, outputTokens: 220, totalTokens: 1320 },
      { inputTokens: 100, outputTokens: 20, totalTokens: 120 }));
    assert.deepEqual(acct.aggregateByModel(pricing), { [MODEL_ROOT]: entry(100, 20) });
  });

  for (const total of [[1110, 210], [10, 2]]) {
    it(`compatibility nonadvancing composition ${total.join("/")} leaves counters and price unchanged`, () => {
      const acct = new CodexUsageAccountant();
      acct.registerThread(ROOT, MODEL_ROOT, true);
      acct.record(ROOT, note(1000, 200), true);
      acct.record(ROOT, usage({ inputTokens: 1100, outputTokens: 220, totalTokens: 1320 },
        { inputTokens: 100, outputTokens: 20, totalTokens: 120 }));
      const before = acct.aggregateByModel(pricing);
      acct.record(ROOT, note(total[0]!, total[1]!));
      assert.equal(acct.usageIncomplete, false, "matching active turn is not freshness evidence");
      assert.deepEqual(acct.aggregateByModel(pricing), before);
      assert.deepEqual(before, { [MODEL_ROOT]: { ...entry(100, 20), costStatus: "metered", costUSD: 0.002 } });
    });
  }
});

describe("CodexUsageAccountant: proved replay and incomplete claims", () => {
  it("never guesses total-last without replay, and later replay cannot clear incomplete", () => {
    const lowerBound = { [MODEL_ROOT]: {
      inputTokens: 0, outputTokens: 0, cacheReadInputTokens: 0, cacheCreationInputTokens: 0,
      reasoningOutputTokens: 0, costStatus: "unreported",
    } };
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    acct.record(ROOT, usage({ inputTokens: 600, totalTokens: 600 }, { inputTokens: 300, totalTokens: 300 }));
    assert.equal(acct.usageIncomplete, true);
    assert.deepEqual(acct.aggregateByModel({ authMode: "subscription", now: new Date("2026-10-05") }), lowerBound);
    assert.equal(acct.takeIncompleteNotice(), true);
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.registerThread("later-child", MODEL_CHILD, false);
    acct.record(ROOT, usage({ inputTokens: 300, totalTokens: 300 }, { inputTokens: 200, totalTokens: 200 }), true);
    assert.equal(acct.usageIncomplete, true);
    assert.deepEqual(acct.aggregateByModel(), lowerBound);
    assert.equal(acct.takeIncompleteNotice(), false);
  });

  it("ready baseline survives no-replay recreation with model, max and priced responses intact", () => {
    const acct = new CodexUsageAccountant();
    assert.equal(acct.hasBaseline(ROOT), false);
    acct.registerThread(ROOT, MODEL_ROOT, true);
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 90, totalTokens: 390 }, { inputTokens: 200, outputTokens: 60, totalTokens: 260 }), true);
    assert.equal(acct.hasBaseline(ROOT), true);
    acct.record(ROOT, usage({ inputTokens: 600, outputTokens: 180, totalTokens: 780 }, { inputTokens: 300, outputTokens: 90, totalTokens: 390 }));
    acct.registerThread(ROOT, MODEL_CHILD, true);
    // Recreation has no historical replay; duplicate/stale notes must preserve both max and responses.
    acct.record(ROOT, usage({ inputTokens: 600, outputTokens: 180, totalTokens: 780 }, { inputTokens: 300, outputTokens: 90, totalTokens: 390 }));
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 90, totalTokens: 390 }, { inputTokens: 200, outputTokens: 60, totalTokens: 260 }));
    acct.record(ROOT, usage({ inputTokens: 1000, outputTokens: 300, totalTokens: 1300 }, { inputTokens: 400, outputTokens: 120, totalTokens: 520 }));
    assert.equal(acct.usageIncomplete, false);
    assert.deepEqual(acct.aggregateByModel({ authMode: "api_key", now: new Date("2026-10-05") }), {
      [MODEL_ROOT]: { inputTokens: 700, outputTokens: 210, cacheReadInputTokens: 0,
        cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "metered", costUSD: 0.0175 },
    });
  });

  it("same accountant retains original baseline and does not price replay last on recreation", () => {
    const acct = new CodexUsageAccountant();
    const first = usage({ inputTokens: 100, outputTokens: 30, totalTokens: 130 }, { inputTokens: 100, outputTokens: 30, totalTokens: 130 });
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, first);
    acct.registerThread(ROOT, MODEL_ROOT, true);
    acct.record(ROOT, first, true);
    acct.record(ROOT, usage({ inputTokens: 300, outputTokens: 90, totalTokens: 390 }, { inputTokens: 200, outputTokens: 60, totalTokens: 260 }));
    const pricing = { authMode: "api_key" as const, now: new Date("2026-10-05") };
    assert.deepEqual(acct.aggregateByModel(pricing), {
      [MODEL_ROOT]: { inputTokens: 300, outputTokens: 90, cacheReadInputTokens: 0,
        cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "metered", costUSD: 0.0075 },
    });
  });
});

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
    // Proved historical replay, matching the characterized paginated cold-resume fixture:
    // the restored snapshot has total = the prior cumulative AND
    // `last` = the prior leg's FINAL response (NON-ZERO, a subset already in `total`). The baseline
    // must be the full `total` ({in:500, out:300}); a `total - last` baseline ({in:400, out:250})
    // would sit below the prior cumulative and re-charge the prior leg's last response.
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }), true);
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
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }), true);
    acct.record(ROOT, usage({ inputTokens: 700, outputTokens: 400, totalTokens: 1100 }, { inputTokens: 200, outputTokens: 100, totalTokens: 300 }));
    // A resume replay redelivers an EARLIER cumulative snapshot — must not increase usage.
    acct.record(ROOT, usage({ inputTokens: 500, outputTokens: 300, totalTokens: 800 }, { inputTokens: 100, outputTokens: 50, totalTokens: 150 }), true);
    const agg = acct.aggregateByModel();
    assert.ok(agg);
    assertEntry(agg[MODEL_ROOT], { input: 200, output: 100, cacheRead: 0, cacheCreation: 0, reasoning: 0 });
  });

  it("baselines at `total` irrespective of `last` (a hypothetical last=0 snapshot charges the same)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, true);
    // Explicitly proved replay with last=0 baselines at full total = {in:400, out:250}.
    acct.record(ROOT, usage({ inputTokens: 400, outputTokens: 250, totalTokens: 650 }, {}), true);
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
    // Only a proved historical resume snapshot with last=0 — baseline = total, so charged is zero.
    acct.record(ROOT, usage({ inputTokens: 400, outputTokens: 250, totalTokens: 650 }, {}), true);
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

  // PRD #1551 (D5): a CUSTOM (unknown-to-the-price-table) worker-root model has no price row,
  // so under api_key it is `unreported` with FULL token totals (never a fabricated dollar), and
  // under subscription it is also `unreported` with token totals — the same accounting as any
  // other unpriced model. This confirms opening custom root models needs no pricing-table change.
  const CUSTOM_MODEL = "gpt-7-custom-preview";
  it("(#1551) a custom (unknown-price) model under api_key is unreported with token totals retained", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, CUSTOM_MODEL, false);
    acct.record(ROOT, usageEvidence(B, B, true)); // well-shaped, reconciling — only the model is unknown
    const agg = acct.aggregateByModel(API_KEY);
    assert.ok(agg);
    const entry = agg[CUSTOM_MODEL]!;
    assert.equal(entry.costStatus, "unreported", "an unknown-price model never meters under api_key");
    assert.ok(!("costUSD" in entry), "no dollar figure invented for an unknown model");
    assert.equal(entry.inputTokens, 300, "input tokens retained");
    assert.equal(entry.outputTokens, 200, "output tokens retained");
    assert.deepEqual(deriveCodexRunCost(agg), { kind: "unreported" });
  });

  it("(#1551) a custom (unknown-price) model under subscription is unreported with token totals", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, CUSTOM_MODEL, false);
    acct.record(ROOT, usageEvidence(B, B, true));
    const entry = acct.aggregateByModel({ authMode: "subscription", now: API_KEY.now })![CUSTOM_MODEL]!;
    assert.equal(entry.costStatus, "unreported", "unknown models have no estimate under either auth mode");
    assert.ok(!("costUSD" in entry));
    assert.equal(entry.inputTokens, 300, "input tokens retained");
    assert.equal(entry.outputTokens, 200, "output tokens retained");
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
    assert.deepEqual(deriveCodexRunCost(agg), { kind: "unreported" });
  });

  it("a subscription run fails pricing closed on malformed evidence, tokens retained", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_ROOT, false);
    acct.record(ROOT, usageEvidence(B, B, false));
    const astra = acct.aggregateByModel({ authMode: "subscription", now: API_KEY.now })![MODEL_ROOT]!;
    assert.equal(astra.costStatus, "unreported", "the evidence gate applies to both credential modes");
    assert.ok(!("costUSD" in astra));
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

// #4580 M1: model segments. `beginModelSegment` cuts a thread's cumulative at the running max;
// no production caller cuts yet (#3253), so these tests drive the public accountant directly.
describe("#4580 model segments (token-only)", () => {
  const MODEL_A = "gpt-6-astra";
  const MODEL_B = "gpt-5.6-sol";
  /** A well-formed note: total = input + output, `last` = the same as the cumulative delta. */
  const wf = (t: Partial<CodexUsageBreakdown>, l: Partial<CodexUsageBreakdown>): CodexThreadTokenUsage => {
    const full = bd(t);
    const last = bd(l);
    return {
      total: { ...full, totalTokens: full.inputTokens + full.outputTokens },
      last: { ...last, totalTokens: last.inputTokens + last.outputTokens },
    };
  };
  const want = (input: number, output: number, cacheRead = 0, cacheCreation = 0, reasoning = 0) =>
    ({ input, output, cacheRead, cacheCreation, reasoning });

  it("an unknown thread and an equal-model cut change nothing (allocating no segment is not observable through the public API)", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    acct.record(ROOT, wf({ inputTokens: 100, outputTokens: 50 }, { inputTokens: 100, outputTokens: 50 }));
    const before = acct.aggregateByModel();
    acct.beginModelSegment("th-unknown", MODEL_B);
    acct.beginModelSegment(ROOT, MODEL_A);
    assert.deepEqual(acct.aggregateByModel(), before);
    acct.record(ROOT, wf({ inputTokens: 150, outputTokens: 70 }, { inputTokens: 50, outputTokens: 20 }));
    assertEntry(acct.aggregateByModel()?.[MODEL_A], want(150, 70));
    assert.equal(acct.aggregateByModel()?.[MODEL_B], undefined);
    assert.equal(acct.usageIncomplete, false);
  });

  it("a fresh-thread cut attributes work before the cut to the old model and later work to the new one", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    const first = acct.record(ROOT, wf({ inputTokens: 100, outputTokens: 50 }, { inputTokens: 100, outputTokens: 50 }));
    assert.equal(first?.model, MODEL_A);
    acct.beginModelSegment(ROOT, MODEL_B);
    const second = acct.record(ROOT, wf({ inputTokens: 250, outputTokens: 120 }, { inputTokens: 150, outputTokens: 70 }));
    assert.equal(second?.model, MODEL_B);
    const agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(100, 50));
    assertEntry(agg?.[MODEL_B], want(150, 70));
  });

  it("a resumed thread without a baseline rebinds, then proved replay is not charged and only new work is", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, true);
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.record(ROOT, wf({ inputTokens: 500, outputTokens: 200 }, { inputTokens: 500, outputTokens: 200 }), true), undefined);
    const live = acct.record(ROOT, wf({ inputTokens: 600, outputTokens: 250 }, { inputTokens: 100, outputTokens: 50 }));
    assert.equal(live?.model, MODEL_B);
    const agg = acct.aggregateByModel();
    assert.equal(agg?.[MODEL_A], undefined, "a healthy rebind leaves no entry for the replaced model");
    assertEntry(agg?.[MODEL_B], want(100, 50));
    assert.equal(acct.usageIncomplete, false);
  });

  it("a quarantined thread only rebinds: it stays quarantined, attributes nothing, and both models report zero", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, true);
    assert.equal(acct.record(ROOT, wf({ inputTokens: 100, outputTokens: 50 }, { inputTokens: 100, outputTokens: 50 })), undefined);
    assert.equal(acct.usageIncomplete, true);
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.record(ROOT, wf({ inputTokens: 500, outputTokens: 200 }, { inputTokens: 500, outputTokens: 200 }), true), undefined,
      "still quarantined: a later proved replay attributes nothing");
    assert.equal(acct.hasBaseline(ROOT), false);
    const agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(0, 0));
    assertEntry(agg?.[MODEL_B], want(0, 0));
  });

  it("a rebind on a thread that is incomplete for another reason keeps the replaced model as a zero unreported entry", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    acct.markIncomplete(ROOT, "invalid_identity");
    acct.beginModelSegment(ROOT, MODEL_B);
    acct.beginModelSegment(ROOT, MODEL_A);
    acct.beginModelSegment(ROOT, MODEL_B);
    const agg = acct.aggregateByModel();
    assert.deepEqual(Object.keys(agg ?? {}).sort(), [MODEL_A, MODEL_B].sort());
    assertEntry(agg?.[MODEL_A], want(0, 0));
    assertEntry(agg?.[MODEL_B], want(0, 0));
  });

  it("duplicate, stale and older-than-cut notes after a cut are not adopted and move no segment", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    const n1 = wf({ inputTokens: 100, outputTokens: 50 }, { inputTokens: 100, outputTokens: 50 });
    const n2 = wf({ inputTokens: 200, outputTokens: 100 }, { inputTokens: 100, outputTokens: 50 });
    acct.record(ROOT, n1);
    acct.record(ROOT, n2);
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.record(ROOT, n2), undefined, "duplicate of the cut point");
    assert.equal(acct.record(ROOT, n1), undefined, "older than the cut");
    assert.equal(acct.record(ROOT, wf({ inputTokens: 10, outputTokens: 5 }, { inputTokens: 10, outputTokens: 5 })), undefined, "stale");
    let agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(200, 100));
    assert.equal(agg?.[MODEL_B], undefined, "no usage yet on the new segment, so a healthy unused model is omitted");
    const live = acct.record(ROOT, wf({ inputTokens: 260, outputTokens: 130 }, { inputTokens: 60, outputTokens: 30 }));
    assert.equal(live?.model, MODEL_B);
    agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(200, 100));
    assertEntry(agg?.[MODEL_B], want(60, 30));
    assert.equal(acct.usageIncomplete, false);
  });

  it("A to B to A conserves every column per model, and each response carries its segment's model", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    const models: (string | undefined)[] = [];
    const rec = (u: CodexThreadTokenUsage) => models.push(acct.record(ROOT, u)?.model);
    rec(wf({ inputTokens: 100, cachedInputTokens: 20, cacheWriteInputTokens: 5, outputTokens: 40, reasoningOutputTokens: 10 },
      { inputTokens: 100, outputTokens: 40 }));
    acct.beginModelSegment(ROOT, MODEL_B);
    rec(wf({ inputTokens: 300, cachedInputTokens: 90, cacheWriteInputTokens: 15, outputTokens: 100, reasoningOutputTokens: 30 },
      { inputTokens: 200, outputTokens: 60 }));
    rec(wf({ inputTokens: 350, cachedInputTokens: 100, cacheWriteInputTokens: 15, outputTokens: 110, reasoningOutputTokens: 33 },
      { inputTokens: 50, outputTokens: 10 }));
    acct.beginModelSegment(ROOT, MODEL_A);
    rec(wf({ inputTokens: 500, cachedInputTokens: 160, cacheWriteInputTokens: 25, outputTokens: 150, reasoningOutputTokens: 50 },
      { inputTokens: 150, outputTokens: 40 }));
    assert.deepEqual(models, [MODEL_A, MODEL_B, MODEL_B, MODEL_A]);
    const agg = acct.aggregateByModel();
    // A: first segment (100/20/5/40/10) plus the last (150/60/10/40/17 raw deltas)
    assertEntry(agg?.[MODEL_A], want(100 - 20 - 5 + (150 - 60 - 10), 40 + 40, 20 + 60, 5 + 10, 10 + 17));
    assertEntry(agg?.[MODEL_B], want(250 - 80 - 10, 70, 80, 10, 23));
    const sum = (k: keyof CodexModelUsageEntry) =>
      Object.values(agg ?? {}).reduce((s, e) => s + (e[k] as number), 0);
    assert.equal(sum("inputTokens"), 500 - 160 - 25);
    assert.equal(sum("cacheReadInputTokens"), 160);
    assert.equal(sum("cacheCreationInputTokens"), 25);
    assert.equal(sum("outputTokens"), 150);
    assert.equal(sum("reasoningOutputTokens"), 50);
  });

  it("an incomplete cut thread taints every segment model and retains tokens", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    acct.record(ROOT, wf({ inputTokens: 100, outputTokens: 50 }, { inputTokens: 100, outputTokens: 50 }));
    acct.beginModelSegment(ROOT, MODEL_B);
    acct.record(ROOT, wf({ inputTokens: 150, outputTokens: 80 }, { inputTokens: 50, outputTokens: 30 }));
    acct.markIncomplete(ROOT, "invalid_identity");
    const agg = acct.aggregateByModel({ authMode: "api_key", now: new Date("2026-01-01T00:00:00Z") });
    assert.equal(agg?.[MODEL_A]?.costStatus, "unreported");
    assert.equal(agg?.[MODEL_B]?.costStatus, "unreported");
    assert.equal(agg?.[MODEL_A]?.inputTokens, 100);
    assert.equal(agg?.[MODEL_B]?.inputTokens, 50);
  });

  it("a cache split that is malformed at a cut cannot inflate the new model's uncached input", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    acct.record(ROOT, wf({ inputTokens: 10, cachedInputTokens: 20, outputTokens: 10 }, { inputTokens: 10, cachedInputTokens: 20, outputTokens: 10 }));
    assert.equal(acct.usageIncomplete, false, "an uncut malformed split stays accepted, as before");
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.usageIncomplete, true);
    acct.record(ROOT, wf({ inputTokens: 20, cachedInputTokens: 20, outputTokens: 20 }, { inputTokens: 10, outputTokens: 10 }));
    const agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(0, 10, 20));
    assertEntry(agg?.[MODEL_B], want(0, 10, 0));
    assert.match(acct.incompleteNoticeText ?? "", /Diagnostic: bucket relationship \(total\.cachedInputTokens\)\./);
  });

  it("a cache split that turns malformed after the cut is adopted, conserved and marks the thread incomplete", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    acct.record(ROOT, wf({ inputTokens: 10, cachedInputTokens: 10, outputTokens: 10 }, { inputTokens: 10, cachedInputTokens: 10, outputTokens: 10 }));
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.usageIncomplete, false, "a well-formed boundary does not mark");
    const adopted = acct.record(ROOT, wf({ inputTokens: 20, cachedInputTokens: 21, outputTokens: 20 }, { inputTokens: 10, cachedInputTokens: 11, outputTokens: 10 }));
    assert.equal(adopted?.model, MODEL_B, "the malformed note is still adopted");
    assert.equal(acct.usageIncomplete, true);
    const agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(0, 10, 10));
    assertEntry(agg?.[MODEL_B], want(0, 10, 11));
  });

  it("a multi-segment thread does not absorb a single-segment sibling's negative raw uncached delta", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread("child-x", MODEL_A, false);
    acct.registerThread("root-y", MODEL_A, false);
    acct.record("child-x", wf({ inputTokens: 100, cachedInputTokens: 101, outputTokens: 60 }, { inputTokens: 100, cachedInputTokens: 101, outputTokens: 60 }));
    acct.record("root-y", wf({ inputTokens: 30, outputTokens: 30 }, { inputTokens: 30, outputTokens: 30 }));
    acct.beginModelSegment("root-y", MODEL_B);
    acct.record("root-y", wf({ inputTokens: 50, outputTokens: 50 }, { inputTokens: 20, outputTokens: 20 }));
    const agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(30, 90, 101));
    assertEntry(agg?.[MODEL_B], want(20, 20));
  });

  it("a note that advances magnitude but regresses a segment-spanning column is refused after a cut as well", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    acct.record(ROOT, wf({ inputTokens: 10, outputTokens: 10, reasoningOutputTokens: 5 }, { inputTokens: 10, outputTokens: 10 }));
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.record(ROOT, wf({ inputTokens: 20, outputTokens: 20, reasoningOutputTokens: 4 }, { inputTokens: 10, outputTokens: 10 })), undefined);
    assert.equal(acct.usageIncomplete, true);
    assertEntry(acct.aggregateByModel()?.[MODEL_A], want(10, 10, 0, 0, 5));
  });

  it("a malformed proved-replay baseline marks the thread incomplete at a cut even when the cut point is well-formed", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, true);
    // Replay baseline: cached exceeds input (malformed split).
    acct.record(ROOT, wf({ inputTokens: 10, cachedInputTokens: 20, outputTokens: 10 }, { inputTokens: 10, outputTokens: 10 }), true);
    assert.equal(acct.usageIncomplete, false, "an uncut malformed baseline is accepted, as before");
    // A well-formed advance: cached unchanged, input grew past it.
    assert.ok(acct.record(ROOT, wf({ inputTokens: 50, cachedInputTokens: 20, outputTokens: 20 }, { inputTokens: 40, outputTokens: 10 })));
    assert.equal(acct.usageIncomplete, false);
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.usageIncomplete, true);
    assert.match(acct.incompleteNoticeText ?? "", /Diagnostic: bucket relationship \(total\.cachedInputTokens\)\./);
  });

  it("a healthy rebind does not remember the replaced model once the thread later becomes incomplete", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, true);
    acct.beginModelSegment(ROOT, MODEL_B);
    assert.equal(acct.usageIncomplete, false);
    // A live note with no proved baseline: missing replay boundary, quarantine.
    assert.equal(acct.record(ROOT, wf({ inputTokens: 100, outputTokens: 50 }, { inputTokens: 100, outputTokens: 50 })), undefined);
    assert.equal(acct.usageIncomplete, true);
    const agg = acct.aggregateByModel();
    assert.equal(agg?.[MODEL_A], undefined, "the replaced model was never used while incomplete");
    assertEntry(agg?.[MODEL_B], want(0, 0));
  });

  it("pins the caller ordering contract owned by #3253: a note delivered after a cut that advances the max belongs to the new model", () => {
    const acct = new CodexUsageAccountant();
    acct.registerThread(ROOT, MODEL_A, false);
    acct.record(ROOT, wf({ inputTokens: 100, outputTokens: 50 }, { inputTokens: 100, outputTokens: 50 }));
    acct.beginModelSegment(ROOT, MODEL_B);
    // An earlier turn's note arriving late: it advances the max, so it is attributed to MODEL_B.
    const late = acct.record(ROOT, wf({ inputTokens: 200, outputTokens: 100 }, { inputTokens: 100, outputTokens: 50 }));
    assert.equal(late?.model, MODEL_B);
    const agg = acct.aggregateByModel();
    assertEntry(agg?.[MODEL_A], want(100, 50));
    assertEntry(agg?.[MODEL_B], want(100, 50));
  });
});

// Seeded randomized conservation (#4580): per column, the sum over emitted models equals the
// thread's accepted final cumulative, whatever the cut pattern. totalTokens is not an emitted
// column; its conservation follows from total = input + output in every generated note.
describe("#4580 randomized conservation across cuts", () => {
  function mulberry32(seed: number): () => number {
    let a = seed >>> 0;
    return () => {
      a = (a + 0x6d2b79f5) >>> 0;
      let t = a;
      t = Math.imul(t ^ (t >>> 15), t | 1);
      t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }
  const MODELS = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6.1-sol"];

  function runFamily(family: "well-formed" | "malformed", baseSeed: number,
    stats = { multiModel: 0, malformedNotes: 0, incomplete: 0 }): string | undefined {
    for (let k = 0; k < 200; k++) {
      const seed = baseSeed + k;
      const rnd = mulberry32(seed);
      const int = (n: number) => Math.floor(rnd() * (n + 1));
      const acct = new CodexUsageAccountant();
      let cur = MODELS[0]!;
      acct.registerThread("t", cur, false);
      const s = { inputTokens: 0, cachedInputTokens: 0, cacheWriteInputTokens: 0, outputTokens: 0, reasoningOutputTokens: 0 };
      let accepted = { ...s };
      const history: CodexThreadTokenUsage[] = [];
      const steps = 3 + int(9);
      let cuts = 0;
      let malformedPhase = true;
      for (let i = 0; i < steps; i++) {
        const kind = int(9);
        if (kind <= 3 && cuts < 4) {
          cuts++;
          // kind 0: a same-model cut; otherwise always a different model, so cuts change attribution.
          cur = kind === 0 ? cur : MODELS[(MODELS.indexOf(cur) + 1 + int(1)) % MODELS.length]!;
          acct.beginModelSegment("t", cur);
          continue;
        }
        if (kind === 4 && history.length > 0) {
          const old = history[int(history.length - 1)]!;
          const r = acct.record("t", old);
          if (r !== undefined) return `seed ${seed}: a stale or duplicate note was adopted`;
          continue;
        }
        const inInc = int(50);
        const outInc = int(50);
        // Raw uncached input before this step; negative while the cache split is malformed.
        const rawBefore = s.inputTokens - s.cachedInputTokens - s.cacheWriteInputTokens;
        s.inputTokens += inInc;
        s.outputTokens += outInc;
        s.reasoningOutputTokens = Math.min(s.reasoningOutputTokens + int(20), s.outputTokens);
        if (family === "malformed" && malformedPhase) {
          // Malformed prefix: cached exceeds input, so the clamped uncached input stays 0.
          s.cachedInputTokens = Math.max(s.cachedInputTokens, s.inputTokens + 1 + int(5));
          s.cacheWriteInputTokens += int(10);
          stats.malformedNotes++;
          if (rnd() < 0.3) malformedPhase = false; // once well-formed, the split stays well-formed
        } else if (rawBefore < 0) {
          // Leaving a malformed split: the clamped uncached input is 0 before, so any raw
          // growth keeps it non-decreasing.
          s.cachedInputTokens += int(10);
          s.cacheWriteInputTokens += int(5);
        } else {
          // Bound the cache increments by this step's input increment so the clamped uncached
          // input never decreases: every generated note is monotone in all seven gated columns.
          const c = int(inInc);
          s.cachedInputTokens += c;
          s.cacheWriteInputTokens += int(inInc - c);
        }
        const note: CodexThreadTokenUsage = {
          total: { ...s, totalTokens: s.inputTokens + s.outputTokens },
          last: { ...bd(), inputTokens: inInc, outputTokens: 1, totalTokens: inInc + 1 },
        };
        history.push(note);
        const r = acct.record("t", note);
        // Every generated note is monotone, so one that advances the magnitude must be adopted;
        // regression refusals are covered by the deterministic gate tests, not here.
        if (inInc + outInc > 0 && r === undefined) return `seed ${seed} (${family}): a monotone advancing note was refused`;
        if (r !== undefined) {
          accepted = { ...s };
          if (r.model !== cur) return `seed ${seed}: adopted model ${r.model} expected ${cur}`;
        }
        if (int(5) === 0) acct.record("t", note); // an immediate duplicate must not move anything
      }
      if (family === "well-formed" && acct.usageIncomplete) return `seed ${seed}: a well-formed sequence ended incomplete`;
      if (acct.usageIncomplete) stats.incomplete++;
      const agg = acct.aggregateByModel() ?? {};
      const withTokens = Object.values(agg).filter(e =>
        e.inputTokens + e.outputTokens + e.cacheReadInputTokens + e.cacheCreationInputTokens > 0).length;
      if (withTokens >= 2) stats.multiModel++;
      const sum = (key: keyof CodexModelUsageEntry) => Object.values(agg).reduce((t, e) => t + (e[key] as number), 0);
      const expected: [keyof CodexModelUsageEntry, number][] = [
        ["inputTokens", Math.max(accepted.inputTokens - accepted.cachedInputTokens - accepted.cacheWriteInputTokens, 0)],
        ["cacheReadInputTokens", accepted.cachedInputTokens],
        ["cacheCreationInputTokens", accepted.cacheWriteInputTokens],
        ["outputTokens", accepted.outputTokens],
        ["reasoningOutputTokens", accepted.reasoningOutputTokens],
      ];
      for (const [key, value] of expected) {
        const got = sum(key);
        if (got !== value) return `seed ${seed} (${family}): ${key} summed to ${got}, expected ${value}`;
      }
    }
    return undefined;
  }

  it("well-formed notes: 200 seeded sequences conserve every emitted column", () => {
    const stats = { multiModel: 0, malformedNotes: 0, incomplete: 0 };
    const mismatch = runFamily("well-formed", 1000, stats);
    assert.equal(mismatch, undefined, mismatch ?? "");
    assert.ok(stats.multiModel >= 100, `only ${stats.multiModel} of 200 sequences carried tokens on 2+ models`);
  });

  it("malformed cache splits: 200 seeded sequences conserve every emitted column", () => {
    // Raw columns and the clamped uncached input stay monotone, so every advancing note is
    // adopted: a malformed prefix (cached above input) is followed by a well-formed tail.
    const stats = { multiModel: 0, malformedNotes: 0, incomplete: 0 };
    const mismatch = runFamily("malformed", 5000, stats);
    assert.equal(mismatch, undefined, mismatch ?? "");
    assert.ok(stats.malformedNotes >= 200, `only ${stats.malformedNotes} malformed notes generated`);
    assert.ok(stats.multiModel >= 50, `only ${stats.multiModel} of 200 sequences carried tokens on 2+ models`);
    assert.ok(stats.incomplete >= 20, `only ${stats.incomplete} of 200 sequences hit the D6 boundary check`);
  });
});
