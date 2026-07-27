import fs from "node:fs/promises";
import os from "node:os";
import type { WorkerClient } from "./client.js";
import type { GitCache } from "./git.js";
import { gitBasicCredential } from "./git.js";
import type { Executor, RunContext } from "./executor.js";
import { PlanRejectedError } from "./executor.js";
import { skillsPluginDir } from "./skills-plugin.js";
import { describeLimit, LimitReachedError } from "./limit.js";
import type { Logger } from "./log.js";
import type {
  AgentSelection,
  AgentSource,
  AgentTemplate,
  ClaimResponse,
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
import { SteeringChannel, type PlanVerdict } from "./steering.js";
import { GitLabClient, ForgejoClient, type ForgeClient } from "./forge.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { sessionTranscriptResolvable } from "./sdk-session.js";
import { errMessage, RUN_ID_RE } from "./util.js";
import {
  buildCheckEnv,
  defaultCheckRunner,
  flagGuardPaths,
  runSelfImproveChecks,
  selfImproveMrSection,
  SELF_IMPROVE_BRANCH,
  type CheckRunner,
} from "./self-improve.js";
import { installJsDeps } from "./js-deps.js";

/** Cap on a reported failure_reason, matching the forge error-body cap
 *  (forge.ts) so a runaway SDK error can't bloat the run row or the stream. */
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
  /** Injected for tests; default opens real GitLab MRs. The worker picks between
   *  this and `forgejo` per claim (`repo.forge_type`, D9). */
  gitlab?: ForgeClient;
  /** Injected for tests; default opens real Forgejo PRs (PRD #65 D9). */
  forgejo?: ForgeClient;
  /** Injected for tests; default is the real `.claude/agents/` parser (PRD #37).
   *  A seam so a test can drive the detection-failure path deterministically. */
  detectRepoAgents?: (worktreePath: string) => Promise<DetectedRepoAgents>;
  /** Injected for tests; default runs the uzi test suites as real subprocesses. A
   *  self_improve run's MR carries these results as its own evidence (PRD #46). */
  checkRunner?: CheckRunner;
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
  private readonly gitlab: ForgeClient;
  private readonly forgejo: ForgeClient;
  private readonly detect: (worktreePath: string) => Promise<DetectedRepoAgents>;
  // Optional test override; production builds it per-run with the scrubbed check env
  // (buildCheckEnv) once the executor's provisioned toolEnv is known (M9).
  private readonly checkRunner?: CheckRunner;
  /** PRD #41: absolute plan-approval deadline (epoch ms) per runId, set on the FIRST
   *  gate entry and reused across every revision round so N rounds share ONE budget (not
   *  24h per round). Cleared when the gate resolves terminally (approve/reject/cancel/
   *  timeout) and, defensively, when the run reaches a terminal state. */
  private readonly gateDeadlines = new Map<string, number>();
  /** PRD #41 (Decision 3): the set of runs that have opened a plan gate at least once.
   *  It distinguishes the FIRST gate (epoch 0 — a verdict already queued when the gate
   *  opens still applies) from a RE-gate after a revision turn (where the epoch must be
   *  advanced so a mid-revision approve of the superseded plan goes stale). Works
   *  regardless of planApprovalTimeoutMs (unlike keying off gateDeadlines). Cleared on a
   *  terminal verdict and, defensively, when the run reaches a terminal state. */
  private readonly gatedRuns = new Set<string>();

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
    this.forgejo = opts.forgejo ?? new ForgejoClient();
    this.detect = opts.detectRepoAgents ?? detectRepoAgents;
    this.checkRunner = opts.checkRunner;
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
    // redactText scrubs strings that reach the API outside a payload (failure_reason,
    // and the PRD #99 agent_label/agent_instance the batcher now carries alongside
    // the payload — `redact` walks inside a payload object and never sees them).
    const secrets = [claim.secrets.forge_pat, claim.secrets.anthropic_oauth_token, this.joinToken, gitBasic];
    const redact = makeRedactor(secrets);
    const redactText = makeTextRedactor(secrets);
    const batcher = new MessageBatcher(this.client, runId, claim.last_seq, this.batchMs, runLog, redact, redactText);

    // Cancel/shutdown spans the whole run; a `cancel` input aborts it via the
    // steering channel, which the executor's ctx.signal watches.
    const cancel = new AbortController();
    // PRD #41: `notify` lets the steering channel post a feed notice when it discards a
    // verdict/revision written against a stale plan version — wired to the batcher here
    // so the channel never reaches into runner internals.
    const steering = new SteeringChannel(this.client, runId, this.pollMs, runLog, cancel, {
      notify: (text) => batcher.emit({ kind: "status", agent: "worker", payload: { text } }),
    });

    // Last SDK session id the executor observed; carried on EVERY state report so
    // resume survives a lost report.
    let observedSessionId: string | undefined;
    // Returns the server's acknowledgement (PRD #35), which every caller here still
    // ignores. Widened from Promise<void> so the park branch can read it without
    // re-plumbing this closure; the annotation is the only change, and awaiting a
    // value nobody binds behaves exactly as before.
    const reportState = (body: Parameters<WorkerClient["reportState"]>[1]): ReturnType<WorkerClient["reportState"]> =>
      this.client.reportState(runId, observedSessionId ? { ...body, session_id: observedSessionId } : body);

    // PRD #108 M3: the batcher's breaker reports OUT OF BAND, never through itself —
    // `concat` is order-preserving, so an emitted explanation would queue behind the
    // poison that tripped it and never land. reportState has bounded retries,
    // 4xx-fatal semantics, and treats an already-terminal server response as
    // success, so if the run has already reported terminal this is a safe no-op
    // rather than a second, racing terminal report. Fire-and-forget: the batcher's
    // trip path must never block on the network.
    batcher.onPermanentFailureReport(({ reason }) => {
      void reportState({ status: "failed", failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN) }).catch((e) =>
        runLog.error("could not report the message-transport failure", { error: errMessage(e) }),
      );
    });

    let barePath: string | undefined;
    let worktreePath: string | undefined;
    // PRD #35: set ONLY by a park the server acknowledged as `limit_wait`. It gates
    // the three filesystem removals in the finally and nothing else. Declared here
    // rather than in the catch so the finally can see it; false is the safe default,
    // so every path that never reaches the park logic cleans up exactly as before.
    let parked = false;
    try {
      runLog.info("run claimed", { repo: claim.repo.url, branch: claim.branch ?? null });
      await reportState({ status: "running" });
      steering.start();

      barePath = await this.git.ensureClone(claim.repo.clone_url, claim.secrets.forge_pat, claim.secrets.forge_username);
      const runnerClone = await this.runnerCloneForClaim(barePath, claim);
      worktreePath = runnerClone.path;
      batcher.emit({ kind: "status", agent: "worker", payload: { text: `runner clone ready on ${runnerClone.branch}` } });

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
      let sessionId = claim.session_id ?? undefined;
      let resumeDropped = false;
      if (sessionId && runHome && !(await sessionTranscriptResolvable(runHome, sessionId, runLog))) {
        resumeDropped = true;
        sessionId = undefined;
        runLog.warn("resume session transcript is not resolvable here; starting a fresh SDK session", {
          run_home: runHome,
        });
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
          },
        });
      }
      // The lead is about to plan with no memory of the earlier turns. If the branch it
      // is standing on already carries pushed work, tell it so in the planning prompt —
      // otherwise the honest degradation just becomes silently duplicated work, which
      // is the harder failure to notice. Both conditions required: a fresh run reading
      // its own first-attempt branch needs no such warning.
      const priorWork = resumeDropped && runnerClone.priorCommits > 0 ? { commits: runnerClone.priorCommits } : undefined;

      // PRD #37: parse the checked-out repo's own agent roster and report it on
      // this first post-checkout `running` state report. It rides the STATE report
      // rather than the gate so that an autopilot run — which never parks at
      // awaiting_approval — records what was detected just the same. The roster is
      // inert data: nothing is assembled until a selection picks the repo source.
      const detection = await this.parseRepoAgents(runnerClone.path, batcher, runLog);
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

      // Cross-run memory (PRD #90): fetch this run's (user, repo) memory so the
      // executor can compose it into the lead's plan prompt as inert, nonce-fenced,
      // untrusted-advisory context. Guarded HARD — a fetch failure (older API, repo-
      // less run 404/409, transport error) or an empty store injects NOTHING and
      // never fails the run: memory is advisory, never load-bearing.
      let memory: Awaited<ReturnType<WorkerClient["getMemory"]>> = [];
      try {
        memory = await this.client.getMemory(runId);
      } catch (err) {
        runLog.warn("could not fetch cross-run memory; continuing without it", { error: errMessage(err) });
      }

      const ctx: RunContext = {
        runId,
        kind: claim.kind ?? "issue",
        issueIid: claim.issue_iid,
        issueTitle: claim.issue_title,
        issueDescription: claim.issue_description,
        pipeline: claim.pipeline,
        worktreePath: runnerClone.path,
        branch: runnerClone.branch,
        // The seed resolved this in the bare; forwarding it is what stops the lead from
        // guessing the branch's parent (judge rec, run 51757591).
        baseCommit: runnerClone.baseCommit,
        defaultBranchCommit: runnerClone.defaultBranchCommit,
        emit: (m) => batcher.emit(m),
        oauthToken: claim.secrets.anthropic_oauth_token,
        agents: claim.agents,
        repoAgents,
        skills: claim.skills,
        skillsDropped: claim.skills_dropped,
        repoSkillsEnabled: claim.repo.skills_enabled ?? false,
        memory,
        config: claim.config,
        // Preflighted above: the claim's id, or undefined when its transcript is not
        // on this worker (issue #105).
        sessionId,
        priorWork,
        // PRD #35 Decision 6b. The RUNNER is the only layer that knows all three
        // facts, which is why it resolves them here rather than the executor reading
        // the claim: the server said the plan is approved, a session id arrived, and
        // issue #105's transcript check did NOT drop it (`sessionId` is cleared above
        // when it did). Passing plan_approved through unconditionally would make the
        // executor skip planning for a run whose session is gone — the one case where
        // it must plan.
        planApproved: (claim.plan_approved ?? false) && !!sessionId,
        approvedPlan: claim.plan_md ?? undefined,
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

      // (b) fetch-back (PRD #51 M3): the agent committed in the RUNNER clone, so the
      // worker now fetches the agent branch BACK into its own bare (single-branch
      // refspec, file://+pack transport, protocol.file.allow pinned — the six B2
      // invariants live in git.fetchAgentBranch) before it inspects or pushes. Done
      // AFTER killAgentTree so no agent process is concurrently mutating the clone,
      // and it brings the agent's objects into the worker bare so the push does not
      // depend on the (soon torn-down) runner clone. `trackingRef` is what push +
      // changedFiles read; the runner clone is never a git source for either.
      const trackingRef = await this.git.fetchAgentBranch(barePath, runnerClone.path, result.branch);

      // Self-improvement MR evidence (PRD #46 Decision 10): with no CI on the uzi
      // repo, the worker itself runs the test suites and flags any guard-critical
      // path the change touched, folding both into the MR description. Best-effort —
      // gathered before the push so the MR opens with its evidence, and a suite that
      // can't run is reported "skipped", never failing the run.
      let selfImproveSection: string | undefined;
      if (claim.kind === "self_improve") {
        batcher.emit({ kind: "status", agent: "worker", payload: { text: "self-improvement: running the test suites for MR evidence" } });
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
        const checkEnv = buildCheckEnv(process.env, runHome ?? os.tmpdir(), result.toolEnv);
        const deps = await installJsDeps(runnerClone.path, checkEnv).catch(() => ({ results: [], truncated: false }));
        for (const note of deps.results) {
          runLog.info("self-improve: dependency install", { ...note });
        }
        // A truncated scan means `results` is a PREFIX of the repo's project dirs, so a
        // check may be about to run somewhere provisioning never reached. Say so rather
        // than let the notes above read as full coverage.
        if (deps.truncated) {
          runLog.warn("self-improve: dependency discovery hit its bound; some dirs were not installed", {
            installed_dirs: deps.results.length,
          });
        }
        const checkRunner = this.checkRunner ?? defaultCheckRunner(checkEnv);
        const checks = await runSelfImproveChecks(runnerClone.path, checkRunner);
        selfImproveSection = selfImproveMrSection(changed === null ? null : flagGuardPaths(changed), checks);
      }

      // The agent signalled done. The WORKER now performs the authenticated push
      // + MR with the PAT — the agent never had a credential.
      batcher.emit({ kind: "status", agent: "worker", payload: { text: "work complete; pushing branch and opening merge request" } });
      await this.git.pushBranch(barePath, result.branch, claim.secrets.forge_pat, claim.repo.clone_url, claim.secrets.forge_username);
      const targetBranch =
        claim.repo.default_branch?.trim() || (await this.git.defaultBranchName(barePath)) || "main";
      // Pick the forge client from the claim's forge_type (absent ⇒ gitlab, R8), so
      // the worker opens an MR on GitLab and a PR on Forgejo from the same code path;
      // each client derives its own API base + project from repo.url (D9). createMergeRequest
      // is idempotent: for a ci_fix on an existing agent branch it returns the EXISTING
      // MR/PR (no second one, PRD #6); for a fresh ci-fix/pipeline-N or agent/issue-N
      // branch it opens one. Reporting its iid keeps the fix branch watched so the
      // verification sync can stamp the verdict.
      const forge = claim.repo.forge_type === "forgejo" ? this.forgejo : this.gitlab;
      const mr = await forge.createMergeRequest({
        repoUrl: claim.repo.url,
        pat: claim.secrets.forge_pat,
        sourceBranch: result.branch,
        targetBranch,
        title: mrTitle(claim),
        description: mrDescription(claim, result.branch, result.agentSelection, selfImproveSection),
      });
      batcher.emit({ kind: "status", agent: "worker", payload: { text: `merge request opened: !${mr.iid} ${mr.webUrl}` } });

      await batcher.close();
      // Persist the MR/PR web URL the forge just handed us (PRD #65 D8), so the web
      // links it directly instead of reconstructing the URL by string surgery. Omit
      // it when the forge returned none (mr.webUrl empty) so the server lands NULL and
      // the legacy forgeUrls.ts reconstruction still applies (R8, additive+optional).
      // prd_done_path (PRD #72 M4) rides the same terminal report. Omitted when the
      // executor set nothing — same `|| undefined` shape as mr_web_url on this line,
      // so "old worker" and "moved no PRD" are indistinguishable on the wire by
      // design, and the api treats both as NULL.
      await reportState({
        status: "completed",
        branch: result.branch,
        mr_iid: mr.iid,
        mr_web_url: mr.webUrl || undefined,
        prd_done_path: result.prdDonePath,
      });
      runLog.info("run completed", { branch: result.branch, mr_iid: mr.iid });
    } catch (err) {
      // PRD #35: a usage-limit death is not an ordinary failure. Handled before the
      // generic path below because that path is terminal in both senses — it reports
      // `failed` and it lets the finally erase the session this run wants to resume from.
      if (err instanceof LimitReachedError) {
        parked = await this.handleLimitReached(err, claim, batcher, reportState, runLog);
        // parked === true is the ONLY thing that preserves on-disk state; see the
        // carve-out in the finally.
      } else {
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
      }
    } finally {
      await steering.stop().catch(() => undefined);
      // PRD #41: drop this run's plan-approval deadline + gate-tracking (normally cleared
      // when the gate resolves terminally, but a run that ends by any other path must not
      // leak either).
      this.gateDeadlines.delete(runId);
      this.gatedRuns.delete(runId);
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
      // EXACTLY the three filesystem removals below are skipped for a parked run,
      // and nothing else in this finally is. The three are what a resume needs:
      // the runner clone, the sibling skills plugin dir, and the per-run HOME that
      // holds the resumable SDK transcript. Preserving only some of them would
      // resume into a session missing its plugins or its worktree.
      //
      // The other four statements in this block — the steering poller stop, the two
      // gate-map deletes, and the secret eviction above — MUST still run on a park.
      // Guarding the whole block (or returning early) would leave a poller running
      // and a gate deadline registered for what may be days.
      if (worktreePath && !parked) {
        await this.git
          .removeRunnerClone(worktreePath)
          .catch((e) => runLog.warn("runner clone cleanup failed", { error: errMessage(e) }));
      }
      // Tear down the sibling skills plugin dir the executor synthesized (PRD #16
      // M4). It is OUTSIDE the runner clone, so removeRunnerClone does not reach it;
      // leave it and each run leaks a dir. Best-effort, like the clone cleanup.
      if (worktreePath && !parked) {
        await fs
          .rm(skillsPluginDir(worktreePath), { recursive: true, force: true })
          .catch((e) => runLog.warn("skills plugin cleanup failed", { error: errMessage(e) }));
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
        await rmTreeForce(runHome).catch((e) => runLog.warn("run HOME cleanup failed", { error: errMessage(e) }));
      }
      if (parked) {
        // ~170 MB of Go module cache was measured under a single run HOME, so say
        // what is being held and why — an operator reading disk pressure needs to
        // connect it to a parked run rather than to a leak.
        runLog.info("run parked on a usage limit; preserving its clone, plugin dir and HOME for resume", {
          run_home: runHome,
        });
      }
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

    if (!claim.wait_on_limit) {
      // Opted out: fail, but with the structured facts attached so the server can
      // say WHY instead of leaving today's bare "agent run failed:
      // error_during_execution".
      runLog.info("run hit a usage limit and is not opted in to waiting", { detail });
      batcher.emit({ kind: "error", agent: "worker", payload: { text: `Anthropic usage limit reached (${detail})` } });
      await batcher.close().catch(() => undefined);
      await reportState({ status: "failed", ...limitFields }).catch((e) =>
        runLog.error("could not report limit failure", { error: errMessage(e) }),
      );
      return false;
    }

    runLog.info("run hit a usage limit; requesting a park", { detail });
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: { text: `Anthropic usage limit reached (${detail}) — pausing this run until the limit resets` },
    });
    await batcher.close().catch(() => undefined);

    let ack: StateAck;
    try {
      ack = await reportState({ status: "limit_wait", ...limitFields });
    } catch (e) {
      // The park request never landed. Clean up: this run is not parked, and a
      // preserved HOME nothing will ever claim is an unbounded leak.
      runLog.error("could not report the park; cleaning up as an unparked run", { error: errMessage(e) });
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
   * Seed the run's RUNNER CLONE + resolve its branch (PRD #51 M3, (b)). An issue run
   * works agent/issue-{iid}. A ci_fix run (PRD #6) targets a fresh
   * `ci-fix/pipeline-{id}` branch when fixing the default branch, or the existing
   * agent branch (updating its MR) when fixing a run branch — keyed by pipeline.ref
   * vs the repo's default branch. The working tree lives ONLY in this clone; the
   * worker fetches the agent branch back from it before pushing (fetchAgentBranch).
   */
  private async runnerCloneForClaim(barePath: string, claim: ClaimResponse) {
    if (claim.kind === "ci_fix" && claim.pipeline) {
      const defaultBranch = claim.repo.default_branch?.trim();
      const fixBranch =
        defaultBranch && claim.pipeline.ref === defaultBranch
          ? `ci-fix/pipeline-${claim.pipeline.id}`
          : claim.pipeline.ref;
      return this.git.runnerCloneForBranch(barePath, fixBranch, fixBranch.replace(/\//g, "-"));
    }
    if (claim.kind === "self_improve") {
      // The FIXED branch (PRD #46 Decision 10): reused every cycle so the worker's
      // idempotent createMergeRequest extends one open MR rather than opening a new
      // one, and successive cycles are tested together.
      return this.git.runnerCloneForBranch(barePath, SELF_IMPROVE_BRANCH, SELF_IMPROVE_BRANCH.replace(/\//g, "-"));
    }
    if (claim.issue_iid == null) throw new Error("issue run claim is missing issue_iid");
    return this.git.createOrAttachRunnerClone(barePath, claim.issue_iid);
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
    batcher: MessageBatcher,
    steering: SteeringChannel,
    // Ignored by the gate, but typed to match the client (PRD #35): a gate that
    // narrowed the return would make the park branch unable to reuse this closure.
    reportState: (body: StateRequest) => Promise<StateAck>,
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
      // Make this report self-contained (F1): carry the roster alongside the selection
      // so both validate + persist atomically, even if the fire-and-forget running
      // roster report above failed and left the column NULL. Gated on length > 0 — on
      // a detection failure repoAgents is [] and the selection resolves to `own`;
      // sending repo_agents: [] would flip NULL ("not reported") to [] ("detected
      // none") and break that deliberate distinction. rosterFor already prefers the
      // reported roster over the column, so this needs no wire change.
      const autopilotState: StateRequest = { status: "running", agent_selection: selection };
      if (repoAgents.length > 0) autopilotState.repo_agents = repoAgentSummaries(repoAgents);
      await reportState(autopilotState).catch((e) =>
        runLog.warn("could not persist autopilot agent selection", { error: errMessage(e) }),
      );
      batcher.emit({ kind: "status", agent: "worker", payload: { text: autopilotSelectionText(selection, repoAgents.length) } });
      runLog.info("plan gate: auto-approved (autopilot)", { run_id: runId, agent_source: selection.source });
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
    await reportState({ status: "awaiting_approval", plan_md: planMd });
    if (this.gatedRuns.has(runId)) steering.bumpEpoch();
    else this.gatedRuns.add(runId);
    const epoch = steering.currentEpoch();
    runLog.info("plan gate: awaiting approval", { run_id: runId, gate_epoch: epoch });

    // A terminal verdict ends the gate → clear the shared per-run gate state. A revise
    // keeps the shared budget/epoch state running; the re-report above does the bump.
    const settle = (v: PlanVerdict): PlanVerdict => {
      if (v.kind !== "revise") {
        this.gateDeadlines.delete(runId);
        this.gatedRuns.delete(runId);
      }
      return v; // NOTE: no bump here — the awaiting_approval re-report bumps.
    };

    if (this.planApprovalTimeoutMs <= 0) return settle(await steering.awaitGateEvent(epoch));

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
      timer = setTimeout(() => resolve({ kind: "reject", reason: "plan approval timed out" }), remaining);
      timer.unref?.();
    });
    try {
      return settle(await Promise.race([steering.awaitGateEvent(epoch), timeout]));
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
