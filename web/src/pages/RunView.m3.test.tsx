// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView, RunSummary } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { mockRuns } from "../mocks/data/runs";
import { summaryCollapse } from "../lib/prefs";
import type { Run } from "../lib/apiTypes";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 }, archives: [],
    }),
  } };
});
function run(over: Partial<Run> = {}): Run {
  return { ...mockRuns[0], id: "m3", kind: "issue", status: "awaiting_approval", plan_md: "# Review this plan",
    gate_revision: 1, outcome_pending: undefined, hold_reason: null, summary_intent: "Implement the requested feature.",
    progress: { state: "waiting", pct: null, phase: "", active_milestone_id: "", milestone_done: 0, milestone_total: 0 },
    ...over };
}
beforeEach(() => {
  const values = new Map<string, string>();
  Object.defineProperty(window, "localStorage", { configurable: true, value: {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
    removeItem: (key: string) => values.delete(key),
  } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it.each([undefined, true, false])("stored preference %s wins at mount and gate exit without remount", saved => {
  if (saved !== undefined) summaryCollapse.setCollapsed("m3", saved);
  const view = render(<RunSummary run={run()} />);
  expect(screen.getByRole("button", { name: saved === true ? "Expand" : "Collapse" }).getAttribute("aria-expanded")).toBe(String(saved !== true));
  view.rerender(<RunSummary run={run({ status: "running" })} />);
  const collapsed = saved ?? true;
  expect(screen.getByRole("button", { name: collapsed ? "Expand" : "Collapse" }).getAttribute("aria-expanded")).toBe(String(!collapsed));
  expect(Boolean(screen.queryByText("What this run will implement"))).toBe(!collapsed);
});

it.each(["missing", "expired", "bad-json", "bad-entry", "read-failure", "write-failure"])(
  "%s preference applies the state default and tolerates storage failure", kind => {
    if (kind === "expired") summaryCollapse.setCollapsed("m3", false, Date.now() - 8 * 24 * 60 * 60 * 1000);
    if (kind === "bad-json") window.localStorage.setItem("uzi.summaryCollapse", "{broken");
    if (kind === "bad-entry") window.localStorage.setItem("uzi.summaryCollapse", JSON.stringify({ m3: { collapsed: "false", savedAt: Date.now() } }));
    if (kind === "read-failure") vi.spyOn(window.localStorage, "getItem").mockImplementation(() => { throw new Error("storage unavailable"); });
    if (kind === "write-failure") vi.spyOn(window.localStorage, "setItem").mockImplementation(() => { throw new Error("quota"); });
    const view = render(<RunSummary run={run()} />);
    expect(screen.getByRole("button", { name: "Collapse" })).toBeTruthy();
    view.rerender(<RunSummary run={run({ status: "running" })} />);
    expect(screen.getByRole("button", { name: "Expand" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Expand" }));
    expect(screen.getByText("What this run will implement")).toBeTruthy();
  });

it.each([true, false])("explicit toggle %s persists over the gate transition", collapsed => {
  const view = render(<RunSummary run={run()} />);
  if (collapsed) fireEvent.click(screen.getByRole("button", { name: "Collapse" }));
  else {
    fireEvent.click(screen.getByRole("button", { name: "Collapse" }));
    fireEvent.click(screen.getByRole("button", { name: "Expand" }));
  }
  expect(summaryCollapse.getCollapsed("m3")).toBe(collapsed);
  view.rerender(<RunSummary run={run({ status: "running" })} />);
  expect(screen.getByRole("button", { name: collapsed ? "Expand" : "Collapse" })).toBeTruthy();
});

it("collapsed excerpt parses lists, references, nested destinations, entities, images and code into safe plain text", () => {
  const intent = "# Intent\n\n- **Build** [feature][ref] and [nested](https://example.com/a_(b))\n- ![diagram](https://example.com/image.png) with ~~old~~ \u202Enew &amp; &#x202e;safe\n\n~~~ts\nconst value = 1;\n~~~\n\n[ref]: https://example.com\n\n<div>HTML</div>";
  const { container } = render(<RunSummary run={run({ status: "running", summary_intent: intent })} />);
  const excerpt = container.querySelector("p")!;
  expect(excerpt.textContent).toBe("Intent Build feature and nested diagram with old new & safe const value = 1;");
  expect(excerpt.querySelector("a, img, code, strong, del, div")).toBeNull();
  expect(excerpt.textContent).not.toMatch(/[\p{Cf}]/u);
});

it("plain excerpt retains code identifiers and caps text at 150 characters", () => {
  const { container } = render(<RunSummary run={run({ status: "completed", summary_intent: "`snake_case` " + "x".repeat(200) })} />);
  expect(container.querySelector("p")?.textContent).toBe("snake_case " + "x".repeat(139));
});

async function mountPage(current: Run, owner = true) {
  const submit = vi.fn();
  vi.mocked(useRunStream).mockReturnValue({
    run: current, messages: [], connected: true, error: "", submit, refreshRun: vi.fn().mockResolvedValue(undefined),
    inputs: [], refreshInputs: vi.fn(), canSteer: owner, confirmedOwner: owner,
  } as ReturnType<typeof useRunStream>);
  const view = render(<MemoryRouter initialEntries={["/runs/m3"]}><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></MemoryRouter>);
  await act(async () => {});
  return { ...view, submit };
}
function before(first: Element, second: Element) {
  expect(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
}

it.each(["awaiting_approval", "completed", "failed", "cancelled"] as const)(
  "%s attention comes before history directly before progress", async status => {
    const { container } = await mountPage(run({ status, issue_input_reason: "permission_unknown" }));
    const attention = screen.getByText(status === "awaiting_approval" ? "Review this plan" : status === "cancelled" ? "Run stopped" : status === "completed" ? "Run completed" : "Run failed");
    const history = screen.getByRole("region", { name: "Issue input at run creation" });
    const progress = screen.getByRole("region", { name: "Run progress" });
    before(attention, history);
    before(history, progress);
    expect(history.nextElementSibling?.contains(progress)).toBe(true);
    // The last visible attention panel and history are siblings without an intervening card.
    let panel = attention;
    while (panel.parentElement !== history.parentElement) panel = panel.parentElement!;
    expect(panel.nextElementSibling).toBe(history);
    expect(container.querySelectorAll("[data-run-progress-card]")).toHaveLength(1);
  });

it.each([
  ["paused", null, /Paused by you/],
  ["paused", "credential_disabled", "Waiting: credential disabled"],
  ["limit_wait", null, /Paused on .*usage limit/],
  ["pool_wait", null, /Waiting for a pooled token/],
  ["recovery_wait", null, /Recovering and resuming automatically/],
] as const)("hold %s/%s stays before history and progress with owner controls gated", async (status, hold_reason, heading) => {
  const { submit } = await mountPage(run({ status, hold_reason, issue_input_reason: "permission_unknown" }), false);
  const attention = screen.getByText(heading);
  const history = screen.getByRole("region", { name: "Issue input at run creation" });
  before(attention, history);
  before(history, screen.getByRole("region", { name: "Run progress" }));
  expect(screen.queryByRole("button", { name: /^(Resume|Resume now|Stop run|Cancel)$/ })).toBeNull();
  expect(submit).not.toHaveBeenCalled();
});

it("approval remains inert for a non-owner while retaining its top-slot content", async () => {
  const { submit } = await mountPage(run(), false);
  expect(screen.getByText("Review this plan")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Approve/ })).toBeNull();
  expect(submit).not.toHaveBeenCalled();
});
