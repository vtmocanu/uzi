import fs from "node:fs/promises";
import type { WorkerClient } from "./client.js";
import type { GitCache } from "./git.js";
import { gitBasicCredential } from "./git.js";
import type { Executor, RunContext } from "./executor.js";
import { PlanRejectedError } from "./executor.js";
import { skillsPluginDir } from "./skills-plugin.js";
import type { Logger } from "./log.js";
import type { ClaimResponse } from "./protocol.js";
import { MessageBatcher } from "./batcher.js";
import { SteeringChannel, type PlanVerdict } from "./steering.js";
import { GitLabClient, gitlabBaseUrl, gitlabProjectPath } from "./gitlab.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { errMessage } from "./util.js";

/** Cap on a reported failure_reason, matching the GitLab error-body cap
 *  (gitlab.ts) so a runaway SDK error can't bloat the run row or the stream. */
const MAX_FAILURE_REASON_LEN = 512;

/** Tuning the runner needs beyond the collaborators (defaults keep M2/M3 tests terse). */
export interface RunnerOptions {
  /** How often the steering channel polls /inputs (default 3s). */
  pollMs?: number;
  /** Plan-approval gate cap; 0 disables (default 24h). */
  planApprovalTimeoutMs?: number;
  /** Injected for tests; default opens real GitLab MRs. */
  gitlab?: GitLabClient;
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

  constructor(
    private readonly client: WorkerClient,
    private readonly git: GitCache,
    private readonly executor: Executor,
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
  }

  async execute(claim: ClaimResponse): Promise<void> {
    const runId = claim.run_id;
    // Register per-run secrets with the logger so they are scrubbed from any
    // output, then never log the claim payload itself.
    this.log.addSecret(claim.secrets.forge_pat);
    if (claim.secrets.anthropic_oauth_token) this.log.addSecret(claim.secrets.anthropic_oauth_token);
    // Defense in depth: the git-over-HTTPS Basic credential (base64(user:pat))
    // only ever lives in a GIT_CONFIG_VALUE (never argv/logs), but register it too
    // so a future leak through the git env would still be scrubbed.
    const gitBasic = gitBasicCredential(claim.secrets.forge_pat, claim.secrets.forge_username);
    this.log.addSecret(gitBasic);

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
      const worktree = await this.git.createOrAttachWorktree(barePath, claim.issue_iid);
      worktreePath = worktree.path;
      batcher.emit({ kind: "status", agent: "worker", payload: { text: `worktree ready on ${worktree.branch}` } });

      const ctx: RunContext = {
        runId,
        issueIid: claim.issue_iid,
        issueTitle: claim.issue_title,
        issueDescription: claim.issue_description,
        worktreePath: worktree.path,
        branch: worktree.branch,
        emit: (m) => batcher.emit(m),
        oauthToken: claim.secrets.anthropic_oauth_token,
        agents: claim.agents,
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
        gatePlan: (planMd) => this.gatePlan(runId, planMd, batcher, steering, reportState, runLog, claim.auto_approve ?? false),
        pullFollowUp: () => steering.pullFollowUp(),
        reportIteration: (iteration) => {
          void reportState({ status: "running", iteration_count: iteration }).catch((e) =>
            runLog.warn("could not report iteration", { error: errMessage(e) }),
          );
        },
      };

      const result = await this.executor.run(ctx);

      // Reap any agent-backgrounded subprocess BEFORE the PAT touches a git child
      // env — otherwise a survivor could read the PAT from that child's
      // /proc/environ during the push (M4 audit B1). The SDK executor also
      // self-reaps in its run() finally; this is the explicit, load-bearing call
      // at the security boundary.
      this.executor.killAgentTree?.();

      // The agent signalled done. The WORKER now performs the authenticated push
      // + MR with the PAT — the agent never had a credential.
      batcher.emit({ kind: "status", agent: "worker", payload: { text: "work complete; pushing branch and opening merge request" } });
      await this.git.pushBranch(barePath, result.branch, claim.secrets.forge_pat, claim.repo.clone_url, claim.secrets.forge_username);
      const targetBranch =
        claim.repo.default_branch?.trim() || (await this.git.defaultBranchName(barePath)) || "main";
      const mr = await this.gitlab.createMergeRequest({
        baseUrl: gitlabBaseUrl(claim.repo.url),
        projectPath: gitlabProjectPath(claim.repo.url),
        pat: claim.secrets.forge_pat,
        sourceBranch: result.branch,
        targetBranch,
        title: mrTitle(claim),
        description: mrDescription(claim, result.branch),
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
    }
  }

  /** Post awaiting_approval with the plan and await the steering verdict, bounded.
   *  For an autopilot run, the plan is still recorded but the gate resolves with an
   *  approve verdict immediately — no awaiting_approval report, no /inputs wait. */
  private async gatePlan(
    runId: string,
    planMd: string,
    batcher: MessageBatcher,
    steering: SteeringChannel,
    reportState: (body: { status: "awaiting_approval"; plan_md: string }) => Promise<void>,
    runLog: Logger,
    autoApprove: boolean,
  ): Promise<PlanVerdict> {
    batcher.emit({ kind: "plan", agent: "lead", payload: { plan_md: planMd } });
    // Get the plan message onto the stream regardless of mode — it is the audit
    // record of what the agent intended, autopilot or not.
    await batcher.flush().catch(() => undefined);

    if (autoApprove) {
      // Auto-approve is a VERDICT SOURCE at the existing gate, not a bypass around
      // it: the plan was recorded above; the run just never enters awaiting_approval
      // (no state flicker, no column-automation churn) and never waits on a human.
      runLog.info("plan gate: auto-approved (autopilot)", { run_id: runId });
      return { kind: "approve" };
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
  return t ? t : `Resolve issue #${claim.issue_iid}`;
}

/** MR body: links + closes the issue, and states the primary directive (humans merge). */
function mrDescription(claim: ClaimResponse, branch: string): string {
  return [
    `Implements issue #${claim.issue_iid}.`,
    "",
    `Closes #${claim.issue_iid}`,
    "",
    "---",
    `Opened automatically by the uzi agent from branch \`${branch}\`. Please review and merge manually — the agent never merges.`,
  ].join("\n");
}
