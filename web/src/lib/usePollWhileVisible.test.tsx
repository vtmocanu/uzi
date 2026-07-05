// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { usePollWhileVisible } from "./usePollWhileVisible";

// jsdom reports document.hidden as false and never changes it; override the getter
// and dispatch the real event to drive the visibility path.
function defineHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
}

beforeEach(() => {
  vi.useFakeTimers();
  defineHidden(false);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  defineHidden(false);
});

describe("usePollWhileVisible", () => {
  it("does not fire on mount, then fires cb on every interval while visible", () => {
    const cb = vi.fn();
    renderHook(() => usePollWhileVisible(cb, 1000));
    expect(cb).not.toHaveBeenCalled(); // liveness is a poll, not an initial load

    act(() => vi.advanceTimersByTime(1000));
    expect(cb).toHaveBeenCalledTimes(1);
    act(() => vi.advanceTimersByTime(2000));
    expect(cb).toHaveBeenCalledTimes(3);
  });

  it("pauses (skips ticks) while the tab is hidden", () => {
    const cb = vi.fn();
    defineHidden(true);
    renderHook(() => usePollWhileVisible(cb, 1000));

    act(() => vi.advanceTimersByTime(3000));
    expect(cb).not.toHaveBeenCalled();
  });

  it("fires an immediate catch-up when the tab becomes visible again", () => {
    const cb = vi.fn();
    defineHidden(true);
    renderHook(() => usePollWhileVisible(cb, 10000));

    act(() => vi.advanceTimersByTime(10000)); // hidden: interval ticks are skipped
    expect(cb).not.toHaveBeenCalled();

    defineHidden(false);
    act(() => document.dispatchEvent(new Event("visibilitychange")));
    expect(cb).toHaveBeenCalledTimes(1); // does not wait for the next interval
  });

  it("does not fire on a visibilitychange that leaves the tab hidden", () => {
    const cb = vi.fn();
    defineHidden(true);
    renderHook(() => usePollWhileVisible(cb, 1000));

    act(() => document.dispatchEvent(new Event("visibilitychange")));
    expect(cb).not.toHaveBeenCalled();
  });

  it("always calls the latest cb without resetting the interval when cb changes", () => {
    const first = vi.fn();
    const second = vi.fn();
    const { rerender } = renderHook(({ cb }) => usePollWhileVisible(cb, 1000), {
      initialProps: { cb: first },
    });

    act(() => vi.advanceTimersByTime(600)); // partway into the first interval
    rerender({ cb: second }); // swap the callback mid-interval
    act(() => vi.advanceTimersByTime(400)); // reach the original 1000ms boundary

    // If the effect had keyed on cb, swapping it would have torn down and
    // recreated the interval — resetting the clock so nothing fires at 1000ms.
    // The ref stash keeps the original interval and invokes the newest cb.
    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledTimes(1);
  });

  it("stops firing and removes the visibility listener after unmount", () => {
    const cb = vi.fn();
    const removeSpy = vi.spyOn(document, "removeEventListener");
    const { unmount } = renderHook(() => usePollWhileVisible(cb, 1000));

    act(() => vi.advanceTimersByTime(1000));
    expect(cb).toHaveBeenCalledTimes(1);

    unmount();
    act(() => vi.advanceTimersByTime(5000));
    expect(cb).toHaveBeenCalledTimes(1); // no further ticks after teardown
    expect(removeSpy).toHaveBeenCalledWith("visibilitychange", expect.any(Function));
  });
});
