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
import path from "node:path";
import { query as sdkQuery } from "@anthropic-ai/claude-agent-sdk";
import type {
  Options as SdkOptions,
  SDKMessage,
  SpawnOptions,
  SpawnedProcess,
} from "@anthropic-ai/claude-agent-sdk";
import type { Executor, ExecutorResult, RunContext } from "./executor.js";
import type { Logger } from "./log.js";
import { buildCheckEnv, buildSdkEnv } from "./sdk-env.js";
import type { DockerWiring } from "./docker-wiring.js";
import { provisionTools } from "./provision.js";
import { provisionRunTools } from "./provision-run.js";
import {
  installJsDeps,
  DETAIL_NO_LOCKFILE,
  type JsDepsInstall,
  type JsDepsResult,
} from "./js-deps.js";
import {
  applySubagentModelOverride,
  assembleAgents,
  planTurnSubagents,
  selectSubagents,
  subagentWriteCapabilities,
} from "./agents.js";
import {
  resolveAgentSelection,
  type AgentSelectionParse,
  type AskUserQuestion,
  type ClaimConfig,
  type Milestone,
  type MilestoneProgress,
} from "./protocol.js";
import {
  buildCIFixPlanPrompt,
  buildImplementPrompt,
  buildLeadSystemPrompt,
  buildPlanPrompt,
  buildRepoInstructionsContext,
  buildRevisePlanPrompt,
  buildSelfImprovePlanPrompt,
  isNotCodePlan,
} from "./prompt.js";
import { readRepoInstructions } from "./repo-instructions.js";
import {
  buildPreToolUseHook,
  buildPathGuardHook,
  buildAgentGuardHook,
  buildSendMessageAliasHook,
  NESTED_AGENT_TOOL,
  SEND_MESSAGE_TOOL,
  ASYNC_DEFERRAL_TOOLS,
} from "./guardrails.js";
import {
  buildSignalMcpServer,
  isSignalToolName,
  scanSignals,
  SIGNAL_SERVER_NAME,
} from "./signals.js";
import {
  classifyLimitFailure,
  LimitReachedError,
  RateLimitObserver,
} from "./limit.js";
import { buildMemoryServer, MEMORY_SERVER_NAME } from "./memory-tools.js";
import { buildForgeToolsServer, FORGE_SERVER_NAME } from "./forge-tools.js";
import { buildFindingsToolsServer, FINDINGS_SERVER_NAME } from "./findings-tools.js";
import type { WorkerClient } from "./client.js";
import type { McpSdkServerConfigWithInstance } from "@anthropic-ai/claude-agent-sdk";
import { qualifiedSkillName, type SkillDrop } from "./skills-plugin.js";
import { prepareSkillPlugin, resolveSkillCaps } from "./skills-run.js";
import { killProcessGroup, spawnDetached } from "./sdk-spawn.js";
import {
  assistantModelOf,
  assistantUsageOf,
  isErrorResult,
  isResult,
  mapSdkMessage,
  orphanInstanceKind,
  sessionIdOf,
} from "./sdk-messages.js";
import { PlanRejectedError } from "./executor.js";
import { clampToDirCharset, errMessage } from "./util.js";
import { SummaryRunner } from "./summary-runner.js";
import { resolvePrdInput, type PrdInput } from "./prd-link.js";

// Fallbacks used only when the claim omits `config`. Wire units are SECONDS
// (PRD §Configuration); converted to ms at the timer.
// PRD #122 M2: this and DEFAULT_MAX_ITERATIONS are the SINGLE-milestone / global
// defaults. On a milestone-structured run the server scales the effective budget by
// milestone count (Decisions 5/5b) and serves the scaled numbers — on the claim config
// for a resume, on the per-iteration state-report ACK for a fresh run — which the loop
// applies over these. A run with no milestones keeps exactly these values.
const DEFAULT_RUN_TIMEOUT_SECONDS = 2 * 60 * 60; // 2h
const DEFAULT_IDLE_TIMEOUT_SECONDS = 10 * 60; // 10m
const DEFAULT_MAX_ITERATIONS = 5; // PRD: RUN_MAX_ITERATIONS default 5 (single-milestone)
// PRD #41: the plan-revision cap (PLAN_MAX_REVISIONS, default 3). The server also
// enforces it at submit time (rejects the 4th revise), so the worker counter is a
// belt-and-suspenders guard that rarely trips.
const DEFAULT_MAX_REVISIONS = 3;
// PRD #72 M4: transport clamp on a declared PRD path. Mirrors
// `prdpath.MaxPathLen` (api/internal/prdpath), which is the AUTHORITY — this
// copy only keeps an absurd string off the wire. Keep the two in step.
const PRD_DONE_PATH_MAX_LEN = 512;

// Static (content-free) failure reasons — safe to persist as failure_reason.
const REASON_WALL = "run exceeded its wall-clock timeout";
const REASON_IDLE = "run stalled: no agent activity within the idle timeout";
const REASON_CANCELLED = "run cancelled";
// Exported so the runner's failed-report site can map it to a fail_origin
// (PRD #69 M7a): a missing Anthropic token is a credential_unavailable failure.
export const REASON_NO_TOKEN = "no Anthropic OAuth token was provided for this run";
const REASON_NO_PLAN =
  "the agent ended the planning turn without submitting a plan";
/** PRD #88 M4: the lead kept asking instead of planning. Distinct from
 *  REASON_NO_PLAN so an operator reading a failed run can tell "it never produced a
 *  plan" from "it produced only questions" — different problems with different fixes. */
const REASON_NO_PLAN_AFTER_CLARIFICATION =
  "the agent kept asking clarifying questions without ever submitting a plan";
const REASON_MAX_ITERATIONS =
  "run reached its milestone-scaled implement/review iteration budget without completing";

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
  /** Root for per-run tool-provisioning dirs (PRD #18 M3), OUTSIDE any clone.
   *  Default: a `provision/` sibling of the provisioning HOME on the data volume. */
  provisionRoot?: string;
  /**
   * SHARED (worker-lifetime) HOME for the tool-provisioning subprocess — the nix
   * single-user profile + devbox state that warm-start relies on (PRD #18; PRD #42
   * Decision 5). Deliberately distinct from the per-run SDK `homeDir`: a per-run
   * provisioning HOME would give every run a cold nix profile and fragment
   * warm-start state, while buying nothing (the per-run provision DIR under
   * `provisionRoot/<runId>` already isolates the synthesized devbox.json, and the
   * nix store is global). Defaults to `homeDir` for callers (tests) that pass one
   * HOME and don't provision; main.ts passes the shared root explicitly.
   */
  provisionHomeDir?: string;
  /** Devbox provisioning fn (PRD #18 M3). Injected in tests so no real nix egress
   *  happens; default = provisionTools. */
  provision?: typeof provisionTools;
  /** The worker's resolved docker wiring (PRD #83 M1 keystone), computed ONCE at
   *  startup (docker-wiring.ts) and the same for every run. Absent/`{}` ⇒ no daemon:
   *  the Bash guardrail denies docker and no DOCKER_HOST reaches the SDK env. Present
   *  ⇒ docker is allowed and DOCKER_HOST is injected. */
  dockerWiring?: DockerWiring;
  /** The worker→API client (PRD #90): threaded so the lead's `save_memory` MCP tool
   *  can POST a cross-run learning. Absent ⇒ no memory server is registered (tests/
   *  stubs that never call it), so the tool wiring is additive and back-compatible. */
  client?: WorkerClient;
  /** Install the cloned repo's JS dependencies (PRD #121 M2); default = installJsDeps.
   *  Injected in tests so no package manager is ever spawned and the kick-off/join
   *  ordering is drivable. */
  installDeps?: typeof installJsDeps;
  /** The inline run-summary generator (PRD #362 M3c); default = a real SummaryRunner
   *  rooted at the SHARED provisioning HOME (a writable data-volume path, never the
   *  clone). Injected in tests so the advisory hooks are drivable with a fake that
   *  records calls and can throw, proving failure isolation with no live SDK. */
  summaryRunner?: SummaryRunner;
}

/** What one turn observed: the session id, and any workflow signals. */
interface TurnResult {
  sessionId?: string;
  plan?: string;
  done: boolean;
  /** PRD #72 M4: the PRD path the lead declared on signal_done, if any. */
  prdDonePath?: string;
  /** PRD #265 M1: the frozen-milestone ids the lead declared FINISHED on signal_done,
   *  if any. Carried into the terminal `completed` report and unioned server-side. */
  milestonesCompleted?: string[];
  /** PRD #88: questions the lead asked via ask_user during this turn. Present and
   *  non-empty ⇒ the caller parks the run OUTSIDE driveTurn. */
  questions?: AskUserQuestion[];
  /** PRD #122 M1: the candidate milestone list the lead passed to submit_plan this
   *  turn, if any. Rides the gate call so the human approves the breakdown. */
  milestones?: Milestone[];
  /** PRD #122 M2: the latest milestone progress the lead reported via report_progress
   *  during this turn (last-wins within the turn). The loop carries it into the NEXT
   *  iteration's `running` report. */
  progress?: MilestoneProgress;
  /** PRD #122 M6: true when the lead called the turn-ending `checkpoint` tool this turn.
   *  Latched (last-wins/latch within the turn, like `done`): once seen it stays set, and
   *  the loop reaps + fetches the committed milestone work back durably before continuing. */
  checkpoint?: boolean;
  /** issue #279: the `summary` the lead passed to signal_done this turn, if any. Last-wins
   *  within the turn (like prdDonePath); persisted as report_md on a report-only completion. */
  summary?: string;
  /** issue #279: true when the lead declared `report_only: true` on signal_done this turn.
   *  Latched (like `done`): the run completes with NO push/MR. Issue runs only. */
  reportOnly?: boolean;
}

/** PRD #88 feed notices for a question that could NOT be put to a human. Both are
 *  "the run continues on a guess", which is pre-#88 behaviour — worth saying on the
 *  feed precisely because the lead had judged a guess costly enough to ask about. */
const QUESTION_UNWIRED_NOTICE =
  "The agent asked a clarifying question, but this run has no way to reach a human — it will proceed on its best judgment.";
const QUESTION_CAP_NOTICE =
  "The agent has already asked its maximum number of clarifying questions for this run — it will proceed on its best judgment.";

/** Default question cap when the claim carries none (an older server, R8). Mirrors
 *  the server's QUESTION_MAX default; the claim value wins when present. */
const DEFAULT_QUESTION_MAX = 5;

function questionMax(config: ClaimConfig | null): number {
  const n = config?.question_max;
  return typeof n === "number" && n > 0 ? n : DEFAULT_QUESTION_MAX;
}

/** Render the questions + answers back into the resumed session as a user turn. The
 *  answers are USER text and the questions are the lead's own, so neither is framed
 *  as an instruction beyond what it is. */
function answeredPrompt(
  questions: AskUserQuestion[],
  answers: string[],
): string {
  const lines = questions.map((q, i) => {
    const a = answers[i]?.trim();
    return `Q: ${q.question}\nA: ${a && a.length > 0 ? a : "(no answer given)"}`;
  });
  return `The human answered your question${questions.length > 1 ? "s" : ""}:\n\n${lines.join("\n\n")}\n\nContinue the work with these answers.`;
}

/** Wrap an answer (or a could-not-ask notice) for a PLANNING turn. The implement-loop
 *  wording says "continue the work", which is wrong before a plan exists — here the
 *  required next step is the plan itself, and saying so is what stops the lead from
 *  treating the answer as permission to start implementing. */
function buildPlanAfterAnswerPrompt(answerBlock: string): string {
  return `${answerBlock}\n\nNow produce the implementation plan and submit it with submit_plan. Do not begin implementing.`;
}

/** Told to the lead when its question could not be delivered. It must not simply
 *  re-ask: nothing about the next turn makes a human any more reachable. */
function unansweredPrompt(questions: AskUserQuestion[]): string {
  const lines = questions.map((q) => `- ${q.question}`).join("\n");
  return `Your question${questions.length > 1 ? "s" : ""} could not be put to a human on this run:\n\n${lines}\n\nProceed on your best judgment. State the assumption you are making, and do not ask again.`;
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
  private readonly provisionRoot: string;
  private readonly provisionHomeDir: string;
  private readonly provision: typeof provisionTools;
  private readonly installDeps: typeof installJsDeps;
  /** Resolved once from the worker's docker wiring (PRD #83 M1). `dockerWired` gates
   *  the Bash guardrail; `dockerHost` is injected into the SDK env when present. */
  private readonly dockerWired: boolean;
  private readonly dockerHost?: string;
  /** The worker→API client for the lead's save_memory tool (PRD #90); undefined in
   *  tests/stubs that never register it. */
  private readonly client?: WorkerClient;
  /** The inline run-summary generator (PRD #362 M3c). Real by default (rooted at the
   *  shared provisioning HOME, a writable data-volume path — never the clone); tests
   *  inject a fake. Only used by the advisory summary hooks. */
  private readonly summaryRunner: SummaryRunner;
  /** Every pid spawned across the current run's turns, for the done-path reap.
   *  Private to THIS instance — one SdkExecutor is built per run (PRD #42 Decision
   *  4), so two concurrent runs can never wipe/kill each other's set. */
  private readonly spawnedPids = new Set<number>();

  /**
   * @param homeDir per-run SDK HOME (`agent-home/<runId>` on $UZI_DATA_DIR, PRD #42
   *   Decision 5) so the SDK's process-global $HOME/.claude state (session
   *   transcripts, history, todos, shell snapshots) can't race or leak between
   *   concurrent runs and survives a container restart for resume. The nix/devbox
   *   provisioning HOME is SEPARATE and shared (opts.provisionHomeDir).
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
    // Provisioning HOME + root are SHARED worker-lifetime paths (Decision 5): they
    // must NOT be derived from the per-run SDK homeDir, or the nix profile/devbox
    // warm-start state would fragment per run. Per-run provisioning dirs live under
    // provisionRoot/<runId>, OUTSIDE any clone (Decision 3), so the synthesized
    // devbox.json is never repo-borne.
    this.provisionHomeDir = opts.provisionHomeDir ?? this.homeDir;
    this.provisionRoot =
      opts.provisionRoot ??
      path.join(path.dirname(this.provisionHomeDir), "provision");
    this.provision = opts.provision ?? provisionTools;
    this.installDeps = opts.installDeps ?? installJsDeps;
    // Docker wiring (PRD #83 M1): derive the two consumer facts ONCE. `dockerHost` set
    // ⇒ a sidecar daemon is reachable, so the guardrail allows docker and DOCKER_HOST is
    // injected into the SDK env; absent ⇒ docker is denied and never injected.
    this.dockerHost = opts.dockerWiring?.dockerHost;
    this.dockerWired = this.dockerHost !== undefined;
    this.client = opts.client;
    // PRD #362 M3c: default the summary runner to the SHARED provisioning HOME
    // (Decision 1 needs an ephemeral SDK home on the writable data volume; that root
    // is where the judge/run put theirs — NOT the clone). Tests inject a fake.
    this.summaryRunner =
      opts.summaryRunner ?? new SummaryRunner(this.log, { homeRoot: this.provisionHomeDir });
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
  private emitSkillDrops(
    ctx: RunContext,
    localDrops: readonly SkillDrop[],
  ): void {
    const all: SkillDrop[] = [...(ctx.skillsDropped ?? []), ...localDrops];
    for (const d of all) {
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: { text: describeSkillDrop(d.name, d.reason) },
      });
    }
  }

  /**
   * PRD #362 M3c PLAN-summary hook. AWAITS a plan summary + deltas for `approvedPlan`
   * (blocking, up to the model timeout INTERNAL to generatePlanSummary — Decision 2:
   * it blocks gate ENTRY, never the terminal outcome), then posts it carrying
   * `plan_md: approvedPlan` as the Decision 3 stale-write guard value.
   *
   * ORDERING (code review PR #387, finding 1): this MUST run AFTER the gate has persisted
   * plan_md, or the server guard `plan_md = @expected` matches 0 rows and 409s the write.
   * It is therefore wired as the gate's `onAwaitingApproval` callback — the gate reports
   * awaiting_approval (SetRunAwaitingApproval writes `approvedPlan`, NUL-stripped, to
   * runs.plan_md) and THEN invokes this, so `planMd` here is the exact persisted text and
   * the guard matches (a superseded plan → 409, dropped). The autopilot short-circuit
   * never persists plan_md and never invokes the callback, so it generates no plan summary.
   *
   * ADVISORY: issue runs only, token + client + resolved-PRD present, and EVERYTHING is
   * wrapped — a null (timeout / model error / unusable output), a 409 stale, a 400 bad
   * deltas, or any throw is logged and swallowed so a summary failure NEVER blocks the
   * gate or changes the run's outcome.
   */
  private async generateAndPostPlanSummary(
    ctx: RunContext,
    approvedPlan: string,
    prdInputP: Promise<PrdInput> | undefined,
  ): Promise<void> {
    const token = ctx.oauthToken?.trim();
    const isIssue = ctx.kind === "issue" || ctx.kind === undefined;
    if (!isIssue || !token || !this.client || !prdInputP) return;
    try {
      const prd = await prdInputP;
      const r = await this.summaryRunner.generatePlanSummary({
        token,
        model: ctx.summaryModel ?? "",
        issueTitle: ctx.issueTitle,
        issueBody: ctx.issueDescription,
        prdText: prd.prdText,
        planMd: approvedPlan,
      });
      if (r) {
        await this.client.postPlanSummary(ctx.runId, {
          summary: r.summary,
          deltas: r.deltas,
          // Mirror the server's NUL-strip (SetRunAwaitingApproval stores
          // stripNULParam(plan_md)) so the stale-write guard's `plan_md = @expected`
          // matches the stored column even for a NUL-bearing plan — otherwise a lone
          // 0x00 in the plan text would silently 409 every plan-summary write.
          plan_md: approvedPlan.replaceAll("\u0000", ""),
        });
      }
    } catch (err) {
      this.log.warn("plan summary hook failed", {
        run_id: ctx.runId,
        error: errMessage(err),
      });
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
    // The shared provisioning HOME lives elsewhere on the data volume; ensure it
    // exists before the install subprocess sets HOME to it (a no-op when it equals
    // the per-run homeDir, e.g. tests that don't split them).
    if (this.provisionHomeDir !== this.homeDir) {
      await fs.mkdir(this.provisionHomeDir, { recursive: true });
    }

    // Tool provisioning (PRD #18 M3): before the SDK starts, install the run's
    // tier-1 (∪ opted-in tier-2) packages in a secret-scrubbed subprocess and fold
    // the resulting (allowlisted) tool env into the SDK env. No packages ⇒ exactly
    // today's behavior. A provision failure FAILS the run — never silent
    // degradation. Shared with the stub executor (provision-run.ts) so the two
    // never drift. HOME here is the SHARED provisioning HOME (warm-start), NOT the
    // per-run SDK homeDir (Decision 5).
    const { toolEnv, provisionDir } = await provisionRunTools(ctx, {
      provisionRoot: this.provisionRoot,
      homeDir: this.provisionHomeDir,
      log: this.log,
      provision: this.provision,
    });

    // JS dependency provisioning (PRD #121 M2). Kicked off HERE — after
    // provisionRunTools, so the install resolves the RUN's provisioned node/npm off
    // toolEnv's PATH rather than the image's, and before the plan turn, so it overlaps
    // the plan turn and (on a human-gated run) the whole `awaiting_approval` wait.
    // JOINED before the first implement turn, below. NOT awaited here: awaiting would
    // throw the overlap away, which is the entire wall-clock argument for doing this.
    const depsAbort = new AbortController();
    const depsInstall = this.startDepsInstall(ctx, toolEnv, depsAbort.signal);
    // The install's per-dir verdicts, kept alive to the END of the run rather than
    // consumed and dropped at the join. The install fires BEFORE the plan turn, so by
    // the time anything downstream asks "were the deps actually there?" the answer is
    // long out of scope — and that question is precisely what gate honesty (PRD #121 M4,
    // split out) has to answer to write a reason line a reviewer can act on. Deliberately
    // INTERNAL: `ExecutorResult` gains no field for it while nothing in this PRD consumes
    // one. The precedent for opening that door cheaply is `toolEnv`, which rides
    // ExecutorResult for exactly this kind of executor-computed state (runner.ts).
    let depsResults: JsDepsResult[] = [];
    // Audit 1: carried alongside the results, because a bounded scan that reads as full
    // coverage is exactly the unexplainable `command not found` this PRD removes — and
    // the prompt's consumer is the agent that will actually run the gates.
    let depsTruncated = false;

    const env = buildSdkEnv(oauthToken, this.homeDir, toolEnv, this.dockerHost);
    // PRD #122 M2: `let`, not `const` — the implement loop raises it to the server's
    // milestone-scaled ceiling when the state-report ACK serves one (Decisions 5/5b). The
    // claim config already carries the scaled value on a RESUME (the run is frozen); the
    // ACK path is for a FRESH run frozen mid-run at plan-approval, whose claim predated the
    // freeze. Seeded from the claim's max_iterations (scaled on a resume) or the default.
    let maxIterations = positive(ctx.config?.max_iterations, DEFAULT_MAX_ITERATIONS);
    // PRD #122 M2: the newest progress snapshot the lead has reported, carried into the
    // next iteration's `running` report so the server sees it. Updated after each turn.
    let latestProgress: MilestoneProgress | undefined;
    // PRD #122 M6: the milestone list FROZEN at the gate, hoisted out of the block-scoped
    // `candidateMilestones` inside the plan-and-gate block so the implement loop can name
    // the milestones in its prompt. Assigned once the gate resolves `approve`; it stays
    // undefined on the pre-approved resume path (no plan-and-gate block runs there) —
    // acceptable for M6, which introduces no claim change to carry it across a resume.
    let frozenMilestones: Milestone[] | undefined;
    const maxRevisions = planMaxRevisionsOf(ctx.config);

    // Skills (PRD #16 M4 + M6). Assemble the run's skill set and materialize a
    // local plugin dir OUTSIDE the clone (loads under `settingSources: []`, so the
    // injection defense never loosens), rebuilt on every claim incl. resume. The
    // same prepareSkillPlugin path the stub executor uses, so the two never drift.
    // The worker owns the gapless seq, so it logs every dropped skill.
    const prepared = await prepareSkillPlugin(
      ctx,
      resolveSkillCaps(ctx.config),
    );
    const runSkills = prepared.runSkills;
    const skillsPluginPath = prepared.pluginPath;
    this.emitSkillDrops(ctx, prepared.drops);

    // Repo instructions (PRD #246 M2). When the repo owner opted in, read + sanitize
    // the clone's ROOT CLAUDE.md and frame it ONCE as a nonce-fenced UNTRUSTED/ADVISORY
    // block — read once (one file read), framed once (one nonce), threaded to BOTH
    // buildLeadSystemPrompt sites below. `settingSources: []` is untouched: this is our
    // own read+inject channel, never the SDK's project loader. Lead-only. The outcome
    // (injected byte count, or the drop reason) is trace-logged like emitSkillDrops.
    let repoInstructionsBlock: string | undefined;
    if (ctx.repoClaudemdEnabled) {
      const result = await readRepoInstructions(ctx.worktreePath);
      if ("text" in result) {
        repoInstructionsBlock = buildRepoInstructionsContext(result.text);
        // Only claim "injected" when the FRAMED block is non-empty. A present but
        // whitespace-only CLAUDE.md frames to "" (buildLeadSystemPrompt then injects
        // nothing), so emitting "injected … (N bytes)" here would be a false status.
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: repoInstructionsBlock
              ? `repo instructions: injected root CLAUDE.md as advisory lead context (${Buffer.byteLength(result.text, "utf8")} bytes)`
              : `repo instructions: not injected (empty)`,
          },
        });
      } else {
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: { text: `repo instructions: not injected (${result.dropped})` },
        });
      }
    }

    // Subagents: each def.skills is its allocated delivered skills (re-filtered to
    // the materialized survivors, so it never lists a uzi:<name> not in the plugin
    // dir) plus the all-templates repo survivors (PRD §Worker point 3).
    const survivorNames = new Set(runSkills.map((s) => s.name));
    const assembled = assembleAgents(
      ctx.agents ?? [],
      survivorNames,
      prepared.repoSurvivorNames,
    );
    // Model precedence (PRD #17 Decision 6): the run owner's per-user default
    // model wins over the lead template's model for the main thread. Set the key
    // ONLY when a model is resolved — `model: undefined` must stay omitted (never
    // an explicit key), so an unset model falls back to the SDK/account default.
    // Computed HERE (before the plan-turn copy at planTurnSubagents) so the PRD #305
    // own-roster override below can run against the same value the lead uses.
    const leadModel = resolveLeadModel(
      ctx.config?.default_model,
      assembled.leadModel,
    );
    // PRD #305: apply the run's model to every OWN subagent now, before planTurnSubagents
    // copies them and before the own-source implement path reuses them, so both the plan
    // turn and an own-source implement follow the lead model. The repo roster is built
    // later and overridden where it is built. Off/no-model = pins win (byte-identical).
    if (ctx.config?.override_subagent_model && leadModel) {
      applySubagentModelOverride(assembled.subagents, leadModel);
    }
    // The plan turn runs with the OWN subagents (PRD #37 Decision 5): the roster
    // choice only takes effect after the plan is approved. #203 additionally
    // strips the file-write tools from each of them, and DROPS any whose declared
    // allowlist consisted only of write tools (see planTurnSubagents).
    //
    // The names come from the RESULT, never from `assembled.subagents`. They feed
    // the plan turn's Agent-guard allowSet AND the "Available subagents to
    // delegate to:" line of every plan prompt, so a name surviving here while its
    // definition is gone would advertise — and let the lead invoke — an agent the
    // SDK cannot resolve. Every use of this list is plan-turn; the implement turn
    // derives its own from `selectSubagents`.
    const planTurn = planTurnSubagents(assembled.subagents);
    const planSubagentNames = Object.keys(planTurn.subagents);
    // PRD #266 M1: each roster name's write capability, derived from the PRE-STRIP
    // `assembled.subagents` (NOT `planTurn.subagents`, which subtracts the write
    // tools and would falsely mark `coder` read-only). Built once and threaded into
    // every plan prompt AND the implement prompt, looked up by roster name.
    const subagentCanWrite = subagentWriteCapabilities(assembled.subagents);
    if (planTurn.dropped.length > 0) {
      // Visible rather than silent: an agent missing from the plan wave is a
      // template-shape problem the operator can fix, and the approver should not
      // have to infer the omission from a plan that never mentions the role.
      this.log.warn("plan turn: dropped subagents whose allowlist is only write tools", {
        run_id: ctx.runId,
        dropped: planTurn.dropped,
      });
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `not planning with ${planTurn.dropped.join(", ")}: ${planTurn.dropped.length === 1 ? "its declared toolset is" : "their declared toolsets are"} file-writing only, and the planning turn grants no write tools`,
        },
      });
    }

    // The Bash + path-guard hooks are roster-independent, so build them once and
    // reuse; only the Agent-guard hook's allowSet is frozen at construction and so
    // must be REBUILT when the implement roster differs from the plan roster (PRD
    // #37 — an excluded or repo-sourced subagent must be denied by the guard).
    const bashHook = buildPreToolUseHook(
      this.log,
      this.secretPaths,
      this.dockerWired,
    );
    const pathHook = buildPathGuardHook(ctx.worktreePath, this.log);
    const preToolUse = (
      allowedSubagents: string[],
    ): NonNullable<SdkOptions["hooks"]>["PreToolUse"] => [
      { matcher: "Bash", hooks: [bashHook] },
      {
        matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep",
        hooks: [pathHook],
      },
      {
        matcher: NESTED_AGENT_TOOL,
        hooks: [buildAgentGuardHook(allowedSubagents, this.log)],
      },
      {
        matcher: SEND_MESSAGE_TOOL,
        hooks: [buildSendMessageAliasHook(allowedSubagents, this.log)],
      },
    ];

    // In-process MCP servers the LEAD (full toolset) reaches. The dep-free signal
    // server (plan gate + done) is always present; the memory server (PRD #90) is
    // added ONLY when a client was threaded (main.ts), surfacing save_memory as
    // `mcp__memory__save_memory`. Registered under a DISTINCT key from the signal
    // server so neither disturbs the other; the lead sets no `tools` allowlist, so a
    // registered MCP tool is callable, and save_memory is not in disallowedTools.
    // PRD #72 M4: `prd_done_path` is exposed on signal_done for `issue` runs only
    // (Decision 13). `?? "issue"` matches runner.ts's own `kind: claim.kind ??
    // "issue"` default; a stricter fail-closed default here would silently break
    // every test that omits kind, and the AUTHORITATIVE gate is the api's, where
    // runs.kind is NOT NULL.
    const isIssueRun = (ctx.kind ?? "issue") === "issue";
    const mcpServers: Record<string, McpSdkServerConfigWithInstance> = {
      [SIGNAL_SERVER_NAME]: buildSignalMcpServer({
        prdDonePath: isIssueRun,
        // PRD #122 M1: milestones on submit_plan for issue runs only (Decision 13),
        // gated on the same isIssueRun discriminator as prd_done_path.
        milestones: isIssueRun,
        // PRD #122 M2: report_progress, gated identically — a non-issue run has no
        // milestone list to progress against.
        progress: isIssueRun,
        // PRD #122 M6: the checkpoint tool, gated on the same isIssueRun discriminator —
        // a non-issue run has no milestone boundary to checkpoint at.
        checkpoint: isIssueRun,
        // issue #279: report_only on signal_done, gated on the same isIssueRun discriminator —
        // a non-issue run (ci_fix/self_improve/prompt) has its own terminal paths.
        reportOnly: isIssueRun,
      }),
    };
    if (this.client) {
      mcpServers[MEMORY_SERVER_NAME] = buildMemoryServer({
        client: this.client,
        runId: ctx.runId,
        log: this.log,
      }).server;
      // PRD #158: run-lane forge READ tools (mcp__forge__*). Endpoints are
      // run-scoped, so the server needs only the client + runId — no RunContext
      // change. Built once here and shared by the lead + every subagent (the
      // fact-checker is granted these on the Go side); the per-run call budget is a
      // single counter inside the server, genuinely per-session.
      mcpServers[FORGE_SERVER_NAME] = buildForgeToolsServer({
        client: this.client,
        runId: ctx.runId,
        log: this.log,
      }).server;
      // PRD #333 M2: the incidental-findings capture tool (mcp__findings__report_-
      // incidental_issue). A PLAIN working tool (POST → emit card → ack, turn
      // continues), so it takes `emit` in addition to the run-scoped {client, runId,
      // log}. Mounted here in the `if (this.client)` block = for the SdkExecutor run
      // lane (issue / ci_fix / prompt / self_improve), NOT chat/judge (separate
      // executors) — which is exactly "all autonomous lanes" (D2). DELIBERATELY not
      // gated on isIssueRun (contrast report_progress/checkpoint above): a bug an agent
      // reads past is equally worth capturing on a ci_fix or self_improve run.
      mcpServers[FINDINGS_SERVER_NAME] = buildFindingsToolsServer({
        client: this.client,
        runId: ctx.runId,
        emit: (m) => ctx.emit(m),
        log: this.log,
      }).server;
    }

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
      plugins: [
        { type: "local", path: skillsPluginPath, skipMcpDiscovery: true },
      ],
      // ALWAYS an explicit list — the full plugin-qualified run union. Omitting it
      // is NOT "skills off" (sdk.d.ts:1872: CLI defaults would apply); `[]` when the
      // run has no skills disables all. Per-subagent scoping is each
      // AgentDefinition.skills; the lead is the main thread, covered by this union.
      skills: runSkills.map((s) => qualifiedSkillName(s.name)),
      systemPrompt: buildLeadSystemPrompt(assembled.leadSystemPrompt, {
        kind: ctx.kind,
        repoInstructions: repoInstructionsBlock,
      }),
      // PLAN-TURN roster: every subagent minus the file-write tools (#203).
      // `baseOptions` drives both planning turns (first plan at the
      // `drivePlanningTurn` call in Phase 1, re-plan in the revise loop) while
      // `implementOptions` spreads `baseOptions` and OVERRIDES this key from
      // `selectSubagents(...)`, which reads `assembled.subagents` directly and is
      // therefore untouched.
      //
      // `agents` is one of THREE keys `implementOptions` overrides, and all three
      // are plan-turn-scoped by that same mechanism: `agents`, `systemPrompt` and
      // `hooks`. Worth knowing which, because `hooks` is where the recorded
      // upgrade path goes — a plan-turn-only `PreToolUse` deny on the write-tool
      // family, the one mechanism that would also reach the LEAD's own writes.
      //
      // DO NOT move this denial down to the `disallowedTools` key below. That one
      // is top-level and is NOT overridden, so the spread carries it into the
      // implement turn and it would strip write tools for the whole run, breaking
      // `coder`. The scoping is the point of doing it here.
      agents: planTurn.subagents,
      // In-process tools the lead calls: the signal server (gate the plan / mark
      // done, see signals.ts) plus, when a client is threaded, the memory server
      // (save_memory, PRD #90). Only the lead (full toolset) reaches them.
      mcpServers,
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      // Block the deferral tools so the lead can't background work to a future
      // turn that the per-turn reap would only wake to a killed subagent (#34).
      // Delegation is forced synchronous by the Agent guard hook below.
      disallowedTools: [...ASYNC_DEFERRAL_TOOLS],
      // The load-bearing deny layer: a PreToolUse deny blocks a tool even under
      // bypassPermissions. Bash screening, the file-tool path jail, AND the M4
      // hard-fail-on-unexpected-subagent guard (item 7) all live here. The plan
      // turn allows the own subagents THAT SURVIVED the #203 transform — the
      // allowSet is frozen at construction, so it must be the same list the
      // `agents` map above holds, or the guard admits an Agent call the SDK
      // cannot resolve. The implement turns rebuild this with the selected
      // roster (PRD #37).
      hooks: {
        PreToolUse: preToolUse(planSubagentNames),
      },
      // Persist discrete blocks only; partial token deltas would flood the seq
      // stream (the live-partial channel is M5, not M3).
      includePartialMessages: false,
    };
    // `leadModel` is computed above (before the plan-turn copy) so the PRD #305
    // own-roster override can use it; here it only drives the top-level lead model.
    if (leadModel) baseOptions.model = leadModel;

    // PRD #122 M2: the wall budget the run STARTED with, kept so the loop can scale the
    // worker's own soft wall reference by the server-served delta exactly once (below).
    // On a resume this is already the scaled claim value; on a fresh run it is the global
    // default until the ACK serves the scaled seconds.
    const initialWallMs = seconds(
      ctx.config?.run_timeout_seconds,
      DEFAULT_RUN_TIMEOUT_SECONDS,
    );
    // Applied-once latch so a repeated ACK cannot keep inflating the wall reference.
    let wallScaled = false;
    const state: RunDrive = {
      currentChild: {},
      wallRemainingMs: initialWallMs,
      reportedSessionId: false,
    };
    const idleMs = seconds(
      ctx.config?.idle_timeout_seconds,
      DEFAULT_IDLE_TIMEOUT_SECONDS,
    );

    // Cancel/shutdown spans the whole run (all turns + the gate). It trips the
    // current turn's abort; the gate also unblocks via the steering verdict. It also
    // reclaims the dependency install (PRD #121 M2) — a cancelled run must not sit at
    // the join waiting out an install whose results nobody will read.
    const onSignal = (): void => {
      this.trip(state, REASON_CANCELLED);
      depsAbort.abort();
    };
    if (ctx.signal) {
      if (ctx.signal.aborted) onSignal();
      else ctx.signal.addEventListener("abort", onSignal, { once: true });
    }

    // The SDK session id evolves across turns; resume each turn from the last.
    let resumeId = ctx.sessionId ?? undefined;

    try {
      // --- PRD #35 Decision 6b + PRD #209 D4: skip planning for an approved plan ---
      //
      // Fires when the plan is approved, there is plan text to implement, AND either a
      // resumable session exists (PRD #35: resume past an approval that already happened)
      // OR the run is SEEDED (PRD #209: the user authored the plan at create time).
      // `planApproved` alone is not enough: without EITHER a session or a seeded plan
      // there is no basis to skip, and without the plan text the implement loop would
      // start with no instructions. All missing ⇒ fall through and plan as today, which
      // is also what a park BEFORE approval does.
      //
      // The seeded relaxation does not weaken the session guard — it removes a guard that
      // never applied. `!!ctx.sessionId` exists to stop a NON-seeded run whose transcript
      // was dropped from skipping planning (it must re-plan — D4 row 3), but a seeded run
      // never had a session, so "no session" is its normal state rather than a lost one.
      // The runner already encodes this: it sets ctx.planApproved only when
      // plan_approved && (sessionId || seeded), so a dropped-transcript non-seeded run
      // arrives here with planApproved=false and this branch is not taken.
      //
      // Skipping the gate is NOT a weakening of the approval guardrail. For a PRD #35
      // resume the approval already happened on this exact plan (a consumed approve_plan
      // input, or autopilot). For a seeded run the plan is the USER'S OWN, supplied
      // through an authenticated create call — there was never a gate to skip, because
      // the human authored the plan instead of approving a worker's. Either way what is
      // skipped is asking a human to approve a plan they already own; the alternative for
      // a seeded run is not "more oversight" but a run that parks unattended and can die
      // on REASON_NO_PLAN.
      const preApproved =
        ctx.planApproved === true &&
        (!!ctx.sessionId || ctx.seeded === true) &&
        !!ctx.approvedPlan?.trim();

      // Hoisted above the skip so the post-gate code (the ci_fix not_code check, the
      // planning label) reads the same values on both paths.
      const isCIFix = ctx.kind === "ci_fix" && ctx.pipeline != null;
      const isSelfImprove = ctx.kind === "self_improve";
      // Assigned by the gate below, or seeded directly on the pre-approved path.
      let approvedPlan: string;
      // The agent selection the approve verdict carried (PRD #37).
      //
      // The persisted selection now arrives ON THE CLAIM (PRD #35), so a pre-approved
      // resume honours the same human verdict that let it skip the gate. This block
      // used to describe a LIVE DEFECT — the claim carried no selection and a human
      // who excluded a subagent got it back after a park. That is fixed; what follows
      // is about the one path that still resolves without a selection.
      //
      // WHEN THE CLAIM CARRIES NOTHING there are two cases and they want the SAME
      // handling, which is why neither is special-cased:
      //   - the run never reached a gate, so there is no human verdict to honour and
      //     the absent-default IS the correct answer, not a degradation; and
      //   - the server predates the field.
      //
      // WHAT THE ABSENT-DEFAULT DOES, kept because it is what the second case gets
      // and because an earlier version of this comment understated it and got one
      // claim wrong outright:
      //
      //  1. The larger effect is not exclusions, it is the SOURCE — `own` becomes
      //     `repo` whenever the clone has a roster. Dropped exclusions are secondary.
      //  2. "Every subagent is still drawn from the same vetted set" — WHAT THIS
      //     COMMENT USED TO SAY — IS FALSE. selectSubagents("repo", …) returns
      //     subagentsFromTemplates(repoTemplates, …): the CLONED REPO's
      //     `.claude/agents/` files, not the owner's admin-allocated templates. A
      //     different set, not the same set differently filtered.
      //  3. The skill-scoping regime changes with the source, per agents.ts's own
      //     docstring: `own` keeps skills per-template (the allocations ARE the
      //     admin's scoping surface there), while `repo` gives every subagent the
      //     run's whole surviving skill set.
      //
      // The SECURITY claim holds throughout, and was checked rather than assumed:
      // repo agents carry REPO_AGENT_DENIED_TOOLS, canonicalized against aliases so a
      // `tools: [Task]` cannot slip through; sdk-executor re-enforces it; and the
      // Agent-guard hook's allowSet is rebuilt from the RESOLVED selection. A roster
      // and scoping difference, never a guardrail bypass — but do not read "not a
      // bypass" as "no difference", which is where the old wording led.
      // Seeded from the run's PERSISTED selection when the claim carried one, so a
      // pre-approved resume honours the same human verdict that let it skip the gate.
      // The gated path below overwrites this from the approve verdict as before.
      //
      // `{status: "absent"}` when the claim carried nothing. That is the right answer
      // for a run that never reached a gate — and for an older server that predates
      // the field — because resolveAgentSelection's absent-default (repo when a
      // roster was detected, else own) is exactly what such a run should get. It is
      // NOT folded into "invalid": absence is not a signal to distrust, and forcing
      // `own` would pin every gate-less run to a source this code guessed.
      let approvedSelection: AgentSelectionParse = ctx.approvedSelection
        ? { status: "ok", selection: ctx.approvedSelection }
        : { status: "absent" };

      /** PRD #88: parks used so far, against QUESTION_MAX. Worker-side because a
       *  question is an in-process ask_user signal — there is no run_user_inputs row
       *  for the server to count, unlike a revise_plan.
       *
       *  Declared ABOVE the preApproved branch, not inside it, and that placement is
       *  load-bearing twice over. The cap is per RUN: M4 lets the lead ask before it
       *  plans, so a budget scoped to the planning branch would reset between the
       *  planning and implement phases and silently be worth 2 x QUESTION_MAX. And a
       *  PRE-APPROVED resume (PRD #35) skips the planning branch entirely while its
       *  implement loop can still ask, so a budget declared in the else-block is not
       *  merely mis-scoped there — it does not exist on that path at all.
       *
       *  NOT DURABLE, and the honest bound is worth stating because the other #88
       *  bound already states its own: this is worker memory, so a worker death
       *  re-queues the run, execute() runs fresh, and the count restarts at 0. The
       *  real lifetime ceiling is QUESTION_MAX x (RUN_MAX_REQUEUES + 1) — 10 on
       *  defaults, not 5 — exactly as QUESTION_TIMEOUT_SECONDS multiplies for the
       *  identical reason. Documenting one and not the other would be worse than
       *  documenting neither: a reader who finds the timeout's caveat reasonably
       *  infers the cap has none. */
      const budget = { asked: 0 };

      // --- PRD #362 M3c: inline run-summary hooks (advisory, issue runs only) ---
      // Guard ALL summary work: issue runs only (kind "issue" or undefined), an OAuth
      // token, and a client to POST to — a stub/e2e run without a token or client is
      // unaffected. resolvePrdInput is memoized ONCE per run (a single file read) and
      // reused by BOTH hooks; it never throws.
      const summariesOn =
        (ctx.kind === "issue" || ctx.kind === undefined) &&
        !!oauthToken &&
        !!this.client;
      const prdInputP: Promise<PrdInput> | undefined = summariesOn
        ? resolvePrdInput(ctx.issueDescription, ctx.worktreePath, this.log)
        : undefined;

      // INTENT hook (Decision 5: BOTH the seeded/pre-approved AND planning paths get
      // an intent summary, so it sits BEFORE the preApproved branch). FIRE-AND-FORGET
      // (Decision 2: intent is async/non-blocking — planning must proceed immediately,
      // so this is deliberately NOT awaited). Skipped when the intent summary is
      // already set (Decision 3: a resume/re-claim must not re-spend the owner's
      // token). Every failure is swallowed to a warn, and the trailing `.catch`
      // guarantees a late rejection can never surface as an unhandledRejection — the
      // run lives through planning+implement, so the promise has time to settle.
      if (summariesOn && !ctx.summaryIntentPresent) {
        void (async () => {
          try {
            const prd = await prdInputP!;
            const summary = await this.summaryRunner.generateIntentSummary({
              token: oauthToken,
              model: ctx.summaryModel ?? "",
              issueTitle: ctx.issueTitle,
              issueBody: ctx.issueDescription,
              prdText: prd.prdText,
            });
            if (summary) await this.client!.postIntentSummary(ctx.runId, summary);
          } catch (err) {
            this.log.warn("intent summary hook failed", {
              run_id: ctx.runId,
              error: errMessage(err),
            });
          }
        })().catch(() => {});
      }

      if (preApproved) {
        // PRD #209: distinguish an externally-supplied (seeded) plan from a PRD #35
        // resume so the transcript records the provenance rather than leaving it
        // inferable. Both phrasings carry "skipping the planning turn and the approval
        // gate" (the M6 e2e pins that substring); a non-seeded resume is unchanged.
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: ctx.seeded
              ? "implementing a user-supplied plan (seeded) — skipping the planning turn and the approval gate"
              : "resuming an already-approved plan — skipping the planning turn and the approval gate",
          },
        });
        approvedPlan = ctx.approvedPlan!;
      } else {
        // --- Phase 1: planning turn ------------------------------------------
        // A ci_fix run (PRD #6) diagnoses a failed pipeline from its frozen snapshot
        // (untrusted job logs) instead of a forge issue; everything else — the plan
        // gate, the implement⇄review loop, the guardrails — is identical.
        let planPrompt: string;
        if (isCIFix) {
          planPrompt = buildCIFixPlanPrompt({
            ref: ctx.pipeline!.ref,
            branch: ctx.branch,
            pipelineWebURL: ctx.pipeline!.web_url,
            failedJobs: ctx.pipeline!.failed_jobs.map((j) => ({
              name: j.name,
              stage: j.stage,
              logTail: j.log_tail,
            })),
            subagentNames: planSubagentNames,
            subagentCanWrite,
            // PRD #90: a ci_fix run can WRITE memory, so it reads the same inert,
            // nonce-fenced cross-run memory back (empty/absent injects nothing).
            memory: ctx.memory,
            // Issue #105: only set when a dropped resume left this turn amnesiac on a
            // branch that already carries pushed work.
            priorWork: ctx.priorWork,
            // Judge rec (run 51757591): the commit this branch was cut from, so the lead
            // does not infer the parent from the clone's freshly-fetched default branch.
            baseCommit: ctx.baseCommit,
            defaultBranchCommit: ctx.defaultBranchCommit,
          });
        } else if (isSelfImprove) {
          // The self_improve run's issue_description carries the untrusted improve_uzi
          // backlog; the trusted "pick one / guardrails / tests" directive lives in the
          // prompt builder, outside the untrusted fence (PRD #46 Decision 10, audit C1).
          planPrompt = buildSelfImprovePlanPrompt({
            branch: ctx.branch,
            recommendations: ctx.issueDescription,
            subagentNames: planSubagentNames,
            subagentCanWrite,
            // PRD #90: a self_improve run can WRITE memory, so it reads the same inert,
            // nonce-fenced cross-run memory back (empty/absent injects nothing).
            memory: ctx.memory,
            // Issue #297: the in-flight avoid-set, rendered in its own untrusted fence.
            inflightTargets: ctx.inflightTargets,
            // Issue #105: see above — the fixed self_improve branch's prior cycles.
            priorWork: ctx.priorWork,
            // See above. The fixed self_improve branch is routinely seeded off a previous
            // cycle's tip, so its base is the least guessable of the three kinds.
            baseCommit: ctx.baseCommit,
            defaultBranchCommit: ctx.defaultBranchCommit,
          });
        } else {
          planPrompt = buildPlanPrompt({
            issueIid: ctx.issueIid ?? 0,
            issueTitle: ctx.issueTitle,
            issueDescription: ctx.issueDescription,
            branch: ctx.branch,
            subagentNames: planSubagentNames,
            subagentCanWrite,
            // PRD #90: inert, nonce-fenced, untrusted-advisory cross-run memory (the
            // runner fetched it at claim time; empty/absent injects nothing).
            memory: ctx.memory,
            // Issue #105: see above — prior pushed work on this issue's branch.
            priorWork: ctx.priorWork,
            // See above.
            baseCommit: ctx.baseCommit,
            defaultBranchCommit: ctx.defaultBranchCommit,
          });
        }
        const planningLabel = isCIFix
          ? "diagnosing CI failure"
          : isSelfImprove
            ? "planning self-improvement"
            : "planning";
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: { text: `starting SDK agent (${planningLabel})` },
        });

        const plan = await this.drivePlanningTurn(
          ctx,
          baseOptions,
          resumeId,
          planPrompt,
          state,
          idleMs,
          budget,
        );
        resumeId = plan.sessionId ?? resumeId;

        // --- Plan gate (+ revision loop, PRD #41) -----------------------------
        // The gate can be re-entered N times under ONE approval budget: a `revise`
        // verdict carries the reviewer's feedback, and the worker runs a fresh planning
        // turn (resumed, so the planning context is retained) and re-gates the new plan.
        // approve/reject/cancel are terminal; the runner advances the steering epoch each
        // time a revise is taken. FAIL-CLOSED: the while-condition + the post-loop guards
        // guarantee the only way past this block is an `approve` (see the explicit guard).
        if (!ctx.gatePlan)
          throw new Error("plan gate is not wired for this run");
        approvedPlan = plan.plan;
        // PRD #122 M1: the CANDIDATE milestone list rides every gate call so the human
        // approves the breakdown. It is REPLACED on each revision round (Decision 2),
        // tracked alongside approvedPlan.
        let candidateMilestones = plan.milestones;
        // PRD #362 M3c PLAN hook (Decision 2): generate + post the plan summary as the
        // gate's onAwaitingApproval callback, so it fires AFTER the gate persists plan_md
        // (the summary's stale-write guard value) and BEFORE the verdict wait — blocking
        // gate ENTRY up to the model timeout but NEVER the terminal outcome. Posting it
        // before the gate (as an earlier revision did) always 409s against a NULL/previous
        // plan_md and is silently dropped. The autopilot short-circuit never invokes the
        // callback, so an auto-approved run generates no plan summary. Advisory — the
        // helper swallows every failure.
        let verdict = await ctx.gatePlan(approvedPlan, candidateMilestones, (planMd) =>
          this.generateAndPostPlanSummary(ctx, planMd, prdInputP),
        );
        let revisions = 0;
        while (verdict.kind === "revise") {
          const feedback = verdict.feedback;
          // Record the reviewer's feedback on the feed. Ordered BEFORE the revision turn
          // (and thus before the next gatePlan flushes the new plan), so the feed never
          // lags the awaiting_approval re-report.
          ctx.emit({
            kind: "plan_feedback",
            agent: "worker",
            payload: { feedback },
          });
          // Belt-and-suspenders (PRD #41 Decision 3c): the SERVER enforces the same cap at
          // submit time and won't enqueue a revise past it, so this should never trip. If it
          // does, DO NOT run another planning turn — record it and re-gate the current plan so
          // the run stays fail-closed rather than revising unbounded.
          if (revisions >= maxRevisions) {
            this.log.warn(
              "plan revision budget exhausted; re-gating without a turn",
              { run_id: ctx.runId, max_revisions: maxRevisions },
            );
            ctx.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: "revision budget exhausted — not revising the plan further",
              },
            });
            verdict = await ctx.gatePlan(approvedPlan, candidateMilestones);
            continue;
          }
          revisions++;
          ctx.emit({
            kind: "plan_revising",
            agent: "worker",
            payload: { round: revisions },
          });
          // A revision turn is a PLANNING turn (pre-approval), so it runs with the OWN
          // subagents (baseOptions), exactly like the first plan turn — the roster
          // selection only takes effect once a plan is APPROVED (PRD #37 Decision 5).
          const turn = await this.drivePlanningTurn(
            ctx,
            baseOptions,
            resumeId,
            buildRevisePlanPrompt(feedback),
            state,
            idleMs,
            budget,
          );
          resumeId = turn.sessionId ?? resumeId;
          approvedPlan = turn.plan;
          // Decision 2: the candidate is REPLACED across a revision round.
          candidateMilestones = turn.milestones;
          // PRD #362 M3c: a re-plan REGENERATES the plan summary (Decision 2), fired from
          // the gate's onAwaitingApproval callback (after the re-report persists the NEW
          // plan_md) so its stale-write guard matches the new plan.
          verdict = await ctx.gatePlan(approvedPlan, candidateMilestones, (planMd) =>
            this.generateAndPostPlanSummary(ctx, planMd, prdInputP),
          );
        }
        if (verdict.kind === "reject")
          throw new PlanRejectedError(verdict.reason);
        if (verdict.kind === "cancel") throw new Error(REASON_CANCELLED);
        // FAIL-CLOSED: the loop + guards above leave only `approve`; any other kind is a
        // bug (e.g. a future verdict variant) and must never fall through into implement.
        if (verdict.kind !== "approve")
          throw new Error(
            `unexpected plan verdict: ${(verdict as { kind: string }).kind}`,
          );
        approvedSelection = verdict.selection;
        // PRD #122 M6: freeze the APPROVED milestone breakdown for the implement loop.
        // `candidateMilestones` is block-scoped and REPLACED across revision rounds
        // (Decision 2), so this captures the final approved list — the one the loop names
        // in its prompt and the checkpoint reports progress against.
        frozenMilestones = candidateMilestones;
      } // end of the plan-and-gate block skipped by a pre-approved resume

      // A ci_fix run whose approved plan is a not_code verdict is done: no code to
      // implement, no branch to push. The run completes with the diagnosis as its
      // value (PRD #6). Detected AFTER approval so a human confirmed the verdict.
      if (isCIFix && isNotCodePlan(approvedPlan)) {
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "diagnosis: not a code problem — completing with no fix",
          },
        });
        this.log.info("ci_fix run: not_code verdict", { run_id: ctx.runId });
        return { branch: ctx.branch, fixVerdict: "not_code" };
      }

      // --- Join the JS dependency install (PRD #121 M2) ---------------------
      // The IMPLEMENT phase must never race the install. Past this point the agent
      // runs `npm test` / `vitest` / `tsc`, and will run its own `npm ci` if it thinks
      // deps are missing; npm has NO cross-process `node_modules` lock, so a concurrent
      // worker-side install in the same dir would corrupt the tree. Joining here is what
      // makes that impossible FOR THE IMPLEMENT TURNS — placed after the not_code return
      // above, so a ci_fix that never implements does not wait for deps it will not use.
      //
      // RESIDUAL, AND IT IS NOT CLOSED BY THIS JOIN: the PLAN turn can do the same thing.
      // It runs under `permissionMode: "bypassPermissions"` with full Bash, and
      // guardrails.ts has no package-manager rule of any kind (verified: it screens git
      // push/history/config, credential reads, /proc, secret paths, `env` and docker —
      // nothing about npm/pnpm/yarn/bun). So a planning agent exploring the repo can run
      // `npm ci` in a dir this install is mid-flight in. That window is DELIBERATE — the
      // overlap is the entire wall-clock argument for starting before the plan turn — so
      // it is named here rather than fixed; closing it would mean giving the overlap up.
      //
      // Worth knowing what it costs, because it is worse than "the deps are missing": a
      // half-written `node_modules` still EXISTS, so `defaultCheckRunner`'s
      // `requires: "node_modules"` pre-flight passes, the check runs against a corrupt
      // tree, and it FAILS — accusing good code of failing, which is the one thing
      // self-improve.ts's status mapping exists to prevent.
      ({ results: depsResults, truncated: depsTruncated } =
        await this.joinDepsInstall(ctx, depsInstall));

      // --- Apply the agent selection at the gate boundary (PRD #37 Decision 5) ---
      // The plan turn ran with the OWN subagents; the approved selection now decides
      // the roster for the implement phase. The lead ALWAYS stays uzi's builtin from
      // the claim, so only the subagents change: rebuild the agents map, the
      // Agent-guard hook (its allowSet is frozen at construction — an excluded or
      // repo-sourced subagent must be denied), the lead system prompt (a repo-source
      // run adds the untrusted-review passage), and the subagent names fed to the
      // implement prompt. Malformed selection resolves to `own`, never repo — true of
      // an INVALID selection, and it does NOT generalise to "we never silently pick
      // repo": an ABSENT selection resolves to repo when a roster was detected, which
      // is what a gate-less run and an older server both get (see further up).
      const repoAvailable = (ctx.repoAgents?.length ?? 0) > 0;
      const resolved = resolveAgentSelection(approvedSelection, repoAvailable);
      if (resolved.note)
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: { text: resolved.note },
        });
      const selection = resolved.selection;
      // Skill scoping is source-dependent from here (PRD #72 M1): `own` keeps the
      // per-template allocations assembled above; `repo` gives every subagent the
      // full survivor set, since a repo roster has no template rows to allocate
      // against. survivorNames is the filter in both cases, so a skill dropped by
      // the cap or a collision is unreachable either way.
      const selectedSubagents = selectSubagents(
        selection.source,
        assembled.subagents,
        ctx.repoAgents ?? [],
        selection.exclusions,
        survivorNames,
        prepared.repoSurvivorNames,
      );
      // PRD #305: repo-source defs are freshly built from repo files here and were NOT
      // touched by the own-roster override above; apply it so the flag covers the repo
      // roster (the primary case: an auto-approved scheduled run resolves to the repo
      // roster). Own-source defs were copied by reference from the already-overridden
      // assembled.subagents, so this repeat is idempotent for them.
      if (ctx.config?.override_subagent_model && leadModel) {
        applySubagentModelOverride(selectedSubagents, leadModel);
      }
      const selectedNames = Object.keys(selectedSubagents);
      // PRD #266 M1: the implement roster's own capability map. Derived from
      // `selectedSubagents` — NOT the plan-turn `subagentCanWrite` built off
      // `assembled.subagents` — because the repo-source path's defs come from the repo
      // roster and are absent from `assembled.subagents`, so reusing the plan map would
      // miss every repo name and mislabel a write-capable repo subagent read-only.
      // `selectedSubagents` is pre-strip on both sources (own: assembled defs unchanged;
      // repo: freshly-mapped repo defs), which is exactly M1's "implement-turn defs".
      const selectedCanWrite = subagentWriteCapabilities(selectedSubagents);
      const implementOptions: SdkOptions = {
        ...baseOptions,
        agents: selectedSubagents,
        systemPrompt: buildLeadSystemPrompt(assembled.leadSystemPrompt, {
          repoSourced: selection.source === "repo",
          kind: ctx.kind,
          repoInstructions: repoInstructionsBlock,
        }),
        hooks: { PreToolUse: preToolUse(selectedNames) },
      };
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text:
            selection.source === "repo"
              ? `implementing with the repo's agents (${selectedNames.join(", ") || "none"})`
              : `implementing with your agent templates (${selectedNames.join(", ") || "none"})`,
        },
      });

      // --- Phase 2: implement ⇄ review loop --------------------------------
      // PRD #209 (M2 validation): the seeded plan BODY must reach the first implement turn.
      // A session-less seeded run (row 2 cold start, or a requeued seeded run whose
      // transcript was dropped) has no plan turn AND no resumable session carrying the plan,
      // so without this the model never sees the user's plan and falls back to the issue —
      // defeating the feature. Gated on the ABSENCE of a session: a seeded RESUME already
      // has the plan in its session (byte-identical), and every non-seeded path leaves this
      // undefined. buildImplementPrompt embeds it first-turn-only, as authoritative
      // instructions (D5), never untrusted-fenced. The gate is embedSeededPlan (extracted
      // so its defense-in-depth `seeded` term is testable — see that function's doc).
      const seededPlanBody = embedSeededPlan({
        preApproved,
        seeded: ctx.seeded === true,
        hasSession: !!ctx.sessionId,
      })
        ? approvedPlan
        : undefined;
      let iteration = 0;
      let followUp: string | undefined;
      // Hoisted: `turn` is declared INSIDE the loop, so the return below cannot see
      // it and the terminating turn's declaration would be discarded by `break`.
      let declaredPrdPath: string | undefined;
      // PRD #265 M1: hoisted for the same reason as declaredPrdPath — the terminating
      // turn's signal_done declaration must survive the `break` below to reach the
      // completed report.
      let declaredMilestonesCompleted: string[] | undefined;
      // issue #279: hoisted for the same reason as declaredPrdPath — the terminating turn's
      // signal_done report_only/summary declaration must survive the `break` below to reach
      // the final ExecutorResult (and thence the runner's report-only completion path).
      let declaredReportOnly = false;
      let declaredSummary: string | undefined;
      for (;;) {
        iteration++;
        // PRD #122 M2: report the iteration (carrying the latest milestone progress) and
        // apply the server-served effective budget. The cap only ever RISES (a scaled run
        // gets more turns; a single/zero-milestone run's ACK carries none, so this is
        // inert and REASON_MAX_ITERATIONS still trips at the default — the regression gate).
        const served = await ctx.reportIteration?.(iteration, latestProgress);
        if (served) {
          if (
            typeof served.maxIterations === "number" &&
            served.maxIterations > maxIterations
          ) {
            maxIterations = served.maxIterations;
          }
          if (typeof served.wallSeconds === "number") {
            const servedMs = served.wallSeconds * 1000;
            // The wall is PAUSED across the plan gate and debited only in-turn
            // (armWall/disarmWall), so at loop-top only the plan turn has been debited and
            // essentially the full initial budget remains. Adding the delta ONCE scales the
            // worker's soft reference to match the sweeper's effective timeout, so a
            // legitimately-scaled run does not self-trip REASON_WALL at the global 2h.
            if (servedMs > initialWallMs && !wallScaled) {
              state.wallRemainingMs += servedMs - initialWallMs;
              wallScaled = true;
            }
          }
        }
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: { text: `implement/review iteration ${iteration}` },
        });
        const turn = await this.driveTurn(
          ctx,
          implementOptions,
          resumeId,
          buildImplementPrompt({
            branch: ctx.branch,
            subagentNames: selectedNames,
            // PRD #266 M1: the implement roster's OWN capability map (selectedCanWrite),
            // derived from the actual un-stripped implement defs so it is correct on
            // both the own AND repo sources — not the plan-turn map, which is keyed by
            // `assembled.subagents` and would miss every repo-source name.
            subagentCanWrite: selectedCanWrite,
            first: iteration === 1,
            iteration,
            // PRD #209 (Decision A): a seeded run's first-turn opening says the user
            // supplied the plan, not that it was "approved". First turn only (gated
            // inside buildImplementPrompt); false/absent for every non-seeded run.
            seeded: ctx.seeded,
            // PRD #209 (M2 validation): the seeded plan body, embedded first-turn-only.
            // Undefined for every path except the session-less seeded cold start (see
            // seededPlanBody above), so resume/gated implement prompts are unchanged.
            seededPlan: seededPlanBody,
            followUp,
            // #157: the join above populated these, so the first implement turn can be told
            // which dirs are ready and which genuinely are not — the facts the plan turn
            // could not have. Correct on the revise path too: the join runs after the LAST
            // gate round, so however many revisions happened, these are the final outcomes.
            deps: depsResults,
            depsTruncated,
            // First turn only (buildImplementPrompt gates it): this is the phase where the
            // lead hands a subagent a diff command, which is where the wrong one was seen.
            baseCommit: ctx.baseCommit,
            defaultBranchCommit: ctx.defaultBranchCommit,
            // PRD #209 (D7): a requeued seeded run whose transcript was dropped re-enters
            // implement COLD — no plan turn carried the prior-work note. Threaded on the
            // pre-approved path ONLY, so an ordinary gated run's implement prompt is
            // byte-identical to before (it never carried this). First turn only.
            priorWork: preApproved ? ctx.priorWork : undefined,
            // PRD #122 M6: name the frozen milestones and their live status so the lead
            // can see the boundaries and checkpoint at each one. Both undefined on the
            // pre-approved resume path (frozenMilestones is never assigned there) and on
            // any run with no approved breakdown, which makes the note additive-absent.
            milestones: frozenMilestones,
            progress: latestProgress,
            // issue #279: teach the lead the report-only evidence path, ISSUE RUNS ONLY —
            // gated on the same isIssueRun discriminator the signal_done schema uses.
            reportOnly: isIssueRun,
          }),
          state,
          idleMs,
        );
        resumeId = turn.sessionId ?? resumeId;
        if (turn.prdDonePath !== undefined) declaredPrdPath = turn.prdDonePath;
        // PRD #265 M1: latch the finished-milestone declaration the same way, so it
        // survives the terminating turn's `break` into the completed report.
        if (turn.milestonesCompleted !== undefined)
          declaredMilestonesCompleted = turn.milestonesCompleted;
        // issue #279: latch report_only true (like done) and take the last-wins summary
        // (like prdDonePath), so a report-only signal_done on the terminating turn reaches
        // the final ExecutorResult after the break below.
        if (turn.reportOnly) declaredReportOnly = true;
        if (turn.summary !== undefined) declaredSummary = turn.summary;
        // PRD #122 M2: carry this turn's reported progress into the NEXT iteration's
        // `running` report. Only overwrite when the turn reported something, so a quiet
        // turn keeps the last known progress rather than blanking it.
        if (turn.progress) latestProgress = turn.progress;
        // PRD #122 M6 (Decision 10): the lead cooperatively checkpointed a completed
        // milestone. Reap + fetch-back durably, then continue — it ended its turn and will
        // be re-prompted for the next milestone. A turn that is BOTH done and checkpoint
        // still terminates: the `if (turn.done) break;` below wins because we skip the
        // continue when done.
        if (turn.checkpoint && !turn.done) {
          await ctx.checkpoint?.({ reap: true, progress: latestProgress });
          continue;
        }
        if (turn.done) break;
        if (iteration >= maxIterations) throw new Error(REASON_MAX_ITERATIONS);

        // PRD #122 M6 (Decision 10b): iteration-boundary fallback checkpoint. Only reached
        // when the model did NOT cooperatively checkpoint this iteration (that path
        // `continue`d above). Fetch-back WITHOUT reaping — a backgrounded dev server the
        // lead means to reuse next iteration must survive (Decision 10b). Best-effort.
        await ctx.checkpoint?.({ reap: false, progress: latestProgress });

        // PRD #88 clarification park. Deliberately OUTSIDE driveTurn, exactly like the
        // plan gate: the idle watchdog only runs inside a turn, so a run waiting on a
        // human cannot trip REASON_IDLE. That property is free by construction here,
        // not something the park has to defend.
        //
        // The answer is fed to the next turn as an ordinary user turn (via followUp)
        // rather than returned as the tool's result — the lead was told to end its
        // turn after asking, so there is no tool call still open to return into. The
        // resumed session keeps full context, so nothing is replayed.
        //
        // `iteration` is deliberately NOT advanced for the answer round-trip: a
        // clarification is not an implement/review iteration, and counting it would
        // let a talkative lead burn the loop budget on questions. The `continue`
        // re-enters the loop, which increments — so the decrement below keeps the
        // clarification turn free.
        const asked = turn.questions;
        if (asked?.length) {
          const verdict = await this.askUserOrContinue(
            ctx,
            asked,
            budget.asked,
          );
          if (verdict.parked) {
            budget.asked++;
            if (verdict.cancelled)
              throw new PlanRejectedError(
                "run cancelled while awaiting clarification",
              );
            followUp = verdict.followUp;
            iteration--;
            continue;
          }
          // Not parked: the cap is spent, or no askUser is wired. Either way the run
          // continues on the lead's judgment; the notice is already on the feed.
          followUp = verdict.followUp;
          iteration--;
          continue;
        }

        // Fold any queued correction into the next turn (FIFO, one per turn).
        followUp = ctx.pullFollowUp?.();
      }

      // js_deps rides the completion log so a finished run's record says whether its
      // gates were runnable — the same question M4 will have to answer from a durable
      // source. It is also what keeps depsResults READ rather than merely assigned.
      this.log.info("SDK run completed", {
        run_id: ctx.runId,
        branch: ctx.branch,
        agent_source: selection.source,
        js_deps: depsResults.map((r) => ({ dir: r.dir, ok: r.ok })),
      });
      // toolEnv (PRD #46 M9): the allowlisted provisioned tool env, so the self_improve
      // check runner can put the run's provisioned toolchains on its subprocess PATH.
      const result: ExecutorResult = {
        branch: ctx.branch,
        agentSelection: { source: selection.source, agents: selectedNames },
        toolEnv,
      };
      // PRD #72 M4: forward the declared PRD path on `issue` runs only, clamped to
      // length. TRANSPORT HYGIENE ONLY — no path-shape checks here. The api owns
      // the grammar (api/internal/prdpath), and a second implementation of it would
      // drift silently in both directions.
      //
      // The key is OMITTED, never set to undefined: `runner.ts` spreads this into
      // the state report, and an own `prdDonePath: undefined` would both change the
      // shape every existing deepStrictEqual on this result asserts and blur the
      // wire distinction between "declared nothing" and "field present but empty".
      if (isIssueRun && declaredPrdPath !== undefined) {
        result.prdDonePath = declaredPrdPath.slice(0, PRD_DONE_PATH_MAX_LEN);
      }
      // PRD #265 M1: forward the declared finished-milestone ids on `issue` runs only,
      // for the same reasons the PRD path is gated and OMITTED-not-undefined above. The
      // ids are transport-hygiene-parsed already (parseProgressIds); the api re-validates
      // membership against the frozen list and unions them into milestones_completed at
      // completion, so a non-issue run — which has no frozen list — never carries them.
      if (isIssueRun && declaredMilestonesCompleted !== undefined) {
        result.milestonesCompleted = declaredMilestonesCompleted;
      }
      // Issue #293 M2: carry the dirs whose deps did not install so the MR can be
      // annotated honestly (a component whose deps are absent could not have run its
      // gates). Reuses the js_deps `ok` signal, which is corroborated against the
      // filesystem so a false "deps ready" cannot be minted. Dir names are clamped
      // with safeDirLabel (repo-controlled text). OMITTED-not-undefined, issue-run
      // only, same convention as prdDonePath/milestonesCompleted above.
      //
      // EXCLUDE by the specific deliberate-skip REASON, not by `manager`: a dir with a
      // package.json but NO recognized lockfile is `{manager:"none", ok:false,
      // detail:DETAIL_NO_LOCKFILE}` — uzi refuses to guess a package manager, so it was
      // never installed rather than failed, and annotating it would cry wolf on a fine
      // delivery. But `manager:"none"` has a SECOND producer: the belt-to-braces
      // `discovery failed` record (`{dir:".", manager:"none", ok:false}`, js-deps.ts),
      // which IS a genuine total failure that must annotate. Keying the exclusion on
      // `manager !== "none"` dropped both and turned that failure into a false green
      // (latent today: discovery is non-throwing, so the record is unreachable until a
      // refactor makes it throw — fixed here so it stays honest if that day comes).
      // Everything else with ok:false (a real manager that failed/was cancelled, or the
      // discovery failure) annotates.
      const gatesUnverified = depsResults
        .filter((r) => !r.ok && r.detail !== DETAIL_NO_LOCKFILE)
        .map((r) => safeDirLabel(r.dir));
      if (isIssueRun && gatesUnverified.length > 0) {
        result.gatesUnverified = gatesUnverified;
      }
      // Issue #293 M2 / review F1: discovery can stop at MAX_PROJECT_DIRS / MAX_SCAN_DIRS
      // (depsTruncated), leaving components past the cap NEVER scanned and so ABSENT from
      // depsResults — their gates could not have run either, but gatesUnverified cannot
      // name them. Carry the flag so the MR annotation says coverage was capped; a silent
      // cap reading as full coverage is the exact lie this PRD exists to remove.
      if (isIssueRun && depsTruncated) {
        result.gatesDiscoveryTruncated = true;
      }
      // issue #279: forward the report-only declaration + captured summary on `issue` runs
      // only, with the SAME OMITTED-not-undefined discipline as prdDonePath above — the keys
      // are absent unless the lead actually declared them, so an ordinary push+MR completion
      // is byte-identical to before and every existing deepStrictEqual on this result holds.
      if (isIssueRun && declaredReportOnly) result.reportOnly = true;
      if (isIssueRun && declaredSummary !== undefined) {
        result.summary = declaredSummary;
      }
      return result;
    } finally {
      this.disarmWall(state);
      if (ctx.signal) ctx.signal.removeEventListener("abort", onSignal);
      // PRD #121 M2: no install may outlive the run. On every path that never reached
      // the join — plan rejected, cancelled, no plan submitted, ci_fix not_code — the
      // install may still be in flight, and the runner tears the clone down and pushes
      // with the PAT the moment this returns. Abort FIRST so the await is bounded by a
      // kill rather than by the install's own 10-minute cap (that is what stops a
      // rejected plan blocking on deps nobody needs); the promise already carries its
      // own catch, so awaiting it can never throw here and mask the real failure.
      depsAbort.abort();
      await depsInstall;
      // Reap every agent subprocess before returning, so none survives into the
      // worker's PAT-bearing push (B1). Covers the failure/cancel/no-plan paths
      // too, not just the runner's explicit pre-push call.
      this.killAgentTree();
      // Remove the per-run provisioning dir (the synthesized devbox.json + profile
      // symlinks). The nix STORE is global (on the data volume), NOT here, so this
      // never evicts the warm-start cache. Best-effort.
      if (provisionDir)
        await fs
          .rm(provisionDir, { recursive: true, force: true })
          .catch(() => undefined);
    }
  }

  /**
   * Start provisioning the clone's JS dependencies (PRD #121 M2), returning a promise
   * that NEVER rejects.
   *
   * The `.catch` is attached HERE, at creation, not at the join — between the two the
   * promise is floating, and a rejection reaching an empty microtask queue is an
   * unhandled rejection that kills the worker process. `installJsDeps` is contracted
   * never to throw, but that contract belongs to the module; this call site must not
   * depend on it holding.
   *
   * ENV: the same scrubbed REPLACEMENT env the self-improve checks use (`buildCheckEnv`)
   * — never a `process.env` spread. The install executes repo-authored package.json /
   * lockfile resolution, so the worker's join token, API URL, forge PAT and OAuth token
   * are absent by construction; PATH comes from the run's provisioned toolEnv so the
   * install uses the RUN's node/npm. HOME is the PER-RUN SDK home, matching what
   * runner.ts already passes for the self-improve checks. Per-run and not the shared
   * provisioning HOME on purpose: a shared HOME would warm the npm cache across runs,
   * but every run's install writes it under the same `runner` uid, so one run could seed
   * content a later run installs. A cold cache per run is the cheaper side of that
   * trade, and the run's HOME is torn down with the run.
   */
  private startDepsInstall(
    ctx: RunContext,
    toolEnv: Record<string, string> | undefined,
    signal: AbortSignal,
  ): Promise<JsDepsInstall> {
    ctx.emit({
      kind: "status",
      agent: "worker",
      payload: {
        text: "installing the repo's JS dependencies (in the background)",
      },
    });
    return this.installDeps(
      ctx.worktreePath,
      buildCheckEnv(process.env, this.homeDir, toolEnv),
      { signal },
    ).catch((err: unknown) => {
      // Best-effort, always: provisioning can never fail the run. Unlike
      // provisionRunTools (whose failure DOES fail the run — the agent would be
      // missing its declared toolchain), a missing node_modules degrades to the
      // agent installing them itself, exactly as it does today.
      this.log.warn("JS dependency provisioning failed", {
        run_id: ctx.runId,
        error: errMessage(err),
      });
      return { results: [], truncated: false };
    });
  }

  /**
   * Wait for the dependency install and report what it did on the run's feed. Never
   * throws: the promise carries its own catch, and a provisioning result — however bad —
   * is information for the user, not a run failure.
   */
  private async joinDepsInstall(
    ctx: RunContext,
    depsInstall: Promise<JsDepsInstall>,
  ): Promise<JsDepsInstall> {
    const { results, truncated } = await depsInstall;
    if (results.length === 0) {
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: { text: "no JS dependencies to install (no lockfile found)" },
      });
      return { results, truncated };
    }
    // One line, naming every dir and — for anything that did not install — why. A
    // silent skip here resurfaces later as an inexplicable `vitest: not found`.
    const installed = results
      .filter((r) => r.ok)
      .map((r) => safeDirLabel(r.dir));
    const skipped = results.filter((r) => !r.ok);
    const parts: string[] = [];
    if (installed.length > 0)
      parts.push(`installed JS dependencies in ${installed.join(", ")}`);
    for (const s of skipped) parts.push(`${safeDirLabel(s.dir)}: ${s.detail}`);
    // Truncation goes on the FEED, not just in a log: without it the line above reads as
    // full coverage, and a `vitest: not found` in dir 13 becomes unexplainable.
    if (truncated)
      parts.push(
        "discovery hit its directory bound — some project dirs were not installed",
      );
    ctx.emit({
      kind: "status",
      agent: "worker",
      payload: { text: parts.join(" — ") },
    });
    this.log.info("JS dependency provisioning", {
      run_id: ctx.runId,
      results,
      truncated,
    });
    return { results, truncated };
  }

  /** Drive ONE SDK turn to its result frame, capturing signals + the session id. */
  /**
   * PRD #88 M4: drive a PLANNING turn, letting the lead ask the human first.
   *
   * Wraps both planning-turn sites — the first plan turn and #41's revision turn —
   * because both end in `if (plan === undefined) throw REASON_NO_PLAN` and a pre-run
   * question ends a planning turn with questions and NO plan. Without this, the
   * feature's own pre-run path hard-fails the run it was meant to clarify. Fixing only
   * the first site would leave a revision turn that asks a question failing exactly
   * that way, which is the harder of the two to reproduce.
   *
   * The contract narrows rather than loosens: "no plan AND no questions" is still
   * REASON_NO_PLAN, so a lead that simply ends its turn without planning still fails
   * closed and un-gated work is still never pushed.
   *
   * On an answer the SAME session is resumed and re-planned, so the lead keeps its
   * full planning context and nothing is replayed. It eventually reaches submit_plan
   * and the normal plan gate; M4 adds no second gate.
   */
  private async drivePlanningTurn(
    ctx: RunContext,
    options: SdkOptions,
    resumeId: string | undefined,
    prompt: string,
    state: RunDrive,
    idleMs: number,
    budget: { asked: number },
  ): Promise<TurnResult & { plan: string }> {
    let turnPrompt = prompt;
    // Bounded independently of the park cap. The cap bounds how many times we ASK;
    // this bounds how many times we re-plan, which also covers the rounds where the
    // question could NOT be asked (cap spent, or askUser unwired) and the lead was
    // told to proceed. Without it a lead that answers every answer with another
    // question would loop here forever, never planning and never failing.
    const maxRounds = questionMax(ctx.config ?? null) + 1;
    for (let round = 0; ; round++) {
      const turn = await this.driveTurn(
        ctx,
        options,
        resumeId,
        turnPrompt,
        state,
        idleMs,
      );
      resumeId = turn.sessionId ?? resumeId;
      if (turn.plan !== undefined)
        return { ...turn, sessionId: resumeId, plan: turn.plan };

      const asked = turn.questions;
      // The unchanged contract: a planning turn that neither planned nor asked is the
      // original error. Never push un-gated work, even if the lead signalled done.
      if (!asked?.length) throw new Error(REASON_NO_PLAN);
      if (round >= maxRounds)
        throw new Error(REASON_NO_PLAN_AFTER_CLARIFICATION);

      const verdict = await this.askUserOrContinue(ctx, asked, budget.asked);
      if (verdict.cancelled) throw new Error(REASON_CANCELLED);
      if (verdict.parked) budget.asked++;
      turnPrompt = buildPlanAfterAnswerPrompt(verdict.followUp);
    }
  }

  /**
   * PRD #88 M1: run one clarification park, or explain why it did not happen.
   *
   * Returns what the next turn should be told. Three non-park outcomes, all of which
   * continue the run rather than ending it:
   *
   *  - No ctx.askUser wired ⇒ FAIL OPEN (a feed notice, proceed). The opposite of the
   *    plan gate, which throws when ctx.gatePlan is missing. An ungated plan would
   *    push unreviewed work; an unaskable question only loses a clarification, and
   *    failing closed would kill runs on any executor that never wired it.
   *  - The per-run cap is spent ⇒ notice, proceed. Without this a lead that answers
   *    every answer with another question would park forever, one deadline at a time.
   *  - A park that resolves with `cancel` ⇒ reported so the caller can abort.
   */
  private async askUserOrContinue(
    ctx: RunContext,
    questions: AskUserQuestion[],
    questionsAsked: number,
  ): Promise<{ parked: boolean; cancelled?: boolean; followUp: string }> {
    const headers = questions.map((q) => q.header || q.question).join("; ");
    if (!ctx.askUser) {
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: { text: QUESTION_UNWIRED_NOTICE },
      });
      return { parked: false, followUp: unansweredPrompt(questions) };
    }
    const cap = questionMax(ctx.config ?? null);
    if (questionsAsked >= cap) {
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `${QUESTION_CAP_NOTICE} (limit ${cap}). Asked: ${headers}`,
        },
      });
      return { parked: false, followUp: unansweredPrompt(questions) };
    }
    const verdict = await ctx.askUser(questions);
    if (verdict.kind === "cancel")
      return { parked: true, cancelled: true, followUp: "" };
    ctx.emit({
      kind: "answer",
      agent: "lead",
      payload: { answers: verdict.answers },
    });
    return {
      parked: true,
      followUp: answeredPrompt(questions, verdict.answers),
    };
  }

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
    options.spawnClaudeCodeProcess = (
      spawnOpts: SpawnOptions,
    ): SpawnedProcess => {
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
    // PRD #35. The observer rides the SAME iteration mapSdkMessage already does —
    // rate_limit_event is simply one of the frame types that mapper drops on its
    // default arm, so nothing is buffered and no second pass over the turn exists.
    // `resultFrame` is kept because classification needs the frame ITSELF
    // (terminal_reason lives on it) and not just the subtype the collapse records.
    const rateLimits = new RateLimitObserver();
    let resultFrame: unknown;

    this.armWall(state);
    // Budget already spent by earlier turns → fail now rather than run unbounded.
    if (state.tripReason) throw new Error(state.tripReason);
    try {
      armIdle();
      const queryInstance = this.queryFn({
        prompt: promptStream(prompt),
        options,
      });
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
              this.log.warn("onSessionId handler threw", {
                run_id: ctx.runId,
                error: errMessage(err),
              });
            }
          }
        }

        // Emit everything EXCEPT the signal tool_use blocks — the plan is
        // surfaced as a `plan` message by the runner, not duplicated as a raw
        // tool_use payload, and signal_done is infra noise. An assistant frame's
        // per-call usage (PRD #40 Decision 11) rides the FIRST message that
        // survives that filter — attached HERE, not in mapAssistant, which cannot
        // see this executor-side drop; every phase terminates on a signal frame, so
        // attaching earlier would systematically lose the lead's terminating-frame
        // usage. A frame whose messages are ALL filtered loses its usage (accepted).
        // Result-frame usage travels inside mapResult's payload instead, so it is
        // never re-attached here (assistantUsageOf is assistant-only, not results).
        // PRD #99: alarm on a frame that carries an invocation id but no role
        // field. Logged HERE rather than in the mapper because sdk-messages.ts is
        // a pure module with no logger, and this is the only executor that can
        // produce subagent frames at all (chat-executor and judge-runner both
        // prevent the Agent tool — chat via disallowedTools, judge via a deny-all
        // PreToolUse hook). The `kind` is the ONLY thing logged — no id, no label —
        // so nothing new reaches `docker logs`, keeping the deliberate omission at
        // batcher.ts's debug line intact.
        // PRD #35: feed the limit observer before anything can `continue` past it.
        // Placed at the top of the per-frame body deliberately — a rate_limit_event
        // maps to zero run messages, so any placement inside the mapSdkMessage loop
        // below would never see one.
        rateLimits.observe(msg);
        const orphanKind = orphanInstanceKind(msg);
        if (orphanKind !== undefined) {
          this.log.warn(
            "frame carried parent_tool_use_id without subagent_type",
            {
              run_id: ctx.runId,
              kind: orphanKind,
            },
          );
        }
        const frameUsage = assistantUsageOf(msg);
        const frameModel = assistantModelOf(msg);
        let usageAttached = false;
        for (const em of mapSdkMessage(msg)) {
          if (em.kind === "tool_use" && isSignalToolName(em.payload["name"]))
            continue;
          if (frameUsage && !usageAttached) {
            em.payload["usage"] = frameUsage;
            // PRD #93 Decision 2: `model` is CO-GATED with usage — same surviving
            // message, same latch. A model is recorded only where that agent's
            // tokens are, so the web derive (which reads it inside its existing
            // `"usage" in payload` branch) can never produce a zero-token agent row
            // from a model-only frame. No usage ⇒ no model, deliberately.
            if (frameModel !== undefined) em.payload["model"] = frameModel;
            usageAttached = true;
          }
          ctx.emit(em);
        }
        const sig = scanSignals(msg);
        if (sig.plan !== undefined) result.plan = sig.plan;
        if (sig.milestones) result.milestones = sig.milestones; // last-wins, alongside plan (PRD #122 M1)
        if (sig.progress) result.progress = sig.progress; // last-wins (PRD #122 M2)
        if (sig.done) result.done = true;
        if (sig.checkpoint) result.checkpoint = true; // latch, like `done` (PRD #122 M6)
        // Last-wins within the turn, mirroring `plan` rather than `done`'s latch:
        // if the lead somehow signals twice, the LAST declaration is the one that
        // describes the tree the worker is about to push (PRD #72 M4).
        if (sig.prdDonePath !== undefined) result.prdDonePath = sig.prdDonePath;
        // PRD #265 M1: last-wins within the turn, mirroring prdDonePath — if the lead
        // signals twice, the LAST declaration describes the finished set the worker is
        // about to reconcile at completion.
        if (sig.milestonesCompleted !== undefined)
          result.milestonesCompleted = sig.milestonesCompleted;
        // issue #279: the summary is last-wins within the turn (like prdDonePath), and
        // report_only latches true (like `done`) — once the lead declares the run
        // report-only it stays report-only through the terminating turn's break.
        if (sig.summary !== undefined) result.summary = sig.summary;
        if (sig.reportOnly) result.reportOnly = true;
        // PRD #88: accumulate across the turn rather than last-wins. A lead told to
        // batch its questions into one call normally makes exactly one, but if it
        // makes two we must ask both — dropping the earlier one would park the run on
        // a question the lead is no longer waiting on.
        if (sig.questions?.length)
          result.questions = [...(result.questions ?? []), ...sig.questions];

        if (isResult(msg)) {
          resultFrame = msg;
          if (isErrorResult(msg)) {
            sawErrorResult = true;
            errorSubtype =
              ((msg as { subtype?: unknown }).subtype as string) ?? "unknown";
          }
          // The turn is done. Abort so a lingering background bash the agent left
          // running can't pin the iterator open (bottega's pattern).
          abortController.abort();
          break;
        }
      }

      if (state.tripReason) throw new Error(state.tripReason);
      if (sawErrorResult) {
        // PRD #35: a usage-limit death is thrown as a TYPED error carrying the
        // normalized reset, so the runner branches on the type rather than on
        // message text. Everything else keeps today's collapse verbatim.
        //
        // The trip check above still wins: a cancel or watchdog that fired during a
        // limit-shaped turn is a deliberate stop, not something to park and resume.
        const limit = classifyLimitFailure(
          resultFrame,
          rateLimits.latest,
          Date.now(),
        );
        if (limit)
          throw new LimitReachedError({ ...limit, detail: errorSubtype });
        throw new Error(`agent run failed: ${errorSubtype}`);
      }
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
    state.wallTimer = setTimeout(
      () => this.trip(state, REASON_WALL),
      state.wallRemainingMs,
    );
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
  yield {
    type: "user",
    message: { role: "user", content: text },
    parent_tool_use_id: null,
  };
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

/**
 * PRD #41: the worker-side plan-revision cap from the claim config. The server sends
 * `plan_max_revisions` in the claim config (workersvc claim.go) and ALSO enforces it
 * at submit time (rejects the 4th revise), so this worker counter is a belt-and-
 * suspenders guard. An explicit 0 (operator DISABLING revisions) is respected as 0 —
 * only an absent/undefined or negative/garbage value falls back to DEFAULT_MAX_REVISIONS,
 * so the worker counter mirrors operator intent (the server gates first, so this is
 * harmless today, but must not silently re-enable revisions a config disabled).
 */
function planMaxRevisionsOf(config: ClaimConfig | null | undefined): number {
  const v = config?.plan_max_revisions;
  if (typeof v === "number" && Number.isFinite(v) && v >= 0)
    return Math.floor(v);
  return DEFAULT_MAX_REVISIONS;
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
export function resolveLeadModel(
  configModel?: string,
  templateModel?: string,
): string | undefined {
  return configModel || templateModel || undefined;
}

/**
 * PRD #209 (M2 validation): whether to embed the seeded plan BODY in the FIRST implement
 * prompt. True only for a session-less seeded pre-approved run — the row-2 cold start, and
 * a requeued seeded run whose transcript was dropped — which has no planning turn and no
 * resumable session carrying the plan. A seeded RESUME already has the plan in its session,
 * and a non-preApproved run never gets here.
 *
 * The `seeded` term is DEFENSE IN DEPTH and today REDUNDANT: `preApproved` already implies
 * `(session || seeded)`, so `preApproved && !session` implies `seeded`. It is kept — and
 * this predicate is EXTRACTED from run() — precisely so the currently-unreachable
 * `{preApproved, !seeded, !session}` combination is testable independently of how
 * `preApproved` is derived. No executor-driven test can observe the term's removal (that
 * state cannot occur through the real `preApproved`); a direct unit test on this function
 * can, so if a future change ever decoupled `preApproved` from `(session || seeded)` the
 * guard that refuses to hand a NON-seeded run an authoritative <plan> block stays pinned.
 */
export function embedSeededPlan(args: {
  preApproved: boolean;
  seeded: boolean;
  hasSession: boolean;
}): boolean {
  return args.preApproved && args.seeded && !args.hasSession;
}

/**
 * Render a discovered directory name for the run's activity feed. `dir` comes from
 * `readdir`, i.e. it is REPO-CONTROLLED text: a repo can commit a directory whose name
 * contains newlines, backticks, or instruction-shaped prose, and this string is
 * persisted to `run_messages` and rendered to a human. Not a path escape and React
 * escapes the HTML, but untrusted text should not be able to shape a status line, so the
 * charset is clamped to what a real project dir needs and the length is bounded.
 */
function safeDirLabel(dir: string): string {
  // 120 chars: this is a rendered feed line, where a long-but-real directory name is
  // more useful than a short one, and React escapes the output — the clamp here is
  // cosmetic plus defence in depth. The PROMPT clamp is load-bearing and uses a
  // tighter bound; both share the charset deliberately (clampToDirCharset).
  return clampToDirCharset(dir, 120);
}
