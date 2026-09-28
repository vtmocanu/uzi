// issue #1866 M1: the run-start environment probe. At run start the worker measures what the
// lead's execution environment cannot do (enumerate /proc, write $HOME, write $TMPDIR) with a
// FIXED script, and the prompt tells the lead (prompt.ts buildEnvironmentFactsBlock), so a gate
// blocked by a verified limit is recorded as blocked instead of retried unchanged.
//
// The probe script is a CONSTANT. No repo script, model text or claim data ever enters its argv
// or its source: argv is exactly [process.execPath, "-e", ENV_PROBE_SCRIPT].
//
// The fact logic is written ONCE, as a plain-JavaScript source string (PROBE_FACTS_SOURCE). The
// shipped script embeds that string, and `probeFacts` (the unit-tested form) is compiled from the
// same string. It is deliberately not derived from a TypeScript function's
// Function.prototype.toString: under tsx (esbuild keepNames) that output carries `__name(...)`
// helper calls that do not exist in a plain `node -e`, so the shipped script would throw.

import type { ChildProcess, SpawnOptions } from "node:child_process";
import { killRunnerGroupOnly, runnerSpawn } from "./runner-uid.js";
import { processGroupPresent } from "./sdk-spawn.js";

/** One measured fact: `ok` (works), `limited` (a verified permission/read-only denial), or
 *  `unverified` (the probe could not tell: it failed, timed out, or saw an unexpected error). */
export type EnvFactStatus = "ok" | "limited" | "unverified";

/** What the prompt formatter renders. `dockerWired` is worker configuration, not a probe. */
export interface EnvFacts {
  harness: "claude" | "codex";
  dockerWired: boolean;
  proc: EnvFactStatus;
  home: EnvFactStatus;
  tmp: EnvFactStatus;
}

const STATUS_WORD: Readonly<Record<EnvFactStatus, string>> = {
  ok: "ok",
  limited: "limited",
  unverified: "not verified",
};

/**
 * issue #1866 M2: the one-line worker status summary of the facts, e.g.
 * `environment facts (claude): /proc limited; $HOME ok; $TMPDIR not verified; docker not wired`.
 * Empty exactly when the prompt block is empty (everything ok and Docker wired), so a run whose
 * prompt carries no facts block emits no status line either.
 */
export function environmentFactsSummary(facts: EnvFacts): string {
  if (facts.proc === "ok" && facts.home === "ok" && facts.tmp === "ok" && facts.dockerWired) return "";
  return (
    `environment facts (${facts.harness}): /proc ${STATUS_WORD[facts.proc]}; $HOME ${STATUS_WORD[facts.home]}; ` +
    `$TMPDIR ${STATUS_WORD[facts.tmp]}; docker ${facts.dockerWired ? "wired" : "not wired"}`
  );
}

interface ProbeFs {
  readdirSync(path: string): readonly unknown[];
  mkdtempSync(prefix: string): string;
  rmdirSync(path: string): void;
}

interface MeasuredFacts {
  proc: EnvFactStatus;
  home: EnvFactStatus;
  tmp: EnvFactStatus;
}

// Plain ES5-style JavaScript (no TypeScript syntax, no template placeholders, no helpers), so it
// runs identically in `node -e` and when compiled below. Self-contained: it closes over nothing.
const PROBE_FACTS_SOURCE = `function (fs, env) {
  var proc = "unverified";
  try {
    var entries = fs.readdirSync("/proc");
    if (entries && entries.length > 0) proc = "ok";
  } catch (e) {
    if (e && e.code === "EACCES") proc = "limited";
  }
  var dirs = [env.HOME, env.TMPDIR];
  var out = [];
  for (var i = 0; i < dirs.length; i++) {
    var dir = dirs[i];
    var status = "unverified";
    if (typeof dir === "string" && dir !== "") {
      var made = null;
      try {
        made = fs.mkdtempSync(dir + "/.uzi-envprobe-");
      } catch (e) {
        var code = e && e.code;
        if (code === "EACCES" || code === "EPERM" || code === "EROFS") status = "limited";
      }
      if (made !== null) {
        try {
          fs.rmdirSync(made);
          status = "ok";
        } catch (e) {
          status = "unverified";
        }
      }
    }
    out.push(status);
  }
  return { proc: proc, home: out[0], tmp: out[1] };
}`;

/**
 * The pure fact logic, compiled from the same source string the shipped script embeds (see the
 * file header). Takes an fs-like object and an env; never throws for fs errors.
 */
// The source is a module constant, never external input.
export const probeFacts = new Function(`return (${PROBE_FACTS_SOURCE});`)() as (
  fs: ProbeFs,
  env: Readonly<Record<string, string | undefined>>,
) => MeasuredFacts;

/**
 * The shipped probe script. Prints exactly one line, `{"uzi_envprobe":1,"proc":…,"home":…,"tmp":…}`,
 * and never throws: any failure prints the all-`unverified` line.
 */
export const ENV_PROBE_SCRIPT = [
  `"use strict";`,
  `var r = { uzi_envprobe: 1, proc: "unverified", home: "unverified", tmp: "unverified" };`,
  `try {`,
  `  var f = (${PROBE_FACTS_SOURCE})(require("node:fs"), process.env);`,
  `  r = { uzi_envprobe: 1, proc: f.proc, home: f.home, tmp: f.tmp };`,
  `} catch (e) {}`,
  `process.stdout.write(JSON.stringify(r) + "\\n");`,
].join("\n");

/** The probe's exact argv. Nothing variable enters it. */
export const ENV_PROBE_ARGV: readonly string[] = Object.freeze([process.execPath, "-e", ENV_PROBE_SCRIPT]);

/**
 * Runs the probe and always settles (the spawner owns reaping; it must not resolve while the
 * probe may still be running unless it reports `cleanedUp: false`). `cleanedUp` is true only
 * when the probe's process group is confirmed gone (or never started).
 */
export type EnvProbeSpawner = (
  argv: readonly string[],
  env: NodeJS.ProcessEnv,
  signal: AbortSignal,
) => Promise<{ code: number; stdout: string; cleanedUp: boolean }>;

/** Thrown by a spawner that could not confirm the probe's cleanup. */
export class ProbeCleanupError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ProbeCleanupError";
  }
}

/** runEnvProbe's fail-closed error: the probe may have left a process behind. */
export class EnvProbeCleanupError extends Error {
  constructor(detail?: string) {
    super(
      "environment probe: cleanup of the probe process could not be confirmed" +
        (detail ? ` (${detail})` : ""),
    );
    this.name = "EnvProbeCleanupError";
  }
}

/** The env the probe runs under: a copy without NODE_OPTIONS / NODE_PATH (a preload or module
 *  path must not change what the fixed script executes). HOME / TMPDIR are kept: they are what
 *  is measured. */
export function probeEnv(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const copy: NodeJS.ProcessEnv = { ...env };
  delete copy.NODE_OPTIONS;
  delete copy.NODE_PATH;
  return copy;
}

const MAX_PROBE_STDOUT_BYTES = 4096;
const STATUSES: ReadonlySet<string> = new Set(["ok", "limited", "unverified"]);

function parseProbeLine(stdout: string): MeasuredFacts | undefined {
  if (Buffer.byteLength(stdout, "utf8") > MAX_PROBE_STDOUT_BYTES) return undefined;
  if (!stdout.endsWith("\n")) return undefined;
  const line = stdout.slice(0, -1);
  if (line.includes("\n") || line.includes("\r")) return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return undefined;
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return undefined;
  const obj = parsed as Record<string, unknown>;
  const keys = Object.keys(obj).sort();
  if (keys.join(",") !== "home,proc,tmp,uzi_envprobe") return undefined;
  if (obj.uzi_envprobe !== 1) return undefined;
  const { proc, home, tmp } = obj;
  if (typeof proc !== "string" || !STATUSES.has(proc)) return undefined;
  if (typeof home !== "string" || !STATUSES.has(home)) return undefined;
  if (typeof tmp !== "string" || !STATUSES.has(tmp)) return undefined;
  return { proc, home, tmp } as MeasuredFacts;
}

/**
 * Run the fixed probe through `spawner` and return the facts. Any failure (spawn error, timeout,
 * non-zero exit, oversize or malformed output) yields all three facts `unverified`. A spawner that
 * cannot confirm cleanup (`cleanedUp: false`, or a thrown ProbeCleanupError) fails closed with
 * EnvProbeCleanupError.
 */
export async function runEnvProbe(
  spawner: EnvProbeSpawner,
  env: NodeJS.ProcessEnv,
  opts: { harness: "claude" | "codex"; dockerWired: boolean; timeoutMs?: number },
): Promise<EnvFacts> {
  const { harness, dockerWired } = opts;
  const timeoutMs = opts.timeoutMs ?? 2000;
  const unverified: EnvFacts = { harness, dockerWired, proc: "unverified", home: "unverified", tmp: "unverified" };
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), timeoutMs);
  let result: { code: number; stdout: string; cleanedUp: boolean };
  try {
    // Awaited to settlement: the spawner owns reaping, so it is never raced and abandoned.
    result = await spawner(ENV_PROBE_ARGV, probeEnv(env), ac.signal);
  } catch (err) {
    if (err instanceof ProbeCleanupError) throw new EnvProbeCleanupError(err.message);
    return unverified;
  } finally {
    clearTimeout(timer);
  }
  if (result.cleanedUp !== true) throw new EnvProbeCleanupError();
  if (ac.signal.aborted || result.code !== 0 || typeof result.stdout !== "string") return unverified;
  const facts = parseProbeLine(result.stdout);
  if (!facts) return unverified;
  return { harness, dockerWired, ...facts };
}

interface RunnerProbeDeps {
  spawn?: (
    command: string,
    args: readonly string[],
    opts: { cwd?: string; env?: NodeJS.ProcessEnv; detached?: boolean; stdio?: SpawnOptions["stdio"] },
  ) => ChildProcess;
  killGroup?: (pid: number | undefined) => boolean;
  groupPresent?: (pgid: number) => boolean | undefined;
  /** How long to wait for `close` after an abort or overflow kill before giving up on it. */
  closeWaitMs?: number;
  /** How long to keep re-checking that the group is gone after a follow-up kill. */
  settleMs?: number;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * The Claude-harness spawner: runs the probe as the runner uid (runnerSpawn) in its own process
 * group with the SDK env. This is a useful baseline for what the lead's commands can do, NOT
 * provably identical to an SDK Bash tool call (the SDK may add its own sandboxing or env).
 *
 * Always resolves. `cleanedUp` is true only when processGroupPresent confirms the group absent
 * (`false`), or the child never spawned; `true` (present) and `undefined` (unknown) fail closed.
 */
export function spawnRunnerProbe(opts: { cwd: string; deps?: RunnerProbeDeps }): EnvProbeSpawner {
  const deps = opts.deps ?? {};
  const spawnFn = deps.spawn ?? runnerSpawn;
  // Group-only (issue #1656): never fall back to the bare pid, which after `close` may name an
  // unrelated, recycled process.
  const killGroup = deps.killGroup ?? ((pid: number | undefined) => pid !== undefined && killRunnerGroupOnly(pid));
  const groupPresent = deps.groupPresent ?? ((pgid: number) => processGroupPresent(pgid));
  const closeWaitMs = deps.closeWaitMs ?? 2000;
  const settleMs = deps.settleMs ?? 200;

  return (argv, env, signal) =>
    new Promise((resolve) => {
      if (signal.aborted) {
        resolve({ code: -1, stdout: "", cleanedUp: true });
        return;
      }
      const [command, ...args] = argv;
      if (command === undefined) {
        resolve({ code: -1, stdout: "", cleanedUp: true });
        return;
      }
      let child: ChildProcess;
      try {
        child = spawnFn(command, args, { cwd: opts.cwd, env, detached: true, stdio: ["ignore", "pipe", "pipe"] });
      } catch {
        // Never spawned: nothing to clean up.
        resolve({ code: -1, stdout: "", cleanedUp: true });
        return;
      }
      const pid = child.pid;
      const chunks: Buffer[] = [];
      let kept = 0;
      let killed = false;
      let settled = false;
      let closeTimer: NodeJS.Timeout | undefined;

      const kill = (): void => {
        if (killed) return;
        killed = true;
        if (child.pid !== undefined) killGroup(child.pid);
        // Bound the wait for `close` once we have asked the group to die.
        closeTimer = setTimeout(() => void finish(-1), closeWaitMs);
      };
      const onAbort = (): void => kill();
      signal.addEventListener("abort", onAbort, { once: true });

      child.stdout?.on("data", (chunk: Buffer) => {
        if (kept > MAX_PROBE_STDOUT_BYTES) return;
        chunks.push(chunk);
        kept += chunk.length;
        // One byte past the cap is enough for runEnvProbe to reject it as oversize.
        if (kept > MAX_PROBE_STDOUT_BYTES) kill();
      });
      child.stderr?.resume();

      const finish = async (code: number): Promise<void> => {
        if (settled) return;
        settled = true;
        if (closeTimer) clearTimeout(closeTimer);
        signal.removeEventListener("abort", onAbort);
        const stdout = Buffer.concat(chunks).subarray(0, MAX_PROBE_STDOUT_BYTES + 1).toString("utf8");
        const exitCode = killed || signal.aborted ? (code === 0 ? -1 : code) : code;
        const target = pid ?? child.pid;
        if (target === undefined) {
          resolve({ code: exitCode === 0 ? -1 : exitCode, stdout, cleanedUp: true });
          return;
        }
        let present = groupPresent(target);
        if (present !== false) {
          // One follow-up kill, then a short bounded re-check (a killed member may not be reaped yet).
          killGroup(target);
          const deadline = Date.now() + settleMs;
          while (present !== false && Date.now() < deadline) {
            await sleep(20);
            present = groupPresent(target);
          }
        }
        resolve({ code: exitCode, stdout, cleanedUp: present === false });
      };

      child.on("error", () => {
        // A spawn failure leaves no pid; any other error is followed by `close`.
        if (child.pid === undefined) void finish(-1);
      });
      child.on("close", (code) => void finish(code ?? -1));
    });
}

/** TEST ONLY: see {@link setDefaultEnvProbeSpawnerForTests}. */
let testDefaultSpawner: ((cwd: string) => EnvProbeSpawner) | undefined;

/** TEST ONLY: install (or, with undefined, clear) the spawner factory {@link defaultEnvProbeSpawner}
 *  returns, so the test preload keeps executor rigs that inject no spawner from starting a real
 *  probe process per run. No environment variable or config reads one; production code never
 *  calls this setter. */
export function setDefaultEnvProbeSpawnerForTests(factory: ((cwd: string) => EnvProbeSpawner) | undefined): void {
  testDefaultSpawner = factory;
}

/** The Claude harness's default probe spawner for a run rooted at `cwd`: {@link spawnRunnerProbe},
 *  unless a test installed a factory with {@link setDefaultEnvProbeSpawnerForTests}. */
export function defaultEnvProbeSpawner(cwd: string): EnvProbeSpawner {
  return testDefaultSpawner ? testDefaultSpawner(cwd) : spawnRunnerProbe({ cwd });
}
