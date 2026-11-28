import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ForgeTokenBanner } from "./ForgeTokenBanner";
import { api } from "../lib/api";
import { forgeApi } from "../mocks/mockApi/forge";

vi.mock("../mocks/mockApi/shared", async original => ({
  ...await original<typeof import("../mocks/mockApi/shared")>(),
  delay: <T,>(value: T) => Promise.resolve(value),
  mockScenario: () => "forge-token-expiring",
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

it("shows the forge-token-expiring banner from the mock forge connection", async () => {
  await page.viewport(1100, 900);
  render(
    <MemoryRouter>
      <div style={{ padding: 24 }}><ForgeTokenBanner userId="browser-owner" /></div>
    </MemoryRouter>,
  );
  const banner = await screen.findByRole("status", { name: "Forge token expiring" });
  expect((banner as HTMLElement).offsetHeight).toBeGreaterThan(0);
  expect(banner.textContent).toContain("token expires on");
  // The date comes from the scenario's own expires_at, formatted as the banner formats it.
  const { connections } = await forgeApi.listConnectionsBackground();
  const iso = connections.find((c) => c.id === "conn-gh")?.privilege_report?.token.expires_at;
  expect(iso).toBeTruthy();
  const date = new Date(Date.parse(iso as string)).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
  expect(date.length).toBeGreaterThan(0);
  expect(banner.textContent).toContain(`expires on ${date}.`);
  expect(banner.textContent).toContain("Update it in Settings → Forge before then.");
  await userEvent.click(within(banner).getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(screen.queryByRole("status", { name: "Forge token expiring" })).toBeNull());
});
