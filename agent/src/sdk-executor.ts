// The Claude Agent SDK executor (PRD #4 M3 + M4). Replaces the M2 stub behind the
// same `Executor` interface, so the runner's claim → worktree → work → report
// state machine is unchanged; only the "work" step drives a real agent.
//
// M4 turns the single M3 turn into the full workflow: a PLANNING turn whose plan
// is gated on human approval (ctx.gatePlan), then an implement⇄review LOOP of
// resumed turns (bottega's model — no mid-turn injection), capped at
// RUN_MAX_ITERATIONS, exiting when the lead calls `signal_done`. Follow-up
// corrections are injected between turns via SDK session resume. Cancel aborts
// the current turn's subprocess group. The WORKER performs the branch push + MR
// after this returns; the executor never holds a credential.
//
// Every agent subprocess spawned across the run's turns is group-killed before
// this returns (killAgentTree, run() finally) — the DONE path, not just a
// watchdog/cancel trip — so an agent-backgrounded process can never survive to
// read the PAT out of the worker's git-push subprocess env (M4 audit B1).
//
// Everything that touches the network is behind an injectable `queryFn` (default
// = the SDK's `query`). Tests pass a fake that returns a controllable message
// stream, so the gate, the loop, the guardrails, sparse env, session/resume, and
// watchdogs are all provable with NO live Anthropic session and NO real token
// (testing-credentials policy). The plan/done signals are observed from that
// stream (see signals.ts), so a scripted fake proves them without a live SDK.

import fs from "node:fs/promises";
import { query as sdkQuery } from "@anthropic-ai/claude-agent-sdk";
import type { Options as SdkOptions, SDKMessage, SpawnOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";
import type { Executor, ExecutorResult, RunContext } from "./executor.js";
import type { Logger } from "./log.js";
import { buildSdkEnv } from "./sdk-env.js";
import { assembleAgents } from "./agents.js";
import { buildImplementPrompt, buildLeadSystemPrompt, buildPlanPrompt } from "./prompt.js";
import { buildPreToolUseHook, buildPathGuardHook, buildAgentGuardHook, NESTED_AGENT_TOOL, ASYNC_DEFERRAL_TOOLS } from "./guardrails.js";
import { buildSignalMcpServer, isSignalToolName, scanSignals, SIGNAL_SERVER_NAME } from "./signals.js";
import { qualifiedSkillName, type SkillDrop } from "./skills-plugin.js";
import { prepareSkillPlugin, resolveSkillCaps } from "./skills-run.js";
import { killProcessGroup, spawnDetached } from "./sdk-spawn.js";
import { isErrorResult, isResult, mapSdkMessage, sessionIdOf } from "./sdk-messages.js";
import { PlanRejectedError } from "./executor.js";
import { errMessage } from "./util.js";

// Fallbacks used only when the claim omits `config`. Wire units are SECONDS
// (PRD §Configuration); converted to ms at the timer.
const DEFAULT_RUN_TIMEOUT_SECONDS = 2 * 60 * 60; // 2h
const DEFAULT_IDLE_TIMEOUT_SECONDS = 10 * 60; // 10m
const DEFAULT_MAX_ITERATIONS = 5; // PRD: RUN_MAX_ITERATIONS default 5

// Static (content-free) failure reasons — safe to persist as failure_reason.
const REASON_WALL = "run exceeded its wall-clock timeout";
const REASON_IDLE = "run stalled: no agent activity within the idle timeout";
const REASON_CANCELLED = "run cancelled";
const REASON_NO_TOKEN = "no Anthropic OAuth token was provided for this run";
const REASON_NO_PLAN = "the agent ended the planning turn without submitting a plan";
const REASON_MAX_ITERATIONS = "run reached the maximum implement/review iterations without completing";

/**
 * Injectable seam over the SDK's `query`. The return only needs to be async
 * iterable for the executor; cancellation goes through `options.abortController`
 * and the process-group kill uses the pid captured by `spawnClaudeCodeProcess`.
 */
export type SdkQueryFn = (params: {
  prompt: AsyncIterable<unknown>;
  options: SdkOptions;
}) => AsyncIterable<SDKMessage>;

const defaultQueryFn: SdkQueryFn = (params) =>
  sdkQuery({ prompt: params.prompt as never, options: params.options });

export interface SdkExecutorOptions {
  /** Override the SDK entrypoint (tests inject a fake transport here). */
  queryFn?: SdkQueryFn;
  /** Spawn the SDK CLI (default = detached spawn). Injected in tests. */
  spawn?: (opts: SpawnOptions) => { pid?: number };
  /** Group-kill a pid (default = killProcessGroup). Injected in tests. */
  kill?: (pid: number | undefined) => boolean;
  /** Worker-credential file paths (UZI_WORKER_TOKEN_FILE) the Bash guard denies. */
  secretPaths?: readonly string[];
}

/** What one turn observed: the session id, and any workflow signals. */
interface TurnResult {
  sessionId?: string;
  plan?: string;
  done: boolean;
}

/** Run-level watchdog/cancel state shared across the plan turn and every loop turn. */
interface RunDrive {
  tripReason?: string;
  currentAbort?: AbortController;
  currentChild: { pid?: number };
  wallRemainingMs: number;
  wallArmedAt?: number;
  wallTimer?: NodeJS.Timeout;
  reportedSessionId: boolean;
}

export class SdkExecutor implements Executor {
  private readonly queryFn: SdkQueryFn;
  private readonly spawn: (opts: SpawnOptions) => { pid?: number };
  private readonly kill: (pid: number | undefined) => boolean;
  private readonly secretPaths: readonly string[];
  /** Every pid spawned across the current run's turns, for the done-path reap. */
  private readonly spawnedPids = new Set<number>();

  /**
   * @param homeDir pinned HOME (a dir on $UZI_DATA_DIR) so SDK session
   *   transcripts under $HOME/.claude/projects survive a container restart.
   */
  constructor(
    private readonly log: Logger,
    private readonly homeDir: string,
    opts: SdkExecutorOptions = {},
  ) {
    this.queryFn = opts.queryFn ?? defaultQueryFn;
    this.spawn = opts.spawn ?? spawnDetached;
    this.kill = opts.kill ?? killProcessGroup;
    this.secretPaths = opts.secretPaths ?? [];
  }

  /**
   * Group-kill every subprocess the agent spawned across this run's turns (M4
   * audit B1). The default per-turn `abort()` only signals the SDK CLI pid, not
   * its process group, so an agent-backgrounded process (`nohup … &`) survives —
   * and could read the PAT out of the git child's /proc/environ during the
   * worker's push. The runner calls this before pushBranch; run()'s finally also
   * calls it so no path leaks an orphan. Idempotent: the set is cleared so a
   * recycled pid is never re-signalled.
   */
  killAgentTree(): void {
    for (const pid of this.spawnedPids) this.kill(pid);
    this.spawnedPids.clear();
  }

  /**
   * Log every dropped skill as a run message (PRD #16): the server's assembly
   * drops that rode the claim (ctx.skillsDropped — shadowed / over-limit) plus the
   * worker's own local cap drops (too_large / over_limit over the combined set).
   * The worker owns the gapless per-run seq, so the SERVER never writes these; it
   * hands the worker the {name, reason} list to emit. One status line per drop.
   */
  private emitSkillDrops(ctx: RunContext, localDrops: readonly SkillDrop[]): void {
    const all: SkillDrop[] = [...(ctx.skillsDropped ?? []), ...localDrops];
    for (const d of all) {
      ctx.emit({ kind: "status", agent: "worker", payload: { text: describeSkillDrop(d.name, d.reason) } });
    }
  }

  async run(ctx: RunContext): Promise<ExecutorResult> {
    this.spawnedPids.clear();
    const oauthToken = ctx.oauthToken?.trim();
    if (!oauthToken) {
      // OAuth is the sole supported credential (no API keys). Detect its
      // absence up front and fail fast rather than spawning a doomed CLI.
      throw new Error(REASON_NO_TOKEN);
    }

    await fs.mkdir(this.homeDir, { recursive: true });

    const env = buildSdkEnv(oauthToken, this.homeDir);
    const maxIterations = positive(ctx.config?.max_iterations, DEFAULT_MAX_ITERATIONS);

    // Skills (PRD #16 M4 + M6). Assemble the run's skill set and materialize a
    // local plugin dir OUTSIDE the clone (loads under `settingSources: []`, so the
    // injection defense never loosens), rebuilt on every claim incl. resume. The
    // same prepareSkillPlugin path the stub executor uses, so the two never drift.
    // The worker owns the gapless seq, so it logs every dropped skill.
    const prepared = await prepareSkillPlugin(ctx, resolveSkillCaps(ctx.config));
    const runSkills = prepared.runSkills;
    const skillsPluginPath = prepared.pluginPath;
    this.emitSkillDrops(ctx, prepared.drops);

    // Subagents: each def.skills is its allocated delivered skills (re-filtered to
    // the materialized survivors, so it never lists a uzi:<name> not in the plugin
    // dir) plus the all-templates repo survivors (PRD §Worker point 3).
    const survivorNames = new Set(runSkills.map((s) => s.name));
    const assembled = assembleAgents(ctx.agents ?? [], survivorNames, prepared.repoSurvivorNames);
    const subagentNames = Object.keys(assembled.subagents);

    const baseOptions: SdkOptions = {
      cwd: ctx.worktreePath,
      // Full replacement — only these keys reach the agent subprocess.
      env: env as unknown as Record<string, string | undefined>,
      // Repo-borne prompt-injection defense: nothing from the cloned repo's
      // .claude/{settings.json,agents,hooks} can grant the agent permissions.
      settingSources: [],
      // Skills (PRD #16 M4): a local plugin dir OUTSIDE the clone, loaded
      // independently of settingSources (SdkPluginConfig is a separate option),
      // so the isolation above never loosens. skipMcpDiscovery: this plugin ships
      // ONLY skills — the SDK host owns MCP, so never read a manifest/.mcp.json.
      plugins: [{ type: "local", path: skillsPluginPath, skipMcpDiscovery: true }],
      // ALWAYS an explicit list — the full plugin-qualified run union. Omitting it
      // is NOT "skills off" (sdk.d.ts:1872: CLI defaults would apply); `[]` when the
      // run has no skills disables all. Per-subagent scoping is each
      // AgentDefinition.skills; the lead is the main thread, covered by this union.
      skills: runSkills.map((s) => qualifiedSkillName(s.name)),
      systemPrompt: buildLeadSystemPrompt(assembled.leadSystemPrompt),
      agents: assembled.subagents,
      // In-process signalling tools the lead calls to gate the plan and mark done
      // (see signals.ts). Only the lead (full toolset) can reach them.
      mcpServers: { [SIGNAL_SERVER_NAME]: buildSignalMcpServer() },
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      // Block the deferral tools so the lead can't background work to a future
      // turn that the per-turn reap would only wake to a killed subagent (#34).
      // Delegation is forced synchronous by the Agent guard hook below.
      disallowedTools: [...ASYNC_DEFERRAL_TOOLS],
      // The load-bearing deny layer: a PreToolUse deny blocks a tool even under
      // bypassPermissions. Bash screening, the file-tool path jail, AND the M4
      // hard-fail-on-unexpected-subagent guard (item 7) all live here.
      hooks: {
        PreToolUse: [
          { matcher: "Bash", hooks: [buildPreToolUseHook(this.log, this.secretPaths)] },
          {
            matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep",
            hooks: [buildPathGuardHook(ctx.worktreePath, this.log)],
          },
          { matcher: NESTED_AGENT_TOOL, hooks: [buildAgentGuardHook(subagentNames, this.log)] },
        ],
      },
      // Persist discrete blocks only; partial token deltas would flood the seq
      // stream (the live-partial channel is M5, not M3).
      includePartialMessages: false,
    };
    // Model precedence (PRD #17 Decision 6): the run owner's per-user default
    // model wins over the lead template's model for the main thread. Set the key
    // ONLY when a model is resolved — `model: undefined` must stay omitted (never
    // an explicit key), so an unset model falls back to the SDK/account default.
    const leadModel = resolveLeadModel(ctx.config?.default_model, assembled.leadModel);
    if (leadModel) baseOptions.model = leadModel;

    const state: RunDrive = {
      currentChild: {},
      wallRemainingMs: seconds(ctx.config?.run_timeout_seconds, DEFAULT_RUN_TIMEOUT_SECONDS),
      reportedSessionId: false,
    };
    const idleMs = seconds(ctx.config?.idle_timeout_seconds, DEFAULT_IDLE_TIMEOUT_SECONDS);

    // Cancel/shutdown spans the whole run (all turns + the gate). It trips the
    // current turn's abort; the gate also unblocks via the steering verdict.
    const onSignal = (): void => this.trip(state, REASON_CANCELLED);
    if (ctx.signal) {
      if (ctx.signal.aborted) onSignal();
      else ctx.signal.addEventListener("abort", onSignal, { once: true });
    }

    // The SDK session id evolves across turns; resume each turn from the last.
    let resumeId = ctx.sessionId ?? undefined;

    try {
      // --- Phase 1: planning turn ------------------------------------------
      ctx.emit({ kind: "status", agent: "worker", payload: { text: "starting SDK agent (planning)" } });
      const plan = await this.driveTurn(ctx, baseOptions, resumeId, buildPlanPrompt({
        issueIid: ctx.issueIid,
        issueTitle: ctx.issueTitle,
        issueDescription: ctx.issueDescription,
        branch: ctx.branch,
        subagentNames,
      }), state, idleMs);
      resumeId = plan.sessionId ?? resumeId;
      // A planning turn that ends without a plan is an error — never push
      // un-gated work, even if the lead prematurely signalled done.
      if (plan.plan === undefined) throw new Error(REASON_NO_PLAN);

      // --- Plan gate --------------------------------------------------------
      if (!ctx.gatePlan) throw new Error("plan gate is not wired for this run");
      const verdict = await ctx.gatePlan(plan.plan);
      if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
      if (verdict.kind === "cancel") throw new Error(REASON_CANCELLED);

      // --- Phase 2: implement ⇄ review loop --------------------------------
      let iteration = 0;
      let followUp: string | undefined;
      for (;;) {
        iteration++;
        ctx.reportIteration?.(iteration);
        ctx.emit({ kind: "status", agent: "worker", payload: { text: `implement/review iteration ${iteration}` } });
        const turn = await this.driveTurn(ctx, baseOptions, resumeId, buildImplementPrompt({
          branch: ctx.branch,
          subagentNames,
          first: iteration === 1,
          iteration,
          followUp,
        }), state, idleMs);
        resumeId = turn.sessionId ?? resumeId;
        if (turn.done) break;
        if (iteration >= maxIterations) throw new Error(REASON_MAX_ITERATIONS);
        // Fold any queued correction into the next turn (FIFO, one per turn).
        followUp = ctx.pullFollowUp?.();
      }

      this.log.info("SDK run completed", { run_id: ctx.runId, branch: ctx.branch });
      return { branch: ctx.branch };
    } finally {
      this.disarmWall(state);
      if (ctx.signal) ctx.signal.removeEventListener("abort", onSignal);
      // Reap every agent subprocess before returning, so none survives into the
      // worker's PAT-bearing push (B1). Covers the failure/cancel/no-plan paths
      // too, not just the runner's explicit pre-push call.
      this.killAgentTree();
    }
  }

  /** Drive ONE SDK turn to its result frame, capturing signals + the session id. */
  private async driveTurn(
    ctx: RunContext,
    baseOptions: SdkOptions,
    resumeId: string | undefined,
    prompt: string,
    state: RunDrive,
    idleMs: number,
  ): Promise<TurnResult> {
    // A trip may already be pending (e.g. a cancel that landed during the gate).
    if (state.tripReason) throw new Error(state.tripReason);

    const abortController = new AbortController();
    state.currentAbort = abortController;
    state.currentChild = {};

    const options: SdkOptions = { ...baseOptions, abortController };
    if (resumeId) options.resume = resumeId;
    else delete options.resume;
    // Spawn the CLI in its own process group so a watchdog trip can group-kill
    // the whole tree (default SDK spawn is not detached).
    options.spawnClaudeCodeProcess = (spawnOpts: SpawnOptions): SpawnedProcess => {
      const proc = this.spawn(spawnOpts);
      if (typeof proc.pid === "number") {
        state.currentChild.pid = proc.pid;
        this.spawnedPids.add(proc.pid); // reaped on the done path (B1)
      }
      return proc as unknown as SpawnedProcess;
    };

    let idleTimer: NodeJS.Timeout | undefined;
    const armIdle = (): void => {
      if (idleTimer) clearTimeout(idleTimer);
      idleTimer = setTimeout(() => this.trip(state, REASON_IDLE), idleMs);
      idleTimer.unref?.();
    };

    const result: TurnResult = { done: false };
    let turnSessionId: string | undefined;
    let sawErrorResult = false;
    let errorSubtype = "unknown";

    this.armWall(state);
    // Budget already spent by earlier turns → fail now rather than run unbounded.
    if (state.tripReason) throw new Error(state.tripReason);
    try {
      armIdle();
      const queryInstance = this.queryFn({ prompt: promptStream(prompt), options });
      for await (const msg of queryInstance) {
        armIdle(); // any message is liveness

        const sid = sessionIdOf(msg);
        if (sid) {
          turnSessionId = sid;
          if (!state.reportedSessionId) {
            state.reportedSessionId = true;
            try {
              ctx.onSessionId?.(sid);
            } catch (err) {
              this.log.warn("onSessionId handler threw", { run_id: ctx.runId, error: errMessage(err) });
            }
          }
        }

        // Emit everything EXCEPT the signal tool_use blocks — the plan is
        // surfaced as a `plan` message by the runner, not duplicated as a raw
        // tool_use payload, and signal_done is infra noise.
        for (const em of mapSdkMessage(msg)) {
          if (em.kind === "tool_use" && isSignalToolName(em.payload["name"])) continue;
          ctx.emit(em);
        }
        const sig = scanSignals(msg);
        if (sig.plan !== undefined) result.plan = sig.plan;
        if (sig.done) result.done = true;

        if (isResult(msg)) {
          if (isErrorResult(msg)) {
            sawErrorResult = true;
            errorSubtype = ((msg as { subtype?: unknown }).subtype as string) ?? "unknown";
          }
          // The turn is done. Abort so a lingering background bash the agent left
          // running can't pin the iterator open (bottega's pattern).
          abortController.abort();
          break;
        }
      }

      if (state.tripReason) throw new Error(state.tripReason);
      if (sawErrorResult) throw new Error(`agent run failed: ${errorSubtype}`);
      result.sessionId = turnSessionId;
      return result;
    } catch (err) {
      // A watchdog/cancel trip surfaces as its static reason, not the raw
      // AbortError the aborted iterator throws.
      if (state.tripReason) throw new Error(state.tripReason);
      throw err instanceof Error ? err : new Error(errMessage(err));
    } finally {
      if (idleTimer) clearTimeout(idleTimer);
      this.disarmWall(state);
      state.currentAbort = undefined;
    }
  }

  /** Record a first-wins watchdog/cancel trip and stop the current turn. */
  private trip(state: RunDrive, reason: string): void {
    if (state.tripReason) return; // first trip wins
    state.tripReason = reason;
    this.log.warn("run watchdog/cancel tripped, aborting SDK", { reason });
    // abort() is the primary, asserted stop (SDK: stdin EOF → grace → signal);
    // the group kill is best-effort defense for orphaned children.
    state.currentAbort?.abort();
    this.kill(state.currentChild.pid);
  }

  /** Arm the wall-clock budget for the active turn (paused across the gate). */
  private armWall(state: RunDrive): void {
    if (state.wallTimer) return;
    if (state.wallRemainingMs <= 0) {
      this.trip(state, REASON_WALL);
      return;
    }
    state.wallArmedAt = Date.now();
    state.wallTimer = setTimeout(() => this.trip(state, REASON_WALL), state.wallRemainingMs);
    state.wallTimer.unref?.();
  }

  /** Pause the wall budget, debiting the elapsed active time. */
  private disarmWall(state: RunDrive): void {
    if (!state.wallTimer) return;
    clearTimeout(state.wallTimer);
    state.wallTimer = undefined;
    if (state.wallArmedAt !== undefined) {
      state.wallRemainingMs -= Date.now() - state.wallArmedAt;
      state.wallArmedAt = undefined;
    }
  }
}

/** One-shot prompt stream: the SDK consumes the lead's user turn. */
async function* promptStream(text: string): AsyncGenerator<unknown> {
  yield { type: "user", message: { role: "user", content: text }, parent_tool_use_id: null };
}

/** Convert an optional seconds value (any number tolerated) to ms. */
function seconds(value: number | undefined, fallback: number): number {
  const s = typeof value === "number" && value > 0 ? value : fallback;
  return Math.round(s * 1000);
}

/** A positive integer override, else the fallback. */
function positive(value: number | undefined, fallback: number): number {
  return typeof value === "number" && value > 0 ? Math.floor(value) : fallback;
}

/** Human-readable run-message text for a dropped skill, by reason code. Unknown
 *  codes degrade to a generic line rather than dropping the log. */
function describeSkillDrop(name: string, reason: string): string {
  switch (reason) {
    case "shadowed":
      return `skill "${name}" was shadowed by a higher-precedence skill of the same name and will not be loaded`;
    case "over_limit":
      return `skill "${name}" was dropped: the run exceeded the maximum number of skills`;
    case "too_large":
      return `skill "${name}" was dropped: its body exceeds the maximum allowed size`;
    case "repo_collision":
      return `repo skill "${name}" was skipped: a higher-precedence skill of the same name is already loaded`;
    case "repo_invalid":
      return `repo skill "${name}" was skipped: invalid name, description, or body`;
    default:
      return `skill "${name}" was dropped (${reason})`;
  }
}

/**
 * Resolve the model for the lead/main thread (PRD #17 Decision 6): the run
 * owner's per-user default (`config.default_model`) wins over the lead template's
 * model. A blank config model falls back to the template (|| not ??), so a
 * defensively empty string can't blank out the template's model — in practice
 * the server sends NULL (omitted), never "". Returns undefined when nothing
 * resolves, so the caller omits the SDK `model` key entirely rather than sending
 * an explicit empty override (an unset model falls back to the SDK/account
 * default). Null-model subagents follow the main thread, so this governs them too.
 */
export function resolveLeadModel(configModel?: string, templateModel?: string): string | undefined {
  return configModel || templateModel || undefined;
}
