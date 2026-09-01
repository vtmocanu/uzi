// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { useAsyncData } from "./useAsyncData";
import { ApiError, errorMessage } from "./apiError";

afterEach(() => {
  cleanup();
});

// A promise whose resolution we control, so overlapping loads can be ordered.
interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (err: unknown) => void;
}
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

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
});
