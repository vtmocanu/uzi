// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ForgeSettings } from "./ForgeSettings";
import { api, ApiError, type ForgeConnection, type PrivilegeReport } from "../lib/api";

// ForgeSettings loads forgeConfig + listConnections and drives the privilege
// endpoints. Mock the api (keep the real ApiError export for the 422 path).
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      forgeConfig: vi.fn(),
      listConnections: vi.fn(),
      createConnection: vi.fn(),
      verifyConnection: vi.fn(),
      privilegeCheck: vi.fn(),
      deleteConnection: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function conn(over: Partial<ForgeConnection> = {}): ForgeConnection {
  return {
    id: "conn-1",
    forge_type: "gitlab",
    base_url: "https://gitlab.example.com",
    bot_username: "uzi-bot",
    bot_forge_user_id: 1,
    created_at: "2026-01-01T00:00:00Z",
    last_verified_at: "2026-07-05T12:00:00Z",
    privilege_status: null,
    privilege_checked_at: null,
    privilege_report: null,
    ...over,
  };
}

function violationReport(): PrivilegeReport {
  return {
    checked_at: "2026-07-05T12:00:00Z",
    status: "violations",
    token: { scopes: ["api"], active: true, violations: [], warnings: [] },
    repos: [
      {
        repo_id: "repo-atlas",
        path: "vtmocanu/atlas-api",
        role: 40,
        member: true,
        violations: ["bot role is Maintainer (40), expected Developer (30)"],
        warnings: [],
      },
    ],
  };
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ForgeSettings />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.forgeConfig.mockResolvedValue({
    allowed_base_urls: ["https://gitlab.example.com"],
    forge_types: ["gitlab"],
  });
  mockApi.listConnections.mockResolvedValue({ connections: [conn()] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ForgeSettings privilege surfacing", () => {
  it("renders an unchecked badge for a never-checked connection", async () => {
    renderPage();
    expect(await screen.findByText("unchecked")).toBeTruthy();
  });

  it("renders least-privilege ✓ and expands to the findings", async () => {
    mockApi.listConnections.mockResolvedValue({
      connections: [
        conn({
          privilege_status: "ok",
          privilege_checked_at: "2026-07-05T12:00:00Z",
          privilege_report: {
            checked_at: "2026-07-05T12:00:00Z",
            status: "ok",
            token: { scopes: ["api"], active: true, violations: [], warnings: [] },
            repos: [{ repo_id: "r1", path: "vtmocanu/uzi", role: 30, member: true, violations: [], warnings: [] }],
          },
        }),
      ],
    });
    renderPage();
    const badge = await screen.findByText("least-privilege ✓");
    fireEvent.click(badge);
    // The expanded panel names the repo and reports it clean.
    expect(await screen.findByText("vtmocanu/uzi")).toBeTruthy();
    expect(screen.getByText(/Everything is least-privilege/)).toBeTruthy();
  });

  it("shows a violation count and the specific finding when expanded", async () => {
    mockApi.listConnections.mockResolvedValue({
      connections: [conn({ privilege_status: "violations", privilege_checked_at: "2026-07-05T12:00:00Z", privilege_report: violationReport() })],
    });
    renderPage();
    const badge = await screen.findByText("1 violation");
    fireEvent.click(badge);
    expect(await screen.findByText(/bot role is Maintainer \(40\)/)).toBeTruthy();
  });

  it("runs a privilege check and updates the badge", async () => {
    renderPage();
    await screen.findByText("unchecked");
    mockApi.privilegeCheck.mockResolvedValue({
      report: {
        checked_at: "2026-07-05T12:30:00Z",
        status: "ok",
        token: { scopes: ["api"], active: true, violations: [], warnings: [] },
        repos: [],
      },
    });
    fireEvent.click(screen.getByRole("button", { name: /Check privileges/ }));
    expect(mockApi.privilegeCheck).toHaveBeenCalledWith("conn-1");
    expect(await screen.findByText("least-privilege ✓")).toBeTruthy();
  });

  it("renders 422 violations with a doc link when a save is rejected", async () => {
    mockApi.createConnection.mockRejectedValue(
      new ApiError(422, "the bot token is over-privileged and was not saved", {
        violations: ["token scopes [api sudo] exceed the required [api]"],
      }),
    );
    renderPage();
    await screen.findByText("unchecked"); // page loaded
    const tokenInput = document.querySelector('input[type="password"]') as HTMLInputElement;
    fireEvent.change(tokenInput, { target: { value: "glpat-oops" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    expect(await screen.findByText(/token scopes \[api sudo\] exceed the required \[api\]/)).toBeTruthy();
    const docLink = screen.getByRole("link", { name: /bot setup guide/ });
    expect(docLink.getAttribute("href")).toBe("/docs/gitlab-bot-setup");
  });
});
