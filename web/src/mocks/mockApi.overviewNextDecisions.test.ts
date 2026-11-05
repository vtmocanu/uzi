// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { overviewDecisions } from "../lib/overview";

// ?mock=overview-next shows every decision kind in the card (PRD #3878 M2).
describe("overview-next mock scenario decisions", () => {
  afterEach(() => localStorage.clear());

  it("seeds a plan review, a question, an owner park and deduplicated open PRs", async () => {
    vi.resetModules();
    localStorage.setItem("uzi_mock_scenario", "overview-next");
    const { mockApi } = await import("./mockApi");
    const runs = (await mockApi.listRuns({ view: "summary" })).runs;
    const items = overviewDecisions(runs);
    const kinds = new Set(items.map((i) => i.kind));
    expect(kinds).toEqual(new Set(["plan", "question", "park", "pr"]));
    const prs = items.filter((i) => i.kind === "pr").map((i) => i.run);
    expect(new Set(prs.map((r) => `${r.repo_id}:${r.mr_iid}`)).size).toBe(prs.length);
    // run-ci-halted and its retry share one MR: exactly one of them is listed.
    expect(prs.filter((r) => r.id === "run-ci-halted" || r.id === "run-ov-ci-halted-retry")).toHaveLength(1);
  });
});
