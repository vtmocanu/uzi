// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunSummaryItem } from "../lib/api";
import type { DecisionItem, HeldDecisionItem, RunDecisionItem } from "../lib/overview";
import { NeedsDecisionCard } from "./NeedsDecisionCard";
import { aRun } from "../pages/overviewFixtures";

afterEach(cleanup);

const NOW = Date.parse("2026-07-05T12:20:00Z");
const SINCE = "2026-07-05T12:06:00Z";

function item(kind: RunDecisionItem["kind"], over: Partial<RunSummaryItem> = {}): RunDecisionItem {
  const verb = { plan: "Review plan", question: "Answer", park: "Resume", pr: "Review PR" }[kind] as RunDecisionItem["verb"];
  return { kind, verb, since: SINCE, run: aRun({ id: `run-${kind}`, ...over }) as RunSummaryItem };
}

function mount(items: DecisionItem[], demo = false) {
  return render(
    <MemoryRouter>
      <NeedsDecisionCard items={items} now={NOW} demo={demo} />
    </MemoryRouter>,
  );
}

const prItem = (over: Partial<RunSummaryItem> = {}) =>
  item("pr", { status: "completed", mr_iid: 12, mr_state: "opened", ...over });

describe("NeedsDecisionCard", () => {
  it("renders nothing when empty", () => {
    const { container } = mount([]);
    expect(container.firstChild).toBeNull();
  });

  it("carries data-attention, the title and a summary", () => {
    const { container } = mount([item("plan"), item("question"), prItem()]);
    expect(container.querySelector("[data-attention]")).not.toBeNull();
    expect(screen.getByText("Needs your decision")).toBeTruthy();
    expect(screen.getByText("2 decisions, 1 PR review")).toBeTruthy();
  });

  it("shows each verb as a link to the run, with the age", () => {
    mount([item("plan"), item("question"), item("park")]);
    for (const [verb, kind] of [["Review plan", "plan"], ["Answer", "question"], ["Resume", "park"]]) {
      expect(screen.getByRole("link", { name: verb }).getAttribute("href")).toBe(`/runs/run-${kind}`);
    }
    expect(screen.getAllByText("waiting 14m")).toHaveLength(3);
  });

  it("rolls long waits into hours and days", () => {
    mount([
      { ...item("plan", { id: "d" }), since: "2026-06-07T12:20:00Z" },
      { ...item("question", { id: "h" }), since: "2026-07-05T07:20:00Z" },
    ]);
    expect(screen.getByText("waiting 28d")).toBeTruthy();
    expect(screen.getByText("waiting 5h")).toBeTruthy();
  });

  it("shows the cached PR state once per group, in the forge's noun when the group shares one", () => {
    mount([prItem({ id: "gh1", forge_type: "github" }), prItem({ id: "gh2", forge_type: "github", mr_iid: 13 })]);
    expect(screen.getAllByText("PR state as last seen by uzi")).toHaveLength(1);
    cleanup();
    mount([prItem({ id: "gl1", forge_type: "gitlab" }), prItem({ id: "gl2", forge_type: "gitlab", mr_iid: 13 })]);
    expect(screen.getAllByText("MR state as last seen by uzi")).toHaveLength(1);
    cleanup();
    mount([prItem({ id: "gh", forge_type: "github" }), prItem({ id: "gl", forge_type: "gitlab" }), prItem({ id: "fj", forge_type: "forgejo" })]);
    expect(screen.getAllByText("PR state as last seen by uzi")).toHaveLength(1);
    expect(screen.queryByText("MR state as last seen by uzi")).toBeNull();
  });

  it("compares forges by the noun shown: GitLab and a forge-less PR both read MR", () => {
    mount([prItem({ id: "gl", forge_type: "gitlab" }), prItem({ id: "nf", forge_type: undefined, mr_iid: 13 })]);
    expect(screen.getAllByText("MR state as last seen by uzi")).toHaveLength(1);
    cleanup();
    mount([prItem({ id: "gh", forge_type: "github" }), prItem({ id: "gl", forge_type: "gitlab", mr_iid: 13 })]);
    expect(screen.getAllByText("PR state as last seen by uzi")).toHaveLength(1);
    expect(screen.queryByText("MR state as last seen by uzi")).toBeNull();
  });

  it("does not show the cached-state text on a non-PR row", () => {
    mount([item("plan")]);
    expect(screen.queryByText(/state as last seen by uzi/)).toBeNull();
  });

  it("titles a single open PR as a one-item group", () => {
    mount([prItem()]);
    expect(screen.getByText("1 pull request is open for your review")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Review PRs" })).toBeTruthy();
  });

  describe("PR links inside the group", () => {
    const ref = () => screen.getByRole("link", { name: /^vtmocanu\/uzi [#!]12$/ });

    it("opens a valid https PR URL in a new tab", () => {
      mount([prItem({ forge_type: "github", mr_web_url: "https://github.com/o/r/pull/12" })]);
      const a = ref();
      expect(a.textContent).toBe("vtmocanu/uzi #12");
      expect(a.getAttribute("href")).toBe("https://github.com/o/r/pull/12");
      expect(a.getAttribute("target")).toBe("_blank");
      expect(a.getAttribute("rel")).toBe("noopener noreferrer");
      const note = document.getElementById(a.getAttribute("aria-describedby") ?? "");
      expect(note?.textContent).toBe("(opens in a new tab)");
    });

    it("rebuilds the PR URL from the issue URL when mr_web_url is absent", () => {
      mount([prItem({ mr_web_url: null, forge_type: "github", issue_web_url: "https://github.com/o/r/issues/7" })]);
      expect(ref().getAttribute("href")).toBe("https://github.com/o/r/pull/12");
    });

    it("rebuilds from the issue URL when mr_web_url is not https", () => {
      mount([prItem({ mr_web_url: "http://github.com/o/r/pull/12", forge_type: "github", issue_web_url: "https://github.com/o/r/issues/7" })]);
      expect(ref().getAttribute("href")).toBe("https://github.com/o/r/pull/12");
    });

    it("falls back to the run when no PR URL can be built", () => {
      mount([prItem({ mr_web_url: null, issue_web_url: null })]);
      const a = ref();
      expect(a.getAttribute("href")).toBe("/runs/run-pr");
      expect(a.getAttribute("target")).toBeNull();
    });

    it.each(["http://example.com/pull/1", "javascript:alert(1)", "data:text/html,x"])(
      "never uses %s as the PR href",
      (bad) => {
        mount([prItem({ mr_web_url: bad })]);
        const href = ref().getAttribute("href");
        expect(href).toBe("/runs/run-pr");
        expect(href).not.toBe(bad);
        fireEvent.click(screen.getByRole("button", { name: "Review PRs" }));
        expect(screen.getByRole("link", { name: "Review PR" }).getAttribute("href")).toBe("/runs/run-pr");
      },
    );

    it("masks the repo name in demo mode and shows it otherwise", () => {
      const over = { repo_path: "acme-corp/secret-repo", forge_type: "github" };
      const { container } = mount([prItem(over)], true);
      expect(screen.getByRole("link", { name: "demo/secret-repo #12" })).toBeTruthy();
      expect(screen.queryByText(/acme-corp/)).toBeNull();
      // The hover title is a render sink textContent cannot see.
      expect(container.querySelector("[data-pr-repo]")?.getAttribute("title")).toBe("demo/secret-repo");
      cleanup();
      const shown = mount([prItem(over)], false);
      expect(screen.getByRole("link", { name: "acme-corp/secret-repo #12" })).toBeTruthy();
      expect(shown.container.querySelector("[data-pr-repo]")?.getAttribute("title")).toBe("acme-corp/secret-repo");
    });

    it("omits the repo part when the run has no repo path", () => {
      mount([prItem({ repo_path: "", forge_type: "github" })]);
      expect(screen.getByRole("link", { name: "#12" })).toBeTruthy();
    });
  });

  // Binding (plan seq 98): every repo path is stripped of bidi/format characters, in demo mode
  // too (maskRepoPath keeps the last segment verbatim). Assembled at runtime.
  describe("repo paths with a bidi override", () => {
    const RLO = String.fromCharCode(0x202e);
    const path = "acme/re" + RLO + "po";

    for (const demo of [false, true]) {
      it(`strips it from a grouped PR item (demo=${demo})`, () => {
        const { container } = mount([prItem({ repo_path: path, forge_type: "github" })], demo);
        const name = demo ? "demo/repo #12" : "acme/repo #12";
        expect(screen.getByRole("link", { name }).textContent).toBe(name);
        expect(container.querySelector("[data-pr-repo]")?.getAttribute("title")).toBe(demo ? "demo/repo" : "acme/repo");
        fireEvent.click(screen.getByRole("button", { name: "Review PRs" }));
        expect(container.textContent).not.toContain(RLO);
        for (const el of container.querySelectorAll("[title]")) expect(el.getAttribute("title")).not.toContain(RLO);
      });

      it(`strips it from a single row's repo text (demo=${demo})`, () => {
        mount([item("plan", { repo_path: path, issue_iid: 7, issue_title: "Title" })], demo);
        const link = screen.getByRole("link", { name: "Review plan" });
        const title = document.getElementById(link.getAttribute("aria-describedby") ?? "") as HTMLElement;
        expect(title.textContent).toBe(`${demo ? "demo/repo" : "acme/repo"} #7 Title`);
        expect((link.closest("li") as HTMLElement).textContent).not.toContain(RLO);
      });
    }
  });

  it("strips unsafe characters from an untrusted title", () => {
    mount([item("plan", { issue_title: "Fix\u200b the \u202ebug" })]);
    const title = screen.getByText(/Fix/);
    expect(title.textContent).not.toMatch(/[\u200b\u202e]/);
  });

  it("leads a single row with the muted issue ref and names the plan's milestones", () => {
    const run = {
      issue_iid: 3881,
      issue_title: "Overview usage",
      status_since: "2026-07-05T12:06:00Z",
      milestones_candidate: [{}, {}, {}, {}] as never,
    };
    mount([item("plan", run)]);
    const link = screen.getByRole("link", { name: "Review plan" });
    const title = document.getElementById(link.getAttribute("aria-describedby") ?? "");
    expect(title?.textContent).toBe("vtmocanu/uzi #3881 Overview usage");
    const row = link.closest("li") as HTMLElement;
    expect(row.querySelector("[data-meta]")?.textContent).toBe("Plan ready, 4 milestones · waiting 14m");
  });

  it("masks a single row's repo in demo mode and omits it when the run has none", () => {
    mount([item("plan", { repo_path: "acme-corp/secret-repo", issue_iid: 7, issue_title: "T" })], true);
    expect(screen.getByText("demo/secret-repo")).toBeTruthy();
    expect(screen.queryByText(/acme-corp/)).toBeNull();
    cleanup();
    mount([item("plan", { repo_path: "", issue_iid: 7, issue_title: "T" })]);
    const link = screen.getByRole("link", { name: "Review plan" });
    expect(document.getElementById(link.getAttribute("aria-describedby") ?? "")?.textContent).toBe("#7 T");
  });

  it("describes each single row's action by its title, so repeated verbs are distinguishable", () => {
    mount([
      item("plan", { id: "p1", issue_title: "First plan" }),
      item("question", { id: "q1", issue_title: "First question" }),
    ]);
    const describedBy = (el: HTMLElement) =>
      (el.getAttribute("aria-describedby") ?? "")
        .split(" ")
        .map((id) => document.getElementById(id)?.textContent)
        .join(" ");
    expect(describedBy(screen.getByRole("link", { name: "Review plan" }))).toBe("vtmocanu/uzi #7 First plan");
    expect(describedBy(screen.getByRole("link", { name: "Answer" }))).toBe("vtmocanu/uzi #7 First question");
  });

  it("tells assistive tech that an expanded PR action opens a new tab without renaming it", () => {
    mount([prItem({ issue_title: "Ship it", mr_web_url: "https://github.com/o/r/pull/12" })]);
    fireEvent.click(screen.getByRole("button", { name: "Review PRs" }));
    const a = screen.getByRole("link", { name: "Review PR" });
    expect(a.getAttribute("href")).toBe("https://github.com/o/r/pull/12");
    expect(a.getAttribute("target")).toBe("_blank");
    const described = (a.getAttribute("aria-describedby") ?? "")
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent?.trim())
      .join(" ");
    expect(described).toBe("Ship it (opens in a new tab)");
  });

  describe("groups", () => {
    const rowsList = (container: HTMLElement) => container.querySelector("[data-decision-rows]") as HTMLElement;
    const prs = (n: number) =>
      Array.from({ length: n }, (_, i) =>
        ({
          ...prItem({ id: `pr-${i}`, mr_iid: 100 + i, forge_type: "github", issue_title: `PR title ${i}` }),
          since: new Date(NOW - (n - i) * 3_600_000).toISOString(),
        }) as RunDecisionItem,
      );
    const heldItem: HeldDecisionItem = {
      kind: "held",
      verb: "Review held work",
      count: 2,
      runId: null,
      since: "2026-07-05T09:20:00Z",
      label: null,
    };

    it("collapses 8 PRs and held work into two rows, held first, previewing three PRs", () => {
      const { container } = mount([heldItem, ...prs(8)]);
      const rows = Array.from(rowsList(container).children);
      expect(rows).toHaveLength(2);
      expect(rows[0].textContent).toContain("Held work needs 2 decisions");
      expect(within(rows[1] as HTMLElement).getByText("8 pull requests are open for your review")).toBeTruthy();
      expect(within(rows[1] as HTMLElement).getAllByRole("link", { name: /^vtmocanu\/uzi #\d+$/ })).toHaveLength(3);
      expect(screen.getByText("+5 more · oldest waiting 8h")).toBeTruthy();
      expect(screen.getAllByText("PR state as last seen by uzi")).toHaveLength(1);
    });

    it("expands and collapses from the keyboard on the native button", () => {
      // @testing-library/user-event is not a dependency: focus the native button and fire the
      // activation it performs (Enter and Space both click a <button>). The real-browser
      // keyboard path is covered in OverviewNext.browser.test.tsx.
      mount([heldItem, ...prs(8)]);
      const button = screen.getByRole("button", { name: "Review PRs" });
      button.focus();
      expect(document.activeElement).toBe(button);
      expect(button.getAttribute("aria-expanded")).toBe("false");
      const listId = button.getAttribute("aria-controls") ?? "";
      const list = document.getElementById(listId);
      expect(list?.tagName).toBe("UL");
      fireEvent.click(button);
      expect(button.getAttribute("aria-expanded")).toBe("true");
      expect(button.textContent).toBe("Show fewer");
      expect(within(list as HTMLElement).getAllByRole("link", { name: /^vtmocanu\/uzi #\d+$/ })).toHaveLength(8);
      expect(within(list as HTMLElement).getAllByRole("link", { name: "Review PR" })).toHaveLength(8);
      expect(screen.queryByText(/more · oldest waiting/)).toBeNull();
      expect(document.activeElement).toBe(button);
      fireEvent.click(button);
      expect(button.getAttribute("aria-expanded")).toBe("false");
      expect(button.textContent).toBe("Review PRs");
      expect(within(list as HTMLElement).getAllByRole("link", { name: /^vtmocanu\/uzi #\d+$/ })).toHaveLength(3);
    });

    it("groups four plans and two questions, each item keeping its own action when expanded", () => {
      const plans = [0, 1, 2, 3].map((i) => item("plan", { id: `plan-${i}`, issue_title: `Plan ${i}` }));
      const questions = [
        item("question", { id: "ask-1", status: "awaiting_input", issue_title: "Ask one" }),
        item("question", { id: "ask-2", status: "awaiting_followup", issue_title: "Ask two" }),
      ];
      const { container } = mount([...plans, ...questions]);
      expect(rowsList(container).children).toHaveLength(2);
      const planRow = screen.getByText("4 plans are ready for review").closest("li") as HTMLElement;
      expect(within(planRow).getByText(/^\+1 more · oldest waiting/)).toBeTruthy();
      expect(within(planRow).getByText("Plan 0")).toBeTruthy();
      expect(within(planRow).queryByText("Plan 3")).toBeNull();
      const askRow = screen.getByText("2 runs need your input").closest("li") as HTMLElement;

      fireEvent.click(within(planRow).getByRole("button", { name: "Review plans" }));
      const planLinks = within(planRow).getAllByRole("link", { name: "Review plan" });
      expect(planLinks.map((a) => a.getAttribute("href"))).toEqual([0, 1, 2, 3].map((i) => `/runs/plan-${i}`));
      expect(document.getElementById(planLinks[3].getAttribute("aria-describedby") ?? "")?.textContent).toBe("#7 Plan 3");
      // Each expanded item reads "ref title age Action" on one line; the age is announced as a wait.
      expect(within(planRow).getAllByText(/^(waiting )?14m$/).map((el) => el.textContent)).toEqual(
        Array(4).fill("waiting 14m"),
      );

      fireEvent.click(within(askRow).getByRole("button", { name: "Open runs" }));
      expect(within(askRow).getAllByRole("link", { name: "Answer" }).map((a) => a.getAttribute("href"))).toEqual([
        "/runs/ask-1",
        "/runs/ask-2",
      ]);
    });

    // Two columns on wide screens only for 2 to 4 rows.
    it.each([
      [1, [item("plan")], false],
      [2, [item("plan"), item("question")], true],
      [4, [heldItem, item("plan"), item("question"), item("park")], true],
      [5, [heldItem, item("plan"), item("question"), item("park"), prItem()], false],
    ] as const)("uses two columns at md for %i rows: %s", (n, items, twoCol) => {
      const { container } = mount([...items]);
      const list = rowsList(container);
      expect(list.children).toHaveLength(n);
      expect(list.classList.contains("md:grid-cols-2")).toBe(twoCol);
    });

    it("puts a group's toggle before the list it reveals in DOM order", () => {
      mount(prs(4));
      const button = screen.getByRole("button", { name: "Review PRs" });
      const list = document.getElementById(button.getAttribute("aria-controls") ?? "") as HTMLElement;
      expect(button.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("keeps a lone park as a normal row with no group button", () => {
      mount([item("park")]);
      expect(screen.getByRole("link", { name: "Resume" }).getAttribute("href")).toBe("/runs/run-park");
      expect(screen.queryByRole("button")).toBeNull();
    });

    it("hides every icon tile from assistive tech", () => {
      const { container } = mount([heldItem, item("plan"), item("question"), item("park"), ...prs(2)]);
      const tiles = container.querySelectorAll("[data-kind-tile]");
      expect(Array.from(tiles).map((t) => t.getAttribute("data-kind-tile"))).toEqual(["held", "plan", "question", "park", "pr"]);
      for (const t of tiles) expect(t.getAttribute("aria-hidden")).toBe("true");
    });

    it("points every group button's aria-controls at an element that exists", () => {
      mount([...prs(2), item("park", { id: "k1" }), item("park", { id: "k2" })]);
      const buttons = screen.getAllByRole("button");
      expect(buttons).toHaveLength(2);
      for (const b of buttons) {
        const id = b.getAttribute("aria-controls");
        expect(id).toBeTruthy();
        expect(document.getElementById(id as string)).not.toBeNull();
        fireEvent.click(b);
        expect(document.getElementById(id as string)).not.toBeNull();
      }
    });
  });

  describe("held work row", () => {
    const heldItem = (over: Partial<HeldDecisionItem> = {}): HeldDecisionItem => ({
      kind: "held",
      verb: "Review held work",
      count: 1,
      runId: "a83c51f0-6d2e-4b7a-9c14-0e5f8a1d3b27",
      since: "2026-07-05T10:20:00Z",
      label: "Decision required",
      ...over,
    });

    it("names a single decision's run by its short id, with the custody meta and age", () => {
      mount([heldItem()]);
      const title = screen.getByText(/Held work from run/);
      expect(title.textContent).toBe("Held work from run a83c51f0 needs a decision");
      expect(screen.getByText("a83c51f0").className).toContain("font-mono");
      expect(screen.getByText("Decision required")).toBeTruthy();
      expect(screen.queryByText("Custody")).toBeNull();
      expect(screen.getByText("waiting 2h")).toBeTruthy();
    });

    it("falls back to Custody when the hold has no label", () => {
      mount([heldItem({ label: null })]);
      expect(screen.getByText("Custody")).toBeTruthy();
    });

    it("reads as a count for several decisions and stays generic when the run is unknown", () => {
      mount([heldItem({ count: 3, runId: null })]);
      expect(screen.getByText("Held work needs 3 decisions")).toBeTruthy();
      expect(screen.getByText("Custody")).toBeTruthy();
      cleanup();
      mount([heldItem({ runId: null, since: null, label: null })]);
      expect(screen.getByText("Held work needs a decision")).toBeTruthy();
      expect(screen.queryByText(/waiting/)).toBeNull();
    });

    it("links to the Workers held-work panel and is described by its title", () => {
      mount([heldItem()]);
      const a = screen.getByRole("link", { name: "Review held work" });
      expect(a.getAttribute("href")).toBe("/workers?tab=workers#recovery-holds");
      const described = (a.getAttribute("aria-describedby") ?? "")
        .split(" ")
        .map((id) => document.getElementById(id)?.textContent)
        .join(" ");
      expect(described).toBe("Held work from run a83c51f0 needs a decision");
    });

    it("strips unsafe characters from the run id", () => {
      mount([heldItem({ runId: "ab\u200bcd\u202eef12345" })]);
      expect(screen.getByText(/Held work from run/).textContent).toBe("Held work from run abcdef12 needs a decision");
    });

    it("counts the aggregate's decisions in the summary", () => {
      mount([item("plan"), heldItem({ count: 2, runId: null }), prItem()]);
      expect(screen.getByText("3 decisions, 1 PR review")).toBeTruthy();
    });
  });
});
