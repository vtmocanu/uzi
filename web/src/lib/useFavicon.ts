import { useCallback, useEffect, useRef } from "react";
import { api } from "./api";
import {
  applyFavicon,
  deriveFaviconState,
  failedRunIds,
  type FaviconRun,
  type FaviconState,
} from "./favicon";
import { onNotificationsChanged } from "./notifications";

// useFavicon (PRD #70 M4) owns the status-favicon poll: it keeps the browser-tab
// icon in sync with the most urgent live state across the signed-in user's runs
// (deriveFaviconState) plus their unread-notification count, so a BACKGROUNDED tab
// still telegraphs "something failed / needs you / is working".
//
// The tab icon is a PURE SIDE EFFECT: nothing renders from the run list, so the
// latest runs live in a ref (runsRef), not React state, and a poll tick performs NO
// setState — a re-poll that returns the same runs re-renders nothing. deriveAndApply
// reads the refs and writes the <link> only when the derived state actually changes
// (lastStateRef), so the tab is never redrawn on a no-op tick.
//
// Why a plain setInterval instead of the shared usePollWhileVisible: the whole
// point of a tab icon is to be read while the tab is HIDDEN, and usePollWhileVisible
// deliberately skips a tick when document.hidden. So this poller fires regardless of
// visibility. Browsers throttle background timers (often to ~1/min), which is fine —
// the visibilitychange catch-up re-polls the instant the user returns, closing any
// gap the throttle opened. The cadence is deliberately gentler than the foreground
// Board/Dashboard polls (~10s): this is an always-on, tab-wide background poll, so a
// ~20s beat keeps it cheap while staying live enough for a status dot. The poll is
// marked passive (X-Uzi-Passive) so the server does NOT slide the session forward on
// it (#331), letting an idle backgrounded tab still reach idle expiry while the
// favicon stays live.
const POLL_MS = 20_000;

export function useFavicon({ unread, enabled }: { unread: number; enabled: boolean }): void {
  // Latest runs, in a ref (not state): the icon is a side effect, so no component
  // renders from this and a poll tick must not trigger React work.
  const runsRef = useRef<FaviconRun[]>([]);
  // Latest unread, mirrored into a ref every render so the stable deriveAndApply
  // callback always reads the current count.
  const unreadRef = useRef(unread);
  unreadRef.current = unread;
  // The "failed baseline": the ids of runs already failed at the hook's FIRST poll.
  // Seeded once so a page that opens with old failures in history never reads them as
  // fresh (deriveFaviconState only reddens a failure NOT in this set). null means "no
  // successful poll yet".
  const baselineRef = useRef<Set<string> | null>(null);
  // The last state actually pushed to the tab icon. The runs poll hands back a
  // fresh array every tick even when nothing changed; without this guard we would
  // regenerate the canvas and rewrite the <link> href on every tick forever. We only
  // re-apply when the DERIVED state changes. Reset on disable so a re-enable re-applies.
  const lastStateRef = useRef<FaviconState | null>(null);

  // deriveAndApply reads the latest values from the refs, derives the tab signal, and
  // applies it if it changed. Stable (no deps) so the poll effect never re-subscribes.
  const deriveAndApply = useCallback(() => {
    let next: FaviconState;
    if (baselineRef.current === null) {
      // No successful runs poll yet: we cannot tell a fresh failure from a
      // pre-existing one without the seeded baseline, so we do NOT redden. But a
      // pure-unread `attention` needs no baseline (Fix 3), so surface it here; with
      // nothing to say, leave the icon as-is rather than forcing idle and fighting a
      // real state the first poll is about to derive.
      if (unreadRef.current > 0) next = "attention";
      else return;
    } else {
      next = deriveFaviconState(runsRef.current, unreadRef.current, baselineRef.current);
    }
    if (next === lastStateRef.current) return;
    lastStateRef.current = next;
    applyFavicon(next);
  }, []);

  // The poll lifecycle is keyed ONLY on `enabled` (deriveAndApply is stable) — never
  // on `unread`. A changing unread prop never tears down and recreates the interval
  // (which would reset the clock). Mirrors the ref-for-latest-callback rationale in
  // usePollWhileVisible.
  useEffect(() => {
    if (!enabled) {
      // Logged out / disabled: no fetch, drop the baseline so a later sign-in
      // re-seeds it, clear the cached runs, and restore the plain static brand mark.
      baselineRef.current = null;
      lastStateRef.current = null;
      runsRef.current = [];
      applyFavicon("idle");
      return;
    }

    let alive = true;
    const poll = () => {
      api
        .listRuns({ passive: true })
        .then(({ runs: latest }) => {
          if (!alive) return;
          // Seed the failed baseline on the first successful poll only.
          if (baselineRef.current === null) baselineRef.current = failedRunIds(latest);
          runsRef.current = latest;
          deriveAndApply();
        })
        // A failed poll is non-fatal: keep the last state rather than blanking the
        // icon (mirrors AppShell's unread-count poll tolerance).
        .catch(() => {});
    };

    poll(); // immediate seed, so the icon reflects state without waiting a full tick
    const interval = setInterval(poll, POLL_MS); // fires even while document.hidden
    // Becoming visible again re-polls immediately, catching up on any ticks the
    // browser throttled while the tab was backgrounded.
    const onVisible = () => {
      if (!document.hidden) poll();
    };
    document.addEventListener("visibilitychange", onVisible);
    // AppShell already refreshes `unread` on this event; we re-poll runs so an
    // approval/failure that lands alongside a notification shows without a full tick.
    const offNotif = onNotificationsChanged(poll);

    return () => {
      alive = false;
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisible);
      offNotif();
    };
  }, [enabled, deriveAndApply]);

  // Re-derive from the ref'd latest runs whenever `unread` changes, so a pure-unread
  // `attention` shows both before and after the baseline is seeded (Fix 3).
  useEffect(() => {
    if (!enabled) return;
    deriveAndApply();
  }, [unread, enabled, deriveAndApply]);
}
