import { describe, expect, it } from "vitest";
import { compareSemver, parseSemver } from "./semver";

describe("parseSemver", () => {
  it("parses a strict X.Y.Z into numeric fields", () => {
    expect(parseSemver("0.10.1")).toEqual([0, 10, 1]);
    expect(parseSemver("1.0.0")).toEqual([1, 0, 0]);
    expect(parseSemver("12.34.56")).toEqual([12, 34, 56]);
  });

  it("returns null for anything that is not strict X.Y.Z — no prerelease support", () => {
    // Documented non-support: prerelease and v-prefixed tags do NOT parse.
    expect(parseSemver("0.1.0-rc.1")).toBeNull();
    expect(parseSemver("dev")).toBeNull();
    expect(parseSemver("demo")).toBeNull();
    expect(parseSemver("v1.2.3")).toBeNull();
    expect(parseSemver("1.2")).toBeNull();
    expect(parseSemver("1.2.3.4")).toBeNull();
    expect(parseSemver("")).toBeNull();
  });
});

describe("compareSemver", () => {
  it("orders by NUMERIC field, not lexically", () => {
    // The whole reason this exists: a string compare would rank 0.9.0 above 0.10.0.
    expect(compareSemver("0.9.0", "0.10.0")).toBeLessThan(0);
    expect(compareSemver("0.10.0", "0.10.1")).toBeLessThan(0);
    expect(compareSemver("0.10.1", "1.0.0")).toBeLessThan(0);
    // …and the whole ascending chain in one line.
    const chain = ["0.9.0", "0.10.0", "0.10.1", "1.0.0"];
    for (let i = 0; i + 1 < chain.length; i++) {
      expect(compareSemver(chain[i], chain[i + 1])).toBeLessThan(0);
    }
  });

  it("is symmetric: >0 in the other direction", () => {
    expect(compareSemver("0.10.0", "0.9.0")).toBeGreaterThan(0);
    expect(compareSemver("1.0.0", "0.10.1")).toBeGreaterThan(0);
  });

  it("returns 0 for equal versions", () => {
    expect(compareSemver("0.10.1", "0.10.1")).toBe(0);
    expect(compareSemver("1.0.0", "1.0.0")).toBe(0);
  });

  it("orders a non-parseable input below any real semver", () => {
    expect(compareSemver("dev", "1.0.0")).toBeLessThan(0);
    expect(compareSemver("1.0.0", "dev")).toBeGreaterThan(0);
    expect(compareSemver("dev", "demo")).toBe(0);
  });
});
