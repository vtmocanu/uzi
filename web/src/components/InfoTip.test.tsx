// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { InfoTip } from "./InfoTip";

afterEach(cleanup);

const tipOf = (trigger: HTMLElement) => document.getElementById(trigger.getAttribute("aria-describedby")!)!;
const isOpen = (trigger: HTMLElement) => tipOf(trigger).getAttribute("data-open") === "true";

describe("InfoTip", () => {
  it("names the icon by its label and describes it by the tip, which stays mounted while closed", () => {
    render(<InfoTip label="About usage figures" tip="Runs count primary work." />);
    const trigger = screen.getByRole("button", { name: "About usage figures" });
    const tip = tipOf(trigger);
    expect(tip.getAttribute("role")).toBe("tooltip");
    expect(tip.textContent).toBe("Runs count primary work.");
    expect(isOpen(trigger)).toBe(false);
    expect(trigger.hasAttribute("title")).toBe(false);
  });

  it("uses the children as the trigger's name when given", () => {
    render(<InfoTip tip="10 failed of 100 finished runs">10.0%</InfoTip>);
    const trigger = screen.getByRole("button", { name: "10.0%" });
    expect(tipOf(trigger).textContent).toBe("10 failed of 100 finished runs");
  });

  it("opens on focus and closes when focus leaves the host", () => {
    render(<><InfoTip label="About" tip="text" /><button type="button">next</button></>);
    const trigger = screen.getByRole("button", { name: "About" });
    fireEvent.focus(trigger);
    expect(isOpen(trigger)).toBe(true);
    fireEvent.blur(trigger, { relatedTarget: screen.getByRole("button", { name: "next" }) });
    expect(isOpen(trigger)).toBe(false);
  });

  it("closes on Escape when opened by focus", () => {
    render(<InfoTip label="About" tip="text" />);
    const trigger = screen.getByRole("button", { name: "About" });
    fireEvent.focus(trigger);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(isOpen(trigger)).toBe(false);
  });

  // WCAG 1.4.13: a hover-opened tip is dismissible without moving the pointer or focus.
  it("closes on Escape when opened by hover alone, without focus", () => {
    render(<InfoTip label="About" tip="text" />);
    const trigger = screen.getByRole("button", { name: "About" });
    fireEvent.mouseEnter(trigger.parentElement!);
    expect(isOpen(trigger)).toBe(true);
    expect(document.activeElement).not.toBe(trigger);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(isOpen(trigger)).toBe(false);
  });

  // The panel sits a few pixels from its trigger: leaving the trigger waits briefly, so the pointer
  // can cross the gap; reaching the panel (inside the host) cancels the close.
  it("stays open while the pointer crosses the gap onto the tip, and closes after leaving", () => {
    vi.useFakeTimers();
    try {
      render(<InfoTip label="About" tip="text" />);
      const trigger = screen.getByRole("button", { name: "About" });
      const host = trigger.parentElement!;
      expect(host.contains(tipOf(trigger))).toBe(true);
      fireEvent.mouseEnter(host);
      fireEvent.mouseLeave(host);
      act(() => vi.advanceTimersByTime(60));
      fireEvent.mouseEnter(host); // the pointer reached the panel
      act(() => vi.advanceTimersByTime(500));
      expect(isOpen(trigger)).toBe(true);
      fireEvent.mouseLeave(host);
      act(() => vi.advanceTimersByTime(500));
      expect(isOpen(trigger)).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  // WCAG 1.4.13 persistence: each input holds the tip open on its own.
  it("stays open after the pointer leaves while the trigger keeps focus", () => {
    vi.useFakeTimers();
    try {
      render(<InfoTip label="About" tip="text" />);
      const trigger = screen.getByRole("button", { name: "About" });
      const host = trigger.parentElement!;
      fireEvent.mouseEnter(host);
      fireEvent.focus(trigger);
      fireEvent.mouseLeave(host);
      act(() => vi.advanceTimersByTime(500));
      expect(isOpen(trigger)).toBe(true);
      fireEvent.blur(trigger, { relatedTarget: null });
      expect(isOpen(trigger)).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("stays open after focus leaves while the pointer is still over it", () => {
    vi.useFakeTimers();
    try {
      render(<InfoTip label="About" tip="text" />);
      const trigger = screen.getByRole("button", { name: "About" });
      const host = trigger.parentElement!;
      fireEvent.focus(trigger);
      fireEvent.mouseEnter(host);
      fireEvent.blur(trigger, { relatedTarget: null });
      expect(isOpen(trigger)).toBe(true);
      fireEvent.mouseLeave(host);
      act(() => vi.advanceTimersByTime(500));
      expect(isOpen(trigger)).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps a clicked tip open after the pointer leaves, until a press outside", () => {
    vi.useFakeTimers();
    try {
      render(<><InfoTip label="About" tip="text" /><p>elsewhere</p></>);
      const trigger = screen.getByRole("button", { name: "About" });
      const host = trigger.parentElement!;
      fireEvent.mouseEnter(host);
      fireEvent.click(trigger);
      fireEvent.mouseLeave(host);
      act(() => vi.advanceTimersByTime(500));
      expect(isOpen(trigger)).toBe(true);
      fireEvent.pointerDown(screen.getByText("elsewhere"));
      expect(isOpen(trigger)).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("stays dismissed after Escape until the next hover, focus or click", () => {
    render(<InfoTip label="About" tip="text" />);
    const trigger = screen.getByRole("button", { name: "About" });
    fireEvent.mouseEnter(trigger.parentElement!);
    fireEvent.focus(trigger);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(isOpen(trigger)).toBe(false);
    fireEvent.click(trigger);
    expect(isOpen(trigger)).toBe(true);
  });

  it("opens on click (a tap) and a second click does not close it", () => {
    render(<InfoTip label="About" tip="text" />);
    const trigger = screen.getByRole("button", { name: "About" });
    fireEvent.click(trigger);
    expect(isOpen(trigger)).toBe(true);
    fireEvent.click(trigger);
    expect(isOpen(trigger)).toBe(true);
  });
});
