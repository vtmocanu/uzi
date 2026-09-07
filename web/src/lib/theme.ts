// The web mirror of the Go theme registry (api/internal/theme, PRD #21): the
// canonical list of themes the SPA can render, the resolution chain, and the
// helpers that stamp <html data-theme> and keep the pre-paint cache fresh. Kept
// in lockstep with the Go registry — adding a theme is one entry here, one
// there, and one CSS block in index.css (SC5). No component branches on the
// value; the only theme-aware code is this module and the two pickers.

export const THEMES = ["ember", "mission", "dawn", "hall", "shadow"] as const;
export type Theme = (typeof THEMES)[number];

// DEFAULT_THEME is the no-op theme: with nothing set anywhere the SPA renders
// ember, exactly as before this PRD. Mirrors theme.Default in Go.
export const DEFAULT_THEME: Theme = "ember";

// Human labels for the pickers. A new theme adds one row here.
export const THEME_LABELS: Record<Theme, string> = {
  ember: "Ember",
  mission: "Mission control",
  dawn: "Dawn",
  hall: "Hall",
  shadow: "Shadow",
};

// STORAGE_KEY is the pre-paint cache slot: the last resolved theme, read by the
// inline <head> script in index.html before the bundle loads so the first paint
// is themed (no flash of the wrong theme). The server-resolved value wins once
// me() returns and refreshes this cache.
const STORAGE_KEY = "uzi.theme";

// isTheme narrows an untrusted value (localStorage, an API field) to a known
// theme id.
export function isTheme(v: unknown): v is Theme {
  return typeof v === "string" && (THEMES as readonly string[]).includes(v);
}

// resolveTheme mirrors the server chain (Go theme.Resolve): a valid override
// wins, else a valid instance default, else ember. Invalid values fall through
// defensively — writes are validated, so this only guards stale/tampered data.
export function resolveTheme(
  override: string | null | undefined,
  instanceDefault: string | null | undefined,
): Theme {
  if (isTheme(override)) return override;
  if (isTheme(instanceDefault)) return instanceDefault;
  return DEFAULT_THEME;
}

// applyTheme stamps <html data-theme> and refreshes the pre-paint cache. Storage
// access is guarded so a private-mode / locked-storage browser still themes (it
// only loses flash-avoidance on the next cold load). Called live on a picker
// change and whenever a session response resolves.
export function applyTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // Storage unavailable (private mode / disabled): theming still works; only
    // the pre-paint cache is skipped.
  }
}

// ---------------------------------------------------------------------------
// Appearance contract (PRD #1167 "Lights on", milestone m1). Web mirror of the
// Go registry in api/internal/theme: each theme carries a fixed polarity, and a
// per-user Appearance splits into a light/dark theme pair plus a system/light/
// dark mode switch and a typeface choice, each field resolved independently
// (override > instance default > compiled fallback). This module only owns the
// data and the resolution; stamping <html> from an Appearance (matchMedia, the
// system-mode listener) is a later milestone and deliberately not here.
// ---------------------------------------------------------------------------

// Polarity is whether a theme reads as light-on-dark or dark-on-light. Every
// theme has exactly one; it is what a light/dark mode switch selects between.
export type Polarity = "light" | "dark";

// POLARITIES is the closed set of polarities, for iteration and validation.
export const POLARITIES = ["light", "dark"] as const;

// THEME_POLARITY pins each theme to its polarity. The two original themes are
// dark; the three PRD #1167 additions are light. Mirrors theme.Polarity in Go.
export const THEME_POLARITY: Record<Theme, Polarity> = {
  ember: "dark",
  mission: "dark",
  dawn: "light",
  hall: "light",
  shadow: "light",
};

// LIGHT_THEMES / DARK_THEMES are DERIVED from THEME_POLARITY (never hand-listed)
// so a new theme's polarity is stated in exactly one place and these cannot
// drift out of sync with it.
export const LIGHT_THEMES: readonly Theme[] = THEMES.filter(
  (t) => THEME_POLARITY[t] === "light",
);
export const DARK_THEMES: readonly Theme[] = THEMES.filter(
  (t) => THEME_POLARITY[t] === "dark",
);

// themePolarity returns the polarity of a known theme id. Mirrors the Go helper.
export function themePolarity(id: Theme): Polarity {
  return THEME_POLARITY[id];
}

// AppearanceMode is the light/dark selector: "system" follows the OS preference,
// "light"/"dark" pin a polarity. Mirrors theme.AppearanceMode in Go.
export type AppearanceMode = "system" | "light" | "dark";

// Typeface is the font choice: "system" is the platform default, "plex" the
// bundled IBM Plex family. Mirrors theme.Typeface in Go.
export type Typeface = "system" | "plex";

// Appearance is a fully-resolved per-user appearance: the mode switch, the theme
// used for each polarity, and the typeface. Mirrors theme.Appearance in Go.
export interface Appearance {
  mode: AppearanceMode;
  light: Theme;
  dark: Theme;
  typeface: Typeface;
}

// isAppearanceMode narrows an untrusted value to a known appearance mode.
export function isAppearanceMode(v: unknown): v is AppearanceMode {
  return v === "system" || v === "light" || v === "dark";
}

// isTypeface narrows an untrusted value to a known typeface.
export function isTypeface(v: unknown): v is Typeface {
  return v === "system" || v === "plex";
}

// isLightTheme narrows to a theme id that is BOTH valid AND light-polarity, so a
// dark theme in a "light" slot (e.g. stale/tampered data) is rejected.
export function isLightTheme(v: unknown): v is Theme {
  return isTheme(v) && THEME_POLARITY[v] === "light";
}

// isDarkTheme narrows to a theme id that is BOTH valid AND dark-polarity.
export function isDarkTheme(v: unknown): v is Theme {
  return isTheme(v) && THEME_POLARITY[v] === "dark";
}

// Compiled fallbacks: the appearance applied when neither the user override nor
// the instance default provides a valid value for a field. Mirrors the Go
// defaults. DEFAULT_THEME (above) stays "ember" for the legacy single-theme
// chain; the dark-slot fallback here is the same ember.
export const DEFAULT_APPEARANCE_MODE: AppearanceMode = "dark";
export const DEFAULT_LIGHT_THEME: Theme = "hall";
export const DEFAULT_DARK_THEME: Theme = "ember";
export const DEFAULT_TYPEFACE: Typeface = "system";

// AppearanceInput is the raw nullable per-field shape both resolveAppearance
// arguments take: user overrides and instance defaults are each a bag of
// possibly-missing, possibly-invalid strings straight off the wire.
type AppearanceInput = {
  mode?: string | null;
  light?: string | null;
  dark?: string | null;
  typeface?: string | null;
};

// resolveAppearance mirrors the server chain (Go theme.ResolveAppearance): each
// field resolves independently — a valid override wins, else a valid instance
// default, else the compiled fallback. "valid" for the theme slots is
// polarity-aware (isLightTheme / isDarkTheme), so a dark id offered for the
// light slot is rejected and falls through rather than being rendered.
export function resolveAppearance(
  overrides: AppearanceInput,
  defaults: AppearanceInput,
): Appearance {
  return {
    mode: isAppearanceMode(overrides.mode)
      ? overrides.mode
      : isAppearanceMode(defaults.mode)
        ? defaults.mode
        : DEFAULT_APPEARANCE_MODE,
    light: isLightTheme(overrides.light)
      ? overrides.light
      : isLightTheme(defaults.light)
        ? defaults.light
        : DEFAULT_LIGHT_THEME,
    dark: isDarkTheme(overrides.dark)
      ? overrides.dark
      : isDarkTheme(defaults.dark)
        ? defaults.dark
        : DEFAULT_DARK_THEME,
    typeface: isTypeface(overrides.typeface)
      ? overrides.typeface
      : isTypeface(defaults.typeface)
        ? defaults.typeface
        : DEFAULT_TYPEFACE,
  };
}
