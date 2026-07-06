// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Repos } from "./Repos";
import { api, type ForgeConnection, type Repo } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listConnections: vi.fn(),
      listProjects: vi.fn(),
      setRepoEnabled: vi.fn(),
      setRepoSkillsEnabled: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const CONN: ForgeConnection = {
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
};

function repo(over: Partial<Repo> & Pick<Repo, "id" | "path_with_namespace">): Repo {
  return {
    connection_id: "conn-1",
    forge_project_id: 1,
    web_url: `https://gitlab.example.com/${over.path_with_namespace}`,
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    pipeline: null,
    ...over,
  };
}

const REPOS: Repo[] = [
  repo({ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi", repo_skills_enabled: false }),
  repo({ id: "repo-atlas", path_with_namespace: "vtmocanu/atlas", repo_skills_enabled: true }),
  repo({ id: "repo-www", path_with_namespace: "example/website", enabled: false }),
];

function rowFor(name: string): HTMLElement {
  const tr = screen.getByText(name).closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return tr;
}

beforeEach(() => {
  mockApi.listConnections.mockResolvedValue({ connections: [CONN] });
  mockApi.listProjects.mockResolvedValue({ repos: REPOS.map((r) => ({ ...r })) });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter>
      <Repos />
    </MemoryRouter>,
  );
}

describe("Repos — repo-skills opt-in", () => {
  it("offers 'Load repo skills' on an enabled repo that has it off", async () => {
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    expect(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: /Load repo skills/ })).toBeTruthy();
  });

  it("shows On + Disable when repo skills are enabled", async () => {
    renderPage();
    await screen.findByText("vtmocanu/atlas");
    const row = within(rowFor("vtmocanu/atlas"));
    expect(row.getByText("On")).toBeTruthy();
    expect(row.getByRole("button", { name: "Disable repo skills" })).toBeTruthy();
  });

  it("a disabled repo has no repo-skills control", async () => {
    renderPage();
    await screen.findByText("example/website");
    const row = within(rowFor("example/website"));
    expect(row.queryByRole("button", { name: /Load repo skills/ })).toBeNull();
  });

  it("enabling shows the trust warning and only loads on confirm", async () => {
    mockApi.setRepoSkillsEnabled.mockResolvedValue({
      repo: { ...REPOS[0], repo_skills_enabled: true },
    });
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: /Load repo skills/ }));

    // The warning spells out what loads and what never does.
    expect(screen.getByText(".claude/skills/")).toBeTruthy();
    expect(screen.getByText(/never the repo/i)).toBeTruthy();
    expect(screen.getByText(/merge-request review discipline/i)).toBeTruthy();
    // Nothing changed yet — enabling is behind the confirm.
    expect(mockApi.setRepoSkillsEnabled).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Enable repo skills" }));
    await waitFor(() => expect(mockApi.setRepoSkillsEnabled).toHaveBeenCalledWith("repo-uzi", true));
  });

  it("renders the warning OUTSIDE the horizontal-scroll container (never clipped)", async () => {
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: /Load repo skills/ }));
    const warning = screen.getByRole("group", { name: /Load repo skills for vtmocanu\/uzi/ });
    // The security caveats must not sit inside an overflow-x-auto region.
    expect(warning.closest(".overflow-x-auto")).toBeNull();
  });

  it("moves focus into the warning when it opens", async () => {
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: /Load repo skills/ }));
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Enable repo skills" })),
    );
  });

  it("disabling repo skills is immediate (no warning)", async () => {
    mockApi.setRepoSkillsEnabled.mockResolvedValue({
      repo: { ...REPOS[1], repo_skills_enabled: false },
    });
    renderPage();
    await screen.findByText("vtmocanu/atlas");
    fireEvent.click(within(rowFor("vtmocanu/atlas")).getByRole("button", { name: "Disable repo skills" }));
    await waitFor(() => expect(mockApi.setRepoSkillsEnabled).toHaveBeenCalledWith("repo-atlas", false));
  });
});
