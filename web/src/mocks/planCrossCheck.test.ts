import { describe, expect, it } from "vitest";
import { mockAwaitingMessages } from "./data/runHistories";
import { mockRuns } from "./data/runs";

describe("M1 plan cross-check mock evidence", () => {
  it("provides parked findings and earlier-plan evidence without a terminal gate reason", () => {
    const parked = mockRuns.find((run) => run.id === "run-awaiting")!;
    expect(parked.status).toBe("awaiting_approval");
    expect(parked.plan_cross_check_summary?.findings?.items?.length).toBeGreaterThan(0);
    expect(parked.plan_cross_check_summary?.historical).toBe(false);
    const event = mockAwaitingMessages.find((m) => m.kind === "cross_check")!;
    expect(event.agent).toBeNull();
    expect(event.payload).toMatchObject({ stage: "plan", verdict: "revise", findings: parked.plan_cross_check_summary?.findings });
    const historical = mockRuns.find((run) => run.plan_cross_check_summary?.historical)!;
    expect(historical.status).toBe("completed");
    expect(historical.plan_cross_check_summary?.findings?.summary).toContain("Earlier-plan");
    expect(historical.plan_cross_check_gate_reason).toBeUndefined();
    expect(historical.plan_cross_check_summary?.reason_class).toBe("approve");
  });
});
