import { describe, expect, it } from "vitest";
import { leaseRemaining } from "./WorkerLeaseBadge";

describe("leaseRemaining (PRD #2006 M2)", () => {
  const now = Date.parse("2026-10-01T12:00:00Z");

  it("formats a live lease with the app's countdown buckets", () => {
    expect(leaseRemaining("2026-10-01T13:12:30Z", now)).toBe("1h 12m");
    expect(leaseRemaining("2026-10-01T12:44:00Z", now)).toBe("44m");
    expect(leaseRemaining("2026-10-01T12:00:20Z", now)).toBe("<1m");
  });

  it("returns null for an absent, unparseable, expired or exactly-now lease", () => {
    expect(leaseRemaining(undefined, now)).toBeNull();
    expect(leaseRemaining("", now)).toBeNull();
    expect(leaseRemaining("not-a-time", now)).toBeNull();
    expect(leaseRemaining("2026-10-01T11:59:00Z", now)).toBeNull();
    expect(leaseRemaining("2026-10-01T12:00:00Z", now)).toBeNull();
  });
});
