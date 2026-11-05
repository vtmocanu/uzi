// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunSummaryItem } from "../lib/api";
import type { AdminRunsState } from "../lib/useOverviewData";
import { OverviewRunsWidget } from "./OverviewRunsWidget";
import { aRun } from "../pages/overviewFixtures";

afterEach(cleanup);

const NOW = Date.parse("2026-07-05T12:20:00Z");
const ME = "admin@uzi.local";

const run = (over: Partial<RunSummaryItem> = {}) => aRun(over) as RunSummaryItem;
const theirs = (over: Partial<RunSummaryItem> = {}) => run({ owner_email: "mira@uzi.local", ...over });
const good = (runs: RunSummaryItem[], lastOkAt: number | null = NOW): AdminRunsState => ({ runs, failed: false, lastOkAt });

function mount(props: Partial<React.ComponentProps<typeof OverviewRunsWidget>> = {}) {
  return render(
    <MemoryRouter>
      <OverviewRunsWidget
        ownRuns={[]}
        adminRuns={null}
        isAdmin={false}
        viewerEmail={ME}
        now={NOW}
        demo={false}
        loading={false}
        {...props}
      />
    </MemoryRouter>,
  );
}

describe("OverviewRunsWidget", () => {
  it("titles the card and links to all runs", () => {
    mount();
    expect(screen.getByRole("heading", { name: "Runs" })).toBeTruthy();
    expect(screen.getByRole("link", { name: /All runs/ }).getAttribute("href")).toBe("/runs");
  });

  it("shows only skeletons while loading", () => {
    const { container } = mount({ loading: true, ownRuns: [run({ issue_title: "Hidden while loading" })] });
    expect(screen.queryByText("Hidden while loading")).toBeNull();
    expect(screen.queryByText(/No active runs/)).toBeNull();
    expect(container.querySelectorAll(".animate-pulse").length).toBeGreaterThan(0);
  });

  it("shows an admin both groups, and excludes terminal runs and the admin's own rows from Other users", () => {
    mount({
      isAdmin: true,
      ownRuns: [run({ id: "mine", issue_title: "My active" }), run({ id: "mine-done", status: "completed", issue_title: "My done" })],
      adminRuns: good([
        run({ id: "mine", owner_email: ME, issue_title: "My active" }),
        theirs({ id: "t1", issue_title: "Their active" }),
        theirs({ id: "t2", status: "failed", issue_title: "Their failed" }),
      ]),
    });
    expect(screen.getByText("Yours")).toBeTruthy();
    expect(screen.getByText("Other users (admin)")).toBeTruthy();
    const mine = within(screen.getByRole("list", { name: "Your runs" }));
    expect(mine.getByText("My active")).toBeTruthy();
    expect(mine.queryByText("My done")).toBeNull();
    const others = within(screen.getByRole("list", { name: "Other users' runs" }));
    expect(others.getByText("Their active")).toBeTruthy();
    expect(others.queryByText("Their failed")).toBeNull();
    expect(others.queryByText("My active")).toBeNull();
    expect(others.getByText("mira@uzi.local")).toBeTruthy();
  });

  it("lists another user's awaiting_approval run only in Other users, with no action", () => {
    mount({
      isAdmin: true,
      adminRuns: good([theirs({
          id: "t1",
          status: "awaiting_approval",
          issue_title: "Their plan",
          progress: { state: "waiting", pct: null, milestone_done: 0, milestone_total: 0, active_milestone_id: "", phase: "" },
        })]),
    });
    const others = within(screen.getByRole("list", { name: "Other users' runs" }));
    expect(others.getByText("Their plan")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Review plan" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Review plan" })).toBeNull();
    // The only link in the group is the stretched run overlay.
    expect(others.getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual(["/runs/t1"]);
    // The row says the run waits on its owner, never on the viewer.
    expect(others.getByText(/waits on owner/)).toBeTruthy();
    expect(others.queryByText(/waits on you/)).toBeNull();
  });

  it("shows a non-admin only their own group", () => {
    mount({
      ownRuns: [run({ issue_title: "Mine" })],
      // Even if rows were passed, a non-admin never renders them.
      adminRuns: good([theirs({ issue_title: "Theirs" })]),
    });
    expect(screen.getByText("Mine")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Yours" })).toBeTruthy();
    expect(screen.queryByText("Theirs")).toBeNull();
    expect(screen.queryByText(/Other users/)).toBeNull();
  });

  it("hides the Other users group when it is empty and the read is healthy", () => {
    mount({ isAdmin: true, ownRuns: [run()], adminRuns: good([theirs({ status: "completed" })]) });
    expect(screen.queryByText("Other users (admin)")).toBeNull();
    expect(screen.queryByText(/unavailable/)).toBeNull();
  });

  it("shows the empty state for Yours", () => {
    mount({ ownRuns: [run({ status: "completed" })] });
    expect(screen.getByText("No active runs. Start one from a board card.")).toBeTruthy();
  });

  it("says other users' runs are unavailable, with the last good read, instead of stale rows", () => {
    mount({
      isAdmin: true,
      ownRuns: [run()],
      adminRuns: { runs: [theirs({ issue_title: "Stale row" })], failed: true, lastOkAt: NOW - 5 * 60_000 },
    });
    const heading = screen.getByText("Other users (admin)");
    expect(heading.className).toContain("text-brand");
    // Only the static sentence is the live region; the ticking read time sits outside it.
    expect(screen.getByRole("status").textContent).toBe("Other users' runs are unavailable");
    expect(screen.getByRole("status").parentElement?.textContent).toBe("Other users' runs are unavailable (last read 5m ago)");
    expect(screen.queryByText("Stale row")).toBeNull();
    expect(screen.queryByRole("list", { name: "Other users' runs" })).toBeNull();
  });

  it("omits the read time when no read ever succeeded", () => {
    mount({ isAdmin: true, adminRuns: { runs: null, failed: true, lastOkAt: null } });
    expect(screen.getByRole("status").textContent).toBe("Other users' runs are unavailable");
  });

  it("masks the owner email on the text and the title attribute in demo mode", () => {
    const raw = "mira.kovacs@uzi.local";
    mount({
      isAdmin: true,
      demo: true,
      adminRuns: good([theirs({ owner_email: raw })]),
    });
    const label = screen.getByText("Mira");
    expect(label.getAttribute("title")).toBe("Mira");
    expect(label.getAttribute("title")).not.toBe(raw);
    expect(screen.queryByText(raw)).toBeNull();
    expect(document.body.innerHTML).not.toContain(raw);
  });
});
