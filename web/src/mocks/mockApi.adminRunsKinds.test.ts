import { afterEach, describe, expect, it, vi } from "vitest";
vi.mock("./mockApi/shared", async original => ({ ...await original<typeof import("./mockApi/shared")>(), delay: <T>(value: T) => Promise.resolve(value) }));
import { mockApi } from "./mockApi";
import { state } from "./store";
import { mockRuns } from "./data";

// #3987: the mock admin list mirrors ListActiveRunsAll, which omits chat, judge and
// cross_check runs (api/internal/store/queries/runtime.sql).
const seeded = ["run-kind-judge", "run-kind-cross-check", "run-kind-issue"];

afterEach(() => {
  for (const id of seeded) state.runs.delete(id);
});

describe("mock adminListRuns run kinds", () => {
  it("omits judge and cross_check runs while still listing an ordinary active run", async () => {
    state.runs.set("run-kind-judge", { ...mockRuns[0], id: "run-kind-judge", kind: "judge", status: "running" });
    state.runs.set("run-kind-cross-check", { ...mockRuns[0], id: "run-kind-cross-check", kind: "cross_check", status: "running" });
    state.runs.set("run-kind-issue", { ...mockRuns[0], id: "run-kind-issue", kind: "issue", status: "running" });

    for (const rows of [(await mockApi.adminListRuns()).runs, (await mockApi.adminListRuns({ view: "summary" })).runs]) {
      const ids = rows.map(r => r.id);
      expect(ids).toContain("run-kind-issue");
      expect(ids).not.toContain("run-kind-judge");
      expect(ids).not.toContain("run-kind-cross-check");
      // The non-terminal judge fixture is excluded too.
      expect(ids).not.toContain("run-judge-meta");
    }
  });
});
