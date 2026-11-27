// @vitest-environment jsdom
//
// PRD #4574 M2 mock fidelity: the plan and code cross-check consent toggles follow the server's
// BothHarnessesUsable lane rule. A selected usable provider source makes its engine usable (even
// without a vendor credential); a selected but unusable source gives 409 (even when the vendor
// credential is usable); with no selected source the vendor credential decides, as before.

import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => {
  vi.resetModules();
  window.localStorage.clear();
});

async function load() {
  vi.resetModules();
  window.localStorage.setItem("uzi_mock_scenario", "providers");
  const { mockApi } = await import("./mockApi");
  const { secrets } = await import("./mockApi/secrets");
  return { api: mockApi, secrets };
}

// Drops every vendor Claude and Codex credential, in place (the module binding is live).
function dropVendorCredentials(secrets: { kind: string }[]): void {
  for (let i = secrets.length - 1; i >= 0; i--) {
    if (["anthropic_token", "codex_auth", "openai_api_key"].includes(secrets[i].kind)) secrets.splice(i, 1);
  }
}

async function selectBothSources(api: Awaited<ReturnType<typeof load>>["api"]): Promise<string> {
  const { provider } = await api.createProvider({
    engine: "codex",
    name: "codex-gw",
    base_url: "https://gateway.example.com/v1",
    auth_style: "bearer",
    model_map: { default: { model: "gw" } },
  });
  await api.putProviderKey(provider.id, "c".repeat(24));
  await api.setCodexProviderSource(provider.id);
  await api.setClaudeProviderSource("prov-gateway");
  return provider.id;
}

const toggles = [
  ["plan", (api: Awaited<ReturnType<typeof load>>["api"], on: boolean) => api.setPlanCrossCheckEnabled(on)],
  ["code", (api: Awaited<ReturnType<typeof load>>["api"], on: boolean) => api.setCodeCrossCheckEnabled(on)],
] as const;

describe.each(toggles)("%s cross-check toggle and provider sources", (_stage, toggle) => {
  it("accepts selected usable sources with no vendor credential", async () => {
    const { api, secrets } = await load();
    dropVendorCredentials(secrets);
    await expect(toggle(api, true)).rejects.toMatchObject({ status: 409 });
    await selectBothSources(api);
    await expect(toggle(api, true)).resolves.toMatchObject({ user: expect.any(Object) });
  });

  it("refuses a selected unusable Claude source even with a usable vendor credential", async () => {
    const { api } = await load();
    await expect(toggle(api, true)).resolves.toBeDefined();
    await selectBothSources(api);
    await api.setProviderKeyEnabled("prov-gateway", false);
    await expect(toggle(api, true)).rejects.toMatchObject({ status: 409 });
  });

  it("refuses a selected unusable Codex source even with a usable vendor credential", async () => {
    const { api } = await load();
    const codexId = await selectBothSources(api);
    await api.deleteProviderKey(codexId);
    await expect(toggle(api, true)).rejects.toMatchObject({ status: 409 });
  });

  it("keeps the no-source rule: the vendor credentials decide", async () => {
    const { api, secrets } = await load();
    await expect(toggle(api, true)).resolves.toBeDefined();
    dropVendorCredentials(secrets);
    await expect(toggle(api, true)).rejects.toMatchObject({ status: 409 });
    await expect(toggle(api, false)).resolves.toBeDefined();
  });
});
