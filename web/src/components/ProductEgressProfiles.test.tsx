// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ProductEgressProfilesPanel } from "./ProductEgressProfiles";
import { api, type EgressProfile, type Product, type ProductEgressProfile } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { MemoryRouter } from "react-router-dom";

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
  const utils = render(
    <MemoryRouter>
      <ProductEgressProfilesPanel product={p} />
    </MemoryRouter>,
  );
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
    render(
      <MemoryRouter>
        <ProductEgressProfilesPanel product={product} />
      </MemoryRouter>,
    );
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

  it("surfaces a failed (non-404) write and keeps the row", async () => {
    mockApi.adminDisallowProductEgressProfile.mockRejectedValue(new ApiError(500, "remove failed"));
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Remove kernel-docs" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("remove failed");
    expect(screen.getByRole("button", { name: "Remove kernel-docs" })).toBeTruthy();
  });

  it("says the site lists are unavailable when the list of all site lists fails to load", async () => {
    mockApi.adminListEgressProfiles.mockRejectedValue(new ApiError(500, "lists down"));
    await openPanel();
    expect(await screen.findByText("Site lists unavailable")).toBeTruthy();
    expect(screen.getByText("lists down")).toBeTruthy();
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
    expect(screen.getByText(/may only name these lists/)).toBeTruthy();
    expect(screen.queryByText(/affects jobs created afterwards/)).toBeNull();
    expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();
    expect(screen.queryByLabelText("Allow another site list")).toBeNull();
    expect(mockApi.adminListEgressProfiles).not.toHaveBeenCalled();
  });

  it("scopes the empty state to the section", async () => {
    mockApi.adminListProductEgressProfiles.mockResolvedValue({ egress_profiles: [] });
    const { container } = await openPanel();
    const section = container.querySelector("details") as HTMLElement;
    const msg = await within(section).findByText(/No site lists allowed/);
    expect(msg).toBeTruthy();
    expect(within(section).getByLabelText("Allow a site list")).toBeTruthy();
  });

  it("keeps the post-write rows across a close and reopen whose refetch fails", async () => {
    mockApi.adminDisallowProductEgressProfile.mockResolvedValue(null);
    const { container } = await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Remove kernel-docs" }));
    expect(await screen.findByText(/No site lists allowed/)).toBeTruthy();

    const details = container.querySelector("details") as HTMLDetailsElement;
    details.open = false;
    fireEvent(details, new Event("toggle"));
    mockApi.adminListProductEgressProfiles.mockRejectedValue(new ApiError(500, "refetch boom"));
    details.open = true;
    fireEvent(details, new Event("toggle"));

    await waitFor(() => expect(mockApi.adminListProductEgressProfiles).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/No site lists allowed/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Remove kernel-docs" })).toBeNull();
  });

  it("moves focus to the select after an add", async () => {
    mockApi.adminAllowProductEgressProfile.mockResolvedValue({
      egress_profiles: [row("kernel-docs"), row("vendor-x")],
    });
    await openPanel();
    const select = (await screen.findByLabelText("Allow another site list")) as HTMLSelectElement;
    await waitFor(() => expect(select.options.length).toBe(3));
    fireEvent.change(select, { target: { value: "vendor-x" } });
    fireEvent.click(screen.getByRole("button", { name: "Allow" }));
    await screen.findByRole("button", { name: "Remove vendor-x" });
    await waitFor(() => expect(document.activeElement).toBe(select));
  });

  it("moves focus to the next row's Remove button after a remove, then to the select when none remain", async () => {
    mockApi.adminListProductEgressProfiles.mockResolvedValue({
      egress_profiles: [row("kernel-docs"), row("vendor-x")],
    });
    mockApi.adminDisallowProductEgressProfile.mockResolvedValue(null);
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Remove kernel-docs" }));
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Remove vendor-x" })),
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove vendor-x" }));
    const select = await screen.findByLabelText("Allow a site list");
    await waitFor(() => expect(document.activeElement).toBe(select));
  });

  it("drops a row the api says is already gone and shows the error", async () => {
    mockApi.adminDisallowProductEgressProfile.mockRejectedValue(
      new ApiError(404, "this product is not allowed to use that egress profile"),
    );
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Remove kernel-docs" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("not allowed to use that egress profile");
    expect(screen.queryByRole("button", { name: "Remove kernel-docs" })).toBeNull();
    expect(screen.getByText(/No site lists allowed/)).toBeTruthy();
  });

  it("says no site lists exist yet, and links to the admin page, when the instance has none", async () => {
    mockApi.adminListEgressProfiles.mockResolvedValue({ egress_profiles: [] });
    await openPanel();
    const select = (await screen.findByLabelText("Allow another site list")) as HTMLSelectElement;
    await waitFor(() => expect(select.options[0].textContent).toBe("No site lists exist yet"));
    expect(select.disabled).toBe(true);
    expect(screen.getByRole("link", { name: "Site lists" }).getAttribute("href")).toBe("/admin/egress-profiles");
  });

  it("shows a loading placeholder, not the exhausted message, while the all-lists request is pending", async () => {
    mockApi.adminListEgressProfiles.mockReturnValue(new Promise(() => {}));
    await openPanel();
    const select = (await screen.findByLabelText("Allow another site list")) as HTMLSelectElement;
    expect(select.options[0].textContent).toBe("Loading site lists…");
    expect(screen.queryByText("No lists left to allow")).toBeNull();
  });

  it("says no lists are left once every list is allowed", async () => {
    mockApi.adminListEgressProfiles.mockResolvedValue({ egress_profiles: [profile("kernel-docs")] });
    await openPanel();
    const select = (await screen.findByLabelText("Allow another site list")) as HTMLSelectElement;
    await waitFor(() => expect(select.options[0].textContent).toBe("No lists left to allow"));
  });
});
