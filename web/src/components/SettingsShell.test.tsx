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
});
