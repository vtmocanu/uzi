// The Claude checker (PRDs #2460 and #2170): a read-only Claude SDK session that reviews a
// Codex lead's plan candidate or committed code. It is the mirror of codex/cross-check.ts (CodexCrossCheck) and is
// pluggable into CrossCheckRunner with the same `run` signature.
//
// Confinement is the isolated lane's, not a weaker copy (isolated-executor.ts):
//   1. `tools` (not `allowedTools`, which does not restrict under bypassPermissions) is exactly
//      Read, Grep, Glob; `disallowedTools` names every shell, web, nested-agent and write tool.
//   2. A matcher-less PreToolUse gate denies any tool outside that set and EVERY tool until the
//      SDK's `system/init` frame has passed checkIsolatedInit (a latch that defaults to refuse).
//   3. The init check itself: tools, MCP servers, plugins, agents and skills must be exactly the
//      surface; guardIsolatedFrameRefusal refuses model output before a passing init.
//   4. ONE path guard (buildIsolatedPreToolUse -> buildPathGuardHook) rooted at the checkout,
//      worker credential paths and /run/uzi-secrets denied, plus Read of this session's own
//      authenticated SDK spill file (sdkHomeDir). The checker appends only a pattern guard for
//      the Glob `pattern` / Grep `glob` fields, which the path guard does not screen.
// The SDK child runs through spawnDetached (the runner uid under the uid split, a process-group
// kill on abort) with a full-replacement env.

import path from "node:path";
import { CODE_COVERAGE_LIMITS } from "./code-cross-check-coverage.js";
import type { ActivitySettle, CheckActivityRecorder } from "./cross-check-activity.js";
import type { EffortLevel, HookInput, HookJSONOutput, Options as SdkOptions, SpawnOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";

import type { ClaimResponse } from "./protocol.js";
import type { Logger } from "./log.js";
import type { SdkQueryFn } from "./sdk-executor.js";
import { codeCrossCheckBrief, codeCrossCheckBriefWithGuidance, codeGuidanceOf, crossCheckBrief, crossCheckPrompt, CrossCheckMalformedError, validateCodeFindings, validateVerdict } from "./codex/cross-check.js";
import { CrossCheckCheckerUnavailableError } from "./codex/model-rejection.js";
import {
  BUILTIN_PLUGINS_DISABLED,
  buildIsolatedPreToolUse,
  checkIsolatedInit,
  guardIsolatedFrameRefusal,
  isolateSdkEnv,
  isolatedDisallowedTools,
  type InitGate,
  type IsolationSurface,
} from "./isolated-executor.js";
import { buildSdkEnv } from "./sdk-env.js";
import { defaultQueryFn, isErrorResult, isResult, mapSdkMessage, promptStream } from "./sdk-messages.js";
import { killProcessGroup, killProcessGroupOnly, processGroupPresent, spawnDetached } from "./sdk-spawn.js";
import { CrossCheckConfinementError, priorCrossCheckFailure } from "./cross-check-confinement.js";
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
 * latch the caller opens through guardIsolatedFrameRefusal.
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
  toolBudgetHook?: (input: HookInput) => Promise<HookJSONOutput>;
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
        ...(input.toolBudgetHook ? [{ hooks: [input.toolBudgetHook] }] : []),
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

/** Hook reservations and assistant tool_use blocks share IDs, so each call counts once.
 * Root and nested frames both count; the latch refuses every call after exhaustion. */
class CodeToolBudget {
  private readonly ids = new Set<string>();
  private readonly executed = new Set<string>();
  private readonly observed = new Set<string>();
  private readonly denied = new Set<string>();
  private exceeded = false;
  constructor(private readonly abort: AbortController) {}
  /** Whether the budget hook refused this tool call (read-only; activity evidence maps it to `limit`). */
  wasDenied(id: string): boolean { return this.denied.has(id); }
  private admit(id: unknown): boolean {
    if (this.exceeded) return false;
    if (typeof id !== "string" || !id || (!this.ids.has(id) && this.ids.size >= 200)) {
      this.exceeded = true;
      this.abort.abort(new CrossCheckConfinementError("tool_budget"));
      return false;
    }
    this.ids.add(id);
    return true;
  }
  readonly hook = async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse") return {};
    const id = input.tool_use_id;
    if (!this.executed.has(id) && this.admit(id)) { this.executed.add(id); return {}; }
    this.exceeded = true;
    if (typeof id === "string") this.denied.add(id);
    this.abort.abort(new CrossCheckConfinementError("tool_budget"));
    return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny",
      permissionDecisionReason: "cross-check tool budget exceeded" } };
  };
  observe(frame: Record<string, unknown> | undefined): void {
    if (frame?.["type"] !== "assistant") return;
    const content = asRecord(frame["message"])?.["content"];
    if (Array.isArray(content)) for (const raw of content) {
      const block = asRecord(raw);
      if (block?.["type"] === "tool_use") {
        const id = block["id"];
        if (!CHECKER_SURFACE.tools.includes(String(block["name"]))) throw new CrossCheckConfinementError("tool_unallowed");
        if (typeof id === "string" && this.observed.has(id)) throw new CrossCheckConfinementError("tool_repeated");
        if (this.admit(id)) this.observed.add(id as string);
      }
    }
    this.assertWithinBudget();
  }
  assertWithinBudget(): void {
    if (this.exceeded) throw new CrossCheckConfinementError("tool_budget");
  }
}

function activityOp(tool: string): "read" | "search" | undefined {
  if (tool === "Read") return "read";
  return tool === "Grep" || tool === "Glob" ? "search" : undefined;
}

const ACTIVITY_PENDING_CAP = 256;
const ACTIVITY_ID_MAX_BYTES = 128;

function nonNegSafeInt(v: unknown): number | undefined {
  return typeof v === "number" && Number.isSafeInteger(v) && v >= 0 ? v : undefined;
}

function toolResultText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  let out = "";
  for (const raw of content) {
    const text = asRecord(raw)?.["text"];
    if (typeof text === "string") out += text + "\n";
  }
  return out;
}

/** Maps SDK assistant tool_use and user tool_result frames onto the activity recorder (PRD #3166).
 * Reads only ids, tool names, path fields and the structured result's counts: never a pattern,
 * glob or file content. Every recorder call is guarded so telemetry cannot fail the check. */
class ActivityMapper {
  private readonly begun = new Map<string, { tool: string; rawPath?: string }>();
  constructor(private readonly recorder: CheckActivityRecorder, private readonly checkout: string,
    private readonly budget: CodeToolBudget | undefined) {}

  observe(frame: Record<string, unknown> | undefined): void {
    try {
      if (frame?.["type"] === "assistant") this.onAssistant(frame);
      else if (frame?.["type"] === "user") this.onUser(frame);
    } catch { /* telemetry only */ }
  }

  private onAssistant(frame: Record<string, unknown>): void {
    const content = asRecord(frame["message"])?.["content"];
    if (!Array.isArray(content)) return;
    for (const raw of content) {
      const block = asRecord(raw);
      if (block?.["type"] !== "tool_use") continue;
      const tool = String(block["name"]);
      const op = activityOp(tool);
      const id = block["id"];
      if (!op) continue;
      // Mirror the recorder bounds: an id it would refuse, or one past the cap, is not tracked here,
      // but it still reaches recorder.begin so the recorder counts it untracked (summary incomplete).
      if (typeof id !== "string") { this.recorder.begin(id, op); continue; }
      const idBytes = Buffer.byteLength(id);
      if (idBytes < 1 || idBytes > ACTIVITY_ID_MAX_BYTES || this.begun.size >= ACTIVITY_PENDING_CAP) {
        this.recorder.begin(id, op);
        continue;
      }
      const input = asRecord(block["input"]);
      const rawPath = tool === "Read" ? input?.["file_path"] : input?.["path"];
      this.begun.set(id, { tool, ...(typeof rawPath === "string" ? { rawPath } : {}) });
      this.recorder.begin(id, op, rawPath);
    }
  }

  private onUser(frame: Record<string, unknown>): void {
    const content = asRecord(frame["message"])?.["content"];
    if (!Array.isArray(content)) return;
    const blocks = content.map(asRecord).filter(b => b?.["type"] === "tool_result");
    // The frame-level structured result belongs to a single tool_result only.
    const structured = blocks.length === 1 ? asRecord(frame["tool_use_result"]) : undefined;
    for (const block of blocks) {
      const id = block?.["tool_use_id"];
      if (typeof id !== "string") continue;
      const info = this.begun.get(id);
      if (!info) continue;
      this.begun.delete(id);
      if (block?.["is_error"] !== true) { this.recorder.settle(id, this.okResult(info.tool, structured)); continue; }
      if (this.budget?.wasDenied(id)) { this.recorder.settle(id, { outcome: "limit" }); continue; }
      if (toolResultText(block["content"]).includes(REASON_PATTERN)) {
        this.recorder.settle(id, { outcome: "denied", category: "outside_checkout" });
      } else if (this.outsideCheckout(info.rawPath)) this.recorder.settle(id, { outcome: "denied" });
      else this.recorder.settle(id, { outcome: "error" });
    }
  }

  private okResult(tool: string, structured: Record<string, unknown> | undefined): ActivitySettle {
    if (tool === "Read") {
      const file = asRecord(structured?.["file"]);
      const start = nonNegSafeInt(file?.["startLine"]);
      const num = nonNegSafeInt(file?.["numLines"]);
      if (start !== undefined && num !== undefined && start >= 1 && num > 0) return { outcome: "ok", lines: [start, start + num - 1] };
      return { outcome: "ok" };
    }
    const matches = tool === "Grep" ? nonNegSafeInt(structured?.["numMatches"]) ?? nonNegSafeInt(structured?.["numFiles"])
      : nonNegSafeInt(structured?.["numFiles"]);
    if (matches === undefined) return { outcome: "ok" };
    return tool === "Glob" && structured?.["truncated"] === true
      ? { outcome: "ok", matches, matchesTruncated: true } : { outcome: "ok", matches };
  }

  private outsideCheckout(rawPath: string | undefined): boolean {
    if (!rawPath) return false;
    const rel = path.relative(path.resolve(this.checkout), path.resolve(this.checkout, rawPath));
    return rel === ".." || rel.startsWith(".." + path.sep) || path.isAbsolute(rel);
  }
}

/** A timeout never certifies a still-pending reader shutdown. */
async function boundedCleanup<T>(work: Promise<T>, deadline: number): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([work, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error("cleanup deadline exceeded")), Math.max(1, deadline - Date.now()));
    })]);
  } finally { if (timer) clearTimeout(timer); }
}

export interface ClaudeCrossCheckOptions {
  /** Worker credential paths the path guard denies. Required and non-empty. */
  secretPaths: readonly string[];
  queryFn?: SdkQueryFn;
  spawn?: (opts: SpawnOptions) => { pid?: number };
  kill?: (pid: number | undefined) => boolean;
  /** Code cleanup uses group-only signals, never a reused bare PID. */
  killGroupOnly?: (pgid: number) => boolean;
  groupPresent?: (pgid: number) => boolean | undefined;
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
    postUsage: (payload: Record<string, unknown>) => Promise<void>, confirmNativeCleanup?: () => void,
    /** Activity recorder (PRD #3166): fed paths, line ranges and counts from SDK frames, never queries or content. */
    activity?: CheckActivityRecorder): Promise<string> {
    if (claim.kind !== "cross_check") throw new Error("cross-check requires a cross_check claim");
    if (claim.secrets.codex) throw new Error("cross-check Claude path refuses a Codex credential");
    const token = claim.secrets.anthropic_oauth_token?.trim();
    if (!token) throw new CrossCheckCheckerUnavailableError();
    const prompt = crossCheckPrompt(claim);
    const code = claim.cross_check?.stage === "code";
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
    let unknownSpawn = false;
    let iterator: AsyncIterator<unknown> | undefined;
    let readersStopped = false;
    let queryStarted = false;
    const budget = code ? new CodeToolBudget(abort) : undefined;
    const mapper = activity ? new ActivityMapper(activity, checkout, budget) : undefined;
    let usageInit = false;
    let resultPosted = false;
    let failed = true;
    let completedText: string | undefined;
    let failure: { error: unknown } | undefined;
    let nativeClean = false;
    try {
      const options = buildClaudeCrossCheckOptions({
        cwd: checkout, homeDir: home, oauthToken: token, log: this.log, secretPaths: this.opts.secretPaths, gate,
        brief: code ? codeCrossCheckBriefWithGuidance("Read, Grep and Glob", codeGuidanceOf(claim)) : crossCheckBrief("Read, Grep and Glob"), maxTurns: this.opts.maxTurns,
        ...(budget ? { toolBudgetHook: budget.hook } : {}),
        ...(model ? { model } : {}), ...(effort ? { effort } : {}),
        spawn: (spawnOpts) => {
          const proc = this.spawn(spawnOpts);
          if (Number.isSafeInteger(proc.pid) && proc.pid! > 0) pids.add(proc.pid!);
          else unknownSpawn = true;
          return proc as unknown as SpawnedProcess;
        },
      });
      options.abortController = abort;
      signal.throwIfAborted();
      let sawContent = false;
      let resultFrame: Record<string, unknown> | undefined;
      // issue #2213: the last statement before the credential-bearing provider spawn.
      assertResidueQuarantineOpen("provider_turn");
      queryStarted = true;
      iterator = this.queryFn({ prompt: promptStream(prompt), options })[Symbol.asyncIterator]();
      while (true) {
        const next = await iterator.next();
        if (next.done) { readersStopped = true; break; }
        const msg = next.value;
        signal.throwIfAborted();
        // Before the budget latch can throw: a hook-denied call's tool_result must still settle as `limit`.
        mapper?.observe(asRecord(msg));
        budget?.assertWithinBudget();
        const refusal = guardIsolatedFrameRefusal(msg, gate, CHECKER_SURFACE);
        if (refusal) throw new CrossCheckConfinementError(refusal.code);
        const frame = asRecord(msg);
        budget?.observe(frame);
        if (frame?.["type"] === "system" && frame["subtype"] === "init") {
          // The validated init frame is the first evidence that the model session started.
          try { activity?.modelStarted(); } catch { /* telemetry only */ }
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
      if (!gate.passed) throw new CrossCheckConfinementError("no_init");
      if (!resultFrame) throw new Error("cross-check ended without a result");
      for (const em of mapSdkMessage(resultFrame)) {
        if (em.payload["event"] === "result") { resultPosted = true; await postUsage({ ...em.payload, is_error: isErrorResult(resultFrame) }); }
      }
      if (isErrorResult(resultFrame)) throw new Error("cross-check model error");
      const text = resultFrame["result"];
      if (typeof text !== "string" || Buffer.byteLength(text) > (code ? CODE_COVERAGE_LIMITS.verdictBytes : MAX_VERDICT_BYTES)) {
        throw new CrossCheckMalformedError("cross-check requires one bounded complete verdict", "result_cap");
      }
      if (code) validateCodeFindings(text);
      else validateVerdict(text);
      failed = false;
      completedText = text;
    } catch (err) {
      failure = { error: err };
    } finally {
      signal.removeEventListener("abort", onCancel);
      abort.abort();
      // Stop query readers before certifying owned native groups. One shared 5s deadline,
      // no retry of reader shutdown; a failed group does not prevent sibling cleanup.
      const deadline = Date.now() + 5000;
      if (iterator && !readersStopped) {
        try {
          if (iterator.return) {
            const stopped = await boundedCleanup(iterator.return(), deadline);
            readersStopped = stopped.done === true;
          }
        } catch { readersStopped = false; }
      }
      let clean = !unknownSpawn && (!queryStarted || readersStopped);
      if (code || confirmNativeCleanup) {
        const present = this.opts.groupPresent ?? processGroupPresent;
        const kill = this.opts.killGroupOnly ?? killProcessGroupOnly;
        for (const pid of pids) {
          try {
            let state = present(pid);
            if (state === true) {
              kill(pid);
              while (state === true && Date.now() < deadline) {
                await new Promise(resolve => setTimeout(resolve, Math.min(25, Math.max(1, deadline - Date.now()))));
                state = present(pid);
              }
            }
            if (state !== false) clean = false;
          } catch { clean = false; }
        }
      } else for (const pid of pids) this.kill(pid);
      nativeClean = clean;
      try {
        if (usageInit && !resultPosted) {
          await postUsage({ event: "result", subtype: "cross_check", is_error: failed || signal.aborted, modelUsage: {} });
        }
      } catch (error) {
        failure ??= { error };
      } finally {
        this.log.removeSecret(token);
      }
    }
    if ((code || confirmNativeCleanup) && !nativeClean) throw new CrossCheckConfinementError("cleanup_unconfirmed", failure ? priorCrossCheckFailure(failure.error) : undefined);
    if (nativeClean) confirmNativeCleanup?.();
    budget?.assertWithinBudget();
    if (failure) throw failure.error;
    if (completedText === undefined) throw new Error("cross-check ended without a result");
    return completedText;
  }
}

/** The UZI_EXECUTOR=stub checker model call: no network, one init frame matching the checker
 *  surface, then canned stage-specific output (mirrors stubJudgeQueryFn for the other lanes).
 *  Only the worker's trusted system brief selects the stage; prompt DATA has no authority. */
export const stubClaudeCrossCheckQueryFn: SdkQueryFn = (async function* ({ options }: Parameters<SdkQueryFn>[0]) {
  const systemPrompt = options.systemPrompt;
  const code = typeof systemPrompt === "object" && !Array.isArray(systemPrompt) &&
    systemPrompt.type === "preset" && typeof systemPrompt.append === "string" &&
    // The locked brief is the prefix; the owner-guidance block, when frozen, follows it.
    systemPrompt.append.startsWith(codeCrossCheckBrief("Read, Grep and Glob"));
  const init = { type: "system", subtype: "init", model: "stub", session_id: "stub", tools: [...CHECKER_SURFACE.tools],
    mcp_servers: [], plugins: [], agents: [], skills: [] };
  if (checkIsolatedInit(init, CHECKER_SURFACE) !== undefined) throw new Error("stub init frame drifted from the checker surface");
  yield init;
  yield { type: "result", subtype: "success", is_error: false, num_turns: 1,
    result: code ? '{"findings":[]}' : JSON.stringify({ verdict: "approve", summary: "[stub cross-check] no model call in e2e", items: [] }) };
} as unknown as SdkQueryFn);
