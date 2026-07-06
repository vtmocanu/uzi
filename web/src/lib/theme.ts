// The web mirror of the Go theme registry (api/internal/theme, PRD #21): the
// canonical list of themes the SPA can render, the resolution chain, and the
// helpers that stamp <html data-theme> and keep the pre-paint cache fresh. Kept
// in lockstep with the Go registry — adding a theme is one entry here, one
// there, and one CSS block in index.css (SC5). No component branches on the
// value; the only theme-aware code is this module and the two pickers.

export const THEMES = ["ember", "mission"] as const;
export type Theme = (typeof THEMES)[number];

// DEFAULT_THEME is the no-op theme: with nothing set anywhere the SPA renders
// ember, exactly as before this PRD. Mirrors theme.Default in Go.
export const DEFAULT_THEME: Theme = "ember";

// Human labels for the pickers. A new theme adds one row here.
export const THEME_LABELS: Record<Theme, string> = {
  ember: "Ember",
  mission: "Mission control",
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
