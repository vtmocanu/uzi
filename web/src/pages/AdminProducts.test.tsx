// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminProducts } from "./AdminProducts";
import { api, type AdminProductToken, type Product, type User } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminListProducts: vi.fn(),
      adminListProductTokens: vi.fn(),
      adminCreateProduct: vi.fn(),
      adminUpdateProduct: vi.fn(),
      adminDeleteProduct: vi.fn(),
      adminRevokeProductToken: vi.fn(),
      // AdminShell's health pip self-fetches; it never settles here, so the pip stays off.
      getAdminHealth: vi.fn(() => new Promise(() => {})),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const NOW = Date.now();
const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();
// Assembled at runtime: no token-shaped literal in tracked source.
const CLS = "uz" + "p_";

function aProduct(over: Partial<Product> = {}): Product {
  return {
    id: "prod-a",
    name: "Helpdesk assistant",
    description: "Opens jobs from tickets.",
    enabled: true,
    deleted_at: null,
    created_at: daysAgo(30),
    active_token_count: 3,
    ...over,
  };
}

function aToken(over: Partial<AdminProductToken> = {}): AdminProductToken {
  return {
    id: "pt1",
    product_id: "prod-a",
    product_name: "Helpdesk assistant",
    name: "support-prod",
    token_prefix: CLS + "4c1e",
    scopes: ["jobs:run"],
    revoked: false,
    created_at: daysAgo(10),
    last_used_at: daysAgo(1),
    last_used_ip: "10.20.3.14",
    expires_at: null,
    user_id: "u-mira",
    owner_email: "mira@uzi.local",
    ...over,
  };
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/admin/products"]}>
      <AdminProducts />
    </MemoryRouter>,
  );
}

const productCard = (name: string) => screen.findByRole("region", { name });

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u-admin", is_admin: true } as User,
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.adminListProducts.mockResolvedValue({ products: [aProduct()] });
  mockApi.adminListProductTokens.mockResolvedValue({ tokens: [aToken()] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AdminProducts list", () => {
  it("shows each product's state and active count, with its tokens grouped beneath it", async () => {
    mockApi.adminListProducts.mockResolvedValue({
      products: [
        aProduct(),
        aProduct({ id: "prod-b", name: "Metrics export", enabled: false, active_token_count: 0 }),
        aProduct({ id: "prod-c", name: "Legacy importer", enabled: false, deleted_at: daysAgo(3), active_token_count: 0 }),
      ],
    });
    mockApi.adminListProductTokens.mockResolvedValue({
      tokens: [
        aToken(),
        aToken({ id: "pt2", product_id: "prod-c", name: "importer", owner_email: "dan@uzi.local", revoked: true }),
      ],
    });
    renderPage();

    const helpdesk = await productCard("Helpdesk assistant");
    expect(within(helpdesk).getByRole("switch", { name: "Disable Helpdesk assistant" }).getAttribute("aria-checked")).toBe("true");
    expect(within(helpdesk).getByText("3 active tokens")).toBeTruthy();
    expect(within(helpdesk).getByText("mira@uzi.local")).toBeTruthy();
    expect(within(helpdesk).getByText("support-prod")).toBeTruthy();
    expect(within(helpdesk).getByText(CLS + "4c1e…")).toBeTruthy();
    expect(within(helpdesk).getByText("jobs:run")).toBeTruthy();
    expect(within(helpdesk).getByText(/from 10\.20\.3\.14/)).toBeTruthy();
    expect(within(helpdesk).getByText("never expires")).toBeTruthy();

    const legacy = await productCard("Legacy importer");
    expect(within(legacy).getByText(/^Deleted /)).toBeTruthy();
    expect(within(legacy).getByText("dan@uzi.local")).toBeTruthy();
    expect(within(legacy).getByText("revoked")).toBeTruthy();
    // A deleted product has no controls: no toggle and no delete.
    expect(within(legacy).queryByRole("switch")).toBeNull();

    const metrics = await productCard("Metrics export");
    expect(within(metrics).getByRole("switch", { name: "Enable Metrics export" }).getAttribute("aria-checked")).toBe("false");
    expect(within(metrics).getByText("No tokens minted for this product.")).toBeTruthy();
  });

  it("renders untrusted product text as text, not markup", async () => {
    const hostile = '<img src=x onerror="alert(1)">';
    mockApi.adminListProducts.mockResolvedValue({ products: [aProduct({ description: hostile })] });
    const { container } = renderPage();
    expect(await screen.findByText(hostile)).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
  });
});

describe("AdminProducts writes", () => {
  it("creates a product and shows a 409 duplicate-name message from the server", async () => {
    mockApi.adminCreateProduct.mockRejectedValueOnce(
      new ApiError(409, "a product with this name already exists"),
    );
    renderPage();
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Helpdesk assistant" } });
    fireEvent.click(screen.getByRole("button", { name: "Register product" }));
    expect((await screen.findByRole("alert")).textContent).toBe("a product with this name already exists");

    mockApi.adminCreateProduct.mockResolvedValueOnce({ product: aProduct({ id: "new", name: "CRM sync" }) });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "CRM sync" } });
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "Syncs." } });
    fireEvent.click(screen.getByRole("button", { name: "Register product" }));
    await waitFor(() => expect(mockApi.adminCreateProduct).toHaveBeenLastCalledWith("CRM sync", "Syncs."));
  });

  it("disables an enabled product through the toggle", async () => {
    mockApi.adminUpdateProduct.mockResolvedValue({ product: aProduct({ enabled: false }) });
    renderPage();
    fireEvent.click(await screen.findByRole("switch", { name: "Disable Helpdesk assistant" }));
    await waitFor(() => expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", { enabled: false }));
  });

  it("confirms delete with the number of active tokens it stops, then reports stopped_token_count", async () => {
    mockApi.adminDeleteProduct.mockResolvedValue({
      product: aProduct({ enabled: false, deleted_at: new Date().toISOString() }),
      stopped_token_count: 3,
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    expect(mockApi.adminDeleteProduct).not.toHaveBeenCalled();
    const confirm = screen.getByRole("group", { name: "Confirm deleting Helpdesk assistant" });
    expect(within(confirm).getByText(/This stops 3 active tokens on their next request\./)).toBeTruthy();

    fireEvent.click(within(confirm).getByRole("button", { name: "Delete product" }));
    await waitFor(() => expect(mockApi.adminDeleteProduct).toHaveBeenCalledWith("prod-a"));
    expect(await screen.findByText("Deleted “Helpdesk assistant”. Stopped 3 active tokens.")).toBeTruthy();
  });

  it("says a disabled product's delete stops 0 tokens", async () => {
    mockApi.adminListProducts.mockResolvedValue({
      products: [aProduct({ enabled: false, active_token_count: 2 })],
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = screen.getByRole("group", { name: "Confirm deleting Helpdesk assistant" });
    expect(within(confirm).getByText(/This stops 0 active tokens: it is disabled/)).toBeTruthy();
  });

  it("admin revoke calls the revoke endpoint for that token", async () => {
    mockApi.adminRevokeProductToken.mockResolvedValue(null);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Revoke support-prod" }));
    await waitFor(() => expect(mockApi.adminRevokeProductToken).toHaveBeenCalledWith("pt1"));
    await waitFor(() => expect(mockApi.adminListProductTokens).toHaveBeenCalledTimes(2));
  });
});
