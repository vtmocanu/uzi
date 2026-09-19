// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { YourUsageCard, FactoryTotalCard, PerUserUsageTable } from "./UsageCards";
import type { SelfUsage, AdminUsage, RunOutcomes } from "../lib/api";
import type { ReactNode } from "react";

afterEach(cleanup);

const bundle = (inp: number, cr: number, out: number, cost: number) => ({
  input_tokens: inp,
  cache_read_tokens: cr,
  cache_creation_tokens: 0,
  output_tokens: out,
  cost_usd: cost,
  // PRD #1429 M1 (D7): a per-run/aggregate bundle carries cost_status; these mock bundles are
  // metered (a real dollar total). The literal type keeps it assignable to CostStatus | "".
  cost_status: "metered" as const,
});
// A RunOutcomes fixture (PRD #1293), mirroring bundle(): finished, then the four terminal
// counts, then the fail_origins map. Callers keep the fixture internally consistent
// (finished === completed + cancelled + planRejected + failed, sum(origins) === failed).
// needsLanding (issue #1418) is the trailing optional sub-cut of failed (default 0, so the
// pre-#1418 positional calls compile unchanged); keep needs_landing <= failed.
const outcomes = (
  finished: number,
  completed: number,
  cancelled: number,
  planRejected: number,
  failed: number,
  origins: Record<string, number> = {},
  needsLanding = 0,
): RunOutcomes => ({
  finished,
  completed,
  cancelled,
  plan_rejected: planRejected,
  failed,
  needs_landing: needsLanding,
  fail_origins: origins,
});
// The empty (nothing-finished) two-window shape, so a fixture that predates PRD #1293 keeps
// the FailedRunsBlock hidden (finished === 0) and behaves exactly as before.
const noOutcomes = () => ({ lifetime: outcomes(0, 0, 0, 0, 0), last_7_days: outcomes(0, 0, 0, 0, 0) });
const noAggregateCostCounts = {
  lifetime_subscription_run_count: 0,
  lifetime_unreported_run_count: 0,
  last7_subscription_run_count: 0,
  last7_unreported_run_count: 0,
} as const;
const noUserCostCounts = { subscription_run_count: 0, unreported_run_count: 0 } as const;
const wrap = (ui: ReactNode) => render(<MemoryRouter>{ui}</MemoryRouter>);

describe("YourUsageCard", () => {
  it("renders the lifetime total, breakdown, and last-7-days kicker", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_610_000, 16_100_000, 710_000, 26.4),
      last_7_days: bundle(200_000, 2_800_000, 100_000, 4.55),
      run_count: 23,
      outcomes: noOutcomes(),
      lifetime_subscription_run_count: 0,
      lifetime_unreported_run_count: 0,
      last7_subscription_run_count: 0,
      last7_unreported_run_count: 0,
    };
    const { container, getByText } = wrap(<YourUsageCard usage={usage} />);
    expect(getByText("Your usage")).toBeTruthy();
    // total = 1.61M + 16.1M + 0.71M = 18.42M
    expect(container.textContent).toContain("18.42M");
    expect(container.textContent).toContain("$26.40");
    expect(getByText(/Across/)).toBeTruthy();
    expect(getByText(/in the last 7 days/)).toBeTruthy();
  });

  it("shows the nothing-yet state (no fabricated 0) when run_count is 0", () => {
    const usage: SelfUsage = {
      lifetime: bundle(0, 0, 0, 0),
      last_7_days: bundle(0, 0, 0, 0),
      run_count: 0,
      outcomes: noOutcomes(),
      lifetime_subscription_run_count: 0,
      lifetime_unreported_run_count: 0,
      last7_subscription_run_count: 0,
      last7_unreported_run_count: 0,
    };
    const { getByText, container } = wrap(<YourUsageCard usage={usage} />);
    expect(getByText(/No usage recorded yet/)).toBeTruthy();
    // No "0 tokens" big number and no "Across N runs" kicker — nothing fabricated.
    expect(container.textContent).not.toContain("0 tokens");
    expect(container.textContent).not.toContain("Across");
  });

  it("renders a genuine $0 metered total as '$0.00' (PRD #1429 D7 supersedes Decision 8)", () => {
    // PRD #40 Decision 8 assumed $0 always meant subscription auth and hid it behind
    // "—". With zero subscription/unreported runs in this window, $0 IS the real
    // metered total (e.g. cache-only calls), so it now renders as an honest figure.
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 0), // nonzero tokens, zero cost
      last_7_days: bundle(0, 0, 0, 0),
      run_count: 2,
      outcomes: noOutcomes(),
      lifetime_subscription_run_count: 0,
      lifetime_unreported_run_count: 0,
      last7_subscription_run_count: 0,
      last7_unreported_run_count: 0,
    };
    const { container } = wrap(<YourUsageCard usage={usage} />);
    expect(container.textContent).toContain("$0.00");
    // No disclosure noise: no subscription/unreported runs exist in this window.
    expect(container.textContent).not.toContain("excluded from the $ total");
  });

  it("a MIXED aggregate discloses the subscription/unreported run counts beside the metered dollar figure", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 12.5),
      last_7_days: bundle(100_000, 0, 20_000, 1.25),
      run_count: 10,
      // 3 metered + 2 subscription + 1 unreported = 6 lifetime runs feeding this bundle.
      lifetime_subscription_run_count: 2,
      lifetime_unreported_run_count: 1,
      last7_subscription_run_count: 1,
      last7_unreported_run_count: 0,
      outcomes: noOutcomes(),
    };
    const { container } = wrap(<YourUsageCard usage={usage} />);
    // Positive: the metered dollar figures are still shown...
    expect(container.textContent).toContain("$12.50");
    expect(container.textContent).toContain("$1.25");
    // ...paired with an explicit disclosure of the excluded runs for BOTH windows, so
    // the $ figure is never read as the complete total.
    expect(container.textContent).toContain("2 subscription + 1 unreported runs excluded from the $ total");
    expect(container.textContent).toContain("1 subscription run excluded from the $ total");
  });

  it("keeps the 'see per-run detail →' arrow glued to 'detail' (no orphaned arrow)", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 1.23),
      last_7_days: bundle(100_000, 0, 50_000, 0.5),
      run_count: 3,
      outcomes: noOutcomes(),
      lifetime_subscription_run_count: 0,
      lifetime_unreported_run_count: 0,
      last7_subscription_run_count: 0,
      last7_unreported_run_count: 0,
    };
    const { container } = wrap(<YourUsageCard usage={usage} />);
    const link = container.querySelector('a[href="/runs"]');
    expect(link).toBeTruthy();
    // (a) whitespace-nowrap prevents the link itself from wrapping.
    expect(link?.className).toContain("whitespace-nowrap");
    // (b) positive pin: the arrow stays glued to "detail" by a genuine non-breaking
    // space — assert via the \u00A0 escape (not a literal glyph) so it stays greppable.
    expect(link?.textContent).toMatch(/detail\u00A0→/);
  });
});

describe("FactoryTotalCard + PerUserUsageTable", () => {
  const admin: AdminUsage = {
    factory: { lifetime: bundle(4_100_000, 37_500_000, 1_730_000, 64.23), last_7_days: bundle(0, 0, 0, 0), run_count: 54, outcomes: noOutcomes(), lifetime_subscription_run_count: 0, lifetime_unreported_run_count: 0, last7_subscription_run_count: 0, last7_unreported_run_count: 0 },
    users: [
      { user_id: "a", email: "big@x", usage: bundle(2_490_000, 22_400_000, 1_020_000, 37.83), run_count: 31, outcomes: outcomes(0, 0, 0, 0, 0), subscription_run_count: 0, unreported_run_count: 0 },
      { user_id: "b", email: "small@x", usage: bundle(1_610_000, 15_100_000, 710_000, 26.4), run_count: 23, outcomes: outcomes(0, 0, 0, 0, 0), subscription_run_count: 0, unreported_run_count: 0 },
    ],
    earliest_run: "2026-05-12T09:00:00Z",
  };

  it("factory card shows the total tokens + user count + since date", () => {
    const { getByText, container } = wrap(<FactoryTotalCard admin={admin} />);
    expect(getByText(/Factory total/)).toBeTruthy();
    expect(container.textContent).toContain("2 users");
    // "since <date>" from earliest_run (locale-formatted; assert the stable bits).
    expect(container.textContent).toContain("since");
    expect(container.textContent).toContain("2026");
  });

  it("omits the 'since' clause when earliest_run is null", () => {
    const { container } = wrap(<FactoryTotalCard admin={{ ...admin, earliest_run: null }} />);
    expect(container.textContent).not.toContain("since");
  });

  it("per-user table renders a row per user, a total row, and share bars by tokens", () => {
    const { getByText, container } = wrap(<PerUserUsageTable admin={admin} />);
    expect(getByText("big@x")).toBeTruthy();
    expect(getByText("small@x")).toBeTruthy();
    expect(getByText("uzi total")).toBeTruthy();
    // One share bar per user, labelled as a fraction of factory tokens.
    expect(container.querySelectorAll('[aria-label$="of factory tokens"]').length).toBe(2);
  });

  it("per-user SHARE% uses largest-remainder rounding so the column sums to exactly 100%", () => {
    // factory total 1000; user totals 905 / 85 / 10 -> raw 90.5 / 8.5 / 1.0.
    // Naive per-row Math.round = 91 + 9 + 1 = 101; Hamilton = 91 + 8 + 1 = 100.
    // The leftover point ties (0.5 vs 0.5); the tie-break awards it to the larger
    // raw total (905), pinning the exact per-row values below.
    const admin3: AdminUsage = {
      factory: { lifetime: bundle(1000, 0, 0, 0), last_7_days: bundle(0, 0, 0, 0), run_count: 3, outcomes: noOutcomes(), lifetime_subscription_run_count: 0, lifetime_unreported_run_count: 0, last7_subscription_run_count: 0, last7_unreported_run_count: 0 },
      users: [
        { user_id: "a", email: "a@x", usage: bundle(905, 0, 0, 0), run_count: 1, outcomes: outcomes(0, 0, 0, 0, 0), subscription_run_count: 0, unreported_run_count: 0 },
        { user_id: "b", email: "b@x", usage: bundle(85, 0, 0, 0), run_count: 1, outcomes: outcomes(0, 0, 0, 0, 0), subscription_run_count: 0, unreported_run_count: 0 },
        { user_id: "c", email: "c@x", usage: bundle(10, 0, 0, 0), run_count: 1, outcomes: outcomes(0, 0, 0, 0, 0), subscription_run_count: 0, unreported_run_count: 0 },
      ],
      earliest_run: null,
    };
    const { container } = wrap(<PerUserUsageTable admin={admin3} />);
    const labels = Array.from(container.querySelectorAll('[aria-label$="of factory tokens"]')).map((el) =>
      el.getAttribute("aria-label"),
    );
    expect(labels).toEqual([
      "91 percent of factory tokens",
      "8 percent of factory tokens",
      "1 percent of factory tokens",
    ]);
    // Bar width reads the same corrected value (not the naive 91/9/1).
    const firstBar = container.querySelector('[aria-label="91 percent of factory tokens"] > span') as HTMLElement | null;
    expect(firstBar?.style.width).toBe("91%");
  });
});

describe("FailedRunsBlock (PRD #1293)", () => {
  it("renders the rate, counts sentence, four-count legend, top causes, and bar aria-label", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 1.23),
      last_7_days: bundle(100_000, 0, 50_000, 0.5),
      run_count: 40,
      ...noAggregateCostCounts,
      outcomes: {
        // 106 / 1024 = 10.35% -> "10.4%"; 3 / 38 = 7.89% -> "7.9%".
        lifetime: outcomes(1024, 861, 45, 12, 106, {
          agent_failure: 48,
          run_timeout: 31,
          worker_lost: 14,
          rate_limited: 9,
          unknown: 4,
        }),
        last_7_days: outcomes(38, 33, 1, 1, 3, { agent_failure: 2, run_timeout: 1 }),
      },
    };
    const { container, getByText } = wrap(<YourUsageCard usage={usage} />);
    expect(getByText("Failed runs")).toBeTruthy();
    // Lifetime rate + counts sentence with the 7-day clause, both one decimal.
    expect(container.textContent).toContain("10.4%");
    expect(container.textContent).toContain("106 of 1024 finished runs");
    expect(container.textContent).toContain("7.9%");
    expect(container.textContent).toContain("(3 of 38) in the last 7 days");
    // Legend: the four outcome labels each beside their lifetime count.
    expect(container.textContent).toContain("completed 861");
    expect(container.textContent).toContain("cancelled 45");
    expect(container.textContent).toContain("plan rejected 12");
    expect(container.textContent).toContain("failed 106");
    // Top causes: the four largest by count, human-labelled; the 5th (unknown 4) is dropped.
    expect(container.textContent).toContain("agent failure 48");
    expect(container.textContent).toContain("run timeout 31");
    expect(container.textContent).toContain("worker lost 14");
    expect(container.textContent).toContain("rate limited 9");
    expect(container.textContent).not.toContain("unknown");
    // The stacked bar carries the same four counts as an accessible sentence.
    const bar = container.querySelector('[role="img"]');
    expect(bar?.getAttribute("aria-label")).toBe(
      "Finished runs: 861 completed, 45 cancelled, 12 plan rejected, 106 failed",
    );
  });

  it("hides the whole block when lifetime.finished === 0 (never a fabricated 0%)", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 1.23),
      last_7_days: bundle(0, 0, 0, 0),
      run_count: 5,
      ...noAggregateCostCounts,
      outcomes: { lifetime: outcomes(0, 0, 0, 0, 0), last_7_days: outcomes(0, 0, 0, 0, 0) },
    };
    const { container } = wrap(<YourUsageCard usage={usage} />);
    expect(container.textContent).not.toContain("Failed runs");
    expect(container.querySelector('[role="img"]')).toBeNull();
  });

  it("splits the failed bar into 'failed' and 'failed, needs landing' (issue #1418)", () => {
    // failed=10 with needs_landing=4: the plain 'failed' segment reflects 6 (10 - 4) and the
    // needs-landing sub-cut is its own adjacent segment of 4. finished = 85+3+2+10 = 100.
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 1.23),
      last_7_days: bundle(100_000, 0, 50_000, 0.5),
      run_count: 40,
      ...noAggregateCostCounts,
      outcomes: {
        lifetime: outcomes(100, 85, 3, 2, 10, { workflow_scope_missing: 4, agent_failure: 6 }, 4),
        last_7_days: outcomes(38, 33, 1, 1, 3, { agent_failure: 3 }, 0),
      },
    };
    const { container } = wrap(<YourUsageCard usage={usage} />);
    // The counts sentence and rate keep reading the FULL failed count (10), not the split.
    expect(container.textContent).toContain("10 of 100 finished runs");
    expect(container.textContent).toContain("10.0%");
    // Legend: the plain failed segment reflects 6, and the needs-landing sub-cut its own 4.
    expect(container.textContent).toContain("failed 6");
    expect(container.textContent).toContain("failed, needs landing 4");
    // The aria-label names the new bucket, with the split counts summing back to failed=10.
    const bar = container.querySelector('[role="img"]');
    expect(bar?.getAttribute("aria-label")).toBe(
      "Finished runs: 85 completed, 3 cancelled, 2 plan rejected, 6 failed, 4 failed, needs landing",
    );
  });

  it("shows no needs-landing segment when needs_landing === 0 (no zero clutter)", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 1.23),
      last_7_days: bundle(100_000, 0, 50_000, 0.5),
      run_count: 40,
      ...noAggregateCostCounts,
      outcomes: {
        lifetime: outcomes(100, 85, 3, 2, 10, { agent_failure: 10 }, 0),
        last_7_days: outcomes(38, 33, 1, 1, 3, { agent_failure: 3 }, 0),
      },
    };
    const { container } = wrap(<YourUsageCard usage={usage} />);
    expect(container.textContent).not.toContain("needs landing");
    // The plain failed segment carries the full failed count when nothing needs landing.
    expect(container.textContent).toContain("failed 10");
    const bar = container.querySelector('[role="img"]');
    expect(bar?.getAttribute("aria-label")).toBe(
      "Finished runs: 85 completed, 3 cancelled, 2 plan rejected, 10 failed",
    );
  });
});

describe("PerUserUsageTable failed columns (PRD #1293)", () => {
  const adminRich: AdminUsage = {
    factory: {
      lifetime: bundle(4_100_000, 37_500_000, 1_730_000, 64.23),
      last_7_days: bundle(0, 0, 0, 0),
      run_count: 54,
      ...noAggregateCostCounts,
      outcomes: {
        lifetime: outcomes(1233, 1041, 52, 14, 126, { agent_failure: 55 }),
        last_7_days: outcomes(46, 40, 1, 1, 4, {}),
      },
    },
    users: [
      // A user with finished runs (106 / 1024 = 10.4%)…
      { user_id: "a", email: "big@x", usage: bundle(2_490_000, 22_400_000, 1_020_000, 37.83), run_count: 31, ...noUserCostCounts, outcomes: outcomes(1024, 861, 45, 12, 106, { agent_failure: 48 }) },
      // …and a broken-token user: outcomes but no usage (D5), finished 0 -> "—".
      { user_id: "z", email: "broke@x", usage: bundle(0, 0, 0, 0), run_count: 0, ...noUserCostCounts, outcomes: outcomes(0, 0, 0, 0, 0) },
    ],
    earliest_run: null,
  };

  it("orders the header exactly User · Runs · Failed · Fail rate · Tokens · Out · Cost · Share (D8)", () => {
    const { container } = wrap(<PerUserUsageTable admin={adminRich} />);
    const headers = Array.from(container.querySelectorAll("thead th")).map((th) => th.textContent);
    expect(headers).toEqual(["User", "Runs", "Failed", "Fail rate", "Tokens", "Out", "Cost", "Share"]);
  });

  it("puts the exact fraction on every fail-rate cell's title (D9)", () => {
    const { container } = wrap(<PerUserUsageTable admin={adminRich} />);
    const cell = container.querySelector('td[title="106 of 1024 finished runs"]');
    expect(cell).toBeTruthy();
    expect(cell?.textContent).toBe("10.4%");
    // The factory total row carries the factory's fraction too.
    expect(container.querySelector('td[title="126 of 1233 finished runs"]')).toBeTruthy();
  });

  it("renders '—' in the fail-rate cell when finished === 0", () => {
    const { container } = wrap(<PerUserUsageTable admin={adminRich} />);
    const zeroCell = container.querySelector('td[title="0 of 0 finished runs"]');
    expect(zeroCell?.textContent).toBe("—");
  });
});

describe("FactoryTotalCard + PerUserUsageTable cost disclosure (PRD #1429 D7)", () => {
  it("an all-metered factory (0 subscription/unreported) discloses nothing", () => {
    const admin: AdminUsage = {
      factory: {
        lifetime: bundle(1_000_000, 0, 200_000, 12.5),
        last_7_days: bundle(0, 0, 0, 0),
        run_count: 4,
        lifetime_subscription_run_count: 0,
        lifetime_unreported_run_count: 0,
        last7_subscription_run_count: 0,
        last7_unreported_run_count: 0,
        outcomes: noOutcomes(),
      },
      users: [
        { user_id: "a", email: "a@x", usage: bundle(1_000_000, 0, 200_000, 12.5), run_count: 4, subscription_run_count: 0, unreported_run_count: 0, outcomes: outcomes(0, 0, 0, 0, 0) },
      ],
      earliest_run: null,
    };
    const { container: factoryContainer } = wrap(<FactoryTotalCard admin={admin} />);
    expect(factoryContainer.textContent).toContain("$12.50");
    expect(factoryContainer.textContent).not.toContain("excluded from the $ total");

    const { container: tableContainer } = wrap(<PerUserUsageTable admin={admin} />);
    expect(tableContainer.textContent).toContain("$12.50");
    expect(tableContainer.textContent).not.toContain("excluded from the $ total");
  });

  it("factory card discloses a MIXED total's excluded subscription/unreported runs beside its metered $", () => {
    const admin: AdminUsage = {
      factory: {
        lifetime: bundle(1_000_000, 0, 200_000, 12.5),
        last_7_days: bundle(0, 0, 0, 0),
        run_count: 7,
        lifetime_subscription_run_count: 2,
        lifetime_unreported_run_count: 1,
        last7_subscription_run_count: 0,
        last7_unreported_run_count: 0,
        outcomes: noOutcomes(),
      },
      users: [
        { user_id: "a", email: "a@x", usage: bundle(1_000_000, 0, 200_000, 12.5), run_count: 7, subscription_run_count: 2, unreported_run_count: 1, outcomes: outcomes(0, 0, 0, 0, 0) },
      ],
      earliest_run: null,
    };
    const { container } = wrap(<FactoryTotalCard admin={admin} />);
    // Positive: the metered figure is still shown, paired with the disclosure.
    expect(container.textContent).toContain("$12.50");
    expect(container.textContent).toContain("2 subscription + 1 unreported runs excluded from the $ total");
  });

  it("per-user table discloses each row's OWN excluded runs, and the total row discloses the factory sum", () => {
    const admin: AdminUsage = {
      factory: {
        lifetime: bundle(4_000_000, 0, 800_000, 40),
        last_7_days: bundle(0, 0, 0, 0),
        run_count: 20,
        lifetime_subscription_run_count: 3,
        lifetime_unreported_run_count: 1,
        last7_subscription_run_count: 0,
        last7_unreported_run_count: 0,
        outcomes: noOutcomes(),
      },
      users: [
        // This user's own 2 subscription runs are excluded from their $30 figure.
        { user_id: "a", email: "big@x", usage: bundle(3_000_000, 0, 600_000, 30), run_count: 12, subscription_run_count: 2, unreported_run_count: 0, outcomes: outcomes(0, 0, 0, 0, 0) },
        // This user is fully metered — no disclosure on their row.
        { user_id: "b", email: "small@x", usage: bundle(1_000_000, 0, 200_000, 10), run_count: 8, subscription_run_count: 0, unreported_run_count: 0, outcomes: outcomes(0, 0, 0, 0, 0) },
      ],
      earliest_run: null,
    };
    const { container } = wrap(<PerUserUsageTable admin={admin} />);
    // Positive: both users' metered dollar figures show...
    expect(container.textContent).toContain("$30.00");
    expect(container.textContent).toContain("$10.00");
    // ...the disclosed user's row names its own excluded count...
    expect(container.textContent).toContain("2 subscription runs excluded from the $ total");
    // ...and the total row discloses the factory-wide sum (3 subscription + 1 unreported),
    // not just this one row's count.
    expect(container.textContent).toContain("3 subscription + 1 unreported runs excluded from the $ total");
  });
});
