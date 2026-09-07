// The Claude run-lane adapter (PRD #1146 / #1106 M2, milestone m1).
//
// Implements the neutral {@link RunHarness}/{@link HarnessTurn} contract for the
// Claude Agent SDK: it owns the SDK-aware side of the run lane the pure reducer
// deliberately does not — decoding raw SDK frames into neutral {@link HarnessEvent}s,
// the lead context read, process ownership (spawnClaudeCodeProcess + the recorded
// pid set), session inspection, and building the terminal record whose deferred
// `materialize` reconstructs today's exact typed exception. It throws NOTHING
// during decode; a terminal provider failure is neutral data (`HarnessTerminal`),
// materialized only at the owner's classification point.
//
// This adapter ASSEMBLES the per-turn SdkOptions from the owner-computed ingredients
// (`ClaudeTurnConfig`): the workflow owner (`sdk-executor.ts`) still computes every
// ingredient — agents via agents.ts, hooks, in-process MCP server INSTANCES, sparse
// env, skills plugin, model/effort/attribution — but the actual `SdkOptions` object
// (including the literal `settingSources: []` isolation) is built here in
// `buildSdkOptions`, keeping the SDK-shape construction inside the Claude adapter.
// Those live Claude bindings ride the private `ClaudeTurnConfig` construction input,
// NOT the neutral `RunTurnRequest`: agents.ts's live in-process MCP server INSTANCE
// references and the concrete SDK AgentDefinitions cannot be round-tripped through a
// neutral `readonly string[]` surface, which is why they travel on the private config
// rather than the neutral request. `prepareTurn` takes that config; this adapter then
// finalizes each turn (resume, the shared abort controller, the pid-recording spawn)
// and runs the query.

import type {
  Options as SdkOptions,
  SDKMessage,
  SpawnOptions,
  SpawnedProcess,
} from "@anthropic-ai/claude-agent-sdk";
import type { Logger } from "./log.js";
import type { ContextUsageReading, SdkQueryFn } from "./sdk-executor.js";
import {
  assistantModelOf,
  assistantUsageOf,
  decodeAssistantItems,
  decodeAttribution,
  decodeResult,
  decodeUserItems,
  orphanInstanceKind,
  promptStream,
  sessionIdOf,
} from "./sdk-messages.js";
import { isSignalToolName, isSubagentFrame, scanSignals } from "./signals.js";
import {
  isExplicitLimitReason,
  LimitReachedError,
  RateLimitObserver,
  type RateLimitObservation,
} from "./limit.js";
import { sessionTranscriptResolvable } from "./sdk-session.js";
import { ASYNC_DEFERRAL_TOOLS } from "./guardrails.js";
import type {
  HarnessContext,
  HarnessContextHook,
  HarnessEvent,
  HarnessItem,
  HarnessLimitEvidence,
  HarnessLimitFailure,
  HarnessSignalName,
  HarnessTerminal,
  HarnessThrownFailure,
  HarnessTurn,
  RunHarness,
  RunTurnRequest,
  SessionPresence,
  TurnSignals,
} from "./harness.js";

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** The five signalling bare names are exactly the HarnessSignalName members, so a
 *  qualified `mcp__uzi__<bare>` recognized by isSignalToolName maps back to its
 *  neutral name. The reducer only checks presence; the value keeps it honest. */
function signalNameOf(name: string | undefined): HarnessSignalName | undefined {
  if (!isSignalToolName(name)) return undefined;
  const bare = String(name).split("__").pop();
  return bare as HarnessSignalName;
}

/**
 * PRD #516 M1: read the lead session's live context-window fill via the SDK
 * Query's `getContextUsage()` control method, mapped to the pinned payload
 * contract `{ used, window, pct }`. Returns `undefined` — never throws — when the
 * method is absent (existing fakes, older CLI), the call errors, or it hangs past
 * the timeout, so the caller simply attaches no `context` and the turn is
 * unaffected (Risk R1/R2, Success Criteria 5). Moved here from sdk-executor.ts as
 * part of the run-lane extraction; the 2000ms bound and swallow-to-undefined
 * contract are unchanged.
 */
async function readLeadContext(
  queryInstance: { getContextUsage?(): Promise<ContextUsageReading> },
  timeoutMs: number,
): Promise<HarnessContext | undefined> {
  const getContextUsage = queryInstance.getContextUsage;
  if (typeof getContextUsage !== "function") return undefined;
  let timer: NodeJS.Timeout | undefined;
  try {
    const reading = await Promise.race([
      getContextUsage.call(queryInstance),
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error("getContextUsage timed out")),
          timeoutMs,
        );
        timer.unref?.();
      }),
    ]);
    return {
      used: reading.totalTokens,
      window: reading.rawMaxTokens,
      pct: reading.percentage,
    };
  } catch {
    return undefined;
  } finally {
    if (timer) clearTimeout(timer);
  }
}

type SdkQueryInstance = AsyncIterable<SDKMessage> & {
  getContextUsage?(): Promise<ContextUsageReading>;
};

/**
 * The private Claude-specific construction input for a turn (PRD #1146 M2). The
 * workflow owner computes every ingredient — roster, prompts, hooks, LIVE in-process
 * MCP server instances, env, model, effort, attribution — and hands them here; the
 * adapter turns them into the actual `SdkOptions` via {@link buildSdkOptions}. This is
 * deliberately distinct from the neutral `RunTurnRequest`: the live SDK bindings
 * (concrete AgentDefinitions, in-process MCP server INSTANCE references) ride this
 * config because no neutral surface could carry them. It is a TYPE, so knip's
 * `ignoreExportsUsedInFile: {interface,type}` spares the cross-file export.
 */
export interface ClaudeTurnConfig {
  cwd: string;
  env: Record<string, string | undefined>;
  skillsPluginPath: string;
  /** Already plugin-qualified by the owner. */
  skills: readonly string[];
  systemPrompt: SdkOptions["systemPrompt"];
  agents: SdkOptions["agents"];
  mcpServers: SdkOptions["mcpServers"];
  preToolUse: NonNullable<SdkOptions["hooks"]>["PreToolUse"];
  model?: string;
  effort?: SdkOptions["effort"];
  attributionSettings?: SdkOptions["settings"];
}

/**
 * Assemble the per-turn `SdkOptions` from the owner-computed ingredients. Keys are in
 * EXACTLY the insertion order the owner's literal used before the M2 extraction, so a
 * snapshot/stringify comparison in the frozen tests still matches byte-for-byte.
 */
function buildSdkOptions(config: ClaudeTurnConfig): SdkOptions {
  const options: SdkOptions = {
    cwd: config.cwd,
    env: config.env,
    // 🔴 GUARDRAIL INVARIANT #6 — keep this a LITERAL here (Semgrep
    // semgrep/settings-sources-isolation.yml enforces it in agent/src): repo-borne
    // prompt-injection defense, nothing from the cloned repo's .claude can grant the
    // agent permissions.
    settingSources: [],
    plugins: [
      { type: "local", path: config.skillsPluginPath, skipMcpDiscovery: true },
    ],
    skills: [...config.skills],
    systemPrompt: config.systemPrompt,
    agents: config.agents,
    mcpServers: config.mcpServers,
    permissionMode: "bypassPermissions",
    allowDangerouslySkipPermissions: true,
    disallowedTools: [...ASYNC_DEFERRAL_TOOLS],
    hooks: { PreToolUse: config.preToolUse },
    includePartialMessages: false,
  };
  if (config.model) options.model = config.model;
  if (config.effort) options.effort = config.effort;
  if (config.attributionSettings) options.settings = config.attributionSettings;
  return options;
}

export interface ClaudeHarnessDeps {
  queryFn: SdkQueryFn;
  spawn: (opts: SpawnOptions) => { pid?: number };
  kill: (pid: number | undefined) => boolean;
  log: Logger;
  contextUsageTimeoutMs: number;
  /** The run's recorded pid set — shared with the owner, which reaps it in
   *  killAgentTree on the done path (the legacy path stays in the owner). */
  spawnedPids: Set<number>;
  /** Per-run SDK HOME, for session inspection. */
  homeDir: string;
}

export class ClaudeHarness implements RunHarness {
  readonly kind = "claude" as const;

  /** The base SdkOptions and current-child sink set before each turn (see
   *  prepareTurn). The options are ASSEMBLED here from the owner's ClaudeTurnConfig. */
  private nextOptions: SdkOptions | undefined;
  private nextChild: { pid?: number } | undefined;

  /** Object-reuse cache: the assembled SdkOptions for the last distinct config, so a
   *  config reused across turns (the plan config across the first plan + re-plan) is
   *  built ONCE and the same SdkOptions instance is reused — matching today's
   *  "baseOptions is the ONE instance". */
  private lastConfig: ClaudeTurnConfig | undefined;
  private lastOptions: SdkOptions | undefined;

  /** Per-turn lead context read state, re-pointed at each turn's query. The hook is
   *  a stable per-run object the reducer holds; turns run strictly sequentially so
   *  request()/get() always route to the active turn. */
  private ctxReader: (() => Promise<HarnessContext | undefined>) | undefined;
  private ctxPending: Promise<HarnessContext | undefined> | undefined;

  readonly contextHook: HarnessContextHook = {
    request: () => {
      if (this.ctxPending === undefined && this.ctxReader) {
        this.ctxPending = this.ctxReader();
      }
    },
    get: () => this.ctxPending ?? Promise.resolve(undefined),
  };

  constructor(private readonly deps: ClaudeHarnessDeps) {}

  async inspectSession(id: string): Promise<SessionPresence> {
    const resolvable = await sessionTranscriptResolvable(
      this.deps.homeDir,
      id,
      this.deps.log,
    );
    return resolvable ? "present" : "absent";
  }

  /**
   * The owner supplies the per-turn ingredients (plan or implement) as a
   * ClaudeTurnConfig plus the current-child sink trip() kills, immediately before
   * startTurn. This adapter assembles the SdkOptions from the config and finalizes
   * them per turn (resume/abort/spawn) in startTurn.
   */
  prepareTurn(config: ClaudeTurnConfig, currentChild: { pid?: number }): void {
    // Object reuse: the plan config is reused across both plan turns (first plan +
    // re-plan), so assemble ONCE per distinct config object and reuse the same
    // SdkOptions instance — matching today's "baseOptions is the ONE instance".
    if (config !== this.lastConfig) {
      this.lastConfig = config;
      this.lastOptions = buildSdkOptions(config);
    }
    this.nextOptions = this.lastOptions;
    this.nextChild = currentChild;
  }

  startTurn(request: RunTurnRequest): HarnessTurn {
    const base = this.nextOptions;
    const currentChild = this.nextChild;
    if (!base || !currentChild) {
      throw new Error("claude harness: prepareTurn was not called for this turn");
    }

    // The SDK reads this controller; link it to the owner's neutral signal (trip /
    // cancel) and to requestStop. Aborting either stops the query, exactly as the
    // pre-extraction driveTurn aborted its single controller.
    const sdkAbort = new AbortController();
    const onOwnerAbort = (): void => sdkAbort.abort();
    if (request.signal.aborted) sdkAbort.abort();
    else request.signal.addEventListener("abort", onOwnerAbort, { once: true });

    const turnOptions: SdkOptions = { ...base, abortController: sdkAbort };
    if (request.resumeSessionId) turnOptions.resume = request.resumeSessionId;
    else delete turnOptions.resume;
    // Spawn the CLI in its own process group (the owner's injected spawn is
    // detached) so a watchdog trip can group-kill the whole tree; record the pid
    // for the done-path reap and expose it to trip via the owner's currentChild.
    turnOptions.spawnClaudeCodeProcess = (
      spawnOpts: SpawnOptions,
    ): SpawnedProcess => {
      const proc = this.deps.spawn(spawnOpts);
      if (typeof proc.pid === "number") {
        currentChild.pid = proc.pid;
        this.deps.spawnedPids.add(proc.pid);
      }
      return proc as unknown as SpawnedProcess;
    };

    this.ctxReader = undefined;
    this.ctxPending = undefined;

    let queryInstance: SdkQueryInstance | undefined;
    const deps = this.deps;
    const decode = (msg: unknown, latest: RateLimitObservation | undefined): HarnessEvent =>
      this.decode(msg, latest);
    const setReader = (qi: SdkQueryInstance): void => {
      this.ctxReader = () => readLeadContext(qi, deps.contextUsageTimeoutMs);
    };

    async function* events(): AsyncGenerator<HarnessEvent> {
      // Created lazily on first iteration so a synchronous queryFn throw surfaces
      // to the owner's for-await (its trip>throw>terminal precedence handles it),
      // exactly where the pre-extraction loop created the query.
      queryInstance = deps.queryFn({
        prompt: promptStream(request.prompt),
        options: turnOptions,
      });
      setReader(queryInstance);
      const rateLimits = new RateLimitObserver();
      try {
        for await (const msg of queryInstance) {
          // Feed the observer before decode: a rate_limit_event maps to an activity
          // event, so any later placement would never see one (as today).
          rateLimits.observe(msg);
          yield decode(msg, rateLimits.latest);
        }
      } finally {
        request.signal.removeEventListener("abort", onOwnerAbort);
      }
    }

    return {
      events: events(),
      requestStop: () => sdkAbort.abort(),
      readContext: (timeoutMs: number) =>
        queryInstance
          ? readLeadContext(queryInstance, timeoutMs)
          : Promise.resolve(undefined),
      close: async () => {
        sdkAbort.abort();
      },
    };
  }

  /** Decode one raw SDK frame into exactly one neutral event (every input is
   *  liveness). IDs / the orphan detector run on every frame, including ignored
   *  kinds. Throws nothing. */
  private decode(
    msg: unknown,
    latest: RateLimitObservation | undefined,
  ): HarnessEvent {
    const sessionId = sessionIdOf(msg);
    const orphanInstanceFrameKind = orphanInstanceKind(msg);
    const rec = asRecord(msg);
    if (!rec) return { kind: "activity", sessionId, orphanInstanceFrameKind };

    const type = rec["type"];
    if (type === "assistant") {
      const items = decodeAssistantItems(rec);
      // The adapter marks recognized signal tool_uses so the reducer can drop them
      // from persisted output (group-before-filter); results are never marked.
      markSignals(items);
      const usageObj = assistantUsageOf(msg);
      return {
        kind: "frame",
        origin: isSubagentFrame(rec) ? { kind: "subagent" } : { kind: "main" },
        attribution: decodeAttribution(rec),
        items,
        usage:
          usageObj !== undefined
            ? { basis: "call", tokens: {}, wire: { usage: usageObj } }
            : undefined,
        model: assistantModelOf(msg),
        // Main-thread-gated raw signal envelope (scanSignals returns {} for a
        // subagent frame); the reducer additionally gates the fold on origin.
        signals: scanSignals(msg) as Readonly<Partial<TurnSignals>>,
        sessionId,
        orphanInstanceFrameKind,
      };
    }
    if (type === "user") {
      return {
        kind: "frame",
        origin: isSubagentFrame(rec) ? { kind: "subagent" } : { kind: "main" },
        attribution: decodeAttribution(rec),
        items: decodeUserItems(rec),
        // Non-assistant frames carry no signals (scanSignals returns {}).
        signals: scanSignals(msg) as Readonly<Partial<TurnSignals>>,
        sessionId,
        orphanInstanceFrameKind,
      };
    }
    if (type === "result") {
      return {
        kind: "turn_finished",
        terminal: buildTerminal(rec, latest),
        sessionId,
        orphanInstanceFrameKind,
      };
    }
    if (type === "system" && rec["subtype"] === "init") {
      return {
        kind: "initialized",
        model: asString(rec["model"]),
        sessionId,
        orphanInstanceFrameKind,
      };
    }
    return { kind: "activity", sessionId, orphanInstanceFrameKind };
  }
}

/** Mark started-tool items whose name is a recognized signal so the reducer drops
 *  them from persisted output. Mutates in place (the items were freshly decoded). */
function markSignals(items: HarnessItem[]): void {
  for (const item of items) {
    if (item.kind === "tool" && item.phase === "started") {
      const sig = signalNameOf(item.name);
      if (sig) item.signal = sig;
    }
  }
}

/**
 * Build the neutral terminal record for a result frame. The DISPLAY subtype and
 * the wire capsule come from the shared decode; the RAW `subtype ?? "unknown"`
 * value is retained ONLY inside the materialize closure (never the display-
 * stringified one), which reconstructs today's exact typed exception at the
 * owner's classification point — the limit exception with facts, else the generic
 * `agent run failed: ${subtype}`. materialize may throw during malformed-value
 * conversion, at that call site, never here during decode.
 */
function buildTerminal(
  msg: Record<string, unknown>,
  latest: RateLimitObservation | undefined,
): HarnessTerminal {
  const decoded = decodeResult(msg);
  // Mirror the pre-extraction driveTurn's raw errorSubtype EXACTLY: the cast is
  // compile-time only, `??` catches null/undefined, so a non-string truthy subtype
  // (e.g. `false`) is retained verbatim for the presence/truthiness decision.
  const rawSubtype: string = (msg["subtype"] as string) ?? "unknown";

  const limitEvidence: HarnessLimitEvidence = {
    explicitExhaustion: isExplicitLimitReason(msg["terminal_reason"]),
    latest:
      latest !== undefined
        ? {
            status: latest.status,
            resetsAtMs: latest.resetsAtMs,
            window: latest.rateLimitType,
          }
        : undefined,
  };

  return {
    outcome: decoded.outcome,
    subtype: decoded.subtype,
    errors: decoded.errors,
    usage: {
      basis: "session",
      tokens: {},
      wire: { usage: decoded.wire.usage, modelUsage: decoded.wire.modelUsage },
    },
    metrics: {
      cost: { kind: "unreported" },
      wire: {
        num_turns: decoded.wire.num_turns,
        duration_ms: decoded.wire.duration_ms,
        total_cost_usd: decoded.wire.total_cost_usd,
      },
    },
    limitEvidence,
    failure: {
      materialize(limit?: HarnessLimitFailure): HarnessThrownFailure {
        if (limit) {
          const original = new LimitReachedError({
            resetsAtMs: limit.resetsAtMs,
            rateLimitType: limit.window,
            detail: rawSubtype,
          });
          return {
            failure: { category: "rate_limit", message: original.message, limit },
            original,
          };
        }
        const original = new Error(`agent run failed: ${rawSubtype}`);
        return {
          failure: { category: "unknown", message: original.message },
          original,
        };
      },
    },
  };
}
