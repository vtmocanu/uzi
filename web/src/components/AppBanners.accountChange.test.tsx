// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { AppBanners } from "./AppBanners";
import { useAuth } from "../auth/AuthContext";
import { setUnauthorizedHandler } from "../lib/api";
import { aUser, authValue } from "../pages/overviewFixtures";

vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
vi.mock("./VaultControls", () => ({ VaultLockedBanner: () => null, VaultRequiredPrompt: () => null }));
vi.mock("./UpdateEscalationBanner", () => ({ UpdateEscalationBanner: () => null }));
vi.mock("./HealthDangerBanner", () => ({ HealthDangerBanner: () => null }));
vi.mock("./HealthAttentionBanner", () => ({ HealthAttentionBanner: () => null }));
vi.mock("./MaintenanceBanner", () => ({ MaintenanceBanner: () => null }));

const DAY = 24 * 3600 * 1000;

function connection(id: string, offsetMs: number) {
  return {
    id,
    forge_type: "github",
    base_url: `https://${id}.example.com`,
    privilege_report: {
      status: "ok",
      checked_at: new Date().toISOString(),
      repos: [],
      token: {
        scopes: [],
        active: true,
        expires_at: new Date(Date.now() + offsetMs).toISOString(),
        violations: [],
        warnings: [],
      },
    },
  };
}

type Pending = { resolve: (r: Response) => void };
let pending: Pending[] = [];

function respond(index: number, status: number, body: unknown) {
  pending[index].resolve({
    ok: status >= 200 && status < 300,
    status,
    text: async () => JSON.stringify(body),
  } as unknown as Response);
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
}

function tree() {
  return (
    <MemoryRouter>
      <AppBanners />
    </MemoryRouter>
  );
}

function signInAs(id: string) {
  vi.mocked(useAuth).mockReturnValue(authValue({ ...aUser, id, is_admin: false }));
}

let handler: ReturnType<typeof vi.fn<() => void>>;

beforeEach(() => {
  pending = [];
  window.sessionStorage.clear();
  handler = vi.fn<() => void>();
  setUnauthorizedHandler(handler);
  vi.stubGlobal(
    "fetch",
    vi.fn(
      (url: string) =>
        new Promise<Response>((resolve) => {
          if (String(url).endsWith("/forge/connections")) pending.push({ resolve });
          else resolve({ ok: false, status: 404, text: async () => "{}" } as unknown as Response);
        }),
    ),
  );
});

afterEach(() => {
  cleanup();
  setUnauthorizedHandler(null);
  vi.unstubAllGlobals();
});

describe("AppBanners across an account change (#4817)", () => {
  it("never shows the previous account's late expired response", async () => {
    signInAs("user-a");
    const { rerender } = render(tree());
    await settle();
    expect(pending).toHaveLength(1);

    signInAs("user-b");
    rerender(tree());
    await settle();
    expect(pending).toHaveLength(2);

    respond(1, 200, { connections: [connection("b", 3 * DAY)] });
    await settle();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();

    respond(0, 200, { connections: [connection("a", -DAY)] });
    await settle();
    expect(screen.queryByRole("alert", { name: "Forge token expired" })).toBeNull();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
  });

  it("a previous account's late 401 does not trip the unauthorized handler", async () => {
    signInAs("user-a");
    const { rerender } = render(tree());
    await settle();
    signInAs("user-b");
    rerender(tree());
    await settle();

    respond(1, 200, { connections: [connection("b", 3 * DAY)] });
    await settle();
    respond(0, 401, { error: "unauthorized" });
    await settle();

    expect(handler).not.toHaveBeenCalled();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
  });
  it("renders for an admin without a duplicate sibling key", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      vi.mocked(useAuth).mockReturnValue(authValue({ ...aUser, id: "admin-1", is_admin: true }));
      render(tree());
      await settle();
      respond(0, 200, { connections: [connection("adm", 3 * DAY)] });
      await settle();
      expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
      const dup = errors.mock.calls.filter((c) => c.some((a) => String(a).includes("same key")));
      expect(dup).toEqual([]);
    } finally {
      errors.mockRestore();
    }
  });
});
