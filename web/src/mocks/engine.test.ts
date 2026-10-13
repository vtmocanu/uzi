// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { handleInput, startNewRun } from "./engine";
import { listMessages, patchRun, state } from "./store";
import { derivePlanDiffBase, derivePlanRevision } from "../pages/runView/PlanPanel";

beforeEach(() => {
  vi.useFakeTimers();
  state.messages.set("run-live", []);
  patchRun("run-live", { status: "queued" });
});
afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
});
function drain() {
  // Forty advances bound this script, including timers scheduled by timers.
  for (let i = 0; i < 40; i++) vi.advanceTimersByTime(5000);
}
it.each(["reject_plan", "revise_plan"] as const)("retains the initial engine plan after %s and revision", (kind) => {
  startNewRun("run-live");
  drain();
  expect(state.runs.get("run-live")!.status).toBe("awaiting_approval");
  handleInput("run-live", kind, "Add validation");
  drain();
  const revision = derivePlanRevision([...listMessages("run-live")]);
  expect(revision.versions).toBe(2);
  expect(revision.priorPlans).toHaveLength(1);
  const messages = [...listMessages("run-live")];
  const plans = messages.filter((m) => m.kind === "plan");
  for (const plan of plans) {
    expect((plan.payload as { plan_md: string }).plan_md.trim().length).toBeGreaterThan(0);
  }
  expect(derivePlanDiffBase(messages)?.base.text).toBe(revision.priorPlans[0]);
  expect(derivePlanDiffBase(messages)?.base.seq).toBe(plans[0].seq);
});
