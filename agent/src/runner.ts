import { randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import type { WorkerClient } from "./client.js";
import type { GitCache } from "./git.js";
import { gitBasicCredential, isWorkflowScopeRejection } from "./git.js";
import type { Executor, RunContext } from "./executor.js";
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
  SELF_IMPROVE_BRANCH,
  type CheckRunner,
} from "./self-improve.js";
import { installJsDeps } from "./js-deps.js";
import { isCIConfigPlan } from "./prompt.js";
import { flagCIConfigPaths, DEFAULT_CI_CONFIG_PATHS } from "./ci-config-guard.js";
import { REASON_PROVISION_FAILED } from "./provision-run.js";
import { REASON_NO_TOKEN } from "./sdk-executor.js";

/** Cap on a reported failure_reason, matching the forge error-body cap
 *  (forge.ts) so a runaway SDK error can't bloat the run row or the stream. */
const MAX_FAILURE_REASON_LEN = 512;

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
    ". The change is valid — land it as a human PR (commit the file yourself with a " +
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
 * PRD #456 M2 — the actionable `failure_reason` for a GitHub run whose branch could not be
 * aligned with the current default branch before the finalize push: its `.github/workflows/**`
 * files are BEHIND the default (main advanced them after this run's clone base), the bot's
 * repo-only PAT cannot push while they differ, and uzi's attempt to merge and then rebase the
 * current default into the branch BOTH conflicted. The run fails without pushing and the
 * agent's diff is preserved (#377's `preserved_patch`) for a human to rebase-and-land.
 *
 * Names the default branch and points at docs/github-bot-setup.md, kept under
 * MAX_FAILURE_REASON_LEN (the default branch name is clamped so the doc link always fits).
 * Exported for a direct length-cap unit test.
 */
export function composeBaseAlignConflictReason(defaultBranch: string): string {
  const db = (defaultBranch || "the default branch").slice(0, 64);
  const reason =
    `This run's branch is behind the default branch (${db}) on .github/workflows files, ` +
    "which uzi's GitHub bot token cannot push while they differ from the default (its scope " +
    "is exactly `repo`, without `workflow`, by design). uzi tried to merge and then rebase the " +
    `current ${db} into the branch to realign those files, but both conflicted, so the run ` +
    "failed without pushing. The work is valid — a human can rebase and land it. " +
    "See docs/github-bot-setup.md. Your diff is preserved below.";
  return reason.slice(0, MAX_FAILURE_REASON_LEN);
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

    // Last SDK session id the executor observed; carried on EVERY state report so
    // resume survives a lost report.
    let observedSessionId: string | undefined;
    // Returns the server's acknowledgement (PRD #35), which every caller here still
    // ignores. Widened from Promise<void> so the park branch can read it without
    // re-plumbing this closure; the annotation is the only change, and awaiting a
    // value nobody binds behaves exactly as before.
    const reportState = (
      body: Parameters<WorkerClient["reportState"]>[1],
    ): ReturnType<WorkerClient["reportState"]> =>
      this.client.reportState(
        runId,
        observedSessionId ? { ...body, session_id: observedSessionId } : body,
      );

    // PRD #108 M3: the batcher's breaker reports OUT OF BAND, never through itself —
    // `concat` is order-preserving, so an emitted explanation would queue behind the
    // poison that tripped it and never land. reportState has bounded retries,
    // 4xx-fatal semantics, and treats an already-terminal server response as
    // success, so if the run has already reported terminal this is a safe no-op
    // rather than a second, racing terminal report. Fire-and-forget: the batcher's
    // trip path must never block on the network.
    batcher.onPermanentFailureReport(({ reason }) => {
      void reportState({
        status: "failed",
        failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN),
      }).catch((e) =>
        runLog.error("could not report the message-transport failure", {
          error: errMessage(e),
        }),
      );
    });

    let barePath: string | undefined;
    let worktreePath: string | undefined;
    // PRD #267: time-based origin-checkpoint gate state (per run). `lastPublish` starts
    // at run start so the first time-based publish fires ~one interval in; both are
    // updated by ANY origin publish (milestone or time). Decision 9: the publish "new
    // work" test keys on `lastPublishedTip`, NOT the fetch-skip below.
    let lastPublish = this.now();
    let lastPublishedTip: string | undefined;
    // PRD #218 M1: the run's branch, hoisted so the park/shutdown fetch-back in the
    // catch can name it. `runnerClone` is declared inside the try and there is no
    // `result` on those paths, so `runnerClone.branch` is the source of truth and it is
    // copied here the moment the clone exists.
    let branch: string | undefined;
    // PRD #218 M1: this run's shutdown-registry entry, hoisted so the catch can read
    // `active.shuttingDown` to tell a graceful shutdown apart from every other failure.
    let active: ActiveRun | undefined;
    // PRD #35: set ONLY by a park the server acknowledged as `limit_wait`. It gates
    // the two filesystem removals in the finally and nothing else. Declared here
    // rather than in the catch so the finally can see it; false is the safe default,
    // so every path that never reaches the park logic cleans up exactly as before.
    let parked = false;
    try {
      runLog.info("run claimed", {
        repo: claim.repo.url,
        branch: claim.branch ?? null,
      });
      await reportState({ status: "running" });
      steering.start();

      barePath = await this.git.ensureClone(
        claim.repo.clone_url,
        claim.secrets.forge_pat,
        claim.secrets.forge_username,
      );
      const runnerClone = await this.runnerCloneForClaim(barePath, claim);
      worktreePath = runnerClone.path;
      branch = runnerClone.branch;
      // PRD #218 M1: register for the shutdown fetch-back now that a clone exists to
      // fetch from. The late-register guard covers the race where shutdown() already
      // fired before this run reached here — abort it at once so it does not run to
      // completion past the grace window.
      active = { cancel, shuttingDown: false };
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

      // PRD #218 M3: say what a resume recovered, in a WORKER status rather than by the
      // lead noticing the tree changed under it. Two honest outcomes:
      //   - the tracking-ref leg fired with commits ahead of default. That leg is reached
      //     ONLY when the ref's owner stamp matches THIS run (M2), so the work provably
      //     belongs to this run's own interrupted attempt — the message says so without
      //     hedging about "an earlier run".
      //   - a RESUME (session id present) fell all the way back to the default branch:
      //     no origin branch and no tracking ref THIS run owns was here — a cross-worker
      //     resume (R1) or a ref that belongs to a different run. The tree is lost for
      //     this run; admit it (the session may still resume with full context, a
      //     separate signal above — this keeps the TREE honest too).
      if (runnerClone.seededFrom === "tracking" && runnerClone.priorCommits > 0) {
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: `recovered ${runnerClone.priorCommits} commit(s) of work from this run's interrupted attempt`,
          },
        });
      } else if (runnerClone.seededFrom === "checkpoint" && runnerClone.priorCommits > 0) {
        // PRD #122 M8: seeded off ANOTHER worker's brokered checkpoint (origin's
        // refs/uzi-checkpoints/<branch>) — a cross-worker recovery the lead cannot infer
        // from the tree alone. priorCommits counts what the checkpoint carries. Gated on
        // priorCommits > 0 (like the tracking notice above) so it never claims to have
        // "recovered 0 commit(s) from a checkpoint".
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: `recovered ${runnerClone.priorCommits} commit(s) from a checkpoint on another worker`,
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
      }
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
      let ciFixHumanApproved = (claim.auto_approve ?? false) !== true;

      const ctx: RunContext = {
        runId,
        kind: claim.kind ?? "issue",
        issueIid: claim.issue_iid,
        issueTitle: claim.issue_title,
        issueDescription: claim.issue_description,
        // PRD #381: carry the snapshotted issue-comment set onto the ctx; the SDK
        // executor threads it to buildPlanPrompt for nonce-fenced rendering. Absent/
        // null ⇒ nothing is rendered.
        issueComments: claim.issue_comments,
        pipeline: claim.pipeline,
        worktreePath: runnerClone.path,
        branch: runnerClone.branch,
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
        config: claim.config,
        // Preflighted above: the claim's id, or undefined when its transcript is not
        // on this worker (issue #105).
        sessionId,
        priorWork,
        // PRD #35 Decision 6b + PRD #209 D4. The RUNNER is the only layer that knows
        // all the facts, which is why it resolves them here rather than the executor
        // reading the claim: the server said the plan is approved, and EITHER a session
        // id arrived that issue #105's transcript check did NOT drop (`sessionId` is
        // cleared above when it did) OR the run is SEEDED. Passing plan_approved through
        // on a dropped-session NON-seeded run would make the executor skip planning for a
        // run whose session is gone — the one case (D4 row 3) where it must re-plan. A
        // seeded run (D4 row 2) is approved with no session by construction, and that is
        // fine because there was never a session to lose.
        planApproved: (claim.plan_approved ?? false) && (!!sessionId || seeded),
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
          observedSessionId = sessionId;
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
            onAwaitingApproval,
          );
          // Human-in-the-loop iff the plan reached an approve verdict via the PARK path
          // (not the auto short-circuit). Read by the pre-push guard below.
          ciFixHumanApproved = verdict.kind === "approve" && !effectiveAutoApprove;
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
        // PRD #122 M2: carry the lead's live progress into the `running` report and return
        // the server-computed effective budget from the ack. Async (unlike M4's fire-and-
        // forget void) so the loop can apply the budget — but still fire-and-forget in
        // spirit: reportState has bounded retries, and the try/catch here guarantees a
        // failed report returns undefined ("no budget update") rather than failing the run.
        reportIteration: async (iteration, progress) => {
          try {
            const ack = await reportState({
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
            });
            const b: IterationBudget = {};
            if (typeof ack.budgetMaxIterations === "number")
              b.maxIterations = ack.budgetMaxIterations;
            if (typeof ack.budgetWallSeconds === "number")
              b.wallSeconds = ack.budgetWallSeconds;
            return b.maxIterations !== undefined || b.wallSeconds !== undefined
              ? b
              : undefined;
          } catch (e) {
            runLog.warn("could not report iteration", { error: errMessage(e) });
            return undefined;
          }
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

          // Skip ONLY the fetch (and reap) when there is nothing new to fetch — do NOT return,
          // so the origin-publish gate below still runs (Decision 9: a commit fetched at an
          // earlier iteration can become publish-eligible on a later tip-unmoved iteration once
          // the interval opens). Reap-before-git is preserved: we reap only on the fetch path,
          // strictly before the credential-free fetch-back.
          if (!tipUnmovedSinceFetch) {
            // Reap ONLY on the model-cooperative checkpoint (Decision 10b), STRICTLY before
            // any CREDENTIALED git — the done path likewise reaps before its fetch-back. The
            // fallback (reap:false) must NOT reap: a backgrounded dev server the lead means to
            // reuse next iteration must survive.
            if (opts.reap) executor.killAgentTree?.();
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
          const hasNewWork = cloneTip !== null && cloneTip !== lastPublishedTip;
          const timeGateOpen =
            this.checkpointIntervalMs > 0 &&
            this.now() - lastPublish >= this.checkpointIntervalMs;
          let published = false;
          if (hasNewWork && (opts.reap || timeGateOpen)) {
            published = await this.publishCheckpointBestEffort(
              barePath,
              runnerClone.branch,
              runId,
              runLog,
            );
            // Advance the time-gate on every ATTEMPT (not just success): this bounds broker
            // retry cadence to <= 1 publish/interval/run even under a persistent broker
            // failure, so a failing publish cannot spam every iteration.
            lastPublish = this.now();
            // PRD #267 Fix 1 (Decision 9 / the "worst-case loss ~one interval" criterion):
            // advance lastPublishedTip ONLY on a CONFIRMED landed publish. On failure the tip
            // stays un-advanced, so hasNewWork stays true and the time-gate retries the SAME
            // tip at the next interval boundary — the idle commit still ships (bounded loss),
            // rather than being marked published-and-forgotten by a transient broker failure.
            if (published) {
              lastPublishedTip = cloneTip ?? lastPublishedTip;
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
            await reportState({
              status: "running",
              ...(opts.progress
                ? {
                    milestones_completed: opts.progress.completed,
                    milestones_in_progress: opts.progress.in_progress,
                  }
                : {}),
            }).catch((e) =>
              runLog.warn("could not report checkpoint progress", {
                error: errMessage(e),
              }),
            );
          }
        },
      };

      const result = await executor.run(ctx);

      // Reap any agent-backgrounded subprocess BEFORE the PAT touches a git child
      // env — otherwise a survivor could read the PAT from that child's
      // /proc/environ during the push (M4 audit B1). This run's executor reaps only
      // this run's subprocess tree (per-run instance, Decision 4); a concurrent
      // sibling's tree is untouched. The SDK executor also self-reaps in its run()
      // finally; this is the explicit, load-bearing call at the security boundary.
      executor.killAgentTree?.();

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
        //   - hasCheckpointRef: origin's checkpoint ref, mirrored into the bare at
        //     clone/fetch time — catches a checkpoint a PRIOR/cross-worker attempt landed.
        // A genuine zero-code run trips NEITHER (nothing committed ⇒ no pack ⇒ no publish),
        // so it still completes report-only below. Refuse loudly, mirroring the
        // undeclared-empty-diff FAIL path, rather than opening a delete-ref capability.
        const publishedCheckpoint =
          lastPublishedTip !== undefined ||
          (await this.git.hasCheckpointRef(barePath, runnerClone.branch));
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
      if ((claim.kind ?? "issue") === "issue") {
        const changedForGuard = await this.git.changedFiles(barePath, trackingRef);
        // changedFiles returns null on diff-FAILURE (keep pushing — fail open) and [] on a
        // CONFIRMED-empty diff. report_only is the sanctioned zero-diff success — a declared
        // report_only already returned above, so reaching here undeclared+empty is the
        // ambiguous case; fail with an actionable reason instead of opening an empty MR.
        if (changedForGuard !== null && changedForGuard.length === 0) {
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

      // Self-improvement MR evidence (PRD #46 Decision 10): with no CI on the uzi
      // repo, the worker itself runs the test suites and flags any guard-critical
      // path the change touched, folding both into the MR description. Best-effort —
      // gathered before the push so the MR opens with its evidence, and a suite that
      // can't run is reported "skipped", never failing the run.
      let selfImproveSection: string | undefined;
      if (claim.kind === "self_improve") {
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
      if (claim.repo.forge_type === "github") {
        // Capture the narrowed bare path in a const the SAME way the push block does
        // (barePath is an outer `let string | undefined` and TS drops the narrowing here).
        const wfBarePath = barePath;
        const changedForWf = await this.git.changedFiles(wfBarePath, trackingRef);
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
            // alignBranchWithDefault; here we preserve the pre-align diff via #377's
            // preserved_patch and fail typed. `trackingRef` still points at the agent's
            // committed work when a merge conflicted first (we re-fetchAgentBranch only after
            // a SUCCESSFUL align), so workflowScopeDiff captures the human-landable diff.
            const defTip = defaultTip;
            const failBaseAlignConflict = async () => {
              const rawPatch = await this.git.workflowScopeDiff(alignBarePath, trackingRef);
              const patch = rawPatch === null ? undefined : redactText(rawPatch);
              batcher.emit({
                kind: "status",
                agent: "worker",
                payload: {
                  text: "could not align the branch with the updated default branch (merge and rebase both conflicted); failing and preserving the diff for a human to land",
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

            batcher.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: "branch is behind the default branch on .github/workflows; aligning before pushing",
              },
            });
            const mergeRes = await alignOp("merge");
            if (mergeRes === "aligned") {
              try {
                await fetchAndPush();
              } catch (e) {
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
                  await fetchAndPush();
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
                await fetchAndPush();
              } else {
                await failBaseAlignConflict();
                return;
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
        await pushToOrigin();
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
            title: mrTitle(claim),
            description: mrDescription(
              claim,
              result.branch,
              result.agentSelection,
              selfImproveSection,
              promptGuardSection,
              result.gatesUnverified,
              result.gatesDiscoveryTruncated,
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
      });
      runLog.info("run completed", { branch: result.branch, mr_iid: mr.iid });
    } catch (err) {
      // PRD #35: a usage-limit death is not an ordinary failure. Handled before the
      // generic path below because that path is terminal in both senses — it reports
      // `failed` and it lets the finally erase the session this run wants to resume from.
      if (err instanceof LimitReachedError) {
        parked = await this.handleLimitReached(
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
        if (parked && barePath && worktreePath && branch) {
          // Belt-and-braces reap before we read the runner-owned clone, matching the
          // done path and the shutdown branch — safe today (the executor's run() finally
          // reaps first) but kept consistent across all three fetch-back sites.
          executor.killAgentTree?.();
          await this.fetchBackBestEffort(barePath, worktreePath, branch, runId, runLog);
        }
      } else if (active?.shuttingDown) {
        // PRD #218 M1 — the worker is shutting down (SIGTERM/SIGINT) and aborted this
        // run mid-flight. The DISCRIMINATOR is the flag, never the error: a user
        // steering-cancel aborts the same controller with the same REASON_CANCELLED and
        // must still fall through to the generic failure below. The run's tree is
        // already reaped (sdk-executor's run() finally kills the agent tree before this
        // catch is entered); killAgentTree here is the belt-and-braces reap at the
        // security boundary before we read the runner-owned clone.
        executor.killAgentTree?.();
        if (barePath && worktreePath && branch) {
          await this.fetchBackBestEffort(barePath, worktreePath, branch, runId, runLog);
        }
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
          err instanceof PlanRejectedError ? err.reason : errMessage(err);
        const reason = redactText(rawReason);
        // PRD #69 M7a: derive the TRUSTED failure class from the RAW reason (before
        // redaction) so a fatal pre-start failure (provisioning / no token) carries a
        // structured origin the judge can key on; an ordinary agent failure maps to
        // undefined and the server defaults it to 'agent_failure'.
        const failOrigin = failOriginForReason(rawReason);
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
      // ── The park carve-out (PRD #35 Decision 6a) ──────────────────────────────
      // EXACTLY two filesystem removals below are skipped for a parked run, and
      // nothing else in this finally is. The two are what a resume needs: the
      // sibling skills plugin dir, and the per-run HOME that holds the resumable
      // SDK transcript. Preserving only one of them would resume into a session
      // missing its plugins or its transcript.
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
      // ⚠ HOW EACH ONE FAILS IF YOU WIDEN `!parked` TO COVER IT, because they do not
      // fail alike and only one of them fails legibly:
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
      if (worktreePath) {
        await this.git.removeRunnerClone(worktreePath).catch((e) =>
          runLog.warn("runner clone cleanup failed", {
            error: errMessage(e),
          }),
        );
      }
      // Tear down the sibling skills plugin dir the executor synthesized (PRD #16
      // M4). It is OUTSIDE the runner clone, so removeRunnerClone does not reach it;
      // leave it and each run leaks a dir. Best-effort, like the clone cleanup.
      if (worktreePath && !parked) {
        await fs
          .rm(skillsPluginDir(worktreePath), { recursive: true, force: true })
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
      if (runHome && !parked) {
        await rmTreeForce(runHome).catch((e) =>
          runLog.warn("run HOME cleanup failed", { error: errMessage(e) }),
        );
      }
      if (parked) {
        // ~170 MB of Go module cache was measured under a single run HOME, so say
        // what is being held and why — an operator reading disk pressure needs to
        // connect it to a parked run rather than to a leak.
        runLog.info(
          "run parked on a usage limit; preserving its plugin dir and HOME for resume",
          {
            run_home: runHome,
          },
        );
      }
    }
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
   * tracking ref checkpointPack reads. Every failure — a null pack, a non-2xx (returned as
   * null by the client), a thrown error — is swallowed with a warn so a publish NEVER fails
   * the run.
   *
   * Returns `true` IFF the publish confirmably LANDED (a non-null publish response), so the
   * caller can advance `lastPublishedTip` only on confirmed success (PRD #267 Fix 1): a
   * swallowed failure returns `false`, leaving the tip un-advanced so `hasNewWork` stays
   * true and the time-gate retries at the next interval boundary.
   */
  private async publishCheckpointBestEffort(
    barePath: string,
    branch: string,
    runId: string,
    runLog: Logger,
  ): Promise<boolean> {
    try {
      const packed = await this.git.checkpointPack(barePath, branch);
      if (!packed) return false;
      const res = await this.client.publishCheckpoint(runId, packed.tipOid, packed.pack);
      return res !== null; // client returns null on any non-2xx / empty body
    } catch (e) {
      runLog.warn("checkpoint publish failed", { error: errMessage(e) });
      return false;
    }
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
    await batcher.close().catch(() => undefined);

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
      return false;
    }

    if (ack.status !== "limit_wait") {
      runLog.warn("the server did not park this run; cleaning up", {
        applied: ack.applied,
        server_status: ack.status ?? "unknown",
      });
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
    if (claim.kind === "ci_fix" && claim.pipeline) {
      const defaultBranch = claim.repo.default_branch?.trim();
      const fixBranch =
        defaultBranch && claim.pipeline.ref === defaultBranch
          ? `ci-fix/pipeline-${claim.pipeline.id}`
          : claim.pipeline.ref;
      return this.git.runnerCloneForBranch(
        barePath,
        fixBranch,
        fixBranch.replace(/\//g, "-"),
        runId,
      );
    }
    if (claim.kind === "self_improve") {
      // The FIXED branch (PRD #46 Decision 10): reused every cycle so the worker's
      // idempotent createMergeRequest extends one open MR rather than opening a new
      // one, and successive cycles are tested together.
      return this.git.runnerCloneForBranch(
        barePath,
        SELF_IMPROVE_BRANCH,
        SELF_IMPROVE_BRANCH.replace(/\//g, "-"),
        runId,
      );
    }
    if (claim.kind === "prompt") {
      // An ad-hoc SCHEDULED prompt run (PRD #241 Decision 10) is repo-ful and
      // ISSUE-LESS — the ci_fix shape, not self_improve (which carries a tracking
      // issue and reuses a FIXED branch across cycles). With no issue_iid there is
      // no agent/issue-{iid} branch to key on, so derive a stable branch from the
      // run id. Each fired prompt run is a distinct run, so `uzi/prompt-{runId}` is
      // unique and collision-free — the worker's idempotent createMergeRequest opens
      // exactly one MR for it (no fixed-branch reuse, unlike self_improve). The
      // run_id also seeds the tracking-ref ownership anchor above.
      const promptBranch = `uzi/prompt-${runId}`;
      return this.git.runnerCloneForBranch(
        barePath,
        promptBranch,
        promptBranch.replace(/\//g, "-"),
        runId,
      );
    }
    if (claim.kind === "task") {
      // PRD #400 M2: a handoff task works a PRE-SEEDED, server-named branch. The CLI
      // (M3) created the run, received the server-assigned `uzi/task/<run-id>` name,
      // and pushed the user's local HEAD to it with the user's own credentials BEFORE
      // this claim — so `claim.branch` is authoritative and already exists on origin.
      // runnerCloneForBranch seeds off `refs/remotes/origin/<branch>` when that ref
      // exists, so the pre-seeded content is picked up automatically. A task MUST carry
      // its branch (the destination is never worker-derived, unlike prompt above); a
      // missing/empty branch is a create-time bug, so fail loudly rather than inventing
      // a name that would push somewhere the user is not watching.
      const taskBranch = claim.branch?.trim();
      if (!taskBranch)
        throw new Error("task run claim is missing its branch (uzi/task/<run-id>)");
      return this.git.runnerCloneForBranch(
        barePath,
        taskBranch,
        "task-" + claim.run_id,
        runId,
      );
    }
    if (claim.issue_iid == null)
      throw new Error("issue run claim is missing issue_iid");
    return this.git.createOrAttachRunnerClone(barePath, claim.issue_iid, runId);
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
    await reportState({
      status: "awaiting_approval",
      plan_md: planMd,
      ...(milestones?.length ? { milestones } : {}),
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
export const REASON_QUESTION_NOT_PARKED =
  "could not park the run to ask a question";

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
function mrTitle(claim: ClaimResponse): string {
  const t = claim.issue_title?.trim();
  if (t) return t;
  if (claim.kind === "ci_fix" && claim.pipeline)
    return `Fix CI: pipeline #${claim.pipeline.id} on ${claim.pipeline.ref}`;
  // An ad-hoc scheduled prompt run (PRD #241) has no issue, so it must never render
  // `Resolve issue #null`. The scheduler sets issue_title from a derived title so
  // the trimmed-title branch above almost always wins; this is the empty-title
  // fallback.
  if (claim.kind === "prompt") return "Scheduled prompt run";
  // A handoff task (PRD #400) is ISSUE-LESS: its issue_title is derived from the
  // inline context's first line, so the trimmed-title branch above almost always
  // wins; this is the empty-context fallback, never `Resolve issue #null`.
  if (claim.kind === "task") return "Handoff task";
  return `Resolve issue #${claim.issue_iid}`;
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
  if (claim.kind === "self_improve") {
    // A self_improve MR references its tracking issue but does NOT `Closes` it — the
    // issue is a stable container reused across cycles (PRD #46 Decision 10). The
    // self-improvement section carries the guard-critical flag, the test evidence,
    // and its own human-merge note.
    return [
      "Autonomous self-improvement change (PRD #46). Picks one top improvement per cycle.",
      "",
      `Tracking issue: #${claim.issue_iid}`,
      ...repoMarker,
      selfImproveSection ?? "",
    ].join("\n");
  }
  if (claim.kind === "ci_fix" && claim.pipeline) {
    return [
      `Fixes the failed CI pipeline for \`${claim.pipeline.ref}\`.`,
      "",
      `Failing pipeline: ${claim.pipeline.web_url}`,
      ...repoMarker,
      "",
      "---",
      footer,
    ].join("\n");
  }
  if (claim.kind === "prompt") {
    // An ad-hoc scheduled prompt run (PRD #241 Decision 10) is ISSUE-LESS: it is
    // created from a schedule's stored prompt against this repo, with no issue to
    // reference or close. Modeled on the self_improve branch above (references the
    // task, does NOT `Closes #…`), never the issue fallback below whose
    // `Implements/Closes #${issue_iid}` would render `#null`. When the change touched
    // a guard-critical path, promptGuardSection carries the flag (empty otherwise).
    return [
      "Ad-hoc scheduled prompt run (PRD #241 Decision 10). This run was created from a",
      "schedule's stored prompt against this repository — there is no tracking issue, so",
      "this MR references the task but closes nothing.",
      ...repoMarker,
      promptGuardSection ?? "",
      "",
      "---",
      footer,
    ].join("\n");
  }
  if (claim.kind === "task") {
    // A handoff task (PRD #400) opens an MR only when the caller passed --mr
    // (open_mr). Like the prompt/self_improve arms above it is ISSUE-LESS, so it
    // references the task and `Closes` nothing — the issue fallback below would render
    // `#null`. When present, base_branch names the source ref the task diverged from,
    // for the reviewer's context.
    const base = claim.base_branch?.trim();
    return [
      "Handoff task (PRD #400). This run worked inline context on the server-named",
      `\`${branch}\` branch${base ? ` (branched from \`${base}\`)` : ""} and opened this merge request because it was created with \`--mr\`.`,
      "There is no tracking issue, so this MR closes nothing.",
      ...repoMarker,
      "",
      "---",
      footer,
    ].join("\n");
  }
  const body = [
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
