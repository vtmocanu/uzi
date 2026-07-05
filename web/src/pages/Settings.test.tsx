// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Settings } from "./Settings";
import { api, ApiError, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSecrets: vi.fn(),
      putAnthropicToken: vi.fn(),
      deleteAnthropicToken: vi.fn(),
      setAutopilotEnabled: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const refresh = vi.fn();

const baseUser: User = {
  id: "u1",
  email: "vlad@uzi.local",
  display_name: "Vlad",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function mockAuth(user: User) {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh,
  });
}

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockAuth(baseUser);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const toggle = () =>
  screen.getByLabelText("Enable autopilot for my account") as HTMLInputElement;

describe("Settings — autopilot opt-in (PRD #19 M3, Decision 7)", () => {
  it("states plainly what autopilot does", () => {
    const { container } = render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const text = container.textContent ?? "";
    expect(text).toMatch(/unattended/i);
    expect(text).toMatch(/skips the pre-execution plan review/i);
    expect(text).toMatch(/your own Anthropic tokens/i);
    expect(text).toMatch(/merge-request review/i);
  });

  it("reflects the current opt-in state (off)", () => {
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    expect(toggle().checked).toBe(false);
  });

  it("reflects the current opt-in state (on)", () => {
    mockAuth({ ...baseUser, autopilot_enabled: true });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    expect(toggle().checked).toBe(true);
  });

  it("enabling calls the API and refreshes the session", async () => {
    mockApi.setAutopilotEnabled.mockResolvedValue({
      user: { ...baseUser, autopilot_enabled: true },
    });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    fireEvent.click(toggle());

    await waitFor(() =>
      expect(mockApi.setAutopilotEnabled).toHaveBeenCalledWith(true),
    );
    expect(refresh).toHaveBeenCalled();
  });

  it("surfaces an error and does not leave the toggle stuck", async () => {
    mockApi.setAutopilotEnabled.mockRejectedValue(
      new ApiError(500, "internal error"),
    );
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    fireEvent.click(toggle());

    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(toggle().disabled).toBe(false);
  });
});
