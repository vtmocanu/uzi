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
    allowed_job_types: [],
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
  mockApi.adminListProductTokens.mockResolvedValue({ truncated: false, tokens: [aToken()] });
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
      truncated: false,
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
    expect(within(helpdesk).getByText("Run jobs")).toBeTruthy();
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
    await waitFor(() => expect(mockApi.adminCreateProduct).toHaveBeenLastCalledWith("CRM sync", "Syncs.", []));
  });

  it("trims a trailing U+0085 as Go's TrimSpace does, instead of refusing it", async () => {
    mockApi.adminCreateProduct.mockResolvedValueOnce({ product: aProduct({ id: "new", name: "Acme" }) });
    renderPage();
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Acme\u0085" } });
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "\u0085Syncs.\u0085" } });
    expect(screen.queryByText(/can’t contain tabs, line breaks/)).toBeNull();
    const register = screen.getByRole("button", { name: "Register product" }) as HTMLButtonElement;
    expect(register.disabled).toBe(false);
    fireEvent.click(register);
    await waitFor(() => expect(mockApi.adminCreateProduct).toHaveBeenLastCalledWith("Acme", "Syncs.", []));
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

describe("AdminProducts allowed job types (PRD #1908)", () => {
  const researchIn = (el: HTMLElement) =>
    within(el).getByRole("checkbox", { name: /^Research/ }) as HTMLInputElement;

  it("registers with no job type by default (fail-closed) and says what that means", async () => {
    mockApi.adminCreateProduct.mockResolvedValueOnce({ product: aProduct({ id: "new", name: "CRM sync" }) });
    renderPage();
    const form = await screen.findByRole("form", { name: "Register a product" });
    expect(researchIn(form).checked).toBe(false);
    expect(within(form).getByText("None checked: this product cannot create jobs.")).toBeTruthy();
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "CRM sync" } });
    fireEvent.click(within(form).getByRole("button", { name: "Register product" }));
    await waitFor(() => expect(mockApi.adminCreateProduct).toHaveBeenLastCalledWith("CRM sync", "", []));
  });

  it("sends the checked types on register and resets the boxes after", async () => {
    mockApi.adminCreateProduct.mockResolvedValueOnce({ product: aProduct({ id: "new", name: "CRM sync" }) });
    renderPage();
    const form = await screen.findByRole("form", { name: "Register a product" });
    fireEvent.click(researchIn(form));
    expect(researchIn(form).checked).toBe(true);
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "CRM sync" } });
    fireEvent.click(within(form).getByRole("button", { name: "Register product" }));
    await waitFor(() =>
      expect(mockApi.adminCreateProduct).toHaveBeenLastCalledWith("CRM sync", "", ["research"]),
    );
    await waitFor(() => expect(researchIn(form).checked).toBe(false));
  });

  it("checking a type on a product PATCHes the whole new list", async () => {
    mockApi.adminUpdateProduct.mockResolvedValue({ product: aProduct({ allowed_job_types: ["research"] }) });
    renderPage();
    const card = await productCard("Helpdesk assistant");
    expect(researchIn(card).checked).toBe(false);
    fireEvent.click(researchIn(card));
    await waitFor(() =>
      expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", { allowed_job_types: ["research"] }),
    );
  });

  it("unchecking the last type sends [] (clear), not an omitted field", async () => {
    mockApi.adminListProducts.mockResolvedValue({ products: [aProduct({ allowed_job_types: ["research"] })] });
    mockApi.adminUpdateProduct.mockResolvedValue({ product: aProduct({ allowed_job_types: [] }) });
    renderPage();
    const card = await productCard("Helpdesk assistant");
    expect(researchIn(card).checked).toBe(true);
    fireEvent.click(researchIn(card));
    await waitFor(() =>
      expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", { allowed_job_types: [] }),
    );
  });

  it("keeps a stored type this build does not know when toggling a known one", async () => {
    mockApi.adminListProducts.mockResolvedValue({ products: [aProduct({ allowed_job_types: ["future_type"] })] });
    mockApi.adminUpdateProduct.mockResolvedValue({ product: aProduct() });
    renderPage();
    fireEvent.click(researchIn(await productCard("Helpdesk assistant")));
    await waitFor(() =>
      expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", {
        allowed_job_types: ["research", "future_type"],
      }),
    );
  });

  it("shows a deleted product's types as text, with no checkbox to change them", async () => {
    mockApi.adminListProducts.mockResolvedValue({
      products: [aProduct({ enabled: false, deleted_at: daysAgo(1), allowed_job_types: ["research"] })],
    });
    renderPage();
    const card = await productCard("Helpdesk assistant");
    expect(within(card).getByText("Job types: Research")).toBeTruthy();
    // Paired with the positive above, and the live-product tests find this checkbox by the
    // same query, so its absence here is not vacuous.
    expect(within(card).queryByRole("checkbox")).toBeNull();
  });
});

describe("AdminProducts register form (review item 1)", () => {
  it("takes the description on one line, since the server refuses any newline", async () => {
    renderPage();
    const desc = await screen.findByLabelText(/^Description/);
    expect(desc.tagName).toBe("INPUT");
  });

  it("flags a pasted tab or invisible character and blocks Register before the server 400s", async () => {
    renderPage();
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "CRM sync" } });
    const register = screen.getByRole("button", { name: "Register product" }) as HTMLButtonElement;
    expect(register.disabled).toBe(false);

    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "a\tb" } });
    expect(
      screen.getByText("Description can’t contain tabs, line breaks or invisible formatting characters."),
    ).toBeTruthy();
    expect(register.disabled).toBe(true);

    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "Syncs." } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "CRM\u202Esync" } });
    expect(
      screen.getByText("Name can’t contain tabs, line breaks or invisible formatting characters."),
    ).toBeTruthy();
    expect(register.disabled).toBe(true);
    fireEvent.click(register);
    expect(mockApi.adminCreateProduct).not.toHaveBeenCalled();
  });

  it("counts the description in bytes, not characters", async () => {
    renderPage();
    // 334 x U+20AC is 334 characters but 1002 bytes.
    fireEvent.change(await screen.findByLabelText(/^Description/), {
      target: { value: "\u20ac".repeat(334) },
    });
    expect(screen.getByText(/Description is 1002 bytes; the limit is 1000/)).toBeTruthy();
  });
});

describe("AdminProducts first-load failure (review item 2's twin)", () => {
  it("shows only the error, not a false No products registered", async () => {
    mockApi.adminListProductTokens.mockRejectedValue(new ApiError(500, "database unavailable"));
    renderPage();
    expect((await screen.findByRole("alert")).textContent).toBe("database unavailable");
    expect(screen.queryByText("No products registered")).toBeNull();
  });

  it("still shows the empty state on a successful empty load (control)", async () => {
    mockApi.adminListProducts.mockResolvedValue({ products: [] });
    mockApi.adminListProductTokens.mockResolvedValue({ truncated: false, tokens: [] });
    renderPage();
    expect(await screen.findByText("No products registered")).toBeTruthy();
  });
});

describe("AdminProducts toggle keeps focus while busy (review item 4)", () => {
  it("marks the switch aria-disabled, not natively disabled, and focus stays on it", async () => {
    let settle!: (v: { product: Product }) => void;
    mockApi.adminUpdateProduct.mockReturnValue(new Promise((r) => (settle = r)));
    renderPage();
    const sw = (await screen.findByRole("switch", { name: "Disable Helpdesk assistant" })) as HTMLButtonElement;
    sw.focus();
    fireEvent.click(sw);

    await waitFor(() => expect(sw.getAttribute("aria-disabled")).toBe("true"));
    expect(sw.disabled).toBe(false);
    expect(document.activeElement).toBe(sw);
    // A second click while in flight is ignored.
    fireEvent.click(sw);
    expect(mockApi.adminUpdateProduct).toHaveBeenCalledTimes(1);

    settle({ product: aProduct({ enabled: false }) });
    await waitFor(() => expect(sw.getAttribute("aria-disabled")).toBeNull());
  });
});

describe("AdminProducts truncated inventory", () => {
  it("names the number the server listed and does not claim a product has no tokens", async () => {
    mockApi.adminListProducts.mockResolvedValue({
      products: [aProduct(), aProduct({ id: "prod-b", name: "Metrics export", active_token_count: 4 })],
    });
    // The count comes from the rows returned, not a cap hard-coded in the client.
    mockApi.adminListProductTokens.mockResolvedValue({
      truncated: true,
      tokens: [aToken(), aToken({ id: "tok-2", name: "second" }), aToken({ id: "tok-3", name: "third" })],
    });
    renderPage();
    expect(
      await screen.findByText("Showing the first 3 tokens, active first; older tokens are not listed."),
    ).toBeTruthy();
    const metrics = await productCard("Metrics export");
    expect(
      within(metrics).getByText(
        "None of this product’s tokens are among the first 3 listed; its tokens may be beyond the list.",
      ),
    ).toBeTruthy();
    // Paired with the positive in "AdminProducts list", where the same card shape says it.
    expect(within(metrics).queryByText("No tokens minted for this product.")).toBeNull();
  });
});
