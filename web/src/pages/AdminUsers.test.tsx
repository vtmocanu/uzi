// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminUsers } from "./AdminUsers";
import { api, ApiError, type SettingsResponse, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listUsers: vi.fn(),
      setUserActive: vi.fn(),
      setUserJudgeEnabled: vi.fn(),
      setUserCIAutofixEnabled: vi.fn(),
      // PRD #69 M4: AdminUsers now reads judge_enforce_all to grey the per-user judge
      // control under enforced mode.
      getSettings: vi.fn(),
      // PRD #3989: the per-row reset link action.
      createResetLink: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

// settingsResp is the minimal admin settings the AdminUsers load path reads:
// judge_enforce_all and (PRD #914) the instance-wide ci_autofix_enabled kill-switch are
// consumed, so the rest is a thin cast. ciAutofixEnabled defaults to on, matching the
// server default, so existing tests keep the per-user CI-autofix control live.
function settingsResp(enforceAll: boolean, ciAutofixEnabled = true): SettingsResponse {
  return {
    settings: {
      judge_enforce_all: enforceAll ? "true" : "false",
      ci_autofix_enabled: ciAutofixEnabled ? "true" : "false",
    },
  } as unknown as SettingsResponse;
}

function aUser(over: Partial<User> = {}): User {
  return {
    id: "u2",
    email: "mira@uzi.local",
    display_name: "Alex Rivera",
    is_admin: false,
    is_active: true,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: false,
    attribution_enabled: true,
    ephemeral_workers_enabled: false,
    ephemeral_docker_enabled: false,
    wait_on_limit: false,
    notify_early_limit_reset: false,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    judge_anthropic_bind_mode: "default",
    created_at: "2026-01-01T00:00:00Z",
    last_login: null,
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { id: "admin1", is_admin: true } } as unknown as ReturnType<typeof useAuth>);
  // Default: enforced mode off, so the per-user judge control is live. Tests that
  // exercise enforced mode override this.
  mockApi.getSettings.mockResolvedValue(settingsResp(false));
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AdminUsers judge toggle (PRD #46 M4)", () => {
  it("shows the per-user judge state and toggles it on", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ judge_enabled: false })] });
    mockApi.setUserJudgeEnabled.mockResolvedValue({ user: aUser({ judge_enabled: true }) });

    render(
    <MemoryRouter>
      <AdminUsers />
    </MemoryRouter>,
  );

    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    // Cells: 0 Email, 1 Name, 2 Role, 3 Status, 4 Judge, 5 CI autofix, 6 Last
    // login, 7 Action. Scope to the Judge cell so the CI-autofix column's own
    // On/Off badge and Enable/Disable button (PRD #71 M1) don't collide.
    const judgeCell = () => within(within(row).getAllByRole("cell")[4]);
    // Off state + an Enable action.
    expect(judgeCell().getByText("Off")).toBeTruthy();
    fireEvent.click(judgeCell().getByText("Enable"));

    // The flag is set on the TARGET's own id (never the actor's, never the body).
    expect(mockApi.setUserJudgeEnabled).toHaveBeenCalledWith("u2", true);
    await waitFor(() => expect(judgeCell().getByText("On")).toBeTruthy());
    // The action flips to Disable once enabled.
    expect(judgeCell().getByText("Disable")).toBeTruthy();
  });

  it("toggles a judge-enabled user off", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ judge_enabled: true })] });
    mockApi.setUserJudgeEnabled.mockResolvedValue({ user: aUser({ judge_enabled: false }) });

    render(
    <MemoryRouter>
      <AdminUsers />
    </MemoryRouter>,
  );
    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    // Judge is cell index 4; scope to it so the CI-autofix column's own
    // Off/Enable (PRD #71 M1) doesn't match a bare within(row) text query.
    const judgeCell = () => within(within(row).getAllByRole("cell")[4]);
    fireEvent.click(judgeCell().getByText("Disable"));
    expect(mockApi.setUserJudgeEnabled).toHaveBeenCalledWith("u2", false);
    await waitFor(() => expect(judgeCell().getByText("Off")).toBeTruthy());
  });
});

describe("AdminUsers judge enforced mode (PRD #69 M4)", () => {
  it("greys and disables the per-user judge control when enforce-all is on", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ judge_enabled: false })] });
    mockApi.getSettings.mockResolvedValue(settingsResp(true));

    render(
      <MemoryRouter>
        <AdminUsers />
      </MemoryRouter>,
    );
    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    const judgeCell = () => within(within(row).getAllByRole("cell")[4]);
    // The Enable button is present but disabled (inert), and the cell annotates why.
    const btn = judgeCell().getByText("Enable") as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(judgeCell().getByText(/Inert/).textContent).toContain("enforced mode");
  });
});

// PRD #914 M2/M3: the admin CI-autofix column is now tri-state (boolean | null). A null
// (inherit) user collapses to On/Disable, exactly as an explicit true does; only an
// explicit false shows Off/Enable. Every existing fixture pins false, so this null→On
// flip was untested — a plain-bool / `?? false` read would render null as Off/Enable and
// still pass the old suite, so this test is what pins the inherit-renders-on semantics.
describe("AdminUsers CI autofix tri-state display (PRD #914 M3)", () => {
  it("shows a null (inherit) user's CI-autofix badge as On and the action as Disable", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ ci_autofix_enabled: null })] });

    render(
      <MemoryRouter>
        <AdminUsers />
      </MemoryRouter>,
    );

    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    // CI autofix is cell index 5 (0 Email, 1 Name, 2 Role, 3 Status, 4 Judge, 5 CI
    // autofix, 6 Last login, 7 Action). Scope to it so the Judge column's own On/Off
    // badge and Enable/Disable button don't collide.
    const ciCell = () => within(within(row).getAllByRole("cell")[5]);
    // Inherit (null) collapses to On/Disable, NOT Off/Enable — a `?? false` or
    // plain-bool read would show Off/Enable here.
    expect(ciCell().getByText("On")).toBeTruthy();
    expect(ciCell().getByText("Disable")).toBeTruthy();
    expect(ciCell().queryByText("Off")).toBeNull();
    expect(ciCell().queryByText("Enable")).toBeNull();
  });
});

// PRD #914 M1: the instance-wide ci_autofix_enabled kill-switch dominates the per-user
// column. When it is "false" every user's CI-autofix is inert regardless of their own
// flag, so the badge must read Off and the toggle must be disabled — otherwise clicking
// "Disable" on an inherit (null) user would persist an explicit false that survives the
// global being re-enabled (the sticky-false trap this fix removes).
describe("AdminUsers CI autofix instance kill-switch (PRD #914 M1)", () => {
  it("shows Off and disables the toggle for a null user when the instance switch is off", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ ci_autofix_enabled: null })] });
    mockApi.getSettings.mockResolvedValue(settingsResp(false, false));

    render(
      <MemoryRouter>
        <AdminUsers />
      </MemoryRouter>,
    );

    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    // CI autofix is cell index 5 (0 Email, 1 Name, 2 Role, 3 Status, 4 Judge, 5 CI
    // autofix, 6 Last login, 7 Action). Scope to it so the Judge column's own inert
    // note and On/Off badge don't collide.
    const ciCell = () => within(within(row).getAllByRole("cell")[5]);
    // Wait for the best-effort settings read to land, then assert the inert state.
    await waitFor(() => expect(ciCell().getByText(/Inert/).textContent).toContain("kill-switch"));
    expect(ciCell().getByText("Off")).toBeTruthy();
    expect(ciCell().queryByText("On")).toBeNull();
    const btn = ciCell().getByText("Enable") as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it("keeps the null user On with an enabled toggle when the instance switch is on", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ ci_autofix_enabled: null })] });
    mockApi.getSettings.mockResolvedValue(settingsResp(false, true));

    render(
      <MemoryRouter>
        <AdminUsers />
      </MemoryRouter>,
    );

    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    const ciCell = () => within(within(row).getAllByRole("cell")[5]);
    // Inherit (null) collapses to On/Disable; the switch being on leaves it live.
    await waitFor(() => expect(ciCell().getByText("On")).toBeTruthy());
    const btn = ciCell().getByText("Disable") as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    expect(ciCell().queryByText(/Inert/)).toBeNull();
  });
});

describe("AdminUsers name fallback (PRD #54)", () => {
  it("renders an em-dash for an empty-string display_name (?? missed it)", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ display_name: "" })] });
    render(
    <MemoryRouter>
      <AdminUsers />
    </MemoryRouter>,
  );
    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    // Cell 0 is the email, cell 1 the name column: an empty name shows "—".
    expect(within(row).getAllByRole("cell")[1].textContent).toBe("—");
  });
});

describe("AdminUsers reset link (PRD #3989)", () => {
  function renderPage() {
    return render(
      <MemoryRouter>
        <AdminUsers />
      </MemoryRouter>,
    );
  }
  const rowOf = async (email: string) => (await screen.findByText(email)).closest("tr")!;

  it("offers Reset link only on active rows that are not the signed-in admin", async () => {
    mockApi.listUsers.mockResolvedValue({
      users: [
        aUser({ id: "admin1", email: "me@uzi.local", is_admin: true }),
        aUser({ id: "u2", email: "mira@uzi.local" }),
        aUser({ id: "u3", email: "gone@uzi.local", is_active: false }),
      ],
    });
    renderPage();
    expect(within(await rowOf("mira@uzi.local")).getByRole("button", { name: "Reset link" })).toBeTruthy();
    // Positive control above; the self and inactive rows carry no such action.
    expect(within(await rowOf("me@uzi.local")).queryByRole("button", { name: "Reset link" })).toBeNull();
    expect(within(await rowOf("gone@uzi.local")).queryByRole("button", { name: "Reset link" })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Reset link" })).toHaveLength(1);
  });

  it("confirms what the reset destroys before issuing anything", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser()] });
    renderPage();
    fireEvent.click(within(await rowOf("mira@uzi.local")).getByRole("button", { name: "Reset link" }));

    const dialog = screen.getByRole("dialog");
    const text = dialog.textContent ?? "";
    expect(text).toMatch(/model credentials \(Anthropic and OpenAI tokens, Codex logins\) and their vault contents are\s+permanently deleted/);
    expect(text).toMatch(/All their browser sessions end/);
    expect(text).toMatch(/A CLI login they already approved but have not finished is refused/);
    expect(text).toMatch(/one\s+still waiting for approval stays pending, but their old sessions can no longer approve it/);
    // The dialog must not promise that waiting CLI logins are cancelled: the backend keeps them pending.
    expect(text).not.toMatch(/cancelled/);
    expect(text).toMatch(/queued Codex runs will fail; they must start new ones/);
    // A non-admin target gets no takeover warning.
    expect(within(dialog).queryByRole("note")).toBeNull();
    // Nothing is issued until the admin confirms.
    expect(mockApi.createResetLink).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(mockApi.createResetLink).not.toHaveBeenCalled();
  });

  it("warns that an admin target's link hands over an admin account", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ is_admin: true })] });
    renderPage();
    fireEvent.click(within(await rowOf("mira@uzi.local")).getByRole("button", { name: "Reset link" }));
    const note = within(screen.getByRole("dialog")).getByRole("note");
    expect(note.textContent).toMatch(/takes over an admin account/);
    expect(note.textContent).toMatch(/only with that\s+person, over a trusted channel/);
  });

  it("shows the link once, with its expiry and kind, and drops it on close", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser()] });
    const token = "tok_" + "A".repeat(39);
    mockApi.createResetLink.mockResolvedValue({
      token,
      expires_at: "2026-11-06T17:00:00Z",
      kind: "password",
      target_is_admin: false,
    });
    renderPage();
    const row = await rowOf("mira@uzi.local");
    fireEvent.click(within(row).getByRole("button", { name: "Reset link" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Create reset link" }));

    await waitFor(() => expect(mockApi.createResetLink).toHaveBeenCalledWith("u2"));
    const link = await screen.findByLabelText("Reset link");
    // The token rides the fragment of the web's own origin, never a path or query.
    expect(link.textContent).toBe(`${window.location.origin}/reset#${token}`);
    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).toMatch(/only once/);
    expect(dialog.textContent).toMatch(/cannot be retrieved\s+again/);
    expect(dialog.textContent).toContain(new Date("2026-11-06T17:00:00Z").toLocaleString());
    expect(dialog.textContent).toMatch(/Password and vault/);
    expect(within(dialog).getByRole("button", { name: "Copy" })).toBeTruthy();

    fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.body.textContent).not.toContain(token);

    // Reopening starts at the confirmation again; the old link is not retrievable.
    fireEvent.click(within(row).getByRole("button", { name: "Reset link" }));
    expect(screen.getByRole("dialog").textContent).not.toContain(token);
    expect(within(screen.getByRole("dialog")).getByRole("button", { name: "Create reset link" })).toBeTruthy();
    expect(mockApi.createResetLink).toHaveBeenCalledTimes(1);
  });

  it("renders a vault_only link and the admin snapshot from the issue response", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser()] });
    mockApi.createResetLink.mockResolvedValue({
      token: "t",
      expires_at: "2026-11-06T17:00:00Z",
      kind: "vault_only",
      target_is_admin: true,
    });
    renderPage();
    fireEvent.click(within(await rowOf("mira@uzi.local")).getByRole("button", { name: "Reset link" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Create reset link" }));
    await screen.findByLabelText("Reset link");
    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).toMatch(/Vault only \(single sign-on account\)/);
    expect(within(dialog).getByRole("note").textContent).toMatch(/takes over an admin account/);
  });

  it("surfaces a server refusal inside the dialog", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser()] });
    mockApi.createResetLink.mockRejectedValue(new ApiError(400, "the seed admin cannot be reset with a link"));
    renderPage();
    fireEvent.click(within(await rowOf("mira@uzi.local")).getByRole("button", { name: "Reset link" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Create reset link" }));
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveProperty(
      "textContent",
      "the seed admin cannot be reset with a link",
    );
    expect(screen.queryByLabelText("Reset link")).toBeNull();
  });
});
