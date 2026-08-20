import { describe, it, expect } from "vitest";
import { parseSemver, compareSemver } from "./semver";

describe("parseSemver", () => {
  it("parses a plain X.Y.Z into numeric fields", () => {
    expect(parseSemver("0.48.0")).toEqual([0, 48, 0]);
    expect(parseSemver("1.2.3")).toEqual([1, 2, 3]);
    expect(parseSemver("0.10.0")).toEqual([0, 10, 0]);
  });

  it("returns null for non-semver, pseudo, prerelease, and unshaped input", () => {
    expect(parseSemver("dev")).toBeNull();
    expect(parseSemver("demo")).toBeNull();
    expect(parseSemver("")).toBeNull();
    expect(parseSemver("Unreleased")).toBeNull();
    // -rc.N prerelease is a documented Model-B limitation → null.
    expect(parseSemver("1.2.3-rc.1")).toBeNull();
    // Not three fields.
    expect(parseSemver("1.2")).toBeNull();
    expect(parseSemver("v1.2.3")).toBeNull();
    expect(parseSemver(undefined)).toBeNull();
    expect(parseSemver(null)).toBeNull();
  });
});

describe("compareSemver", () => {
  it("orders numerically, not lexically (0.9.0 < 0.10.0)", () => {
    expect(compareSemver("0.9.0", "0.10.0")).toBeLessThan(0);
    expect(compareSemver("0.10.0", "0.9.0")).toBeGreaterThan(0);
  });

  it("orders across major/minor/patch", () => {
    expect(compareSemver("1.0.0", "0.99.0")).toBeGreaterThan(0);
    expect(compareSemver("0.48.0", "0.48.1")).toBeLessThan(0);
    expect(compareSemver("2.0.0", "1.9.9")).toBeGreaterThan(0);
  });

  it("returns 0 for equal versions", () => {
    expect(compareSemver("0.48.0", "0.48.0")).toBe(0);
  });

  it("pushes an unparseable side last, and treats two unparseable as equal", () => {
    expect(compareSemver("dev", "0.48.0")).toBeGreaterThan(0); // dev sorts last
    expect(compareSemver("0.48.0", "dev")).toBeLessThan(0);
    expect(compareSemver("dev", "demo")).toBe(0);
  });
});
