// @vitest-environment jsdom
// PRD #3881 M2: the per-user rows and factory total shared by both Overview usage cards.
import { afterEach, describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { harnessShare, perUserRows } from "./usageByUser";
import { mockApi } from "../mocks/mockApi";
import { failureAgo } from "../components/usageParts";
import { UsageCard } from "../components/UsageCards";
import type { AdminUsage, AdminUsageUser, RunOutcomes, RunUsage, SelfUsage } from "./api";

afterEach(() => { cleanup(); vi.useRealTimers(); });

const bundle = (cost: number, tokens = 100): RunUsage => ({
  input_tokens: tokens, cache_read_tokens: 0, cache_creation_tokens: 0,
  output_tokens: 0, cost_usd: cost, cost_status: "metered",
});
const outcomes = (finished: number, failed: number, over: Partial<RunOutcomes> = {}): RunOutcomes => ({
  finished, completed: finished - failed, cancelled: 0, plan_rejected: 0, base_diverged: 0, failed,
  needs_landing: 0, fail_origins: failed > 0 ? { agent_failure: failed } : {},
  last_failed_at: failed > 0 ? "2026-10-07T06:00:00Z" : null, last_failed_run_id: null,
  last_failed_origin: failed > 0 ? "agent_failure" : null, last_failed_user_id: null,
  completed_since_last_failure: null, ...over,
});

// Three users. Runs differ from finished runs, so a fail rate over runs would be wrong.
// "b" and "c" tie on 7-day cost and tokens, so the user_id tie-break orders them.
interface Spec {
  id: string; runs: number; runs7: number; cost: number; cost7: number;
  fin: number; failed: number; fin7: number; failed7: number; prs: number; prs7: number;
}
const SPECS: Spec[] = [
  { id: "a", runs: 20, runs7: 6, cost: 30, cost7: 3, fin: 15, failed: 3, fin7: 5, failed7: 1, prs: 9, prs7: 2 },
  { id: "c", runs: 10, runs7: 4, cost: 10, cost7: 7, fin: 8, failed: 1, fin7: 3, failed7: 1, prs: 4, prs7: 1 },
  { id: "b", runs: 5, runs7: 3, cost: 10, cost7: 7, fin: 4, failed: 0, fin7: 2, failed7: 0, prs: 2, prs7: 1 },
];
const sum = (f: (s: Spec) => number) => SPECS.reduce((n, s) => n + f(s), 0);

function fixture(): { self: SelfUsage; admin: AdminUsage } {
  const users: AdminUsageUser[] = SPECS.map((s) => ({
    user_id: s.id, email: `${s.id}@example.com`,
    usage: bundle(s.cost), run_count: s.runs, outcomes: outcomes(s.fin, s.failed),
    subscription_run_count: 0, unreported_run_count: 0,
    last_7_days: bundle(s.cost7), last7_run_count: s.runs7, last7_outcomes: outcomes(s.fin7, s.failed7),
    last7_subscription_run_count: 0, last7_unreported_run_count: 0,
    pr_count: s.prs, last7_pr_count: s.prs7,
  }));
  const factory: SelfUsage = {
    lifetime: bundle(sum((s) => s.cost), sum(() => 100)), last_7_days: bundle(sum((s) => s.cost7), sum(() => 100)),
    run_count: sum((s) => s.runs),
    lifetime_subscription_run_count: 0, lifetime_unreported_run_count: 0,
    last7_subscription_run_count: 0, last7_unreported_run_count: 0,
    lifetime_pr_count: sum((s) => s.prs), last7_pr_count: sum((s) => s.prs7),
    outcomes: {
      lifetime: outcomes(sum((s) => s.fin), sum((s) => s.failed)),
      last_7_days: outcomes(sum((s) => s.fin7), sum((s) => s.failed7)),
    },
  };
  return { self: factory, admin: { factory, users, earliest_run: null } };
}

const micro = (usd: number) => Math.round(usd * 1e6);

describe("perUserRows", () => {
  it("sums the rows to the factory row in both windows", () => {
    const { admin } = fixture();
    const life = perUserRows(admin, "lifetime");
    expect(life.factoryRuns).toBe(admin.factory.run_count);
    expect(life.rows.reduce((n, r) => n + r.selectedRuns, 0)).toBe(life.factoryRuns);
    expect(life.factoryPRs).toBe(life.rows.reduce((n, r) => n + r.selectedPRs!, 0));
    expect(life.factoryOutcomes.failed).toBe(life.rows.reduce((n, r) => n + r.selectedOutcomes.failed, 0));
    expect(life.rows.reduce((n, r) => n + micro(r.b.cost), 0)).toBe(micro(life.factory.cost));

    const week = perUserRows(admin, "last_7_days");
    expect(week.factoryRuns).toBe(sum((s) => s.runs7));
    expect(week.factoryPRs).toBe(admin.factory.last7_pr_count);
    expect(week.factoryOutcomes.failed).toBe(week.rows.reduce((n, r) => n + r.selectedOutcomes.failed, 0));
    expect(week.rows.reduce((n, r) => n + micro(r.b.cost), 0)).toBe(micro(week.factory.cost));
    expect(week.factoryRuns).not.toBe(admin.factory.run_count);
  });

  it("takes the lifetime factory runs from the factory count, not the row sum", () => {
    const { admin } = fixture();
    admin.factory.run_count = 50;
    expect(perUserRows(admin, "lifetime").factoryRuns).toBe(50);
    expect(perUserRows(admin, "last_7_days").factoryRuns).toBe(sum((s) => s.runs7));
  });

  it("takes the fail rate's denominator from finished runs, not runs", () => {
    const { admin } = fixture();
    const a = perUserRows(admin, "lifetime").rows.find((r) => r.user_id === "a")!;
    expect(a.selectedRuns).toBe(20);
    expect(a.selectedOutcomes.finished).toBe(15);
    expect(a.selectedOutcomes.finished).not.toBe(a.selectedRuns);
    const w = perUserRows(admin, "last_7_days").rows.find((r) => r.user_id === "a")!;
    expect([w.selectedRuns, w.selectedOutcomes.finished]).toEqual([6, 5]);
  });

  it("uses the same lifetime outcomes for the last failure in both windows", () => {
    const { admin } = fixture();
    const life = perUserRows(admin, "lifetime").rows;
    const week = perUserRows(admin, "last_7_days").rows;
    for (const r of life) {
      const other = week.find((w) => w.user_id === r.user_id)!;
      expect(r.outcomes).toBe(admin.users.find((u) => u.user_id === r.user_id)!.outcomes);
      expect(other.outcomes).toBe(r.outcomes);
    }
  });

  it("is unavailable on an older API without the seven-day fields, and returns no rows", () => {
    const { admin } = fixture();
    delete admin.users[1].last7_run_count;
    const week = perUserRows(admin, "last_7_days");
    expect(week.available).toBe(false);
    expect(week.rows).toEqual([]);
    const life = perUserRows(admin, "lifetime");
    expect(life.available).toBe(false);
    expect(life.rows).toHaveLength(3);
  });

  it("drops the PR total, never a partial sum, when one row lacks its count", () => {
    const { admin } = fixture();
    delete admin.users[0].pr_count;
    const life = perUserRows(admin, "lifetime");
    expect(life.showPRs).toBe(false);
    expect(life.factoryPRs).toBeNull();
    expect(life.rows.find((r) => r.user_id === "a")!.selectedPRs).toBeUndefined();
    // The other window is judged on its own counts.
    expect(perUserRows(admin, "last_7_days").factoryPRs).toBe(sum((s) => s.prs7));
  });

  it("sorts by cost, then tokens, then output, then user id", () => {
    const { admin } = fixture();
    expect(perUserRows(admin, "lifetime").rows.map((r) => r.user_id)).toEqual(["a", "b", "c"]);
    expect(perUserRows(admin, "last_7_days").rows.map((r) => r.user_id)).toEqual(["b", "c", "a"]);
    admin.users.find((u) => u.user_id === "c")!.last_7_days = bundle(7, 500);
    expect(perUserRows(admin, "last_7_days").rows.map((r) => r.user_id)).toEqual(["c", "b", "a"]);
    // Equal totals (100): the larger output wins, ahead of the user id.
    admin.users.find((u) => u.user_id === "c")!.last_7_days = { ...bundle(7, 99), output_tokens: 1 };
    admin.users.find((u) => u.user_id === "b")!.last_7_days = bundle(7, 100);
    expect(perUserRows(admin, "last_7_days").rows.map((r) => r.user_id)).toEqual(["c", "b", "a"]);
  });
});

describe("harnessShare (#3876)", () => {
  it("splits 7 Claude / 3 Codex as 70 / 30", () => {
    expect(harnessShare({ claude: 7, codex: 3 })).toEqual({ claude: 70, codex: 30 });
  });

  it("rounds 1 Claude / 2 Codex to 33 / 67, summing to 100", () => {
    const share = harnessShare({ claude: 1, codex: 2 })!;
    expect(share).toEqual({ claude: 33, codex: 67 });
    expect(share.claude + share.codex).toBe(100);
  });

  it("is null for a window with no runs or no split", () => {
    expect(harnessShare({ claude: 0, codex: 0 })).toBeNull();
    expect(harnessShare(undefined)).toBeNull();
  });

  it("gives Codex the rounding remainder on a half-way tie (1 Claude / 7 Codex, 12.5%, is 13 / 87, not 13 / 88)", () => {
    expect(harnessShare({ claude: 1, codex: 7 })).toEqual({ claude: 13, codex: 87 });
  });

  it("rounds 1 Codex run of 201 to 100 / 0", () => {
    expect(harnessShare({ claude: 200, codex: 1 })).toEqual({ claude: 100, codex: 0 });
  });
});

describe("perUserRows harness split (#3876)", () => {
  const withHarness = (admin: AdminUsage) => admin.users.forEach((u, i) => {
    u.harness_runs = { claude: u.run_count - i, codex: i };
    u.last7_harness_runs = { claude: u.last7_run_count! - i, codex: i };
  });

  it("shows the column when every row reports the selected window's split", () => {
    const { admin } = fixture();
    withHarness(admin);
    for (const w of ["lifetime", "last_7_days"] as const) {
      const r = perUserRows(admin, w);
      expect(r.showHarness).toBe(true);
      for (const row of r.rows) {
        const u = admin.users.find((x) => x.user_id === row.user_id)!;
        expect(row.selectedHarness).toBe(w === "lifetime" ? u.harness_runs : u.last7_harness_runs);
      }
    }
  });

  it("hides it when one row lacks the selected window's split, judging each window on its own", () => {
    const { admin } = fixture();
    withHarness(admin);
    delete admin.users[0].last7_harness_runs;
    expect(perUserRows(admin, "last_7_days").showHarness).toBe(false);
    expect(perUserRows(admin, "lifetime").showHarness).toBe(true);
    withHarness(admin);
    delete admin.users[2].harness_runs;
    expect(perUserRows(admin, "lifetime").showHarness).toBe(false);
    expect(perUserRows(admin, "last_7_days").showHarness).toBe(true);
  });

  it("hides it on an older API without the split", () => {
    const { admin } = fixture();
    expect(perUserRows(admin, "lifetime").showHarness).toBe(false);
    expect(perUserRows(admin, "last_7_days").showHarness).toBe(false);
  });

  it("gives every mock user a split summing to their run counts, one of them Claude-only", async () => {
    const admin = await mockApi.getAdminUsage();
    for (const u of admin.users) {
      expect(u.harness_runs!.claude + u.harness_runs!.codex).toBe(u.run_count);
      expect(u.last7_harness_runs!.claude + u.last7_harness_runs!.codex).toBe(u.last7_run_count);
    }
    expect(admin.users.some((u) => u.harness_runs!.codex === 0 && u.last7_harness_runs!.codex === 0)).toBe(true);
  });
});

describe("failureAgo", () => {
  const now = Date.parse("2026-10-07T09:30:00Z");
  const at = (iso: string | null, over: Partial<RunOutcomes> = {}) => outcomes(10, 1, { last_failed_at: iso, ...over });

  it("appends ago to durations only", () => {
    expect(failureAgo(at("2026-10-07T06:30:00Z"), now)).toBe("3h ago");
    expect(failureAgo(at("2026-10-07T09:29:45Z"), now)).toBe("<1m ago");
    expect(failureAgo(at("2026-10-07T09:10:00Z"), now)).toBe("20m ago");
    expect(failureAgo(at("2026-10-04T09:30:00Z"), now)).toBe("3d ago");
  });

  it("passes the non-duration words through unchanged", () => {
    expect(failureAgo(at(null), now)).toBe("Unavailable");
    expect(failureAgo(at("not-a-date"), now)).toBe("Unavailable");
    expect(failureAgo(outcomes(0, 0), now)).toBe("–");
    expect(failureAgo(outcomes(5, 0), now)).toBe("no failures");
  });
});

describe("the current Overview's UsageCard", () => {
  const mount = (admin: AdminUsage) =>
    render(createElement(MemoryRouter, null, createElement(UsageCard, {
      self: admin.factory, admin, window: "lifetime", onWindowChange: () => {},
    })));

  const TOTAL_CELLS = ["uzi total", "35", "4", "14.8%", "3h", "300", "0", "$50.00", " "];

  it("keeps its window toggle right-aligned", () => {
    mount(fixture().admin);
    expect(screen.getByRole("group", { name: "Usage reporting period" }).className).toContain("ml-auto");
  });

  it("hides the PRs header and the total's PR cell when a row lacks its count", () => {
    const { admin } = fixture();
    delete admin.users[0].pr_count;
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-07T09:00:00Z"));
    mount(admin);
    const users = within(screen.getByRole("region", { name: "Per-user usage, all time" }));
    const headers = users.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).not.toContain("PRs");
    expect(headers).toHaveLength(9);
    const rows = users.getAllByRole("row");
    const total = rows[rows.length - 1];
    // The total row's remaining cells keep their values and order: label, Runs, Failed, Fail
    // rate, Since last failure, Tokens, Out, Cost, Cost share (blank).
    expect(within(total).getAllByRole("cell").map((c) => c.textContent)).toEqual(TOTAL_CELLS);
  });
});
