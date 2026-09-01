// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { useNow } from "./useNow";

const T0 = 1_700_000_000_000;

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(T0);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("useNow", () => {
  it("returns Date.now() at mount", () => {
    const { result } = renderHook(() => useNow(1000));
    expect(result.current).toBe(T0);
  });

  it("advances to the new Date.now() after each interval", () => {
    const { result } = renderHook(() => useNow(1000));
    act(() => vi.advanceTimersByTime(1000));
    expect(result.current).toBe(T0 + 1000);
    act(() => vi.advanceTimersByTime(2000));
    expect(result.current).toBe(T0 + 3000);
  });

  it("clears the interval on unmount (timer count returns to 0)", () => {
    const { unmount } = renderHook(() => useNow(1000));
    expect(vi.getTimerCount()).toBe(1);
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("sets no timer and stays constant when intervalMs is null", () => {
    const { result } = renderHook(() => useNow(null));
    expect(vi.getTimerCount()).toBe(0);
    expect(result.current).toBe(T0);
    act(() => vi.advanceTimersByTime(60_000));
    expect(vi.getTimerCount()).toBe(0);
    expect(result.current).toBe(T0);
  });
});
