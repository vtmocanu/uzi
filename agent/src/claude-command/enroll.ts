// Memory-guard enrollment for a guarded Claude model command (issue #2471, M2). Mirrors
// `enrollMemoryCommand` in codex/memory-command.ts over the Claude command root: the supervisor
// is the enrolled root and the bash wrapper (`started.childPid`) is the primary.
//
// Authority. Only metadata minted by the command lane (`mintClaudeCommandMetadata`) can enroll,
// and only into the captured context it was minted for ({commands, context, flight,
// lifecycleRevision}), exactly like `isBrokerModelCommand`. A forged, cloned or unminted object
// is never enrolled; the command it describes still runs, bounded by its own deadline.
//
// Disposal. `disposeRootOnce` is the single-flight whole-tree disposal of one supervisor
// handle. The lane's own D7 sequence and the guard's cancel/drain call it, so a root is disposed
// by one in-flight operation however many parties ask. Only an in-flight operation or a confirmed
// success is shared: an unconfirmed disposal is dropped so a later caller (the guard's drain
// retry, the lane's own dispose) may retry it, matching the launcher's `dispose` contract.

import { setTimeout as delay } from "node:timers/promises";

import type { CapturedMemoryContext } from "../codex/broker.js";
import { boundedCommandWait } from "../codex/memory-command.js";
import type { CodexRootHandle } from "../codex/launcher.js";
import type { WorkerMemoryCommands } from "../worker-memory-commands.js";

type Entry = ReturnType<WorkerMemoryCommands["register"]>;
type Observation = Awaited<ReturnType<Entry["observe"]>>;

export interface ClaudeCommandMetadata {
  /** The handler's tool_use id (verified against a PreToolUse record by the lane). */
  readonly toolId: string;
  readonly attemptId: string;
  readonly commandId: string;
  /** The model command text; the registry redacts and bounds it. */
  readonly command: string;
}

/** The message of the error `enrollClaudeCommand` throws when the captured admission closed. */
export const ADMISSION_CLOSED_MESSAGE = "captured memory admission closed";

const minted = new WeakMap<ClaudeCommandMetadata, CapturedMemoryContext>();

/** Mint enrollment metadata bound to the captured context. Called only by the command lane. */
export function mintClaudeCommandMetadata(captured: CapturedMemoryContext, fields: ClaudeCommandMetadata): ClaudeCommandMetadata {
  const metadata = Object.freeze({
    toolId: fields.toolId, attemptId: fields.attemptId, commandId: fields.commandId, command: fields.command,
  });
  minted.set(metadata, Object.freeze({ ...captured }));
  return metadata;
}

/** Identity membership: true only for the exact object `mintClaudeCommandMetadata` returned,
 *  and, when `captured` is given, only for the context it was minted for. */
export function isMintedClaudeCommand(value: ClaudeCommandMetadata | undefined, captured?: CapturedMemoryContext): value is ClaudeCommandMetadata {
  if (value === undefined || typeof value !== "object" || value === null) return false;
  const bound = minted.get(value);
  if (bound === undefined) return false;
  if (!captured) return true;
  return bound.commands === captured.commands && bound.context === captured.context
    && bound.flight === captured.flight && bound.lifecycleRevision === captured.lifecycleRevision;
}

const disposals = new WeakMap<CodexRootHandle, Promise<boolean>>();

/**
 * Dispose one supervisor root and share the result while it is in flight or once confirmed.
 * Resolves true only for a clean, drained disposal; a rejection or an unclean outcome is false
 * and is NOT cached, so a later caller starts a fresh disposal. The starting caller's
 * `timeoutMs` bounds the underlying disposal; callers joining it share that budget.
 */
export function disposeRootOnce(handle: CodexRootHandle, timeoutMs: number): Promise<boolean> {
  const existing = disposals.get(handle);
  if (existing) return existing;
  const flight: Promise<boolean> = handle.dispose(timeoutMs).then((outcome) => outcome.clean, () => false).then((ok) => {
    if (!ok && disposals.get(handle) === flight) disposals.delete(handle);
    return ok;
  });
  disposals.set(handle, flight);
  return flight;
}

/** The bounded wait on `observed`, cut short (undefined) the moment `signal` aborts. The losing
 *  side is never left as an unhandled rejection. */
async function observeUnlessAborted<T>(observed: Promise<T>, timeoutMs: number, signal?: AbortSignal): Promise<T | undefined> {
  const bounded = boundedCommandWait(observed, timeoutMs);
  if (!signal) return bounded;
  if (signal.aborted) { bounded.catch(() => undefined); return undefined; }
  let onAbort: (() => void) | undefined;
  const aborted = new Promise<undefined>((resolve) => {
    onAbort = () => resolve(undefined);
    signal.addEventListener("abort", onAbort, { once: true });
  });
  try {
    return await Promise.race([bounded, aborted]);
  } finally {
    if (onAbort) signal.removeEventListener("abort", onAbort);
    bounded.catch(() => undefined);
  }
}

/** Pause between initial observations that raced the wrapper's own startup. */
const STALE_RETRY_MS = 20;
/** Upper bound of the re-sampling window; a shorter deadline shrinks it to a quarter of itself. */
const STALE_WINDOW_MS = 1000;

/**
 * The initial supervisor observation. The launcher reports `unavailable`/`stale` when a process
 * in the tree appears, exits or changes parent between the supervisor's two passes, and also for
 * a malformed observe header (`validObservation`) or a supervisor/root start-time change. While
 * the wrapper is still forking (process substitution, `tee`) that is a transient condition, so
 * the sample is repeated. The repeating is bounded: it stops once min(STALE_WINDOW_MS,
 * deadlineMs / 4) has elapsed and returns the last stale sample (the command then runs
 * unenrolled), so a tree that never settles (make -j, a test runner) cannot hold enrollment up.
 * Each observe has its own budget of deadlineMs / 2, so the last one, started inside the window,
 * settles by 3/4 of the deadline, before the outer `deadlineMs` bound in `enrollClaudeCommand`.
 * Every other outcome is returned as is. `stop` aborts the loop and its pause timer.
 */
async function initialObservation(handle: CodexRootHandle, deadlineMs: number, stop: AbortSignal) {
  const windowEnd = performance.now() + Math.min(STALE_WINDOW_MS, deadlineMs / 4);
  const observeBudgetMs = Math.max(1, deadlineMs / 2);
  for (;;) {
    const sample = await handle.observe(observeBudgetMs);
    const transient = sample.state === "unavailable" && sample.reason === "stale";
    if (!transient || stop.aborted || performance.now() + STALE_RETRY_MS >= windowEnd) return sample;
    try { await delay(STALE_RETRY_MS, undefined, { signal: stop }); } catch { return sample; }
  }
}

export interface ClaudeCommandEnrollment {
  readonly entry: Entry;
  /** Resolves when the guard resolves a pressure outcome for this entry (watch only). */
  readonly outcome: ReturnType<WorkerMemoryCommands["watchOutcome"]>;
  /** Single-flight disposal; records a confirmed drain and unregisters on success. */
  readonly reap: (budgetMs?: number) => Promise<{ ok: boolean }>;
  /** The guard's prepared outcome for this entry, when it prepared one. */
  readonly preparedOutcome: () => ReturnType<WorkerMemoryCommands["outcome"]>;
  /** True once the guard's own cancel or drain passed its checks and started a disposal. */
  readonly guardStopped: () => boolean;
  readonly recordDisposal: (confirmed: boolean) => void;
  readonly unregister: () => void;
}

/**
 * Enroll a lane-minted command into the captured memory context. Returns undefined (the command
 * runs unenrolled) for unminted metadata, an uncertain initial observation, or a primary that
 * already exited with nothing adopted. Throws when the captured admission closed while enrolling.
 */
export async function enrollClaudeCommand(
  captured: CapturedMemoryContext | undefined,
  metadata: ClaudeCommandMetadata | undefined,
  handle: CodexRootHandle,
  deadlineMs: number,
  signal?: AbortSignal,
): Promise<ClaudeCommandEnrollment | undefined> {
  if (!captured || !isMintedClaudeCommand(metadata, captured)) return undefined;
  const { commands, context, flight, lifecycleRevision } = captured;
  const current = () => commands.isCurrent(context, flight, lifecycleRevision);
  if (!current() || signal?.aborted || !commands.admitsCommands(context)) throw new Error(ADMISSION_CLOSED_MESSAGE);
  const startedAtMs = performance.now();
  // The initial observe honours the abort: an aborted command goes straight to disposal instead of
  // waiting out the observe budget (the disposal join budget in lane.ts counts on this).
  const stopInitial = new AbortController();
  let initial: Awaited<ReturnType<typeof initialObservation>> | undefined;
  try { initial = await observeUnlessAborted(initialObservation(handle, deadlineMs, stopInitial.signal), deadlineMs, signal); }
  finally { stopInitial.abort(); }
  if (!initial || !current() || signal?.aborted || !commands.admitsCommands(context)) throw new Error(ADMISSION_CLOSED_MESSAGE);
  // Read-only uncertainty prevents memory intervention, not ordinary execution.
  if (initial.state !== "complete") return undefined;
  if (initial.supervisor.pid !== handle.supervisorPid
    || initial.supervisor.pid !== handle.started.supervisorPid || initial.root.pid !== handle.started.childPid) {
    throw new Error("memory enrollment evidence unavailable");
  }
  const supervisor = Object.freeze({ ...initial.supervisor });
  const primary = Object.freeze({ ...initial.root });
  const same = (a: typeof supervisor, b: typeof supervisor) => a.pid === b.pid && a.startTime === b.startTime;
  const convert = (evidence: Awaited<ReturnType<CodexRootHandle["observe"]>>, startedAt: number): Observation => {
    const completedAtMs = performance.now();
    if (evidence.state !== "complete" || !same(evidence.supervisor, supervisor) || !same(evidence.root, primary)) {
      return { available: false, complete: false, startedAtMs: startedAt, completedAtMs, rows: [] };
    }
    return { available: true, complete: true, startedAtMs: startedAt, completedAtMs,
      supervisor, primary, rows: evidence.processes.map(({ pid, startTime, rss }) => ({ pid, startTime, rssBytes: rss })) };
  };
  // A primary that has already exited with no adopted children needs no enrollment.
  if (convert(initial, startedAtMs).rows.length === 0) return undefined;
  let entry!: Entry;
  let guardStopped = false;
  const reap = async (budgetMs = deadlineMs): Promise<{ ok: boolean }> => {
    const budget = Math.min(deadlineMs, budgetMs);
    const completion = disposeRootOnce(handle, budget).then((ok) => {
      if (ok) {
        commands.recordDrain(entry, { confirmed: true });
        commands.unregister(entry);
      }
      return { ok };
    });
    try { return await boundedCommandWait(completion, budget); }
    catch { return { ok: false }; }
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
    guardStopped = true;
    return reap(end - performance.now());
  };
  entry = commands.register(context, {
    source: "claude-model-command", toolId: metadata.toolId, attemptId: metadata.attemptId,
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
  return {
    entry,
    outcome: commands.watchOutcome(entry),
    reap,
    preparedOutcome: () => commands.outcome(entry),
    guardStopped: () => guardStopped,
    recordDisposal: (confirmed: boolean) => {
      commands.recordDrain(entry, { confirmed, detail: confirmed ? undefined : "supervisor disposal unconfirmed" });
    },
    unregister: () => { commands.unregister(entry); },
  };
}
