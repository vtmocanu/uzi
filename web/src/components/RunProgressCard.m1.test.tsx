// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunProgressCard } from "./RunProgressCard";
import type { RunActivity, RunProgress } from "../lib/apiTypes";

vi.mock("../lib/useNow", () => ({ useNow: () => Date.parse("2026-10-09T12:00:00Z") }));
afterEach(cleanup);
type CardRun = Parameters<typeof RunProgressCard>[0]["run"];
const frozen = [1, 2, 3, 4].map(n => ({id: `m${n}`, title: `Task ${n}`}));
const activity: RunActivity = {agent: "coder", agent_label: "Wire the gate", tool: "Edit", detail: "src/gate.ts", at: "2026-10-09T11:59:00Z", seq: 1};
const lane = {...activity, agent_instance: "coder-1"};
const note = {text: "Checking the gate", at: "2026-10-09T11:57:00Z"};
function pct(over: Partial<RunProgress> = {}): RunProgress {
  return {state: "percent", pct: 11, milestone_done: 99, milestone_total: 100, active_milestone_id: "m2", phase: "implement", now_note: note, ...over};
}
function mount(over: Partial<CardRun> = {}, live: RunActivity | null = null, stage: string | null = null) {
  return render(<MemoryRouter><RunProgressCard run={{id: "r", status: "running", health_since: null, milestones: frozen, milestones_completed: null, retry_not_before: null, recovery_retry_not_before: null, progress: pct(), ...over}} activity={live} stage={stage} /></MemoryRouter>);
}
const row = (id: string) => screen.getByText(id).closest("li") as HTMLElement;
const visualHead = (container: HTMLElement) => {
  const copy = container.querySelector("[data-progress-head]")!.cloneNode(true) as HTMLElement;
  copy.querySelectorAll(".sr-only").forEach(el => el.remove());
  return copy.textContent;
};

describe("M1 unified progress invariants", () => {
  it("one frozen list; unreported stays truthful in the visual count and accessible track", () => {
    const {container} = mount({progress: pct({now_note: undefined})});
    expect(container.querySelectorAll("ul")).toHaveLength(1);
    expect(container.querySelectorAll("li")).toHaveLength(4);
    expect(visualHead(container)).toBe("≈11%·– of 4 reported");
    expect(screen.queryByText("0 of 4 reported complete")).toBeNull();
    expect(screen.getByRole("img").getAttribute("aria-label")).toContain("– of 4 reported; no milestone completion reported");
    expect(screen.getByRole("img").getAttribute("aria-label")).not.toContain("0 of 4 milestones reported complete");
    expect([...container.querySelectorAll("[data-seg]")].every(el => el.textContent === "")).toBe(true);
  });

  it.each([{completed: []}, {completed: ["m1", "m1", "foreign"]}])("count uses frozen membership with completion set %j, never DTO count", ({completed}) => {
    const {container} = mount({milestones_completed: completed, progress: pct({now_note: undefined})});
    const expected = completed.length ? "1 of 4 reported complete" : "0 of 4 reported complete";
    expect(screen.getByText(expected)).toBeTruthy();
    expect(visualHead(container)).toBe(`≈11%·${expected}`);
    expect(container.querySelectorAll('[aria-label="reported complete"]')).toHaveLength(completed.length ? 1 : 0);
  });

  it("Now, age, phase, iteration and run stage attach to active ID independently of row state", () => {
    const {container} = mount({milestones_in_progress: ["m3"], iteration_count: 4}, activity, "Writing");
    expect(within(row("m2")).getByText(/Now:/).closest("p")?.textContent).toBe("Now: Checking the gate · model summary · 3m ago");
    expect(within(row("m2")).getByText("phase: implement · iteration 4 · run: Writing")).toBeTruthy();
    expect(within(row("m3")).queryByText(/Now:/)).toBeNull();
    expect(within(row("m3")).getByText("coder")).toBeTruthy();
    expect(container.querySelectorAll("[data-progress-now]")).toHaveLength(1);
    expect(container.querySelector("[data-progress-head] [data-progress-context]")).toBeNull();
  });

  it.each(["", "deploy"])("unknown phase %s preserves iteration and sanitised run stage on active row", phase => {
    const {container} = mount({progress: pct({phase: phase as RunProgress["phase"]}), iteration_count: 3}, null, "Wri\u202Eting");
    expect(within(row("m2")).getByText("iteration 3 · run: Writing")).toBeTruthy();
    expect(container.querySelector("[data-progress-phase]")).toBeNull();
    expect(container.querySelector("[data-progress-head]")?.textContent).not.toContain("run: Writing");
  });

  it.each(["ghost", ""])("unmatched active ID %s leaves Now under head and iteration/stage in head", id => {
    const {container} = mount({progress: pct({active_milestone_id: id}), iteration_count: 2}, null, "Thinking");
    const now = container.querySelector("[data-progress-now]")!;
    expect(now.closest("li")).toBeNull();
    expect(now.compareDocumentPosition(container.querySelector("ul")!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(now.textContent).toContain("model summary · 3m ago");
    expect(container.querySelector("[data-progress-head]")?.textContent).toContain("phase: implement · iteration 2 · run: Thinking");
    expect(container.querySelectorAll("[data-progress-now]")).toHaveLength(1);
  });

  it("authoritative owner plus second lane deduplicates owner, suppresses current activity", () => {
    const {container} = mount({milestones_in_progress: ["m2"], milestones_agents: [{id: "m2", agent: "coder", agent_label: "Declared task"}], milestones_live: [{milestone_id: "m2", lanes: [lane, {...lane, agent: "reviewer", agent_instance: "reviewer-1", agent_label: "Review gate"}]}]}, {...activity, agent: "stale"});
    expect(within(row("m2")).getAllByText("coder")).toHaveLength(1);
    expect(within(row("m2")).getByText("reviewer")).toBeTruthy();
    expect(screen.queryByText("Declared task")).toBeNull();
    expect(screen.queryByText("stale")).toBeNull();
    expect(container.querySelectorAll(".animate-pulse")).toHaveLength(2);
  });

  it("declared owner without attributed live lane is quiet and truthful plain text", () => {
    const {container} = mount({milestones_in_progress: ["m2"], milestones_agents: [{id: "m2", agent: "arch\u202Eitect", agent_label: "Check **gate**\u200B"}], milestones_live: [{milestone_id: "m2", lanes: [lane]}]}, activity);
    const assigned = within(row("m2")).getByText("architect").parentElement?.parentElement as HTMLElement;
    expect(assigned.textContent).toBe("Assigned owner: architect · Check **gate**");
    expect(assigned.querySelector(".animate-pulse")).toBeNull();
    expect(assigned.querySelector("strong")).toBeNull();
    expect(within(row("m2")).getByText("coder")).toBeTruthy();
    expect(container.querySelectorAll(".animate-pulse")).toHaveLength(1);
  });

  it("parked declared owner without a live frame stays unpulsed", () => {
    mount({status: "limit_wait", progress: pct({state: "parked", pct: null}), milestones_in_progress: ["m2"], milestones_agents: [{id: "m2", agent: "architect", agent_label: "Coordinate"}]});
    const assigned = within(row("m2")).getByText("architect").parentElement?.parentElement as HTMLElement;
    expect(assigned.textContent).toContain("Assigned owner: architect");
    expect(assigned.querySelector(".animate-pulse")).toBeNull();
  });

  it.each([null, "stalled", "parked"] as const)("pre-feature/stalled/parked %s retains exactly one first-in-progress fallback", state => {
    const {container} = mount({status: state === "parked" ? "limit_wait" : "running", progress: state === null ? null : pct({state, pct: null}), milestones_in_progress: ["m3", "m2"]}, activity);
    expect(within(row("m2")).getByText("coder")).toBeTruthy();
    expect(within(row("m3")).queryByText("coder")).toBeNull();
    expect(screen.getAllByText("Wire the gate")).toHaveLength(1);
    expect(container.querySelector("[data-progress-activity]")).toBeNull();
  });

  it("paused DTO lanes disappear while the declared owner stays quiet", () => {
    const {container} = mount({status: "paused", progress: pct({state: "parked", pct: null}), milestones_in_progress: ["m2"], milestones_agents: [{id: "m2", agent: "architect", agent_label: "Coordinate"}], milestones_live: [{milestone_id: "m2", lanes: [lane]}]}, activity);
    expect(screen.getByText("architect")).toBeTruthy();
    expect(screen.queryByText("coder")).toBeNull();
    expect(screen.queryByText("Edit src/gate.ts")).toBeNull();
    expect(container.querySelector(".animate-pulse")).toBeNull();
  });

  it("completed precedence prevents fallback activity under a completed row", () => {
    mount({milestones_completed: ["m1"], milestones_in_progress: ["m1", "m2"]}, activity);
    expect(within(row("m1")).getByLabelText("reported complete")).toBeTruthy();
    expect(within(row("m1")).queryByText("coder")).toBeNull();
    expect(within(row("m2")).getByText("coder")).toBeTruthy();
    expect(screen.getAllByText("Wire the gate")).toHaveLength(1);
  });

  it("stale completed-row lanes do not disable fallback under the live row", () => {
    mount({milestones_completed: ["m1"], milestones_in_progress: ["m1", "m2"], milestones_live: [{milestone_id: "m1", lanes: [lane]}]}, activity);
    expect(within(row("m1")).queryByText("coder")).toBeNull();
    expect(within(row("m2")).getByText("coder")).toBeTruthy();
  });

  it("nothing in progress keeps one unattached activity strip", () => {
    mount({progress: null}, activity);
    expect(screen.getAllByText("coder")).toHaveLength(1);
    expect(screen.getByText("coder").closest("li")).toBeNull();
  });

  it.each([null, "percent", "stalled", "parked"] as const)("no milestones with progress %s retains sanitized label or tool fallback once", state => {
    const {container} = mount({milestones: null, progress: state === null ? null : pct({state, pct: state === "percent" ? 11 : null}), status: state === "parked" ? "limit_wait" : "running"}, {...activity, agent: "co\u202Eder", agent_label: "", tool: "Ed\u200Bit"});
    expect(screen.getAllByText("coder · Edit")).toHaveLength(1);
    expect(container.querySelector("ul")).toBeNull();
  });

  it.each(["completed", "failed", "cancelled"] as const)("terminal %s suppresses stale percent-state Now, owners and lanes", status => {
    const {container} = mount({status, milestones_in_progress: ["m2"], milestones_agents: [{id: "m2", agent: "coder", agent_label: "Declared"}], milestones_live: [{milestone_id: "m2", lanes: [lane]}], iteration_count: 5}, activity, "Writing");
    expect(container.querySelector("[data-progress-now]")).toBeNull();
    expect(screen.queryByText("coder")).toBeNull();
    expect(screen.queryByText("Declared")).toBeNull();
    expect(container.querySelector(".animate-pulse")).toBeNull();
    expect(container.querySelector("[data-progress-context]")).toBeNull();
    expect(container.querySelectorAll("li")).toHaveLength(4);
  });
});
