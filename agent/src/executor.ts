import { execFile } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import type { AgentTemplate, ClaimConfig, MessageKind } from "./protocol.js";

const execFileAsync = promisify(execFile);

/** A message the executor emits to the run stream (seq assigned downstream). */
export interface EmittedMessage {
  kind: MessageKind;
  agent?: string;
  payload: Record<string, unknown>;
}

/**
 * Everything an executor needs to work one run.
 *
 * The fields below `emit` are consumed only by the SDK executor (M3); the M2
 * stub ignores them, so they are optional and the interface stays backward
 * compatible. The runner populates them from the claim.
 */
export interface RunContext {
  runId: string;
  issueIid: number;
  issueTitle: string;
  issueDescription: string;
  /** Checked-out worktree the executor edits and commits in (local only). */
  worktreePath: string;
  branch: string;
  /** Append a message to the run's live stream. */
  emit(msg: EmittedMessage): void;
  /** Anthropic subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN) for the SDK. */
  oauthToken?: string;
  /** PRD #3 templates (lead + subagents) mapped to SDK AgentDefinitions. */
  agents?: AgentTemplate[];
  /** Per-run caps (timeouts in SECONDS, iterations); converted at use sites. */
  config?: ClaimConfig | null;
  /** SDK session to resume; null/absent for a fresh run. */
  sessionId?: string | null;
  /** Called once with the SDK session id when first observed (for /state). */
  onSessionId?(sessionId: string): void;
  /** Aborts the SDK subprocess when signalled (cancel/shutdown; wired in M4). */
  signal?: AbortSignal;
}

export interface ExecutorResult {
  /** The branch to report as completed. M4 adds the pushed MR iid. */
  branch: string;
}

/**
 * The unit M3 replaces with the Claude Agent SDK. Keeping it behind this
 * interface is what lets M2 exercise the full state machine (claim → worktree →
 * work → report) with no AI in the loop.
 */
export interface Executor {
  run(ctx: RunContext): Promise<ExecutorResult>;
}

/**
 * M2 stand-in for the real agent: writes a marker file into the worktree and
 * makes a single local commit. No SDK, no network, no push (push + MR is M4).
 * The commit lands on `agent/issue-{iid}` in the shared bare object store, so a
 * test can assert the branch advanced exactly as a real run would.
 */
export class StubExecutor implements Executor {
  constructor(private readonly log: Logger) {}

  async run(ctx: RunContext): Promise<ExecutorResult> {
    ctx.emit({ kind: "status", agent: "worker", payload: { text: "stub executor starting" } });

    const markerPath = path.join(ctx.worktreePath, "UZI_RUN.md");
    const marker = [
      `# uzi stub run`,
      ``,
      `- run_id: ${ctx.runId}`,
      `- issue: #${ctx.issueIid} — ${ctx.issueTitle}`,
      `- branch: ${ctx.branch}`,
      ``,
      `This file is written by the M2 stub executor to prove the claim →`,
      `worktree → work → report loop end to end. M3 replaces it with the`,
      `Claude Agent SDK.`,
      ``,
    ].join("\n");
    await fs.writeFile(markerPath, marker, "utf8");

    // Local commit only. Identity is pinned per-invocation via -c so nothing is
    // written to the worktree's git config, and gpg signing is forced off so a
    // signing config on the host image can't block the commit.
    await this.git(ctx.worktreePath, ["add", "UZI_RUN.md"]);
    await this.git(ctx.worktreePath, [
      "-c", "user.name=uzi-agent",
      "-c", "user.email=uzi-agent@uzi.local",
      "-c", "commit.gpgsign=false",
      "commit", "-m", `uzi stub: work on issue #${ctx.issueIid}`,
    ]);

    ctx.emit({ kind: "text", agent: "worker", payload: { text: "stub work committed locally" } });
    this.log.info("stub executor committed marker", { run_id: ctx.runId, branch: ctx.branch });
    return { branch: ctx.branch };
  }

  private async git(cwd: string, args: string[]): Promise<void> {
    await execFileAsync("git", ["-C", cwd, ...args], {
      env: { ...process.env, GIT_TERMINAL_PROMPT: "0" },
      timeout: 60_000,
    });
  }
}
