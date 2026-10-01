// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { safeNextPath } from "./safeNextPath";
import { clearPendingReturn, consumePendingReturn, setPendingReturn } from "./pendingReturn";

const KEY = "uzi.pendingReturn";
const ID = "6f0e4b0a-1c2d-4e3f-8a9b-0c1d2e3f4a5b";
const GOOD = `/connect?request=${ID}`;

beforeEach(() => window.sessionStorage.clear());
afterEach(() => window.sessionStorage.clear());

describe("pending return", () => {
  it("returns a stored /connect path once, then nothing", () => {
    setPendingReturn(GOOD);
    expect(consumePendingReturn()).toBe(GOOD);
    expect(consumePendingReturn()).toBeNull();
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
  });

  it("clear drops the entry", () => {
    setPendingReturn(GOOD);
    clearPendingReturn();
    expect(consumePendingReturn()).toBeNull();
  });

  // Whatever safeNextPath rejects must never come back, even planted straight into storage.
  it.each([
    ["protocol-relative", `//evil.example/connect?request=${ID}`],
    ["backslash", `/\\evil.example/connect?request=${ID}`],
    ["absolute URL", `https://evil.example/connect?request=${ID}`],
    ["javascript URL", "javascript:alert(1)"],
    ["not rooted", `connect?request=${ID}`],
  ])("rejects %s, which safeNextPath also rejects", (_name, path) => {
    expect(safeNextPath(path)).toBe("/dashboard");
    window.sessionStorage.setItem(KEY, path);
    expect(consumePendingReturn()).toBeNull();
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
  });

  // Safe by safeNextPath, but not a consent return: still refused.
  it.each([
    ["another page", "/settings/access"],
    ["the dashboard", "/dashboard"],
    ["a CLI login", "/cli-auth?request=req-1"],
    ["connect without a request", "/connect"],
    ["connect with a non-uuid request", "/connect?request=not-a-uuid"],
    ["connect with extra query", `/connect?request=${ID}&redirect_uri=https://evil.example`],
    ["connect with a fragment", `/connect?request=${ID}#x`],
    ["connect with a trailing path", `/connect/x?request=${ID}`],
  ])("rejects %s", (_name, path) => {
    expect(safeNextPath(path)).toBe(path);
    window.sessionStorage.setItem(KEY, path);
    expect(consumePendingReturn()).toBeNull();
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
  });

  it("does not store a path it would never return", () => {
    setPendingReturn("/settings");
    setPendingReturn(`//evil.example/connect?request=${ID}`);
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
  });
});
