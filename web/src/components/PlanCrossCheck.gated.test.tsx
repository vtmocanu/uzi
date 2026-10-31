// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { PlanCrossCheckRound, PlanCrossCheckSummary, Run } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";
import { mockRuns } from "../mocks/data/runs";
import { PlanCrossCheck } from "./PlanCrossCheck";

afterEach(cleanup);

const awaiting = mockRuns.find((r) => r.id === "run-awaiting")!;
function run(over: Partial<Run> = {}): Run {
  return { ...awaiting, plan_cross_check_required: false, plan_cross_check_gated: true,
    plan_cross_check_gate_reason: null, plan_cross_check_summary: undefined, ...over };
}
function summary(over: Partial<PlanCrossCheckSummary> = {}): PlanCrossCheckSummary {
  return { round: 1, verdict: "approve", reason_class: "approve", findings: null, checker_run_id: null,
    checker_model: null, checker_effort: null, checker_model_source: null, checker_effort_source: null,
    usage: null, historical: false, ...over };
}
function round(n: number, verdict: string, over: Partial<PlanCrossCheckRound> = {}): PlanCrossCheckRound {
  return { round: n, verdict, reason_class: verdict === "pending" ? null : verdict, findings: null,
    decided_at: null, response: { state: "no_following_candidate", text: "" }, ...over };
}
function view(r: Run) {
  const out = render(<MemoryRouter><PlanCrossCheck run={r} /></MemoryRouter>);
  const region = out.getByRole("region", { name: "Plan cross-check" });
  return { region, badge: region.querySelector("header")!, rounds: () => within(region).queryByRole("list", { name: "Plan cross-check rounds" }) };
}

describe("gated mode line and guards", () => {
  it("renders a gated run with no evidence yet instead of nothing", () => {
    const { region } = view(run({ status: "running", plan_md: null }));
    expect(within(region).getByText("Mode: checked before your review")).toBeTruthy();
    expect(within(region.querySelector("header")!).getByText("Pending")).toBeTruthy();
    expect(within(region).getByText("Runs when the plan is ready, before your review.")).toBeTruthy();
  });
  it("renders nothing when an older server omits every new field", () => {
    const control = render(<MemoryRouter><PlanCrossCheck run={run({ status: "running", plan_md: null })} /></MemoryRouter>);
    expect(control.container.textContent).toContain("Mode: checked before your review");
    cleanup();
    const legacy: Run = { ...awaiting, plan_cross_check_required: false, plan_cross_check_gate_reason: undefined, plan_cross_check_summary: undefined };
    delete legacy.plan_cross_check_gated;
    const r = render(<MemoryRouter><PlanCrossCheck run={legacy} /></MemoryRouter>);
    expect(r.container.innerHTML).toBe("");
  });
  it("adds no rounds, deadline or mode to a required run from an older server", () => {
    const s = summary({ verdict: "pending", reason_class: null, findings: { summary: "Latest only", items: null } });
    const legacy = view({ ...awaiting, plan_cross_check_required: true, plan_cross_check_gate_reason: null, plan_cross_check_summary: s });
    expect(within(legacy.region).getByText("No result recorded yet.")).toBeTruthy();
    expect(within(legacy.region).getByText("Latest only")).toBeTruthy();
    expect(legacy.rounds()).toBeNull();
    expect(legacy.region.textContent).not.toMatch(/Mode:|Check in progress|Lead.s response/);
    cleanup();
    // Positive control: the same summary with the new fields renders each of them.
    const current = view(run({ status: "running", plan_md: null, plan_cross_check_summary: { ...s,
      deadline_at: "2026-10-31T15:20:00Z", rounds: [round(1, "pending")] } }));
    expect(current.rounds()).toBeTruthy();
    expect(current.region.textContent).toMatch(/Mode:.*Check in progress.*Lead.s response/s);
  });
});

describe("the shared passed rule", () => {
  const passed = summary({ round: 2, rounds: [round(1, "revise"), round(2, "approve")] });
  it("says passed only for an unrevised approval with no gate reason", () => {
    const { badge } = view(run({ plan_cross_check_summary: passed }));
    expect(within(badge).getByText("Passed after 2 rounds")).toBeTruthy();
    cleanup();
    const one = view(run({ plan_cross_check_summary: summary({ rounds: [round(1, "approve")] }) }));
    expect(within(one.badge).getByText("Passed after 1 round")).toBeTruthy();
  });
  it("pass then revise: the approval checked an earlier plan", () => {
    const { region, badge } = view(run({ plan_cross_check_summary: { ...passed, historical: true } }));
    expect(within(badge).getByText("Checked an earlier version of this plan")).toBeTruthy();
    expect(region.textContent).not.toContain("Passed after");
    expect(within(region).getByText("These findings concern an earlier plan; they do not certify the current plan.")).toBeTruthy();
  });
  it("block then revise: never passed, and the block is earlier-plan evidence", () => {
    const blocked = summary({ verdict: "block", reason_class: "block", rounds: [round(1, "block")] });
    const atGate = view(run({ plan_cross_check_gate_reason: "block", plan_cross_check_summary: blocked }));
    expect(within(atGate.badge).getByText("Blocked")).toBeTruthy();
    cleanup();
    const revised = view(run({ plan_cross_check_summary: { ...blocked, historical: true } }));
    expect(within(revised.badge).getByText("Checked an earlier version of this plan")).toBeTruthy();
    expect(revised.region.textContent).not.toContain("Passed after");
  });
  it("worker_unsupported then revise: unavailable, then not checked", () => {
    const unsupported = view(run({ plan_cross_check_gate_reason: "worker_unsupported" }));
    expect(within(unsupported.badge).getByText("Unavailable, not supported on this worker")).toBeTruthy();
    expect(within(unsupported.region).getByText("Current gate: Not supported on this worker")).toBeTruthy();
    cleanup();
    const revised = view(run({ plan_cross_check_gate_reason: null }));
    expect(within(revised.badge).getByText("Not checked")).toBeTruthy();
    expect(within(revised.region).getByText("No check result for this plan.")).toBeTruthy();
    expect(revised.region.textContent).not.toContain("Passed after");
  });
  it("an approval with a gate reason, or a non-approving latest round, is not passed", () => {
    const late = view(run({ plan_cross_check_gate_reason: "timed_out", plan_cross_check_summary: passed }));
    expect(within(late.badge).getByText("Timed out")).toBeTruthy();
    expect(late.region.textContent).not.toContain("Passed after");
    cleanup();
    const skew = view(run({ plan_cross_check_summary: { ...passed, rounds: [round(1, "approve"), round(2, "revise")] } }));
    expect(within(skew.badge).getByText("Passed")).toBeTruthy();
    expect(skew.region.textContent).not.toContain("Passed after");
  });
  it.each([
    ["timed_out", "Timed out"], ["model_timeout", "Timed out"], ["revisions_exhausted", "Revisions exhausted"],
    ["interrupted", "Interrupted"], ["checker_unavailable", "Unavailable"], ["model_error", "Unavailable"],
    ["future_reason", "Unavailable"],
  ])("labels gate reason %s as %s", (reason, label) => {
    const { badge } = view(run({ plan_cross_check_gate_reason: reason }));
    expect(within(badge).getByText(label)).toBeTruthy();
  });
});

describe("each round's response", () => {
  const states: [string, string, boolean][] = [
    ["reported", "Reported", true],
    ["truncated", "Reported, truncated", true],
    ["not_reported", "Not reported", false],
    ["unknown", "Unknown", false],
    ["replanned_after_interruption", "Replanned after an interruption", false],
    ["reported_in_presented_plan", "From the presented plan; not checked", true],
    ["no_following_candidate", "No following plan", false],
    ["future_state", "Response unavailable", false],
  ];
  it.each(states)("renders state %s fenced as unverified claims", (state, label, showsText) => {
    const s = summary({ round: 1, verdict: "revise", reason_class: "revise", rounds: [round(1, "revise", {
      findings: { summary: "Checker said this", items: null }, response: { state, text: "Lead claims **fixed**" } })] });
    const { region, rounds } = view(run({ plan_cross_check_gate_reason: "revise", plan_cross_check_summary: s }));
    const item = within(rounds()!).getByRole("listitem", { name: "Round 1: Changes requested" });
    const fences = [...item.querySelectorAll(":scope > div.rounded-md")];
    expect(fences).toHaveLength(2);
    expect(fences[0].textContent).toContain("Checker findings (untrusted, may be wrong)");
    expect(fences[0].textContent).toContain("Checker said this");
    expect(fences[1].textContent).toContain("Lead’s response (unverified claims)");
    expect(within(fences[1] as HTMLElement).getByText(label)).toBeTruthy();
    // The text renders only for a state that carries one, and only inside the response fence.
    expect(fences[1].textContent!.includes("Lead claims fixed")).toBe(showsText);
    expect(fences[0].textContent).not.toContain("Lead claims");
    expect(region.textContent).not.toMatch(/resolved|verified fix/i);
  });
  it("strips control and format characters from response text", () => {
    const s = summary({ verdict: "revise", reason_class: "revise", rounds: [round(1, "revise", {
      response: { state: "reported", text: "ok‮\u0007 <script>alert(1)</script>" } })] });
    const { rounds } = view(run({ plan_cross_check_gate_reason: "revise", plan_cross_check_summary: s }));
    expect(rounds()!.textContent).toContain("ok");
    expect(rounds()!.textContent).not.toMatch(/\p{Cf}/u);
    expect(rounds()!.querySelector("script")).toBeNull();
  });
});

describe("three-round gated mock", () => {
  it("shows every round with only its own findings and response", () => {
    const gated = mockRuns.find((r) => r.id === "run-gated-checked")!;
    const { badge, rounds } = view(gated);
    expect(within(badge).getByText("Passed after 3 rounds")).toBeTruthy();
    const items = within(rounds()!).getAllByRole("listitem").filter((li) => li.parentElement === rounds());
    expect(items.map((li) => li.getAttribute("aria-label"))).toEqual(["Round 1: Changes requested", "Round 2: Changes requested", "Round 3: Passed"]);
    const own = [
      { finding: "Delivery is not idempotent.", response: "records a sent marker per run" },
      { finding: "The dedupe key ignores the gate revision.", response: "keyed on (run, gate revision)" },
    ];
    own.forEach((mine, i) => {
      expect(items[i].textContent).toContain(mine.finding);
      expect(items[i].textContent).toContain(mine.response);
      for (const [j, other] of own.entries()) {
        if (j === i) continue;
        expect(items[i].textContent).not.toContain(other.finding);
        expect(items[i].textContent).not.toContain(other.response);
      }
    });
    expect(within(items[2]).getByText("No following plan")).toBeTruthy();
    for (const o of own) expect(items[2].textContent).not.toContain(o.response);
  });
});

describe("in-progress gated mock", () => {
  it("shows the check in progress with its deadline", () => {
    const checking = mockRuns.find((r) => r.id === "run-gated-checking")!;
    const deadline = checking.plan_cross_check_summary!.deadline_at!;
    const { region, badge } = view(checking);
    expect(within(badge).getByText("Pending")).toBeTruthy();
    const line = within(region).getByText((_, el) => el?.tagName === "P" && el.textContent === `Check in progress, deadline ${formatLocalTime(deadline)}`);
    expect(line.querySelector("time")?.getAttribute("datetime")).toBe(deadline);
    expect(region.textContent).not.toContain("No result recorded yet.");
  });
  it("falls back to no-result copy without a deadline", () => {
    const checking = mockRuns.find((r) => r.id === "run-gated-checking")!;
    const { region } = view({ ...checking, plan_cross_check_summary: { ...checking.plan_cross_check_summary!, deadline_at: undefined } });
    expect(within(region).getByText("No result recorded yet.")).toBeTruthy();
    expect(region.textContent).not.toContain("Check in progress");
  });
});
