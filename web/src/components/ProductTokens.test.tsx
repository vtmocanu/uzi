// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ProductTokens } from "./ProductTokens";
import { AccessSettings } from "../pages/AccessSettings";
import { api, type MintableProduct, type ProductToken, type User } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listProductTokens: vi.fn(),
      listMintableProducts: vi.fn(),
      createProductToken: vi.fn(),
      revokeProductToken: vi.fn(),
      listCliTokens: vi.fn(),
      createCliToken: vi.fn(),
      revokeCliToken: vi.fn(),
      revokeAllCliTokens: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const NOW = Date.now();
const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();

// Token-shaped values are assembled at runtime (source hygiene: no complete
// uzp_ token literal in tracked source).
const CLS = "uz" + "p_";
const MINTED = CLS + "f00dfeedf00dfeedf00dfeedf00dfeed";

const PRODUCTS: MintableProduct[] = [
  { id: "prod-a", name: "Helpdesk assistant", description: "Opens jobs from tickets." },
  { id: "prod-b", name: "Metrics export", description: "Reads results." },
];

function aToken(over: Partial<ProductToken> = {}): ProductToken {
  return {
    id: "pt1",
    product_id: "prod-a",
    product_name: "Helpdesk assistant",
    name: "support-prod",
    token_prefix: CLS + "4c1e",
    scopes: ["jobs:run", "jobs:read"],
    revoked: false,
    created_at: daysAgo(10),
    last_used_at: daysAgo(1),
    last_used_ip: "10.20.3.14",
    expires_at: daysAgo(-80),
    ...over,
  };
}

function renderCard() {
  return render(
    <MemoryRouter>
      <ProductTokens />
    </MemoryRouter>,
  );
}

async function fillAndMint(productName = "Helpdesk assistant", name = "prod") {
  fireEvent.change(await screen.findByLabelText("Product"), {
    target: { value: PRODUCTS.find((p) => p.name === productName)!.id },
  });
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: name } });
  fireEvent.click(screen.getByRole("button", { name: "Create product token" }));
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1", is_admin: false } as User,
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.listProductTokens.mockResolvedValue({ tokens: [] });
  mockApi.listMintableProducts.mockResolvedValue({ products: PRODUCTS });
  mockApi.listCliTokens.mockResolvedValue({ tokens: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ProductTokens mint form", () => {
  it("defaults the expiry to 90 days and sends the chosen product, name, scopes and expiry", async () => {
    mockApi.createProductToken.mockResolvedValue({
      token: MINTED,
      product_token: aToken({ id: "new", name: "prod" }),
    });
    renderCard();

    const expiry = (await screen.findByLabelText("Expires after")) as HTMLSelectElement;
    expect(expiry.value).toBe("90d");
    fireEvent.click(screen.getByRole("checkbox", { name: /Run jobs/ }));
    await fillAndMint("Metrics export", "prod");

    await waitFor(() =>
      expect(mockApi.createProductToken).toHaveBeenCalledWith({
        product_id: "prod-b",
        name: "prod",
        scopes: ["jobs:read", "jobs:run"],
        expiry: "90d",
      }),
    );
  });

  it("requires at least one scope before it can mint", async () => {
    renderCard();
    fireEvent.change(await screen.findByLabelText("Product"), { target: { value: "prod-a" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "prod" } });
    const create = screen.getByRole("button", { name: "Create product token" }) as HTMLButtonElement;
    expect(create.disabled).toBe(false);

    fireEvent.click(screen.getByRole("checkbox", { name: /Read jobs/ }));
    expect(create.disabled).toBe(true);
    expect(screen.getByText("Pick at least one permission.")).toBeTruthy();
  });

  it("shows the server's 409 cap message verbatim", async () => {
    const capMsg =
      "you already have 10 active tokens for this product; revoke one before minting another";
    mockApi.createProductToken.mockRejectedValue(new ApiError(409, capMsg));
    renderCard();
    await fillAndMint();
    expect((await screen.findByRole("alert")).textContent).toBe(capMsg);
  });

  it("explains the empty state when there is no product to mint for", async () => {
    mockApi.listMintableProducts.mockResolvedValue({ products: [] });
    renderCard();
    expect(await screen.findByText("No products to connect yet")).toBeTruthy();
    expect(screen.getByText(/Admin → Products/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Create product token" })).toBeNull();
  });
});

describe("ProductTokens mint is show-once", () => {
  it("shows the token once with a copy button and warning, and drops it on Done", async () => {
    mockApi.createProductToken.mockResolvedValue({
      token: MINTED,
      product_token: aToken({ id: "new", name: "prod" }),
    });
    renderCard();
    await fillAndMint();

    const status = await screen.findByRole("status");
    expect(within(status).getByText(MINTED)).toBeTruthy();
    expect(within(status).getByText(/once and never again/)).toBeTruthy();
    expect(within(status).getByText(/won’t see this value again/)).toBeTruthy();
    expect(within(status).getByRole("button", { name: "Copy" })).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(status));

    fireEvent.click(within(status).getByRole("button", { name: "Done" }));
    // Present a moment ago (asserted above), gone now.
    expect(screen.queryByText(MINTED)).toBeNull();
  });

  it("does not bring the token back on a fresh mount (a page refresh)", async () => {
    mockApi.createProductToken.mockResolvedValue({
      token: MINTED,
      product_token: aToken({ id: "new", name: "prod" }),
    });
    const first = renderCard();
    await fillAndMint();
    expect(await screen.findByText(MINTED)).toBeTruthy();
    first.unmount();

    // The server now lists the minted row (metadata only); the value is nowhere.
    mockApi.listProductTokens.mockResolvedValue({ tokens: [aToken({ id: "new", name: "prod" })] });
    renderCard();
    expect(await screen.findByText("prod")).toBeTruthy();
    expect(screen.queryByText(MINTED)).toBeNull();
  });
});

describe("ProductTokens list", () => {
  it("renders product, name, prefix, scopes, last used, IP and expiry", async () => {
    mockApi.listProductTokens.mockResolvedValue({ tokens: [aToken()] });
    renderCard();
    const row = (await screen.findByText("support-prod")).closest("li")!;
    expect(within(row).getByText(CLS + "4c1e…")).toBeTruthy();
    expect(within(row).getByText("for Helpdesk assistant")).toBeTruthy();
    expect(within(row).getByText("jobs:run")).toBeTruthy();
    expect(within(row).getByText("jobs:read")).toBeTruthy();
    expect(within(row).getByText(/last used/)).toBeTruthy();
    expect(within(row).getByText(/from 10\.20\.3\.14/)).toBeTruthy();
    expect(within(row).getByText(/expires /)).toBeTruthy();
  });

  it("marks revoked and expired tokens and offers Revoke only on the active one", async () => {
    mockApi.listProductTokens.mockResolvedValue({
      tokens: [
        aToken({ id: "live", name: "live" }),
        aToken({ id: "gone", name: "gone", revoked: true }),
        aToken({ id: "old", name: "old", expires_at: daysAgo(2) }),
        aToken({ id: "forever", name: "forever", expires_at: null, revoked: true }),
      ],
    });
    renderCard();
    await screen.findByText("live");
    expect(screen.getAllByText("revoked")).toHaveLength(2);
    expect(screen.getAllByText("expired")).toHaveLength(1);
    expect(screen.getByText(/never expires/)).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /^Revoke / }).map((b) => b.getAttribute("aria-label"))).toEqual([
      "Revoke live",
    ]);
  });

  it("flags an active token whose product is no longer mintable (disabled or deleted)", async () => {
    mockApi.listProductTokens.mockResolvedValue({
      tokens: [
        aToken({ id: "ok", name: "fine" }),
        aToken({ id: "off", name: "stranded", product_id: "prod-gone", product_name: "Old tool" }),
      ],
    });
    renderCard();
    const stranded = (await screen.findByText("stranded")).closest("li")!;
    const badge = within(stranded).getByText("product unavailable");
    expect(badge.getAttribute("title")).toMatch(/disabled or deleted/);
    expect(screen.getAllByText("product unavailable")).toHaveLength(1);
  });

  it("revoke calls DELETE for that token and reloads", async () => {
    mockApi.listProductTokens.mockResolvedValue({ tokens: [aToken({ id: "pt9", name: "ci" })] });
    mockApi.revokeProductToken.mockResolvedValue(null);
    renderCard();
    fireEvent.click(await screen.findByRole("button", { name: "Revoke ci" }));
    await waitFor(() => expect(mockApi.revokeProductToken).toHaveBeenCalledWith("pt9"));
    await waitFor(() => expect(mockApi.listProductTokens).toHaveBeenCalledTimes(2));
  });

  it("renders an untrusted token name as text, not markup", async () => {
    const hostile = '<img src=x onerror="alert(1)">';
    mockApi.listProductTokens.mockResolvedValue({ tokens: [aToken({ name: hostile })] });
    const { container } = renderCard();
    expect(await screen.findByText(hostile)).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
  });
});

describe("Settings → Access: Revoke all covers product tokens (PRD #1907 D8)", () => {
  it("calls revoke-all and refreshes the product-token list", async () => {
    mockApi.listProductTokens
      .mockResolvedValueOnce({ tokens: [aToken({ id: "p1", name: "helpdesk" })] })
      .mockResolvedValue({ tokens: [aToken({ id: "p1", name: "helpdesk", revoked: true })] });
    mockApi.revokeAllCliTokens.mockResolvedValue(null);
    render(
      <MemoryRouter>
        <AccessSettings />
      </MemoryRouter>,
    );

    // Only a product token is active, so the shared Revoke all names it.
    fireEvent.click(await screen.findByRole("button", { name: "Revoke all" }));
    expect(screen.getByText(/Revoke all 1 product token\?/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Revoke helpdesk" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Revoke all anyway" }));

    await waitFor(() => expect(mockApi.revokeAllCliTokens).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(mockApi.listProductTokens).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("revoked")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Revoke helpdesk" })).toBeNull();
  });
});
