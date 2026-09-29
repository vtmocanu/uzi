// The IsolatedRunner (PRD #1906 M4): a slim runner for a profile-bound research claim
// (one carrying `isolated_fetch`), modelled on the JudgeRunner. It has NO git
// collaborator at all: no clone, no worktree, no push, no MR, and it never reaches
// RunRunner.execute or ensureClone. That absence is structural, not a runtime check.
//
// It owns the run's own state reports, its terminal outbox journal, its active-run
// registration (so it rides the heartbeat's ActiveSnapshot), and cancel (a `cancel`
// input aborts the session through the chat steering channel's poll of GET /inputs).
// It works in a fresh per-run directory under the data dir, removed when the run ends
// (lane workers are run-bound, so nothing survives into another run either way).
//
// Fail closed, before anything starts (the FailClosedExecutor idiom, codex-executor.ts):
// a claim with a Codex block (Decision 10: Codex's own web search is server-side), a
// forge credential (Decision 12), a malformed grant, or a worker without UZI_FETCHER_URL
// and UZI_FETCHER_CA_FILE is reported failed without a session, a workspace or a fetch.

import fsp from "node:fs/promises";
import path from "node:path";

import type { ActiveRunRegistry } from "./active-run-registry.js";
import { MessageBatcher } from "./batcher.js";
import type { WorkerClient } from "./client.js";
import { buildFetchToolsServer } from "./fetch-tools.js";
import { IsolatedExecutor, type IsolatedContext } from "./isolated-executor.js";
import { LimitReachedError } from "./limit.js";
import type { Logger } from "./log.js";
import type { Outbox } from "./outbox.js";
import type { ClaimResponse, IsolatedFetchClaim, StateRequest } from "./protocol.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { ChatSteering } from "./steering.js";
import { makeTerminalOutboxDeps, postTerminalState, type TerminalOutboxDeps } from "./terminal-resolve.js";
import { errMessage } from "./util.js";

const MAX_FAILURE_REASON_LEN = 512;
const DEFAULT_MAX_TURNS = 200;
const DEFAULT_TIMEOUT_MS = 60 * 60 * 1000;
const DEFAULT_POLL_MS = 3000;

/** What the runner drives: the real IsolatedExecutor, or a test fake. */
export interface IsolatedExecutorLike {
  run(ctx: IsolatedContext): Promise<void>;
}

/** The cancel channel: started with the run, stopped at its end. */
interface IsolatedCancelSource {
  start(): void;
  stop(): Promise<void>;
  claimLost?(): boolean;
}

export interface IsolatedRunnerOptions {
  /** The worker data dir; per-run dirs are `<dataDir>/isolated/<runId>`. */
  dataDir: string;
  /** UZI_FETCHER_URL / UZI_FETCHER_CA_FILE. Either unset means every isolated claim fails closed. */
  fetcherUrl?: string;
  fetcherCaFile?: string;
  batchMs: number;
  /** The worker's join token, redacted from every message payload. */
  joinToken?: string;
  /** Worker credential paths the path guard denies. */
  secretPaths?: readonly string[];
  executor?: IsolatedExecutorLike;
  makeSource?: (runId: string, cancel: AbortController, log: Logger, claimGeneration: number) => IsolatedCancelSource;
  pollMs?: number;
  maxTurns?: number;
  activeRuns?: ActiveRunRegistry;
  outbox?: Outbox;
  rearm?: Map<string, () => void>;
  outboxTerminalMaxBytes?: number;
  gapFillMax?: number;
}

/** A shape-valid isolated grant, or undefined. */
function validGrant(v: unknown): IsolatedFetchClaim | undefined {
  if (!v || typeof v !== "object") return undefined;
  const g = v as Record<string, unknown>;
  if (typeof g.credential !== "string" || !g.credential.trim()) return undefined;
  if (typeof g.profile !== "string") return undefined;
  if (!Array.isArray(g.hosts) || !g.hosts.every((h) => typeof h === "string")) return undefined;
  return { credential: g.credential, profile: g.profile, hosts: g.hosts as string[] };
}

/**
 * Why this isolated claim must not start, or undefined when it may. Pure: no I/O, so a
 * refused claim touches nothing on the worker.
 */
function preflightReason(claim: ClaimResponse, opts: IsolatedRunnerOptions): string | undefined {
  if (!validGrant(claim.isolated_fetch)) return "profile-bound claim carried a malformed isolated_fetch grant";
  const secrets = claim.secrets as Partial<ClaimResponse["secrets"]> | undefined;
  if (secrets?.codex !== undefined && secrets.codex !== null) {
    return "a profile-bound run cannot run on the Codex harness (its web search is server-side)";
  }
  if (secrets?.forge_pat) return "a profile-bound claim must not carry a forge credential";
  if (!opts.fetcherUrl || !opts.fetcherCaFile) {
    return "this worker is not configured for the isolated lane (UZI_FETCHER_URL and UZI_FETCHER_CA_FILE are required)";
  }
  if (!secrets?.anthropic_oauth_token?.trim()) return "profile-bound claim carried no Anthropic OAuth token";
  return undefined;
}

function buildPrompt(claim: ClaimResponse, grant: IsolatedFetchClaim): string {
  const hosts = grant.hosts.length ? grant.hosts.map((h) => `- ${h}`).join("\n") : "- (none)";
  return [
    `Research task: ${claim.issue_title ?? ""}`,
    "",
    claim.issue_description ?? "",
    "",
    `Allowed site list "${grant.profile}" (the fetcher enforces it; other hosts are refused):`,
    hosts,
  ].join("\n");
}

export class IsolatedRunner {
  private readonly executor: IsolatedExecutorLike;
  private readonly terminalDeps: TerminalOutboxDeps | undefined;
  private readonly makeSource: NonNullable<IsolatedRunnerOptions["makeSource"]>;

  constructor(
    private readonly client: WorkerClient,
    private readonly log: Logger,
    private readonly opts: IsolatedRunnerOptions,
  ) {
    this.executor = opts.executor ?? new IsolatedExecutor(log, { secretPaths: opts.secretPaths ?? [] });
    this.terminalDeps = makeTerminalOutboxDeps(opts.outbox, client, {
      gapFillMax: opts.gapFillMax ?? 10_000,
      terminalMaxBytes: opts.outboxTerminalMaxBytes ?? Math.round(1.25 * 1024 * 1024),
      log,
    });
    const pollMs = opts.pollMs ?? DEFAULT_POLL_MS;
    this.makeSource =
      opts.makeSource ??
      ((runId, cancel, runLog, generation) => new ChatSteering(client, runId, pollMs, runLog, cancel, {}, generation));
  }

  /** Report this run failed (journaled when an outbox is wired). Never throws. */
  private async reportFailed(runId: string, generation: number, reason: string, cause: unknown, fence: number): Promise<void> {
    const body: StateRequest =
      cause instanceof LimitReachedError
        ? { status: "failed", rate_limit_type: cause.rateLimitType, limit_resets_at: cause.resetsAtMs, claim_generation: generation }
        : { status: "failed", failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN), claim_generation: generation };
    try {
      await postTerminalState(this.terminalDeps, this.client, {
        runId,
        claimGeneration: generation,
        phase: "running",
        messagesThroughSeq: fence,
        body,
      });
    } catch (err) {
      this.log.warn("isolated failed-state report failed", { run_id: runId, error: errMessage(err) });
    }
  }

  /** Run one isolated claim end to end. Never throws. */
  async execute(claim: ClaimResponse): Promise<void> {
    const runId = claim.run_id;
    const generation = claim.claim_generation ?? 0;
    const runLog = this.log.child({ run_id: runId, kind: "isolated" });
    const grant = validGrant(claim.isolated_fetch);
    const token = claim.secrets?.anthropic_oauth_token?.trim() ?? "";
    // Registered before anything can log; evicted in the finally.
    const runSecrets = [token, grant?.credential ?? ""].filter((s) => s.length > 0);
    for (const s of runSecrets) this.log.addSecret(s);
    this.opts.activeRuns?.add(runId, generation);

    const refused = preflightReason(claim, this.opts);
    if (refused || !grant) {
      runLog.error("isolated claim refused before start", { reason: refused });
      await this.reportFailed(runId, generation, refused ?? "malformed isolated claim", undefined, 0);
      this.finish(runId, runSecrets);
      return;
    }

    const secrets = [token, grant.credential, this.opts.joinToken];
    const redactText = makeTextRedactor(secrets);
    const runDir = path.join(this.opts.dataDir, "isolated", runId);
    let batcher: MessageBatcher | undefined;
    let source: IsolatedCancelSource | undefined;
    try {
      let ca: Buffer;
      try {
        ca = await fsp.readFile(this.opts.fetcherCaFile!);
      } catch (err) {
        throw new Error(`could not read the fetcher CA bundle: ${errMessage(err)}`);
      }

      const ack = await this.client.reportState(runId, { status: "running", claim_generation: generation });
      if (ack?.staleClaim) {
        runLog.warn("isolated claim superseded (stale) at running report; abandoning");
        return;
      }

      await fsp.rm(runDir, { recursive: true, force: true });
      const workspace = path.join(runDir, "workspace");
      const homeDir = path.join(runDir, "home");
      await fsp.mkdir(workspace, { recursive: true, mode: 0o700 });
      await fsp.mkdir(homeDir, { recursive: true, mode: 0o700 });

      const b = new MessageBatcher(this.client, runId, claim.last_seq ?? 0, this.opts.batchMs, runLog, makeRedactor(secrets), redactText, {
        ...(this.opts.outbox ? { outbox: this.opts.outbox } : {}),
        generation,
      });
      batcher = b;
      this.opts.rearm?.set(runId, () => b.rearm());

      const cancel = new AbortController();
      source = this.makeSource(runId, cancel, runLog, generation);
      source.start();

      const fetch = buildFetchToolsServer({
        fetcherUrl: this.opts.fetcherUrl!,
        ca,
        credential: grant.credential,
        workspace,
        log: runLog,
      });
      const timeoutSeconds = claim.config?.run_timeout_seconds;
      await this.executor.run({
        runId,
        oauthToken: token,
        workspace,
        homeDir,
        prompt: buildPrompt(claim, grant),
        fetchServer: fetch.server,
        emit: (m) => b.emit(m),
        signal: cancel.signal,
        maxTurns: this.opts.maxTurns ?? DEFAULT_MAX_TURNS,
        timeoutMs: typeof timeoutSeconds === "number" && timeoutSeconds > 0 ? timeoutSeconds * 1000 : DEFAULT_TIMEOUT_MS,
        ...(claim.config?.default_model ? { model: claim.config.default_model } : {}),
        ...(claim.config?.default_effort ? { effort: claim.config.default_effort } : {}),
      });
      await source.stop();
      if (source.claimLost?.()) {
        await b.close().catch(() => undefined);
        return;
      }
      b.emit({ kind: "status", agent: "worker", payload: { text: "research run completed" } });
      await b.close();
      await postTerminalState(this.terminalDeps, this.client, {
        runId,
        claimGeneration: generation,
        phase: "running",
        messagesThroughSeq: b.currentSeq(),
        body: { status: "completed", claim_generation: generation },
      });
      runLog.info("isolated run completed");
    } catch (err) {
      await source?.stop().catch(() => undefined);
      if (source?.claimLost?.()) {
        await batcher?.close().catch(() => undefined);
        return;
      }
      const reason = redactText(errMessage(err));
      runLog.error("isolated run failed", { error: reason });
      batcher?.emit({ kind: "error", agent: "worker", payload: { text: reason } });
      await batcher?.close().catch(() => undefined);
      await this.reportFailed(runId, generation, reason, err, batcher?.currentSeq() ?? 0);
    } finally {
      await fsp.rm(runDir, { recursive: true, force: true }).catch((err) =>
        runLog.warn("could not remove the isolated run directory", { error: errMessage(err) }),
      );
      this.finish(runId, runSecrets);
    }
  }

  private finish(runId: string, runSecrets: string[]): void {
    this.opts.rearm?.delete(runId);
    this.opts.activeRuns?.remove(runId);
    for (const s of runSecrets) this.log.removeSecret(s);
  }
}
