// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminUsers } from "./AdminUsers";
import { api, type SettingsResponse, type User } from "../lib/api";
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
