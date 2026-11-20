import type { ProviderDTO } from "../../lib/api";
import { minsAgo } from "./time";

// Allowed gateway base URLs the demo operator configured (UZI_PROVIDER_ALLOWED_BASE_URLS).
export const mockProviderAllowedBaseUrls: string[] = [
  "https://llm-gateway.example.internal/anthropic",
  "https://gateway.example.com/v1",
];

// The ?mock=providers scenario: feature on, one provider with a key and a probe
// result (auth ok, tool use not supported), so both result lines are browsable.
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
    has_key: true,
    key_enabled: true,
    probe: { auth_result: "ok", tool_result: "not_supported", probed_at: minsAgo(12) },
  },
];
