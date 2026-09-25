// @vitest-environment jsdom
//
// MoreActionsMenu (PRD #1645 D13): the APG menu-button keyboard contract. Focus is
// asserted by identity (toBe), never by text (.claude/rules/web.md).
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MoreActionsMenu, type MoreActionsItem } from "./MoreActionsMenu";

afterEach(cleanup);

function setup(over: Partial<MoreActionsItem>[] = []) {
  const onSelect = [vi.fn(), vi.fn(), vi.fn()];
  const items: MoreActionsItem[] = [
    { key: "a", label: "Alpha", onSelect: onSelect[0] },
    { key: "b", label: "Bravo", onSelect: onSelect[1] },
    { key: "c", label: "Charlie", onSelect: onSelect[2] },
  ].map((it, i) => ({ ...it, ...over[i] }));
  const { container } = render(
    <div>
      <MoreActionsMenu label="More actions for Job on repo" items={items} />
      <button type="button">outside</button>
    </div>,
  );
  const button = screen.getByRole("button", { name: "More actions for Job on repo" });
  return { button, onSelect, container };
}

const item = (name: string) => screen.getByRole("menuitem", { name });

describe("MoreActionsMenu", () => {
  it("is closed initially, with the menu-button attributes", () => {
    const { button } = setup();
    expect(button.getAttribute("aria-haspopup")).toBe("menu");
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it.each(["Enter", " ", "ArrowDown"])("%j on the button opens the menu with focus on the first item", (key) => {
    const { button } = setup();
    button.focus();
    fireEvent.keyDown(button, { key });
    expect(screen.getByRole("menu")).toBeTruthy();
    expect(button.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(item("Alpha"));
  });

  it("ArrowUp on the button opens on the last item", () => {
    const { button } = setup();
    fireEvent.keyDown(button, { key: "ArrowUp" });
    expect(document.activeElement).toBe(item("Charlie"));
  });

  it("a click opens it too, focusing the first item", () => {
    const { button } = setup();
    fireEvent.click(button);
    expect(document.activeElement).toBe(item("Alpha"));
  });

  it("ArrowDown/ArrowUp move between items and wrap at both ends", () => {
    const { button } = setup();
    fireEvent.keyDown(button, { key: "ArrowDown" });
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    expect(document.activeElement).toBe(item("Bravo"));
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    expect(document.activeElement).toBe(item("Charlie"));
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    expect(document.activeElement).toBe(item("Alpha")); // wrapped forward
    fireEvent.keyDown(menu, { key: "ArrowUp" });
    expect(document.activeElement).toBe(item("Charlie")); // wrapped backward
  });

  it("Home and End jump to the first and last item", () => {
    const { button } = setup();
    fireEvent.keyDown(button, { key: "ArrowDown" });
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "End" });
    expect(document.activeElement).toBe(item("Charlie"));
    fireEvent.keyDown(menu, { key: "Home" });
    expect(document.activeElement).toBe(item("Alpha"));
  });

  it("Escape closes the menu and returns focus to the button", () => {
    const { button } = setup();
    fireEvent.keyDown(button, { key: "ArrowDown" });
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    expect(screen.queryByRole("menu")).toBeNull();
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(button);
  });

  it("Enter on an item activates it, closes the menu and returns focus to the button", () => {
    const { button, onSelect } = setup();
    fireEvent.keyDown(button, { key: "ArrowDown" });
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    fireEvent.keyDown(menu, { key: "Enter" });
    expect(onSelect[1]).toHaveBeenCalledTimes(1);
    expect(onSelect[0]).not.toHaveBeenCalled();
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it("a disabled item is focusable but inert, and carries its reason as a description", () => {
    const { button, onSelect } = setup([{}, { disabled: true, description: "Not for this one" }]);
    fireEvent.keyDown(button, { key: "ArrowDown" });
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    const bravo = item("Bravo");
    expect(document.activeElement).toBe(bravo);
    expect(bravo.getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(bravo.getAttribute("aria-describedby")!)?.textContent).toBe("Not for this one");
    fireEvent.keyDown(menu, { key: "Enter" });
    fireEvent.click(bravo);
    expect(onSelect[1]).not.toHaveBeenCalled();
    expect(screen.getByRole("menu")).toBeTruthy();
  });

  it("a pointerdown outside closes the menu", () => {
    const { button } = setup();
    fireEvent.click(button);
    expect(screen.getByRole("menu")).toBeTruthy();
    fireEvent.pointerDown(screen.getByRole("button", { name: "outside" }));
    expect(screen.queryByRole("menu")).toBeNull();
  });

  // B1: a browser refuses focus on a visibility:hidden element, and jsdom does not, so pin
  // the order directly: every focus() the menu makes on an item must happen while the
  // menu is already visible (placed), on the first open and on a re-open alike.
  it("moves focus into the menu only once it is visible, on the first open and a re-open", () => {
    const seen: string[] = [];
    const real = HTMLElement.prototype.focus;
    const spy = vi.spyOn(HTMLElement.prototype, "focus").mockImplementation(function (this: HTMLElement, opts) {
      if (this.getAttribute("role") === "menuitem") {
        seen.push((this.closest('[role="menu"]') as HTMLElement).style.visibility);
      }
      real.call(this, opts);
    });
    try {
      const { button } = setup();
      fireEvent.keyDown(button, { key: "ArrowDown" });
      expect(document.activeElement).toBe(item("Alpha"));
      fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
      fireEvent.keyDown(button, { key: "ArrowUp" });
      expect(document.activeElement).toBe(item("Charlie"));
      expect(seen.length).toBeGreaterThanOrEqual(2);
      expect(seen.every((v) => v === "visible")).toBe(true);
    } finally {
      spy.mockRestore();
    }
  });

  // B2: the menu escapes its row (a paused row's opacity is a stacking context), so it
  // renders in document.body, and a press inside it still counts as inside.
  it("renders the menu in document.body, outside the owner's subtree, wired to its button", () => {
    const { button, container } = setup();
    fireEvent.click(button);
    const menu = screen.getByRole("menu");
    expect(container.contains(menu)).toBe(false);
    expect(menu.parentElement).toBe(document.body);
    expect(button.getAttribute("aria-controls")).toBe(menu.id);
    expect(menu.getAttribute("aria-labelledby")).toBe(button.id);
    fireEvent.pointerDown(item("Bravo"));
    expect(screen.getByRole("menu")).toBe(menu);
  });

  it("Tab closes the menu and puts focus back on the button, so the tab sequence resumes from it", () => {
    const { button } = setup();
    fireEvent.keyDown(button, { key: "ArrowDown" });
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Tab" });
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it("anchors its right edge from the layout viewport's clientWidth (scrollbar excluded)", () => {
    const html = document.documentElement;
    const had = Object.getOwnPropertyDescriptor(html, "clientWidth");
    Object.defineProperty(html, "clientWidth", { configurable: true, value: 1000 });
    try {
      const { button } = setup();
      vi.spyOn(button, "getBoundingClientRect").mockReturnValue({
        top: 100, bottom: 128, left: 900, right: 940, width: 40, height: 28, x: 900, y: 100, toJSON: () => ({}),
      });
      fireEvent.click(button);
      const menu = screen.getByRole("menu");
      expect(menu.style.right).toBe("60px");
      expect(menu.style.top).toBe("132px");
    } finally {
      if (had) Object.defineProperty(html, "clientWidth", had);
      else delete (html as unknown as { clientWidth?: number }).clientWidth;
    }
  });

  it("keeps its left edge on screen when the button sits near the left of a narrow viewport", () => {
    const html = document.documentElement;
    const had = Object.getOwnPropertyDescriptor(html, "clientWidth");
    Object.defineProperty(html, "clientWidth", { configurable: true, value: 390 });
    const width = vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(256);
    try {
      const { button } = setup();
      vi.spyOn(button, "getBoundingClientRect").mockReturnValue({
        top: 100, bottom: 128, left: 180, right: 215, width: 35, height: 28, x: 180, y: 100, toJSON: () => ({}),
      });
      fireEvent.click(button);
      const right = parseFloat(screen.getByRole("menu").style.right);
      // Unclamped, right = 390 - 215 = 175 and the left edge would sit at 390 - 175 - 256 = -41.
      const left = 390 - right - 256;
      expect(left).toBeGreaterThanOrEqual(4);
      expect(right).toBeGreaterThanOrEqual(4);
    } finally {
      width.mockRestore();
      if (had) Object.defineProperty(html, "clientWidth", had);
      else delete (html as unknown as { clientWidth?: number }).clientWidth;
    }
  });
});
