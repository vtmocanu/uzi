import { setTimeout as delay } from "node:timers/promises";

/** Sleep that resolves early (not rejects) when the signal aborts. */
export async function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  try {
    await delay(ms, undefined, { signal });
  } catch {
    // AbortError — treat a cancelled sleep as "woke up early".
  }
}

/** Best-effort human-readable message for an unknown thrown value. */
export function errMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

/**
 * Server-issued run ids are UUIDs. Validate before a runId ever becomes a
 * filesystem path segment (the per-run HOME `agent-home/<runId>`, the per-run
 * provisioning dir) so a malformed id can't collapse a shared dir (an empty id
 * would resolve the per-run HOME back to the shared root) or traverse out (a
 * separator/`..`). Single source of truth for the shape, shared by runner.ts and
 * provision-run.ts.
 */
export const RUN_ID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
