// Harness-neutral background dependency provisioning and run-feed reporting.
import type { RunContext } from "./executor.js";
import type { Logger } from "./log.js";
import { installJsDeps, type JsDepsInstall } from "./js-deps.js";
import { buildCheckEnv } from "./sdk-env.js";
import { clampToDirCharset, errMessage } from "./util.js";

/**
 * Start eagerly so provisioning overlaps planning. Attach the rejection handler
 * immediately, before the caller can leave the promise floating until its join.
 * Synchronous injected throws use the same best-effort warning and safe failure result.
 *
 * buildCheckEnv supplies a scrubbed replacement env and the run's provisioned PATH.
 * HOME is per-run: sharing the runner-writable npm cache would let one run seed
 * content installed by another run.
 */
export function startDepsInstall(
  ctx: Pick<RunContext, "runId" | "worktreePath" | "emit">,
  log: Logger,
  homeDir: string,
  toolEnv: Record<string, string> | undefined,
  signal: AbortSignal,
  installDeps: typeof installJsDeps = installJsDeps,
): Promise<JsDepsInstall> {
  ctx.emit({
    kind: "status",
    agent: "worker",
    payload: {
      text: "installing the repo's JS dependencies (in the background)",
    },
  });
  const failed = (err: unknown): JsDepsInstall => {
    log.warn("JS dependency provisioning failed", {
      run_id: ctx.runId,
      error: errMessage(err),
    });
    return {
      results: [{ dir: ".", manager: "none", ok: false, detail: "dependency installer failed" }],
      truncated: false,
    };
  };
  try {
    return installDeps(
      ctx.worktreePath,
      buildCheckEnv(process.env, homeDir, toolEnv),
      { signal },
    ).catch(failed);
  } catch (err: unknown) {
    return Promise.resolve(failed(err));
  }
}

/** Report an already-joined result; lifecycle ownership stays with the caller. */
export function reportDepsInstall(
  ctx: Pick<RunContext, "runId" | "emit">,
  log: Logger,
  { results, truncated }: JsDepsInstall,
): JsDepsInstall {
  if (results.length === 0 && !truncated) {
    ctx.emit({
      kind: "status",
      agent: "worker",
      payload: { text: "no JS dependencies to install (no lockfile found)" },
    });
    return { results, truncated };
  }
  // One line, naming every dir and — for anything that did not install — why. A
  // silent skip here resurfaces later as an inexplicable `vitest: not found`.
  const installed = results
    .filter((r) => r.ok)
    .map((r) => safeDirLabel(r.dir));
  const skipped = results.filter((r) => !r.ok);
  const parts: string[] = [];
  if (installed.length > 0)
    parts.push(`installed JS dependencies in ${installed.join(", ")}`);
  for (const s of skipped) parts.push(`${safeDirLabel(s.dir)}: ${s.detail}`);
  // Truncation goes on the FEED, not just in a log: without it the line above reads as
  // full coverage, and a `vitest: not found` in dir 13 becomes unexplainable.
  if (truncated)
    parts.push(
      "discovery hit its directory bound — some project dirs were not installed",
    );
  ctx.emit({
    kind: "status",
    agent: "worker",
    payload: { text: parts.join(" — ") },
  });
  log.info("JS dependency provisioning", {
    run_id: ctx.runId,
    results,
    truncated,
  });
  return { results, truncated };
}

/**
 * Render a discovered directory name for the run's activity feed. `dir` comes from
 * `readdir`, i.e. it is REPO-CONTROLLED text: a repo can commit a directory whose name
 * contains newlines, backticks, or instruction-shaped prose, and this string is
 * persisted to `run_messages` and rendered to a human. Not a path escape and React
 * escapes the HTML, but untrusted text should not be able to shape a status line, so the
 * charset is clamped to what a real project dir needs and the length is bounded.
 */
export function safeDirLabel(dir: string): string {
  // 120 chars: this is a rendered feed line, where a long-but-real directory name is
  // more useful than a short one, and React escapes the output — the clamp here is
  // cosmetic plus defence in depth. The PROMPT clamp is load-bearing and uses a
  // tighter bound; both share the charset deliberately (clampToDirCharset).
  return clampToDirCharset(dir, 120);
}
