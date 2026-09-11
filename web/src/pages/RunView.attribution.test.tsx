// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MilestoneChecklist } from "./RunView";
import type { MilestoneAgent, Run, RunActivity } from "../lib/api";

// PRD #1224 M5 (web render): per-milestone agent attribution in MilestoneChecklist. These
// tests pin the D8 "effective attribution" trigger, the D3 live-enrichment join (unique
// match only), the D8 fallback back-compat, and the untrusted agent_label fold. They render
// MilestoneChecklist directly (RunView needs routing + a live stream to mount) and pin useNow
// so the client-side age token is deterministic, mirroring RunView.nowline.test.tsx.

const AT = "2026-09-03T12:00:00Z";
const NOW = Date.parse(AT) + 40_000; // 40s after the activity instant → "40s"

// MilestoneChecklist ages the now line via useNow; pin it so "40s" is deterministic.
vi.mock("../lib/useNow", () => ({ useNow: () => NOW }));

function anActivity(over: Partial<RunActivity> = {}): RunActivity {
  return {
    agent: "coder",
    agent_label: "live task label",
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
  { id: "m1", title: "Alpha" },
  { id: "m2", title: "Beta" },
  { id: "m3", title: "Gamma" },
];

function agent(id: string, role: string, label: string): MilestoneAgent {
  return { id, agent: role, agent_label: label };
}

afterEach(cleanup);

describe("PRD #1224 M5: per-milestone agent attribution (web)", () => {
  // Pins D8 (a non-empty effective list activates per-milestone rendering) + D3 (live
  // tool/age is added only to the milestone whose declared agent uniquely matches activity;
  // the sibling shows its DECLARED role + label only).
  it("shows each declared role+label, and live tool/age only on the unique activity match", () => {
    render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: ["m1"],
          milestones_in_progress: ["m2", "m3"],
          milestones_agents: [agent("m2", "coder", "Wire the limiter"), agent("m3", "tester", "Add coverage")],
        })}
        // activity.agent matches EXACTLY one effective attribution (m2's coder).
        activity={anActivity({ agent: "coder" })}
      />,
    );

    const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;
    const gammaRow = screen.getByText("Gamma").closest("li") as HTMLElement;

    // m2 (the unique match): declared role + label AND live tool/age.
    expect(within(betaRow).getByText("coder")).toBeTruthy();
    expect(within(betaRow).getByText("Wire the limiter")).toBeTruthy();
    expect(within(betaRow).getByText("Edit api/internal/limits/window.go")).toBeTruthy();
    expect(within(betaRow).getByText("40s ago")).toBeTruthy();

    // m3 (the sibling): declared role + label ONLY — no live tool, no age.
    expect(within(gammaRow).getByText("tester")).toBeTruthy();
    expect(within(gammaRow).getByText("Add coverage")).toBeTruthy();
    expect(within(gammaRow).queryByText("Edit api/internal/limits/window.go")).toBeNull();
    expect(within(gammaRow).queryByText("40s ago")).toBeNull();

    // Exactly one live age token across the whole checklist.
    expect(screen.getAllByText("40s ago")).toHaveLength(1);
  });

  // Pins D3's repeated-role rule: two effective attributions share a role that also equals
  // activity.agent → the match is ambiguous, so live tool/age is SUPPRESSED on BOTH; every
  // strip shows declared role + label only.
  it("suppresses live tool/age on BOTH strips when the matching role is repeated", () => {
    render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: ["m1"],
          milestones_in_progress: ["m2", "m3"],
          milestones_agents: [agent("m2", "coder", "Front half"), agent("m3", "coder", "Back half")],
        })}
        activity={anActivity({ agent: "coder" })}
      />,
    );

    // Both strips render the repeated role and their own labels…
    expect(screen.getAllByText("coder")).toHaveLength(2);
    expect(screen.getByText("Front half")).toBeTruthy();
    expect(screen.getByText("Back half")).toBeTruthy();

    // …but NO age token appears on either (live join is ambiguous → suppressed on all).
    expect(screen.queryByText("40s ago")).toBeNull();
    expect(screen.queryByText(/ago/)).toBeNull();
    // …and the live tool line is not rendered anywhere.
    expect(screen.queryByText("Edit api/internal/limits/window.go")).toBeNull();
  });

  // Pins D8 fallback back-compat: null / [] / stale-only effective lists ALL fall back to
  // today's render — exactly ONE strip, under the first in-progress milestone, sourced from
  // the activity prop (role/label/live all from `activity`, not from any attribution).
  describe("D8 fallback → today's first-in-progress strip", () => {
    const cases: Array<{ name: string; agents: MilestoneAgent[] | null }> = [
      { name: "milestones_agents: null", agents: null },
      { name: "milestones_agents: [] (empty)", agents: [] },
      // stale-only: an entry whose id (m1) is NOT in the live in-progress set → effective [].
      { name: "stale-only (id not in progress)", agents: [agent("m1", "coder", "stale")] },
    ];

    for (const { name, agents } of cases) {
      it(name, () => {
        render(
          <MilestoneChecklist
            run={run({
              milestones,
              milestones_completed: [],
              milestones_in_progress: ["m2"],
              milestones_agents: agents,
            })}
            activity={anActivity({ agent: "coder", agent_label: "live task label" })}
          />,
        );

        // Exactly one strip, sourced from activity, under the first in-progress row (m2).
        const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;
        expect(within(betaRow).getByText("coder")).toBeTruthy();
        expect(within(betaRow).getByText("live task label")).toBeTruthy();
        expect(within(betaRow).getByText("Edit api/internal/limits/window.go")).toBeTruthy();
        expect(screen.getAllByText("40s ago")).toHaveLength(1);
        // The stale attribution's label never renders (it is not in progress).
        expect(screen.queryByText("stale")).toBeNull();
      });
    }
  });

  // Pins the untrusted-field fold: a hostile agent_label on an EFFECTIVE entry (RIGHT-TO-LEFT
  // OVERRIDE U+202E + zero-width space U+200B, written as escapes) is scrubbed through
  // stripUnsafeChars before render.
  it("strips control/bidi characters from a hostile declared agent_label", () => {
    const { container } = render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: [],
          milestones_in_progress: ["m2"],
          milestones_agents: [agent("m2", "coder", "safe\u202Ela\u200Bbel")],
        })}
        activity={anActivity({ agent: "coder" })}
      />,
    );

    // The dangerous characters are gone; the readable label survives.
    expect(screen.getByText("safelabel")).toBeTruthy();
    expect(container.textContent).not.toContain("\u202E");
    expect(container.textContent).not.toContain("\u200B");
  });
});
