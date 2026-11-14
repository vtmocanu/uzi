/**
 * Compile-time production-dark gate for #4007 slice B (worker terminal-preservation producer).
 *
 * Not configurable: no config, environment variable, chart value, server feature or agent input
 * reaches it. Enabling is a separate reviewed change that follows T4, the owner escape (D) and a
 * one-worker activation. Until then every entry point added by later milestones begins with the
 * `terminalPreservationEnabled()` check on RunRunner / RecoveryCoordinator and does nothing.
 */
export const TERMINAL_PRESERVATION_ENABLED: boolean = false;

/**
 * Constructor-only, test-only seams. Never passed by main.ts or worker.ts; its mere presence turns
 * the gate on for that one instance. Later milestones add fake seams here.
 */
export interface TerminalPreservationTestHooks {
  label?: string;
}

/** The single gate decision: the compile-time constant, or a test override injected at construction. */
export function terminalPreservationActive(override: TerminalPreservationTestHooks | undefined): boolean {
  return TERMINAL_PRESERVATION_ENABLED || override !== undefined;
}
