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
