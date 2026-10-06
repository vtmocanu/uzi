// @vitest-environment jsdom
//
// The HealthStatusProvider fetch gate (PRD #1484 M5). The tested invariant: a NON-admin
// session makes ZERO requests to /api/admin/health, with a positive control that an ADMIN
// session does make it. Both consolidate on the ONE provider mounted in AppShell, so the
// gate is proven where it actually lives. Asserted on the mocked api CALL LOG (vi.fn), the
// same "no request" seam AppShell/CustodyBoardAlert tests use.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "./api";
import { useAuth } from "../auth/AuthContext";
import { HealthStatusProvider } from "./useAdminHealth";
import { HealthDangerBanner } from "../components/HealthDangerBanner";
import { healthySilentDoc, ownerOnlyDoc, incidentDoc } from "../mocks/data/health";

// Replace only `api` (keep ApiError, MOCK_MODE, types real), and stub useAuth so the
// provider's isAdmin read is driven per test.
vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return { ...actual, api: { getAdminHealth: vi.fn(), snoozeAdminHealth: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Mount the provider with a real consumer under it (the banner), so the test exercises the
// exact production wiring: consumers read the provider, and the provider is the only fetcher.
function renderProvider() {
  return render(
    <MemoryRouter>
      <HealthStatusProvider>
        <HealthDangerBanner />
      </HealthStatusProvider>
    </MemoryRouter>,
  );
}

describe("HealthStatusProvider — the isAdmin fetch gate", () => {
  it("makes NO request to /api/admin/health for a non-admin session", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { is_admin: false } } as never);
    mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
    renderProvider();
    // Flush the mount effect's microtasks; the isAdmin gate returns before any fetch is
    // issued, so the call log stays empty.
    await Promise.resolve();
    await Promise.resolve();
    expect(mockApi.getAdminHealth).not.toHaveBeenCalled();
  });

  it("shares scoped owner danger with the banner without an instance alert", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { is_admin: true } } as never);
    mockApi.getAdminHealth.mockResolvedValue(ownerOnlyDoc());
    renderProvider();
    await act(async () => { await Promise.resolve(); });
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("alert", { name: "Instance health" })).toBeNull();
  });

  it("shows a confirmed instance blocker through the shared provider", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { is_admin: true } } as never);
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc());
    renderProvider();
    expect(await screen.findByRole("alert", { name: "Instance health" })).toBeTruthy();
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(1);
  });

  it("DOES request /api/admin/health for an admin session (positive control)", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { is_admin: true } } as never);
    mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
    renderProvider();
    await waitFor(() => expect(mockApi.getAdminHealth).toHaveBeenCalled());
  });
});
