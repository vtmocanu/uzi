import { describe, expect, it } from "vitest";
import { mockAwaitingMessages } from "./data/runHistories";
import { mockRuns } from "./data/runs";
import { mockGatedCheckedMessages } from "./data/planCrossCheckGated";

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

describe("PRD #3708 gated plan cross-check mock evidence", () => {
  it("keeps only each candidate's preceding dispositions", () => {
    const plans = mockGatedCheckedMessages.filter((m) => m.kind === "plan").map((m) => (m.payload as { text: string }).text);
    expect(plans).toHaveLength(3);
    const marker = (text: string) => text.split("## Cross-check dispositions")[1] ?? "";
    expect(marker(plans[0])).toBe("");
    expect(marker(plans[1])).toContain("Delivery is not idempotent");
    expect(marker(plans[1])).not.toContain("dedupe key");
    expect(marker(plans[2])).toContain("dedupe key");
    expect(marker(plans[2])).not.toContain("Delivery is not idempotent");
    const gated = mockRuns.find((run) => run.id === "run-gated-checked")!;
    expect(gated.plan_md).toBe(plans[2]);
    expect(gated.plan_cross_check_gated).toBe(true);
    expect(gated.plan_cross_check_required).toBe(false);
    const rounds = gated.plan_cross_check_summary!.rounds!;
    expect(rounds.map((r) => r.response.state)).toEqual(["reported", "reported", "no_following_candidate"]);
    expect(marker(plans[1])).toContain(rounds[0].response.text);
    expect(marker(plans[2])).toContain(rounds[1].response.text);
  });
  it("has an in-progress gated run with a future deadline and no plan", () => {
    const checking = mockRuns.find((run) => run.id === "run-gated-checking")!;
    expect(checking.status).toBe("running");
    expect(checking.plan_md).toBeNull();
    expect(checking.plan_cross_check_summary?.verdict).toBe("pending");
    expect(Date.parse(checking.plan_cross_check_summary!.deadline_at!)).toBeGreaterThan(Date.now());
  });
});
