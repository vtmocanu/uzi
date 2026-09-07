import { randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import type { WorkerClient } from "./client.js";
import { RequestError } from "./client.js";
import type { GitCache, RunnerClone, CheckpointOverlayContext } from "./git.js";
import {
  gitBasicCredential,
  isNonFastForwardRejection,
  isPushProtectionRejection,
  isWorkflowScopeRejection,
} from "./git.js";
import type { SecretFinding } from "./secret-scan-guard.js";
import type { Executor, ExecutorResult, RunContext } from "./executor.js";
import { PlanRejectedError } from "./executor.js";
import { skillsPluginDir } from "./skills-plugin.js";
import { describeLimit, LimitReachedError } from "./limit.js";
import type { Logger } from "./log.js";
import type {
  AgentSelection,
  AgentSource,
  AgentTemplate,
  AskUserQuestion,
  ClaimConfig,
  ClaimResponse,
  IterationBudget,
  Milestone,
  StateAck,
  StateRequest,
} from "./protocol.js";
import { resolveAgentSelection } from "./protocol.js";
import { resolveRunKind, RUN_KIND_PROFILES } from "./run-kind.js";
import {
  describeRepoAgentNote,
  detectRepoAgents,
  repoAgentSummaries,
  type DetectedRepoAgents,
} from "./repoagents.js";
import { MessageBatcher } from "./batcher.js";
import { rmTreeForce } from "./rmtree.js";
import {
  SteeringChannel,
  type AnswerVerdict,
  type PlanVerdict,
} from "./steering.js";
import { GitLabClient, ForgejoClient, GitHubClient, type ForgeClient } from "./forge.js";
import { withForgeRetry } from "./forge-retry.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { sessionTranscriptResolvable } from "./sdk-session.js";
import { errMessage, RUN_ID_RE } from "./util.js";
import {
  buildCheckEnv,
  defaultCheckRunner,
  flagGuardPaths,
  guardCriticalMrSection,
  runSelfImproveChecks,
  selfImproveMrSection,
  type CheckRunner,
} from "./self-improve.js";
import { installJsDeps } from "./js-deps.js";
import { detectToolchain, type ToolchainDetection } from "./toolchain-detect.js";
import { isCIConfigPlan } from "./prompt.js";
import { flagCIConfigPaths, DEFAULT_CI_CONFIG_PATHS } from "./ci-config-guard.js";
import { REASON_PROVISION_FAILED } from "./provision-run.js";
import { REASON_NO_TOKEN } from "./sdk-executor.js";

/** Cap on a reported failure_reason, matching the forge error-body cap
 *  (forge.ts) so a runaway SDK error can't bloat the run row or the stream. */
const MAX_FAILURE_REASON_LEN = 512;

/** PRD #974 follow-up (#1077): a terminal push_secret_blocked report whose reportState
 *  exhausted its bounded retries and threw. Carrying the typed origin + safe reason through
 *  execute()'s generic catch preserves fail_origin=push_secret_blocked (instead of defaulting
 *  to agent_failure) WITHOUT ever attaching preserved_patch. */
class TerminalReportError extends Error {
  constructor(
    readonly reason: string,
    readonly failOrigin: string,
    cause?: unknown,
  ) {
    super("terminal state report failed", { cause });
    this.name = "TerminalReportError";
  }
}

/** Map a known failure-reason CONSTANT to the server's fail_origin enum (PRD #69
 *  M7a). Authored WORKER-SIDE from the reason constant the throw site used — it never
 *  parses free text: it matches only the fixed prefixes the two fatal pre-start
 *  throwers emit (provision-run appends `: <detail>` after REASON_PROVISION_FAILED;
 *  sdk-executor throws REASON_NO_TOKEN verbatim). Ordinary agent failures return
 *  undefined, so `fail_origin` is omitted and the server defaults them to
 *  'agent_failure'. Sent unvalidated; the server allowlists it. */
export function failOriginForReason(rawReason: string): string | undefined {
  if (rawReason.startsWith(REASON_PROVISION_FAILED)) return "provisioning_failed";
  if (rawReason.startsWith(REASON_NO_TOKEN)) return "credential_unavailable";
  return undefined;
}

/**
 * PRD #377 M1 — compose the actionable `failure_reason` for a GitHub run whose branch
 * touches `.github/workflows/**`, a path the bot's repo-only PAT cannot push. It names the
 * offending path(s) and points at `docs/github-bot-setup.md`.
 *
 * The path LIST is truncated to fit MAX_FAILURE_REASON_LEN (showing the first paths + an
 * "and N more" tail) — but the doc link, which lives in the fixed suffix, is NEVER cut. The
 * truncation math is done BEFORE assembly (against the budget left after the fixed prefix +
 * suffix), not by blindly slicing the whole string at the end. Exported for a direct
 * truncation unit test. The caller still applies `.slice(0, MAX_FAILURE_REASON_LEN)` as a
 * belt-and-braces net after the doc link is guaranteed to fit.
 */
export function composeWorkflowScopeReason(paths: string[]): string {
  const prefix =
    "This run's branch changes workflow files that uzi's GitHub bot token cannot push " +
    "(its scope is exactly `repo`, without `workflow`, by design): ";
  const suffix =
    ". The change is valid; land it as a human PR (commit the file yourself with a " +
    "workflow-scoped token). See docs/github-bot-setup.md. Your diff is preserved below.";
  const budget = MAX_FAILURE_REASON_LEN - prefix.length - suffix.length;
  let list = paths.join(", ");
  if (list.length > budget) {
    // Drop trailing paths (replaced by an "and N more" tail) until the list fits the
    // budget. The doc link is in `suffix`, so it is untouched by this truncation.
    let shown = paths.length;
    for (; shown > 0; shown--) {
      const more = paths.length - shown;
      const candidate =
        paths.slice(0, shown).join(", ") + (more > 0 ? `, and ${more} more` : "");
      if (candidate.length <= budget) {
        list = candidate;
        break;
      }
    }
    if (shown === 0) {
      // Pathological: even one path overflows the budget (a single very long path). Keep a
      // hard-truncated first entry so the fixed suffix — and its doc link — still fits.
      list = paths[0]!.slice(0, Math.max(0, budget - 1)) + "…";
    }
  }
  return prefix + list + suffix;
}

/**
 * PRD #974 M2 — compose the actionable, capped `failure_reason` for a GitHub run whose branch
 * carries a secret GitHub Push Protection (GH013) would reject at push. It NAMES the offending
 * commit + path(s) (the first finding, plus an "and N more" tail like composeWorkflowScopeReason)
 * so a human knows exactly what to scrub, says the branch could not be pushed, and points out
 * that the diff is preserved for a human to scrub-and-land.
 *
 * The variable part (the finding list) is truncated to fit MAX_FAILURE_REASON_LEN against the
 * budget left after the fixed prefix + suffix — the "diff is preserved" pointer in the suffix is
 * NEVER cut. The truncation math is done BEFORE assembly (mirroring composeWorkflowScopeReason),
 * not by slicing the whole string at the end. Exported for a direct cap unit test; the caller
 * still applies `.slice(0, MAX_FAILURE_REASON_LEN)` as a belt-and-braces net.
 */
export function composePushSecretBlockedReason(findings: SecretFinding[]): string {
  const prefix =
    "This run's branch could not be pushed: it carries a secret GitHub Push Protection blocks " +
    "(GH013). Offending: ";
  const suffix =
    ". The change is otherwise valid; a human can scrub the secret from the commit(s) and land " +
    "it. Your diff is preserved below.";
  const budget = MAX_FAILURE_REASON_LEN - prefix.length - suffix.length;
  // One human-readable label per finding: `<short-commit> <path> (<rule>)`. The file path (and
  // rule id) come from gitleaks' report of ATTACKER-authored repo content, so a committed
  // filename can carry control bytes (ESC, newline) that would forge rows / inject ANSI when this
  // failure_reason is later rendered in a CLI/TUI terminal. Strip the C0 control range + DEL at
  // this WRITE site so the stored reason cannot carry them (the render boundary is defense in
  // depth, not the only guard).
  // eslint-disable-next-line no-control-regex
  const stripControl = (s: string): string => s.replace(/[\u0000-\u001f\u007f]/g, "");
  const labels = findings.map((f) => {
    const shortCommit = f.commit ? stripControl(f.commit.slice(0, 8)) : "?";
    const rule = f.ruleId ? ` (${stripControl(f.ruleId)})` : "";
    return `${shortCommit} ${stripControl(f.file)}:${f.startLine}${rule}`;
  });
  let list = labels.join("; ");
  if (list.length > budget) {
    // Drop trailing labels (replaced by an "and N more" tail) until the list fits the budget.
    let shown = labels.length;
    for (; shown > 0; shown--) {
      const more = labels.length - shown;
      const candidate =
        labels.slice(0, shown).join("; ") + (more > 0 ? `; and ${more} more` : "");
      if (candidate.length <= budget) {
        list = candidate;
        break;
      }
    }
    if (shown === 0) {
      // Pathological: even one label overflows the budget. Keep a hard-truncated first label so
      // the fixed suffix (and its "diff is preserved" pointer) still fits.
      list = (labels[0] ?? "").slice(0, Math.max(0, budget - 1)) + "…";
    }
  }
  return prefix + list + suffix;
}

/**
 * PRD #456 M2 — the actionable `failure_reason` for a GitHub run whose branch could not be
 * aligned with the current default branch before the finalize push: its `.github/workflows/**`
 * files are BEHIND the default (main advanced them after this run's clone base), the bot's
 * repo-only PAT cannot push while they differ, and uzi's attempt to merge and then rebase the
 * current default into the branch either BOTH conflicted, or the aligned branch could not be
 * pushed without rewriting already-published history (a non-fast-forward the bot cannot
 * force-push — NB2). The run fails without pushing and the agent's diff is preserved (#377's
 * `preserved_patch`) for a human to rebase-and-land.
 *
 * Names the default branch once and points at docs/github-bot-setup.md. The branch name is
 * the only variable part and is clamped against a computed budget (MAX_FAILURE_REASON_LEN
 * minus the fixed prefix + suffix lengths), so the fixed suffix — the doc link and the
 * "Your diff is preserved below." pointer — always fits MAX_FAILURE_REASON_LEN and is never
 * truncated. Exported for a direct length-cap unit test.
 */
export function composeBaseAlignConflictReason(defaultBranch: string): string {
  const db = defaultBranch || "the default branch";
  const prefix = "This run's branch is behind the default branch (";
  const suffix =
    ") on .github/workflows files, which uzi's GitHub bot token cannot push while they " +
    "differ from the default (its scope is `repo`, without `workflow`, by design). uzi tried " +
    "to merge then rebase the current default into the branch to realign those files, but could " +
    "not realign and safely push it, so the run failed without pushing. The work is valid; a " +
    "human can rebase and land it. See docs/github-bot-setup.md. Your diff is preserved below.";
  // Clamp the branch name (the only variable part) against the budget left after the fixed
  // prefix + suffix, so the doc link + preserved-diff pointer in `suffix` always survive.
  const budget = MAX_FAILURE_REASON_LEN - prefix.length - suffix.length;
  const branch = db.length > budget ? db.slice(0, Math.max(0, budget - 1)) + "…" : db;
  // Belt-and-braces final net, mirroring composeWorkflowScopeReason's caller.
  return (prefix + branch + suffix).slice(0, MAX_FAILURE_REASON_LEN);
}

/**
 * Thrown when a SEEDED run's clone was cut from a commit that diverges from the one the
 * user planned against AND the run was created with --require-base (PRD #209 M4, Open
 * Question 3). The runner catches it on the generic failure path and reports `failed`
 * with this message, which names both commits. Without --require-base a divergence only
 * warns into the feed and the run implements anyway. The message carries commit SHAs
 * only, never a secret.
 */
export class BaseCommitDivergedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "BaseCommitDivergedError";
  }
}

/**
 * Whether a planned-against commit and the clone's resolved base name the SAME commit,
 * tolerant of git's abbreviated SHAs (PRD #209 M4): a match when either is a prefix of
 * the other, compared case-insensitively. Both sides must be non-empty — an empty side is
 * never a match, so a missing base never reads as "matches everything". Equality is the
 * degenerate prefix case, so it is covered without a special branch.
 */
export function baseCommitsMatch(planned: string, actual: string): boolean {
  const a = planned.trim().toLowerCase();
  const b = actual.trim().toLowerCase();
  if (a === "" || b === "") return false;
  return a.startsWith(b) || b.startsWith(a);
}

/**
 * Evaluate a seeded run's base-commit staleness (PRD #209 M4). It is the whole staleness
 * decision, factored out of the runner so the fail path is directly testable by TYPE (the
 * runner swallows the throw on its generic failure path, so a runner-level test can only
 * see the resulting `failed` state, not the error class). Returns:
 *   - `undefined` when there is no planned commit, or it matches the clone's base
 *     (prefix-tolerant) — the caller proceeds silently;
 *   - a warning STRING naming both commits when they diverge and `requireBase` is false —
 *     the caller emits it to the feed and implements anyway;
 * and THROWS BaseCommitDivergedError when they diverge and `requireBase` is true, so the
 * caller lets it reach the run-failure path and never implements against a diverged base.
 */
export function evaluateBaseStaleness(
  plannedBase: string | undefined,
  actualBase: string,
  requireBase: boolean,
): string | undefined {
  if (!plannedBase || baseCommitsMatch(plannedBase, actualBase)) return undefined;
  const detail =
    `the seeded plan was written against commit ${plannedBase}, but this clone's ` +
    `base commit is ${actualBase}`;
  if (requireBase) {
    throw new BaseCommitDivergedError(
      `${detail}; --require-base is set, so this run will not implement against a diverged base`,
    );
  }
  return (
    `${detail}; the plan may reference files that have moved since it was written — ` +
    `implementing anyway (create the run with --require-base to stop instead)`
  );
}

/**
 * What the per-run executor factory yields for one execution (PRD #42 Decisions
 * 4/5): a freshly-constructed executor plus the per-run HOME to clean on terminal.
 *  - `executor`: built anew for THIS run, so `SdkExecutor.spawnedPids` and
 *    `killAgentTree` are private to it — two concurrent runs can never wipe or kill
 *    each other's subprocess tree (the B1 pre-push reap). Serial runs shared one
 *    instance before; that latent hazard is closed by construction here.
 *  - `homeDir`: the run's private HOME (`agent-home/<runId>`), removed by the runner
 *    when the run reaches a terminal state. Optional so a test/stub factory that owns
 *    its HOME (or needs none) yields undefined and the runner skips the cleanup.
 */
export interface RunExecution {
  executor: Executor;
  homeDir?: string;
}

/** Build a per-execution executor for a run id (called once per `execute`). */
export type ExecutorFactory = (runId: string) => RunExecution;

/**
 * PRD #218 M1 — the worker-shutdown registry entry for one in-flight run. A graceful
 * SIGTERM/SIGINT (`Runner.shutdown`) must fetch each running run's committed work back
 * into the worker bare before the container is killed, so the sweeper requeues a run
 * whose TREE is safe rather than one whose work was destroyed on the next re-claim.
 *  - `cancel`: the run's own per-run AbortController (the executor watches its signal),
 *    so shutdown aborts the SDK turn the same way a steering cancel does.
 *  - `shuttingDown`: the DISCRIMINATOR. Only a shutdown sets it; a user steering-cancel
 *    aborts the same controller with the same error, so the flag — never the error — is
 *    what routes a run to the fetch-back-and-requeue branch instead of today's failure.
 */
interface ActiveRun {
  cancel: AbortController;
  shuttingDown: boolean;
}

/**
 * PRD #949 M2 — the per-run state carrier for RunRunner.execute(). Holds the
 * cross-phase state the extracted phase methods, the (still-inline) back half, and
 * the catch/finally read: the const collaborators built in the prologue plus the
 * mutables the phases fill in (barePath/worktreePath/branch/runnerClone/result and
 * the park/shutdown flags). Unexported on purpose — an exported-but-unused carrier
 * would redden knip (deadcode:agent).
 */
interface RunFlight {
  readonly runId: string;
  readonly executor: Executor;
  readonly runHome: string | undefined;
  readonly runScopedSecrets: string[];
  readonly runLog: Logger;
  readonly redact: ReturnType<typeof makeRedactor>;
  readonly redactText: ReturnType<typeof makeTextRedactor>;
  readonly batcher: MessageBatcher;
  readonly cancel: AbortController;
  readonly steering: SteeringChannel;
  readonly reportState: (
    body: Parameters<WorkerClient["reportState"]>[1],
  ) => ReturnType<WorkerClient["reportState"]>;
  observedSessionId: string | undefined;
  barePath: string | undefined;
  worktreePath: string | undefined;
  branch: string | undefined;
  active: ActiveRun | undefined;
  parked: boolean;
  preserveSession: boolean;
  lastPublish: number;
  lastPublishedTip: string | undefined;
  /** PRD #1062 M2 (#1036): the current tip of `refs/uzi-checkpoints/<branch>` as this run last
   *  knows it — seeded from `claim.checkpoint_tip`, advanced to the declared overlay/real tip on
   *  every CONFIRMED publish. Fed to `checkpointPack` as the overlay's `prevCheckpointTip` so a
   *  second sequential overlay carries the prior tip as parent[0] (base-first) and the broker
   *  accepts it as a fast-forward. */
  lastCheckpointRefTip: string | undefined;
  /** issue #1086 (F2 from #1036): the tip of the LAST overlay/real tip we ATTEMPTED to publish
   *  when the publish result was AMBIGUOUS (a thrown error or a non-2xx — the broker may have
   *  ACCEPTED the push before the HTTP ACK was lost). Undefined when no attempt is pending. The
   *  next overlay chains from this (as parent[0]) via `attempted ?? confirmed`, so the chain
   *  reaches the broker's actual ref whether or not the prior push landed; a CONFIRMED publish
   *  clears it. In-memory only — NOT seeded from the claim: `claim.checkpoint_tip` is a
   *  server-confirmed anchor, and a resume self-heals via `lastCheckpointRefTip`. */
  lastAttemptedCheckpointRefTip: string | undefined;
  /** issue #1030: distinct checkpoint-publish failure/skip outcomes already surfaced on
   *  the run feed for THIS run, keyed as `http:<code>` / `skip:<reason>` / `error`. Dedupes
   *  the feed line so the ~20-min time-gated retry of a persistently-failing publish does
   *  not spam the feed. */
  reportedPublishOutcomes: Set<string>;
  runnerClone: RunnerClone | undefined;
  ciFixHumanApproved: boolean;
  result: ExecutorResult | undefined;
}

/** Tuning the runner needs beyond the collaborators (defaults keep M2/M3 tests terse). */
export interface RunnerOptions {
  /** How often the steering channel polls /inputs (default 3s). */
  pollMs?: number;
  /** Plan-approval gate cap; 0 disables (default 24h). */
  planApprovalTimeoutMs?: number;
  /** PRD #88: fallback answer deadline in ms. The claim's question_timeout_seconds
   *  takes precedence; this covers a server that does not send one. */
  questionTimeoutMs?: number;
  /** Injected for tests; default opens real GitLab MRs. The worker picks between
   *  this and `forgejo` per claim (`repo.forge_type`, D9). */
  gitlab?: ForgeClient;
  /** Injected for tests; default opens real Forgejo PRs (PRD #65 D9). */
  forgejo?: ForgeClient;
  /** Injected for tests; default opens real GitHub PRs (PRD #238 D9). */
  github?: ForgeClient;
  /** Injected for tests; default is the real `.claude/agents/` parser (PRD #37).
   *  A seam so a test can drive the detection-failure path deterministically. */
  detectRepoAgents?: (worktreePath: string) => Promise<DetectedRepoAgents>;
  /** Injected for tests; default runs the uzi test suites as real subprocesses. A
   *  self_improve run's MR carries these results as its own evidence (PRD #46). */
  checkRunner?: CheckRunner;
  /** PRD #267: min interval between time-based origin checkpoint publishes on the
   *  reap:false path; 0 disables. Default 20m. */
  checkpointIntervalMs?: number;
  /** PRD #1030 M4: CLIENT-side cap (ms) on the graceful-shutdown durability sequence
   *  (WIP-marker commit + fetch-back + checkpoint publish) so a slow/unreachable forge
   *  cannot hang the shutdown past the k8s termination grace. Default 15s — see the
   *  budget note at the shutdown branch. Injectable so a test can drive the timeout
   *  deterministically against a hanging publish. */
  shutdownPublishTimeoutMs?: number;
  /** Injectable clock for tests; defaults to Date.now. */
  now?: () => number;
  /** Injectable answer-deadline timer for tests; defaults to setTimeout (unref'd)
   *  paired with a clearTimeout canceller. Lets a test arm and OBSERVE the per-run
   *  answer budget deterministically — the per-run-vs-per-question distinction is a
   *  fact about the `remaining` the second park arms — instead of racing a wall-clock
   *  bound that flakes under CPU contention. Returns a canceller. */
  setTimer?: (cb: () => void, ms: number) => () => void;
}

/**
 * Drives one claimed run through the full M4 workflow:
 *   claim → running → seed runner clone → PLAN turn → approval gate → implement⇄
 *   review loop → worker fetches the agent branch back + pushes + opens MR →
 *   completed | failed
 * and always tears the runner clone down (keeping the worker bare clone). Under
 * PRD #51 (b) the agent commits in the runner clone; the worker is bare-only.
 *
 * The plan gate, follow-up injection, and cancel are steered through a single
 * /inputs poller (SteeringChannel); the executor drives the SDK turns and calls
 * back here to gate/report; and — the primary directive — the WORKER (never the
 * agent) performs the authenticated push + MR with the PAT once the agent signals
 * done.
 */
export class RunRunner {
  private readonly pollMs: number;
  private readonly planApprovalTimeoutMs: number;
  /** PRD #88 fallback answer deadline, used when the claim carries none (an older
   *  server). The claim value wins when present — see questionTimeoutMs. */
  private readonly questionTimeoutMs: number;
  private readonly gitlab: ForgeClient;
  private readonly forgejo: ForgeClient;
  private readonly github: ForgeClient;
  private readonly detect: (
    worktreePath: string,
  ) => Promise<DetectedRepoAgents>;
  // Optional test override; production builds it per-run with the scrubbed check env
  // (buildCheckEnv) once the executor's provisioned toolEnv is known (M9).
  private readonly checkRunner?: CheckRunner;
  /** PRD #267: min interval between time-based origin checkpoint publishes on the
   *  reap:false path; 0 disables. */
  private readonly checkpointIntervalMs: number;
  /** PRD #1030 M4: client-side cap (ms) on the graceful-shutdown durability sequence. */
  private readonly shutdownPublishTimeoutMs: number;
  /** PRD #267: injectable clock (defaults to Date.now), so the time-gate is testable.
   *  Also feeds the PRD #88 answer-deadline math (askUser), so that budget is testable
   *  on the same clock. */
  private readonly now: () => number;
  /** PRD #88: injectable answer-deadline timer (defaults to setTimeout/clearTimeout),
   *  so a test can arm and observe the per-run answer budget without a wall-clock race. */
  private readonly setTimer: (cb: () => void, ms: number) => () => void;
  /** PRD #41: absolute plan-approval deadline (epoch ms) per runId, set on the FIRST
   *  gate entry and reused across every revision round so N rounds share ONE budget (not
   *  24h per round). Cleared when the gate resolves terminally (approve/reject/cancel/
   *  timeout) and, defensively, when the run reaches a terminal state. */
  private readonly gateDeadlines = new Map<string, number>();
  /** PRD #88: per-run ABSOLUTE answer deadline, shaped exactly like gateDeadlines —
   *  one budget for the run, not a fresh clock per question, so N questions cannot
   *  extend a parked run indefinitely.
   *
   *  Not durable, and that is worth stating rather than implying: the map is worker
   *  memory, so a worker death re-queues the run and the resuming worker starts a
   *  fresh budget. The honest worst case is QUESTION_TIMEOUT x (RUN_MAX_REQUEUES + 1). */
  private readonly questionDeadlines = new Map<string, number>();
  /** PRD #88: the question id a run is currently parked on. Seeded from the claim on a
   *  resume so a re-park re-uses the SAME id rather than minting a new one — which is
   *  what lets an answer submitted before a worker death still be honoured. */
  private readonly openQuestionIds = new Map<string, string>();
  /** PRD #88: how many times each run has parked, for the LOG LINE only.
   *
   *  Not a guard and not the cap. QUESTION_MAX is enforced in sdk-executor against
   *  its own per-execute counter, and the staleness key is the question id — this
   *  ordinal is never compared to anything. It is deliberately kept off the wire:
   *  a field named for an arrival ordinal, sitting in the question payload, reads as
   *  a staleness discriminator to the next person, and this feature has exactly one
   *  of those. A surface that wants "question 2 of this run" can count `question`
   *  messages in the feed, where the ordinal is a fact rather than a claim. */
  private readonly questionCounts = new Map<string, number>();
  /** PRD #41 (Decision 3): the set of runs that have opened a plan gate at least once.
   *  It distinguishes the FIRST gate (epoch 0 — a verdict already queued when the gate
   *  opens still applies) from a RE-gate after a revision turn (where the epoch must be
   *  advanced so a mid-revision approve of the superseded plan goes stale). Works
   *  regardless of planApprovalTimeoutMs (unlike keying off gateDeadlines). Cleared on a
   *  terminal verdict and, defensively, when the run reaches a terminal state. */
  private readonly gatedRuns = new Set<string>();
  /** PRD #218 M1: the in-flight runs, so a graceful shutdown can abort each and let its
   *  catch fetch the committed work back before the container dies. Registered once the
   *  runner clone exists (there is nothing to fetch back before that) and deregistered
   *  in the terminal finally. */
  private readonly activeRuns = new Map<string, ActiveRun>();
  /** PRD #218 M1: set by `shutdown()`. Read when a run registers so a run that starts
   *  DURING the shutdown drain (a late claim) is aborted immediately rather than running
   *  to completion past the grace window. */
  private shuttingDownGlobal = false;

  constructor(
    private readonly client: WorkerClient,
    private readonly git: GitCache,
    /** Per-execution executor factory (PRD #42 Decision 4). Called once per
     *  `execute` so each run drives its OWN executor instance. */
    private readonly makeExecutor: ExecutorFactory,
    private readonly log: Logger,
    private readonly batchMs: number,
    /** The worker's join token — redacted from message payloads (it lives in
     *  the worker env, reachable via a /proc read of the parent). */
    private readonly joinToken?: string,
    opts: RunnerOptions = {},
  ) {
    this.pollMs = opts.pollMs ?? 3_000;
    this.planApprovalTimeoutMs = opts.planApprovalTimeoutMs ?? 24 * 60 * 60_000;
    this.questionTimeoutMs = opts.questionTimeoutMs ?? 24 * 60 * 60_000;
    this.gitlab = opts.gitlab ?? new GitLabClient();
    this.forgejo = opts.forgejo ?? new ForgejoClient();
    this.github = opts.github ?? new GitHubClient();
    this.detect = opts.detectRepoAgents ?? detectRepoAgents;
    this.checkRunner = opts.checkRunner;
    this.checkpointIntervalMs = opts.checkpointIntervalMs ?? 20 * 60_000;
    this.shutdownPublishTimeoutMs = opts.shutdownPublishTimeoutMs ?? 15_000;
    this.now = opts.now ?? (() => Date.now());
    this.setTimer =
      opts.setTimer ??
      ((cb, ms) => {
        const t = setTimeout(cb, ms);
        t.unref?.();
        return () => clearTimeout(t);
      });
  }

  async execute(claim: ClaimResponse): Promise<void> {
    const runId = claim.run_id;
    // Defense in depth (PRD #42): runId becomes a path segment in the per-run HOME
    // (agent-home/<runId>) which the runner fs.rm's on terminal. It is a
    // server-issued UUID, but reject anything not UUID-shaped BEFORE it reaches a
    // path — an empty id would resolve the per-run HOME back to the SHARED root
    // (removing chat's HOME + every run's HOME on terminal) and a separator/`..`
    // would escape it. Same guard provision-run.ts applies to the provisioning dir.
    // Content-free message: never echo the (rejected) id into a log/failure_reason.
    if (!RUN_ID_RE.test(runId))
      throw new Error("refusing to execute a run with an invalid run id");
    // This run's OWN executor + private HOME (PRD #42 Decisions 4/5), built fresh
    // per execution so nothing subprocess-scoped is shared with a concurrent run.
    const { executor, homeDir: runHome } = this.makeExecutor(runId);
    // Register per-run secrets with the logger so they are scrubbed from any
    // output, then never log the claim payload itself. Tracked in runScopedSecrets
    // and evicted on terminal (Decision 7) so a completed run's PAT/token does not
    // linger in the process-lifetime scrub set; the registry is reference-counted,
    // so evicting these never un-scrubs a still-active sibling run that shares them.
    const gitBasic = gitBasicCredential(
      claim.secrets.forge_pat,
      claim.secrets.forge_username,
    );
    // Defense in depth for gitBasic: the git-over-HTTPS Basic credential
    // (base64(user:pat)) only ever lives in a GIT_CONFIG_VALUE (never argv/logs),
    // but register it too so a future leak through the git env would still be scrubbed.
    const runScopedSecrets = [claim.secrets.forge_pat, gitBasic];
    if (claim.secrets.anthropic_oauth_token)
      runScopedSecrets.push(claim.secrets.anthropic_oauth_token);
    for (const s of runScopedSecrets) this.log.addSecret(s);

    const flight = this.buildFlight(
      claim,
      runId,
      executor,
      runHome,
      gitBasic,
      runScopedSecrets,
    );
    const { runLog, batcher, reportState, redactText, steering } = flight;
    try {
      await this.phaseClone(claim, flight);
      const sessionId = await this.phaseResume(claim, flight);
      await this.phasePreflightHandoff(claim, flight, sessionId);
      await this.phasePublish(claim, flight);
    } catch (err) {
      // PRD #35: a usage-limit death is not an ordinary failure. Handled before the
      // generic path below because that path is terminal in both senses — it reports
      // `failed` and it lets the finally erase the session this run wants to resume from.
      if (err instanceof LimitReachedError) {
        flight.parked = await this.handleLimitReached(
          err,
          claim,
          batcher,
          reportState,
          runLog,
        );
        // parked === true is the ONLY thing that preserves on-disk state; see the
        // carve-out in the finally.
        //
        // PRD #218 M1: fetch the agent's committed work back into the worker bare
        // BEFORE the finally's carve-out — the tracking ref is where the next claim's
        // reseed (M2) reads it from, and it survives the `fs.rm` that the clone does
        // not. Only when the run actually parked (a resume is coming) and a clone
        // existed to fetch from. Best-effort: a park that fails is worse than a park
        // that loses work (D4), so a failed fetch-back must not undo the park.
        if (flight.parked) {
          if (flight.barePath && flight.worktreePath && flight.branch) {
          const barePath = flight.barePath;
          const worktreePath = flight.worktreePath;
          const branch = flight.branch;
          // Belt-and-braces reap before we read the runner-owned clone, matching the
          // done path and the shutdown branch — safe today (the executor's run() finally
          // reaps first) but kept consistent across all three fetch-back sites.
          executor.killAgentTree?.();
          // PRD #759 M1: before the fetch-back, commit any uncommitted work in the
          // runner-owned clone to a clearly-marked THROWAWAY commit (subject-prefixed
          // wip(park):), run as the runner uid in the clone. This is the one thing run
          // #685 lacked — every durability layer below captures COMMITTED commits only,
          // so mid-milestone uncommitted edits were `fs.rm`'d on the next claim. Making a
          // durable marked commit exist means the fetch-back just below carries it to the
          // local tracking ref, and the #628 broker further down publishes it to
          // refs/uzi-checkpoints/<branch>; M2 strips it back to uncommitted at adopt time
          // so it never reaches the MR. commitWipMarker already swallows every error
          // (best-effort — a park that loses work is worse than a missing WIP commit, D4);
          // the .catch is belt-and-braces so nothing on this line can undo the park.
          await this.git.commitWipMarker(worktreePath).catch(() => false);
          await this.fetchBackBestEffort(barePath, worktreePath, branch, runId, runLog);
          // PRD #628 M2: publish a checkpoint to origin on the park path so a DIFFERENT
          // worker re-claiming this limit_wait run can recover the committed tree from
          // refs/uzi-checkpoints/<branch> instead of reseeding off default and
          // re-implementing already-committed milestones. This is a ONE-SHOT publish, not
          // the mid-run time-gate (timeGateOpen is local to the mid-run checkpoint closure;
          // neither it nor lastPublishedTip is read here): publish unconditionally. An empty park is
          // harmless — checkpointPack returns null (no RPC) when the tracking ref is ABSENT,
          // and when it is present-but-unmoved it brokers a zero-object pack that adds
          // nothing to origin (verified 2026-08-23); either way no spurious tree is
          // published. It runs AFTER the fetch-back so the tracking ref checkpointPack reads
          // is current. Best-effort
          // (publishCheckpointBestEffort swallows/logs/surfaces every failure — null pack,
          // non-2xx, thrown error — and never throws): a publish failure must never undo the
          // park, exactly as the fetch-back above must not (D4). The publish stays on the
          // join-token seam (checkpointPack local read → client.publishCheckpoint), never a
          // git push / PAT (ADR-628 guardrail invariants).
          //
          // issue #1030: the park-publish result is now EXPLICIT on the feed rather than
          // ignored — a success line, and a failure line that names the durability
          // consequence (a resume on another worker restarts from the default branch). The
          // `false` case covers a real publish failure AND an empty park (null pack / no
          // committed work): in both, nothing landed on refs/uzi-checkpoints/<branch>, so a
          // cross-worker resume does restart from default, which the line states truthfully.
          // publishCheckpointBestEffort ALSO emits the specific HTTP/skip outcome (deduped),
          // so a failure shows both the cause and this consequence. This batcher.emit lands
          // because handleLimitReached now FLUSHES (not closes) the batcher on the park
          // branch, leaving it open until the close just below.
          // PRD #1062 M2 (#1036): the park path is reaped (the agent tree is dead by the time
          // the catch runs), so a PAT git op is permitted — build the overlay so a branch behind
          // `main` on `.github/workflows` still checkpoints durably instead of skipping.
          const parkOverlay = await this.buildCheckpointOverlay(claim, flight, barePath);
          const parkPublished = await this.publishCheckpointBestEffort(flight, barePath, branch, parkOverlay);
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text: parkPublished
                ? "park checkpoint published to origin"
                : "park checkpoint NOT published — a resume on another worker will restart from the default branch",
            },
          });
          }
          // Close the batcher on the park path (handleLimitReached deferred the close so the
          // checkpoint-publish outcome above could reach the feed). Closed exactly once here
          // for every parked run, including the edge where the paths above were absent.
          await batcher.close().catch(() => undefined);
        }
      } else if (flight.active?.shuttingDown) {
        // PRD #218 M1 — the worker is shutting down (SIGTERM/SIGINT) and aborted this
        // run mid-flight. The DISCRIMINATOR is the flag, never the error: a user
        // steering-cancel aborts the same controller with the same REASON_CANCELLED and
        // must still fall through to the generic failure below. The run's tree is
        // already reaped (sdk-executor's run() finally kills the agent tree before this
        // catch is entered); killAgentTree here is the belt-and-braces reap at the
        // security boundary before we read the runner-owned clone.
        executor.killAgentTree?.();
        if (flight.barePath && flight.worktreePath && flight.branch) {
          const barePath = flight.barePath;
          const worktreePath = flight.worktreePath;
          const branch = flight.branch;
          // PRD #1030 M4: publish a FINAL checkpoint on graceful shutdown, mirroring the
          // park path's ordering (commitWipMarker → fetchBackBestEffort → publishCheckpoint).
          // Before M4 this branch was fetch-back ONLY, so a roll-while-running / eviction /
          // OOM / node-drain lost up to a whole ~20-min checkpoint interval of committed
          // work AND any uncommitted mid-milestone edits. Now:
          //   1. commitWipMarker captures uncommitted work into a throwaway `wip(park):`
          //      marker commit (best-effort; the .catch is belt-and-braces so nothing here
          //      undoes the requeue), exactly as the park path does — the shutdown branch
          //      previously did NOT do this, so uncommitted work was lost even from the marker.
          //   2. fetchBackBestEffort moves that tip to the local tracking ref.
          //   3. publishCheckpointBestEffort brokers the delta to refs/uzi-checkpoints/<branch>
          //      AFTER the fetch-back (so checkpointPack reads the current tip), one-shot and
          //      unconditional on the join-token seam (never a git push / PAT), same signature
          //      the park path uses. So a DIFFERENT worker re-claiming this requeued run
          //      recovers the committed tree instead of cold-starting from the default branch.
          //
          // BUDGET (issue #1030 M4 / PRD #1030): this whole best-effort sequence runs inside
          // the k8s termination grace. No terminationGracePeriodSeconds is set on the worker
          // pod (confirmed in controller/internal/kube/materializer.go), so the default 30s
          // applies: after SIGTERM the process must exit before SIGKILL at 30s. The local git
          // steps (marker, fetch-back) are sub-second; the ONLY step that can hang is the
          // publish RPC to the api, and the server-side maxPublishDuration is 60s — longer
          // than the entire grace — so the CLIENT must not simply await it. We cap the whole
          // sequence with a Promise.race against shutdownPublishTimeoutMs (default 15s): that
          // leaves ~15s of the 30s grace for the runtime's own SIGTERM teardown and for the
          // concurrently-aborted sibling runs' sequences (shutdown() aborts every active run
          // at once; their catch branches race the same wall clock, so the cost is the slowest
          // plus contention, not the sum). 15s is generous for a healthy publish yet caps a
          // hung/unreachable forge well short of both the 30s SIGKILL and the 60s server cap.
          const durability = (async (): Promise<boolean> => {
            await this.git.commitWipMarker(worktreePath).catch(() => false);
            await this.fetchBackBestEffort(barePath, worktreePath, branch, runId, runLog);
            // PRD #1062 M2 (#1036): the graceful-shutdown path already reaped the agent tree
            // (killAgentTree above), so the overlay's PAT default-fetch is permitted here — a
            // behind-on-workflows branch checkpoints durably instead of skipping.
            const shutdownOverlay = await this.buildCheckpointOverlay(claim, flight, barePath);
            return this.publishCheckpointBestEffort(flight, barePath, branch, shutdownOverlay);
          })();
          // undefined ⇒ the budget elapsed before the sequence finished: nothing confirmably
          // landed, so a cross-worker resume does restart from default — the same truthful
          // consequence as an outright publish failure. `=== true` folds false + undefined
          // into the NOT-published line. The orphaned durability promise (on a timeout) is
          // best-effort and never rejects, so leaving it running past this point is safe.
          const published = await this.raceShutdownBudget(
            durability,
            this.shutdownPublishTimeoutMs,
          );
          // issue #1030 M4: surface the outcome on the feed the same way the park path does,
          // reusing the batcher emit + the reportPublishOutcome dedupe from M1. This lands
          // because it is emitted BEFORE the single batcher.close() below — the shutdown
          // branch closes the batcher exactly once, further down, never here.
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text:
                published === true
                  ? "shutdown checkpoint published to origin"
                  : "shutdown checkpoint NOT published — a resume on another worker will restart from the default branch",
            },
          });
        }
        // PRD #556 M1: a shutdown interrupt now preserves the same two filesystem dirs a
        // park does (the sibling skills plugin dir and the per-run HOME holding the
        // resumable SDK transcript), so a same-worker re-claim within the affinity grace
        // can resume the SDK session instead of restarting it from scratch. Scoped to
        // ONLY those two fs removals in the finally — every other cleanup still runs.
        flight.preserveSession = true;
        runLog.info("run interrupted by worker shutdown; leaving it for requeue");
        await batcher.close().catch(() => undefined);
        // NO reportState: the run stays non-terminal so the server's sweeper requeues
        // it. Reporting `failed` here would turn a recoverable interruption into a dead
        // run — the exact outcome the fetch-back exists to prevent.
      } else {
        // failure_reason goes straight to reportState, bypassing the batcher's
        // redactor, and the sdk-executor catch-all re-throws raw SDK errors into this
        // path — so scrub it here with the run's own secret set. A plan rejection
        // carries the user's verbatim reason; scrubbing it too is harmless for plain
        // text and a safety net if the user pasted a secret.
        const rawReason =
          err instanceof PlanRejectedError
            ? err.reason
            : err instanceof TerminalReportError
              ? err.reason
              : errMessage(err);
        const reason = redactText(rawReason);
        // PRD #69 M7a: derive the TRUSTED failure class from the RAW reason (before
        // redaction) so a fatal pre-start failure (provisioning / no token) carries a
        // structured origin the judge can key on; an ordinary agent failure maps to
        // undefined and the server defaults it to 'agent_failure'. PRD #1077: a
        // TerminalReportError carries its own typed origin (push_secret_blocked) whose
        // terminal report threw after exhausting retries — honor it verbatim.
        const failOrigin =
          err instanceof TerminalReportError
            ? err.failOrigin
            : failOriginForReason(rawReason);
        runLog.error("run failed", { error: reason });
        batcher.emit({
          kind: "error",
          agent: "worker",
          payload: { text: reason },
        });
        await batcher.close().catch(() => undefined);
        // Cap what lands in the run row (matches the GitLab error-body cap).
        await reportState({
          status: "failed",
          failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN),
          fail_origin: failOrigin,
        }).catch((e) =>
          runLog.error("could not report failed state", {
            error: errMessage(e),
          }),
        );
      }
    } finally {
      // PRD #218 M1: drop the shutdown-registry entry. A terminal run (or a parked one)
      // must not stay abortable — shutdown() iterating a stale entry would abort a
      // controller nobody is watching, and the map would leak an entry per run.
      this.activeRuns.delete(runId);
      await steering.stop().catch(() => undefined);
      // PRD #41: drop this run's plan-approval deadline + gate-tracking (normally cleared
      // when the gate resolves terminally, but a run that ends by any other path must not
      // leak either).
      this.gateDeadlines.delete(runId);
      this.gatedRuns.delete(runId);
      // PRD #88: the clarification park's per-run state follows the SAME rule as the
      // gate maps above, and for the same reason main gives below — these are
      // in-memory per-run entries that would otherwise be held for the whole length of
      // a park. Correct on the resume path too: openQuestionIds is re-seeded from
      // claim.open_question_id, so clearing it here loses nothing a resume needs.
      this.questionDeadlines.delete(runId);
      this.openQuestionIds.delete(runId);
      this.questionCounts.delete(runId);
      // Evict this run's secrets from the logger (Decision 7). Reaching this finally
      // means execute() ran to a terminal report OR the run PARKED (PRD #35); a
      // requeue (worker death) still never returns here.
      //
      // This eviction runs on BOTH paths and is deliberately outside the park
      // carve-out below. The secrets are re-delivered on the next claim, and this
      // worker goes on to run other runs in the meantime — leaving a parked run's
      // decrypted PAT and Anthropic token registered in the logger for the days a
      // seven-day window can last is not a cleanup nicety, it is the security half
      // of the same decision.
      //
      // (This block used to assert "reaching this finally means execute() ran to a
      // terminal report … the evict/HOME-cleanup below only ever fire for a run that
      // will not resume". The park is the first path that reaches here and DOES
      // resume, so both sentences are corrected above rather than left to mislead.)
      for (const s of runScopedSecrets) this.log.removeSecret(s);
      // ── The park / shutdown carve-out (PRD #35 Decision 6a; PRD #556 M1) ──────
      // EXACTLY two filesystem removals below are skipped for a parked run, and
      // nothing else in this finally is. The two are what a resume needs: the
      // sibling skills plugin dir, and the per-run HOME that holds the resumable
      // SDK transcript. Preserving only one of them would resume into a session
      // missing its plugins or its transcript.
      //
      // (PRD #556 M1: a WORKER-SHUTDOWN interrupt now ALSO preserves these exact
      // two dirs, via the separate `preserveSession` flag — a same-worker re-claim
      // within the affinity grace resumes the SDK session instead of restarting it.
      // It is deliberately scoped to ONLY these two fs removals, exactly like the
      // park flag; the secret eviction above and the steering.stop() / gate-map
      // deletes stay structurally upstream and unguarded, so neither flag reaches
      // them. The widening warning below applies to `preserveSession` too — it must
      // never be extended to cover any of those.)
      //
      // (PRD #218 M6, 2026-08-04: the runner clone is no longer preserved on a
      // park — the clone leg below runs unconditionally now. `runnerCloneForBranch`
      // unconditionally deletes the clone on every claim and re-seeds it from a
      // bare ref, so a parked run's own commits were never recovered from the
      // preserved clone directory in the first place. PRD #218 M1/M2 moved the real
      // durability off the clone: the agent's committed branch is fetched back into
      // this worker's bare repo on both the park and shutdown paths, anchored to
      // the writing run's id, and an owned resume reseeds off that tracking ref.
      // That made preserving the clone redundant, and M7 proved it live on
      // dev-cluster (2026-08-04): a real worker eviction recovered committed work
      // from the tracking ref, seeded_from=tracking, marker byte-identical. So the
      // clone leg is removed here — dropping the whole guard is wrong (`worktreePath`
      // is the undefined guard and `removeRunnerClone(undefined)` would fire), only
      // the `&& !parked` is gone. The plugin-dir and HOME legs are UNCHANGED and
      // still load-bearing exactly as the sentence above says.)
      //
      // The other four statements in this block — the steering poller stop, the two
      // gate-map deletes, and the secret eviction above — MUST still run on a park.
      // Guarding the whole block (or returning early) would leave a poller running
      // and a gate deadline registered for what may be days.
      //
      // ⚠ HOW EACH ONE FAILS IF YOU WIDEN `!parked` (or the sibling `!preserveSession`
      // from PRD #556 M1) TO COVER IT, because they do not fail alike and only one of
      // them fails legibly — neither flag may ever reach these upstream statements:
      //   - the SECRET EVICTION fails SILENTLY. Measured by the M1 reviewer: guarding
      //     it passed typecheck and every runner test with exit 0, leaving a parked
      //     run's decrypted PAT and Anthropic token in the logger for the length of
      //     the window. There is now a test for exactly that ("still evicts the
      //     run-scoped secrets ... when the run parks"); it is the only thing
      //     standing between that mistake and a green build.
      //   - `steering.stop()` fails as a PROCESS HANG, not a red assertion: the
      //     poller keeps the event loop alive and the test file never exits. Read
      //     CLAUDE.md before concluding flake — `node --test` prints `ℹ fail 0`
      //     for a timeout, so the tally will say everything passed while the exit
      //     code says otherwise. A hang here is this bug until proven otherwise.
      if (flight.worktreePath) {
        await this.git.removeRunnerClone(flight.worktreePath).catch((e) =>
          runLog.warn("runner clone cleanup failed", {
            error: errMessage(e),
          }),
        );
      }
      // Tear down the sibling skills plugin dir the executor synthesized (PRD #16
      // M4). It is OUTSIDE the runner clone, so removeRunnerClone does not reach it;
      // leave it and each run leaks a dir. Best-effort, like the clone cleanup.
      if (flight.worktreePath && !flight.parked && !flight.preserveSession) {
        await fs
          .rm(skillsPluginDir(flight.worktreePath), { recursive: true, force: true })
          .catch((e) =>
            runLog.warn("skills plugin cleanup failed", {
              error: errMessage(e),
            }),
          );
      }
      // Remove this run's private HOME (agent-home/<runId>, Decision 5). The SDK
      // session transcript under it is only needed to resume, and a terminal run
      // never resumes. A concurrent sibling's HOME is a distinct dir, untouched.
      //
      // rmTreeForce, not fs.rm (PRD #108 M6): the Go module cache under this HOME
      // writes its package directories mode 0555, and `force: true` suppresses
      // ENOENT — not the EACCES that unlinking inside a read-only directory
      // raises. Every Go-touching run stranded its module cache (167.3 MB
      // measured for one run). Still best-effort and still swallowing its own
      // error: this is a `finally`, and a cleanup that threw would convert a
      // completed run into a failed one, which is strictly worse than a leak.
      if (runHome && !flight.parked && !flight.preserveSession) {
        await rmTreeForce(runHome).catch((e) =>
          runLog.warn("run HOME cleanup failed", { error: errMessage(e) }),
        );
      }
      if (flight.parked) {
        // ~170 MB of Go module cache was measured under a single run HOME, so say
        // what is being held and why — an operator reading disk pressure needs to
        // connect it to a parked run rather than to a leak.
        runLog.info(
          "run parked on a usage limit; preserving its plugin dir and HOME for resume",
          {
            run_home: runHome,
          },
        );
      } else if (flight.preserveSession) {
        // PRD #556 M1: a shutdown-preserved HOME holds the same ~170 MB of Go module
        // cache a parked one does, so log it with its provenance too — an operator
        // reading disk pressure needs to connect the held dir to the shutdown interrupt
        // (a same-worker resume) rather than to a leak.
        runLog.info(
          "run interrupted by worker shutdown; preserving its plugin dir and HOME for a same-worker resume",
          {
            run_home: runHome,
          },
        );
      }
    }
  }

  private async phasePublish(claim: ClaimResponse, flight: RunFlight): Promise<void> {
    const { runLog, batcher, reportState, redactText, executor, runHome } = flight;
    const runId = claim.run_id;
    const result = flight.result!;
    const runnerClone = flight.runnerClone!;
    const barePath = flight.barePath!;
    const lastPublishedTip = flight.lastPublishedTip;
    const ciFixHumanApproved = flight.ciFixHumanApproved;
    // A ci_fix run that judged the failure not a code problem (PRD #6) completes
    // with the diagnosis and NO push/MR — there is nothing to land.
    if (result.fixVerdict === "not_code") {
      executor.killAgentTree?.();
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "not a code problem: completing with the diagnosis, no merge request",
        },
      });
      await batcher.close();
      await reportState({ status: "completed", fix_verdict: "not_code" });
      runLog.info("ci_fix run completed with not_code verdict", {
        run_id: runId,
      });
      return;
    }

    // issue #279: a DECLARED report-only run — the lead's deliverable is a report,
    // command output, or verification result with NO code change to land, so the run
    // completes with its findings and NO push/MR (mirroring the ci_fix not_code path
    // above). killAgentTree was already called at the security boundary above, so we do
    // NOT double-call it here (the not_code block's second call is a redundancy — not
    // copied). Returns before fetchAgentBranch: there is no branch to fetch or push.
    if (result.reportOnly) {
      // issue #299: a report-only completion opens NO branch and NO MR, so if this run
      // ALREADY published committed work to a checkpoint ref on origin
      // (refs/uzi-checkpoints/<branch>), completing report-only would leave that ref
      // orphaned — un-landed, with nothing to supersede it. ADR-0279 documented this as
      // an accepted edge resting on the convention "a genuine zero-code run never
      // checkpoints"; enforce that convention here instead. Detection is the UNION of
      // two signals, each covering a gap the other has:
      //   - lastPublishedTip: a checkpoint THIS worker confirmed-landed mid-run (set only
      //     on a landed publish), which may not yet be mirrored into the bare's local ref.
      //   - hasCommittedCheckpoint: origin's checkpoint ref, mirrored into the bare at
      //     clone/fetch time — catches a checkpoint a PRIOR/cross-worker attempt landed.
      //     Per PRD #759 it IGNORES a marker-only `wip(park):` checkpoint (an abandoned
      //     usage-limit-park WIP marker with no committed milestone below it), while a
      //     real committed milestone still blocks.
      // A genuine zero-code run trips NEITHER (nothing committed ⇒ no pack ⇒ no publish),
      // so it still completes report-only below. Refuse loudly, mirroring the
      // undeclared-empty-diff FAIL path, rather than opening a delete-ref capability.
      const publishedCheckpoint =
        lastPublishedTip !== undefined ||
        (await this.git.hasCommittedCheckpoint(barePath, runnerClone.branch));
      if (publishedCheckpoint) {
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "report_only was set but this run published a checkpoint to origin; failing to avoid orphaning it",
          },
        });
        await batcher.close();
        await reportState({
          status: "failed",
          failure_reason:
            "signal_done was called with report_only, but this run published committed work to a checkpoint ref (refs/uzi-checkpoints/" +
            runnerClone.branch +
            ") on origin. A report-only completion opens no branch or merge request and would orphan that checkpoint. If this run has code to land, call signal_done WITHOUT report_only so the work lands as a merge request; report_only is only valid for a run that committed nothing.",
        });
        runLog.info("run failed: report_only declared after a checkpoint was published", {
          run_id: runId,
        });
        return;
      }
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "report-only run: recording findings; no branch pushed and no merge request opened",
        },
      });
      await batcher.close();
      await reportState({
        status: "completed",
        report_only: true,
        report_md: result.summary,
      });
      runLog.info("run completed report-only (no MR)", { run_id: runId });
      return;
    }

    // (b) fetch-back (PRD #51 M3): the agent committed in the RUNNER clone, so the
    // worker now fetches the agent branch BACK into its own bare (single-branch
    // refspec, file://+pack transport, protocol.file.allow pinned — the six B2
    // invariants live in git.fetchAgentBranch) before it inspects or pushes. Done
    // AFTER killAgentTree so no agent process is concurrently mutating the clone,
    // and it brings the agent's objects into the worker bare so the push does not
    // depend on the (soon torn-down) runner clone. `trackingRef` is what push +
    // changedFiles read; the runner clone is never a git source for either.
    const trackingRef = await this.git.fetchAgentBranch(
      barePath,
      runnerClone.path,
      result.branch,
      runId,
    );

    // issue #279: an UNDECLARED zero-diff guard, ISSUE runs only. A declared report_only
    // already returned above, so reaching here on an issue run with a confirmed-empty diff
    // is the ambiguous "forgot to commit / should have set report_only" case — a
    // committed-nothing issue run must not open an empty MR.
    if (resolveRunKind(claim.kind) === "issue") {
      const changedForGuard = await this.git.changedFiles(barePath, trackingRef);
      // changedFiles returns null on diff-FAILURE (keep pushing — fail open) and [] on a
      // CONFIRMED-empty diff. report_only is the sanctioned zero-diff success — a declared
      // report_only already returned above, so reaching here undeclared+empty is the
      // ambiguous case; fail with an actionable reason instead of opening an empty MR.
      if (changedForGuard !== null && changedForGuard.length === 0) {
        // PRD #634 M3: an operator scope directive can truncate a run at its very first
        // milestone boundary, before any milestone produced committed work — a legitimate
        // zero-slice, NOT the ambiguous "forgot to commit" failure below. But guard against
        // orphaning: if this run DID publish a checkpoint to origin (a mid-run milestone
        // landed there), there IS committed work to land, so fall through to the push+MR
        // path. Only the genuinely-empty case completes report-only with no MR. Detection
        // reuses the SAME publishedCheckpoint union the declared-report_only path uses,
        // which (PRD #759) ignores a marker-only `wip(park):` checkpoint while a real
        // committed milestone still blocks.
        if (result.scopeCapped) {
          const publishedCheckpoint =
            lastPublishedTip !== undefined ||
            (await this.git.hasCommittedCheckpoint(barePath, runnerClone.branch));
          if (!publishedCheckpoint) {
            batcher.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: "operator scope directive stopped this run before any milestone produced committed work; recording it, no merge request",
              },
            });
            await batcher.close();
            await reportState({
              status: "completed",
              report_only: true,
              scope_capped: true,
              report_md:
                result.summary ??
                "Stopped by operator scope directive before any committed work; nothing to land.",
            });
            runLog.info("run completed scope-capped with no committed work (no MR)", {
              run_id: runId,
            });
            return;
          }
          // published checkpoint exists → there IS work to land; fall through to push+MR.
        } else {
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text: "no changes were committed and report_only was not set; failing",
            },
          });
          await batcher.close();
          await reportState({
            status: "failed",
            failure_reason:
              "signal_done was called but no changes were committed, and report_only was not set. If this run's deliverable is a report or command output with no code change, call signal_done with report_only: true.",
          });
          runLog.info("run failed: signal_done with empty diff and no report_only", {
            run_id: runId,
          });
          return;
        }
      }
    }

    // issue #341: a `prompt` run (schedule-fired ad-hoc work) that reaches signal_done
    // having committed NOTHING has no diff to land, so pushing it would open an empty
    // merge request — the #242 pathology, for the schedule path. Unlike the issue-kind
    // guard above, a zero-commit prompt run is not the ambiguous "forgot to commit"
    // failure: an ad-hoc prompt whose deliverable is an investigation/answer legitimately
    // produces no code, so it completes as report-only (no branch, no MR), mirroring the
    // declared report_only terminal above — INCLUDING its issue #299 checkpoint-orphan
    // guard — rather than failing. ADR-0279 §2 forbids broadening the issue-kind gate
    // onto other kinds precisely because they have their own terminal paths; this IS that
    // separate terminal for `prompt`. A prompt run that DID commit falls through to the
    // normal push/MR path unchanged.
    if (claim.kind === "prompt") {
      const changedForPrompt = await this.git.changedFiles(barePath, trackingRef);
      // Preserve the null-vs-[] split: changedFiles returns null on diff-FAILURE (keep
      // pushing — fail open) and [] on a CONFIRMED-empty diff. Only a confirmed-empty
      // diff completes report-only; a diff-failure must NOT be treated as empty.
      if (changedForPrompt !== null && changedForPrompt.length === 0) {
        // issue #299: a report-only completion opens NO branch and NO MR, so if this run
        // ALREADY published committed work to a checkpoint ref on origin
        // (refs/uzi-checkpoints/<branch>), completing report-only would orphan that ref.
        // This mirrors the declared report_only terminal above: detect via the UNION of
        // lastPublishedTip (a checkpoint THIS worker confirmed-landed mid-run) and
        // hasCommittedCheckpoint (origin's checkpoint ref, mirrored into the bare at
        // clone/fetch time — catches a prior/cross-worker landing; per PRD #759 it ignores
        // a marker-only `wip(park):` checkpoint while a real committed milestone still
        // blocks). A genuine zero-code prompt run trips NEITHER and still completes
        // report-only below.
        const publishedCheckpoint =
          lastPublishedTip !== undefined ||
          (await this.git.hasCommittedCheckpoint(barePath, runnerClone.branch));
        if (publishedCheckpoint) {
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text: "report_only was set but this run published a checkpoint to origin; failing to avoid orphaning it",
            },
          });
          await batcher.close();
          await reportState({
            status: "failed",
            failure_reason:
              "signal_done was called with report_only, but this run published committed work to a checkpoint ref (refs/uzi-checkpoints/" +
              runnerClone.branch +
              ") on origin. A report-only completion opens no branch or merge request and would orphan that checkpoint. If this run has code to land, call signal_done WITHOUT report_only so the work lands as a merge request; report_only is only valid for a run that committed nothing.",
          });
          runLog.info("prompt run failed: report_only after a checkpoint was published", {
            run_id: runId,
          });
          return;
        }
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "prompt run committed no changes: recording findings; no branch pushed and no merge request opened",
          },
        });
        await batcher.close();
        await reportState({
          status: "completed",
          report_only: true,
          report_md: result.summary,
          // PRD #929 M2: a scheduled prompt run's deliverable can be a structured proposal
          // (title + body) the agent conveyed on signal_done, which the server files as a
          // forge issue. Thread it through ONLY when the agent actually produced one — the
          // key is OMITTED otherwise (spread nothing) so a normal/mr-mode prompt run's
          // completion payload is byte-identical to before.
          ...(result.proposal !== undefined
            ? { proposal: result.proposal }
            : {}),
        });
        runLog.info("prompt run completed report-only (no MR): zero-diff", {
          run_id: runId,
          has_proposal: result.proposal !== undefined,
        });
        return;
      }
    }

    // Self-improvement MR evidence (PRD #46 Decision 10): with no CI on the uzi
    // repo, the worker itself runs the test suites and flags any guard-critical
    // path the change touched, folding both into the MR description. Best-effort —
    // gathered before the push so the MR opens with its evidence, and a suite that
    // can't run is reported "skipped", never failing the run.
    let selfImproveSection: string | undefined;
    // PRD #686 M4: uzi's SELF_IMPROVE_CHECKS (go test ./..., web/agent npm test,
    // web build) are hardcoded to uzi's OWN layout and are meaningless against an
    // arbitrary target repo, so this evidence block runs ONLY in dogfood mode. In
    // generic mode it is skipped: selfImproveSection stays unset and no uzi-shaped
    // check/guard-path evidence is produced — the generic plan directive already
    // told the agent to discover and run the target repo's own gates during the run.
    if (claim.kind === "self_improve" && claim.self_improve_dogfood) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "self-improvement: running the test suites for MR evidence",
        },
      });
      // changedFiles returns null when the diff could not be computed → pass null
      // through so the MR section fails CLOSED (a loud "guard-path check unavailable"
      // note) instead of silently suppressing the flag (M5 audit). Under (b) this is
      // a WORKER-BARE tree-diff of the fetched tracking ref (no runner-owned config
      // source read), while the checks below still run in the runner clone.
      const changed = await this.git.changedFiles(barePath, trackingRef);
      // M9: the checks execute agent-authored code as the worker uid, so they run
      // under a SCRUBBED replacement env (no join token / API URL / PAT / OAuth token
      // by construction) with the run's provisioned toolchains on PATH. The frozen
      // `--ignore-scripts` install runs best-effort so vitest/tsc exist; a failure
      // just leaves the check honestly skipped.
      //
      // PRD #121 M2: this used the hardcoded `["web", "agent"]` dir list; it now
      // reuses the same lockfile-driven installer the executor runs pre-plan, so
      // there is ONE install path instead of two that can drift. For the uzi repo
      // that discovery resolves to exactly web/ + agent/ (the only two tracked
      // package.json files, both with a package-lock.json), which is what
      // SELF_IMPROVE_CHECKS pre-flights on — asserted in js-deps.test.ts. The
      // executor has already installed these once; re-running is deliberate, because
      // the agent may have edited a package.json or lockfile during the run and the
      // checks must test what it actually left behind.
      const checkEnv = buildCheckEnv(
        process.env,
        runHome ?? os.tmpdir(),
        result.toolEnv,
      );
      const deps = await installJsDeps(runnerClone.path, checkEnv).catch(
        () => ({ results: [], truncated: false }),
      );
      for (const note of deps.results) {
        runLog.info("self-improve: dependency install", { ...note });
      }
      // A truncated scan means `results` is a PREFIX of the repo's project dirs, so a
      // check may be about to run somewhere provisioning never reached. Say so rather
      // than let the notes above read as full coverage.
      if (deps.truncated) {
        runLog.warn(
          "self-improve: dependency discovery hit its bound; some dirs were not installed",
          {
            installed_dirs: deps.results.length,
          },
        );
      }
      const checkRunner = this.checkRunner ?? defaultCheckRunner(checkEnv);
      const checks = await runSelfImproveChecks(
        runnerClone.path,
        checkRunner,
      );
      selfImproveSection = selfImproveMrSection(
        changed === null ? null : flagGuardPaths(changed),
        checks,
      );
    }

    // Guard-critical flag for an ad-hoc scheduled prompt run (PRD #241 Decision 10,
    // the one worker-side follow-up). self_improve gets this flag by KIND — it is
    // hardcoded API-side to uzi's own repo, a decision the worker CANNOT reproduce:
    // the claim carries no repo-identity field, so there is no clean worker-side
    // "is this the uzi repo" signal to gate on (flagged for the owner in the M8
    // report — a repo-identity flag on the claim would be the API-side alternative).
    // Instead we key the flag on the CHANGED PATHS: GUARD_CRITICAL_PATTERNS match
    // uzi's own security-critical source paths only, so flagGuardPaths fires exactly
    // when a prompt run actually touches uzi's guard surface (i.e. it targets the
    // uzi repo) and is an empty no-op on any other repo. We do NOT run
    // SELF_IMPROVE_CHECKS here — those are uzi's own gate suite and are meaningless
    // against an arbitrary repo. Best-effort, gathered before the push like the
    // self_improve evidence above.
    let promptGuardSection: string | undefined;
    if (claim.kind === "prompt") {
      // null (diff failed) → fail CLOSED with a loud "guard-path check unavailable"
      // note, exactly as the self_improve path does above (M5 audit).
      const changed = await this.git.changedFiles(barePath, trackingRef);
      promptGuardSection = guardCriticalMrSection(
        changed === null ? null : flagGuardPaths(changed),
      );
    }

    // PRD #71 M5 (load-bearing): for an auto-approved ci_fix run (no human in the loop),
    // REFUSE to push a diff that touches a protected CI-config path, or a diff that could
    // not be computed. A human-approved run (manual, or an auto CI-config plan that parked
    // and was approved) is never blocked — a human was in the loop, as in the manual flow.
    if (claim.kind === "ci_fix" && !ciFixHumanApproved) {
      // Capture the narrowed bare path in a const the SAME way the push block does
      // (barePath is an outer `let string | undefined` and TS drops the narrowing here).
      const pushBarePathForGuard = barePath;
      // Worker-side FLOOR: when the claim omits ci_config_paths (a bug or an older
      // server), fall back to the static defaults so the backstop cannot fail OPEN.
      // This floor covers the static defaults only; the server-produced set additionally
      // carries the project's real ci_config_path (see DEFAULT_CI_CONFIG_PATHS).
      const ciConfigPaths = claim.config?.ci_config_paths?.length
        ? claim.config.ci_config_paths
        : DEFAULT_CI_CONFIG_PATHS;
      const changed = await this.git.changedFiles(pushBarePathForGuard, trackingRef);
      const flagged = changed === null ? null : flagCIConfigPaths(changed, ciConfigPaths);
      if (changed === null || (flagged && flagged.length > 0)) {
        const reason =
          changed === null
            ? "auto CI-fix push refused: could not compute the diff to verify it does not edit CI config (failing closed)"
            : `auto CI-fix push refused: an auto-approved fix may not edit CI config (${flagged!.join(", ")}); a CI-config fix needs human approval`;
        batcher.emit({ kind: "status", agent: "worker", payload: { text: reason } });
        runLog.warn("ci-fix: CI-config push guard refused push", { run_id: runId, reason });
        // Fail the run CLOSED — no push, no MR. Throwing here lands on the method's
        // generic catch (the `else` at ~:1203), which reports status:"failed" with this
        // reason and does NOT re-queue (unlike LimitReached/shutdown). Same terminal
        // convention the push/MR failures use.
        throw new Error(reason);
      }
    }

    // PRD #377 M1: a GitHub run whose branch touches .github/workflows/** cannot be
    // pushed by the bot's repo-only PAT (privcheck forbids the workflow scope by design).
    // Detect it here, BEFORE the doomed push, and end the run in a typed `failed` outcome
    // that preserves the agent's diff for a human to land — instead of face-planting into
    // GitHub's opaque "without workflow scope" rejection and discarding the committed work.
    // Serves every forge-pushing kind (the failed path is not issue-gated).
    // #377 / issue #631: computed once here, reused by the base-align overlay gate below
    // (both read changedFiles(barePath, trackingRef)); recomputed there only if this failed open.
    let changedForWf: string[] | null = null;
    if (claim.repo.forge_type === "github") {
      // Capture the narrowed bare path in a const the SAME way the push block does
      // (barePath is an outer `let string | undefined` and TS drops the narrowing here).
      const wfBarePath = barePath;
      changedForWf = await this.git.changedFiles(wfBarePath, trackingRef);
      // D6: a null diff (diff-computation failure) fails OPEN to the normal push — do not
      // fail a possibly-legitimate non-workflow run on an inability to compute the diff.
      const wfHits =
        changedForWf === null
          ? null
          : flagCIConfigPaths(changedForWf, [".github/workflows/**"]);
      if (wfHits && wfHits.length > 0) {
        // Compose an actionable, capped failure_reason that names the offending path(s)
        // (truncating the path LIST if needed, never the doc link) and points at
        // docs/github-bot-setup.md.
        const reason = composeWorkflowScopeReason(wfHits);
        // Preserve the agent's diff so a human can land it without re-deriving it from the
        // transcript. redactText scrubs the run's secrets before it reaches the api; a null
        // diff (best-effort failure) just omits the patch — the typed failure still lands.
        const rawPatch = await this.git.workflowScopeDiff(wfBarePath, trackingRef);
        const patch = rawPatch === null ? undefined : redactText(rawPatch);
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text:
              "branch changes .github/workflows, which the bot token cannot push; failing early and preserving the diff for a human to land",
          },
        });
        runLog.info(
          "run failed: branch touches .github/workflows which the bot PAT cannot push; preserving diff",
          { run_id: runId, paths: wfHits },
        );
        await batcher.close();
        await reportState({
          status: "failed",
          failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN),
          fail_origin: "workflow_scope_missing",
          preserved_patch: patch,
        });
        return;
      }
    }

    // PRD #974 M2 / #1077: the single terminal reporter for a push_secret_blocked failure.
    // If reportState exhausts its bounded retries and throws, rethrow a typed sentinel so
    // execute()'s generic catch preserves the push_secret_blocked origin instead of defaulting
    // to agent_failure — and NEVER attach preserved_patch (the diff carries the detected secret).
    const reportPushSecretBlocked = async (reason: string): Promise<void> => {
      const capped = reason.slice(0, MAX_FAILURE_REASON_LEN);
      try {
        await reportState({
          status: "failed",
          failure_reason: capped,
          fail_origin: "push_secret_blocked",
          preserved_patch: undefined,
        });
      } catch (e) {
        throw new TerminalReportError(capped, "push_secret_blocked", e);
      }
    };

    // PRD #974 M2 (load-bearing security): a GitHub run's committed range is scanned for secrets
    // with the pinned gitleaks (default ruleset, all three silencers GitHub Push Protection
    // ignores DISABLED — see git.secretScanRange) BEFORE the doomed push, mirroring the #377
    // workflow-scope guard above. On a TRUSTWORTHY scan with a real finding, fail the run early
    // and typed with the diff preserved — instead of face-planting into GitHub's opaque GH013
    // rejection and discarding the committed work. An UNTRUSTWORTHY scan (broken/empty) fails
    // OPEN: it does NOT block the run, and the GH013 remote backstop at the push below covers a
    // real secret. GitHub-only: GitLab/Forgejo have no equivalent push-side secret rejection.
    if (claim.repo.forge_type === "github") {
      const scanBarePath = barePath;
      const scan = await this.git.secretScanRange(scanBarePath, trackingRef);
      if (scan.trusted && scan.findings.length > 0) {
        const reason = composePushSecretBlockedReason(scan.findings);
        // Do NOT preserve the diff on a secret block. redactText only scrubs the run's OWN
        // secrets (forge PAT / Anthropic / join token / gitBasic); a gitleaks finding is by
        // definition a DIFFERENT secret whose value we do not even have here (the report is
        // `--redact`'d to file/line/rule only), so a preserved patch would persist the detected
        // secret into runs.preserved_patch and render it in RunView — the exact leak this feature
        // exists to prevent. The committed work stays recoverable from the worker branch/PVC, and
        // the failure_reason names each offending commit+path.
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "branch carries a secret GitHub Push Protection would reject; failing early and preserving the diff",
          },
        });
        runLog.info(
          "run failed: branch carries a secret GitHub Push Protection would reject (GH013); preserving diff",
          {
            run_id: runId,
            findings: scan.findings.map((f) => ({
              commit: f.commit,
              file: f.file,
              rule: f.ruleId,
            })),
          },
        );
        await batcher.close();
        await reportPushSecretBlocked(reason);
        return;
      }
      if (!scan.trusted) {
        runLog.warn(
          "finalize secret scan: not trustworthy (broken/empty scan); pushing and relying on the GH013 remote backstop",
          { run_id: runId },
        );
      }
    }

    // The single authenticated finalize push (PAT-bearing, worker-owned; the agent never
    // has a credential). Captured once as a closure so the align path (below) and the
    // normal path push through EXACTLY ONE code path — the run must never push twice.
    // PRD #284 Layer A: a transient push failure (a dropped HTTP/2 stream, a 5xx, a
    // connection reset) retries rather than discarding the agent's already-committed work;
    // a permanent rejection (auth, protected branch, non-fast-forward) fails fast and
    // propagates to the catch below. The push is idempotent on retry (non-forced, same
    // commits → "Everything up-to-date"). Capture the narrowed bare path: barePath is an
    // outer `let` (string | undefined) and TS drops the narrowing inside the closure.
    const finalizeBarePath = barePath;
    const pushToOrigin = () =>
      withForgeRetry(
        () =>
          this.git.pushBranch(
            finalizeBarePath,
            result.branch,
            claim.secrets.forge_pat,
            claim.repo.clone_url,
            claim.secrets.forge_username,
          ),
        { log: runLog },
      );

    // PRD #974 M2 — the GH013 remote backstop. When a finalize push (the normal path OR an
    // align-path push) is rejected by GitHub Push Protection for a secret the pre-push gitleaks
    // scan missed (GitHub's pattern set is broader than gitleaks', and the two are not
    // identical), route it to the SAME typed push_secret_blocked failure instead of losing the
    // typing to the generic catch (raw message, defaulted fail_origin).
    // Do NOT preserve the diff: the push carries a secret, and at the backstop there are no
    // finding spans at all, so a preserved patch would persist the secret into
    // runs.preserved_patch / RunView — the exact leak this feature prevents. The committed work
    // stays recoverable from the run's branch/PVC; the fixed capped reason points a human there
    // (the backstop has no gitleaks findings to name, so it cannot use composePushSecretBlockedReason).
    const failPushSecretBlocked = async () => {
      const reason =
        "the push was rejected by GitHub Push Protection (GH013): it carries a secret. " +
        "The committed work is on the run's branch — scrub the secret from its history and re-push.";
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "push rejected by GitHub Push Protection (GH013); failing (work is on the run branch — scrub the secret and re-push)",
        },
      });
      runLog.info("run failed: push rejected by GitHub Push Protection (GH013)", {
        run_id: runId,
      });
      await batcher.close();
      await reportPushSecretBlocked(reason);
    };

    // PRD #456 M1: a GitHub run can be merely BEHIND the default branch on
    // .github/workflows/** (main advanced those files after this run's clone base) WITHOUT
    // having touched them. The bot's repo-only PAT push is then rejected atomically —
    // losing ALL the run's work — even though #377's guard above (which fires only when the
    // BRANCH modifies a workflow) did not trip. Align the branch's workflow tree with the
    // FRESH default before pushing: merge first (SHA-preserving, D2), and if the merged push
    // is STILL workflow-scope-rejected fall back to a rebase (the empirically proven #422
    // recovery). On an unresolvable conflict, fail the run typed + preserve the diff (M2)
    // rather than face-plant into GitHub's opaque rejection and discard the committed work.
    // GitHub-only: GitLab/Forgejo impose no workflow-scope rule.
    let alignPushed = false;
    if (claim.repo.forge_type === "github") {
      const alignBarePath = barePath;
      const alignDefaultBranch =
        claim.repo.default_branch?.trim() ||
        (await this.git.defaultBranchName(alignBarePath)) ||
        "main";
      // Detection is best-effort (N2/D6 posture): a fetch/diff failure must NOT block a push
      // that may well succeed (the branch may not actually be behind) — fall through to the
      // normal push, never fail a run on an inability to compute the align target.
      let defaultTip: string | undefined;
      let differs = false;
      try {
        defaultTip = await this.git.fetchDefaultTip(
          alignBarePath,
          alignDefaultBranch,
          claim.secrets.forge_pat,
          claim.repo.clone_url,
          claim.secrets.forge_username,
        );
        differs = await this.git.workflowTreeDiffers(
          alignBarePath,
          trackingRef,
          defaultTip,
        );
      } catch (e) {
        runLog.warn(
          "finalize base-align: could not compute the align target; pushing without aligning",
          { run_id: runId, error: errMessage(e) },
        );
      }
      if (defaultTip && differs) {
        // The pre-align committed agent tip — the base every align strategy starts from, so
        // a rebase FALLBACK after a clean merge replays the ORIGINAL commits, not the merge.
        const originalAgentTip = await this.git.branchTip(
          runnerClone.path,
          result.branch,
        );
        if (!originalAgentTip) {
          runLog.warn(
            "finalize base-align: could not resolve the branch tip; pushing without aligning",
            { run_id: runId },
          );
        } else {
          // The conflict-failure path (M2). The abort already ran inside
          // alignBranchWithDefault; here we preserve the diff via #377's preserved_patch and
          // fail typed. We diff the pre-align agent tip (`originalAgentTip`, declared at :1848,
          // non-null under the :1852 guard) — NOT `trackingRef` — so the preserved patch is
          // exactly the agent's human-landable work. Issue #631: when a strategy ALIGNED and
          // then had its push rejected (the overlay's arm (c), or a merge/rebase whose push was
          // rejected) `fetchAndPush` re-fetched the ALIGNED tip into `trackingRef`, so diffing
          // `trackingRef` yielded a SUPERSET (agent work PLUS the aligning strategy's own
          // workflow-subtree/merge changes) if a LATER strategy then conflicted and landed here.
          // Diffing `originalAgentTip` eliminates that superset: its objects were fetched into
          // the worker bare by the finalize `fetchAgentBranch` before any align, so
          // workflowScopeDiff resolves it. In the non-push conflict paths originalAgentTip ==
          // trackingRef, so those are unchanged; in the clobber-safety path (a branch that
          // edited a workflow) originalAgentTip carries that edit, so it is still preserved.
          const defTip = defaultTip;
          const failBaseAlignConflict = async () => {
            const rawPatch = await this.git.workflowScopeDiff(alignBarePath, originalAgentTip);
            const patch = rawPatch === null ? undefined : redactText(rawPatch);
            batcher.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: "could not realign the branch with the updated default branch and safely push it (merge and rebase conflicted, or the aligned branch could not be fast-forwarded); failing and preserving the diff for a human to land",
              },
            });
            runLog.info("run failed: finalize base-align conflict; preserving diff", {
              run_id: runId,
            });
            await batcher.close();
            await reportState({
              status: "failed",
              failure_reason: composeBaseAlignConflictReason(alignDefaultBranch),
              fail_origin: "finalize_base_align_conflict",
              preserved_patch: patch,
            });
          };

          // Run one align STRATEGY, treating an UNEXPECTED throw (the S3 count-mismatch
          // guard, or any git error) exactly like a `"conflict"` return. This is the whole
          // point of the feature: the agent's work must be PRESERVED on failure, so an
          // unexpected align error must route to failBaseAlignConflict (typed fail + diff),
          // NOT escape to the generic catch below (raw message, no preserved_patch, defaulted
          // fail_origin). Scoped to the align OPERATION only — the push keeps its own
          // handling (workflow-scope → rebase fallback; any other push error rethrows).
          const alignOp = async (strategy: "merge" | "rebase"): Promise<"aligned" | "conflict"> => {
            try {
              return await this.git.alignBranchWithDefault(
                runnerClone.path,
                result.branch,
                originalAgentTip,
                defTip,
                strategy,
              );
            } catch (e) {
              runLog.warn(
                "finalize base-align: unexpected error during align; preserving diff and failing typed",
                { run_id: runId, strategy, error: errMessage(e) },
              );
              return "conflict";
            }
          };

          // Re-fetch the aligned tip into the worker bare's tracking ref, then push once.
          const fetchAndPush = async () => {
            await this.git.fetchAgentBranch(
              alignBarePath,
              runnerClone.path,
              result.branch,
              runId,
            );
            await pushToOrigin();
            alignPushed = true;
          };

          // Push the aligned branch. Two rejections here are the base-align-conflict path,
          // not a mislabel: a REPEAT workflow-scope rejection means the default's workflow
          // files moved again DURING our align (double-TOCTOU); a NON-FAST-FORWARD rejection
          // means the rebase fallback rewrote the history of an already-published branch (a
          // resume, or the self_improve fixed branch) so this non-forced push cannot
          // fast-forward, and force-push is denied by the guardrails by design. Both preserve
          // the diff and fail typed rather than lose it to the generic catch. Any OTHER push
          // error still rethrows unchanged (a genuine auth/transient/protected-branch failure
          // must not be mislabelled as a base-align conflict). Returns true if it
          // preserved-and-failed (the caller must then `return`), false on a successful push.
          const pushAlignedOrPreserve = async (): Promise<boolean> => {
            try {
              await fetchAndPush();
              return false;
            } catch (e) {
              // PRD #974 M2: an aligned push rejected by GitHub Push Protection (GH013) is a
              // secret the pre-push gitleaks scan missed — route it to the typed
              // push_secret_blocked preserve-and-fail (diffing the pre-align agent tip) rather
              // than the base-align-conflict path or the generic catch.
              if (isPushProtectionRejection(e)) {
                runLog.info(
                  "finalize base-align: aligned push rejected by GitHub Push Protection (GH013); preserving diff and failing typed",
                  { run_id: runId },
                );
                await failPushSecretBlocked();
                return true;
              }
              const nonFf = isNonFastForwardRejection(e);
              if (!isWorkflowScopeRejection(e) && !nonFf) throw e;
              // Record WHICH cause fired so an operator reading logs can tell the two apart:
              // a repeat workflow-scope rejection (the default's workflow files moved again
              // DURING our align) versus a non-fast-forward (the rebase rewrote an
              // already-published branch's history, a resume or the self_improve fixed
              // branch, that the bot cannot force-push).
              runLog.info(
                nonFf
                  ? "finalize base-align: aligned push rejected non-fast-forward (rebase rewrote an already-published branch's history the bot cannot force-push); preserving diff and failing typed"
                  : "finalize base-align: aligned push STILL workflow-scope-rejected (default moved again during align); preserving diff and failing typed",
                { run_id: runId },
              );
              await failBaseAlignConflict();
              return true;
            }
          };

          // Issue #627 — the overlay gate (correctness pin). FRESHLY recompute the branch's
          // workflow-change signal here: the #377 guard earlier FAILS OPEN on a null diff, so
          // a branch that modified a workflow file can still reach this block. Overlaying the
          // default's workflow subtree would then CLOBBER that agent edit, so the overlay is
          // allowed ONLY when the diff succeeded AND the branch provably modified NO workflow
          // file. Any other case (null diff, or a real workflow edit) falls straight into the
          // EXISTING merge → rebase → preserve chain, unchanged.
          // Issue #631: reuse the #377 guard's changedFiles result (identical barePath+trackingRef);
          // recompute only when #377 failed open (null diff), so a transient diff failure gets a retry.
          const alignChanged =
            changedForWf === null
              ? await this.git.changedFiles(alignBarePath, trackingRef)
              : changedForWf;
          const alignWfHits =
            alignChanged === null
              ? null
              : flagCIConfigPaths(alignChanged, [".github/workflows/**"]);
          const canOverlay =
            alignChanged !== null && alignWfHits !== null && alignWfHits.length === 0;

          // Emit once here so the status fires on BOTH the overlay and the fallback paths.
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text: "branch is behind the default branch on .github/workflows; aligning before pushing",
            },
          });

          // PRIMARY (issue #627): overlay ONLY the default tip's .github/workflows/ subtree
          // onto the agent tip. It cannot conflict and is a fast-forward (original agent SHAs
          // preserved, nothing rebased), and it makes the tip's workflow tree equal main's —
          // all GitHub's tip-vs-default check requires — WITHOUT dragging in main's unrelated
          // changes the way a whole-tree merge/rebase does. Invoked OUTSIDE alignOp on
          // purpose: alignOp maps any throw to "conflict" (→ preserve-and-fail), which is
          // WRONG for the overlay — an overlay error must fall back to merge/rebase, not
          // preserve-and-fail. So the overlay gets its own try/catch here.
          let overlayHandled = false;
          if (canOverlay) {
            let overlayAligned = false;
            try {
              const res = await this.git.alignBranchWithDefault(
                runnerClone.path,
                result.branch,
                originalAgentTip,
                defTip,
                "workflow-subtree",
              );
              overlayAligned = res === "aligned"; // the overlay never returns "conflict"
            } catch (e) {
              // (b) the overlay git op threw (a GENUINE unexpected git error) → fall back to
              // merge/rebase, NOT preserve-and-fail. Distinct message from (c) below.
              runLog.warn(
                "finalize base-align: workflow-subtree overlay errored; falling back to merge/rebase",
                { run_id: runId, error: errMessage(e) },
              );
            }
            if (overlayAligned) {
              try {
                await fetchAndPush(); // sets alignPushed = true on success
                overlayHandled = true;
              } catch (e) {
                // PRD #974 M2: an overlay push rejected by GitHub Push Protection (GH013) is a
                // secret gitleaks missed — typed preserve-and-fail, not a fall-back to
                // merge/rebase (which cannot clear a secret) nor the generic catch.
                if (isPushProtectionRejection(e)) {
                  runLog.info(
                    "finalize base-align: workflow-subtree overlay push rejected by GitHub Push Protection (GH013); preserving diff and failing typed",
                    { run_id: runId },
                  );
                  await failPushSecretBlocked();
                  return;
                }
                if (!isWorkflowScopeRejection(e) && !isNonFastForwardRejection(e)) throw e;
                // (c) the overlay pushed but was STILL rejected (workflow-scope: the default
                // moved again during our align; or non-fast-forward: a resumed/rewritten
                // branch) → fall back to merge/rebase. Distinct message from (b) above so an
                // operator can tell the two failure modes apart.
                runLog.info(
                  "finalize base-align: workflow-subtree overlay push still rejected; falling back to merge/rebase",
                  { run_id: runId },
                );
              }
            }
          }

          if (!overlayHandled) {
            const mergeRes = await alignOp("merge");
            if (mergeRes === "aligned") {
              try {
                await fetchAndPush();
              } catch (e) {
                // PRD #974 M2: a merge push rejected by GitHub Push Protection (GH013) is a secret
                // gitleaks missed — typed preserve-and-fail, not the rebase fallback (which cannot
                // clear a secret) nor the generic catch.
                if (isPushProtectionRejection(e)) {
                  runLog.info(
                    "finalize base-align: merge push rejected by GitHub Push Protection (GH013); preserving diff and failing typed",
                    { run_id: runId },
                  );
                  await failPushSecretBlocked();
                  return;
                }
                if (isNonFastForwardRejection(e)) {
                  // Issue #631: a non-fast-forward rejection (an already-published branch — a resume, or
                  // the self_improve fixed branch — whose merge push cannot fast-forward) can't be cleared
                  // by the rebase fallback (it also can't force-push), so preserve the diff and fail typed
                  // rather than escape to the generic catch (raw message, no preserved_patch). This
                  // matches pushAlignedOrPreserve (:1945-1946), which likewise fails typed on non-ff.
                  // (The overlay push catch at :2020 handles non-ff differently — it can still fall
                  // back to merge/rebase — so this arm deliberately does NOT mirror it: once at the
                  // merge, a rebase cannot clear a non-ff on an already-published branch.)
                  runLog.info(
                    "finalize base-align: merge push rejected non-fast-forward; preserving diff and failing typed",
                    { run_id: runId },
                  );
                  await failBaseAlignConflict();
                  return;
                }
                if (!isWorkflowScopeRejection(e)) throw e;
                // The merge did NOT clear GitHub's workflow-scope rejection → the proven
                // rebase fallback (#422). alignBranchWithDefault rewinds to originalAgentTip
                // first, so the rebase replays the ORIGINAL agent commits onto the fresh
                // default rather than the merge commit.
                runLog.info(
                  "finalize base-align: merge push still workflow-scope-rejected; trying rebase fallback",
                  { run_id: runId },
                );
                const rebaseRes = await alignOp("rebase");
                if (rebaseRes === "aligned") {
                  if (await pushAlignedOrPreserve()) return;
                } else {
                  await failBaseAlignConflict();
                  return;
                }
              }
            } else {
              // The merge conflicted (or errored) — a rebase may still replay cleanly where a
              // single merge did not, so try it before giving up.
              runLog.info("finalize base-align: merge conflicted; trying rebase", {
                run_id: runId,
              });
              const rebaseRes = await alignOp("rebase");
              if (rebaseRes === "aligned") {
                if (await pushAlignedOrPreserve()) return;
              } else {
                await failBaseAlignConflict();
                return;
              }
            }
          }
        }
      }
    }

    // PRD #400 M2: a TASK run always pushes its branch back (the deliverable is the
    // commits the user pulls from uzi/task/<id>) but opens a merge request only when
    // it opted in (`uzi handoff --mr` → runs.open_mr → claim.open_mr). Every non-task
    // kind keeps its current MR behaviour unconditionally. This is a distinct
    // kind+flag gate on MR-OPEN only — a no-MR task still pushes, so it does NOT take
    // the report_only path above (which pushes nothing).
    const openMr = claim.kind !== "task" || claim.open_mr === true;

    // The agent signalled done. The WORKER now performs the authenticated push
    // (+ MR when openMr) with the PAT — the agent never had a credential.
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: {
        text: openMr
          ? "work complete; pushing branch and opening merge request"
          : "work complete; pushing branch (no merge request — pull the branch)",
      },
    });
    // The finalize push. Skipped when the PRD #456 align path above already pushed the
    // aligned branch — the run pushes through EXACTLY ONE code path (`pushToOrigin`), so a
    // successful align-push and the normal push converge here without ever double-pushing.
    if (!alignPushed) {
      try {
        await pushToOrigin();
      } catch (e) {
        // PRD #974 M2 backstop: a GitHub Push Protection (GH013) rejection here means a secret
        // the pre-push gitleaks scan missed — route it to the typed preserve-and-fail rather than
        // the generic catch. Any OTHER push error rethrows (unchanged behavior).
        if (isPushProtectionRejection(e)) {
          await failPushSecretBlocked();
          return;
        }
        throw e;
      }
    }

    // PRD #400 M2: a no-MR task completes HERE — the branch is pushed, there is
    // nothing more to open. Report the branch on the completion payload (the same
    // `branch: result.branch` the MR path reports below) so runs.branch carries
    // uzi/task/<id> and the user knows exactly what to pull. No mr_iid/mr_web_url:
    // there is no merge request.
    if (!openMr) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `task complete; pushed ${result.branch} (no merge request — pull the branch)`,
        },
      });
      await batcher.close();
      await reportState({
        status: "completed",
        branch: result.branch,
        prd_done_path: result.prdDonePath,
        milestones_completed: result.milestonesCompleted,
      });
      runLog.info("task run completed (no MR)", { branch: result.branch });
      return;
    }

    const targetBranch =
      claim.repo.default_branch?.trim() ||
      (await this.git.defaultBranchName(barePath)) ||
      "main";
    // Pick the forge client from the claim's forge_type (absent ⇒ gitlab, R8), so
    // the worker opens an MR on GitLab and a PR on Forgejo/GitHub from the same code
    // path; each client derives its own API base + project from repo.url (D9).
    // createMergeRequest is idempotent: for a ci_fix on an existing agent branch it
    // returns the EXISTING MR/PR (no second one, PRD #6); for a fresh ci-fix/pipeline-N
    // or agent/issue-N branch it opens one. Reporting its iid keeps the fix branch
    // watched so the verification sync can stamp the verdict.
    const forge =
      claim.repo.forge_type === "forgejo"
        ? this.forgejo
        : claim.repo.forge_type === "github"
          ? this.github
          : this.gitlab;
    // PRD #284 Layer A/D3: wrap the WHOLE createMergeRequest call (POST → duplicate
    // → findOpenMr GET) in the retry loop, not just its final thrown status. It is
    // already idempotent — on a duplicate it adopts the existing MR/PR — but a
    // transient findOpenMr failure after a duplicate POST would otherwise fail a run
    // whose MR actually exists; retrying the whole call re-runs it instead.
    const mr = await withForgeRetry(
      () =>
        forge.createMergeRequest({
          repoUrl: claim.repo.url,
          pat: claim.secrets.forge_pat,
          sourceBranch: result.branch,
          targetBranch,
          title: mrTitle(claim, result.scopeCapped),
          description: mrDescription(
            claim,
            result.branch,
            result.agentSelection,
            selfImproveSection,
            promptGuardSection,
            result.gatesUnverified,
            result.gatesDiscoveryTruncated,
            result.scopeCapped,
          ),
        }),
      { log: runLog },
    );
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: { text: `merge request opened: !${mr.iid} ${mr.webUrl}` },
    });

    await batcher.close();
    // Persist the MR/PR web URL the forge just handed us (PRD #65 D8), so the web
    // links it directly instead of reconstructing the URL by string surgery. Omit
    // it when the forge returned none (mr.webUrl empty) so the server lands NULL and
    // the legacy forgeUrls.ts reconstruction still applies (R8, additive+optional).
    // prd_done_path (PRD #72 M4) rides the same terminal report. Omitted when the
    // executor set nothing — same `|| undefined` shape as mr_web_url on this line,
    // so "old worker" and "moved no PRD" are indistinguishable on the wire by
    // design, and the api treats both as NULL.
    // PRD #265 M1: the finished-milestone ids the lead declared on signal_done ride the
    // same terminal report. Omitted when the executor set nothing (non-issue run, or the
    // lead declared none) — same absent-vs-present discipline as prd_done_path, so the
    // server UNIONs them into milestones_completed only when actually declared and a
    // no-declaration completion is byte-identical to before.
    await reportState({
      status: "completed",
      branch: result.branch,
      mr_iid: mr.iid,
      mr_web_url: mr.webUrl || undefined,
      prd_done_path: result.prdDonePath,
      milestones_completed: result.milestonesCompleted,
      // PRD #634 M3: stamp the scope-capped disposition so the server records
      // stop_kind='scope_capped'. OMITTED (not false) on a normal completion, so the wire
      // shape is unchanged for every non-truncated run.
      scope_capped: result.scopeCapped ? true : undefined,
    });
    runLog.info("run completed", { branch: result.branch, mr_iid: mr.iid });
  }

  private buildFlight(
    claim: ClaimResponse,
    runId: string,
    executor: Executor,
    runHome: string | undefined,
    gitBasic: string,
    runScopedSecrets: string[],
  ): RunFlight {
    const runLog = this.log.child({
      run_id: runId,
      issue_iid: claim.issue_iid,
    });
    // Same secret set for both redactors: the batcher scrubs run_message payloads;
    // redactText scrubs strings that reach the API outside a payload (failure_reason,
    // and the PRD #99 agent_label/agent_instance the batcher now carries alongside
    // the payload — `redact` walks inside a payload object and never sees them).
    const secrets = [
      claim.secrets.forge_pat,
      claim.secrets.anthropic_oauth_token,
      this.joinToken,
      gitBasic,
    ];
    const redact = makeRedactor(secrets);
    const redactText = makeTextRedactor(secrets);
    const batcher = new MessageBatcher(
      this.client,
      runId,
      claim.last_seq,
      this.batchMs,
      runLog,
      redact,
      redactText,
    );

    // Cancel/shutdown spans the whole run; a `cancel` input aborts it via the
    // steering channel, which the executor's ctx.signal watches.
    const cancel = new AbortController();
    // PRD #41: `notify` lets the steering channel post a feed notice when it discards a
    // verdict/revision written against a stale plan version — wired to the batcher here
    // so the channel never reaches into runner internals.
    const steering = new SteeringChannel(
      this.client,
      runId,
      this.pollMs,
      runLog,
      cancel,
      {
        notify: (text) =>
          batcher.emit({ kind: "status", agent: "worker", payload: { text } }),
      },
    );
    // issue #552 M3: a graceful `uzi run stop` (PRD #517 M4) consumed into the worker's
    // stopRequested flag is lost if the worker dies before winding the park down. The
    // server re-delivers the durable runs.stop_kind='stopped' fact as claim.stop_pending
    // on every claim; seeding it here reconstructs the sticky stop state so the resumed
    // run's interactive park ends `stopped` immediately (steering.awaitFollowUp's arm-time
    // check) instead of waiting out the idle timeout. Absent ⇒ untouched (today's path).
    if (claim.stop_pending) steering.seedStopRequested();

    // Last SDK session id the executor observed; carried on EVERY state report so
    // resume survives a lost report.
    // Returns the server's acknowledgement (PRD #35), which every caller here still
    // ignores. Widened from Promise<void> so the park branch can read it without
    // re-plumbing this closure; the annotation is the only change, and awaiting a
    // value nobody binds behaves exactly as before.
    const flight: RunFlight = {
      runId,
      executor,
      runHome,
      runScopedSecrets,
      runLog,
      redact,
      redactText,
      batcher,
      cancel,
      steering,
      observedSessionId: undefined,
      reportState: (body) =>
        this.client.reportState(
          runId,
          flight.observedSessionId
            ? { ...body, session_id: flight.observedSessionId }
            : body,
        ),
      barePath: undefined,
      worktreePath: undefined,
      // PRD #267: time-based origin-checkpoint gate state (per run). `lastPublish` starts
      // at run start so the first time-based publish fires ~one interval in; both are
      // updated by ANY origin publish (milestone or time). Decision 9: the publish "new
      // work" test keys on `lastPublishedTip`, NOT the fetch-skip below.
      lastPublish: this.now(),
      lastPublishedTip: undefined,
      // PRD #1062 M2 (#1036): the checkpoint ref tip this run last knows, for the overlay's
      // prevCheckpointTip. Seeded from the persisted tip on the claim (a resume already has an
      // overlay on the ref), advanced on every confirmed publish.
      lastCheckpointRefTip: claim.checkpoint_tip ?? undefined,
      lastAttemptedCheckpointRefTip: undefined,
      // issue #1030: per-run dedupe set for checkpoint-publish outcome feed lines.
      reportedPublishOutcomes: new Set<string>(),
      // PRD #218 M1: the run's branch, hoisted so the park/shutdown fetch-back in the
      // catch can name it. `runnerClone` is declared inside the try and there is no
      // `result` on those paths, so `runnerClone.branch` is the source of truth and it is
      // copied here the moment the clone exists.
      branch: undefined,
      // PRD #218 M1: this run's shutdown-registry entry, hoisted so the catch can read
      // `active.shuttingDown` to tell a graceful shutdown apart from every other failure.
      active: undefined,
      // PRD #35: set ONLY by a park the server acknowledged as `limit_wait`. It gates
      // the two filesystem removals in the finally and nothing else. Declared here
      // rather than in the catch so the finally can see it; false is the safe default,
      // so every path that never reaches the park logic cleans up exactly as before.
      parked: false,
      // PRD #556 M1: set ONLY by a worker-shutdown interrupt (the `active?.shuttingDown`
      // catch branch). Like `parked`, it gates EXACTLY the two filesystem removals in the
      // finally (the sibling skills plugin dir and the per-run HOME) and nothing else — so
      // a same-worker re-claim within the affinity grace can resume the SDK session. It is
      // a distinct flag from `parked` on purpose: `parked` also drives park-only report
      // semantics, resume seeding, and the park log, none of which apply to a shutdown.
      preserveSession: false,
      ciFixHumanApproved: false,
      runnerClone: undefined,
      result: undefined,
    };

    // PRD #108 M3: the batcher's breaker reports OUT OF BAND, never through itself —
    // `concat` is order-preserving, so an emitted explanation would queue behind the
    // poison that tripped it and never land. reportState has bounded retries,
    // 4xx-fatal semantics, and treats an already-terminal server response as
    // success, so if the run has already reported terminal this is a safe no-op
    // rather than a second, racing terminal report. Fire-and-forget: the batcher's
    // trip path must never block on the network.
    batcher.onPermanentFailureReport(({ reason }) => {
      void flight
        .reportState({
          status: "failed",
          failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN),
        })
        .catch((e) =>
          runLog.error("could not report the message-transport failure", {
            error: errMessage(e),
          }),
        );
    });

    return flight;
  }

  private async phaseClone(claim: ClaimResponse, flight: RunFlight): Promise<void> {
    const { runLog, reportState, steering, batcher, cancel } = flight;
    const runId = claim.run_id;
    runLog.info("run claimed", {
      repo: claim.repo.url,
      branch: claim.branch ?? null,
    });
    await reportState({ status: "running" });
    steering.start();

    const barePath = (flight.barePath = await this.git.ensureClone(
      claim.repo.clone_url,
      claim.secrets.forge_pat,
      claim.secrets.forge_username,
    ));
    const runnerClone = (flight.runnerClone = await this.runnerCloneForClaim(barePath, claim));
    flight.worktreePath = runnerClone.path;
    flight.branch = runnerClone.branch;
    // PRD #218 M1: register for the shutdown fetch-back now that a clone exists to
    // fetch from. The late-register guard covers the race where shutdown() already
    // fired before this run reached here — abort it at once so it does not run to
    // completion past the grace window.
    const active: ActiveRun = (flight.active = { cancel, shuttingDown: false });
    this.activeRuns.set(runId, active);
    if (this.shuttingDownGlobal) {
      active.shuttingDown = true;
      cancel.abort();
    }
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: { text: `runner clone ready on ${runnerClone.branch}` },
    });
  }

  private async phaseResume(
    claim: ClaimResponse,
    flight: RunFlight,
  ): Promise<string | undefined> {
    const { runLog, reportState, batcher, runHome } = flight;
    const runId = claim.run_id;
    const runnerClone = flight.runnerClone!;
    // PRD #218 M3 / #759 M5: say what a resume recovered, in a WORKER status rather than by
    // the lead noticing the tree changed under it. Two axes cross here — WHAT kind of work
    // was recovered, committed vs. uncommitted, and each is honest about a distinct outcome:
    //   - COMMITTED work (priorCommits > 0): the tracking-ref leg (same-worker, its owner
    //     stamp matched THIS run — M2) or the checkpoint leg (cross-worker, #122 M8). The
    //     message says which and names the count.
    //   - UNCOMMITTED WIP (runnerClone.wipRecovered — #759 M2): a `wip(park):` marker was
    //     `reset --soft` back to the working tree, so its edits returned UNCOMMITTED and the
    //     marker never enters the history. priorCommits was computed AFTER that reset (off
    //     the marker's parent), so it NEVER counts the marker — a pure WIP recovery has
    //     priorCommits === 0. This is a PARTIAL, unreviewed snapshot, NOT a committed
    //     milestone, so its wording says to verify it against the plan.
    //   - NOTHING recovered on a RESUME (session id present, seededFrom "default", no WIP):
    //     no origin branch, no tracking ref THIS run owns, no recoverable WIP — a
    //     cross-worker resume (R1) or a diverged checkpoint that could not be applied. The
    //     tree is lost for this run; admit it (the #218 M3 loss notice, unchanged wording).
    //
    // Branch order is load-bearing: the pure-WIP branch (3) MUST precede the loss branch (4).
    // A cross-worker DIVERGED WIP recovery (#759 M2 leg #4) recovers the WIP tree onto the new
    // floor but leaves seededFrom === "default" (the base is the floor, not the checkpoint) with
    // wipRecovered === true. If the loss branch ran first it would FALSELY fire "no earlier work
    // could be recovered" on exactly that successful recovery. With branch 3 ahead of it, the
    // loss notice fires only when NOTHING — committed or WIP — was recovered: the residual
    // #218-M3 loss case.
    const wipRecovered = runnerClone.wipRecovered === true;
    if (runnerClone.seededFrom === "tracking" && runnerClone.priorCommits > 0) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: wipRecovered
            ? `recovered ${runnerClone.priorCommits} commit(s) plus your uncommitted work-in-progress from this run's interrupted attempt`
            : `recovered ${runnerClone.priorCommits} commit(s) of work from this run's interrupted attempt`,
        },
      });
    } else if (runnerClone.seededFrom === "checkpoint" && runnerClone.priorCommits > 0) {
      // PRD #122 M8: seeded off ANOTHER worker's brokered checkpoint (origin's
      // refs/uzi-checkpoints/<branch>) — a cross-worker recovery the lead cannot infer
      // from the tree alone. priorCommits counts what the checkpoint carries. Gated on
      // priorCommits > 0 (like the tracking notice above) so it never claims to have
      // "recovered 0 commit(s) from a checkpoint". #759 M5: a checkpoint can ALSO carry a
      // reset-soft'd WIP tree alongside its commits — mention it when it did.
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: wipRecovered
            ? `recovered ${runnerClone.priorCommits} commit(s) plus your uncommitted work-in-progress from a checkpoint on another worker`
            : `recovered ${runnerClone.priorCommits} commit(s) from a checkpoint on another worker`,
        },
      });
    } else if (wipRecovered) {
      // PRD #759 M5: a PURE uncommitted-WIP recovery — no committed milestones came back
      // (priorCommits === 0). This is the #685 shape and covers same-worker tracking with
      // zero commits, a cross-worker clean checkpoint with zero commits, AND the
      // cross-worker DIVERGED cherry-pick leg (seededFrom stays "default"/origin). The
      // recovered content is an UNCOMMITTED, PARTIAL snapshot from an interrupted attempt,
      // NOT a committed milestone — so tell the agent to verify it against the plan before
      // building on it. This branch precedes the loss notice so the diverged case
      // (seededFrom === "default" + wipRecovered) never falsely reports total loss.
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text:
            "recovered your uncommitted work-in-progress from this run's interrupted attempt — " +
            "a partial snapshot, so verify it against the plan before continuing",
        },
      });
    } else if (claim.session_id != null && runnerClone.seededFrom === "default") {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text:
            "no earlier work could be recovered for this run on this worker — " +
            "starting from the default branch, so some work may be repeated",
        },
      });
    }
    // PRD #122 M8: a mirrored checkpoint existed but DIVERGED from origin, so origin won
    // and the checkpointed work was set aside. Independent of the seed leg (the base is
    // origin/default here, not the checkpoint) — say so LOUDLY rather than dropping it
    // silently.
    if (runnerClone.checkpointSetAside) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "checkpointed work was set aside — origin diverged; starting from origin",
        },
      });
    }

    // PRD #628 M4: signal the server to CLEAR this run's stale milestones_completed when
    // the reseed recovered NO committed work (seededFrom === "default", equivalently
    // priorCommits === 0). milestones_completed is a monotone union server-side, so pass-1's
    // milestones would otherwise read as "done" while pass-2 re-implements them from the
    // default branch — the "marked done but still working on them" symptom. This is a
    // DEDICATED, ONE-SHOT run-start report: the field rides THIS report only and NEVER a
    // reportIteration heartbeat (the server clears on it, so emitting it per-heartbeat would
    // wipe live progress every iteration). AWAITED because correctness depends on delivery
    // (reportState has bounded retries) — but best-effort in spirit: it must not fail the run,
    // so a failed report is logged and swallowed. Keyed on the TREE signal, NOT the session
    // signal (RESUME_LINEAGE_BREAK_EVENT) — the two diverge once M2 lands, and a re-claim that
    // recovers the tree via checkpoint (seededFrom "checkpoint") legitimately keeps its
    // milestones. Gated on claim.session_id != null (a RESUME), mirroring the tree-loss
    // feed message at ~:603: a brand-new first attempt has no prior milestones to clear,
    // so it must NOT emit a spurious extra run-start report (that perturbs the status-report
    // sequence other runner tests assert, e.g. runner-push-mr).
    if (claim.session_id != null && runnerClone.seededFrom === "default") {
      try {
        await reportState({ status: "running", seeded_from_default: true });
      } catch (e) {
        runLog.warn("could not report seeded_from_default (milestone reset signal)", {
          error: errMessage(e),
        });
      }
    }

    // Resume preflight (issue #105). The claim carries the session id the run last
    // reported, but the transcript it names lives under the per-run HOME on the
    // worker that WROTE it — and a requeued run whose affinity grace lapsed can land
    // on a different worker, where it does not exist. Not only cross-worker: a session
    // is keyed by HOME *and* cwd, so a replaced volume or a changed clone path loses
    // it on the same box. The SDK resolves a resume locally, so an unresolvable id
    // does not start fresh: it fails the very first turn with `error_during_execution`
    // and takes the whole run with it. If it is gone, drop the resume and SAY so —
    // continuing without the earlier context beats losing the run, but only if the
    // feed admits the context is gone rather than quietly re-treading ground.
    //
    // The check globs this HOME's project dirs rather than computing the one the cwd
    // encodes to (sdk-session.ts explains why the computed path would false-absent on
    // a symlinked data dir). A per-run HOME holds exactly one project dir — this run's
    // own clone — so the glob is precise here regardless.
    //
    // Only when this run HAS a private HOME: the stub executor has none (main.ts),
    // which is exactly the "no SDK session to resume" case, so the e2e stub flow is
    // untouched by construction rather than by an executor-kind check here.
    // PRD #88: seed the open question id from the claim. The server re-delivers it
    // from the runs row on every resume, so a worker that picks up a run parked
    // before a death re-parks on the SAME question rather than minting a new id —
    // which is what keeps an answer the user already submitted valid. Without this
    // seeding the identity guard would still be keyed on identity, but the identity
    // itself would change across the requeue, reproducing exactly the silent
    // rejection the clock-based designs were rejected for.
    if (claim.open_question_id)
      this.openQuestionIds.set(runId, claim.open_question_id);

    let sessionId = claim.session_id ?? undefined;
    if (
      sessionId &&
      runHome &&
      !(await sessionTranscriptResolvable(runHome, sessionId, runLog))
    ) {
      sessionId = undefined;
      runLog.warn(
        "resume session transcript is not resolvable here; starting a fresh SDK session",
        {
          run_home: runHome,
          event: RESUME_LINEAGE_BREAK_EVENT,
        },
      );
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          // Says what is true without over-claiming a cause: the usual one is a
          // re-claim by another worker, but the same box loses it too if the cwd
          // changed or the volume was replaced. Both facts the reader needs are
          // stated — the context is gone, AND that is why work may be re-tread.
          text:
            "this run was picked up again, but its earlier session could not be found on this worker — " +
            "continuing WITHOUT its earlier context, so some work may be repeated",
          event: RESUME_LINEAGE_BREAK_EVENT,
        },
      });
    } else if (claim.session_id != null && sessionId != null) {
      // PRD #556 M2 (D5) — the positive resume signal, mutually exclusive with the
      // lineage-break branch above. This fires only when a REAL prior session existed
      // (claim.session_id != null — a resume, not a fresh first attempt and not a
      // seeded run whose claim.session_id is null) AND it RESOLVED on THIS worker's
      // HOME (the preflight above did NOT clear sessionId). Because the guard above
      // requires `runHome` to even attempt resolution, sessionId survives with no
      // runHome (stub executor / e2e) too — but there the resume is a no-op with no
      // transcript to resolve, so guarding on the same runHome condition keeps this
      // silent there, matching the lineage-break guard's intent.
      if (runHome) {
        runLog.info(
          "resume session transcript resolved here; continuing the prior SDK session",
          {
            run_home: runHome,
            event: RESUME_CONTINUED_EVENT,
          },
        );
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text:
              "this run was picked up again and its earlier session was found on this worker — " +
              "continuing WITH its prior context (no re-plan)",
            event: RESUME_CONTINUED_EVENT,
          },
        });
      }
    }
    return sessionId;
  }

  private async phasePreflightHandoff(
    claim: ClaimResponse,
    flight: RunFlight,
    sessionId: string | undefined,
  ): Promise<ExecutorResult> {
    const { runLog, reportState, batcher, steering, cancel, executor } = flight;
    const runId = claim.run_id;
    const runnerClone = flight.runnerClone!;
    const barePath = flight.barePath;
    // Tell the lead whenever the branch it is standing on already carries commits it did
    // not make this turn, so the honest degradation never becomes silently duplicated work.
    // PRD #218 M3 widened the condition to `priorCommits > 0` alone: it previously fired
    // ONLY when the resume was dropped, which missed the case where the session survives but
    // the TREE was recovered from the tracking ref (or is prior pushed work), where the lead
    // equally needs to know its branch is not empty. A fresh run reading its own empty
    // first-attempt branch counts 0 and gets nothing.
    //
    // PRD #209 D7: this note normally reaches the lead in the PLANNING prompt, but a
    // session-less SEEDED run has no planning turn, so it rides the IMPLEMENT prompt instead
    // (threaded on the pre-approved path) — otherwise a requeued seeded run whose transcript
    // was dropped would re-implement cold on a branch that already carries pushed commits,
    // with no prior-work note.
    const priorWork =
      runnerClone.priorCommits > 0
        ? { commits: runnerClone.priorCommits }
        : undefined;

    // PRD #209 (D4): a SEEDED run's plan was authored by the user at create time, so
    // it is approved with NO server-side approve_plan input and — on a fresh seeded
    // run — no SDK session. That is a legitimate "approved, no session" state, NOT the
    // dropped-transcript one the `&& sessionId` guard below protects against: there
    // was never a session to lose. The runner is the layer that can tell the two apart
    // (it holds the preflight result), so it folds `seeded` into planApproved here
    // rather than leaving the executor to read the claim.
    const seeded = claim.plan_source === "seeded";

    // PRD #209 M4 — staleness guard. A seeded run carries the commit the user planned
    // against (claim.planned_base_commit). evaluateBaseStaleness compares it to the
    // clone's resolved base (runnerClone.baseCommit, the same field forwarded into
    // RunContext below): only a seeded run sets planned_base_commit, so an ordinary run
    // yields undefined and proceeds silently. On a divergence it either returns a warning
    // to emit (default) or, under --require-base (claim.require_base_match), THROWS
    // BaseCommitDivergedError BEFORE any implement work — the generic catch-all then
    // fails the run, so it never implements against a diverged base (Open Question 3).
    const staleWarning = evaluateBaseStaleness(
      claim.planned_base_commit ?? undefined,
      runnerClone.baseCommit,
      claim.require_base_match ?? false,
    );
    if (staleWarning) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: staleWarning },
      });
    }

    // PRD #37: parse the checked-out repo's own agent roster and report it on
    // this first post-checkout `running` state report. It rides the STATE report
    // rather than the gate so that an autopilot run — which never parks at
    // awaiting_approval — records what was detected just the same. The roster is
    // inert data: nothing is assembled until a selection picks the repo source.
    const detection = await this.parseRepoAgents(
      runnerClone.path,
      batcher,
      runLog,
    );
    const repoAgents = detection.agents;
    if (detection.ok) {
      // Non-fatal, and fire-and-forget (matching the session-id report below): an
      // INFORMATIONAL roster report must never fail a run. An older API without the
      // repo_agents field 400s DisallowUnknownFields, which the client treats as
      // permanent — awaiting the report here would turn a working run into a failed
      // one over a field the run does not depend on. On a detection FAILURE we send
      // no roster at all, so the column stays NULL ("not reported") rather than `[]`
      // ("scanned, found none") — the two must stay distinguishable.
      void reportState({
        status: "running",
        repo_agents: repoAgentSummaries(repoAgents),
      }).catch((e) =>
        runLog.warn("could not report repo agent roster", {
          error: errMessage(e),
        }),
      );
    }

    // PRD #84 M4: infer the run's requirement set from the checked-out clone with the
    // deterministic scan, ONCE, best-effort. A scan failure must never break a run, so
    // it is caught and yields `undefined` ("emit nothing"). The result rides the same
    // two reports as `milestones` — the CANDIDATE set on the awaiting_approval report and
    // the FROZEN set on the autopilot running report — threaded through gatePlan below.
    let toolchainDetection: ToolchainDetection | undefined;
    try {
      toolchainDetection = await detectToolchain(runnerClone.path);
    } catch (err) {
      runLog.warn("toolchain detection failed; continuing without a requirement set", {
        error: errMessage(err),
      });
    }

    // Cross-run memory (PRD #90): fetch this run's (user, repo) memory so the
    // executor can compose it into the lead's plan prompt as inert, nonce-fenced,
    // untrusted-advisory context. Guarded HARD — a fetch failure (older API, repo-
    // less run 404/409, transport error) or an empty store injects NOTHING and
    // never fails the run: memory is advisory, never load-bearing.
    let memory: Awaited<ReturnType<WorkerClient["getMemory"]>> = [];
    try {
      memory = await this.client.getMemory(runId);
    } catch (err) {
      runLog.warn("could not fetch cross-run memory; continuing without it", {
        error: errMessage(err),
      });
    }

    // PRD #71 M5: did a HUMAN approve this ci_fix plan? On a PRE-APPROVED RESUME the gate
    // does not run this execution, so derive from durable claim state. The server CLEARS
    // auto_approve the moment a run PARKS at the plan gate (SetRunAwaitingApproval,
    // symmetric with the seeded plan_source='agent' decouple), so a resumed run still
    // carrying auto_approve=true was AUTO-approved WITHOUT ever parking (no human), while
    // auto_approve=false means either a manual run or an auto run that parked and was
    // human-approved. A fresh run's gate closure overwrites this precisely.
    flight.ciFixHumanApproved = (claim.auto_approve ?? false) !== true;

    // PRD #759 M4: resume a provably-reviewed approved run without re-plan/re-gate on a
    // dropped-session cross-worker resume. plan_source==='agent' is a POSITIVE allowlist
    // (worker-authored-but-gated; #209 D8 makes plan_source track plan_md provenance) — do
    // NOT write `!== "seeded"`, which fails OPEN on any future unreviewed provenance value.
    // humanApproved is computed FRESH from claim.auto_approve here (NOT reused from the
    // mutable ciFixHumanApproved, which the gate closure reassigns): the server clears
    // auto_approve when a run parks at the plan gate, so auto_approve!==true ⟺ a human saw
    // the gate. recoveryFailed ⟺ the reseed recovered NOTHING: no committed tree AND no WIP
    // snapshot. seededFrom "default" ALONE is not loss — the diverged cross-worker cherry-pick
    // leg recovers the WIP onto the advanced floor with seededFrom "default" + wipRecovered
    // true (ADR-0759; the reseed-feed block above special-cases the same pair as a WIP
    // recovery, not total loss). So wipRecovered is folded in here too — without it a
    // human-approved run whose WIP came back cleanly on the diverged leg would FALSELY re-gate.
    // The re-gate fallback (FLAG D) needs BOTH: an autopilot recovery-failed resume has no
    // human at the gate to protect, but a human-approved recovery-failed run RE-GATES so the
    // human notices the lost tree — #209's loss-detection gate, kept exactly where #209 put it.
    const humanApproved = (claim.auto_approve ?? false) !== true; // false ⟺ a human saw the gate
    const recoveryFailed =
      runnerClone.seededFrom === "default" && runnerClone.wipRecovered !== true; // no tree AND no WIP
    const m4ResumeReviewedPlan =
      (claim.plan_approved ?? false) &&
      claim.plan_source === "agent" &&
      !!claim.plan_md?.trim() &&
      !sessionId &&
      !seeded && // the D4-row-3 dropped-session case
      !(humanApproved && recoveryFailed); // re-gate a human-approved run that lost its tree

    // PRD #1064 M1 (Decision 1): ONE per-run promise chain serializing EVERY `running`
    // report of the run — the immediate `reportProgress` pushes, the turn-boundary
    // `reportIteration`, and the checkpoint report. `reportState` has bounded retries, so a
    // late immediate push (`in_progress: [m2]`) can sit in its retry loop while the next
    // turn's `reportIteration` (`in_progress: []`) is issued; without serialization the two
    // reads could reach the api out of order and resurrect a finished milestone as
    // in-progress. Chaining every report through this single variable makes the reports
    // reach the api in the order they were made. The chain itself NEVER rejects (each link
    // is isolated below) so one failed report cannot break the ordering of later ones.
    let runningReportChain: Promise<unknown> = Promise.resolve();
    const enqueueRunningReport = <T>(task: () => Promise<T>): Promise<T> => {
      // Run `task` after the current tail settles, regardless of its outcome (pass the same
      // handler to both arms — the tail never rejects, but this is belt-and-braces). The
      // returned promise carries THIS task's result (and rejection) for a caller that awaits
      // it; the tail we keep is made non-rejecting so the next enqueue never inherits a
      // rejection from this one.
      const run = runningReportChain.then(task, task);
      runningReportChain = run.then(
        () => undefined,
        () => undefined,
      );
      return run;
    };

    const ctx: RunContext = {
      runId,
      kind: resolveRunKind(claim.kind),
      issueIid: claim.issue_iid,
      issueTitle: claim.issue_title,
      issueDescription: claim.issue_description,
      // PRD #381: carry the snapshotted issue-comment set onto the ctx; the SDK
      // executor threads it to buildPlanPrompt for nonce-fenced rendering. Absent/
      // null ⇒ nothing is rendered.
      issueComments: claim.issue_comments,
      // PRD #700 M4: carry the mr_rework run's snapshotted MR review comments onto
      // the ctx, exactly like issueComments; the SDK executor threads it to
      // buildPlanPrompt for nonce-fenced rendering. Absent/null (every non-mr_rework
      // kind) ⇒ nothing is rendered.
      reviewComments: claim.review_comments,
      pipeline: claim.pipeline,
      worktreePath: runnerClone.path,
      branch: runnerClone.branch,
      // PRD #501 REC B: thread the autopilot flag to plan-build time so the plan
      // builders render the no-human-in-the-loop note. Absent ⇒ false.
      autoApprove: claim.auto_approve ?? false,
      // PRD #517 M3: an interactive task run parks at awaiting_followup on signal_done
      // instead of finalizing (see the awaitFollowUp callback below). Absent ⇒ false, so
      // every non-interactive run (and every older server) is byte-identical to today.
      interactive: claim.interactive ?? false,
      // The seed resolved this in the bare; forwarding it is what stops the lead from
      // guessing the branch's parent (judge rec, run 51757591).
      baseCommit: runnerClone.baseCommit,
      defaultBranchCommit: runnerClone.defaultBranchCommit,
      emit: (m) => batcher.emit(m),
      oauthToken: claim.secrets.anthropic_oauth_token,
      // PRD #362 M3c: the run-summary model resolved server-side (user-value-wins),
      // and whether the intent summary is already set so the executor skips
      // re-generating it on a resume/re-claim (Decision 3). Both absent on older
      // servers, which the summary hooks tolerate (undefined model → account default,
      // false present → generate).
      summaryModel: claim.summary_model ?? undefined,
      summaryIntentPresent: claim.summary_intent_present ?? false,
      agents: claim.agents,
      repoAgents,
      skills: claim.skills,
      skillsDropped: claim.skills_dropped,
      repoSkillsEnabled: claim.repo.skills_enabled ?? false,
      repoClaudemdEnabled: claim.repo.claudemd_enabled ?? false,
      memory,
      // Issue #297: the self_improve in-flight avoid-set (best-effort; absent ⇒ empty).
      inflightTargets: claim.inflight_targets,
      // PRD #686 D11: the open self-improve MRs' "what was proposed" text (best-effort;
      // absent ⇒ empty, so the picker's non-overlap block is simply omitted).
      openSelfImproveMRs: claim.self_improve_open_mrs,
      // PRD #686 M4: dogfood flag (absent ⇒ generic; older server never sets it).
      selfImproveDogfood: claim.self_improve_dogfood,
      config: claim.config,
      // PRD #1064 M1 (Decision 2): the claim's FROZEN milestone list, the transition-frame
      // title fallback on a pre-approved resume whose loop-scope frozen list is undefined.
      frozenMilestones: claim.milestones,
      // Preflighted above: the claim's id, or undefined when its transcript is not
      // on this worker (issue #105).
      sessionId,
      priorWork,
      // issue #222: this run executed before (it reported a session), so this claim's
      // reseed wiped whatever an earlier attempt left in the tree. Read the RAW
      // claim.session_id, not the `sessionId` var cleared above on a dropped transcript —
      // a dropped-session resume still had its tree destroyed. Same discriminator the
      // reseed feed-status uses (this.emit "starting from the default branch" above).
      resumed: claim.session_id != null,
      // PRD #35 Decision 6b + PRD #209 D4. The RUNNER is the only layer that knows
      // all the facts, which is why it resolves them here rather than the executor
      // reading the claim: the server said the plan is approved, and EITHER a session
      // id arrived that issue #105's transcript check did NOT drop (`sessionId` is
      // cleared above when it did) OR the run is SEEDED. Passing plan_approved through
      // on a dropped-session NON-seeded run would make the executor skip planning for a
      // run whose session is gone — the one case (D4 row 3) where it must re-plan. A
      // seeded run (D4 row 2) is approved with no session by construction, and that is
      // fine because there was never a session to lose.
      planApproved:
        (claim.plan_approved ?? false) &&
        (!!sessionId || seeded || m4ResumeReviewedPlan),
      // PRD #759 M4: a provably-reviewed cross-worker resume skips the re-gate and
      // embeds the persisted plan body on the implement turn. Drives the relaxed
      // preApproved session guard and the embedSeededPlan reviewedResume term.
      reviewedPlanResume: m4ResumeReviewedPlan,
      // PRD #759 M2: the reseed recovered an uncommitted WIP snapshot (a wip(park):
      // marker reset --soft back to the tree), so a cold resumed lead is told to treat
      // the dirty tree as mid-edit and reconcile it against the plan (R1).
      wipRecovered: runnerClone.wipRecovered,
      // PRD #209: the executor relaxes its own session guard for a seeded run and
      // emits the "plan supplied externally" feed line off this.
      seeded,
      approvedPlan: claim.plan_md ?? undefined,
      // The persisted selection, replayed on the claim (PRD #35). Passed through
      // unconditionally rather than gated on planApproved: it is the run's
      // selection whether or not this particular resume skips the gate, and the
      // executor only reads it on the path that has no verdict to supply one.
      approvedSelection: claim.agent_selection,
      signal: cancel.signal,
      // Persist the SDK session id the moment the executor learns it, so a
      // re-queued run can resume it. Best-effort.
      onSessionId: (sessionId) => {
        flight.observedSessionId = sessionId;
        void reportState({ status: "running" }).catch((e) =>
          runLog.warn("could not persist session id", {
            error: errMessage(e),
          }),
        );
      },
      // The plan gate: surface the plan, post awaiting_approval, and return the
      // verdict the steering channel resolves (bounded so an abandoned plan
      // fails rather than wedging the worker). An autopilot claim short-circuits
      // to an approve verdict (see gatePlan) — the run never parks at the gate.
      gatePlan: async (planMd, milestones, onAwaitingApproval) => {
        // PRD #71 M5: a CI-config-classified ci_fix plan must NOT take the auto-approve
        // short-circuit — it parks for human review even on an auto-triggered run. We
        // force the gate by passing autoApprove=false for that case; gatePlan is otherwise
        // unchanged. Non-ci_fix and code-plan ci_fix runs keep today's behavior exactly.
        const forceGate = claim.kind === "ci_fix" && isCIConfigPlan(planMd);
        const effectiveAutoApprove = (claim.auto_approve ?? false) && !forceGate;
        const verdict = await this.gatePlan(
          runId,
          planMd,
          milestones,
          batcher,
          steering,
          reportState,
          runLog,
          effectiveAutoApprove,
          repoAgents,
          toolchainDetection,
          // PRD #212: the runner clone path for the gate's runner-uid `git status`.
          // Use runnerClone.path (const, string), NOT the `worktreePath` local
          // (string | undefined — does not narrow in this closure).
          runnerClone.path,
          onAwaitingApproval,
        );
        // Human-in-the-loop iff the plan reached an approve verdict via the PARK path
        // (not the auto short-circuit). Read by the pre-push guard below.
        flight.ciFixHumanApproved = verdict.kind === "approve" && !effectiveAutoApprove;
        return verdict;
      },
      // PRD #88 clarification park: surface the question, post awaiting_input, and
      // return the answer the steering channel resolves. An autopilot claim
      // short-circuits to a sentinel answer (see askUser) — such a run never parks.
      askUser: (questions) =>
        this.askUser(
          runId,
          questions,
          batcher,
          steering,
          reportState,
          runLog,
          claim.auto_approve ?? false,
          claim.config ?? null,
        ),
      pullFollowUp: () => steering.pullFollowUp(),
      // PRD #517 M3: the interactive-task follow-up park. The executor calls this after a
      // clean signal_done on an interactive run (it has already checkpoint-pushed): report
      // awaiting_followup, verify the park took, then BLOCK on the steering channel until
      // the next follow-up (or an idle/cancel end).
      //
      // CONSUME-BEFORE-REPORT ORDERING (the server's SetRunRunning wake guard): the report
      // of `running` that un-parks the run is the loop's NEXT reportIteration, which the
      // executor only reaches AFTER this resolves with a follow-up. steering.awaitFollowUp
      // resolves that follow-up ONLY from the poll loop's post-route service step, i.e.
      // after ConsumeRunInputs stamped consumed_at — so a consumed follow_up input always
      // exists before `running` is reported, and the server admits the wake. This mirrors
      // askUser's settle discipline; the ordering is satisfied by construction here.
      awaitFollowUp: async (idleMs) => {
        // issue #552 M1 (mid-turn wake-guard bug): report `awaiting_followup` — which stamps
        // the open_followup_id watermark — ONLY when the run is genuinely going idle. If a
        // follow-up (or stop/cancel) arrived mid-turn and is already buffered, reporting the
        // park would fold that already-consumed-but-not-yet-applied follow-up INTO the
        // watermark, so its own wake `running` report would then fail the server's
        // `id > watermark` guard and strand a live run at awaiting_followup. Skip the park
        // report in that case and service the buffered outcome directly (the run stays
        // `running`, no spurious park). A follow-up arriving AFTER this point is consumed
        // after the stamp, so its id > watermark and it wakes normally.
        //
        // issue #559 M2: the park report now CARRIES the watermark it wants stamped —
        // `open_followup_id` = the highest follow_up id the worker has already DELIVERED
        // (steering.getLastDeliveredFollowUpId()). The server clamps/floors this instead of
        // deriving MAX(consumed follow_up id) itself. This closes the residual race where a
        // follow-up consumed by the poll loop DURING this report's DB round-trip would fold
        // into a server-derived MAX(consumed) and strand the run: the last-DELIVERED id does
        // NOT advance during the round-trip (the follow-up waiter is armed only AFTER this
        // report returns, below), so the racing follow-up is excluded and its later wake wins.
        if (!steering.hasPendingFollowUpOutcome()) {
          // Read the ACK the same way askUser and the limit park do: the park TOOK only if
          // the server reports `awaiting_followup`. SetRunAwaitingFollowup (M2) matches
          // nothing when the run went terminal under us or is no longer ours (or is not an
          // interactive task) — without this check the worker would block on a follow-up no
          // surface can produce, since the status never changed. Fail loudly instead.
          const ack = await reportState({
            status: "awaiting_followup",
            open_followup_id: steering.getLastDeliveredFollowUpId(),
          });
          const parked = (ack as { status?: string } | undefined)?.status;
          if (parked !== "awaiting_followup") {
            throw new Error(
              `${REASON_FOLLOWUP_NOT_PARKED} (server reports ${parked ?? "an unreadable status"})`,
            );
          }
          runLog.info("interactive task: awaiting follow-up", { run_id: runId });
        } else {
          // issue #559 M3: the SKIP path. A follow-up (or stop/cancel) is already buffered,
          // so we deliberately do NOT report awaiting_followup (that would fold the
          // not-yet-applied follow-up into the watermark — the #558/#552 fix above) and
          // service the buffered outcome directly. But skipping the park report ALSO skips
          // the ACK that, on the non-skip path, caught a mid-turn reclaim or terminal
          // transition (status != awaiting_followup → throw). Restore that ownership/
          // terminality check cheaply with a read-only ownership probe.
          //
          // ONLY a DEFINITIVE answer throws: a terminal status, or a definitive NOT-OWNED
          // (HTTP 404 → reclaimed by another worker). A TRANSIENT error (network / 5xx /
          // anything that is neither a 404 nor a terminal status) logs a warning and
          // PROCEEDS — the non-skip path's reportState has bounded retries and never fails
          // the run on a transient blip, and the run self-heals anyway at the next
          // ACK-checked park report plus the SetRunRunning worker_id pin. We must not
          // introduce a new spurious-failure mode, so a transient probe error is not one.
          let ownershipStatus: string | undefined;
          try {
            ownershipStatus = (await this.client.getRunOwnership(runId)).status;
          } catch (err) {
            if (err instanceof RequestError && err.status === 404) {
              throw new Error(
                `${REASON_FOLLOWUP_NOT_PARKED} (server reports the run is not owned by this worker)`,
              );
            }
            // Transient (network / 5xx / unreadable): proceed and let the next park
            // report's ACK + the SetRunRunning worker_id pin be the backstop.
            runLog.warn(
              "interactive task: ownership probe failed transiently on the follow-up skip path; proceeding",
              { run_id: runId, error: String(err) },
            );
          }
          if (ownershipStatus !== undefined && FOLLOWUP_TERMINAL_STATUSES.has(ownershipStatus)) {
            throw new Error(
              `${REASON_FOLLOWUP_NOT_PARKED} (server reports ${ownershipStatus})`,
            );
          }
        }
        return steering.awaitFollowUp(idleMs);
      },
      // PRD #122 M2: carry the lead's live progress into the `running` report and return
      // the server-computed effective budget from the ack. Async (unlike M4's fire-and-
      // forget void) so the loop can apply the budget — but still fire-and-forget in
      // spirit: reportState has bounded retries, and the try/catch here guarantees a
      // failed report returns undefined ("no budget update") rather than failing the run.
      reportIteration: async (iteration, progress) => {
        try {
          // PRD #1064 M1: enqueue onto the per-run chain so any still-in-flight immediate
          // push (from the prior turn's scan loop) is sent BEFORE this turn-boundary report
          // — awaiting the chain here is what guarantees a pending push is never dropped and
          // never overtakes this snapshot.
          const ack = await enqueueRunningReport(() =>
            reportState({
              status: "running",
              iteration_count: iteration,
              // Omit the fields entirely when the lead has reported no progress, so the
              // wire shape matches an old worker's (additive-optional, never null/[]).
              ...(progress
                ? {
                    milestones_completed: progress.completed,
                    milestones_in_progress: progress.in_progress,
                  }
                : {}),
            }),
          );
          const b: IterationBudget = {};
          if (typeof ack.budgetMaxIterations === "number")
            b.maxIterations = ack.budgetMaxIterations;
          if (typeof ack.budgetWallSeconds === "number")
            b.wallSeconds = ack.budgetWallSeconds;
          // PRD #634 M2: carry the operator scope ceiling + fresh completed count off the
          // ACK so m3's loop-top gate can read them off `served`.
          if (typeof ack.scopeCeiling === "number")
            b.scopeCeiling = ack.scopeCeiling;
          if (typeof ack.completedCount === "number")
            b.completedCount = ack.completedCount;
          // Without the scope/completed fields in this return guard, a non-budget-scaled
          // run's ACK (no budget fields) would return `undefined` and m3's loop-top gate at
          // `if (served)` would never see the ceiling. This is behavior-preserving for the
          // existing budget logic — sdk-executor.ts's budget block type-checks each field
          // individually, so making `served` truthy more often is inert for budget (it only
          // newly enables m3's scope read).
          return b.maxIterations !== undefined ||
            b.wallSeconds !== undefined ||
            b.scopeCeiling !== undefined ||
            b.completedCount !== undefined
            ? b
            : undefined;
        } catch (e) {
          runLog.warn("could not report iteration", { error: errMessage(e) });
          return undefined;
        }
      },
      // PRD #1064 M1 (Decision 1): push the observed milestone progress to the server the
      // MOMENT the scan loop sees a `report_progress` signal — a `running` report carrying
      // the milestone fields and NO `iteration_count`, exactly the shape the checkpoint
      // closure below already sends mid-run (Resolved facts: `SetRunRunning` only RAISES
      // `iteration_count` via GREATEST and its WHERE guard makes a late push a no-op on a
      // parked/cancelled/finished run, so this is safe).
      //
      // ENQUEUE-AND-RETURN: the report is chained onto the per-run running-report chain (so
      // it serializes with the turn-boundary `reportIteration` and the checkpoint report)
      // and this returns IMMEDIATELY — the scan loop must never block on a network report.
      // A failure is logged and swallowed; an informational field never fails a run.
      reportProgress: (progress) => {
        void enqueueRunningReport(() =>
          reportState({
            status: "running",
            milestones_completed: progress.completed,
            milestones_in_progress: progress.in_progress,
          }),
        ).catch((e) =>
          runLog.warn("could not report progress", { error: errMessage(e) }),
        );
        return Promise.resolve();
      },
      // PRD #122 M6: durably checkpoint the run's committed milestone work MID-RUN
      // (Decisions 6, 7, 10, 10b). It is the SAME credential-free fetch-back the done and
      // park paths use (#218's fetchAgentBranch), fired at a milestone boundary so a hard
      // crash loses at most "since the last milestone" rather than the whole run.
      //
      // REAP-BEFORE-GIT is the load-bearing invariant (B1/M4 audit): when reaping, the
      // agent tree is killed BEFORE any CREDENTIALED git runs, so a survivor cannot read a
      // credential out of a git child's /proc/environ — the same ordering the done path
      // uses (killAgentTree before fetchBackBestEffort). The pre-reap tip reads below
      // (branchTip/trackingTip) are credential-free local rev-parse, and the only
      // credential-bearing-CLASS op here is the fetch-back, itself credential-free
      // (file://, no PAT) — so a future credentialed git op MUST stay after the reap.
      // Best-effort throughout: a checkpoint must NEVER fail the run.
      checkpoint: async (opts) => {
        // `barePath` is the outer `let` (string | undefined); it is set before the run
        // reaches the executor, but narrow it so the closure is honest rather than `!`.
        if (!barePath) return;
        // Decision 6 tip-movement check: has the runner clone's branch tip moved since the
        // last checkpoint wrote the tracking ref? A null trackTip (never checkpointed) or a
        // null cloneTip (unresolvable) is NOT a match, so it falls through to a real fetch.
        const cloneTip = await this.git.branchTip(runnerClone.path, runnerClone.branch);
        const trackTip = await this.git.trackingTip(barePath, runnerClone.branch);
        const tipUnmovedSinceFetch =
          trackTip !== null && cloneTip !== null && trackTip === cloneTip;

        // Reap on EVERY model-cooperative checkpoint (Decision 10b), STRICTLY before ANY git
        // below — both the credential-free fetch-back and the #1036 overlay's PAT-bearing
        // fetchDefaultTip. The reap is tied to opts.reap (the real "safe to reap" signal: the
        // agent's turn has ALREADY ENDED), NOT to the fetch: the fetch-back is credential-free
        // but the M2 overlay is not, so REAP-BEFORE-GIT requires the reap precede it here,
        // regardless of tip movement. The fallback (reap:false) must NOT reap: a backgrounded
        // dev server the lead means to reuse next iteration must survive. The done path
        // likewise reaps before its fetch-back.
        if (opts.reap) executor.killAgentTree?.();

        // Skip ONLY the fetch when there is nothing new to fetch — do NOT return, so the
        // origin-publish gate below still runs (Decision 9: a commit fetched at an earlier
        // iteration can become publish-eligible on a later tip-unmoved iteration once the
        // interval opens).
        if (!tipUnmovedSinceFetch) {
          // Fetch back, credential-free (#218's helper): brings the committed work into
          // refs/uzi-runner/<branch> where the reseed reads it. Best-effort, never fails.
          await this.fetchBackBestEffort(
            barePath,
            runnerClone.path,
            runnerClone.branch,
            runId,
            runLog,
          );
        } else {
          runLog.info("checkpoint fetch skipped: branch tip unmoved since last checkpoint", {
            run_id: runId,
            branch: runnerClone.branch,
          });
        }

        // PRD #267: origin-publish gate. The publish is CREDENTIAL-FREE (a pack brokered to
        // the api via publishCheckpoint, no PAT — publishCheckpointBestEffort -> git.checkpointPack
        // (local objects) -> client.publishCheckpoint (worker join token)), so it is safe on the
        // reap:false path with the agent tree ALIVE. PRD #122 Decisions 10b/14 dissolved the
        // reap/publish coupling for the broker (it was a property of the rejected worker-side
        // push, not a correctness invariant); reap:false originally did not publish purely for
        // scope + broker cost, which the time-gate now bounds (<=1 publish/interval/run).
        //   - reap:true  (milestone): publish whenever there is new committed work not yet on
        //     origin. Behaviourally equivalent to the old always-publish minus a redundant
        //     re-publish of an already-published tip.
        //   - reap:false (iteration boundary, PRD #267): publish only when the time-gate is open
        //     AND there is new committed work. "new work" keys on lastPublishedTip (Decision 9),
        //     NOT the fetch-skip above, so a commit that then goes idle for >= the interval still
        //     ships exactly once.
        const hasNewWork = cloneTip !== null && cloneTip !== flight.lastPublishedTip;
        const timeGateOpen =
          this.checkpointIntervalMs > 0 &&
          this.now() - flight.lastPublish >= this.checkpointIntervalMs;
        let published = false;
        if (hasNewWork && (opts.reap || timeGateOpen)) {
          // PRD #1062 M2 (#1036): attempt the `.github/workflows` overlay ONLY on the reap:true
          // (milestone) path. On that path the agent tree was ALREADY reaped unconditionally
          // above (opts.reap ⇒ killAgentTree, hoisted before both fetch paths), so the overlay's
          // default-tip fetch — a PAT git op — runs with no live agent, satisfying REAP-BEFORE-GIT
          // (~:2745). The overlay is FORBIDDEN on the reap:false path where the agent is still
          // ALIVE. So reap:false publishes with NO overlay (undefined) — byte-behaviourally unchanged.
          const midRunOverlay = opts.reap
            ? await this.buildCheckpointOverlay(claim, flight, barePath)
            : undefined;
          published = await this.publishCheckpointBestEffort(
            flight,
            barePath,
            runnerClone.branch,
            midRunOverlay,
          );
          // Advance the time-gate on every ATTEMPT (not just success): this bounds broker
          // retry cadence to <= 1 publish/interval/run even under a persistent broker
          // failure, so a failing publish cannot spam every iteration.
          flight.lastPublish = this.now();
          // PRD #267 Fix 1 (Decision 9 / the "worst-case loss ~one interval" criterion):
          // advance lastPublishedTip ONLY on a CONFIRMED landed publish. On failure the tip
          // stays un-advanced, so hasNewWork stays true and the time-gate retries the SAME
          // tip at the next interval boundary — the idle commit still ships (bounded loss),
          // rather than being marked published-and-forgotten by a transient broker failure.
          if (published) {
            flight.lastPublishedTip = cloneTip ?? flight.lastPublishedTip;
            // PRD #267 M3: make the time-based publish observable — the "committed work is
            // now safe on origin" moment for a reap:false checkpoint (the milestone/reap:true
            // publish is already visible via its running report below). Only for the time
            // path so we do not double-log the milestone case.
            if (!opts.reap) {
              runLog.info("checkpoint published to origin (time-based)", {
                run_id: runId,
                branch: runnerClone.branch,
                tip: cloneTip,
              });
            }
          }
        }
        // Report the checkpointed milestone as a `running` report — additive-optional
        // (milestone fields omitted when no progress) and wrapped so it never throws. NO
        // iteration_count: a checkpoint is not an iteration-boundary report, so leaving it
        // out keeps it from regressing the server's GREATEST-merged iteration counter.
        //
        // PRD #267 Fix 2: emit ONLY when there was real activity — a fetch (tip moved since
        // the last checkpoint) OR a publish. On a pure-idle checkpoint (tip unmoved AND
        // nothing published) stay silent, restoring the pre-M1 early-return behaviour while
        // still signalling a time-based publish of an idle tip ("work is now safe on origin").
        if (!tipUnmovedSinceFetch || published) {
          // PRD #1064 M1: enqueue onto the per-run chain so this checkpoint report stays
          // ordered behind any pending immediate `reportProgress` push.
          await enqueueRunningReport(() =>
            reportState({
              status: "running",
              ...(opts.progress
                ? {
                    milestones_completed: opts.progress.completed,
                    milestones_in_progress: opts.progress.in_progress,
                  }
                : {}),
            }),
          ).catch((e) =>
            runLog.warn("could not report checkpoint progress", {
              error: errMessage(e),
            }),
          );
        }
      },
      // Issue #281: a cheap fingerprint of the runner clone's committed + working-tree
      // state for the executor's no-progress detector — the runner-owned clone's branch
      // tip (committed work) plus `git status --porcelain` (uncommitted changes). Both are
      // runner-uid reads of the runner-owned clone (branchTip / worktreeStatus), the same
      // reads the checkpoint closure and the plan gate already do. Returns null when EITHER
      // read fails — an unresolvable tip OR an unreadable status — which the executor treats
      // as "cannot assert unchanged" (no trip). worktreeStatus (not planChangedFiles) is used
      // deliberately: planChangedFiles swallows a failed read to [], which the fingerprint
      // would encode identically to a genuinely clean tree, so a persistently failing status
      // could let the detector trip without the tree ever having been verified (CodeRabbit #655).
      worktreeFingerprint: async () => {
        if (!barePath) return null;
        const tip = await this.git.branchTip(runnerClone.path, runnerClone.branch);
        if (tip === null) return null;
        const dirty = await this.git.worktreeStatus(runnerClone.path);
        if (dirty === null) return null;
        return `${tip}\n${dirty.join("\n")}`;
      },
    };

    // PRD #1064 M1: drain the per-run running-report chain before this phase yields control,
    // so any still-in-flight immediate `reportProgress` push reaches the api BEFORE the
    // terminal report. A final SDK frame can carry both `report_progress` and `signal_done`:
    // the scan loop fires reportProgress FIRE-AND-FORGET (not awaited by the executor), so
    // without this drain the terminal report can land first and the queued `running` push then
    // no-ops against the finished run (SetRunRunning's WHERE guard), losing a milestone
    // completion reported only via report_progress. The chain never rejects (each link is
    // isolated) and each `reportState` has bounded retries, so this await is bounded and cannot
    // reject — it only enforces the ordering D1 already promises.
    //
    // It runs in a `finally` so BOTH terminal paths are covered: on success phasePublish sends
    // `completed` next; on a reject the catch in execute() sends `failed` (or takes the
    // park/requeue branch). Draining on the reject path keeps a late push from no-oping against
    // that terminal report too — the original success-only drain left this leg exposed.
    let result: ExecutorResult;
    try {
      result = await executor.run(ctx);
    } finally {
      await runningReportChain;
    }

    // Reap any agent-backgrounded subprocess BEFORE the PAT touches a git child
    // env — otherwise a survivor could read the PAT from that child's
    // /proc/environ during the push (M4 audit B1). This run's executor reaps only
    // this run's subprocess tree (per-run instance, Decision 4); a concurrent
    // sibling's tree is untouched. The SDK executor also self-reaps in its run()
    // finally; this is the explicit, load-bearing call at the security boundary.
    executor.killAgentTree?.();
    flight.result = result;
    return result;
  }


  /**
   * PRD #218 M1 — trigger the graceful worker shutdown. SYNCHRONOUS and does NO git:
   * it only flips the global flag (so a run claimed during the drain aborts on
   * registration) and aborts every in-flight run's controller, marking each so its
   * catch takes the fetch-back-and-requeue branch. The fetch-backs themselves run
   * inside each `execute()` as it unwinds — deliberately NOT here, because the caller
   * is a signal handler and `controller.abort()` (main.ts) races the async reap chain;
   * a git fetch started here could read a runner-owned clone the run() unwind is still
   * tearing down (R5). The worker's claim-loop drain (`Promise.allSettled(active)`)
   * then waits for those unwinding executes, and process exit is gated on it, so the
   * fetch-backs complete inside the container's termination grace.
   */
  shutdown(): void {
    this.shuttingDownGlobal = true;
    for (const a of this.activeRuns.values()) {
      a.shuttingDown = true;
      a.cancel.abort();
    }
  }

  /**
   * PRD #218 M1 — fetch the agent branch back into the worker bare, best-effort. The
   * park and shutdown paths share this: both need the interrupted attempt's committed
   * work in `refs/uzi-runner/<branch>` (where M2's reseed reads it) before the clone is
   * removed or the container dies. It is the SAME hardened primitive the done path
   * runs, at a different time — no new trust-boundary crossing. A failure is swallowed
   * with a warn (D4): a fetch-back that fails must not undo the park or block the
   * requeue.
   */
  /**
   * PRD #1030 M4: bound a best-effort promise by a client-side budget, resolving to
   * `undefined` when the budget elapses first (never rejecting). Used to cap the
   * graceful-shutdown durability sequence so a slow/unreachable forge cannot hang the
   * shutdown past the k8s termination grace — see the budget note at the shutdown branch.
   * The timer is armed through `this.setTimer` (unref'd) so a pending budget never keeps
   * the event loop alive, and is cancelled the moment `work` settles so a completed
   * publish leaves no dangling timer.
   */
  private async raceShutdownBudget<T>(
    work: Promise<T>,
    budgetMs: number,
  ): Promise<T | undefined> {
    let cancelTimer: (() => void) | undefined;
    const timeout = new Promise<undefined>((resolve) => {
      cancelTimer = this.setTimer(() => resolve(undefined), budgetMs);
    });
    try {
      return await Promise.race([work, timeout]);
    } finally {
      cancelTimer?.();
    }
  }

  private async fetchBackBestEffort(
    barePath: string,
    worktreePath: string,
    branch: string,
    runId: string,
    runLog: Logger,
  ): Promise<void> {
    await this.git.fetchAgentBranch(barePath, worktreePath, branch, runId).catch((e) =>
      runLog.warn("fetch-back on interruption failed; work may not be recoverable", {
        error: errMessage(e),
      }),
    );
  }

  /**
   * PRD #122 M8 — broker the checkpoint pack to origin, best-effort. Mirrors
   * fetchBackBestEffort for symmetry: compute the delta pack of
   * `<origin|default>..refs/uzi-runner/<branch>` (null ⇒ nothing to publish) and ship it
   * to the api's publish RPC, which lands it at `refs/uzi-checkpoints/<branch>` for another
   * worker to recover cross-worker. Fired on BOTH the milestone (reap:true) checkpoint and
   * the PRD #267 time-gated (reap:false) checkpoint, always AFTER the fetch-back updated the
   * tracking ref checkpointPack reads. Every non-success — a null pack (nothing to publish),
   * a non-2xx ({@link PublishResult} `ok: false`), a best-effort skip, or a thrown error — is
   * swallowed (never fails the run) but, since issue #1030, is also SURFACED: a non-2xx,
   * skip, or thrown error emits a deduped run-feed line (see {@link reportPublishOutcome}); a
   * null pack stays silent because there was genuinely nothing to publish.
   *
   * Returns `true` IFF the publish confirmably LANDED (a 2xx whose body reports
   * `published: true`), so the caller can advance `lastPublishedTip` only on confirmed
   * success (PRD #267 Fix 1): a swallowed failure returns `false`, leaving the tip
   * un-advanced so `hasNewWork` stays true and the time-gate retries at the next interval
   * boundary.
   *
   * issue #1030: a failure or a best-effort SKIP is no longer swallowed silently. A non-2xx
   * (HTTP <code>), a 2xx `{ published: false, skipped: <reason> }`, or a thrown error each
   * emits a `status` line onto the run feed and a `runLog.warn`, deduped per distinct
   * outcome per run (see {@link reportPublishOutcome}). A null pack (nothing to publish —
   * an absent tracking ref or an unmoved tip) is NOT a failure and stays silent.
   */
  /**
   * PRD #1062 M2 (#1036): build the `.github/workflows` overlay context for a checkpoint publish,
   * or undefined when no overlay must be attempted. Undefined for every non-GitHub forge (the
   * overlay closes a GitHub-only `workflow` scope gap) — so `checkpointPack` behaves exactly as
   * today off GitHub. `defaultBranch` is resolved the way the finalize align resolves it. Callers
   * pass the result ONLY on paths where the agent tree is already reaped: the overlay's default
   * fetch is a PAT git op, forbidden while the agent is alive (the REAP-BEFORE-GIT invariant).
   */
  private async buildCheckpointOverlay(
    claim: ClaimResponse,
    flight: RunFlight,
    barePath: string,
  ): Promise<CheckpointOverlayContext | undefined> {
    if (claim.repo.forge_type !== "github") return undefined;
    const defaultBranch =
      claim.repo.default_branch?.trim() ||
      (await this.git.defaultBranchName(barePath)) ||
      "main";
    return {
      defaultBranch,
      pat: claim.secrets.forge_pat,
      cloneUrl: claim.repo.clone_url,
      username: claim.secrets.forge_username,
      // issue #1086 (F2): chain from the ATTEMPTED tip when one is pending (an ambiguous prior
      // publish), else the CONFIRMED tip, so the next overlay descends the broker's actual ref
      // whether or not the prior push landed.
      prevCheckpointTip: flight.lastAttemptedCheckpointRefTip ?? flight.lastCheckpointRefTip,
    };
  }

  private async publishCheckpointBestEffort(
    flight: RunFlight,
    barePath: string,
    branch: string,
    overlay?: CheckpointOverlayContext,
  ): Promise<boolean> {
    // issue #1086 (F2): two-tip reconciliation. The CONFIRMED tip advances only on a real ACK; an
    // ambiguous result (non-2xx, or a throw after the pack tip is known) records the ATTEMPTED tip
    // so the next overlay chains from it. Caveat: under PERSISTENT consecutive ACK loss the
    // confirmed tip never advances while attempted marches forward, so each pack re-includes the
    // whole un-landed overlay chain; this is bounded by the broker's object cap and self-clears on
    // the first landed ACK.
    let packedTip: string | undefined;
    try {
      const packed = await this.git.checkpointPack(barePath, branch, overlay);
      if (!packed) return false; // nothing to publish — not a failure, stay silent
      packedTip = packed.tipOid;
      const res = await this.client.publishCheckpoint(flight.runId, packed.tipOid, packed.pack);
      if (res.ok && res.body.published === true) {
        // PRD #1062 M2 (#1036): a CONFIRMED publish advances the known checkpoint ref tip to the
        // declared tip (the overlay `O_ov`, or realTip on the no-overlay path), so the NEXT
        // overlay carries it as parent[0] (base-first) and stays a fast-forward the broker takes.
        flight.lastCheckpointRefTip = packed.tipOid;
        // issue #1086 (F2): a confirmed publish reconciles the broker's ref, so clear any pending
        // attempted tip — the confirmed tip is now authoritative.
        flight.lastAttemptedCheckpointRefTip = undefined;
        return true;
      }
      if (res.ok) {
        // A 2xx that did NOT publish: a best-effort server-side skip. Name the reason.
        // issue #1086 (F2): record NOTHING as attempted — the server definitively did not advance
        // its ref, so the next overlay must keep chaining from the confirmed tip.
        const reason = res.body.skipped ?? "unknown";
        this.reportPublishOutcome(flight, `skip:${reason}`, `checkpoint publish skipped: ${reason}`);
      } else {
        // issue #1086 (F2): a non-2xx is AMBIGUOUS — the broker may have accepted the push before
        // the ACK was lost — so record the attempted tip for the next overlay to chain from.
        flight.lastAttemptedCheckpointRefTip = packed.tipOid;
        this.reportPublishOutcome(
          flight,
          `http:${res.httpStatus}`,
          `checkpoint publish failed: HTTP ${res.httpStatus}`,
        );
      }
      return false;
    } catch (e) {
      // issue #1086 (F2): a throw is AMBIGUOUS too, but only after the pack tip was obtained — a
      // throw DURING checkpointPack leaves packedTip undefined and records nothing.
      if (packedTip !== undefined) flight.lastAttemptedCheckpointRefTip = packedTip;
      this.reportPublishOutcome(flight, "error", `checkpoint publish failed: ${errMessage(e)}`);
      return false;
    }
  }

  /**
   * issue #1030: make a checkpoint-publish failure or skip VISIBLE. Always `runLog.warn`s
   * the outcome; emits a run-feed `status` line at most ONCE per distinct outcome key per
   * run, so the ~20-min time-gated retry of a persistently-failing publish does not spam the
   * feed. Reuses the same `batcher.emit({ kind: "status", agent: "worker", … })` mechanism
   * every other worker-authored status line uses.
   */
  private reportPublishOutcome(flight: RunFlight, key: string, text: string): void {
    flight.runLog.warn(text, { run_id: flight.runId });
    if (flight.reportedPublishOutcomes.has(key)) return;
    flight.reportedPublishOutcomes.add(key);
    flight.batcher.emit({ kind: "status", agent: "worker", payload: { text } });
  }

  /**
   * Handle a usage-limit death (PRD #35). Returns whether the run is PARKED, which
   * is the sole input to the cleanup carve-out above.
   *
   * 🔴 THE ANSWER IS `status === "limit_wait"`, NEVER `applied`.
   *
   * "Not parked" has five causes, and three of them are DESIGNED outcomes rather
   * than errors: the retry budget is exhausted, the computed retry_not_before
   * exceeded RUN_LIMIT_MAX_PARK, or wait_on_limit is false and the server coerced
   * the report. On all three the server FAILS THE RUN AND ANSWERS 200 — a
   * transition was applied, just not the requested one — so `applied` is true while
   * the run is emphatically not parked. Since budget exhaustion is the ordinary end
   * of a run that keeps hitting limits, an `applied`-keyed branch would leak the
   * clone, the plugin dir and up to ~170 MB of HOME on the most common cause of all.
   *
   * Testing one literal positively also makes the default arm "clean up", so an
   * unforeseen cause, a future status, an older server that echoes nothing, and a
   * parse failure all land on the safe side by construction. An enumeration of the
   * five causes would go stale; this cannot.
   *
   * On not-parked the runner sends NO further state report. The server has already
   * decided and recorded the outcome — a `failed` report on top of it would clobber
   * a `cancelled`, and would overwrite the server-composed limit sentence with a
   * worker-composed one.
   *
   * ── Why the feed payload omits Decision 10's `attempt` ───────────────────────
   * The worker CANNOT KNOW IT, and a guess would be wrong by construction. The park
   * count is `runs.limit_wait_count`, which the SERVER increments inside
   * SetRunLimitWait — strictly AFTER this message is emitted, and after the batcher
   * that carries it has been closed. The claim payload does not deliver the previous
   * value either (it carries `requeue_count`, a different counter for worker deaths).
   * So the best a worker could emit is a stale N-1 that disagrees with the run row.
   *
   * Nothing is lost: `limit_wait_count` is on RunDTO and on the web `Run` type, so a
   * renderer showing "attempt N" reads the authoritative value it already has,
   * rather than a snapshot frozen into a feed row that never updates when the run
   * parks again.
   */
  private async handleLimitReached(
    err: LimitReachedError,
    claim: ClaimResponse,
    batcher: MessageBatcher,
    reportState: (body: StateRequest) => Promise<StateAck>,
    runLog: Logger,
  ): Promise<boolean> {
    const detail = describeLimit(err);
    // The structured fields ride BOTH the park and the opt-out failure report. The
    // worker never composes the user-facing sentence from them (Decision 8): it
    // reports the raw type and reset, and the server — which owns the allowlist —
    // writes the reason. Composing it here would carry an unvalidated rateLimitType
    // into the run row as free text by a different route.
    const limitFields = {
      rate_limit_type: err.rateLimitType,
      limit_resets_at: err.resetsAtMs,
    };
    // The feed payload for the two structured kinds (PRD #35 Decision 10). Kept
    // STRUCTURED rather than interpolated into a sentence because the renderer must
    // map `rate_limit_type` through a known-value lookup with a neutral fallback: a
    // feed payload is worker-authored and the server's enum allowlist does not reach
    // it, and you cannot map a value that has been baked into prose without
    // re-parsing the prose — which is worse than not structuring it at all.
    //
    // `resets_at` is an ISO string, matching every other timestamp the web renders
    // and sidestepping the seconds-vs-milliseconds ambiguity that `resetsAt` itself
    // carries. OMITTED ENTIRELY when the reset is unknown, never null, so "unknown"
    // is one shape on the wire rather than two.
    //
    // `attempt` from Decision 10 is deliberately ABSENT — see the note in
    // handleLimitReached's doc comment.
    const feedPayload: Record<string, unknown> = {};
    if (err.rateLimitType !== undefined)
      feedPayload["rate_limit_type"] = err.rateLimitType;
    if (err.resetsAtMs !== undefined)
      feedPayload["resets_at"] = new Date(err.resetsAtMs).toISOString();

    if (!claim.wait_on_limit) {
      // Opted out: fail, but with the structured facts attached so the server can
      // say WHY instead of leaving today's bare "agent run failed:
      // error_during_execution".
      runLog.info("run hit a usage limit and is not opted in to waiting", {
        detail,
      });
      batcher.emit({
        kind: "limit_hit",
        agent: "worker",
        payload: { ...feedPayload },
      });
      await batcher.close().catch(() => undefined);
      // PRD #69 M7a: this opt-out failure is definitionally rate-limit-caused, so
      // stamp the trusted class alongside the structured limit fields. (The server
      // also stamps 'rate_limited' on the parallel limit_wait→non-park path, so the
      // class is recorded regardless of which report shape reached it.)
      await reportState({
        status: "failed",
        fail_origin: "rate_limited",
        ...limitFields,
      }).catch((e) =>
        runLog.error("could not report limit failure", {
          error: errMessage(e),
        }),
      );
      return false;
    }

    runLog.info("run hit a usage limit; requesting a park", { detail });
    batcher.emit({
      kind: "limit_wait",
      agent: "worker",
      payload: { ...feedPayload },
    });
    // issue #1030: FLUSH (not close) here so the limit_wait feed line lands before the state
    // report, exactly as the prior close did — but leave the batcher OPEN so the park
    // durability block in execute() can emit the checkpoint-publish outcome onto the feed.
    // execute()'s park block closes the batcher once, on every parked run. The two
    // non-park returns below still CLOSE it themselves, since execute() emits nothing more
    // on those paths.
    await batcher.flush().catch(() => undefined);

    let ack: StateAck;
    try {
      ack = await reportState({ status: "limit_wait", ...limitFields });
    } catch (e) {
      // The park request never landed. Clean up: this run is not parked, and a
      // preserved HOME nothing will ever claim is an unbounded leak.
      runLog.error(
        "could not report the park; cleaning up as an unparked run",
        { error: errMessage(e) },
      );
      await batcher.close().catch(() => undefined);
      return false;
    }

    if (ack.status !== "limit_wait") {
      runLog.warn("the server did not park this run; cleaning up", {
        applied: ack.applied,
        server_status: ack.status ?? "unknown",
      });
      await batcher.close().catch(() => undefined);
      return false;
    }
    return true;
  }

  /**
   * Parse the clone's `.claude/agents/*.md` (PRD #37), logging every skipped or
   * clamped file to the run stream. Detection is best-effort by construction: a
   * repo without the directory has no agents (ok: true, agents: []), and an
   * enumeration FAILURE (e.g. an unreadable dir) is reported as a warning and a run
   * message with ok: false — neither is ever a run failure, because a repo's
   * optional roster must not be able to stop the run it was detected for.
   *
   * The `ok` flag is what keeps a detection FAILURE distinguishable from a genuinely
   * empty roster at the API: the caller reports `repo_agents: []` only when ok, so a
   * failure leaves the column NULL ("not reported") rather than `[]` ("found none").
   */
  private async parseRepoAgents(
    worktreePath: string,
    batcher: MessageBatcher,
    runLog: Logger,
  ): Promise<{ agents: AgentTemplate[]; ok: boolean }> {
    let detected;
    try {
      detected = await this.detect(worktreePath);
    } catch (err) {
      runLog.warn("repo agent detection failed", { error: errMessage(err) });
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "could not read the repo's .claude/agents/; continuing with your own agent templates",
        },
      });
      return { agents: [], ok: false };
    }
    for (const note of detected.notes) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: describeRepoAgentNote(note) },
      });
    }
    if (detected.agents.length > 0) {
      const names = detected.agents.map((a) => a.name).join(", ");
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `detected ${detected.agents.length} agent(s) in the repo's .claude/agents/: ${names}`,
        },
      });
    }
    runLog.info("repo agents detected", {
      count: detected.agents.length,
      dropped: detected.notes.length,
    });
    return { agents: detected.agents, ok: true };
  }

  /**
   * Seed the run's RUNNER CLONE + resolve its branch (PRD #51 M3, (b)). An issue run
   * works agent/issue-{iid}. A ci_fix run (PRD #6) targets a fresh
   * `ci-fix/pipeline-{id}` branch when fixing the default branch, or the existing
   * agent branch (updating its MR) when fixing a run branch — keyed by pipeline.ref
   * vs the repo's default branch. The working tree lives ONLY in this clone; the
   * worker fetches the agent branch back from it before pushing (fetchAgentBranch).
   */
  private async runnerCloneForClaim(barePath: string, claim: ClaimResponse) {
    // PRD #218 M2: thread the run id as the tracking-ref OWNERSHIP anchor. The git layer
    // stays claim-agnostic — it consults the tracking ref only when its stamp matches
    // this run id, so neither a fresh run nor a different run on the same issue can
    // inherit a dead run's orphan ref. A requeue/resume keeps the same run_id, so a run
    // resuming its OWN parked work matches; every other run does not.
    const runId = claim.run_id;
    // PRD #1030 M3: a SEPARATE, additional signal (not the runId ownership anchor) — a
    // RESUME is `claim.session_id != null` (a run that executed before, re-claimed after a
    // rate-limit park), distinct from a fresh first attempt or a seeded run (session_id
    // null). Threaded into the clone path to relax ONLY the cross-worker checkpoint
    // ancestry test on an unpushed branch (see runnerCloneForBranch): when `main` advanced
    // during the park, a valid mirrored checkpoint diverges from the moved default and the
    // strict-descendant test would discard the committed milestones and cold-start.
    const resume = claim.session_id != null;
    // issue #1042 M4: the OWNER ANCHOR for the cross-worker checkpoint adopt guard — the tip
    // THIS run last published to its own refs/uzi-checkpoints/<branch> ref, persisted
    // server-side on every publish (M2) and delivered on the claim. runnerCloneForBranch
    // adopts a mirrored checkpoint on the resume/unpushed leg ONLY when this equals the
    // checkpoint's current SHA, so a resumed run that never published its own checkpoint
    // cannot seed off a PRIOR (possibly plan-rejected) run's work. `?? undefined` maps the
    // wire's null (a never-published run) to the "do not adopt" sentinel the git layer reads.
    const expectedCheckpointTip = claim.checkpoint_tip ?? undefined;
    // PRD #983 M4b: the per-kind branch derivations (ci_fix's default-branch vs run-branch
    // choice, self_improve/prompt's fresh-per-cycle run-id branch, task/mr_rework's
    // pre-seeded branch with its loud missing-branch guard) live in RUN_KIND_PROFILES. A
    // row that returns undefined — ci_fix with no pipeline, and the issue/chat/judge kinds
    // that have no cloneBranch — falls through to the issue path below, byte-identically.
    const cloneBranch = RUN_KIND_PROFILES[resolveRunKind(claim.kind)].cloneBranch?.(
      claim,
      runId,
    );
    if (cloneBranch)
      return this.git.runnerCloneForBranch(
        barePath,
        cloneBranch.branch,
        cloneBranch.slug,
        runId,
        resume,
        expectedCheckpointTip,
      );
    if (claim.issue_iid == null)
      throw new Error("issue run claim is missing issue_iid");
    return this.git.createOrAttachRunnerClone(barePath, claim.issue_iid, runId, resume, expectedCheckpointTip);
  }

  /** Post awaiting_approval with the plan and await the steering verdict, bounded.
   *  For an autopilot run, the plan is still recorded but the gate resolves with an
   *  approve verdict immediately — no awaiting_approval report, no /inputs wait. It
   *  also resolves + records the run's DEFAULT agent selection (PRD #37 Decision 6),
   *  since a self-approved run never receives an approve_plan input to carry one.
   *
   *  PRD #41 plan revision: this is called ONCE PER ROUND by the executor's gate loop.
   *  A RE-gate (every round after the first) bumps the steering epoch at the re-report,
   *  so a verdict written against the previous plan version goes stale; the first gate
   *  does not bump (epoch 0). Each call then awaits the epoch-aware event. A `revise`
   *  verdict is RETURNED
   *  to the caller — the executor runs a fresh plan turn with the feedback and calls
   *  gatePlan again; approve/reject/cancel are terminal. The 24h approval budget is an
   *  ABSOLUTE deadline computed on the first entry and threaded across rounds, so N
   *  revision rounds share ONE budget rather than resetting the clock each round. The
   *  autopilot short-circuit is unchanged and never returns a revise. */
  private async gatePlan(
    runId: string,
    planMd: string,
    // PRD #122 M1: the CANDIDATE milestone list from the just-submitted plan. Rides
    // the awaiting_approval report (human-gated) or the autopilot running report as
    // the FROZEN list (Decision 2). Additive-optional — only included when non-empty.
    milestones: Milestone[] | undefined,
    batcher: MessageBatcher,
    steering: SteeringChannel,
    // Ignored by the gate, but typed to match the client (PRD #35): a gate that
    // narrowed the return would make the park branch unable to reuse this closure.
    reportState: (body: StateRequest) => Promise<StateAck>,
    runLog: Logger,
    autoApprove: boolean,
    repoAgents: AgentTemplate[],
    // PRD #84 M4: the plan-time inferred requirement set (deterministic detectToolchain()),
    // or undefined when the scan failed. Rides the same two reports as `milestones` — the
    // CANDIDATE set on the awaiting_approval report and the FROZEN set on the autopilot
    // running report — each field emitted only when its array is non-empty.
    toolchainDetection: ToolchainDetection | undefined,
    // PRD #212: the runner clone path (= runnerClone.path), so the gate can run a
    // runner-uid `git status --porcelain` there to surface plan-turn worktree writes.
    worktreePath: string,
    // PRD #362 M3c: advisory hook fired AFTER the awaiting_approval report persists
    // plan_md, BEFORE the verdict wait (see the RunContext.gatePlan doc). Never reached
    // on the autopilot branch, which returns above without persisting plan_md.
    onAwaitingApproval?: (planMd: string) => Promise<void>,
  ): Promise<PlanVerdict> {
    batcher.emit({ kind: "plan", agent: "lead", payload: { plan_md: planMd } });
    // Get the plan message onto the stream regardless of mode — it is the audit
    // record of what the agent intended, autopilot or not.
    await batcher.flush().catch(() => undefined);

    if (autoApprove) {
      // Auto-approve is a VERDICT SOURCE at the existing gate, not a bypass around
      // it: the plan was recorded above; the run just never enters awaiting_approval
      // (no state flicker, no column-automation churn) and never waits on a human.
      // The default selection (repo agents when detected, else the owner's
      // templates) is resolved here, persisted via the running report (the only
      // channel a no-input run has, Decision 6), and stated on the feed. The
      // executor re-resolves the SAME absent-parse to build the identical roster.
      const selection = resolveAgentSelection(
        { status: "absent" },
        repoAgents.length > 0,
      ).selection;
      // Make this report self-contained (F1): carry the roster alongside the selection
      // so both validate + persist atomically, even if the fire-and-forget running
      // roster report above failed and left the column NULL. Gated on length > 0 — on
      // a detection failure repoAgents is [] and the selection resolves to `own`;
      // sending repo_agents: [] would flip NULL ("not reported") to [] ("detected
      // none") and break that deliberate distinction. rosterFor already prefers the
      // reported roster over the column, so this needs no wire change.
      const autopilotState: StateRequest = {
        status: "running",
        agent_selection: selection,
      };
      if (repoAgents.length > 0)
        autopilotState.repo_agents = repoAgentSummaries(repoAgents);
      // PRD #122 M1 (Decision 2): an autopilot run never reports awaiting_approval, so
      // the FROZEN milestone list rides this self-contained running report instead —
      // mirroring the repo_agents conditional above. Only when non-empty (additive-
      // optional, never []). Reporting stays fire-and-forget via the .catch below.
      if (milestones?.length) autopilotState.milestones = milestones;
      // PRD #84 M4: the FROZEN requirement set rides this same self-contained running
      // report (an autopilot run never reports awaiting_approval), each field only when
      // non-empty — same conditional-spread discipline as milestones/repo_agents above.
      Object.assign(autopilotState, toolchainReportFields(toolchainDetection));
      await reportState(autopilotState).catch((e) =>
        runLog.warn("could not persist autopilot agent selection", {
          error: errMessage(e),
        }),
      );
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: autopilotSelectionText(selection, repoAgents.length) },
      });
      runLog.info("plan gate: auto-approved (autopilot)", {
        run_id: runId,
        agent_source: selection.source,
      });
      return { kind: "approve", selection: { status: "ok", selection } };
    }

    // PRD #41 round-awareness (Decision 3): the gate epoch is advanced at the
    // awaiting_approval RE-report — the last step before awaiting a new round — NOT
    // when a revise is taken. A revision planning turn runs BETWEEN rounds; bumping
    // early would leave that whole window at the new epoch, so an approve clicked
    // mid-revision would be accepted at the v2 gate (a plan no human saw). Bumping
    // here, after v2 is reported, stamps such a mid-revision approve at the PRIOR
    // epoch, so it is discarded with a feed notice. The FIRST gate for a run does
    // NOT bump: epoch 0 lets a verdict already queued when the gate opens apply.
    // PRD #122 M1: the CANDIDATE milestone list rides the awaiting_approval report so
    // the human approves the breakdown (Decision 2). Only include the key when
    // non-empty — additive-optional, never `null`/`[]`, so a no-milestone run's report
    // stays byte-for-byte as today. The api freezes candidate→frozen at approve.
    // PRD #212: surface the plan turn's worktree writes at the gate. Runner-uid status,
    // best-effort (planChangedFiles swallows errors → []), computed EVERY round so a
    // revision gate reflects that round's tree (a revert between rounds clears the list).
    const planChangedFiles = await this.git.planChangedFiles(worktreePath);
    await reportState({
      status: "awaiting_approval",
      plan_md: planMd,
      ...(milestones?.length ? { milestones } : {}),
      // PRD #84 M4: the CANDIDATE requirement set rides the awaiting_approval report so
      // the server can gate plan-approval on worker eligibility. Each field only when
      // non-empty (additive-optional), matching the milestones conditional above.
      ...toolchainReportFields(toolchainDetection),
      // PRD #212 (Decision 3): ALWAYS send (empty [] when clean), NOT conditionally
      // spread — so each gate round REPLACES the server's list (M1's COALESCE clears on
      // empty), keeping a revision gate from showing a stale earlier round's writes.
      plan_changed_files: planChangedFiles,
    });
    if (this.gatedRuns.has(runId)) steering.bumpEpoch();
    else this.gatedRuns.add(runId);
    const epoch = steering.currentEpoch();
    runLog.info("plan gate: awaiting approval", {
      run_id: runId,
      gate_epoch: epoch,
    });

    // PRD #362 M3c: plan_md is now persisted (the awaiting_approval report above), so the
    // plan-summary POST's stale-write guard (`plan_md = @expected`) can match. Fire the
    // hook HERE — after persist, before the verdict wait — never on the autopilot branch,
    // which returned above without persisting plan_md. ADVISORY: swallow any throw so a
    // summary failure can never wedge the gate or change the run's outcome (the hook
    // itself also swallows internally; this is belt-and-suspenders).
    if (onAwaitingApproval) {
      try {
        await onAwaitingApproval(planMd);
      } catch (e) {
        runLog.warn("plan summary hook failed", {
          run_id: runId,
          error: errMessage(e),
        });
      }
    }

    // A terminal verdict ends the gate → clear the shared per-run gate state. A revise
    // keeps the shared budget/epoch state running; the re-report above does the bump.
    const settle = (v: PlanVerdict): PlanVerdict => {
      if (v.kind !== "revise") {
        this.gateDeadlines.delete(runId);
        this.gatedRuns.delete(runId);
      }
      return v; // NOTE: no bump here — the awaiting_approval re-report bumps.
    };

    if (this.planApprovalTimeoutMs <= 0)
      return settle(await steering.awaitGateEvent(epoch));

    // One absolute deadline across all revision rounds: set it on the first entry and
    // reuse it, so the per-round timer counts down the REMAINING budget (not a fresh 24h).
    let deadlineAt = this.gateDeadlines.get(runId);
    if (deadlineAt === undefined) {
      deadlineAt = Date.now() + this.planApprovalTimeoutMs;
      this.gateDeadlines.set(runId, deadlineAt);
    }
    const remaining = Math.max(0, deadlineAt - Date.now());
    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<PlanVerdict>((resolve) => {
      timer = setTimeout(
        () => resolve({ kind: "reject", reason: "plan approval timed out" }),
        remaining,
      );
      timer.unref?.();
    });
    try {
      return settle(
        await Promise.race([steering.awaitGateEvent(epoch), timeout]),
      );
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  /**
   * PRD #88 M1 clarification park: emit the question, park the run at
   * awaiting_input, and resolve with the human's answer.
   *
   * Structured to mirror gatePlan, including the ABSOLUTE deadline shared across all
   * of a run's questions. Three things differ, each for a stated reason:
   *
   *  - The question id is minted ONCE per park and REUSED on a resume re-park (seeded
   *    from the claim). Everything about the stale-answer guard rests on that
   *    stability, on both sides of the wire.
   *  - The question message is emitted and flushed BEFORE the state report, so the
   *    question is durable before any surface learns the run parked. A surface that
   *    saw awaiting_input with no question yet would render a park it cannot explain.
   *  - Timeout FAILS the run ("clarification timed out") rather than resolving with a
   *    verdict. The PRD puts a configurable default-action out of scope, so
   *    fail-closed is the fixed choice.
   */
  private async askUser(
    runId: string,
    questions: AskUserQuestion[],
    batcher: MessageBatcher,
    steering: SteeringChannel,
    reportState: (body: StateRequest) => Promise<unknown>,
    runLog: Logger,
    autoApprove: boolean,
    config: ClaimConfig | null,
  ): Promise<AnswerVerdict> {
    if (autoApprove) {
      // Autopilot is "no human in the loop" (PRD #19), so a park would wedge the run
      // until its deadline with nobody to answer. Resolve immediately with a sentinel
      // and DO NOT report awaiting_input — an autopilot run must never enter the
      // parked state at all, which is what a test can actually observe.
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: AUTOPILOT_ANSWER_NOTICE },
      });
      runLog.info("clarification: auto-resolved (autopilot)", {
        run_id: runId,
        questions: questions.length,
      });
      return {
        kind: "answer",
        answers: questions.map(() => AUTOPILOT_SENTINEL_ANSWER),
      };
    }

    const ordinal = (this.questionCounts.get(runId) ?? 0) + 1;
    this.questionCounts.set(runId, ordinal);

    // Reuse the id this run is already parked on (a resume re-parks on the SAME
    // question); mint one only for a genuinely new question.
    let questionId = this.openQuestionIds.get(runId);
    if (questionId === undefined) {
      questionId = randomUUID();
      this.openQuestionIds.set(runId, questionId);
    }

    batcher.emit({
      kind: "question",
      agent: "lead",
      payload: { question_id: questionId, questions },
    });
    // Durable before the park is announced — see the doc comment.
    await batcher.flush().catch(() => undefined);

    // Read the ACK, and read it the way PRD #35 established for its own park:
    // `status === "awaiting_input"`, NEVER `applied`. A declined park can still have
    // applied a different transition, so `applied` is true while the park did not
    // happen — StateAck.applied's own doc says diagnostics only.
    //
    // Why this matters here and not before the merge: pre-#35 reportState returned
    // void, so discarding it was correct. The information now exists, and without
    // this check the two parks in this file would be asymmetric — the limit park
    // verifies, the question park does not — which reads as an oversight rather than
    // a decision.
    //
    // What the check buys: SetRunAwaitingInput matches nothing when the run went
    // terminal under us or is no longer ours. Without it the worker would then await
    // an answer NO SURFACE CAN PRODUCE — the status never changed, so no UI, Slack
    // card or CLI ever shows a question — and the run would sit until
    // QUESTION_TIMEOUT and die claiming a human ignored it. Failing here instead
    // turns a silent 24h wait into an immediate, explained failure.
    //
    // A concurrent cancel is the one decline that resolves itself (the steering
    // channel's sticky `cancelled` flag settles the wait independently), so this is
    // belt-and-braces on that path and the only defence on the others.
    const ack = await reportState({
      status: "awaiting_input",
      open_question_id: questionId,
    });
    const parked = (ack as { status?: string } | undefined)?.status;
    if (parked !== "awaiting_input") {
      this.openQuestionIds.delete(runId);
      throw new Error(
        `${REASON_QUESTION_NOT_PARKED} (server reports ${parked ?? "an unreadable status"})`,
      );
    }
    runLog.info("clarification: awaiting answer", {
      run_id: runId,
      question_id: questionId,
      question_ordinal: ordinal,
    });

    // LOAD-BEARING, and it reads as incidental — hence this note. Dropping the open
    // question id here is what makes each park mint a fresh randomUUID, and that in
    // turn is the ONLY reason SubmitInput's "reject an answer unless the run is
    // awaiting_input" check can be described as belt-and-braces rather than as the
    // primary defence. Its server-side counterparts are the two clears in
    // runtime.sql (SetRunRunning and SetRunAwaitingApproval); this is the worker's
    // half of the same invariant — no resolved id is left behind anywhere.
    const settle = (v: AnswerVerdict): AnswerVerdict => {
      this.openQuestionIds.delete(runId);
      if (v.kind === "answer") {
        // PRD #307: a plan-phase clarification resumes into more PLANNING turns, so
        // neither onSessionId (latched once per run) nor reportIteration (implement
        // loop only) fires again — the run would stay stuck at awaiting_input. Emit a
        // `running` report on the answer so the server runs SetRunRunning, which is the
        // one existing transition that clears open_question_id (its consumed-answer
        // guard already passes: ConsumeRunInputs stamps consumed_at in the same
        // RETURNING statement that handed us this answer). Shared by the implement-phase
        // park too: there this same awaiting_input -> running is the one the loop's next
        // reportIteration would otherwise perform, so it is harmless (and equally relies
        // on the consumed-answer guard — it is NOT a running -> running no-op, because the
        // awaiting_input park report is awaited and persisted before settle runs). NOT
        // emitted on cancel/timeout (timeout throws before settle; cancel is guarded out).
        void reportState({ status: "running" }).catch((e) =>
          runLog.warn("could not report running after clarification", {
            error: errMessage(e),
          }),
        );
      }
      return v;
    };

    const timeoutMs = questionTimeoutMs(config, this.questionTimeoutMs);
    if (timeoutMs <= 0) return settle(await steering.awaitAnswer(questionId));

    let deadlineAt = this.questionDeadlines.get(runId);
    if (deadlineAt === undefined) {
      deadlineAt = this.now() + timeoutMs;
      this.questionDeadlines.set(runId, deadlineAt);
    }
    const remaining = Math.max(0, deadlineAt - this.now());
    let cancel: (() => void) | undefined;
    const timeout = new Promise<never>((_resolve, reject) => {
      cancel = this.setTimer(
        () => reject(new Error(REASON_QUESTION_TIMEOUT)),
        remaining,
      );
    });
    try {
      return settle(
        await Promise.race([steering.awaitAnswer(questionId), timeout]),
      );
    } finally {
      cancel?.();
    }
  }
}

/** PRD #332 / issue #334 — the `run_usage` lineage-break marker. Tagged onto the
 *  worker slog line AND the run-feed status payload emitted when a resume is DROPPED
 *  and the runner starts a FRESH SDK session (the ONLY path that breaks run_usage
 *  resume-lineage). Low-cardinality and stable so a maintainer can aggregate its
 *  frequency — e.g. `count(*) from run_messages where payload->>'event' =
 *  'resume_lineage_break'` — to decide whether #332's deferred Option B is worth a
 *  schema+protocol change. Renaming this literal silently breaks that aggregation. */
export const RESUME_LINEAGE_BREAK_EVENT = "resume_lineage_break";

/** PRD #556 M2 (D5) — the positive counterpart to `resume_lineage_break`. Emitted
 *  when a re-claim RESOLVES its prior SDK session and CONTINUES it (M1 preserves the
 *  per-run HOME on a worker-shutdown interrupt, so a same-worker re-claim within the
 *  affinity grace now finds the transcript and resumes silently — this gives operators
 *  a signal to distinguish "resumed cleanly" from "re-planned"). Purely additive and
 *  mutually exclusive with the lineage-break event; queryable the same way via
 *  `payload->>'event' = 'resume_continued'`. Renaming this literal breaks that query. */
export const RESUME_CONTINUED_EVENT = "resume_continued";

/** The answer an AUTOPILOT run receives instead of parking (PRD #88 Decision 8).
 *  Frozen wording: M5's test asserts it byte-exactly, and the lead is told to record
 *  the assumption precisely so an unattended run's guesses stay auditable. */
export const AUTOPILOT_SENTINEL_ANSWER =
  "no human available — proceed on your best judgment, and note the assumption you made";

const AUTOPILOT_ANSWER_NOTICE =
  "The agent asked a clarifying question, but this is an autopilot run with no human in the loop — it was told to proceed on its best judgment and record the assumption.";

/** Failure reason when a parked run's answer deadline expires (PRD #88 Decision 5a).
 *  Fail-closed: the PRD puts a configurable default action out of scope. */
export const REASON_QUESTION_TIMEOUT = "clarification timed out";

/** The server declined the clarification park (PRD #88). Distinct from a timeout so
 *  an operator can tell "nobody answered" from "the question was never askable" —
 *  the second means the run went terminal or changed hands mid-ask, and no human ever
 *  saw a question to answer. */
const REASON_QUESTION_NOT_PARKED =
  "could not park the run to ask a question";

/** The server declined the interactive-task follow-up park (PRD #517 M3). Mirrors
 *  REASON_QUESTION_NOT_PARKED: the run went terminal or changed hands mid-turn, or the
 *  run is not an interactive task, so the awaiting_followup transition matched nothing and
 *  no surface can produce a follow-up. Fail loudly rather than block on a park that never
 *  took. */
export const REASON_FOLLOWUP_NOT_PARKED =
  "could not park the interactive task to await a follow-up";

/** issue #559 M3: the run statuses that mean the interactive turn must NOT continue —
 *  a run that went terminal under us on the follow-up park-SKIP path. Checked against the
 *  read-only ownership probe's reported status; a terminal answer throws
 *  REASON_FOLLOWUP_NOT_PARKED just as a non-awaiting_followup ACK does on the non-skip path. */
const FOLLOWUP_TERMINAL_STATUSES: ReadonlySet<string> = new Set([
  "completed",
  "failed",
  "cancelled",
]);

/** PRD #84 M4: the additive `StateRequest` fields for a plan-time toolchain detection.
 *  Each array field is included ONLY when non-empty — mirroring the `milestones?.length ?
 *  {milestones} : {}` conditional-spread discipline, so a run that detected nothing (or
 *  whose scan failed and passed `undefined`) reports byte-for-byte as before. `size_class`
 *  is included whenever a detection was computed (it is soft/display-only). */
function toolchainReportFields(
  detection: ToolchainDetection | undefined,
): Partial<StateRequest> {
  if (!detection) return {};
  const fields: Partial<StateRequest> = { size_class: detection.size_class };
  if (detection.required_capabilities.length)
    fields.required_capabilities = detection.required_capabilities;
  if (detection.required_tools.length)
    fields.required_tools = detection.required_tools;
  return fields;
}

/** The effective answer deadline: the server-configured claim value when it is
 *  present and positive, else the worker default. An older server omits it (R8). */
function questionTimeoutMs(
  config: ClaimConfig | null,
  fallbackMs: number,
): number {
  const secs = config?.question_timeout_seconds;
  return typeof secs === "number" && secs > 0 ? secs * 1000 : fallbackMs;
}

/** MR title from the issue snapshot (never empty). */
function mrTitle(
  claim: ClaimResponse,
  scopeCapped?: { completedCount: number; total?: number },
): string {
  // PRD #634 M3: a partial delivery from an operator scope directive is prefixed so the
  // reviewer sees at a glance the MR does not complete the issue.
  const prefix = scopeCapped ? "[partial] " : "";
  const t = claim.issue_title?.trim();
  if (t) return prefix + t;
  // PRD #983 M4b: the per-kind empty-title fallbacks (ci_fix's pipeline line, prompt/
  // task/mr_rework's fixed labels) live in RUN_KIND_PROFILES. A row's undefined — every
  // issue-shaped kind, and ci_fix with no pipeline — takes the `Resolve issue #<iid>`
  // fallback below, never `Resolve issue #null` for the issue-less kinds whose derived
  // issue_title almost always won the trimmed branch above.
  const kindTitle = RUN_KIND_PROFILES[resolveRunKind(claim.kind)].mrTitle?.(claim);
  if (kindTitle !== undefined) return kindTitle;
  return `${prefix}Resolve issue #${claim.issue_iid}`;
}

/** MR body: links + closes the issue (issue run) or links the failing pipeline
 *  (ci_fix, PRD #6), states the primary directive (humans merge), and — when the
 *  run used the repo's own agents (PRD #37 Decision 3b) — a marker so the human
 *  reviewer knows the internal review loop was performed by repo-authored agents,
 *  not by uzi's built-in reviewer. */
function mrDescription(
  claim: ClaimResponse,
  branch: string,
  agentSelection?: { source: AgentSource; agents: string[] },
  selfImproveSection?: string,
  promptGuardSection?: string,
  gatesUnverified?: string[],
  gatesDiscoveryTruncated?: boolean,
  scopeCapped?: { completedCount: number; total?: number },
): string {
  const footer = `Opened automatically by the uzi agent from branch \`${branch}\`. Please review and merge manually — the agent never merges.`;
  const repoMarker =
    agentSelection?.source === "repo"
      ? [
          "",
          "> ⚠️ This run used agent definitions from the repository's own " +
            `\`.claude/agents/\` (${agentSelection.agents.join(", ") || "none"}). The internal ` +
            "review was performed by those repo-authored agents, not by uzi's built-in " +
            "reviewer — review this change accordingly.",
        ]
      : [];
  // PRD #983 M4b: the per-kind MR bodies (self_improve's tracking-issue reference,
  // ci_fix's pipeline body, prompt/task/mr_rework's issue-less bodies) live in
  // RUN_KIND_PROFILES.mrBody, each returning its exact array-`.join("\n")` string from
  // one explicit context bag. A row's undefined — ci_fix with no pipeline, and the
  // issue/chat/judge kinds that carry no mrBody — falls through to the issue body below
  // (the scopeCapped / gates / Closes arm), which stays here as the richest arm.
  const kindBody = RUN_KIND_PROFILES[resolveRunKind(claim.kind)].mrBody?.(claim, {
    branch,
    baseBranch: claim.base_branch?.trim(),
    repoMarker,
    footer,
    selfImproveSection,
    promptGuardSection,
  });
  if (kindBody !== undefined) return kindBody;
  // PRD #634 M3: a partial delivery from an operator scope directive does NOT close the
  // issue — it delivered only the approved slice of milestones — so the closing line is
  // replaced with a partial-delivery statement and a scope-note blockquote is inserted.
  const body = scopeCapped
    ? [
        `Implements part of #${claim.issue_iid} (partial delivery — see the scope note below; this MR does NOT close the issue).`,
        "",
        "> ⚠️ **Partial delivery — operator scope directive.** The operator narrowed this run's",
        `> scope mid-flight. ${scopeCapped.completedCount}${typeof scopeCapped.total === "number" ? ` of ${scopeCapped.total}` : ""} approved milestone(s) were completed and`,
        "> are included here; any remaining milestones were deferred to a follow-up run. Review this",
        "> as a partial implementation — it does not complete the issue.",
        ...repoMarker,
      ]
    : [
        `Implements issue #${claim.issue_iid}.`,
        "",
        `Closes #${claim.issue_iid}`,
        ...repoMarker,
      ];
  const gatesSection = gatesUnverifiedMrSection(gatesUnverified, gatesDiscoveryTruncated);
  if (gatesSection) body.push("", gatesSection);
  body.push("", "---", footer);
  return body.join("\n");
}

/** Issue #293 M2: an "unverified gates" note for the MR body, or "" when every
 *  component's deps installed AND discovery saw the whole tree. Dir names arrive already
 *  clamped (safeDirLabel). The truncation caveat (review F1) fires even when `dirs` is
 *  empty: a capped discovery means components it never reached could be unverified too,
 *  which named dirs alone cannot say. */
function gatesUnverifiedMrSection(dirs?: string[], discoveryTruncated?: boolean): string {
  const named = dirs ?? [];
  if (named.length === 0 && !discoveryTruncated) return "";
  const parts: string[] = [];
  if (named.length > 0) {
    const list = named.map((d) => `\`${d}\``).join(", ");
    parts.push(
      `JS dependencies did not install in: ${list}. Gates that need them (e.g. \`vitest\`, \`knip\`) could not run on this change, so treat those gates as unverified, not passing.`,
    );
  }
  if (discoveryTruncated) {
    parts.push(
      "Dependency discovery stopped at its scan cap, so components beyond it were never checked and their gates may also be unverified.",
    );
  }
  return `> ⚠️ **Quality gates unverified.** ${parts.join(" ")}`;
}

/** Feed text for an autopilot run's resolved default selection (PRD #37 Decision
 *  6). Repo source names the count; own source names the fallback. */
function autopilotSelectionText(
  selection: AgentSelection,
  repoCount: number,
): string {
  return selection.source === "repo"
    ? `autopilot: using the ${repoCount} agent(s) from the repo's .claude/agents/`
    : "autopilot: using your own agent templates";
}
