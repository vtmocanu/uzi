// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OverviewRunRow } from "./OverviewRunRow";
import { aRun } from "../pages/overviewFixtures";

afterEach(cleanup);

function row(props: { owner?: string; demo: boolean }) {
  render(
    <MemoryRouter>
      <ul>
        <OverviewRunRow run={aRun({ issue_title: "Some title" })} now={Date.parse("2026-07-05T12:05:00Z")} {...props} />
      </ul>
    </MemoryRouter>,
  );
}

describe("OverviewRunRow owner", () => {
  it("shows no owner on the viewer's own rows", () => {
    row({ demo: false });
    expect(screen.queryByText(/@/)).toBeNull();
  });

  it("shows another user's email with the same value in its title", () => {
    row({ owner: "mira.lopez@uzi.local", demo: false });
    const owner = screen.getByText("mira.lopez@uzi.local");
    expect(owner.getAttribute("title")).toBe("mira.lopez@uzi.local");
  });

  it("strips zero-width and bidi-override characters from the owner label and its title", () => {
    row({ owner: "mi\u200bra@uz\u202ei.local", demo: false });
    const owner = screen.getByText("mira@uzi.local");
    expect(owner.textContent).not.toMatch(/[\u200b\u202e]/);
    expect(owner.getAttribute("title")).toBe("mira@uzi.local");
  });

  it("masks the owner email in the text and in the title attribute in demo mode", () => {
    row({ owner: "mira.lopez@uzi.local", demo: true });
    expect(document.body.textContent).not.toContain("mira.lopez");
    expect(document.body.textContent).not.toContain("@uzi.local");
    const titled = [...document.querySelectorAll("[title]")].map((el) => el.getAttribute("title") ?? "");
    expect(titled.some((t) => t.includes("mira.lopez") || t.includes("uzi.local"))).toBe(false);
    // The masked owner is still rendered, and its title carries the same masked text.
    const masked = document.querySelector("span[title].truncate");
    expect(masked?.textContent).toBeTruthy();
    expect(masked?.getAttribute("title")).toBe(masked?.textContent);
  });
});

// The row anatomy below was pinned on the retired old Overview page and now lives with the
// component that renders it, which the Overview renders.
const NOW = Date.parse("2026-07-05T12:05:00Z");

function renderRun(run: ReturnType<typeof aRun>, now = NOW) {
  const ui = (n: number) => (
    <MemoryRouter>
      <ul>
        <OverviewRunRow run={run} now={n} demo={false} />
      </ul>
    </MemoryRouter>
  );
  const view = render(ui(now));
  return { ...view, at: (n: number) => view.rerender(ui(n)) };
}

describe("OverviewRunRow title and badges", () => {
  it("strips bidi and zero-width characters out of the forge-supplied title (#124)", () => {
    const { container } = renderRun(aRun({ issue_title: "Fix the \u202Eparser\u200B bug", issue_iid: 7 }));
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("Fix the parser bug")).toBeTruthy();
    // The link's accessible name is a separate sink for the same untrusted title.
    expect(screen.getByRole("link").getAttribute("aria-label")).toBe("Open run for issue #7: Fix the parser bug");
  });

  it("shows a compact M{done}/{total} badge on a milestone-structured run (PRD #122)", () => {
    renderRun(
      aRun({
        issue_title: "Milestone run",
        milestones: ["a", "b", "c", "d", "e"].map((id) => ({ id, title: id.toUpperCase() })),
        milestones_completed: ["a", "b"],
      }),
    );
    expect(screen.getByText("M2/5")).toBeTruthy();
  });

  it("adds no milestone badge for a run with no milestones", () => {
    renderRun(aRun({ issue_title: "Plain run", milestones: null }));
    expect(screen.getByText("Plain run")).toBeTruthy();
    expect(screen.queryByText(/^M\d+\/\d+$/)).toBeNull();
  });

  it("opens a repo-less job row with its kind chip and shows no repo path (PRD #1908)", () => {
    const meta = (title: string) => screen.getByText(title).parentElement!.querySelector("p.flex")!;
    renderRun(aRun({ issue_title: "Issue row", issue_iid: 12 }));
    // Control: an issue row's meta line starts with its repo path.
    expect(meta("Issue row").textContent).toMatch(/^vtmocanu\/uzi /);
    cleanup();
    renderRun(
      aRun({ id: "run-job", kind: "job", repo_id: null, repo_path: "", forge_type: "", issue_iid: null, issue_title: "Summarise the incident timeline" }),
    );
    const jobMeta = meta("Summarise the incident timeline");
    expect(jobMeta.textContent).toMatch(/^job$/i);
    expect(jobMeta.textContent).not.toContain("vtmocanu/uzi");
    expect(screen.getByRole("link", { name: "Open run: Summarise the incident timeline" })).toBeTruthy();
  });
});

describe("OverviewRunRow live 'now' strip", () => {
  const activity = { agent: "coder", agent_label: "Wiring the collector", tool: "Write", detail: "api/collector.go", at: "2026-07-05T12:00:00Z", seq: 5 };

  it("shows the strip for a running run (positive control)", () => {
    renderRun(aRun({ issue_title: "Running now", status: "running", current_activity: activity }));
    expect(screen.getByText("Wiring the collector")).toBeTruthy();
    expect(screen.getByText("Running now").closest("li")?.querySelector(".bg-ok.animate-pulse")).toBeTruthy();
  });

  it.each(["paused", "limit_wait", "pool_wait", "recovery_wait"] as const)(
    "hides stale activity on %s even when current_activity is populated (#1197)",
    (status) => {
      renderRun(aRun({ issue_title: `Held ${status}`, status, current_activity: activity }));
      expect(screen.getByText(`Held ${status}`)).toBeTruthy();
      expect(screen.queryByText("Wiring the collector")).toBeNull();
      expect(screen.getByText(`Held ${status}`).closest("li")?.querySelector(".bg-ok.animate-pulse")).toBeNull();
    },
  );
});

describe("OverviewRunRow status progress", () => {
  const progress = (over: object) => ({ state: "percent", pct: 70, milestone_done: 2, milestone_total: 3, active_milestone_id: "c", phase: "", ...over }) as never;

  it("shows server progress inside the running status pill and keeps an ordinary pill without an estimate", () => {
    renderRun(
      aRun({
        issue_title: "Percent run",
        milestones: ["a", "b", "c"].map((id) => ({ id, title: id.toUpperCase() })),
        milestones_completed: ["a", "b"],
        progress: progress({}),
      }),
    );
    const row = screen.getByText("Percent run").closest("li")!;
    const bar = within(row).getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("70");
    expect(bar.getAttribute("aria-valuetext")).toBe("Running, about 70%; 2 of 3 milestones done");
    expect(within(row).getByText("M2/3")).toBeTruthy();
    expect(bar.textContent).toBe("running · 70%");
    expect(bar.closest(".hidden")).toBeNull();
    expect(row.querySelector('[data-run-progress="percent"]')).toBeNull();
    cleanup();
    renderRun(aRun({ issue_title: "Pre-feature run" }));
    expect(screen.getByText("Pre-feature run").closest("li")!.querySelector("[data-run-progress]")).toBeNull();
  });

  it("a stalled cell replaces the health badge from sm up; phones keep the badge", () => {
    renderRun(
      aRun({
        issue_title: "Stalled run",
        status: "running",
        health: "looping",
        health_since: "2026-07-05T11:41:00Z",
        progress: progress({ state: "stalled", pct: null, milestone_done: 1, active_milestone_id: "b" }),
      }),
    );
    const row = screen.getByText("Stalled run").closest("li")!;
    const cell = row.querySelector('[data-run-progress="stalled"]')!;
    expect(cell.parentElement?.className).toBe("hidden sm:contents");
    expect(cell.textContent).toContain("(looping)");
    expect(within(row).getByText(/^⚠ looping/).parentElement?.className).toBe("contents sm:hidden");
  });

  it("ages the stalled cell on the now prop it is handed, not on a private Date.now()", () => {
    const T0 = Date.parse("2026-07-05T12:00:00Z");
    const view = renderRun(
      aRun({
        issue_title: "Stalled run",
        status: "running",
        health: "stalled",
        health_since: new Date(T0 - 5 * 60_000).toISOString(),
        progress: progress({ state: "stalled", pct: null, milestone_done: 1, active_milestone_id: "b" }),
      }),
      T0,
    );
    const visible = () =>
      screen.getByText("Stalled run").closest("li")!.querySelector('[data-run-progress="stalled"] [aria-hidden="true"]:not(:first-child)')!.textContent;
    expect(visible()).toBe(" \u00b7 5m");
    view.at(T0 + 10 * 60_000);
    expect(visible()).toBe(" \u00b7 15m");
  });
});

describe("OverviewRunRow account cooldown", () => {
  const T = Date.parse("2026-10-28T12:00:00Z");
  const retained = { agent: "coder", agent_label: "Coder", tool: "Bash", detail: "retained cooldown activity", at: "2026-10-28T12:00:00Z", seq: 1 };
  const waiting = { state: "waiting", pct: null, milestone_done: 0, milestone_total: 0, active_milestone_id: "", phase: "" } as never;

  it("shows one visible owner wait while the cooldown holds, and goes quiet once it clears", () => {
    const run = aRun({ health: "waiting_account_cooldown", account_cooldown_class: "rate_limit", account_cooldown_eligible_at: "2026-10-28T12:01:37Z", progress: waiting });
    renderRun(run, T);
    const row = () => screen.getByText("Live run row").closest("li")!;
    const waits = () => [...row().querySelectorAll("*")].filter((el) => !el.childElementCount && !el.closest(".sr-only") && el.textContent?.includes("waiting for account cooldown"));
    expect(waits()).toHaveLength(1);
    expect(row().textContent).not.toMatch(/waits on you|⚠|%/);
    cleanup();
    renderRun({ ...run, health: "ok", account_cooldown_class: undefined, account_cooldown_eligible_at: undefined }, T);
    expect(waits()).toHaveLength(0);
  });

  it.each(["running", "awaiting_approval"] as const)("keeps a %s row quiet with expired cooldown metadata", (status) => {
    renderRun(aRun({ status, health: "waiting_account_cooldown", account_cooldown_class: "rate_limit", account_cooldown_eligible_at: "2026-10-28T11:59:59Z", current_activity: retained }), T);
    const row = screen.getByText("Live run row").closest("li")!;
    expect(row.textContent).toContain("waiting");
    expect(row.textContent).not.toMatch(/waiting for account cooldown|Coder/);
    expect(row.querySelector(".animate-pulse")).toBeNull();
  });

  it.each([
    ["class missing", { account_cooldown_class: undefined }],
    ["eligibility missing", { account_cooldown_eligible_at: undefined }],
  ] as const)("keeps a running row quiet when the cooldown %s", (_label, missing) => {
    renderRun(aRun({ status: "running", health: "waiting_account_cooldown", account_cooldown_class: "rate_limit", account_cooldown_eligible_at: "2026-10-28T12:01:37Z", current_activity: retained, ...missing }), T);
    const row = screen.getByText("Live run row").closest("li")!;
    expect(row.textContent).toContain("waiting");
    expect(row.textContent).not.toMatch(/waiting for account cooldown|Coder/);
    expect(row.querySelector(".animate-pulse")).toBeNull();
  });

  it.each([false, true])("suppresses retained activity under a cooldown (revising=%s)", (is_revising) => {
    renderRun(aRun({ status: "awaiting_approval", is_revising, health: "waiting_account_cooldown", account_cooldown_class: "rate_limit", account_cooldown_eligible_at: "2099-01-01T00:00:37Z", current_activity: retained }), T);
    expect(screen.queryByText("Coder")).toBeNull();
  });
});
