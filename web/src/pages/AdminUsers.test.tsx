// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AdminUsers } from "./AdminUsers";
import { api, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { listUsers: vi.fn(), setUserActive: vi.fn(), setUserJudgeEnabled: vi.fn() },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function aUser(over: Partial<User> = {}): User {
  return {
    id: "u2",
    email: "mira@uzi.local",
    display_name: "Mira Ionescu",
    is_admin: false,
    is_active: true,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: false,
    wait_on_limit: false,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    created_at: "2026-01-01T00:00:00Z",
    last_login: null,
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { id: "admin1", is_admin: true } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AdminUsers judge toggle (PRD #46 M4)", () => {
  it("shows the per-user judge state and toggles it on", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ judge_enabled: false })] });
    mockApi.setUserJudgeEnabled.mockResolvedValue({ user: aUser({ judge_enabled: true }) });

    render(<AdminUsers />);

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

    render(<AdminUsers />);
    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    // Judge is cell index 4; scope to it so the CI-autofix column's own
    // Off/Enable (PRD #71 M1) doesn't match a bare within(row) text query.
    const judgeCell = () => within(within(row).getAllByRole("cell")[4]);
    fireEvent.click(judgeCell().getByText("Disable"));
    expect(mockApi.setUserJudgeEnabled).toHaveBeenCalledWith("u2", false);
    await waitFor(() => expect(judgeCell().getByText("Off")).toBeTruthy());
  });
});

describe("AdminUsers name fallback (PRD #54)", () => {
  it("renders an em-dash for an empty-string display_name (?? missed it)", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [aUser({ display_name: "" })] });
    render(<AdminUsers />);
    const row = (await screen.findByText("mira@uzi.local")).closest("tr")!;
    // Cell 0 is the email, cell 1 the name column: an empty name shows "—".
    expect(within(row).getAllByRole("cell")[1].textContent).toBe("—");
  });
});
