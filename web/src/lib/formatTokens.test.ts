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
});
