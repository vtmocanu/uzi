import { setTimeout as delay } from "node:timers/promises";
import type { WorkerClient } from "./client.js";
import type { CodeCrossCheckStatus } from "./code-cross-check-contract.js";

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
    if (current.candidate_generation !== generation || current.interrupted_at !== null) return { ...current, findings: [] };
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
      if (next.candidate_generation !== generation || next.interrupted_at !== null) return { ...next, findings: [] };
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
