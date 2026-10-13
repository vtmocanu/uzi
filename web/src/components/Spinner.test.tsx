// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, render } from "@testing-library/react";
import { Spinner } from "./ui";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function preference(reduce: boolean) {
  const media = { matches: reduce, addEventListener: vi.fn(), removeEventListener: vi.fn() };
  vi.stubGlobal("matchMedia", vi.fn(() => media));
  return media;
}

it("stays static without a timer under reduced motion and follows preference changes", () => {
  vi.useFakeTimers();
  const media = preference(true);
  const { container, unmount } = render(<Spinner />);
  expect(vi.getTimerCount()).toBe(0);
  expect(container.textContent).toBe("⠋");
  const update = media.addEventListener.mock.calls[0][1] as () => void;
  media.matches = false;
  act(update);
  expect(vi.getTimerCount()).toBe(1);
  act(() => vi.advanceTimersByTime(80));
  expect(container.textContent).toBe("⠙");
  media.matches = true;
  act(update);
  expect(vi.getTimerCount()).toBe(0);
  expect(container.textContent).toBe("⠋");
  unmount();
  expect(media.removeEventListener).toHaveBeenCalledWith("change", update);
});

it.each([false, true])("animates and cleans up by default; missing matchMedia %s", missing => {
  vi.useFakeTimers();
  if (missing) vi.stubGlobal("matchMedia", undefined);
  else preference(false);
  const { container, unmount } = render(<Spinner />);
  expect(vi.getTimerCount()).toBe(1);
  act(() => vi.advanceTimersByTime(160));
  expect(container.textContent).toBe("⠹");
  unmount();
  expect(vi.getTimerCount()).toBe(0);
});
