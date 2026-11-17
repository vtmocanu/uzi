// Demo-mode custom providers (PRD #4320 M3). Feature is OFF unless ?mock=providers,
// matching a real instance whose operator has not set UZI_PROVIDER_ALLOWED_BASE_URLS.

import type { ProviderDTO, ProviderProbe } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { mockProviderAllowedBaseUrls, mockProviders } from "../data";
import { delay, mockScenario, requireSession } from "./shared";

const featureOn = mockScenario() === "providers";
let providers: ProviderDTO[] = featureOn ? mockProviders.map((p) => ({ ...p })) : [];
let nextId = 1;

function fail(status: number, reason: string, message: string): never {
  throw new ApiError(status, message, { error: message, reason });
}

function find(id: string): ProviderDTO {
  const p = providers.find((x) => x.id === id);
  if (!p) throw new ApiError(404, "provider not found", { error: "provider not found" });
  return p;
}

function live(id: string): ProviderDTO {
  const p = find(id);
  if (p.retired) fail(409, "provider_retired", "provider is retired");
  return p;
}

function checkUrl(url: string): void {
  if (!mockProviderAllowedBaseUrls.includes(url)) {
    fail(422, "provider_base_url_not_allowed", "base URL is not in the operator allowlist");
  }
}

function update(id: string, patch: Partial<ProviderDTO>): ProviderDTO {
  const next = { ...find(id), ...patch };
  providers = providers.map((p) => (p.id === id ? next : p));
  return { ...next };
}

export const providersApi = {
  listProviders: async () => {
    requireSession();
    return delay({
      enabled: featureOn,
      allowed_base_urls: featureOn ? [...mockProviderAllowedBaseUrls] : [],
      providers: providers.map((p) => ({ ...p })),
    });
  },
  createProvider: async (input: {
    name: string;
    base_url: string;
    auth_style: ProviderDTO["auth_style"];
    model_map: ProviderDTO["model_map"];
  }) => {
    requireSession();
    checkUrl(input.base_url);
    if (providers.some((p) => p.name === input.name)) {
      fail(409, "provider_name_taken", "a provider with that name already exists");
    }
    const created: ProviderDTO = {
      id: `prov-mock-${nextId++}`,
      name: input.name,
      engine: "claude",
      retired: false,
      current_revision: 1,
      protocol: "anthropic_messages",
      base_url: input.base_url,
      auth_style: input.auth_style,
      model_map: input.model_map,
      has_key: false,
      key_enabled: false,
      probe: null,
    };
    providers = [...providers, created];
    return delay({ ...created });
  },
  patchProvider: async (id: string, body: { name?: string; model_map?: ProviderDTO["model_map"] }) => {
    requireSession();
    live(id);
    return delay(update(id, body));
  },
  patchProviderEndpoint: async (
    id: string,
    body: { base_url?: string; auth_style?: ProviderDTO["auth_style"] },
  ) => {
    requireSession();
    live(id);
    if (body.base_url !== undefined) checkUrl(body.base_url);
    return delay(update(id, body));
  },
  putProviderKey: async (id: string, _key: string) => {
    requireSession();
    live(id);
    return delay(update(id, { has_key: true, key_enabled: true, probe: null }));
  },
  deleteProviderKey: async (id: string) => {
    requireSession();
    live(id);
    return delay(update(id, { has_key: false, key_enabled: false }));
  },
  setProviderKeyEnabled: async (id: string, enabled: boolean) => {
    requireSession();
    const p = live(id);
    if (!p.has_key) fail(409, "provider_no_key", "provider has no key");
    return delay(update(id, { key_enabled: enabled }));
  },
  retireProvider: async (id: string) => {
    requireSession();
    return delay(update(id, { retired: true, key_enabled: false }));
  },
  deleteProvider: async (id: string) => {
    requireSession();
    find(id);
    providers = providers.filter((p) => p.id !== id);
    return delay(null);
  },
  testProvider: async (id: string) => {
    requireSession();
    const p = live(id);
    if (!p.has_key || !p.key_enabled) fail(409, "provider_no_key", "provider has no enabled key");
    const probe: ProviderProbe = {
      auth_result: "ok",
      tool_result: "passed",
      probed_at: new Date().toISOString(),
    };
    update(id, { probe });
    return delay({ probe });
  },
};
