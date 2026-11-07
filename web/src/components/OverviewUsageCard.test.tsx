// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, within } from "@testing-library/react";
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
afterEach(() => { cleanup(); vi.restoreAllMocks(); setDemoMode(false); });

describe("OverviewUsageCard (#3881 M1)", () => {
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
    fireEvent.click(scope.getByRole("button", { name: "Factory · all users" }));
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

  it("does not render the dropped side-by-side or run-count pieces", () => {
    const { self, admin } = fixture();
    const view = mount({ self, admin });
    expect(view.queryByText(/runs with token usage/)).toBeNull();
    expect(view.getAllByRole("region", { name: /^(Your|Factory) usage$/ })).toHaveLength(1);
    fireEvent.click(view.getByRole("button", { name: "Factory · all users" }));
    expect(view.queryByText(/runs with token usage/)).toBeNull();
    expect(view.getAllByRole("region", { name: /^(Your|Factory) usage$/ })).toHaveLength(1);
  });
});
