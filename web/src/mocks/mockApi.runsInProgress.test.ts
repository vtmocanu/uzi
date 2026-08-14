// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { isTerminalRun, type Run } from "../lib/api";
import { mockChatRuns, mockCrewRuns, mockHistoryRuns, mockLaneRuns, mockRuns } from "./data";

// Each test re-imports a fresh mockApi so its in-memory run state starts from the seed.
async function freshApi() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

// The runs the mock store seeds into state.runs, deduped by id EXACTLY as store.ts's Map
// does (later arrays win on an id collision). An independent reconstruction of the seed so
// the expectation is DERIVED from the fixtures, never snapshotted as a bare number — a
// fixture gaining a run must not turn this red for a reason that is not about the mock.
const seeded: Run[] = (() => {
  const m = new Map<string, Run>();
  for (const r of [...mockRuns, ...mockChatRuns, ...mockCrewRuns, ...mockLaneRuns, ...mockHistoryRuns]) m.set(r.id, r);
  return [...m.values()];
})();

// The badge's predicate (PRD #239 Decision 1 + Decision 4): non-terminal, kind NOT IN
// ('chat','judge') — the same scope the real ListRunsForUser uses.
const inProgress = (r: Run) => !isTerminalRun(r.status) && r.kind !== "chat" && r.kind !== "judge";
const expected = seeded.filter(inProgress).length;

describe("mockApi.runsInProgressCount parity (PRD #239 M2)", () => {
  // Fixture precondition — the boundary the count must be able to move across. Without a run
  // in EACH of these buckets the exclusion the count claims to make would be vacuous: a judge
  // run that could never be counted proves nothing about judge being excluded.
  it("fixtures discriminate the boundary (Decisions 1 + 4)", () => {
    expect(seeded.some((r) => isTerminalRun(r.status))).toBe(true); // a terminal run to drop
    expect(seeded.some(inProgress)).toBe(true); // an included, counted run
    expect(seeded.some((r) => r.status === "awaiting_approval" || r.status === "awaiting_input")).toBe(true);
    expect(seeded.some((r) => r.status === "limit_wait")).toBe(true);
    // The excluded kinds must be present AND non-terminal, or excluding them changes nothing.
    expect(seeded.some((r) => r.kind === "chat" && !isTerminalRun(r.status))).toBe(true);
    expect(seeded.some((r) => r.kind === "judge" && !isTerminalRun(r.status))).toBe(true);
    expect(expected).toBeGreaterThan(0); // the demo shows a real, non-zero number
  });

  it("counts exactly the non-terminal, non-chat/judge runs", async () => {
    const api = await freshApi();
    expect((await api.runsInProgressCount()).count).toBe(expected);
  });

  // The three exclusions, each pinned by showing the count would be STRICTLY larger if that
  // filter were dropped — so `toBe(expected)` above genuinely rejects a mock that forgot it.
  it("EXCLUDES terminal runs", () => {
    const withTerminal = seeded.filter((r) => r.kind !== "chat" && r.kind !== "judge").length;
    expect(withTerminal).toBeGreaterThan(expected);
  });

  it("EXCLUDES chat runs", () => {
    const withChat = seeded.filter((r) => !isTerminalRun(r.status) && r.kind !== "judge").length;
    expect(withChat).toBeGreaterThan(expected);
  });

  it("EXCLUDES judge runs", () => {
    const withJudge = seeded.filter((r) => !isTerminalRun(r.status) && r.kind !== "chat").length;
    expect(withJudge).toBeGreaterThan(expected);
  });
});
