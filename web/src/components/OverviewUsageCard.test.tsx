// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { useState } from "react";
import { OverviewUsageCard } from "./OverviewUsageCard";
import type { UsageWindow } from "./UsageCards";
import type { AdminUsage, RunOutcomes, SelfUsage } from "../lib/api";
import { setDemoMode } from "../lib/demoMode";

const bundle = (input: number, cache: number, output: number, cost: number) => ({
  input_tokens: input, cache_read_tokens: cache, cache_creation_tokens: 0,
  output_tokens: output, cost_usd: cost, cost_status: "metered" as const,
});
const outcomes = (overrides: Partial<RunOutcomes> = {}): RunOutcomes => ({
  finished: 100, completed: 85, cancelled: 3, plan_rejected: 2, base_diverged: 0, failed: 10,
  needs_landing: 4, fail_origins: { agent_failure: 6, run_timeout: 2, workflow_scope_missing: 2 },
  last_failed_at: "2026-10-07T06:00:00Z", last_failed_run_id: "self-failure",
  last_failed_origin: "agent_failure", last_failed_user_id: null,
  completed_since_last_failure: 14, ...overrides,
});
function fixture(): { self: SelfUsage; admin: AdminUsage } {
  const self: SelfUsage = {
    lifetime: bundle(1000, 2000, 500, 1234.56), last_7_days: bundle(100, 200, 50, 12.34),
    run_count: 23, lifetime_subscription_run_count: 8, lifetime_unreported_run_count: 2,
    last7_subscription_run_count: 1, last7_unreported_run_count: 0,
    lifetime_pr_count: 50, last7_pr_count: 2,
    outcomes: {
      lifetime: outcomes(),
      last_7_days: outcomes({ finished: 20, completed: 17, cancelled: 1, plan_rejected: 0, base_diverged: 0,
        failed: 2, needs_landing: 1, fail_origins: { agent_failure: 1, run_timeout: 1 },
        last_failed_at: null, last_failed_run_id: null, last_failed_origin: null,
        completed_since_last_failure: null }),
    },
  };
  const admin: AdminUsage = {
    factory: {
      ...self, lifetime: bundle(2000, 4000, 1000, 2469.12),
      last_7_days: bundle(300, 600, 150, 37.02), run_count: 46,
      lifetime_pr_count: 100, last7_pr_count: 6,
      outcomes: {
        lifetime: outcomes({ finished: 200, completed: 170, cancelled: 6, plan_rejected: 4, base_diverged: 0,
          failed: 20, needs_landing: 8, fail_origins: { agent_failure: 12, run_timeout: 4, workflow_scope_missing: 4 },
          last_failed_at: "2026-10-07T10:00:00Z", last_failed_run_id: "factory-failure", last_failed_user_id: "owner" }),
        last_7_days: { ...self.outcomes.last_7_days, finished: 40, completed: 34, cancelled: 2,
          failed: 4, needs_landing: 2, fail_origins: { agent_failure: 2, run_timeout: 2 } },
      },
    },
    users: ["owner", "other"].map((id) => ({
      user_id: id, email: `${id}.person@example.com`, usage: self.lifetime,
      run_count: 23, outcomes: self.outcomes.lifetime, subscription_run_count: 4, unreported_run_count: 1,
      last_7_days: self.last_7_days, last7_run_count: 3, last7_outcomes: self.outcomes.last_7_days,
      last7_subscription_run_count: 0, last7_unreported_run_count: 0,
    })),
    earliest_run: "2026-05-12T09:00:00Z",
  };
  return { self, admin };
}

function Harness({ self, admin, initial = "lifetime", onChange }: {
  self: SelfUsage; admin?: AdminUsage; initial?: UsageWindow; onChange?: (w: UsageWindow) => void;
}) {
  const [window, setWindow] = useState(initial);
  return (
    <OverviewUsageCard
      self={self} admin={admin} window={window} viewerId="owner"
      onWindowChange={(w) => { onChange?.(w); setWindow(w); }}
    />
  );
}
const mount = (props: Parameters<typeof Harness>[0]) => render(<MemoryRouter><Harness {...props} /></MemoryRouter>);
beforeEach(() => { localStorage.clear(); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); setDemoMode(false); });

describe("OverviewUsageCard (#3881 M1)", () => {
  it("draws both usage toggles at one weight, like Issue work's, so a pick never shifts them (#4052)", () => {
    const { self, admin } = fixture();
    const view = mount({ self, admin });
    const weights = () =>
      ["Usage scope", "Usage reporting period"].flatMap((name) =>
        within(view.getByRole("group", { name })).getAllByRole("button").map((b) => ({
          medium: b.classList.contains("font-medium"),
          semibold: b.classList.contains("font-semibold"),
        })),
      );
    const steady = Array(4).fill({ medium: true, semibold: false });
    expect(weights()).toEqual(steady);
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(weights()).toEqual(steady);
  });

  it("shows the four figures for All time with the cost disclosure and 'at least' per-PR cost sharing its title", () => {
    const { self } = fixture();
    const view = mount({ self });
    const region = within(view.getByRole("region", { name: "Your usage" }));
    for (const label of ["Cost (API-equivalent)", "Pull requests opened", "Failed runs rate", "Tokens"]) {
      expect(region.getByRole("heading", { name: label })).toBeTruthy();
    }
    expect(region.getByText("$1,235")).toBeTruthy();
    expect(region.getByText("50")).toBeTruthy();
    expect(region.getByText("10.0%")).toBeTruthy();
    expect(region.getByText("3.5k")).toBeTruthy();
    expect(region.getByText("in 1.0k, cached 2.0k, out 500")).toBeTruthy();
    const disclosure = region.getByText(/8 runs without a cost estimate/);
    const perPr = region.getByText("at least $24.69 per PR");
    expect(disclosure.getAttribute("title")).toBeTruthy();
    expect(perPr.getAttribute("title")).toBe(disclosure.getAttribute("title"));
    expect(region.queryByText(/estimated at API list prices/)).toBeNull();
  });

  it("shows the Last 7 days figures and the change against all time", () => {
    const { self } = fixture();
    const view = mount({ self, initial: "last_7_days" });
    const region = within(view.getByRole("region", { name: "Your usage" }));
    expect(region.getByText("$12.34")).toBeTruthy();
    const prs = within(region.getByRole("heading", { name: "Pull requests opened" }).parentElement!);
    expect(prs.getByText("2")).toBeTruthy();
    expect(prs.getByText("at least $6.17 per PR")).toBeTruthy();
    expect(region.getByText("350")).toBeTruthy();
    expect(region.getByText("0.0 pp vs all-time 10.0%")).toBeTruthy();
  });

  it("reads 'estimated at API list prices' and a plain per-PR cost when cost is complete", () => {
    const { self } = fixture();
    self.lifetime_subscription_run_count = 0;
    self.lifetime_unreported_run_count = 0;
    const view = mount({ self });
    expect(view.getByText("estimated at API list prices")).toBeTruthy();
    expect(view.getByText("$24.69 per PR")).toBeTruthy();
  });

  it("reads '— per PR' at zero PRs and '—' with no sub-line for an absent count", () => {
    const { self } = fixture();
    self.lifetime_pr_count = 0;
    const view = mount({ self });
    expect(view.getByText("— per PR")).toBeTruthy();
    cleanup();
    const older = fixture().self;
    delete older.lifetime_pr_count;
    const second = mount({ self: older });
    const kpi = second.getByRole("heading", { name: "Pull requests opened" }).parentElement!;
    expect(within(kpi).getByText("—")).toBeTruthy();
    expect(kpi.textContent).not.toMatch(/per PR/);
  });

  it("labels the outcome bar with rejected = plan_rejected + base_diverged", () => {
    const { self } = fixture();
    self.outcomes.lifetime = outcomes({ plan_rejected: 1, base_diverged: 2 });
    const view = mount({ self });
    expect(view.getByRole("img", { name: "Finished runs: 85 completed, 3 cancelled, 3 rejected, 10 failed" })).toBeTruthy();
  });

  it("says no runs finished for empty outcomes", () => {
    const { self } = fixture();
    self.outcomes.lifetime = outcomes({ finished: 0, completed: 0, cancelled: 0, plan_rejected: 0, failed: 0, fail_origins: {} });
    const view = mount({ self });
    expect(view.getByText("No finished runs in this period.")).toBeTruthy();
  });

  it("lists the top three causes and sums the rest into 'other causes'", () => {
    const { self } = fixture();
    self.outcomes.lifetime = outcomes({
      failed: 20, fail_origins: { agent_failure: 8, run_timeout: 5, workflow_scope_missing: 3, push_secret_blocked: 2, unknown: 2 },
    });
    const view = mount({ self });
    const list = within(view.getByRole("heading", { name: "Top causes" }).parentElement!);
    const rows = list.getAllByRole("listitem");
    expect(rows).toHaveLength(4);
    expect(rows[0].textContent).toContain("8");
    expect(rows[3].textContent).toBe("other causes4");
    expect(within(rows[3]).getByRole("link", { name: "other causes" }).getAttribute("href")).toBe("/runs/history?status=failed");
  });

  it("shows the latest failure (all time) with its run link", () => {
    const { self } = fixture();
    const view = mount({ self, initial: "last_7_days" });
    const text = view.getByText(/Latest failure \(all time\)/);
    expect(text.textContent).toMatch(/ago$/);
    expect(view.getByRole("link", { name: /run self-fai/ }).getAttribute("href")).toBe("/runs/self-failure");
  });

  it("reports window changes", () => {
    const { self } = fixture();
    const onChange = vi.fn();
    const view = mount({ self, onChange });
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(onChange).toHaveBeenCalledWith("last_7_days");
  });

  it("renders no scope toggle without admin data", () => {
    const view = mount({ self: fixture().self });
    expect(view.queryByRole("group", { name: "Usage scope" })).toBeNull();
    expect(view.getByRole("group", { name: "Usage reporting period" })).toBeTruthy();
  });

  it("switches every figure to the factory values from the scope toggle", () => {
    const { self, admin } = fixture();
    const view = mount({ self, admin });
    const scope = within(view.getByRole("group", { name: "Usage scope" }));
    expect(scope.getByRole("button", { name: "You" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(scope.getByRole("button", { name: "All users" }));
    expect(view.queryByRole("region", { name: "Your usage" })).toBeNull();
    const region = within(view.getByRole("region", { name: "Factory usage" }));
    expect(region.getByText("$2,469")).toBeTruthy();
    expect(region.getByText("100")).toBeTruthy();
    expect(region.getByText("at least $24.69 per PR")).toBeTruthy();
    expect(region.getByText("10.0%")).toBeTruthy();
    expect(region.getByText(/^across every user since /)).toBeTruthy();
    expect(region.getByText("7.0k")).toBeTruthy();
    expect(region.getByRole("img", { name: "Finished runs: 170 completed, 6 cancelled, 4 rejected, 20 failed" })).toBeTruthy();
    expect(region.getByText(/owner\.person@example\.com/)).toBeTruthy();
    expect(region.getByRole("link", { name: /run factory-/ }).getAttribute("href")).toBe("/runs/factory-failure");
    expect(region.queryByRole("link", { name: "Your failed runs →" })).toBeNull();
  });

  it("remembers the scope choice per browser across a remount", () => {
    const { self, admin } = fixture();
    const first = mount({ self, admin });
    fireEvent.click(first.getByRole("button", { name: "All users" }));
    expect(JSON.parse(localStorage.getItem("uzi.overviewUsageScope")!)).toBe("all");
    cleanup();
    const second = mount({ self, admin });
    expect(second.getByRole("button", { name: "All users" }).getAttribute("aria-pressed")).toBe("true");
    expect(second.getByRole("region", { name: "Factory usage" })).toBeTruthy();
    fireEvent.click(second.getByRole("button", { name: "You" }));
    cleanup();
    expect(mount({ self, admin }).getByRole("region", { name: "Your usage" })).toBeTruthy();
  });

  it("reads an unknown stored value as You", () => {
    const { self, admin } = fixture();
    localStorage.setItem("uzi.overviewUsageScope", JSON.stringify("factory"));
    expect(mount({ self, admin }).getByRole("region", { name: "Your usage" })).toBeTruthy();
  });

  it("reads as You when storage throws", () => {
    const { self, admin } = fixture();
    // A saved "all" first, so only a handled read error can yield You.
    localStorage.setItem("uzi.overviewUsageScope", JSON.stringify("all"));
    vi.spyOn(localStorage, "getItem").mockImplementation(() => { throw new Error("denied"); });
    expect(mount({ self, admin }).getByRole("region", { name: "Your usage" })).toBeTruthy();
  });

  it("never writes the scope for a non-admin, even with a saved all", () => {
    const { self } = fixture();
    localStorage.setItem("uzi.overviewUsageScope", JSON.stringify("all"));
    const setItem = vi.spyOn(localStorage, "setItem");
    const view = mount({ self });
    expect(view.getByRole("region", { name: "Your usage" })).toBeTruthy();
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(setItem).not.toHaveBeenCalledWith("uzi.overviewUsageScope", expect.anything());
    // Positive control: the same spy sees an admin's write, so the negative above is not vacuous.
    cleanup();
    const admin = mount({ self, admin: fixture().admin });
    fireEvent.click(admin.getByRole("button", { name: "All users" }));
    expect(setItem).toHaveBeenCalledWith("uzi.overviewUsageScope", JSON.stringify("all"));
  });

  it("does not render the dropped side-by-side or run-count pieces", () => {
    const { self, admin } = fixture();
    const view = mount({ self, admin });
    expect(view.queryByText(/runs with token usage/)).toBeNull();
    expect(view.getAllByRole("region", { name: /^(Your|Factory) usage$/ })).toHaveLength(1);
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    expect(view.queryByText(/runs with token usage/)).toBeNull();
    expect(view.getAllByRole("region", { name: /^(Your|Factory) usage$/ })).toHaveLength(1);
  });

  it("renders no chart: no canvas or svg, and the outcome bar is the only image", () => {
    const { self, admin } = fixture();
    const view = mount({ self, admin });
    const noChart = () => {
      expect(view.container.querySelector("canvas, svg")).toBeNull();
      expect(view.getAllByRole("img").map((i) => i.getAttribute("aria-label")))
        .toEqual([expect.stringMatching(/^Finished runs: /)]);
    };
    noChart();
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    noChart();
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    noChart();
    fireEvent.click(view.getByRole("button", { name: "You" }));
    noChart();
  });
  it("shows the recoverable-work note from needs_landing", () => {
    const view = mount({ self: fixture().self });
    expect(view.getByText("4 failed runs with recoverable work. May include work already landed.")).toBeTruthy();
    cleanup();
    const none = fixture().self;
    none.outcomes.lifetime = outcomes({ needs_landing: 0 });
    expect(mount({ self: none }).queryByText(/recoverable work/)).toBeNull();
  });

  it("masks the failure owner's email in demo mode in the Factory scope", () => {
    const { self, admin } = fixture();
    const view = mount({ self, admin });
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    const region = within(view.getByRole("region", { name: "Factory usage" }));
    expect(region.getByTitle("owner.person@example.com")).toBeTruthy();
    act(() => setDemoMode(true));
    expect(region.queryByText(/owner\.person@example\.com/)).toBeNull();
    expect(region.queryByTitle("owner.person@example.com")).toBeNull();
    expect(region.getByTitle("Owner").textContent).toBe("Owner");
  });

  it("signs the 7-day change against all time in both scopes", () => {
    const { self, admin } = fixture();
    self.outcomes.last_7_days = { ...self.outcomes.last_7_days, finished: 20, failed: 1 };
    admin.factory.outcomes.last_7_days = { ...admin.factory.outcomes.last_7_days, finished: 40, failed: 2 };
    const view = mount({ self, admin, initial: "last_7_days" });
    expect(view.getByText("−5.0 pp vs all-time 10.0%")).toBeTruthy();
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    expect(view.getByText("−5.0 pp vs all-time 10.0%")).toBeTruthy();
    cleanup();
    const worse = fixture().self;
    worse.outcomes.last_7_days = { ...worse.outcomes.last_7_days, finished: 20, failed: 4 };
    expect(mount({ self: worse, initial: "last_7_days" }).getByText("+10.0 pp vs all-time 10.0%")).toBeTruthy();
  });

  it("shows a muted empty state in the causes list when nothing failed", () => {
    const { self } = fixture();
    self.outcomes.lifetime = outcomes({ failed: 0, fail_origins: {} });
    const view = mount({ self });
    expect(view.getByRole("heading", { name: "Top causes" })).toBeTruthy();
    expect(view.getByText("No failures in this period.")).toBeTruthy();
  });

  it("renders 'other causes' as plain text in the Factory scope", () => {
    const { self, admin } = fixture();
    admin.factory.outcomes.lifetime = {
      ...admin.factory.outcomes.lifetime,
      fail_origins: { agent_failure: 8, run_timeout: 5, workflow_scope_missing: 3, push_secret_blocked: 2, unknown: 2 },
    };
    const view = mount({ self, admin });
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    const row = view.getByText("other causes").closest("li")!;
    expect(row.textContent).toBe("other causes4");
    expect(within(row).queryByRole("link")).toBeNull();
  });

  it("says the latest failure is unavailable when its time cannot be parsed", () => {
    const { self } = fixture();
    self.outcomes.lifetime = outcomes({ last_failed_at: "not-a-date", failed: 10 });
    const view = mount({ self });
    expect(view.getByText("Latest failure (all time): unavailable")).toBeTruthy();
  });

  it("drops the Factory all-time sub-line when earliest_run is null", () => {
    const { self, admin } = fixture();
    admin.earliest_run = null;
    const view = mount({ self, admin });
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    expect(view.queryByText(/across every user/)).toBeNull();
  });
});

describe("OverviewUsageCard by-user table (#3881 M2)", () => {
  const HEADERS = ["User", "Runs", "PRs", "Failed", "Fail rate", "Last failure", "Cost"];
  const withPRs = (admin: AdminUsage) => {
    admin.users.forEach((u, i) => { u.pr_count = 10 + i; u.last7_pr_count = 1 + i; });
  };
  const factoryView = (props: Parameters<typeof Harness>[0]) => {
    const view = mount(props);
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    return view;
  };
  const section = (view: ReturnType<typeof mount>) => within(view.getByRole("region", { name: "By user" }));
  const headers = (view: ReturnType<typeof mount>) => section(view).getAllByRole("columnheader").map((h) => h.textContent);
  const cells = (row: HTMLElement) => Array.from(row.children).map((c) => c.textContent);
  const rowOf = (view: ReturnType<typeof mount>, name: RegExp | string) =>
    section(view).getAllByRole("row").find((r) => {
      const first = r.firstElementChild?.textContent ?? "";
      return typeof name === "string" ? first === name : name.test(first);
    })!;
  const PRS = 2;

  it("appears only in the Factory scope", () => {
    const { self, admin } = fixture();
    const view = mount({ self, admin });
    expect(view.queryByRole("region", { name: "By user" })).toBeNull();
    fireEvent.click(view.getByRole("button", { name: "All users" }));
    expect(view.getByRole("region", { name: "By user" })).toBeTruthy();
    fireEvent.click(view.getByRole("button", { name: "You" }));
    expect(view.queryByRole("region", { name: "By user" })).toBeNull();
    cleanup();
    expect(mount({ self }).queryByRole("region", { name: "By user" })).toBeNull();
  });

  it("titles the section and notes the window", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    expect(section(view).getByRole("heading", { name: "By user" })).toBeTruthy();
    expect(section(view).getByText("all time, by cost")).toBeTruthy();
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(section(view).getByText("last 7 days, by cost")).toBeTruthy();
  });

  it("has exactly the same seven headers with and without PR counts, in both windows, when the API sends no harness split", () => {
    const { self, admin } = fixture();
    withPRs(admin);
    const view = factoryView({ self, admin });
    expect(headers(view)).toEqual(HEADERS);
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(headers(view)).toEqual(HEADERS);
    cleanup();
    const bare = fixture();
    const second = factoryView({ self: bare.self, admin: bare.admin });
    expect(headers(second)).toEqual(HEADERS);
    fireEvent.click(second.getByRole("button", { name: "Last 7 days" }));
    expect(headers(second)).toEqual(HEADERS);
    expect(second.queryByRole("columnheader", { name: /Tokens|Out|Cost share/ })).toBeNull();
  });

  // Issue #3876: the Claude / Codex column.
  const withHarness = (admin: AdminUsage) => {
    admin.users.forEach((u) => { u.harness_runs = { claude: 20, codex: 3 }; u.last7_harness_runs = { claude: 2, codex: 1 }; });
  };
  const HARNESS = 7;

  it("adds Claude / Codex as the eighth and last header when the API sends the split, in both windows", () => {
    const { self, admin } = fixture();
    withHarness(admin);
    const view = factoryView({ self, admin });
    expect(headers(view)).toEqual([...HEADERS, "Claude / Codex"]);
    expect(section(view).getByRole("columnheader", { name: "Claude / Codex" }).getAttribute("title")).toBe(
      "Share of each user's runs by harness, the same runs as the Runs column; cross-check runs count under the harness they ran on",
    );
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(headers(view)).toEqual([...HEADERS, "Claude / Codex"]);
    // The Factory row keeps the table rectangular with an empty cell.
    const total = cells(rowOf(view, "Factory"));
    expect(total).toHaveLength(8);
    expect(total[HARNESS]).toBe("");
  });

  it("shows a 7-day user's 7 Claude / 3 Codex runs as 70 / 30, with the counts in the title", () => {
    const { self, admin } = fixture();
    withHarness(admin);
    admin.users[0].last7_harness_runs = { claude: 7, codex: 3 };
    const view = factoryView({ self, admin });
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    const cell = rowOf(view, /^owner\./).children[HARNESS] as HTMLElement;
    expect(cell.textContent).toBe("70 / 30");
    expect(cell.getAttribute("title")).toBe("7 Claude, 3 Codex runs");
    expect(cell.querySelector("[aria-hidden='true']")).toBeTruthy();
    expect(cells(rowOf(view, /^other\./))[HARNESS]).toBe("67 / 33");
  });

  it("omits the column against a response without the split, keeping the other headers", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    expect(section(view).queryByRole("columnheader", { name: "Claude / Codex" })).toBeNull();
    for (const h of HEADERS) expect(section(view).getByRole("columnheader", { name: h })).toBeTruthy();
    expect(cells(rowOf(view, "Factory"))).toHaveLength(7);
    cleanup();
    const full = fixture();
    withHarness(full.admin);
    const second = factoryView({ self: full.self, admin: full.admin });
    expect(section(second).getByRole("columnheader", { name: "Claude / Codex" })).toBeTruthy();
  });

  it("reads a dash for a user with no runs in the window", () => {
    const { self, admin } = fixture();
    withHarness(admin);
    admin.users[1].last7_harness_runs = { claude: 0, codex: 0 };
    const view = factoryView({ self, admin });
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    const cell = rowOf(view, /^other\./).children[HARNESS] as HTMLElement;
    expect(cell.textContent).toBe("—");
    expect(cell.getAttribute("title")).toBeNull();
    expect(cells(rowOf(view, /^owner\./))[HARNESS]).toBe("67 / 33");
  });

  it("titles the headers and scopes them to their columns", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    const th = (name: string) => section(view).getByRole("columnheader", { name });
    expect(th("Last failure").getAttribute("title")).toBe(
      "All time: the API serves the last failure time for all time only, so this column is the same in both windows",
    );
    expect(th("Runs").getAttribute("title")).toBe("Runs with recorded usage in the period");
    expect(th("Fail rate").getAttribute("title")).toBe("Failed of finished runs in the period");
    for (const h of section(view).getAllByRole("columnheader")) expect(h.getAttribute("scope")).toBe("col");
  });

  it("reads a dash, never 0, for a PR count the API does not report", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    for (const r of section(view).getAllByRole("row").slice(1)) expect(cells(r)[PRS]).toBe("—");
    expect(cells(rowOf(view, "Factory"))[PRS]).toBe("—");
    expect(section(view).queryByText("0", { selector: "td" })).toBeNull();
  });

  it("shows a reported count, a dash for a row without one, and a dash for the Factory total", () => {
    const { self, admin } = fixture();
    admin.users[0].pr_count = 7;
    const view = factoryView({ self, admin });
    expect(cells(rowOf(view, /^owner\./))[PRS]).toBe("7");
    expect(cells(rowOf(view, /^other\./))[PRS]).toBe("—");
    expect(cells(rowOf(view, "Factory"))[PRS]).toBe("—");
    cleanup();
    const full = fixture();
    withPRs(full.admin);
    const second = factoryView({ self: full.self, admin: full.admin });
    expect(cells(rowOf(second, "Factory"))[PRS]).toBe("21");
  });

  it("marks only the viewer's row with (you)", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    expect(section(view).getAllByText(/\(you\)/)).toHaveLength(1);
    expect(cells(rowOf(view, /^owner\./))[0]).toBe("owner.person@example.com (you)");
    expect(cells(rowOf(view, /^other\./))[0]).toBe("other.person@example.com");
  });

  it("shows the same Last failure text in both windows, for user rows and the Factory row", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-07T10:00:00Z"));
    const { self, admin } = fixture();
    admin.factory.outcomes.lifetime = { ...admin.factory.outcomes.lifetime, last_failed_at: "2026-10-07T06:30:00Z" };
    const view = factoryView({ self, admin });
    const LAST = 5;
    const read = () => [cells(rowOf(view, /^owner\./))[LAST], cells(rowOf(view, /^other\./))[LAST], cells(rowOf(view, "Factory"))[LAST]];
    expect(read()).toEqual(["4h ago", "4h ago", "3h ago"]);
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(read()).toEqual(["4h ago", "4h ago", "3h ago"]);
  });

  it("strips control and bidi characters from an email", () => {
    const { self, admin } = fixture();
    admin.users[0].email = "ev‮i\u0007l@example.com";
    const view = factoryView({ self, admin });
    expect(cells(rowOf(view, /^evil@/))[0]).toBe("evil@example.com (you)");
  });

  it("masks the email in demo mode", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    expect(cells(rowOf(view, /^owner\.person@/))[0]).toBe("owner.person@example.com (you)");
    act(() => setDemoMode(true));
    expect(section(view).queryByText(/owner\.person@example\.com/)).toBeNull();
    expect(cells(rowOf(view, /^Owner/))[0]).toBe("Owner (you)");
  });

  it("explains an unavailable seven-day period instead of a table", () => {
    const { self, admin } = fixture();
    delete admin.users[0].last7_run_count;
    const view = factoryView({ self, admin, initial: "last_7_days" });
    expect(section(view).getByText("Seven-day per-user usage is unavailable. Upgrade the API to view this period.")).toBeTruthy();
    expect(section(view).queryByRole("table")).toBeNull();
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(section(view).getByRole("table")).toBeTruthy();
  });

  it("says so when the factory has no users yet", () => {
    const { self, admin } = fixture();
    admin.users = [];
    const view = factoryView({ self, admin });
    expect(section(view).getByText("No usage across the factory yet.")).toBeTruthy();
  });

  it("shows the Factory row's Runs as the lifetime count, or the seven-day sum", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    expect(cells(rowOf(view, "Factory"))[1]).toBe("46");
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(cells(rowOf(view, "Factory"))[1]).toBe("6");
    expect(cells(rowOf(view, /^owner\./))[1]).toBe("3");
  });

  it("states the fail rate over finished runs, and the cost with its disclosure", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    const row = cells(rowOf(view, /^owner\./));
    expect(row[3]).toBe("10");
    expect(row[4]).toBe("10.0%of 100 finished");
    expect(row[6]).toMatch(/^\$1,235.+/);
    expect(within(rowOf(view, /^owner\./)).getByTitle("10 of 100 finished runs")).toBeTruthy();
    expect(cells(rowOf(view, "Factory"))[4]).toBe("10.0%of 200 finished");
  });

  it("takes the Factory row's lifetime Runs from the factory count, not the row sum", () => {
    const { self, admin } = fixture();
    admin.factory.run_count = 50;
    const view = factoryView({ self, admin });
    expect(cells(rowOf(view, "Factory"))[1]).toBe("50");
    expect(cells(rowOf(view, /^owner\./))[1]).toBe("23");
  });

  it("discloses the Factory row's own incomplete cost, per window", () => {
    const { self, admin } = fixture();
    Object.assign(admin.factory, {
      lifetime_subscription_run_count: 5, lifetime_unreported_run_count: 3,
      last7_subscription_run_count: 2, last7_unreported_run_count: 1,
    });
    const view = factoryView({ self, admin });
    const note = () => within(rowOf(view, "Factory")).getByText(/^\+ /);
    expect(note().textContent).toBe("+ 5 runs without a cost estimate and 3 unreported runs");
    expect(note().getAttribute("title")).toBe(
      "Runs without a cost estimate: tokens only; Unreported runs: cost incomplete; recorded partial costs included",
    );
    // A user row keeps its own counts.
    expect(within(rowOf(view, /^owner\./)).getByText(/^\+ /).textContent).toBe("+ 4 runs without a cost estimate and 1 unreported run");
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(note().textContent).toBe("+ 2 runs without a cost estimate and 1 unreported run");
  });

  it("names the scroll wrapper as a region and each row's first cell as a row header", () => {
    const { self, admin } = fixture();
    const view = factoryView({ self, admin });
    const scroll = view.getByRole("region", { name: "By user, scrollable" });
    expect(scroll.hasAttribute("tabindex")).toBe(false);
    expect(view.getByRole("table", { name: "By user" })).toBeTruthy();
    expect(scroll.getAttribute("data-testid")).toBe("by-user-scroll");
    expect(section(view).getAllByRole("rowheader").map((h) => h.textContent)).toEqual([
      "other.person@example.com", "owner.person@example.com (you)", "Factory",
    ]);
  });
});

describe("OverviewUsageCard controlled scope and By user focus (#4042)", () => {
  function Controlled({ self, admin, scope, onScopeChange, focusByUser }: {
    self: SelfUsage; admin: AdminUsage; scope: "you" | "all"; onScopeChange: (s: "you" | "all") => void; focusByUser?: number;
  }) {
    return (
      <MemoryRouter>
        <OverviewUsageCard
          self={self} admin={admin} window="lifetime" viewerId="owner" onWindowChange={() => {}}
          scope={scope} onScopeChange={onScopeChange} focusByUser={focusByUser}
        />
      </MemoryRouter>
    );
  }

  it("draws the scope from the prop, reports a pick and leaves the saved choice to the parent", () => {
    const { self, admin } = fixture();
    const onScopeChange = vi.fn();
    const view = render(<Controlled self={self} admin={admin} scope="all" onScopeChange={onScopeChange} />);
    expect(view.getByRole("region", { name: "By user" })).toBeTruthy();
    expect(view.getByRole("button", { name: "All users" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(view.getByRole("button", { name: "You" }));
    expect(onScopeChange).toHaveBeenCalledWith("you");
    // Still driven by the prop: no change until the parent passes a new one.
    expect(view.getByRole("region", { name: "By user" })).toBeTruthy();
    expect(localStorage.getItem("uzi.overviewUsageScope")).toBeNull();
    view.rerender(<Controlled self={self} admin={admin} scope="you" onScopeChange={onScopeChange} />);
    expect(view.queryByRole("region", { name: "By user" })).toBeNull();
    expect(view.getByRole("region", { name: "Your usage" })).toBeTruthy();
  });

  it("focuses the By user section when focusByUser is bumped, once per value", () => {
    const { self, admin } = fixture();
    const view = render(<Controlled self={self} admin={admin} scope="all" onScopeChange={() => {}} focusByUser={0} />);
    const byUser = view.getByRole("region", { name: "By user" });
    expect(byUser.id).toBe("overview-usage-by-user");
    expect(document.activeElement).not.toBe(byUser);
    view.rerender(<Controlled self={self} admin={admin} scope="all" onScopeChange={() => {}} focusByUser={1} />);
    expect(document.activeElement).toBe(byUser);
    (document.activeElement as HTMLElement).blur();
    view.rerender(<Controlled self={self} admin={admin} scope="all" onScopeChange={() => {}} focusByUser={1} />);
    expect(document.activeElement).not.toBe(byUser);
  });

  it("does not refocus for the same counter value after the section is hidden and shown again", () => {
    const { self, admin } = fixture();
    const ui = (scope: "you" | "all", n: number) => (
      <Controlled self={self} admin={admin} scope={scope} onScopeChange={() => {}} focusByUser={n} />
    );
    const view = render(ui("all", 1));
    expect(document.activeElement).toBe(view.getByRole("region", { name: "By user" }));
    (document.activeElement as HTMLElement).blur();
    view.rerender(ui("you", 1));
    view.rerender(ui("all", 1));
    expect(document.activeElement).not.toBe(view.getByRole("region", { name: "By user" }));
  });

  it("focuses the section once it mounts when the scope and the bump arrive together", () => {
    const { self, admin } = fixture();
    const view = render(<Controlled self={self} admin={admin} scope="you" onScopeChange={() => {}} focusByUser={0} />);
    view.rerender(<Controlled self={self} admin={admin} scope="all" onScopeChange={() => {}} focusByUser={1} />);
    expect(document.activeElement).toBe(view.getByRole("region", { name: "By user" }));
  });
});
