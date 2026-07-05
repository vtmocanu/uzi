// A tiny typed localStorage wrapper for cosmetic, per-browser UI preferences
// (first use: the collapsible sidebar; also the per-board hide-empty toggle).
// Values are JSON-encoded. Every access is guarded for a missing DOM (SSR / test
// setups without window) and wrapped in try/catch so a disabled/quota-full store
// can never throw into render — a failed read returns the fallback and a failed
// write is silently dropped (the preference is cosmetic).
//
// multica's localStorage helpers do the same guarding (onboarding/
// source-backfill-dismiss.ts, editor/extensions/mention-recency.ts); we skip
// their useSyncExternalStore reactivity because no two components watch one key.
export const prefs = {
  get<T>(key: string, fallback: T): T {
    if (typeof window === "undefined") return fallback;
    try {
      const raw = window.localStorage.getItem(key);
      if (raw === null) return fallback;
      return JSON.parse(raw) as T;
    } catch {
      return fallback;
    }
  },
  set<T>(key: string, value: T): void {
    if (typeof window === "undefined") return;
    try {
      window.localStorage.setItem(key, JSON.stringify(value));
    } catch {
      // Cosmetic-only: ignore private-mode / quota errors rather than surface them.
    }
  },
};
