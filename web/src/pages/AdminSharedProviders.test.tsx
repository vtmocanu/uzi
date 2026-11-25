// @vitest-environment jsdom
// PRD #4324 M4: Admin → Shared providers. Pins keyless create, name/model-map edit with the
// endpoint shown read-only, retire, the delete confirmation's key and selection counts, and
// the 409 refusals (selected-as-source, name collision).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminSharedProviders } from "./AdminSharedProviders";
import { api, type AdminSharedProvider } from "../lib/api";
import { ApiError } from "../lib/apiError";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminListSharedProviders: vi.fn(),
      adminCreateSharedProvider: vi.fn(),
      adminPatchSharedProvider: vi.fn(),
      adminRetireSharedProvider: vi.fn(),
      adminDeleteSharedProvider: vi.fn(),
      listProviders: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);
const URL_A = "https://gw.example.com/anthropic";
const URL_B = "https://other.example.com/v1";

function shared(over: Partial<AdminSharedProvider> = {}): AdminSharedProvider {
  return {
    id: "s1",
    name: "platform-gateway",
    engine: "claude",
    retired: false,
    current_revision: 2,
    protocol: "anthropic_messages",
    base_url: URL_A,
    auth_style: "x_api_key",
    model_map: { haiku: { model: "gw-small", context_window: 32768 } },
    credential_mode: "per_user_key",
    selected_user_count: 0,
    attached_key_count: 3,
    ...over,
  };
}

function conflict(reason: string): ApiError {
  return new ApiError(409, "server text", { error: "server text", reason });
}

async function renderPage(rows: AdminSharedProvider[] = [shared()]) {
  mockApi.adminListSharedProviders.mockResolvedValue({ providers: rows });
  render(
    <MemoryRouter>
      <AdminSharedProviders />
    </MemoryRouter>,
  );
  if (rows.length) await screen.findByRole("list", { name: "Shared providers" });
  else await screen.findByText("No shared providers yet");
}

beforeEach(() => {
  mockApi.listProviders.mockResolvedValue({ enabled: true, allowed_base_urls: [URL_A, URL_B], providers: [] });
});
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("AdminSharedProviders — list", () => {
  it("shows the counts and the fixed endpoint and auth style without any key field", async () => {
    await renderPage([shared({ selected_user_count: 1, attached_key_count: 3 })]);
    expect(screen.getByTestId("selected-count-s1").textContent).toBe("Selected by 1 user");
    expect(screen.getByTestId("key-count-s1").textContent).toBe("3 personal keys attached");
    const fixed = within(screen.getByTestId("fixed-facts"));
    expect(fixed.getByText("Fixed at creation")).toBeTruthy();
    expect(fixed.getByText(URL_A)).toBeTruthy();
    expect(fixed.getByText("x-api-key")).toBeTruthy();
    expect(screen.queryByLabelText(/key/i)).toBeNull();
  });

  it("renders an untrusted name as plain text", async () => {
    await renderPage([shared({ name: "<img src=x onerror=alert(1)>" })]);
    expect(screen.getByRole("heading", { name: "<img src=x onerror=alert(1)>" })).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("AdminSharedProviders — create", () => {
  it("creates a keyless definition from the allowlist and reloads", async () => {
    mockApi.adminCreateSharedProvider.mockResolvedValue({ provider: shared({ id: "s2", name: "new-gw" }) });
    await renderPage([]);
    fireEvent.click(screen.getByRole("button", { name: "New shared provider" }));
    const form = within(screen.getByRole("form", { name: "New shared provider" }));
    expect(form.queryByLabelText(/key/i)).toBeNull();
    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "  new-gw " } });
    fireEvent.change(form.getByLabelText("Base URL"), { target: { value: URL_B } });
    fireEvent.change(form.getByLabelText("Auth style"), { target: { value: "x_api_key" } });
    expect(form.getByTestId("auth-advice").textContent).toMatch(/forward each user's key to another host\. Bearer is recommended/);
    fireEvent.change(form.getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    fireEvent.change(form.getByLabelText("Haiku context window"), { target: { value: "32768" } });
    fireEvent.click(form.getByRole("button", { name: "Create shared provider" }));
    await waitFor(() =>
      expect(mockApi.adminCreateSharedProvider).toHaveBeenCalledWith({
        name: "new-gw",
        base_url: URL_B,
        auth_style: "x_api_key",
        model_map: { haiku: { model: "gw-small", context_window: 32768 } },
      }),
    );
    expect(await screen.findByText(/Shared provider new-gw created/)).toBeTruthy();
    expect(mockApi.adminListSharedProviders).toHaveBeenCalledTimes(2);
  });

  it("validates locally before calling the server", async () => {
    await renderPage([]);
    fireEvent.click(screen.getByRole("button", { name: "New shared provider" }));
    const form = within(screen.getByRole("form", { name: "New shared provider" }));
    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "gw" } });
    fireEvent.click(form.getByRole("button", { name: "Create shared provider" }));
    expect((await form.findByRole("alert")).textContent).toBe("Pick a base URL.");
    expect(mockApi.adminCreateSharedProvider).not.toHaveBeenCalled();
  });

  it("explains a 409 name collision inside the form", async () => {
    mockApi.adminCreateSharedProvider.mockRejectedValue(conflict("provider_name_taken"));
    await renderPage([]);
    fireEvent.click(screen.getByRole("button", { name: "New shared provider" }));
    const form = within(screen.getByRole("form", { name: "New shared provider" }));
    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "Platform-Gateway" } });
    fireEvent.change(form.getByLabelText("Base URL"), { target: { value: URL_A } });
    fireEvent.change(form.getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    fireEvent.click(form.getByRole("button", { name: "Create shared provider" }));
    expect((await form.findByRole("alert")).textContent).toMatch(/A shared provider with that name already exists\. Shared names are unique regardless of case/);
  });

  it("offers no create when custom providers are off", async () => {
    mockApi.listProviders.mockResolvedValue({ enabled: false, allowed_base_urls: [], providers: [] });
    await renderPage([]);
    expect(screen.getByText(/UZI_PROVIDER_ALLOWED_BASE_URLS/)).toBeTruthy();
    expect((screen.getByRole("button", { name: "New shared provider" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("AdminSharedProviders — edit", () => {
  it("edits name and model map, with the endpoint read-only, sending only what changed", async () => {
    mockApi.adminPatchSharedProvider.mockResolvedValue({ provider: shared({ name: "renamed" }) });
    await renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Edit platform-gateway" }));
    const form = within(screen.getByRole("form", { name: "Edit platform-gateway" }));
    expect(form.queryByLabelText("Base URL")).toBeNull();
    expect(form.queryByLabelText("Auth style")).toBeNull();
    expect(within(form.getByTestId("fixed-facts")).getByText(URL_A)).toBeTruthy();
    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "renamed" } });
    fireEvent.click(form.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.adminPatchSharedProvider).toHaveBeenCalledWith("s1", { name: "renamed" }));
    expect(await screen.findByText("Changes to renamed saved.")).toBeTruthy();
  });

  it("sends a model-map-only change without the name", async () => {
    mockApi.adminPatchSharedProvider.mockResolvedValue({ provider: shared() });
    await renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Edit platform-gateway" }));
    const form = within(screen.getByRole("form", { name: "Edit platform-gateway" }));
    fireEvent.change(form.getByLabelText("Sonnet model"), { target: { value: "gw-large" } });
    fireEvent.click(form.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(mockApi.adminPatchSharedProvider).toHaveBeenCalledWith("s1", {
        model_map: { haiku: { model: "gw-small", context_window: 32768 }, sonnet: { model: "gw-large" } },
      }),
    );
  });

  it("explains a 409 name collision on rename", async () => {
    mockApi.adminPatchSharedProvider.mockRejectedValue(conflict("provider_name_taken"));
    await renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Edit platform-gateway" }));
    const form = within(screen.getByRole("form", { name: "Edit platform-gateway" }));
    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "taken" } });
    fireEvent.click(form.getByRole("button", { name: "Save changes" }));
    expect((await form.findByRole("alert")).textContent).toMatch(/A shared provider with that name already exists/);
  });

  it("offers no edit or retire on a retired definition, only delete", async () => {
    await renderPage([shared({ retired: true })]);
    expect(screen.getByText("Retired")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit platform-gateway" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Retire platform-gateway" })).toBeNull();
    expect(screen.getByRole("button", { name: "Delete platform-gateway" })).toBeTruthy();
  });
});

describe("AdminSharedProviders — retire", () => {
  it("asks for confirmation, explains the effect, then retires", async () => {
    mockApi.adminRetireSharedProvider.mockResolvedValue({ provider: shared({ retired: true }) });
    await renderPage([shared({ selected_user_count: 2 })]);
    fireEvent.click(screen.getByRole("button", { name: "Retire platform-gateway" }));
    expect(mockApi.adminRetireSharedProvider).not.toHaveBeenCalled();
    const box = within(screen.getByRole("group", { name: "Confirm retiring platform-gateway" }));
    expect(box.getByText(/2 users keep it selected but cannot use it/)).toBeTruthy();
    expect(box.getByText(/runs already started keep their binding/)).toBeTruthy();
    expect(document.activeElement).toBe(box.getByRole("button", { name: "Cancel" }));
    fireEvent.click(box.getByRole("button", { name: "Retire definition" }));
    await waitFor(() => expect(mockApi.adminRetireSharedProvider).toHaveBeenCalledWith("s1"));
    expect(await screen.findByText("platform-gateway retired.")).toBeTruthy();
  });
});

describe("AdminSharedProviders — delete", () => {
  it("warns how many keys go with it, then reports the deleted key count", async () => {
    mockApi.adminDeleteSharedProvider.mockResolvedValue({ deleted: true, deleted_key_count: 3 });
    await renderPage([shared({ selected_user_count: 0, attached_key_count: 3 })]);
    fireEvent.click(screen.getByRole("button", { name: "Delete platform-gateway" }));
    expect(mockApi.adminDeleteSharedProvider).not.toHaveBeenCalled();
    const group = screen.getByRole("group", { name: "Confirm deleting platform-gateway" });
    const box = within(group);
    expect(box.getByTestId("delete-keys-s1").textContent).toMatch(/This also deletes 3 personal keys users attached to it/);
    expect(box.getByTestId("delete-selected-s1").textContent).toBe("No user has it selected as their Claude source.");
    const confirm = box.getByRole("button", { name: "Delete definition and keys" });
    // Focus starts on the safe choice, so an Enter right after opening cannot delete.
    expect(document.activeElement).toBe(box.getByRole("button", { name: "Cancel" }));
    fireEvent.click(confirm);
    await waitFor(() => expect(mockApi.adminDeleteSharedProvider).toHaveBeenCalledWith("s1"));
    expect(await screen.findByText("platform-gateway deleted, with 3 attached keys.")).toBeTruthy();
  });

  it("shows the selected-user count and refuses to confirm while anyone has it selected", async () => {
    await renderPage([shared({ selected_user_count: 2, attached_key_count: 1 })]);
    fireEvent.click(screen.getByRole("button", { name: "Delete platform-gateway" }));
    const box = within(screen.getByRole("group", { name: "Confirm deleting platform-gateway" }));
    // The refusal leads; the confirm box never asks a question it will refuse to act on.
    const lead = box.getByTestId("delete-selected-s1");
    expect(lead.textContent).toMatch(/^platform-gateway can't be deleted yet: 2 users have it selected as their Claude source\./);
    expect(box.queryByText(/Delete platform-gateway\?/)).toBeNull();
    expect(lead.compareDocumentPosition(box.getByTestId("delete-keys-s1")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(box.getByTestId("delete-keys-s1").textContent).toMatch(/would also delete 1 personal key users attached/);
    expect((box.getByRole("button", { name: "Delete definition and keys" }) as HTMLButtonElement).disabled).toBe(true);
    expect(document.activeElement).toBe(box.getByRole("button", { name: "Cancel" }));
  });

  it("does not tell the admin to retire an already-retired definition that is still selected", async () => {
    await renderPage([shared({ retired: true, selected_user_count: 1, attached_key_count: 1 })]);
    fireEvent.click(screen.getByRole("button", { name: "Delete platform-gateway" }));
    const box = within(screen.getByRole("group", { name: "Confirm deleting platform-gateway" }));
    const lead = box.getByTestId("delete-selected-s1").textContent ?? "";
    expect(lead).toMatch(/1 user has it selected as their Claude source\. Ask them to choose another source\./);
    expect(lead).not.toMatch(/Retire it/);
  });

  it("reloads on a 404, so a definition another admin deleted leaves no ghost card", async () => {
    mockApi.adminDeleteSharedProvider.mockRejectedValue(new ApiError(404, "not found", { error: "not found" }));
    await renderPage([shared({ selected_user_count: 0 })]);
    fireEvent.click(screen.getByRole("button", { name: "Delete platform-gateway" }));
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [] });
    const group = screen.getByRole("group", { name: "Confirm deleting platform-gateway" });
    fireEvent.click(within(group).getByRole("button", { name: "Delete definition and keys" }));
    expect(await screen.findByText("No shared providers yet")).toBeTruthy();
    expect(screen.queryByTestId("selected-count-s1")).toBeNull();
    expect(mockApi.adminListSharedProviders).toHaveBeenCalledTimes(2);
  });

  it("reloads on a 404 from retire too", async () => {
    mockApi.adminRetireSharedProvider.mockRejectedValue(new ApiError(404, "not found", { error: "not found" }));
    await renderPage([shared()]);
    fireEvent.click(screen.getByRole("button", { name: "Retire platform-gateway" }));
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [] });
    fireEvent.click(within(screen.getByRole("group", { name: "Confirm retiring platform-gateway" })).getByRole("button", { name: "Retire definition" }));
    expect(await screen.findByText("No shared providers yet")).toBeTruthy();
  });

  it("surfaces a 409 in-use refusal from stale counts inside the confirmation and refreshes them", async () => {
    mockApi.adminDeleteSharedProvider.mockRejectedValue(conflict("provider_in_use_as_source"));
    await renderPage([shared({ selected_user_count: 0 })]);
    fireEvent.click(screen.getByRole("button", { name: "Delete platform-gateway" }));
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [shared({ selected_user_count: 1 })] });
    const group = screen.getByRole("group", { name: "Confirm deleting platform-gateway" });
    fireEvent.click(within(group).getByRole("button", { name: "Delete definition and keys" }));
    expect((await within(group).findByRole("alert")).textContent).toMatch(
      /Delete refused: at least one user still has this definition selected as their Claude source/,
    );
    await waitFor(() => expect(screen.getByTestId("selected-count-s1").textContent).toBe("Selected by 1 user"));
    expect(screen.getByRole("group", { name: "Confirm deleting platform-gateway" })).toBeTruthy();
  });
});
