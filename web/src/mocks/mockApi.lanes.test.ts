import { describe, it, expect } from "vitest";
import { mockBusyMessages, mockLaneMessages, mockLaneRuns, mockLiveTokensMessages } from "./data";

// Mock mode is the only way to browse this feature without a live worker, and until
// PRD #99's M6 every fixture in data.ts hardcoded agent_instance/agent_label to null.
// The consequence was not cosmetic: `VITE_UZI_MOCK=1 npm run dev` rendered every run
// as legacy role lanes, so the whole feature was undemoable AND unreviewable in a
// browser — the same blind spot that let the Vitest suite stay green while the lane
// grouping was removable.
//
// These assertions are deliberately about the FIXTURE's expressive power, not about
// rendering: they fail if someone reverts the columns to null or collapses the two
// coder invocations into one, which is exactly how the gap would come back.
describe("PRD #99 mock lane fixtures", () => {
  it("gives run-lanes two DISTINCT same-role invocations with their own labels", () => {
    const coders = mockLaneMessages.filter((m) => m.agent === "coder");
    const ids = new Set(coders.map((m) => m.agent_instance));
    expect(coders.length).toBeGreaterThan(0);
    expect(ids.has(null)).toBe(false);
    // Two invocations, not one: a single id here is Problem 2 and the demo shows a
    // merged `coder` block instead of the feature.
    expect(ids.size).toBe(2);
    expect(new Set(coders.map((m) => m.agent_label)).size).toBe(2);
  });

  it("keeps the two coder invocations NON-ADJACENT", () => {
    // Contiguous frames of one role render correctly even under the pre-#99
    // consecutive-author grouping, so an adjacent pair cannot demonstrate the fix.
    const roles = mockLaneMessages.filter((m) => m.agent_instance !== null).map((m) => m.agent_instance);
    const firstA = roles.indexOf("toolu_01coderA");
    const firstB = roles.indexOf("toolu_01coderB");
    const between = roles.slice(Math.min(firstA, firstB) + 1, Math.max(firstA, firstB));
    expect(between.some((id) => id !== "toolu_01coderA" && id !== "toolu_01coderB")).toBe(true);
  });

  it("leaves the lead's own turns NULL on both columns (the role-fallback lane)", () => {
    const lead = mockLaneMessages.filter((m) => m.agent === "lead");
    expect(lead.length).toBeGreaterThan(0);
    expect(lead.every((m) => m.agent_instance === null && m.agent_label === null)).toBe(true);
  });

  it("gives run-busy a doubled role plus a labelless lane and an over-clamp label", () => {
    const byRole = new Map<string, Set<string | null>>();
    for (const m of mockBusyMessages) {
      if (!m.agent || m.agent_instance === null) continue;
      const set = byRole.get(m.agent) ?? new Set();
      set.add(m.agent_instance);
      byRole.set(m.agent, set);
    }
    // At least one role has two instances -> the rollup is reachable.
    expect([...byRole.values()].some((s) => s.size >= 2)).toBe(true);
    // A lane with an instance but no label -> the role-only title path.
    expect(mockBusyMessages.some((m) => m.agent_instance !== null && m.agent_label === null)).toBe(true);
    // A label past the 48-rune layout clamp -> the truncation signal is visible.
    expect(mockBusyMessages.some((m) => (m.agent_label?.length ?? 0) > 48)).toBe(true);
  });

  it("registers the demo runs so they are reachable in mock mode", () => {
    expect(mockLaneRuns.map((r) => r.id).sort()).toEqual(["run-busy", "run-lanes", "run-live-tokens", "run-stalled"]);
  });

  it("makes run-stalled differ from run-busy ONLY in health", () => {
    // The pair is the point: `stalled` is a run-level condition, not a lane property,
    // so the only way to browse "a stalled role sorts first" is a second run over the
    // same stream. If these two ever diverge in anything but health, the comparison
    // stops being a controlled one and run-busy's ordering tests stop describing it.
    const busy = mockLaneRuns.find((r) => r.id === "run-busy");
    const stalled = mockLaneRuns.find((r) => r.id === "run-stalled");
    expect(busy?.health).toBe("ok");
    expect(stalled?.health).toBe("looping");
    expect(stalled?.status).toBe(busy?.status);
  });
});

// PRD #237: the live-only token fixture. These assertions guard the three properties the
// run-page's LIVE token section needs to be demoable — usage-bearing frames spanning two
// model keys, byte-identical (agent_instance, usage) duplicates the panel must collapse,
// and NO result frame — so a later edit that "tidies" the fixture into a shape that can no
// longer exhibit the state fails here rather than silently in a browser.
describe("PRD #237 mock live-token fixture", () => {
  const usageFrames = mockLiveTokensMessages.filter(
    (m) => m.kind === "text" && !!(m.payload as { usage?: unknown }).usage,
  );

  it("has per-call usage frames spanning exactly two model keys", () => {
    expect(usageFrames.length).toBeGreaterThan(0);
    const models = new Set(usageFrames.map((m) => (m.payload as { model?: string }).model));
    expect(models).toEqual(new Set(["claude-opus-4-8", "claude-sonnet-5"]));
  });

  it("carries a lead lane (null instance) and a subagent lane (non-null instance)", () => {
    expect(usageFrames.some((m) => m.agent === "lead" && m.agent_instance === null)).toBe(true);
    expect(usageFrames.some((m) => m.agent === "coder" && m.agent_instance !== null)).toBe(true);
  });

  it("includes a duplicate (agent_instance, usage) pair the panel must collapse", () => {
    const keys = usageFrames.map((m) =>
      JSON.stringify([m.agent_instance, (m.payload as { usage: unknown }).usage]),
    );
    // A byte-identical (agent_instance, usage) appears at least twice — the SDK's
    // repeat-usage behaviour the dedup exists to absorb.
    expect(keys.length - new Set(keys).size).toBeGreaterThanOrEqual(1);
  });

  it("carries NO result frame, so tokens are live-only (nothing confirmed)", () => {
    expect(
      mockLiveTokensMessages.some((m) => (m.payload as { event?: string }).event === "result"),
    ).toBe(false);
  });

  it("keeps each agent's usage frames on a single model", () => {
    const byAgent = new Map<string, Set<string | undefined>>();
    for (const m of usageFrames) {
      const set = byAgent.get(m.agent!) ?? new Set();
      set.add((m.payload as { model?: string }).model);
      byAgent.set(m.agent!, set);
    }
    for (const models of byAgent.values()) expect(models.size).toBe(1);
  });
});
