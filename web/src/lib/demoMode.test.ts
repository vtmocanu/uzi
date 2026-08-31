// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { isDemoMode, setDemoMode, subscribeDemoMode, useDemoMode } from "./demoMode";

// This jsdom build does not expose window.localStorage (same as prefs.test.ts / theme.test.ts),
// so back it with a Map-based Storage stub the demoMode helpers read and write through.
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
  cleanup();
  // Leave the store in the off state so a leaked subscriber never sees a stale on.
  setDemoMode(false);
  vi.restoreAllMocks();
});

describe("isDemoMode / setDemoMode", () => {
  it("defaults to off when nothing is stored", () => {
    expect(isDemoMode()).toBe(false);
  });

  it("round-trips on/off through localStorage", () => {
    setDemoMode(true);
    expect(isDemoMode()).toBe(true);
    expect(window.localStorage.getItem("uzi_demo_mode")).toBe("1");
    setDemoMode(false);
    expect(isDemoMode()).toBe(false);
  });

  it('reads a hand-set "true" value as on', () => {
    window.localStorage.setItem("uzi_demo_mode", "true");
    expect(isDemoMode()).toBe(true);
  });

  it("reads an unrelated value as off", () => {
    window.localStorage.setItem("uzi_demo_mode", "yes");
    expect(isDemoMode()).toBe(false);
  });

  it("returns off when the store throws on read (private window)", () => {
    vi.spyOn(window.localStorage, "getItem").mockImplementation(() => {
      throw new Error("access denied");
    });
    expect(isDemoMode()).toBe(false);
  });

  it("swallows a throwing setItem and leaves both the store and the hook off", () => {
    vi.spyOn(window.localStorage, "setItem").mockImplementation(() => {
      throw new Error("quota exceeded");
    });
    const { result } = renderHook(() => useDemoMode());
    expect(result.current).toBe(false);
    expect(() => act(() => setDemoMode(true))).not.toThrow();
    // The write failed, so the persisted flag never changed: demo mode stays off rather
    // than silently reporting on. Assert via BOTH channels — the direct read AND the hook
    // value. The hook returns the cached snapshot, which setDemoMode must recompute from
    // the store after the throw (not blindly trust `on`); isDemoMode() alone re-reads the
    // store and so would pass even if the cache went stale-true.
    expect(isDemoMode()).toBe(false);
    expect(result.current).toBe(false);
  });
});

describe("subscribeDemoMode", () => {
  it("notifies a local subscriber in the same tab on setDemoMode", () => {
    const cb = vi.fn();
    const unsubscribe = subscribeDemoMode(cb);
    setDemoMode(true);
    expect(cb).toHaveBeenCalledTimes(1);
    setDemoMode(false);
    expect(cb).toHaveBeenCalledTimes(2);
    unsubscribe();
    setDemoMode(true);
    expect(cb).toHaveBeenCalledTimes(2); // no further calls after unsubscribe
  });

  it("reacts to a cross-tab storage event for our key", () => {
    const cb = vi.fn();
    const unsubscribe = subscribeDemoMode(cb);
    window.dispatchEvent(new StorageEvent("storage", { key: "uzi_demo_mode", newValue: "1" }));
    expect(cb).toHaveBeenCalledTimes(1);
    // A storage.clear() event (key === null) also fires our subscribers.
    window.dispatchEvent(new StorageEvent("storage", { key: null }));
    expect(cb).toHaveBeenCalledTimes(2);
    // An unrelated key does not.
    window.dispatchEvent(new StorageEvent("storage", { key: "other", newValue: "x" }));
    expect(cb).toHaveBeenCalledTimes(2);
    unsubscribe();
  });
});

describe("useDemoMode", () => {
  it("returns the current snapshot and re-renders live on toggle", () => {
    const { result } = renderHook(() => useDemoMode());
    expect(result.current).toBe(false);
    act(() => setDemoMode(true));
    expect(result.current).toBe(true);
    act(() => setDemoMode(false));
    expect(result.current).toBe(false);
  });
});
