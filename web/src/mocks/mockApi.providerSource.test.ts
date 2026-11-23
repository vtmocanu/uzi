// @vitest-environment jsdom
//
// PRD #4320 M7 mock fidelity: the demo's Claude-source selection mirrors
// handler.SetMyClaudeProviderSource (409 provider_source_unusable for an unusable provider,
// null clears, an unknown id is a 404) and GET /me/settings reflects the selection.
// The demo build is not meant to differ from the server: delete of the selected provider is
// refused with provider_in_use_as_source, like the server.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

function installStorage(): void {
  const m = new Map<string, string>();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
      setItem: (k: string, v: string) => void m.set(k, String(v)),
      removeItem: (k: string) => void m.delete(k),
      clear: () => m.clear(),
      key: (i: number) => [...m.keys()][i] ?? null,
      get length() {
        return m.size;
      },
    } as Storage,
  });
}

async function load() {
  vi.resetModules();
  installStorage();
  window.localStorage.setItem("uzi_mock_scenario", "providers");
  return (await import("./mockApi")).mockApi;
}

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => {
  vi.useRealTimers();
  window.localStorage.clear();
});

describe("mock claude provider source", () => {
  it("starts on Anthropic, selects a usable provider and reflects it in settings", async () => {
    const api = await load();
    expect((await api.getMySettings()).settings.claude_provider_id).toBeNull();
    await expect(api.setClaudeProviderSource("prov-gateway")).resolves.toEqual({ claude_provider_id: "prov-gateway" });
    expect((await api.getMySettings()).settings.claude_provider_id).toBe("prov-gateway");
    await expect(api.setClaudeProviderSource(null)).resolves.toEqual({ claude_provider_id: null });
    expect((await api.getMySettings()).settings.claude_provider_id).toBeNull();
  });

  it("refuses an unknown provider (404) and an unusable one (409 provider_source_unusable)", async () => {
    const api = await load();
    await expect(api.setClaudeProviderSource("nope")).rejects.toMatchObject({ status: 404 });
    await api.setProviderKeyEnabled("prov-gateway", false);
    await expect(api.setClaudeProviderSource("prov-gateway")).rejects.toMatchObject({
      status: 409,
      body: { reason: "provider_source_unusable" },
    });
    await api.setProviderKeyEnabled("prov-gateway", true);
    await api.retireProvider("prov-gateway");
    await expect(api.setClaudeProviderSource("prov-gateway")).rejects.toMatchObject({
      status: 409,
      body: { reason: "provider_source_unusable" },
    });
    expect((await api.getMySettings()).settings.claude_provider_id).toBeNull();
  });

  it("refuses to delete the selected provider until it is cleared", async () => {
    const api = await load();
    await api.setClaudeProviderSource("prov-gateway");
    await expect(api.deleteProvider("prov-gateway")).rejects.toMatchObject({
      status: 409,
      body: { reason: "provider_in_use_as_source" },
    });
    await api.setClaudeProviderSource(null);
    await expect(api.deleteProvider("prov-gateway")).resolves.toBeNull();
  });
});

it("roundtrips Codex mappings and keeps source selection independent with mismatch 404", async () => {
  const api = await load();
  const map = Object.fromEntries(["default", "gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"].map(role => [role, { model: "gw-" + role, context_window: 32768 }]));
  const { providers, allowed_base_urls } = await api.listProviders();
  const { provider: code } = await api.createProvider({ name: "code", engine: "codex", protocol: "openai_responses", base_url: allowed_base_urls[0], auth_style: "bearer", model_map: map });
  expect(code.model_map).toEqual(map);
  await api.putProviderKey(code.id, "k".repeat(24));
  await api.setClaudeProviderSource(providers[0].id);
  await api.setCodexProviderSource(code.id);
  await expect(api.setClaudeProviderSource(code.id)).rejects.toMatchObject({ status: 404 });
  await expect(api.setCodexProviderSource(providers[0].id)).rejects.toMatchObject({ status: 404 });
  await expect(api.setCodexProviderSource("unknown")).rejects.toMatchObject({ status: 404 });
  await expect(api.deleteProvider(code.id)).rejects.toMatchObject({ status: 409 });
  await api.setCodexProviderSource(null);
  const settings = (await api.getMySettings()).settings;
  expect(settings.codex_provider_id).toBeNull();
  expect(settings.claude_provider_id).toBe(providers[0].id);
  expect((await api.listProviders()).providers.find(p => p.id === code.id)?.model_map).toEqual(map);
  await expect(api.patchProviderEndpoint(code.id, { auth_style: "x_api_key" })).rejects.toMatchObject({ status: 400 });
  expect(JSON.stringify(await api.listProviders())).not.toContain("k".repeat(24));
});
