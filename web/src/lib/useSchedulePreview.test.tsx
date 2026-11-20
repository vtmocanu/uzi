// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { StrictMode, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import type { ScheduleSettingsPreview, ScheduleSettingsPreviewRequest } from "./apiTypes";

const previewScheduleSettings = vi.fn();
vi.mock("./api", () => ({
  api: {
    previewScheduleSettings: (...args: unknown[]) => previewScheduleSettings(...args),
  },
}));

import { api } from "./api";
import { useSchedulePreview } from "./useSchedulePreview";

const responses = RECORDED_EFFECTIVE_SCENARIOS[0].schedule_previews;
const respA = responses[0].response;
const respB = responses[1].response;

const reqA: ScheduleSettingsPreviewRequest = { target: "sweep", model: null, override_subagent_model: false };
const reqB: ScheduleSettingsPreviewRequest = { ...reqA, harness: "codex" };

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

async function flush(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  previewScheduleSettings.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

type Props = { req: ScheduleSettingsPreviewRequest | null; refreshKey?: string };

function setup(initial: Props, debounceMs = 0, strict = false) {
  const wrapper = strict ? ({ children }: { children: ReactNode }) => <StrictMode>{children}</StrictMode> : undefined;
  return renderHook(({ req, refreshKey }: Props) => useSchedulePreview(req, { debounceMs, refreshKey }), {
    initialProps: initial,
    wrapper,
  });
}

describe("useSchedulePreview", () => {
  it("null request is idle and fetches nothing", async () => {
    const { result } = setup({ req: null });
    await flush(50);
    expect(result.current.status).toBe("idle");
    expect(previewScheduleSettings).not.toHaveBeenCalled();
  });

  it("loading then ready with a view", async () => {
    previewScheduleSettings.mockResolvedValue(respA);
    const { result } = setup({ req: reqA });
    expect(result.current.status).toBe("loading");
    await flush();
    expect(result.current.status).toBe("ready");
    expect(result.current.preview).toBe(respA);
    expect(result.current.view?.harness?.chosenAtCreate).toBe(true);
    expect(previewScheduleSettings).toHaveBeenCalledTimes(1);
    expect(previewScheduleSettings).toHaveBeenCalledWith(reqA);
  });

  it("rejection is error", async () => {
    previewScheduleSettings.mockRejectedValue(new Error("boom"));
    const { result } = setup({ req: reqA });
    await flush();
    expect(result.current.status).toBe("error");
    expect(result.current.view).toBeNull();
  });

  it("a missing api method is error", async () => {
    const saved = (api as unknown as Record<string, unknown>).previewScheduleSettings;
    (api as unknown as Record<string, unknown>).previewScheduleSettings = undefined;
    try {
      const { result } = setup({ req: reqA });
      await flush();
      expect(result.current.status).toBe("error");
    } finally {
      (api as unknown as Record<string, unknown>).previewScheduleSettings = saved;
    }
  });

  it("debounce coalesces to the last request", async () => {
    previewScheduleSettings.mockResolvedValue(respB);
    const { result, rerender } = setup({ req: reqA }, 100);
    await flush(50);
    rerender({ req: reqB });
    await flush(50);
    expect(previewScheduleSettings).not.toHaveBeenCalled();
    await flush(60);
    expect(previewScheduleSettings).toHaveBeenCalledTimes(1);
    expect(previewScheduleSettings).toHaveBeenCalledWith(reqB);
    expect(result.current.status).toBe("ready");
  });

  it("a superseded in-flight response for the same key does not publish after A to B to A", async () => {
    const a1 = deferred<ScheduleSettingsPreview>();
    const a2 = deferred<ScheduleSettingsPreview>();
    previewScheduleSettings.mockReturnValueOnce(a1.promise).mockReturnValueOnce(new Promise(() => {})).mockReturnValueOnce(a2.promise);
    const { result, rerender } = setup({ req: reqA });
    await flush();
    rerender({ req: reqB });
    await flush();
    rerender({ req: reqA });
    await flush();
    expect(previewScheduleSettings).toHaveBeenCalledTimes(3);
    await act(async () => {
      a1.resolve(respB);
      await Promise.resolve();
    });
    await flush();
    expect(result.current.status).toBe("loading");
    await act(async () => {
      a2.resolve(respA);
      await Promise.resolve();
    });
    await flush();
    expect(result.current.status).toBe("ready");
    expect(result.current.preview).toBe(respA);
  });

  it("A to B to A within the debounce window never shows a stale ready", async () => {
    previewScheduleSettings.mockResolvedValue(respA);
    const { result, rerender } = setup({ req: reqA }, 100);
    await flush(100);
    expect(result.current.status).toBe("ready");
    rerender({ req: reqB });
    expect(result.current.status).toBe("loading");
    rerender({ req: reqA });
    await flush(10);
    expect(result.current.status).toBe("loading");
    await flush(100);
    expect(result.current.status).toBe("ready");
    expect(previewScheduleSettings).toHaveBeenCalledTimes(2);
  });

  it("out of order: the older response never replaces the latest", async () => {
    const slowA = deferred<ScheduleSettingsPreview>();
    previewScheduleSettings.mockImplementation((r: ScheduleSettingsPreviewRequest) =>
      r.harness === "codex" ? Promise.resolve(respB) : slowA.promise,
    );
    const { result, rerender } = setup({ req: reqA });
    await flush();
    rerender({ req: reqB });
    await flush();
    expect(result.current.preview).toBe(respB);
    await act(async () => {
      slowA.resolve(respA);
      await Promise.resolve();
    });
    await flush();
    expect(result.current.status).toBe("ready");
    expect(result.current.preview).toBe(respB);
  });

  it("works under StrictMode double effects", async () => {
    previewScheduleSettings.mockResolvedValue(respA);
    const { result } = setup({ req: reqA }, 0, true);
    await flush();
    expect(result.current.status).toBe("ready");
  });

  it("unmount drops the in-flight response without warnings", async () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    const d = deferred<ScheduleSettingsPreview>();
    previewScheduleSettings.mockReturnValue(d.promise);
    const { unmount } = setup({ req: reqA });
    await flush();
    unmount();
    await act(async () => {
      d.resolve(respA);
      await Promise.resolve();
    });
    expect(err).not.toHaveBeenCalled();
  });

  it("unmount before the debounce fires sends nothing", async () => {
    const { unmount } = setup({ req: reqA }, 100);
    unmount();
    await flush(200);
    expect(previewScheduleSettings).not.toHaveBeenCalled();
  });

  it("a refreshKey change refetches", async () => {
    previewScheduleSettings.mockResolvedValue(respA);
    const { result, rerender } = setup({ req: reqA, refreshKey: "1" });
    await flush();
    expect(result.current.status).toBe("ready");
    rerender({ req: reqA, refreshKey: "2" });
    expect(result.current.status).toBe("loading");
    await flush();
    expect(result.current.status).toBe("ready");
    expect(previewScheduleSettings).toHaveBeenCalledTimes(2);
  });

  it("going back to null returns to idle", async () => {
    previewScheduleSettings.mockResolvedValue(respA);
    const { result, rerender } = setup({ req: reqA });
    await flush();
    rerender({ req: null });
    expect(result.current.status).toBe("idle");
  });
});
