// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ForgeSettings } from "./ForgeSettings";
import { api, ApiError, type ForgeConnection, type PrivilegeReport } from "../lib/api";
import { mockForgeConfigMultiForge } from "../mocks/data";

// Only the api object is swapped; ApiError and the types stay real so the page's
// `instanceof ApiError` checks match what the mocked methods throw. The page loads
// forgeConfig + listConnections and drives both the identity-mapping (PRD #19) and
// privilege (PRD #5) endpoints.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      forgeConfig: vi.fn(),
      listConnections: vi.fn(),
      createConnection: vi.fn(),
      verifyConnection: vi.fn(),
      updateConnection: vi.fn(),
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
    human_username: null,
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
        role: "admin",
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

const usernameField = () => screen.getByLabelText(/Your username on/i) as HTMLInputElement;

describe("ForgeSettings — forge identity mapping (PRD #19 M3)", () => {
  it("shows the identity section seeded from the stored username", async () => {
    mockApi.listConnections.mockResolvedValue({ connections: [conn({ human_username: "vlad" })] });
    renderPage();
    await waitFor(() => expect(usernameField().value).toBe("vlad"));
    // Explains the autopilot attribution purpose.
    expect(screen.getByText(/attribute an/i)).toBeTruthy();
  });

  it("saves an edited username", async () => {
    mockApi.updateConnection.mockResolvedValue({ connection: conn({ human_username: "vlad" }) });
    renderPage();
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "vlad" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    await waitFor(() => expect(mockApi.updateConnection).toHaveBeenCalledWith("conn-1", "vlad"));
    expect(await screen.findByText(/Saved your forge username: vlad/i)).toBeTruthy();
  });

  it("surfaces the verified-or-warned warning while still saving", async () => {
    mockApi.updateConnection.mockResolvedValue({
      connection: conn({ human_username: "ghost" }),
      warning: "Saved, but no forge account with this username was found — double-check it.",
    });
    renderPage();
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "ghost" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    // The warning Alert carries the not-found message.
    expect(await screen.findByText(/no forge account with this username/i)).toBeTruthy();
  });

  it("hard-rejects a username already mapped by another user (409)", async () => {
    mockApi.updateConnection.mockRejectedValue(
      new ApiError(409, "that forge username is already mapped by another user on this host"),
    );
    renderPage();
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "taken" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    expect(await screen.findByText(/already mapped by another user/i)).toBeTruthy();
  });
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
            repos: [{ repo_id: "r1", path: "vtmocanu/uzi", role: "write", member: true, violations: [], warnings: [] }],
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
      connections: [
        conn({ privilege_status: "violations", privilege_checked_at: "2026-07-05T12:00:00Z", privilege_report: violationReport() }),
      ],
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

describe("ForgeSettings — forge-type picker (PRD #65 D11, lands dark)", () => {
  const tokenInput = () => document.querySelector('input[type="password"]') as HTMLInputElement;

  it("hides the picker and sends forge_type gitlab while only one type is advertised (dark landing)", async () => {
    // Default beforeEach config advertises forge_types: ["gitlab"].
    mockApi.createConnection.mockResolvedValue({ connection: conn() });
    renderPage();
    await screen.findByText("unchecked"); // page loaded

    // No forge-type picker while a single type is advertised — the form is unchanged.
    expect(screen.queryByLabelText("Forge type")).toBeNull();

    fireEvent.change(tokenInput(), { target: { value: "glpat-ok" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    // The connect call still carries forge_type "gitlab" — byte-identical to before.
    await waitFor(() =>
      expect(mockApi.createConnection).toHaveBeenCalledWith("https://gitlab.example.com", "glpat-ok", "gitlab"),
    );
  });

  it("shows the picker when >1 type is advertised and defaults the selection to gitlab", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigMultiForge);
    mockApi.createConnection.mockResolvedValue({ connection: conn() });
    renderPage();
    await screen.findByText("unchecked");

    const picker = screen.getByLabelText("Forge type") as HTMLSelectElement;
    expect(picker).toBeTruthy();
    // Options render the friendly platform names, default selected is the first (gitlab).
    expect(picker.value).toBe("gitlab");
    expect(screen.getByRole("option", { name: "GitLab" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Forgejo" })).toBeTruthy();

    fireEvent.change(tokenInput(), { target: { value: "glpat-ok" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    // Untouched selection still sends gitlab.
    await waitFor(() =>
      expect(mockApi.createConnection).toHaveBeenCalledWith("https://gitlab.example.com", "glpat-ok", "gitlab"),
    );
  });

  it("threads the chosen forge type through createConnection", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigMultiForge);
    mockApi.createConnection.mockResolvedValue({ connection: conn({ forge_type: "forgejo" }) });
    renderPage();
    const picker = (await screen.findByLabelText("Forge type")) as HTMLSelectElement;

    fireEvent.change(picker, { target: { value: "forgejo" } });
    fireEvent.change(tokenInput(), { target: { value: "tok-forgejo" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    await waitFor(() =>
      expect(mockApi.createConnection).toHaveBeenCalledWith(
        "https://gitlab.example.com",
        "tok-forgejo",
        "forgejo",
      ),
    );
  });
});
