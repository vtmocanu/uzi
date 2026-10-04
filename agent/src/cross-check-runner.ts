import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { RequestError, type WorkerClient, type PlanCrossCheckFindings } from "./client.js";
import type { ClaimResponse } from "./protocol.js";
import type { GitCache } from "./git.js";
import type { Logger } from "./log.js";
import type { ActiveRunRegistry } from "./active-run-registry.js";
import type { Outbox } from "./outbox.js";
import { CodexCrossCheck, CrossCheckMalformedError } from "./codex/cross-check.js";
import { selectCodexBinding } from "./codex/select.js";
import { ChatSteering } from "./steering.js";
import { makeTerminalOutboxDeps, postTerminalState, isStaleClaimRefusal } from "./terminal-resolve.js";
import { errMessage } from "./util.js";
import { rmHomeTree } from "./rmtree.js";
import { uidSplitActive } from "./runner-uid.js";

// The verdict route uses reason-class refusals, whereas ordinary state routes use disposition.
function checkerClaimRefused(err: unknown): boolean {
  if (isStaleClaimRefusal(err)) return true;
  if (!(err instanceof RequestError) || err.status !== 409) return false;
  try {
    const body = JSON.parse(err.body) as { reason?: unknown };
    return body.reason === "cross_check_refused" || body.reason === "interrupted";
  } catch { return false; }
}

interface CrossCheckRunnerOptions {
  homeRoot?: string;
  modelTimeoutMs?: number;
  pollMs?: number;
  activeRuns?: ActiveRunRegistry;
  outbox?: Outbox;
  outboxTerminalMaxBytes?: number;
  gapFillMax?: number;
  model?: Pick<CodexCrossCheck, "run">;
}

/** Report-only plan checker. The child owns its checkout, usage and terminal journal. */
export class CrossCheckRunner {
  private readonly model: Pick<CodexCrossCheck, "run">;
  private readonly terminalDeps;
  constructor(private readonly client: WorkerClient, private readonly git: GitCache,
    private readonly log: Logger, private readonly opts: CrossCheckRunnerOptions = {}) {
    this.model = opts.model ?? new CodexCrossCheck(client, log);
    this.terminalDeps = makeTerminalOutboxDeps(opts.outbox, client, {
      gapFillMax: opts.gapFillMax ?? 10_000,
      terminalMaxBytes: opts.outboxTerminalMaxBytes ?? Math.round(1.25 * 1024 * 1024), log,
    });
  }

  async execute(claim: ClaimResponse, signal?: AbortSignal): Promise<void> {
    const runId = claim.run_id;
    const generation = claim.claim_generation ?? 0;
    const cancel = new AbortController();
    const abort = (): void => cancel.abort(signal?.reason);
    signal?.addEventListener("abort", abort, { once: true });
    if (signal?.aborted) abort();
    let timer: NodeJS.Timeout | undefined;
    let timedOut = false;
    let checkout: string | undefined;
    let home: string | undefined;
    let steering: ChatSteering | undefined;
    let seq = claim.last_seq ?? 0;
    let verdictDelivered = false;
    const custodyLost = async (): Promise<boolean> => {
      try {
        const ack = await this.client.reportState(runId, { status: "running", claim_generation: claim.claim_generation });
        return ack?.staleClaim === true;
      } catch (err) { return isStaleClaimRefusal(err); }
    };
    let reason: "malformed" | "model_error" | "model_timeout" | "checker_unavailable" | "confinement_failed" = "checker_unavailable";
    this.opts.activeRuns?.add(runId, generation);
    try {
      const ack = await this.client.reportState(runId, { status: "running", claim_generation: claim.claim_generation });
      if (ack?.staleClaim) return;
      const candidate = claim.cross_check;
      if (claim.kind !== "cross_check" || selectCodexBinding(claim.secrets).kind !== "codex"
        || !candidate || candidate.stage !== "plan" || candidate.round !== 1
        || !/^[a-f0-9]{40}$/.test(candidate.base_commit)) throw new Error("invalid plan checker claim");
      const deadline = Date.parse(candidate.deadline_at);
      const timeout = Math.min(this.opts.modelTimeoutMs ?? 15 * 60_000, deadline - Date.now());
      if (!Number.isFinite(timeout) || timeout <= 0) {
        reason = "model_timeout";
        throw new Error("cross-check deadline expired");
      }
      timer = setTimeout(() => { timedOut = true; cancel.abort(new Error("cross-check timed out")); }, timeout);
      cancel.signal.throwIfAborted();
      steering = new ChatSteering(this.client, runId, this.opts.pollMs ?? 1000, this.log, cancel,
        { onFollowUp: () => this.log.warn("cross-check ignores follow-up input", { run_id: runId }) }, generation);
      steering.start();
      const bare = await this.git.ensureClone(claim.repo.clone_url, claim.secrets.forge_pat, claim.secrets.forge_username);
      cancel.signal.throwIfAborted();
      checkout = await this.git.runnerCloneAtCommit(bare, candidate.base_commit, runId);
      cancel.signal.throwIfAborted();
      await fs.mkdir(this.opts.homeRoot ?? os.tmpdir(), { recursive: true });
      home = await fs.mkdtemp(path.join(this.opts.homeRoot ?? os.tmpdir(), "uzi-cross-check-"));
      if (uidSplitActive()) await fs.chmod(home, 0o2770);
      reason = "model_error";
      const text = await this.model.run(claim, checkout, home, cancel.signal, async (payload) => {
        await this.client.postMessages(runId, [{ seq: ++seq, kind: "status", agent: "cross-checker", payload }], generation);
      });
      cancel.signal.throwIfAborted();
      reason = "malformed";
      const parsed = JSON.parse(text) as PlanCrossCheckFindings & { verdict: "approve" | "revise" | "block" };
      if (!parsed || !["approve", "revise", "block"].includes(parsed.verdict)
        || typeof parsed.summary !== "string" || !Array.isArray(parsed.items)
        || Object.keys(parsed).sort().join(",") !== "items,summary,verdict") throw new Error("invalid cross-check verdict");
      reason = "model_error";
      const probe = await this.client.reportState(runId, { status: "running", claim_generation: claim.claim_generation }, cancel.signal);
      cancel.signal.throwIfAborted();
      if (probe?.staleClaim || steering.claimLost()) return;
      reason = "model_error";
      const decision = parsed.verdict === "approve" ? { verdict: "approve", reason_class: "approve" } as const
        : parsed.verdict === "revise" ? { verdict: "revise", reason_class: "revise" } as const
        : { verdict: "block", reason_class: "block" } as const;
      await this.client.reportCrossCheckVerdict(runId, generation, { ...parsed, ...decision }, cancel.signal);
      verdictDelivered = true;
      await postTerminalState(this.terminalDeps, this.client, {
        runId, claimGeneration: generation, phase: "running", messagesThroughSeq: seq,
        body: { status: "completed", claim_generation: claim.claim_generation },
      });
    } catch (err) {
      if (isStaleClaimRefusal(err) || steering?.claimLost()) return;
      if (checkerClaimRefused(err) && await custodyLost()) return;
      if (timedOut) reason = "model_timeout";
      else if (err instanceof CrossCheckMalformedError) reason = "malformed";
      else if (/confinement|cleanup unconfirmed/.test(errMessage(err))) reason = "confinement_failed";
      try {
        if (!verdictDelivered) await this.client.reportCrossCheckVerdict(runId, generation, {
          verdict: "failed", reason_class: reason, summary: "The cross-check did not complete.", items: [],
        });
      } catch (delivery) {
        if (isStaleClaimRefusal(delivery) || (checkerClaimRefused(delivery) && await custodyLost())) return;
        this.log.warn("cross-check failed verdict delivery failed", { run_id: runId, error: errMessage(delivery) });
      }
      try {
        await postTerminalState(this.terminalDeps, this.client, {
          runId, claimGeneration: generation, phase: "running", messagesThroughSeq: seq,
          body: { status: "failed", failure_reason: "The cross-check did not complete.", claim_generation: claim.claim_generation },
        });
      } catch (terminal) {
        this.log.warn("cross-check failed-state delivery pending", { run_id: runId, error: errMessage(terminal) });
      }
    } finally {
      if (timer) clearTimeout(timer);
      cancel.abort();
      signal?.removeEventListener("abort", abort);
      await steering?.stop().catch((err) => this.log.warn("cross-check steering cleanup failed", { error: errMessage(err) }));
      if (checkout) await this.git.removeRunnerClone(checkout).catch((err) =>
        this.log.warn("cross-check checkout cleanup failed", { error: errMessage(err) }));
      if (home) await rmHomeTree(home).catch((err) =>
        this.log.warn("cross-check home cleanup failed", { error: errMessage(err) }));
      this.opts.activeRuns?.remove(runId);
    }
  }
}
