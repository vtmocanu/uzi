// @vitest-environment jsdom
//
// Modal (PRD #66 M9 follow-up): the shared accessible dialog shell. These pin the
// a11y behaviours the two M9 override modals rely on — focus moves into the dialog
// on open, Escape closes, focus is restored to the trigger on close, and the
// backdrop closes only when allowed. Mirrors ScheduleModal's Escape test.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Modal } from "./Modal";

afterEach(() => {
  cleanup();
});

describe("Modal", () => {
  it("closes on Escape (same path as an explicit Cancel)", () => {
    const onClose = vi.fn();
    render(
      <Modal label="Test dialog" onClose={onClose}>
        <div>panel</div>
      </Modal>,
    );
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("moves focus into the dialog on open and restores it to the trigger on close", () => {
    const trigger = document.createElement("button");
    document.body.appendChild(trigger);
    trigger.focus();
    expect(document.activeElement).toBe(trigger);

    const { unmount } = render(
      <Modal label="Test dialog" onClose={vi.fn()}>
        <div>
          <button>Inside</button>
        </div>
      </Modal>,
    );

    // On open, focus lands on the dialog container (tabIndex={-1}), so Tab starts
    // inside and a screen reader is on the dialog rather than the page behind it.
    expect(document.activeElement).toBe(screen.getByRole("dialog"));

    unmount();
    // On close, focus returns to whatever was focused when the dialog opened.
    expect(document.activeElement).toBe(trigger);
    trigger.remove();
  });

  it("closes on a backdrop click, but not while a submit is in flight", () => {
    const onClose = vi.fn();
    const { rerender } = render(
      <Modal label="Test dialog" onClose={onClose}>
        <div>panel</div>
      </Modal>,
    );
    // A mousedown on the backdrop itself (target === currentTarget) closes.
    fireEvent.mouseDown(screen.getByRole("dialog"));
    expect(onClose).toHaveBeenCalledTimes(1);

    onClose.mockClear();
    rerender(
      <Modal label="Test dialog" onClose={onClose} closeOnBackdrop={false}>
        <div>panel</div>
      </Modal>,
    );
    fireEvent.mouseDown(screen.getByRole("dialog"));
    expect(onClose).not.toHaveBeenCalled();
  });
});
