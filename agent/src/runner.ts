import fs from "node:fs/promises";
import type { WorkerClient } from "./client.js";
import type { GitCache } from "./git.js";
import { gitBasicCredential } from "./git.js";
import type { Executor, RunContext } from "./executor.js";
import { PlanRejectedError } from "./executor.js";
import { skillsPluginDir } from "./skills-plugin.js";
import type { Logger } from "./log.js";
import type { AgentSelection, AgentSource, AgentTemplate, ClaimResponse, StateRequest } from "./protocol.js";
import { resolveAgentSelection } from "./protocol.js";
import {
  describeRepoAgentNote,
  detectRepoAgents,
  repoAgentSummaries,
  type DetectedRepoAgents,
} from "./repoagents.js";
import { MessageBatcher } from "./batcher.js";
import { SteeringChannel, type PlanVerdict } from "./steering.js";
import { GitLabClient, gitlabBaseUrl, gitlabProjectPath } from "./gitlab.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { errMessage, RUN_ID_RE } from "./util.js";
import {
  defaultCheckRunner,
  flagGuardPaths,
  runSelfImproveChecks,
  selfImproveMrSection,
  SELF_IMPROVE_BRANCH,
  type CheckRunner,
} from "./self-improve.js";

/** Cap on a reported failure_reason, matching the GitLab error-body cap
 *  (gitlab.ts) so a runaway SDK error can't bloat the run row or the stream. */
const MAX_FAILURE_REASON_LEN = 512;

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

/** Tuning the runner needs beyond the collaborators (defaults keep M2/M3 tests terse). */
export interface RunnerOptions {
  /** How often the steering channel polls /inputs (default 3s). */
  pollMs?: number;
  /** Plan-approval gate cap; 0 disables (default 24h). */
  planApprovalTimeoutMs?: number;
  /** Injected for tests; default opens real GitLab MRs. */
  gitlab?: GitLabClient;
  /** Injected for tests; default is the real `.claude/agents/` parser (PRD #37).
   *  A seam so a test can drive the detection-failure path deterministically. */
  detectRepoAgents?: (worktreePath: string) => Promise<DetectedRepoAgents>;
  /** Injected for tests; default runs the uzi test suites as real subprocesses. A
   *  self_improve run's MR carries these results as its own evidence (PRD #46). */
  checkRunner?: CheckRunner;
}

/**
 * Drives one claimed run through the full M4 workflow:
 *   claim → running → clone/worktree → PLAN turn → approval gate → implement⇄
 *   review loop → worker pushes branch + opens MR → completed | failed
 * and always tears the worktree down (keeping the bare clone).
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
  private readonly gitlab: GitLabClient;
  private readonly detect: (worktreePath: string) => Promise<DetectedRepoAgents>;
  private readonly checkRunner: CheckRunner;

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
    this.gitlab = opts.gitlab ?? new GitLabClient();
    this.detect = opts.detectRepoAgents ?? detectRepoAgents;
    this.checkRunner = opts.checkRunner ?? defaultCheckRunner();
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
    if (!RUN_ID_RE.test(runId)) throw new Error("refusing to execute a run with an invalid run id");
    // This run's OWN executor + private HOME (PRD #42 Decisions 4/5), built fresh
    // per execution so nothing subprocess-scoped is shared with a concurrent run.
    const { executor, homeDir: runHome } = this.makeExecutor(runId);
    // Register per-run secrets with the logger so they are scrubbed from any
    // output, then never log the claim payload itself. Tracked in runScopedSecrets
    // and evicted on terminal (Decision 7) so a completed run's PAT/token does not
    // linger in the process-lifetime scrub set; the registry is reference-counted,
    // so evicting these never un-scrubs a still-active sibling run that shares them.
    const gitBasic = gitBasicCredential(claim.secrets.forge_pat, claim.secrets.forge_username);
    // Defense in depth for gitBasic: the git-over-HTTPS Basic credential
    // (base64(user:pat)) only ever lives in a GIT_CONFIG_VALUE (never argv/logs),
    // but register it too so a future leak through the git env would still be scrubbed.
    const runScopedSecrets = [claim.secrets.forge_pat, gitBasic];
    if (claim.secrets.anthropic_oauth_token) runScopedSecrets.push(claim.secrets.anthropic_oauth_token);
    for (const s of runScopedSecrets) this.log.addSecret(s);

    const runLog = this.log.child({ run_id: runId, issue_iid: claim.issue_iid });
    // Same secret set for both redactors: the batcher scrubs run_message payloads;
    // redactText scrubs strings that reach the API outside a payload (failure_reason).
    const secrets = [claim.secrets.forge_pat, claim.secrets.anthropic_oauth_token, this.joinToken, gitBasic];
    const redact = makeRedactor(secrets);
    const redactText = makeTextRedactor(secrets);
    const batcher = new MessageBatcher(this.client, runId, claim.last_seq, this.batchMs, runLog, redact);

    // Cancel/shutdown spans the whole run; a `cancel` input aborts it via the
    // steering channel, which the executor's ctx.signal watches.
    const cancel = new AbortController();
    const steering = new SteeringChannel(this.client, runId, this.pollMs, runLog, cancel);

    // Last SDK session id the executor observed; carried on EVERY state report so
    // resume survives a lost report.
    let observedSessionId: string | undefined;
    const reportState = (body: Parameters<WorkerClient["reportState"]>[1]): Promise<void> =>
      this.client.reportState(runId, observedSessionId ? { ...body, session_id: observedSessionId } : body);

    let barePath: string | undefined;
    let worktreePath: string | undefined;
    try {
      runLog.info("run claimed", { repo: claim.repo.url, branch: claim.branch ?? null });
      await reportState({ status: "running" });
      steering.start();

      barePath = await this.git.ensureClone(claim.repo.clone_url, claim.secrets.forge_pat, claim.secrets.forge_username);
      const worktree = await this.worktreeForClaim(barePath, claim);
      worktreePath = worktree.path;
      batcher.emit({ kind: "status", agent: "worker", payload: { text: `worktree ready on ${worktree.branch}` } });

      // PRD #37: parse the checked-out repo's own agent roster and report it on
      // this first post-checkout `running` state report. It rides the STATE report
      // rather than the gate so that an autopilot run — which never parks at
      // awaiting_approval — records what was detected just the same. The roster is
      // inert data: nothing is assembled until a selection picks the repo source.
      const detection = await this.parseRepoAgents(worktree.path, batcher, runLog);
      const repoAgents = detection.agents;
      if (detection.ok) {
        // Non-fatal, and fire-and-forget (matching the session-id report below): an
        // INFORMATIONAL roster report must never fail a run. An older API without the
        // repo_agents field 400s DisallowUnknownFields, which the client treats as
        // permanent — awaiting the report here would turn a working run into a failed
        // one over a field the run does not depend on. On a detection FAILURE we send
        // no roster at all, so the column stays NULL ("not reported") rather than `[]`
        // ("scanned, found none") — the two must stay distinguishable.
        void reportState({ status: "running", repo_agents: repoAgentSummaries(repoAgents) }).catch((e) =>
          runLog.warn("could not report repo agent roster", { error: errMessage(e) }),
        );
      }

      const ctx: RunContext = {
        runId,
        kind: claim.kind ?? "issue",
        issueIid: claim.issue_iid,
        issueTitle: claim.issue_title,
        issueDescription: claim.issue_description,
        pipeline: claim.pipeline,
        worktreePath: worktree.path,
        branch: worktree.branch,
        emit: (m) => batcher.emit(m),
        oauthToken: claim.secrets.anthropic_oauth_token,
        agents: claim.agents,
        repoAgents,
        skills: claim.skills,
        skillsDropped: claim.skills_dropped,
        repoSkillsEnabled: claim.repo.skills_enabled ?? false,
        config: claim.config,
        sessionId: claim.session_id,
        signal: cancel.signal,
        // Persist the SDK session id the moment the executor learns it, so a
        // re-queued run can resume it. Best-effort.
        onSessionId: (sessionId) => {
          observedSessionId = sessionId;
          void reportState({ status: "running" }).catch((e) =>
            runLog.warn("could not persist session id", { error: errMessage(e) }),
          );
        },
        // The plan gate: surface the plan, post awaiting_approval, and return the
        // verdict the steering channel resolves (bounded so an abandoned plan
        // fails rather than wedging the worker). An autopilot claim short-circuits
        // to an approve verdict (see gatePlan) — the run never parks at the gate.
        gatePlan: (planMd) =>
          this.gatePlan(runId, planMd, batcher, steering, reportState, runLog, claim.auto_approve ?? false, repoAgents),
        pullFollowUp: () => steering.pullFollowUp(),
        reportIteration: (iteration) => {
          void reportState({ status: "running", iteration_count: iteration }).catch((e) =>
            runLog.warn("could not report iteration", { error: errMessage(e) }),
          );
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
        batcher.emit({ kind: "status", agent: "worker", payload: { text: "not a code problem: completing with the diagnosis, no merge request" } });
        await batcher.close();
        await reportState({ status: "completed", fix_verdict: "not_code" });
        runLog.info("ci_fix run completed with not_code verdict", { run_id: runId });
        return;
      }

      // Self-improvement MR evidence (PRD #46 Decision 10): with no CI on the uzi
      // repo, the worker itself runs the test suites and flags any guard-critical
      // path the change touched, folding both into the MR description. Best-effort —
      // gathered before the push so the MR opens with its evidence, and a suite that
      // can't run is reported "skipped", never failing the run.
      let selfImproveSection: string | undefined;
      if (claim.kind === "self_improve") {
        batcher.emit({ kind: "status", agent: "worker", payload: { text: "self-improvement: running the test suites for MR evidence" } });
        const changed = await this.git.changedFiles(barePath, worktree.path);
        const checks = await runSelfImproveChecks(worktree.path, this.checkRunner);
        selfImproveSection = selfImproveMrSection(flagGuardPaths(changed), checks);
      }

      // The agent signalled done. The WORKER now performs the authenticated push
      // + MR with the PAT — the agent never had a credential.
      batcher.emit({ kind: "status", agent: "worker", payload: { text: "work complete; pushing branch and opening merge request" } });
      await this.git.pushBranch(barePath, result.branch, claim.secrets.forge_pat, claim.repo.clone_url, claim.secrets.forge_username);
      const targetBranch =
        claim.repo.default_branch?.trim() || (await this.git.defaultBranchName(barePath)) || "main";
      // createMergeRequest is idempotent: for a ci_fix on an existing agent branch it
      // returns the EXISTING MR (no second MR, PRD #6); for a fresh ci-fix/pipeline-N
      // or agent/issue-N branch it opens one. Reporting its iid keeps the fix branch
      // watched so the verification sync can stamp the verdict.
      const mr = await this.gitlab.createMergeRequest({
        baseUrl: gitlabBaseUrl(claim.repo.url),
        projectPath: gitlabProjectPath(claim.repo.url),
        pat: claim.secrets.forge_pat,
        sourceBranch: result.branch,
        targetBranch,
        title: mrTitle(claim),
        description: mrDescription(claim, result.branch, result.agentSelection, selfImproveSection),
      });
      batcher.emit({ kind: "status", agent: "worker", payload: { text: `merge request opened: !${mr.iid} ${mr.webUrl}` } });

      await batcher.close();
      await reportState({ status: "completed", branch: result.branch, mr_iid: mr.iid });
      runLog.info("run completed", { branch: result.branch, mr_iid: mr.iid });
    } catch (err) {
      // failure_reason goes straight to reportState, bypassing the batcher's
      // redactor, and the sdk-executor catch-all re-throws raw SDK errors into this
      // path — so scrub it here with the run's own secret set. A plan rejection
      // carries the user's verbatim reason; scrubbing it too is harmless for plain
      // text and a safety net if the user pasted a secret.
      const reason = redactText(err instanceof PlanRejectedError ? err.reason : errMessage(err));
      runLog.error("run failed", { error: reason });
      batcher.emit({ kind: "error", agent: "worker", payload: { text: reason } });
      await batcher.close().catch(() => undefined);
      // Cap what lands in the run row (matches the GitLab error-body cap).
      await reportState({ status: "failed", failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN) }).catch((e) =>
        runLog.error("could not report failed state", { error: errMessage(e) }),
      );
    } finally {
      await steering.stop().catch(() => undefined);
      // Evict this run's secrets from the logger now the run is terminal (Decision
      // 7). Reaching this finally means execute() ran to a terminal report; a
      // requeue (worker death) never returns here, so the evict/HOME-cleanup below
      // only ever fire for a run that will not resume.
      for (const s of runScopedSecrets) this.log.removeSecret(s);
      if (barePath && worktreePath) {
        await this.git
          .removeWorktree(barePath, worktreePath)
          .catch((e) => runLog.warn("worktree cleanup failed", { error: errMessage(e) }));
      }
      // Tear down the sibling skills plugin dir the executor synthesized (PRD #16
      // M4). It is OUTSIDE the worktree, so removeWorktree does not reach it; leave
      // it and each run leaks a dir. Best-effort, like the worktree cleanup.
      if (worktreePath) {
        await fs
          .rm(skillsPluginDir(worktreePath), { recursive: true, force: true })
          .catch((e) => runLog.warn("skills plugin cleanup failed", { error: errMessage(e) }));
      }
      // Remove this run's private HOME (agent-home/<runId>, Decision 5). The SDK
      // session transcript under it is only needed to resume, and a terminal run
      // never resumes. A concurrent sibling's HOME is a distinct dir, untouched.
      if (runHome) {
        await fs
          .rm(runHome, { recursive: true, force: true })
          .catch((e) => runLog.warn("run HOME cleanup failed", { error: errMessage(e) }));
      }
    }
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
        payload: { text: "could not read the repo's .claude/agents/; continuing with your own agent templates" },
      });
      return { agents: [], ok: false };
    }
    for (const note of detected.notes) {
      batcher.emit({ kind: "status", agent: "worker", payload: { text: describeRepoAgentNote(note) } });
    }
    if (detected.agents.length > 0) {
      const names = detected.agents.map((a) => a.name).join(", ");
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: `detected ${detected.agents.length} agent(s) in the repo's .claude/agents/: ${names}` },
      });
    }
    runLog.info("repo agents detected", { count: detected.agents.length, dropped: detected.notes.length });
    return { agents: detected.agents, ok: true };
  }

  /**
   * Resolve the run's worktree + branch. An issue run works agent/issue-{iid}. A
   * ci_fix run (PRD #6) targets a fresh `ci-fix/pipeline-{id}` branch when fixing
   * the default branch, or the existing agent branch (updating its MR) when fixing
   * a run branch — keyed by pipeline.ref vs the repo's default branch.
   */
  private async worktreeForClaim(barePath: string, claim: ClaimResponse) {
    if (claim.kind === "ci_fix" && claim.pipeline) {
      const defaultBranch = claim.repo.default_branch?.trim();
      const fixBranch =
        defaultBranch && claim.pipeline.ref === defaultBranch
          ? `ci-fix/pipeline-${claim.pipeline.id}`
          : claim.pipeline.ref;
      return this.git.worktreeForBranch(barePath, fixBranch, fixBranch.replace(/\//g, "-"));
    }
    if (claim.kind === "self_improve") {
      // The FIXED branch (PRD #46 Decision 10): reused every cycle so the worker's
      // idempotent createMergeRequest extends one open MR rather than opening a new
      // one, and successive cycles are tested together.
      return this.git.worktreeForBranch(barePath, SELF_IMPROVE_BRANCH, SELF_IMPROVE_BRANCH.replace(/\//g, "-"));
    }
    if (claim.issue_iid == null) throw new Error("issue run claim is missing issue_iid");
    return this.git.createOrAttachWorktree(barePath, claim.issue_iid);
  }

  /** Post awaiting_approval with the plan and await the steering verdict, bounded.
   *  For an autopilot run, the plan is still recorded but the gate resolves with an
   *  approve verdict immediately — no awaiting_approval report, no /inputs wait. It
   *  also resolves + records the run's DEFAULT agent selection (PRD #37 Decision 6),
   *  since a self-approved run never receives an approve_plan input to carry one. */
  private async gatePlan(
    runId: string,
    planMd: string,
    batcher: MessageBatcher,
    steering: SteeringChannel,
    reportState: (body: StateRequest) => Promise<void>,
    runLog: Logger,
    autoApprove: boolean,
    repoAgents: AgentTemplate[],
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
      const selection = resolveAgentSelection({ status: "absent" }, repoAgents.length > 0).selection;
      await reportState({ status: "running", agent_selection: selection }).catch((e) =>
        runLog.warn("could not persist autopilot agent selection", { error: errMessage(e) }),
      );
      batcher.emit({ kind: "status", agent: "worker", payload: { text: autopilotSelectionText(selection, repoAgents.length) } });
      runLog.info("plan gate: auto-approved (autopilot)", { run_id: runId, agent_source: selection.source });
      return { kind: "approve", selection: { status: "ok", selection } };
    }

    await reportState({ status: "awaiting_approval", plan_md: planMd });
    runLog.info("plan gate: awaiting approval", { run_id: runId });

    if (this.planApprovalTimeoutMs <= 0) return steering.awaitVerdict();
    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<PlanVerdict>((resolve) => {
      timer = setTimeout(() => resolve({ kind: "reject", reason: "plan approval timed out" }), this.planApprovalTimeoutMs);
      timer.unref?.();
    });
    try {
      return await Promise.race([steering.awaitVerdict(), timeout]);
    } finally {
      if (timer) clearTimeout(timer);
    }
  }
}

/** MR title from the issue snapshot (never empty). */
function mrTitle(claim: ClaimResponse): string {
  const t = claim.issue_title?.trim();
  if (t) return t;
  if (claim.kind === "ci_fix" && claim.pipeline) return `Fix CI: pipeline #${claim.pipeline.id} on ${claim.pipeline.ref}`;
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
  return [
    `Implements issue #${claim.issue_iid}.`,
    "",
    `Closes #${claim.issue_iid}`,
    ...repoMarker,
    "",
    "---",
    footer,
  ].join("\n");
}

/** Feed text for an autopilot run's resolved default selection (PRD #37 Decision
 *  6). Repo source names the count; own source names the fallback. */
function autopilotSelectionText(selection: AgentSelection, repoCount: number): string {
  return selection.source === "repo"
    ? `autopilot: using the ${repoCount} agent(s) from the repo's .claude/agents/`
    : "autopilot: using your own agent templates";
}
