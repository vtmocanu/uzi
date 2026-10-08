// The Claude plan checker (PRD #2460): a one-turn, read-only Claude SDK session that reviews a
// Codex lead's plan candidate. It is the mirror of codex/cross-check.ts (CodexCrossCheck) and is
// pluggable into CrossCheckRunner with the same `run` signature.
//
// Confinement is the isolated lane's, not a weaker copy (isolated-executor.ts):
//   1. `tools` (not `allowedTools`, which does not restrict under bypassPermissions) is exactly
//      Read, Grep, Glob; `disallowedTools` names every shell, web, nested-agent and write tool.
//   2. A matcher-less PreToolUse gate denies any tool outside that set and EVERY tool until the
//      SDK's `system/init` frame has passed checkIsolatedInit (a latch that defaults to refuse).
//   3. The init check itself: tools, MCP servers, plugins, agents and skills must be exactly the
//      surface; guardIsolatedFrame refuses model output before a passing init.
//   4. ONE path guard (buildIsolatedPreToolUse -> buildPathGuardHook) rooted at the checkout,
//      worker credential paths and /run/uzi-secrets denied, plus Read of this session's own
//      authenticated SDK spill file (sdkHomeDir). The checker appends only a pattern guard for
//      the Glob `pattern` / Grep `glob` fields, which the path guard does not screen.
// The SDK child runs through spawnDetached (the runner uid under the uid split, a process-group
// kill on abort) with a full-replacement env.

import type { EffortLevel, HookInput, HookJSONOutput, Options as SdkOptions, SpawnOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";

import type { ClaimResponse } from "./protocol.js";
import type { Logger } from "./log.js";
import type { SdkQueryFn } from "./sdk-executor.js";
import { crossCheckBrief, crossCheckPrompt, CrossCheckMalformedError, validateVerdict } from "./codex/cross-check.js";
import { CrossCheckCheckerUnavailableError } from "./codex/model-rejection.js";
import {
  BUILTIN_PLUGINS_DISABLED,
  buildIsolatedPreToolUse,
  checkIsolatedInit,
  guardIsolatedFrame,
  isolateSdkEnv,
  isolatedDisallowedTools,
  type InitGate,
  type IsolationSurface,
} from "./isolated-executor.js";
import { buildSdkEnv } from "./sdk-env.js";
import { defaultQueryFn, isErrorResult, isResult, mapSdkMessage, promptStream } from "./sdk-messages.js";
import { killProcessGroup, spawnDetached } from "./sdk-spawn.js";
import { assertResidueQuarantineOpen } from "./residue-quarantine.js";

/** The checker's whole tool surface: read-only, no MCP, no plugin, no skill. */
const CHECKER_SURFACE: IsolationSurface = { tools: ["Read", "Grep", "Glob"], mcpServers: [], plugins: [], skills: [] };

/** The verdict is the final result text; anything larger is malformed. */
const MAX_VERDICT_BYTES = 64 * 1024;
/** A bounded turn budget: a check is a handful of reads, never an open-ended session. */
const DEFAULT_MAX_TURNS = 60;
const EFFORTS: readonly string[] = ["low", "medium", "high", "xhigh", "max"];

const REASON_PATTERN = "denied: a Glob or Grep pattern must be relative to the checkout and must not contain '..'";
// An absolute or home-relative pattern, or a `..` segment, at the start or after a `/`, `{` or `,`
// (brace alternatives expand to separate patterns).
const ESCAPING_PATTERN = /(^|[{,])\s*[/\\~]|(^|[/\\{,])\.\.([/\\},]|$)/;
// Brace syntax can assemble a `..` segment from pieces of different alternatives (`{a,.}./*`
// expands to `../*`), so every expansion is screened. A pattern with unbalanced braces or more
// than MAX_BRACE_EXPANSIONS expansions is denied rather than screened partially.
const MAX_BRACE_EXPANSIONS = 256;

/** All brace expansions of `pattern`, or null when the braces are unbalanced or the expansion
 *  count exceeds the cap. Nested and sequential groups expand; `{x}` counts as one alternative. */
function expandBraces(pattern: string): string[] | null {
  const out: string[] = [];
  const walk = (s: string): boolean => {
    const open = s.indexOf("{");
    if (open < 0) {
      if (s.includes("}")) return false;
      out.push(s);
      return out.length <= MAX_BRACE_EXPANSIONS;
    }
    if (s.slice(0, open).includes("}")) return false;
    let depth = 0;
    let start = open + 1;
    const alternatives: string[] = [];
    for (let i = open; i < s.length; i++) {
      const c = s[i];
      if (c === "{") depth++;
      else if (c === "}" && --depth === 0) {
        alternatives.push(s.slice(start, i));
        const prefix = s.slice(0, open);
        const suffix = s.slice(i + 1);
        return alternatives.every(alt => walk(prefix + alt + suffix));
      } else if (c === "," && depth === 1) {
        alternatives.push(s.slice(start, i));
        start = i + 1;
      }
    }
    return false;
  };
  return walk(pattern) ? out : null;
}

/** Bounds before expanding, so the recursive walk stays shallow and cheap. */
const MAX_PATTERN_LENGTH = 1024;
const MAX_BRACE_GROUPS = 32;

// A backslash is denied outright: on Linux workers it is only an escape, and an escaped brace or
// comma would group alternatives differently from expandBraces. Any error denies (fail closed).
const escapesCheckout = (pattern: string): boolean => {
  try {
    if (pattern.length > MAX_PATTERN_LENGTH || pattern.includes("\\")) return true;
    if ((pattern.match(/\{/g)?.length ?? 0) > MAX_BRACE_GROUPS) return true;
    if (ESCAPING_PATTERN.test(pattern)) return true;
    const expansions = expandBraces(pattern);
    return expansions === null || expansions.some(e => ESCAPING_PATTERN.test(e));
  } catch {
    return true;
  }
};

/** Defense in depth for the one path the path guard cannot see: extractToolPaths screens only
 *  file_path/path/notebook_path, so a Glob `pattern` or Grep `glob` is screened here. A pattern
 *  that walks an in-checkout symlink is not caught; the real boundary is the runner uid and the
 *  path guard on `path`. */
function buildCheckerPatternGuard(log: Logger): (input: HookInput) => Promise<HookJSONOutput> {
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse") return {};
    const toolInput = input.tool_input && typeof input.tool_input === "object" ? input.tool_input as Record<string, unknown> : {};
    const field = input.tool_name === "Glob" ? toolInput["pattern"] : input.tool_name === "Grep" ? toolInput["glob"] : undefined;
    // The CLI splits a Grep glob on whitespace after this hook runs, so each token is screened too.
    if (typeof field !== "string" || !(escapesCheckout(field) || field.split(/\s+/).some(t => t !== "" && escapesCheckout(t)))) return {};
    log.warn("cross-check pattern guard denied a pattern", { tool: input.tool_name });
    return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: REASON_PATTERN } };
  };
}

/**
 * Assemble the SDK options of a read-only Claude checker session. Pure, exported so the suite
 * asserts the confinement on the built options and the composed hook list with no live session,
 * and stage-neutral (no plan-specific input) so the code stage can reuse it. `gate` is the init
 * latch the caller opens through guardIsolatedFrame.
 */
export function buildClaudeCrossCheckOptions(input: {
  cwd: string;
  homeDir: string;
  oauthToken: string;
  log: Logger;
  secretPaths: readonly string[];
  gate: InitGate;
  /** The checker brief, appended to the claude_code preset. */
  brief: string;
  maxTurns?: number;
  model?: string;
  effort?: EffortLevel;
  spawn?: (opts: SpawnOptions) => SpawnedProcess;
}): SdkOptions {
  const options: SdkOptions = {
    cwd: input.cwd,
    // A full replacement: only these keys reach the SDK child (sdk-env.ts).
    env: isolateSdkEnv(buildSdkEnv(input.oauthToken, input.homeDir)) as unknown as Record<string, string | undefined>,
    // Guardrail invariant #6 (semgrep/settings-sources-isolation.yml): nothing from disk, so a
    // cloned repo's .claude/ can grant the checker nothing. Keep this a LITERAL.
    settingSources: [],
    settings: { enabledPlugins: BUILTIN_PLUGINS_DISABLED },
    tools: [...CHECKER_SURFACE.tools],
    disallowedTools: isolatedDisallowedTools(CHECKER_SURFACE),
    mcpServers: {},
    strictMcpConfig: true,
    plugins: [],
    skills: [],
    agents: {},
    systemPrompt: { type: "preset", preset: "claude_code", append: input.brief },
    permissionMode: "bypassPermissions",
    allowDangerouslySkipPermissions: true,
    maxTurns: input.maxTurns ?? DEFAULT_MAX_TURNS,
    hooks: {
      PreToolUse: [
        // The isolated lane's gate, ONE path guard (with this session's spill Read) and write guard.
        ...buildIsolatedPreToolUse({
          surface: CHECKER_SURFACE, gate: input.gate, cwd: input.cwd, log: input.log,
          secretPaths: input.secretPaths, sdkHomeDir: input.homeDir,
        }),
        { matcher: "Glob|Grep", hooks: [buildCheckerPatternGuard(input.log)] },
      ],
    },
    includePartialMessages: false,
    spawnClaudeCodeProcess: input.spawn ?? ((o) => spawnDetached(o) as unknown as SpawnedProcess),
  };
  if (input.model) options.model = input.model;
  if (input.effort) options.effort = input.effort;
  return options;
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

/** Whether a top-level assistant frame carries real model or tool content (not an API-error stub). */
function hasContent(msg: Record<string, unknown>): boolean {
  const content = asRecord(msg["message"])?.["content"];
  return Array.isArray(content) && content.length > 0;
}

export interface ClaudeCrossCheckOptions {
  /** Worker credential paths the path guard denies. Required and non-empty. */
  secretPaths: readonly string[];
  queryFn?: SdkQueryFn;
  spawn?: (opts: SpawnOptions) => { pid?: number };
  kill?: (pid: number | undefined) => boolean;
  maxTurns?: number;
}

export class ClaudeCrossCheck {
  private readonly queryFn: SdkQueryFn;
  private readonly spawn: (opts: SpawnOptions) => { pid?: number };
  private readonly kill: (pid: number | undefined) => boolean;

  constructor(private readonly log: Logger, private readonly opts: ClaudeCrossCheckOptions) {
    if (!opts?.secretPaths || opts.secretPaths.length === 0) {
      throw new Error("ClaudeCrossCheck requires a non-empty secretPaths (the worker-credential deny set)");
    }
    this.queryFn = opts.queryFn ?? defaultQueryFn;
    this.spawn = opts.spawn ?? spawnDetached;
    this.kill = opts.kill ?? killProcessGroup;
  }

  /** Same contract as CodexCrossCheck.run: returns the verdict JSON text or throws. */
  async run(claim: ClaimResponse, checkout: string, home: string, signal: AbortSignal,
    postUsage: (payload: Record<string, unknown>) => Promise<void>): Promise<string> {
    if (claim.kind !== "cross_check") throw new Error("cross-check requires a cross_check claim");
    if (claim.secrets.codex) throw new Error("cross-check Claude path refuses a Codex credential");
    const token = claim.secrets.anthropic_oauth_token?.trim();
    if (!token) throw new CrossCheckCheckerUnavailableError();
    const prompt = crossCheckPrompt(claim);
    const effort = claim.config?.default_effort;
    if (claim.cross_check?.effort_source === "pin" && !EFFORTS.includes(effort ?? "")) {
      throw new CrossCheckCheckerUnavailableError();
    }
    const model = claim.config?.default_model?.trim();
    const modelPinned = claim.cross_check?.model_source === "pin";

    this.log.addSecret(token);
    const gate: InitGate = { passed: false };
    const abort = new AbortController();
    const onCancel = (): void => abort.abort(signal.reason);
    if (signal.aborted) abort.abort(signal.reason);
    else signal.addEventListener("abort", onCancel, { once: true });
    const pids = new Set<number>();
    let usageInit = false;
    let resultPosted = false;
    let failed = true;
    try {
      const options = buildClaudeCrossCheckOptions({
        cwd: checkout, homeDir: home, oauthToken: token, log: this.log, secretPaths: this.opts.secretPaths, gate,
        brief: crossCheckBrief("Read, Grep and Glob"), maxTurns: this.opts.maxTurns,
        ...(model ? { model } : {}), ...(effort ? { effort } : {}),
        spawn: (spawnOpts) => {
          const proc = this.spawn(spawnOpts);
          if (typeof proc.pid === "number") pids.add(proc.pid);
          return proc as unknown as SpawnedProcess;
        },
      });
      options.abortController = abort;
      signal.throwIfAborted();
      let sawContent = false;
      let resultFrame: Record<string, unknown> | undefined;
      // issue #2213: the last statement before the credential-bearing provider spawn.
      assertResidueQuarantineOpen("provider_turn");
      for await (const msg of this.queryFn({ prompt: promptStream(prompt), options })) {
        signal.throwIfAborted();
        const refusal = guardIsolatedFrame(msg, gate, CHECKER_SURFACE);
        if (refusal) throw new Error(`cross-check confinement refused: ${refusal}`);
        const frame = asRecord(msg);
        if (frame?.["type"] === "system" && frame["subtype"] === "init") {
          for (const em of mapSdkMessage(msg)) {
            if (em.payload["event"] === "init") { await postUsage({ ...em.payload, harness: "claude" }); usageInit = true; }
          }
          continue;
        }
        if (frame?.["type"] === "assistant" && (frame["parent_tool_use_id"] ?? null) === null) {
          const error = frame["error"];
          if (error !== undefined && error !== null) {
            // The SDK's typed assistant-frame classification (SDKAssistantMessageError). Only a
            // PINNED model rejected before any model content is the user's configuration; any
            // other error, or the same classification on a worker-default model, stays model_error.
            if (error === "model_not_found" && modelPinned && !sawContent) throw new CrossCheckCheckerUnavailableError();
            throw new Error("cross-check model error");
          }
          if (hasContent(frame)) sawContent = true;
        }
        if (isResult(msg)) { resultFrame = frame; break; }
      }
      // A result without a passing init frame means the tool set was never verified.
      if (!gate.passed) throw new Error("cross-check confinement refused: the SDK never reported its tool set (no init frame)");
      if (!resultFrame) throw new Error("cross-check ended without a result");
      for (const em of mapSdkMessage(resultFrame)) {
        if (em.payload["event"] === "result") { resultPosted = true; await postUsage({ ...em.payload, is_error: isErrorResult(resultFrame) }); }
      }
      if (isErrorResult(resultFrame)) throw new Error("cross-check model error");
      const text = resultFrame["result"];
      if (typeof text !== "string" || Buffer.byteLength(text) > MAX_VERDICT_BYTES) {
        throw new CrossCheckMalformedError("cross-check requires one bounded complete verdict");
      }
      validateVerdict(text);
      failed = false;
      return text;
    } finally {
      signal.removeEventListener("abort", onCancel);
      abort.abort();
      for (const pid of pids) this.kill(pid);
      try {
        if (usageInit && !resultPosted) {
          await postUsage({ event: "result", subtype: "cross_check", is_error: failed || signal.aborted, modelUsage: {} });
        }
      } finally {
        this.log.removeSecret(token);
      }
    }
  }
}

/** The UZI_EXECUTOR=stub checker model call: no network, one init frame matching the checker
 *  surface, then a canned APPROVE (mirrors stubJudgeQueryFn for the other lanes). */
export const stubClaudeCrossCheckQueryFn: SdkQueryFn = (async function* () {
  const init = { type: "system", subtype: "init", model: "stub", session_id: "stub", tools: [...CHECKER_SURFACE.tools],
    mcp_servers: [], plugins: [], agents: [], skills: [] };
  if (checkIsolatedInit(init, CHECKER_SURFACE) !== undefined) throw new Error("stub init frame drifted from the checker surface");
  yield init;
  yield { type: "result", subtype: "success", is_error: false, num_turns: 1,
    result: JSON.stringify({ verdict: "approve", summary: "[stub cross-check] no model call in e2e", items: [] }) };
} as unknown as SdkQueryFn);
