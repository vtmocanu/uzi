// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunSummaryItem } from "../lib/api";
import type { DecisionItem } from "../lib/overview";
import { NeedsDecisionCard } from "./NeedsDecisionCard";
import { aRun } from "../pages/overviewFixtures";

afterEach(cleanup);

const NOW = Date.parse("2026-07-05T12:20:00Z");
const SINCE = "2026-07-05T12:06:00Z";

function item(kind: DecisionItem["kind"], over: Partial<RunSummaryItem> = {}): DecisionItem {
  const verb = { plan: "Review plan", question: "Answer", park: "Resume", pr: "Review PR" }[kind] as DecisionItem["verb"];
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
    const at = (iso: string) => ({ ...item("plan"), since: iso });
    mount([
      { ...at("2026-06-07T12:20:00Z"), run: aRun({ id: "d" }) as RunSummaryItem },
      { ...at("2026-07-05T07:20:00Z"), run: aRun({ id: "h" }) as RunSummaryItem },
    ]);
    expect(screen.getByText("waiting 28d")).toBeTruthy();
    expect(screen.getByText("waiting 5h")).toBeTruthy();
  });

  it("labels the cached PR state per forge", () => {
    mount([prItem({ id: "gh", forge_type: "github" }), prItem({ id: "gl", forge_type: "gitlab" }), prItem({ id: "fj", forge_type: "forgejo" })]);
    expect(screen.getAllByText("PR state as last seen by uzi")).toHaveLength(2);
    expect(screen.getAllByText("MR state as last seen by uzi")).toHaveLength(1);
  });

  it("does not show the cached-state text on a non-PR row", () => {
    mount([item("plan")]);
    expect(screen.queryByText(/state as last seen by uzi/)).toBeNull();
  });

  it("opens a valid https PR URL in a new tab", () => {
    mount([prItem({ mr_web_url: "https://github.com/o/r/pull/12" })]);
    const a = screen.getByRole("link", { name: "Review PR" });
    expect(a.getAttribute("href")).toBe("https://github.com/o/r/pull/12");
    expect(a.getAttribute("target")).toBe("_blank");
    expect(a.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("rebuilds the PR URL from the issue URL when mr_web_url is absent", () => {
    mount([prItem({ mr_web_url: null, forge_type: "github", issue_web_url: "https://github.com/o/r/issues/7" })]);
    expect(screen.getByRole("link", { name: "Review PR" }).getAttribute("href")).toBe("https://github.com/o/r/pull/12");
  });

  it("falls back to the run when no PR URL can be built", () => {
    mount([prItem({ mr_web_url: null, issue_web_url: null })]);
    const a = screen.getByRole("link", { name: "Review PR" });
    expect(a.getAttribute("href")).toBe("/runs/run-pr");
    expect(a.getAttribute("target")).toBeNull();
  });

  it.each(["http://example.com/pull/1", "javascript:alert(1)", "data:text/html,x"])(
    "never uses %s as the PR href",
    (bad) => {
      mount([prItem({ mr_web_url: bad })]);
      const href = screen.getByRole("link", { name: "Review PR" }).getAttribute("href");
      expect(href).toBe("/runs/run-pr");
      expect(href).not.toBe(bad);
    },
  );

  it("strips unsafe characters from an untrusted title", () => {
    mount([item("plan", { issue_title: "Fix​ the ‮bug" })]);
    const title = screen.getByText(/Fix/);
    expect(title.textContent).not.toMatch(/[​‮]/);
  });

  it("describes each action link by its row title, so repeated verbs are distinguishable", () => {
    mount([
      item("plan", { id: "p1", issue_title: "First plan" }),
      item("plan", { id: "p2", issue_title: "Second plan" }),
    ]);
    const links = screen.getAllByRole("link", { name: "Review plan" });
    expect(links).toHaveLength(2);
    const describedBy = (el: HTMLElement) =>
      (el.getAttribute("aria-describedby") ?? "")
        .split(" ")
        .map((id) => document.getElementById(id)?.textContent)
        .join(" ");
    expect(describedBy(links[0])).toBe("First plan");
    expect(describedBy(links[1])).toBe("Second plan");
  });

  it("tells assistive tech that the PR link opens a new tab without renaming it", () => {
    mount([prItem({ issue_title: "Ship it", mr_web_url: "https://github.com/o/r/pull/12" })]);
    const a = screen.getByRole("link", { name: "Review PR" });
    const described = (a.getAttribute("aria-describedby") ?? "")
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent?.trim())
      .join(" ");
    expect(described).toBe("Ship it (opens in a new tab)");
  });

  it("masks the repo path in demo mode and shows it otherwise", () => {
    const run = { repo_path: "acme-corp/secret-repo" };
    mount([item("plan", run)], true);
    expect(screen.getByText(/demo\/secret-repo/)).toBeTruthy();
    expect(screen.queryByText(/acme-corp/)).toBeNull();
    cleanup();
    mount([item("plan", run)], false);
    expect(screen.getByText(/acme-corp\/secret-repo/)).toBeTruthy();
  });
});
