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
});
