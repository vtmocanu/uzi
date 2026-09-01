import { useEffect, useState } from "react";

// useNow returns a Date.now() clock that re-renders on the given cadence, so a
// component rendering a relative time / countdown ages without a re-render from
// elsewhere. Pass null to disable ticking (for a site that only ticks under a
// condition) — the value then stays at its mount time.
export function useNow(intervalMs: number | null): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (intervalMs == null) return;
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}
