import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, within, waitFor } from "@testing-library/react";
import { Providers } from "./Providers";
import { api, type ProviderDTO } from "../lib/api";

vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: {
    listProviders: vi.fn(), getMySettings: vi.fn(), createProvider: vi.fn(),
    putProviderKey: vi.fn(), testProvider: vi.fn(), setCodexProviderSource: vi.fn(),
  } };
});
afterEach(cleanup);

it("creates, keys, tests, selects and clears Codex while preserving Claude", async () => {
  await page.viewport(1100, 900);
  const mock = vi.mocked(api);
  const url = "https://gateway.example/v1";
  let providers: ProviderDTO[] = [];
  let codex: string | null = null;
  mock.listProviders.mockImplementation(async () => ({ enabled: true, allowed_base_urls: [url], providers }));
  mock.getMySettings.mockImplementation(async () => ({ settings: { claude_provider_id: "claude-existing", codex_provider_id: codex } }) as never);
  mock.createProvider.mockImplementation(async input => {
    const provider: ProviderDTO = { ...input, engine: "codex", protocol: "openai_responses", id: "code", retired: false, current_revision: 1, has_key: false, key_enabled: false, probe: null };
    providers = [provider];
    return { provider };
  });
  mock.putProviderKey.mockImplementation(async () => {
    providers = [{ ...providers[0], has_key: true, key_enabled: true }];
    return { provider: providers[0] };
  });
  mock.testProvider.mockResolvedValue({ probe: { auth_result: "ok", tool_result: "passed", probed_at: "2026-11-21T00:00:00Z" } });
  mock.setCodexProviderSource.mockImplementation(async id => { codex = id; return { codex_provider_id: id }; });
  const { container } = render(<Providers />);
  const form = within(await screen.findByRole("form", { name: "Add a provider" }));
  await userEvent.selectOptions(form.getByLabelText("Engine"), "codex");
  await userEvent.fill(form.getByLabelText("Provider name"), "Code gateway");
  await userEvent.selectOptions(form.getByLabelText("Base URL"), url);
  await userEvent.fill(form.getByLabelText("Default model (required)"), "gw-code");
  await userEvent.fill(form.getByLabelText("Default context window"), "32768");
  await userEvent.click(form.getByRole("button", { name: "Add provider" }));
  const row = within(await screen.findByTestId("provider-code"));
  await userEvent.click(row.getByRole("button", { name: "Set key" }));
  const key = row.getByLabelText("Provider key") as HTMLInputElement;
  expect(key.type).toBe("password");
  const secret = "k".repeat(24);
  await userEvent.fill(key, secret);
  await userEvent.click(row.getByRole("button", { name: "Save key" }));
  await waitFor(() => expect(row.queryByLabelText("Provider key")).toBeNull());
  expect(key.value).toBe("");
  expect(container.innerHTML).not.toContain(secret);
  await userEvent.click(row.getByRole("button", { name: "Test" }));
  await row.findByText("Authentication worked");
  await userEvent.click(row.getByRole("button", { name: "Use as Codex source" }));
  await waitFor(() => expect(screen.getByTestId("codex-source").textContent).toContain("Code gateway"));
  expect((row.getByRole("button", { name: "Delete" }) as HTMLButtonElement).disabled).toBe(true);
  await userEvent.click(row.getByRole("button", { name: "Stop using Codex source" }));
  await waitFor(() => expect(screen.getByTestId("codex-source").textContent).toContain("OpenAI"));
  expect(screen.getByTestId("claude-source").textContent).toContain("a provider that no longer exists");
  expect(codex).toBeNull();
  expect(mock.putProviderKey).toHaveBeenCalledWith("code", secret);
});
