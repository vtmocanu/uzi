import type { CapturedMemoryContext, SpawnCommandOptions } from "./broker.js";
import { setTimeout as delay } from "node:timers/promises";
import { bindMemoryCallback, isBrokerModelCommand } from "./broker.js";
import type { CodexRootHandle, ObservationOutcome } from "./launcher.js";
import type { ExecutionRegistry, RegisteredRoot, ReapOutcome } from "./registry.js";
import type { WorkerMemoryCommands } from "../worker-memory-commands.js";

type Entry = ReturnType<WorkerMemoryCommands["register"]>;
type Observation = Awaited<ReturnType<Entry["observe"]>>;

/** One foreground wait, bounded independently of a utility's promise behaviour.
 * A timeout rejects this caller; it does not attest that the utility has stopped.
 */
export async function boundedCommandWait<T>(promise: Promise<T>, timeoutMs: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([promise, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error("command cleanup/outcome deadline")), Math.max(1, timeoutMs));
    })]);
  } finally { if (timer) clearTimeout(timer); }
}

const STALE_RETRY_MS = 20;
const STALE_WINDOW_MS = 1000;

/** Mirror the Claude adapter's initial-observation policy without changing either adapter's
 * enrollment contract. Only stale samples can start another read, within min(1 s, deadline/4).
 * Each read gets deadline/2, so the last in-flight read can finish after the retry-start window
 * but before the outer deadline. Abort ends this wait and prevents further retries. */
async function initialObservation(handle: CodexRootHandle, deadlineMs: number, signal?: AbortSignal) {
  if (signal?.aborted) return undefined;
  const windowEnd = performance.now() + Math.min(STALE_WINDOW_MS, deadlineMs / 4);
  const observeBudgetMs = Math.max(1, deadlineMs / 2);
  const stop = new AbortController();
  let onAbort: (() => void) | undefined;
  const aborted = new Promise<undefined>(resolve => {
    onAbort = () => resolve(undefined);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
  const observe = async (): Promise<ObservationOutcome | undefined> => {
    for (;;) {
      if (signal?.aborted || stop.signal.aborted) return undefined;
      const sample = await handle.observe(observeBudgetMs);
      if (sample.state !== "unavailable" || sample.reason !== "stale" || stop.signal.aborted ||
          signal?.aborted || performance.now() + STALE_RETRY_MS >= windowEnd) return sample;
      try { await delay(STALE_RETRY_MS, undefined, { signal: stop.signal }); }
      catch { return sample; }
    }
  };
  try {
    return await boundedCommandWait(Promise.race([observe(), aborted]), deadlineMs);
  } finally {
    stop.abort();
    if (onAbort) signal?.removeEventListener("abort", onAbort);
  }
}

/** Adapt only broker-minted model calls to the already registered supervisor.
 * Internal probes and other utility spawns never receive enrollment authority.
 */
export async function enrollMemoryCommand(
  captured: CapturedMemoryContext | undefined,
  metadata: SpawnCommandOptions["modelCommand"],
  registry: ExecutionRegistry,
  root: RegisteredRoot,
  handle: CodexRootHandle,
  deadlineMs: number,
  signal?: AbortSignal,
) {
  if (!captured || !isBrokerModelCommand(metadata, captured)) return undefined;
  const { commands, context, flight, lifecycleRevision } = captured;
  const current = () => commands.isCurrent(context, flight, lifecycleRevision);
  if (!current() || signal?.aborted || !commands.admitsCommands(context)) throw new Error("captured memory admission closed");
  const startedAtMs = performance.now();
  const initial = await initialObservation(handle, deadlineMs, signal);
  if (!initial || !current() || signal?.aborted || !commands.admitsCommands(context)) throw new Error("captured memory admission closed");
  // Read-only uncertainty prevents memory intervention, not ordinary execution.
  // The registered root still owns exit, abort, deadline and disposal handling.
  if (initial.state !== "complete") return undefined;
  if (initial.supervisor.pid !== handle.supervisorPid
    || initial.supervisor.pid !== handle.started.supervisorPid || initial.root.pid !== handle.started.childPid) throw new Error("memory enrollment evidence unavailable");
  const supervisor = Object.freeze({ ...initial.supervisor });
  const primary = Object.freeze({ ...initial.root });
  const same = (a: typeof supervisor, b: typeof supervisor) => a.pid === b.pid && a.startTime === b.startTime;
  const convert = (evidence: Awaited<ReturnType<CodexRootHandle["observe"]>>, startedAtMs: number): Observation => {
    const completedAtMs = performance.now();
    if (evidence.state !== "complete" || !same(evidence.supervisor, supervisor) || !same(evidence.root, primary)) {
      return { available: false, complete: false, startedAtMs, completedAtMs, rows: [] };
    }
    return { available: true, complete: true, startedAtMs, completedAtMs,
      supervisor, primary, rows: evidence.processes.map(({ pid, startTime, rss }) => ({ pid, startTime, rssBytes: rss })) };
  };
  // A primary that has already exited with no adopted children needs no enrollment.
  if (convert(initial, startedAtMs).rows.length === 0) return undefined;
  let entry!: Entry;
  const reap = async (budgetMs = deadlineMs): Promise<ReapOutcome> => {
    const budget = Math.min(deadlineMs, budgetMs);
    // Reconcile on the already owned reap promise, even when this foreground wait expires.
    const completion = registry.reapRoot(root, budget).then((result) => {
      if (result.ok) {
        commands.recordDrain(entry, { confirmed: true });
        commands.unregister(entry);
      }
      return result;
    });
    try { return await boundedCommandWait(completion, budget); }
    catch (error) {
      registry.poison({ category: "protocol", message: "command cleanup completion unconfirmed" });
      throw error;
    }
  };
  const authorizedReap = async (authority: Parameters<Entry["cancel"]>[0]) => {
    const end = performance.now() + authority.remainingMs;
    if (!current() || authority.signal.aborted || !authority.authorized()) throw new Error("stale memory cancellation");
    // Refresh exact supervisor and original primary attestation before disposal.
    const observation = await entry.observe(authority);
    const selection = commands.select([{ entry, observation }], performance.now(), authority.remainingMs);
    if (!selection || !current() || authority.signal.aborted || !authority.authorized()
      || performance.now() >= end || !commands.checkBeforeCancel(selection, observation, performance.now(), authority.remainingMs)) {
      throw new Error("stale memory cancellation");
    }
    return reap(end - performance.now());
  };
  entry = commands.register(context, {
    source: "codex-model-command", toolId: metadata.toolId, attemptId: metadata.attemptId,
    command: metadata.command, root: supervisor, primary,
    observe: async (budget) => {
      if (!current() || budget?.signal.aborted) return { available: false, complete: false, startedAtMs: performance.now(), completedAtMs: performance.now(), rows: [] };
      const started = performance.now();
      const evidence = await boundedCommandWait(handle.observe(budget?.remainingMs ?? deadlineMs), budget?.remainingMs ?? deadlineMs);
      if (!current() || budget?.signal.aborted) return { available: false, complete: false, startedAtMs: started, completedAtMs: performance.now(), rows: [] };
      return convert(evidence, started);
    },
    cancel: async (authority) => {
      const result = await authorizedReap(authority);
      commands.recordDrain(entry, { confirmed: result.ok, detail: result.ok ? undefined : "supervisor disposal unconfirmed" });
    },
    drain: async (budget) => {
      if (!budget || budget.signal.aborted || !budget.authorized() || !current()) return { confirmed: false, detail: "stale memory drain" };
      const result = await authorizedReap(budget);
      return { confirmed: result.ok, detail: result.ok ? undefined : "supervisor disposal unconfirmed" };
    },
  }, metadata.commandId);
  bindMemoryCallback(metadata, entry);
  const outcome = commands.watchOutcome(entry);
  return {
    entry, outcome, reap,
    preparedOutcome: () => commands.outcome(entry),
    recordDisposal: (confirmed: boolean) => {
      commands.recordDrain(entry, { confirmed, detail: confirmed ? undefined : "supervisor disposal unconfirmed" });
    },
    unregister: () => commands.unregister(entry),
  };
}
