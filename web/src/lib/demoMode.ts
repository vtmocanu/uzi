// PRD #886 — Demo mode state + reactivity.
//
// Per-device "demo mode" toggle, persisted in localStorage under `uzi_demo_mode`
// (mirrors the `uzi_mock_scenario` convention in web/src/mocks/mockApi.ts). When on,
// pure-display sites mask identifying values (see demoMask.ts) for clean screenshots;
// the real data in the DB/API is untouched and no other user is affected. Off by
// default. Every localStorage access is wrapped in try/catch — a private window can
// throw — and a failed/missing read means OFF.
//
// Unlike prefs.ts (which deliberately skips useSyncExternalStore because "no two
// components watch one key"), demo mode is the opposite case: MANY components read ONE
// key and must all re-render live on toggle. So this module exposes a proper external
// store (subscribe + getSnapshot) driving a useDemoMode() hook via useSyncExternalStore.
import { useSyncExternalStore } from "react";

const KEY = "uzi_demo_mode";

// isDemoMode reads the persisted flag. Guards a missing DOM (SSR / test setups without
// window) like prefs.ts, and wraps the read in try/catch so a disabled/quota-full or
// private-mode store can never throw into render. A failed or absent read is OFF.
// Truthy is the canonical "1" that setDemoMode writes; "true" is also accepted so a
// hand-set value reads sensibly.
export function isDemoMode(): boolean {
  if (typeof window === "undefined") return false;
  try {
    const raw = window.localStorage.getItem(KEY);
    return raw === "1" || raw === "true";
  } catch {
    return false;
  }
}

// Local in-tab subscribers. The browser `storage` event does NOT fire in the tab that
// made the change, so the toggling tab needs an explicit notify to re-render live; other
// tabs are covered by the `storage` listener attached in subscribeDemoMode.
const subscribers = new Set<() => void>();
let storageListenerAttached = false;

function notifyLocal(): void {
  for (const cb of subscribers) cb();
}

// The single window `storage` handler, attached once while there is >=1 subscriber.
// The event fires only in OTHER tabs; we react solely to our own key (or key === null,
// which fires on storage.clear()).
function onStorage(e: StorageEvent): void {
  if (e.key === KEY || e.key === null) notifyLocal();
}

// setDemoMode persists the flag (try/catch — private mode / quota) and then notifies
// local in-tab subscribers directly, so the toggling tab re-renders immediately even
// though its own write raises no `storage` event.
export function setDemoMode(on: boolean): void {
  if (typeof window !== "undefined") {
    try {
      window.localStorage.setItem(KEY, on ? "1" : "0");
    } catch {
      // Screenshot-only convenience: ignore private-mode / quota errors rather than
      // surface them. The in-tab notify below still fires so live components update.
    }
  }
  notifyLocal();
}

// subscribeDemoMode registers a local subscriber and, once, attaches the cross-tab
// `storage` listener so OTHER tabs re-render on change. Returns an unsubscribe that
// removes the subscriber and detaches the listener when the last one leaves.
export function subscribeDemoMode(cb: () => void): () => void {
  subscribers.add(cb);
  if (!storageListenerAttached && typeof window !== "undefined") {
    window.addEventListener("storage", onStorage);
    storageListenerAttached = true;
  }
  return () => {
    subscribers.delete(cb);
    if (subscribers.size === 0 && storageListenerAttached && typeof window !== "undefined") {
      window.removeEventListener("storage", onStorage);
      storageListenerAttached = false;
    }
  };
}

// useDemoMode is the React hook. getSnapshot returns the plain boolean primitive so it
// is referentially stable (no tearing / render loops); getServerSnapshot is isDemoMode
// too, which already returns false when there is no window (SSR).
export function useDemoMode(): boolean {
  return useSyncExternalStore(subscribeDemoMode, isDemoMode, isDemoMode);
}
