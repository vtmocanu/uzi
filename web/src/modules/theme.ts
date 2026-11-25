// `sdk.theme` for modules (PRD #4686, "Host SDK"): the current light/dark polarity and a
// subscription. The polarity comes from the validated <html data-theme> stamp that
// lib/theme.ts maintains, so a stale or tampered value falls back to the default theme's
// polarity rather than leaking an unknown string to a module.

import { DEFAULT_THEME, isTheme, themePolarity, type Polarity } from "../lib/theme";

export type ThemeMode = Polarity;

export function currentThemeMode(): ThemeMode {
  const stamped = document.documentElement.dataset.theme;
  return themePolarity(isTheme(stamped) ? stamped : DEFAULT_THEME);
}

// Calls `cb` only when the polarity changes (switching between two themes of the same
// polarity does not notify). Returns the unsubscribe function.
export function subscribeThemeMode(cb: (mode: ThemeMode) => void): () => void {
  let last = currentThemeMode();
  const observer = new MutationObserver(() => {
    const next = currentThemeMode();
    if (next === last) return;
    last = next;
    cb(next);
  });
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => observer.disconnect();
}
