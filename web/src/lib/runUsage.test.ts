import { describe, it, expect } from "vitest";
import { deriveRunUsage } from "./runUsage";
import type { RunMessage } from "./api";

let seq = 0;
function msg(kind: string, agent: string | null, payload: unknown): RunMessage {
  return { seq: ++seq, kind, agent, agent_instance: null, agent_label: null, payload, created_at: "2026-07-12T00:00:00Z" };
}
function beforeEachReset() {
  seq = 0;
}

// A cumulative result frame (verdict b): usage totals are session-to-date.
function resultFrame(
  agent: string,
  cum: { input: number; cacheRead: number; cacheCreation?: number; output: number; cost: number },
  meta: { turns: number; durationMs: number; kind?: "status" | "error"; subtype?: string },
): RunMessage {
  return msg(meta.kind ?? "status", agent, {
    event: "result",
    subtype: meta.subtype ?? (meta.kind === "error" ? "error_max_turns" : "success"),
    num_turns: meta.turns,
    duration_ms: meta.durationMs,
    total_cost_usd: cum.cost,
    usage: {
      input_tokens: cum.input,
      cache_read_input_tokens: cum.cacheRead,
      cache_creation_input_tokens: cum.cacheCreation ?? 0,
      output_tokens: cum.output,
    },
  });
}

// An assistant frame carrying per-call usage (attached by the worker to one msg).
function assistantUsage(
  agent: string,
  u: { input: number; cacheRead?: number; cacheCreation?: number; output: number },
): RunMessage {
  return msg("text", agent, {
    text: "…",
    usage: {
      input_tokens: u.input,
      cache_read_input_tokens: u.cacheRead ?? null, // BetaUsage cache fields are nullable
      cache_creation_input_tokens: u.cacheCreation ?? null,
      output_tokens: u.output,
    },
  });
}

describe("deriveRunUsage", () => {
  it("differences cumulative result frames into per-phase deltas; total is the last cumulative", () => {
    beforeEachReset();
    const messages = [
      msg("status", "lead", { event: "init", model: "claude-sonnet-5" }),
      resultFrame("lead", { input: 21_400, cacheRead: 188_000, output: 6_100, cost: 0.24 }, { turns: 9, durationMs: 100_000 }),
      resultFrame("lead", { input: 80_300, cacheRead: 800_300, output: 34_800, cost: 1.26 }, { turns: 34, durationMs: 200_000 }),
    ];
    const d = deriveRunUsage(messages);

    expect(d.hasUsage).toBe(true);
    expect(d.model).toBe("claude-sonnet-5");
    expect(d.phases.map((p) => p.label)).toEqual(["Plan", "Implement · iteration 1"]);

    // Plan = first frame's cumulative (no predecessor).
    expect(d.phases[0]).toMatchObject({ fresh: 21_400, cached: 188_000, out: 6_100, turns: 9 });
    expect(d.phases[0].costUsd).toBeCloseTo(0.24, 6);
    // Iteration 1 = frame2 − frame1 (delta), turns/duration are per-invocation (raw).
    expect(d.phases[1]).toMatchObject({ fresh: 58_900, cached: 612_300, out: 28_700, turns: 34, durationMs: 200_000 });
    expect(d.phases[1].costUsd).toBeCloseTo(1.02, 6);

    // Run total = sum of deltas = last cumulative; turns/duration SUM.
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

  it("has no usage for a pre-feature run (result frames without a usage object)", () => {
    beforeEachReset();
    const messages = [
      msg("status", "lead", { event: "result", subtype: "success", num_turns: 3 }), // no usage key
      msg("text", "lead", { text: "done" }),
    ];
    const d = deriveRunUsage(messages);
    expect(d.hasUsage).toBe(false);
    expect(d.phases).toHaveLength(0);
    expect(d.agents).toHaveLength(0);
  });

  it("folds an error result frame's usage (a failed phase still spent tokens)", () => {
    beforeEachReset();
    const messages = [
      resultFrame("lead", { input: 500, cacheRead: 0, output: 120, cost: 0.03 }, { turns: 7, durationMs: 44_000, kind: "error" }),
    ];
    const d = deriveRunUsage(messages);
    expect(d.hasUsage).toBe(true);
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
});
