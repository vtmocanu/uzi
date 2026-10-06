// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { PlanCrossCheckSummary, Run } from "../lib/apiTypes";
import { mockRuns } from "../mocks/data/runs";
import { PlanCrossCheck, PlanCrossCheckEvent } from "./PlanCrossCheck";
import { RunEventRow } from "./RunEvent";
import { PlanPanel } from "../pages/runView/PlanPanel";

vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, listSecrets: vi.fn().mockResolvedValue({ secrets: [] }) } };
});
afterEach(cleanup);
const child = "11111111-1111-4111-8111-111111111111";
function summary(over: Partial<PlanCrossCheckSummary> = {}): PlanCrossCheckSummary {
  return { round: 1, verdict: "approve", reason_class: "approve", findings: null,
    checker_run_id: null, checker_model: null, checker_effort: null, usage: null, historical: false, ...over };
}
function run(over: Partial<Run> = {}): Run { return { ...mockRuns[0], ...over }; }
const cases: [unknown, unknown, string][] = [
  ["pending", null, "Pending"], ["approve", "approve", "Passed"],
  ["revise", "revise", "Changes requested"], ["block", "block", "Blocked"],
  ...Object.entries({
    timed_out: "Timed out (verdict deadline)", model_timeout: "Timed out (checker model timeout)", checker_unavailable: "Checker unavailable",
    interrupted: "Interrupted", malformed: "Malformed checker response", model_error: "Checker model error",
    confinement_failed: "Checker confinement failed", superseded: "Superseded", checker_failed: "Checker failed",
    codex_lead_unsupported: "Not yet supported for a Codex lead",
    planning_diff_refused: "Planning diff refused", candidate_refused: "Candidate refused",
  }).map(([reason, label]): [unknown, unknown, string] => ["failed", reason, label]),
  ["approve", null, "Outcome unavailable"], ["approve", "", "Outcome unavailable"],
  ["approve", "block", "Outcome unavailable"], ["failed", "future", "Outcome unavailable"],
  [null, null, "Outcome unavailable"], ["", "", "Outcome unavailable"],
  ["failed", "__proto__", "Outcome unavailable"], ["failed", "toString", "Outcome unavailable"],
  ["revise", "approve", "Outcome unavailable"], ["block", null, "Outcome unavailable"],
];
describe("M1 plan cross-check outcomes at real sinks", () => {
  it.each(cases)("%s / %s", (verdict, reason_class, label) => {
    const s = summary({ verdict: verdict as string, reason_class: reason_class as string | null });
    const r = render(<MemoryRouter>
      <PlanCrossCheck run={run({ plan_cross_check_summary: s })} />
      <RunEventRow msg={{ seq: 1, kind: "cross_check", agent: null, agent_instance: null, agent_label: null,
        created_at: "", payload: { stage: "plan", verdict, reason_class } }} live={false} />
    </MemoryRouter>);
    expect(within(r.getByRole("region", { name: "Plan cross-check" })).getByText(`Checked candidate: ${label}`)).toBeTruthy();
    expect(r.getByText(`Plan cross-check of checked candidate: ${label}`)).toBeTruthy();
  });
  it.each(cases.filter(([verdict]) => verdict === "failed").filter(([, reason]) => reason !== "future" && reason !== "__proto__" && reason !== "toString"))("preserves gate reason %s / %s without summary", (_verdict, reason, label) => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_gate_reason: reason as string })} />);
    expect(r.getByText(`Current gate: ${label}`)).toBeTruthy();
    expect(r.getByText("Outcome unavailable")).toBeTruthy();
  });
  it("renders accessible parked and historical mock findings", () => {
    const parked = mockRuns.find((r) => r.id === "run-awaiting")!;
    const historical = mockRuns.find((r) => r.plan_cross_check_summary?.historical)!;
    const r = render(<MemoryRouter><PlanPanel run={parked} busy={false} canSteer={false} onApprove={vi.fn()} onReject={vi.fn()} />
      <PlanCrossCheck run={historical} /></MemoryRouter>);
    expect(r.getByRole("heading", { name: "Checker findings" })).toBeTruthy();
    expect(r.getByRole("heading", { name: "Earlier-plan checker findings" })).toBeTruthy();
    expect(r.getByText("Earlier-plan evidence: Passed")).toBeTruthy();
  });
  it("retains gate reasons with no summary and keeps historical evidence separate", () => {
    const s = summary({ historical: true, checker_run_id: child, findings: { summary: "# Earlier evidence", items: null } });
    const r = render(<MemoryRouter><PlanPanel run={run({ status: "awaiting_approval", plan_cross_check_summary: s,
      plan_cross_check_gate_reason: "candidate_refused" })} busy={false} canSteer={false} onApprove={vi.fn()} onReject={vi.fn()} />
      <PlanCrossCheckEvent payload={{ verdict: "approve", reason_class: "approve", checker_run_id: child }} detail={s} />
    </MemoryRouter>);
    expect(r.getByText("Current gate: Candidate refused")).toBeTruthy();
    expect(r.getByRole("heading", { name: "Earlier evidence" })).toBeTruthy();
    expect(r.getByText("Earlier-plan evidence: Passed")).toBeTruthy();
    expect(r.getByText("Plan cross-check of earlier-plan candidate: Passed")).toBeTruthy();
    cleanup();
    // The checker run was deleted: the summary lost its id, the event kept it.
    const orphaned = render(<MemoryRouter><PlanCrossCheckEvent payload={{ verdict: "approve", reason_class: "approve", checker_run_id: child }}
      detail={summary({ historical: true, checker_run_id: null })} /></MemoryRouter>);
    expect(orphaned.getByText("Plan cross-check of earlier-plan candidate: Passed")).toBeTruthy();
    cleanup();
    const absent = render(<PlanCrossCheck run={run({ plan_cross_check_gate_reason: "checker_unavailable" })} />);
    expect(absent.getByText("Current gate: Checker unavailable")).toBeTruthy();
    expect(absent.getByText("Outcome unavailable")).toBeTruthy();
  });
  it.each(["metered", "subscription", "unreported", "", "future"])("honest cost %s", (status) => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({
      usage: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, cost_usd: 0,
        cost_status: status as "metered" },
    }) })} />);
    expect(r.container.textContent).toContain(status === "metered" ? "Checker cost: $0.00" : status === "subscription" ? "subscription usage" : "cost unavailable");
    if (status !== "metered") expect(r.container.textContent).not.toContain("$0");
  });
  it("shows the reported metered amount, not a zero default", () => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({
      usage: { input_tokens: 10, output_tokens: 5, cache_read_tokens: 0, cache_creation_tokens: 0, cost_usd: 1.25,
        cost_status: "metered" },
    }) })} />);
    expect(r.container.textContent).toContain("Checker cost: $1.25");
  });
  it.each(["panel", "detail", "event"])("hardens and bounds %s findings and attributes", (sink) => {
    const findings = { summary: '# Safe\u202e\u0007 heading\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))\n\n[good](https://example.com "review &#8238;denied&#8236;")\n\n&#x202e;plain',
      items: Array.from({ length: 40 }, () => ({ file: "", severity: "", summary: "x".repeat(20000), rationale: "tail omitted" })) };
    const s = summary({ findings, checker_run_id: "../unsafe", checker_model: "<img>\u202e", checker_effort: "high\u0007" });
    const r = render(<MemoryRouter>{sink === "event" ? <PlanCrossCheckEvent payload={s} /> :
      sink === "panel" ? <PlanPanel run={run({ plan_cross_check_summary: s })} busy={false} canSteer={false} onApprove={vi.fn()} onReject={vi.fn()} /> :
      <PlanCrossCheck run={run({ plan_cross_check_summary: s })} />}</MemoryRouter>);
    const region = r.getByRole("region", { name: sink === "event" ? "Plan cross-check event" : "Plan cross-check" });
    expect(region.textContent).not.toMatch(/\p{Cf}/u);
    expect(region.textContent).not.toContain(String.fromCharCode(7));
    expect(region.querySelector("script, img, [onerror]")).toBeNull();
    expect(within(region).getByText("bad").getAttribute("href")).toBeNull();
    expect(within(region).getByText("good").getAttribute("title")).toBe("review denied");
    expect(within(region).queryByText("Checker run")).toBeNull();
    expect(region.textContent!.length).toBeLessThan(17500);
    expect(within(region).getByText("Findings truncated for display.")).toBeTruthy();
  });
  it.each(["../unsafe", "/runs/123", "https://example.com", "", "11111111-1111-4111-8111-111111111111\u202e"])("refuses child ID %s", (id) => {
    const r = render(<MemoryRouter><PlanCrossCheck run={run({ plan_cross_check_summary: summary({ checker_run_id: id }) })} />
      <PlanCrossCheckEvent payload={{ checker_run_id: id }} /></MemoryRouter>);
    expect(r.queryAllByRole("link")).toHaveLength(0);
  });
  it("renders checker evidence while the plan is being revised", () => {
    const r = render(<MemoryRouter><PlanPanel run={run({ plan_cross_check_summary: summary({ verdict: "pending", reason_class: null }) })}
      messages={[{ seq: 1, kind: "plan_revising", payload: {}, agent: null, agent_instance: null, agent_label: null, created_at: "" }]}
      busy={false} canSteer={false} onApprove={vi.fn()} onReject={vi.fn()} /></MemoryRouter>);
    expect(r.getByRole("heading", { name: /Revising the plan/ })).toBeTruthy();
    expect(r.getByText("Checked candidate: Pending")).toBeTruthy();
  });
  it("links only validated checker IDs and accepts null findings", () => {
    const r = render(<MemoryRouter><PlanCrossCheck run={run({ plan_cross_check_summary: summary({ checker_run_id: child }) })} /></MemoryRouter>);
    expect(r.getByRole("link", { name: "Checker run" }).getAttribute("href")).toBe(`/runs/${child}`);
    expect(r.getByText(/Checker cost: Unavailable/)).toBeTruthy();
  });
});
