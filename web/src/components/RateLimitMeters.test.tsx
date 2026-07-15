// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { RateLimitCard, SidebarRateLimits } from "./RateLimitMeters";
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
  seven_day: { pct: 47, resets_at: nowSecs + 200_000 },
  source: "usage_endpoint",
  synced_at: new Date(Date.now() - 2 * 60_000).toISOString(),
  stale: false,
};

describe("RateLimitCard (Settings)", () => {
  it("renders the two live meters and a Live badge on an ok reading", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(okReading);
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    expect(screen.getByText("8%")).toBeTruthy();
    expect(screen.getByText("47%")).toBeTruthy();
    expect(screen.getByText("Live")).toBeTruthy();
    // Countdown is rendered client-side from the epoch (Decision 7).
    expect(screen.getAllByText(/resets in/).length).toBeGreaterThan(0);
  });

  it("greys the windows and drops the Live badge on 'unavailable'", async () => {
    mockApi.getMyRateLimits.mockResolvedValue({ status: "unavailable" });
    render(<RateLimitCard />);
    await screen.findByText("Claude limits");
    expect(screen.getByText("No reading yet")).toBeTruthy();
    expect(screen.getAllByText("no reading yet")).toHaveLength(2);
    expect(screen.queryByText("Live")).toBeNull();
  });

  it("renders nothing when the user has no token", async () => {
    mockApi.getMyRateLimits.mockResolvedValue({ status: "no_token" });
    render(<RateLimitCard />);
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByText("Claude limits")).toBeNull();
  });
});

describe("SidebarRateLimits", () => {
  it("shows the 5h/7d micro-bars on an ok reading", async () => {
    mockApi.getMyRateLimits.mockResolvedValue(okReading);
    render(<SidebarRateLimits />);
    await screen.findByLabelText("Claude rate limits");
    expect(screen.getByText("5h")).toBeTruthy();
    expect(screen.getByText("7d")).toBeTruthy();
    expect(screen.getByText("8%")).toBeTruthy();
  });

  it("renders nothing for no_token / unavailable (no dead chrome)", async () => {
    mockApi.getMyRateLimits.mockResolvedValue({ status: "unavailable" });
    render(<SidebarRateLimits />);
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByLabelText("Claude rate limits")).toBeNull();
  });
});
