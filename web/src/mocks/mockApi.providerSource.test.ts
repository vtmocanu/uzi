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

// PRD #4324: the demo's admin-shared definitions behave like the server's. Owner routes are a
// 404 for them, the user attaches only their own key, and the admin delete is guarded by the
// selection and reports the keys it removed.
describe("mock admin-shared providers", () => {
  const SHARED = "prov-shared-platform";

  it("lists shared rows to the user, refuses owner routes and keeps key actions", async () => {
    const api = await load();
    const row = (await api.listProviders()).providers.find(p => p.id === SHARED)!;
    expect(row.credential_mode).toBe("per_user_key");
    expect((await api.listProviders()).providers.find(p => p.id === "prov-gateway")!.credential_mode).toBeNull();
    await expect(api.patchProvider(SHARED, { name: "x" })).rejects.toMatchObject({ status: 404 });
    await expect(api.patchProviderEndpoint(SHARED, { auth_style: "bearer" })).rejects.toMatchObject({ status: 404 });
    await expect(api.retireProvider(SHARED)).rejects.toMatchObject({ status: 404 });
    await expect(api.deleteProvider(SHARED)).rejects.toMatchObject({ status: 404 });
    await api.putProviderKey(SHARED, "k".repeat(24));
    await api.testProvider(SHARED);
    expect((await api.listProviders()).providers.find(p => p.id === SHARED)!.probe).not.toBeNull();
    // A rotation makes the old Test result unknown.
    await api.putProviderKey(SHARED, "r".repeat(24));
    expect((await api.listProviders()).providers.find(p => p.id === SHARED)!.probe).toBeNull();
    await expect(api.setClaudeProviderSource(SHARED)).resolves.toEqual({ claude_provider_id: SHARED });
  });

  it("counts the demo user's selection and key into the admin view and guards delete", async () => {
    const api = await load();
    await api.putProviderKey(SHARED, "k".repeat(24));
    await api.setClaudeProviderSource(SHARED);
    const view = (await api.adminListSharedProviders()).providers;
    expect(view.map(p => p.id)).toEqual([SHARED, "prov-shared-legacy"]);
    expect(view[0]).toMatchObject({ selected_user_count: 3, attached_key_count: 4 });
    expect(JSON.stringify(view)).not.toContain("k".repeat(24));
    await expect(api.adminDeleteSharedProvider(SHARED)).rejects.toMatchObject({ status: 409, body: { reason: "provider_in_use_as_source" } });
    await expect(api.adminDeleteSharedProvider("prov-shared-legacy")).resolves.toEqual({ deleted: true, deleted_key_count: 1 });
    expect((await api.listProviders()).providers.some(p => p.id === "prov-shared-legacy")).toBe(false);
    await expect(api.adminDeleteSharedProvider("prov-gateway")).rejects.toMatchObject({ status: 404 });
  });

  it("creates keyless, refuses a case-insensitive name collision, edits and retires", async () => {
    const api = await load();
    const { provider } = await api.adminCreateSharedProvider({
      name: "new-gw", base_url: "https://gateway.example.com/v1", auth_style: "bearer", model_map: { haiku: { model: "gw" } },
    });
    expect(provider).toMatchObject({ selected_user_count: 0, attached_key_count: 0, credential_mode: "per_user_key" });
    expect((await api.listProviders()).providers.find(p => p.id === provider.id)).toMatchObject({ has_key: false, credential_mode: "per_user_key" });
    await expect(api.adminCreateSharedProvider({
      name: "PLATFORM-gateway", base_url: "https://gateway.example.com/v1", auth_style: "bearer", model_map: { haiku: { model: "gw" } },
    })).rejects.toMatchObject({ status: 409, body: { reason: "provider_name_taken" } });
    await expect(api.adminPatchSharedProvider(provider.id, { name: "Platform-Gateway" })).rejects.toMatchObject({ status: 409 });
    const edited = (await api.adminPatchSharedProvider(provider.id, { model_map: { haiku: { model: "gw2" } } })).provider;
    expect(edited).toMatchObject({ current_revision: 2, model_map: { haiku: { model: "gw2" } } });
    await api.adminRetireSharedProvider(provider.id);
    await expect(api.adminPatchSharedProvider(provider.id, { name: "z" })).rejects.toMatchObject({ status: 409, body: { reason: "provider_retired" } });
  });

  // PRD #4324: retirement never clears a source or falls back. The selection stays (unusable),
  // a re-select is refused, and only an explicit clear moves the user off it.
  it("keeps a retired selected definition as the source and refuses a re-select", async () => {
    const api = await load();
    await api.putProviderKey(SHARED, "k".repeat(24));
    await api.setClaudeProviderSource(SHARED);
    await api.adminRetireSharedProvider(SHARED);
    expect((await api.getMySettings()).settings.claude_provider_id).toBe(SHARED);
    expect((await api.listProviders()).providers.find(p => p.id === SHARED)).toMatchObject({ retired: true });
    await expect(api.setClaudeProviderSource(SHARED)).rejects.toMatchObject({
      status: 409,
      body: { reason: "provider_source_unusable" },
    });
    expect((await api.getMySettings()).settings.claude_provider_id).toBe(SHARED);
    await expect(api.setClaudeProviderSource(null)).resolves.toEqual({ claude_provider_id: null });
    expect((await api.getMySettings()).settings.claude_provider_id).toBeNull();
  });
});
