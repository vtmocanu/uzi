// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { AppBanners } from "./AppBanners";
import { useAuth } from "../auth/AuthContext";
import { aUser, authValue } from "../pages/overviewFixtures";

vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
vi.mock("./VaultControls", () => ({ VaultLockedBanner: () => null, VaultRequiredPrompt: () => null }));
vi.mock("./UpdateEscalationBanner", () => ({ UpdateEscalationBanner: () => null }));
vi.mock("./HealthDangerBanner", () => ({ HealthDangerBanner: () => null }));
vi.mock("./HealthAttentionBanner", () => ({ HealthAttentionBanner: () => null }));
vi.mock("./MaintenanceBanner", () => ({ MaintenanceBanner: () => null }));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("AppBanners forge token notice", () => {
  it("a non-admin user with an expired connection token sees the expired banner", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue({ ...aUser, is_admin: false }));
    const yesterday = new Date(Date.now() - 24 * 3600 * 1000).toISOString();
    const connection = {
      id: "c1",
      forge_type: "github",
      base_url: "https://github.com",
      privilege_report: {
        status: "violations",
        checked_at: yesterday,
        token: {
          scopes: ["repo"],
          active: false,
          expires_at: yesterday,
          violations: ["token is not active", "token has expired"],
          warnings: [],
        },
        repos: [],
      },
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        const ok = String(url).endsWith("/forge/connections");
        const body = ok ? { connections: [connection] } : { error: "not found" };
        return { ok, status: ok ? 200 : 404, text: async () => JSON.stringify(body) } as unknown as Response;
      }),
    );
    render(
      <MemoryRouter>
        <AppBanners />
      </MemoryRouter>,
    );
    const banner = await screen.findByRole("alert", { name: "Forge token expired" });
    expect(banner.textContent).toContain("token has expired or is inactive");
  });
});
