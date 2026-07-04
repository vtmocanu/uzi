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
import { buildPreToolUseHook, buildPathGuardHook, buildAgentGuardHook, NESTED_AGENT_TOOL } from "./guardrails.js";
import { buildSignalMcpServer, isSignalToolName, scanSignals, SIGNAL_SERVER_NAME } from "./signals.js";
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
  }

  async run(ctx: RunContext): Promise<ExecutorResult> {
    const oauthToken = ctx.oauthToken?.trim();
    if (!oauthToken) {
      // OAuth is the sole supported credential (no API keys). Detect its
      // absence up front and fail fast rather than spawning a doomed CLI.
      throw new Error(REASON_NO_TOKEN);
    }

    await fs.mkdir(this.homeDir, { recursive: true });

    const env = buildSdkEnv(oauthToken, this.homeDir);
    const assembled = assembleAgents(ctx.agents ?? []);
    const subagentNames = Object.keys(assembled.subagents);
    const maxIterations = positive(ctx.config?.max_iterations, DEFAULT_MAX_ITERATIONS);

    const baseOptions: SdkOptions = {
      cwd: ctx.worktreePath,
      // Full replacement — only these keys reach the agent subprocess.
      env: env as unknown as Record<string, string | undefined>,
      // Repo-borne prompt-injection defense: nothing from the cloned repo's
      // .claude/{settings.json,agents,hooks} can grant the agent permissions.
      settingSources: [],
      systemPrompt: buildLeadSystemPrompt(assembled.leadSystemPrompt),
      agents: assembled.subagents,
      // In-process signalling tools the lead calls to gate the plan and mark done
      // (see signals.ts). Only the lead (full toolset) can reach them.
      mcpServers: { [SIGNAL_SERVER_NAME]: buildSignalMcpServer() },
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      // The load-bearing deny layer: a PreToolUse deny blocks a tool even under
      // bypassPermissions. Bash screening, the file-tool path jail, AND the M4
      // hard-fail-on-unexpected-subagent guard (item 7) all live here.
      hooks: {
        PreToolUse: [
          { matcher: "Bash", hooks: [buildPreToolUseHook(this.log)] },
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
    if (assembled.leadModel) baseOptions.model = assembled.leadModel;

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
      const proc = spawnDetached(spawnOpts);
      state.currentChild.pid = proc.pid;
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
    killProcessGroup(state.currentChild.pid);
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
