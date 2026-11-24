// Demo-mode custom providers for Claude and Codex. Feature is OFF unless ?mock=providers,
// matching a real instance whose operator has not set UZI_PROVIDER_ALLOWED_BASE_URLS.

import type { CreateProviderInput, ProviderDTO, ProviderProbe } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { mockProviderAllowedBaseUrls, mockProviders } from "../data";
import { delay, mockScenario, requireSession } from "./shared";

const featureOn = mockScenario() === "providers";
let providers: ProviderDTO[] = featureOn ? mockProviders.map((p) => ({ ...p })) : [];
let nextId = 1;
// The user's selected Claude source (UserSettings.claude_provider_id); null = Anthropic.
let claudeProviderId: string | null = null;
let codexProviderId: string | null = null;
export function mockCodexProviderId(): string | null { return codexProviderId; }

/** Read by the mock GET /me/settings so the settings projection and this module agree. */
export function mockClaudeProviderId(): string | null {
  return claudeProviderId;
}

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
    fail(400, "provider_base_url_not_allowed", "base URL is not in the operator allowlist");
  }
}

function noKey(): never {
  throw new ApiError(404, "the provider has no key", { error: "the provider has no key" });
}

function nameTaken(name: string, exceptId?: string): boolean {
  const n = name.trim().toLowerCase();
  return providers.some((p) => p.id !== exceptId && p.name.toLowerCase() === n);
}

// bump mirrors a new revision: the stored Test result belongs to the old one.
function bump(id: string, patch: Partial<ProviderDTO>): ProviderDTO {
  return update(id, { ...patch, current_revision: find(id).current_revision + 1, probe: null });
}

function update(id: string, patch: Partial<ProviderDTO>): ProviderDTO {
  const next = { ...find(id), ...patch };
  providers = providers.map((p) => (p.id === id ? next : p));
  return { ...next };
}

function validate(engine: ProviderDTO["engine"], auth: ProviderDTO["auth_style"], map: ProviderDTO["model_map"]) {
  const roles = engine === "codex" ? ["default", "gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"] : ["haiku", "sonnet", "opus", "fable"];
  if (engine === "codex" && auth !== "bearer") fail(400, "invalid_provider", "Codex requires Bearer auth");
  if (!map[engine === "codex" ? "default" : "haiku"]) fail(400, "invalid_provider", "required model missing");
  for (const [role, entry] of Object.entries(map)) {
    if (!roles.includes(role) || !/^[A-Za-z0-9._:/@-]{1,256}$/.test(entry.model) || (entry.context_window !== undefined && (!Number.isInteger(entry.context_window) || entry.context_window < 8192 || entry.context_window > 10000000))) fail(400, "invalid_provider", "invalid model map");
  }
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
  createProvider: async (input: CreateProviderInput) => {
    requireSession();
    checkUrl(input.base_url);
    const engine = input.engine ?? "claude";
    if (input.protocol !== undefined && input.protocol !== (engine === "codex" ? "openai_responses" : "anthropic_messages")) fail(400, "invalid_provider", "protocol does not match engine");
    validate(engine, input.auth_style, input.model_map);
    if (nameTaken(input.name)) {
      fail(409, "provider_name_taken", "a provider with that name already exists");
    }
    const created: ProviderDTO = {
      id: `prov-mock-${nextId++}`,
      name: input.name,
      engine: input.engine ?? "claude",
      retired: false,
      current_revision: 1,
      protocol: input.protocol ?? (input.engine === "codex" ? "openai_responses" : "anthropic_messages"),
      base_url: input.base_url,
      auth_style: input.auth_style,
      model_map: input.model_map,
      has_key: false,
      key_enabled: false,
      probe: null,
    };
    providers = [...providers, created];
    return delay({ provider: { ...created } });
  },
  patchProvider: async (id: string, body: { name?: string; model_map?: ProviderDTO["model_map"] }) => {
    requireSession();
    const p = live(id);
    if (body.model_map) validate(p.engine, p.auth_style, body.model_map);
    if (body.name !== undefined && nameTaken(body.name, id)) {
      fail(409, "provider_name_taken", "a provider with that name already exists");
    }
    const { model_map, ...rename } = body;
    if (rename.name !== undefined) update(id, { name: rename.name.trim() });
    return delay({ provider: model_map ? bump(id, { model_map }) : find(id) });
  },
  patchProviderEndpoint: async (
    id: string,
    body: { base_url?: string; auth_style?: ProviderDTO["auth_style"] },
  ) => {
    requireSession();
    const p = live(id);
    validate(p.engine, body.auth_style ?? p.auth_style, p.model_map);
    if (body.base_url !== undefined) checkUrl(body.base_url);
    return delay({ provider: bump(id, body) });
  },
  putProviderKey: async (id: string, _key: string) => {
    requireSession();
    find(id);
    return delay({ provider: update(id, { has_key: true, key_enabled: true }) });
  },
  deleteProviderKey: async (id: string) => {
    requireSession();
    if (!find(id).has_key) noKey();
    update(id, { has_key: false, key_enabled: false });
    return delay(null);
  },
  setProviderKeyEnabled: async (id: string, enabled: boolean) => {
    requireSession();
    if (!find(id).has_key) noKey();
    return delay({ provider: update(id, { key_enabled: enabled }) });
  },
  retireProvider: async (id: string) => {
    requireSession();
    return delay({ provider: update(id, { retired: true }) });
  },
  deleteProvider: async (id: string) => {
    requireSession();
    find(id);
    // Mirrors the server: either engine selection protects its provider from deletion.
    if (claudeProviderId === id || codexProviderId === id) {
      fail(409, "provider_in_use_as_source", "the provider is in use as a model source");
    }
    providers = providers.filter((p) => p.id !== id);
    return delay(null);
  },
  // Mirrors handler.SetMyClaudeProviderSource: null clears; an unknown id is a 404; a retired,
  // keyless or key-disabled provider is a 409 provider_source_unusable.
  setClaudeProviderSource: async (providerId: string | null) => {
    requireSession();
    if (providerId === null) {
      claudeProviderId = null;
      return delay({ claude_provider_id: null });
    }
    const p = find(providerId);
    if (p.engine !== "claude") fail(404, "provider_not_found", "provider not found");
    const why = p.retired
      ? "the provider is retired"
      : !p.has_key
        ? "it has no key"
        : !p.key_enabled
          ? "its key is disabled"
          : null;
    if (why) fail(409, "provider_source_unusable", `the provider cannot be the Claude source: ${why}`);
    claudeProviderId = providerId;
    return delay({ claude_provider_id: providerId });
  },
  setCodexProviderSource: async (providerId: string | null) => {
    requireSession();
    if (providerId === null) {
      codexProviderId = null;
      return delay({ codex_provider_id: null });
    }
    const p = find(providerId);
    if (p.engine !== "codex") fail(404, "provider_not_found", "provider not found");
    const why = p.retired
      ? "the provider is retired"
      : !p.has_key
        ? "it has no key"
        : !p.key_enabled
          ? "its key is disabled"
          : null;
    if (why) fail(409, "provider_source_unusable", `the provider cannot be the Codex source: ${why}`);
    codexProviderId = providerId;
    return delay({ codex_provider_id: providerId });
  },
  testProvider: async (id: string) => {
    requireSession();
    const p = live(id);
    if (!p.has_key || !p.key_enabled) noKey();
    const probe: ProviderProbe = {
      auth_result: "ok",
      tool_result: "passed",
      probed_at: new Date().toISOString(),
    };
    update(id, { probe });
    return delay({ probe });
  },
};
