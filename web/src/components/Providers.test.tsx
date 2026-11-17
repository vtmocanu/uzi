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
    expect(within(first).getByText(/\(no context window\)/)).toBeTruthy();
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
    expect(screen.getByTestId("ctx-warning-haiku").textContent).toMatch(/assumes a 200k-token window/);
    fireEvent.change(within(form()).getByLabelText("Haiku context window"), { target: { value: "65536" } });
    expect(screen.queryByTestId("ctx-warning-haiku")).toBeNull();
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
    mockApi.createProvider.mockResolvedValue(provider());
    await renderCard(list({ providers: [] }));
    fireEvent.change(within(form()).getByLabelText("Provider name"), { target: { value: "gw" } });
    fireEvent.change(within(form()).getByLabelText("Base URL"), { target: { value: URL_B } });
    fireEvent.change(within(form()).getByLabelText("Auth style"), { target: { value: "x_api_key" } });
    fireEvent.change(within(form()).getByLabelText("Haiku model (required)"), { target: { value: "gw-small" } });
    fireEvent.change(within(form()).getByLabelText("Haiku context window"), { target: { value: "65536" } });
    fireEvent.click(within(form()).getByRole("button", { name: "Add provider" }));
    await waitFor(() =>
      expect(mockApi.createProvider).toHaveBeenCalledWith({
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
    mockApi.putProviderKey.mockResolvedValue(provider());
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
