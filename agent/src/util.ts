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

/**
 * Clamp a directory name discovered in a CLONE to a safe rendering, bounded to `max`.
 *
 * SHARED ON PURPOSE, and the sharing is the point: `dir` is `readdir` output on an
 * untrusted repo, and it reaches a user (the run feed) and a model (the implement
 * prompt). The two call sites differ ONLY in their bound; the CHARSET is the
 * security-relevant half and must not drift between them. The plausible bad edit is
 * someone widening the feed's charset for legibility — defensible there in isolation,
 * and dangerous the moment anyone assumes the two behave the same. A single primitive
 * forces that conversation; two copies let it happen silently.
 *
 * It removes STRUCTURE (quotes, newlines, tags, anything that could close a fence or
 * start a new line) and bounds VOLUME. It does NOT make the result meaningless: the
 * surviving characters — letters, digits, `.`, `-`, `_`, `/`, `@` — are enough to spell
 * prose, so a caller placing the result in a prompt still needs a fence around it. See
 * `depsProvisionImplementNote` in prompt.ts, which does both.
 */
export function clampToDirCharset(dir: string, max: number): string {
  const cleaned = dir.replace(/[^A-Za-z0-9._/@-]/g, "?");
  return cleaned.length > max ? `${cleaned.slice(0, max)}…` : cleaned;
}
