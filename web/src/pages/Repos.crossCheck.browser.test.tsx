/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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
      setRepoEnabled: vi.fn(),
      deleteRepo: vi.fn(),
      setRepoSkillsEnabled: vi.fn(),
      setRepoClaudemdEnabled: vi.fn(),
      setRepoTrustFlags: vi.fn(),
      setRepoCodeCrossCheckInputs: vi.fn(),
      getRepoToolProfile: vi.fn(),
      listToolAllowlist: vi.fn(),
      setRepoToolProfile: vi.fn(),
      setRepoDevboxOptIn: vi.fn(),
      setRepoFoldImproveUziBacklog: vi.fn(),
      setRepoAgentSourceDefault: vi.fn(),
      setRepoRequiredCapabilities: vi.fn(),
      setRepoIgnoredInferredCapabilities: vi.fn(),
      setRepoGuardrailOverride: vi.fn(),
      clearRepoGuardrailOverride: vi.fn(),
      requestGuardrailOverride: vi.fn(),
      getProjectSyncStatus: vi.fn(),
      getProjectSyncOwnerType: vi.fn(),
      provisionProjectSync: vi.fn(),
      adoptProjectSync: vi.fn(),
      resyncProjectSync: vi.fn(),
      autocreateProjectSyncColumns: vi.fn(),
      disableProjectSync: vi.fn(),
      getProjectSyncVisibility: vi.fn(),
      setProjectSyncVisibility: vi.fn(),
      shareProjectSync: vi.fn(),
      unshareProjectSync: vi.fn(),
    },
  };
});

vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

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

const REPO: Repo = {
  id: "repo-uzi",
  path_with_namespace: "vtmocanu/uzi",
  connection_id: "conn-1",
  forge_project_id: 1,
  web_url: "https://gitlab.example.com/vtmocanu/uzi",
  default_branch: "main",
  enabled: true,
  repo_skills_enabled: false,
  repo_claudemd_enabled: false,
  repo_devbox_opt_in: false,
  repo_fold_improve_uzi_backlog: false,
  env_var_count: 0,
  pipeline: null,
  guardrail_blocked: false,
  docker_allowlisted: false,
  docker_blocked: false,
};

const SWITCH = "Give the cross-checkers this repository's CLAUDE.md";

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1", is_admin: false },
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.listConnections.mockResolvedValue({ connections: [CONN] });
  mockApi.listProjects.mockResolvedValue({ repos: [{ ...REPO }] });
  mockApi.getRepoToolProfile.mockResolvedValue({ packages: [] });
  mockApi.listToolAllowlist.mockResolvedValue({ allowlist: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

async function openCrossCheckPanel(): Promise<HTMLElement> {
  render(
    <MemoryRouter>
      <Repos />
    </MemoryRouter>,
  );
  const name = await screen.findByText("vtmocanu/uzi");
  const row = name.closest("tr");
  if (!row) throw new Error("no row for vtmocanu/uzi");
  await userEvent.click(within(row).getByRole("button", { name: /Code cross-check settings for/ }));
  return screen.getByRole("group", { name: /Code cross-check for vtmocanu\/uzi/ });
}

it("explains both code and plan CLAUDE.md consent and saves with the new notice", async () => {
  mockApi.setRepoCodeCrossCheckInputs.mockResolvedValue({
    repo: { ...REPO, code_cross_check_guidance: "", cross_check_repo_instructions_enabled: true },
  });
  const panel = await openCrossCheckPanel();

  // The heading renders &rsquo; (U+2019); SWITCH (ASCII apostrophe) is the Toggle's accessible name.
  expect(within(panel).getByText("Give the cross-checkers this repository\u2019s CLAUDE.md", { selector: "p" })).toBeTruthy();
  const text = panel.textContent ?? "";
  expect(text).toContain("Code checks read the root CLAUDE.md at a freshly fetched default-branch tip");
  expect(text).toContain("consent is frozen when the check is created");
  expect(text).toContain("Plan checks read it at the plan’s frozen base commit");
  expect(text).toContain("only when the lead’s Repo instructions setting is also on");
  expect(text).toContain("Plan-check consent is read when the checker starts, so a change here also affects a plan check still waiting to start");
  expect(text).toContain("code: default-branch tip");
  expect(text).toContain("plan: frozen base commit");

  const toggle = within(panel).getByRole("switch", { name: SWITCH });
  expect(toggle.getAttribute("aria-checked")).toBe("false");
  await userEvent.click(toggle);
  await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe("true"));

  await userEvent.click(within(panel).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(mockApi.setRepoCodeCrossCheckInputs).toHaveBeenCalledTimes(1));
  expect(await within(panel).findByText("Saved. New code cross-checks use these settings; plan cross-checks read the CLAUDE.md setting when their checker starts.")).toBeTruthy();
});
