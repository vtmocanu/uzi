// Opt-in Issue #1716 worker-image fixture: a Codex command (runner-cmd 10003, supervisor +
// command sandbox) runs git in the runner-owned (10002) checkout. Every check runs and prints
// PASS/FAIL, so a base-code run (no withCommandGitTrust, labelled UNFIXED) shows which fail.
import assert from "node:assert/strict";
import { execFile, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { promisify } from "node:util";
import { pathToFileURL } from "node:url";
import type { Readable } from "node:stream";

const src = process.env.CODEX_GIT_TRUST_SRC ?? "/app/src";
const load = async (name: string) => import(pathToFileURL(path.join(src, name)).href);
const exec = promisify(execFile);
const root = `/data/runner/codex-git-trust-${process.pid}`;
const checkoutA = path.join(root, "worktree");
const nestedB = path.join(checkoutA, "nested-b");
const DUBIOUS = "detected dubious ownership";

type Spawn = (argv: string[], opts: { cwd?: string; env?: NodeJS.ProcessEnv }) =>
  Promise<{ code: number; stdout: string; stderr: string }>;
type Wrap = (command: string, args: readonly string[]) => { command: string; args: string[] };

const failures: string[] = [];

function check(label: string, ok: boolean, detail: string): void {
  if (ok) {
    console.log(`PASS: ${label}`);
  } else {
    const lines = detail.split("\n");
    console.log(`FAIL: ${label}: ${lines.slice(0, 3).join(" | ")}${lines.length > 3 ? " | ..." : ""}`);
    failures.push(label);
  }
}

/** Run `argv` as the worker-owned runner identity (10002), the checkout's owner. */
async function asRunner(wrap: Wrap, argv: string[]): Promise<string> {
  const [cmd, ...rest] = argv;
  const w = wrap(cmd!, rest);
  return (await exec(w.command, w.args, { cwd: root, env: { PATH: "/usr/bin:/bin" } })).stdout;
}

/** The first line of `text` naming the dubious-ownership rejection, for the log. */
function dubiousLine(text: string): string {
  return text.split("\n").find((line) => line.includes(DUBIOUS)) ?? "(none)";
}

/**
 * issue #1769 m3 part 2 — the finalize import (`GitCache.ensureRunnerCloneObjects`, the m2
 * agent's boundary-machinery API for pulling in whatever the run's finalize/push path still
 * needs from the bare) under the REAL boundary machinery (a real `ExecutionRegistry` +
 * `createCodexExecutionSafety` + `boundaryProcessSpawnerForTest("required")` — the same seam
 * the runner itself uses, per `codex-executor.ts`'s doc comment on that test seam).
 *
 * Gated on FIXED (the mounted src exports both `boundaryProcessSpawnerForTest` and
 * `ensureRunnerCloneObjects`) AND `probe.status === 0` (needs real Landlock: the whole point
 * is proving the import works with the bare NOT granted to the sandboxed command).
 *
 * Three cases, each run inside its own `finalize` boundary:
 *   (a) SUCCESS   — a fresh default-branch commit is imported and merged in.
 *   (b) PRODUCER  — a well-formed but bare-absent tip makes `pack-objects` fail.
 *   (c) CONSUMER  — the clone's `.git/objects/pack` is made unwritable, so `index-pack` fails.
 * Before (a), a calibration check proves the process scan can see a sandboxed command-identity
 * process at all. After each case, `producer and consumer reaped` is a backstop: an OS-level
 * observation that no git pack process (zombies included) survives the boundary's reap (see
 * `proveReaped`; git.ts's own teardown is pinned by agent/test/git-import.test.ts), then two
 * invariants: no live `command`-kind root remains (the guard
 * `spawnBoundaryProcess`'s `worker_pat` ordering relies on), and a fresh `worker_pat` boundary
 * action still succeeds — proof the registry was left usable, not wedged by the prior case.
 *
 * A fourth section is an OBSERVATION, not an assertion: it tries to hold a model `command`
 * root live while a `worker_pat` `spawnBoundaryProcess` runs (which the guard would refuse with
 * "command roots live"), and logs what happened. The boundary reaps that root before the
 * action runs, so the refusal is not reachable through the public API (see the body).
 */
async function runFinalizeImportPart(
  gitMod: Record<string, any>,
  gitCache: Record<string, any>,
  codex: Record<string, any>,
  launcher: Record<string, any>,
  registryMod: Record<string, any>,
  safetyMod: Record<string, any>,
  runner: Record<string, any>,
  bare: string,
  clonePath: string,
  branch: string,
  headTip: string,
  baseCommit: string,
  originPath: string,
  scCommandEnv: NodeJS.ProcessEnv,
): Promise<void> {
  let epoch = 190;
  const newRegistry = () => new registryMod.ExecutionRegistry(registryMod.newLocalExecutionEpoch(epoch++));
  const newSafety = (registry: Record<string, any>) =>
    safetyMod.createCodexExecutionSafety(
      registry,
      async () => { throw new Error("unused: spawnRoot is not exercised by the boundary-process path"); },
      undefined,
      undefined,
      codex.boundaryProcessSpawnerForTest("required"),
    );

  // A plain (non-boundary) sandboxed command, for the pre-import "tip is unresolvable" check.
  const sandboxRun = async (script: string): Promise<{ code: number; stdout: string; stderr: string }> => {
    const roots = newRegistry();
    const spawn: Spawn = codex.makeDefaultSpawnCommand(roots, launcher.launchCodexEffectRoot, 5000, clonePath, scCommandEnv, "required");
    return spawn(["/bin/sh", "-c", script], { cwd: clonePath });
  };

  /** Proves the registry is left usable: no live `command` root, and a fresh `worker_pat`
   *  boundary action (a plain read of the bare) still succeeds. */
  const proveClean = async (tag: string, registry: Record<string, any>, safety: Record<string, any>): Promise<void> => {
    check(`${tag}: no live command-kind root`, registry.hasLiveCommandRoot() === false, "a command root is still live");
    const result = await safety.withBoundary({ boundary: "finalize", deadlineMs: 30_000 }, async (permit: any) => {
      const proc = await safety.spawnBoundaryProcess(permit, {
        identity: "worker_pat",
        argv: ["/usr/bin/git", "-C", bare, "rev-parse", "HEAD"],
        cwd: bare,
        env: { PATH: "/usr/bin:/bin" },
      });
      const out = await drain(proc.stdout as Readable);
      const term = await proc.completed;
      return { code: term.code, stdout: out.toString("utf8").trim() };
    });
    check(`${tag}: a subsequent worker_pat boundary action succeeds`, result.code === 0 && /^[0-9a-f]{40}$/.test(result.stdout),
      `code=${result.code} stdout=${JSON.stringify(result.stdout)}`);
  };

  // ── CALIBRATION: the process scan can see a command-identity process ────────────────────
  // The reaped checks below pass on an EMPTY scan, so an empty scan must first be shown to
  // mean something: a scanner that cannot see runner-cmd's (uid 10003) processes from the
  // fixture's uid would pass them vacuously. Spawn a uniquely-named sleep through the same
  // sandboxed command path `sandboxRun` uses, and require the scanner to see it as uid 10003.
  const calibrationArg = "3.1769";
  const isCalibrationSleep = (e: ProcEntry): boolean => e.comm === "sleep" && e.cmdline.includes(`sleep ${calibrationArg}`);
  const calibrationSpawn: Spawn = codex.makeDefaultSpawnCommand(newRegistry(), launcher.launchCodexEffectRoot, 5000, clonePath, scCommandEnv, "required");
  const calibrationRun = calibrationSpawn(["/bin/sleep", calibrationArg], { cwd: clonePath });
  calibrationRun.catch(() => undefined); // awaited below; this only keeps a rejection from going unhandled while polling
  const seenAsCommand = (s: ProcScan): boolean => s.found.some((e) => e.uid === COMMAND_UID);
  const calibrationScan = await pollProcesses(isCalibrationSleep, 2000, seenAsCommand);
  const calibrationResult = await calibrationRun.then(
    (r) => `code=${r.code} stderr=${r.stderr.trim() || "(empty)"}`,
    (err: unknown) => `threw ${err instanceof Error ? err.message : String(err)}`,
  );
  const calibrated = seenAsCommand(calibrationScan);
  check("FINALIZE-IMPORT: process scan sees a sandboxed command-identity process (calibration)", calibrated,
    `wanted comm=(sleep) cmdline~"sleep ${calibrationArg}" uid=${COMMAND_UID}; `
    + `found=${calibrationScan.found.map(formatProc).join("; ") || "(none)"} `
    + `unreadable=${calibrationScan.unreadable.join("; ") || "(none)"} spawn: ${calibrationResult}`);

  /** `${tag}: producer and consumer reaped` is a BACKSTOP: an OS-level observation, independent
   *  of the execution registry, that no git pack process (`pack-objects` / `index-pack`, zombies
   *  included, see {@link isGitPackProcess}) survives the boundary's reap. It is only as good as
   *  the scan, which is why the calibration check above must pass first, and why an unreadable
   *  pid fails it instead of counting as gone. git.ts's own teardown (both exit statuses awaited
   *  before `ensureRunnerCloneObjects` settles) is pinned by agent/test/git-import.test.ts, not
   *  here. `importSettled` and `hasLiveCommandRoot()` are also required but are corroborating,
   *  not independent: both are the machinery under test reporting on itself. */
  const proveReaped = async (tag: string, registry: Record<string, any>, importSettled: boolean): Promise<void> => {
    const liveCommand = registry.hasLiveCommandRoot() as boolean;
    const scan = await pollNoGitPackProcesses(2000);
    check(`${tag}: producer and consumer reaped`,
      importSettled && !liveCommand && scan.found.length === 0 && scan.unreadable.length === 0,
      `${calibrated ? "" : "calibration FAILED, so an empty scan proves nothing; "}`
      + `leftover=${scan.found.map(formatProc).join("; ") || "(none)"} `
      + `unreadable=${scan.unreadable.join("; ") || "(none)"} `
      + `(corroborating: importSettled=${importSettled} hasLiveCommandRoot=${liveCommand})`);
  };

  // ── (a) SUCCESS ────────────────────────────────────────────────────────────────────────
  await fs.writeFile(path.join(originPath, "finalize-a.txt"), "finalize-a\n");
  await plainGit(originPath, ["add", "finalize-a.txt"]);
  await plainGit(originPath, ["commit", "-q", "-m", "finalize import case a"]);
  const newTipA = await plainGit(originPath, ["rev-parse", "HEAD"]);

  const preImport = await sandboxRun(`git cat-file -e ${newTipA}`);
  check("FINALIZE-IMPORT (a): sandboxed cat-file -e newTip FAILS before import",
    preImport.code !== 0, `code=${preImport.code} stderr=${preImport.stderr.trim()}`);

  const fetchedTipA = await gitCache.fetchDefaultTip(bare, "main");
  assert.equal(fetchedTipA, newTipA, "fetchDefaultTip returned the newly advanced default-branch tip");

  const registryA = newRegistry();
  const safetyA = newSafety(registryA);
  let settledA = false;
  await safetyA.withBoundary({ boundary: "finalize", deadlineMs: 60_000 }, (permit: any) =>
    gitCache.withBoundaryProcessSpawner(
      (p: unknown) => safetyA.spawnBoundaryProcess(permit, p),
      permit.signal,
      async () => {
        await gitCache.ensureRunnerCloneObjects(bare, clonePath, newTipA, [baseCommit]);
        settledA = true;
        const aligned = await gitCache.alignBranchWithDefault(clonePath, branch, headTip, newTipA, "merge");
        check("FINALIZE-IMPORT (a): alignBranchWithDefault returns \"aligned\"", aligned === "aligned", `result=${aligned}`);
      },
    ));
  const postImport = await sandboxRun(`git cat-file -e ${newTipA}`);
  check("FINALIZE-IMPORT (a): sandboxed cat-file -e newTip succeeds after import",
    postImport.code === 0, `code=${postImport.code} stderr=${postImport.stderr.trim()}`);
  await proveReaped("FINALIZE-IMPORT (a)", registryA, settledA);
  await proveClean("FINALIZE-IMPORT (a)", registryA, safetyA);

  // ── (b) PRODUCER failure: a well-formed 40-hex tip absent from the bare ─────────────────
  const absentTip = createHashLikeSha("finalize-import-absent-b");
  const registryB = newRegistry();
  const safetyB = newSafety(registryB);
  let threwB: unknown;
  let settledB = false;
  try {
    await safetyB.withBoundary({ boundary: "finalize", deadlineMs: 60_000 }, (permit: any) =>
      gitCache.withBoundaryProcessSpawner(
        (p: unknown) => safetyB.spawnBoundaryProcess(permit, p),
        permit.signal,
        async () => {
          try {
            await gitCache.ensureRunnerCloneObjects(bare, clonePath, absentTip, [baseCommit]);
          } finally {
            settledB = true;
          }
        },
      ));
  } catch (err) {
    threwB = err;
  }
  check("FINALIZE-IMPORT (b): a bare-absent tip throws RunnerCloneImportError",
    threwB instanceof gitMod.RunnerCloneImportError, `threw=${threwB instanceof Error ? threwB.constructor.name : typeof threwB}: ${(threwB as Error)?.message}`);
  await proveReaped("FINALIZE-IMPORT (b)", registryB, settledB);
  await proveClean("FINALIZE-IMPORT (b)", registryB, safetyB);

  // ── (c) CONSUMER failure: the clone's objects/pack is unwritable as the runner ──────────
  await fs.writeFile(path.join(originPath, "finalize-c.txt"), "finalize-c\n");
  await plainGit(originPath, ["add", "finalize-c.txt"]);
  await plainGit(originPath, ["commit", "-q", "-m", "finalize import case c"]);
  const newTipC = await plainGit(originPath, ["rev-parse", "HEAD"]);
  const fetchedTipC = await gitCache.fetchDefaultTip(bare, "main");
  assert.equal(fetchedTipC, newTipC, "fetchDefaultTip returned case c's advanced tip");

  const packDir = path.join(clonePath, ".git", "objects", "pack");
  const chmodRO = runner.runnerCommand("chmod", ["0555", packDir]);
  await exec(chmodRO.command, chmodRO.args, { env: { PATH: "/usr/bin:/bin" } });
  let threwC: unknown;
  try {
    const registryC = newRegistry();
    const safetyC = newSafety(registryC);
    let settledC = false;
    try {
      await safetyC.withBoundary({ boundary: "finalize", deadlineMs: 60_000 }, (permit: any) =>
        gitCache.withBoundaryProcessSpawner(
          (p: unknown) => safetyC.spawnBoundaryProcess(permit, p),
          permit.signal,
          async () => {
            try {
              await gitCache.ensureRunnerCloneObjects(bare, clonePath, newTipC, [baseCommit, newTipA]);
            } finally {
              settledC = true;
            }
          },
        ));
    } catch (err) {
      threwC = err;
    }
    check("FINALIZE-IMPORT (c): an unwritable clone objects/pack throws RunnerCloneImportError",
      threwC instanceof gitMod.RunnerCloneImportError, `threw=${threwC instanceof Error ? threwC.constructor.name : typeof threwC}: ${(threwC as Error)?.message}`);
    await proveReaped("FINALIZE-IMPORT (c)", registryC, settledC);
    await proveClean("FINALIZE-IMPORT (c)", registryC, safetyC);
  } finally {
    const chmodRW = runner.runnerCommand("chmod", ["0755", packDir]);
    await exec(chmodRW.command, chmodRW.args, { env: { PATH: "/usr/bin:/bin" } });
  }

  // ── GUARD INTACT: NOT constructible through the current public API — see below ──────────
  //
  // The plan asked for: spawn a long `sleep` as a live `command`-kind root (via
  // `makeDefaultSpawnCommand`, the SAME registry a finalize boundary will use), then prove a
  // `worker_pat` `spawnBoundaryProcess` is refused ("command roots live") while it is still
  // live. This was attempted exactly as specified and DOES NOT hold, for an architectural
  // reason (not a fixture bug), confirmed empirically below:
  //
  //   `command`-kind roots may only be RESERVED while `ExecutionRegistry.state() === "open"`
  //   (`reserveLaunch`). `withBoundary`'s `runBoundary` always reaps EVERY registered root,
  //   `command`-kind included (`reapProcesses` iterates `this.roots` with no kind filter),
  //   BEFORE minting the permit and invoking the boundary's action. So by the time an action
  //   body can call `spawnBoundaryProcess`, either (a) the live command root's `reap()` (which
  //   forcibly disposes/kills the sandboxed process) already completed and marked it
  //   `reaped = true` — `hasLiveCommandRoot()` is then false and the `worker_pat` call is
  //   correctly admitted, or (b) that reap failed/timed out and `withBoundary` itself throws
  //   before the action ever runs (a `CodexBoundaryError`, not the `[R3-2]`
  //   "command roots live" refusal). There is no public sequencing that leaves a `command`
  //   root both LIVE and the boundary OPEN at the same time: attempting it here (a `sleep 20`
  //   spawned just before `withBoundary`, same registry, exactly as specified) hit case (a) —
  //   the sleep was reaped (killed) during quiesce+reap, `hasLiveCommandRoot()` read false
  //   inside the action, and the `worker_pat` action was admitted cleanly. The `[R3-2]` guard
  //   in `codex/safety.ts` (`spawnBoundaryProcess`'s `hasLiveCommandRoot()` check) is real
  //   defense-in-depth for a reap-seam that falsely reports clean; it is not reachable from a
  //   real, successfully-completing boundary using only the documented public surface
  //   (`ExecutionRegistry`, `createCodexExecutionSafety`, `boundaryProcessSpawnerForTest`,
  //   `makeDefaultSpawnCommand`). Exercising it would need either a fake `spawnProcess`/
  //   `spawnRoot` seam that lies about reap (not "the real boundary machinery" this part is
  //   scoped to) or a new test-only registry seam to hold a root live past reap — neither
  //   exists today.
  //
  // Left running below as an OBSERVATION (not a PASS/FAIL check) so a future public-API
  // addition that changes this is visible in the log without silently flipping this section
  // green or red.
  const guardRegistry = newRegistry();
  const guardSafety = newSafety(guardRegistry);
  const commandSpawn: Spawn = codex.makeDefaultSpawnCommand(guardRegistry, launcher.launchCodexEffectRoot, 60_000, clonePath, scCommandEnv, "required");
  const sleeping = commandSpawn(["/bin/sh", "-c", "sleep 20"], { cwd: clonePath });
  sleeping.catch(() => undefined);
  let guardThrew: unknown;
  let guardHadLiveCommandRootInAction = false;
  try {
    await guardSafety.withBoundary({ boundary: "finalize", deadlineMs: 30_000 }, async (permit: any) => {
      guardHadLiveCommandRootInAction = guardRegistry.hasLiveCommandRoot();
      await guardSafety.spawnBoundaryProcess(permit, {
        identity: "worker_pat",
        argv: ["/usr/bin/git", "-C", bare, "rev-parse", "HEAD"],
        cwd: bare,
        env: { PATH: "/usr/bin:/bin" },
      });
    });
  } catch (err) {
    guardThrew = err;
  }
  const guardMessage = guardThrew instanceof Error ? guardThrew.message : String(guardThrew);
  console.log(`OBSERVATION: GUARD INTACT (not asserted): hasLiveCommandRoot() inside the action=${guardHadLiveCommandRootInAction} `
    + `worker_pat threw=${guardThrew !== undefined} (${guardMessage})`);
  // The boundary reaped (disposed) that command root, and its spawn promise is not guaranteed to
  // settle afterwards. Awaiting it unbounded let the event loop drain with main() still pending,
  // so node exited 0 before the rest of the fixture ran (issue #1769 acceptance). Bound it.
  const sleepSettled = await Promise.race([
    sleeping.then(() => true, () => true),
    // Deliberately NOT unref()'d: this timer is what keeps the event loop alive while waiting.
    new Promise<boolean>((resolve) => { setTimeout(() => resolve(false), 25_000); }),
  ]);
  console.log(`OBSERVATION: GUARD INTACT: the reaped command root's spawn promise settled=${sleepSettled}`);
}

/** A deterministic, well-formed-looking 40-hex object id that is guaranteed absent from the
 *  fixture's own stand-in bare (it is never written by any git command here). */
function createHashLikeSha(seed: string): string {
  return createHash("sha1").update(seed).digest("hex");
}

/** One process seen by {@link scanProcesses}: its pid, `comm` (from `stat`, without the
 *  parentheses), state letter (`Z` = zombie), cmdline (NUL separators shown as spaces; empty for a
 *  zombie or kernel thread) and real uid (from `status`; `?` when it could not be read). */
interface ProcEntry { pid: string; comm: string; state: string; cmdline: string; uid: string }

/** A scan's result: the matching processes, plus every pid whose `cmdline` or `stat` could not
 *  be read for a reason other than the pid having exited. */
interface ProcScan { found: ProcEntry[]; unreadable: string[] }

const PROC_ROOT = "/proc";
/** The Codex command identity's uid (runner-cmd). */
const COMMAND_UID = "10003";

function formatProc(e: ProcEntry): string {
  return `${e.pid}: state=${e.state} comm=(${e.comm}) uid=${e.uid} cmdline=${e.cmdline || "(empty)"}`;
}

/** Reads `<procfs>/<pid>/<file>`. Returns `"gone"` when the pid exited between the readdir and
 *  the read (ENOENT, ESRCH), and `{ error }` for any other failure (EACCES included): an
 *  unreadable pid is NOT evidence that it is gone, so the caller reports it instead of skipping. */
async function readProcFile(pid: string, file: string): Promise<string | "gone" | { error: string }> {
  try {
    return (await fs.readFile(path.join(PROC_ROOT, pid, file))).toString("utf8");
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "ESRCH") return "gone";
    return { error: `${pid}: ${file}: ${code ?? String(err)}` };
  }
}

/** Every process in the container (other than this fixture) for which `match` holds. Each pid's
 *  `cmdline` AND `stat` are read, so a zombie (empty cmdline, still in the process table) is seen
 *  by its `comm` and state. A pid that exited mid-scan (ENOENT, ESRCH) is skipped; one that is
 *  unreadable (EACCES or any other error) lands in `unreadable`, which callers treat as a failed
 *  scan rather than as "gone". */
async function scanProcesses(match: (e: ProcEntry) => boolean): Promise<ProcScan> {
  const found: ProcEntry[] = [];
  const unreadable: string[] = [];
  for (const pid of await fs.readdir(PROC_ROOT)) {
    if (!/^[0-9]+$/.test(pid) || Number(pid) === process.pid) continue;
    const cmdlineRaw = await readProcFile(pid, "cmdline");
    if (cmdlineRaw === "gone") continue;
    const statRaw = await readProcFile(pid, "stat");
    if (statRaw === "gone") continue;
    if (typeof cmdlineRaw !== "string" || typeof statRaw !== "string") {
      for (const r of [cmdlineRaw, statRaw]) if (typeof r !== "string") unreadable.push(r.error);
      continue;
    }
    // stat is `pid (comm) state ...`; comm may itself contain parentheses, so split on the LAST ")".
    const open = statRaw.indexOf("(");
    const close = statRaw.lastIndexOf(")");
    if (open < 0 || close < open) {
      unreadable.push(`${pid}: stat: unparseable ${JSON.stringify(statRaw.slice(0, 80))}`);
      continue;
    }
    const entry: ProcEntry = {
      pid,
      comm: statRaw.slice(open + 1, close),
      state: statRaw.slice(close + 1).trim().split(/\s+/)[0] || "?",
      cmdline: cmdlineRaw.split("\0").join(" ").trim(),
      uid: "?",
    };
    if (!match(entry)) continue;
    const status = await readProcFile(pid, "status");
    if (typeof status === "string") entry.uid = /^Uid:\s+(\d+)/m.exec(status)?.[1] ?? "?";
    found.push(entry);
  }
  return { found, unreadable };
}

/** A git pack process: `comm` is `git` (how `git pack-objects` / `git index-pack` appear, and the
 *  only trace a zombie leaves, its cmdline being empty) or a dashed `git-*` helper, or the cmdline
 *  names `pack-objects` / `index-pack`. Present regardless of state: a zombie counts. */
function isGitPackProcess(e: ProcEntry): boolean {
  return e.comm === "git" || e.comm.startsWith("git-") || /pack-objects|index-pack/.test(e.cmdline);
}

/** Polls {@link scanProcesses} every 100ms until `done(scan)` holds or `budgetMs` has elapsed,
 *  and returns the last scan. */
async function pollProcesses(
  match: (e: ProcEntry) => boolean,
  budgetMs: number,
  done: (s: ProcScan) => boolean,
): Promise<ProcScan> {
  const deadline = Date.now() + budgetMs;
  for (;;) {
    const scan = await scanProcesses(match);
    if (done(scan) || Date.now() >= deadline) return scan;
    await sleep(100);
  }
}

/** Polls until no git pack process (zombies included) AND no unreadable pid remain, for up to
 *  `budgetMs`, giving a just-killed process time to be reaped before the check fails. */
async function pollNoGitPackProcesses(budgetMs: number): Promise<ProcScan> {
  return pollProcesses(isGitPackProcess, budgetMs, (s) => s.found.length === 0 && s.unreadable.length === 0);
}

/** Drains a packfile stream (checkpointPack's `pack`) to a Buffer for `git index-pack --stdin`. */
async function drain(r: Readable): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const c of r) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

/** Plain git helper for the fixture's own stand-in "forge" repo: not part of the boundary under
 *  test, so it runs as whatever uid the fixture process already is (worker, 10001). */
async function plainGit(dir: string, args: string[]): Promise<string> {
  const identity = ["-c", "user.email=fixture@example.com", "-c", "user.name=fixture", "-c", "commit.gpgsign=false"];
  const { stdout } = await exec("git", ["-C", dir, ...identity, ...args], { env: { PATH: "/usr/bin:/bin" } });
  return stdout.trim();
}

/**
 * issue #1769 m3 part 1 — the shared-clone section, with its OWN FIXED/UNFIXED label (whether
 * the mounted src's git.js exports `RunnerCloneMaterializationError`; base commit 3989078e does
 * not). Builds a REALISTIC owner-matched DIVERGED wip(park) checkpoint through the REAL
 * `GitCache` — the exact shape `agent/test/git-materialize.test.ts`'s "owner-matched diverged
 * checkpoint leg" test drives — seeds the tested runner clone with `{ selfContained: true }`,
 * then proves under the real Codex command sandbox that the clone works standalone (FIXED) or
 * fails reading the bare's borrowed objects (UNFIXED, the pre-m1 `--shared` clone).
 */
async function runSharedCloneSection(
  codex: Record<string, any>,
  launcher: Record<string, any>,
  registry: Record<string, any>,
  runner: Record<string, any>,
  probe: { status: number | null },
): Promise<void> {
  const git = await load("git.js");
  const logMod = await load("log.js");
  const logger = logMod.createLogger("error");

  // Replicate the entrypoint's runner-writable carve-out (agent/templates/entrypoint.sh, "PRD
  // #51 M4: runner-owned /data subtree carve-out"): worker:runner, setgid + sticky, so the
  // runner-uid clone/checkout GitCache performs below can create its own per-run dir under it.
  const runnerRoot = "/data/runner";
  await fs.mkdir(runnerRoot, { recursive: true });
  await fs.chown(runnerRoot, 10001, 10002);
  await fs.chmod(runnerRoot, 0o3775);

  const gitCache = new git.GitCache("/data", logger);
  const scFixed = typeof git.RunnerCloneMaterializationError === "function";
  const scLabel = scFixed ? "FIXED" : "UNFIXED";
  console.log(`SHARED-CLONE MODE: ${scLabel} (src=${src})`);
  // Captured here (before any check() call in this section) so the per-section RESULT
  // line below (issue #1769 m3 review) covers every check this whole function runs,
  // including the finalize-import part.
  const scFailuresBefore = failures.length;

  // 1. The stand-in "forge": a default branch with a couple of commits.
  const originPath = path.join(root, "shared-origin");
  await fs.mkdir(originPath, { recursive: true });
  await plainGit(originPath, ["init", "-q", "-b", "main"]);
  await fs.writeFile(path.join(originPath, "README.md"), "one\n");
  await plainGit(originPath, ["add", "README.md"]);
  await plainGit(originPath, ["commit", "-q", "-m", "commit 1"]);
  await fs.writeFile(path.join(originPath, "second.txt"), "two\n");
  await plainGit(originPath, ["add", "second.txt"]);
  await plainGit(originPath, ["commit", "-q", "-m", "commit 2"]);
  const floor = await plainGit(originPath, ["rev-parse", "HEAD"]);

  // 2. The worker's bare, exactly like production (GitCache.reposRoot = <dataDir>/repos).
  const bare = await gitCache.ensureClone(originPath);

  // 3. A wip(park) marker over the pre-advance floor, published as THIS run's own checkpoint
  //    (refs/uzi-checkpoints/<branch>) — the git-materialize.test.ts "owner-matched diverged
  //    checkpoint leg" template, driven through the real GitCache.
  const branch = "agent/issue-shared-clone";
  const seedRunId = "run-shared-a";
  const seed = await gitCache.runnerCloneForBranch(bare, branch, "seed", seedRunId, false);
  await fs.writeFile(path.join(seed.path, "WIP.txt"), "diverged wip\n");
  const marked = await gitCache.commitWipMarker(seed.path);
  assert.equal(marked, true, "the wip(park) marker committed");
  // `seed.path` is RUNNER-owned (GitCache clones/checks out as runner uid), so reading it as the
  // worker-uid fixture process trips git's dubious-ownership check; read it as the runner.
  const markerWrapped = runner.runnerCommand("git", ["-C", seed.path, "rev-parse", "HEAD"]);
  const marker = (await exec(markerWrapped.command, markerWrapped.args, { env: { PATH: "/usr/bin:/bin" } })).stdout.trim();
  await gitCache.fetchAgentBranch(bare, seed.path, branch, seedRunId);
  const packed = await gitCache.checkpointPack(bare, branch);
  if (!packed) throw new Error("checkpointPack returned null — nothing to publish");
  const packBuf = await drain(packed.pack);
  const indexPack = spawnSync("git", ["-C", originPath, "index-pack", "--stdin", "--fix-thin"], {
    input: packBuf,
    env: { PATH: "/usr/bin:/bin" },
  });
  if (indexPack.status !== 0) throw new Error(`index-pack failed: ${indexPack.stderr?.toString()}`);
  await plainGit(originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, marker]);

  // 4. Advance the origin default branch PAST the checkpoint's parent: the checkpoint marker
  //    now DIVERGES from the new tip (its own parent, `floor`, stays an ancestor of it).
  await fs.writeFile(path.join(originPath, "advance.txt"), "advance\n");
  await plainGit(originPath, ["add", "advance.txt"]);
  await plainGit(originPath, ["commit", "-q", "-m", "advance default"]);
  const advancedTip = await plainGit(originPath, ["rev-parse", "HEAD"]);
  assert.notEqual(advancedTip, floor, "the default branch actually advanced");

  // Refresh the bare: pulls the advanced default AND mirrors refs/uzi-checkpoints/* (fetch()).
  await gitCache.ensureClone(originPath);

  // 5. THE SEED UNDER TEST — a fresh run (a DIFFERENT runId, so NOT ownedHere), owner-matched
  //    via `expectedCheckpointTip`, `resume: false`. `selfContained: true` is always passed: the
  //    base src (no such param) silently ignores it (old `--shared` clone, UNFIXED), the mounted
  //    src (m1) materializes it (FIXED).
  const finalRunId = "run-shared-b";
  const rc = await gitCache.runnerCloneForBranch(bare, branch, "final", finalRunId, false, marker, { selfContained: true });
  check(`SHARED-CLONE ${scLabel}: checkpoint was diverged + owner-matched (wipRecovered)`, rc.wipRecovered === true,
    `wipRecovered=${rc.wipRecovered} seededFrom=${rc.seededFrom} checkpointSetAside=${rc.checkpointSetAside}`);
  check(`SHARED-CLONE ${scLabel}: base is the advanced default tip (checkpoint set aside, not adopted)`, rc.baseCommit === advancedTip,
    `baseCommit=${rc.baseCommit} advancedTip=${advancedTip}`);

  // 6. Calibration: DAC allows reading the bare and a SIBLING runner clone from OUTSIDE the
  //    sandbox (runner-cmd, plain setpriv — no Codex supervisor/command sandbox), so a denial
  //    inside the sandbox below is Landlock, never a permissions mistake.
  const asCommandRoot = async (argv: string[], cwd: string): Promise<{ code: number; stdout: string; stderr: string }> => {
    const [cmd, ...rest] = argv;
    const w = runner.commandRootCommand(cmd!, rest);
    try {
      const { stdout } = await exec(w.command, w.args, { cwd, env: { PATH: "/usr/bin:/bin" } });
      return { code: 0, stdout, stderr: "" };
    } catch (err) {
      const e = err as { code?: number; stdout?: string; stderr?: string };
      return { code: e.code ?? 1, stdout: e.stdout ?? "", stderr: e.stderr ?? "" };
    }
  };
  // rc.path is RUNNER-owned (uid 10002); runner-cmd (uid 10003) needs an explicit
  // safe.directory exception to avoid a dubious-ownership false negative unrelated to Landlock.
  const calibStatus = await asCommandRoot(["git", "-c", `safe.directory=${rc.path}`, "status", "--porcelain"], rc.path);
  check(`SHARED-CLONE ${scLabel}: calibration git status in the tested clone (outside sandbox)`, calibStatus.code === 0,
    `code=${calibStatus.code} stderr=${calibStatus.stderr}`);
  const calibBare = await asCommandRoot(["ls", path.join(bare, "objects", "pack")], rc.path);
  check(`SHARED-CLONE ${scLabel}: calibration ls bare objects/pack (outside sandbox)`, calibBare.code === 0,
    `code=${calibBare.code} stderr=${calibBare.stderr}`);
  const calibSibling = await asCommandRoot(["ls", path.join(seed.path, ".git")], rc.path);
  check(`SHARED-CLONE ${scLabel}: calibration ls sibling clone .git (outside sandbox)`, calibSibling.code === 0,
    `code=${calibSibling.code} stderr=${calibSibling.stderr}`);

  // 7. Under the real Codex command sandbox. Grant git trust (issue #1716, landed on both srcs)
  //    so a "dubious ownership" rejection can never be mistaken for the Landlock denial (#1769)
  //    this section actually tests.
  const sc1716Fixed = typeof codex.withCommandGitTrust === "function" && typeof codex.canonicalCheckoutPath === "function";
  const scCommandEnv: NodeJS.ProcessEnv = sc1716Fixed
    ? codex.withCommandGitTrust(codex.buildCommandEnv("/tmp", {}), await codex.canonicalCheckoutPath(rc.path))
    : codex.buildCommandEnv("/tmp", {});

  // issue #1769 m3 part 2 — the finalize import, gated on FIXED (both the boundary-machinery
  // test seam and the import API must exist on the mounted src) and real Landlock (the whole
  // point is proving the import works with the bare NOT sandbox-granted).
  const importFixed = scFixed
    && typeof codex.boundaryProcessSpawnerForTest === "function"
    && typeof gitCache.ensureRunnerCloneObjects === "function";
  if (importFixed && probe.status === 0) {
    const safetyMod = await load("codex/safety.js");
    const headTipWrapped = runner.runnerCommand("git", ["-C", rc.path, "rev-parse", "HEAD"]);
    const headTip = (await exec(headTipWrapped.command, headTipWrapped.args, { env: { PATH: "/usr/bin:/bin" } })).stdout.trim();
    await runFinalizeImportPart(
      git, gitCache, codex, launcher, registry, safetyMod, runner,
      bare, rc.path, branch, headTip, rc.baseCommit, originPath, scCommandEnv,
    );
  } else {
    console.log(`SKIP: FINALIZE-IMPORT part; importFixed=${importFixed} probe.status=${probe.status}`);
  }

  const runMode = async (mode: "best-effort" | "required") => {
    const roots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(mode === "required" ? 173 : 172));
    const spawn: Spawn = codex.makeDefaultSpawnCommand(roots, launcher.launchCodexEffectRoot, 5000, rc.path, scCommandEnv, mode);
    const sh = (script: string) => spawn(["/bin/sh", "-c", script], { cwd: rc.path });
    const tag = `SHARED-CLONE ${scLabel} ${mode}`;

    for (const args of ["status", "diff HEAD", "log --oneline"]) {
      const r = await sh(`git ${args}`);
      check(`${tag}: git ${args}`, r.code === 0, `code=${r.code} stderr=${r.stderr.trim()}`);
    }
    // The cat-file checks below are UNFIXED-discriminating ONLY under real Landlock
    // confinement: an UNFIXED (`--shared`) clone resolves these shas through its
    // alternate into the bare, which sits outside the sandbox's granted root
    // (`rc.path`) and is only rejected when the kernel actually confines reads.
    // Without Landlock the alternate is freely readable regardless of FIXED/UNFIXED,
    // so the checks would spuriously pass for UNFIXED too — skip them instead.
    if (probe.status === 0) {
      for (const sha of [rc.baseCommit, marker, advancedTip]) {
        const r = await sh(`git cat-file -e ${sha}`);
        check(`${tag}: cat-file -e ${sha.slice(0, 12)}`, r.code === 0, `code=${r.code} stderr=${r.stderr.trim()}`);
      }
    } else {
      console.log(`SKIP: ${tag}: cat-file -e checks (UNFIXED-discriminating, need Landlock); sandbox --probe returned ${probe.status}`);
    }
    // Checked BEFORE the write+commit below: that commit stages+commits WHATEVER is in the
    // index (including this cherry-pick), so the WIP file is only OBSERVABLY staged pre-commit.
    // On the SECOND mode invocation (required, reusing the same clone) it is already committed
    // by best-effort's own write+commit — check its committed presence there instead.
    if (mode === "best-effort") {
      const staged = await sh("git diff --cached --name-only");
      check(`${tag}: cherry-picked WIP.txt is staged`, staged.code === 0 && staged.stdout.includes("WIP.txt"),
        `code=${staged.code} stdout=${JSON.stringify(staged.stdout)}`);
    } else {
      const committed = await sh("git log --name-only --format= -1 HEAD");
      check(`${tag}: cherry-picked WIP.txt was committed (by the prior best-effort mode)`,
        committed.code === 0 && committed.stdout.includes("WIP.txt"),
        `code=${committed.code} stdout=${JSON.stringify(committed.stdout)}`);
    }

    const commitMsg = `sc-${mode}-${Date.now()}`;
    const commit = await sh(
      `printf '%s\\n' "${commitMsg}" > "sc-${mode}.txt" && git add "sc-${mode}.txt" && git -c user.name=x -c user.email=x@example.com commit -q -m "${commitMsg}"`);
    check(`${tag}: write + git add + git commit`, commit.code === 0, `code=${commit.code} stderr=${commit.stderr.trim()}`);

    // 8. Negative controls, unconditional on FIXED/UNFIXED: the bare and a sibling runner clone
    //    are OUTSIDE the granted root either way — Landlock confines to what the command sandbox
    //    grants, which is only `rc.path` — so a denial here is proof of confinement, not of m1.
    //    Best-effort mode WITHOUT Landlock has no filesystem confinement at all (the sandbox
    //    degrades to unconfined), so these denials would spuriously FAIL there; skip them.
    if (probe.status === 0) {
      const bareLs = await sh(`ls "${path.join(bare, "objects", "pack")}"`);
      check(`${tag}: ls bare objects/pack is denied`, bareLs.code !== 0, `code=${bareLs.code} stderr=${bareLs.stderr.trim()}`);
      const siblingLs = await sh(`ls "${path.join(seed.path, ".git")}"`);
      check(`${tag}: ls sibling clone .git is denied`, siblingLs.code !== 0, `code=${siblingLs.code} stderr=${siblingLs.stderr.trim()}`);
    } else {
      console.log(`SKIP: ${tag}: bare/sibling denial checks (need Landlock); sandbox --probe returned ${probe.status}`);
    }

    check(`${tag}: no live command root`, roots.hasLiveCommandRoot() === false, "a command root is still live");
  };
  await runMode("best-effort");
  if (probe.status === 0) await runMode("required");
  else console.log("SKIP: SHARED-CLONE Codex required mode; sandbox --probe returned 10");

  // issue #1769 m3 review — a per-section result line, distinct from the #1716 section's
  // final RESULT below: a before-run headline must not read as the #1716 section's own
  // FIXED/PASS when the shared-clone section is the one that is UNFIXED/FAIL.
  const scFailureCount = failures.length - scFailuresBefore;
  console.log(`SHARED-CLONE RESULT: ${scLabel} ${scFailureCount > 0 ? "FAIL" : "PASS"}`);
}

async function main(): Promise<void> {
  if (process.env.UZI_UID_SPLIT !== "1" || process.getuid?.() !== 10001) {
    console.log("SKIP: root entrypoint did not establish worker/runner uid split");
    process.exitCode = 77;
    return;
  }
  // Production posture (main.ts): the worker runs with umask 002 under the uid split, and the
  // runner children inherit it, so checkout files are group-`runner` writable for runner-cmd.
  process.umask(0o002);
  const codex = await load("codex/codex-executor.js");
  const launcher = await load("codex/launcher.js");
  const registry = await load("codex/registry.js");
  const runner = await load("runner-uid.js");

  const parent = await fs.stat(path.dirname(root));
  console.log(`OBSERVATION: ${path.dirname(root)} uid=${parent.uid} gid=${parent.gid} mode=${(parent.mode & 0o7777).toString(8)}`);
  await fs.mkdir(root, { recursive: true, mode: 0o2770 });
  // Group `runner` (gid 10002) explicitly, which the worker is a member of: the production clone
  // parent gets it by setgid inheritance from /data/runner, which an older entrypoint may not set.
  await fs.chown(root, 10001, 10002);
  await fs.chmod(root, 0o2770);
  // Checkout A and the nested repository B are both created and owned by the runner (10002),
  // like the production runner clone; B lives inside A's Landlock-permitted subtree, so a
  // rejection there is git's ownership check, not a confinement failure.
  await asRunner(runner.runnerCommand, ["/bin/sh", "-ceu", [
    'git init -q "$1"',
    'chmod 2770 "$1"',
    'printf "one\\n" > "$1/file.txt"',
    'git -C "$1" add file.txt',
    'git -C "$1" -c user.name=runner -c user.email=runner@example.com commit -q -m seed',
    'printf "two\\n" > "$1/file.txt"',
    'git init -q "$2"',
    'printf "b\\n" > "$2/b.txt"',
    'git -C "$2" add b.txt',
    'git -C "$2" -c user.name=runner -c user.email=runner@example.com commit -q -m b',
  ].join("\n"), "sh", checkoutA, nestedB]);
  assert.equal((await fs.stat(checkoutA)).uid, 10002, "checkout A is owned by the runner");
  assert.equal((await fs.stat(nestedB)).uid, 10002, "nested B is owned by the runner");

  const fixed = typeof codex.withCommandGitTrust === "function" && typeof codex.canonicalCheckoutPath === "function";
  const label = fixed ? "FIXED" : "UNFIXED";
  // The same composition run() uses (issue #1716); a base-code src has no trust helper.
  const commandEnv: NodeJS.ProcessEnv = fixed
    ? codex.withCommandGitTrust(codex.buildCommandEnv("/tmp", {}), await codex.canonicalCheckoutPath(checkoutA))
    : codex.buildCommandEnv("/tmp", {});
  console.log(`MODE: ${label} (src=${src}); inline git config: ${JSON.stringify(
    Object.fromEntries(Object.entries(commandEnv).filter(([k]) => k.startsWith("GIT_CONFIG"))))}`);

  const probe = spawnSync("/usr/local/bin/uzi-codex-command-sandbox", ["--probe"], { encoding: "utf8" });
  if (probe.error) throw probe.error;
  if (probe.status !== 0 && probe.status !== 10) throw new Error(`sandbox --probe: ${probe.status}: ${probe.stderr}`);
  console.log(`LANDLOCK: probe=${probe.status}`);
  // Acceptance guard (issue #1769 m3): when the caller demands Landlock (a CI lane that must
  // never silently degrade to unconfined "required" skips), a missing ABI is a hard FAIL, never
  // the exit-77 SKIP a missing Docker/image/uid-split gets.
  if (process.env.CODEX_GIT_TRUST_REQUIRE_LANDLOCK === "1" && probe.status !== 0) {
    console.log(`RESULT: FAIL — CODEX_GIT_TRUST_REQUIRE_LANDLOCK=1 but sandbox --probe returned ${probe.status} (no Landlock ABI)`);
    process.exitCode = 1;
    return;
  }

  let commits = 0;
  const execute = async (mode: "best-effort" | "required") => {
    const roots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(mode === "required" ? 171 : 170));
    const spawn: Spawn = codex.makeDefaultSpawnCommand(roots, launcher.launchCodexEffectRoot, 5000, checkoutA, commandEnv, mode);
    const sh = (script: string, cwd = checkoutA) => spawn(["/bin/sh", "-c", script], { cwd });
    const tag = `${label} ${mode}`;

    const id = await sh("id -u");
    check(`${tag}: command identity is runner-cmd`, id.code === 0 && id.stdout.trim() === "10003",
      `code=${id.code} stdout=${JSON.stringify(id.stdout)} stderr=${id.stderr}`);

    for (const args of ["status", "log --oneline", "diff HEAD"]) {
      const r = await sh(`git ${args}`);
      check(`${tag}: git ${args} in A`, r.code === 0, `code=${r.code} ${dubiousLine(r.stderr)} stderr=${r.stderr.trim()}`);
    }

    commits += 1;
    const message = `cmd-${mode}-${commits}`;
    const commit = await sh(
      `printf '%s\\n' "${message}" > "cmd-${mode}.txt" && git add "cmd-${mode}.txt" && git -c user.name=x -c user.email=x@example.com commit -q -m "${message}"`);
    check(`${tag}: write + git add + git commit in A`, commit.code === 0,
      `code=${commit.code} ${dubiousLine(commit.stderr)} stderr=${commit.stderr.trim()}`);
    if (commit.code === 0) {
      let head = "";
      try {
        head = await asRunner(runner.runnerCommand, ["git", "-C", checkoutA, "log", "--oneline", "-1"]);
      } catch (error) {
        head = `ERROR ${(error as Error).message}`;
      }
      check(`${tag}: the owner (runner) sees the command's commit`, head.includes(message), `log -1: ${head.trim()}`);
    }

    const nodeGit = await spawn([process.execPath, "-e",
      'const r = require("node:child_process").spawnSync("git", ["status", "--porcelain"], { encoding: "utf8" }); process.stderr.write(r.stderr ?? ""); process.exit(r.error ? 99 : r.status ?? 98);'],
    { cwd: checkoutA });
    check(`${tag}: node child spawning git status in A`, nodeGit.code === 0,
      `code=${nodeGit.code} ${dubiousLine(nodeGit.stderr)} stderr=${nodeGit.stderr.trim()}`);

    const readB = await sh(`cat "${nestedB}/.git/HEAD" && ls "${nestedB}/.git"`);
    check(`${tag}: B's .git is readable under the sandbox`, readB.code === 0 && readB.stdout.includes("ref:"),
      `code=${readB.code} stderr=${readB.stderr.trim()}`);

    const statusB = await sh(`git -C "${nestedB}" status`);
    check(`${tag}: git -C nested-b status is rejected as dubious ownership`,
      statusB.code !== 0 && statusB.stderr.includes(DUBIOUS), `code=${statusB.code} stderr=${statusB.stderr.trim()}`);

    const control = await sh("git -c safe.directory= status");
    check(`${tag}: negative control git -c safe.directory= status in A is rejected`,
      control.code !== 0 && control.stderr.includes(DUBIOUS), `code=${control.code} stderr=${control.stderr.trim()}`);
    if (control.stderr.includes(DUBIOUS)) console.log(`OBSERVATION: ${tag} control: ${dubiousLine(control.stderr)}`);

    check(`${tag}: no live command root`, roots.hasLiveCommandRoot() === false, "a command root is still live");
  };
  await execute("best-effort");
  if (probe.status === 0) await execute("required");
  else console.log("SKIP: Codex required mode; sandbox --probe returned 10");

  // issue #1769 m3 part 1 — the shared-clone section, its own FIXED/UNFIXED label (whether the
  // mounted src exports RunnerCloneMaterializationError), folded into the same `failures`/exit
  // code so a red before-run is visibly red overall.
  await runSharedCloneSection(codex, launcher, registry, runner, probe);

  if (failures.length > 0) {
    console.log(`RESULT: ${label} FAIL (${failures.length} check(s)): ${failures.join("; ")}`);
    process.exitCode = 1;
  } else {
    console.log(`RESULT: ${label} PASS`);
  }
}
// A fixture that ends before main() finishes (the event loop drained on a promise that never
// settles) must never read as a pass: without this, node exits 0 with no RESULT line.
let mainCompleted = false;
process.on("exit", () => {
  if (!mainCompleted) {
    console.log("RESULT: FAIL — the fixture ended before main() completed (a pending promise never settled)");
    process.exitCode = 1;
  }
});
main().then(() => { mainCompleted = true; }, (error: unknown) => { console.error(error); process.exitCode = 1; mainCompleted = true; });
