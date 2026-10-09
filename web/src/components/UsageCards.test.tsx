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
  finished: 100, completed: 85, cancelled: 3, plan_rejected: 2, failed: 10,
  needs_landing: 4, fail_origins: { agent_failure: 6, run_timeout: 2, workflow_scope_missing: 2 },
  last_failed_at: "2026-10-07T06:00:00Z", last_failed_run_id: "self-failure",
  last_failed_origin: "agent_failure", last_failed_user_id: null,
  completed_since_last_failure: 14, ...overrides,
});
const emptyOutcomes = (): RunOutcomes => outcomes({
  finished: 0, completed: 0, cancelled: 0, plan_rejected: 0, failed: 0,
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
      last_7_days: outcomes({ finished: 20, completed: 17, cancelled: 1, plan_rejected: 0,
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
        lifetime: outcomes({ finished: 200, completed: 170, cancelled: 6, plan_rejected: 4,
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
  it("ranks subscription users by total tokens before metered cost in both windows", () => {
    const f = fixture();
    f.admin.users[0].usage = f.admin.users[0].last_7_days = bundle(900, 0, 100, 0);
    f.admin.users[1].usage = f.admin.users[1].last_7_days = bundle(0, 0, 100, 50);
    f.admin.factory.lifetime = f.admin.factory.last_7_days = bundle(900, 0, 200, 50);
    const view = mount(f);
    for (const period of ["Last 7 days", "All time"]) {
      fireEvent.click(view.getByRole("button", { name: period }));
      const rows = columns(view).users.getAllByRole("row").slice(1, -1);
      expect(rows.map((row) => row.querySelector("td")?.textContent)).toEqual([
        "owner.person@example.com", "other.person@example.com",
      ]);
      expect(within(rows[0]).getByRole("img").getAttribute("aria-label")).toBe("91 percent of factory tokens");
    }
  });
  it("breaks equal token totals by cost, output and user ID", () => {
    const f = fixture();
    f.admin.users = ["d", "c", "b", "a"].map((id, i) => ({
      ...f.admin.users[0], user_id: id, email: `${id}@example.com`,
      last_7_days: bundle([80, 90, 80, 80][i], 0, [20, 10, 20, 20][i], [0, 2, 2, 2][i]),
    }));
    f.admin.factory.last_7_days = bundle(330, 0, 70, 6);
    const rows = columns(mount(f)).users.getAllByRole("row").slice(1, -1);
    expect(rows.map((row) => row.querySelector("td")?.textContent)).toEqual([
      "a@example.com", "b@example.com", "c@example.com", "d@example.com",
    ]);
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
    for (const name of ["Metered cost", "Failed runs rate", "Tokens"]) {
      expect(view.getAllByRole("heading", { name, level: 4 })).toHaveLength(2);
    }
  });

  it.each([
    [0, 1, "+ 1 unreported run", "Unreported runs: cost unavailable"],
    [2, 0, "+ 2 Codex sub runs", "Codex subscription runs: no per-run cost reported"],
    [1, 2, "+ 1 Codex sub run and 2 unreported runs", "Codex subscription runs: no per-run cost reported; Unreported runs: cost unavailable"],
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
    self.outcomes.lifetime = outcomes({ finished: 3, completed: 2, failed: 1, cancelled: 0, plan_rejected: 0, needs_landing: 0, fail_origins: { agent_failure: 1 } });
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
    expect(you.queryByText(/^plan rejected/)).toBeNull();
    const bar = you.getByRole("img");
    expect(bar.getAttribute("aria-label")).toBe("Finished runs: 17 completed, 1 cancelled, 2 failed");
    expect(bar.children).toHaveLength(3);
    expect((bar.lastElementChild as HTMLElement).style.width).toBe("10%");
    expect(you.getByText("1 failed runs with recoverable work. May include work already landed.")).toBeTruthy();
    // Paired with the positive replacement note and full failed bar assertions.
    expect(bar.getAttribute("aria-label")).not.toContain("need landing");
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(you.getByText("plan rejected", { exact: false })).toBeTruthy();
    expect(bar.children).toHaveLength(4);
    expect(you.getByText("4 failed runs with recoverable work. May include work already landed.")).toBeTruthy();
  });
  it("hides recoverability at zero and causes when no failures exist", () => {
    const { self } = fixture();
    self.outcomes.last_7_days = outcomes({ finished: 20, completed: 20, cancelled: 0, plan_rejected: 0, failed: 0, needs_landing: 0, fail_origins: {} });
    const view = mount({ self });
    expect(view.queryByText(/failed runs with recoverable work/)).toBeNull();
    expect(view.queryByText(/Top causes:/)).toBeNull(); expect(view.getByText("0.0%")).toBeTruthy();
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(view.getByText(/4 failed runs with recoverable work/)).toBeTruthy(); expect(view.getByText(/Top causes:/)).toBeTruthy();
  });
  it("pairs each selected cost with its own exclusions and full title (#1429 D7)", () => {
    const view = mount({ self: fixture().self });
    expect(view.getByText("+ 1 Codex sub run").getAttribute("title")).toBe("Codex subscription runs: no per-run cost reported");
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    expect(view.getByText("+ 8 Codex sub runs and 2 unreported runs").getAttribute("title")).toBe("Codex subscription runs: no per-run cost reported; Unreported runs: cost unavailable");
    expect(view.queryByText("+ 1 Codex sub run")).toBeNull();
  });
  it("shows real zero metered cost without exclusions when the window is fully metered", () => {
    const { self } = fixture(); self.last_7_days.cost_usd = 0; self.last7_subscription_run_count = 0;
    const view = mount({ self }); expect(view.getByText("$0.00")).toBeTruthy(); expect(view.queryByText(/^\+ \d+ (Codex sub|unreported) runs?/)).toBeNull();
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
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual(["50 percent of factory tokens", "50 percent of factory tokens"]);
    const row = users.getByText("owner.person@example.com").closest("tr")!;
    expect(row.querySelectorAll("td")[3].getAttribute("title")).toBe("10 of 100 finished runs");
    expect(row.textContent).toContain("+ 4 Codex sub runs and 1 unreported run");
  });
  it("rounds per-user token shares to exactly 100% with largest remainders", () => {
    const f = fixture();
    f.admin.factory.lifetime = bundle(1000, 0, 0, 0);
    f.admin.users = [905, 85, 10].map((tokens, i) => ({
      ...f.admin.users[0], user_id: `share-${i}`, email: `share-${i}@example.com`,
      usage: bundle(tokens, 0, 0, 0),
    }));
    const view = mount({ ...f, initial: "lifetime" });
    const users = within(view.getByRole("region", { name: /^Per-user usage,/ }));
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual([
      "91 percent of factory tokens", "8 percent of factory tokens", "1 percent of factory tokens",
    ]);
    expect((users.getAllByRole("img")[0].firstElementChild as HTMLElement).style.width).toBe("91%");
  });

  it("selects metrics before sorting by tokens, cost, output and user ID in both directions", () => {
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
    expect(recent.map((r) => r[0])).toEqual(["c@example.com", "a@example.com", "b@example.com", "d@example.com", "e@example.com", "uzi total"]);
    expect(recent[0]).toEqual(["c@example.com", "2", "0", "—", "6h", "20", "20", "$1.00+ 1 Codex sub run", "67%"]);
    expect(recent[1][7]).toBe("$2.00+ 1 unreported run");
    expect(recent[2].slice(1, 8)).toEqual(["0", "0", "—", "6h", "0", "0", "$0.00"]);
    expect(recent[4].slice(1, 4)).toEqual(["0", "1", "50.0%"]);
    expect(recent[5].slice(1, 4)).toEqual(["3", "1", "50.0%"]);
    expect(recent[5].slice(5, 8)).toEqual(["30", "30", "$3.00+ 1 Codex sub run and 1 unreported run"]);
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual([
      "67 percent of factory tokens", "33 percent of factory tokens", "0 percent of factory tokens",
      "0 percent of factory tokens", "0 percent of factory tokens",
    ]);
    expect(users.getAllByRole("row")[5].querySelectorAll("td")[3].title).toBe("1 of 2 finished runs");
    const totalRecency = recent[5][4];
    fireEvent.click(view.getByRole("button", { name: "All time" }));
    const lifetime = rowCells();
    expect(lifetime.map((r) => r[0])).toEqual(["c@example.com", "a@example.com", "b@example.com", "d@example.com", "e@example.com", "uzi total"]);
    expect(lifetime[1].slice(1, 4)).toEqual(["4", "10", "10.0%"]);
    expect(lifetime[2][7]).toBe("$2.00+ 2 Codex sub runs");
    expect(lifetime[5].slice(1, 4)).toEqual(["12", "20", "10.0%"]);
    expect(lifetime[5].slice(5, 8)).toEqual(["70", "70", "$6.00+ 2 Codex sub runs and 1 unreported run"]);
    expect(lifetime[5][4]).toBe(totalRecency);
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual([
      "43 percent of factory tokens", "29 percent of factory tokens", "28 percent of factory tokens",
      "0 percent of factory tokens", "0 percent of factory tokens",
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

  it("accepts empty users in both windows and renders zero shares for a zero factory", () => {
    const f = fixture(); f.admin.factory.last_7_days = bundle(0, 0, 0, 0);
    for (const u of f.admin.users) u.last_7_days = bundle(0, 0, 0, 0);
    const view = mount(f); const users = columns(view).users;
    expect(users.getAllByRole("img").map((bar) => bar.getAttribute("aria-label"))).toEqual([
      "0 percent of factory tokens", "0 percent of factory tokens",
    ]);
    cleanup(); f.admin.users = []; const empty = mount(f);
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
      plan_rejected: 0, needs_landing: 0, fail_origins: {}, last_failed_at: null, last_failed_run_id: null });
    expect(mount({ self }).getByText("No recorded failures · 4 completed runs, none failed")).toBeTruthy();
    cleanup();
    self.outcomes.lifetime.last_failed_at = "invalid";
    expect(mount({ self }).getByText("Latest failure: unavailable")).toBeTruthy();
  });

});
