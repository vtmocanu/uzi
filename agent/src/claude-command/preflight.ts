// Boot preflight for the guarded-command launcher (issue #2471, M1). When the memory guard is
// enabled the worker must be able to start the immutable supervisor at the runner uid and have
// it accept the profile, or it refuses to register (worker.ts throws on a failed result).
//
// The probe launches the supervisor through the same root-launch primitive the guarded command
// uses, with an inert `/bin/true` child instead of the wrapper. It requires `started` evidence,
// then a clean disposal: state drained and a supervisor exit of 0 (both enforced by the
// handle's dispose), all within one deadline. It never throws; failures are results.

import {
  killUnreapedSupervisor,
  launchRunnerSupervisedProbe,
  type LauncherDeps,
  type RunnerCommandChild,
  type RunnerCommandRoot,
  type RunnerCommandSpawned,
} from "../codex/launcher.js";

export type MemoryLauncherPreflightResult = { ok: true } | { ok: false; reason: string };

/** What the preflight asks its launch seam for: deadlines and the post-spawn child hook. */
export interface MemoryLauncherProbeOptions {
  readonly deadlines: { readonly started: number };
  readonly onSpawned: (root: RunnerCommandSpawned, child: RunnerCommandChild) => void;
}

export interface MemoryLauncherPreflightDeps {
  /** Launch seam (tests inject a fake); the default launches the real probe. */
  readonly launch?: (options: MemoryLauncherProbeOptions) => Promise<RunnerCommandRoot>;
  /** Total budget for launch, `started` and disposal (default 15000 ms). */
  readonly deadlineMs?: number;
  /** Launcher deps for the default launch seam. */
  readonly launcherDeps?: LauncherDeps;
  /** Kill seam for an unreaped supervisor after a failure (default: killUnreapedSupervisor). */
  readonly killUnreaped?: (child: RunnerCommandChild) => boolean;
}

const DEFAULT_PREFLIGHT_DEADLINE_MS = 15_000;
const PROFILE_REASON_RE = /profile:[A-Za-z0-9_]+/;
const MAX_REASON_CHARS = 200;

/** The `profile:<field>` token an abnormal supervisor report left anywhere on an error chain. */
function failureReason(error: unknown): string {
  const seen = new Set<unknown>();
  let fallback: string | undefined;
  const queue: unknown[] = [error];
  while (queue.length > 0 && seen.size < 8) {
    const current = queue.shift();
    if (typeof current !== "object" || current === null || seen.has(current)) continue;
    seen.add(current);
    const message = current instanceof Error ? current.message : "";
    const profile = PROFILE_REASON_RE.exec(message);
    if (profile) return profile[0];
    fallback ??= message || undefined;
    const link = current as { cause?: unknown; cleanupCause?: unknown };
    queue.push(link.cause, link.cleanupCause);
  }
  return (fallback ?? String(error)).slice(0, MAX_REASON_CHARS);
}

export async function memoryLauncherPreflight(deps: MemoryLauncherPreflightDeps = {}): Promise<MemoryLauncherPreflightResult> {
  const deadlineMs = deps.deadlineMs ?? DEFAULT_PREFLIGHT_DEADLINE_MS;
  const startedAt = Date.now();
  const remaining = (): number => Math.max(1, deadlineMs - (Date.now() - startedAt));
  let child: RunnerCommandChild | undefined;
  const launch = deps.launch ?? ((options: MemoryLauncherProbeOptions) =>
    launchRunnerSupervisedProbe({ ...deps.launcherDeps, deadlines: { ...deps.launcherDeps?.deadlines, ...options.deadlines } },
      options.onSpawned));
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`launcher preflight deadline exceeded (${String(deadlineMs)}ms)`)), deadlineMs);
  });
  const attempt = (async (): Promise<MemoryLauncherPreflightResult> => {
    const root = await launch({
      deadlines: { started: deadlineMs },
      onSpawned: (_root, spawned) => { child = spawned; },
    });
    child ??= root.child;
    if (!root.handle.started) return { ok: false, reason: "no started evidence" };
    const outcome = await root.handle.dispose(remaining());
    if (!outcome.clean) return { ok: false, reason: failureReason(new Error(outcome.reason)) };
    if (outcome.event?.state !== "drained") return { ok: false, reason: "disposal was not drained" };
    return { ok: true };
  })();
  void attempt.catch(() => undefined);
  try {
    const result = await Promise.race([attempt, timeout]);
    if (!result.ok && child) (deps.killUnreaped ?? killUnreapedSupervisor)(child);
    return result;
  } catch (error) {
    if (child) {
      try { (deps.killUnreaped ?? killUnreapedSupervisor)(child); } catch { /* the verdict stands */ }
    }
    return { ok: false, reason: failureReason(error) };
  } finally {
    if (timer) clearTimeout(timer);
  }
}
