// @vitest-environment jsdom
//
// PRD #4540 M3 mock fidelity: organization-funded (operator_key) definitions live in a store of
// their own. The demo user's provider list never shows them, the owner routes answer 404 for
// them, and only the admin routes see and manage them, as on the server.

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

const OP = "prov-operator-org";
const GW = "https://gateway.example.com/v1";
const MAP = { haiku: { model: "org-small" } };

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => {
  vi.useRealTimers();
  window.localStorage.clear();
});

describe("mock organization-funded definitions", () => {
  it("keeps operator rows out of the user list and answers 404 on every owner route", async () => {
    const api = await load();
    const mine = (await api.listProviders()).providers.map((p) => p.id);
    expect(mine).not.toContain(OP);
    await expect(api.patchProvider(OP, { name: "x" })).rejects.toMatchObject({ status: 404 });
    await expect(api.retireProvider(OP)).rejects.toMatchObject({ status: 404 });
    await expect(api.deleteProvider(OP)).rejects.toMatchObject({ status: 404 });
    await expect(api.putProviderKey(OP, "k")).rejects.toMatchObject({ status: 404 });
    await expect(api.testProvider(OP)).rejects.toMatchObject({ status: 404 });
    await expect(api.setClaudeProviderSource(OP)).rejects.toMatchObject({ status: 404 });
  });

  it("lists the seeded operator definition for the admin with its mode and key state, never a key", async () => {
    const api = await load();
    const row = (await api.adminListSharedProviders()).providers.find((p) => p.id === OP)!;
    expect(row.credential_mode).toBe("operator_key");
    expect(row.operator_key).toMatchObject({ present: true, enabled: true });
    expect(JSON.stringify(row)).not.toMatch(/"key"/);
    const perUser = (await api.adminListSharedProviders()).providers.find((p) => p.id === "prov-shared-platform")!;
    expect(perUser.credential_mode).toBe("per_user_key");
    expect(perUser.operator_key).toBeUndefined();
  });

  it("creates an operator definition only for Claude with the acknowledgment", async () => {
    const api = await load();
    const base = { name: "new-org", base_url: GW, auth_style: "bearer" as const, model_map: MAP };
    await expect(api.adminCreateSharedProvider({ ...base, engine: "claude", credential_mode: "operator_key" })).rejects.toMatchObject({ status: 400 });
    await expect(
      api.adminCreateSharedProvider({ ...base, engine: "codex", model_map: { default: { model: "m" } }, credential_mode: "operator_key", acknowledge_recipient_trust: true }),
    ).rejects.toMatchObject({ status: 400 });
    const { provider } = await api.adminCreateSharedProvider({ ...base, engine: "claude", credential_mode: "operator_key", acknowledge_recipient_trust: true });
    expect(provider.credential_mode).toBe("operator_key");
    expect(provider.operator_key).toMatchObject({ present: false, enabled: false });
    expect((await api.listProviders()).providers.map((p) => p.id)).not.toContain(provider.id);
    await expect(api.adminCreateSharedProvider({ ...base, engine: "claude" })).rejects.toMatchObject({ status: 409, body: { reason: "provider_name_taken" } });
  });

  it("sets, disables, tests and deletes the key through the admin routes", async () => {
    const api = await load();
    expect((await api.adminSetOperatorKeyEnabled(OP, false)).provider.operator_key).toMatchObject({ present: true, enabled: false });
    await expect(api.adminTestOperatorKey(OP)).rejects.toMatchObject({ status: 409 });
    expect((await api.adminSetOperatorKeyEnabled(OP, true)).provider.operator_key?.enabled).toBe(true);
    const { probe } = await api.adminTestOperatorKey(OP);
    expect(probe.auth_result).toBe("ok");
    const put = (await api.adminPutOperatorKey(OP, "replacement")).provider;
    expect(put.operator_key).toMatchObject({ present: true, enabled: true });
    expect(put.operator_key?.probe).toBeUndefined();
    const del = (await api.adminDeleteOperatorKey(OP)).provider;
    expect(del.operator_key).toMatchObject({ present: false, enabled: false });
    await expect(api.adminTestOperatorKey(OP)).rejects.toMatchObject({ status: 409 });
  });

  it("refuses the key routes for a per-user definition with provider_not_operator_key", async () => {
    const api = await load();
    const id = "prov-shared-platform";
    for (const call of [
      () => api.adminPutOperatorKey(id, "k"),
      () => api.adminSetOperatorKeyEnabled(id, false),
      () => api.adminDeleteOperatorKey(id),
      () => api.adminTestOperatorKey(id),
    ]) {
      await expect(call()).rejects.toMatchObject({ status: 409, body: { reason: "provider_not_operator_key" } });
    }
    await expect(api.adminPutOperatorKey("nope", "k")).rejects.toMatchObject({ status: 404 });
  });

  it("deletes an operator definition from the admin list", async () => {
    const api = await load();
    await expect(api.adminDeleteSharedProvider(OP)).resolves.toEqual({ deleted: true, deleted_key_count: 0 });
    expect((await api.adminListSharedProviders()).providers.map((p) => p.id)).not.toContain(OP);
  });
});
