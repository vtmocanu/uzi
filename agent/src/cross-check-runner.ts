import fs from "node:fs/promises";
import { decodeCrossCheckClaim, decodeCodeSnapshotCleanup } from "./code-cross-check-contract.js";
import os from "node:os";
import path from "node:path";
import { RequestError, type WorkerClient, type PlanCrossCheckFindings } from "./client.js";
import type { ClaimResponse } from "./protocol.js";
import type { GitCache, CodeSnapshotBootCandidate } from "./git.js";
import type { Logger } from "./log.js";
import type { ActiveRunRegistry } from "./active-run-registry.js";
import type { Outbox } from "./outbox.js";
import { CodexCrossCheck, CrossCheckMalformedError, validateCodeFindings } from "./codex/cross-check.js";
import type { SdkQueryFn } from "./sdk-executor.js";
import { ClaudeCrossCheck } from "./claude-cross-check.js";
import { SECRET_PATH_PREFIXES } from "./guardrails.js";
import { selectCodexBinding } from "./codex/select.js";
import { CrossCheckCheckerUnavailableError } from "./codex/model-rejection.js";
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
  /** The Claude checker (PRD #2460), used for a claim that carries only an Anthropic credential. */
  claudeModel?: Pick<ClaudeCrossCheck, "run">;
  /** Worker credential paths the Claude checker's path guard denies (default: the built-in prefix). */
  secretPaths?: readonly string[];
  /** Test seam for the default Claude checker's SDK query (UZI_EXECUTOR=stub injects the canned one). */
  claudeQueryFn?: SdkQueryFn;
}

/** Report-only plan checker. The child owns its checkout, usage and terminal journal. */
export class CrossCheckRunner {
  private readonly model: Pick<CodexCrossCheck, "run">;
  private readonly claudeModel: Pick<ClaudeCrossCheck, "run">;
  private readonly terminalDeps;
  constructor(private readonly client: WorkerClient, private readonly git: GitCache,
    private readonly log: Logger, private readonly opts: CrossCheckRunnerOptions = {}) {
    this.model = opts.model ?? new CodexCrossCheck(client, log);
    this.claudeModel = opts.claudeModel ?? new ClaudeCrossCheck(log, {
      secretPaths: opts.secretPaths ?? SECRET_PATH_PREFIXES,
      ...(opts.claudeQueryFn ? { queryFn: opts.claudeQueryFn } : {}),
    });
    this.terminalDeps = makeTerminalOutboxDeps(opts.outbox, client, {
      gapFillMax: opts.gapFillMax ?? 10_000,
      terminalMaxBytes: opts.outboxTerminalMaxBytes ?? Math.round(1.25 * 1024 * 1024), log,
    });
  }

  async snapshotBootCodeSnapshots(signal?: AbortSignal): Promise<CodeSnapshotBootCandidate[]> {
    return await this.git.discoverCodeSnapshots(signal);
  }

  async cleanupBootCodeSnapshots(candidates: CodeSnapshotBootCandidate[], signal: AbortSignal): Promise<void> {
    // Freeze pre-registration observations; at most 256 reads, no retries. A failed
    // candidate does not block siblings; the 30s deadline retains the remaining refs.
    const frozen = structuredClone(candidates.slice(0, 256));
    const deadline = Date.now() + 30_000;
    const passSignal = AbortSignal.any([signal, AbortSignal.timeout(30_000)]);
    let errors = 0;
    for (const candidate of frozen) {
      if (passSignal.aborted || Date.now() >= deadline) break;
      const { barePath, leadId, head, metadata } = candidate;
      const child = metadata.latestChild;
      if (!child || metadata.readers.length === 0 || metadata.readers.some(r => r.state !== "CLOSED")) continue;
      try {
        const server = decodeCodeSnapshotCleanup(await this.client.getCodeSnapshotCleanup(leadId,
          AbortSignal.any([passSignal, AbortSignal.timeout(Math.min(3000, deadline - Date.now()))])));
        if (passSignal.aborted || Date.now() >= deadline) break;
        const terminal = server.checker_status;
        const leadTerminal = server.lead_status === "completed" || server.lead_status === "failed" || server.lead_status === "cancelled";
        if (server.lead_run_id !== leadId || server.head_commit !== head ||
            (server.outcome !== "completed" && server.outcome !== "failed") ||
            server.checker_run_id !== child.childId || server.checker_claim_generation !== child.childGeneration ||
            (terminal !== "completed" && terminal !== "failed" && terminal !== "cancelled") ||
            (!leadTerminal && server.owned_by_worker !== false)) continue;
        await this.git.deleteCodeSnapshot(barePath, leadId, head, {
          sealedOutcome: { persisted: true, sealed: true, stage: "code", leadId, head,
            pinGeneration: metadata.pinGeneration, outcome: server.outcome },
          child: { childId: child.childId, childGeneration: child.childGeneration, terminalStatus: terminal },
          bootCandidate: candidate, leadDisposition: leadTerminal ? "terminal" : "foreign",
        });
      } catch {
        errors++;
      }
    }
    if (errors) this.log.warn("code snapshot boot cleanup retained candidates after errors", { count: errors });
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
    let modelInvoked = false;
    let nativeCleanupConfirmed = false;
    let readersClean = true;
    let snapshotOwned = false;
    const code = claim.cross_check?.stage === "code" ? claim.cross_check : undefined;
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
      const candidate = decodeCrossCheckClaim(claim.cross_check);
      // Single-family custody: a Codex checker holds only a Codex credential, a Claude checker only
      // the Anthropic token (PRD #2460); a claim carrying both, or neither, is invalid.
      const family = selectCodexBinding(claim.secrets).kind;
      const hasAnthropic = Boolean(claim.secrets.anthropic_oauth_token);
      const checker = family === "codex" ? (hasAnthropic ? undefined : this.model)
        : hasAnthropic ? this.claudeModel : undefined;
      if (claim.kind !== "cross_check" || !checker || !candidate
        || (candidate.stage !== "plan" && candidate.stage !== "code")
        || !Number.isInteger(candidate.round) || candidate.round < 1 || candidate.round > 5
        || !/^[a-f0-9]{40}$/.test(candidate.base_commit)
        || (code && (candidate.round !== 1 || !/^[a-f0-9]{40}$/.test(code.head_commit))))
        throw new Error("invalid checker claim");
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
      const bare = code ? this.git.barePathFor(claim.repo.clone_url)
        : await this.git.ensureClone(claim.repo.clone_url, claim.secrets.forge_pat, claim.secrets.forge_username);
      cancel.signal.throwIfAborted();
      if (code) {
        // Only the existing local bare and exact lead ref admit a code reader. No origin fetch.
        checkout = await this.git.runnerCloneAtCommit(bare, code.head_commit, runId, code.lead_run_id, generation);
        snapshotOwned = true;
        const diff = await this.git.readBare(bare,
          ["diff", "--no-ext-diff", "--no-textconv", "--no-color", `${code.base_commit}...${code.head_commit}`],
          { maxBytes: 1024 * 1024, signal: cancel.signal });
        // Never present a truncated patch as a complete diff; file tools still see exact H.
        code.code_diff = diff.truncated ? (checker === this.claudeModel
          ? "Diff exceeds 1 MiB; inspect committed files through Read, Grep and Glob."
          : "Diff exceeds 1 MiB; inspect committed files through Read and Search.") : diff.text;
      } else checkout = await this.git.runnerCloneAtCommit(bare, candidate.base_commit, runId);
      cancel.signal.throwIfAborted();
      await fs.mkdir(this.opts.homeRoot ?? os.tmpdir(), { recursive: true });
      home = await fs.mkdtemp(path.join(this.opts.homeRoot ?? os.tmpdir(), "uzi-cross-check-"));
      if (uidSplitActive()) await fs.chmod(home, 0o2770);
      reason = "model_error";
      if (!checkout) throw new Error("checker checkout unavailable");
      modelInvoked = true;
      const text = await checker!.run(claim, checkout, home, cancel.signal, async (payload) => {
        await this.client.postMessages(runId, [{ seq: ++seq, kind: "status", agent: "cross-checker", payload }], generation);
      }, code ? () => { nativeCleanupConfirmed = true; } : undefined);
      cancel.signal.throwIfAborted();
      reason = "malformed";
      if (code) {
        if (!nativeCleanupConfirmed) throw new Error("cross-check cleanup unconfirmed");
        const findings = validateCodeFindings(text);
        const probe = await this.client.reportState(runId, { status: "running", claim_generation: claim.claim_generation }, cancel.signal);
        cancel.signal.throwIfAborted();
        if (probe?.staleClaim || steering.claimLost()) return;
        await this.client.reportCodeCrossCheckVerdict(runId, generation, { outcome: "completed", findings }, cancel.signal);
        verdictDelivered = true;
        await postTerminalState(this.terminalDeps, this.client, {
          runId, claimGeneration: generation, phase: "running", messagesThroughSeq: seq,
          body: { status: "completed", claim_generation: claim.claim_generation },
        });
        return;
      }
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
      else if (!cancel.signal.aborted && err instanceof CrossCheckCheckerUnavailableError) reason = "checker_unavailable";
      else if (err instanceof CrossCheckMalformedError || (err instanceof Error && /invalid code cross-check|JSON/.test(err.message))) reason = "malformed";
      else if (/confinement|cleanup unconfirmed/.test(errMessage(err))) reason = "confinement_failed";
      try {
        if (!verdictDelivered) {
          if (code) await this.client.reportCodeCrossCheckVerdict(runId, generation, { outcome: "failed", reason_class: reason, findings: [] });
          else await this.client.reportCrossCheckVerdict(runId, generation, {
            verdict: "failed", reason_class: reason, summary: "The cross-check did not complete.", items: [],
          });
          verdictDelivered = true;
        }
      } catch (delivery) {
        if (isStaleClaimRefusal(delivery) || (checkerClaimRefused(delivery) && await custodyLost())) return;
        this.log.warn("cross-check failed verdict delivery failed", { run_id: runId, error: errMessage(delivery) });
      }
      const failureReason = reason === "checker_unavailable" && err instanceof CrossCheckCheckerUnavailableError
        ? "plan cross-check: checker unavailable" : "The cross-check did not complete.";
      try {
        await postTerminalState(this.terminalDeps, this.client, {
          runId, claimGeneration: generation, phase: "running", messagesThroughSeq: seq,
          body: { status: "failed", failure_reason: failureReason, claim_generation: claim.claim_generation },
        });
      } catch (terminal) {
        this.log.warn("cross-check failed-state delivery pending", { run_id: runId, error: errMessage(terminal) });
      }
    } finally {
      if (timer) clearTimeout(timer);
      cancel.abort();
      signal?.removeEventListener("abort", abort);
      await steering?.stop().catch((err) => {
        readersClean = false;
        this.log.warn("cross-check steering cleanup failed", { error: errMessage(err) });
      });
      if (checkout) await this.git.removeRunnerClone(checkout).catch((err) => {
        readersClean = false;
        this.log.warn("cross-check checkout cleanup failed", { error: errMessage(err) });
      });
      if (home) await rmHomeTree(home).catch((err) => {
        readersClean = false;
        this.log.warn("cross-check home cleanup failed", { error: errMessage(err) });
      });
      if (code && snapshotOwned && checkout && readersClean && (!modelInvoked || nativeCleanupConfirmed)) {
        // One bounded server read; unknown responses retain the exact reader and ref.
        try {
          const metadata = await this.client.getCodeSnapshotCleanup(code.lead_run_id, AbortSignal.timeout(3000));
          const terminal = metadata.checker_status;
          if (metadata.lead_run_id === code.lead_run_id && metadata.head_commit === code.head_commit &&
            metadata.checker_run_id === runId &&
            metadata.checker_claim_generation === generation &&
            (metadata.outcome === "completed" || metadata.outcome === "failed") &&
            (terminal === "completed" || terminal === "failed" || terminal === "cancelled")) {
            await this.git.closeCodeSnapshotReader(this.git.barePathFor(claim.repo.clone_url),
              code.lead_run_id, checkout, { childId: runId, childGeneration: generation,
                terminalStatus: terminal, cleanupConfirmed: true });
            const bare = this.git.barePathFor(claim.repo.clone_url);
            const local = await this.git.codeSnapshotCandidate(bare, code.lead_run_id);
            if (local) await this.git.deleteCodeSnapshot(bare, code.lead_run_id, code.head_commit, {
              sealedOutcome: { persisted: true, sealed: true, stage: "code", leadId: code.lead_run_id,
                head: code.head_commit, pinGeneration: local.metadata.pinGeneration, outcome: metadata.outcome },
              child: { childId: runId, childGeneration: generation, terminalStatus: terminal },
            });
          }
        } catch (err) {
          this.log.warn("code snapshot retained after cleanup error", { error: errMessage(err) });
        }
      }
      this.opts.activeRuns?.remove(runId);
    }
  }
}
