// @vitest-environment jsdom
// PRD #3879 M1: the Issue work card.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { IssueLine } from "./IssueLine";
import { issueLineModel, type IssueStage, type IssueWorkScope } from "../lib/issueLine";
import { maskEmail } from "../lib/demoMask";
import { aRun } from "../pages/overviewFixtures";

afterEach(cleanup);

type Run = ReturnType<typeof aRun>;
type ScopeToggle = { value: IssueWorkScope; onChange: (s: IssueWorkScope) => void; allDisabled: boolean };

function mount(
  ownRuns: Run[],
  otherRuns: Run[] = [],
  props: { selected?: IssueStage | null; onSelect?: (s: IssueStage | null) => void; demo?: boolean; scope?: IssueWorkScope; scopeToggle?: ScopeToggle } = {},
) {
  const onSelect = props.onSelect ?? vi.fn();
  const view = render(
    <IssueLine
      model={issueLineModel({ ownRuns, otherRuns })}
      selected={props.selected ?? null}
      onSelect={onSelect}
      demo={props.demo ?? false}
      scope={props.scope ?? "you"}
      scopeToggle={props.scopeToggle}
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
      "queued",
      "planning",
      "review",
      "building",
      "pr",
      "asking",
      "parked",
      "other",
    ]);
    expect(within(region).queryByRole("button", { name: "Show all" })).toBeNull();
  });

  it("shows each station's count, its caption and one badge per run", () => {
    mount([
      aRun({ id: "q", status: "queued", issue_iid: 11 }),
      aRun({ id: "b1", issue_iid: 21 }),
      aRun({ id: "b2", issue_iid: 22 }),
      aRun({ id: "g", status: "awaiting_approval", issue_iid: 31 }),
    ]);
    const queued = station(/^Queued/);
    expect(queued.getAttribute("data-count")).toBe("1");
    expect(queued.textContent).toContain("Claiming a slot");
    expect(within(queued).getByText("#11")).toBeTruthy();
    const building = station(/^Building/);
    expect(building.getAttribute("data-count")).toBe("2");
    expect(building.textContent).toContain("Implement, review, validate");
    expect(within(building).getAllByText(/^#2\d$/)).toHaveLength(2);
    const gate = station(/^Plan review/);
    expect(gate.getAttribute("data-count")).toBe("1");
    expect(gate.textContent).toContain("Waiting on you");
    expect(within(gate).getByText("#31")).toBeTruthy();
    expect(station(/^Planning/).textContent).toContain("Nothing planning");
  });

  it("counts a job with no issue reference under Other work, badged with its kind label", () => {
    mount([aRun({ id: "j", kind: "job", status: "running", issue_iid: null })]);
    const other = screen.getByRole("button", { name: /Other work/ });
    expect(other.getAttribute("data-count")).toBe("1");
    expect(station(/^Building/).getAttribute("data-count")).toBe("0");
    expect(station(/^Building/).textContent).toContain("Nothing building");
  });

  it("strips a bidi override from the owner shown in another user's badge tooltip", () => {
    const other = theirs(aRun({ id: "x", status: "queued", issue_iid: 2 }), "\u202Eevil@x.example");
    mount([], [other], { scope: "all" });
    const title = within(station(/^Queued/)).getByText("#2").getAttribute("title");
    expect(title).toBe("evil@x.example");
    expect(title).not.toContain("\u202E");
  });

  it("names the owner in the tooltip of another user's badge only, masked in demo mode", () => {
    const own = aRun({ id: "mine", status: "queued", issue_iid: 1 });
    const other = theirs(aRun({ id: "x", status: "queued", issue_iid: 2 }), "mira.kovacs@uzi.local");
    mount([own], [other], { scope: "all" });
    const queued = station(/^Queued/);
    expect(within(queued).getByText("#1").getAttribute("title")).toBeNull();
    expect(within(queued).getByText("#2").getAttribute("title")).toBe("mira.kovacs@uzi.local");
    cleanup();
    mount([own], [other], { scope: "all", demo: true });
    const masked = maskEmail("mira.kovacs@uzi.local", true);
    expect(masked).not.toContain("@");
    expect(within(station(/^Queued/)).getByText("#2").getAttribute("title")).toBe(masked);
  });

  it("adds a cross-check tag to the chip, with its meaning in the tooltip", () => {
    mount([
      aRun({ id: "c", issue_iid: 4040, status: "running", cross_check_active: "code" }),
      aRun({ id: "plain", issue_iid: 4041, status: "running" }),
      aRun({ id: "g", issue_iid: 4042, status: "awaiting_approval", plan_cross_check_gate_reason: "revisions_exhausted" }),
    ]);
    const building = station(/^Building/);
    const chip = within(building).getByText("#4040");
    expect(chip.textContent).toBe("#4040 cross-checking");
    expect(within(chip).getByText("cross-checking")).toBeTruthy();
    expect(chip.getAttribute("title")).toBe("Code cross-check in progress (advisory, does not block)");
    expect(within(building).getByText("#4041").textContent).toBe("#4041");
    expect(within(building).getByText("#4041").getAttribute("title")).toBeNull();
    const gated = within(station(/^Plan review/)).getByText("#4042");
    expect(within(gated).getByText("cross-check: revise").className).toContain("text-warn");
    expect(gated.getAttribute("title")).toBe("Revisions exhausted");
    const blocked = aRun({ id: "b", issue_iid: 4043, status: "awaiting_approval", plan_cross_check_gate_reason: "block" });
    cleanup();
    mount([blocked]);
    expect(within(within(station(/^Plan review/)).getByText("#4043")).getByText("cross-check: blocked").className).toContain(
      "text-danger",
    );
  });

  it("shows completion tags ahead of a code cross-check, including another user's run", () => {
    const other = theirs(aRun({ id: "other", issue_iid: 4230, status: "running", completion_phase: "checking", cross_check_active: "code" }));
    mount([
      aRun({ id: "check", issue_iid: 4198, status: "running", completion_phase: "checking" }),
      aRun({ id: "rework", issue_iid: 4201, status: "running", completion_phase: "reworking" }),
      aRun({ id: "plain", issue_iid: 4209, status: "running" }),
    ], [other], { scope: "all" });
    const building = station(/^Building/);
    expect(building.textContent).toContain("1 building · 2 checking completion · 1 reworking");
    expect(within(building).getByText("#4198").textContent).toBe("#4198 checking completion");
    expect(within(building).getByText("#4198").getAttribute("title")).toBe("Checking completion");
    expect(within(building).getByText("#4201").textContent).toBe("#4201 reworking milestones");
    expect(within(building).getByText("#4201").getAttribute("title")).toBe("Reworking unmet milestones");
    const combined = within(building).getByText("#4230");
    expect(combined.textContent).toBe("#4230 checking completion");
    expect(combined.getAttribute("title")).toBe("mira@uzi.local · Checking completion; code cross-check also running (advisory)");
    expect(within(combined).queryByText("cross-checking")).toBeNull();
    expect(within(building).getByText("#4209").textContent).toBe("#4209");
  });

  it("tags a claimed run under plan check in Queued, the tooltip naming the check", () => {
    mount([aRun({ id: "q", issue_iid: 4044, status: "claimed", cross_check_active: "plan" })]);
    const chip = within(station(/^Queued/)).getByText("#4044");
    expect(chip.textContent).toBe("#4044 cross-checking");
    expect(chip.getAttribute("title")).toBe("Plan cross-check in progress");
  });

  it("joins the owner and the tag meaning in another user's chip tooltip", () => {
    const other = theirs(aRun({ id: "x", issue_iid: 7, status: "running", cross_check_active: "plan", is_planning: true }));
    mount([], [other], { scope: "all" });
    const chip = within(station(/^Planning/)).getByText("#7");
    expect(chip.getAttribute("title")).toBe("mira@uzi.local · Plan cross-check in progress");
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
    expect(gateCount.className).not.toContain("text-fg");
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

  it("draws the gate count in plain text at zero and in the warn tone above zero", () => {
    mount([aRun({ id: "b" })]);
    const zero = within(station(/^Plan review/)).getByText("0");
    expect(zero.className).toContain("text-fg");
    expect(zero.className).not.toContain("text-warn");
    cleanup();
    mount([aRun({ id: "g", status: "awaiting_approval" })]);
    expect(within(station(/^Plan review/)).getByText("1").className).toContain("text-warn");
  });

  it("hides the connecting segments below md and shows them from md up", () => {
    mount([aRun({ id: "b" })]);
    for (const seg of Array.from(document.querySelectorAll<HTMLElement>("[data-segment]"))) {
      expect(seg.className).toMatch(/(^| )hidden( |$)/);
      expect(seg.className).toContain("md:block");
    }
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
      screen
        .getAllByRole("button")
        .filter((b) => b.hasAttribute("data-stage") && b.getAttribute("aria-pressed") === "true")
        .map((b) => b.getAttribute("data-stage"));
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
        scope="you"
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

  it("clears the selection by pressing the pressed stage again, and has no Show all button", () => {
    const { onSelect } = mount([aRun()], [], { selected: "building" });
    expect(station(/^Building/).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(station(/^Building/));
    expect(onSelect).toHaveBeenCalledWith(null);
    expect(screen.queryByRole("button", { name: "Show all" })).toBeNull();
  });

  describe("scope toggle", () => {
    it("renders only when scopeToggle is given", () => {
      mount([aRun()]);
      expect(screen.queryByRole("group", { name: "Issue work scope" })).toBeNull();
      cleanup();
      mount([aRun()], [], { scopeToggle: { value: "you", onChange: vi.fn(), allDisabled: false } });
      const group = screen.getByRole("group", { name: "Issue work scope" });
      expect(within(group).getAllByRole("button").map((b) => b.textContent)).toEqual(["You", "All users"]);
      expect(within(group).getByRole("button", { name: "You" }).getAttribute("aria-pressed")).toBe("true");
      expect(within(group).getByRole("button", { name: "All users" }).getAttribute("aria-pressed")).toBe("false");
    });

    it("disables All users when allDisabled, and leaves it enabled otherwise", () => {
      mount([aRun()], [], { scopeToggle: { value: "you", onChange: vi.fn(), allDisabled: true } });
      expect((screen.getByRole("button", { name: "All users" }) as HTMLButtonElement).disabled).toBe(true);
      expect((screen.getByRole("button", { name: "You" }) as HTMLButtonElement).disabled).toBe(false);
      cleanup();
      mount([aRun()], [], { scopeToggle: { value: "you", onChange: vi.fn(), allDisabled: false } });
      expect((screen.getByRole("button", { name: "All users" }) as HTMLButtonElement).disabled).toBe(false);
    });

    it("calls onChange with the clicked option", () => {
      const onChange = vi.fn();
      mount([aRun()], [], { scopeToggle: { value: "you", onChange, allDisabled: false } });
      fireEvent.click(screen.getByRole("button", { name: "All users" }));
      expect(onChange).toHaveBeenLastCalledWith("all");
      fireEvent.click(screen.getByRole("button", { name: "You" }));
      expect(onChange).toHaveBeenLastCalledWith("you");
    });

    it("puts the toggle last in the header, after the title and the note", () => {
      mount([aRun()], [], { scopeToggle: { value: "you", onChange: vi.fn(), allDisabled: false } });
      const header = screen.getByRole("heading", { name: "Issue work" }).parentElement as HTMLElement;
      const children = Array.from(header.children);
      expect(children[0].tagName).toBe("H2");
      expect(children[1].textContent).toContain("on the line");
      expect(children[2].getAttribute("aria-label")).toBe("Issue work scope");
      expect(children).toHaveLength(3);
    });
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

  it("writes the header note, adding all users for the all scope only", () => {
    const runs = [aRun({ id: "a", status: "queued" }), aRun({ id: "b", kind: "ci_fix" })];
    mount(runs);
    expect(screen.getByText("1 run on the line, 1 other")).toBeTruthy();
    cleanup();
    mount(runs, [theirs(aRun({ id: "x", status: "queued" }))], { scope: "all" });
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
      scope: "all",
    });
    expect(screen.getByRole("button", { name: /Questions/ }).textContent).toContain("Waiting on owners");
    expect(screen.queryByRole("button", { name: /Asking you/ })).toBeNull();
    expect(station(/^Plan review/).textContent).toContain("Waiting on owners");
  });
});
