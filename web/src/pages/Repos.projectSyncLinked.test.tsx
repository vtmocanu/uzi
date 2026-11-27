// @vitest-environment jsdom
// Project sync is not offered on a repo linked to an issue tracker (PRD 4173 M6): the API
// refuses it, so the Repos page says so instead of showing the controls.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Repos } from "./Repos";
import { api, type ForgeConnection, type Repo } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listConnections: vi.fn(),
      listProjects: vi.fn(),
      getRepoToolProfile: vi.fn(),
      listToolAllowlist: vi.fn(),
      listIssueTrackerConnections: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const GITHUB: ForgeConnection = {
  id: "conn-gh",
  forge_type: "github",
  base_url: "https://github.com",
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
    connection_id: "conn-gh",
    forge_project_id: 1,
    web_url: `https://github.com/${over.path_with_namespace}`,
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    repo_fold_improve_uzi_backlog: false,
    pipeline: null,
    guardrail_blocked: false,
    docker_allowlisted: false,
    docker_blocked: false,
    env_var_count: 0,
    ...over,
  };
}

const LINK = {
  connection_id: "itc-1",
  project_id: "0-4",
  project_short_name: "ATL",
  tag_id: "6-2",
  tag_name: "uzi",
  linked_at: "2026-11-19T10:00:00Z",
};

const rowFor = (name: string) => screen.getByText(name).closest("tr")!;

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { id: "u1", is_admin: true } } as unknown as ReturnType<typeof useAuth>);
  mockApi.listConnections.mockResolvedValue({ connections: [GITHUB] });
  mockApi.listProjects.mockResolvedValue({
    repos: [
      repo({ id: "r-plain", path_with_namespace: "acme/plain" }),
      repo({ id: "r-linked", path_with_namespace: "acme/linked", issue_tracker_link: LINK }),
    ],
  });
  mockApi.getRepoToolProfile.mockResolvedValue({ packages: [] });
  mockApi.listToolAllowlist.mockResolvedValue({ allowlist: [] });
  mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [] });
});

afterEach(cleanup);

describe("Repos: Project sync on a repo linked to an issue tracker (PRD 4173 M6)", () => {
  it("says it is not offered, with no Manage button, and keeps the control on an unlinked repo", async () => {
    render(
      <MemoryRouter>
        <Repos />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("acme/linked")).toBeTruthy());
    const linked = rowFor("acme/linked");
    expect(linked.textContent).toContain("Not offered on a repo linked to an issue tracker");
    expect(linked.querySelector('button[aria-label^="Project sync settings"]')).toBeNull();
    const plain = rowFor("acme/plain");
    expect(plain.textContent).not.toContain("Not offered on a repo linked to an issue tracker");
    expect(plain.querySelector('button[aria-label^="Project sync settings"]')).not.toBeNull();
  });
});
