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

// NavLink flags the active tab with aria-current="page"; the "Account & token"
// tab uses end so it does not stay lit on the nested /settings/* routes.
function current(name: string): string | null {
  return screen.getByRole("link", { name }).getAttribute("aria-current");
}

describe("SettingsShell tabs", () => {
  it("lights Account & token on /settings only (end match)", () => {
    renderAt("/settings");
    expect(current("Account & token")).toBe("page");
    expect(current("Forge")).toBeNull();
    expect(current("Workers")).toBeNull();
  });

  it("lights Forge on /settings/forge without keeping Account & token active", () => {
    renderAt("/settings/forge");
    expect(current("Forge")).toBe("page");
    expect(current("Account & token")).toBeNull();
    expect(current("Workers")).toBeNull();
  });

  it("lights Workers on /settings/workers", () => {
    renderAt("/settings/workers");
    expect(current("Workers")).toBe("page");
    expect(current("Forge")).toBeNull();
    expect(current("Account & token")).toBeNull();
  });

  // Issue #204: at 390px the full tab strip overflows (scrollWidth 401 vs clientWidth 390)
  // and used to scroll the whole page body sideways. jsdom has no layout engine, so this
  // asserts the class contract that fixes it rather than a measured width: the tab ROW
  // scrolls within itself (overflow-x-auto) and the tabs keep their size (shrink-0 /
  // whitespace-nowrap) so they overflow-and-scroll instead of compressing to fit.
  it("lets the tab row scroll within itself so the page body never scrolls sideways (#204)", () => {
    renderAt("/settings/workers");
    const row = screen.getByRole("link", { name: "Workers" }).parentElement as HTMLElement;
    expect(row.className).toContain("overflow-x-auto");
    // The underline still spans the row.
    expect(row.className).toMatch(/\bborder-b\b/);
    for (const name of ["Account & token", "Forge", "Workers", "Access", "Memory"]) {
      const tab = screen.getByRole("link", { name });
      expect(tab.className).toMatch(/\bshrink-0\b/);
      expect(tab.className).toContain("whitespace-nowrap");
    }
  });
});
