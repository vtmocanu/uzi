import assert from "node:assert/strict";
import { existsSync, rmSync } from "node:fs";
import type { CodexRootHandle, DisposeOutcome } from "../../agent/src/codex/launcher.js";

type Step = "transport" | "dispose" | "provider" | "remove";
export interface CleanupResult {
  readonly step: Step;
  readonly completed: boolean;
  readonly error?: unknown;
}
export interface PCleanupResources {
  transport?: { close(): void | Promise<void> };
  handle?: Pick<CodexRootHandle, "dispose" | "started" | "supervisorPid">;
  provider?: { close(): void | Promise<void> };
  base?: string;
  /** Test-only seam for verifying removal failures without leaking a directory. */
  remove?: (base: string) => void | Promise<void>;
}

export class PCleanupError extends AggregateError {
  constructor(
    readonly results: readonly CleanupResult[],
    readonly base: string | undefined,
    readonly baseAbsent: boolean,
    readonly disposalError: unknown,
    readonly primaryFailure: { error: unknown } | undefined,
  ) {
    const failures = results.filter(result => !result.completed).map(result => result.error);
    // Put disposal first among teardown errors, even if transport close failed earlier.
    const ordered = results.some(result => result.step === "dispose" && !result.completed)
      ? [disposalError, ...results.filter(result => result.step !== "dispose" && !result.completed).map(result => result.error)]
      : failures;
    super(primaryFailure ? [primaryFailure.error, ...ordered] : ordered,
      "P cleanup failed: " + results.filter(result => !result.completed)
        .map(result => result.step + ": " + String(result.error)).join("; "),
      { cause: primaryFailure ? primaryFailure.error : ordered[0] });
  }
}

/**
 * One sequential attempt per acquired resource; a failure never skips sibling steps.
 * Disposal uses the launcher's existing deadline. No retries or extra timers.
 * Both the after-hook and finally callers await this before they can succeed.
 */
export async function cleanupP(
  resources: PCleanupResources,
  primaryFailure?: { error: unknown },
  diagnostic?: (message: string) => void,
): Promise<void> {
  const results: CleanupResult[] = [];
  let disposalError: unknown;
  const attempt = async (step: Step, operation: () => void | Promise<void>): Promise<void> => {
    try {
      await operation();
      results.push({ step, completed: true });
    } catch (error) {
      if (step === "dispose") disposalError = error;
      results.push({ step, completed: false, error });
    }
  };
  if (resources.transport) await attempt("transport", () => resources.transport!.close());
  if (resources.handle) await attempt("dispose", async () => {
    const outcome: DisposeOutcome = await resources.handle!.dispose();
    assert.ok(outcome.clean, outcome.clean ? "confirmed clean disposal" : outcome.reason);
    // The P shims spawn the app-server directly: this is the process-bound PID.
    const ownedPid = resources.handle!.supervisorPid;
    assert.ok(ownedPid !== undefined && Number.isSafeInteger(ownedPid) && ownedPid > 0,
      "P cleanup requires a positive safe process PID");
    assert.equal(resources.handle!.started.childPid, ownedPid,
      "started child PID matches the spawned process");
    assert.deepEqual(outcome.event.killed, [ownedPid], "dispose killed the exact owned live child");
    assert.deepEqual(outcome.event.reaped, [ownedPid], "dispose observed the exact owned child exit");
  });
  if (resources.provider) await attempt("provider", () => resources.provider!.close());
  if (resources.base) await attempt("remove", async () => {
    if (resources.remove) await resources.remove(resources.base!);
    else rmSync(resources.base!, { recursive: true, force: true });
    assert.equal(existsSync(resources.base!), false, "P temporary directory was removed");
  });
  if (results.some(result => !result.completed)) {
    const error = new PCleanupError(results, resources.base,
      resources.base === undefined || !existsSync(resources.base), disposalError, primaryFailure);
    if (primaryFailure && diagnostic) {
      // node:test can omit an after-hook rejection when the test body already failed.
      // Report only after every cleanup attempt; reporting must never replace the error.
      try {
        diagnostic(`${error.message}; cleanup results: ${results.map(result =>
          `${result.step}=${result.completed ? "completed" : "failed"}`).join(", ")}; ` +
          `base=${String(error.base)}; baseAbsent=${error.baseAbsent}`);
      } catch { /* Preserve the primary and teardown failures if reporting faults. */ }
    }
    throw error;
  }
}
