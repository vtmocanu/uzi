/**
 * Compile-time production-dark gate for #4007 slice B (worker terminal-preservation producer).
 *
 * Not configurable: no config, environment variable, chart value, server feature or agent input
 * reaches it. Enabling is a separate reviewed change that follows T4, the owner escape (D) and a
 * one-worker activation. Until then every entry point added by later milestones begins with the
 * `terminalPreservationEnabled()` check on RunRunner / RecoveryCoordinator and does nothing.
 */
import type { TerminalOperationDeps } from "./terminal-operation.js";

export const TERMINAL_PRESERVATION_ENABLED: boolean = false;

/**
 * Constructor-only, test-only seams. Never passed by main.ts or worker.ts; its mere presence turns
 * the gate on for that one instance. Later milestones add fake seams here.
 */
export interface TerminalPreservationTestHooks {
  label?: string;
  /** Worker id used when the client has no registered incarnation. */
  workerId?: string;
  /** Attempt id used when the flight has none (a worker without attempt paths). */
  attemptId?: string;
  /** Lane sub-deadline cap and permit reserve (ms); the lane's own defaults otherwise. */
  capMs?: number;
  reserveMs?: number;
  /** Replacement engine effects (capture, prepare, send, store, ...) for one instance. */
  deps?: Partial<Pick<TerminalOperationDeps, "store" | "owner" | "universe" | "capture" | "aggregate" | "prepare" | "coverage" |
    "recheck" | "send">>;
  store?: TerminalOperationDeps["store"];
  owner?: TerminalOperationDeps["owner"];
}

/** The single gate decision: the compile-time constant, or a test override injected at construction. */
export function terminalPreservationActive(override: TerminalPreservationTestHooks | undefined): boolean {
  return TERMINAL_PRESERVATION_ENABLED || override !== undefined;
}
