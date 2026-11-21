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

let varIdSeq = 0;

function envVar(over: Partial<RunEnvVar> & Pick<RunEnvVar, "name">): RunEnvVar {
  return {
    var_id: `00000000-0000-4000-8000-${String(++varIdSeq).padStart(12, "0")}`,
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

// Like openPanel, but waits for the first listed variable instead of API_KEY.
async function openPanel2(): Promise<HTMLElement> {
  render(
    <MemoryRouter>
      <Repos />
    </MemoryRouter>,
  );
  await screen.findByText("vtmocanu/uzi");
  fireEvent.click(screen.getByRole("button", { name: "Environment variables for vtmocanu/uzi" }));
  const g = await screen.findByRole("group", { name: "Environment variables for vtmocanu/uzi" });
  await within(g).findByText("API_BASE_URL");
  return g;
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
    expect(panel.textContent).toContain("Variables are set in the environment of every command a repo’s issue, CI-fix, prompt, self-improvement, rework and task runs execute; review, isolated, chat, judge, job and cross-check runs do not receive them.");
    expect(panel.textContent).toContain("Secret values are redacted from run output and checked before anything is pushed.");
    expect(panel.textContent).not.toContain("not yet injected");
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

  const PEM = ["-----BEGIN KEY-----", "  MIIBVgIBADAN", "", "\tindented-tab", "-----END KEY-----", ""].join("\n");
  it.each([
    ["single-line secret", "one-line-value", true, false],
    ["multiline secret", PEM, true, true],
    ["single-line plain", "one-line-value", false, false],
    ["multiline plain", PEM, false, true],
    ["single-line plain with spaces, unicode and a trailing space", "  grüße 日本語 value ", false, false],
    ["multiline LF value", "line1\nline2\n", true, true],
  ])("submits a %s byte-exact", async (_label, text, secret, multiline) => {
    mockApi.putRepoEnv.mockResolvedValue({ variable: envVar({ name: "MULTI", secret, value: null }) });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    if (!secret) fireEvent.click(within(panel).getByRole("switch", { name: "Secret" }));
    if (multiline) fireEvent.click(within(panel).getByRole("switch", { name: "Multiline" }));
    expect(within(panel).getByLabelText("Value").tagName).toBe(multiline ? "TEXTAREA" : "INPUT");
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "MULTI" } });
    fireEvent.change(within(panel).getByLabelText("Value"), { target: { value: text } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    await waitFor(() => expect(mockApi.putRepoEnv).toHaveBeenCalled());
    const sent = mockApi.putRepoEnv.mock.calls[0][2];
    expect(sent.value).toBe(text);
    expect(sent.secret).toBe(secret);
  });

  it("conceals a secret value in a native password input and shows a plain one as text", async () => {
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    const value = within(panel).getByLabelText("Value") as HTMLInputElement;
    expect(value.tagName).toBe("INPUT");
    expect(value.type).toBe("password");
    expect(value.getAttribute("autocomplete")).toBe("new-password");
    fireEvent.click(within(panel).getByRole("switch", { name: "Secret" }));
    const plain = within(panel).getByLabelText("Value") as HTMLInputElement;
    expect(plain.tagName).toBe("INPUT");
    expect(plain.type).toBe("text");
    expect(plain.getAttribute("autocomplete")).toBe("off");
  });

  it("multiline mode shows the masking and LF help and a textarea with autocomplete off", async () => {
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    expect(within(panel).queryByText(/not portable across browsers/)).toBeNull();
    fireEvent.click(within(panel).getByRole("switch", { name: "Multiline" }));
    const value = within(panel).getByLabelText("Value") as HTMLTextAreaElement;
    expect(value.tagName).toBe("TEXTAREA");
    expect(value.getAttribute("autocomplete")).toBe("off");
    expect(within(panel).getByText(/not portable across browsers/).textContent).toContain("Line endings are stored as LF");
  });

  it("editing a plain value with a newline opens in multiline and cannot leave it", async () => {
    mockApi.listRepoEnv.mockResolvedValue(list([API_KEY, envVar({ name: "CERT", value: "a\nb" })]));
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Edit CERT" }));
    const value = within(panel).getByLabelText("Value") as HTMLTextAreaElement;
    expect(value.tagName).toBe("TEXTAREA");
    expect(value.value).toBe("a\nb");
    const toggle = within(panel).getByRole("switch", { name: "Multiline" }) as HTMLButtonElement;
    // Locked on but still focusable, and described by the hint and help texts.
    expect(toggle.disabled).toBe(false);
    expect(toggle.getAttribute("aria-disabled")).toBe("true");
    const described = toggle.getAttribute("aria-describedby")!.split(" ").map((id) => document.getElementById(id)?.textContent);
    expect(described.join(" ")).toContain("Multiline stays on while the value contains a line break.");
    expect(described.join(" ")).toContain("Line endings are stored as LF.");
    fireEvent.click(toggle);
    expect(within(panel).getByLabelText("Value").tagName).toBe("TEXTAREA");
    // Once the line break is gone the toggle is free again.
    fireEvent.change(value, { target: { value: "ab" } });
    expect(within(panel).getByRole("switch", { name: "Multiline" }).getAttribute("aria-disabled")).toBeNull();
  });

  it("shows the browser-portability sentence for a multiline secret only", async () => {
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.click(within(panel).getByRole("switch", { name: "Multiline" }));
    expect(within(panel).getByText(/not portable across browsers/)).toBeTruthy();
    fireEvent.click(within(panel).getByRole("switch", { name: "Secret" }));
    expect(within(panel).queryByText(/not portable across browsers/)).toBeNull();
    expect(within(panel).getByText("Line endings are stored as LF.")).toBeTruthy();
  });

  it("pasting a value with line breaks into the single-line field switches to multiline with LF", async () => {
    mockApi.putRepoEnv.mockResolvedValue({ variable: envVar({ name: "MIXED", secret: true, value: null }) });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "MIXED" } });
    const mixedEndings = "a\r\nb\rc";
    fireEvent.paste(within(panel).getByLabelText("Value"), { clipboardData: { getData: () => mixedEndings } });
    const value = within(panel).getByLabelText("Value") as HTMLTextAreaElement;
    expect(value.tagName).toBe("TEXTAREA");
    // What the editor displays (the textarea getter normalizes endings on its own)...
    expect(value.value).toBe("a\nb\nc");
    // ...and what Save sends from the draft, which only the paste handler normalizes.
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    await waitFor(() => expect(mockApi.putRepoEnv).toHaveBeenCalled());
    expect(mockApi.putRepoEnv.mock.calls[0][2].value).toBe("a\nb\nc");
  });

  it.each([
    ["LF", "token\n"],
    ["CRLF", "token\r\n"],
  ])("pasting a value with one trailing %s break stays single-line and saves it trimmed", async (_l, pasted) => {
    mockApi.putRepoEnv.mockResolvedValue({ variable: envVar({ name: "TOK", secret: true, value: null }) });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "TOK" } });
    fireEvent.paste(within(panel).getByLabelText("Value"), { clipboardData: { getData: () => pasted } });
    const value = within(panel).getByLabelText("Value") as HTMLInputElement;
    expect(value.tagName).toBe("INPUT");
    expect(value.value).toBe("token");
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    await waitFor(() => expect(mockApi.putRepoEnv).toHaveBeenCalled());
    expect(mockApi.putRepoEnv.mock.calls[0][2].value).toBe("token");
  });

  it("pasting multi-line text with a trailing break switches to multiline and keeps the break", async () => {
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.paste(within(panel).getByLabelText("Value"), { clipboardData: { getData: () => "a\nb\n" } });
    const value = within(panel).getByLabelText("Value") as HTMLTextAreaElement;
    expect(value.tagName).toBe("TEXTAREA");
    expect(value.value).toBe("a\nb\n");
  });

  it("a trimmed paste replaces the selection, stays single-line and puts the caret after it", async () => {
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    const before = within(panel).getByLabelText("Value") as HTMLInputElement;
    fireEvent.change(before, { target: { value: "leftMIDright" } });
    before.setSelectionRange(4, 7);
    fireEvent.paste(before, { clipboardData: { getData: () => "X\n" } });
    const value = within(panel).getByLabelText("Value") as HTMLInputElement;
    expect(value.tagName).toBe("INPUT");
    expect(value.value).toBe("leftXright");
    expect(document.activeElement).toBe(value);
    expect([value.selectionStart, value.selectionEnd]).toEqual([5, 5]);
  });

  it.each([
    ["secret", true, "new-password"],
    ["plain", false, "off"],
  ])("a %s draft marks Name and Value as password-manager ignored", async (_l, secret, valueAutoComplete) => {
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    if (!secret) fireEvent.click(within(panel).getByRole("switch", { name: "Secret" }));
    const name = within(panel).getByLabelText("Name");
    const value = within(panel).getByLabelText("Value");
    for (const el of [name, value]) {
      expect(el.hasAttribute("data-1p-ignore")).toBe(true);
      expect(el.getAttribute("data-lpignore")).toBe("true");
      expect(el.hasAttribute("data-bwignore")).toBe(true);
    }
    expect(name.getAttribute("autocomplete")).toBe("off");
    expect(value.getAttribute("autocomplete")).toBe(valueAutoComplete);
    fireEvent.click(within(panel).getByRole("switch", { name: "Multiline" }));
    const area = within(panel).getByLabelText("Value");
    expect(area.tagName).toBe("TEXTAREA");
    expect(area.hasAttribute("data-1p-ignore")).toBe(true);
    expect(area.getAttribute("data-lpignore")).toBe("true");
    expect(area.hasAttribute("data-bwignore")).toBe(true);
    expect(area.getAttribute("autocomplete")).toBe("off");
  });

  it.each([
    ["a plain variable", BASE_URL],
    ["a plain variable whose value the vault withholds", { ...BASE_URL, value: null, value_withheld: true }],
  ])("flipping %s to secret without touching the value sends no value", async (_l, variable) => {
    mockApi.listRepoEnv.mockResolvedValue(list([variable]));
    mockApi.putRepoEnv.mockResolvedValue({ variable: { ...variable, secret: true } });
    const panel = await openPanel2();
    fireEvent.click(within(panel).getByRole("button", { name: "Edit API_BASE_URL" }));
    fireEvent.click(within(panel).getByRole("switch", { name: "Secret" }));
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    await waitFor(() =>
      expect(mockApi.putRepoEnv).toHaveBeenCalledWith("repo-uzi", "API_BASE_URL", { description: "Sandbox base URL", secret: true }),
    );
  });

  it("switching a secret to plain with an empty value still sends the empty value", async () => {
    mockApi.putRepoEnv.mockResolvedValue({ variable: envVar({ name: "API_KEY" }) });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Edit API_KEY" }));
    const value = within(panel).getByLabelText("Value") as HTMLInputElement;
    expect(value.placeholder).toBe("Leave empty to keep the stored value");
    fireEvent.click(within(panel).getByRole("switch", { name: "Secret" }));
    // The keep-the-stored-value hint would be false now: an empty field is saved as empty.
    const plainValue = within(panel).getByLabelText("Value") as HTMLInputElement;
    expect(plainValue.placeholder).not.toMatch(/keep the stored value/i);
    expect(plainValue.placeholder).toMatch(/empty field saves an empty value/);
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    await waitFor(() =>
      expect(mockApi.putRepoEnv).toHaveBeenCalledWith("repo-uzi", "API_KEY", { description: "Sandbox key", secret: false, value: "" }),
    );
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

  it("names the holding runs as links on a 409 env_history_full save", async () => {
    // PRD #4416 M2: a full secret-version history names the runs holding versions as links.
    mockApi.putRepoEnv.mockRejectedValue(
      new ApiError(409, "secret version history is full", { code: "env_history_full", held_by_runs: ["run-a", "run-b"] }),
    );
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "TOKEN" } });
    fireEvent.change(within(panel).getByLabelText("Value"), { target: { value: SECRET_VALUE } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    expect(await within(panel).findByText("secret version history is full")).toBeTruthy();
    expect(within(panel).getByRole("link", { name: "run-a" }).getAttribute("href")).toBe("/runs/run-a");
    expect(within(panel).getByRole("link", { name: "run-b" }).getAttribute("href")).toBe("/runs/run-b");
    expect(panel.textContent).toContain("kept while the runs holding them are active, for at least 24 hours after they end, and longer while their retained work is kept. You can still Disable or Delete a variable.");
    expect(panel.textContent).not.toContain("Finish or cancel them");
  });

  it("shows no run links when env_history_full names none", async () => {
    mockApi.putRepoEnv.mockRejectedValue(new ApiError(409, "secret version history is full", { code: "env_history_full" }));
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Add variable" }));
    fireEvent.change(within(panel).getByLabelText("Name"), { target: { value: "TOKEN" } });
    fireEvent.change(within(panel).getByLabelText("Value"), { target: { value: SECRET_VALUE } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save variable" }));
    expect(await within(panel).findByText("secret version history is full")).toBeTruthy();
    expect(within(panel).queryByRole("link")).toBeNull();
    expect(panel.textContent).toContain("You can still Disable or Delete a variable");
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

  it("renders two pending-deletion rows of the same name and rev without a duplicate-key warning", async () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      mockApi.listRepoEnv.mockResolvedValue(
        list([
          API_KEY,
          envVar({ name: "OLD_KEY", var_id: "11111111-1111-4111-8111-111111111111", secret: true, value: null, state: "pending_deletion", rev: 1 }),
          envVar({ name: "OLD_KEY", var_id: "22222222-2222-4222-8222-222222222222", secret: true, value: null, state: "pending_deletion", rev: 1 }),
        ]),
      );
      const panel = await openPanel();
      expect(within(panel).getAllByText("OLD_KEY").length).toBe(2);
      expect(within(panel).getAllByText("Pending deletion").length).toBe(2);
      const dupKey = errSpy.mock.calls.filter((c) => String(c[0]).includes("same key"));
      expect(dupKey).toEqual([]);
    } finally {
      errSpy.mockRestore();
    }
  });

  it("lists names with plain values withheld while the vault is locked", async () => {
    mockApi.listRepoEnv.mockResolvedValue(list([API_KEY, { ...BASE_URL, value: null, value_withheld: true }], {}, true));
    const panel = await openPanel();
    expect(within(panel).getByText("API_BASE_URL")).toBeTruthy();
    expect(within(panel).getByText("withheld")).toBeTruthy();
    expect(within(panel).getByText(/plain values are withheld/)).toBeTruthy();
  });
});
