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
import { ForgeSettings } from "./ForgeSettings";
import { api, ApiError, type ForgeConnection } from "../lib/api";

// Only the api object is swapped; ApiError and types stay real so the page's
// `instanceof ApiError` checks match what the mocked methods throw.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      forgeConfig: vi.fn(),
      listConnections: vi.fn(),
      createConnection: vi.fn(),
      verifyConnection: vi.fn(),
      updateConnection: vi.fn(),
      deleteConnection: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const connection: ForgeConnection = {
  id: "conn-1",
  forge_type: "gitlab",
  base_url: "https://gitlab.example.com",
  bot_username: "uzi-bot",
  bot_forge_user_id: 4021,
  human_username: null,
  created_at: "2026-06-01T00:00:00Z",
  last_verified_at: "2026-07-01T00:00:00Z",
};

beforeEach(() => {
  mockApi.forgeConfig.mockResolvedValue({
    allowed_base_urls: ["https://gitlab.example.com"],
    forge_types: ["gitlab"],
  });
  mockApi.listConnections.mockResolvedValue({ connections: [connection] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const usernameField = () =>
  screen.getByLabelText(/Your username on/i) as HTMLInputElement;

describe("ForgeSettings — forge identity mapping (PRD #19 M3)", () => {
  it("shows the identity section seeded from the stored username", async () => {
    mockApi.listConnections.mockResolvedValue({
      connections: [{ ...connection, human_username: "vlad" }],
    });
    render(
      <MemoryRouter>
        <ForgeSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(usernameField().value).toBe("vlad"));
    // Explains the autopilot attribution purpose.
    expect(screen.getByText(/attribute an/i)).toBeTruthy();
  });

  it("saves an edited username", async () => {
    mockApi.updateConnection.mockResolvedValue({
      connection: { ...connection, human_username: "vlad" },
    });
    render(
      <MemoryRouter>
        <ForgeSettings />
      </MemoryRouter>,
    );
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "vlad" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    await waitFor(() =>
      expect(mockApi.updateConnection).toHaveBeenCalledWith("conn-1", "vlad"),
    );
    expect(
      await screen.findByText(/Saved your forge username: vlad/i),
    ).toBeTruthy();
  });

  it("surfaces the verified-or-warned warning while still saving", async () => {
    mockApi.updateConnection.mockResolvedValue({
      connection: { ...connection, human_username: "ghost" },
      warning:
        "Saved, but no forge account with this username was found — double-check it.",
    });
    render(
      <MemoryRouter>
        <ForgeSettings />
      </MemoryRouter>,
    );
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "ghost" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    // The warning Alert (role="status") carries the not-found message.
    expect(
      await screen.findByText(/no forge account with this username/i),
    ).toBeTruthy();
  });

  it("hard-rejects a username already mapped by another user (409)", async () => {
    mockApi.updateConnection.mockRejectedValue(
      new ApiError(
        409,
        "that forge username is already mapped by another user on this host",
      ),
    );
    render(
      <MemoryRouter>
        <ForgeSettings />
      </MemoryRouter>,
    );
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "taken" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    expect(
      await screen.findByText(/already mapped by another user/i),
    ).toBeTruthy();
  });
});
