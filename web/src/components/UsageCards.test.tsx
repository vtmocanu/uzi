// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { YourUsageCard, FactoryTotalCard, PerUserUsageTable } from "./UsageCards";
import type { SelfUsage, AdminUsage } from "../lib/api";
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
const wrap = (ui: ReactNode) => render(<MemoryRouter>{ui}</MemoryRouter>);

describe("YourUsageCard", () => {
  it("renders the lifetime total, breakdown, and last-7-days kicker", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_610_000, 16_100_000, 710_000, 26.4),
      last_7_days: bundle(200_000, 2_800_000, 100_000, 4.55),
      run_count: 23,
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
    const usage: SelfUsage = { lifetime: bundle(0, 0, 0, 0), last_7_days: bundle(0, 0, 0, 0), run_count: 0, lifetime_subscription_run_count: 0, lifetime_unreported_run_count: 0, last7_subscription_run_count: 0, last7_unreported_run_count: 0 };
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
    factory: { lifetime: bundle(4_100_000, 37_500_000, 1_730_000, 64.23), last_7_days: bundle(0, 0, 0, 0), run_count: 54, lifetime_subscription_run_count: 0, lifetime_unreported_run_count: 0, last7_subscription_run_count: 0, last7_unreported_run_count: 0 },
    users: [
      { user_id: "a", email: "big@x", usage: bundle(2_490_000, 22_400_000, 1_020_000, 37.83), run_count: 31, subscription_run_count: 0, unreported_run_count: 0 },
      { user_id: "b", email: "small@x", usage: bundle(1_610_000, 15_100_000, 710_000, 26.4), run_count: 23, subscription_run_count: 0, unreported_run_count: 0 },
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
      factory: { lifetime: bundle(1000, 0, 0, 0), last_7_days: bundle(0, 0, 0, 0), run_count: 3, lifetime_subscription_run_count: 0, lifetime_unreported_run_count: 0, last7_subscription_run_count: 0, last7_unreported_run_count: 0 },
      users: [
        { user_id: "a", email: "a@x", usage: bundle(905, 0, 0, 0), run_count: 1, subscription_run_count: 0, unreported_run_count: 0 },
        { user_id: "b", email: "b@x", usage: bundle(85, 0, 0, 0), run_count: 1, subscription_run_count: 0, unreported_run_count: 0 },
        { user_id: "c", email: "c@x", usage: bundle(10, 0, 0, 0), run_count: 1, subscription_run_count: 0, unreported_run_count: 0 },
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
      },
      users: [
        { user_id: "a", email: "a@x", usage: bundle(1_000_000, 0, 200_000, 12.5), run_count: 4, subscription_run_count: 0, unreported_run_count: 0 },
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
      },
      users: [
        { user_id: "a", email: "a@x", usage: bundle(1_000_000, 0, 200_000, 12.5), run_count: 7, subscription_run_count: 2, unreported_run_count: 1 },
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
      },
      users: [
        // This user's own 2 subscription runs are excluded from their $30 figure.
        { user_id: "a", email: "big@x", usage: bundle(3_000_000, 0, 600_000, 30), run_count: 12, subscription_run_count: 2, unreported_run_count: 0 },
        // This user is fully metered — no disclosure on their row.
        { user_id: "b", email: "small@x", usage: bundle(1_000_000, 0, 200_000, 10), run_count: 8, subscription_run_count: 0, unreported_run_count: 0 },
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
