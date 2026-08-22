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

// PRD #362 Decision 9: the per-run collapse preference for the run-summary cards.
// Client-side only (the pref is not worth a server round-trip and per-browser is
// acceptable), keyed by run id under ONE localStorage object so the whole map GCs in a
// single read. Each entry carries a savedAt; entries older than the TTL are dropped on
// read, so the store does not grow without bound as runs come and go. DEFAULT EXPANDED:
// an absent (or GC'd) entry reads as not-collapsed.
const SUMMARY_COLLAPSE_KEY = "uzi.summaryCollapse";
const SUMMARY_COLLAPSE_TTL_MS = 7 * 24 * 60 * 60 * 1000; // 7 days

interface CollapseEntry {
  collapsed: boolean;
  savedAt: number;
}
type CollapseStore = Record<string, CollapseEntry>;

// pruneCollapse returns the store with expired / malformed entries removed, plus whether
// anything was dropped (so a read writes the pruned store back only when it changed).
// `now` is injected (default Date.now()) so the 7-day expiry is testable without touching
// the system clock.
function pruneCollapse(store: CollapseStore, now: number): { pruned: CollapseStore; changed: boolean } {
  const pruned: CollapseStore = {};
  let changed = false;
  for (const [id, entry] of Object.entries(store ?? {})) {
    if (
      entry != null &&
      typeof entry.savedAt === "number" &&
      typeof entry.collapsed === "boolean" &&
      now - entry.savedAt < SUMMARY_COLLAPSE_TTL_MS
    ) {
      pruned[id] = entry;
    } else {
      changed = true; // expired, or a shape we did not write — drop it
    }
  }
  return { pruned, changed };
}

export const summaryCollapse = {
  // getCollapsed reads the per-run choice, GC-ing the store as a side effect. Absent =
  // expanded (false), which is the PRD default.
  getCollapsed(runId: string, now: number = Date.now()): boolean {
    const store = prefs.get<CollapseStore>(SUMMARY_COLLAPSE_KEY, {});
    const { pruned, changed } = pruneCollapse(store, now);
    if (changed) prefs.set(SUMMARY_COLLAPSE_KEY, pruned);
    return pruned[runId]?.collapsed ?? false;
  },
  // setCollapsed records the choice with a fresh savedAt, pruning expired siblings in the
  // same write so a long-lived browser never accumulates dead entries.
  setCollapsed(runId: string, collapsed: boolean, now: number = Date.now()): void {
    const store = prefs.get<CollapseStore>(SUMMARY_COLLAPSE_KEY, {});
    const { pruned } = pruneCollapse(store, now);
    pruned[runId] = { collapsed, savedAt: now };
    prefs.set(SUMMARY_COLLAPSE_KEY, pruned);
  },
};

// Issue #578: remember the last-selected Boards forge connection so a returning user lands
// back on it. Unlike summaryCollapse this is a SINGLE scalar (the chosen connection id),
// not a keyed map, so there is no GC loop — an entry past its 7-day TTL simply reads as
// null and the next set overwrites it.
const SELECTED_FORGE_KEY = "uzi.selectedForge";
const SELECTED_FORGE_TTL_MS = 7 * 24 * 60 * 60 * 1000; // 7 days

interface SelectedForgeEntry {
  id: string;
  savedAt: number;
}

export const selectedForge = {
  // get returns the remembered connection id, or null when unset, expired, or malformed.
  get(now: number = Date.now()): string | null {
    const entry = prefs.get<SelectedForgeEntry | null>(SELECTED_FORGE_KEY, null);
    if (
      entry != null &&
      typeof entry.id === "string" &&
      typeof entry.savedAt === "number" &&
      now - entry.savedAt < SELECTED_FORGE_TTL_MS
    ) {
      return entry.id;
    }
    return null;
  },
  // set records the chosen connection id with a fresh savedAt.
  set(id: string, now: number = Date.now()): void {
    prefs.set<SelectedForgeEntry>(SELECTED_FORGE_KEY, { id, savedAt: now });
  },
};
