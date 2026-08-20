// @vitest-environment jsdom
//
// ChangelogDrawer (PRD #415 M2): the left slide-in drawer built on the shared Modal
// shell. These pin the M2 surface — a version heading per release, the four Modal
// a11y behaviours (Escape/backdrop/✕ close, focus restore), and an independently
// scrolling body — without depending on the real bundled CHANGELOG.md content, so
// the assertions stay deterministic as the changelog grows. `releases` is mocked to
// a fixed set (which also keeps the build-time import.meta.glob out of this test).
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

vi.mock("../lib/changelog", () => ({
  releases: [
    {
      version: "Unreleased",
      date: null,
      body: "",
      groups: [],
      unreleased: true,
      notReleased: false,
    },
    {
      version: "0.48.0",
      date: "2026-08-20",
      body: "Added a thing.",
      groups: [],
      unreleased: false,
      notReleased: false,
    },
    {
      version: "0.47.0",
      date: "2026-08-10",
      body: "Fixed a thing.",
      groups: [],
      unreleased: false,
      notReleased: false,
    },
  ],
}));

import { ChangelogDrawer } from "./ChangelogDrawer";

afterEach(cleanup);

describe("ChangelogDrawer", () => {
  it("renders a version heading for each release — v-prefixed numbers, the raw token for Unreleased", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);
    expect(screen.getByText("v0.48.0")).toBeTruthy();
    expect(screen.getByText("v0.47.0")).toBeTruthy();
    // Unreleased keeps its raw token (displayVersion only v-prefixes numeric versions).
    expect(screen.getByText("Unreleased")).toBeTruthy();
  });

  it("closes on Escape", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer onClose={onClose} />);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes on a backdrop click", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer onClose={onClose} />);
    // The dialog element IS the backdrop container; a mousedown whose target is it
    // (not a child) closes.
    fireEvent.mouseDown(screen.getByRole("dialog"));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes when the ✕ button is activated", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer onClose={onClose} />);
    fireEvent.click(screen.getByRole("button", { name: "Close changelog" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("moves focus into the dialog on open and restores it to the trigger on close", () => {
    const trigger = document.createElement("button");
    document.body.appendChild(trigger);
    trigger.focus();
    expect(document.activeElement).toBe(trigger);

    const { unmount } = render(<ChangelogDrawer onClose={vi.fn()} />);
    // Modal focuses its container (role="dialog", tabIndex={-1}) on open, so the
    // trap starts inside the drawer rather than on the page behind it.
    expect(document.activeElement).toBe(screen.getByRole("dialog"));

    unmount();
    expect(document.activeElement).toBe(trigger);
    trigger.remove();
  });

  it("gives the body its own scroll container so a long changelog scrolls independently", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);
    const body = screen.getByTestId("changelog-body");
    expect(body.className).toContain("overflow-y-auto");
    // The version headings live inside that scroll container, not the pinned header.
    expect(body.textContent).toContain("v0.48.0");
  });

  it("carries the running version on a data attribute for M3 to mark", () => {
    render(<ChangelogDrawer runningVersion="0.48.0" onClose={vi.fn()} />);
    // The panel is the dialog's child; the attribute rides it without rendering any
    // current-version UI yet (that is M3).
    expect(screen.getByRole("dialog").querySelector('[data-running-version="0.48.0"]')).not.toBeNull();
  });
});
