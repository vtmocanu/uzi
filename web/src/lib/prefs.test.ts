// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { prefs } from "./prefs";

// This jsdom build does not expose window.localStorage (Node warns "localStorage
// is not available because --localstorage-file was not provided"), so back it with
// a Map-based Storage stub the prefs helper reads and writes through. In a real
// browser window.localStorage is present; prefs' own try/catch is what keeps it
// safe when a store is absent or throws (private mode, quota).
function makeStorage(): Storage {
  const m = new Map<string, string>();
  return {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
}

beforeEach(() => {
  Object.defineProperty(window, "localStorage", { configurable: true, value: makeStorage() });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("prefs", () => {
  it("returns the fallback when the key is unset", () => {
    expect(prefs.get("uzi.absent", false)).toBe(false);
    expect(prefs.get("uzi.absent", 7)).toBe(7);
  });

  it("round-trips values through set/get", () => {
    prefs.set("uzi.flag", true);
    expect(prefs.get("uzi.flag", false)).toBe(true);
    prefs.set("uzi.obj", { a: 1 });
    expect(prefs.get<{ a: number }>("uzi.obj", { a: 0 })).toEqual({ a: 1 });
  });

  it("preserves a stored falsy value (does not confuse false with unset)", () => {
    prefs.set("uzi.flag", false);
    expect(prefs.get("uzi.flag", true)).toBe(false);
  });

  it("returns the fallback when the stored value is corrupt JSON", () => {
    window.localStorage.setItem("uzi.bad", "{not json");
    expect(prefs.get("uzi.bad", "safe")).toBe("safe");
  });

  it("swallows a throwing setItem (storage unavailable / quota) instead of raising", () => {
    vi.spyOn(window.localStorage, "setItem").mockImplementation(() => {
      throw new Error("quota exceeded");
    });
    expect(() => prefs.set("uzi.k", 1)).not.toThrow();
  });

  it("swallows a throwing getItem and returns the fallback", () => {
    vi.spyOn(window.localStorage, "getItem").mockImplementation(() => {
      throw new Error("access denied");
    });
    expect(prefs.get("uzi.k", "fallback")).toBe("fallback");
  });
});
