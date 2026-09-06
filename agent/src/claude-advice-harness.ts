// Claude advice query construction and terminal metadata. model-pass.ts owns the
// existing timeout, ephemeral HOME lifecycle and text-returning compatibility API.

import type { HookInput, HookJSONOutput, Options as SdkOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";
import { spawnDetached } from "./sdk-spawn.js";
import { buildSdkEnv } from "./sdk-env.js";
import { promptStream, mapSdkMessage, isResult, isErrorResult } from "./sdk-messages.js";
import { RateLimitObserver, type RateLimitObservation } from "./limit.js";
import type { Logger } from "./log.js";
import type { SdkQueryFn } from "./sdk-executor.js";
import type { AdviceHarness, AdviceRequest, AdviceResult, AdviceResultPolicy } from "./harness.js";

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

/** The neutral advice seam's Claude adapter: builds the isolation-shaped SdkOptions and
 *  streams one tool-less turn, absorbing the old `consume` body verbatim. The timeout/HOME compatibility owner
 *  in model-pass.ts invokes this adapter; conformance tests observe its neutral result. */
export class ClaudeAdviceHarness implements AdviceHarness {
  readonly kind = "claude";

  constructor(private readonly inputs: ClaudeAdviceInputs) {}

  async run(request: AdviceRequest, policy: AdviceResultPolicy): Promise<AdviceResult> {
    const env = buildSdkEnv(this.inputs.token, this.inputs.homeDir);
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

    let text = "";
    let terminalMsg: unknown;
    let terminalIsError = false;
    const rateLimits = new RateLimitObserver();
    for await (const msg of this.inputs.queryFn({ prompt: promptStream(request.prompt), options })) {
      rateLimits.observe(msg);
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
      usage: undefined,
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
