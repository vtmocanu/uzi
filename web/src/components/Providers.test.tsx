// @vitest-environment jsdom
// PRD #4320 M3: the Settings "Custom providers" card. Pins the feature-off state,
// list rendering, create validation (haiku required, context-window warning), the
// write-only key form, the two-line Test result with its caveat, and the error copy
// for the 409 / 429 reasons.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Providers } from "./Providers";
import { api, type ProviderDTO, type ProvidersList } from "../lib/api";
import { ApiError } from "../lib/apiError";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listProviders: vi.fn(),
      createProvider: vi.fn(),
      patchProvider: vi.fn(),
      patchProviderEndpoint: vi.fn(),
      putProviderKey: vi.fn(),
      deleteProviderKey: vi.fn(),
      setProviderKeyEnabled: vi.fn(),
      retireProvider: vi.fn(),
      deleteProvider: vi.fn(),
      testProvider: vi.fn(),
      getMySettings: vi.fn(),
      setClaudeProviderSource: vi.fn(),
      setCodexProviderSource: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);
const URL_A = "https://gw.example.com/anthropic";
const URL_B = "https://other.example.com/v1";

function provider(over: Partial<ProviderDTO> = {}): ProviderDTO {
  return {
    id: "p1",
    name: "team-gateway",
    engine: "claude",
    retired: false,
    current_revision: 1,
    protocol: "anthropic_messages",
    base_url: URL_A,
    auth_style: "bearer",
    model_map: { haiku: { model: "gw-small", context_window: 32768 }, sonnet: { model: "gw-large" } },
    has_key: true,
    key_enabled: true,
    probe: null,
    ...over,
  };
}

function list(over: Partial<ProvidersList> = {}): ProvidersList {
  return { enabled: true, allowed_base_urls: [URL_A, URL_B], providers: [provider()], ...over };
}

function apiError(status: number, reason: string): ApiError {
  return new ApiError(status, "server text", { error: "server text", reason });
}

async function renderCard(data: ProvidersList = list()) {
  mockApi.listProviders.mockResolvedValue(data);
  const view = render(<Providers />);
  await screen.findByText("Custom providers");
  await waitFor(() => expect(mockApi.listProviders).toHaveBeenCalled());
  return view;
}

beforeEach(() => {
  mockApi.listProviders.mockReset();
  mockApi.getMySettings.mockReset().mockResolvedValue({ settings: { claude_provider_id: null } } as never);
  mockApi.setClaudeProviderSource.mockReset();
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Providers — feature off", () => {
  it("explains the operator has not enabled custom providers and offers no form", async () => {
    await renderCard(list({ enabled: false, allowed_base_urls: [], providers: [] }));
    expect(await screen.findByText(/UZI_PROVIDER_ALLOWED_BASE_URLS/)).toBeTruthy();
    expect(screen.getByText(/Your operator has not enabled them/)).toBeTruthy();
    expect(screen.queryByRole("form", { name: "Add a provider" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add provider" })).toBeNull();
  });
});

describe("Providers — list", () => {
  it("labels a blank slot 200k assumed only when no mapped model has a window", async () => {
    await renderCard(list({ providers: [provider({ id: "p3", model_map: { haiku: { model: "gw-small" }, sonnet: { model: "gw-large" } } })] }));
    const row = await screen.findByTestId("provider-p3");
    expect(within(row).getAllByText(/\(no context window: 200k assumed\)/)).toHaveLength(2);
  });

  it("never claims a 200k assumption for a Codex provider without context windows", async () => {
    await renderCard(list({ providers: [provider({ id: "cx", engine: "codex", protocol: "openai_responses", model_map: { default: { model: "gw-code" } } })] }));
    const row = await screen.findByTestId("provider-cx");
    expect(within(row).getByText("gw-code")).toBeTruthy();
    expect(within(row).queryByText(/200k/)).toBeNull();
    expect(within(row).getAllByText(/\(no context window: Codex runtime fallback\)/)).toHaveLength(1);
  });

  it("renders name, URL, auth style, models, key state and retired badge as text", async () => {
    await renderCard(
      list({
        providers: [
          provider({ name: "<b>bold</b>", base_url: URL_A }),
          provider({ id: "p2", name: "old-gw", retired: true, auth_style: "x_api_key", has_key: false, key_enabled: false }),
        ],
      }),
    );
    const first = await screen.findByTestId("provider-p1");
    expect(within(first).getByText("<b>bold</b>")).toBeTruthy();
    expect(first.querySelector("b")).toBeNull();
    expect(within(first).getByText(URL_A)).toBeTruthy();
    expect(within(first).getByText("Authorization: Bearer")).toBeTruthy();
    expect(within(first).getByText("gw-small")).toBeTruthy();
    // p1 maps haiku with a 32768 window and sonnet with none: the smallest configured window applies.
    expect(within(first).getByText(/\(no context window: uzi requests the smallest configured one, 32,768 tokens\)/)).toBeTruthy();
    expect(within(first).queryByText(/200k assumed/)).toBeNull();
    expect(within(first).getByText("Key enabled")).toBeTruthy();

    const second = screen.getByTestId("provider-p2");
    expect(within(second).getByText("Retired")).toBeTruthy();
    expect(within(second).getByText("x-api-key")).toBeTruthy();
    expect(within(second).getByText("No key")).toBeTruthy();
    // A retired provider offers only Test (disabled) and Delete.
    expect(within(second).queryByRole("button", { name: "Retire" })).toBeNull();
    expect(within(second).queryByRole("button", { name: /Edit/ })).toBeNull();
    expect((within(second).getByRole("button", { name: "Test" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("disables Test without an enabled key", async () => {
    await renderCard(list({ providers: [provider({ key_enabled: false })] }));
    const row = await screen.findByTestId("provider-p1");
    expect((within(row).getByRole("button", { name: "Test" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("Providers — Test", () => {
  it("shows the two result lines and the compatibility caveat", async () => {
    mockApi.testProvider.mockResolvedValue({
      probe: { auth_result: "ok", tool_result: "not_supported", probed_at: "2026-01-01T00:00:00Z" },
    });
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Test" }));
    const results = await within(row).findByTestId("probe-results");
    const lines = Array.from(results.querySelectorAll("p")).map((p) => p.textContent);
    expect(lines[0]).toBe("Authentication worked");
    expect(lines[1]).toBe("Tool use not supported by the gateway");
    expect(lines[2]).toContain("Neither result certifies full compatibility");
  });

  it("shows the rate-limit message on 429", async () => {
    mockApi.testProvider.mockRejectedValue(apiError(429, "provider_probe_rate_limited"));
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Test" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/rate limited/);
  });
});

describe("Providers — create", () => {
  const form = () => screen.getByRole("form", { name: "Add a provider" });

  it("requires haiku and does not call the API", async () => {
    await renderCard(list({ providers: [] }));
    fireEvent.change(within(form()).getByLabelText("Provider name"), { target: { value: "gw" } });
    fireEvent.change(within(form()).getByLabelText("Base URL"), { target: { value: URL_A } });
    fireEvent.click(within(form()).getByRole("button", { name: "Add provider" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/Haiku model is required/);
    expect(mockApi.createProvider).not.toHaveBeenCalled();
  });

  it("warns for a mapped model without a context window and clears it when set", async () => {
    await renderCard(list({ providers: [] }));
    expect(screen.queryByTestId("ctx-warning-haiku")).toBeNull();
    fireEvent.change(within(form()).getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    expect(screen.getByTestId("ctx-warning-haiku").textContent).toMatch(/smallest window configured on any model applies to all of them, and uzi assumes 200k tokens if none is configured; set the gateway model's real window/);
    fireEvent.change(within(form()).getByLabelText("Haiku context window"), { target: { value: "65536" } });
    expect(screen.queryByTestId("ctx-warning-haiku")).toBeNull();
  });

  it("warns the CLI may apply its own window for a name containing claude, with or without a window", async () => {
    await renderCard(list({ providers: [] }));
    fireEvent.change(within(form()).getByLabelText("Haiku model (required)"), { target: { value: "us.anthropic.Claude-haiku-4-5" } });
    // No window: both the 200k-assumed warning and the claude caveat show.
    expect(screen.getByTestId("ctx-warning-haiku").textContent).toMatch(/uzi assumes 200k tokens if none is configured/);
    expect(screen.getByTestId("ctx-unenforced-haiku").textContent).toMatch(
      /may apply its own window to model names it recognizes as Anthropic models.*ignoring both a configured window and uzi's 200k default \(measured at Claude Code 2\.1\.325\)/,
    );
    // With a window: the no-window warning clears, the caveat stays.
    fireEvent.change(within(form()).getByLabelText("Haiku context window"), { target: { value: "65536" } });
    expect(screen.queryByTestId("ctx-warning-haiku")).toBeNull();
    expect(screen.getByTestId("ctx-unenforced-haiku").textContent).toMatch(/may apply its own window/);
    // A non-claude name drops the caveat.
    fireEvent.change(within(form()).getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    expect(screen.queryByTestId("ctx-unenforced-haiku")).toBeNull();
  });

  it("rejects an out-of-range context window", async () => {
    await renderCard(list({ providers: [] }));
    fireEvent.change(within(form()).getByLabelText("Provider name"), { target: { value: "gw" } });
    fireEvent.change(within(form()).getByLabelText("Base URL"), { target: { value: URL_A } });
    fireEvent.change(within(form()).getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    fireEvent.change(within(form()).getByLabelText("Haiku context window"), { target: { value: "100" } });
    fireEvent.click(within(form()).getByRole("button", { name: "Add provider" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/context window must be/);
    expect(mockApi.createProvider).not.toHaveBeenCalled();
  });

  it("submits the allowlisted URL, auth style and model map", async () => {
    mockApi.createProvider.mockResolvedValue({ provider: provider() });
    await renderCard(list({ providers: [] }));
    fireEvent.change(within(form()).getByLabelText("Provider name"), { target: { value: "gw" } });
    fireEvent.change(within(form()).getByLabelText("Base URL"), { target: { value: URL_B } });
    fireEvent.change(within(form()).getByLabelText("Auth style"), { target: { value: "x_api_key" } });
    fireEvent.change(within(form()).getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    fireEvent.change(within(form()).getByLabelText("Haiku context window"), { target: { value: "65536" } });
    fireEvent.click(within(form()).getByRole("button", { name: "Add provider" }));
    await waitFor(() =>
      expect(mockApi.createProvider).toHaveBeenCalledWith({
        engine: "claude",
        protocol: "anthropic_messages",
        name: "gw",
        base_url: URL_B,
        auth_style: "x_api_key",
        model_map: { haiku: { model: "gw-small", context_window: 65536 } },
      }),
    );
  });
});

describe("Providers — key", () => {
  it("sends the key, then clears the field and never echoes it", async () => {
    mockApi.putProviderKey.mockResolvedValue({ provider: provider() });
    const secret = "k".repeat(24);
    const { container } = await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Rotate key" }));
    const input = within(row).getByLabelText("Provider key") as HTMLInputElement;
    expect(input.type).toBe("password");
    fireEvent.change(input, { target: { value: secret } });
    fireEvent.click(within(row).getByRole("button", { name: "Save key" }));
    await waitFor(() => expect(mockApi.putProviderKey).toHaveBeenCalledWith("p1", secret));
    await waitFor(() => expect(within(row).queryByLabelText("Provider key")).toBeNull());
    expect(container.textContent).not.toContain(secret);
    expect(container.innerHTML).not.toContain(secret);
  });

  it("rejects a too-short key without calling the API", async () => {
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Rotate key" }));
    fireEvent.change(within(row).getByLabelText("Provider key"), { target: { value: "short" } });
    fireEvent.click(within(row).getByRole("button", { name: "Save key" }));
    expect(await within(row).findByText(/16-4096 printable ASCII/)).toBeTruthy();
    expect(mockApi.putProviderKey).not.toHaveBeenCalled();
  });
});

describe("Providers — key trimming and actions", () => {
  it("trims the pasted key before validating and sending", async () => {
    mockApi.putProviderKey.mockResolvedValue({ provider: provider() });
    const secret = "k".repeat(20);
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Rotate key" }));
    fireEvent.change(within(row).getByLabelText("Provider key"), { target: { value: `  ${secret}\n` } });
    fireEvent.click(within(row).getByRole("button", { name: "Save key" }));
    await waitFor(() => expect(mockApi.putProviderKey).toHaveBeenCalledWith("p1", secret));
  });

  it("disables and enables the key", async () => {
    mockApi.setProviderKeyEnabled.mockResolvedValue({ provider: provider() });
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Disable key" }));
    await waitFor(() => expect(mockApi.setProviderKeyEnabled).toHaveBeenCalledWith("p1", false));
  });

  it("enables a disabled key", async () => {
    mockApi.setProviderKeyEnabled.mockResolvedValue({ provider: provider() });
    await renderCard(list({ providers: [provider({ key_enabled: false })] }));
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Enable key" }));
    await waitFor(() => expect(mockApi.setProviderKeyEnabled).toHaveBeenCalledWith("p1", true));
  });

  it("asks before deleting the key, and Cancel deletes nothing", async () => {
    mockApi.deleteProviderKey.mockResolvedValue(null);
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Delete key" }));
    expect(mockApi.deleteProviderKey).not.toHaveBeenCalled();
    fireEvent.click(within(row).getByRole("button", { name: "Cancel" }));
    expect(mockApi.deleteProviderKey).not.toHaveBeenCalled();
    fireEvent.click(within(row).getByRole("button", { name: "Delete key" }));
    fireEvent.click(within(row).getByRole("button", { name: "Confirm delete key" }));
    await waitFor(() => expect(mockApi.deleteProviderKey).toHaveBeenCalledWith("p1"));
  });

  it("retires a provider", async () => {
    mockApi.retireProvider.mockResolvedValue({ provider: provider({ retired: true }) });
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Retire" }));
    await waitFor(() => expect(mockApi.retireProvider).toHaveBeenCalledWith("p1"));
  });

  it("tells the user to delete (not retire) at the provider limit", async () => {
    mockApi.createProvider.mockRejectedValue(apiError(409, "provider_limit_reached"));
    await renderCard(list({ providers: [] }));
    const f = screen.getByRole("form", { name: "Add a provider" });
    fireEvent.change(within(f).getByLabelText("Provider name"), { target: { value: "gw" } });
    fireEvent.change(within(f).getByLabelText("Base URL"), { target: { value: URL_A } });
    fireEvent.change(within(f).getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    fireEvent.click(within(f).getByRole("button", { name: "Add provider" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/Delete one first\./);
  });
});

describe("Providers — edit", () => {
  const openEdit = async () => {
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Edit name and models" }));
    return row;
  };

  it("a rename sends only the name", async () => {
    mockApi.patchProvider.mockResolvedValue({ provider: provider() });
    await renderCard();
    const row = await openEdit();
    fireEvent.change(within(row).getByLabelText("Provider name"), { target: { value: "  renamed " } });
    fireEvent.click(within(row).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.patchProvider).toHaveBeenCalledTimes(1));
    expect(mockApi.patchProvider.mock.calls[0]).toEqual(["p1", { name: "renamed" }]);
  });

  it("a model change sends only the model map", async () => {
    mockApi.patchProvider.mockResolvedValue({ provider: provider() });
    await renderCard();
    const row = await openEdit();
    fireEvent.change(within(row).getByLabelText("Haiku context window"), { target: { value: "65536" } });
    fireEvent.click(within(row).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.patchProvider).toHaveBeenCalledTimes(1));
    expect(mockApi.patchProvider.mock.calls[0]).toEqual([
      "p1",
      { model_map: { haiku: { model: "gw-small", context_window: 65536 }, sonnet: { model: "gw-large" } } },
    ]);
  });

  it("makes no request when nothing changed", async () => {
    await renderCard();
    const row = await openEdit();
    fireEvent.click(within(row).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(within(row).queryByRole("button", { name: "Save changes" })).toBeNull());
    expect(mockApi.patchProvider).not.toHaveBeenCalled();
  });

  it("endpoint edit sends the chosen URL and auth style", async () => {
    mockApi.patchProviderEndpoint.mockResolvedValue({ provider: provider() });
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Edit endpoint" }));
    fireEvent.change(within(row).getByLabelText("Base URL"), { target: { value: URL_B } });
    fireEvent.change(within(row).getByLabelText("Auth style"), { target: { value: "x_api_key" } });
    fireEvent.click(within(row).getByRole("button", { name: "Save endpoint" }));
    await waitFor(() =>
      expect(mockApi.patchProviderEndpoint).toHaveBeenCalledWith("p1", { base_url: URL_B, auth_style: "x_api_key" }),
    );
  });
});

describe("Providers — delete", () => {
  it("asks for confirmation, then shows a clear message on 409 provider_in_use_as_source", async () => {
    mockApi.deleteProvider.mockRejectedValue(apiError(409, "provider_in_use_as_source"));
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    fireEvent.click(within(row).getByRole("button", { name: "Delete" }));
    expect(mockApi.deleteProvider).not.toHaveBeenCalled();
    fireEvent.click(within(row).getByRole("button", { name: "Confirm delete" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/still in use as a model source/);
  });
});

describe("Providers — Claude source (PRD #4320 M7)", () => {
  const USE = { name: "Use as Claude source" };

  it("shows Anthropic as the current source and the redirect warning by default", async () => {
    await renderCard();
    const box = await screen.findByTestId("claude-source");
    expect(box.textContent).toMatch(/Current Claude source: Anthropic/);
    expect(box.textContent).toMatch(/Use only a gateway you trust not to redirect/);
    expect(box.textContent).toMatch(/your key to another path on the same host/);
    expect(box.textContent).toMatch(/Bearer auth limits this/);
    expect(box.textContent).not.toMatch(/Only allowlist gateways/);
    expect(box.textContent).not.toMatch(/Bearer auth is safer/);
    expect(box.textContent).not.toMatch(/wait in the queue until workers support providers/);
  });

  it("selects a provider, then reloads and shows it as the source with Stop using", async () => {
    mockApi.setClaudeProviderSource.mockResolvedValue({ claude_provider_id: "p1" });
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: "p1" } } as never);
    fireEvent.click(within(row).getByRole("button", USE));
    await waitFor(() => expect(mockApi.setClaudeProviderSource).toHaveBeenCalledWith("p1"));
    await waitFor(() => expect(screen.getByTestId("claude-source").textContent).toMatch(/Current Claude source: team-gateway/));
    expect(within(row).getByText("Claude source")).toBeTruthy();
    expect(within(row).queryByRole("button", USE)).toBeNull();
  });

  it("shows the PUT response's source even when the settings reload fails", async () => {
    mockApi.setClaudeProviderSource.mockResolvedValue({ claude_provider_id: "p1" });
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    mockApi.getMySettings.mockRejectedValue(new Error("boom"));
    fireEvent.click(within(row).getByRole("button", USE));
    await waitFor(() => expect(mockApi.getMySettings).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByTestId("claude-source").textContent).toMatch(/Current Claude source: team-gateway/));
    expect(within(row).getByText("Claude source")).toBeTruthy();
  });

  it("disables Delete on the current Claude source and says why", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: "p1" } } as never);
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    await waitFor(() => expect(screen.getByTestId("delete-blocked-p1").textContent).toMatch(/Stop using it before you delete it/));
    const del = within(row).getByRole("button", { name: "Delete" }) as HTMLButtonElement;
    expect(del.disabled).toBe(true);
    expect(del.title).toMatch(/Stop using it before you delete it/);
  });

  it("Stop using reverts to Anthropic", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: "p1" } } as never);
    mockApi.setClaudeProviderSource.mockResolvedValue({ claude_provider_id: null });
    await renderCard();
    const row = await screen.findByTestId("provider-p1");
    await waitFor(() => expect(screen.getByTestId("claude-source").textContent).toMatch(/team-gateway/));
    mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: null } } as never);
    fireEvent.click(within(row).getByRole("button", { name: "Stop using Claude source" }));
    await waitFor(() => expect(mockApi.setClaudeProviderSource).toHaveBeenCalledWith(null));
    await waitFor(() => expect(screen.getByTestId("claude-source").textContent).toMatch(/Anthropic/));
  });

  it.each([
    ["retired", { retired: true }, /retired provider cannot be your model source/],
    ["keyless", { has_key: false, key_enabled: false }, /Set a key before using/],
    ["key disabled", { key_enabled: false }, /Enable the key before using/],
  ])("disables Use as Claude source for a %s provider and says why", async (_n, over, reason) => {
    await renderCard(list({ providers: [provider(over)] }));
    const row = await screen.findByTestId("provider-p1");
    const btn = within(row).getByRole("button", USE) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.title).toMatch(reason);
    expect(screen.getByTestId("source-blocked-p1").textContent).toMatch(reason);
  });

  it("offers no source controls when the feature is off and Anthropic is selected", async () => {
    await renderCard(list({ enabled: false, allowed_base_urls: [], providers: [] }));
    expect(screen.queryByTestId("claude-source")).toBeNull();
    expect(screen.queryByRole("button", USE)).toBeNull();
  });

  it("can stop using a selected source while the feature is off", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: "p1" } } as never);
    mockApi.setClaudeProviderSource.mockResolvedValue({ claude_provider_id: null });
    await renderCard(list({ enabled: false, allowed_base_urls: [], providers: [] }));
    const stop = await screen.findByRole("button", { name: "Stop using Claude source" });
    // A failed refresh must not hide the successful clear returned by the PUT.
    mockApi.getMySettings.mockRejectedValue(new Error("refresh unavailable"));
    fireEvent.click(stop);
    await waitFor(() => expect(mockApi.setClaudeProviderSource).toHaveBeenCalledWith(null));
    expect(await screen.findByText("Claude runs use the native source again.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("button", { name: "Stop using Claude source" })).toBeNull());
    expect(screen.queryByRole("button", USE)).toBeNull();
    expect(screen.queryByRole("button", { name: "Add provider" })).toBeNull();
  });

  it("keeps the feature-off clear control available when clearing fails", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: "p1" } } as never);
    mockApi.setClaudeProviderSource.mockRejectedValue(new Error("clear unavailable"));
    await renderCard(list({ enabled: false, allowed_base_urls: [], providers: [] }));
    fireEvent.click(await screen.findByRole("button", { name: "Stop using Claude source" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    const retry = screen.getByRole("button", { name: "Stop using Claude source" }) as HTMLButtonElement;
    await waitFor(() => expect(retry.disabled).toBe(false));
    expect(mockApi.setClaudeProviderSource).toHaveBeenCalledWith(null);
  });

  it("surfaces a provider_source_unusable refusal", async () => {
    mockApi.setClaudeProviderSource.mockRejectedValue(apiError(409, "provider_source_unusable"));
    await renderCard();
    fireEvent.click(within(await screen.findByTestId("provider-p1")).getByRole("button", USE));
    expect((await screen.findByRole("alert")).textContent).toMatch(/cannot be your model source/);
  });

  it("still renders the list when the settings read fails", async () => {
    mockApi.getMySettings.mockRejectedValue(new Error("boom"));
    await renderCard();
    expect(await screen.findByTestId("provider-p1")).toBeTruthy();
    expect(screen.getByTestId("claude-source").textContent).toMatch(/unknown/);
  });
});


it("creates all eight Codex mappings and resets incompatible engine drafts", async () => {
  await renderCard(list({ providers: [] }));
  const f = within(screen.getByRole("form", { name: "Add a provider" }));
  fireEvent.change(f.getByLabelText("Haiku model (required)"), { target: { value: "old" } });
  fireEvent.change(f.getByLabelText("Auth style"), { target: { value: "x_api_key" } });
  fireEvent.change(f.getByLabelText("Engine"), { target: { value: "codex" } });
  expect(f.queryByLabelText("Haiku model (required)")).toBeNull();
  expect((f.getByLabelText("Auth style") as HTMLSelectElement).options.length).toBe(1);
  fireEvent.change(f.getByLabelText("Provider name"), { target: { value: "code" } });
  fireEvent.change(f.getByLabelText("Base URL"), { target: { value: URL_A } });
  fireEvent.click(f.getByRole("button", { name: "Add provider" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Default model is required");
  const map: Record<string, { model: string; context_window: number }> = {};
  for (const [role, label] of [["default", "Default"], ...["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"].map(r => [r, r])]) {
    fireEvent.change(f.getByLabelText(label + " model" + (role === "default" ? " (required)" : "")), { target: { value: "gw-" + role } });
    if (role === "default") expect(screen.getByTestId("ctx-warning-default").textContent).toContain("runtime fallback");
    fireEvent.change(f.getByLabelText(label + " context window"), { target: { value: "32768" } });
    map[role] = { model: "gw-" + role, context_window: 32768 };
  }
  fireEvent.click(f.getByRole("button", { name: "Add provider" }));
  await waitFor(() => expect(mockApi.createProvider).toHaveBeenCalledWith({ engine: "codex", protocol: "openai_responses", name: "code", base_url: URL_A, auth_style: "bearer", model_map: map }));
});

it("edits Codex maps without changing engine and selects only the Codex source", async () => {
  const code = provider({ id: "code", name: "code", engine: "codex", protocol: "openai_responses", model_map: { default: { model: "gw", context_window: 32768 }, "gpt-6-luna": { model: "moon", context_window: 65536 }, "gpt-6.2-sol": { model: "sun", context_window: 100000 }, "gpt-6.2-astra": { model: "star", context_window: 200000 } } });
  mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: "p1", codex_provider_id: null } } as never);
  mockApi.setCodexProviderSource.mockResolvedValue({ codex_provider_id: "code" });
  await renderCard(list({ providers: [provider(), code] }));
  const row = within(await screen.findByTestId("provider-code"));
  fireEvent.click(row.getByRole("button", { name: "Edit name and models" }));
  expect(row.queryByLabelText("Engine")).toBeNull();
  expect((row.getByLabelText("gpt-6-luna context window") as HTMLInputElement).value).toBe("65536");
  for (const [slot, context] of [["gpt-6.2-sol", "100000"], ["gpt-6.2-astra", "200000"]]) {
    expect((row.getByLabelText(slot + " context window") as HTMLInputElement).value).toBe(context);
  }
  fireEvent.change(row.getByLabelText("Default context window"), { target: { value: "8192" } });
  fireEvent.click(row.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(mockApi.patchProvider).toHaveBeenCalledWith("code", { model_map: { default: { model: "gw", context_window: 8192 }, "gpt-6-luna": { model: "moon", context_window: 65536 }, "gpt-6.2-sol": { model: "sun", context_window: 100000 }, "gpt-6.2-astra": { model: "star", context_window: 200000 } } }));
  mockApi.getMySettings.mockRejectedValue(new Error("advisory unavailable"));
  fireEvent.click(row.getByRole("button", { name: "Use as Codex source" }));
  await waitFor(() => expect(mockApi.setCodexProviderSource).toHaveBeenCalledWith("code"));
  await waitFor(() => expect(screen.getByTestId("codex-source").textContent).toContain("code"));
  expect(screen.getByTestId("claude-source").textContent).toContain("team-gateway");
  expect((row.getByRole("button", { name: "Delete" }) as HTMLButtonElement).disabled).toBe(true);
});

it("clears each feature-off source independently", async () => {
  mockApi.getMySettings.mockResolvedValue({ settings: { claude_provider_id: "p1", codex_provider_id: "c1" } } as never);
  mockApi.setCodexProviderSource.mockResolvedValue({ codex_provider_id: null });
  await renderCard(list({ enabled: false }));
  const stop = await screen.findByRole("button", { name: "Stop using Codex source" });
  mockApi.getMySettings.mockRejectedValue(new Error("advisory unavailable"));
  fireEvent.click(stop);
  await waitFor(() => expect(screen.queryByRole("button", { name: "Stop using Codex source" })).toBeNull());
  expect(screen.getByRole("button", { name: "Stop using Claude source" })).toBeTruthy();
});
