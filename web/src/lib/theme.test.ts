// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect } from "vitest";
import { isTheme, resolveTheme, applyTheme, DEFAULT_THEME, THEMES } from "./theme";

// This jsdom build does not expose window.localStorage (same as prefs.test.ts),
// so back it with a Map-based Storage stub applyTheme's cache writes through.
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
  document.documentElement.removeAttribute("data-theme");
});

describe("isTheme", () => {
  it("accepts the registry themes and rejects anything else", () => {
    for (const t of THEMES) expect(isTheme(t)).toBe(true);
    for (const v of ["", "neon", "Ember", null, undefined, 3]) expect(isTheme(v)).toBe(false);
  });
});

describe("resolveTheme (override > instance default > ember)", () => {
  it("override wins when valid", () => {
    expect(resolveTheme("mission", "ember")).toBe("mission");
  });
  it("falls to the instance default when there is no override", () => {
    expect(resolveTheme(null, "mission")).toBe("mission");
  });
  it("falls to ember when nothing is set", () => {
    expect(resolveTheme(null, null)).toBe(DEFAULT_THEME);
    expect(DEFAULT_THEME).toBe("ember");
  });
  it("an invalid override or default falls through (never renders a bogus value)", () => {
    expect(resolveTheme("neon", "mission")).toBe("mission");
    expect(resolveTheme("neon", "bogus")).toBe("ember");
  });
});

describe("applyTheme", () => {
  it("stamps <html data-theme> and refreshes the pre-paint cache", () => {
    applyTheme("mission");
    expect(document.documentElement.dataset.theme).toBe("mission");
    expect(window.localStorage.getItem("uzi.theme")).toBe("mission");
  });
  it("ember stamps data-theme=ember, matching the ember token block (a no-op)", () => {
    applyTheme("ember");
    expect(document.documentElement.dataset.theme).toBe("ember");
  });
});
