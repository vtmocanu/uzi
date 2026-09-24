// The read-only model pass (PRD #920 M2): the ONE place uzi's advice lane (judge,
// review, summary) runs its tool-less, repo-isolated SDK query, races it against
// a wall-clock timeout, and accumulates the model's text.
//
// ClaudeAdviceHarness in claude-advice-harness.ts owns SDK options, the literal
// settingSources: [] and the deny-all hook. This file owns timeout/HOME lifecycle
// and the text-returning compatibility boundary for judge/review/summary.
//
// FAILURE HANDLING. review/summary get the helper's DEFAULT: an error result frame
// throws a generic `${label} model call returned an error result`, a success frame
// returns the accumulated text. The judge injects rate-limit classification via
// `onResult` (it throws LimitReachedError on a usage-limit death, else the same generic
// error; on success it captures the terminal frame). The helper ALWAYS runs a
// RateLimitObserver so `onResult` always has the latest observation — it is inert for
// review/summary (zero behavior impact) and supplies `latest` to the judge.
//
// PRD #1429 M3: the pass now chooses the advice harness by CLAIM SHAPE. When the caller
// supplies `opts.codex` (a Codex judge/review claim carrying secrets.codex), it builds a
// CodexAdviceHarness through the INJECTED factory (`opts.codex.buildHarness` — produced by
// `makeProductionCodexAdviceHarnessFactory` in codex/codex-executor.ts, one of the two
// sites `semgrep/codex-fixed-constructor.yml` allows to construct one) instead of
// `ClaudeAdviceHarness`. This file never constructs a Codex class itself. The Claude path
// below is otherwise BYTE-IDENTICAL to before this change.

import { promises as fs } from "node:fs";
import path from "node:path";

import type { EffortLevel } from "@anthropic-ai/claude-agent-sdk";

import { ClaudeAdviceHarness } from "./claude-advice-harness.js";
import { uidSplitActive } from "./runner-uid.js";
import { rmHomeTree } from "./rmtree.js";
import { LimitReachedError, type RateLimitObservation } from "./limit.js";
import { errMessage } from "./util.js";
import type { Logger } from "./log.js";
import type { WorkerClient } from "./client.js"; // type-only — erased at runtime; client.ts imports no runner/model-pass, so no cycle
import { postTerminalState, type TerminalOutboxDeps } from "./terminal-resolve.js";
import type { StateRequest } from "./protocol.js";
import type { SdkQueryFn } from "./sdk-executor.js"; // type-only — erased at runtime, so no import cycle
import type { CodexBinding } from "./codex/select.js"; // type-only — erased at runtime, no construction here
import type { CodexAdviceHarnessFactory } from "./codex/codex-executor.js"; // type-only — the injected seam, never constructed here
import type {
  AdviceRequest,
  AdviceResultPolicy,
} from "./harness.js"; // type-only — erased at runtime; harness.ts has no SDK import, so no cycle

/** Options for runReadOnlyModelPass — one tool-less, repo-isolated model turn. */
export interface ReadOnlyModelPassOpts {
  /** The Anthropic token for the CLAUDE path. Omit ONLY when `codex` is set — a Codex
   *  judge/review claim carries no Anthropic token at all (PRD #1429 M3, D4's auditor
   *  invariant: no anthropic_oauth_token, no run_credential_epochs row). */
  token?: string;
  /** The model id; applied to the query only when non-empty. */
  model?: string;
  /** Reasoning effort; applied to the query only when set. */
  effort?: EffortLevel;
  systemPrompt: string;
  prompt: string;
  /** Root under which the per-pass ephemeral SDK HOME is created. */
  homeRoot: string;
  /** mkdtemp prefix, e.g. "uzi-judge-" | "uzi-review-" | "uzi-summary-". */
  homePrefix: string;
  /** "judge" | "review" | "summary" — used in the timeout + generic-error messages. */
  label: string;
  timeoutMs: number;
  /** Bounded grace, after the wall-clock abort, to wait for the SDK query to settle
   *  before the ephemeral HOME is removed — so cleanup does not race the aborted CLI
   *  while it is still exiting and may still touch $HOME. Defaults to
   *  DEFAULT_ABORT_GRACE_MS; the three runners omit it. Tests inject a value to drive
   *  the timeout path deterministically. */
  graceMs?: number;
  queryFn: SdkQueryFn;
  /** The deny-hook reason string (verbatim per runner). */
  denyReason: string;
  log: Logger;
  /** Judge-only seam. Invoked once on the TERMINAL result frame (error OR success). It
   *  MAY throw to override the default failure (the judge throws LimitReachedError on a
   *  usage-limit death); on a success frame it is a capture side-effect. When omitted
   *  (review/summary), the helper's default applies: throw a generic
   *  `${label} model call returned an error result` on an error frame, return the
   *  accumulated text on success. */
  onResult?(msg: unknown, ctx: { isError: boolean; latest: RateLimitObservation | undefined }): void;
  /** PRD #1429 M3: when set, this pass runs the ISOLATED CODEX advice harness (built via
   *  the injected `buildHarness` factory — never constructed in this file, per
   *  semgrep/codex-fixed-constructor.yml) instead of ClaudeAdviceHarness. Set by the caller
   *  when the claim carries a validated `secrets.codex` block
   *  (codex/select.ts's selectCodexBinding). Mutually exclusive with the Claude path below
   *  in practice — `token` is simply unused when this is set. */
  codex?: {
    readonly runId: string;
    readonly binding: CodexBinding;
    readonly buildHarness: CodexAdviceHarnessFactory;
  };
}

/** Default bounded grace (ms) for the aborted SDK CLI to finish exiting before its
 *  ephemeral HOME is removed. Generous versus the near-immediate ProcessTransport.close()
 *  termination scheduling, and it is a best-effort cleanup guard, not a correctness
 *  guarantee — the grace only ever elapses on the timeout path when the query does not
 *  settle promptly after abort (the normal success path settles the query before cleanup,
 *  so no grace is waited). */
const DEFAULT_ABORT_GRACE_MS = 500;

/** Wait for the (possibly aborted) SDK query to settle before its HOME is removed, so the
 *  ephemeral HOME is not rm'd while the aborted CLI is still exiting and may still touch
 *  $HOME (the SDK's abort handler terminates the CLI asynchronously). Bounded by graceMs
 *  so a query that never settles after abort can never wedge cleanup. NEVER rejects: the
 *  query's own rejection was already surfaced to the caller by the race in
 *  runReadOnlyModelPass, so it is swallowed here (attaching a rejection handler also keeps
 *  a post-abort rejection from surfacing as an unhandled rejection) — cleanup must never
 *  fail a run. */
async function awaitQuerySettled(query: Promise<unknown>, graceMs: number): Promise<void> {
  let graceTimer: NodeJS.Timeout | undefined;
  const grace = new Promise<void>((resolve) => {
    graceTimer = setTimeout(resolve, graceMs);
    graceTimer.unref?.();
  });
  try {
    await Promise.race([query.then(() => {}, () => {}), grace]);
  } finally {
    if (graceTimer) clearTimeout(graceTimer);
  }
}

/**
 * Run one read-only model pass: create an ephemeral SDK HOME, race the isolation-shaped
 * SDK query against a wall-clock timeout, accumulate the model's text, and clean up the
 * HOME. Returns the accumulated text (review/summary) — the judge captures its terminal
 * frame via `onResult` and reads the text from the return value.
 *
 * PRD #1429 M3: when `opts.codex` is set this delegates to {@link runCodexAdviceModelPass}
 * instead — an EARLY branch, before any of the Claude-specific HOME/timer setup below runs,
 * so the Claude path stays byte-identical to before this change.
 */
export async function runReadOnlyModelPass(opts: ReadOnlyModelPassOpts): Promise<string> {
  if (opts.codex) return runCodexAdviceModelPass(opts, opts.codex);

  const token = opts.token;
  if (!token) {
    // Defensive, not a runtime condition to degrade from: every existing Claude caller
    // (judge-runner.ts/review-runner.ts) already checked for a non-empty token before ever
    // calling this function without `codex` set (see their own "no anthropic token" fallback
    // branches). Reaching here with neither is a programming error.
    throw new Error(`${opts.label} model pass requires a token or a codex claim`);
  }
  const homeDir = await fs.mkdtemp(path.join(opts.homeRoot, opts.homePrefix));
  // PRD #51 M4: the advice SDK CLI runs as the `runner` uid (spawnClaudeCodeProcess ->
  // runnerSpawn), but fs.mkdtemp FORCES mode 0700 (Node ignores umask) and this runner
  // runs in the WORKER process, so the HOME is worker-owned 0700 — the runner gets ZERO
  // access (the setgid /data/agent-home parent sets the dir's group `runner`, but 0700
  // grants the group nothing) and the CLI cannot write $HOME/.claude. Under the split,
  // widen it to 2770 (group `runner` rwx) so the runner can use it. Group membership
  // does NOT let the worker rm it: the CLI writes runner-owned private (0700) dirs
  // inside, which only a `runner`-uid helper can remove (rmHomeTree, #1607). The unit-test / single-uid (#58)
  // path leaves 0700 (the pass runs as the worker — same uid, 0700 is correct + tighter).
  if (uidSplitActive()) await fs.chmod(homeDir, 0o2770);
  // Wall-clock cap: abort the SDK query (native cancellation) AND hard-reject the race,
  // so a hung/retrying model call can never wedge the run — the pass settles within
  // timeoutMs and the caller falls back.
  const abort = new AbortController();
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      abort.abort();
      reject(new Error(`${opts.label} model call exceeded ${opts.timeoutMs}ms`));
    }, opts.timeoutMs);
  });
  // The default neutral policy: throw the generic labeled error on a failed terminal,
  // do nothing on success. This is byte-identical to today's `else if (isError) throw`
  // branch. The judge's raw-msg shim REPLACES it (never called alongside), mirroring the
  // old if/else. Either callback fires synchronously inside the terminal iteration body,
  // before `break`, so a throw propagates out of run().
  const policy: AdviceResultPolicy = {
    onTerminal: (_t, { isError }) => {
      if (isError) throw new Error(`${opts.label} model call returned an error result`);
    },
  };
  const request: AdviceRequest = {
    label: opts.label as AdviceRequest["label"],
    systemPrompt: opts.systemPrompt,
    prompt: opts.prompt,
    model: opts.model,
    effort: opts.effort,
    output: { kind: "text" },
    signal: abort.signal,
    timeoutMs: opts.timeoutMs,
    graceMs: opts.graceMs,
  };
  const runPromise = new ClaudeAdviceHarness({
    token,
    homeDir,
    abort,
    queryFn: opts.queryFn,
    denyReason: opts.denyReason,
    log: opts.log,
    rawResultShim: opts.onResult,
  })
    .run(request, policy)
    .then((r) => r.text);
  try {
    return await Promise.race([runPromise, timeout]);
  } finally {
    if (timer) clearTimeout(timer);
    // Defer HOME cleanup until the query settles (bounded by a grace after abort): on the
    // timeout path the race rejects as soon as `abort.abort()` fires, but the SDK
    // terminates the CLI asynchronously, so removing HOME here immediately could race the
    // aborted CLI while it is still exiting and may still touch $HOME. On the success path
    // the query has already settled, so this returns without waiting.
    await awaitQuerySettled(runPromise, opts.graceMs ?? DEFAULT_ABORT_GRACE_MS);
    // Best-effort HOME cleanup. The M6 reclaim sweep will NEVER collect this directory:
    // it is named `uzi-<label>-*`, not a run UUID, so the sweep's RUN_ID_RE filter skips
    // it BY DESIGN — this warn is the only thing anywhere that will say a dir stranded.
    // Still best-effort: a cleanup must never fail a run.
    await rmHomeTree(homeDir).catch((e) =>
      opts.log.warn(`${opts.label} HOME cleanup failed`, { home_dir: homeDir, error: errMessage(e) }),
    );
  }
}

/**
 * PRD #1429 M3: the CODEX sibling of {@link runReadOnlyModelPass}'s Claude path above — the
 * SAME timeout/policy/request shape, but the harness is built via the INJECTED
 * `codex.buildHarness` factory (produced by
 * `codex/codex-executor.ts`'s `makeProductionCodexAdviceHarnessFactory`, one of the two
 * sites `semgrep/codex-fixed-constructor.yml` allows to construct a Codex class — this file
 * never constructs one itself) instead of `new ClaudeAdviceHarness(...)`.
 *
 * There is no ephemeral HOME for this function to create or clean up: the Codex advice
 * harness launches its own isolated, disposable provider root and disposes it inside its
 * own `run()` (codex-advice-harness.ts / codex-executor.ts's
 * `makeProductionLaunchAdviceRoot`), so unlike the Claude path there is nothing here for a
 * `finally` to `rm`.
 *
 * `opts.onResult` (the judge's raw-SDK-message rate-limit shim) is Claude-SDK-specific and
 * is NOT invoked on this path: Codex M3 carries no rate-limit facts yet
 * (codex-advice-harness.ts's `decodeTerminal`), so the neutral `policy` below is the whole
 * failure contract — an error terminal throws the same generic labeled error the
 * review/summary Claude callers get.
 */
async function runCodexAdviceModelPass(
  opts: ReadOnlyModelPassOpts,
  codex: NonNullable<ReadOnlyModelPassOpts["codex"]>,
): Promise<string> {
  const abort = new AbortController();
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      abort.abort();
      reject(new Error(`${opts.label} model call exceeded ${opts.timeoutMs}ms`));
    }, opts.timeoutMs);
  });
  const policy: AdviceResultPolicy = {
    onTerminal: (_t, { isError }) => {
      if (isError) throw new Error(`${opts.label} model call returned an error result`);
    },
  };
  const request: AdviceRequest = {
    label: opts.label as AdviceRequest["label"],
    systemPrompt: opts.systemPrompt,
    prompt: opts.prompt,
    model: opts.model,
    effort: opts.effort,
    output: { kind: "text" },
    signal: abort.signal,
    timeoutMs: opts.timeoutMs,
    graceMs: opts.graceMs,
  };
  const runPromise = (async (): Promise<string> => {
    const harness = await codex.buildHarness({ runId: codex.runId, binding: codex.binding, signal: abort.signal });
    const result = await harness.run(request, policy);
    return result.text;
  })();
  try {
    return await Promise.race([runPromise, timeout]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

/** The advice lane's failure_reason byte cap. Deliberately 500 — NOT runner.ts's
 *  MAX_FAILURE_REASON_LEN (512). The two lanes cap independently; unifying them would be
 *  a behavior change (a cap is behavior), so the advice lane keeps 500. See PRD #920 D5.
 *  Module-local (not exported): its only consumer is safeReportFailed below, and the agent
 *  knip gate (`exports: error`, ignoreExportsUsedInFile scoped to interface+type only)
 *  reddens on an exported constant used solely in its own file. */
const ADVICE_FAILURE_REASON_LEN = 500;

/** Best-effort "this advice run failed" report. NEVER throws into the caller — a
 *  state-report failure must not fail an advice run (judge/review — summary has no
 *  failed-state path and does not call this). The optional
 *  `cause` lets the judge map a LimitReachedError to the server's structured limit facts
 *  (PRD #35 Decision 8): failure_reason is OMITTED in that case so the server composes the
 *  sentence from its own allowlisted enum rather than the worker smuggling an unvalidated
 *  rateLimitType in as free text.
 *
 *  `claimGeneration` (PRD #1247 M2 fix round) is the run-lane claim generation this failed
 *  report is made against. The judge/review runners call this from OUTSIDE the RunRunner
 *  reportState stamping closure, so they thread the claim's generation through here to stamp
 *  it — UNCONDITIONALLY, mirroring that closure — on BOTH failed body shapes, so a
 *  credential_switch_v1 capability worker's failed advice report engages the server's
 *  per-query generation fence instead of being refused with a 409. Undefined on a pre-#1296
 *  claim, which leaves the field off the wire.
 *
 *  `terminalDeps` (PRD #1391 Run B M3b, D6) journals the terminal STATE write-ahead when an outbox
 *  is wired, so a lost judge/review terminal cannot leave the run `running` forever (the timeout
 *  sweep excludes judge runs, fact 12). NEVER the verdict/review POST — the caller posts that
 *  separately, before this. The message fence is 0: a judge/review trace is a single advisory usage
 *  frame that never gates api sub-work, so no fence is needed and none can strand the outcome.
 *  Undefined (a test without spill, or a store that failed closed) ⇒ today's un-journaled report. */
export async function safeReportFailed(
  client: Pick<WorkerClient, "reportState">,
  log: Logger,
  label: string,
  runId: string,
  reason: string,
  cause?: unknown,
  claimGeneration?: number,
  terminalDeps?: TerminalOutboxDeps,
): Promise<void> {
  try {
    const body: StateRequest =
      cause instanceof LimitReachedError
        ? {
            status: "failed",
            rate_limit_type: cause.rateLimitType,
            limit_resets_at: cause.resetsAtMs,
            claim_generation: claimGeneration,
          }
        : {
            status: "failed",
            failure_reason: reason.slice(0, ADVICE_FAILURE_REASON_LEN),
            claim_generation: claimGeneration,
          };
    await postTerminalState(terminalDeps, client, {
      runId,
      claimGeneration: claimGeneration ?? 0,
      phase: "running",
      messagesThroughSeq: 0,
      body,
    });
  } catch (err) {
    log.warn(`${label} failed-state report failed`, { run_id: runId, error: errMessage(err) });
  }
}
