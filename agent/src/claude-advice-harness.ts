// Claude advice query construction and terminal metadata. model-pass.ts owns the
// existing timeout, ephemeral HOME lifecycle and text-returning compatibility API.

import type { HookInput, HookJSONOutput, Options as SdkOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";
import { spawnDetached } from "./sdk-spawn.js";
import { buildSdkEnv } from "./sdk-env.js";
import { promptStream, mapSdkMessage, isResult, isErrorResult } from "./sdk-messages.js";
import { RateLimitObserver, type RateLimitObservation } from "./limit.js";
import type { Logger } from "./log.js";
import type { SdkQueryFn } from "./sdk-executor.js";
import type {
  AdviceHarness,
  AdviceModelUsage,
  AdviceRequest,
  AdviceResult,
  AdviceResultPolicy,
  AdviceUsageSnapshot,
  HarnessModelUsage,
  HarnessUsage,
} from "./harness.js";
import { assertResidueQuarantineOpen } from "./residue-quarantine.js";

/** A PreToolUse deny for EVERY tool: the advice runners (judge/review/summary) are
 *  read-only. A deny is authoritative even under bypassPermissions (the same property
 *  guardrails.ts relies on). Internal to the Claude advice adapter. */
function buildDenyAllHook(reason: string) {
  return async (_input: HookInput): Promise<HookJSONOutput> => ({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: reason,
    },
  });
}

/** Construction inputs for ClaudeAdviceHarness — the Claude-specific isolation and
 *  compatibility surface the advice adapter absorbs from the old `consume` body. Kept
 *  in the Claude module rather than the SDK-free harness contract. */
interface ClaudeAdviceInputs {
  token: string;
  homeDir: string;
  abort: AbortController;
  queryFn: SdkQueryFn;
  denyReason: string;
  log: Logger;
  /** Claude-only raw-msg compat shim (the judge's onResult). When present it REPLACES the
   *  policy's default onTerminal, mirroring today's `if (opts.onResult) … else …`. */
  rawResultShim?(msg: unknown, ctx: { isError: boolean; latest: RateLimitObservation | undefined }): void;
}

/** The env var the bundled Claude CLI reads as its output-token cap (the SDK `Options` has no
 *  field for it). buildSdkEnv passes an allowlist only, so it is set on the built env. */
const MAX_OUTPUT_TOKENS_ENV = "CLAUDE_CODE_MAX_OUTPUT_TOKENS";

function rec(v: unknown): Record<string, unknown> | undefined {
  return typeof v === "object" && v !== null && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function tokens(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) && v >= 0 ? Math.floor(v) : undefined;
}

function marker(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

/** One assistant message's usage as the pass sees it. Per column GREATEST, the same merge rule
 *  as UsageLeg in usage-recorder.ts: a message's usage can be seen more than once (partial then
 *  final) and only ever grows. */
interface MessageUsage {
  model: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheCreation: number;
  cacheCreation5m?: number;
  cacheCreation1h?: number;
  serviceTier?: string;
  speed?: string;
  inferenceGeo?: string;
}

/** The running per-model usage evidence of ONE advice pass (PRD #2603). Fed by every assistant
 *  message (deduped by message.id, model from message.model) and then replaced by the terminal
 *  result's `modelUsage`, which carries the provider's own costUSD. Reading `snapshot()` at any
 *  point yields what the pass has provably spent so far, so an abort that never reaches a
 *  terminal frame still has its tokens counted. Never throws. */
class AdviceUsageTracker {
  private readonly byMessage = new Map<string, MessageUsage>();
  private terminal: AdviceUsageSnapshot | undefined;

  observeAssistant(msg: unknown): void {
    try {
      const m = rec(msg);
      if (m?.["type"] !== "assistant") return;
      const message = rec(m["message"]);
      const id = typeof message?.["id"] === "string" ? message["id"] : undefined;
      const model = marker(message?.["model"]);
      const usage = rec(message?.["usage"]);
      // A synthetic model is the SDK's own placeholder, not a provider call.
      if (id === undefined || model === undefined || model === "<synthetic>" || usage === undefined) return;
      let cur = this.byMessage.get(id);
      if (cur === undefined) {
        cur = { model, input: 0, output: 0, cacheRead: 0, cacheCreation: 0 };
        this.byMessage.set(id, cur);
      }
      cur.input = Math.max(cur.input, tokens(usage["input_tokens"]) ?? 0);
      cur.output = Math.max(cur.output, tokens(usage["output_tokens"]) ?? 0);
      cur.cacheRead = Math.max(cur.cacheRead, tokens(usage["cache_read_input_tokens"]) ?? 0);
      cur.cacheCreation = Math.max(cur.cacheCreation, tokens(usage["cache_creation_input_tokens"]) ?? 0);
      const split = rec(usage["cache_creation"]);
      const c5 = tokens(split?.["ephemeral_5m_input_tokens"]);
      const c1 = tokens(split?.["ephemeral_1h_input_tokens"]);
      if (c5 !== undefined) cur.cacheCreation5m = Math.max(cur.cacheCreation5m ?? 0, c5);
      if (c1 !== undefined) cur.cacheCreation1h = Math.max(cur.cacheCreation1h ?? 0, c1);
      cur.serviceTier = marker(usage["service_tier"]) ?? cur.serviceTier;
      cur.speed = marker(usage["speed"]) ?? cur.speed;
      cur.inferenceGeo = marker(usage["inference_geo"]) ?? cur.inferenceGeo;
    } catch {
      // never fail a frame over its usage
    }
  }

  /** The terminal result's `modelUsage` replaces the running evidence, but only when it names at
   *  least one model: an error result with no modelUsage keeps what the messages showed. */
  observeTerminal(msg: unknown): void {
    try {
      const mu = rec(rec(msg)?.["modelUsage"]);
      if (mu === undefined) return;
      const out: Record<string, AdviceModelUsage> = {};
      for (const [model, raw] of Object.entries(mu)) {
        const e = rec(raw);
        if (e === undefined || model === "") continue;
        const entry: AdviceModelUsage = {
          inputTokens: tokens(e["inputTokens"]) ?? 0,
          outputTokens: tokens(e["outputTokens"]) ?? 0,
          cacheReadInputTokens: tokens(e["cacheReadInputTokens"]) ?? 0,
          cacheCreationInputTokens: tokens(e["cacheCreationInputTokens"]) ?? 0,
        };
        const cost = e["costUSD"];
        if (typeof cost === "number" && Number.isFinite(cost) && cost >= 0) entry.costUSD = cost;
        out[model] = entry;
      }
      if (Object.keys(out).length > 0) this.terminal = out;
    } catch {
      // never fail a frame over its usage
    }
  }

  /** The latest snapshot, or undefined when nothing has been observed. */
  snapshot(): AdviceUsageSnapshot | undefined {
    if (this.terminal) return this.terminal;
    const out: Record<string, AdviceModelUsage> = {};
    for (const u of this.byMessage.values()) {
      const e = (out[u.model] ??= { inputTokens: 0, outputTokens: 0, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 });
      e.inputTokens += u.input;
      e.outputTokens += u.output;
      e.cacheReadInputTokens += u.cacheRead;
      e.cacheCreationInputTokens += u.cacheCreation;
      if (u.cacheCreation5m !== undefined) e.cacheCreation5mInputTokens = (e.cacheCreation5mInputTokens ?? 0) + u.cacheCreation5m;
      if (u.cacheCreation1h !== undefined) e.cacheCreation1hInputTokens = (e.cacheCreation1hInputTokens ?? 0) + u.cacheCreation1h;
      if (u.serviceTier !== undefined) e.service_tier = u.serviceTier;
      if (u.speed !== undefined) e.speed = u.speed;
      if (u.inferenceGeo !== undefined) e.inference_geo = u.inferenceGeo;
    }
    return Object.keys(out).length > 0 ? out : undefined;
  }
}

/** The neutral view of a snapshot for AdviceResult.usage: tokens summed over the models, and the
 *  provider cost per model only where the provider reported one (never invented). */
function toHarnessUsage(snapshot: AdviceUsageSnapshot): HarnessUsage {
  let input = 0;
  let read = 0;
  let write = 0;
  let output = 0;
  const models: Record<string, HarnessModelUsage> = {};
  for (const [model, u] of Object.entries(snapshot)) {
    input += u.inputTokens;
    read += u.cacheReadInputTokens;
    write += u.cacheCreationInputTokens;
    output += u.outputTokens;
    models[model] = {
      tokens: {
        inputUncachedTokens: u.inputTokens,
        inputCacheReadTokens: u.cacheReadInputTokens,
        inputCacheWriteTokens: u.cacheCreationInputTokens,
        outputTokens: u.outputTokens,
      },
      cost: u.costUSD !== undefined ? { kind: "metered", usd: u.costUSD, source: "provider" } : { kind: "unreported" },
    };
  }
  return {
    basis: "session",
    tokens: {
      inputUncachedTokens: input,
      inputCacheReadTokens: read,
      inputCacheWriteTokens: write,
      outputTokens: output,
    },
    models,
  };
}

/** The neutral advice seam's Claude adapter: builds the isolation-shaped SdkOptions and
 *  streams one tool-less turn, absorbing the old `consume` body verbatim. The timeout/HOME compatibility owner
 *  in model-pass.ts invokes this adapter; conformance tests observe its neutral result. */
export class ClaudeAdviceHarness implements AdviceHarness {
  readonly kind = "claude";

  constructor(private readonly inputs: ClaudeAdviceInputs) {}

  async run(request: AdviceRequest, policy: AdviceResultPolicy): Promise<AdviceResult> {
    const env = buildSdkEnv(this.inputs.token, this.inputs.homeDir);
    // PRD #2603: the output cap. buildSdkEnv is an allowlist, so the one variable the bundled
    // CLI reads is set on the object it returned, and only when the request asks for it.
    if (request.maxOutputTokens !== undefined) env[MAX_OUTPUT_TOKENS_ENV] = String(request.maxOutputTokens);
    const options: SdkOptions = {
      env: env as unknown as Record<string, string | undefined>,
      abortController: this.inputs.abort,
      // 🔴 ISOLATION SINGLE POINT OF TRUTH. `settingSources: []` MUST stay a LITERAL at
      // this one query site. The semgrep rule semgrep/settings-sources-isolation.yml
      // fires on a WIDENED value only and is BLIND to an OMITTED key — so this is now the
      // ONLY place the advice-lane literal lives, and a future edit that dropped this key
      // would pass semgrep silently and re-open the repo-borne prompt-injection vector.
      // Do NOT extract it to a variable, do NOT spread it in, do NOT delete it.
      settingSources: [],
      systemPrompt: request.systemPrompt,
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      includePartialMessages: false,
      hooks: { PreToolUse: [{ hooks: [buildDenyAllHook(this.inputs.denyReason)] }] },
      // Route the model-reasoning SDK CLI through the runner-uid detached spawn like every
      // other SDK spawn (uniform boundary); the deny-all hook already blocks code-exec, so
      // this is defense-in-depth. (PRD #51 M4 — keep this rationale.)
      spawnClaudeCodeProcess: (spawnOpts) => spawnDetached(spawnOpts) as unknown as SpawnedProcess,
    };
    if (request.model) options.model = request.model;
    if (request.effort) options.effort = request.effort;
    if (request.thinking === "disabled") options.thinking = { type: "disabled" };

    let text = "";
    let terminalMsg: unknown;
    let terminalIsError = false;
    const rateLimits = new RateLimitObserver();
    const tracker = new AdviceUsageTracker();
    const publish = (): void => {
      if (!request.usageObserver) return;
      const snap = tracker.snapshot();
      if (snap) request.usageObserver(snap);
    };
    // issue #2213: the last statement before the credential-bearing provider spawn.
    assertResidueQuarantineOpen("provider_turn");
    for await (const msg of this.inputs.queryFn({ prompt: promptStream(request.prompt), options })) {
      rateLimits.observe(msg);
      tracker.observeAssistant(msg);
      publish();
      for (const em of mapSdkMessage(msg)) {
        if (em.kind === "text") {
          const t = (em.payload as { text?: string }).text;
          if (t) text += t;
        }
      }
      if (isResult(msg)) {
        const isError = isErrorResult(msg);
        terminalMsg = msg;
        terminalIsError = isError;
        // BEFORE the policy / rawResultShim below: onTerminal throws on an error result, and the
        // spend of a failed call still counts.
        tracker.observeTerminal(msg);
        publish();
        if (this.inputs.rawResultShim) {
          // Judge: hand it the RAW SDK msg, unchanged from today's onResult path.
          this.inputs.rawResultShim(msg, { isError, latest: rateLimits.latest });
        } else {
          // review/summary default: the neutral policy. Byte-identical throw/success to
          // today's `else if (isError) throw …`.
          policy.onTerminal(this.neutralTerminal(msg, isError), { isError, latest: undefined });
        }
        break;
      }
    }
    return {
      text,
      // Clean EOF preserves accumulated text without inventing a provider result.
      end: terminalMsg === undefined
        ? { kind: "exhausted" }
        : { kind: "terminal", terminal: this.neutralTerminal(terminalMsg, terminalIsError) },
      usage: (() => {
        const snap = tracker.snapshot();
        return snap ? toHarnessUsage(snap) : undefined;
      })(),
    };
  }

  /** A private MINIMAL neutral terminal builder that reads ONLY the raw `subtype` field.
   *  It is unread by every M2 caller (the run-lane decoder lives elsewhere) and MUST NOT
   *  be able to throw — no failure closure, no usage, no limit evidence. Do not grow a
   *  real terminal decoder here. */
  private neutralTerminal(msg: unknown, isError: boolean) {
    return {
      outcome: isError ? ("failed" as const) : ("success" as const),
      subtype: (() => {
        const s = (msg as Record<string, unknown> | undefined)?.["subtype"];
        return typeof s === "string" ? s : "unknown";
      })(),
      errors: [] as readonly string[],
      metrics: { cost: { kind: "unreported" as const } },
    };
  }
}
