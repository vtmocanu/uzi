// @vitest-environment jsdom
// PRD #3879 M1: the Issue work card.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { IssueLine } from "./IssueLine";
import { issueLineModel, type IssueStage } from "../lib/issueLine";
import { maskEmail } from "../lib/demoMask";
import { aRun } from "../pages/overviewFixtures";

afterEach(cleanup);

type Run = ReturnType<typeof aRun>;

function mount(
  ownRuns: Run[],
  otherRuns: Run[] = [],
  props: { selected?: IssueStage | null; onSelect?: (s: IssueStage | null) => void; demo?: boolean; factory?: boolean } = {},
) {
  const onSelect = props.onSelect ?? vi.fn();
  const view = render(
    <IssueLine
      model={issueLineModel({ ownRuns, otherRuns })}
      selected={props.selected ?? null}
      onSelect={onSelect}
      demo={props.demo ?? false}
      factory={props.factory ?? false}
    />,
  );
  return { ...view, onSelect };
}

const station = (name: RegExp) => screen.getByRole("button", { name }) as HTMLElement;
const theirs = (r: Run, email = "mira@uzi.local"): Run => ({ ...r, owner_email: email });

describe("IssueLine", () => {
  it("is a labelled region carrying the Shadow marker, with a filter group of five stations and three bays", () => {
    mount([aRun()]);
    const region = screen.getByRole("region", { name: "Issue work" });
    expect(region.hasAttribute("data-live")).toBe(true);
    const group = within(region).getByRole("group", { name: "Filter runs by stage" });
    expect(within(group).getAllByRole("button").map((b) => b.getAttribute("data-stage"))).toEqual([
      "queued",
      "planning",
      "review",
      "building",
      "pr",
    ]);
    expect(within(region).getAllByRole("button").map((b) => b.getAttribute("data-stage"))).toEqual([
      null,
      "queued",
      "planning",
      "review",
      "building",
      "pr",
      "asking",
      "parked",
      "other",
    ]);
    expect(within(region).getByRole("button", { name: "Show all" })).toBeTruthy();
  });

  it("shows each station's count, its caption and one badge per run", () => {
    mount([
      aRun({ id: "q", status: "queued", issue_iid: 11 }),
      aRun({ id: "b1", issue_iid: 21 }),
      aRun({ id: "b2", issue_iid: 22 }),
      aRun({ id: "g", status: "awaiting_approval", issue_iid: 31 }),
    ]);
    const queued = station(/^Queued/);
    expect(queued.textContent).toContain("1");
    expect(queued.textContent).toContain("Claiming a slot");
    expect(within(queued).getByText("#11")).toBeTruthy();
    const building = station(/^Building/);
    expect(building.textContent).toContain("2");
    expect(building.textContent).toContain("Implement, review, validate");
    expect(within(building).getAllByText(/^#2\d$/)).toHaveLength(2);
    const gate = station(/^Plan review/);
    expect(gate.textContent).toContain("Waiting on you");
    expect(within(gate).getByText("#31")).toBeTruthy();
    expect(station(/^Planning/).textContent).toContain("Nothing planning");
  });

  it("falls back to the kind label for a run with no issue reference", () => {
    mount([aRun({ id: "j", kind: "job", status: "running", issue_iid: null })]);
    const other = screen.getByRole("button", { name: /Other work/ });
    expect(other.textContent).toContain("1");
    expect(station(/^Building/).textContent).toContain("Nothing building");
  });

  it("names the owner in the tooltip of another user's badge only, masked in demo mode", () => {
    const own = aRun({ id: "mine", status: "queued", issue_iid: 1 });
    const other = theirs(aRun({ id: "x", status: "queued", issue_iid: 2 }), "mira.kovacs@uzi.local");
    mount([own], [other], { factory: true });
    const queued = station(/^Queued/);
    expect(within(queued).getByText("#1").getAttribute("title")).toBeNull();
    expect(within(queued).getByText("#2").getAttribute("title")).toBe("mira.kovacs@uzi.local");
    cleanup();
    mount([own], [other], { factory: true, demo: true });
    const masked = maskEmail("mira.kovacs@uzi.local", true);
    expect(masked).not.toContain("@");
    expect(within(station(/^Queued/)).getByText("#2").getAttribute("title")).toBe(masked);
  });

  it("collapses badges past twelve into a +N badge", () => {
    const runs = Array.from({ length: 15 }, (_, i) => aRun({ id: `r${i}`, issue_iid: 100 + i }));
    mount(runs);
    const building = station(/^Building/);
    expect(within(building).getAllByText(/^#1\d\d$/)).toHaveLength(12);
    expect(within(building).getByText("+3")).toBeTruthy();
  });

  it("draws the gate as a rotated square with its count in the warn tone, and tones each station", () => {
    mount([
      aRun({ id: "q", status: "queued" }),
      aRun({ id: "p", is_planning: true }),
      aRun({ id: "g", status: "awaiting_approval" }),
      aRun({ id: "b" }),
      aRun({ id: "pr", status: "completed", mr_iid: 3, mr_state: "opened", finished_at: "2026-07-05T12:00:00Z" }),
    ]);
    const node = (name: RegExp) => station(name).querySelector("[data-node]") as HTMLElement;
    expect(node(/^Plan review/).getAttribute("data-node")).toBe("gate");
    expect(node(/^Plan review/).className).toContain("rotate-45");
    expect(node(/^Plan review/).className).toContain("border-warn");
    const gateCount = within(station(/^Plan review/)).getByText("1");
    expect(gateCount.className).toContain("text-warn");
    expect(node(/^Queued/).className).toContain("border-faint");
    expect(node(/^Planning/).className).toContain("border-plan");
    expect(node(/^Building/).className).toContain("border-info");
    expect(node(/^PR open/).className).toContain("border-ok");
    // The station badges take the same tones as the status pills.
    expect(within(station(/^Planning/)).getByText("#7").className).toContain("text-plan");
    expect(within(station(/^Building/)).getByText("#7").className).toContain("text-info");
    expect(within(station(/^Plan review/)).getByText("#7").className).toContain("text-warn");
    expect(within(station(/^PR open/)).getByText("#7").className).toContain("text-ok");
  });

  it("flows only into Planning and Building while they hold live work", () => {
    const seg = (name: RegExp) => station(name).querySelector("[data-segment]") as HTMLElement | null;
    mount([aRun({ id: "p", is_planning: true }), aRun({ id: "g", status: "awaiting_approval" })]);
    expect(seg(/^Queued/)).toBeNull();
    expect(seg(/^Planning/)?.className).toContain("issue-line-flow");
    expect(seg(/^Building/)?.className).not.toContain("issue-line-flow");
    expect(seg(/^Plan review/)?.className).not.toContain("issue-line-flow");
    expect(seg(/^PR open/)?.className).not.toContain("issue-line-flow");
    cleanup();
    mount([aRun({ id: "b" })]);
    expect(seg(/^Planning/)?.className).not.toContain("issue-line-flow");
    expect(seg(/^Building/)?.className).toContain("issue-line-flow");
  });

  it("is pressed only on the selected stage, and toggles through onSelect", () => {
    const { onSelect, rerender } = mount([aRun({ id: "g", status: "awaiting_approval" })]);
    const pressed = () =>
      screen.getAllByRole("button").filter((b) => b.getAttribute("aria-pressed") === "true").map((b) => b.getAttribute("data-stage"));
    expect(pressed()).toEqual([]);
    for (const b of screen.getAllByRole("button")) {
      if (b.getAttribute("data-stage")) expect(b.getAttribute("aria-pressed")).toBe("false");
    }
    fireEvent.click(station(/^Plan review/));
    expect(onSelect).toHaveBeenLastCalledWith("review");

    rerender(
      <IssueLine
        model={issueLineModel({ ownRuns: [aRun({ id: "g", status: "awaiting_approval" })], otherRuns: [] })}
        selected="review"
        onSelect={onSelect}
        demo={false}
        factory={false}
      />,
    );
    expect(pressed()).toEqual(["review"]);
    fireEvent.click(station(/^Plan review/));
    expect(onSelect).toHaveBeenLastCalledWith(null);
    fireEvent.click(screen.getByRole("button", { name: /Other work/ }));
    expect(onSelect).toHaveBeenLastCalledWith("other");
    fireEvent.click(screen.getByRole("button", { name: /Parked/ }));
    expect(onSelect).toHaveBeenLastCalledWith("parked");
    fireEvent.click(screen.getByRole("button", { name: /Asking you/ }));
    expect(onSelect).toHaveBeenLastCalledWith("asking");
  });

  it("returns to all runs with Show all", () => {
    const { onSelect } = mount([aRun()], [], { selected: "building" });
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    expect(onSelect).toHaveBeenCalledWith(null);
  });

  it("describes the PR station as a count of PRs that is not active, never merged, with no action", () => {
    mount([
      aRun({ id: "p1", status: "completed", mr_iid: 3, mr_state: "opened", repo_id: "r1", finished_at: "2026-07-05T12:00:00Z" }),
      aRun({ id: "p2", status: "completed", mr_iid: 4, mr_state: "opened", repo_id: "r1", finished_at: "2026-07-05T12:00:00Z" }),
    ]);
    const pr = station(/^PR open/);
    expect(pr.textContent).toContain("2PRs");
    expect(pr.textContent).toContain("PR state as last seen by uzi");
    expect(pr.textContent).toContain("Not counted as active");
    expect(pr.textContent).not.toMatch(/merged/i);
    expect(pr.querySelector("a, button")).toBeNull();
    cleanup();
    mount([aRun({ status: "completed", mr_iid: 3, mr_state: "opened", finished_at: "2026-07-05T12:00:00Z" })]);
    expect(station(/^PR open/).textContent).toContain("1PR");
    expect(station(/^PR open/).textContent).not.toContain("1PRs");
  });

  it("writes the header note, adding all users for the admin's factory scope", () => {
    const runs = [aRun({ id: "a", status: "queued" }), aRun({ id: "b", kind: "ci_fix" })];
    mount(runs);
    expect(screen.getByText("1 run on the line, 1 other")).toBeTruthy();
    cleanup();
    mount(runs, [theirs(aRun({ id: "x", status: "queued" }))], { factory: true });
    expect(screen.getByText("2 runs on the line, 1 other, all users")).toBeTruthy();
  });

  it("fills the bays: Asking you, Parked with its reason, Other work", () => {
    mount([
      aRun({ id: "a", status: "awaiting_input", issue_iid: 409 }),
      aRun({ id: "p", status: "limit_wait", issue_iid: 94 }),
      aRun({ id: "o", kind: "mr_rework", status: "running", issue_iid: null }),
    ]);
    const asking = screen.getByRole("button", { name: /Asking you/ });
    expect(asking.textContent).toContain("#409 paused on a question");
    const parked = screen.getByRole("button", { name: /Parked/ });
    expect(parked.textContent).toContain("Resumes on its own");
    expect(parked.textContent).toContain("#94 limit wait");
    const other = screen.getByRole("button", { name: /Other work/ });
    expect(other.textContent).toContain("1");
    expect(other.textContent).toContain("Jobs, self-improve, CI fixes, reworks");
  });

  it("never implies the viewer can act on another user's question or plan", () => {
    mount([], [theirs(aRun({ id: "x", status: "awaiting_input" })), theirs(aRun({ id: "y", status: "awaiting_approval" }))], {
      factory: true,
    });
    expect(screen.getByRole("button", { name: /Questions/ }).textContent).toContain("Waiting on owners");
    expect(screen.queryByRole("button", { name: /Asking you/ })).toBeNull();
    expect(station(/^Plan review/).textContent).toContain("Waiting on owners");
  });
});
