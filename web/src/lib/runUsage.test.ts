import { describe, it, expect } from "vitest";
import { deriveRunUsage, cacheDisplayPct } from "./runUsage";
import type { RunMessage } from "./api";

let seq = 0;
function msg(kind: string, agent: string | null, payload: unknown): RunMessage {
  return { seq: ++seq, kind, agent, agent_instance: null, agent_label: null, payload, created_at: "2026-07-12T00:00:00Z" };
}
function beforeEachReset() {
  seq = 0;
}

type ModelCum = { input: number; cacheRead?: number; cacheCreation?: number; output: number; cost: number };

const SENTINEL_TOKENS = 777;
const SENTINEL_COST = 7.77;

/**
 * A cumulative result frame (verdict b): usage totals are session-to-date, split per
 * model exactly as the SDK forwards them.
 *
 * 🔴 The top-level `usage` and `total_cost_usd` this emits are DELIBERATE SENTINELS
 * that disagree with `modelUsage`, and every assertion below is pinned to the
 * modelUsage figures. That is not decoration: issue #195 was the run page reading
 * top-level `usage` while every rollup folded `modelUsage`, and on the live SDK pin
 * the two disagree by 2.5-3.3x. With the sentinels here, an implementation that
 * regresses to the top-level field cannot pass these tests by coincidence. Do not
 * "tidy" them into agreement — that deletes the only thing making these frames say
 * which field was read.
 */
function modelResultFrame(
  agent: string,
  models: Record<string, ModelCum>,
  meta: { turns: number; durationMs: number; kind?: "status" | "error"; subtype?: string },
): RunMessage {
  const modelUsage = Object.fromEntries(
    Object.entries(models).map(([m, c]) => [
      m,
      {
        inputTokens: c.input,
        outputTokens: c.output,
        cacheReadInputTokens: c.cacheRead ?? 0,
        cacheCreationInputTokens: c.cacheCreation ?? 0,
        costUSD: c.cost,
      },
    ]),
  );
  return msg(meta.kind ?? "status", agent, {
    event: "result",
    subtype: meta.subtype ?? (meta.kind === "error" ? "error_max_turns" : "success"),
    num_turns: meta.turns,
    duration_ms: meta.durationMs,
    total_cost_usd: SENTINEL_COST,
    usage: {
      input_tokens: SENTINEL_TOKENS,
      cache_read_input_tokens: SENTINEL_TOKENS,
      cache_creation_input_tokens: SENTINEL_TOKENS,
      output_tokens: SENTINEL_TOKENS,
    },
    modelUsage,
  });
}

/** The single-model shorthand the pre-#195 tests were written against. */
function resultFrame(
  agent: string,
  cum: { input: number; cacheRead: number; cacheCreation?: number; output: number; cost: number },
  meta: { turns: number; durationMs: number; kind?: "status" | "error"; subtype?: string },
): RunMessage {
  return modelResultFrame(agent, { "claude-sonnet-5": cum }, meta);
}

// An assistant frame carrying per-call usage (attached by the worker to one msg).
function assistantUsage(
  agent: string,
  u: { input: number; cacheRead?: number; cacheCreation?: number; output: number; model?: string },
): RunMessage {
  return msg("text", agent, {
    text: "…",
    ...(u.model ? { model: u.model } : {}), // PRD #93: co-gated with usage, same frame
    usage: {
      input_tokens: u.input,
      cache_read_input_tokens: u.cacheRead ?? null, // BetaUsage cache fields are nullable
      cache_creation_input_tokens: u.cacheCreation ?? null,
      output_tokens: u.output,
    },
  });
}

// Like assistantUsage but with a chosen agent_instance, to exercise the live dedup
// key `(agent_instance, usage)` (issue #237). The base `msg` hard-codes agent_instance
// null (the lead lane, where two byte-identical calls collapse); a subagent invocation
// carries a distinct non-null id, so its calls never collapse into another lane's.
function assistantUsageInst(
  agent: string,
  agentInstance: string | null,
  u: { input: number; cacheRead?: number; cacheCreation?: number; output: number; model?: string },
): RunMessage {
  return { ...assistantUsage(agent, u), agent_instance: agentInstance };
}

// PRD #516: a lead usage frame that ALSO co-carries `payload.context` (the lead's
// context-window fill), exactly as the agent attaches it alongside `payload.usage`.
function assistantUsageCtx(
  agent: string,
  u: { input: number; cacheRead?: number; cacheCreation?: number; output: number; model?: string },
  ctx: { used: number; window: number; pct: number },
): RunMessage {
  const base = assistantUsage(agent, u);
  return { ...base, payload: { ...(base.payload as Record<string, unknown>), context: ctx } };
}

describe("deriveRunUsage", () => {
  it("differences cumulative result frames into per-phase deltas; total is the high-water mark", () => {
    beforeEachReset();
    const messages = [
      msg("status", "lead", { event: "init", model: "claude-sonnet-5" }),
      resultFrame("lead", { input: 21_400, cacheRead: 188_000, output: 6_100, cost: 0.24 }, { turns: 9, durationMs: 100_000 }),
      resultFrame("lead", { input: 80_300, cacheRead: 800_300, output: 34_800, cost: 1.26 }, { turns: 34, durationMs: 200_000 }),
    ];
    const d = deriveRunUsage(messages);

    expect(d.hasConfirmed).toBe(true);
    expect(d.model).toBe("claude-sonnet-5");
    expect(d.phases.map((p) => p.label)).toEqual(["Plan", "Implement · iteration 1"]);

    // Plan = first frame's cumulative (no predecessor).
    expect(d.phases[0]).toMatchObject({ fresh: 21_400, cached: 188_000, out: 6_100, turns: 9 });
    expect(d.phases[0].costUsd).toBeCloseTo(0.24, 6);
    // Iteration 1 = frame2 − frame1 (delta), turns/duration are per-invocation (raw).
    expect(d.phases[1]).toMatchObject({ fresh: 58_900, cached: 612_300, out: 28_700, turns: 34, durationMs: 200_000 });
    expect(d.phases[1].costUsd).toBeCloseTo(1.02, 6);

    // Run total = sum of the clamped deltas = each model's HIGH-WATER MARK, which on
    // this monotonic fixture coincides with the last cumulative. That coincidence is
    // exactly why the retired mechanism survived a rename here, and why it is spelled
    // out rather than left as "= last cumulative"; the cases below discriminate.
    // turns/duration SUM.
    expect(d.total).toMatchObject({ fresh: 80_300, cached: 800_300, out: 34_800, turns: 43, durationMs: 300_000, phaseCount: 2 });
    expect(d.total.costUsd).toBeCloseTo(1.26, 6);
    expect(d.cacheHitRatio).toBeCloseTo(800_300 / 880_600, 6);
    expect(d.phaseUsageBySeq.get(d.phases[1].seq)).toBe(d.phases[1]);
  });

  it("sums per-call assistant usage per agent (null cache fields coalesce to 0)", () => {
    beforeEachReset();
    const messages = [
      assistantUsage("lead", { input: 20_000, cacheRead: 180_000, cacheCreation: 1_000, output: 6_000 }),
      assistantUsage("coder", { input: 50_000, cacheRead: 600_000, output: 28_000 }), // null cache_creation
      assistantUsage("reviewer", { input: 8_000, cacheRead: 30_000, output: 700 }),
      assistantUsage("coder", { input: 1_000, output: 100 }), // null cache fields → 0
      resultFrame("lead", { input: 79_000, cacheRead: 810_000, output: 34_800, cost: 1.2 }, { turns: 5, durationMs: 100 }),
    ];
    const d = deriveRunUsage(messages);
    const byAgent = Object.fromEntries(d.agents.map((a) => [a.agent, a]));

    expect(byAgent["lead"]).toMatchObject({ fresh: 21_000, cached: 180_000, out: 6_000 });
    expect(byAgent["coder"]).toMatchObject({ fresh: 51_000, cached: 600_000, out: 28_100 });
    expect(byAgent["reviewer"]).toMatchObject({ fresh: 8_000, cached: 30_000, out: 700 });
    expect(d.agentTotal).toMatchObject({ fresh: 80_000, cached: 810_000, out: 34_800 });
    // The result frame's own usage is NEVER counted into the per-agent sum.
    expect(d.agents.reduce((n, a) => n + a.out, 0)).toBe(34_800);
  });

  it("has no usage for a pre-feature run (result frames carrying no modelUsage)", () => {
    beforeEachReset();
    const messages = [
      msg("status", "lead", { event: "result", subtype: "success", num_turns: 3 }), // no usage, no modelUsage
      msg("text", "lead", { text: "done" }),
    ];
    const d = deriveRunUsage(messages);
    expect(d.hasConfirmed).toBe(false);
    expect(d.phases).toHaveLength(0);
    expect(d.agents).toHaveLength(0);
    expect(d.modelTotals).toEqual([]);
  });

  it("folds an error result frame's usage (a failed phase still spent tokens)", () => {
    beforeEachReset();
    const messages = [
      resultFrame("lead", { input: 500, cacheRead: 0, output: 120, cost: 0.03 }, { turns: 7, durationMs: 44_000, kind: "error" }),
    ];
    const d = deriveRunUsage(messages);
    expect(d.hasConfirmed).toBe(true);
    expect(d.phases[0]).toMatchObject({ fresh: 500, out: 120, isError: true });
    expect(d.total.out).toBe(120);
  });

  it("clamps a non-monotonic delta to 0 (a requeue reset must not go negative)", () => {
    beforeEachReset();
    const messages = [
      resultFrame("lead", { input: 5_000, cacheRead: 0, output: 2_000, cost: 0.5 }, { turns: 10, durationMs: 1_000 }),
      // A cross-process resume that failed to reseed → cumulative drops below prior.
      resultFrame("lead", { input: 1_000, cacheRead: 0, output: 400, cost: 0.1 }, { turns: 3, durationMs: 500 }),
    ];
    const d = deriveRunUsage(messages);
    expect(d.phases[1]).toMatchObject({ fresh: 0, out: 0 }); // clamped, not negative
    expect(d.phases[1].costUsd).toBe(0);
  });

  // ── Issue #195: result frames are read PER MODEL, off `modelUsage` ───────────

  it("reads modelUsage, never the frame's top-level usage or total_cost_usd", () => {
    beforeEachReset();
    // Both fields ride every real result frame, and on the shipped SDK pin they
    // disagree — the run page read the wrong one for the whole of PRD #40's life.
    const d = deriveRunUsage([
      modelResultFrame(
        "lead",
        { "claude-opus-5": { input: 5_000, cacheRead: 40_000, cacheCreation: 1_000, output: 900, cost: 0.42 } },
        { turns: 4, durationMs: 1_000 },
      ),
    ]);
    expect(d.total).toMatchObject({ fresh: 6_000, cached: 40_000, out: 900 });
    expect(d.total.costUsd).toBeCloseTo(0.42, 6);
    // The sentinels the helper writes into the top-level fields must not appear.
    expect([d.total.fresh, d.total.cached, d.total.out]).not.toContain(777);
    expect(d.total.costUsd).not.toBeCloseTo(7.77, 6);
  });

  it("keeps a model that vanishes from a later frame's modelUsage (the #195 shape)", () => {
    beforeEachReset();
    // haiku is in the first result frame and gone from the second — measured on run
    // 84b6a933 and on 16 other live runs. The server retains it via GREATEST per
    // (run_id, session_id, model), so the client must too. An implementation that
    // sums modelUsage PER FRAME and differences the sums telescopes to the LAST
    // frame's models and loses haiku entirely: it would report fresh 300 / out 40 /
    // cost 3 here instead of 305 / 41 / 3.5.
    const d = deriveRunUsage([
      modelResultFrame(
        "lead",
        {
          "claude-opus-5": { input: 100, output: 10, cost: 1 },
          "claude-haiku-4-5-20251001": { input: 5, output: 1, cost: 0.5 },
        },
        { turns: 3, durationMs: 1_000 },
      ),
      modelResultFrame("lead", { "claude-opus-5": { input: 300, output: 40, cost: 3 } }, { turns: 6, durationMs: 2_000 }),
    ]);
    expect(d.phases[0]).toMatchObject({ fresh: 105, out: 11 });
    expect(d.phases[1]).toMatchObject({ fresh: 200, out: 30 }); // opus only; haiku contributes 0
    expect(d.phases[1].costUsd).toBeCloseTo(2, 6);
    expect(d.total).toMatchObject({ fresh: 305, out: 41 });
    expect(d.total.costUsd).toBeCloseTo(3.5, 6);
  });

  it("clamps per model, so one model's regression cannot eat another's growth", () => {
    beforeEachReset();
    // Whole-frame clamping would see 200 → 350 and report a 150 delta. Per model,
    // opus regressed (delta clamped to 0) and sonnet genuinely grew by 200.
    const d = deriveRunUsage([
      modelResultFrame(
        "lead",
        { "claude-opus-5": { input: 100, output: 0, cost: 0 }, "claude-sonnet-5": { input: 100, output: 0, cost: 0 } },
        { turns: 1, durationMs: 1 },
      ),
      modelResultFrame(
        "lead",
        { "claude-opus-5": { input: 50, output: 0, cost: 0 }, "claude-sonnet-5": { input: 300, output: 0, cost: 0 } },
        { turns: 1, durationMs: 1 },
      ),
    ]);
    expect(d.phases[1].fresh).toBe(200);
    expect(d.total.fresh).toBe(400);
  });

  it("skips a usage-only result frame entirely: no phase row AND no agent row", () => {
    beforeEachReset();
    // Decided with the user, 2026-08-02: mirror the server's `len(p.ModelUsage) == 0`
    // guard exactly. A fallback to top-level `usage` would reintroduce the divergence.
    //
    // The second half of this is the sharper assertion. The skip is written as a
    // `continue`, and a fall-through would drop this frame into the per-agent branch
    // (which fires on any payload with a `usage` key) and fold the run's CUMULATIVE
    // total into one agent's per-call sum. `agents` is what observes that; `phases`
    // alone cannot. This is also the file's one remaining `usage`-only frame — keep
    // it, or nothing left here exercises that branch at all.
    const d = deriveRunUsage([
      msg("status", "lead", {
        event: "result",
        subtype: "success",
        num_turns: 5,
        duration_ms: 900,
        total_cost_usd: 1.5,
        usage: { input_tokens: 9_000, cache_read_input_tokens: 1_000, output_tokens: 400 },
      }),
    ]);
    expect(d.hasConfirmed).toBe(false);
    expect(d.phases).toHaveLength(0);
    expect(d.agents).toHaveLength(0);
    expect(d.agentTotal).toMatchObject({ fresh: 0, cached: 0, out: 0 });
    expect(d.total).toMatchObject({ fresh: 0, cached: 0, out: 0, phaseCount: 0 });
    expect(d.modelTotals).toEqual([]);
  });

  it("skips a zero-work frame (modelUsage: {}) without spending a phase label on it", () => {
    beforeEachReset();
    // A real one exists in the live DB (run e2d7427b seq 318: num_turns 0,
    // duration_ms 288, every usage field 0). The server folds nothing for it, so it
    // produces no phase row — and, because the label counter only advances on rows
    // that are pushed, the NEXT frame is still "Implement · iteration 1" rather than
    // 2. That renumbering is the visible consequence of the skip; it is asserted here
    // so a future change to the labelling cannot happen silently.
    const d = deriveRunUsage([
      modelResultFrame("lead", { "claude-opus-5": { input: 100, output: 10, cost: 1 } }, { turns: 3, durationMs: 10 }),
      msg("status", "lead", { event: "result", subtype: "success", num_turns: 0, duration_ms: 288, modelUsage: {} }),
      modelResultFrame("lead", { "claude-opus-5": { input: 300, output: 40, cost: 3 } }, { turns: 4, durationMs: 20 }),
    ]);
    expect(d.phases.map((p) => p.label)).toEqual(["Plan", "Implement · iteration 1"]);
    expect(d.total).toMatchObject({ fresh: 300, out: 40, phaseCount: 2, turns: 7, durationMs: 30 });
  });

  // The three malformed-entry shapes, each pinned against what Go actually does with
  // the same bytes. Measured 2026-08-02 by running the real `resultUsagePayload`
  // through encoding/json; the arms genuinely differ, so one blanket rule is wrong.

  it("loses the phase row for a frame whose ONLY model id is empty, totals still agreeing", () => {
    beforeEachReset();
    // The SECOND trigger for "no phase row", alongside `modelUsage: {}`. Measured on
    // both sides 2026-08-02: Go folds the frame (the map is non-empty) and then skips
    // the entry via `if model == ""`, writing ZERO rows; this reader skips the entry,
    // is left with nothing, and skips the frame. Totals therefore agree — both record
    // nothing — and the only observable difference is the phase row, the same accepted
    // consequence as the empty map.
    const d = deriveRunUsage([msg("status", "lead", {
      event: "result",
      subtype: "success",
      num_turns: 1,
      duration_ms: 1,
      modelUsage: { "": { inputTokens: 10, outputTokens: 1 } },
    })]);
    expect(d.hasConfirmed).toBe(false);
    expect(d.phases).toHaveLength(0);
    expect(d.total).toMatchObject({ fresh: 0, out: 0 });
    expect(d.modelTotals).toEqual([]);
  });

  it("still folds a frame that has an empty model id ALONGSIDE a real one", () => {
    beforeEachReset();
    // The mixed case is NOT affected: Go folds the frame and writes one row, and so do
    // we. Without this, the test above would be satisfied by a reader that discarded
    // any frame containing an empty id — which would lose real usage.
    const d = deriveRunUsage([msg("status", "lead", {
      event: "result",
      subtype: "success",
      num_turns: 1,
      duration_ms: 1,
      modelUsage: { "": { inputTokens: 10 }, "claude-opus-5": { inputTokens: 7, outputTokens: 2 } },
    })]);
    expect(d.hasConfirmed).toBe(true);
    expect(d.total).toMatchObject({ fresh: 7, out: 2 });
    expect(d.modelTotals.map((t) => t.model)).toEqual(["claude-opus-5"]);
  });

  it("skips a frame whose modelUsage entry is a number (Go fails the whole payload)", () => {
    beforeEachReset();
    // Go: "cannot unmarshal number into ... resultModelUsage" → the server folds
    // NOTHING for this frame, valid siblings included. Dropping only the bad entry
    // here would leave the client counting a frame the server ignored.
    const d = deriveRunUsage([
      msg("status", "lead", {
        event: "result",
        subtype: "success",
        modelUsage: { "claude-opus-5": { inputTokens: 100, outputTokens: 10, costUSD: 1 }, "claude-sonnet-5": 5 },
      }),
    ]);
    expect(d.hasConfirmed).toBe(false);
    expect(d.phases).toHaveLength(0);
  });

  it("skips a frame whose modelUsage entry is an ARRAY, which typeof calls an object", () => {
    beforeEachReset();
    // `typeof [] === "object"`, so without an explicit Array.isArray check this folds
    // as an all-zero model while Go answers "cannot unmarshal array" and skips the
    // frame. The valid sibling is what makes the divergence expensive.
    const d = deriveRunUsage([
      msg("status", "lead", {
        event: "result",
        subtype: "success",
        modelUsage: { "claude-opus-5": { inputTokens: 100, outputTokens: 10, costUSD: 1 }, "claude-sonnet-5": [] },
      }),
    ]);
    expect(d.hasConfirmed).toBe(false);
    expect(d.phases).toHaveLength(0);
    expect(d.modelTotals).toEqual([]);
  });

  it("folds a NULL modelUsage entry as an all-zero model, because Go does", () => {
    beforeEachReset();
    // Unmarshalling `null` into a struct is a documented no-op returning NO error, so
    // Go lands the key with the zero value and folds the frame. Measured:
    // {"good":{"inputTokens":10},"bad":null} → Go FOLDS 2 rows, bad all-zero.
    // Treating null as a poison discards a frame whose VALID models the server has
    // already folded — the costly direction of the two.
    const d = deriveRunUsage([
      msg("status", "lead", {
        event: "result",
        subtype: "success",
        num_turns: 2,
        duration_ms: 50,
        modelUsage: { "claude-opus-5": { inputTokens: 100, outputTokens: 10, costUSD: 1 }, "claude-sonnet-5": null },
      }),
    ]);
    expect(d.hasConfirmed).toBe(true);
    expect(d.phases).toHaveLength(1);
    expect(d.total).toMatchObject({ fresh: 100, out: 10 });
    expect(d.modelTotals).toEqual([
      { model: "claude-opus-5", input: 100, cacheCreation: 0, cached: 0, out: 10, costUsd: 1 },
      { model: "claude-sonnet-5", input: 0, cacheCreation: 0, cached: 0, out: 0, costUsd: 0 },
    ]);
  });

  // ── Issue #195: the mark is a RUNNING MAX, which is what makes the run total
  // equal the server's. Every case below is red under last-seen semantics, and the
  // whole 1595-test suite was green under BOTH before they existed — nothing else in
  // the repo distinguishes them.

  it("totals a non-monotonic model at its high-water mark, exactly as the server does", () => {
    beforeEachReset();
    // The server is MAX over frames per model (00063_run_usage_totals_view.sql). Under
    // last-seen this reports 7000 (5000 + 0 + 2000); the server holds 5000.
    const d = deriveRunUsage([
      modelResultFrame("lead", { m: { input: 5_000, output: 0, cost: 0 } }, { turns: 3, durationMs: 10 }),
      modelResultFrame("lead", { m: { input: 1_000, output: 0, cost: 0 } }, { turns: 4, durationMs: 20 }),
      modelResultFrame("lead", { m: { input: 3_000, output: 0, cost: 0 } }, { turns: 5, durationMs: 30 }),
    ]);
    expect(d.total.fresh).toBe(5_000);
    expect(d.modelTotals).toEqual([{ model: "m", input: 5_000, cacheCreation: 0, cached: 0, out: 0, costUsd: 0 }]);
  });

  it("renders a post-reset phase as 0 tokens beside nonzero turns and duration", () => {
    beforeEachReset();
    // The PRICE of the running max, asserted so it cannot change unnoticed. Phases 2
    // and 3 did real work the run total already counts under the earlier high-water
    // mark, so their rows read 0 tokens while still reporting their own turns and
    // duration. Last-seen would render 2000 on phase 3 and overcount the run by 2000.
    const d = deriveRunUsage([
      modelResultFrame("lead", { m: { input: 5_000, output: 0, cost: 0 } }, { turns: 3, durationMs: 10 }),
      modelResultFrame("lead", { m: { input: 1_000, output: 0, cost: 0 } }, { turns: 4, durationMs: 20 }),
      modelResultFrame("lead", { m: { input: 3_000, output: 0, cost: 0 } }, { turns: 5, durationMs: 30 }),
    ]);
    expect(d.phases.map((p) => p.fresh)).toEqual([5_000, 0, 0]);
    expect(d.phases.map((p) => p.turns)).toEqual([3, 4, 5]);
    expect(d.phases.map((p) => p.durationMs)).toEqual([10, 20, 30]);
    expect(d.total).toMatchObject({ turns: 12, durationMs: 60 });
  });

  it("holds the mark through a dip that later recovers past it", () => {
    beforeEachReset();
    // [100, 80, 150]: last-seen gives 170, the server gives 150.
    const d = deriveRunUsage([
      modelResultFrame("lead", { m: { input: 100, output: 0, cost: 0 } }, { turns: 1, durationMs: 1 }),
      modelResultFrame("lead", { m: { input: 80, output: 0, cost: 0 } }, { turns: 1, durationMs: 1 }),
      modelResultFrame("lead", { m: { input: 150, output: 0, cost: 0 } }, { turns: 1, durationMs: 1 }),
    ]);
    expect(d.total.fresh).toBe(150);
  });

  it("clamps a negative cumulative at 0, the way nonNegTokens does before GREATEST", () => {
    beforeEachReset();
    // The server clamps the value at fold time (nonNegTokens), stores 0 then 100, and
    // answers 100. This test pins that BEHAVIOUR, and deliberately not any one guard:
    // the clamp is enforced in THREE independent places — `tokens()` at read, the zero
    // seed on the running max, and `clampDelta` — so no single-clamp mutation is
    // observable here. Measured 2026-08-02: `tokens()` losing its clamp reddens NOTHING
    // in the suite; `clampDelta` losing its clamp reddens six tests but not this one;
    // only the two together redden this one.
    //
    // An earlier version of this comment said last-seen would compute
    // max(0, 100 − (−5)) = 105. It does not: `tokens()` floors the −5 at read, so it
    // never reaches `prev`, and last-seen also answers 100. That counterfactual was
    // wrong, and the sentence claiming the zero seed worked "with no separate negative
    // guard" was wrong about code eight lines above it.
    const d = deriveRunUsage([
      modelResultFrame("lead", { m: { input: -5, output: -1, cost: -2 } }, { turns: 1, durationMs: 1 }),
      modelResultFrame("lead", { m: { input: 100, output: 20, cost: 3 } }, { turns: 1, durationMs: 1 }),
    ]);
    expect(d.total).toMatchObject({ fresh: 100, out: 20 });
    expect(d.total.costUsd).toBeCloseTo(3, 6);
    expect(d.modelTotals[0]).toMatchObject({ input: 100, out: 20, costUsd: 3 });
  });

  it("never lets a negative reach modelTotals, on a SINGLE frame", () => {
    beforeEachReset();
    // The gap the four-variant matrix exposed. The two-frame test above cannot see
    // this: its second frame is positive, so the mark recovers to 100 either way and
    // `modelTotals` reads correctly even with both guards gone. Only a lone negative
    // frame leaves the mark itself negative.
    //
    // `total` is floored by clampDelta and never moves, so asserting it here would
    // pin nothing — modelTotals is the ONLY surface on which the seed's flooring of
    // the mark (via mergeMax) is observable at all. Measured on this input:
    //   tokens() removed AND `?? cur`  ->  modelTotals.input -5
    // every other single- or zero-mutation variant  ->  0.
    const d = deriveRunUsage([
      modelResultFrame("lead", { m: { input: -5, cacheCreation: -7, cacheRead: -3, output: -1, cost: -2 } }, { turns: 1, durationMs: 1 }),
    ]);
    expect(d.modelTotals).toEqual([{ model: "m", input: 0, cacheCreation: 0, cached: 0, out: 0, costUsd: 0 }]);
    expect(d.total).toMatchObject({ fresh: 0, cached: 0, out: 0 });
  });

  it("maxes input and cache_creation as SEPARATE columns, not as their sum", () => {
    beforeEachReset();
    // The server GREATESTs input_tokens and cache_creation_tokens independently, and
    // MAX(a)+MAX(b) is not MAX(a+b). Here input falls 100 → 50 while cache_creation
    // rises 0 → 200: per column the run holds 100 + 200 = 300, while an implementation
    // tracking the combined `fresh` as one column sees 100 → 250 and reports 250.
    const d = deriveRunUsage([
      modelResultFrame("lead", { m: { input: 100, cacheCreation: 0, output: 0, cost: 0 } }, { turns: 1, durationMs: 1 }),
      modelResultFrame(
        "lead",
        { m: { input: 50, cacheCreation: 200, output: 0, cost: 0 } },
        { turns: 1, durationMs: 1 },
      ),
    ]);
    expect(d.total.fresh).toBe(300);
    expect(d.modelTotals[0]).toMatchObject({ input: 100, cacheCreation: 200 });
  });

  it("quantizes each model's cost to microdollars, where numericUSD does", () => {
    beforeEachReset();
    // numericUSD rounds to numeric(12,6) BEFORE storage, per frame per model. Doing it
    // at the same point makes the client's per-model cost the NEAREST DOUBLE to the
    // stored decimal (not "bit-equal to the decimal" — a double never is one), so the
    // run total carries float-summation noise (~1e-14) rather than rounding error
    // competing with the assertion's own tolerance.
    const d = deriveRunUsage([
      modelResultFrame("lead", { m: { input: 1, output: 1, cost: 67.45905024999998 } }, { turns: 1, durationMs: 1 }),
    ]);
    expect(d.modelTotals[0].costUsd).toBe(67.45905); // exactly, not toBeCloseTo
  });

  // ── Issue #195: a model id is untrusted worker payload, and it keys the RUN TOTAL.

  it("counts models named __proto__ and constructor without inheriting or vanishing", () => {
    beforeEachReset();
    // On a plain `{}` accumulator: the `__proto__` write goes through the prototype
    // setter and the model is invisible client-side while the server folds it
    // normally, and `constructor` reads back the inherited FUNCTION so every delta is
    // NaN and the NaN propagates through phases.reduce into the whole panel. A Map has
    // neither failure. Ordinary model ids cannot observe any of this.
    const d = deriveRunUsage([
      modelResultFrame(
        "lead",
        {
          // COMPUTED key, deliberately: written as a plain `__proto__:` literal, JS
          // object syntax sets the PROTOTYPE instead of creating an own property, so
          // `Object.entries` never yields it and this test would silently assert
          // nothing about the hazard it is named for.
          ["__proto__"]: { input: 100, output: 10, cost: 1 },
          constructor: { input: 200, output: 20, cost: 2 },
          "claude-opus-5": { input: 300, output: 30, cost: 3 },
        },
        { turns: 1, durationMs: 1 },
      ),
    ]);
    // A DIAGNOSTIC, not a defence, and SUBSUMED FOR COVERAGE by the value assertion
    // below: `toMatchObject({ fresh: 600 })` fails whenever `fresh` is NaN, so no
    // mutant reaches these that it would not already catch. They are first so the
    // reported failure NAMES the mechanism instead of leaving it in a diff.
    //
    // `.not.toBeNaN()` rather than `expect(Number.isNaN(x)).toBe(false)`, because that
    // form prints "expected true to be false" and names nothing at all. Measured:
    //   bare isNaN     -> expected true to be false                        <- useless
    //   not.toBeNaN    -> expected NaN not to be NaN                       <- names it
    //   toMatchObject  -> expected { fresh: NaN, … } to match { fresh: 600 }
    // Two earlier versions of this comment were wrong about these lines: one called
    // them scaffolding "made live" by the reorder (they close no gap), the next
    // claimed the reorder sharpened the message while shipping the bare form, which
    // made the message strictly worse. The matcher change is what makes the claim true.
    expect(d.total.fresh).not.toBeNaN();
    expect(d.total.out).not.toBeNaN();
    expect(d.modelTotals.map((t) => t.model)).toEqual(["__proto__", "claude-opus-5", "constructor"]);
    expect(d.total).toMatchObject({ fresh: 600, out: 60 });
    expect(d.total.costUsd).toBeCloseTo(6, 6);
    // Nothing leaked onto Object.prototype on the way through.
    expect(({} as Record<string, unknown>)["__proto__"]).toBe(Object.prototype);
  });

  it("caps a model id at 200 CODE POINTS, cutting where Go's []rune does", () => {
    beforeEachReset();
    // truncateRunes is string([]rune(s)[:200]). `.slice(0, 200)` is UTF-16 code units,
    // so on astral input it keeps only 100 code points AND ends on a lone surrogate,
    // which is not encodable as UTF-8 — Go's own round-trip would replace it with
    // U+FFFD and the two sides would key the row differently.
    const astral = "\u{1F600}".repeat(201);
    const faithful = Array.from(astral).slice(0, 200).join("");
    const d = deriveRunUsage([
      modelResultFrame("lead", { [astral]: { input: 100, output: 10, cost: 1 } }, { turns: 1, durationMs: 1 }),
    ]);
    // Assert the STRING, not its length: the mixed case below produces 200 code points
    // either way, so a length assertion passes on the broken implementation.
    expect(d.modelTotals[0].model).toBe(faithful);
    expect(d.modelTotals[0].model).not.toBe(astral.slice(0, 200));
  });

  it("caps on a rune boundary, which a code-unit cut of the same LENGTH gets wrong", () => {
    beforeEachReset();
    // 199 ASCII + 5 astral. Both cuts yield 200 code points, so only the content
    // differs — the naive one ends in an unpaired high surrogate.
    const mixed = "a".repeat(199) + "\u{1F600}".repeat(5);
    const faithful = Array.from(mixed).slice(0, 200).join("");
    const naive = mixed.slice(0, 200);
    expect(Array.from(faithful)).toHaveLength(200);
    expect(Array.from(naive)).toHaveLength(200); // the length assertion cannot tell them apart
    expect(faithful).not.toBe(naive);
    const d = deriveRunUsage([
      modelResultFrame("lead", { [mixed]: { input: 100, output: 10, cost: 1 } }, { turns: 1, durationMs: 1 }),
    ]);
    expect(d.modelTotals[0].model).toBe(faithful);
  });

  it("merges two over-long ids that the cap collides by MAX, never by sum", () => {
    beforeEachReset();
    // Server-side the cap makes these one row, merged with GREATEST. Without the cap
    // the client would keep them distinct and SUM them (300 instead of 200).
    const base = "m".repeat(200);
    const d = deriveRunUsage([
      modelResultFrame(
        "lead",
        { [base + "aaa"]: { input: 100, output: 0, cost: 0 }, [base + "bbb"]: { input: 200, output: 0, cost: 0 } },
        { turns: 1, durationMs: 1 },
      ),
    ]);
    expect(d.modelTotals).toHaveLength(1);
    expect(d.total.fresh).toBe(200);
  });

  // ── PRD #93: per-agent model, read off the same frames that carry usage ──────

  it("records each agent's model from its usage frames (one model per agent)", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsage("lead", { input: 100, output: 10, model: "claude-opus-4-8" }),
      assistantUsage("lead", { input: 100, output: 10, model: "claude-opus-4-8" }),
      assistantUsage("coder", { input: 200, output: 20, model: "claude-sonnet-5" }),
    ]);
    const byAgent = Object.fromEntries(d.agents.map((a) => [a.agent, a]));

    expect(byAgent["lead"]).toMatchObject({
      model: "claude-opus-4-8",
      otherModels: 0,
      modelCounts: { "claude-opus-4-8": 2 },
    });
    expect(byAgent["coder"]).toMatchObject({ model: "claude-sonnet-5", otherModels: 0 });
    // Mixed run → the total row's distinct set, sorted ascending.
    expect(d.agentModels).toEqual(["claude-opus-4-8", "claude-sonnet-5"]);
  });

  it("picks the most frequent model as an agent's primary, with the others counted", () => {
    beforeEachReset();
    // The minority model is deliberately seen FIRST, so this fixture discriminates:
    // an implementation that returned the first-seen (or insertion-ordered) entry
    // instead of the most frequent one would answer opus here and fail.
    const d = deriveRunUsage([
      assistantUsage("coder", { input: 100, output: 10, model: "claude-opus-4-8" }),
      assistantUsage("coder", { input: 100, output: 10, model: "claude-sonnet-5" }),
      assistantUsage("coder", { input: 100, output: 10, model: "claude-sonnet-5" }),
    ]);
    expect(d.agents[0]).toMatchObject({
      model: "claude-sonnet-5", // 2 frames beats opus's 1, despite opus being first
      otherModels: 1, // rendered "+1"
      modelCounts: { "claude-sonnet-5": 2, "claude-opus-4-8": 1 },
    });
  });

  it("breaks an equal-frequency model tie lexicographically (deterministic, not frame order)", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsage("coder", { input: 100, output: 10, model: "claude-sonnet-5" }), // seen FIRST
      assistantUsage("coder", { input: 100, output: 10, model: "claude-opus-4-8" }),
    ]);
    expect(d.agents[0]).toMatchObject({ model: "claude-opus-4-8", otherModels: 1 });
  });

  it("leaves the model null for a pre-feature agent (usage frames, no model key)", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsage("lead", { input: 100, output: 10 }),
      assistantUsage("lead", { input: 100, output: 10 }),
    ]);
    expect(d.agents[0]).toMatchObject({ model: null, otherModels: 0 });
    expect(d.agents[0].modelCounts).toEqual({}); // toMatchObject({}) would match anything
    expect(d.agentModels).toEqual([]); // total row renders "—", never a fabricated model
  });

  it("reports a single-model run as one distinct model", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsage("lead", { input: 100, output: 10, model: "claude-opus-4-8" }),
      assistantUsage("coder", { input: 100, output: 10, model: "claude-opus-4-8" }),
    ]);
    expect(d.agentModels).toEqual(["claude-opus-4-8"]);
  });

  it("ignores a model-only frame: no agent row, no model recorded (Decision 2 co-gating)", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      msg("status", "lead", { event: "init", model: "claude-opus-4-8" }), // the strip's model
      msg("text", "ghost", { text: "…", model: "claude-haiku-9" }), // model without usage
      assistantUsage("coder", { input: 100, output: 10 }),
    ]);
    expect(d.agents.map((a) => a.agent)).toEqual(["coder"]);
    expect(d.agents[0]).toMatchObject({ model: null });
    expect(d.agents[0].modelCounts).toEqual({});
    expect(d.agentModels).toEqual([]);
    expect(d.model).toBe("claude-opus-4-8"); // the init-frame path is untouched
  });

  // A model id is untrusted payload data, so it must never collide with
  // Object.prototype's keys — the count map is null-prototype (see newModelCounts).

  it("counts a model named after an Object.prototype key without inheriting it", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsage("coder", { input: 100, output: 10, model: "constructor" }),
      assistantUsage("coder", { input: 100, output: 10, model: "claude-sonnet-5" }),
      assistantUsage("coder", { input: 100, output: 10, model: "claude-sonnet-5" }),
      assistantUsage("coder", { input: 100, output: 10, model: "claude-sonnet-5" }),
    ]);
    // With a plain `{}` accumulator this count would be `<function constructor>1`
    // (a string), the b[1]-a[1] sort would go NaN, and the primary would be wrong.
    expect(d.agents[0].modelCounts["constructor"]).toBe(1);
    expect(d.agents[0].modelCounts["claude-sonnet-5"]).toBe(3);
    expect(d.agents[0]).toMatchObject({ model: "claude-sonnet-5", otherModels: 1 });
    expect(d.agentModels).toEqual(["claude-sonnet-5", "constructor"]);
  });

  it("counts a model named __proto__ and leaves Object.prototype unpolluted", () => {
    beforeEachReset();
    const d = deriveRunUsage([assistantUsage("coder", { input: 100, output: 10, model: "__proto__" })]);
    // On a plain `{}` the write is a silent no-op and the count vanishes entirely.
    expect(d.agents[0].modelCounts["__proto__"]).toBe(1);
    expect(Object.keys(d.agents[0].modelCounts)).toEqual(["__proto__"]);
    expect(d.agents[0]).toMatchObject({ model: "__proto__", otherModels: 0 });
    expect(d.agentModels).toEqual(["__proto__"]);
    // The map itself has no prototype, and nothing leaked onto Object.prototype.
    expect(Object.getPrototypeOf(d.agents[0].modelCounts)).toBeNull();
    expect(({} as Record<string, unknown>)["__proto__"]).toBe(Object.prototype);
  });

  // ── Issue #237: the LIVE aggregate, DEDUPED by `(agent_instance, usage)` ───────
  // A separate surface from the confirmed per-agent sum. The dedup collapses byte-
  // identical calls on one lane; it must NOT leak into `agents`/`agentTotal`, which
  // keep counting every frame raw.

  it("dedups two identical (agent_instance, usage) frames into ONE live record, on the lead lane", () => {
    beforeEachReset();
    // agent_instance null is the lead lane: two byte-identical calls collapse.
    const d = deriveRunUsage([
      assistantUsageInst("lead", null, { input: 100, cacheRead: 50, cacheCreation: 10, output: 5, model: "claude-opus-4-8" }),
      assistantUsageInst("lead", null, { input: 100, cacheRead: 50, cacheCreation: 10, output: 5, model: "claude-opus-4-8" }),
    ]);
    // Live: counted ONCE. fresh = input + cache_creation = 110, cached = cache_read = 50.
    expect(d.hasLiveTokens).toBe(true);
    expect(d.liveTotal).toEqual({ fresh: 110, cached: 50 });
    expect(d.liveByAgent).toEqual([{ agent: "lead", fresh: 110, cached: 50 }]);
    expect(d.liveByModel).toEqual([{ model: "claude-opus-4-8", fresh: 110, cached: 50 }]);
    // The raw confirmed per-agent sum STILL counts BOTH frames — dedup did not leak.
    expect(d.agents[0]).toMatchObject({ agent: "lead", fresh: 220, cached: 100, out: 10 });
    expect(d.agentTotal).toEqual({ fresh: 220, cached: 100, out: 10 });
  });

  it("dedups two identical frames on a SUBAGENT lane too (same non-null agent_instance)", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsageInst("coder", "inv-1", { input: 200, cacheRead: 20, output: 2, model: "claude-sonnet-5" }),
      assistantUsageInst("coder", "inv-1", { input: 200, cacheRead: 20, output: 2, model: "claude-sonnet-5" }),
    ]);
    // Live: once. fresh = 200 (no cache_creation), cached = 20.
    expect(d.liveTotal).toEqual({ fresh: 200, cached: 20 });
    expect(d.liveByAgent).toEqual([{ agent: "coder", fresh: 200, cached: 20 }]);
    expect(d.liveByModel).toEqual([{ model: "claude-sonnet-5", fresh: 200, cached: 20 }]);
    // Raw sum still counts both frames.
    expect(d.agents[0]).toMatchObject({ agent: "coder", fresh: 400, cached: 40, out: 4 });
  });

  it("does NOT over-dedup: same agent_instance but DIFFERENT usage each counts once", () => {
    beforeEachReset();
    // Same lane, distinct usage signatures → distinct keys → both counted.
    const d = deriveRunUsage([
      assistantUsageInst("coder", "inv-9", { input: 100, output: 5 }),
      assistantUsageInst("coder", "inv-9", { input: 200, output: 5 }),
    ]);
    expect(d.liveTotal).toEqual({ fresh: 300, cached: 0 });
    expect(d.liveByAgent).toEqual([{ agent: "coder", fresh: 300, cached: 0 }]);
  });

  it("splits the deduped live totals across models and agents on a multi-agent fixture", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsageInst("lead", null, { input: 100, cacheRead: 10, output: 1, model: "claude-opus-4-8" }),
      // A byte-identical lead call: collapses, so lead contributes 100/10 ONCE.
      assistantUsageInst("lead", null, { input: 100, cacheRead: 10, output: 1, model: "claude-opus-4-8" }),
      assistantUsageInst("coder", "inv-1", { input: 200, cacheRead: 20, output: 2, model: "claude-sonnet-5" }),
      assistantUsageInst("coder", "inv-1", { input: 300, cacheRead: 30, output: 3, model: "claude-sonnet-5" }),
    ]);
    // liveTotal == the DEDUPED sum of the underlying per-call fresh/cached.
    expect(d.liveTotal).toEqual({ fresh: 100 + 200 + 300, cached: 10 + 20 + 30 });
    // liveByModel: keyed by the co-gated model, sorted by model id ascending.
    expect(d.liveByModel).toEqual([
      { model: "claude-opus-4-8", fresh: 100, cached: 10 },
      { model: "claude-sonnet-5", fresh: 500, cached: 50 },
    ]);
    // liveByAgent: keyed by agent, in insertion order (lead first, coder second).
    expect(d.liveByAgent).toEqual([
      { agent: "lead", fresh: 100, cached: 10 },
      { agent: "coder", fresh: 500, cached: 50 },
    ]);
  });

  it("carries NO out and NO cost on any live aggregate (per-call output is a snapshot)", () => {
    beforeEachReset();
    const d = deriveRunUsage([
      assistantUsageInst("lead", null, { input: 100, cacheRead: 10, output: 5, model: "claude-opus-4-8" }),
    ]);
    expect("out" in d.liveTotal).toBe(false);
    expect(Object.keys(d.liveTotal).sort()).toEqual(["cached", "fresh"]);
    expect("out" in d.liveByModel[0]).toBe(false);
    expect("costUsd" in d.liveByModel[0]).toBe(false);
    expect(Object.keys(d.liveByModel[0]).sort()).toEqual(["cached", "fresh", "model"]);
    expect(Object.keys(d.liveByAgent[0]).sort()).toEqual(["agent", "cached", "fresh"]);
  });

  it("gates confirmed and live INDEPENDENTLY", () => {
    beforeEachReset();
    // Assistant usage only, no result frame: live is up, confirmed is not.
    const live = deriveRunUsage([assistantUsageInst("lead", null, { input: 100, output: 5 })]);
    expect(live.hasLiveTokens).toBe(true);
    expect(live.hasConfirmed).toBe(false);

    // A result frame only, no assistant usage: confirmed is up, live is not.
    beforeEachReset();
    const confirmed = deriveRunUsage([
      resultFrame("lead", { input: 100, cacheRead: 0, output: 5, cost: 0.1 }, { turns: 1, durationMs: 1 }),
    ]);
    expect(confirmed.hasConfirmed).toBe(true);
    expect(confirmed.hasLiveTokens).toBe(false);
  });

  it("does not flip hasLiveTokens for an all-zero usage frame (never a fabricated 0)", () => {
    beforeEachReset();
    // `readUsage` returns {fresh:0,cached:0,out:0} for any `usage` that is merely an
    // object, so a record IS seen for an empty/all-zero frame. The gate keys off POSITIVE
    // input tokens, not record presence, so the live panel never renders a fabricated 0 —
    // the same refusal the confirmed gate makes. An all-zero record still counts as seen
    // (it does not throw), but contributes nothing to liveTotal, so the gate stays false.
    const d = deriveRunUsage([
      msg("text", "lead", { text: "…", usage: {} }),
      msg("text", "lead", { text: "…", usage: { input_tokens: 0, cache_read_input_tokens: 0, output_tokens: 0 } }),
    ]);
    expect(d.hasLiveTokens).toBe(false);
    expect(d.liveTotal).toEqual({ fresh: 0, cached: 0 });
    expect(d.liveByModel).toEqual([]);
    // The very next positive frame flips it true, so the gate is not merely always-false.
    beforeEachReset();
    const withReal = deriveRunUsage([
      msg("text", "lead", { text: "…", usage: {} }),
      assistantUsage("lead", { input: 1, output: 0 }),
    ]);
    expect(withReal.hasLiveTokens).toBe(true);
  });
});

// ── The cache-percentage display invariant (issue #195 web-ux follow-up) ───────

describe("cacheDisplayPct", () => {
  it("never reads 100% while fresh tokens exist — the 99.6% case that prompted this", () => {
    // Real runs sit in the 97-99.6% band, so this is the ordinary case, not an edge.
    // `Math.round(0.996 * 100)` is 100, which labelled the strip "100% from cache"
    // beside a zero-width warn segment while 4,000 fresh tokens sat in the same panel.
    expect(cacheDisplayPct({ fresh: 4_000, cached: 996_000 })).toBe(99);
    expect(cacheDisplayPct({ fresh: 1, cached: 10_000_000 })).toBe(99);
  });

  it("never reads 0% while any cache reads exist — the mirror, which flooring would break", () => {
    // Flooring is the obvious fix for the case above and introduces this one: a small
    // but real cache ratio would render "0% from cache" and a zero-width info segment.
    expect(cacheDisplayPct({ fresh: 996_000, cached: 4_000 })).toBe(1);
    expect(cacheDisplayPct({ fresh: 10_000_000, cached: 1 })).toBe(1);
  });

  it("still reports the true endpoints when they are true", () => {
    expect(cacheDisplayPct({ fresh: 1_000, cached: 0 })).toBe(0); // nothing cached
    expect(cacheDisplayPct({ fresh: 0, cached: 1_000 })).toBe(100); // genuinely all cache
    expect(cacheDisplayPct({ fresh: 0, cached: 0 })).toBe(0); // no usage at all
  });

  it("holds the two-sided clamp, and integrality, at every ratio", () => {
    // Named for what these assertions can actually fail on. An earlier version was
    // called "keeps the two bar segments summing to exactly 100" and asserted
    // `pct + (100 - pct) === 100`, which is algebraically 100 for any integer and so
    // could not redden against any production change — while the NAME pointed a reader
    // at exactly that line and invited them to stop looking.
    //
    // The segments-sum invariant is real and is pinned where it CAN fail: at the
    // component level in RunUsage.test.tsx, `expect(widths).toEqual(["99%", "1%"])`,
    // because the warn segment's `100 - pct` is computed there and not here.
    for (const [fresh, cached] of [
      [0, 0], [1, 0], [0, 1], [1, 1], [4_000, 996_000], [996_000, 4_000],
      [1, 10_000_000], [10_000_000, 1], [21_400, 188_000], [80_300, 800_300],
    ]) {
      const pct = cacheDisplayPct({ fresh, cached });
      const at = `fresh=${fresh} cached=${cached}`;
      expect(Number.isInteger(pct), `${at}: a fractional width would break the bar`).toBe(true);
      expect(pct, at).toBeGreaterThanOrEqual(0);
      expect(pct, at).toBeLessThanOrEqual(100);
      if (fresh > 0) expect(pct, `${at}: fresh tokens exist, so 100% would be a lie`).toBeLessThan(100);
      if (cached > 0) expect(pct, `${at}: cache reads exist, so 0% would be a lie`).toBeGreaterThan(0);
    }
  });
});

// PRD #516 — the lead's context-window fill (window FILL, not token spend). It rides the
// lead's usage-latched assistant frame as `payload.context` and is derived onto
// `RunUsage.leadContext`, latest-wins, lead-only.
describe("deriveRunUsage.leadContext", () => {
  beforeEachReset();

  it("keeps the LATEST lead reading across multiple turns (last-wins)", () => {
    const d = deriveRunUsage([
      assistantUsageCtx("lead", { input: 100, output: 10 }, { used: 40_000, window: 200_000, pct: 20 }),
      assistantUsageCtx("lead", { input: 200, output: 20 }, { used: 110_000, window: 200_000, pct: 55 }),
      assistantUsageCtx("lead", { input: 300, output: 30 }, { used: 156_000, window: 200_000, pct: 78 }),
    ]);
    // The third turn wins — not the first, not a max, but the last in stream order.
    expect(d.leadContext).toEqual({ used: 156_000, window: 200_000, pct: 78 });
  });

  it("is undefined when no frame carries a context", () => {
    const d = deriveRunUsage([
      assistantUsage("lead", { input: 100, output: 10 }),
      assistantUsage("lead", { input: 200, output: 20 }),
    ]);
    expect(d.leadContext).toBeUndefined();
  });

  it("IGNORES a context on a non-lead (subagent) frame — the lead-only guard", () => {
    const d = deriveRunUsage([
      // A subagent frame carrying a synthetic context must never populate leadContext.
      assistantUsageCtx("code-reviewer", { input: 500, output: 50 }, { used: 190_000, window: 200_000, pct: 95 }),
    ]);
    expect(d.leadContext).toBeUndefined();
  });

  it("takes the lead reading even when a later subagent frame carries its own context", () => {
    const d = deriveRunUsage([
      assistantUsageCtx("lead", { input: 100, output: 10 }, { used: 80_000, window: 200_000, pct: 40 }),
      // A subagent frame AFTER the lead's must not overwrite the lead reading.
      assistantUsageCtx("worker", { input: 500, output: 50 }, { used: 199_000, window: 200_000, pct: 99 }),
    ]);
    expect(d.leadContext).toEqual({ used: 80_000, window: 200_000, pct: 40 });
  });

  it("preserves pct > 100 unclamped (over the compaction line) in the data layer", () => {
    const d = deriveRunUsage([
      assistantUsageCtx("lead", { input: 300, output: 30 }, { used: 220_000, window: 200_000, pct: 110 }),
    ]);
    expect(d.leadContext).toEqual({ used: 220_000, window: 200_000, pct: 110 });
    // The bar-width clamp lives in the render layer; the derived data keeps the truth.
    expect(d.leadContext?.pct).toBe(110);
  });

  it("ignores a malformed context (non-finite / missing fields)", () => {
    const d = deriveRunUsage([
      msg("text", "lead", {
        text: "…",
        usage: { input_tokens: 100, output_tokens: 10 },
        context: { used: 40_000, window: 200_000 }, // no pct → not a valid reading
      }),
    ]);
    expect(d.leadContext).toBeUndefined();
  });
});
