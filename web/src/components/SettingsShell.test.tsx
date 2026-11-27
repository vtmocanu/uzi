// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsShell } from "./SettingsShell";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <SettingsShell description="d">
        <div>tab body</div>
      </SettingsShell>
    </MemoryRouter>,
  );
}

// NavLink flags the active tab with aria-current="page"; the "Account & tokens"
// tab uses end so it does not stay lit on the nested /settings/* routes.
function current(name: string): string | null {
  return screen.getByRole("link", { name }).getAttribute("aria-current");
}

describe("SettingsShell tabs", () => {
  it("lights Account & tokens on /settings only (end match)", () => {
    renderAt("/settings");
    expect(current("Account & tokens")).toBe("page");
    expect(current("Forge")).toBeNull();
  });

  it("lights Forge on /settings/forge without keeping Account & tokens active", () => {
    renderAt("/settings/forge");
    expect(current("Forge")).toBe("page");
    expect(current("Account & tokens")).toBeNull();
  });

  // Workers moved to /workers (a Factory page); it must not resurface as a tab.
  // Paired with the positive assertions above so this cannot go vacuous: the
  // shell demonstrably renders tabs, and none of them is Workers.
  it("has no Workers tab any more", () => {
    renderAt("/settings");
    expect(screen.queryByRole("link", { name: "Workers" })).toBeNull();
  });

  // The Run defaults tab (split out of the overloaded Account & tokens tab) is a
  // real destination of its own.
  it("lights Run defaults on /settings/run-defaults", () => {
    renderAt("/settings/run-defaults");
    expect(current("Run defaults")).toBe("page");
    expect(current("Account & tokens")).toBeNull();
  });

  // The Appearance tab (theme/mode/typeface + demo mode, split out of Account &
  // tokens) is cosmetic, so it closes the strip (#4665; Models follows the credentials it
  // depends on), and lights on its own route without keeping the end-matched Account &
  // tokens active.
  it("puts Appearance last and lights it on /settings/appearance only", () => {
    renderAt("/settings/appearance");
    const tabs = screen.getAllByRole("link");
    expect(tabs.map((a) => a.textContent)).toEqual([
      "Account & tokens",
      "Models",
      "Run defaults",
      "Forge",
      "Issue trackers",
      "Access",
      "Memory",
      "Appearance",
    ]);
    expect(current("Appearance")).toBe("page");
    expect(current("Account & tokens")).toBeNull();
  });

  // Models sits right after Account & tokens (the credentials that decide which models a run can
  // use) and before Run defaults (which links back to it), and lights on its own route.
  it("puts Models between Account & tokens and Run defaults and lights it on /settings/models", () => {
    renderAt("/settings/models");
    const labels = screen.getAllByRole("link").map((a) => a.textContent);
    expect(labels.slice(0, 3)).toEqual(["Account & tokens", "Models", "Run defaults"]);
    expect(current("Models")).toBe("page");
    expect(current("Run defaults")).toBeNull();
    expect(current("Account & tokens")).toBeNull();
  });

  // Issue #204: at 390px a five-tab strip overflows (measured scrollWidth 401 vs
  // clientWidth 390 on the original five) and used to scroll the whole page body
  // sideways. jsdom has no layout engine, so this asserts the class contract that
  // fixes it rather than a measured width: the tab ROW scrolls within itself
  // (overflow-x-auto) and the tabs keep their size (shrink-0 / whitespace-nowrap)
  // so they overflow-and-scroll instead of compressing to fit.
  it("lets the tab row scroll within itself so the page body never scrolls sideways (#204)", () => {
    renderAt("/settings");
    const row = screen.getByRole("link", { name: "Forge" }).parentElement as HTMLElement;
    expect(row.className).toContain("overflow-x-auto");
    // The underline still spans the row, drawn as an inset line (#4419) so the row box
    // never overflows by a pixel and grows a vertical scrollbar.
    expect(row.className).toContain("shadow-[inset_0_-1px_0_rgb(var(--edge))]");
    for (const name of ["Account & tokens", "Appearance", "Models", "Run defaults", "Forge", "Access", "Memory"]) {
      const tab = screen.getByRole("link", { name });
      expect(tab.className).toMatch(/\bshrink-0\b/);
      expect(tab.className).toContain("whitespace-nowrap");
    }
  });

  // Seven tabs overflow a phone strip: at 390px an active later tab sat past the
  // right edge with scrollLeft 0. The shell reveals the active tab on mount and on every
  // switch by writing the ROW's scrollLeft only, never the window's. jsdom has no layout,
  // so geometry is stubbed: the row spans 0-358 and each tab is 110px wide in order.
  it("scrolls the active tab into view within the strip on mount and on a tab switch", () => {
    const rect = (left: number, right: number) =>
      ({ left, right, top: 0, bottom: 0, width: right - left, height: 0, x: left, y: 0, toJSON() {} }) as DOMRect;
    vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
      if (this.tagName === "A") {
        const i = Array.from(this.parentElement!.children).indexOf(this);
        return rect(i * 110, i * 110 + 110);
      }
      return rect(0, 358);
    });
    const writes: { el: Element; value: number }[] = [];
    vi.spyOn(Element.prototype, "scrollLeft", "set").mockImplementation(function (this: Element, value: number) {
      writes.push({ el: this, value });
    });
    const windowScroll = vi.spyOn(window, "scrollTo").mockImplementation(() => {});

    renderAt("/settings/forge");
    const row = screen.getByRole("link", { name: "Forge" }).parentElement as HTMLElement;
    // Forge is the fourth tab (330-440): its right edge is 82px past the row's.
    expect(writes).toEqual([{ el: row, value: 82 }]);

    writes.length = 0;
    fireEvent.click(screen.getByRole("link", { name: "Appearance" }));
    expect(current("Appearance")).toBe("page");
    // Appearance is the eighth tab (770-880): 522px past the row's right edge.
    expect(writes).toEqual([{ el: row, value: 522 }]);
    expect(windowScroll).not.toHaveBeenCalled();
  });

  it("leaves the strip alone when the active tab is already visible", () => {
    vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(
      () => ({ left: 0, right: 100, top: 0, bottom: 0, width: 100, height: 0, x: 0, y: 0, toJSON() {} }) as DOMRect,
    );
    const set = vi.spyOn(Element.prototype, "scrollLeft", "set").mockImplementation(() => {});
    renderAt("/settings");
    expect(current("Account & tokens")).toBe("page");
    expect(set).not.toHaveBeenCalled();
  });
});
