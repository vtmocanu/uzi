// The Claude Agent SDK executor (PRD #4 M3). Replaces the M2 stub behind the
// same `Executor` interface, so the runner's claim → worktree → work → report
// state machine is unchanged; only the "work" step now drives a real agent.
//
// Everything that touches the network is behind an injectable `queryFn` (default
// = the SDK's `query`). Tests pass a fake that returns a controllable message
// stream, so the guardrails, sparse env, prompt discipline, session/resume, and
// watchdogs are all provable with NO live Anthropic session and NO real token
// (testing-credentials policy).

import fs from "node:fs/promises";
import { query as sdkQuery } from "@anthropic-ai/claude-agent-sdk";
import type { Options as SdkOptions, SDKMessage, SpawnOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";
import type { Executor, ExecutorResult, RunContext } from "./executor.js";
import type { Logger } from "./log.js";
import { buildSdkEnv } from "./sdk-env.js";
import { assembleAgents } from "./agents.js";
import { buildLeadPrompt, buildLeadSystemPrompt } from "./prompt.js";
import { buildPreToolUseHook, buildPathGuardHook } from "./guardrails.js";
import { killProcessGroup, spawnDetached } from "./sdk-spawn.js";
import { isErrorResult, isResult, mapSdkMessage, sessionIdOf } from "./sdk-messages.js";
import { errMessage } from "./util.js";

// Fallbacks used only when the claim omits `config`. Wire units are SECONDS
// (PRD §Configuration); converted to ms at the timer.
const DEFAULT_RUN_TIMEOUT_SECONDS = 2 * 60 * 60; // 2h
const DEFAULT_IDLE_TIMEOUT_SECONDS = 10 * 60; // 10m

/** Static (content-free) failure reasons — safe to persist as failure_reason. */
const REASON_WALL = "run exceeded its wall-clock timeout";
const REASON_IDLE = "run stalled: no agent activity within the idle timeout";
const REASON_CANCELLED = "run cancelled";
const REASON_NO_TOKEN = "no Anthropic OAuth token was provided for this run";

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

    const options: SdkOptions = {
      cwd: ctx.worktreePath,
      // Full replacement — only these keys reach the agent subprocess. The
      // strict SdkEnv interface has no index signature, so widen through
      // unknown to the SDK's index-signature env type.
      env: env as unknown as Record<string, string | undefined>,
      // Repo-borne prompt-injection defense: nothing from the cloned repo's
      // .claude/{settings.json,agents,hooks} can grant the agent permissions.
      settingSources: [],
      // Preset + append (bottega's shape) so Claude Code's own tool-use system
      // prompt is kept, not replaced by a bare string.
      systemPrompt: buildLeadSystemPrompt(assembled.leadSystemPrompt),
      // Programmatic subagents (each already disallows nested Agent spawning).
      agents: assembled.subagents,
      // Allow-by-default + explicit denies (see guardrails.ts for why not
      // `default`/`dontAsk`). bypassPermissions requires this ack flag.
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      // The load-bearing deny layer: even under bypassPermissions a PreToolUse
      // deny blocks the tool. The Bash hook screens commands; the path hook
      // screens the file tools so `Read /proc/<pid>/environ`, out-of-worktree
      // paths, and `.git/` access can't sidestep the Bash deny-list.
      hooks: {
        PreToolUse: [
          { matcher: "Bash", hooks: [buildPreToolUseHook(this.log)] },
          {
            matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep",
            hooks: [buildPathGuardHook(ctx.worktreePath, this.log)],
          },
        ],
      },
      // Persist discrete blocks only; partial token deltas would flood the seq
      // stream (the live-partial channel is M5, not M3).
      includePartialMessages: false,
    };
    if (assembled.leadModel) options.model = assembled.leadModel;
    // Resume the SDK session on a re-queued run; the batcher continues seq
    // numbering from the claim's last_seq (set by the runner), so no message is
    // dropped or renumbered across the restart.
    if (ctx.sessionId) options.resume = ctx.sessionId;

    const abortController = new AbortController();
    options.abortController = abortController;

    // Spawn the CLI in its own process group and capture the pid, so a watchdog
    // trip can group-kill the whole tree (the default SDK spawn is not detached,
    // so kill(-pid) would not otherwise reach a backgrounded bash). Only invoked
    // on a real run; the fake queryFn used in tests never calls it.
    const child: { pid?: number } = {};
    options.spawnClaudeCodeProcess = (spawnOpts: SpawnOptions): SpawnedProcess => {
      const proc = spawnDetached(spawnOpts);
      child.pid = proc.pid;
      return proc as unknown as SpawnedProcess;
    };

    const prompt = buildLeadPrompt({
      issueIid: ctx.issueIid,
      issueTitle: ctx.issueTitle,
      issueDescription: ctx.issueDescription,
      branch: ctx.branch,
      subagentNames,
    });

    return this.drive(ctx, options, prompt, abortController, child);
  }

  private async drive(
    ctx: RunContext,
    options: SdkOptions,
    prompt: string,
    abortController: AbortController,
    child: { pid?: number },
  ): Promise<ExecutorResult> {
    // A watchdog trip / external cancel records a static reason and aborts the
    // subprocess; the reason is thrown after the loop unwinds so the runner
    // reports `failed` with it.
    let tripReason: string | undefined;
    let idleTimer: NodeJS.Timeout | undefined;
    let wallTimer: NodeJS.Timeout | undefined;
    let queryInstance: AsyncIterable<SDKMessage> | undefined;
    let reportedSessionId = false;

    const idleMs = seconds(ctx.config?.idle_timeout_seconds, DEFAULT_IDLE_TIMEOUT_SECONDS);
    const wallMs = seconds(ctx.config?.run_timeout_seconds, DEFAULT_RUN_TIMEOUT_SECONDS);

    const trip = (reason: string): void => {
      if (tripReason) return; // first trip wins
      tripReason = reason;
      this.log.warn("run watchdog/cancel tripped, aborting SDK", { run_id: ctx.runId, reason });
      // abort() is the primary, asserted stop (SDK: stdin EOF → grace → signal);
      // the group kill is best-effort defense for orphaned children.
      abortController.abort();
      killProcessGroup(child.pid);
    };

    const armIdle = (): void => {
      if (idleTimer) clearTimeout(idleTimer);
      idleTimer = setTimeout(() => trip(REASON_IDLE), idleMs);
      idleTimer.unref?.();
    };

    // External cancel (M4 wires this from a user `cancel` input / shutdown).
    const onSignal = (): void => trip(REASON_CANCELLED);
    if (ctx.signal) {
      if (ctx.signal.aborted) onSignal();
      else ctx.signal.addEventListener("abort", onSignal, { once: true });
    }

    ctx.emit({ kind: "status", agent: "worker", payload: { text: "starting SDK agent" } });

    try {
      wallTimer = setTimeout(() => trip(REASON_WALL), wallMs);
      wallTimer.unref?.();
      armIdle();

      queryInstance = this.queryFn({ prompt: promptStream(prompt), options });

      let sawErrorResult = false;
      let errorSubtype = "unknown";
      for await (const msg of queryInstance) {
        armIdle(); // any message is liveness

        const sid = sessionIdOf(msg);
        if (sid && !reportedSessionId) {
          reportedSessionId = true;
          // Surface the session id so the runner can persist it via /state,
          // making a future resume possible (PRD §Session persistence).
          try {
            ctx.onSessionId?.(sid);
          } catch (err) {
            this.log.warn("onSessionId handler threw", { run_id: ctx.runId, error: errMessage(err) });
          }
        }

        for (const em of mapSdkMessage(msg)) ctx.emit(em);

        if (isResult(msg)) {
          if (isErrorResult(msg)) {
            sawErrorResult = true;
            errorSubtype = (msg as { subtype?: unknown }).subtype as string ?? "unknown";
          }
          // The turn is done. Abort so a lingering background bash the agent
          // left running can't pin the iterator open (bottega's pattern).
          abortController.abort();
          break;
        }
      }

      if (tripReason) throw new Error(tripReason);
      if (sawErrorResult) throw new Error(`agent run failed: ${errorSubtype}`);

      this.log.info("SDK run completed", { run_id: ctx.runId, branch: ctx.branch });
      return { branch: ctx.branch };
    } catch (err) {
      // A watchdog/cancel trip surfaces as its static reason, not the raw
      // AbortError the aborted iterator throws.
      if (tripReason) throw new Error(tripReason);
      throw err instanceof Error ? err : new Error(errMessage(err));
    } finally {
      if (idleTimer) clearTimeout(idleTimer);
      if (wallTimer) clearTimeout(wallTimer);
      if (ctx.signal) ctx.signal.removeEventListener("abort", onSignal);
    }
  }
}

/** One-shot prompt stream: the SDK consumes the lead's first user turn. */
async function* promptStream(text: string): AsyncGenerator<unknown> {
  yield { type: "user", message: { role: "user", content: text }, parent_tool_use_id: null };
}

/** Convert an optional seconds value (any number tolerated) to ms. */
function seconds(value: number | undefined, fallback: number): number {
  const s = typeof value === "number" && value > 0 ? value : fallback;
  return Math.round(s * 1000);
}
