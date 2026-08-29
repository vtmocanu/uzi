// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { ensureWebStorage } from "./test-setup";

// Node-independent regression test for issue #340: on Node >=26 the jsdom environment exposes
// no `localStorage`, so a bare `localStorage.clear()` throws. We reproduce that "no Storage"
// condition explicitly (rather than by Node version, since the repo pins Node 24 where jsdom
// provides one) and assert the setup guard installs a working in-memory store.

const originalLocal = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
const originalSession = Object.getOwnPropertyDescriptor(globalThis, "sessionStorage");

afterEach(() => {
  // Restore whatever was there before this file forced things, so no state leaks across tests.
  if (originalLocal) Object.defineProperty(globalThis, "localStorage", originalLocal);
  if (originalSession) Object.defineProperty(globalThis, "sessionStorage", originalSession);
});

describe("ensureWebStorage (issue #340)", () => {
  it("installs a working Storage when the global is undefined, so bare localStorage round-trips", () => {
    // Force the exact Node-26 shadow: no Storage at all.
    Object.defineProperty(globalThis, "localStorage", { value: undefined, configurable: true, writable: true });
    Object.defineProperty(globalThis, "sessionStorage", { value: undefined, configurable: true, writable: true });

    ensureWebStorage();

    // Bare global access (the AuthContext.test.tsx failure shape) must now work.
    expect(typeof localStorage.clear).toBe("function");
    localStorage.setItem("k", "v");
    expect(localStorage.getItem("k")).toBe("v");
    expect(localStorage.length).toBe(1);
    expect(localStorage.key(0)).toBe("k");
    localStorage.removeItem("k");
    expect(localStorage.getItem("k")).toBeNull();
    localStorage.setItem("a", "1");
    localStorage.clear();
    expect(localStorage.getItem("a")).toBeNull();
    expect(localStorage.length).toBe(0);

    // sessionStorage is polyfilled the same way.
    sessionStorage.setItem("s", "1");
    expect(sessionStorage.getItem("s")).toBe("1");
  });

  it("is a no-op when a working Storage already exists (Node 24 / real jsdom path)", () => {
    const sentinel = {
      getItem: () => "sentinel",
      setItem: () => {},
      removeItem: () => {},
      clear: () => {},
      key: () => null,
      length: 0,
    } as unknown as Storage;
    Object.defineProperty(globalThis, "localStorage", { value: sentinel, configurable: true, writable: true });

    ensureWebStorage();

    // The guard must not replace an already-working store.
    expect(globalThis.localStorage).toBe(sentinel);
    expect(localStorage.getItem("anything")).toBe("sentinel");
  });
});
