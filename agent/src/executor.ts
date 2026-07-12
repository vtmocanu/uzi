import { execFile } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import type { AgentSource, AgentTemplate, ClaimConfig, ClaimPipeline, ClaimSkill, ClaimSkillDrop, FixVerdict, MessageKind, RunKind } from "./protocol.js";
import type { PlanVerdict } from "./steering.js";
import { prepareSkillPlugin, resolveSkillCaps } from "./skills-run.js";
import { provisionRunTools } from "./provision-run.js";
import type { provisionTools } from "./provision.js";

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
  /** Run kind (PRD #6). "issue" (default when absent) works issueIid's card;
   *  "ci_fix" diagnoses + fixes `pipeline`. */
  kind?: RunKind;
  /** The worked issue for an issue run; null for a ci_fix run (no issue). */
  issueIid: number | null;
  issueTitle: string;
  issueDescription: string;
  /** The failed-pipeline snapshot for a ci_fix run (PRD #6): what the agent
   *  diagnoses + fixes. Present only for kind="ci_fix". Log tails are UNTRUSTED. */
  pipeline?: ClaimPipeline | null;
  /** Checked-out worktree the executor edits and commits in (local only). */
  worktreePath: string;
  branch: string;
  /** Append a message to the run's live stream. */
  emit(msg: EmittedMessage): void;
  /** Anthropic subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN) for the SDK. */
  oauthToken?: string;
  /** PRD #3 templates (lead + subagents) mapped to SDK AgentDefinitions. */
  agents?: AgentTemplate[];
  /** PRD #37: the roster the worker parsed out of the clone's `.claude/agents/`,
   *  with prompt bodies. Detected before the first turn but INERT until a selection
   *  activates it at the gate (M3); the lead always comes from `agents` above, so a
   *  repo file named `lead` is only ever a subagent candidate. Empty when the repo
   *  ships none. */
  repoAgents?: AgentTemplate[];
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
  /** ci_fix only (PRD #6): "not_code" when the agent judged the failure NOT a code
   *  problem (infra/flaky/secret/runner). The runner then completes the run with the
   *  diagnosis and NO push/MR — approving a no-op costs nothing and the diagnosis is
   *  the value. Absent ⇒ a real fix was committed and must be pushed. */
  fixVerdict?: FixVerdict;
  /** PRD #37: the resolved subagent roster the IMPLEMENT phase actually ran with —
   *  the source and the surviving agent names. The runner uses it for the MR
   *  description marker (a `repo`-source run states the internal review was repo-
   *  authored, Decision 3b). Absent for a stub/ci_fix-not_code run that assembled
   *  no roster. */
  agentSelection?: { source: AgentSource; agents: string[] };
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

/**
 * Sentinel in an issue's title/description that makes the stub executor throw
 * after the plan gate, standing in for an agent that fails mid-implementation.
 * The E2E harness uses it to drive the autopilot failure path (a failed run →
 * exactly one terminal failure comment) with no live SDK. Off unless present.
 */
export const STUB_FAIL_SENTINEL = "UZI_STUB_FAIL";

/**
 * Sentinel in a ci_fix run's title/description that makes the stub executor return
 * a not_code verdict after the plan gate (PRD #6), standing in for an agent that
 * judged the failure not a code problem. Drives the E2E not_code path with no live
 * SDK. Off unless present.
 */
export const STUB_NOT_CODE_SENTINEL = "UZI_STUB_NOT_CODE";

/**
 * Sentinel that makes the stub emit a scripted INTERLEAVED multi-agent message
 * stream during the implement phase (PRD #43 M5). A real parallel-subagent run
 * produces messages from several agents woven together; the SDK is the only thing
 * that emits them, so the isolated E2E stack (stub, no live SDK) needs the stub to
 * stand in. The scripted stream lets the E2E prove the persistence/replay contract
 * — gapless per-run seq, strict order, and per-agent attribution surviving REST
 * `?after=<seq>` replay — with no Anthropic session. Off unless present.
 */
export const STUB_INTERLEAVE_SENTINEL = "UZI_STUB_INTERLEAVE";

/**
 * The scripted stream STUB_INTERLEAVE_SENTINEL emits, in emit order. Each frame
 * carries a 1-based `step` in its payload so a consumer can pin exact order and
 * attribution independently of the run's other (worker) messages. The agents
 * alternate lead → coder → reviewer, and each name recurs NON-ADJACENTLY (a second
 * `coder`, then `reviewer`, then `lead`): that is exactly the interleaving a
 * parallel run yields, and it is what makes name-based attribution non-trivial to
 * preserve across persistence and reconnect replay.
 */
export const STUB_INTERLEAVE_STREAM: ReadonlyArray<{ agent: string; text: string }> = [
  { agent: "lead", text: "parallel dispatch: units A (api) and B (web)" },
  { agent: "coder", text: "unit A: editing the api scope" },
  { agent: "reviewer", text: "review wave: auditing unit A" },
  { agent: "coder", text: "unit B: editing the web scope" },
  { agent: "reviewer", text: "review wave: auditing unit B" },
  { agent: "lead", text: "integration: scopes disjoint, committing once" },
];

// PRD #40 M6: the stub stands in for the live SDK's usage-bearing frames. It emits
// a per-agent (coder) assistant message carrying PER-CALL usage (Decision 11) and a
// terminal `result` frame carrying CUMULATIVE usage + per-model breakdown (folded
// into run_usage by the API). The live SDK path emits both; without them the E2E
// could not prove usage lands on the run, aggregates into /api/usage, or surfaces a
// per-agent row. Numbers are arbitrary but FIXED so the E2E asserts them exactly.
export const STUB_RESULT_USAGE = {
  input_tokens: 21400,
  cache_read_input_tokens: 188000,
  cache_creation_input_tokens: 0,
  output_tokens: 6100,
};
export const STUB_RESULT_MODEL_USAGE = {
  "claude-fable-5": { inputTokens: 21400, outputTokens: 6100, cacheReadInputTokens: 188000, cacheCreationInputTokens: 0, costUSD: 0.24 },
};
export const STUB_RESULT_COST_USD = 0.24;
export const STUB_CODER_USAGE = {
  input_tokens: 12000,
  cache_read_input_tokens: 90000,
  cache_creation_input_tokens: 0,
  output_tokens: 3000,
};

/** Options for the stub executor. */
export interface StubExecutorOptions {
  /**
   * Drive the M4 plan-approval gate before committing (emit a plan → the runner
   * posts awaiting_approval → await the human verdict). Off by default so the
   * bare M2 stub goes straight to a local commit; the E2E harness turns it on
   * (UZI_STUB_PLAN_GATE) to exercise the full workflow with no live SDK.
   */
  planGate?: boolean;
  /**
   * When set, the stub exercises the SAME tool-provisioning path as the SDK
   * executor (PRD #18 M8): pinned HOME for the install subprocess. Omitted ⇒ no
   * provisioning (unit tests). The E2E sets it and stubs the `devbox` binary, so a
   * provisioned-tool run is observable with no live SDK and no substituter egress.
   */
  homeDir?: string;
  /** Injected in tests so no real devbox/nix egress happens; default = provisionTools. */
  provision?: typeof provisionTools;
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

    // Delivered agent templates (PRD #18 M7/M8): report the claim's template set so
    // a user-scoped template's delivery — after the server's allocation +
    // shared-precedence resolution — is observable in the E2E (the stub runs no lead).
    const agentNames = (ctx.agents ?? []).map((a) => a.name);
    ctx.emit({ kind: "status", agent: "worker", payload: { text: `agents: ${agentNames.join(", ") || "(none)"}`, agents: agentNames } });

    // Tool provisioning (PRD #18 M8): exercise the SAME path as the SDK executor
    // (provision-run.ts), against the E2E's stubbed devbox. No packages ⇒ a no-op;
    // a provision failure fails the run, matching the SDK executor. The provisioned
    // env is unused by the stub — the point is to prove the install path end to end.
    if (this.opts.homeDir) {
      const provisionRoot = path.join(path.dirname(this.opts.homeDir), "provision");
      const { provisionDir } = await provisionRunTools(ctx, {
        provisionRoot,
        homeDir: this.opts.homeDir,
        log: this.log,
        provision: this.opts.provision,
      });
      if (provisionDir) await fs.rm(provisionDir, { recursive: true, force: true }).catch(() => undefined);
    }

    const isCIFix = ctx.kind === "ci_fix";
    const label = isCIFix ? `CI fix on \`${ctx.branch}\`` : `issue #${ctx.issueIid}`;
    // A ci_fix run can conclude that the failure is not a code problem (PRD #6):
    // the E2E drives that path via the sentinel.
    const notCode =
      isCIFix &&
      (ctx.issueDescription.includes(STUB_NOT_CODE_SENTINEL) ||
        ctx.issueTitle.includes(STUB_NOT_CODE_SENTINEL) ||
        (ctx.pipeline?.failed_jobs ?? []).some((j) => j.log_tail.includes(STUB_NOT_CODE_SENTINEL)));

    if (this.opts.planGate && ctx.gatePlan) {
      const planMd = [
        isCIFix ? `## Stub CI-fix diagnosis for \`${ctx.branch}\`` : `## Stub plan for issue #${ctx.issueIid}`,
        ``,
        ...(notCode
          ? [`VERDICT: not_code`, ``, `The failure is an infra/flaky problem, not a code bug (stub diagnosis).`]
          : [`- write a marker file documenting the run`, `- commit it on \`${ctx.branch}\``]),
        ``,
        `Generated by the stub executor to drive the plan-approval gate with`,
        `no live Anthropic session.`,
      ].join("\n");
      const verdict = await ctx.gatePlan(planMd);
      if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
      if (verdict.kind === "cancel") throw new Error("run cancelled");
      if (notCode) {
        ctx.emit({ kind: "status", agent: "worker", payload: { text: "stub diagnosis: not a code problem" } });
        return { branch: ctx.branch, fixVerdict: "not_code" };
      }
      ctx.reportIteration?.(1);
      ctx.emit({ kind: "status", agent: "worker", payload: { text: "plan approved; implementing" } });
    }

    // E2E failure hook: throw AFTER the (auto-)approved plan so the run fails
    // during "implementation", exercising the worker-terminated failure path.
    if (ctx.issueDescription.includes(STUB_FAIL_SENTINEL) || ctx.issueTitle.includes(STUB_FAIL_SENTINEL)) {
      throw new Error(`stub executor: forced failure (${STUB_FAIL_SENTINEL} sentinel present)`);
    }

    // PRD #43 M5: emit a scripted interleaved multi-agent stream so the E2E can
    // assert that persistence + REST replay keep it gapless, ordered, and
    // per-agent attributed — the piece the live SDK would otherwise produce. Emits
    // are synchronous and ordered, so the downstream batcher assigns them a gapless
    // seq run; the trailing "committed" message keeps them mid-stream, not last.
    if (ctx.issueDescription.includes(STUB_INTERLEAVE_SENTINEL) || ctx.issueTitle.includes(STUB_INTERLEAVE_SENTINEL)) {
      STUB_INTERLEAVE_STREAM.forEach((frame, i) => {
        ctx.emit({ kind: "text", agent: frame.agent, payload: { text: frame.text, step: i + 1 } });
      });
    }

    const markerPath = path.join(ctx.worktreePath, "UZI_RUN.md");
    const marker = [
      `# uzi stub run`,
      ``,
      `- run_id: ${ctx.runId}`,
      `- ${label} — ${ctx.issueTitle}`,
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
      "commit", "-m", `uzi stub: work on ${label}`,
    ]);

    ctx.emit({ kind: "text", agent: "worker", payload: { text: "stub work committed locally" } });

    // PRD #40 M6: stand in for the live SDK's usage frames — a per-agent (coder)
    // message with per-call usage (Decision 11) and the terminal result frame with
    // cumulative usage + modelUsage (the API folds it into run_usage). Emitted on
    // every stub run, so a normal E2E run carries usage without a sentinel.
    ctx.emit({ kind: "text", agent: "coder", payload: { text: "coder: implemented the change", usage: STUB_CODER_USAGE } });
    ctx.emit({
      kind: "status",
      agent: "lead",
      payload: {
        event: "result",
        subtype: "success",
        num_turns: 9,
        duration_ms: 100000,
        total_cost_usd: STUB_RESULT_COST_USD,
        usage: STUB_RESULT_USAGE,
        modelUsage: STUB_RESULT_MODEL_USAGE,
      },
    });

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
