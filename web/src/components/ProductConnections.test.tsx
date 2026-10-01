// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ProductConnectionsPanel } from "./ProductConnections";
import { api, type AdminOAuthConnection, type Product } from "../lib/api";
import { ApiError } from "../lib/apiError";

// PRD #1910 M5: the admin Connections panel on a product card.

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminListProductConnections: vi.fn(),
      adminRevokeOAuthConnection: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);
const NOW = Date.now();
const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();

const product: Product = {
  id: "prod-a",
  name: "Helpdesk assistant",
  description: "",
  enabled: true,
  deleted_at: null,
  created_at: daysAgo(60),
  active_token_count: 0,
  allowed_job_types: [],
};

const conn = (over: Partial<AdminOAuthConnection> = {}): AdminOAuthConnection => ({
  id: "g1",
  user_id: "u1",
  owner_email: "ann@example.test",
  scopes: ["jobs:run"],
  connected_at: daysAgo(3),
  created_at: daysAgo(30),
  last_used_at: null,
  ...over,
});

async function openPanel() {
  const utils = render(<ProductConnectionsPanel product={product} />);
  const details = utils.container.querySelector("details") as HTMLDetailsElement;
  details.open = true;
  fireEvent(details, new Event("toggle"));
  await waitFor(() => expect(mockApi.adminListProductConnections).toHaveBeenCalledWith("prod-a"));
  return utils;
}

beforeEach(() => {
  mockApi.adminListProductConnections.mockResolvedValue({ connections: [conn()], truncated: false });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ProductConnectionsPanel", () => {
  it("loads nothing until opened", () => {
    render(<ProductConnectionsPanel product={product} />);
    expect(mockApi.adminListProductConnections).not.toHaveBeenCalled();
  });

  it("lists who connected the product, with scopes in plain words and last use", async () => {
    await openPanel();
    expect(await screen.findByText("ann@example.test")).toBeTruthy();
    expect(screen.getByText("Run jobs")).toBeTruthy();
    expect(screen.getByText(/never used/)).toBeTruthy();
    expect(screen.getByText("1 connected")).toBeTruthy();
  });

  it("revokes after a confirm: the API is called with the connection id and the row goes", async () => {
    mockApi.adminListProductConnections
      .mockResolvedValueOnce({ connections: [conn()], truncated: false })
      .mockResolvedValueOnce({ connections: [], truncated: false });
    mockApi.adminRevokeOAuthConnection.mockResolvedValue(null);
    await openPanel();

    fireEvent.click(await screen.findByRole("button", { name: "Revoke the connection of ann@example.test" }));
    const confirm = screen.getByRole("group", { name: "Confirm revoking the connection of ann@example.test" });
    expect(document.activeElement).toBe(confirm);
    expect(mockApi.adminRevokeOAuthConnection).not.toHaveBeenCalled();
    fireEvent.click(within(confirm).getByRole("button", { name: "Revoke connection" }));

    await waitFor(() => expect(mockApi.adminRevokeOAuthConnection).toHaveBeenCalledWith("g1"));
    expect(await screen.findByText("No one has connected this product.")).toBeTruthy();
    expect(screen.getByRole("status").textContent).toMatch(/Revoked the connection of ann@example.test/);
  });

  it("shows the cut notice when the server truncated the list, and an error when the revoke fails", async () => {
    mockApi.adminListProductConnections.mockResolvedValue({ connections: [conn()], truncated: true });
    mockApi.adminRevokeOAuthConnection.mockRejectedValue(new ApiError(500, "internal error"));
    await openPanel();
    expect(await screen.findByText(/Showing the 1 most recent connections/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Revoke the connection of ann@example.test" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke connection" }));
    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(screen.getByText("ann@example.test")).toBeTruthy();
  });

  it("renders the user's email as plain text, never as markup", async () => {
    const hostile = '<img src=x onerror="alert(1)">@example.test';
    mockApi.adminListProductConnections.mockResolvedValue({ connections: [conn({ owner_email: hostile })], truncated: false });
    const { container } = await openPanel();
    expect(await screen.findByText(hostile)).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
  });
});
