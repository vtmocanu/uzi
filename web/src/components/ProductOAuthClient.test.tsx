// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ProductOAuthPanel } from "./ProductOAuthClient";
import { api, type Product, type ProductOAuthClient } from "../lib/api";
import { ApiError } from "../lib/apiError";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminSetProductOAuth: vi.fn(),
      adminRotateProductClientSecret: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const notClient: ProductOAuthClient = {
  redirect_uris: [],
  scopes: [],
  has_secret: false,
  secret_prefix: "",
  rotated_at: null,
  is_client: false,
};

const fullClient: ProductOAuthClient = {
  redirect_uris: ["https://app.example.com/cb", "http://127.0.0.1:8123/cb"],
  scopes: ["jobs:run"],
  has_secret: true,
  secret_prefix: "uzs_Qm4x",
  rotated_at: "2026-09-20T10:00:00Z",
  is_client: true,
};

const product = (oauth?: ProductOAuthClient, over: Partial<Product> = {}): Product => ({
  id: "prod-a",
  name: "Helpdesk assistant",
  description: "",
  enabled: true,
  deleted_at: null,
  created_at: "2026-08-01T10:00:00Z",
  active_token_count: 0,
  allowed_job_types: [],
  ...(oauth ? { oauth_client: oauth } : {}),
  ...over,
});

const onChanged = vi.fn();

// The panel renders its body only once the disclosure is open, as the sibling panels do.
function renderPanel(p: Product) {
  const utils = render(<ProductOAuthPanel product={p} onChanged={onChanged} />);
  const details = utils.container.querySelector("details") as HTMLDetailsElement;
  details.open = true;
  fireEvent(details, new Event("toggle"));
  return utils;
}

beforeEach(() => {
  onChanged.mockReset().mockResolvedValue(undefined);
  mockApi.adminSetProductOAuth.mockReset();
  mockApi.adminRotateProductClientSecret.mockReset();
});
afterEach(cleanup);

describe("ProductOAuthPanel", () => {
  it("shows a registered client's status, URIs, scopes and secret prefix", () => {
    renderPanel(product(fullClient));
    expect(screen.getByText("Client")).toBeTruthy();
    expect(screen.getAllByText("https://app.example.com/cb").length).toBeGreaterThan(0);
    expect(screen.getByText("Run jobs", { selector: "dd" })).toBeTruthy();
    expect(screen.getByText(/uzs_Qm4x/)).toBeTruthy();
  });

  it("says what a half-registered product is missing", () => {
    renderPanel(product({ ...notClient, redirect_uris: ["https://a.example.com/cb"], scopes: ["jobs:read"] }));
    expect(screen.getByText(/still needs a client secret/)).toBeTruthy();
  });

  it("treats a product from an api without oauth_client as not a client", () => {
    renderPanel(product());
    expect(screen.getByText("Not a client")).toBeTruthy();
    expect(screen.getByText(/still needs a redirect URI, a scope, a client secret/)).toBeTruthy();
  });

  it("saves the URIs (one per line) and the checked scopes together", async () => {
    mockApi.adminSetProductOAuth.mockResolvedValue({ product: product(fullClient) });
    renderPanel(product(notClient));
    fireEvent.change(screen.getByLabelText(/Redirect URIs/), {
      target: { value: "https://a.example.com/cb\n\n http://127.0.0.1:9000/cb \n" },
    });
    fireEvent.click(screen.getByLabelText(/Run jobs/));
    fireEvent.click(screen.getByRole("button", { name: "Save OAuth settings" }));
    await waitFor(() =>
      expect(mockApi.adminSetProductOAuth).toHaveBeenCalledWith(
        "prod-a",
        ["https://a.example.com/cb", "http://127.0.0.1:9000/cb"],
        ["jobs:run"],
      ),
    );
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(screen.getByText("OAuth client settings saved.")).toBeTruthy();
  });

  it("refuses a bad URI before sending and disables save", () => {
    renderPanel(product(notClient));
    fireEvent.change(screen.getByLabelText(/Redirect URIs/), { target: { value: "http://localhost:8080/cb" } });
    fireEvent.click(screen.getByLabelText(/Run jobs/));
    expect(screen.getByText(/never localhost/)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save OAuth settings" }) as HTMLButtonElement).disabled).toBe(true);
    expect(mockApi.adminSetProductOAuth).not.toHaveBeenCalled();
  });

  it("needs a scope when there are URIs", () => {
    renderPanel(product(notClient));
    fireEvent.change(screen.getByLabelText(/Redirect URIs/), { target: { value: "https://a.example.com/cb" } });
    expect(screen.getByText("Pick at least one scope.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save OAuth settings" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("clears both lists when the URIs are emptied", async () => {
    mockApi.adminSetProductOAuth.mockResolvedValue({ product: product(notClient) });
    renderPanel(product(fullClient));
    fireEvent.change(screen.getByLabelText(/Redirect URIs/), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save OAuth settings" }));
    await waitFor(() => expect(mockApi.adminSetProductOAuth).toHaveBeenCalledWith("prod-a", [], []));
  });

  it("surfaces the server's refusal", async () => {
    mockApi.adminSetProductOAuth.mockRejectedValue(new ApiError(409, "product is deleted"));
    renderPanel(product(notClient));
    fireEvent.change(screen.getByLabelText(/Redirect URIs/), { target: { value: "https://a.example.com/cb" } });
    fireEvent.click(screen.getByLabelText(/Read jobs/));
    fireEvent.click(screen.getByRole("button", { name: "Save OAuth settings" }));
    expect(await screen.findByText("product is deleted")).toBeTruthy();
  });

  it("rotating the secret asks first, then shows the new secret once", async () => {
    const secret = "uzs_" + "A".repeat(43);
    mockApi.adminRotateProductClientSecret.mockResolvedValue({ client_secret: secret, product: product(fullClient) });
    renderPanel(product(fullClient));
    fireEvent.click(screen.getByRole("button", { name: "Rotate secret" }));
    expect(mockApi.adminRotateProductClientSecret).not.toHaveBeenCalled();
    fireEvent.click(screen.getAllByRole("button", { name: "Rotate secret" })[0]);
    expect(await screen.findByText(secret)).toBeTruthy();
    expect(screen.getByText(/once and never again/)).toBeTruthy();
    expect(onChanged).toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText(secret)).toBeNull();
  });

  it("creates the first secret without a confirm", async () => {
    mockApi.adminRotateProductClientSecret.mockResolvedValue({ client_secret: "uzs_" + "B".repeat(43), product: product(notClient) });
    renderPanel(product(notClient));
    fireEvent.click(screen.getByRole("button", { name: "Create client secret" }));
    await waitFor(() => expect(mockApi.adminRotateProductClientSecret).toHaveBeenCalledWith("prod-a"));
  });

  it("is read-only for a deleted product", () => {
    renderPanel(product(fullClient, { deleted_at: "2026-09-01T00:00:00Z", enabled: false }));
    expect(screen.getByText("A deleted product cannot change.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save OAuth settings" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Rotate secret" })).toBeNull();
  });
});
