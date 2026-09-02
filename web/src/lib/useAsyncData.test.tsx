// @vitest-environment jsdom
import { useEffect } from "react";
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, renderHook, waitFor } from "@testing-library/react";
import { useAsyncData } from "./useAsyncData";
import { ApiError, errorMessage } from "./apiError";
import { deferred, type Deferred } from "../test-helpers";

afterEach(() => {
  cleanup();
});

// A fetcher that hands back a fresh controllable deferred per call, so a test can
// resolve calls in any order and observe loading between them.
function makeController() {
  const calls: Array<Deferred<string>> = [];
  const fetcher = vi.fn(() => {
    const d = deferred<string>();
    calls.push(d);
    return d.promise;
  });
  return { fetcher, calls };
}

describe("useAsyncData", () => {
  it("success sets data, clears loading and leaves error empty", async () => {
    const { result } = renderHook(() =>
      useAsyncData(() => Promise.resolve("hi"), []),
    );
    await waitFor(() => expect(result.current.data).toBe("hi"));
    expect(result.current.loading).toBe(false);
    expect(result.current.error).toBe("");
  });

  it("maps error via fallback and keeps data null", async () => {
    const err = new Error("network"); // not an ApiError -> errorMessage returns the fallback
    const fallback = "Failed to load widgets";
    const { result } = renderHook(() =>
      useAsyncData(() => Promise.reject(err), [], { fallback }),
    );
    await waitFor(() => expect(result.current.error).toBe(fallback));
    expect(result.current.error).toBe(errorMessage(err, fallback));
    expect(result.current.data).toBeNull();
    expect(result.current.loading).toBe(false);
  });

  it("uses a custom mapError over the fallback path", async () => {
    // Give an ApiError so the fallback path would yield the server message; the
    // custom mapper must win and produce a different string.
    const err = new ApiError(404, "server-said-not-found");
    const fallback = "generic-fallback";
    const mapped = "custom-mapped-message";
    const { result } = renderHook(() =>
      useAsyncData(() => Promise.reject(err), [], {
        fallback,
        mapError: () => mapped,
      }),
    );
    await waitFor(() => expect(result.current.error).toBe(mapped));
    // Distinct from what either fallback branch would have produced.
    expect(result.current.error).not.toBe(errorMessage(err, fallback));
    expect(result.current.error).not.toBe(fallback);
  });

  it("reload() refetches and updates data", async () => {
    let n = 0;
    const { result } = renderHook(() =>
      useAsyncData(() => Promise.resolve(++n), []),
    );
    await waitFor(() => expect(result.current.data).toBe(1));
    act(() => { void result.current.reload(); });
    await waitFor(() => expect(result.current.data).toBe(2));
  });

  it("reload() returns a promise that settles AFTER data updates (await reload())", async () => {
    // Migrated mutation handlers do `await reload()` before clearing a busy spinner,
    // so the returned promise must not resolve until the refetch's state is applied.
    let n = 0;
    const { result } = renderHook(() =>
      useAsyncData(() => Promise.resolve(++n), []),
    );
    await waitFor(() => expect(result.current.data).toBe(1));
    await act(async () => {
      await result.current.reload();
    });
    // Immediately after the awaited reload resolves, the new data is already applied.
    expect(result.current.data).toBe(2);
  });

  it("enabled:false never fetches; flipping to true fetches with loading armed first", async () => {
    const fetcher = vi.fn(() => Promise.resolve("data"));
    const { result, rerender } = renderHook(
      ({ enabled }) => useAsyncData(fetcher, [], { enabled }),
      { initialProps: { enabled: false } },
    );
    expect(fetcher).not.toHaveBeenCalled();
    expect(result.current.loading).toBe(false);

    rerender({ enabled: true });
    // No blank/loaded frame: loading is already true and data still null before
    // the fetch resolves.
    expect(result.current.loading).toBe(true);
    expect(result.current.data).toBeNull();
    expect(fetcher).toHaveBeenCalledTimes(1);

    await waitFor(() => expect(result.current.data).toBe("data"));
    expect(result.current.loading).toBe(false);
  });

  it("skeleton 'initial': first mount arms loading; a deps change and reload do NOT re-arm", async () => {
    const { fetcher, calls } = makeController();
    const { result, rerender } = renderHook(
      ({ dep }) => useAsyncData(fetcher, [dep], { skeleton: "initial" }),
      { initialProps: { dep: 1 } },
    );
    // First mount: loading armed until the first fetch lands.
    expect(result.current.loading).toBe(true);
    await act(async () => {
      calls[0].resolve("a");
    });
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toBe("a");

    // Deps change: does NOT re-arm; stale data stays visible while the refetch runs.
    rerender({ dep: 2 });
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toBe("a");
    await act(async () => {
      calls[1].resolve("b");
    });
    expect(result.current.data).toBe("b");
    expect(result.current.loading).toBe(false);

    // Manual reload(): does NOT re-arm either.
    act(() => { void result.current.reload(); });
    expect(result.current.loading).toBe(false);
    await act(async () => {
      calls[2].resolve("c");
    });
    expect(result.current.data).toBe("c");
  });

  it("skeleton 'deps': a deps change re-arms loading; reload does NOT", async () => {
    const { fetcher, calls } = makeController();
    const { result, rerender } = renderHook(
      ({ dep }) => useAsyncData(fetcher, [dep], { skeleton: "deps" }),
      { initialProps: { dep: 1 } },
    );
    expect(result.current.loading).toBe(true);
    await act(async () => {
      calls[0].resolve("a");
    });
    expect(result.current.loading).toBe(false);

    // Deps-driven refetch re-arms the skeleton.
    rerender({ dep: 2 });
    expect(result.current.loading).toBe(true);
    await act(async () => {
      calls[1].resolve("b");
    });
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toBe("b");

    // Manual reload() does NOT re-arm.
    act(() => { void result.current.reload(); });
    expect(result.current.loading).toBe(false);
    await act(async () => {
      calls[2].resolve("c");
    });
    expect(result.current.data).toBe("c");
  });

  it("skeleton 'always': reload() re-arms loading", async () => {
    const { fetcher, calls } = makeController();
    const { result } = renderHook(() =>
      useAsyncData(fetcher, [], { skeleton: "always" }),
    );
    expect(result.current.loading).toBe(true);
    await act(async () => {
      calls[0].resolve("a");
    });
    expect(result.current.loading).toBe(false);

    act(() => { void result.current.reload(); });
    expect(result.current.loading).toBe(true);
    await act(async () => {
      calls[1].resolve("b");
    });
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toBe("b");
  });

  it("stale-response guard: an older fetch resolving LAST does not clobber a newer one", async () => {
    // MUTATION CHECK: this test exercises the `gen === genRef.current` guard in
    // useAsyncData. If that check is removed, the older fetch (calls[0], issued
    // first and resolved LAST here) would setData("old") after the newer result,
    // overwriting "new" with "old" — the two values are deliberately different so
    // the final expect(...).toBe("new") would then fail.
    const { fetcher, calls } = makeController();
    const { result, rerender } = renderHook(
      ({ dep }) => useAsyncData(fetcher, [dep], { skeleton: "initial" }),
      { initialProps: { dep: 1 } },
    );
    // Older load in flight.
    expect(fetcher).toHaveBeenCalledTimes(1);

    // Deps change starts a newer load; the effect cleanup bumps the generation so
    // the older load is now stale.
    rerender({ dep: 2 });
    expect(fetcher).toHaveBeenCalledTimes(2);

    // Resolve the NEWER load first — it wins.
    await act(async () => {
      calls[1].resolve("new");
    });
    expect(result.current.data).toBe("new");

    // Now resolve the OLDER load LAST — the guard must drop it.
    await act(async () => {
      calls[0].resolve("old");
    });
    expect(result.current.data).toBe("new");
  });

  // 6. BLANK-FRAME FIX (E): a disabled->enabled flip must not commit a
  // loading=false frame between the flip and the fetch. A probe records `loading`
  // in a post-commit effect (so only COMMITTED renders are captured, not the
  // render-phase-adjustment restart), and the first committed render after the
  // flip must already be loading=true.
  // MUTATION CHECK: removing the render-phase adjustment block (E) reddens this —
  // the flip then commits loading=false first, so renders[countBeforeFlip] is
  // false. (Confirmed: observed `expected false to be true`.)
  it("no blank frame: enabled false->true commits loading=true on the first render after the flip", async () => {
    const renders: boolean[] = [];
    const fetcher = vi.fn(() => Promise.resolve("data"));
    function Probe({ enabled }: { enabled: boolean }) {
      const { loading } = useAsyncData(fetcher, [], { enabled });
      // No dep array: runs after EVERY commit, so `renders` is the committed
      // loading sequence (discarded render-phase restarts never commit).
      useEffect(() => {
        renders.push(loading);
      });
      return null;
    }
    const { rerender } = render(<Probe enabled={false} />);
    expect(fetcher).not.toHaveBeenCalled();
    const countBeforeFlip = renders.length;

    await act(async () => {
      rerender(<Probe enabled={true} />);
    });

    // The first render committed after the flip already shows loading=true.
    expect(renders[countBeforeFlip]).toBe(true);
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  });

  // 7. reload() is a no-op while disabled (D).
  // MUTATION CHECK: removing the `trigger === "reload" && !enabledRef.current`
  // early return reddens this — reload() then calls the fetcher. (Confirmed:
  // observed `expected "spy" to not be called at all, but actually been called
  // 1 times`.)
  it("reload() while disabled is a no-op that resolves without fetching", async () => {
    const fetcher = vi.fn(() => Promise.resolve("data"));
    const { result } = renderHook(() =>
      useAsyncData(fetcher, [], { enabled: false }),
    );
    expect(fetcher).not.toHaveBeenCalled();
    await act(async () => {
      await result.current.reload();
    });
    expect(fetcher).not.toHaveBeenCalled();
  });

  // onFetchStart (B): fires synchronously at the start of every fetch, before the
  // fetcher, on both the initial deps fetch and a reload().
  // MUTATION CHECK: removing the `onStartRef.current?.()` call reddens this —
  // onFetchStart is never invoked. (Confirmed: observed
  // `expected "spy" to be called 1 times, but got 0 times`.)
  it("onFetchStart fires at the start of the initial fetch and again on reload, before the fetcher", async () => {
    const order: string[] = [];
    const onFetchStart = vi.fn(() => order.push("start"));
    const fetcher = vi.fn(() => {
      order.push("fetch");
      return Promise.resolve("x");
    });
    const { result } = renderHook(() =>
      useAsyncData(fetcher, [], { onFetchStart }),
    );
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
    expect(onFetchStart).toHaveBeenCalledTimes(1);

    await act(async () => {
      await result.current.reload();
    });
    expect(onFetchStart).toHaveBeenCalledTimes(2);
    expect(fetcher).toHaveBeenCalledTimes(2);
    // onFetchStart precedes the fetcher on each cycle.
    expect(order).toEqual(["start", "fetch", "start", "fetch"]);
  });

  // isCurrent (A): the fetcher receives a ctx whose isCurrent() reports whether
  // this fetch is still the latest. An older, superseded fetch sees false once a
  // newer one has been issued; the newest sees true.
  // MUTATION CHECK: reverting the ctx wiring (invoking `fetcherRef.current()`
  // with no argument) reddens this — the captured ctx is undefined and
  // `ctxs[0].isCurrent()` throws. (Confirmed: observed a TypeError reading
  // 'isCurrent' of undefined.)
  it("ctx.isCurrent(): a superseded fetch reports false while the newest reports true", async () => {
    const calls: Array<Deferred<string>> = [];
    const ctxs: Array<{ isCurrent: () => boolean }> = [];
    const fetcher = vi.fn((ctx: { isCurrent: () => boolean }) => {
      ctxs.push(ctx);
      const d = deferred<string>();
      calls.push(d);
      return d.promise;
    });
    const { rerender } = renderHook(
      ({ dep }) => useAsyncData(fetcher, [dep], { skeleton: "initial" }),
      { initialProps: { dep: 1 } },
    );
    expect(fetcher).toHaveBeenCalledTimes(1);
    // Only one fetch in flight: it is current.
    expect(ctxs[0].isCurrent()).toBe(true);

    // A deps change issues a newer fetch; the older one is now superseded.
    rerender({ dep: 2 });
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(ctxs[0].isCurrent()).toBe(false); // older fetch: superseded
    expect(ctxs[1].isCurrent()).toBe(true); // newer fetch: current

    // Settle both so no act warning is emitted.
    await act(async () => {
      calls[1].resolve("new");
      calls[0].resolve("old");
    });
  });

  // arm-clears-error (C): when a fetch arms a visible skeleton, the hook's own
  // error is cleared at that moment (before the fetch settles).
  // MUTATION CHECK: dropping the `setError("")` from the arm block reddens this —
  // the error survives the re-arm. (Confirmed: observed
  // `expected 'boom' to be ''`.)
  it("arming a skeleton clears the hook's own error before the next fetch settles", async () => {
    const calls: Array<Deferred<string>> = [];
    const fetcher = vi.fn(() => {
      const d = deferred<string>();
      calls.push(d);
      return d.promise;
    });
    const { result } = renderHook(() =>
      useAsyncData(fetcher, [], { skeleton: "always", fallback: "boom" }),
    );
    // First fetch fails -> error is set.
    await act(async () => {
      calls[0].reject(new Error("x"));
    });
    expect(result.current.error).toBe("boom");
    expect(result.current.loading).toBe(false);

    // A reload re-arms the skeleton; error must clear as loading arms.
    act(() => {
      void result.current.reload();
    });
    expect(result.current.loading).toBe(true);
    expect(result.current.error).toBe(""); // cleared at arm, before settle

    // Settle the re-armed fetch; error stays cleared.
    await act(async () => {
      calls[1].resolve("ok");
    });
    expect(result.current.error).toBe("");
  });
});
