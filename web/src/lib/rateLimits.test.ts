// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import {
  formatAgo,
  formatCountdown,
  rowState,
  sortAdminRows,
  statusBadge,
  type RowState,
} from "./rateLimits";
import type { AdminRateLimitUser, MyRateLimits } from "./api";

const NOW = Date.parse("2026-07-15T12:00:00Z");
const NOW_SECS = Math.floor(NOW / 1000);

function ok(pct5: number, pct7: number, over: Partial<Extract<MyRateLimits, { status: "ok" }>> = {}): MyRateLimits {
  return {
    status: "ok",
    five_hour: { pct: pct5, resets_at: NOW_SECS + 5000 },
    seven_day: { pct: pct7, resets_at: NOW_SECS + 200_000 },
    source: "usage_endpoint",
    synced_at: new Date(NOW - 2 * 60_000).toISOString(),
    stale: false,
    ...over,
  };
}

function row(id: string, name: string, limits: MyRateLimits, vault_locked = false): AdminRateLimitUser {
  return { id, name, email: `${name}@x`, vault_locked, limits };
}

describe("formatCountdown (Decision 7)", () => {
  it("renders days/hours/minutes and the null / past edges", () => {
    expect(formatCountdown(null, NOW)).toBeNull();
    expect(formatCountdown(NOW_SECS + 2 * 86_400 + 4 * 3600, NOW)).toBe("2d 4h");
    expect(formatCountdown(NOW_SECS + 3600 + 23 * 60, NOW)).toBe("1h 23m");
    expect(formatCountdown(NOW_SECS + 44 * 60, NOW)).toBe("44m");
    expect(formatCountdown(NOW_SECS + 30, NOW)).toBe("<1m");
    expect(formatCountdown(NOW_SECS - 10, NOW)).toBe("now");
  });
});

describe("formatAgo", () => {
  it("renders compact relative time and tolerates a bad timestamp", () => {
    expect(formatAgo(new Date(NOW - 30_000).toISOString(), NOW)).toBe("30s ago");
    expect(formatAgo(new Date(NOW - 2 * 60_000).toISOString(), NOW)).toBe("2m ago");
    expect(formatAgo(new Date(NOW - 3 * 3600_000).toISOString(), NOW)).toBe("3h ago");
    expect(formatAgo(new Date(NOW - 2 * 86_400_000).toISOString(), NOW)).toBe("2d ago");
    expect(formatAgo("not-a-date", NOW)).toBe("just now");
  });
});

describe("rowState — the five row states", () => {
  const cases: [string, MyRateLimits, boolean, RowState][] = [
    ["no token", { status: "no_token" }, false, "no_token"],
    ["unavailable", { status: "unavailable" }, false, "unavailable"],
    ["stale reading", ok(31, 12, { stale: true }), true, "stale"],
    ["live, both low", ok(8, 47), false, "live_ok"],
    ["live, one window in warn band", ok(62, 83), false, "live_warn"],
    ["live, one window in danger band", ok(97, 71), false, "live_danger"],
  ];
  it.each(cases)("classifies %s", (_label, limits, _locked, expected) => {
    expect(rowState(limits)).toBe(expected);
  });
});

describe("statusBadge", () => {
  it("maps each state to its pill", () => {
    expect(statusBadge({ status: "no_token" }, false)).toMatchObject({ tone: "neutral", label: "no token" });
    expect(statusBadge({ status: "unavailable" }, false)).toMatchObject({ tone: "neutral", label: "no reading yet" });
    expect(statusBadge(ok(31, 12, { stale: true }), true)).toMatchObject({ tone: "neutral", label: "🔒 vault locked" });
    expect(statusBadge(ok(31, 12, { stale: true }), false)).toMatchObject({ tone: "neutral", label: "stale" });
    expect(statusBadge(ok(8, 47), false)).toMatchObject({ tone: "ok", label: "Live", dot: true });
    // A warn window stays "Live" (the bar carries the amber), not an escalated pill.
    expect(statusBadge(ok(62, 83), false)).toMatchObject({ tone: "ok", label: "Live" });
    expect(statusBadge(ok(97, 71), false)).toMatchObject({ tone: "danger", label: "5h nearly out" });
    expect(statusBadge(ok(71, 97), false)).toMatchObject({ tone: "danger", label: "7d nearly out" });
    expect(statusBadge(ok(96, 99), false)).toMatchObject({ tone: "danger", label: "5h & 7d nearly out" });
  });
});

describe("sortAdminRows", () => {
  it("orders danger → warn → ok → stale → unavailable → no_token", () => {
    const users = [
      row("6", "irina", { status: "no_token" }),
      row("3", "vlad", ok(8, 47)),
      row("1", "ana", ok(97, 71)),
      row("5", "dana", { status: "unavailable" }),
      row("2", "radu", ok(62, 83)),
      row("4", "mihai", ok(31, 12, { stale: true }), true),
    ];
    expect(sortAdminRows(users).map((u) => u.name)).toEqual([
      "ana",
      "radu",
      "vlad",
      "mihai",
      "dana",
      "irina",
    ]);
  });

  it("tie-breaks two live-ok rows by 5h% desc", () => {
    const users = [row("a", "low", ok(10, 20)), row("b", "high", ok(40, 5))];
    expect(sortAdminRows(users).map((u) => u.name)).toEqual(["high", "low"]);
  });
});
