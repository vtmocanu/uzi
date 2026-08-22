// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { prefs, selectedForge, summaryCollapse } from "./prefs";

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

// PRD #362 Decision 9: the per-run summary-collapse preference. The 7-day expiry is
// exercised via the injected `now` param rather than the system clock — cleaner and not
// dependent on fake-timer interaction with Date.
describe("summaryCollapse (PRD #362 Decision 9)", () => {
  const DAY = 24 * 60 * 60 * 1000;
  const t0 = 1_700_000_000_000;

  it("defaults to expanded when the run has no stored choice", () => {
    expect(summaryCollapse.getCollapsed("run-1")).toBe(false);
  });

  it("persists a collapse choice and reads it back after a reload (fresh read, no state)", () => {
    summaryCollapse.setCollapsed("run-1", true);
    expect(summaryCollapse.getCollapsed("run-1")).toBe(true);
    // Per-run: a different run is unaffected.
    expect(summaryCollapse.getCollapsed("run-2")).toBe(false);
  });

  it("can be toggled back to expanded", () => {
    summaryCollapse.setCollapsed("run-1", true);
    summaryCollapse.setCollapsed("run-1", false);
    expect(summaryCollapse.getCollapsed("run-1")).toBe(false);
  });

  it("drops an entry older than 7 days on read (GC) while keeping a fresh sibling", () => {
    summaryCollapse.setCollapsed("stale", true, t0);
    // Re-touch a second run 8 days later; that write GCs 'stale' and re-stamps 'fresh'.
    summaryCollapse.setCollapsed("fresh", true, t0 + 8 * DAY);
    expect(summaryCollapse.getCollapsed("stale", t0 + 8 * DAY)).toBe(false); // expired → default
    expect(summaryCollapse.getCollapsed("fresh", t0 + 8 * DAY)).toBe(true); // still fresh
  });

  it("keeps an entry that is exactly under the 7-day boundary", () => {
    summaryCollapse.setCollapsed("edge", true, t0);
    // Just under 7 days: still valid.
    expect(summaryCollapse.getCollapsed("edge", t0 + 7 * DAY - 1)).toBe(true);
  });

  it("removes the expired key from storage, not just from the returned value", () => {
    summaryCollapse.setCollapsed("stale", true, t0);
    summaryCollapse.getCollapsed("stale", t0 + 8 * DAY); // read triggers GC-writeback
    const raw = window.localStorage.getItem("uzi.summaryCollapse");
    expect(raw).not.toBeNull();
    expect(JSON.parse(raw!)).not.toHaveProperty("stale");
  });

  it("returns the default when the stored map is corrupt, without throwing", () => {
    window.localStorage.setItem("uzi.summaryCollapse", "{not json");
    expect(() => summaryCollapse.getCollapsed("run-1")).not.toThrow();
    expect(summaryCollapse.getCollapsed("run-1")).toBe(false);
  });
});

// Issue #578: the last-selected Boards forge connection. A single scalar with a 7-day TTL,
// so an expired entry simply reads as null (no GC loop). The expiry is exercised via the
// injected `now` param rather than the system clock.
describe("selectedForge (issue #578)", () => {
  const DAY = 24 * 60 * 60 * 1000;
  const t0 = 1_700_000_000_000;

  it("round-trips a selected connection id", () => {
    selectedForge.set("conn-2");
    expect(selectedForge.get()).toBe("conn-2");
  });

  it("returns null when nothing is stored", () => {
    expect(selectedForge.get()).toBeNull();
  });

  it("expires after 7 days, and a fresh set is readable at that time", () => {
    selectedForge.set("conn-2", t0);
    expect(selectedForge.get(t0 + 8 * DAY)).toBeNull(); // expired → null
    selectedForge.set("conn-3", t0 + 8 * DAY);
    expect(selectedForge.get(t0 + 8 * DAY)).toBe("conn-3");
  });

  it("keeps an entry that is exactly under the 7-day boundary", () => {
    selectedForge.set("conn-2", t0);
    expect(selectedForge.get(t0 + 7 * DAY - 1)).toBe("conn-2");
  });

  it("returns null when the stored value is corrupt, without throwing", () => {
    window.localStorage.setItem("uzi.selectedForge", "{not json");
    expect(() => selectedForge.get()).not.toThrow();
    expect(selectedForge.get()).toBeNull();
  });
});
