// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useChipPopover } from "./useChipPopover";

function Harness() {
  const { open, setOpen, hostRef, panelRef, triggerRef, panelId } = useChipPopover();
  return (
    <>
      <div ref={hostRef}>
        <button ref={triggerRef} type="button" onClick={() => setOpen((v) => !v)} aria-controls={open ? panelId : undefined}>
          Chip
        </button>
        {open && <div id={panelId} ref={panelRef} role="dialog" aria-label="Chip panel" tabIndex={-1}>Panel</div>}
      </div>
      <button type="button">Other control</button>
      <div data-testid="empty-page">Empty page</div>
    </>
  );
}

let nextFrame = 0;
let frames: Map<number, FrameRequestCallback>;
let cancelFrame: ReturnType<typeof vi.fn>;

function flushFrame() {
  for (const [id, callback] of frames) {
    frames.delete(id);
    act(() => callback(0));
  }
}

beforeEach(() => {
  nextFrame = 0;
  frames = new Map();
  vi.stubGlobal("requestAnimationFrame", vi.fn((callback: FrameRequestCallback) => {
    const id = ++nextFrame;
    frames.set(id, callback);
    return id;
  }));
  cancelFrame = vi.fn((id: number) => frames.delete(id));
  vi.stubGlobal("cancelAnimationFrame", cancelFrame);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("useChipPopover", () => {
  it("focuses the panel on open; Escape closes it and returns focus synchronously", () => {
    render(<Harness />);
    const chip = screen.getByRole("button", { name: "Chip" });
    fireEvent.click(chip);
    const panel = screen.getByRole("dialog", { name: "Chip panel" });
    expect(document.activeElement).toBe(panel);
    expect(chip.getAttribute("aria-controls")).toBe(panel.id);

    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Chip panel" })).toBeNull();
    expect(document.activeElement).toBe(chip);
    expect(chip.hasAttribute("aria-controls")).toBe(false);
  });

  it("keeps focus on another clicked control after an outside mousedown", () => {
    render(<Harness />);
    const chip = screen.getByRole("button", { name: "Chip" });
    fireEvent.click(chip);
    const other = screen.getByRole("button", { name: "Other control" });
    fireEvent.mouseDown(other);
    other.focus(); // jsdom does not perform the browser's native mousedown focus move.
    expect(screen.queryByRole("dialog", { name: "Chip panel" })).toBeNull();
    flushFrame();
    expect(document.activeElement).toBe(other);
  });

  it("returns focus to the chip when an outside mousedown leaves focus on body", () => {
    render(<Harness />);
    const chip = screen.getByRole("button", { name: "Chip" });
    fireEvent.click(chip);
    fireEvent.mouseDown(screen.getByTestId("empty-page"));
    expect(document.activeElement).toBe(document.body);
    flushFrame();
    expect(document.activeElement).toBe(chip);
  });

  it("cancels a pending click-away focus frame on unmount", () => {
    const view = render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: "Chip" }));
    fireEvent.mouseDown(screen.getByTestId("empty-page"));
    expect(frames.size).toBe(1);
    view.unmount();
    expect(cancelFrame).toHaveBeenCalledWith(1);
    expect(frames.size).toBe(0);
  });
});
