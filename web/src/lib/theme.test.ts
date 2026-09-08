// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import {
  isTheme,
  applyAppearance,
  DEFAULT_THEME,
  THEMES,
  POLARITIES,
  THEME_POLARITY,
  LIGHT_THEMES,
  DARK_THEMES,
  themePolarity,
  isAppearanceMode,
  isTypeface,
  isLightTheme,
  isDarkTheme,
  DEFAULT_APPEARANCE_MODE,
  DEFAULT_LIGHT_THEME,
  DEFAULT_DARK_THEME,
  DEFAULT_TYPEFACE,
  resolveAppearance,
} from "./theme";

// This jsdom build does not expose window.localStorage (same as prefs.test.ts),
// so back it with a Map-based Storage stub applyAppearance's cache writes through.
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
  // Clear any live system-mode listener held module-level in theme.ts, so one
  // test's armed matchMedia subscription cannot bleed into the next. Switching to
  // an explicit mode tears it down (no matchMedia needed for an explicit mode).
  applyAppearance({ mode: "dark", light: "hall", dark: "ember", typeface: "system" });
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-font");
});

describe("isTheme", () => {
  it("accepts the registry themes and rejects anything else", () => {
    for (const t of THEMES) expect(isTheme(t)).toBe(true);
    for (const v of ["", "neon", "Ember", null, undefined, 3]) expect(isTheme(v)).toBe(false);
  });
});

describe("polarity registry (PRD #1167 m1)", () => {
  it("POLARITIES is exactly light and dark", () => {
    expect([...POLARITIES]).toEqual(["light", "dark"]);
  });

  it("THEME_POLARITY / themePolarity agree and cover every theme", () => {
    const expected: Record<(typeof THEMES)[number], "light" | "dark"> = {
      ember: "dark",
      mission: "dark",
      dawn: "light",
      hall: "light",
      shadow: "light",
    };
    for (const t of THEMES) {
      expect(THEME_POLARITY[t]).toBe(expected[t]);
      expect(themePolarity(t)).toBe(expected[t]);
    }
  });

  it("LIGHT_THEMES / DARK_THEMES are derived from THEME_POLARITY (no drift)", () => {
    expect([...LIGHT_THEMES]).toEqual(["dawn", "hall", "shadow"]);
    expect([...DARK_THEMES]).toEqual(["ember", "mission"]);
    // Every theme lands in exactly one bucket, and each bucket matches polarity.
    for (const t of LIGHT_THEMES) expect(THEME_POLARITY[t]).toBe("light");
    for (const t of DARK_THEMES) expect(THEME_POLARITY[t]).toBe("dark");
    expect(LIGHT_THEMES.length + DARK_THEMES.length).toBe(THEMES.length);
  });
});

describe("appearance narrowers", () => {
  it("isAppearanceMode accepts system/light/dark and rejects junk", () => {
    for (const v of ["system", "light", "dark"]) expect(isAppearanceMode(v)).toBe(true);
    for (const v of ["", "System", "auto", "ember", null, undefined, 1])
      expect(isAppearanceMode(v)).toBe(false);
  });

  it("isTypeface accepts system/plex and rejects junk", () => {
    for (const v of ["system", "plex"]) expect(isTypeface(v)).toBe(true);
    for (const v of ["", "Plex", "serif", null, undefined, 0]) expect(isTypeface(v)).toBe(false);
  });

  it("isLightTheme requires a valid id AND light polarity", () => {
    for (const v of ["dawn", "hall", "shadow"]) expect(isLightTheme(v)).toBe(true);
    // dark themes are valid ids but wrong polarity -> rejected
    expect(isLightTheme("ember")).toBe(false);
    expect(isLightTheme("mission")).toBe(false);
    for (const v of ["", "neon", "Dawn", null, undefined, 2]) expect(isLightTheme(v)).toBe(false);
  });

  it("isDarkTheme requires a valid id AND dark polarity", () => {
    for (const v of ["ember", "mission"]) expect(isDarkTheme(v)).toBe(true);
    // light themes are valid ids but wrong polarity -> rejected
    expect(isDarkTheme("dawn")).toBe(false);
    expect(isDarkTheme("hall")).toBe(false);
    for (const v of ["", "neon", "Ember", null, undefined, 2]) expect(isDarkTheme(v)).toBe(false);
  });
});

describe("resolveAppearance (per-field: override > default > compiled fallback)", () => {
  it("a valid override wins for every field", () => {
    const got = resolveAppearance(
      { mode: "light", light: "dawn", dark: "mission", typeface: "plex" },
      { mode: "system", light: "hall", dark: "ember", typeface: "system" },
    );
    expect(got).toEqual({ mode: "light", light: "dawn", dark: "mission", typeface: "plex" });
  });

  it("an invalid or null override falls to a valid instance default, per field", () => {
    const got = resolveAppearance(
      { mode: null, light: "bogus", dark: undefined, typeface: "serif" },
      { mode: "light", light: "shadow", dark: "mission", typeface: "plex" },
    );
    expect(got).toEqual({ mode: "light", light: "shadow", dark: "mission", typeface: "plex" });
  });

  it("invalid override AND invalid default falls to the compiled fallback, per field", () => {
    const got = resolveAppearance(
      { mode: "auto", light: "neon", dark: "neon", typeface: "comic" },
      { mode: "auto", light: "neon", dark: "neon", typeface: "comic" },
    );
    expect(got).toEqual({
      mode: DEFAULT_APPEARANCE_MODE,
      light: DEFAULT_LIGHT_THEME,
      dark: DEFAULT_DARK_THEME,
      typeface: DEFAULT_TYPEFACE,
    });
  });

  it("a polarity-mismatched theme override is rejected and falls through", () => {
    // ember is a valid id but dark: invalid for the light slot -> take default.
    // dawn is a valid id but light: invalid for the dark slot -> take default.
    const got = resolveAppearance(
      { light: "ember", dark: "dawn" },
      { light: "hall", dark: "mission" },
    );
    expect(got.light).toBe("hall");
    expect(got.dark).toBe("mission");
    // and when the default is ALSO mismatched, the compiled fallback wins.
    const got2 = resolveAppearance(
      { light: "ember", dark: "dawn" },
      { light: "mission", dark: "hall" },
    );
    expect(got2.light).toBe(DEFAULT_LIGHT_THEME);
    expect(got2.dark).toBe(DEFAULT_DARK_THEME);
  });

  it("a fully-null overrides object with empty defaults yields the compiled fallback", () => {
    const got = resolveAppearance(
      { mode: null, light: null, dark: null, typeface: null },
      {},
    );
    expect(got).toEqual({ mode: "dark", light: "hall", dark: "ember", typeface: "system" });
  });

  it("the compiled fallbacks are the documented values", () => {
    expect(DEFAULT_APPEARANCE_MODE).toBe("dark");
    expect(DEFAULT_LIGHT_THEME).toBe("hall");
    expect(DEFAULT_DARK_THEME).toBe("ember");
    expect(DEFAULT_TYPEFACE).toBe("system");
    // DEFAULT_THEME (the web mirror of Go theme.Default) is unchanged.
    expect(DEFAULT_THEME).toBe("ember");
  });
});

// A controllable fake MediaQueryList (jsdom does not implement matchMedia): a
// settable `matches` plus add/removeEventListener spies, and a fireChange() that
// invokes every captured listener so a test can simulate an OS colour-scheme flip.
function makeMatchMedia(initialDark: boolean) {
  let matches = initialDark;
  const listeners = new Set<() => void>();
  const add = vi.fn((_type: string, cb: () => void) => {
    listeners.add(cb);
  });
  const remove = vi.fn((_type: string, cb: () => void) => {
    listeners.delete(cb);
  });
  const mql = {
    get matches() {
      return matches;
    },
    addEventListener: add,
    removeEventListener: remove,
  };
  return {
    matchMedia: vi.fn(() => mql),
    add,
    remove,
    setMatches: (v: boolean) => {
      matches = v;
    },
    fireChange: () => listeners.forEach((cb) => cb()),
  };
}

function stubMatchMedia(mm: { matchMedia: unknown }) {
  Object.defineProperty(window, "matchMedia", { configurable: true, value: mm.matchMedia });
}

describe("applyAppearance (PRD #1167 m5)", () => {
  it("stamps data-theme=light on mode=light regardless of the OS, plus data-font", () => {
    stubMatchMedia(makeMatchMedia(true)); // OS dark, but an explicit light wins
    applyAppearance({ mode: "light", light: "dawn", dark: "ember", typeface: "plex" });
    expect(document.documentElement.dataset.theme).toBe("dawn");
    expect(document.documentElement.dataset.font).toBe("plex");
  });

  it("stamps data-theme=dark (the dark slot) on mode=dark", () => {
    stubMatchMedia(makeMatchMedia(false));
    applyAppearance({ mode: "dark", light: "dawn", dark: "mission", typeface: "system" });
    expect(document.documentElement.dataset.theme).toBe("mission");
  });

  it("stamps per matchMedia on mode=system (light OS -> light slot, dark OS -> dark slot)", () => {
    stubMatchMedia(makeMatchMedia(false));
    applyAppearance({ mode: "system", light: "hall", dark: "ember", typeface: "system" });
    expect(document.documentElement.dataset.theme).toBe("hall");

    stubMatchMedia(makeMatchMedia(true));
    applyAppearance({ mode: "system", light: "hall", dark: "ember", typeface: "system" });
    expect(document.documentElement.dataset.theme).toBe("ember");
  });

  it("writes the uzi.appearance cache with the narrowed values", () => {
    stubMatchMedia(makeMatchMedia(false));
    applyAppearance({ mode: "light", light: "dawn", dark: "mission", typeface: "plex" });
    expect(window.localStorage.getItem("uzi.appearance")).toBe(
      JSON.stringify({ mode: "light", light: "dawn", dark: "mission", typeface: "plex" }),
    );
  });

  it("narrows bad fields to the compiled fallbacks before stamping/caching", () => {
    stubMatchMedia(makeMatchMedia(false));
    applyAppearance({ mode: "auto", light: "ember", dark: "dawn", typeface: "comic" });
    // mode->dark (default), light->hall, dark->ember, typeface->system; painted dark slot.
    expect(document.documentElement.dataset.theme).toBe("ember");
    expect(document.documentElement.dataset.font).toBe("system");
    expect(window.localStorage.getItem("uzi.appearance")).toBe(
      JSON.stringify({ mode: "dark", light: "hall", dark: "ember", typeface: "system" }),
    );
  });

  it("subscribes a change listener ONLY for system mode, and removes it on switch to explicit", () => {
    const mm = makeMatchMedia(false);
    stubMatchMedia(mm);
    applyAppearance({ mode: "system", light: "hall", dark: "ember", typeface: "system" });
    // Armed on this call's mql; its own remove spy has not fired yet.
    expect(mm.add).toHaveBeenCalledTimes(1);
    expect(mm.remove).not.toHaveBeenCalled();
    // Switching to an explicit mode tears the listener down.
    applyAppearance({ mode: "dark", light: "hall", dark: "ember", typeface: "system" });
    expect(mm.remove).toHaveBeenCalledTimes(1);
  });

  it("the system listener restamps data-theme on a simulated OS change", () => {
    const mm = makeMatchMedia(false); // OS light initially
    stubMatchMedia(mm);
    applyAppearance({ mode: "system", light: "dawn", dark: "mission", typeface: "system" });
    expect(document.documentElement.dataset.theme).toBe("dawn");
    // OS flips to dark; firing the change event restamps to the dark slot.
    mm.setMatches(true);
    mm.fireChange();
    expect(document.documentElement.dataset.theme).toBe("mission");
  });
});
