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
 * The devbox `devbox install` backoff schedule. 3 retries ⇒ 4 attempts.
 *
 * This is a SEPARATE constant, deliberately NOT a reuse of
 * {@link FORGE_RETRY_SCHEDULE}, for two reasons:
 *
 *   1. `FORGE_RETRY_SCHEDULE` is pinned by a differential test to
 *      `DEFAULT_TERMINAL_RETRY_SCHEDULE` (`client.ts`). That pin encodes a
 *      SEMANTIC link to the worker→API terminal-callback hop, which the devbox
 *      retry does not share; borrowing that constant would couple two unrelated
 *      decisions and make either one un-tunable without dragging the other.
 *
 *   2. Per-attempt wall-clock is already bounded by the 10-min `execFileAsync`
 *      timeout in provision.ts: any attempt that hangs to that limit becomes a
 *      SIGTERM kill, which {@link classifyDevboxError} classifies PERMANENT and so
 *      stops immediately (never retried). This schedule therefore governs only
 *      fast-failing transient errors, and the total retry wall-clock stays far
 *      under RUN_TIMEOUT (2h).
 */
export const DEVBOX_RETRY_SCHEDULE = [1_000, 4_000, 10_000];

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

/**
 * TRANSIENT patterns for a devbox/nix/curl `devbox install` failure. These are
 * deliberately NETWORK-SCOPED PHRASES, not bare tokens (mirroring ADR-0284's
 * `Could not read from remote repository` note): a bare `ssl`/`tls` token or a
 * bare `50[023]` number would mis-read a deterministic error as transient — a
 * package name like `openssl` (`package 'openssl-connector' has an unfree
 * license`) or a nix eval file:line ref like `default.nix:503:12` must NOT be
 * read transient. So the SSL/HTTP-5xx entries are anchored to their network
 * context (`SSL handshake`, `returned error: 503`, `HTTP error 503`, `service
 * unavailable`) rather than the bare token. Each entry is scoped so only a
 * genuine network/timeout condition hits. Case-insensitive.
 */
const DEVBOX_TRANSIENT_PATTERNS: RegExp[] = [
  /curl:\s*\(28\)/i, // operation timed out
  /curl:\s*\(6\)/i, // could not resolve host
  /curl:\s*\(7\)/i, // failed to connect
  /curl:\s*\(35\)/i, // TLS handshake
  /curl:\s*\(56\)/i, // recv failure
  /timeout was reached/i,
  /unable to download/i,
  /could not resolve host/i,
  /temporary failure in name resolution/i,
  /connection reset/i,
  /connection refused/i,
  /connection timed out/i,
  /network is unreachable/i,
  /TLS handshake/i,
  /SSL handshake/i,
  /SSL connect error/i,
  /SSL_ERROR_SYSCALL/i,
  /returned error:\s*50[023]\b/i, // curl: (22) The requested URL returned error: 503
  /HTTP(?:\/[\d.]+)?\s*(?:error\s*)?50[023]\b/i, // nix "HTTP error 503" / "HTTP/1.1 503"
  /service unavailable/i,
  /bad gateway/i,
  /gateway time-?out/i,
];

/**
 * Classify a devbox/nix/curl `devbox install` error as "transient" (retry) or
 * "permanent" (fail fast). Permanent-first, fail-closed.
 *
 * It inspects the RAW error thrown by the injected `run`. On the default path
 * that is an `execFileAsync` rejection carrying `.stderr` / `.code` / `.killed` /
 * `.signal`.
 *
 * Precedence is load-bearing:
 *   1. A worker-timeout SIGTERM/SIGKILL kill (the 10-min `execFileAsync` timeout
 *      in provision.ts tripped) is a HUNG install, not a transient blip —
 *      retrying it would give 3×10min of dead wall-clock. Never retried, and
 *      checked FIRST so that even a transient-looking stderr on a killed process
 *      still classifies permanent.
 *   2. Otherwise, match the combined stderr + message text against the
 *      narrowly-scoped {@link DEVBOX_TRANSIENT_PATTERNS} ⇒ transient.
 *   3. No match ⇒ permanent (fail-closed): deterministic failures (unknown
 *      package, malformed manifest, resolver error) fail fast, no retry.
 */
export function classifyDevboxError(err: unknown): "transient" | "permanent" {
  // 1. Worker-timeout kill, checked FIRST (precedence).
  if (typeof err === "object" && err !== null) {
    const obj = err as { killed?: unknown; signal?: unknown };
    if (
      obj.killed === true &&
      (obj.signal === "SIGTERM" || obj.signal === "SIGKILL")
    ) {
      return "permanent";
    }
  }

  // 2. Build the text to match: stderr (if a string) plus the message. Testing
  //    both is belt-and-suspenders — devbox errors carry stderr on `.stderr` and
  //    concatenated into `.message`, but the injected `run` may differ.
  let text = "";
  if (typeof err === "object" && err !== null) {
    const stderr = (err as { stderr?: unknown }).stderr;
    if (typeof stderr === "string") text += stderr + "\n";
  }
  text += err instanceof Error ? err.message : String(err);

  for (const re of DEVBOX_TRANSIENT_PATTERNS) {
    if (re.test(text)) return "transient";
  }

  // 3. Fail-closed default.
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

export interface WithRetryOptions {
  /** REQUIRED classifier — no default; each caller supplies its own. */
  classify: (e: unknown) => "transient" | "permanent";
  /** Backoff delays; defaults to {@link FORGE_RETRY_SCHEDULE}. N sleeps ⇒ N+1 attempts. */
  schedule?: number[];
  /** Injectable sleep (tests record delays and resolve immediately). */
  sleep?: (ms: number) => Promise<void>;
  /** Optional logger; a warn is emitted before each retry sleep. */
  log?: { warn: (msg: string, meta?: Record<string, unknown>) => void };
  /** Message logged (log.warn) before each retry sleep. Default a generic phrase. */
  logMessage?: string;
}

/**
 * Generic bounded-backoff retry runner. Run `fn`, retrying it on a TRANSIENT
 * error (per `opts.classify`) using the backoff schedule. A permanent error (or
 * an exhausted schedule) rethrows the last error unchanged, so the caller's
 * existing catch still fires and the run fails as it does today.
 *
 * N schedule entries ⇒ up to N+1 attempts (1 initial + N retries).
 */
export async function withRetry<T>(fn: () => Promise<T>, opts: WithRetryOptions): Promise<T> {
  const classify = opts.classify;
  const schedule = opts.schedule ?? FORGE_RETRY_SCHEDULE;
  const sleep = opts.sleep ?? sleepReal;

  for (let attempt = 0; ; attempt++) {
    try {
      return await fn();
    } catch (err) {
      if (classify(err) === "transient" && attempt < schedule.length) {
        const delay = schedule[attempt]!;
        opts.log?.warn(opts.logMessage ?? "transient error; retrying", {
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

/**
 * Run `fn`, retrying it on a TRANSIENT error using the backoff schedule. A
 * permanent error (or an exhausted schedule) rethrows the last error unchanged, so
 * the caller's existing catch still fires and the run fails as it does today.
 *
 * A thin wrapper over {@link withRetry} that supplies the forge defaults.
 *
 * N schedule entries ⇒ up to N+1 attempts (1 initial + N retries).
 */
export async function withForgeRetry<T>(
  fn: () => Promise<T>,
  opts: WithForgeRetryOptions = {},
): Promise<T> {
  return withRetry(fn, {
    classify: opts.classify ?? classifyForgeError,
    schedule: opts.schedule ?? FORGE_RETRY_SCHEDULE,
    sleep: opts.sleep,
    log: opts.log,
    logMessage: "transient forge error; retrying push/MR-create",
  });
}
