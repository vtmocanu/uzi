import { useCallback, useEffect, useRef, useState } from "react";
import { errorMessage } from "./apiError";

// useAsyncData centralizes the hand-rolled load/loading/error/reload cycle that
// web/src repeats ~33 times (PRD #950). `data` starts null; `error` is the
// errorMessage() string every site stores today (D1), "" when none. On success
// it sets data and clears error; on failure it sets error and KEEPS the last
// data (a blip never blanks working data). Every fetch is stale-response guarded
// by a monotonic generation counter — the Judge.tsx pattern that superseded the
// `alive` flag — so an older overlapping load can never clobber a newer one, and
// a fetch in flight at a deps change / unmount never lands.
//
// It does NOT poll: polling is composed on top by callers via usePollWhileVisible
// calling `reload`, so a swallowed poll path stays the caller's concern (D3).
export function useAsyncData<T>(
  fetcher: () => Promise<T>,
  deps: unknown[],
  opts?: {
    enabled?: boolean; // default true; when false the fetcher is never called
    skeleton?: "initial" | "deps" | "always"; // when loading re-arms; default "initial"
    fallback?: string; // errorMessage(err, fallback) when no mapError
    mapError?: (err: unknown) => string; // overrides fallback when provided
  },
): { data: T | null; loading: boolean; error: string; reload: () => Promise<void> } {
  const enabled = opts?.enabled ?? true;
  const skeleton = opts?.skeleton ?? "initial";

  const [data, setData] = useState<T | null>(null);
  // Initialize loading to `enabled` so an enabled:false->true flip shows no blank
  // frame: the fetch effect re-arms it true before the first fetch resolves.
  const [loading, setLoading] = useState(enabled);
  const [error, setError] = useState("");

  // Stash the caller's fetcher and error-mapper in refs (the usePollWhileVisible
  // cbRef idiom) so the fetch effect keys off `deps`/`enabled`, not off a closure
  // recreated every render — callers need not useCallback their fetcher.
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const mapErrRef = useRef<(err: unknown) => string>(() => "");
  mapErrRef.current =
    opts?.mapError ?? ((err) => errorMessage(err, opts?.fallback ?? ""));

  // Monotonic invalidation counter. A fetch applies its result only if its stamp
  // is still the latest issued; the effect cleanup bumps it to invalidate any
  // in-flight fetch. `firstRef` decides skeleton arming on the very first fetch.
  const genRef = useRef(0);
  const firstRef = useRef(true);

  const runFetch = useCallback(
    (trigger: "deps" | "reload"): Promise<void> => {
      const first = firstRef.current;
      firstRef.current = false;
      // Arm the skeleton (set loading true before awaiting) per the level:
      //   "always"  -> every fetch, including reload()
      //   "deps"    -> the first fetch and every deps-driven refetch, not reload()
      //   "initial" -> only the very first fetch
      const arm =
        skeleton === "always" ||
        (skeleton === "deps" && trigger === "deps") ||
        first;
      const gen = ++genRef.current;
      if (arm) setLoading(true);
      // Return the settle promise so a caller can `await reload()` — the migrated
      // mutation handlers await their refetch before clearing a row's busy spinner,
      // exactly as they awaited the inline `load()` today.
      return fetcherRef.current().then(
        (result) => {
          if (gen !== genRef.current) return; // a newer fetch superseded this one
          setData(result);
          setError("");
          setLoading(false);
        },
        (err) => {
          if (gen !== genRef.current) return; // superseded: drop this stale error too
          setError(mapErrRef.current(err)); // keep the last data on failure (D1)
          setLoading(false);
        },
      );
    },
    [skeleton],
  );

  const reload = useCallback((): Promise<void> => runFetch("reload"), [runFetch]);

  useEffect(() => {
    if (!enabled) {
      setLoading(false); // disabled: never fetch, and loading resolves false
      return;
    }
    // Alias the generation ref to a local so the cleanup does not read `genRef`
    // directly (react-hooks/exhaustive-deps, as in Judge.tsx): this ref is a
    // monotonic invalidation counter, not a DOM node, so reading its live value
    // in cleanup is intentional, not the stale-node hazard the rule guards.
    const genCounter = genRef;
    void runFetch("deps"); // deps-driven fetch; its settle promise is only awaited via reload()
    // Bump the generation so a fetch in flight when deps change / the component
    // unmounts never applies its result.
    return () => {
      genCounter.current++;
    };
    // The `deps` spread is the whole point of a generic fetch hook; the rule
    // cannot statically check a caller-supplied array, so it is disabled here.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, runFetch, ...deps]);

  return { data, loading, error, reload };
}
