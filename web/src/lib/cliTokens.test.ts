import { describe, it, expect } from "vitest";
import { CLI_TOKEN_STALE_DAYS, isCliTokenStale } from "./cliTokens";
import type { CliToken } from "./api";

const NOW = Date.parse("2026-07-17T12:00:00Z");
const daysBefore = (d: number) => new Date(NOW - d * 86_400_000).toISOString();

function token(over: Partial<CliToken> = {}): CliToken {
  return {
    id: "t1",
    name: "laptop",
    token_prefix: "uzc_a1b2",
    scope: "user",
    revoked: false,
    created_at: daysBefore(10),
    last_used_at: daysBefore(1),
    last_used_ip: "10.0.0.1",
    expires_at: null,
    ...over,
  };
}

describe("isCliTokenStale", () => {
  it("is false for a recently used token", () => {
    expect(isCliTokenStale(token({ last_used_at: daysBefore(1) }), NOW)).toBe(false);
  });

  it("is true when last used 90+ days ago", () => {
    expect(isCliTokenStale(token({ last_used_at: daysBefore(CLI_TOKEN_STALE_DAYS + 1) }), NOW)).toBe(true);
  });

  it("measures a never-used token from created_at", () => {
    // Unused since it existed: created > 90 days ago ⇒ stale; recent ⇒ not.
    expect(isCliTokenStale(token({ last_used_at: null, created_at: daysBefore(120) }), NOW)).toBe(true);
    expect(isCliTokenStale(token({ last_used_at: null, created_at: daysBefore(5) }), NOW)).toBe(false);
  });

  it("never flags a revoked token (already dead — nudging to revoke is noise)", () => {
    expect(isCliTokenStale(token({ revoked: true, last_used_at: daysBefore(400) }), NOW)).toBe(false);
  });

  it("is false at exactly the boundary minus a hair, true at the boundary", () => {
    expect(isCliTokenStale(token({ last_used_at: daysBefore(CLI_TOKEN_STALE_DAYS) }), NOW)).toBe(true);
    expect(isCliTokenStale(token({ last_used_at: daysBefore(CLI_TOKEN_STALE_DAYS - 1) }), NOW)).toBe(false);
  });
});
