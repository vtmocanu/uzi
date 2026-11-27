// PRD #4320 modules 5-7 (agent half): the provider binding a provider-bound claim carries.
//
//   - decodeProviderBinding: the fail-closed claim decoder. The api is the destination authority
//     (operator allowlist); the worker validates only the binding's SHAPE and its exclusivity with
//     any vendor credential. A claim that fails it is malformed: the claim loop treats the throw
//     like every other malformed claim (nothing starts, nothing is spent).
//   - sdkCredentialFromBinding: the one place a decoded binding becomes the SdkCredential every
//     spawn site hands to buildSdkEnv.
//   - resolveProviderModel: model resolution through the bound map. An unmapped alias or a full
//     vendor model ID is a typed refusal; nothing is ever rewritten.

import { CODEX_MODEL_IDS } from "./codex/model-vocabulary.js";
import type { EmittedMessage } from "./executor.js";
import type { ClaimProviderBinding, ClaimClaudeProviderBinding, ClaimCodexProviderBinding, ClaimProviderModel } from "./protocol.js";
import type { SdkCredential } from "./sdk-env.js";

/** The protocol capability a worker must advertise (at register) to accept a provider-bound claim. */
export const CLAUDE_PROVIDER_CAPABILITY = "claude_provider_v1";

/** The additional protocol capability a worker must advertise (at register) to accept a Claude
 *  provider-bound JUDGE claim (PRD #4322 M1a): this image runs the provider judge path (typed raw-frame
 *  limit settlement, exact-key redaction). The api places a provider-bound judge only on a worker
 *  advertising it, and client.ts requireProviderCapability refuses one on a registration that did not. */
export const CLAUDE_PROVIDER_JUDGE_CAPABILITY = "claude_provider_judge_v1";

/** The additional protocol capability a worker must advertise (at register) to accept a Claude
 *  provider-bound JOB claim (PRD #4322 M1b): this image runs the provider job path (job-runner.ts:
 *  exact-key redaction of results and output files, staged capture, typed gateway-limit settlement).
 *  The api places a provider-bound job only on a worker advertising it, and client.ts
 *  requireProviderCapability refuses one on a registration that did not. */
export const CLAUDE_PROVIDER_JOB_CAPABILITY = "claude_provider_job_v1";

/** The protocol capability a worker must advertise (at register) to accept a Codex provider-bound
 *  claim. Advertised only together with the Codex harness and runtime v2 capabilities (worker.ts),
 *  and the api admits a Codex provider claim only on provider, harness and runtime v2. */
export const CODEX_PROVIDER_CAPABILITY = "codex_provider_v1";

/** The additional protocol capability a worker must advertise (at register) to accept a Codex
 *  provider-bound JUDGE claim (issue #4574): this image runs the Codex provider judge path
 *  (judge-runner.ts judgeCodexProvider). Advertised under the same gate as CODEX_PROVIDER_CAPABILITY;
 *  the api places a Codex provider judge only on a worker advertising it, and client.ts
 *  requireProviderCapability refuses one on a registration that did not. */
export const CODEX_PROVIDER_JUDGE_CAPABILITY = "codex_provider_judge_v1";

/** The reason a claim whose lane cannot serve a provider reports (the server refuses these lanes
 *  first; this is the worker's second layer). */
export const PROVIDER_LANE_UNSUPPORTED = "provider_lane_unsupported";

/** The reason a provider model refusal carries. */
const PROVIDER_MODEL_UNMAPPED = "provider_model_unmapped";

/** The limit-park detail a terminal 429/529 from a provider gateway is reported with (PRD #4320 D11). */
export const PROVIDER_GATEWAY_LIMIT = "provider_gateway_limit";

const MAX_BASE_URL_BYTES = 2048;
const MIN_CREDENTIAL = 16;
const MAX_CREDENTIAL = 4096;
const MODEL_PATTERN = /^[A-Za-z0-9._:/@-]{1,256}$/;
const PRINTABLE_ASCII = /^[\x21-\x7e]+$/;
const MIN_CONTEXT_WINDOW = 8192;
const MAX_CONTEXT_WINDOW = 10_000_000;
const ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;
type Alias = (typeof ALIASES)[number];

/** Thrown for a provider binding that fails the fail-closed decode. The message names the rule, never a value. */
export class ProviderBindingError extends Error {
  constructor(readonly rule: string) {
    super(`malformed provider binding on claim: ${rule}`);
    this.name = "ProviderBindingError";
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function isLoopbackHost(hostname: string): boolean {
  const h = hostname.toLowerCase().replace(/^\[|\]$/g, "");
  return h === "localhost" || h === "::1" || /^127(?:\.\d{1,3}){3}$/.test(h);
}

function decodeModel(v: unknown, what: string): { model: string; context_window?: number } {
  if (!isRecord(v)) throw new ProviderBindingError(`${what} is not an object`);
  const model = v.model;
  if (typeof model !== "string" || !MODEL_PATTERN.test(model)) throw new ProviderBindingError(`${what} model is outside the allowed form`);
  const out: { model: string; context_window?: number } = { model };
  if (v.context_window !== undefined) {
    const w = v.context_window;
    if (typeof w !== "number" || !Number.isInteger(w) || w < MIN_CONTEXT_WINDOW || w > MAX_CONTEXT_WINDOW) {
      throw new ProviderBindingError(`${what} context_window is outside ${MIN_CONTEXT_WINDOW}..${MAX_CONTEXT_WINDOW}`);
    }
    out.context_window = w;
  }
  return out;
}

/**
 * The fail-closed decode of `claim.secrets.provider`. Returns undefined when the claim carries no
 * binding (a vendor claim), the decoded and re-built binding (no unknown fields survive) when it
 * does, and THROWS a {@link ProviderBindingError} for anything else: a binding next to a vendor
 * OAuth token or a Codex block, a non-https base URL (unless a loopback host AND this worker's own
 * `UZI_PROVIDER_ALLOW_LOOPBACK=true`), userinfo, an unknown engine, protocol or auth style, a model
 * map without its required slot or with an unknown slot, a model outside `[A-Za-z0-9._:/@-]{1,256}`, a
 * credential outside 16..4096 printable ASCII characters, an oversized base URL, or a context
 * window outside 8192..10000000.
 */
export function decodeProviderBinding(secrets: unknown, env: NodeJS.ProcessEnv = process.env): ClaimProviderBinding | undefined {
  if (!isRecord(secrets) || !Object.hasOwn(secrets, "provider")) return undefined;
  const raw = secrets.provider;
  if (!isRecord(raw)) throw new ProviderBindingError("provider is not an object");
  const oauth = secrets.anthropic_oauth_token;
  if (oauth !== undefined && oauth !== null && oauth !== "") throw new ProviderBindingError("provider next to an anthropic_oauth_token");
  if (secrets.codex !== undefined && secrets.codex !== null) throw new ProviderBindingError("provider next to a codex block");

  const { provider_id: providerId, revision, engine, protocol, base_url: baseUrl, auth_style: authStyle, credential } = raw;
  if (typeof providerId !== "string" || providerId === "") throw new ProviderBindingError("provider_id is missing");
  if (typeof revision !== "number" || !Number.isSafeInteger(revision) || revision < 1) throw new ProviderBindingError("revision is not a positive integer");
  if (engine !== "claude" && engine !== "codex") throw new ProviderBindingError("unknown engine");
  if (protocol !== (engine === "claude" ? "anthropic_messages" : "openai_responses")) throw new ProviderBindingError("unknown protocol");
  if (authStyle !== "bearer" && !(engine === "claude" && authStyle === "x_api_key")) throw new ProviderBindingError("unknown auth_style");

  if (typeof baseUrl !== "string" || baseUrl === "" || Buffer.byteLength(baseUrl, "utf8") > MAX_BASE_URL_BYTES) {
    throw new ProviderBindingError("base_url is missing or oversized");
  }
  let url: URL;
  try { url = new URL(baseUrl); } catch { throw new ProviderBindingError("base_url does not parse"); }
  if (url.username !== "" || url.password !== "") throw new ProviderBindingError("base_url carries userinfo");
  if (url.protocol !== "https:") {
    const loopbackOk = url.protocol === "http:" && isLoopbackHost(url.hostname) && env.UZI_PROVIDER_ALLOW_LOOPBACK === "true";
    if (!loopbackOk) throw new ProviderBindingError("base_url is not https");
  }

  if (typeof credential !== "string" || credential.length < MIN_CREDENTIAL || credential.length > MAX_CREDENTIAL || !PRINTABLE_ASCII.test(credential)) {
    throw new ProviderBindingError("credential is outside the allowed form");
  }

  const rawMap = raw.model_map;
  if (!isRecord(rawMap)) throw new ProviderBindingError("model_map is not an object");
  if (engine === "codex") {
    const ids = CODEX_MODEL_IDS;
    for (const key of Object.keys(rawMap)) {
      if (key !== "default" && !ids.some(id => id === key)) throw new ProviderBindingError("model_map has an unknown slot");
    }
    if (!Object.hasOwn(rawMap, "default")) throw new ProviderBindingError("model_map has no default");
    const modelMap: ClaimCodexProviderBinding["model_map"] = { default: decodeModel(rawMap.default, "mapped slot") };
    for (const id of ids) {
      if (Object.hasOwn(rawMap, id)) modelMap[id] = decodeModel(rawMap[id], "mapped slot");
    }
    return { provider_id: providerId, revision, engine, protocol: "openai_responses",
      base_url: baseUrl, auth_style: "bearer", credential, model_map: modelMap };
  }
  for (const k of Object.keys(rawMap)) if (!(ALIASES as readonly string[]).includes(k)) throw new ProviderBindingError("model_map has an unknown alias");
  if (rawMap.haiku === undefined) throw new ProviderBindingError("model_map has no haiku");
  const modelMap: ClaimClaudeProviderBinding["model_map"] = { haiku: decodeModel(rawMap.haiku, "haiku") };
  for (const alias of ["opus", "sonnet", "fable"] as const) {
    if (rawMap[alias] !== undefined) modelMap[alias] = decodeModel(rawMap[alias], alias);
  }

  return {
    provider_id: providerId,
    revision,
    engine: "claude",
    protocol: "anthropic_messages",
    base_url: baseUrl,
    auth_style: authStyle,
    credential,
    model_map: modelMap,
  };
}

/** Execution refusal shared by claim admission and the Claude SDK boundary. */
export class ProviderLaneUnsupportedError extends Error {
  readonly reason = PROVIDER_LANE_UNSUPPORTED;
  constructor() {
    super(PROVIDER_LANE_UNSUPPORTED);
    this.name = "ProviderLaneUnsupportedError";
  }
}

/** The SdkCredential of a decoded binding. */
export function sdkCredentialFromBinding(b: ClaimProviderBinding): SdkCredential {
  if (b.engine !== "claude") throw new ProviderLaneUnsupportedError();
  return { kind: "provider", baseUrl: b.base_url, authStyle: b.auth_style, key: b.credential, modelMap: b.model_map };
}

/**
 * PRD #4320 P7: a provider run reports usage tokens but never cost. The SDK prices with the vendor
 * table, so a mapped gateway model has no valid basis; the result frame's `total_cost_usd` and each
 * `modelUsage` entry's `costUSD` are removed before the frame is emitted (the api then reads cost as
 * unreported). Returns the message unchanged when it carries neither.
 */
export function stripProviderCost(em: EmittedMessage): EmittedMessage {
  const p = em.payload;
  const hasTotal = Object.hasOwn(p, "total_cost_usd");
  const mu = p["modelUsage"];
  const muHasCost = isRecord(mu) && Object.values(mu).some((e) => isRecord(e) && Object.hasOwn(e, "costUSD"));
  if (!hasTotal && !muHasCost) return em;
  const payload: Record<string, unknown> = { ...p };
  delete payload["total_cost_usd"];
  if (muHasCost) {
    payload["modelUsage"] = Object.fromEntries(
      Object.entries(mu as Record<string, unknown>).map(([model, e]) => {
        if (!isRecord(e)) return [model, e];
        const { costUSD: _drop, ...rest } = e;
        return [model, rest];
      }),
    );
  }
  return { ...em, payload };
}

/** Whether a terminal HTTP status from a provider gateway is a gateway limit (429 rate limit, 529 overload). */
export function isGatewayLimitStatus(status: number | null | undefined): boolean {
  return status === 429 || status === 529;
}

/** The typed refusal of a model the bound map does not carry. Names the model; never rewrites it. */
export class ProviderModelUnmappedError extends Error {
  readonly reason = PROVIDER_MODEL_UNMAPPED;
  constructor(readonly model: string) {
    super(`${PROVIDER_MODEL_UNMAPPED}: model ${JSON.stringify(model)} is not in the provider's model map`);
    this.name = "ProviderModelUnmappedError";
  }
}

/**
 * Resolve a requested model through a provider's map. Allowed: a Claude alias the map carries
 * (`opus`, `sonnet`, `haiku`, `fable`) or exactly one of the mapped target model strings. An empty
 * or unset model, or the SDK's `inherit`, means the CLI default slot, which is the `sonnet` alias. Anything else throws
 * {@link ProviderModelUnmappedError}. The returned string is what the SDK `model` option receives:
 * the alias or mapped target as given, never a rewrite.
 */
export function resolveProviderModel(model: string | undefined, modelMap: Readonly<Partial<Record<string, ClaimProviderModel>>>): string {
  const effective = model === undefined || model.trim() === "" || model === "inherit" ? "sonnet" : model;
  if ((ALIASES as readonly string[]).includes(effective)) {
    if (modelMap[effective as Alias] !== undefined) return effective;
    throw new ProviderModelUnmappedError(effective);
  }
  for (const alias of ALIASES) if (modelMap[alias]?.model === effective) return effective;
  throw new ProviderModelUnmappedError(effective);
}

/**
 * Resolve every explicit model of a subagent roster through a provider's map (PRD #4320 module 7:
 * templates and repo-sourced agents alike). An agent with no `model`, or `model: inherit`, inherits the lead's, which the
 * caller has already resolved; an unmapped one throws {@link ProviderModelUnmappedError} naming it.
 * Nothing is rewritten.
 */
export function assertAgentModelsMapped(
  agents: Record<string, { model?: string }>,
  modelMap: Readonly<Partial<Record<string, ClaimProviderModel>>>,
): void {
  for (const def of Object.values(agents)) {
    if (def.model !== undefined && def.model !== "" && def.model !== "inherit") resolveProviderModel(def.model, modelMap);
  }
}


/** Internal Codex selections may use default; an explicit owner pin must have its own slot. */
export function resolveCodexProviderModelSlot(
  model: string | undefined,
  modelMap: ClaimCodexProviderBinding["model_map"],
  explicit = false,
): keyof ClaimCodexProviderBinding["model_map"] {
  if (model !== undefined && Object.hasOwn(modelMap, model)) return model as keyof typeof modelMap;
  if (explicit) throw new ProviderModelUnmappedError(model ?? "");
  return "default";
}
