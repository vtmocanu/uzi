// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MilestoneChecklist } from "./RunView";
import type { MilestoneAgent, MilestoneLane, Run, RunActivity } from "../lib/api";

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
    harness: "claude", // PRD #1429 M1: harness joined RunDTO (NOT NULL, default claude).
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
    recovery_wait_cause: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    forge_park_max: 0,
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

// One LIVE lane (PRD #1353): a subagent working an in-progress milestone right now. `at` defaults
// to AT so its client-side age token is the same deterministic "40s ago" the activity fixtures use.
function lane(over: Partial<MilestoneLane> = {}): MilestoneLane {
  return {
    agent: "reviewer",
    agent_instance: "inst-1",
    agent_label: "Reviewing the diff",
    tool: "Read",
    detail: "web/src/pages/RunView.tsx",
    at: AT,
    ...over,
  };
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

  // PRD #1353 M1 (honesty fix): the pulsing green dot is reserved for a strip backed by a
  // REAL live frame. Two in-progress milestones attributed to distinct agents (m2→coder,
  // m3→tester) with activity.agent = "coder": m2 is the unique live match (live !== null),
  // m3 is an IDLE declared owner (live === null). Only the live strip may read as "working
  // now" — pre-fix BOTH strips pulsed (every attributed strip renders variant="active"),
  // which is the dishonesty this pin catches.
  it("pulses the dot only on the live strip, not the idle declared owner", () => {
    const { container } = render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: ["m1"],
          milestones_in_progress: ["m2", "m3"],
          milestones_agents: [agent("m2", "coder", "Wire the limiter"), agent("m3", "tester", "Add coverage")],
        })}
        // activity matches EXACTLY m2's coder; m3's tester is an idle declared owner.
        activity={anActivity({ agent: "coder" })}
      />,
    );

    // Exactly ONE pulsing dot across the whole checklist — today (pre-fix) there are TWO.
    expect(container.querySelectorAll(".animate-pulse")).toHaveLength(1);

    const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;
    const gammaRow = screen.getByText("Gamma").closest("li") as HTMLElement;
    const betaDot = betaRow.querySelector("span.rounded-full") as HTMLElement;
    const gammaDot = gammaRow.querySelector("span.rounded-full") as HTMLElement;

    // The live strip (coder, unique match) pulses; the idle declared owner (tester) does not.
    expect(betaDot.classList.contains("animate-pulse")).toBe(true);
    expect(gammaDot.classList.contains("animate-pulse")).toBe(false);
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

describe("PRD #1353 M4: per-milestone LIVE LANES (web)", () => {
  // IDLE owner + live lanes: an in-progress milestone whose declared owner (coder) is NOT among
  // its lanes (reviewer + tester) — the "assigned to X, but Y/Z are working it" case. Both
  // lanes render as distinct live lines with their tool + age token; the declared owner renders as
  // a QUIET "assigned to" line (its role + label, no age token) because it is not itself a lane.
  it("renders a live line per lane plus a quiet declared owner", () => {
    render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: ["m1"],
          milestones_in_progress: ["m2"],
          milestones_agents: [agent("m2", "coder", "Owner label")],
          milestones_live: [
            {
              milestone_id: "m2",
              lanes: [
                lane({ agent: "reviewer", agent_instance: "rev-1", agent_label: "Reviewing", tool: "Read", detail: "a.ts" }),
                lane({ agent: "tester", agent_instance: "test-1", agent_label: "Testing", tool: "Bash", detail: "go test" }),
              ],
            },
          ],
        })}
        // activity is IGNORED in the live-lanes path \u2014 lanes supersede the current_activity line.
        activity={anActivity({ agent: "coder" })}
      />,
    );

    const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;

    // The declared owner renders QUIET: role + label, no live tool/age.
    expect(within(betaRow).getByText("coder")).toBeTruthy();
    expect(within(betaRow).getByText("Owner label")).toBeTruthy();

    // Both lanes render as live lines with their own tool + age token.
    expect(within(betaRow).getByText("reviewer")).toBeTruthy();
    expect(within(betaRow).getByText("tester")).toBeTruthy();
    expect(within(betaRow).getByText("Read a.ts")).toBeTruthy();
    expect(within(betaRow).getByText("Bash go test")).toBeTruthy();

    // Exactly two live age tokens \u2014 one per lane; the quiet owner shows none.
    expect(within(betaRow).getAllByText("40s ago")).toHaveLength(2);
  });

  // common-case dedup (tui-ux): the declared owner is ALSO one of the live lanes \u2014 the dispatched
  // owner (coder) is the one actually working. Pre-fix the quiet "assigned to" owner strip
  // DUPLICATED the owner's live lane strip (same role + label, once quiet + once live). The owner
  // must now appear ONCE, as its live lane line; the quiet owner strip is suppressed.
  it("shows the owner once (its live lane) when the owner is also a lane", () => {
    render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: ["m1"],
          milestones_in_progress: ["m2"],
          milestones_agents: [agent("m2", "coder", "Owner label")],
          milestones_live: [
            {
              milestone_id: "m2",
              lanes: [
                lane({ agent: "coder", agent_instance: "coder-1", agent_label: "Coding", tool: "Edit", detail: "x.ts" }),
                lane({ agent: "reviewer", agent_instance: "rev-1", agent_label: "Reviewing", tool: "Read", detail: "a.ts" }),
              ],
            },
          ],
        })}
        // activity is IGNORED in the live-lanes path.
        activity={anActivity({ agent: "coder" })}
      />,
    );

    const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;

    // The owner (coder) is drawn ONCE \u2014 only its live lane line, not a separate quiet owner strip.
    expect(within(betaRow).getAllByText("coder")).toHaveLength(1);
    // The declared owner's own label never renders (its quiet strip is suppressed); the coder lane
    // carries its OWN lane label instead.
    expect(within(betaRow).queryByText("Owner label")).toBeNull();
    expect(within(betaRow).getByText("Coding")).toBeTruthy();
    // Both lanes render as live lines with their own tool.
    expect(within(betaRow).getByText("reviewer")).toBeTruthy();
    expect(within(betaRow).getByText("Edit x.ts")).toBeTruthy();
    expect(within(betaRow).getByText("Read a.ts")).toBeTruthy();

    // Two strip dots (the two lanes), BOTH pulsing \u2014 no third quiet owner dot.
    const dots = betaRow.querySelectorAll("span.rounded-full");
    expect(dots).toHaveLength(2);
    expect(dots[0].classList.contains("animate-pulse")).toBe(true);
    expect(dots[1].classList.contains("animate-pulse")).toBe(true);
    // Exactly two live age tokens (one per lane); the suppressed owner shows none.
    expect(within(betaRow).getAllByText("40s ago")).toHaveLength(2);
  });

  // repeated role by instance: two lanes both agent:"reviewer" with distinct agent_instance and
  // distinct tools \u2192 BOTH render (keyed by instance), never collapsed to one line.
  it("renders both lanes when a role repeats across distinct instances", () => {
    render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: ["m1"],
          milestones_in_progress: ["m2"],
          // No declared owner here, so only the two lanes render.
          milestones_live: [
            {
              milestone_id: "m2",
              lanes: [
                lane({ agent: "reviewer", agent_instance: "rev-1", agent_label: "First reviewer", tool: "Read", detail: "a.ts" }),
                lane({ agent: "reviewer", agent_instance: "rev-2", agent_label: "Second reviewer", tool: "Grep", detail: "b.ts" }),
              ],
            },
          ],
        })}
        activity={anActivity({ agent: "coder" })}
      />,
    );

    const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;
    // Two lines for the repeated role \u2014 distinct instances are NOT deduped.
    expect(within(betaRow).getAllByText("reviewer")).toHaveLength(2);
    expect(within(betaRow).getByText("First reviewer")).toBeTruthy();
    expect(within(betaRow).getByText("Second reviewer")).toBeTruthy();
    expect(within(betaRow).getByText("Read a.ts")).toBeTruthy();
    expect(within(betaRow).getByText("Grep b.ts")).toBeTruthy();
    expect(within(betaRow).getAllByText("40s ago")).toHaveLength(2);
  });

  // quiet owner + live lanes: the owner strip has NO pulsing dot (live === null \u2192 M1 quiet); each
  // lane strip DOES (live !== null \u2192 green + pulsing). The dots render owner-first, then each lane.
  it("pulses the dot on each lane but not on the quiet declared owner", () => {
    render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: ["m1"],
          milestones_in_progress: ["m2"],
          milestones_agents: [agent("m2", "coder", "Owner label")],
          milestones_live: [
            {
              milestone_id: "m2",
              lanes: [
                lane({ agent: "reviewer", agent_instance: "rev-1" }),
                lane({ agent: "tester", agent_instance: "test-1" }),
              ],
            },
          ],
        })}
        activity={anActivity({ agent: "coder" })}
      />,
    );

    const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;
    // owner + 2 lanes = 3 strip dots, in render order.
    const dots = betaRow.querySelectorAll("span.rounded-full");
    expect(dots).toHaveLength(3);
    // The declared owner (first) is quiet; each lane pulses.
    expect(dots[0].classList.contains("animate-pulse")).toBe(false);
    expect(dots[1].classList.contains("animate-pulse")).toBe(true);
    expect(dots[2].classList.contains("animate-pulse")).toBe(true);
    // And exactly two pulsing dots across the whole checklist (the two lanes).
    expect(betaRow.querySelectorAll(".animate-pulse")).toHaveLength(2);
  });

  // back-compat (D5): null / [] / present-but-all-empty-lanes ALL fall through to the pre-#1353
  // render \u2014 here the #1224 attributed path, whose unique-match behaviour is unchanged.
  describe("no live lanes \u2192 today's render (back-compat)", () => {
    const cases: Array<{ name: string; live: Run["milestones_live"] }> = [
      { name: "milestones_live: null", live: null },
      { name: "milestones_live: [] (empty)", live: [] },
      { name: "present but lanes: [] (no live lane)", live: [{ milestone_id: "m2", lanes: [] }] },
    ];

    for (const { name, live } of cases) {
      it(name, () => {
        render(
          <MilestoneChecklist
            run={run({
              milestones,
              milestones_completed: ["m1"],
              milestones_in_progress: ["m2", "m3"],
              milestones_agents: [agent("m2", "coder", "Wire the limiter"), agent("m3", "tester", "Add coverage")],
              milestones_live: live,
            })}
            activity={anActivity({ agent: "coder" })}
          />,
        );

        const betaRow = screen.getByText("Beta").closest("li") as HTMLElement;
        const gammaRow = screen.getByText("Gamma").closest("li") as HTMLElement;

        // Identical to the #1224 unique-match render: m2 (coder) enriched, m3 (tester) declared-only.
        expect(within(betaRow).getByText("coder")).toBeTruthy();
        expect(within(betaRow).getByText("Edit api/internal/limits/window.go")).toBeTruthy();
        expect(within(betaRow).getByText("40s ago")).toBeTruthy();
        expect(within(gammaRow).getByText("tester")).toBeTruthy();
        expect(within(gammaRow).queryByText("40s ago")).toBeNull();
        expect(screen.getAllByText("40s ago")).toHaveLength(1);
      });
    }
  });

  // sanitization: hostile agent / agent_label / tool on a lane (RIGHT-TO-LEFT OVERRIDE U+202E +
  // zero-width space U+200B, written as escapes) are scrubbed through stripUnsafeChars at render.
  it("strips control/bidi characters from hostile lane fields", () => {
    const { container } = render(
      <MilestoneChecklist
        run={run({
          milestones,
          milestones_completed: [],
          milestones_in_progress: ["m2"],
          milestones_live: [
            {
              milestone_id: "m2",
              lanes: [
                lane({
                  agent: "rev\u202Eiewer",
                  agent_instance: "rev-1",
                  agent_label: "safe\u202Ela\u200Bbel",
                  tool: "Re\u200Bad",
                  detail: "a.ts",
                }),
              ],
            },
          ],
        })}
        activity={anActivity({ agent: "coder" })}
      />,
    );

    // Role, label and tool all survive with the dangerous characters removed.
    expect(screen.getByText("reviewer")).toBeTruthy();
    expect(screen.getByText("safelabel")).toBeTruthy();
    expect(screen.getByText("Read a.ts")).toBeTruthy();
    expect(container.textContent).not.toContain("\u202E");
    expect(container.textContent).not.toContain("\u200B");
  });
});
