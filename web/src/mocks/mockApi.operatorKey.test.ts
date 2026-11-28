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
    ]) {
      await expect(call()).rejects.toMatchObject({ status: 409, body: { reason: "provider_not_operator_key" } });
    }
    // Test only reads operator-mode definitions, so a per-user one is not found.
    await expect(api.adminTestOperatorKey(id)).rejects.toMatchObject({ status: 404 });
    await expect(api.adminPutOperatorKey("nope", "k")).rejects.toMatchObject({ status: 404 });
  });

  it("deletes an operator definition from the admin list", async () => {
    const api = await load();
    await expect(api.adminDeleteSharedProvider(OP)).resolves.toEqual({ deleted: true, deleted_key_count: 0 });
    expect((await api.adminListSharedProviders()).providers.map((p) => p.id)).not.toContain(OP);
  });

  it("allows a key on a retired definition and keeps the enablement on replace", async () => {
    const api = await load();
    await api.adminSetOperatorKeyEnabled(OP, false);
    expect((await api.adminPutOperatorKey(OP, "next")).provider.operator_key).toMatchObject({ present: true, enabled: false });
    await api.adminRetireSharedProvider(OP);
    expect((await api.adminPutOperatorKey(OP, "again")).provider.operator_key).toMatchObject({ present: true, enabled: false });
    expect((await api.adminSetOperatorKeyEnabled(OP, true)).provider.operator_key?.enabled).toBe(true);
    await expect(api.adminTestOperatorKey(OP)).rejects.toMatchObject({ status: 409, body: { reason: "provider_retired" } });
  });

  it("answers a keyless enable, disable or delete with a bare 404 and a keyless Test with a bare 409", async () => {
    const api = await load();
    await api.adminDeleteOperatorKey(OP);
    for (const call of [() => api.adminSetOperatorKeyEnabled(OP, true), () => api.adminSetOperatorKeyEnabled(OP, false), () => api.adminDeleteOperatorKey(OP)]) {
      const err = await call().catch((e) => e);
      expect(err).toMatchObject({ status: 404, message: "the provider has no key" });
      expect(err.body.reason).toBeUndefined();
    }
    const t = await api.adminTestOperatorKey(OP).catch((e) => e);
    expect(t.status).toBe(409);
    expect(t.body.reason).toBeUndefined();
  });

  it("treats enabling or disabling to the current state as a no-op that keeps the Test result", async () => {
    const api = await load();
    await api.adminTestOperatorKey(OP);
    const same = (await api.adminSetOperatorKeyEnabled(OP, true)).provider.operator_key;
    expect(same?.probe).toBeDefined();
    const flipped = (await api.adminSetOperatorKeyEnabled(OP, false)).provider.operator_key;
    expect(flipped?.probe).toBeUndefined();
  });
});

// PRD #4540 M2 mock fidelity, admin side: recipient grants mirror the server's admin grant API.
describe("mock recipient grants", () => {
  it("starts a new definition with no recipients and reports both counts in the definition DTO", async () => {
    const api = await load();
    const { provider } = await api.adminCreateSharedProvider({
      engine: "claude", name: "fresh-org", base_url: GW, auth_style: "bearer", model_map: MAP, credential_mode: "operator_key", acknowledge_recipient_trust: true,
    });
    expect(provider).toMatchObject({ operator_all_users: false, named_grant_count: 0 });
    await expect(api.adminListProviderGrants(provider.id)).resolves.toEqual({ operator_all_users: false, named_grant_count: 0, grants: [], next_cursor: null });
    const perUser = (await api.adminListSharedProviders()).providers.find((p) => p.id === "prov-shared-platform")!;
    expect(perUser).toMatchObject({ operator_all_users: false, named_grant_count: 0 });
  });

  it("adds and removes grants idempotently and pages them by user id", async () => {
    const api = await load();
    const before = (await api.adminListProviderGrants(OP)).named_grant_count;
    await expect(api.adminPutProviderGrant(OP, "u-admin")).resolves.toBeNull();
    await expect(api.adminPutProviderGrant(OP, "u-admin")).resolves.toBeNull();
    const all = await api.adminListProviderGrants(OP);
    expect(all.named_grant_count).toBe(before + 1);
    const ids = all.grants.map((g) => g.user_id);
    expect(ids).toEqual([...ids].sort());
    const first = await api.adminListProviderGrants(OP, { limit: 1 });
    expect(first.grants).toHaveLength(1);
    expect(first.next_cursor).toBe(first.grants[0].user_id);
    const rest = await api.adminListProviderGrants(OP, { cursor: first.next_cursor!, limit: 500 });
    expect([...first.grants, ...rest.grants].map((g) => g.user_id)).toEqual(ids);
    expect(rest.next_cursor).toBeNull();
    await expect(api.adminListProviderGrants(OP, { limit: 501 })).rejects.toMatchObject({ status: 400 });
    await expect(api.adminDeleteProviderGrant(OP, "u-admin")).resolves.toBeNull();
    await expect(api.adminDeleteProviderGrant(OP, "u-admin")).resolves.toBeNull();
    expect((await api.adminListSharedProviders()).providers.find((p) => p.id === OP)!.named_grant_count).toBe(before);
  });

  it("refuses an unknown user with 404 user_not_found and a per-user definition with 409", async () => {
    const api = await load();
    await expect(api.adminPutProviderGrant(OP, "u-nobody")).rejects.toMatchObject({ status: 404, body: { reason: "user_not_found" } });
    const id = "prov-shared-platform";
    for (const call of [
      () => api.adminListProviderGrants(id),
      () => api.adminPutProviderGrant(id, "u-admin"),
      () => api.adminDeleteProviderGrant(id, "u-admin"),
      () => api.adminSetProviderAllUsers(id, true),
      () => api.adminRevokeProviderRecipients(id),
    ]) {
      await expect(call()).rejects.toMatchObject({ status: 409, body: { reason: "provider_not_operator_key" } });
    }
    await expect(api.adminPutProviderGrant("nope", "u-admin")).rejects.toMatchObject({ status: 404 });
  });

  it("switches all users, and revoke-all turns it off and removes every named grant, also after retire", async () => {
    const api = await load();
    await api.adminPutProviderGrant(OP, "u-admin");
    await expect(api.adminSetProviderAllUsers(OP, true)).resolves.toBeNull();
    await expect(api.adminSetProviderAllUsers(OP, true)).resolves.toBeNull();
    expect((await api.adminListSharedProviders()).providers.find((p) => p.id === OP)!.operator_all_users).toBe(true);
    await api.adminRetireSharedProvider(OP);
    const named = (await api.adminListProviderGrants(OP)).named_grant_count;
    expect(named).toBeGreaterThan(0);
    await expect(api.adminRevokeProviderRecipients(OP)).resolves.toEqual({ named_grants_removed: named });
    await expect(api.adminListProviderGrants(OP)).resolves.toEqual({ operator_all_users: false, named_grant_count: 0, grants: [], next_cursor: null });
    await expect(api.adminRevokeProviderRecipients(OP)).resolves.toEqual({ named_grants_removed: 0 });
  });
});
