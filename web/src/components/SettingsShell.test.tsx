// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsShell } from "./SettingsShell";

afterEach(cleanup);

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
  // tokens) is the SECOND tab, immediately after Account & tokens, and lights on
  // its own route without keeping the end-matched Account & tokens active.
  it("puts Appearance second and lights it on /settings/appearance only", () => {
    renderAt("/settings/appearance");
    const tabs = screen.getAllByRole("link");
    expect(tabs[1].textContent).toBe("Appearance");
    expect(current("Appearance")).toBe("page");
    expect(current("Account & tokens")).toBeNull();
  });

  // PRD #3263: Models sits between Appearance and Run defaults and lights on its own route.
  it("puts Models between Appearance and Run defaults and lights it on /settings/models", () => {
    renderAt("/settings/models");
    const labels = screen.getAllByRole("link").map((a) => a.textContent);
    expect(labels.slice(1, 4)).toEqual(["Appearance", "Models", "Run defaults"]);
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
});
