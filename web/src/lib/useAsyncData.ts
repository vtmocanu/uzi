import { useCallback, useEffect, useRef, useState } from "react";
import { errorMessage } from "./apiError";

// useAsyncData centralizes the hand-rolled load/loading/error/reload cycle —
// the ~34 hand-rolled load cycles PRD #950 identified, 27 migrated to this hook.
// `data` starts null; `error` is the errorMessage() string every site stores
// today (D1), "" when none. On success it sets data and clears error; on failure
// it sets error and KEEPS the last data (a blip never blanks working data). Every
// fetch is stale-response guarded by a monotonic generation counter — the
// Judge.tsx pattern that superseded the `alive` flag — so an older overlapping
// load can never clobber a newer one. That guarantee covers the hook's OWN
// data/error/loading via the gen guard: a fetch in flight at a deps change /
// unmount never lands its result into hook state. A side-effect fetcher that
// seeds its OWN setters gets the same guarantee for those values only by wrapping
// each in ctx.isCurrent() — the gen guard cannot reach a setter the hook never
// sees.
//
// It does NOT poll: polling is composed on top by callers via usePollWhileVisible
// calling `reload`, so a swallowed poll path stays the caller's concern (D3).
export function useAsyncData<T>(
  fetcher: (ctx: { isCurrent: () => boolean }) => Promise<T>,
  deps: unknown[],
  opts?: {
    enabled?: boolean; // default true; when false the fetcher is never called
    skeleton?: "initial" | "deps" | "always"; // when loading re-arms; default "initial"
    fallback?: string; // errorMessage(err, fallback) when no mapError
    mapError?: (err: unknown) => string; // overrides fallback when provided
    // called synchronously at the start of every fetch (deps-driven and reload)
    // so a caller can clear its own error slot; restores the pre-migration load()
    // opener.
    onFetchStart?: () => void;
  },
): { data: T | null; loading: boolean; error: string; reload: () => Promise<void> } {
  const enabled = opts?.enabled ?? true;
  const skeleton = opts?.skeleton ?? "initial";

  const [data, setData] = useState<T | null>(null);
  // Loading is armed for a disabled->enabled transition in two complementary
  // places, so no `loading=false, data=null` blank frame is ever committed on an
  // enable: the `useState(enabled)` initializer covers the FIRST mount, and the
  // render-phase adjustment below covers a LATER enabled flip (React's documented
  // "adjust state during render on a prop change" pattern — it re-renders before
  // commit, so the loaded frame is never painted). That adjustment also arms
  // loading on a re-enable for skeleton:"initial" sites, which is intentional.
  const [loading, setLoading] = useState(enabled);
  const [error, setError] = useState("");

  // Latest `enabled`, read inside runFetch's reload path without widening its
  // deps. `prevEnabledRef` tracks the value across renders to detect a flip.
  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;
  const prevEnabledRef = useRef(enabled);
  if (enabled !== prevEnabledRef.current) {
    prevEnabledRef.current = enabled;
    setLoading(enabled);
  }

  // Stash the caller's fetcher and error-mapper in refs (the usePollWhileVisible
  // cbRef idiom) so the fetch effect keys off `deps`/`enabled`, not off a closure
  // recreated every render — callers need not useCallback their fetcher.
  const fetcherRef = useRef<(ctx: { isCurrent: () => boolean }) => Promise<T>>(
    fetcher,
  );
  fetcherRef.current = fetcher;
  const mapErrRef = useRef<(err: unknown) => string>(() => "");
  mapErrRef.current =
    opts?.mapError ?? ((err) => errorMessage(err, opts?.fallback ?? ""));
  const onStartRef = useRef(opts?.onFetchStart);
  onStartRef.current = opts?.onFetchStart;

  // Monotonic invalidation counter. A fetch applies its result only if its stamp
  // is still the latest issued; the effect cleanup bumps it to invalidate any
  // in-flight fetch. `firstRef` decides skeleton arming on the very first fetch.
  const genRef = useRef(0);
  const firstRef = useRef(true);

  const runFetch = useCallback(
    (trigger: "deps" | "reload"): Promise<void> => {
      // A reload() while disabled is a no-op: don't bump the generation, fire
      // onFetchStart, or fetch. The deps path is already guarded by `enabled` in
      // the effect, so this only intercepts the imperative reload().
      if (trigger === "reload" && !enabledRef.current) return Promise.resolve();
      // Fire the caller's start hook synchronously at the very top so it can clear
      // its own error slot on every fetch (deps-driven and reload). It is stashed
      // in a ref (like fetcherRef) so it is NOT a runFetch dependency — adding it
      // would rebuild runFetch every render and re-fire the fetch effect.
      onStartRef.current?.();
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
      // A stamp the fetcher can consult: true until a newer fetch supersedes this
      // one. A side-effect fetcher wraps its own setters in ctx.isCurrent() to get
      // the same stale-drop guarantee the hook applies to its own state.
      const isCurrent = () => gen === genRef.current;
      // Clear the hook's own error only when arming a visible skeleton (not on a
      // non-arming reload), so a re-arm shows a clean loading frame.
      if (arm) {
        setError("");
        setLoading(true);
      }
      // Return the settle promise so a caller can `await reload()` — the migrated
      // mutation handlers await their refetch before clearing a row's busy spinner,
      // exactly as they awaited the inline `load()` today.
      return fetcherRef.current({ isCurrent }).then(
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
