import { describe, it, expect } from "vitest";
import { canReworkNow, canToggleMrRework, effectiveMrRework } from "./mrRework";
import type { Run } from "./api";

// PRD #841 + #1202. canToggleMrRework gates the per-run toggle; canReworkNow gates the
// on-demand "Rework now" affordance. The reworkable-kind set was widened past `issue` to
// include `prompt` and `self_improve` by PRD #908 (those runs open MRs the watcher
// reworks too), so both predicates share that set.

describe("canToggleMrRework — the per-run toggle gate (PRD #841 / #908)", () => {
  it("is true for issue / prompt / self_improve with an OPEN MR", () => {
    for (const kind of ["issue", "prompt", "self_improve"] as const) {
      expect(canToggleMrRework({ kind, mr_state: "opened" })).toBe(true);
    }
  });

  it("is true for the three kinds when the MR is not yet observed (null)", () => {
    for (const kind of ["issue", "prompt", "self_improve"] as const) {
      expect(canToggleMrRework({ kind, mr_state: null })).toBe(true);
    }
  });

  it("is false for a chat run (never opens a reworkable MR)", () => {
    expect(canToggleMrRework({ kind: "chat", mr_state: "opened" })).toBe(false);
  });

  it("is false once the MR is merged or closed, even for a reworkable kind", () => {
    expect(canToggleMrRework({ kind: "issue", mr_state: "merged" })).toBe(false);
    expect(canToggleMrRework({ kind: "prompt", mr_state: "closed" })).toBe(false);
  });
});

describe("canReworkNow — the on-demand rework gate (PRD #1202)", () => {
  const base: Pick<Run, "kind" | "status" | "mr_iid" | "mr_state"> = {
    kind: "issue",
    status: "completed",
    mr_iid: 42,
    mr_state: "opened",
  };

  it("is true for a COMPLETED issue / prompt / self_improve run with an open MR", () => {
    for (const kind of ["issue", "prompt", "self_improve"] as const) {
      expect(canReworkNow({ ...base, kind })).toBe(true);
    }
  });

  it("is false while the run is still running (not completed)", () => {
    expect(canReworkNow({ ...base, status: "running" })).toBe(false);
  });

  it("is false when there is no MR (mr_iid null)", () => {
    expect(canReworkNow({ ...base, mr_iid: null })).toBe(false);
  });

  it("is false once the MR is merged", () => {
    expect(canReworkNow({ ...base, mr_state: "merged" })).toBe(false);
  });

  it("is false for a chat run", () => {
    expect(canReworkNow({ ...base, kind: "chat" })).toBe(false);
  });
});

// A tiny guard so a refactor of effectiveMrRework's tri-state coalesce can't silently
// drop through the wrong default here (kept minimal — the toggle's own suite covers it).
describe("effectiveMrRework — tri-state coalesce (PRD #841)", () => {
  it("run override wins; else user default; else default-ON", () => {
    expect(effectiveMrRework({ mr_rework_enabled: false }, true)).toBe(false);
    expect(effectiveMrRework({ mr_rework_enabled: null }, false)).toBe(false);
    expect(effectiveMrRework({ mr_rework_enabled: null }, null)).toBe(true);
  });
});
