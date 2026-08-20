// @vitest-environment jsdom
//
// ChangelogDrawer (PRD #415 M2): the left slide-in release-notes panel built on
// the shared Modal shell. These pin the drawer's own contract — mount only while
// open, the three close paths (Esc, backdrop, the ✕ button) each reaching
// onClose, focus restored to the trigger on close, an independently-scrollable
// body, the version headings rendered from `releases`, and the empty-state
// fallback. The Modal's a11y mechanics themselves are covered by Modal.test.tsx.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { ChangelogDrawer } from "./ChangelogDrawer";
import type { Release } from "../lib/changelog";

// `releases` is bundled from CHANGELOG.md at build time, so mock the module to
// drive both a populated and an empty list deterministically. A live getter lets
// each test swap the array before rendering.
const { releasesRef } = vi.hoisted(() => ({ releasesRef: { current: [] as Release[] } }));
vi.mock("../lib/changelog", () => ({
  get releases() {
    return releasesRef.current;
  },
}));

function makeRelease(version: string, date: string | null): Release {
  return { version, date, body: "", groups: [], released: true };
}

const SAMPLE: Release[] = [
  makeRelease("0.48.0", "2026-08-19"),
  makeRelease("0.47.0", "2026-08-01"),
];

beforeEach(() => {
  releasesRef.current = SAMPLE;
});

afterEach(cleanup);

describe("ChangelogDrawer", () => {
  it("renders nothing when closed and the dialog when open", () => {
    const { rerender } = render(<ChangelogDrawer open={false} onClose={vi.fn()} />);
    expect(screen.queryByRole("dialog")).toBeNull();

    rerender(<ChangelogDrawer open onClose={vi.fn()} />);
    expect(screen.getByRole("dialog", { name: "Release notes" })).toBeTruthy();
  });

  it("closes on Escape", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer open onClose={onClose} />);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes on a backdrop click", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer open onClose={onClose} />);
    // A mousedown on the backdrop itself (target === currentTarget) closes.
    fireEvent.mouseDown(screen.getByRole("dialog"));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes on the ✕ button", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer open onClose={onClose} />);
    fireEvent.click(screen.getByRole("button", { name: "Close release notes" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("restores focus to the trigger after close (Modal restore)", () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            open changelog
          </button>
          <ChangelogDrawer open={open} onClose={() => setOpen(false)} />
        </>
      );
    }
    render(<Harness />);
    const trigger = screen.getByRole("button", { name: "open changelog" });
    trigger.focus();
    fireEvent.click(trigger);
    // Modal moves focus into the dialog on open.
    expect(document.activeElement).toBe(screen.getByRole("dialog"));

    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    // On close the drawer unmounts the Modal, which restores focus to the trigger.
    expect(document.activeElement).toBe(trigger);
  });

  it("gives the body its own scroll container and lists the releases", () => {
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    const body = screen.getByTestId("changelog-body");
    // Independent body scroll, per the spec — a positive assertion on the class
    // that provides it.
    expect(body.className).toContain("overflow-y-auto");
    // The list is present rather than the empty-state paragraph.
    expect(screen.getByRole("list")).toBeTruthy();
    expect(screen.queryByText("No release notes yet.")).toBeNull();
  });

  it("renders a version heading and date per release", () => {
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    // displayVersion prefixes numeric versions with v.
    expect(screen.getByText("v0.48.0")).toBeTruthy();
    expect(screen.getByText("v0.47.0")).toBeTruthy();
    expect(screen.getByText("2026-08-19")).toBeTruthy();
    expect(screen.getByText("2026-08-01")).toBeTruthy();
  });

  it("flags the running version as current, and only that one", () => {
    render(<ChangelogDrawer open onClose={vi.fn()} version="0.48.0" />);
    // Positive: the matching release is marked current…
    const current = screen.getByText("current");
    expect(current).toBeTruthy();
    // …and negative: there is exactly one such marker, not one per release.
    expect(screen.getAllByText("current")).toHaveLength(1);
  });

  it("renders a graceful message when there are no releases", () => {
    releasesRef.current = [];
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    expect(screen.getByText("No release notes yet.")).toBeTruthy();
    // Negative pair: no list is rendered in the empty state.
    expect(screen.queryByRole("list")).toBeNull();
  });
});
