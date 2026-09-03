// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MilestoneChecklist } from "./RunView";
import type { Run, RunActivity } from "../lib/api";

// PRD #1064 M3: the run-view "now" line hosted by MilestoneChecklist — the three variants
// (active under the in-progress row, unattached under the header, waiting), the D4 first-
// in-progress selection, the untrusted-field folding, the reduced-motion dot, and the D5
// milestone-only / byte-compat contract. `activity` is derived in RunView from the live
// frames and passed down; here it is supplied directly.

const AT = "2026-09-03T12:00:00Z";
const NOW = Date.parse(AT) + 40_000; // 40s after the activity instant → "40s"

// MilestoneChecklist ages the now line via useNow; pin it so "40s" is deterministic.
vi.mock("../lib/useNow", () => ({ useNow: () => NOW }));

function anActivity(over: Partial<RunActivity> = {}): RunActivity {
  return {
    agent: "coder",
    agent_label: "Decouple both detectors from the branch convention",
    tool: "Edit",
    detail: "api/internal/poller/ci_autofix.go",
    at: AT,
    seq: 12,
    ...over,
  };
}

function run(over: Partial<Run>): Run {
  return {
    id: "r1",
    repo_id: "repo1",
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    kind: "issue",
    issue_iid: 87,
    issue_title: "Add rate limiting",
    issue_description: "d",
    title: null,
    resume_of_run_id: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: null,
    model: null,
    override_subagent_model: false,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    stop_reason: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    milestones: null,
    milestones_completed: null,
    milestones_in_progress: null,
    milestones_candidate: null,
    budget_max_iterations: null,
    budget_wall_seconds: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: AT,
    updated_at: AT,
    ...over,
  };
}

const milestones = [
  { id: "m1", title: "First milestone" },
  { id: "m2", title: "Second milestone" },
  { id: "m3", title: "Third milestone" },
];

afterEach(cleanup);

describe("MilestoneChecklist now line (PRD #1064 M3)", () => {
  it("renders the active strip under the in-progress row and names the id in the header", () => {
    render(
      <MilestoneChecklist
        run={run({ milestones, milestones_completed: ["m1"], milestones_in_progress: ["m2"] })}
        activity={anActivity()}
      />,
    );
    expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
    expect(screen.getByText("coder")).toBeTruthy();
    expect(screen.getByText("Decouple both detectors from the branch convention")).toBeTruthy();
    expect(screen.getByText("Edit api/internal/poller/ci_autofix.go")).toBeTruthy();
    expect(screen.getByText("40s ago")).toBeTruthy();
  });

  it("renders an UNATTACHED strip under the header when nothing is declared in progress", () => {
    render(
      <MilestoneChecklist
        run={run({ milestones, milestones_completed: ["m1"], milestones_in_progress: [] })}
        activity={anActivity()}
      />,
    );
    // No in-progress id → no ◐ marker in the header, but the now line still shows.
    expect(screen.queryByText(/◐/)).toBeNull();
    expect(screen.getByText("coder")).toBeTruthy();
    expect(screen.getByText("40s ago")).toBeTruthy();
  });

  it("renders NO strip when there is no activity (a milestone run with nothing acting)", () => {
    render(
      <MilestoneChecklist
        run={run({ milestones, milestones_completed: ["m1"], milestones_in_progress: ["m2"] })}
        activity={null}
      />,
    );
    // The checklist itself still renders (the ◐ header marker is present), but no now line.
    expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
    expect(screen.queryByText("coder")).toBeNull();
    expect(screen.queryByText("40s ago")).toBeNull();
  });

  it("shows the WAITING variant on a rate/pool-limited run", () => {
    render(
      <MilestoneChecklist
        run={run({ status: "limit_wait", milestones, milestones_completed: ["m1"], milestones_in_progress: ["m2"] })}
        activity={anActivity()}
      />,
    );
    expect(screen.getByText("waiting on rate limit · 40s")).toBeTruthy();
    expect(screen.queryByText("40s ago")).toBeNull();
  });

  it("D4: with several ids in progress, only the FIRST by frozen order is named and carries the strip", () => {
    render(
      <MilestoneChecklist
        run={run({ milestones, milestones_completed: [], milestones_in_progress: ["m3", "m2"] })}
        activity={anActivity()}
      />,
    );
    // Frozen order is m1,m2,m3 → m2 is first-in-progress, not m3 (input order).
    expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
    expect(screen.queryByText(/◐\s*m3/)).toBeNull();
    // Exactly one now strip, even though two ids are in progress.
    expect(screen.getAllByText("40s ago")).toHaveLength(1);
    // The second in-progress id still renders as an in-progress mark (◐).
    expect(screen.getAllByLabelText("in progress").length).toBeGreaterThanOrEqual(2);
  });

  it("folds all four untrusted fields through stripUnsafeChars", () => {
    render(
      <MilestoneChecklist
        run={run({ milestones, milestones_completed: [], milestones_in_progress: ["m2"] })}
        // RIGHT-TO-LEFT OVERRIDE (U+202E) and zero-width space (U+200B) as escapes.
        activity={anActivity({
          agent: "co\u202Eder",
          agent_label: "task\u200Bone",
          tool: "Ed\u202Eit",
          detail: "api/\u200Bx.go",
        })}
      />,
    );
    expect(screen.getByText("coder")).toBeTruthy();
    expect(screen.getByText("taskone")).toBeTruthy();
    expect(screen.getByText("Edit api/x.go")).toBeTruthy();
  });

  it("the now-line dot uses animate-pulse (index.css neutralizes it under prefers-reduced-motion)", () => {
    const { container } = render(
      <MilestoneChecklist
        run={run({ milestones, milestones_completed: ["m1"], milestones_in_progress: ["m2"] })}
        activity={anActivity()}
      />,
    );
    expect(container.querySelector(".animate-pulse")).not.toBeNull();
  });

  // D5: the run-view now line exists ONLY for milestone runs. A run with no frozen
  // milestone list renders NOTHING even when activity is present — byte-identical to a
  // pre-#1064 non-milestone run.
  it("D5: renders nothing for a non-milestone run even when activity is present", () => {
    const { container } = render(<MilestoneChecklist run={run({ milestones: null })} activity={anActivity()} />);
    expect(container.textContent).toBe("");
  });
});
