// The read-only model pass (PRD #920 M2): the ONE place uzi's advice lane (judge,
// review, summary) constructs its tool-less, repo-isolated SDK query, races it against
// a wall-clock timeout, and accumulates the model's text.
//
// 🔴 THIS FILE IS THE ISOLATION SINGLE POINT OF TRUTH FOR THE ADVICE LANE. Before M2
// the judge/review/summary runners each built the same isolation-shaped SdkOptions
// (`settingSources: []`, a deny-all PreToolUse hook, bypassPermissions, the runner-uid
// detached spawn) at their own `query` site. They are now ONE call to
// runReadOnlyModelPass below, so the advice-lane literal `settingSources: []` lives at
// exactly one query site (this file), alongside sdk-executor.ts and chat-executor.ts.
//
// 🔴 SEMGREP OMITTED-KEY BLINDNESS. The semgrep rule
// semgrep/settings-sources-isolation.yml fires on a WIDENED value only
// (`settingSources: ["project"]`, `settingSources: someVar`) and is BLIND to an
// OMITTED key — the SDK default is fail-open (an absent `settingSources` loads the
// checked-out repo's `.claude/` as configuration, the exact repo-borne
// prompt-injection vector this isolation blocks). So a future edit that DROPPED the
// key from the options below would pass semgrep silently and re-open the vector. Keep
// `settingSources: []` a literal at the query site (see runReadOnlyModelPass), do not
// extract it, do not spread it in.
//
// FAILURE HANDLING. review/summary get the helper's DEFAULT: an error result frame
// throws a generic `${label} model call returned an error result`, a success frame
// returns the accumulated text. The judge injects rate-limit classification via
// `onResult` (it throws LimitReachedError on a usage-limit death, else the same generic
// error; on success it captures the terminal frame). The helper ALWAYS runs a
// RateLimitObserver so `onResult` always has the latest observation — it is inert for
// review/summary (zero behavior impact) and supplies `latest` to the judge.

import { promises as fs } from "node:fs";
import path from "node:path";

import type {
  HookInput,
  HookJSONOutput,
  Options as SdkOptions,
  SpawnedProcess,
} from "@anthropic-ai/claude-agent-sdk";

import { spawnDetached } from "./sdk-spawn.js";
import { uidSplitActive } from "./runner-uid.js";
import { buildSdkEnv } from "./sdk-env.js";
import { rmTreeForce } from "./rmtree.js";
import { promptStream, mapSdkMessage, isResult, isErrorResult } from "./sdk-messages.js";
import { RateLimitObserver, LimitReachedError, type RateLimitObservation } from "./limit.js";
import { errMessage } from "./util.js";
import type { Logger } from "./log.js";
import type { WorkerClient } from "./client.js"; // type-only — erased at runtime; client.ts imports no runner/model-pass, so no cycle
import type { SdkQueryFn } from "./sdk-executor.js"; // type-only — erased at runtime, so no import cycle

/** A PreToolUse deny for EVERY tool: the advice runners (judge/review/summary) are
 *  read-only. A deny is authoritative even under bypassPermissions (the same property
 *  guardrails.ts relies on). Internal to this file since M2 — the only caller is
 *  runReadOnlyModelPass below (the advice lane no longer builds the hook itself). */
function buildDenyAllHook(reason: string) {
  return async (_input: HookInput): Promise<HookJSONOutput> => ({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: reason,
    },
  });
}

/** Options for runReadOnlyModelPass — one tool-less, repo-isolated model turn. */
export interface ReadOnlyModelPassOpts {
  token: string;
  /** The model id; applied to the query only when non-empty. */
  model?: string;
  systemPrompt: string;
  prompt: string;
  /** Root under which the per-pass ephemeral SDK HOME is created. */
  homeRoot: string;
  /** mkdtemp prefix, e.g. "uzi-judge-" | "uzi-review-" | "uzi-summary-". */
  homePrefix: string;
  /** "judge" | "review" | "summary" — used in the timeout + generic-error messages. */
  label: string;
  timeoutMs: number;
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
}

/**
 * Run one read-only model pass: create an ephemeral SDK HOME, race the isolation-shaped
 * SDK query against a wall-clock timeout, accumulate the model's text, and clean up the
 * HOME. Returns the accumulated text (review/summary) — the judge captures its terminal
 * frame via `onResult` and reads the text from the return value.
 */
export async function runReadOnlyModelPass(opts: ReadOnlyModelPassOpts): Promise<string> {
  const homeDir = await fs.mkdtemp(path.join(opts.homeRoot, opts.homePrefix));
  // PRD #51 M4: the advice SDK CLI runs as the `runner` uid (spawnClaudeCodeProcess ->
  // runnerSpawn), but fs.mkdtemp FORCES mode 0700 (Node ignores umask) and this runner
  // runs in the WORKER process, so the HOME is worker-owned 0700 — the runner gets ZERO
  // access (the setgid /data/agent-home parent sets the dir's group `runner`, but 0700
  // grants the group nothing) and the CLI cannot write $HOME/.claude. Under the split,
  // widen it to 2770 (group `runner` rwx) so the runner can use it and the worker (a
  // `runner`-group member) can still rm it on cleanup. The unit-test / single-uid (#58)
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
  try {
    return await Promise.race([consume(opts, homeDir, abort), timeout]);
  } finally {
    if (timer) clearTimeout(timer);
    // Best-effort HOME cleanup. The M6 reclaim sweep will NEVER collect this directory:
    // it is named `uzi-<label>-*`, not a run UUID, so the sweep's RUN_ID_RE filter skips
    // it BY DESIGN — this warn is the only thing anywhere that will say a dir stranded.
    // Still best-effort: a cleanup must never fail a run.
    await rmTreeForce(homeDir).catch((e) =>
      opts.log.warn(`${opts.label} HOME cleanup failed`, { home_dir: homeDir, error: errMessage(e) }),
    );
  }
}

/** Build the isolation-shaped options and stream one tool-less turn. */
async function consume(opts: ReadOnlyModelPassOpts, homeDir: string, abort: AbortController): Promise<string> {
  const env = buildSdkEnv(opts.token, homeDir);
  const options: SdkOptions = {
    env: env as unknown as Record<string, string | undefined>,
    abortController: abort,
    // 🔴 ISOLATION SINGLE POINT OF TRUTH. `settingSources: []` MUST stay a LITERAL at
    // this one query site. The semgrep rule semgrep/settings-sources-isolation.yml
    // fires on a WIDENED value only and is BLIND to an OMITTED key — so this is now the
    // ONLY place the advice-lane literal lives, and a future edit that dropped this key
    // would pass semgrep silently and re-open the repo-borne prompt-injection vector.
    // Do NOT extract it to a variable, do NOT spread it in, do NOT delete it.
    settingSources: [],
    systemPrompt: opts.systemPrompt,
    permissionMode: "bypassPermissions",
    allowDangerouslySkipPermissions: true,
    includePartialMessages: false,
    hooks: { PreToolUse: [{ hooks: [buildDenyAllHook(opts.denyReason)] }] },
    // Route the model-reasoning SDK CLI through the runner-uid detached spawn like every
    // other SDK spawn (uniform boundary); the deny-all hook already blocks code-exec, so
    // this is defense-in-depth. (PRD #51 M4 — keep this rationale.)
    spawnClaudeCodeProcess: (spawnOpts) => spawnDetached(spawnOpts) as unknown as SpawnedProcess,
  };
  if (opts.model) options.model = opts.model;

  let text = "";
  const rateLimits = new RateLimitObserver();
  for await (const msg of opts.queryFn({ prompt: promptStream(opts.prompt), options })) {
    rateLimits.observe(msg);
    for (const em of mapSdkMessage(msg)) {
      if (em.kind === "text") {
        const t = (em.payload as { text?: string }).text;
        if (t) text += t;
      }
    }
    if (isResult(msg)) {
      const isError = isErrorResult(msg);
      if (opts.onResult) {
        opts.onResult(msg, { isError, latest: rateLimits.latest });
      } else if (isError) {
        throw new Error(`${opts.label} model call returned an error result`);
      }
      break;
    }
  }
  return text;
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
 *  rateLimitType in as free text. */
export async function safeReportFailed(
  client: Pick<WorkerClient, "reportState">,
  log: Logger,
  label: string,
  runId: string,
  reason: string,
  cause?: unknown,
): Promise<void> {
  try {
    const body =
      cause instanceof LimitReachedError
        ? { status: "failed" as const, rate_limit_type: cause.rateLimitType, limit_resets_at: cause.resetsAtMs }
        : { status: "failed" as const, failure_reason: reason.slice(0, ADVICE_FAILURE_REASON_LEN) };
    await client.reportState(runId, body);
  } catch (err) {
    log.warn(`${label} failed-state report failed`, { run_id: runId, error: errMessage(err) });
  }
}
