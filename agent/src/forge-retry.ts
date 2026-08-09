// Layer A of PRD #284: a bounded, fast retry with a transient-vs-permanent
// classifier for the worker's final push + MR/PR-create hop. A transient forge
// error (a dropped HTTP/2 stream, a 5xx, a connection reset) fails the whole run
// today, discarding the agent's already-committed work (observed on issue #216,
// run 8fc2fa47). This wraps those two calls in a backoff loop that retries ONLY
// transient errors and fails fast on permanent ones.
//
// The one non-negotiable (D2/D9): a PERMANENT forge rejection — auth failure, a
// protected-branch guardrail, a non-fast-forward `[rejected]` — is NEVER retried.
// Retrying a guardrail rejection would weaken it. So the classifier checks the
// permanent patterns FIRST and they win, and an unmatched error defaults to
// permanent (fail fast — the safe default).

import { ForgeError } from "./forge.js";

/**
 * The push/MR-create backoff schedule (~30s total). N sleeps ⇒ N+1 attempts.
 *
 * This is a DELIBERATE second implementation of the worker→API terminal-callback
 * decision (`client.ts:443` `isTransient` + `client.ts:50`
 * `DEFAULT_TERMINAL_RETRY_SCHEDULE`), for the worker→FORGE hop that shape never
 * covered. The two are kept pinned to the same values via the M6 differential
 * test; if you change one, change the other (or the test will flag the drift).
 */
export const FORGE_RETRY_SCHEDULE = [1_000, 2_000, 4_000, 8_000, 16_000];

/**
 * PERMANENT stderr/message patterns, checked FIRST — a hit means "fail fast, never
 * retry" (D9). Case-insensitive. Note git's generic `Could not read from remote
 * repository` trailer is deliberately absent: it also trails an auth/permission
 * denial, so on its own it must NOT be classified transient.
 */
const PERMANENT_PATTERNS: RegExp[] = [
  /authentication failed/i,
  /could not read Username/i,
  /terminal prompts disabled/i,
  /permission .* denied/i,
  /permission denied/i,
  /access denied/i,
  /protected branch/i,
  /pre-receive hook declined/i,
  /remote rejected/i,
  /\[rejected\]/i,
  /non-fast-forward/i,
  /fetch first/i,
  /\b40[134]\b/, // 401 / 403 / 404
];

/**
 * TRANSIENT stderr/message patterns, checked only AFTER the permanent set misses —
 * a hit means "retry within the schedule". Case-insensitive.
 */
const TRANSIENT_PATTERNS: RegExp[] = [
  /stream .* reset/i,
  /INTERNAL_ERROR/i,
  /connection reset/i,
  /could not resolve host/i,
  /couldn'?t connect/i,
  /\b50[023]\b/, // 500 / 502 / 503
  /bad gateway/i,
  /gateway time-?out/i,
  /\btimed? ?out\b/i,
  /TLS/i,
  /EOF/i,
];

/**
 * Classify a forge/git error as "transient" (retry) or "permanent" (fail fast).
 *
 * Precedence is load-bearing (D9): permanent-first.
 *   - A `ForgeError` is classified by HTTP status: 0 (transport failure) ⇒
 *     transient; >=500 || 408 || 429 ⇒ transient; any other status ⇒ permanent.
 *     This mirrors `isTransient` (`client.ts:443`).
 *   - Otherwise, match the error's message string against the PERMANENT patterns
 *     first (they win), then the TRANSIENT patterns.
 *   - No match ⇒ permanent (the safe default: fail fast).
 */
export function classifyForgeError(err: unknown): "transient" | "permanent" {
  if (err instanceof ForgeError) {
    if (err.status === 0) return "transient";
    if (err.status >= 500 || err.status === 408 || err.status === 429) return "transient";
    return "permanent";
  }
  const msg = err instanceof Error ? err.message : String(err);
  for (const re of PERMANENT_PATTERNS) {
    if (re.test(msg)) return "permanent";
  }
  for (const re of TRANSIENT_PATTERNS) {
    if (re.test(msg)) return "transient";
  }
  return "permanent";
}

const sleepReal = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

export interface WithForgeRetryOptions {
  /** Classifier under test-injection; defaults to {@link classifyForgeError}. */
  classify?: (e: unknown) => "transient" | "permanent";
  /** Backoff delays; defaults to {@link FORGE_RETRY_SCHEDULE}. N sleeps ⇒ N+1 attempts. */
  schedule?: number[];
  /** Injectable sleep (tests record delays and resolve immediately). */
  sleep?: (ms: number) => Promise<void>;
  /** Optional logger; a warn is emitted before each retry sleep. */
  log?: { warn: (msg: string, meta?: Record<string, unknown>) => void };
}

/**
 * Run `fn`, retrying it on a TRANSIENT error using the backoff schedule. A
 * permanent error (or an exhausted schedule) rethrows the last error unchanged, so
 * the caller's existing catch still fires and the run fails as it does today.
 *
 * N schedule entries ⇒ up to N+1 attempts (1 initial + N retries).
 */
export async function withForgeRetry<T>(
  fn: () => Promise<T>,
  opts: WithForgeRetryOptions = {},
): Promise<T> {
  const classify = opts.classify ?? classifyForgeError;
  const schedule = opts.schedule ?? FORGE_RETRY_SCHEDULE;
  const sleep = opts.sleep ?? sleepReal;

  for (let attempt = 0; ; attempt++) {
    try {
      return await fn();
    } catch (err) {
      if (classify(err) === "transient" && attempt < schedule.length) {
        const delay = schedule[attempt]!;
        opts.log?.warn("transient forge error; retrying push/MR-create", {
          attempt: attempt + 1,
          delay_ms: delay,
          error: err instanceof Error ? err.message : String(err),
        });
        await sleep(delay);
        continue;
      }
      throw err;
    }
  }
}
