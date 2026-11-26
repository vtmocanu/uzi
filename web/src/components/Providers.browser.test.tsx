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
    putProviderKey: vi.fn(), testProvider: vi.fn(), setCodexProviderSource: vi.fn(), setClaudeProviderSource: vi.fn(),
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
    const provider: ProviderDTO = { ...input, engine: "codex", protocol: "openai_responses", id: "code", retired: false, current_revision: 1, credential_mode: null, has_key: false, key_enabled: false, probe: null };
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

// PRD #4324: an admin-shared x-api-key definition at phone width. The custody disclosure must
// stack its two columns and never overflow the row; at desktop width they sit side by side.
// Attach, then select through the disclosure, with no owner action anywhere on the row.
it("lays out the shared-provider disclosure at 390px and 1100px, then attaches and selects", async () => {
  const mock = vi.mocked(api);
  const url = "https://gateway.example.com/a/very/long/path/that/must/wrap/inside/the/disclosure/v1";
  let row: ProviderDTO = {
    id: "s1", name: "platform-gateway", engine: "claude", retired: false, current_revision: 2,
    protocol: "anthropic_messages", base_url: url, auth_style: "x_api_key",
    model_map: { haiku: { model: "platform-small", context_window: 65536 } },
    credential_mode: "per_user_key", has_key: false, key_enabled: false, probe: null,
  };
  let claude: string | null = null;
  mock.listProviders.mockImplementation(async () => ({ enabled: true, allowed_base_urls: [url], providers: [row] }));
  mock.getMySettings.mockImplementation(async () => ({ settings: { claude_provider_id: claude, codex_provider_id: null } }) as never);
  mock.putProviderKey.mockImplementation(async () => { row = { ...row, has_key: true, key_enabled: true }; return { provider: row }; });
  mock.setClaudeProviderSource.mockImplementation(async id => { claude = id; return { claude_provider_id: id }; });

  await page.viewport(390, 844);
  render(<div className="px-4"><Providers /></div>);
  const r = within(await screen.findByTestId("provider-s1"));
  expect(r.getByText("Managed by your admin")).toBeTruthy();
  for (const name of ["Edit name and models", "Edit endpoint", "Retire", "Delete"]) expect(r.queryByRole("button", { name })).toBeNull();
  await userEvent.click(r.getByRole("button", { name: "Set key" }));
  const box = r.getByRole("region", { name: "Before you attach your key" });
  const [admin, mine] = Array.from(box.querySelectorAll("h4")).map(h => h.parentElement!.getBoundingClientRect());
  expect(mine.top).toBeGreaterThanOrEqual(admin.bottom - 1); // stacked on a phone
  expect(box.scrollWidth).toBeLessThanOrEqual(box.clientWidth + 1);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
  expect(r.getByTestId("shared-disclosure-xapikey-s1")).toBeTruthy();

  await page.viewport(1100, 900);
  await waitFor(() => {
    const [a, m] = Array.from(box.querySelectorAll("h4")).map(h => h.parentElement!.getBoundingClientRect());
    expect(Math.abs(a.top - m.top)).toBeLessThan(2); // side by side on a desktop
    expect(m.left).toBeGreaterThan(a.right - 1);
  });

  await userEvent.fill(r.getByLabelText("Provider key"), "k".repeat(24));
  await userEvent.click(r.getByRole("button", { name: "Save key" }));
  await waitFor(() => expect(r.queryByLabelText("Provider key")).toBeNull());
  await userEvent.click(r.getByRole("button", { name: "Use as Claude source" }));
  expect(mock.setClaudeProviderSource).not.toHaveBeenCalled();
  await r.findByRole("region", { name: "Before you use this provider" });
  await userEvent.click(r.getByRole("button", { name: "Confirm and use as Claude source" }));
  await waitFor(() => expect(screen.getByTestId("claude-source").textContent).toContain("platform-gateway (managed by your admin)"));
  expect(claude).toBe("s1");
});

// Issue #4633: a shared Codex definition in the Codex picker. Opening the select panel moves
// focus onto the disclosure (Codex redirect wording, no x-api-key line); one Tab reaches
// Confirm, and Enter selects it as the Codex source only.
it("focuses the shared Codex disclosure and selects it as the Codex source on Confirm", async () => {
  await page.viewport(390, 844);
  const mock = vi.mocked(api);
  const row: ProviderDTO = {
    id: "sc", name: "shared-codex", engine: "codex", retired: false, current_revision: 1,
    protocol: "openai_responses", base_url: "https://gateway.example.com/v1", auth_style: "bearer",
    model_map: { default: { model: "gw-code" } },
    credential_mode: "per_user_key", has_key: true, key_enabled: true, probe: null,
  };
  let codex: string | null = null;
  mock.listProviders.mockImplementation(async () => ({ enabled: true, allowed_base_urls: [row.base_url], providers: [row] }));
  mock.getMySettings.mockImplementation(async () => ({ settings: { claude_provider_id: null, codex_provider_id: codex } }) as never);
  mock.setCodexProviderSource.mockImplementation(async id => { codex = id; return { codex_provider_id: id }; });
  // Earlier cases in this file select sources too; count only this case's calls.
  mock.setClaudeProviderSource.mockClear();
  mock.setCodexProviderSource.mockClear();
  render(<div className="px-4"><Providers /></div>);
  const r = within(await screen.findByTestId("provider-sc"));
  await userEvent.click(r.getByRole("button", { name: "Use as Codex source" }));
  const box = await r.findByRole("region", { name: "Before you use this provider" });
  expect(document.activeElement).toBe(box);
  expect(r.getByTestId("shared-disclosure-redirect-sc").textContent).toMatch(/Codex runtime redirects may forward request bodies/);
  expect(r.queryByTestId("shared-disclosure-xapikey-sc")).toBeNull();
  expect(box.scrollWidth).toBeLessThanOrEqual(box.clientWidth + 1);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
  await userEvent.tab();
  const confirm = r.getByRole("button", { name: "Confirm and use as Codex source" });
  expect(document.activeElement).toBe(confirm);
  expect(mock.setCodexProviderSource).not.toHaveBeenCalled();
  await userEvent.keyboard("{Enter}");
  await waitFor(() => expect(screen.getByTestId("codex-source").textContent).toContain("shared-codex (managed by your admin)"));
  expect(codex).toBe("sc");
  expect(mock.setClaudeProviderSource).not.toHaveBeenCalled();
});
