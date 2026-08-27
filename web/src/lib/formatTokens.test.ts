import { describe, it, expect } from "vitest";
import { formatTokens, formatCost } from "./formatTokens";

describe("formatTokens", () => {
  it("renders sub-1k as a bare integer (Decision 10)", () => {
    expect(formatTokens(0)).toBe("0");
    expect(formatTokens(1)).toBe("1");
    expect(formatTokens(999)).toBe("999");
  });

  it("renders 1k–1M with one decimal and a k suffix", () => {
    expect(formatTokens(1000)).toBe("1.0k");
    expect(formatTokens(6100)).toBe("6.1k");
    expect(formatTokens(48_200)).toBe("48.2k");
    expect(formatTokens(188_000)).toBe("188.0k");
    expect(formatTokens(999_949)).toBe("999.9k");
  });

  it("renders ≥1M with two decimals and an M suffix", () => {
    expect(formatTokens(1_000_000)).toBe("1.00M");
    expect(formatTokens(1_170_000)).toBe("1.17M");
    expect(formatTokens(1_280_000)).toBe("1.28M");
    expect(formatTokens(18_400_000)).toBe("18.40M");
  });

  it("keeps M all the way to just under 1B, and pins the known top-of-tier wart", () => {
    expect(formatTokens(999_000_000)).toBe("999.00M");
    // 999_990_000 / 1e6 is EXACTLY 999.99, so this pins the tier but proves nothing
    // about rounding. The pair below is the real edge: the tier is chosen from the
    // RAW value, so the top of M rounds up past its own unit rather than tipping
    // into B. Documented in formatTokens.ts; pinned here so it cannot drift silently.
    expect(formatTokens(999_990_000)).toBe("999.99M");
    expect(formatTokens(999_994_999)).toBe("999.99M");
    expect(formatTokens(999_995_000)).toBe("1000.00M"); // known wart, not a typo
    expect(formatTokens(999_999_999)).toBe("1000.00M"); // ← the shape #77 is named after
  });

  it("renders ≥1B with two decimals and a B suffix (#77)", () => {
    expect(formatTokens(1_000_000_000)).toBe("1.00B");
    // The literal regression: this used to render "5400.00M".
    expect(formatTokens(5_400_000_000)).toBe("5.40B");
    expect(formatTokens(999_000_000_000)).toBe("999.00B");
  });

  it("renders ≥1T with two decimals and a T suffix (#77)", () => {
    expect(formatTokens(1_000_000_000_000)).toBe("1.00T");
    expect(formatTokens(2_300_000_000_000)).toBe("2.30T");
    // No tier above T: very large values keep scaling in T rather than overflowing
    // into an undefined suffix.
    expect(formatTokens(45_000_000_000_000)).toBe("45.00T");
  });

  it("rounds to the nearest whole token before scaling", () => {
    expect(formatTokens(48_249)).toBe("48.2k");
    expect(formatTokens(48_250)).toBe("48.3k");
  });

  it("coerces non-finite / negative to 0 rather than NaN", () => {
    expect(formatTokens(Number.NaN)).toBe("0");
    expect(formatTokens(-5)).toBe("0");
    expect(formatTokens(Number.POSITIVE_INFINITY)).toBe("0");
  });
});

describe("formatCost", () => {
  it("renders two-decimal dollars", () => {
    expect(formatCost(0)).toBe("$0.00");
    expect(formatCost(1.87)).toBe("$1.87");
    expect(formatCost(0.611)).toBe("$0.61");
  });
  it("coerces non-finite / negative to $0.00", () => {
    expect(formatCost(Number.NaN)).toBe("$0.00");
    expect(formatCost(-1)).toBe("$0.00");
  });
  it("drops the cents at $1000 or more (rounded, no separator)", () => {
    expect(formatCost(1118.63)).toBe("$1119");
    expect(formatCost(1000)).toBe("$1000"); // threshold is inclusive
    expect(formatCost(1500.5)).toBe("$1501"); // rounds up
    expect(formatCost(999.99)).toBe("$999.99"); // just below, unchanged
  });
});
