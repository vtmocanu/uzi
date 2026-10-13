import { describe, expect, it, vi } from "vitest";
import { seedUnifiedProgress } from "./data/unifiedProgress";
import { mockScenario } from "./mockScenario";
import type { Run, RunMessage } from "../lib/apiTypes";
vi.mock("./mockScenario", () => ({mockScenario: vi.fn()}));

describe("run-progress named scenario", () => {
  it("leaves other scenarios alone", () => {
    vi.mocked(mockScenario).mockReturnValue("custody");
    const runs = new Map<string, Run>();
    const messages = new Map<string, RunMessage[]>();
    seedUnifiedProgress(runs, messages);
    expect(runs.size).toBe(0);
    expect(messages.size).toBe(0);
  });
  it("seeds current-contract running, approval, failed, completed and assigned-owner examples", () => {
    vi.mocked(mockScenario).mockReturnValue("run-progress");
    const runs = new Map<string, Run>();
    const messages = new Map<string, RunMessage[]>();
    seedUnifiedProgress(runs, messages);
    expect([...runs.keys()]).toEqual(["run-progress-running", "run-progress-approval", "run-progress-failed", "run-progress-completed", "run-progress-assigned"]);
    const running = runs.get("run-progress-running")!;
    expect(running.milestones_live?.[0].lanes.map(l => l.agent)).toEqual(["documenter", "spec-keeper"]);
    expect(running.milestones_agents?.[0].agent).toBe("documenter");
    expect(running.progress?.active_milestone_id).toBe("m9");
    expect(running.progress?.now_note?.text).toBe("Working on the run-recovery and CLI docs.");
    expect(running.iteration_count).toBeGreaterThan(1);
    expect(running.usage_estimated_tail?.cost_status).toBe("estimated");
    expect(running.usage_estimated_tail?.cost_usd).toBeGreaterThan(0);
    const assigned = runs.get("run-progress-assigned")!;
    expect(assigned.milestones_agents?.[0].agent).toBe("architect");
    expect(assigned.milestones_live?.[0].lanes.map(l => l.agent)).not.toContain("architect");
    const approval = runs.get("run-progress-approval")!;
    expect(approval.status).toBe("awaiting_approval");
    expect(approval.milestones).toBeNull();
    expect(approval.milestones_candidate).toHaveLength(4);
    for (const state of ["failed", "completed"] as const) {
      const run = runs.get(`run-progress-${state}`)!;
      expect(run.status).toBe(state);
      expect(run.current_activity).toBeNull();
      expect(run.milestones_live).toEqual([]);
      expect(run.progress).toBeNull();
      expect(messages.get(run.id)?.length).toBeGreaterThan(0);
    }
    expect(runs.get("run-progress-completed")?.milestones_completed).toEqual(["m6", "m7", "m8", "m9"]);
  });
});
