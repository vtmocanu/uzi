// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ProductEgressProfilesPanel } from "./ProductEgressProfiles";
import { api, type EgressProfile, type Product, type ProductEgressProfile } from "../lib/api";
import { ApiError } from "../lib/apiError";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminListProductEgressProfiles: vi.fn(),
      adminAllowProductEgressProfile: vi.fn(),
      adminDisallowProductEgressProfile: vi.fn(),
      adminListEgressProfiles: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const product: Product = {
  id: "prod-a",
  name: "Helpdesk assistant",
  description: "",
  enabled: true,
  deleted_at: null,
  created_at: new Date().toISOString(),
  active_token_count: 0,
  allowed_job_types: [],
};

const row = (name: string, description?: string): ProductEgressProfile => ({
  name,
  ...(description ? { description } : {}),
  created_at: new Date().toISOString(),
});

const profile = (name: string): EgressProfile =>
  ({ id: `id-${name}`, name, description: "", hosts: [], multi_publisher_override: [], warnings: [] }) as unknown as EgressProfile;

async function openPanel(p: Product = product) {
  const utils = render(<ProductEgressProfilesPanel product={p} />);
  const details = utils.container.querySelector("details") as HTMLDetailsElement;
  details.open = true;
  fireEvent(details, new Event("toggle"));
  await waitFor(() => expect(mockApi.adminListProductEgressProfiles).toHaveBeenCalledWith("prod-a"));
  return utils;
}

beforeEach(() => {
  mockApi.adminListProductEgressProfiles.mockResolvedValue({
    egress_profiles: [row("kernel-docs", "Kernel docs")],
  });
  mockApi.adminListEgressProfiles.mockResolvedValue({
    egress_profiles: [profile("kernel-docs"), profile("model cards/v2"), profile("vendor-x")],
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ProductEgressProfilesPanel", () => {
  it("loads nothing until opened", () => {
    render(<ProductEgressProfilesPanel product={product} />);
    expect(mockApi.adminListProductEgressProfiles).not.toHaveBeenCalled();
    expect(mockApi.adminListEgressProfiles).not.toHaveBeenCalled();
  });

  it("lists the allowed lists and explains what they gate", async () => {
    await openPanel();
    expect(await screen.findByText("kernel-docs")).toBeTruthy();
    expect(screen.getByText("Kernel docs")).toBeTruthy();
    expect(screen.getByText(/may only name these lists/)).toBeTruthy();
    expect(screen.getByText(/affects jobs created afterwards/)).toBeTruthy();
  });

  it("offers only lists not yet allowed", async () => {
    await openPanel();
    const select = (await screen.findByLabelText("Allow another site list")) as HTMLSelectElement;
    await waitFor(() => expect(select.options.length).toBe(3));
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "model cards/v2", "vendor-x"]);
  });

  it("adds a list with PUT by name and shows the returned list", async () => {
    mockApi.adminAllowProductEgressProfile.mockResolvedValue({
      egress_profiles: [row("kernel-docs", "Kernel docs"), row("model cards/v2", "Cards")],
    });
    await openPanel();
    const select = (await screen.findByLabelText("Allow another site list")) as HTMLSelectElement;
    await waitFor(() => expect(select.options.length).toBe(3));
    fireEvent.change(select, { target: { value: "model cards/v2" } });
    fireEvent.click(screen.getByRole("button", { name: "Allow" }));

    await waitFor(() => expect(mockApi.adminAllowProductEgressProfile).toHaveBeenCalledWith("prod-a", "model cards/v2"));
    expect(await screen.findByText("Cards")).toBeTruthy();
    // The added list left the add choices: only vendor-x remains.
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "vendor-x"]);
  });

  it("removes a list with DELETE and drops its row", async () => {
    mockApi.adminDisallowProductEgressProfile.mockResolvedValue(null);
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Remove kernel-docs" }));

    await waitFor(() => expect(mockApi.adminDisallowProductEgressProfile).toHaveBeenCalledWith("prod-a", "kernel-docs"));
    expect(await screen.findByText(/No site lists allowed/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Remove kernel-docs" })).toBeNull();
  });

  it("surfaces a failed write and keeps the row", async () => {
    mockApi.adminDisallowProductEgressProfile.mockRejectedValue(new ApiError(404, "egress profile is not allowed for this product"));
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Remove kernel-docs" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("not allowed for this product");
    expect(screen.getByText("kernel-docs")).toBeTruthy();
  });

  it("surfaces a failed load", async () => {
    mockApi.adminListProductEgressProfiles.mockRejectedValue(new ApiError(500, "boom"));
    await openPanel();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("boom");
  });

  it("is read-only for a deleted product: rows show, no write controls, no add choices fetched", async () => {
    const deleted: Product = { ...product, deleted_at: new Date().toISOString() };
    await openPanel(deleted);
    expect(await screen.findByText("kernel-docs")).toBeTruthy();
    expect(screen.getByText(/This product is deleted/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();
    expect(screen.queryByLabelText("Allow another site list")).toBeNull();
    expect(mockApi.adminListEgressProfiles).not.toHaveBeenCalled();
  });

  it("scopes the empty state to the section", async () => {
    mockApi.adminListProductEgressProfiles.mockResolvedValue({ egress_profiles: [] });
    const { container } = await openPanel();
    const msg = await within(container).findByText(/No site lists allowed/);
    expect(msg).toBeTruthy();
  });
});
