// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { mockRuns } from "./data/runs";
import { state } from "./store";
import type { MilestoneResetOptions } from "../lib/apiTypes";

const id = "mock-reset-consent";
const reset: MilestoneResetOptions = {
  reconciliation: { mode: "reset", reason: "Replace the work", expected_completion_revision: null },
  expected_reset_state_digest: "reviewed-digest",
};
beforeEach(() => {
  vi.useFakeTimers();
  state.runs.set(id, { ...mockRuns[0], id, status: "awaiting_approval", gate_revision: 4,
    completion_revision: null, reset_state_digest: "reviewed-digest",
    milestones: [{ id: "M1", title: "Original work" }],
    milestones_candidate: [{ id: "M1", title: "Replacement work" }],
    milestones_completed: ["M1"], milestones_in_progress: ["M1"], milestones_agents: [],
  });
});
afterEach(() => {
  vi.clearAllTimers(); vi.useRealTimers();
  state.runs.delete(id); state.messages.delete(id);
});
it("refuses ordinary changed-plan approval and reset without the reviewed gate", async () => {
  await expect(mockApi.submitRunInput(id, "approve_plan", "", undefined, undefined, undefined, 4))
    .rejects.toMatchObject({ body: { reason: "milestone_reconciliation_required" } });
  await expect(mockApi.submitRunInput(id, "approve_plan", "", undefined, undefined, undefined, undefined, reset))
    .rejects.toMatchObject({ body: { reason: "milestone_reconciliation_conflict" } });
  expect(state.runs.get(id)?.milestones_completed).toEqual(["M1"]);
});
it("resets progress while retaining a legacy null revision and readable audit history", async () => {
  const pending = mockApi.submitRunInput(id, "approve_plan", "", undefined, undefined, undefined, 4, reset);
  await vi.advanceTimersByTimeAsync(150);
  await pending;
  const current = state.runs.get(id)!;
  expect(current.milestones).toEqual([{ id: "M1", title: "Replacement work" }]);
  expect(current.milestones_completed).toEqual([]);
  expect(current.completion_revision).toBeNull();
  expect(current.milestone_reconciliations?.[0].reason).toBe("Replace the work");
});
it("pages audit records without losing records between cursors", async () => {
  const current = state.runs.get(id)!;
  current.milestone_reconciliations = Array.from({ length: 51 }, (_, i) => ({ gate_revision: 100 - i }));
  const latestRead = mockApi.getRun(id);
  await vi.advanceTimersByTimeAsync(60);
  const latest = (await latestRead).run;
  expect(latest.milestone_reconciliations).toHaveLength(50);
  expect(latest.milestone_reconciliations?.[49].gate_revision).toBe(51);
  expect(latest.milestone_reconciliations_next_before_gate_revision).toBe(51);
  const olderRead = mockApi.getRun(id, 51);
  await vi.advanceTimersByTimeAsync(60);
  expect((await olderRead).run.milestone_reconciliations?.map(row => row.gate_revision)).toEqual([50]);
});
