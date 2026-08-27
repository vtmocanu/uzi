// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import type { FaviconRun } from "./favicon";

// Spies used inside the hoisted vi.mock factories. vi.hoisted lets us define them
// above the mock so the hoisted factory can safely close over them.
const { applyFavicon, listRuns } = vi.hoisted(() => ({
  applyFavicon: vi.fn(),
  listRuns: vi.fn(),
}));

// Keep the real pure derivation (deriveFaviconState / failedRunIds) but replace the
// DOM-writing applyFavicon with a spy, so we assert the DERIVED state without a real
// canvas or <link>.
vi.mock("./favicon", async () => {
  const actual = await vi.importActual<typeof import("./favicon")>("./favicon");
  return { ...actual, applyFavicon };
});

// Controllable listRuns: each call resolves with whatever `nextRuns` currently holds.
let nextRuns: FaviconRun[] = [];
vi.mock("./api", () => ({ api: { listRuns: (...args: unknown[]) => listRuns(...args) } }));

// notifications: capture the subscriber but we don't need to drive it here.
vi.mock("./notifications", () => ({ onNotificationsChanged: () => () => {} }));

import { useFavicon } from "./useFavicon";

function run(id: string, status: string): FaviconRun {
  return { id, status, stop_kind: null };
}

// Flush the pending listRuns promise (and any follow-up microtasks) inside act, so
// the poll's `.then` runs before we assert.
async function flush() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  applyFavicon.mockReset();
  listRuns.mockReset();
  nextRuns = [];
  // Default: resolve with the current nextRuns. Individual tests override for the
  // pre-baseline (hanging poll) cases.
  listRuns.mockImplementation(() => Promise.resolve({ runs: nextRuns }));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("useFavicon", () => {
  it("Fix 3: applies attention from unread BEFORE any runs poll resolves", async () => {
    // listRuns never resolves this tick, so the baseline stays null.
    listRuns.mockReturnValueOnce(new Promise(() => {}));
    renderHook(() => useFavicon({ unread: 1, enabled: true }));

    // The unread-keyed effect runs synchronously on mount; no poll has resolved.
    expect(applyFavicon).toHaveBeenCalledWith("attention");
  });

  it("Fix 3: nothing is applied pre-baseline with no unread (no premature redden)", async () => {
    // First poll hangs, so the baseline stays null and runsRef stays empty. With no
    // unread there is nothing the pre-baseline branch can surface, so it must apply
    // nothing at all — in particular it must not redden, which requires the seeded
    // baseline to tell a fresh failure from a pre-existing one.
    listRuns.mockReturnValueOnce(new Promise(() => {}));
    renderHook(() => useFavicon({ unread: 0, enabled: true }));

    expect(applyFavicon).not.toHaveBeenCalledWith("failed");
    expect(applyFavicon).not.toHaveBeenCalled();
  });

  it("Fix 3: reddens only once a poll resolves to seed the baseline with a fresh failure", async () => {
    // First poll: empty, seeds an empty baseline. Second poll: a NEW failed run —
    // fresh relative to the seeded baseline, so it reddens.
    nextRuns = [];
    const { rerender } = renderHook((props: { unread: number }) => useFavicon({ ...props, enabled: true }), {
      initialProps: { unread: 0 },
    });
    await flush(); // seed baseline (empty), derive idle
    expect(applyFavicon).not.toHaveBeenCalledWith("failed");

    nextRuns = [run("r1", "failed")];
    await act(async () => {
      vi.advanceTimersByTime(20_000); // next poll tick
    });
    await flush();
    rerender({ unread: 0 });
    expect(applyFavicon).toHaveBeenCalledWith("failed");
  });

  it("Fix 4: two polls with the SAME runs apply the icon at most once for that state", async () => {
    nextRuns = [run("r1", "running")];
    renderHook(() => useFavicon({ unread: 0, enabled: true }));
    await flush(); // first poll resolves -> running
    expect(applyFavicon).toHaveBeenCalledWith("running");
    const callsAfterFirst = applyFavicon.mock.calls.length;

    // Second tick, identical runs: the lastStateRef guard should suppress a re-apply.
    await act(async () => {
      vi.advanceTimersByTime(20_000);
    });
    await flush();
    expect(applyFavicon.mock.calls.length).toBe(callsAfterFirst);
  });

  it("derives a normal seeded run (running) after the poll resolves", async () => {
    nextRuns = [run("r1", "running")];
    renderHook(() => useFavicon({ unread: 0, enabled: true }));
    await flush();
    expect(applyFavicon).toHaveBeenCalledWith("running");
  });

  it("#331: marks the favicon poll passive so the server skips the rolling refresh", async () => {
    renderHook(() => useFavicon({ unread: 0, enabled: true }));
    await flush(); // immediate seed poll
    expect(listRuns).toHaveBeenCalledWith({ passive: true });
  });
});
