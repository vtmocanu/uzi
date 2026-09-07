// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect } from "vitest";
import {
  isTheme,
  resolveTheme,
  applyTheme,
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
    // DEFAULT_THEME (legacy single-theme chain) is unchanged.
    expect(DEFAULT_THEME).toBe("ember");
  });
});
