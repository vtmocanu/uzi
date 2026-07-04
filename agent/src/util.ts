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
