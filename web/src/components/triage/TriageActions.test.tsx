// @vitest-environment jsdom
//
// TriageActions is the shared action row. These cover the contract the wiring depends on: an
// absent handler hides its button, the Dismiss ▾ menu is a real menu that closes on Escape
// (refocusing its trigger) and on an outside click, the reasoned items carry the surface's
// subline copy, and the sr-only live region emits `announce` for the owned mode.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { TriageActions } from "./TriageActions";

afterEach(cleanup);

describe("TriageActions — which buttons render", () => {
  it("renders File issue · Mark done · Dismiss ▾ in that order when all handlers are given", () => {
    render(<TriageActions onFile={vi.fn()} onMarkDone={vi.fn()} onDismiss={vi.fn()} />);
    const buttons = screen.getAllByRole("button").map((b) => b.textContent);
    expect(buttons).toEqual(["File issue", "Mark done", "Dismiss ▾"]);
  });

  it("hides File issue when onFile is absent", () => {
    render(<TriageActions onMarkDone={vi.fn()} onDismiss={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "File issue" })).toBeNull();
  });

  it("hides Mark done when onMarkDone is absent (the Findings row)", () => {
    render(<TriageActions onFile={vi.fn()} onDismiss={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Mark done" })).toBeNull();
    expect(screen.getByRole("button", { name: "File issue" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dismiss ▾" })).toBeTruthy();
  });

  it("hides Dismiss ▾ when onDismiss is absent", () => {
    render(<TriageActions onFile={vi.fn()} onMarkDone={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Dismiss ▾" })).toBeNull();
  });

  it("fires the plain handlers on click", () => {
    const onFile = vi.fn();
    const onMarkDone = vi.fn();
    render(<TriageActions onFile={onFile} onMarkDone={onMarkDone} onDismiss={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "File issue" }));
    fireEvent.click(screen.getByRole("button", { name: "Mark done" }));
    expect(onFile).toHaveBeenCalledTimes(1);
    expect(onMarkDone).toHaveBeenCalledTimes(1);
  });

  it("disables every button while busy", () => {
    render(<TriageActions onFile={vi.fn()} onMarkDone={vi.fn()} onDismiss={vi.fn()} busy />);
    for (const b of screen.getAllByRole("button")) expect((b as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("TriageActions — the Dismiss ▾ menu", () => {
  function openMenu(dismissCopy: "judge" | "finding", onDismiss = vi.fn()) {
    render(<TriageActions onFile={vi.fn()} onDismiss={onDismiss} dismissCopy={dismissCopy} />);
    const trigger = screen.getByRole("button", { name: "Dismiss ▾" });
    expect(trigger.getAttribute("aria-haspopup")).toBe("menu");
    fireEvent.click(trigger);
    return { trigger, menu: screen.getByRole("menu"), onDismiss };
  }

  it("shows Won't do / Not an issue with the JUDGE subline copy", () => {
    const { menu } = openMenu("judge");
    expect(within(menu).getByText("Won't do")).toBeTruthy();
    expect(within(menu).getByText("Valid, but not worth acting on")).toBeTruthy();
    expect(within(menu).getByText("Not an issue")).toBeTruthy();
    expect(within(menu).getByText("False positive, the judge got it wrong")).toBeTruthy();
  });

  it("shows the FINDING subline copy for not_an_issue", () => {
    const { menu } = openMenu("finding");
    expect(within(menu).getByText("False positive, the worker got it wrong")).toBeTruthy();
    // The other subline is shared across surfaces.
    expect(within(menu).getByText("Valid, but not worth acting on")).toBeTruthy();
  });

  it("calls onDismiss with the chosen reason and closes the menu", () => {
    const { menu, onDismiss } = openMenu("judge");
    fireEvent.click(within(menu).getByText("Not an issue"));
    expect(onDismiss).toHaveBeenCalledWith("not_an_issue");
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("closes on Escape and returns focus to the Dismiss trigger", () => {
    const { trigger } = openMenu("judge");
    expect(screen.getByRole("menu")).toBeTruthy();
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(screen.queryByRole("menu")).toBeNull();
    // Focus by IDENTITY, not text (a body-level textContent match is vacuous).
    expect(document.activeElement).toBe(trigger);
  });

  it("closes on a pointerdown outside the menu wrapper", () => {
    openMenu("judge");
    expect(screen.getByRole("menu")).toBeTruthy();
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole("menu")).toBeNull();
  });
});

describe("TriageActions — the live region (owned mode)", () => {
  it("emits announce in a polite sr-only status region", () => {
    render(<TriageActions onMarkDone={vi.fn()} announce="Marked done" />);
    const region = screen.getByText("Marked done");
    expect(region.getAttribute("role")).toBe("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(region.className).toContain("sr-only");
  });

  it("renders NO internal region when the caller hosts its own (owned mode)", () => {
    // The owned caller (run page) hosts a persistent region a level up because its row swaps
    // TriageActions out on a mutation; passing callerHostsLiveRegion suppresses the doomed
    // internal one so the two do not double-announce.
    const { container } = render(
      <TriageActions onMarkDone={vi.fn()} announce="Marked done" callerHostsLiveRegion />,
    );
    expect(container.querySelector('[role="status"]')).toBeNull();
    expect(screen.queryByText("Marked done")).toBeNull();
  });
});
