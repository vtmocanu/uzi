import { describe, it, expect } from "vitest";
import { formatUptimeSince } from "./formatUptimeSince";

// A fixed "now" so every case is deterministic; each fixture is an offset back from it.
const NOW = Date.parse("2026-08-08T12:00:00Z");
const ago = (seconds: number) => new Date(NOW - seconds * 1000).toISOString();

describe("formatUptimeSince", () => {
  // ── the four buckets, matching formatCountdown's vocabulary ──────────────
  it("renders days ≥ 1 as 'Nd Nh'", () => {
    // 2 days + 4 hours ago → "2d 4h"
    expect(formatUptimeSince(ago(2 * 86_400 + 4 * 3_600), NOW)).toBe("2d 4h");
  });

  it("renders hours ≥ 1 as 'Nh Nm'", () => {
    // 1 hour + 23 minutes ago → "1h 23m"
    expect(formatUptimeSince(ago(3_600 + 23 * 60), NOW)).toBe("1h 23m");
  });

  it("renders minutes ≥ 1 as 'Nm'", () => {
    // 44 minutes ago → "44m"
    expect(formatUptimeSince(ago(44 * 60), NOW)).toBe("44m");
  });

  it("renders under a minute as '<1m' — boundary at 59s and 60s", () => {
    // 59s still floors to "<1m"; 60s crosses into "1m".
    expect(formatUptimeSince(ago(59), NOW)).toBe("<1m");
    expect(formatUptimeSince(ago(60), NOW)).toBe("1m");
  });

  // ── invalid input → "" (only an unparseable instant) ─────────────────────
  it("returns '' for an unparseable timestamp", () => {
    expect(formatUptimeSince("not-a-date", NOW)).toBe("");
  });

  // ── clock skew: a future anchor floors at '<1m', matching the Go twin ─────
  it("floors a future/clock-skewed instant at '<1m' (never a dangling label)", () => {
    // An anchor a few seconds AHEAD of the browser clock is skew for an online
    // worker, not "no uptime": floor to "<1m" like uptimeCell's negative time.Since,
    // so the row never renders "· up " with no duration.
    expect(formatUptimeSince(new Date(NOW + 10 * 60 * 1000).toISOString(), NOW)).toBe("<1m");
  });
});
