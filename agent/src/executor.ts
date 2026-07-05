import { execFile } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import type { AgentTemplate, ClaimConfig, ClaimSkill, ClaimSkillDrop, MessageKind } from "./protocol.js";
import type { PlanVerdict } from "./steering.js";
import { prepareSkillPlugin, resolveSkillCaps } from "./skills-run.js";

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
  /** M4 (PRD #16): the per-run skill union — materialized into a local plugin dir
   *  and enabled via the SDK `skills` list. */
  skills?: ClaimSkill[];
  /** M4 (PRD #16): skills the server dropped at claim assembly (shadowed /
   *  over-limit). The worker owns the gapless seq, so it logs these. */
  skillsDropped?: ClaimSkillDrop[];
  /** M6 (PRD #16): the repo owner opted in to loading skills from the clone's own
   *  .claude/skills. Only then does the worker enumerate them (skills only, lowest
   *  precedence); default off. */
  repoSkillsEnabled?: boolean;
  /** Per-run caps (timeouts in SECONDS, iterations); converted at use sites. */
  config?: ClaimConfig | null;
  /** SDK session to resume; null/absent for a fresh run. */
  sessionId?: string | null;
  /** Called once with the SDK session id when first observed (for /state). */
  onSessionId?(sessionId: string): void;
  /** Aborts the SDK subprocess when signalled (cancel/shutdown; wired in M4). */
  signal?: AbortSignal;
  /**
   * M4 plan gate. Called by the executor after the lead submits a plan: the
   * runner posts /state awaiting_approval with the plan and returns the user's
   * verdict (approve/reject/cancel), polled from /inputs. Absent in M2/M3.
   */
  gatePlan?(planMd: string): Promise<PlanVerdict>;
  /** M4: dequeue the next queued follow-up to inject into the next loop turn. */
  pullFollowUp?(): string | undefined;
  /** M4: report a running/iteration heartbeat (server persists via GREATEST). */
  reportIteration?(iteration: number): void;
}

export interface ExecutorResult {
  /** The branch to report as completed. The runner pushes it + opens the MR. */
  branch: string;
}

/**
 * Thrown by the executor when the human rejects the plan at the gate. The runner
 * catches it and reports `failed` with the (user-supplied) reason verbatim,
 * rather than treating it as an internal crash. The reason is human/user text,
 * never a secret.
 */
export class PlanRejectedError extends Error {
  constructor(readonly reason: string) {
    super(reason);
    this.name = "PlanRejectedError";
  }
}

/**
 * The unit M3 replaces with the Claude Agent SDK. Keeping it behind this
 * interface is what lets M2 exercise the full state machine (claim → worktree →
 * work → report) with no AI in the loop.
 */
export interface Executor {
  run(ctx: RunContext): Promise<ExecutorResult>;
  /**
   * Group-kill every subprocess the agent spawned for the last run. The runner
   * calls this BEFORE any PAT-bearing git op (push) so an agent-backgrounded
   * process cannot survive to read the PAT out of the git child's /proc/environ
   * (M4 audit B1). Optional so the M2 stub and test executors need not implement
   * it; the SDK executor also self-reaps in its own run() finally.
   */
  killAgentTree?(): void;
}

/** Options for the stub executor. */
export interface StubExecutorOptions {
  /**
   * Drive the M4 plan-approval gate before committing (emit a plan → the runner
   * posts awaiting_approval → await the human verdict). Off by default so the
   * bare M2 stub goes straight to a local commit; the E2E harness turns it on
   * (UZI_STUB_PLAN_GATE) to exercise the full workflow with no live SDK.
   */
  planGate?: boolean;
}

/**
 * M2 stand-in for the real agent: writes a marker file into the worktree and
 * makes a single local commit. No SDK, no network, no push (push + MR is M4).
 * The commit lands on `agent/issue-{iid}` in the shared bare object store, so a
 * test can assert the branch advanced exactly as a real run would.
 *
 * With `planGate` on it also drives the M4 plan gate: it submits a static plan,
 * calls ctx.gatePlan (parking the run at awaiting_approval), and honours the
 * verdict — approve continues to the commit, reject/cancel abort — so the whole
 * plan→approve→work→MR path is provable end-to-end without a live Anthropic
 * session (mirrors SdkExecutor's gate handling).
 */
export class StubExecutor implements Executor {
  constructor(
    private readonly log: Logger,
    private readonly opts: StubExecutorOptions = {},
  ) {}

  async run(ctx: RunContext): Promise<ExecutorResult> {
    ctx.emit({ kind: "status", agent: "worker", payload: { text: "stub executor starting" } });

    // Synthesize the skills plugin the SAME way the SDK executor does (shared
    // prepareSkillPlugin), then read it back and report exactly what landed on
    // disk. This makes skill delivery + the repo-skill opt-in observable in the M6
    // E2E, which runs the stub (no live SDK session). Dropped skills are logged
    // too. Non-fatal: a skills failure must never fail the stub's core commit path.
    try {
      const prepared = await prepareSkillPlugin(ctx, resolveSkillCaps(ctx.config));
      const loaded = await readPluginSkillNames(prepared.pluginPath);
      ctx.emit({ kind: "status", agent: "worker", payload: { text: `plugin skills: ${loaded.join(", ") || "(none)"}`, plugin_skills: loaded } });
      const drops = [...(ctx.skillsDropped ?? []), ...prepared.drops];
      for (const d of drops) {
        ctx.emit({ kind: "status", agent: "worker", payload: { text: `skill dropped: ${d.name} (${d.reason})`, dropped_skill: d.name, dropped_reason: d.reason } });
      }
    } catch (err) {
      this.log.warn("stub: skills plugin synthesis failed", { run_id: ctx.runId, error: String(err) });
    }

    if (this.opts.planGate && ctx.gatePlan) {
      const planMd = [
        `## Stub plan for issue #${ctx.issueIid}`,
        ``,
        `- write a marker file documenting the run`,
        `- commit it on \`${ctx.branch}\``,
        ``,
        `Generated by the M6 stub executor to drive the plan-approval gate with`,
        `no live Anthropic session.`,
      ].join("\n");
      const verdict = await ctx.gatePlan(planMd);
      if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
      if (verdict.kind === "cancel") throw new Error("run cancelled");
      ctx.reportIteration?.(1);
      ctx.emit({ kind: "status", agent: "worker", payload: { text: "plan approved; implementing" } });
    }

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

/** Read back the skill directory names actually materialized under a plugin dir
 *  (`<pluginPath>/skills/<name>/`), sorted. This reflects what is ON DISK, so the
 *  E2E asserting on it proves the plugin was truly synthesized (not just that the
 *  claim carried the names). Missing dir ⇒ no skills. */
async function readPluginSkillNames(pluginPath: string): Promise<string[]> {
  try {
    const entries = await fs.readdir(path.join(pluginPath, "skills"), { withFileTypes: true });
    return entries.filter((e) => e.isDirectory()).map((e) => e.name).sort();
  } catch {
    return [];
  }
}
