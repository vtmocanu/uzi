// @vitest-environment jsdom
//
// TriageDisposedRow is the run page's disposed state, surviving the swap to the shared set. It
// must keep the coarse "resolved Xh ago" (from the app's one duration helper), the
// server-computed stale badge, and Undo. Time is pinned with fake timers so the elapsed string
// is exact rather than flaky at a minute boundary.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { TriageDisposedRow } from "./TriageDisposedRow";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("TriageDisposedRow", () => {
  it("renders 'resolved Xh Ym ago' from the resolve time", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T12:00:00Z"));
    // 90 minutes earlier.
    render(<TriageDisposedRow resolvedAt="2026-01-01T10:30:00Z" onUndo={vi.fn()} />);
    expect(screen.getByText("resolved 1h 30m ago")).toBeTruthy();
  });

  it("accepts a Date as well as an ISO string", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T12:00:00Z"));
    render(<TriageDisposedRow resolvedAt={new Date("2026-01-01T11:55:00Z")} onUndo={vi.fn()} />);
    expect(screen.getByText("resolved 5m ago")).toBeTruthy();
  });

  it("degrades to a bare 'resolved' on an unparseable timestamp", () => {
    render(<TriageDisposedRow resolvedAt="not-a-date" onUndo={vi.fn()} />);
    expect(screen.getByText("resolved")).toBeTruthy();
  });

  it("shows the stale badge only when stale", () => {
    const { rerender } = render(<TriageDisposedRow resolvedAt="2026-01-01T10:00:00Z" onUndo={vi.fn()} stale />);
    expect(screen.getByText("recommendation changed since you resolved")).toBeTruthy();
    rerender(<TriageDisposedRow resolvedAt="2026-01-01T10:00:00Z" onUndo={vi.fn()} stale={false} />);
    expect(screen.queryByText("recommendation changed since you resolved")).toBeNull();
  });

  it("fires onUndo on the Undo click, and disables it while busy", () => {
    const onUndo = vi.fn();
    const { rerender } = render(<TriageDisposedRow resolvedAt="2026-01-01T10:00:00Z" onUndo={onUndo} />);
    const undo = screen.getByRole("button", { name: "Undo" });
    fireEvent.click(undo);
    expect(onUndo).toHaveBeenCalledTimes(1);

    rerender(<TriageDisposedRow resolvedAt="2026-01-01T10:00:00Z" onUndo={onUndo} busy />);
    expect((screen.getByRole("button", { name: "Undo" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
