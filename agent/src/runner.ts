import type { WorkerClient } from "./client.js";
import type { GitCache } from "./git.js";
import type { Executor, RunContext } from "./executor.js";
import type { Logger } from "./log.js";
import type { ClaimResponse } from "./protocol.js";
import { MessageBatcher } from "./batcher.js";
import { errMessage } from "./util.js";

/**
 * Drives one claimed run through the state machine:
 *   claim → running → clone/worktree → execute → completed | failed
 * and always tears the worktree down (keeping the bare clone). M4 inserts the
 * plan gate and MR push around executor.run(); M2 proves the loop with a stub.
 */
export class RunRunner {
  constructor(
    private readonly client: WorkerClient,
    private readonly git: GitCache,
    private readonly executor: Executor,
    private readonly log: Logger,
    private readonly batchMs: number,
  ) {}

  async execute(claim: ClaimResponse): Promise<void> {
    const runId = claim.run_id;
    // Register per-run secrets with the logger so they are scrubbed from any
    // output, then never log the claim payload itself.
    this.log.addSecret(claim.secrets.forge_pat);
    if (claim.secrets.anthropic_oauth_token) this.log.addSecret(claim.secrets.anthropic_oauth_token);

    const runLog = this.log.child({ run_id: runId, issue_iid: claim.issue_iid });
    const batcher = new MessageBatcher(this.client, runId, claim.last_seq, this.batchMs, runLog);

    let barePath: string | undefined;
    let worktreePath: string | undefined;
    try {
      runLog.info("run claimed", { repo: claim.repo.url, branch: claim.branch ?? null });
      await this.client.reportState(runId, { status: "running" });

      barePath = await this.git.ensureClone(claim.repo.url, claim.secrets.forge_pat);
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
      };
      const result = await this.executor.run(ctx);

      await batcher.close();
      await this.client.reportState(runId, { status: "completed", branch: result.branch });
      runLog.info("run completed", { branch: result.branch });
    } catch (err) {
      const reason = errMessage(err);
      runLog.error("run failed", { error: reason });
      batcher.emit({ kind: "error", agent: "worker", payload: { text: reason } });
      await batcher.close().catch(() => undefined);
      await this.client
        .reportState(runId, { status: "failed", failure_reason: reason })
        .catch((e) => runLog.error("could not report failed state", { error: errMessage(e) }));
    } finally {
      if (barePath && worktreePath) {
        await this.git
          .removeWorktree(barePath, worktreePath)
          .catch((e) => runLog.warn("worktree cleanup failed", { error: errMessage(e) }));
      }
    }
  }
}
