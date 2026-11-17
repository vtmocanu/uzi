// @vitest-environment jsdom
// Environment variables panel on the Repos page (PRD #4416 M1).
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Repos } from "./Repos";
import { api, ApiError, type ForgeConnection, type Repo, type RunEnvList, type RunEnvVar } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listConnections: vi.fn(),
      listProjects: vi.fn(),
      listRepoEnv: vi.fn(),
      putRepoEnv: vi.fn(),
      disableRepoEnv: vi.fn(),
      enableRepoEnv: vi.fn(),
      deleteRepoEnv: vi.fn(),
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
  env_var_count: 2,
  pipeline: null,
  guardrail_blocked: false,
  docker_allowlisted: false,
  docker_blocked: false,
};

// Assembled at runtime so no credential-shaped literal sits in source.
const SECRET_VALUE = ["s3cr", "et-val", "ue-1234"].join("");

function envVar(over: Partial<RunEnvVar> & Pick<RunEnvVar, "name">): RunEnvVar {
  return {
    secret: false,
    description: "",
    value: "https://sandbox.example.com",
    value_withheld: false,
    state: "enabled",
    rev: 1,
    disabled_at: null,
    created_at: "2026-07-01T10:00:00Z",
    updated_at: "2026-07-02T10:00:00Z",
    ...over,
  };
}

function list(vars: RunEnvVar[], over: Partial<RunEnvList["counts"]> = {}, vaultLocked = false): RunEnvList {
  return {
    variables: vars,
    counts: {
      total: vars.length,
      disabled: vars.filter((v) => v.state === "disabled").length,
      pending_deletion: vars.filter((v) => v.state === "pending_deletion").length,
      limit: 50,
      ...over,
    },
    vault_locked: vaultLocked,
  };
}

const BASE_URL = envVar({ name: "API_BASE_URL", description: "Sandbox base URL" });
const API_KEY = envVar({ name: "API_KEY", secret: true, value: null, description: "Sandbox key" });

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { id: "u1", is_admin: false } } as unknown as ReturnType<typeof useAuth>);
  mockApi.listConnections.mockResolvedValue({ connections: [CONN] });
  mockApi.listProjects.mockResolvedValue({ repos: [{ ...REPO }] });
  mockApi.listRepoEnv.mockResolvedValue(list([API_KEY, BASE_URL]));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

async function openPanel(): Promise<HTMLElement> {
  render(
    <MemoryRouter>
      <Repos />
    </MemoryRouter>,
  );
  await screen.findByText("vtmocanu/uzi");
  fireEvent.click(screen.getByRole("button", { name: "Environment variables for vtmocanu/uzi" }));
  return await screen.findByRole("group", { name: "Environment variables for vtmocanu/uzi" }).then(async (g) => {
    await within(g).findByText("API_KEY");
    return g;
  });
}

describe("Repos environment variables panel", () => {
  it("shows the live count on the repo row", async () => {
    render(
      <MemoryRouter>
        <Repos />
      </MemoryRouter>,
    );
    const row = (await screen.findByText("vtmocanu/uzi")).closest("tr")!;
    expect(within(row).getByText("2")).toBeTruthy();
  });

  it("lists names, kind, plain values, masked secrets, header counts and the fixed notes", async () => {
    mockApi.listRepoEnv.mockResolvedValue(
      list([API_KEY, BASE_URL, envVar({ name: "OLD_KEY", secret: true, value: null, state: "pending_deletion" })], { disabled: 0 }),
    );
    const panel = await openPanel();
    expect(within(panel).getByText("3 of 50, 0 disabled, 1 pending deletion")).toBeTruthy();
    expect(within(panel).getByText("https://sandbox.example.com")).toBeTruthy();
    expect(within(panel).getAllByLabelText("Secret value, set").length).toBe(2);
    expect(within(panel).getByText("Sandbox base URL")).toBeTruthy();
    expect(within(panel).getByText("Pending deletion")).toBeTruthy();
    expect(panel.textContent).toContain("The agent can read every value, secret or not.");
    expect(panel.textContent).toContain("Variables are not yet injected into runs.");
    expect(panel.textContent).toContain("does not revoke the credential; revoke it at the provider");
  });

  it("creates a secret by default and never renders the value back", async () => {
    mockApi.putRepoEnv.mockResolvedValue({ variable: envVar({ name: "NEW_TOKEN", secret: true, value: null }) });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    expect(within(panel).getByRole("switch", { name: "Secret" }).getAttribute("aria-checked")).toBe("true");
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "NEW_TOKEN" } });
    fireEvent.change(within(panel).getByLabelText("Value"), { target: { value: SECRET_VALUE } });
    fireEvent.change(within(panel).getByLabelText("Description"), { target: { value: "ci token" } });
    mockApi.listRepoEnv.mockResolvedValue(list([API_KEY, BASE_URL, envVar({ name: "NEW_TOKEN", secret: true, value: null })]));
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    await waitFor(() =>
      expect(mockApi.putRepoEnv).toHaveBeenCalledWith("repo-uzi", "NEW_TOKEN", { value: SECRET_VALUE, secret: true, description: "ci token" }),
    );
    await within(panel).findByText("NEW_TOKEN");
    expect(document.body.textContent).not.toContain(SECRET_VALUE);
    expect((document.body.innerHTML.match(new RegExp(SECRET_VALUE, "g")) ?? []).length).toBe(0);
  });

  it("editing a secret without a value sends only the description and never prefills the value", async () => {
    mockApi.putRepoEnv.mockResolvedValue({ variable: API_KEY });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Edit API_KEY" }));
    const value = within(panel).getByLabelText("Value") as HTMLInputElement;
    expect(value.value).toBe("");
    fireEvent.change(within(panel).getByLabelText("Description"), { target: { value: "rotated by hand" } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    await waitFor(() => expect(mockApi.putRepoEnv).toHaveBeenCalledWith("repo-uzi", "API_KEY", { description: "rotated by hand" }));
  });

  it("disables and re-enables a variable", async () => {
    mockApi.disableRepoEnv.mockResolvedValue({ variable: { ...BASE_URL, state: "disabled" } });
    mockApi.enableRepoEnv.mockResolvedValue({ variable: BASE_URL });
    const panel = await openPanel();
    mockApi.listRepoEnv.mockResolvedValue(list([API_KEY, { ...BASE_URL, state: "disabled" }]));
    fireEvent.click(within(panel).getByRole("button", { name: "Disable API_BASE_URL" }));
    await waitFor(() => expect(mockApi.disableRepoEnv).toHaveBeenCalledWith("repo-uzi", "API_BASE_URL"));
    expect(await within(panel).findByText("2 of 50, 1 disabled, 0 pending deletion")).toBeTruthy();
    fireEvent.click(within(panel).getByRole("button", { name: "Enable API_BASE_URL" }));
    await waitFor(() => expect(mockApi.enableRepoEnv).toHaveBeenCalledWith("repo-uzi", "API_BASE_URL"));
  });

  it("asks for confirmation before deleting and updates the row count", async () => {
    mockApi.deleteRepoEnv.mockResolvedValue({ state: "deleted" });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Delete API_BASE_URL" }));
    expect(mockApi.deleteRepoEnv).not.toHaveBeenCalled();
    mockApi.listRepoEnv.mockResolvedValue(list([API_KEY]));
    fireEvent.click(within(panel).getByRole("button", { name: "Confirm delete" }));
    await waitFor(() => expect(mockApi.deleteRepoEnv).toHaveBeenCalledWith("repo-uzi", "API_BASE_URL"));
    const row = screen.getByText("vtmocanu/uzi").closest("tr")!;
    await waitFor(() => expect(within(row).getByText("1")).toBeTruthy());
  });

  it("shows the pending-deletion outcome of a delete", async () => {
    mockApi.deleteRepoEnv.mockResolvedValue({ state: "pending_deletion" });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Delete API_KEY" }));
    fireEvent.click(within(panel).getByRole("button", { name: "Confirm delete" }));
    expect(await within(panel).findByText("API_KEY: Pending deletion, kept to mask runs")).toBeTruthy();
  });

  it("shows a 422 field error inline with its rule", async () => {
    mockApi.putRepoEnv.mockRejectedValue(
      new ApiError(422, "value: a secret must be at least 8 characters", { field: "value", rule: "a secret must be at least 8 characters" }),
    );
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "SHORT" } });
    fireEvent.change(within(panel).getByLabelText("Value"), { target: { value: "x" } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    expect(await within(panel).findByText("value: a secret must be at least 8 characters")).toBeTruthy();
  });

  it("shows the vault-locked message on a 409 vault_locked save", async () => {
    mockApi.putRepoEnv.mockRejectedValue(new ApiError(409, "vault is locked", { code: "vault_locked" }));
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "TOKEN" } });
    fireEvent.change(within(panel).getByLabelText("Value"), { target: { value: SECRET_VALUE } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    expect(await within(panel).findByText(/Your vault is locked/)).toBeTruthy();
  });

  it("lists names with plain values withheld while the vault is locked", async () => {
    mockApi.listRepoEnv.mockResolvedValue(list([API_KEY, { ...BASE_URL, value: null, value_withheld: true }], {}, true));
    const panel = await openPanel();
    expect(within(panel).getByText("API_BASE_URL")).toBeTruthy();
    expect(within(panel).getByText("withheld")).toBeTruthy();
    expect(within(panel).getByText(/plain values are withheld/)).toBeTruthy();
  });
});
