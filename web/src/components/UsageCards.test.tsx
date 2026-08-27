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
});
const wrap = (ui: ReactNode) => render(<MemoryRouter>{ui}</MemoryRouter>);

describe("YourUsageCard", () => {
  it("renders the lifetime total, breakdown, and last-7-days kicker", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_610_000, 16_100_000, 710_000, 26.4),
      last_7_days: bundle(200_000, 2_800_000, 100_000, 4.55),
      run_count: 23,
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
    const usage: SelfUsage = { lifetime: bundle(0, 0, 0, 0), last_7_days: bundle(0, 0, 0, 0), run_count: 0 };
    const { getByText, container } = wrap(<YourUsageCard usage={usage} />);
    expect(getByText(/No usage recorded yet/)).toBeTruthy();
    // No "0 tokens" big number and no "Across N runs" kicker — nothing fabricated.
    expect(container.textContent).not.toContain("0 tokens");
    expect(container.textContent).not.toContain("Across");
  });

  it("renders a $0 cost as '—' (Decision 8: subscription auth)", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 0), // nonzero tokens, zero cost
      last_7_days: bundle(0, 0, 0, 0),
      run_count: 2,
    };
    const { container } = wrap(<YourUsageCard usage={usage} />);
    expect(container.textContent).toContain("—");
    expect(container.textContent).not.toContain("$0.00");
  });

  it("keeps the 'see per-run detail →' arrow glued to 'detail' (no orphaned arrow)", () => {
    const usage: SelfUsage = {
      lifetime: bundle(1_000_000, 0, 200_000, 1.23),
      last_7_days: bundle(100_000, 0, 50_000, 0.5),
      run_count: 3,
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
    factory: { lifetime: bundle(4_100_000, 37_500_000, 1_730_000, 64.23), last_7_days: bundle(0, 0, 0, 0), run_count: 54 },
    users: [
      { user_id: "a", email: "big@x", usage: bundle(2_490_000, 22_400_000, 1_020_000, 37.83), run_count: 31 },
      { user_id: "b", email: "small@x", usage: bundle(1_610_000, 15_100_000, 710_000, 26.4), run_count: 23 },
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
});
