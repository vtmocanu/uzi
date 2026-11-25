import type { ProviderDTO } from "../../lib/api";
import { minsAgo } from "./time";

// Allowed gateway base URLs the demo operator configured (UZI_PROVIDER_ALLOWED_BASE_URLS).
export const mockProviderAllowedBaseUrls: string[] = [
  "https://llm-gateway.example.internal/anthropic",
  "https://gateway.example.com/v1",
];

// The ?mock=providers scenario: feature on, one personal provider with a key and a probe
// result (auth ok, tool use not supported), so both result lines are browsable, plus two
// admin-shared definitions (PRD #4324): an active x-api-key one the demo user has not
// attached a key to yet (so the attach disclosure shows its cross-host line), and a retired
// one nobody selects (so the admin page can demonstrate a guarded delete).
export const mockProviders: ProviderDTO[] = [
  {
    id: "prov-gateway",
    name: "team-gateway",
    engine: "claude",
    retired: false,
    current_revision: 1,
    protocol: "anthropic_messages",
    base_url: "https://llm-gateway.example.internal/anthropic",
    auth_style: "bearer",
    model_map: {
      haiku: { model: "gateway-small", context_window: 32768 },
      sonnet: { model: "gateway-large" },
    },
    credential_mode: null,
    has_key: true,
    key_enabled: true,
    probe: { auth_result: "ok", tool_result: "not_supported", probed_at: minsAgo(12) },
  },
  {
    id: "prov-shared-platform",
    name: "platform-gateway",
    engine: "claude",
    retired: false,
    current_revision: 2,
    protocol: "anthropic_messages",
    base_url: "https://gateway.example.com/v1",
    auth_style: "x_api_key",
    model_map: {
      haiku: { model: "platform-small", context_window: 65536 },
      sonnet: { model: "platform-large", context_window: 200000 },
    },
    credential_mode: "per_user_key",
    has_key: false,
    key_enabled: false,
    probe: null,
  },
  {
    id: "prov-shared-legacy",
    name: "legacy-gateway",
    engine: "claude",
    retired: true,
    current_revision: 1,
    protocol: "anthropic_messages",
    base_url: "https://llm-gateway.example.internal/anthropic",
    auth_style: "bearer",
    model_map: { haiku: { model: "legacy-small" } },
    credential_mode: "per_user_key",
    has_key: false,
    key_enabled: false,
    probe: null,
  },
];

// Other demo users' use of each shared definition, which the admin counts add to the demo
// user's own selection and key. Platform: two others selected it, three attached keys.
export const mockSharedProviderOtherUsers: Record<string, { selected: number; keys: number }> = {
  "prov-shared-platform": { selected: 2, keys: 3 },
  "prov-shared-legacy": { selected: 0, keys: 1 },
};
