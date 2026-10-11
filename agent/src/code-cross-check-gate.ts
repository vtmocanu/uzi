import { setTimeout as delay } from "node:timers/promises";
import type { WorkerClient } from "./client.js";
import { decodeCodeCrossCheckDispositions, type CodeCrossCheckDispositionBatch, type CodeCrossCheckRecord, type CodeCrossCheckStatus } from "./code-cross-check-contract.js";
import { buildAutomaticRevisionPrompt } from "./prompt.js";

export type CodeCrossCheckSummary = CodeCrossCheckRecord | { incomplete: string };
function maskSupersededAdvice(row: CodeCrossCheckRecord): CodeCrossCheckRecord {
  return { ...row, findings: [], ...(row.dispositions ? { dispositions: [] } : {}) };
}
export function persistedCodeCrossCheckSummary(status: CodeCrossCheckStatus | undefined, generation: number): CodeCrossCheckSummary {
  if (!status || "result" in status) return { incomplete: "status unavailable" };
  if (status.candidate_generation !== generation && status.interrupted_at === null) return { incomplete: "interrupted" };
  return status.interrupted_at === null ? status : maskSupersededAdvice(status);
}
export type CodeCrossCheckDecision = { action: "proceed" } | { action: "repair"; followUp: string };

/** Run-local authority; every use also revalidates the persisted generation and row identity. */
export class CodeCrossCheckGate {
  private called = false;
  private active: CodeCrossCheckRecord | undefined;
  private summary: CodeCrossCheckSummary | undefined;
  private reporting = false;
  private repairSignal: AbortController | undefined;
  constructor(private readonly options: Parameters<typeof checkCode>[0] & {
    client: Pick<WorkerClient, "codeCrossCheckStatus" | "submitCodeCrossCheck" | "reportCodeCrossCheckDispositions">;
  }) {}

  private remember(status: CodeCrossCheckStatus | undefined): CodeCrossCheckRecord | undefined {
    this.summary = persistedCodeCrossCheckSummary(status, this.options.generation);
    return "incomplete" in this.summary ? undefined : this.summary;
  }

  private async read(): Promise<CodeCrossCheckStatus | undefined> {
    try {
      const status = await this.options.client.codeCrossCheckStatus(
        this.options.runId, this.options.generation,
        AbortSignal.any([this.options.signal, AbortSignal.timeout(this.options.requestMs ?? 3000)]));
      this.remember(status);
      return status;
    } catch {
      this.options.signal.throwIfAborted();
      return this.remember(undefined);
    }
  }

  private current(row: CodeCrossCheckStatus | undefined): row is CodeCrossCheckRecord {
    return row !== undefined && !("result" in row) && row.candidate_generation === this.options.generation
      && row.interrupted_at === null && row.outcome === "completed"
      && row.head_commit !== null && row.base_commit !== null;
  }

  private same(row: CodeCrossCheckRecord, authorized: CodeCrossCheckRecord): boolean {
    return row.candidate_digest === authorized.candidate_digest
      && row.head_commit === authorized.head_commit && row.base_commit === authorized.base_commit
      && row.checker_run_id === authorized.checker_run_id
      && JSON.stringify(row.findings) === JSON.stringify(authorized.findings);
  }

  repairActive(): boolean {
    return this.active !== undefined && !this.options.signal.aborted && this.repairSignal?.signal.aborted === false;
  }

  async repair(): Promise<CodeCrossCheckRecord | undefined> {
    const authorized = this.active;
    if (!authorized) return undefined;
    const row = await this.read();
    // An unavailable GET refuses this use; only a fresh successful read can authorize a retry.
    if (row === undefined) return undefined;
    if (this.active !== authorized || this.options.signal.aborted || !this.current(row) || !this.same(row, authorized)) {
      if (this.active === authorized) this.active = undefined;
      return undefined;
    }
    return row;
  }

  async report(batch: CodeCrossCheckDispositionBatch): Promise<CodeCrossCheckStatus> {
    if (this.reporting) throw new Error("code cross-check report already in flight");
    this.reporting = true;
    try {
      const authorized = this.active;
      const repairSignal = this.repairSignal;
      const row = await this.repair();
      if (!row) throw new Error("code cross-check repair is not active");
      const validated = decodeCodeCrossCheckDispositions(batch, row.findings);
      if (validated.some(d => d.disposition === "not_reported")) throw new Error("invalid model disposition");
      if (!authorized || this.active !== authorized || !repairSignal || repairSignal.signal.aborted) throw new Error("code cross-check repair is not active");
      this.options.signal.throwIfAborted();
      // The server finalizes write-once; identical ACK retries remain fenced to this active pass.
      const result = await this.options.client.reportCodeCrossCheckDispositions(
        this.options.runId, this.options.generation, batch,
        AbortSignal.any([this.options.signal, repairSignal.signal, AbortSignal.timeout(this.options.requestMs ?? 3000)]));
      repairSignal.signal.throwIfAborted();
      this.options.signal.throwIfAborted();
      this.remember(result);
      return result;
    } finally {
      this.reporting = false;
    }
  }

  observed(): CodeCrossCheckSummary | undefined { return this.summary; }

  async persistedSummary(): Promise<CodeCrossCheckSummary | undefined> {
    if (this.called) await this.read();
    return this.summary;
  }

  async done(): Promise<CodeCrossCheckDecision> {
    const first = !this.called;
    this.called = true;
    // Closing authority precedes final done. Later calls only read; they never snapshot or submit.
    this.repairSignal?.abort();
    this.active = undefined;
    const row = first ? this.remember(await checkCode(this.options)) : await this.read();
    if (!this.current(row)) return { action: "proceed" };
    if (first && row.findings.length > 0 && row.finalized_at == null) {
      this.repairSignal = new AbortController();
      this.active = row;
      return { action: "repair", followUp: buildAutomaticRevisionPrompt("code", { findings: row.findings }) };
    }
    if (row.finalized_at == null) {
      try {
        this.remember(await this.options.client.reportCodeCrossCheckDispositions(
          this.options.runId, this.options.generation, [],
          AbortSignal.any([this.options.signal, AbortSignal.timeout(this.options.requestMs ?? 3000)])));
      } catch {
        this.options.signal.throwIfAborted();
        await this.read();
      }
    }
    return { action: "proceed" };
  }
}

/** One attempt, persisted status first. Unknown reads never authorize a new submission. */
export async function checkCode(options: {
  client: Pick<WorkerClient, "codeCrossCheckStatus" | "submitCodeCrossCheck">;
  runId: string; generation: number; signal: AbortSignal;
  snapshot(): Promise<{ head_commit: string; base_commit: string } | { reason_class: "snapshot_failed" | "worker_unsupported" } | undefined>;
  pollMs?: number; requestMs?: number;
}): Promise<CodeCrossCheckStatus | undefined> {
  const { client, runId, generation, signal } = options;
  const request = <T>(fn: (signal: AbortSignal) => Promise<T>, remaining = 3000) =>
    fn(AbortSignal.any([signal, AbortSignal.timeout(Math.max(1, Math.min(options.requestMs ?? 3000, remaining)))]));
  try {
    signal.throwIfAborted();
    let current = await request((owned) => client.codeCrossCheckStatus(runId, generation, owned));
    if ("result" in current) {
      const snapshot = await options.snapshot();
      signal.throwIfAborted();
      if (snapshot === undefined) return undefined;
      // One submit only: an ambiguous POST is never retried. A later gate reads persisted evidence.
      current = await request((owned) => client.submitCodeCrossCheck(runId, generation, snapshot, owned));
    }
    if ("result" in current) return undefined;
    if (current.candidate_generation !== generation || current.interrupted_at !== null) return maskSupersededAdvice(current);
    const identity = JSON.stringify([current.head_commit, current.base_commit, current.candidate_digest, current.deadline_at]);
    // Server deadline includes queue time; two hours also bounds a malformed far-future deadline.
    const deadline = Math.min(Date.parse(current.deadline_at), Date.now() + 2 * 60 * 60_000);
    while (current.outcome === "pending") {
      const remaining = deadline - Date.now();
      if (remaining > 0) await delay(Math.min(options.pollMs ?? 15000, remaining), undefined, { signal });
      // The deadline GET lets the authoritative server settle its timeout. Give that
      // final read its own short request budget; never submit again or veto publication.
      const atDeadline = Date.now() >= deadline;
      const next = await request((owned) => client.codeCrossCheckStatus(runId, generation, owned),
        atDeadline ? options.requestMs ?? 3000 : deadline - Date.now());
      if ("result" in next) return undefined;
      if (next.candidate_generation !== generation || next.interrupted_at !== null) return maskSupersededAdvice(next);
      if (identity !== JSON.stringify([next.head_commit, next.base_commit, next.candidate_digest, next.deadline_at])) return undefined;
      current = next;
      if (atDeadline) return current;
    }
    return current;
  } catch {
    signal.throwIfAborted();
    return undefined;
  }
}
