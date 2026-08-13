// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { RateLimitAnnouncer, RateLimitCard, SidebarRateLimits } from "./RateLimitMeters";
import { api, type MyRateLimits } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getMyRateLimits: vi.fn() } };
});

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const nowSecs = Math.floor(Date.now() / 1000);
const okReading: MyRateLimits = {
  status: "ok",
  five_hour: { pct: 8, resets_at: nowSecs + 5000 },
  seven_day: { pct: 27, resets_at: nowSecs + 200_000 },
  source: "usage_endpoint",
  synced_at: new Date(Date.now() - 2 * 60_000).toISOString(),
  stale: false,
};
const warnReading: MyRateLimits = {
  ...okReading,
  five_hour: { pct: 62, resets_at: nowSecs + 5000 },
  seven_day: { pct: 83, resets_at: nowSecs + 200_000 },
};
// A danger-TONE reading (88 ≥ 85) that has NOT crossed 95 — the badge stays
// "Live" but the bar goes red, and the announcer steps the tone without the
// dedicated ≥95 emergency signal.
const dangerBandReading: MyRateLimits = {
  ...okReading,
  five_hour: { pct: 88, resets_at: nowSecs + 5000 },
  seven_day: { pct: 71, resets_at: nowSecs + 200_000 },
};
const dangerReading: MyRateLimits = {
  ...okReading,
  five_hour: { pct: 97, resets_at: nowSecs + 5000 },
  seven_day: { pct: 71, resets_at: nowSecs + 200_000 },
};
const staleReading: MyRateLimits = {
  status: "ok",
  five_hour: { pct: 31, resets_at: null },
  seven_day: { pct: 12, resets_at: null },
  source: "header_probe",
  synced_at: new Date(Date.now() - 3 * 3600_000).toISOString(),
  stale: true,
};
// PRD #217: a park-time reading — five_hour 100% recorded at the usage-limit park
// with source "limit_report", and a synced_at deliberately OLDER than the 100% it
// carries (the park does not bump synced_at, D3). Not stale (14m < 15m staleness),
// so the meter is live and the source disclosure — not the stale sentence — shows.
const limitReportReading: MyRateLimits = {
  status: "ok",
  five_hour: { pct: 100, resets_at: nowSecs + 2000 },
  seven_day: { pct: 40, resets_at: nowSecs + 200_000 },
  source: "limit_report",
  synced_at: new Date(Date.now() - 14 * 60_000).toISOString(),
  stale: false,
};

// Since PRD #104 M5 the endpoint returns ONE READING PER TOKEN. These tests are
// about how a single reading renders, so tokens() wraps one as the user's default
// — the multi-token rendering has its own tests below.
// auto_eligible/auto_status (PRD #111 M2) ride every token row. Defaulted here to
// the un-pooled state, which is what every pre-#111 fixture described.
function tokens(limits: MyRateLimits) {
  return {
    tokens: [
      {
        secret_id: "sec-1",
        label: "default",
        is_default: true,
        auto_eligible: false,
        auto_status: "not_pooled" as const,
        limits,
      },
    ],
  };
}
// A token-less user is an EMPTY list, not a no_token status.
const noTokens = { tokens: [] };

describe("RateLimitCard (Settings)", () => {
  it("renders the two live meters and a Live badge on an ok reading", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    expect(screen.getByText("8%")).toBeTruthy();
    expect(screen.getByText("27%")).toBeTruthy();
    expect(screen.getByText("Live")).toBeTruthy();
    // Countdown is rendered client-side from the epoch (Decision 7).
    expect(screen.getAllByText(/resets in/).length).toBeGreaterThan(0);
    // A usage_endpoint reading carries NO park-time disclosure (PRD #217).
    expect(screen.queryByText("Recorded at usage limit")).toBeNull();
  });

  it("discloses a limit_report reading as recorded at the usage limit (PRD #217)", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(limitReportReading));
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    // The 100% bar is a live reading, so the status pill still escalates…
    expect(screen.getByText("5h nearly out")).toBeTruthy();
    // …but the park-time source is disclosed: the 100% was recorded at the park,
    // NEWER than the "updated Xm ago" timestamp beside it (D3/D6).
    expect(screen.getByText("Recorded at usage limit")).toBeTruthy();
    expect(screen.getByText(/recorded when this token hit its usage limit/)).toBeTruthy();
    // The ordinary poll phrasing must NOT appear for a park-time reading.
    expect(screen.queryByText(/refreshes every few minutes/)).toBeNull();
  });

  it("greys the windows and drops the Live badge on 'unavailable'", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens({ status: "unavailable" }));
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    expect(screen.getByText("No reading yet")).toBeTruthy();
    expect(screen.getAllByText("no reading yet")).toHaveLength(2);
    expect(screen.queryByText("Live")).toBeNull();
  });

  it("renders nothing when the user has no token", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(noTokens);
    render(<RateLimitCard />);
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByText("Claude limits")).toBeNull();
  });

  it("swaps Live for a neutral stale badge on a stale reading", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(staleReading));
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    expect(screen.getByText("stale")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.getByText(/reading is stale/)).toBeTruthy();
    // Percentages still shown; no live countdown when resets_at is null.
    expect(screen.getByText("31%")).toBeTruthy();
    expect(screen.queryByText(/resets in/)).toBeNull();
    // Both meter bars grey out on a stale reading (Decision 3), like the sidebar
    // and admin table.
    const fills = ["5-hour window", "7-day window"].map(
      (name) => screen.getByRole("progressbar", { name }).firstChild as HTMLElement,
    );
    for (const fill of fills) expect(fill.className).toMatch(/opacity-40/);
  });

  it("escalates the badge to danger on a ≥95% reading and paints the 5h bar red", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerReading));
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    expect(screen.getByText("5h nearly out")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    // The 5h window bar is red (bg-danger) — NOT amber (bg-warn, which is warn).
    const bar5h = screen.getByRole("progressbar", { name: "5-hour window" }).firstChild as HTMLElement;
    expect(bar5h.className).toMatch(/bg-danger/);
    expect(bar5h.className).not.toMatch(/bg-warn/);
  });

  it("keeps a warn reading on the Live badge but paints the bar amber", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(warnReading));
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    expect(screen.getByText("Live")).toBeTruthy();
    const bar7d = screen.getByRole("progressbar", { name: "7-day window" }).firstChild as HTMLElement;
    expect(bar7d.className).toMatch(/bg-warn/);
  });
});

describe("SidebarRateLimits", () => {
  it("shows the 5h/7d micro-bars on an ok reading", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<SidebarRateLimits />);
    await screen.findByLabelText("Claude rate limits");
    expect(screen.getByText("5h")).toBeTruthy();
    expect(screen.getByText("7d")).toBeTruthy();
    expect(screen.getByText("8%")).toBeTruthy();
  });

  it("renders nothing for no_token / unavailable (no dead chrome)", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens({ status: "unavailable" }));
    render(<SidebarRateLimits />);
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByLabelText("Claude rate limits")).toBeNull();
  });

  it("dims both micro-bars on a stale reading", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(staleReading));
    render(<SidebarRateLimits />);
    await screen.findByLabelText("Claude rate limits");
    const fills = screen.getAllByRole("progressbar").map((b) => b.firstChild as HTMLElement);
    expect(fills).toHaveLength(2);
    for (const fill of fills) expect(fill.className).toMatch(/opacity-40/);
  });
});

describe("RateLimitAnnouncer (aria-live)", () => {
  // Fake timers so we can advance the 60s poll and the 30s useNow clock and flush
  // the mocked-fetch microtasks deterministically.
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  // flush advances the fake clock by ms and drains the fetch promises inside act.
  const flush = (ms = 0) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)));
  const region = () => screen.getByRole("status").textContent;

  it("announces once when the worst window steps ok → warn", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<RateLimitAnnouncer />);
    await flush(); // seed the ref at ok — first read never announces
    expect(region()).toBe("");

    mockApi.getMyRateLimits.mockResolvedValue(tokens(warnReading));
    await flush(60_000); // 60s poll → warn (7d at 83%)
    expect(region()).toMatch(/^7-day window at 83%, resets in /);
  });

  it("announces again when the tone steps warn → danger", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(warnReading));
    render(<RateLimitAnnouncer />);
    await flush(); // seed at warn, silent (first read)
    expect(region()).toBe("");

    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerBandReading));
    await flush(60_000); // → danger tone (5h at 88%), still below the 95 emergency
    expect(region()).toMatch(/^5-hour window at 88%, resets in /);
  });

  it("fires the dedicated ≥95 emergency announcement when the worst window crosses 95", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerBandReading));
    render(<RateLimitAnnouncer />);
    await flush(); // seed at danger tone (88%) but NOT critical — silent first read
    expect(region()).toBe("");

    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerReading));
    await flush(60_000); // 5h crosses 95 → dedicated emergency, distinct wording
    expect(region()).toMatch(/^5-hour window nearly out at 97%/);
    const announced = region();
    const calls = mockApi.getMyRateLimits.mock.calls.length;

    await flush(30_000); // a bare useNow clock tick, no new poll
    expect(mockApi.getMyRateLimits.mock.calls.length).toBe(calls); // no extra fetch
    expect(region()).toBe(announced); // not re-fired
  });

  it("announces the ≥95 emergency even when it steps straight through from ok", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<RateLimitAnnouncer />);
    await flush(); // seed ok — first read never announces
    expect(region()).toBe("");

    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerReading));
    await flush(60_000); // ok → danger jump that also crosses 95
    // The critical-crossing branch wins over the plain tone step-up: we get the
    // emergency wording, NOT "5-hour window at 97%".
    expect(region()).toMatch(/^5-hour window nearly out at 97%/);
  });

  it("re-arms silently after dropping below 95", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<RateLimitAnnouncer />);
    await flush(); // seed ok — silent
    expect(region()).toBe("");

    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerReading));
    await flush(60_000); // crosses 95 → emergency fires once
    expect(region()).toMatch(/nearly out at 97%/);
    const emergency = region();

    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerBandReading));
    await flush(60_000); // worst window back in the 85–94 danger band (88%)
    // Dropping below 95 announces nothing — it silently re-arms the emergency ref.
    expect(region()).toBe(emergency);

    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerReading));
    await flush(60_000); // crosses 95 again → emergency fires AGAIN (ref re-armed)
    expect(region()).toMatch(/^5-hour window nearly out at 97%/);
  });

  it("stays silent on the first read even when already in danger", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(dangerReading));
    render(<RateLimitAnnouncer />);
    await flush();
    expect(region()).toBe("");
  });

  it("stays silent when consecutive polls keep the same tone", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<RateLimitAnnouncer />);
    await flush(); // seed ok
    await flush(60_000); // a second ok poll — no step up
    expect(region()).toBe("");
  });

  it("does not re-announce on a bare 30s clock tick with no tone change", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<RateLimitAnnouncer />);
    await flush();
    mockApi.getMyRateLimits.mockResolvedValue(tokens(warnReading));
    await flush(60_000); // announce warn
    const announced = region();
    expect(announced).toMatch(/^7-day window at 83%/);
    const calls = mockApi.getMyRateLimits.mock.calls.length;

    await flush(30_000); // a useNow clock tick, no 60s poll
    expect(mockApi.getMyRateLimits.mock.calls.length).toBe(calls); // no extra fetch
    expect(region()).toBe(announced); // message unchanged, not re-fired
  });

  it("does not announce on a stale reading even at a danger pct", async () => {
    const staleDanger: MyRateLimits = { ...staleReading, five_hour: { pct: 97, resets_at: null } };
    mockApi.getMyRateLimits.mockResolvedValue(tokens(okReading));
    render(<RateLimitAnnouncer />);
    await flush(); // seed ok
    mockApi.getMyRateLimits.mockResolvedValue(tokens(staleDanger));
    await flush(60_000);
    expect(region()).toBe("");
  });
});

// PRD #309 M4 — the burn-rate forecast, wired end to end through the Settings card.
// These drive the real accumulation (useReadingSeries) across polls, so they prove
// the WIRING (series → burnForecast → RateLimitForecastMeter), not just the helper.
describe("RateLimitCard forecast (PRD #309)", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());
  const flush = (ms = 0) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)));

  // A 5-hour reading at pct5 with a far-out reset, so a rising trailing series
  // projects well past the cap. Not stale → it accrues a sample.
  const fiveHourAt = (pct5: number): MyRateLimits => ({
    status: "ok",
    five_hour: { pct: pct5, resets_at: nowSecs + 5000 },
    seven_day: { pct: 20, resets_at: nowSecs + 200_000 },
    source: "usage_endpoint",
    synced_at: new Date().toISOString(),
    stale: false,
  });

  it("draws a coral ghost + » once a rising trailing series projects over the cap", async () => {
    let poll = 0;
    // 40, 48, 56 … capped below 100 so late polls are flat (no reset/decay restart).
    mockApi.getMyRateLimits.mockImplementation(async () => {
      const pct5 = Math.min(40 + poll * 8, 96);
      poll += 1;
      return tokens(fiveHourAt(pct5));
    });
    render(<RateLimitCard />);
    await flush(); // initial read (40%)
    for (let k = 0; k < 6; k++) await flush(60_000); // rising polls, >3-min span accrues

    const bar5h = screen.getByRole("progressbar", { name: "5-hour window" });
    expect(bar5h.getAttribute("aria-valuetext")).toMatch(/projected \d+% by reset, over$/);
    expect(screen.getByText("»")).toBeTruthy();
    // The projected % is NEVER inline visible text (D4): the row shows only the
    // current pct, not the projection.
    expect(screen.getByText("»").closest("div")?.parentElement?.textContent).not.toMatch(/projected/);
  });

  it("stays silent (plain bar) on a FLAT series — proving it reads the slope, not presence", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(tokens(fiveHourAt(62))); // constant 62%
    render(<RateLimitCard />);
    await flush();
    for (let k = 0; k < 6; k++) await flush(60_000);

    const bar5h = screen.getByRole("progressbar", { name: "5-hour window" });
    expect(bar5h.getAttribute("aria-valuetext")).not.toMatch(/projected/);
    expect(screen.queryByText("»")).toBeNull();
  });

  it("clears the forecast the moment a forecasting row goes stale (no ghost on a frozen bar)", async () => {
    let poll = 0;
    mockApi.getMyRateLimits.mockImplementation(async () => {
      const pct5 = Math.min(40 + poll * 8, 88);
      const stale = poll >= 7; // polls 0..6 rise live; 7+ freeze the same reading stale
      poll += 1;
      const r = fiveHourAt(pct5);
      return tokens(r.status === "ok" ? { ...r, stale } : r);
    });
    render(<RateLimitCard />);
    await flush(); // 40%
    for (let k = 0; k < 6; k++) await flush(60_000); // rising, live → forecast appears
    expect(screen.getByText("»")).toBeTruthy();

    await flush(60_000); // poll 7 → same pct, stale=true
    await flush(60_000); // settle
    expect(screen.queryByText("»")).toBeNull(); // ghost gone immediately (rowForecast gate)
    const bar5h = screen.getByRole("progressbar", { name: "5-hour window" });
    expect(bar5h.getAttribute("aria-valuetext")).not.toMatch(/projected/);
  });
});
