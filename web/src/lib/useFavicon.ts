import { useEffect, useRef, useState } from "react";
import { api } from "./api";
import { applyFavicon, deriveFaviconState, failedRunIds, type FaviconRun } from "./favicon";
import { onNotificationsChanged } from "./notifications";

// useFavicon (PRD #70 M4) owns the status-favicon poll: it keeps the browser-tab
// icon in sync with the most urgent live state across the signed-in user's runs
// (deriveFaviconState) plus their unread-notification count, so a BACKGROUNDED tab
// still telegraphs "something failed / needs you / is working".
//
// Why a plain setInterval instead of the shared usePollWhileVisible: the whole
// point of a tab icon is to be read while the tab is HIDDEN, and usePollWhileVisible
// deliberately skips a tick when document.hidden. So this poller fires regardless of
// visibility. Browsers throttle background timers (often to ~1/min), which is fine —
// the visibilitychange catch-up re-polls the instant the user returns, closing any
// gap the throttle opened. The cadence is deliberately gentler than the foreground
// Board/Dashboard polls (~10s): this is an always-on, tab-wide background poll, so a
// ~20s beat keeps it cheap while staying live enough for a status dot.
const POLL_MS = 20_000;

export function useFavicon({ unread, enabled }: { unread: number; enabled: boolean }): void {
  // Latest runs, in state so the derive-and-apply effect below re-runs when either
  // the runs OR the unread count changes.
  const [runs, setRuns] = useState<FaviconRun[]>([]);
  // The "failed baseline": the ids of runs already failed at the hook's FIRST poll.
  // Seeded once so a page that opens with old failures in history never reads them as
  // fresh (deriveFaviconState only reddens a failure NOT in this set). null means "no
  // successful poll yet", which also gates the derive effect from applying too early.
  const baselineRef = useRef<Set<string> | null>(null);

  // The poll lifecycle is keyed ONLY on `enabled` — never on `unread`. `unread` is
  // consumed purely by the derive effect below, so a changing unread prop never tears
  // down and recreates the interval (which would reset the clock). Mirrors the
  // ref-for-latest-callback rationale in usePollWhileVisible.
  useEffect(() => {
    if (!enabled) {
      // Logged out / disabled: no fetch, drop the baseline so a later sign-in
      // re-seeds it, and restore the plain static brand mark.
      baselineRef.current = null;
      setRuns([]);
      applyFavicon("idle");
      return;
    }

    let alive = true;
    const poll = () => {
      api
        .listRuns()
        .then(({ runs }) => {
          if (!alive) return;
          // Seed the failed baseline on the first successful poll only.
          if (baselineRef.current === null) baselineRef.current = failedRunIds(runs);
          setRuns(runs);
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
  }, [enabled]);

  // Derive the tab signal and apply it whenever the runs or the unread count change.
  // Gated on a seeded baseline so nothing is applied before the first poll resolves.
  useEffect(() => {
    if (!enabled || baselineRef.current === null) return;
    applyFavicon(deriveFaviconState(runs, unread, baselineRef.current));
  }, [runs, unread, enabled]);
}
