// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent as domFireEvent, render, within } from "@testing-library/react";
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
// Testing Library's event map does not include the native details toggle event.
const fireEvent = Object.assign(domFireEvent, {
  toggle: (node: Element) => domFireEvent(node, new Event("toggle")),
});
afterEach(cleanup);
const child = "11111111-1111-4111-8111-111111111111";
function summary(over: Partial<PlanCrossCheckSummary> = {}): PlanCrossCheckSummary {
  return { round: 1, verdict: "approve", reason_class: "approve", findings: null,
    checker_run_id: null, checker_model: null, checker_effort: null, checker_model_source: null, checker_effort_source: null, usage: null, historical: false, ...over };
}
function run(over: Partial<Run> = {}): Run { return { ...mockRuns[0], ...over }; }
const cases: [unknown, unknown, string][] = [
  ["pending", null, "Pending"], ["approve", "approve", "Passed"],
  ["revise", "revise", "Changes requested"], ["block", "block", "Blocked"],
  ...Object.entries({
    timed_out: "Timed out (verdict deadline)", model_timeout: "Timed out (checker model timeout)", checker_unavailable: "Checker unavailable",
    interrupted: "Interrupted", malformed: "Malformed checker response", model_error: "Checker model error",
    confinement_failed: "Checker confinement failed", superseded: "Superseded", checker_failed: "Checker failed",
    approved_not_stored: "Approved plan not stored", revisions_exhausted: "Revisions exhausted",
    codex_lead_unsupported: "Not yet supported for a Codex lead",
    planning_diff_refused: "Planning diff refused", candidate_refused: "Candidate refused",
  }).map(([reason, label]): [unknown, unknown, string] => ["failed", reason, label]),
  ["approve", null, "Outcome unavailable"], ["approve", "", "Outcome unavailable"],
  ["approve", "block", "Outcome unavailable"], ["failed", "future", "Outcome unavailable"],
  [null, null, "Outcome unavailable"], ["", "", "Outcome unavailable"],
  ["failed", "__proto__", "Outcome unavailable"], ["failed", "toString", "Outcome unavailable"],
  ["revise", "approve", "Outcome unavailable"], ["block", null, "Outcome unavailable"],
];
describe("plan cross-check headers", () => {
  it.each(["standalone", "inset"] as const)("renders a named %s shell and heading", (presentation) => {
    const r = render(<MemoryRouter><PlanCrossCheck presentation={presentation}
      run={run({ plan_cross_check_summary: summary({ checker_run_id: child }) })} /></MemoryRouter>);
    const region = r.getByRole("region", { name: "Plan cross-check" });
    const heading = within(region).getByRole("heading", { name: "Plan cross-check", level: presentation === "inset" ? 3 : 2 });
    const shell = presentation === "inset" ? region : region.firstElementChild!;
    for (const token of [presentation === "inset" ? "rounded-lg" : "rounded-xl", "border", "border-edge", "bg-surface", presentation === "inset" ? "p-4" : "p-5"]) {
      expect(shell.classList.contains(token)).toBe(true);
    }
    expect(region.classList.contains("rounded-md")).toBe(false);
    expect(region.classList.contains("p-3")).toBe(false);
    if (presentation === "standalone") {
      expect(region.classList.contains("border")).toBe(false);
      expect(region.classList.contains("p-5")).toBe(false);
    }
    const link = within(region).getByRole("link", { name: "Open checker run" });
    expect(link.closest("header")).toBe(heading.closest("header"));
    expect(link.classList.contains("focus-visible:ring-brand/60")).toBe(true);
  });
  it("places the event link in its header without repeating event text", () => {
    const r = render(<MemoryRouter><PlanCrossCheckEvent payload={{ verdict: "approve", reason_class: "approve", checker_run_id: child }} /></MemoryRouter>);
    const region = r.getByRole("region", { name: "Plan cross-check event" });
    const heading = within(region).getByRole("heading", { level: 2, name: "Plan cross-check of checked candidate: Passed" });
    expect(within(region).getAllByText("Plan cross-check of checked candidate: Passed")).toHaveLength(1);
    const link = within(region).getByRole("link", { name: "Open checker run" });
    expect(link.closest("header")).toBe(heading.closest("header"));
    expect(link.getAttribute("href")).toBe(`/runs/${child}`);
    expect(region.firstElementChild?.classList.contains("rounded-xl")).toBe(true);
  });
});

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
    expect(r.container.textContent).toContain(status === "metered" ? "Checker cost: $0.00 · API-equivalent" : status === "subscription" ? "Checker cost: No estimate · no cost estimate recorded" : "cost unavailable");
    if (status !== "metered") expect(r.container.textContent).not.toContain("$0");
  });
  it("shows the reported metered amount, not a zero default", () => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({
      usage: { input_tokens: 10, output_tokens: 5, cache_read_tokens: 0, cache_creation_tokens: 0, cost_usd: 1.25,
        cost_status: "metered" },
    }) })} />);
    expect(r.container.textContent).toContain("Checker cost: $1.25 · API-equivalent");
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
    expect(within(region).queryByText("Open checker run")).toBeNull();
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
    expect(r.getByRole("link", { name: "Open checker run" }).getAttribute("href")).toBe(`/runs/${child}`);
    expect(r.getByText(/Checker cost: Unavailable/)).toBeTruthy();
  });
});

describe("plan cross-check rounds at real sinks", () => {
  it.each([false, true])("compares event rounds with latest summary (historical %s)", (historical) => {
    const detail = summary({ round: 2, historical });
    const r = render(<MemoryRouter>
      {[1, 2, 3].map((round) => <RunEventRow key={round} msg={{
        seq: round, kind: "cross_check", agent: null, agent_instance: null, agent_label: null, created_at: "",
        payload: { stage: "plan", round, verdict: round === 1 ? "revise" : "approve",
          reason_class: round === 1 ? "revise" : "approve" },
      }} live={false} planCheckDetail={detail} />)}
    </MemoryRouter>);
    expect(r.getByText("Plan cross-check (round 1) of earlier-plan candidate: Changes requested")).toBeTruthy();
    expect(r.getByText(`Plan cross-check (round 2) of ${historical ? "earlier-plan" : "checked"} candidate: Passed`)).toBeTruthy();
    expect(r.getByText("Plan cross-check (round 3) of checked candidate: Passed")).toBeTruthy();
  });
  it("compares a legacy missing round as round 1 without adding a label", () => {
    const r = render(<RunEventRow msg={{ seq: 1, kind: "cross_check", agent: null,
      agent_instance: null, agent_label: null, created_at: "",
      payload: { stage: "plan", verdict: "revise", reason_class: "revise" },
    }} live={false} planCheckDetail={summary({ round: 2 })} />);
    expect(r.getByText("Plan cross-check of earlier-plan candidate: Changes requested")).toBeTruthy();
  });
  it("displays the protocol's highest valid round", () => {
    const r = render(<PlanCrossCheckEvent payload={{ round: 5, verdict: "approve", reason_class: "approve" }}
      detail={summary({ round: 5 })} />);
    expect(r.getByText("Plan cross-check (round 5) of checked candidate: Passed")).toBeTruthy();
  });
  it.each([null, 0, -1, 1.5, 6, Number.MAX_SAFE_INTEGER + 1, NaN, Infinity,
    "2", "<img>", "2\u202e", {}, []])("rejects unsafe event round %j", (round) => {
    const r = render(<PlanCrossCheckEvent payload={{ round, verdict: "approve", reason_class: "approve" }}
      detail={summary({ round: 2, historical: true })} />);
    expect(r.getByText("Plan cross-check of checked candidate: Passed")).toBeTruthy();
    expect(r.container.querySelector("img")).toBeNull();
  });
  it.each([0, 6, NaN, "2"])("does not infer history from unsafe summary round %j", (round) => {
    const r = render(<PlanCrossCheckEvent payload={{ round: 1, verdict: "approve", reason_class: "approve" }}
      detail={summary({ round: round as number, historical: true })} />);
    expect(r.getByText("Plan cross-check (round 1) of checked candidate: Passed")).toBeTruthy();
  });
  it.each([
    ["approved_not_stored", "Approved plan not stored"],
    ["revisions_exhausted", "Revisions exhausted"],
  ])("presents %s in PlanPanel and RunEvent", (reason, label) => {
    const detail = summary({ round: 2, verdict: "failed", reason_class: reason });
    const r = render(<MemoryRouter>
      <PlanPanel run={run({ status: "awaiting_approval", plan_cross_check_summary: detail,
        plan_cross_check_gate_reason: reason })} busy={false} canSteer={false}
        onApprove={vi.fn()} onReject={vi.fn()} />
      <RunEventRow msg={{ seq: 1, kind: "cross_check", agent: null, agent_instance: null,
        agent_label: null, created_at: "", payload: { stage: "plan", round: 2,
          verdict: "failed", reason_class: reason } }} live={false} planCheckDetail={detail} />
    </MemoryRouter>);
    expect(r.getByText(`Current gate: ${label}`)).toBeTruthy();
    expect(r.getByText(`Checked candidate: ${label}`)).toBeTruthy();
    expect(r.getByText(`Plan cross-check (round 2) of checked candidate: ${label}`)).toBeTruthy();
  });
});

describe("planning-diff refusal sub-code", () => {
  it.each([
    ["unsupported_entry", "Unsupported entry (symlink, submodule or special file)"],
    ["diff_too_large", "Diff too large"],
    ["secret_detected", "Secret detected"],
    ["future_code", "Reason unavailable"],
  ])("shows %s with the planning_diff_refused gate", (code, label) => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_gate_reason: "planning_diff_refused", plan_cross_check_diff_refusal: code })} />);
    expect(r.getByText("Current gate: Planning diff refused")).toBeTruthy();
    expect(r.getByText(`Refusal: ${label}`)).toBeTruthy();
  });
  it("hides a stale sub-code under another gate reason or none", () => {
    const other = render(<PlanCrossCheck run={run({ plan_cross_check_gate_reason: "revise", plan_cross_check_diff_refusal: "unsupported_entry" })} />);
    expect(other.container.textContent).not.toContain("Refusal:");
    cleanup();
    const bare = render(<PlanCrossCheck run={run({ plan_cross_check_gate_reason: "planning_diff_refused", plan_cross_check_diff_refusal: null })} />);
    expect(bare.container.textContent).not.toContain("Refusal:");
  });
});

describe("recorded checker sources", () => {
  it.each([
    ["pin", "worker default", "pin", "worker default"],
    [null, null, "unknown", "unknown"],
    ["provider-attribute\u202e", "worker_default", "unknown", "unknown"],
  ])("renders only closed recorded labels %s/%s", (modelSource, effortSource, modelLabel, effortLabel) => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({
      checker_model: "historical-model", checker_effort: "high", historical: true,
      checker_model_source: modelSource as PlanCrossCheckSummary["checker_model_source"],
      checker_effort_source: effortSource as PlanCrossCheckSummary["checker_effort_source"],
    }) })} />);
    const model = r.getByText(`Model: historical-model (${modelLabel})`);
    const effort = r.getByText(`Effort: high (${effortLabel})`);
    expect(model.closest("p")).toBe(effort.closest("p"));
    expect(r.container.textContent).not.toContain("provider-attribute");
    expect(r.getByText("These findings concern an earlier plan; they do not certify the current plan.")).toBeTruthy();
  });
});

describe("plan evidence rows and recording states", () => {
  it.each([3, 5])("renders %s ordered rows with inert paths and native disclosure", count => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({ findings: {
      summary: "Overview", items: Array.from({ length: count }, (_, i) => ({
        file: "pkg/__init__.py", severity: i === 0 ? "toString" : i === 1 ? "error" : "warning",
        summary: "**Title " + i + "**", rationale: "Inline `code` [safe](https://example.com) [bad](javascript:alert(1))",
      })),
    } }) })} />);
    const rows = [...r.container.querySelectorAll("li")];
    expect(rows.map(row => row.querySelector("strong")?.textContent)).toEqual(Array.from({ length: count }, (_, i) => "Title " + i));
    const chip = rows[0].querySelector("code")!;
    expect(chip.textContent).toBe("pkg/__init__.py");
    expect(chip.querySelector("strong, em")).toBeNull();
    expect(rows[0].textContent).toContain("Severity unavailable");
    expect(rows[1].querySelector(".text-danger")).toBeTruthy();
    expect(rows[2].querySelector(".text-warn")).toBeTruthy();
    expect(rows[0].querySelector(".judge-prose.text-muted code")?.textContent).toBe("code");
    expect(rows[0].querySelector('a[href="https://example.com"]')).toBeTruthy();
    expect(r.container.querySelector('a[href^="javascript:"]')).toBeNull();
    const details = r.container.querySelector("details");
    expect(rows.slice(0, 3).every(row => !row.closest("details"))).toBe(true);
    if (count === 3) { expect(details).toBeNull(); return; }
    expect(rows.slice(3).every(row => row.closest("details") === details)).toBe(true);
    const toggle = r.getByText("Show 2 more findings");
    expect(toggle.tagName).toBe("SUMMARY");
    expect(toggle.className).toContain("focus-visible:ring-2");
    (details as HTMLDetailsElement).open = true;
    fireEvent.toggle(details!);
    expect(r.getByText("Hide 2 more findings")).toBeTruthy();
    (details as HTMLDetailsElement).open = false;
    fireEvent.toggle(details!);
    expect(r.getByText("Show 2 more findings")).toBeTruthy();
  });
  it("consumes summary then file/severity/summary/rationale without a new path cap", () => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({ findings: {
      summary: "S".repeat(1000), items: [
        { file: "P".repeat(1000), severity: "info", summary: "T".repeat(10000), rationale: "R".repeat(5000) },
        ...Array.from({ length: 20 }, () => ({ file: "omitted", severity: "info", summary: "omitted", rationale: "omitted" })),
      ],
    } }) })} />);
    const rows = r.container.querySelectorAll("li");
    expect(rows).toHaveLength(20);
    expect(rows[0].querySelector("code")?.textContent).toBe("P".repeat(1000));
    expect(rows[0].querySelector(".judge-prose.text-muted")?.textContent).toBe("R".repeat(4380));
    expect(rows[1].textContent).not.toContain("omitted");
    const notice = r.getByText("Findings truncated for display.");
    const details = r.container.querySelector("details")!;
    expect(details.open).toBe(false);
    expect(notice.closest("details")).toBeNull();
    details.open = true;
    fireEvent.toggle(details);
    expect(r.getByText("Hide 17 more findings")).toBeTruthy();
    expect(notice.closest("details")).toBeNull();
  });
  it.each(["queued", "running", "completed", "failed", "cancelled"] as const)("labels missing required plan evidence for %s", status => {
    const terminal = ["completed", "failed", "cancelled"].includes(status);
    const r = render(<PlanCrossCheck run={run({ status, plan_cross_check_required: true, plan_cross_check_summary: undefined, plan_cross_check_gate_reason: undefined })} />);
    expect(r.getByText(terminal ? "Not recorded" : "Pending")).toBeTruthy();
    expect(r.getByText(terminal ? "This run ended without a plan cross-check result." : "Runs when the plan is ready, before it can proceed.")).toBeTruthy();
    if (!terminal) {
      expect(r.queryByText("Not recorded")).toBeNull();
      expect(r.queryByText(/This run ended/)).toBeNull();
      expect(r.queryByRole("link", { name: "Open checker run" })).toBeNull();
      expect(r.queryByText(/Model:|Checker cost:|Checked candidate:/)).toBeNull();
    }
  });
  it.each([null, ""])("shows valid pending reason %s with no-result copy", reason_class => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({ verdict: "pending", reason_class }) })} />);
    expect(r.getByText("Pending").closest("header")).toBeTruthy();
    expect(r.getByText("No result recorded yet.")).toBeTruthy();
  });
  it("prioritizes historical state while retaining strict outcome evidence", () => {
    const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({ historical: true, verdict: "approve", reason_class: "block" }) })} />);
    expect(r.getByText("Superseded").closest("header")).toBeTruthy();
    expect(r.getByText("Earlier-plan evidence")).toBeTruthy();
    expect(r.getByText("Earlier-plan evidence: Outcome unavailable")).toBeTruthy();
    expect(r.getByText("These findings concern an earlier plan; they do not certify the current plan.")).toBeTruthy();
  });
});

it("renders summary inline code, literal hostile path and info/info/warning in source order", () => {
  const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({ findings: {
    summary: "Overview uses `plan_summary`.", items: ["info", "info", "warning"].map((severity, i) => ({
      file: "<img src=x>__hostile__*path*", severity, summary: "Row " + i, rationale: "Reason " + i,
    })),
  } }) })} />);
  const inline = r.getByText("plan_summary");
  expect(inline.tagName).toBe("CODE");
  expect(inline.closest(".judge-prose")).toBeTruthy();
  const rows = [...r.container.querySelectorAll("li")];
  expect(rows.map(row => row.querySelector(".judge-prose")?.textContent)).toEqual(["Row 0", "Row 1", "Row 2"]);
  expect(rows.map(row => row.querySelector(".shrink-0")?.textContent)).toEqual(["info", "info", "warning"]);
  expect(rows[2].querySelector(".text-warn")).toBeTruthy();
  for (const row of rows) {
    const chip = row.querySelector("code")!;
    expect(chip.textContent).toBe("<img src=x>__hostile__*path*");
    expect(chip.querySelector("img, em, strong")).toBeNull();
  }
  expect(r.container.querySelector("img, .animate-pulse, .overflow-auto, .overflow-y-auto, .overflow-scroll, .overflow-y-scroll")).toBeNull();
  expect(r.queryByText(/Checking/)).toBeNull();
});

it.each(["summary", "file", "title", "detail", "items"])("discloses isolated plan %s truncation outside either disclosure state", field => {
  const items = Array.from({ length: field === "items" ? 21 : 5 }, (_, i) => ({
    file: "path_" + i, severity: "info", summary: "Title " + i, rationale: "Detail " + i,
  }));
  if (field === "file") items[0].file = "P".repeat(16385);
  if (field === "title") items[0].summary = "T".repeat(16385);
  if (field === "detail") items[0].rationale = "D".repeat(16385);
  const r = render(<PlanCrossCheck run={run({ plan_cross_check_summary: summary({ findings: {
    summary: field === "summary" ? "S".repeat(16385) : "Overview", items,
  } }) })} />);
  const notice = r.getByText("Findings truncated for display.");
  const details = r.container.querySelector("details")!;
  expect(details.open).toBe(false);
  expect(notice.closest("details")).toBeNull();
  expect(r.container.querySelectorAll("li")).toHaveLength(field === "items" ? 20 : 5);
  if (field === "items") expect(r.queryByText("Title 20")).toBeNull();
  details.open = true;
  fireEvent.toggle(details);
  expect(r.getByText(field === "items" ? "Hide 17 more findings" : "Hide 2 more findings")).toBeTruthy();
  expect(notice.closest("details")).toBeNull();
});
