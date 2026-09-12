// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MilestoneChecklist } from "./RunView";
import type { Run, RunActivity } from "../lib/api";

// PRD #1224 M4 — BASELINE CHARACTERIZATION (web).
//
// This locks TODAY's structure of the UNATTRIBUTED multi-in-progress milestone render,
// captured on the pre-renderer-change tree (before M5). It is the concrete, recorded
// baseline for the hard back-compat contract (PRD SC2 / Decision 8): a run with NO
// EFFECTIVE ATTRIBUTION (milestones_agents null/absent) must render EXACTLY as today.
//
// It is a CHARACTERIZATION test, NOT mutation-pinned (that is M7): it must pass on the
// CURRENT tree AND stay green after M5 lands, because M5 preserves the unattributed
// render. Today's behavior it locks in: with two milestones in progress at once, the
// acting agent attaches to only the FIRST in-progress milestone (m2) via a single active
// MilestoneNowStrip; the second in-progress milestone (m3) keeps its bare ◐ mark and
// gets NO strip / NO agent line; and the header shows the `◐ <firstInProgress>` marker.
//
// D8 back-compat baseline: M5 MUST keep this green for the unattributed case.

const AT = "2026-09-03T12:00:00Z";
const NOW = Date.parse(AT) + 40_000; // 40s after the activity instant → "40s"

// MilestoneChecklist ages the now line via useNow; pin it so "40s" is deterministic.
vi.mock("../lib/useNow", () => ({ useNow: () => NOW }));

function anActivity(over: Partial<RunActivity> = {}): RunActivity {
  return {
    agent: "coder",
    agent_label: "Wire the limiter",
    tool: "Edit",
    detail: "api/internal/limits/window.go",
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
    // The unattributed case under test: no per-milestone agent attribution.
    milestones_agents: null,
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

// The canonical scenario: a running ISSUE run with three frozen milestones, m1 done and
// BOTH m2 and m3 in progress at once (the case per-milestone attribution targets), an
// activity present, and milestones_agents null (unattributed).
const milestones = [
  { id: "m1", title: "Alpha" },
  { id: "m2", title: "Beta" },
  { id: "m3", title: "Gamma" },
];

function canonicalRun(): Run {
  return run({
    milestones,
    milestones_completed: ["m1"],
    milestones_in_progress: ["m2", "m3"],
    milestones_agents: null,
  });
}

afterEach(cleanup);

describe("PRD #1224 M4 baseline: unattributed multi-in-progress milestone render (web)", () => {
  it("attaches the single now-strip to the FIRST in-progress milestone (m2) only", () => {
    render(<MilestoneChecklist run={canonicalRun()} activity={anActivity()} />);

    // Exactly ONE MilestoneNowStrip renders (the active variant). Each active strip prints
    // the role once and a single "40s ago" age token, so both counts are the strip count.
    expect(screen.getAllByText("40s ago")).toHaveLength(1);
    expect(screen.getAllByText("coder")).toHaveLength(1);

    // …and it is associated with the FIRST in-progress milestone row (m2 "Beta"): the strip
    // is rendered inside that <li>.
    const betaRow = screen.getByText("Beta").closest("li");
    expect(betaRow).not.toBeNull();
    expect(within(betaRow as HTMLElement).getByText("coder")).toBeTruthy();
    expect(within(betaRow as HTMLElement).getByText("40s ago")).toBeTruthy();
    expect(within(betaRow as HTMLElement).getByText("Edit api/internal/limits/window.go")).toBeTruthy();
  });

  it("gives the SECOND in-progress milestone (m3) a bare ◐ mark and NO strip / NO agent line", () => {
    render(<MilestoneChecklist run={canonicalRun()} activity={anActivity()} />);

    const gammaRow = screen.getByText("Gamma").closest("li");
    expect(gammaRow).not.toBeNull();
    // m3 carries its in-progress mark…
    expect(within(gammaRow as HTMLElement).getByLabelText("in progress")).toBeTruthy();
    // …but no now-strip: no role, no age, no tool line attached to it.
    expect(within(gammaRow as HTMLElement).queryByText("coder")).toBeNull();
    expect(within(gammaRow as HTMLElement).queryByText("40s ago")).toBeNull();

    // Both m2 and m3 render an in-progress mark (D9: the mark still carries "two lanes
    // active"); the single strip does not suppress m3's mark.
    expect(screen.getAllByLabelText("in progress")).toHaveLength(2);
  });

  it("shows the `◐ <firstInProgress>` header marker for m2 (unchanged, non-goal for M5)", () => {
    render(<MilestoneChecklist run={canonicalRun()} activity={anActivity()} />);
    expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
    // Only the first in-progress id is named in the header, never m3.
    expect(screen.queryByText(/◐\s*m3/)).toBeNull();
  });
});
