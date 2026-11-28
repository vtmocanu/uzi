// @vitest-environment jsdom
// PRD #4324 M4: Admin → Shared providers. Pins keyless create, name/model-map edit with the
// endpoint shown read-only, retire, the delete confirmation's key and selection counts, and
// the 409 refusals (selected-as-source, name collision). Issue #4633: the engine select
// (Claude by default, or Codex) and the per-engine copy, slots and auth.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminSharedProviders } from "./AdminSharedProviders";
import { api, type AdminProviderGrants, type AdminSharedProvider, type User } from "../lib/api";
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
      adminPutOperatorKey: vi.fn(),
      adminSetOperatorKeyEnabled: vi.fn(),
      adminDeleteOperatorKey: vi.fn(),
      adminTestOperatorKey: vi.fn(),
      adminListProviderGrants: vi.fn(),
      adminPutProviderGrant: vi.fn(),
      adminDeleteProviderGrant: vi.fn(),
      adminSetProviderAllUsers: vi.fn(),
      adminRevokeProviderRecipients: vi.fn(),
      listUsers: vi.fn(),
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
    operator_all_users: false,
    named_grant_count: 0,
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

const USERS = [
  { id: "u-ana", email: "ana@example.com", display_name: "Ana Pop", is_admin: false, is_active: true },
  { id: "u-bo", email: "bo@example.com", display_name: null, is_admin: false, is_active: true },
  { id: "u-cy", email: "cy@example.com", display_name: "Cy Ion", is_admin: true, is_active: true },
] as User[];

function grantsPage(ids: string[], over: Partial<AdminProviderGrants> = {}): AdminProviderGrants {
  return { operator_all_users: false, named_grant_count: ids.length, grants: ids.map((user_id) => ({ user_id })), next_cursor: null, ...over };
}

beforeEach(() => {
  mockApi.listProviders.mockResolvedValue({ enabled: true, allowed_base_urls: [URL_A, URL_B], providers: [] });
  mockApi.listUsers.mockResolvedValue({ users: USERS });
  mockApi.adminListProviderGrants.mockResolvedValue(grantsPage([]));
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
        engine: "claude",
        name: "new-gw",
        base_url: URL_B,
        auth_style: "x_api_key",
        model_map: { haiku: { model: "gw-small", context_window: 32768 } },
      }),
    );
    expect(await screen.findByText(/Shared provider new-gw created/)).toBeTruthy();
    expect(mockApi.adminListSharedProviders).toHaveBeenCalledTimes(2);
  });

  it("defaults to Claude, then switching to Codex resets the map and auth and creates a Codex definition", async () => {
    mockApi.adminCreateSharedProvider.mockResolvedValue({ provider: shared({ id: "s2", name: "codex-gw", engine: "codex", protocol: "openai_responses", auth_style: "bearer" }) });
    await renderPage([]);
    fireEvent.click(screen.getByRole("button", { name: "New shared provider" }));
    const form = within(screen.getByRole("form", { name: "New shared provider" }));
    const engine = form.getByLabelText("Engine") as HTMLSelectElement;
    // Before any switch: Claude, its slots and both auth styles.
    expect(engine.value).toBe("claude");
    expect(form.getByTestId("engine-protocol").textContent).toBe("Protocol: Anthropic Messages");
    expect(form.getByLabelText("Haiku model (required)")).toBeTruthy();
    expect(form.queryByLabelText("Default model (required)")).toBeNull();
    const authValues = () => [...(form.getByLabelText("Auth style") as HTMLSelectElement).options].map((o) => o.value);
    expect(authValues()).toEqual(["bearer", "x_api_key"]);
    fireEvent.change(form.getByLabelText("Auth style"), { target: { value: "x_api_key" } });
    fireEvent.change(form.getByLabelText("Haiku model (required)"), { target: { value: "typed-before-switch" } });

    fireEvent.change(engine, { target: { value: "codex" } });
    expect(form.getByTestId("engine-protocol").textContent).toBe("Protocol: OpenAI Responses");
    expect(authValues()).toEqual(["bearer"]);
    expect((form.getByLabelText("Auth style") as HTMLSelectElement).value).toBe("bearer");
    expect(form.queryByLabelText("Haiku model (required)")).toBeNull();
    const advice = form.getByTestId("auth-advice").textContent ?? "";
    expect(advice).toMatch(/Codex runtime redirects may forward request bodies and authentication headers/);
    expect(advice).not.toMatch(/but not another host/);

    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "codex-gw" } });
    fireEvent.change(form.getByLabelText("Base URL"), { target: { value: URL_B } });
    fireEvent.click(form.getByRole("button", { name: "Create shared provider" }));
    expect((await form.findByRole("alert")).textContent).toBe("Default model is required.");
    expect(mockApi.adminCreateSharedProvider).not.toHaveBeenCalled();

    fireEvent.change(form.getByLabelText("Default model (required)"), { target: { value: "gw-code" } });
    fireEvent.click(form.getByRole("button", { name: "Create shared provider" }));
    await waitFor(() =>
      expect(mockApi.adminCreateSharedProvider).toHaveBeenCalledWith({
        engine: "codex",
        name: "codex-gw",
        base_url: URL_B,
        auth_style: "bearer",
        model_map: { default: { model: "gw-code" } },
      }),
    );
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

  it("labels a Codex definition by its engine and edits it with the Codex slots", async () => {
    const codex = shared({ engine: "codex", protocol: "openai_responses", auth_style: "bearer", model_map: { default: { model: "gw-code" } } });
    mockApi.adminPatchSharedProvider.mockResolvedValue({ provider: codex });
    await renderPage([codex]);
    expect(screen.getByText("Codex, OpenAI Responses, revision 2")).toBeTruthy();
    expect(screen.getByText(/^Default:/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Edit platform-gateway" }));
    const form = within(screen.getByRole("form", { name: "Edit platform-gateway" }));
    expect(form.queryByLabelText("Haiku model (required)")).toBeNull();
    expect((form.getByLabelText("Default model (required)") as HTMLInputElement).value).toBe("gw-code");
    fireEvent.change(form.getByLabelText("gpt-6-luna model"), { target: { value: "gw-luna" } });
    fireEvent.click(form.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(mockApi.adminPatchSharedProvider).toHaveBeenCalledWith("s1", {
        model_map: { default: { model: "gw-code" }, "gpt-6-luna": { model: "gw-luna" } },
      }),
    );
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

  it("names the Codex source for a Codex definition in the delete confirmation and its refusal", async () => {
    const codex = (over: Partial<AdminSharedProvider>) => shared({ engine: "codex", protocol: "openai_responses", auth_style: "bearer", model_map: { default: { model: "gw" } }, ...over });
    mockApi.adminDeleteSharedProvider.mockRejectedValue(conflict("provider_in_use_as_source"));
    await renderPage([codex({ selected_user_count: 0 })]);
    fireEvent.click(screen.getByRole("button", { name: "Delete platform-gateway" }));
    const group = screen.getByRole("group", { name: "Confirm deleting platform-gateway" });
    expect(within(group).getByTestId("delete-selected-s1").textContent).toBe("No user has it selected as their Codex source.");
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [codex({ selected_user_count: 2 })] });
    fireEvent.click(within(group).getByRole("button", { name: "Delete definition and keys" }));
    expect((await within(group).findByRole("alert")).textContent).toMatch(/selected as their Codex source/);
    await waitFor(() =>
      expect(screen.getByTestId("delete-selected-s1").textContent).toMatch(/2 users have it selected as their Codex source\./),
    );
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

// PRD #4540 M3: organization-funded (operator_key) definitions.
function operator(over: Partial<AdminSharedProvider> = {}): AdminSharedProvider {
  return shared({
    id: "o1",
    name: "org-gateway",
    credential_mode: "operator_key",
    attached_key_count: 0,
    auth_style: "bearer",
    operator_key: { present: true, enabled: true, set_at: "2026-11-20T10:00:00Z" },
    ...over,
  });
}

async function openOperatorForm() {
  await renderPage([]);
  fireEvent.click(screen.getByRole("button", { name: "New shared provider" }));
  const form = within(screen.getByRole("form", { name: "New shared provider" }));
  fireEvent.change(form.getByLabelText("Credential source"), { target: { value: "operator_key" } });
  return form;
}

describe("AdminSharedProviders — organization key create", () => {
  it("offers the credential source for Claude only", async () => {
    await renderPage([]);
    fireEvent.click(screen.getByRole("button", { name: "New shared provider" }));
    const form = within(screen.getByRole("form", { name: "New shared provider" }));
    const source = form.getByLabelText("Credential source") as HTMLSelectElement;
    expect([...source.options].map((o) => o.value)).toEqual(["per_user_key", "operator_key"]);
    fireEvent.change(source, { target: { value: "operator_key" } });
    fireEvent.change(form.getByLabelText("Engine"), { target: { value: "codex" } });
    expect(form.queryByLabelText("Credential source")).toBeNull();
    expect(form.queryByRole("group", { name: "Organization key disclosures" })).toBeNull();
    // Back to Claude: the choice starts over at per-user keys.
    fireEvent.change(form.getByLabelText("Engine"), { target: { value: "claude" } });
    expect((form.getByLabelText("Credential source") as HTMLSelectElement).value).toBe("per_user_key");
  });

  it("shows no disclosures for per-user keys", async () => {
    await renderPage([]);
    fireEvent.click(screen.getByRole("button", { name: "New shared provider" }));
    const form = within(screen.getByRole("form", { name: "New shared provider" }));
    expect(form.queryByRole("group", { name: "Organization key disclosures" })).toBeNull();
    expect(form.queryByLabelText(/acknowledge/i)).toBeNull();
  });

  const disclosures: [string, RegExp][] = [
    ["disclosure-copy", /receive the key and can copy it.*revoking access in uzi does not take copies back.*rotating the key at the gateway/i],
    ["disclosure-redirect", /redirecting gateway can forward request bodies to the redirect host.*same-host sibling path.*another host with the x_api_key.*Bearer is recommended/i],
    ["disclosure-product-token", /external product's job token.*can spend the key/i],
    ["disclosure-shared-identity", /shared identity.*requests, logs or batches.*expose recipients' work/i],
    ["disclosure-secret-key", /rotating UZI_SECRET_KEY invalidates the stored key/i],
    ["disclosure-authorized-users", /restricted to the organization's authorized users/i],
  ];
  it.each(disclosures)("states disclosure %s", async (testId, pattern) => {
    const form = await openOperatorForm();
    const group = within(form.getByRole("group", { name: "Organization key disclosures" }));
    expect(group.getByTestId(testId).textContent).toMatch(pattern);
  });

  it("lists exactly the six disclosures", async () => {
    const form = await openOperatorForm();
    const group = form.getByRole("group", { name: "Organization key disclosures" });
    expect(group.querySelectorAll("li").length).toBe(6);
  });

  it("keeps Save disabled until the acknowledgment is checked, then sends it", async () => {
    mockApi.adminCreateSharedProvider.mockResolvedValue({ provider: operator() });
    const form = await openOperatorForm();
    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "org-gateway" } });
    fireEvent.change(form.getByLabelText("Base URL"), { target: { value: URL_B } });
    fireEvent.change(form.getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    const save = form.getByRole("button", { name: "Create shared provider" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    const ack = form.getByRole("checkbox", { name: /acknowledge/i }) as HTMLInputElement;
    expect(ack.checked).toBe(false);
    fireEvent.click(ack);
    expect(save.disabled).toBe(false);
    fireEvent.click(ack);
    expect(save.disabled).toBe(true);
    fireEvent.click(ack);
    fireEvent.click(save);
    await waitFor(() =>
      expect(mockApi.adminCreateSharedProvider).toHaveBeenCalledWith({
        engine: "claude",
        name: "org-gateway",
        base_url: URL_B,
        auth_style: "bearer",
        model_map: { haiku: { model: "gw-small" } },
        credential_mode: "operator_key",
        acknowledge_recipient_trust: true,
      }),
    );
  });

  it("drops the acknowledgment when the choice goes back to per-user keys", async () => {
    const form = await openOperatorForm();
    fireEvent.click(form.getByRole("checkbox", { name: /acknowledge/i }));
    fireEvent.change(form.getByLabelText("Credential source"), { target: { value: "per_user_key" } });
    expect((form.getByRole("button", { name: "Create shared provider" }) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.change(form.getByLabelText("Credential source"), { target: { value: "operator_key" } });
    expect((form.getByRole("checkbox", { name: /acknowledge/i }) as HTMLInputElement).checked).toBe(false);
    expect((form.getByRole("button", { name: "Create shared provider" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("AdminSharedProviders — organization key card", () => {
  it("shows the mode, key status and last Test result, and keeps per-user counts off the card", async () => {
    await renderPage([
      operator({ operator_key: { present: true, enabled: false, set_at: "2026-11-20T10:00:00Z", probe: { auth_result: "ok", tool_result: "passed", probed_at: "2026-11-21T00:00:00Z" } } }),
    ]);
    expect(screen.getByTestId("credential-mode-o1").textContent).toBe("Organization key");
    expect(screen.getByTestId("key-status-o1").textContent).toMatch(/Key set, disabled/);
    expect(screen.getByTestId("key-status-o1").textContent).toMatch(/Set /);
    const probe = screen.getByTestId("probe-results-o1").textContent ?? "";
    expect(probe).toMatch(/Authentication worked/);
    expect(probe).toMatch(/Tool use passed/);
    expect(screen.queryByTestId("key-count-o1")).toBeNull();
  });

  it("says when there is no key or no Test yet", async () => {
    await renderPage([operator({ operator_key: { present: false, enabled: false } })]);
    expect(screen.getByTestId("key-status-o1").textContent).toMatch(/No key set/);
    expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Not tested yet/);
    expect(screen.queryByRole("button", { name: /Disable/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Delete organization key/ })).toBeNull();
    expect((screen.getByRole("button", { name: "Test organization key for org-gateway" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("leaves a per-user card without any organization key controls", async () => {
    await renderPage([shared()]);
    expect(screen.queryByLabelText("Organization key")).toBeNull();
    expect(screen.queryByTestId("credential-mode-s1")).toBeNull();
  });

  it("sets a key through a password input, never echoes it, and clears the input", async () => {
    mockApi.adminPutOperatorKey.mockResolvedValue({ provider: operator() });
    await renderPage([operator({ operator_key: { present: false, enabled: false } })]);
    const input = screen.getByLabelText("Organization key") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.autocomplete).toBe("new-password");
    const save = screen.getByRole("button", { name: "Set organization key for org-gateway" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(input, { target: { value: "org-secret-value" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(mockApi.adminPutOperatorKey).toHaveBeenCalledWith("o1", "org-secret-value"));
    await waitFor(() => expect(mockApi.adminListSharedProviders).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/Organization key for org-gateway saved/)).toBeTruthy();
    expect((screen.getByLabelText("Organization key") as HTMLInputElement).value).toBe("");
    expect(document.body.textContent).not.toContain("org-secret-value");
  });

  it("clears the typed key even when the save is refused", async () => {
    mockApi.adminPutOperatorKey.mockRejectedValue(new ApiError(400, "bad key", { error: "bad key" }));
    await renderPage([operator()]);
    fireEvent.change(screen.getByLabelText("Organization key"), { target: { value: "org-secret-value" } });
    fireEvent.click(screen.getByRole("button", { name: "Replace organization key for org-gateway" }));
    expect(await screen.findByText("bad key")).toBeTruthy();
    expect((screen.getByLabelText("Organization key") as HTMLInputElement).value).toBe("");
  });

  it("disables and enables the key with the effect stated", async () => {
    mockApi.adminSetOperatorKeyEnabled.mockResolvedValue({ provider: operator() });
    await renderPage([operator()]);
    expect(screen.getByTestId("disable-effect-o1").textContent).toMatch(
      /stops everyone at the next claim.*copies recipients already obtained are not revoked/i,
    );
    fireEvent.click(screen.getByRole("button", { name: "Disable organization key for org-gateway" }));
    await waitFor(() => expect(mockApi.adminSetOperatorKeyEnabled).toHaveBeenCalledWith("o1", false));
  });

  it("enables a disabled key", async () => {
    mockApi.adminSetOperatorKeyEnabled.mockResolvedValue({ provider: operator() });
    await renderPage([operator({ operator_key: { present: true, enabled: false } })]);
    fireEvent.click(screen.getByRole("button", { name: "Enable organization key for org-gateway" }));
    await waitFor(() => expect(mockApi.adminSetOperatorKeyEnabled).toHaveBeenCalledWith("o1", true));
  });

  it("deletes the key only after a confirmation that opens on Cancel and names the limit", async () => {
    mockApi.adminDeleteOperatorKey.mockResolvedValue({ provider: operator({ operator_key: { present: false, enabled: false } }) });
    await renderPage([operator()]);
    fireEvent.click(screen.getByRole("button", { name: "Delete organization key for org-gateway" }));
    expect(mockApi.adminDeleteOperatorKey).not.toHaveBeenCalled();
    const group = screen.getByRole("group", { name: "Confirm deleting the organization key of org-gateway" });
    expect(group.textContent).toMatch(/everyone stops at the next claim.*copies recipients already obtained are not taken back/i);
    expect(document.activeElement).toBe(within(group).getByRole("button", { name: "Cancel" }));
    fireEvent.click(within(group).getByRole("button", { name: "Delete key" }));
    await waitFor(() => expect(mockApi.adminDeleteOperatorKey).toHaveBeenCalledWith("o1"));
  });

  it("runs Test and shows its result, naming that it uses the key", async () => {
    const probe = { auth_result: "rejected" as const, tool_result: "not_run" as const, probed_at: "2026-11-21T00:00:00Z" };
    mockApi.adminTestOperatorKey.mockResolvedValue({ probe });
    await renderPage([operator()]);
    // The result shown is the server's, read back after the Test.
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [operator({ operator_key: { present: true, enabled: true, probe } })] });
    expect(screen.getByTestId("test-effect-o1").textContent).toMatch(/up to two small requests.*authentication check.*single-tool check/i);
    fireEvent.click(screen.getByRole("button", { name: "Test organization key for org-gateway" }));
    await waitFor(() => expect(mockApi.adminTestOperatorKey).toHaveBeenCalledWith("o1"));
    await waitFor(() => expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Authentication rejected/));
  });

  const OK_PROBE = { auth_result: "ok" as const, tool_result: "passed" as const, probed_at: "2026-11-21T00:00:00Z" };
  const withProbe = (over: Partial<AdminSharedProvider> = {}) =>
    operator({ operator_key: { present: true, enabled: true, probe: OK_PROBE }, ...over });

  // Renders a card, runs a Test, and leaves the server's reloaded read showing the result.
  async function testedCard() {
    mockApi.adminTestOperatorKey.mockResolvedValue({ probe: OK_PROBE });
    await renderPage([operator()]);
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [withProbe()] });
    fireEvent.click(screen.getByRole("button", { name: "Test organization key for org-gateway" }));
    await waitFor(() => expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Authentication worked/));
  }

  it("drops a shown Test result when a later key write reloads the card without one", async () => {
    mockApi.adminPutOperatorKey.mockResolvedValue({ provider: operator() });
    await testedCard();
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [operator()] });
    fireEvent.change(screen.getByLabelText("Organization key"), { target: { value: "next-key" } });
    fireEvent.click(screen.getByRole("button", { name: "Replace organization key for org-gateway" }));
    await waitFor(() => expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Not tested yet/));
  });

  it("shows no stale Test result after Retire, once the server read hides it", async () => {
    mockApi.adminRetireSharedProvider.mockResolvedValue({ provider: operator({ retired: true }) });
    await testedCard();
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [operator({ retired: true })] });
    fireEvent.click(screen.getByRole("button", { name: "Retire org-gateway" }));
    fireEvent.click(screen.getByRole("button", { name: "Retire definition" }));
    await waitFor(() => expect(screen.getByTestId("test-effect-o1").textContent).toMatch(/A retired definition cannot be tested/));
    expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Not tested yet/);
    expect(screen.getByTestId("probe-results-o1").textContent).not.toMatch(/Authentication worked/);
  });

  it("shows no stale Test result after a model-map edit makes a new revision", async () => {
    mockApi.adminPatchSharedProvider.mockResolvedValue({ provider: operator({ current_revision: 2 }) });
    await testedCard();
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [operator({ current_revision: 2 })] });
    fireEvent.click(screen.getByRole("button", { name: "Edit org-gateway" }));
    const form = within(screen.getByRole("form", { name: "Edit org-gateway" }));
    fireEvent.change(form.getByLabelText("Sonnet model"), { target: { value: "gw-large" } });
    fireEvent.click(form.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.adminPatchSharedProvider).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Not tested yet/));
    expect(screen.getByTestId("probe-results-o1").textContent).not.toMatch(/Authentication worked/);
  });

  it("shows the written key state and no old verdict when the refresh after a replace fails", async () => {
    mockApi.adminPutOperatorKey.mockResolvedValue({ provider: operator() });
    await testedCard();
    mockApi.adminListSharedProviders.mockRejectedValue(new Error("network down"));
    fireEvent.change(screen.getByLabelText("Organization key"), { target: { value: "next-key" } });
    fireEvent.click(screen.getByRole("button", { name: "Replace organization key for org-gateway" }));
    await waitFor(() => expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Not tested yet/));
    expect(screen.getByTestId("probe-results-o1").textContent).not.toMatch(/Authentication worked/);
  });

  it("shows the key disabled and no old verdict when the refresh after a disable fails", async () => {
    mockApi.adminSetOperatorKeyEnabled.mockResolvedValue({ provider: operator({ operator_key: { present: true, enabled: false } }) });
    await testedCard();
    mockApi.adminListSharedProviders.mockRejectedValue(new Error("network down"));
    fireEvent.click(screen.getByRole("button", { name: "Disable organization key for org-gateway" }));
    await waitFor(() => expect(screen.getByTestId("key-status-o1").textContent).toMatch(/Key set, disabled/));
    expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Not tested yet/);
  });

  it("shows the key gone when the refresh after a key delete fails", async () => {
    mockApi.adminDeleteOperatorKey.mockResolvedValue({ provider: operator({ operator_key: { present: false, enabled: false } }) });
    await testedCard();
    mockApi.adminListSharedProviders.mockRejectedValue(new Error("network down"));
    fireEvent.click(screen.getByRole("button", { name: "Delete organization key for org-gateway" }));
    const group = screen.getByRole("group", { name: "Confirm deleting the organization key of org-gateway" });
    fireEvent.click(within(group).getByRole("button", { name: "Delete key" }));
    await waitFor(() => expect(screen.getByTestId("key-status-o1").textContent).toMatch(/No key set/));
    expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Not tested yet/);
  });

  it("shows this Test's result when the refresh after a Test fails", async () => {
    const probe = { auth_result: "rejected" as const, tool_result: "not_run" as const, probed_at: "2026-11-21T00:00:00Z" };
    mockApi.adminTestOperatorKey.mockResolvedValue({ probe });
    await renderPage([operator()]);
    mockApi.adminListSharedProviders.mockRejectedValue(new Error("network down"));
    fireEvent.click(screen.getByRole("button", { name: "Test organization key for org-gateway" }));
    await waitFor(() => expect(screen.getByTestId("probe-results-o1").textContent).toMatch(/Authentication rejected/));
  });

  it("says on create that nobody can use an organization-key definition until access is granted", async () => {
    mockApi.adminCreateSharedProvider.mockResolvedValue({ provider: operator() });
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [operator()] });
    const form = await openOperatorForm();
    fireEvent.change(form.getByLabelText("Provider name"), { target: { value: "org-gateway" } });
    fireEvent.change(form.getByLabelText("Base URL"), { target: { value: URL_B } });
    fireEvent.change(form.getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    fireEvent.click(form.getByRole("checkbox", { name: /acknowledge/i }));
    fireEvent.click(form.getByRole("button", { name: "Create shared provider" }));
    const notice = await screen.findByText(/Shared provider org-gateway created/);
    expect(notice.textContent).toMatch(/Set its organization key and grant access next: no user can see or use it until you do/);
    expect(notice.textContent).not.toMatch(/attach their own key/);
    expect(notice.textContent).not.toMatch(/staged|later release|not yet available/i);
  });

  it("does not mention an organization key in the delete copy when none is set", async () => {
    mockApi.adminDeleteSharedProvider.mockResolvedValue({ deleted: true, deleted_key_count: 0 });
    await renderPage([operator({ operator_key: { present: false, enabled: false } })]);
    fireEvent.click(screen.getByRole("button", { name: "Delete org-gateway" }));
    const box = within(screen.getByRole("group", { name: "Confirm deleting org-gateway" }));
    expect(box.getByTestId("delete-keys-o1").textContent).not.toMatch(/organization key/i);
    fireEvent.click(box.getByRole("button", { name: "Delete definition" }));
    expect(await screen.findByText("org-gateway deleted.")).toBeTruthy();
  });

  // PRD #4540 M2 retired the M1 "staged / later release" copy for default-deny. Each check pairs the
  // new wording (positive) with a did-the-old-copy-come-back guard on the same element.
  const DENY = /^No user can see or use this definition until you grant access; granted users' runs receive the organization key\./;
  const OLD = /staged|later release|not yet available|once recipients/i;
  it("states default-deny on the card and in the create form, with no staged copy left", async () => {
    await renderPage([operator()]);
    const card = screen.getByTestId("access-notice-o1");
    expect(card.textContent).toMatch(DENY);
    expect(card.textContent).not.toMatch(OLD);
    const keyPanel = screen.getByTestId("operator-key-o1");
    expect(keyPanel.textContent).toMatch(/Copies recipients already obtained keep working until the old key is rotated at the gateway/);
    expect(keyPanel.textContent).not.toMatch(OLD);
    cleanup();
    const form = await openOperatorForm();
    const formNotice = form.getByTestId("access-notice-new");
    expect(formNotice.textContent).toMatch(DENY);
    expect(formNotice.textContent).toMatch(/These terms apply to every user you grant access to\./);
    expect(formNotice.textContent).not.toMatch(OLD);
    const intro = form.getByText(/You set one organization key after creating it\./);
    expect(intro.textContent).toMatch(/until you grant access; granted users' runs receive the organization key/);
    expect(intro.textContent).not.toMatch(OLD);
    // The redirect disclosure is unchanged, and the disclosure group as a whole carries no staged copy.
    expect(form.getByTestId("disclosure-redirect").textContent).toBe(
      "A redirecting gateway can forward request bodies to the redirect host, and can send the key to a same-host sibling path, or to another host with the x_api_key auth style. Bearer is recommended.",
    );
    const group = form.getByRole("group", { name: "Organization key disclosures" });
    expect(group.textContent).toContain("No user can see or use this definition until you grant access");
    expect(group.textContent).not.toMatch(OLD);
    expect(form.getByTestId("disclosure-copy").textContent).toMatch(/rotate at the gateway.*per-key spend limits/i);
  });

  it("keeps the key controls on a retired definition but not Test", async () => {
    await renderPage([operator({ retired: true })]);
    expect(screen.getByRole("button", { name: "Replace organization key for org-gateway" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Disable organization key for org-gateway" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete organization key for org-gateway" })).toBeTruthy();
    expect((screen.getByRole("button", { name: "Test organization key for org-gateway" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("says an operator definition delete also deletes its organization key", async () => {
    mockApi.adminDeleteSharedProvider.mockResolvedValue({ deleted: true, deleted_key_count: 0 });
    await renderPage([operator()]);
    fireEvent.click(screen.getByRole("button", { name: "Delete org-gateway" }));
    const box = within(screen.getByRole("group", { name: "Confirm deleting org-gateway" }));
    const text = box.getByTestId("delete-keys-o1").textContent ?? "";
    expect(text).toMatch(/organization key and that key's Test result/);
    expect(text).not.toMatch(/personal key/);
    fireEvent.click(box.getByRole("button", { name: "Delete definition and key" }));
    expect(await screen.findByText("org-gateway deleted, with its organization key.")).toBeTruthy();
  });

  it("explains a rate-limited Test", async () => {
    mockApi.adminTestOperatorKey.mockRejectedValue(new ApiError(429, "slow", { error: "slow", reason: "provider_probe_rate_limited" }));
    await renderPage([operator()]);
    fireEvent.click(screen.getByRole("button", { name: "Test organization key for org-gateway" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/Tests are rate limited/);
  });

  it("explains a stale Test and refreshes the card", async () => {
    mockApi.adminTestOperatorKey.mockRejectedValue(
      new ApiError(409, "the organization key or provider changed while it was being tested; test it again", { error: "the organization key or provider changed while it was being tested; test it again" }),
    );
    await renderPage([operator()]);
    fireEvent.click(screen.getByRole("button", { name: "Test organization key for org-gateway" }));
    expect((await screen.findByRole("alert")).textContent).toBe("The provider or key changed while testing; test again.");
    await waitFor(() => expect(mockApi.adminListSharedProviders).toHaveBeenCalledTimes(2));
  });

  it("explains a key route refused for a per-user definition", async () => {
    mockApi.adminSetOperatorKeyEnabled.mockRejectedValue(conflict("provider_not_operator_key"));
    await renderPage([operator()]);
    fireEvent.click(screen.getByRole("button", { name: "Disable organization key for org-gateway" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/not organization-funded/);
  });
});

describe("AdminSharedProviders — recipients (PRD #4540 M2)", () => {
  const recipients = () => within(screen.getByTestId("recipients-o1"));

  it("renders the recipients section only on an organization-key card", async () => {
    await renderPage([shared(), operator()]);
    expect(screen.getByTestId("recipients-o1")).toBeTruthy();
    expect(screen.queryByTestId("recipients-s1")).toBeNull();
    expect(screen.queryByRole("switch")).not.toBeNull();
    expect(screen.getAllByRole("switch")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: /Revoke all recipients of platform-gateway/ })).toBeNull();
    await waitFor(() => expect(mockApi.adminListProviderGrants).toHaveBeenCalledWith("o1", { cursor: undefined, limit: 50 }));
    expect(mockApi.adminListProviderGrants).not.toHaveBeenCalledWith("s1", expect.anything());
  });

  it("reads neither users nor grants when no card is organization-funded", async () => {
    await renderPage([shared()]);
    expect(mockApi.listUsers).not.toHaveBeenCalled();
    expect(mockApi.adminListProviderGrants).not.toHaveBeenCalled();
  });

  it("shows the named-grant count and the granted users joined to their names", async () => {
    mockApi.adminListProviderGrants.mockResolvedValue(grantsPage(["u-ana", "u-bo", "u-gone"]));
    await renderPage([operator({ named_grant_count: 3 })]);
    expect(recipients().getByTestId("named-count-o1").textContent).toBe("Named users: 3");
    expect(recipients().getByTestId("recipient-summary-o1").textContent).toBe("3 named users have access.");
    const list = within(await recipients().findByRole("list", { name: "Users granted org-gateway" }));
    expect(list.getByText("Ana Pop")).toBeTruthy();
    expect(list.getByText("ana@example.com")).toBeTruthy();
    expect(list.getByText("bo@example.com")).toBeTruthy();
    // A grant the user list does not know shows its id, never a blank row.
    expect(list.getByText("Unknown user")).toBeTruthy();
    expect(list.getByText("u-gone")).toBeTruthy();
  });

  it("says nobody has access yet on a fresh definition", async () => {
    await renderPage([operator()]);
    expect(recipients().getByTestId("recipient-summary-o1").textContent).toBe("No recipients yet.");
    expect((recipients().getByRole("switch", { name: "All users can use org-gateway" })).getAttribute("aria-checked")).toBe("false");
  });

  it("pages the grant list with the cursor", async () => {
    mockApi.adminListProviderGrants
      .mockResolvedValueOnce(grantsPage(["u-ana"], { named_grant_count: 2, next_cursor: "u-ana" }))
      .mockResolvedValueOnce(grantsPage(["u-bo"], { named_grant_count: 2 }));
    await renderPage([operator({ named_grant_count: 2 })]);
    fireEvent.click(await recipients().findByRole("button", { name: "Show more recipients" }));
    await waitFor(() => expect(mockApi.adminListProviderGrants).toHaveBeenLastCalledWith("o1", { cursor: "u-ana", limit: 50 }));
    const list = within(await recipients().findByRole("list", { name: "Users granted org-gateway" }));
    await waitFor(() => expect(list.getByText("bo@example.com")).toBeTruthy());
    expect(list.getByText("Ana Pop")).toBeTruthy();
    expect(recipients().queryByRole("button", { name: "Show more recipients" })).toBeNull();
  });

  it("grants a picked user through PUT and offers only users not already named", async () => {
    mockApi.adminListProviderGrants.mockResolvedValue(grantsPage(["u-ana"]));
    mockApi.adminPutProviderGrant.mockResolvedValue(null);
    await renderPage([operator({ named_grant_count: 1 })]);
    await recipients().findByText("Ana Pop");
    const picker = recipients().getByLabelText("Grant access to a user") as HTMLSelectElement;
    expect([...picker.options].map((o) => o.value)).toEqual(["", "u-bo", "u-cy"]);
    const grant = recipients().getByRole("button", { name: "Grant org-gateway to the chosen user" }) as HTMLButtonElement;
    expect(grant.disabled).toBe(true);
    fireEvent.change(picker, { target: { value: "u-cy" } });
    fireEvent.click(grant);
    await waitFor(() => expect(mockApi.adminPutProviderGrant).toHaveBeenCalledWith("o1", "u-cy"));
    expect(await screen.findByText(/Cy Ion can now use org-gateway; their runs receive the organization key/)).toBeTruthy();
    expect(mockApi.adminListSharedProviders).toHaveBeenCalledTimes(2);
  });

  it("removes a granted user through DELETE", async () => {
    mockApi.adminListProviderGrants.mockResolvedValue(grantsPage(["u-ana"]));
    mockApi.adminDeleteProviderGrant.mockResolvedValue(null);
    await renderPage([operator({ named_grant_count: 1 })]);
    fireEvent.click(await recipients().findByRole("button", { name: "Remove Ana Pop from org-gateway" }));
    await waitFor(() => expect(mockApi.adminDeleteProviderGrant).toHaveBeenCalledWith("o1", "u-ana"));
    expect(await screen.findByText(/Ana Pop no longer has access to org-gateway/)).toBeTruthy();
  });

  it("turns all users on and off through the all-users PUT, with the effect stated", async () => {
    mockApi.adminSetProviderAllUsers.mockResolvedValue(null);
    await renderPage([operator()]);
    expect(recipients().getByTestId("all-users-effect-o1").textContent).toMatch(/only the named users below have access/);
    const sw = recipients().getByRole("switch", { name: "All users can use org-gateway" });
    expect(sw.getAttribute("aria-describedby")).toBe(recipients().getByTestId("all-users-effect-o1").id);
    mockApi.adminListSharedProviders.mockResolvedValue({ providers: [operator({ operator_all_users: true })] });
    fireEvent.click(sw);
    await waitFor(() => expect(mockApi.adminSetProviderAllUsers).toHaveBeenCalledWith("o1", true));
    await waitFor(() => expect(recipients().getByRole("switch", { name: "All users can use org-gateway" }).getAttribute("aria-checked")).toBe("true"));
    expect(recipients().getByTestId("recipient-summary-o1").textContent).toBe("All users have access.");
    fireEvent.click(recipients().getByRole("switch", { name: "All users can use org-gateway" }));
    await waitFor(() => expect(mockApi.adminSetProviderAllUsers).toHaveBeenLastCalledWith("o1", false));
  });

  it("revokes every recipient only after a confirmation on Cancel, and reports the removed count", async () => {
    mockApi.adminListProviderGrants.mockResolvedValue(grantsPage(["u-ana", "u-bo"], { operator_all_users: true }));
    mockApi.adminRevokeProviderRecipients.mockResolvedValue({ named_grants_removed: 2 });
    await renderPage([operator({ operator_all_users: true, named_grant_count: 2 })]);
    expect(recipients().getByTestId("revoke-effect-o1").textContent).toMatch(
      /Turns all users off and removes every named grant\. Takes effect at the next claim; copies already obtained are revoked only by rotating the key at the gateway\./,
    );
    fireEvent.click(recipients().getByRole("button", { name: "Revoke all recipients of org-gateway" }));
    expect(mockApi.adminRevokeProviderRecipients).not.toHaveBeenCalled();
    const group = within(screen.getByRole("group", { name: "Confirm revoking all recipients of org-gateway" }));
    expect(document.activeElement).toBe(group.getByRole("button", { name: "Cancel" }));
    expect(group.getByText(/2 named grants are removed/)).toBeTruthy();
    fireEvent.click(group.getByRole("button", { name: "Revoke all recipients" }));
    await waitFor(() => expect(mockApi.adminRevokeProviderRecipients).toHaveBeenCalledWith("o1"));
    expect(await screen.findByText(/Revoked every recipient of org-gateway: all users turned off and 2 named grants removed/)).toBeTruthy();
  });

  it("labels each incident stop with its effect", async () => {
    await renderPage([operator()]);
    expect(screen.getByTestId("disable-effect-o1").textContent).toMatch(/^Stops everyone at the next claim/);
    expect(screen.getByTestId("delete-key-effect-o1").textContent).toMatch(/^\s*Stops everyone at the next claim/);
    expect(recipients().getByTestId("stop-scope-o1").textContent).toMatch(/Retire stops fresh use only/);
    fireEvent.click(screen.getByRole("button", { name: "Retire org-gateway" }));
    expect(screen.getByTestId("retire-effect-o1").textContent).toMatch(/Retire stops fresh use only: runs already started keep the organization key/);
  });

  it("explains a grant refused at the cap", async () => {
    mockApi.adminPutProviderGrant.mockRejectedValue(conflict("provider_grant_cap_reached"));
    await renderPage([operator()]);
    await recipients().findByLabelText("Grant access to a user");
    fireEvent.change(recipients().getByLabelText("Grant access to a user"), { target: { value: "u-bo" } });
    fireEvent.click(recipients().getByRole("button", { name: "Grant org-gateway to the chosen user" }));
    expect((await recipients().findByRole("alert")).textContent).toMatch(/already has 10,000 named recipients, the limit\. Remove one first, or give access to all users/);
  });

  it("explains a grant to a user who no longer exists and refreshes the user list", async () => {
    mockApi.adminPutProviderGrant.mockRejectedValue(new ApiError(404, "user not found", { error: "user not found", reason: "user_not_found" }));
    await renderPage([operator()]);
    await recipients().findByLabelText("Grant access to a user");
    fireEvent.change(recipients().getByLabelText("Grant access to a user"), { target: { value: "u-bo" } });
    fireEvent.click(recipients().getByRole("button", { name: "Grant org-gateway to the chosen user" }));
    expect((await recipients().findByRole("alert")).textContent).toMatch(/That user no longer exists, so access was not granted/);
    await waitFor(() => expect(mockApi.listUsers).toHaveBeenCalledTimes(2));
  });

  it("keeps removal but no new grant on a retired definition", async () => {
    mockApi.adminListProviderGrants.mockResolvedValue(grantsPage(["u-ana"]));
    await renderPage([operator({ retired: true, named_grant_count: 1 })]);
    expect(await recipients().findByRole("button", { name: "Remove Ana Pop from org-gateway" })).toBeTruthy();
    expect(recipients().getByRole("button", { name: "Revoke all recipients of org-gateway" })).toBeTruthy();
    expect(recipients().queryByLabelText("Grant access to a user")).toBeNull();
    expect((recipients().getByRole("switch", { name: "All users can use org-gateway" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("warns that recipients cannot run while the key is missing or disabled", async () => {
    await renderPage([operator({ named_grant_count: 1, operator_key: { present: true, enabled: false } })]);
    expect(recipients().getByTestId("recipients-blocked-o1").textContent).toMatch(/cannot run on it until its key is set and enabled/);
  });
});
