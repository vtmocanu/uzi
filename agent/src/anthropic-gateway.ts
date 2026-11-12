// The Anthropic-compatible gateway (LiteLLM, Portkey, Cloudflare AI Gateway, a corporate
// proxy) that every Claude CLI call routes through when an admin configured one. The api
// sends it on register and on every heartbeat (null when off, or when the operator's
// ANTHROPIC_GATEWAY_ALLOWED_BASE_URLS no longer admits it); buildSdkEnv reads it when it
// builds the CLI's environment. Worker-wide on purpose: the setting is instance-wide, and
// one value read at every CLI spawn reaches every run kind without per-claim plumbing.
//
// Not a secret: the key it routes is the user's own Anthropic token, delivered per claim
// exactly as before.

export interface AnthropicGateway {
  /** A gateway is configured but unusable (not allowlisted, invalid, or a wire this worker
   *  cannot read). The CLI is then pointed at BLOCKED_BASE_URL so the saved token, a gateway
   *  key, fails closed instead of reaching api.anthropic.com. */
  blocked?: true;
  baseUrl: string;
  /** "authorization" (Bearer, ANTHROPIC_AUTH_TOKEN) or "x-api-key" (ANTHROPIC_API_KEY). */
  authHeader: "authorization" | "x-api-key";
  /** Claude Code alias -> gateway model name. */
  models: Partial<Record<GatewayAlias, string>>;
}

/** The aliases Claude Code resolves through ANTHROPIC_DEFAULT_<ALIAS>_MODEL. */
export const GATEWAY_ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;
type GatewayAlias = (typeof GATEWAY_ALIASES)[number];

/** An unresolvable host (RFC 6761 .invalid): the CLI's calls fail at DNS, so no token leaves. */
export const BLOCKED_BASE_URL = "https://anthropic-gateway-blocked.invalid";
const BLOCKED: AnthropicGateway = { blocked: true, baseUrl: BLOCKED_BASE_URL, authHeader: "authorization", models: {} };

/** The one model-name rule, shared verbatim with the api (settings_gateway.go) and the
 *  admin card, so a value the api stores is never one this worker refuses. */
const MODEL_RE = /^[A-Za-z0-9._:/@-]{1,200}$/;

let current: AnthropicGateway | undefined;

export function anthropicGateway(): AnthropicGateway | undefined {
  return current;
}

/** Replace the gateway from a register/heartbeat field. null clears it (no gateway
 *  configured): the CLI talks to Anthropic. Absent keeps the current value. A blocked or
 *  unreadable field blocks: a gateway exists, so the token is its key. */
export function setAnthropicGatewayFromWire(wire: unknown): void {
  // An ABSENT field (an older api pod mid-rollout) is no information: keep what we know.
  // Only an explicit null turns the gateway off.
  if (wire === undefined) return;
  current = decodeAnthropicGateway(wire);
}

export function decodeAnthropicGateway(wire: unknown): AnthropicGateway | undefined {
  if (wire === null || wire === undefined) return undefined;
  return decodeConfigured(wire) ?? BLOCKED;
}

function decodeConfigured(wire: unknown): AnthropicGateway | undefined {
  if (typeof wire !== "object" || wire === null) return undefined;
  const w = wire as { base_url?: unknown; auth_header?: unknown; models?: unknown; blocked?: unknown };
  if (w.blocked === true || typeof w.base_url !== "string") return undefined;
  let url: URL;
  try {
    url = new URL(w.base_url);
  } catch {
    return undefined;
  }
  if (url.protocol !== "https:" || url.username || url.password) return undefined;
  const authHeader = w.auth_header === "x-api-key" ? "x-api-key" : w.auth_header === "authorization" ? "authorization" : undefined;
  if (!authHeader) return undefined;
  const models: Partial<Record<GatewayAlias, string>> = {};
  if (w.models !== undefined && w.models !== null) {
    if (typeof w.models !== "object") return undefined;
    for (const [alias, model] of Object.entries(w.models as Record<string, unknown>)) {
      if (!(GATEWAY_ALIASES as readonly string[]).includes(alias) || typeof model !== "string" || !MODEL_RE.test(model)) return undefined;
      models[alias as GatewayAlias] = model;
    }
  }
  return { baseUrl: w.base_url, authHeader, models };
}
