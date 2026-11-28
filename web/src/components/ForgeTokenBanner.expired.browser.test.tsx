import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ForgeTokenBanner } from "./ForgeTokenBanner";
import { api } from "../lib/api";
import { forgeApi } from "../mocks/mockApi/forge";

vi.mock("../mocks/mockApi/shared", async original => ({
  ...await original<typeof import("../mocks/mockApi/shared")>(),
  delay: <T,>(value: T) => Promise.resolve(value),
  mockScenario: () => "forge-token-expired",
}));

vi.mock("../lib/api", async original => ({
  ...await original<typeof import("../lib/api")>(),
  api: { listConnectionsBackground: vi.fn() },
}));

beforeEach(() => {
  window.sessionStorage.clear();
  vi.mocked(api.listConnectionsBackground).mockImplementation(forgeApi.listConnectionsBackground);
});
afterEach(() => { cleanup(); window.sessionStorage.clear(); vi.clearAllMocks(); });

it("shows the forge-token-expired banner from the mock forge connection", async () => {
  await page.viewport(1100, 900);
  render(
    <MemoryRouter>
      <div style={{ padding: 24 }}><ForgeTokenBanner userId="browser-owner" /></div>
    </MemoryRouter>,
  );
  const banner = await screen.findByRole("alert", { name: "Forge token expired" });
  expect((banner as HTMLElement).offsetHeight).toBeGreaterThan(0);
  expect(banner.textContent).toContain("has expired or is inactive");
  expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();
});
