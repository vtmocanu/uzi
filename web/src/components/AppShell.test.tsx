// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import type { ReactElement } from "react";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell, forgeIcon } from "./AppShell";
import { GitIcon, GitLabIcon } from "./icons";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// AppShell joins repos (board children) with forge connections web-side and gates
// the signed-in shell on useAuth; both are mocked so these stay page-level and
// offline.
vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: { listRepos: vi.fn(), listConnections: vi.fn() },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const user = {
  id: "u1",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

const repos = [
  {
    id: "repo-uzi",
    connection_id: "conn-1",
    forge_project_id: 1,
    path_with_namespace: "vtmocanu/uzi",
    web_url: "https://gitlab.example.com/vtmocanu/uzi",
    default_branch: "main",
    enabled: true,
  },
  {
    id: "repo-atlas",
    connection_id: "conn-2",
    forge_project_id: 2,
    path_with_namespace: "vtmocanu/atlas",
    web_url: "https://gitlab.example.com/vtmocanu/atlas",
    default_branch: "main",
    enabled: true,
  },
];

const gitlabConnection = {
  id: "conn-1",
  forge_type: "gitlab",
  base_url: "https://gitlab.example.com",
  bot_username: "bot",
  bot_forge_user_id: 1,
  human_username: null,
  created_at: "2026-01-01T00:00:00Z",
  last_verified_at: null,
};

function renderShell(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell>
        <div>content</div>
      </AppShell>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
  });
  mockApi.listRepos.mockResolvedValue({ repos });
  mockApi.listConnections.mockResolvedValue({ connections: [gitlabConnection] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AppShell navigation", () => {
  it("renders the grouped nav with board children that carry forge icons, and no standalone Forge entry", async () => {
    renderShell("/dashboard");

    // Board children arrive from listRepos; await one before asserting.
    const uziBoard = await screen.findByRole("link", { name: "vtmocanu/uzi" });

    for (const group of ["Work", "Factory", "Configure", "Help"]) {
      expect(screen.getByText(group)).toBeTruthy();
    }
    for (const item of ["Overview", "Boards", "Runs", "Agents", "Workers", "Settings", "Docs"]) {
      expect(screen.getByRole("link", { name: item })).toBeTruthy();
    }

    // Every board child renders an inline forge glyph.
    expect(uziBoard.querySelector("svg")).not.toBeNull();
    expect(screen.getByRole("link", { name: "vtmocanu/atlas" }).querySelector("svg")).not.toBeNull();

    // Decision 3: Forge is reachable only under Settings, never as its own entry.
    expect(screen.queryByRole("link", { name: "Forge" })).toBeNull();
  });

  it("still renders board children (with the fallback icon) when the connections join fails", async () => {
    mockApi.listConnections.mockRejectedValue(new Error("offline"));
    renderShell("/dashboard");

    const uziBoard = await screen.findByRole("link", { name: "vtmocanu/uzi" });
    expect(uziBoard.querySelector("svg")).not.toBeNull();
  });

  it("keeps Settings lit across /settings/* except /settings/workers (owned by the Workers entry)", async () => {
    // /settings itself → Settings active.
    renderShell("/settings");
    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    cleanup();

    // A forge tab lands under /settings/forge → Settings stays lit.
    renderShell("/settings/forge");
    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("link", { name: "Workers" }).getAttribute("aria-current")).toBeNull();
    cleanup();

    // /settings/workers belongs to the Factory "Workers" entry, not Settings.
    renderShell("/settings/workers");
    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());
    expect(screen.getByRole("link", { name: "Workers" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBeNull();
  });
});

describe("forgeIcon (Decision 2 mapping)", () => {
  it("maps gitlab to the tanuki and any other/unknown type to the generic git mark", () => {
    expect((forgeIcon("gitlab") as ReactElement).type).toBe(GitLabIcon);
    expect((forgeIcon("forgejo") as ReactElement).type).toBe(GitIcon);
    expect((forgeIcon(undefined) as ReactElement).type).toBe(GitIcon);
  });
});
