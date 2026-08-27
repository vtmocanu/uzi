import { useEffect, useRef } from "react";

// usePollWhileVisible runs `cb` every `intervalMs` while the tab is visible: it
// skips a tick when `document.hidden`, and fires an immediate catch-up the moment
// the tab becomes visible again. Extracted from the board's PRD #12 poll effect so
// the dashboard reuses the same proven liveness (Board.tsx / Dashboard.tsx).
//
// The latest `cb` is stashed in a ref (the useInterval pattern) so inline-arrow
// callers are safe: the interval effect depends only on `intervalMs`, never on
// `cb`, so a caller passing a fresh closure each render does NOT tear down and
// recreate the interval (which would reset the clock and, for a callback that is
// recreated every render, never fire). It also does not silently rely on the
// caller memoising `cb` with useCallback.
export function usePollWhileVisible(cb: () => void, intervalMs: number): void {
  const cbRef = useRef(cb);
  useEffect(() => {
    cbRef.current = cb;
  }, [cb]);

  useEffect(() => {
    const tick = () => {
      if (document.hidden) return;
      cbRef.current();
    };
    const interval = setInterval(tick, intervalMs);
    const onVisible = () => {
      if (!document.hidden) tick();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [intervalMs]);
}
