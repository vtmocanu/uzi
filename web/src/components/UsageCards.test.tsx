// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { useState } from "react";
import { UsageCard, type UsageWindow } from "./UsageCards";
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
const emptyOutcomes = (): RunOutcomes => outcomes({
  finished: 0, completed: 0, cancelled: 0, plan_rejected: 0, base_diverged: 0, failed: 0,
  needs_landing: 0, fail_origins: {}, last_failed_at: null, last_failed_run_id: null,
  last_failed_origin: null, completed_since_last_failure: null,
});
function fixture(): { self: SelfUsage; admin: AdminUsage } {
  const self: SelfUsage = {
    lifetime: bundle(1000, 2000, 500, 1234.56), last_7_days: bundle(100, 200, 50, 12.34),
    run_count: 23, lifetime_subscription_run_count: 8, lifetime_unreported_run_count: 2,
    last7_subscription_run_count: 1, last7_unreported_run_count: 0,
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
      last_7_days: id === "owner" ? self.last_7_days : bundle(200, 400, 100, 24.68),
      last7_run_count: id === "owner" ? 3 : 7, last7_outcomes: self.outcomes.last_7_days,
      last7_subscription_run_count: id === "owner" ? 1 : 0, last7_unreported_run_count: 0,
    })),
    earliest_run: "2026-05-12T09:00:00Z",
  };
  return { self, admin };
}
function Harness({ self, admin, initial = "last_7_days" }: { self: SelfUsage; admin?: AdminUsage; initial?: UsageWindow }) {
  const [window, onWindowChange] = useState(initial);
  return <UsageCard self={self} admin={admin} window={window} onWindowChange={onWindowChange} />;
}
const mount = (props: Parameters<typeof Harness>[0]) => render(<MemoryRouter><Harness {...props} /></MemoryRouter>);
const columns = (view: ReturnType<typeof mount>) => ({
  you: within(view.getByRole("region", { name: "Your usage" })),
  factory: within(view.getByRole("region", { name: "Factory usage" })),
  users: within(view.getByRole("region", { name: /^Per-user usage,/ })),
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); setDemoMode(false); });

describe("UsageCard (#40, #1293, #1429 D7)", () => {
  it("retains recorded mixed-run partial dollars while disclosing incomplete cost", () => {
    const { self } = fixture();
    self.last_7_days = bundle(1000, 0, 200, 0.61);
    self.last7_subscription_run_count = 0;
    self.last7_unreported_run_count = 1;
    const view = mount({ self });
    expect(view.getByText("$0.61")).toBeTruthy();
    expect(view.getByText("+ 1 unreported run").getAttribute("title")).toBe("Unreported runs: cost incomplete; recorded partial costs included");
    expect(view.getByRole("heading", { name: "Cost (API-equivalent)" })).toBeTruthy();
  });
  it("ranks users by recorded cost before tokens in both windows and retains incomplete-cost disclosures", () => {
    const f = fixture();
    f.admin.users[0].usage = f.admin.users[0].last_7_days = bundle(900, 0, 100, 0);
    f.admin.users[1].usage = f.admin.users[1].last_7_days = bundle(0, 0, 100, 50);
    f.admin.factory.lifetime = f.admin.factory.last_7_days = bundle(900, 0, 200, 50);
    const view = mount(f);
    for (const period of ["Last 7 days", "All time"]) {
      fireEvent.click(view.getByRole("button", { name: period }));
      const rows = columns(view).users.getAllByRole("row").slice(1, -1);
      expect(rows.map((row) => row.querySelector("td")?.textContent)).toEqual([
        "other.person@example.com", "owner.person@example.com",
      ]);
      expect(within(rows[0]).getByRole("img").getAttribute("aria-label")).toBe("100 percent of recorded factory cost");
      expect(within(rows[1]).getByRole("img").getAttribute("aria-label")).toBe("0 percent of recorded factory cost");
      if (period === "All time") {
        expect(rows[1].textContent).toContain("+ 4 runs without a cost estimate and 1 unreported run");
      }
      expect(columns(view).users.getByRole("columnheader", { name: "Cost share" }).title)
        .toBe("Share of recorded API-equivalent cost in the selected period");
    }
  });
  it("breaks equal costs by tokens, output and user ID", () => {
    const f = fixture();
    f.admin.users = ["d", "c", "b", "a"].map((id, i) => ({
      ...f.admin.users[0], user_id: id, email: `${id}@example.com`,
      last_7_days: bundle([79, 90, 80, 80][i], 0, [20, 10, 20, 20][i], 2),
    }));
    f.admin.factory.last_7_days = bundle(329, 0, 70, 8);
    const rows = columns(mount(f)).users.getAllByRole("row").slice(1, -1);
    expect(rows.map((row) => row.querySelector("td")?.textContent)).toEqual([
      "a@example.com", "b@example.com", "c@example.com", "d@example.com",
    ]);
  });
  it("orders costs before display rounding even when both render the same dollars", () => {
    const f = fixture();
    f.admin.users[0].last_7_days = bundle(1000, 0, 0, 1.000001);
    f.admin.users[1].last_7_days = bundle(1, 0, 0, 1.000002);
    f.admin.factory.last_7_days = bundle(1001, 0, 0, 2.000003);
    const rows = columns(mount(f)).users.getAllByRole("row").slice(1, -1);
    expect(rows.map((row) => row.querySelector("td")?.textContent)).toEqual([
      "other.person@example.com", "owner.person@example.com",
    ]);
    for (const row of rows) expect(within(row).getByText("$1.00")).toBeTruthy();
  });
  it("switches both scopes with one accessible toggle and the embedded table follows the window", () => {
    const view = mount(fixture());
    const { you, factory, users } = columns(view);
    expect(view.getAllByRole("group", { name: "Usage reporting period" })).toHaveLength(1);
    const last7 = view.getByRole("button", { name: "Last 7 days" });
    const allTime = view.getByRole("button", { name: "All time" });
    expect(last7.getAttribute("aria-pressed")).toBe("true");
    expect(allTime.getAttribute("aria-pressed")).toBe("false");
    expect(you.getByText("$12.34")).toBeTruthy();
    expect(factory.getByText("$37.02")).toBeTruthy();
    expect(you.getByText("350")).toBeTruthy();
    expect(factory.getByText("1.1k")).toBeTruthy();
    expect(you.getByText("0.0 pp vs all-time 10.0%")).toBeTruthy();
    expect(you.queryByText(/runs with token usage/)).toBeNull();
    expect(view.getByRole("heading", { name: "Per user · last 7 days" })).toBeTruthy();
    const tableBefore = users.getByRole("table").textContent;
    fireEvent.click(allTime);
    expect(allTime.getAttribute("aria-pressed")).toBe("true");
    expect(last7.getAttribute("aria-pressed")).toBe("false");
    expect(you.getByText("$1,235")).toBeTruthy();
    expect(factory.getByText("$2,469")).toBeTruthy();
    expect(you.getByText("23 runs with token usage")).toBeTruthy();
    expect(factory.getByText(/46 runs with token usage by 2 users since/)).toBeTruthy();
    expect(you.queryByText(/pp vs all-time/)).toBeNull();
    expect(view.getByRole("heading", { name: "Per user · all time" })).toBeTruthy();
    expect(users.getByRole("table").textContent).not.toBe(tableBefore);
    fireEvent.click(last7);
    expect(users.getByRole("table").textContent).toBe(tableBefore);
    fireEvent.click(allTime);
    expect(users.getAllByText("$1235")).toHaveLength(2);
    allTime.focus(); expect(document.activeElement).toBe(allTime);
    for (const cls of ["font-semibold", "focus-visible:outline-solid", "focus-visible:outline-2", "focus-visible:outline-ring", "min-h-11", "sm:min-h-8"]) {
      expect(allTime.classList.contains(cls)).toBe(true);
    }
  });
  it("nests metric headings below the scope headings", () => {
    const view = mount(fixture());
    expect(view.getByRole("heading", { name: "You", level: 3 })).toBeTruthy();
    expect(view.getByRole("heading", { name: "Factory · all users", level: 3 })).toBeTruthy();
    expect(view.getByRole("heading", { name: "Per user · last 7 days", level: 3 })).toBeTruthy();
    for (const name of ["Cost (API-equivalent)", "Failed runs rate", "Tokens"]) {
      expect(view.getAllByRole("heading", { name, level: 4 })).toHaveLength(2);
    }
  });

  it.each([
    [0, 1, "+ 1 unreported run", "Unreported runs: cost incomplete; recorded partial costs included"],
    [2, 0, "+ 2 runs without a cost estimate", "Runs without a cost estimate: tokens only"],
    [1, 2, "+ 1 run without a cost estimate and 2 unreported runs", "Runs without a cost estimate: tokens only; Unreported runs: cost incomplete; recorded partial costs included"],
  ] as const)("formats compact disclosures and explanatory title for %i subscription and %i unreported runs", (subscription, unreported, short, full) => {
    const f = fixture();
    for (const usage of [f.self, f.admin.factory, ...f.admin.users]) {
      usage.last7_subscription_run_count = subscription;
      usage.last7_unreported_run_count = unreported;
    }
    const view = mount(f);
    const notes = view.getAllByText(short);
    expect(notes).toHaveLength(5); // personal card, factory card, two users, factory total
    for (const note of notes) expect(note.getAttribute("title")).toBe(full);
  });

  it("renders a non-admin single column without factory or per-user data", () => {
    const view = mount({ self: fixture().self });
    expect(view.getByRole("region", { name: "Your usage" })).toBeTruthy();
    expect(view.queryByRole("region", { name: "Factory usage" })).toBeNull();
    expect(view.queryByRole("region", { name: /^Per-user usage,/ })).toBeNull();
    expect(view.getByRole("link", { name: "Your failed runs →" }).getAttribute("href")).toBe("/runs/history?status=failed");
  });
  it.each(["recent", "lifetime"])("hides the baseline when the %s denominator is zero", (scope) => {
    const { self } = fixture();
    self.outcomes[scope === "recent" ? "last_7_days" : "lifetime"] = emptyOutcomes();
    const view = mount({ self });
    expect(view.queryByText(/pp vs all-time/)).toBeNull();
    if (scope === "recent") {
      const you = within(view.getByRole("region", { name: "Your usage" }));
      expect(you.getByText("—")).toBeTruthy();
      expect(you.getByText("No finished runs in this period.")).toBeTruthy();
      expect(you.queryByRole("img")).toBeNull();
    }
  });
  it("computes a signed percentage-point difference from unrounded rates", () => {
    const { self } = fixture();
    self.outcomes.lifetime = outcomes({ finished: 3, completed: 2, failed: 1, cancelled: 0, plan_rejected: 0, base_diverged: 0, needs_landing: 0, fail_origins: { agent_failure: 1 } });
    const view = mount({ self });
    expect(view.getByText("−23.3 pp vs all-time 33.3%")).toBeTruthy();
  });
  it("keeps top three sorted causes and counts hidden origins with a personal-only link", () => {
    const f = fixture();
    const origins = { unknown: 1, worker_lost: 2, run_timeout: 3, agent_failure: 3, rate_limited: 1 };
    f.self.outcomes.last_7_days = outcomes({ fail_origins: origins });
    f.admin.factory.outcomes.last_7_days = outcomes({ fail_origins: origins });
    const view = mount(f); const { you, factory } = columns(view);
    expect(you.getByText(/Top causes:/).textContent).toBe("Top causes: agent failure 3 · run timeout 3 · worker lost 2 +2 more");
    expect(you.getByRole("link", { name: "+2 more" }).getAttribute("href")).toBe("/runs/history?status=failed");
    expect(factory.getByText("+2 more")).toBeTruthy();
    expect(factory.queryByRole("link", { name: "+2 more" })).toBeNull();
    expect(factory.queryByRole("link", { name: "Your failed runs →" })).toBeNull();
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(you.queryByText("+2 more")).toBeNull();
    expect(you.getByText(/Top causes:/).textContent).toContain("agent failure 6");
  });
  it("hides zero legend items and keeps the full failed bar unsplit (#1418)", () => {
    const view = mount(fixture()); const { you } = columns(view);
    expect(you.queryByText(/^rejected/)).toBeNull();
    const bar = you.getByRole("img");
    expect(bar.getAttribute("aria-label")).toBe("Finished runs: 17 completed, 1 cancelled, 2 failed");
    expect(bar.children).toHaveLength(3);
    expect((bar.lastElementChild as HTMLElement).style.width).toBe("10%");
    expect(you.getByText("1 failed runs with recoverable work. May include work already landed.")).toBeTruthy();
    // Paired with the positive replacement note and full failed bar assertions.
    expect(bar.getAttribute("aria-label")).not.toContain("need landing");
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(you.getByText("rejected", { exact: false })).toBeTruthy();
    expect(bar.children).toHaveLength(4);
    expect(you.getByText("4 failed runs with recoverable work. May include work already landed.")).toBeTruthy();
  });
  it("hides recoverability at zero and causes when no failures exist", () => {
    const { self } = fixture();
    self.outcomes.last_7_days = outcomes({ finished: 20, completed: 20, cancelled: 0, plan_rejected: 0, base_diverged: 0, failed: 0, needs_landing: 0, fail_origins: {} });
    const view = mount({ self });
    expect(view.queryByText(/failed runs with recoverable work/)).toBeNull();
    expect(view.queryByText(/Top causes:/)).toBeNull(); expect(view.getByText("0.0%")).toBeTruthy();
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(view.getByText(/4 failed runs with recoverable work/)).toBeTruthy(); expect(view.getByText(/Top causes:/)).toBeTruthy();
  });
  it("pairs each selected cost with its own exclusions and full title (#1429 D7)", () => {
    const view = mount({ self: fixture().self });
    expect(view.getByText("+ 1 run without a cost estimate").getAttribute("title")).toBe("Runs without a cost estimate: tokens only");
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(view.getByText("+ 8 runs without a cost estimate and 2 unreported runs").getAttribute("title")).toBe("Runs without a cost estimate: tokens only; Unreported runs: cost incomplete; recorded partial costs included");
    expect(view.queryByText("+ 1 run without a cost estimate")).toBeNull();
  });
  it("shows real zero metered cost without exclusions when the window is fully metered", () => {
    const { self } = fixture(); self.last_7_days.cost_usd = 0; self.last7_subscription_run_count = 0;
    const view = mount({ self }); expect(view.getByText("$0.00")).toBeTruthy(); expect(view.queryByText(/^\+ \d+ (runs? without a cost estimate|unreported runs?)/)).toBeNull();
  });
  it("keeps lifetime recency and intact run links in both windows", () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-07T12:00:00Z"));
    const view = mount(fixture()); const { you, factory } = columns(view);
    for (const window of ["Last 7 days", "All time"]) {
      fireEvent.click(view.getByRole("button", { name: window }));
      expect(you.getByText("6h")).toBeTruthy(); expect(factory.getByText("2h")).toBeTruthy(); expect(you.getByText("14 completed since")).toBeTruthy();
      expect(factory.getByRole("link", { name: "run factory- →" }).getAttribute("href")).toBe("/runs/factory-failure");
      for (const child of factory.getByText("Latest failure", { exact: false }).closest("p")!.children) {
        expect(child.textContent?.trim().startsWith("·")).toBe(false);
      }
    }
  });
  it("sanitizes origin and owner in text and titles, and masks the owner in demo mode", () => {
    const f = fixture(); f.admin.users[0].email = "owner.\u202Eperson\u200B@example.com";
    f.admin.factory.outcomes.lifetime.last_failed_origin = "new\u202Eorigin\u200B";
    f.admin.factory.outcomes.last_7_days.fail_origins = { "new\u202Eorigin\u200B": 4 };
    const view = mount(f); const { factory } = columns(view);
    expect(factory.getByTitle("neworigin").textContent).toBe("neworigin");
    expect(factory.getByTitle("owner.person@example.com").textContent).toBe("owner.person@example.com");
    expect(factory.getByText(/Top causes:/).textContent).toBe("Top causes: neworigin 4");
    for (const node of view.getByRole("region", { name: "Factory usage" }).querySelectorAll("[title], [aria-label]")) {
      expect((node.getAttribute("title") ?? "") + (node.getAttribute("aria-label") ?? "")).not.toMatch(/[\u202E\u200B]/);
    }
    act(() => setDemoMode(true)); expect(factory.getByTitle("Owner").textContent).toBe("Owner"); expect(factory.queryByTitle("owner.person@example.com")).toBeNull();
  });
  it("keeps seven fixed row slots when optional content differs", () => {
    const f = fixture(); f.self.outcomes.last_7_days = emptyOutcomes(); const view = mount(f);
    for (const name of ["Your usage", "Factory usage"]) {
      const column = view.getByRole("region", { name }); expect(column.children).toHaveLength(7); expect(column.classList.contains("md:grid-rows-subgrid")).toBe(true);
    }
  });
  it("retains all-time table shares, disclosure and failure-denominator attributes", () => {
    const view = mount({ ...fixture(), initial: "lifetime" }); const { users } = columns(view);
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual(["50 percent of recorded factory cost", "50 percent of recorded factory cost"]);
    const row = users.getByText("owner.person@example.com").closest("tr")!;
    expect(row.querySelectorAll("td")[3].getAttribute("title")).toBe("10 of 100 finished runs");
    expect(row.textContent).toContain("+ 4 runs without a cost estimate and 1 unreported run");
  });
  it("rounds per-user microdollar cost shares to exactly 100% with largest remainders", () => {
    const f = fixture();
    f.admin.factory.lifetime = bundle(1000, 0, 0, 0.001);
    f.admin.users = [905, 85, 10].map((microUSD, i) => ({
      ...f.admin.users[0], user_id: `share-${i}`, email: `share-${i}@example.com`,
      usage: bundle(1, 0, 0, microUSD / 1e6),
    }));
    const view = mount({ ...f, initial: "lifetime" });
    const users = within(view.getByRole("region", { name: /^Per-user usage,/ }));
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual([
      "91 percent of recorded factory cost", "8 percent of recorded factory cost", "1 percent of recorded factory cost",
    ]);
    expect((users.getAllByRole("img")[0].firstElementChild as HTMLElement).style.width).toBe("91%");
    expect(users.getAllByRole("img").reduce((sum, bar) =>
      sum + Number.parseInt(bar.getAttribute("aria-label")!, 10), 0)).toBe(100);
  });

  it("selects metrics before sorting by cost, tokens, output and user ID in both directions", () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-07T12:00:00Z"));
    const f = fixture();
    f.admin.users = ["b", "a", "c", "d", "e"].map((id, i) => ({
      ...f.admin.users[0], user_id: id, email: id + "@example.com",
      usage: bundle(0, 0, [20, 20, 30, 0, 0][i], [2, 2, 2, 0, 0][i]),
      run_count: [3, 4, 5, 0, 0][i], outcomes: outcomes(),
      subscription_run_count: i === 0 ? 2 : 0, unreported_run_count: i === 1 ? 1 : 0,
      last_7_days: bundle(0, 0, [0, 10, 20, 0, 0][i], [0, 2, 1, 0, 0][i]),
      last7_run_count: [0, 1, 2, 0, 0][i],
      last7_outcomes: i === 4 ? outcomes({ finished: 2, failed: 1 }) : emptyOutcomes(),
      last7_subscription_run_count: i === 2 ? 1 : 0, last7_unreported_run_count: i === 1 ? 1 : 0,
    }));
    f.admin.factory.lifetime = bundle(0, 0, 70, 6);
    f.admin.factory.last_7_days = bundle(0, 0, 30, 3);
    f.admin.factory.run_count = 12;
    f.admin.factory.lifetime_subscription_run_count = 2;
    f.admin.factory.lifetime_unreported_run_count = 1;
    f.admin.factory.last7_subscription_run_count = 1;
    f.admin.factory.last7_unreported_run_count = 1;
    f.admin.factory.outcomes.last_7_days = outcomes({ finished: 2, failed: 1 });
    const view = mount(f); const users = columns(view).users;
    const rowCells = () => users.getAllByRole("row").slice(1).map((row) =>
      Array.from(row.querySelectorAll("td")).map((cell) => cell.textContent));
    const recent = rowCells();
    expect(recent.map((r) => r[0])).toEqual(["a@example.com", "c@example.com", "b@example.com", "d@example.com", "e@example.com", "uzi total"]);
    expect(recent[1]).toEqual(["c@example.com", "2", "0", "—", "6h", "20", "20", "$1.00+ 1 run without a cost estimate", "33%"]);
    expect(recent[0][7]).toBe("$2.00+ 1 unreported run");
    expect(recent[2].slice(1, 8)).toEqual(["0", "0", "—", "6h", "0", "0", "$0.00"]);
    expect(recent[4].slice(1, 4)).toEqual(["0", "1", "50.0%"]);
    expect(recent[5].slice(1, 4)).toEqual(["3", "1", "50.0%"]);
    expect(recent[5].slice(5, 8)).toEqual(["30", "30", "$3.00+ 1 run without a cost estimate and 1 unreported run"]);
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual([
      "67 percent of recorded factory cost", "33 percent of recorded factory cost", "0 percent of recorded factory cost",
      "0 percent of recorded factory cost", "0 percent of recorded factory cost",
    ]);
    expect(users.getAllByRole("row")[5].querySelectorAll("td")[3].title).toBe("1 of 2 finished runs");
    const totalRecency = recent[5][4];
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    const lifetime = rowCells();
    expect(lifetime.map((r) => r[0])).toEqual(["c@example.com", "a@example.com", "b@example.com", "d@example.com", "e@example.com", "uzi total"]);
    expect(lifetime[1].slice(1, 4)).toEqual(["4", "10", "10.0%"]);
    expect(lifetime[2][7]).toBe("$2.00+ 2 runs without a cost estimate");
    expect(lifetime[5].slice(1, 4)).toEqual(["12", "20", "10.0%"]);
    expect(lifetime[5].slice(5, 8)).toEqual(["70", "70", "$6.00+ 2 runs without a cost estimate and 1 unreported run"]);
    expect(lifetime[5][4]).toBe(totalRecency);
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual([
      "34 percent of recorded factory cost", "33 percent of recorded factory cost", "33 percent of recorded factory cost",
      "0 percent of recorded factory cost", "0 percent of recorded factory cost",
    ]);
    fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
    expect(rowCells()).toEqual(recent);
  });

  it.each(["last_7_days", "last7_run_count", "last7_outcomes", "last7_subscription_run_count", "last7_unreported_run_count"] as const)(
    "requires %s on every user for seven-day data while lifetime remains usable", (field) => {
      const f = fixture(); delete f.admin.users[1][field];
      const view = mount(f); const users = columns(view).users;
      expect(users.queryByRole("table")).toBeNull();
      expect(users.getByText("Seven-day per-user usage is unavailable. Upgrade the API to view this period.")).toBeTruthy();
      fireEvent.click(view.getByRole("button", { name: "All time" }));
      expect(users.getByRole("table")).toBeTruthy();
      fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
      expect(users.queryByRole("table")).toBeNull();
    },
  );

  it("renders unavailable cost shares for zero factory cost even with recorded tokens", () => {
    const f = fixture();
    f.admin.factory.lifetime = f.admin.factory.last_7_days = bundle(2000, 0, 0, 0);
    for (const u of f.admin.users) u.usage = u.last_7_days = bundle(1000, 0, 0, 0);
    const view = mount(f); const users = columns(view).users;
    for (const period of ["Last 7 days", "All time"]) {
      fireEvent.click(view.getByRole("button", { name: period }));
      expect(users.queryAllByRole("img")).toHaveLength(0);
      expect(users.getAllByTitle("Cost share unavailable: no recorded factory cost in the selected period")
        .map((share) => share.textContent)).toEqual(["—", "—"]);
    }
  });

  it("accepts empty users in both windows", () => {
    const f = fixture(); f.admin.users = []; const empty = mount(f);
    for (const period of ["Last 7 days", "All time"]) {
      fireEvent.click(empty.getByRole("button", { name: period }));
      expect(empty.getByText("No usage across the factory yet.")).toBeTruthy();
      expect(empty.queryByText(/Upgrade the API/)).toBeNull();
      expect(empty.queryByRole("table")).toBeNull();
    }
  });

  it("distinguishes no failures, no finished runs and unavailable recency", () => {
    const { self } = fixture();
    self.outcomes.lifetime = emptyOutcomes();
    const view = mount({ self });
    expect(view.getByText("No finished runs yet")).toBeTruthy();
    cleanup();
    self.outcomes.lifetime = outcomes({ finished: 4, completed: 4, failed: 0, cancelled: 0,
      plan_rejected: 0, base_diverged: 0, needs_landing: 0, fail_origins: {}, last_failed_at: null, last_failed_run_id: null });
    expect(mount({ self }).getByText("No recorded failures · 4 completed runs, none failed")).toBeTruthy();
    cleanup();
    self.outcomes.lifetime.last_failed_at = "invalid";
    expect(mount({ self }).getByText("Latest failure: unavailable")).toBeTruthy();
  });

  describe("PR counts (#3686)", () => {
    // Cost is complete in these windows unless a test says otherwise.
    const complete = (f: ReturnType<typeof fixture>) => {
      for (const s of [f.self, f.admin.factory]) {
        s.lifetime_subscription_run_count = 0; s.lifetime_unreported_run_count = 0;
        s.last7_subscription_run_count = 0; s.last7_unreported_run_count = 0;
      }
    };
    const withPRs = () => {
      const f = fixture(); complete(f);
      f.self.lifetime_pr_count = 4; f.self.last7_pr_count = 2;
      f.admin.factory.lifetime_pr_count = 8; f.admin.factory.last7_pr_count = 6;
      f.admin.users[0].pr_count = 5; f.admin.users[0].last7_pr_count = 2;
      f.admin.users[1].pr_count = 7; f.admin.users[1].last7_pr_count = 3;
      return f;
    };
    type Scope = ReturnType<typeof columns>["users"];
    const slot = (scope: Scope) => scope.getByText(/\d+ PRs?\b|runs with token usage/).textContent;
    const prCells = (users: Scope) => {
      const headers = users.getAllByRole("columnheader").map((h) => h.textContent);
      const idx = headers.indexOf("PRs");
      return { headers, idx, cells: users.getAllByRole("row").slice(1).map((r) => ({
        name: r.querySelector("td")!.textContent, pr: idx < 0 ? undefined : r.querySelectorAll("td")[idx].textContent })) };
    };

    it("renders the lifetime row for the personal and factory columns and the PR text alone for 7 days", () => {
      const view = mount({ ...withPRs(), initial: "lifetime" }); const { you, factory } = columns(view);
      expect(slot(you)).toBe("23 runs with token usage · 4 PRs · $308.64 per PR");
      expect(slot(factory)).toMatch(/^46 runs with token usage by 2 users since .*2026 · 8 PRs · \$308\.64 per PR$/);
      fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
      expect(slot(you)).toBe("2 PRs · $6.17 per PR");
      expect(slot(factory)).toBe("6 PRs · $6.17 per PR");
      expect(you.queryByText(/runs with token usage/)).toBeNull();
    });

    it("uses the singular for exactly one PR", () => {
      const f = withPRs(); f.self.last7_pr_count = 1;
      expect(slot(columns(mount(f)).you)).toBe("1 PR · $12.34 per PR");
    });

    it("renders a dash instead of a per-PR cost for zero PRs", () => {
      const f = withPRs(); f.self.last7_pr_count = 0;
      expect(slot(columns(mount(f)).you)).toBe("0 PRs · — per PR");
    });

    it("groups thousands of PRs with en-US separators", () => {
      const f = withPRs(); f.self.last7_pr_count = 1310;
      expect(slot(columns(mount(f)).you)).toBe("1,310 PRs · $0.01 per PR");
    });

    it("says at least when the cost is incomplete and reuses the cost disclosure title", () => {
      const f = fixture(); f.self.lifetime_pr_count = 4; f.self.last7_pr_count = 2;
      const view = mount({ self: f.self, initial: "lifetime" });
      const row = view.getByText(/runs with token usage/);
      expect(row.textContent).toBe("23 runs with token usage · 4 PRs · at least $308.64 per PR");
      const span = view.getByText("at least $308.64 per PR");
      expect(span.tagName).toBe("SPAN");
      const costNote = view.getByText(/^\+ 8 runs without a cost estimate/);
      expect(costNote.getAttribute("title")).toBeTruthy();
      expect(span.getAttribute("title")).toBe(costNote.getAttribute("title"));
      fireEvent.click(view.getByRole("button", { name: "Last 7 days" }));
      expect(view.getByText("at least $6.17 per PR").getAttribute("title"))
        .toBe(view.getByText(/^\+ 1 run without a cost estimate/).getAttribute("title"));
    });

    it("renders no PR text for an incomplete-cost zero count", () => {
      const f = fixture(); f.self.last7_pr_count = 0;
      const view = mount({ self: f.self });
      expect(view.getByText("0 PRs · — per PR")).toBeTruthy();
      expect(view.queryByText(/at least/)).toBeNull();
    });

    it("puts a PRs column right after Runs with per-row values and a summed total in both windows", () => {
      const view = mount(withPRs()); const { users } = columns(view);
      let t = prCells(users);
      expect(t.headers.slice(0, 3)).toEqual(["User", "Runs", "PRs"]);
      expect(Object.fromEntries(t.cells.map((c) => [c.name, c.pr])))
        .toEqual({ "other.person@example.com": "3", "owner.person@example.com": "2", "uzi total": "5" });
      fireEvent.click(view.getByRole("button", { name: "All time" }));
      t = prCells(users);
      expect(t.headers.slice(0, 3)).toEqual(["User", "Runs", "PRs"]);
      expect(Object.fromEntries(t.cells.map((c) => [c.name, c.pr]))).toEqual({ "other.person@example.com": "7", "owner.person@example.com": "5", "uzi total": "12" });
      // Position, not just header text: the PR cell sits directly after the Runs cell.
      const allRows = users.getAllByRole("row");
      const total = allRows[allRows.length - 1].querySelectorAll("td");
      expect(total[1].textContent).toBe("46");
      expect(total[2].textContent).toBe("12");
    });

    it("omits the PRs column and all PR text when the server reports no counts", () => {
      const f = fixture();
      const view = mount(f); const { you, factory, users } = columns(view);
      for (const period of ["Last 7 days", "All time"]) {
        fireEvent.click(view.getByRole("button", { name: period }));
        expect(view.container.textContent).not.toMatch(/\bPRs?\b/);
        expect(users.getAllByRole("columnheader").map((h) => h.textContent)).not.toContain("PRs");
      }
      fireEvent.click(view.getByRole("button", { name: "All time" }));
      expect(slot(you)).toBe("23 runs with token usage");
      expect(factory.getByText(/runs with token usage/).textContent).toMatch(/^46 runs with token usage by 2 users since .*2026$/);
    });

    it("omits the PRs column when one user lacks the selected window's count", () => {
      const f = withPRs(); delete f.admin.users[1].last7_pr_count;
      const view = mount(f); const { users } = columns(view);
      expect(users.getAllByRole("columnheader").map((h) => h.textContent)).not.toContain("PRs");
      expect(users.getAllByRole("row").every((r) => r.querySelectorAll("td").length === 0 || r.querySelectorAll("td").length === 9)).toBe(true);
      fireEvent.click(view.getByRole("button", { name: "All time" }));
      expect(users.getAllByRole("columnheader").map((h) => h.textContent)).toContain("PRs");
    });
  });

});

it.each([undefined, 3])("sums rejected outcomes with base_diverged=%s", (baseDiverged) => {
  const { self } = fixture();
  self.outcomes.last_7_days = outcomes({ finished: 20, completed: baseDiverged === undefined ? 8 : 5, cancelled: 0, plan_rejected: 2, base_diverged: baseDiverged });
  if (baseDiverged === undefined) delete self.outcomes.last_7_days.base_diverged;
  const view = mount({ self });
  const bar = within(view.getByRole("region", { name: "Your usage" })).getByRole("img");
  expect(bar.getAttribute("aria-label")).toBe(baseDiverged === undefined
    ? "Finished runs: 8 completed, 2 rejected, 10 failed"
    : "Finished runs: 5 completed, 5 rejected, 10 failed");
  expect((bar.children[1] as HTMLElement).style.width).toBe(baseDiverged === undefined ? "10%" : "25%");
});
