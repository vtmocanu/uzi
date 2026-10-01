// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OAuthConnections } from "./OAuthConnections";
import { AccessSettings } from "../pages/AccessSettings";
import { api, type OAuthConnection, type User } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { useAuth } from "../auth/AuthContext";

// PRD #1910 M5: Settings → Access → Connected products.

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listOAuthConnections: vi.fn(),
      revokeOAuthConnection: vi.fn(),
      listProductTokens: vi.fn(),
      listMintableProducts: vi.fn(),
      listCliTokens: vi.fn(),
      revokeAllCliTokens: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const NOW = Date.now();
const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();

function aConnection(over: Partial<OAuthConnection> = {}): OAuthConnection {
  return {
    id: "g1",
    product_id: "prod-a",
    product_name: "Helpdesk assistant",
    scopes: ["jobs:run", "jobs:read"],
    connected_at: daysAgo(3),
    created_at: daysAgo(30),
    last_used_at: daysAgo(1),
    refresh_issued_at: daysAgo(3),
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1", is_admin: false } as User,
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.listOAuthConnections.mockResolvedValue({ connections: [aConnection()] });
  mockApi.listProductTokens.mockResolvedValue({ truncated: false, tokens: [] });
  mockApi.listMintableProducts.mockResolvedValue({ products: [] });
  mockApi.listCliTokens.mockResolvedValue({ tokens: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("OAuthConnections", () => {
  it("lists a connection with its product, scopes in plain words, connected date and last use", async () => {
    render(<OAuthConnections />);
    expect(await screen.findByText("Helpdesk assistant")).toBeTruthy();
    expect(screen.getByText("Run jobs")).toBeTruthy();
    expect(screen.getByText("Read jobs and their results")).toBeTruthy();
    expect(screen.getByText(new RegExp(`connected ${new Date(daysAgo(3)).toLocaleDateString()}`))).toBeTruthy();
    expect(screen.getByText(/last used/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Revoke Helpdesk assistant" })).toBeTruthy();
  });

  it("says never used when the connection has no use yet", async () => {
    mockApi.listOAuthConnections.mockResolvedValue({ connections: [aConnection({ last_used_at: null })] });
    render(<OAuthConnections />);
    expect(await screen.findByText(/never used/)).toBeTruthy();
  });

  it("lists a connection whose access tokens have all expired and revokes it: the API is called and the row goes", async () => {
    // The wire row carries no token state at all: a connection is listed while it is live.
    mockApi.listOAuthConnections
      .mockResolvedValueOnce({ connections: [aConnection({ last_used_at: daysAgo(40) })] })
      .mockResolvedValueOnce({ connections: [] });
    mockApi.revokeOAuthConnection.mockResolvedValue(null);
    render(<OAuthConnections />);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke Helpdesk assistant" }));
    const confirm = screen.getByRole("group", { name: "Confirm revoking Helpdesk assistant" });
    // The warning, not the destructive button, takes focus when the confirm arms.
    expect(document.activeElement).toBe(confirm);
    expect(mockApi.revokeOAuthConnection).not.toHaveBeenCalled();
    fireEvent.click(within(confirm).getByRole("button", { name: "Revoke connection" }));

    await waitFor(() => expect(mockApi.revokeOAuthConnection).toHaveBeenCalledWith("g1"));
    expect(await screen.findByText("No connected products")).toBeTruthy();
    expect(screen.queryByText("Helpdesk assistant")).toBeNull();
    expect(mockApi.listOAuthConnections).toHaveBeenCalledTimes(2);
  });

  it("cancels the confirm without calling the API", async () => {
    render(<OAuthConnections />);
    fireEvent.click(await screen.findByRole("button", { name: "Revoke Helpdesk assistant" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("group", { name: /Confirm revoking/ })).toBeNull();
    expect(mockApi.revokeOAuthConnection).not.toHaveBeenCalled();
  });

  it("shows the empty state when nothing is connected", async () => {
    mockApi.listOAuthConnections.mockResolvedValue({ connections: [] });
    render(<OAuthConnections />);
    expect(await screen.findByText("No connected products")).toBeTruthy();
    expect(screen.getByText(/approve it, it shows up here/)).toBeTruthy();
  });

  it("renders a product name as plain text, never as markup", async () => {
    const hostile = '<img src=x onerror="alert(1)"><b>Evil</b>';
    mockApi.listOAuthConnections.mockResolvedValue({ connections: [aConnection({ product_name: hostile })] });
    const { container } = render(<OAuthConnections />);
    expect(await screen.findByText(hostile)).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
  });

  it("keeps the row and shows the error when the revoke fails, and drops a stale row on a 404", async () => {
    mockApi.revokeOAuthConnection.mockRejectedValueOnce(new ApiError(500, "internal error"));
    render(<OAuthConnections />);
    fireEvent.click(await screen.findByRole("button", { name: "Revoke Helpdesk assistant" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke connection" }));
    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(screen.getByText("Helpdesk assistant")).toBeTruthy();

    mockApi.revokeOAuthConnection.mockRejectedValueOnce(new ApiError(404, "connection not found"));
    mockApi.listOAuthConnections.mockResolvedValue({ connections: [] });
    fireEvent.click(screen.getByRole("button", { name: "Revoke connection" }));
    expect(await screen.findByText("No connected products")).toBeTruthy();
  });

  it("shows only the error when the first load fails, never an empty state", async () => {
    mockApi.listOAuthConnections.mockRejectedValue(new ApiError(500, "database unavailable"));
    render(<OAuthConnections />);
    expect(await screen.findByText("database unavailable")).toBeTruthy();
    expect(screen.queryByText("No connected products")).toBeNull();
  });
});

describe("Settings → Access: Connected products and Revoke all", () => {
  const renderPage = () =>
    render(
      <MemoryRouter>
        <AccessSettings />
      </MemoryRouter>,
    );

  it("refreshes the Revoke all count after one connection is revoked", async () => {
    mockApi.listOAuthConnections
      .mockResolvedValueOnce({
        connections: [aConnection(), aConnection({ id: "g2", product_id: "prod-b", product_name: "Metrics export" })],
      })
      .mockResolvedValue({ connections: [aConnection({ id: "g2", product_id: "prod-b", product_name: "Metrics export" })] });
    mockApi.revokeOAuthConnection.mockResolvedValue(null);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Revoke all" }));
    expect(screen.getByText(/^Revoke all 2 connected products\?/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    fireEvent.click(await screen.findByRole("button", { name: "Revoke Helpdesk assistant" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke connection" }));
    await waitFor(() => expect(mockApi.revokeOAuthConnection).toHaveBeenCalledWith("g1"));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Revoke Helpdesk assistant" })).toBeNull());

    fireEvent.click(screen.getByRole("button", { name: "Revoke all" }));
    expect(screen.getByText(/^Revoke all 1 connected product\?/)).toBeTruthy();
  });
});
