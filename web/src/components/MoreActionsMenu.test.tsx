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
  render(
    <div>
      <MoreActionsMenu label="More actions for Job on repo" items={items} />
      <button type="button">outside</button>
    </div>,
  );
  const button = screen.getByRole("button", { name: "More actions for Job on repo" });
  return { button, onSelect };
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
});
