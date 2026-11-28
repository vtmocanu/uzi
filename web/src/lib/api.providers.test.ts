// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { api, MOCK_MODE } from "./api";
afterEach(() => vi.unstubAllGlobals());
it("sends Codex engine, Responses protocol and all mappings over the real transport", async () => {
  expect(MOCK_MODE).toBe(false);
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify({ provider: {} }), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const model_map = Object.fromEntries(["default", "gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"].map(role => [role, { model: "gateway-" + role, context_window: 32768 }]));
  const input = { engine: "codex" as const, protocol: "openai_responses" as const, name: "code", base_url: "https://gateway.example/v1", auth_style: "bearer" as const, model_map };
  await api.createProvider(input);
  expect(fetchMock.mock.calls[0][0]).toBe("/api/me/providers");
  expect(JSON.parse((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)).toEqual(input);
  await api.setCodexProviderSource("code");
  expect(fetchMock.mock.calls[1][0]).toBe("/api/me/providers/codex-source");
  expect(JSON.parse((fetchMock.mock.calls[1] as unknown as [string, RequestInit])[1].body as string)).toEqual({ provider_id: "code" });
});
it("sends a shared Codex definition's engine with the Responses protocol", async () => {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify({ provider: {} }), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const model_map = { default: { model: "gateway-default" } };
  await api.adminCreateSharedProvider({ engine: "codex", name: "shared-code", base_url: "https://gateway.example/v1", auth_style: "bearer", model_map });
  expect(fetchMock.mock.calls[0][0]).toBe("/api/admin/providers");
  expect(JSON.parse((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)).toEqual({
    engine: "codex", protocol: "openai_responses", name: "shared-code", base_url: "https://gateway.example/v1", auth_style: "bearer", model_map,
  });
});
it("sends a shared Claude definition's engine with the Messages protocol", async () => {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify({ provider: {} }), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const model_map = { haiku: { model: "gateway-haiku" } };
  await api.adminCreateSharedProvider({ engine: "claude", name: "shared-gw", base_url: "https://gateway.example", auth_style: "x_api_key", model_map });
  expect(JSON.parse((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)).toEqual({
    engine: "claude", protocol: "anthropic_messages", name: "shared-gw", base_url: "https://gateway.example", auth_style: "x_api_key", model_map,
  });
});
it("sends the organization-key create fields and calls the four key routes with the right verbs", async () => {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify({ provider: {}, probe: {} }), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const call = (i: number) => {
    const [url, init] = fetchMock.mock.calls[i] as unknown as [string, RequestInit];
    return { url, method: init.method, body: init.body ? JSON.parse(init.body as string) : undefined };
  };
  const model_map = { haiku: { model: "gateway-haiku" } };
  await api.adminCreateSharedProvider({ engine: "claude", name: "org", base_url: "https://gateway.example", auth_style: "bearer", model_map, credential_mode: "operator_key", acknowledge_recipient_trust: true });
  expect(call(0).body).toMatchObject({ credential_mode: "operator_key", acknowledge_recipient_trust: true });
  await api.adminPutOperatorKey("p 1", "the-key");
  expect(call(1)).toMatchObject({ url: "/api/admin/providers/p%201/key", method: "PUT", body: { key: "the-key" } });
  await api.adminSetOperatorKeyEnabled("p1", false);
  expect(call(2)).toMatchObject({ url: "/api/admin/providers/p1/key/enabled", method: "PATCH", body: { enabled: false } });
  await api.adminDeleteOperatorKey("p1");
  expect(call(3)).toMatchObject({ url: "/api/admin/providers/p1/key", method: "DELETE" });
  await api.adminTestOperatorKey("p1");
  expect(call(4)).toMatchObject({ url: "/api/admin/providers/p1/test", method: "POST" });
});
it("calls the recipient grant routes with the right verbs, paths and bodies (PRD #4540 M2)", async () => {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify({}), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const call = (i: number) => {
    const [url, init] = fetchMock.mock.calls[i] as unknown as [string, RequestInit];
    return { url, method: init.method ?? "GET", body: init.body ? JSON.parse(init.body as string) : undefined };
  };
  await api.adminListProviderGrants("p 1");
  expect(call(0)).toMatchObject({ url: "/api/admin/providers/p%201/grants", method: "GET" });
  await api.adminListProviderGrants("p1", { cursor: "u/1", limit: 50 });
  expect(call(1).url).toBe("/api/admin/providers/p1/grants?cursor=u%2F1&limit=50");
  await api.adminPutProviderGrant("p1", "u 1");
  expect(call(2)).toMatchObject({ url: "/api/admin/providers/p1/grants/u%201", method: "PUT", body: undefined });
  await api.adminDeleteProviderGrant("p1", "u1");
  expect(call(3)).toMatchObject({ url: "/api/admin/providers/p1/grants/u1", method: "DELETE" });
  await api.adminSetProviderAllUsers("p1", true);
  expect(call(4)).toMatchObject({ url: "/api/admin/providers/p1/all-users", method: "PUT", body: { enabled: true } });
  await api.adminRevokeProviderRecipients("p1");
  expect(call(5)).toMatchObject({ url: "/api/admin/providers/p1/revoke-recipients", method: "POST" });
});
