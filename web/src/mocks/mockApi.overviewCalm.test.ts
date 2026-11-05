// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { overviewDecisions } from "../lib/overview";

// ?mock=overview-calm: active runs, but nothing for the viewer to decide (PRD #3878).
describe("overview-calm mock scenario", () => {
  afterEach(() => localStorage.clear());

  it("keeps active runs and leaves the decision list empty", async () => {
    vi.resetModules();
    localStorage.setItem("uzi_mock_scenario", "overview-calm");
    const { mockApi } = await import("./mockApi");
    const runs = (await mockApi.listRuns({ view: "summary" })).runs;
    expect(runs.some((r) => r.status === "running")).toBe(true);
    const holds = await mockApi.getRecoveryHolds();
    expect(holds.aggregate.open_holds).toBe(0);
    expect(overviewDecisions(runs, holds)).toEqual([]);
  });

  it("does not change the default scenario", async () => {
    vi.resetModules();
    const { mockApi } = await import("./mockApi");
    const runs = (await mockApi.listRuns({ view: "summary" })).runs;
    expect(overviewDecisions(runs).length).toBeGreaterThan(0);
  });
});
