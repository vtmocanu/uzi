// @vitest-environment jsdom
//
// The app-wide maintenance banner (PRD #3454) through the real MaintenanceProvider: shown for
// every signed-in user while maintenance is on, with the reason as plain text; hidden when off,
// signed out, or when the read fails.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api, type MaintenanceState } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { MaintenanceProvider } from "../lib/useMaintenance";
import { MaintenanceBanner } from "./MaintenanceBanner";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getMaintenance: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const on = (reason: string | null): MaintenanceState => ({ active: true, since: "2026-10-25T12:00:00Z", reason });

async function renderBanner(user: { is_admin: boolean } | null) {
  vi.mocked(useAuth).mockReturnValue({ user } as never);
  const utils = render(
    <MemoryRouter>
      <MaintenanceProvider>
        <MaintenanceBanner />
      </MaintenanceProvider>
    </MemoryRouter>,
  );
  await act(async () => { await Promise.resolve(); });
  return utils;
}

const banner = () => screen.queryByRole("status", { name: "Maintenance mode" });

describe("MaintenanceBanner", () => {
  it("shows for a non-admin while active, with the reason as plain text and no admin link", async () => {
    mockApi.getMaintenance.mockResolvedValue(on("Node pool upgrade <b>now</b>‮\n"));
    await renderBanner({ is_admin: false });
    const b = banner();
    expect(b).toBeTruthy();
    expect(b!.textContent).toContain("uzi is in maintenance mode");
    expect(b!.textContent).toContain("New runs wait in the queue until it ends; running work finishes.");
    expect(b!.textContent).toContain("Reason: Node pool upgrade <b>now</b>");
    expect(b!.querySelector("b")).toBeNull();
    expect(b!.textContent).not.toMatch(/[\p{Cc}\p{Cf}]/u);
    expect(screen.queryByRole("link", { name: "Manage maintenance" })).toBeNull();
  });

  it("links an admin to the maintenance card", async () => {
    mockApi.getMaintenance.mockResolvedValue(on(null));
    await renderBanner({ is_admin: true });
    expect(banner()!.textContent).not.toContain("Reason:");
    expect(screen.getByRole("link", { name: "Manage maintenance" }).getAttribute("href")).toBe(
      "/admin/settings#maintenance",
    );
  });

  it("is hidden while maintenance is off", async () => {
    mockApi.getMaintenance.mockResolvedValue({ active: false, since: null, reason: null });
    await renderBanner({ is_admin: false });
    expect(mockApi.getMaintenance).toHaveBeenCalledTimes(1);
    expect(banner()).toBeNull();
  });

  it("fails quietly: no banner when the read errors", async () => {
    mockApi.getMaintenance.mockRejectedValue(new Error("offline"));
    await renderBanner({ is_admin: false });
    expect(banner()).toBeNull();
  });

  it("makes no request for a signed-out session", async () => {
    mockApi.getMaintenance.mockResolvedValue(on("x"));
    await renderBanner(null);
    expect(mockApi.getMaintenance).not.toHaveBeenCalled();
    expect(banner()).toBeNull();
  });
});
