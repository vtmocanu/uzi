import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

export interface Fixture {
  /** A normal (non-bare) repo on disk with one commit on `main`, used as origin. */
  originPath: string;
  /** An empty dir to hand to GitCache as UZI_DATA_DIR. */
  dataDir: string;
  cleanup(): void;
}

/**
 * Turn git's AUTO-MAINTENANCE off in a throwaway repo (issue #127).
 *
 * Every git command that writes into a repo — `commit` here, and the `receive-pack`
 * the code under test's `git push` runs, both with GIT_DIR=<repo>/.git, so both read
 * THIS repo-local config — ends by spawning `git maintenance run --auto --detach`.
 * Measured with `GIT_TRACE2_EVENT` on git 2.55, in a 3-loose-object repo:
 *
 *     child_start: ["git","maintenance","run","--auto","--no-quiet","--detach"]
 *     region_enter: maintenance/detach
 *
 * The spawn is UNCONDITIONAL — nothing about this repo is small enough to prevent it;
 * the child detaches and takes `objects/maintenance.lock` BEFORE evaluating whether
 * any task (gc.auto's threshold) needs to run. `--detach` means that child DAEMONIZES:
 * it outlives the git
 * process node awaited, and keeps writing inside `.git` (`objects/maintenance.lock`,
 * and `gc.log`/`gc.pid` if the gc task then runs). A teardown `fs.rmSync` racing it
 * gets ENOTEMPTY — `force: true` suppresses ENOENT, not ENOTEMPTY. Measured on an
 * idle laptop: `objects/maintenance.lock` still held 4.1 ms AFTER `git commit`
 * resolved (1 run in 10); a loaded CI runner stretches that window, which is how it
 * failed the v0.11.6 tag pipeline in teardown.
 *
 * `maintenance.auto=false` suppresses the spawn outright (verified: zero maintenance
 * children in the trace for both `commit` and `push`). `gc.auto=0` covers git older
 * than the maintenance rework, where the detaching process is `git gc --auto` itself.
 * A throwaway single-commit repo has nothing to maintain, so disabling beats
 * foregrounding it via `maintenance.autoDetach=false`.
 */
export function disableAutoMaintenance(repoPath: string, env: NodeJS.ProcessEnv): void {
  execFileSync("git", ["-C", repoPath, "config", "maintenance.auto", "false"], { env, stdio: "pipe" });
  execFileSync("git", ["-C", repoPath, "config", "gc.auto", "0"], { env, stdio: "pipe" });
  // A SECOND detached daemon the two keys above do not cover: `git fsmonitor--daemon
  // run --detach`, spawned wherever core.fsmonitor is true. It reparents to init and
  // watches the directory for its whole life, so it holds handles indefinitely rather
  // than for the milliseconds a maintenance child does — the difference between a race
  // and a permanent block. Callers here pass a GIT_CONFIG_GLOBAL=/dev/null env, so this
  // key is belt-and-braces for the fixture origin; the repos that actually leaked are
  // the ones `src/git.ts` creates, and it sets the same key for the reason written
  // there. Kept symmetrical on purpose: a reader comparing the two helpers should not
  // have to wonder whether the omission was deliberate. See that comment for the
  // measurement (a 21-day-old daemon holding a deleted fixture).
  execFileSync("git", ["-C", repoPath, "config", "core.fsmonitor", "false"], { env, stdio: "pipe" });
}

/**
 * Best-effort ledger line for scripts/tmpdir-leak-guard.sh (issue #2020): when the
 * guard exported UZI_TMPDIR_GUARD_LEDGER, append one JSON line so a leftover
 * `uzi-agent-test-*` dir can be attributed to its creator. A failed write is
 * swallowed: the ledger is evidence only and must never fail a test.
 */
function ledger(event: "created" | "removed", base: string, extra: Record<string, unknown> = {}): void {
  const file = process.env.UZI_TMPDIR_GUARD_LEDGER;
  if (!file) return;
  try {
    const line = JSON.stringify({
      entry: path.basename(base),
      path: base,
      event,
      pid: process.pid,
      at: new Date().toISOString(),
      ...extra,
    });
    fs.appendFileSync(file, line + "\n");
  } catch {
    // best-effort
  }
}

/** Up to 4 `file:line` stack frames under agent/test/, excluding this file. */
function callerSites(): string[] {
  const sites: string[] = [];
  for (const frame of (new Error().stack ?? "").split("\n").slice(1)) {
    const m = /\(?((?:file:\/\/)?[^()\s]*\/test\/[^()\s]+?:\d+):\d+\)?\s*$/.exec(frame);
    const site = m?.[1];
    if (site === undefined || site.includes("/fixture-repo.ts:")) continue;
    sites.push(site.replace(/^file:\/\//, ""));
    if (sites.length === 4) break;
  }
  return sites;
}

/**
 * Build a throwaway git "origin" on disk. A bare clone of a local path needs no
 * network and no auth, so the whole worktree lifecycle is exercisable offline.
 *
 * `files` (repo-relative path → contents) are committed alongside the README, so
 * a test can ship a repo that carries e.g. its own `.claude/agents/`.
 */
export function makeFixture(files: Record<string, string> = {}, opts: { testName?: string } = {}): Fixture {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-agent-test-"));
  // Recorded before the git work so a later throw in makeFixture still leaves a created line.
  ledger("created", base, { file: process.argv[1] ?? null, test: opts.testName ?? null, site: callerSites() });
  const originPath = path.join(base, "origin");
  const dataDir = path.join(base, "data");
  fs.mkdirSync(originPath);
  fs.mkdirSync(dataDir);

  // Fixture construction must own its repo, index, objects and config. Keep the
  // non-Git environment (including PATH, TMPDIR and the ledger) for local tooling;
  // never mutate process.env or the environment used by production gitEnv.
  const env: NodeJS.ProcessEnv = { ...process.env };
  for (const key of Object.keys(env)) {
    if (key.startsWith("GIT_")) delete env[key];
  }
  env.GIT_CONFIG_GLOBAL = "/dev/null";
  env.GIT_CONFIG_SYSTEM = "/dev/null";
  env.GIT_TERMINAL_PROMPT = "0";
  const git = (args: string[]): void => {
    execFileSync("git", ["-C", originPath, ...args], { env, stdio: "pipe" });
  };
  execFileSync("git", ["init", "-b", "main", originPath], { env, stdio: "pipe" });
  git(["config", "user.email", "fixture@uzi.local"]);
  git(["config", "user.name", "fixture"]);
  git(["config", "commit.gpgsign", "false"]);
  // Before the first commit: nothing may leave a detached git process running in
  // here (issue #127 — see disableAutoMaintenance).
  disableAutoMaintenance(originPath, env);
  fs.writeFileSync(path.join(originPath, "README.md"), "# fixture\n");
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  git(["add", "."]);
  git(["commit", "-m", "init"]);

  return {
    originPath,
    dataDir,
    cleanup() {
      // BELT-AND-BRACES, not the fix (issue #127). The fix is disableAutoMaintenance
      // above, which stops any detached git process existing in `origin` at all —
      // measured over a full suite run: 68 detached maintenance children inside the
      // fixture origin before, 0 after, including all 32 spawned by the `receive-pack`
      // of a push (the exposure the CI failure named: `rmdir '.../origin/.git'`).
      //
      // The retry covers the repos PRODUCT CODE creates under `dataDir` (worker bare +
      // runner clone), which live inside `base` and still spawn ~115 of their own per
      // run. Note precisely why it is a retry and not more config: those repos ARE
      // reachable — `gitEnv()` passes `process.env.GIT_CONFIG_GLOBAL` straight through
      // (`agent/src/git.ts:576`), and exporting one pointing at these settings takes
      // 115 -> 8, measured. It is not taken because doing so stops the integration
      // tests exercising gitEnv's PRODUCTION `/dev/null` default, and
      // `test/git-hardening.test.ts:49-65` deliberately mutates that var. That fidelity
      // cost, not impossibility, is the reason. (The GIT_CONFIG_COUNT/KEY_n route does
      // NOT work at all: gitEnv builds a replacement env and resets the count from 0,
      // `git.ts:602-609`.)
      //
      // WHAT THE RETRY ACTUALLY BUYS, measured rather than assumed — Node's rimraf
      // sweeps a directory's children ONCE, then loops `rmdirSync(path)` alone; it never
      // re-scans. So it rescues only entries that REMOVE THEMSELVES: `maintenance.lock`
      // (git unlinks it) is recovered at ~515 ms where 0 retries fails instantly. For a
      // PERSISTENT entry — `gc.log`/`gc.pid`, which git leaves on purpose and which is
      // this issue's own hypothesis for the error naming `.git` itself — retrying cannot
      // help and merely converts a 216 ms failure into a 3020 ms one. Per directory, on
      // a blocking sleep.
      //
      // Retries alone would have been the wrong sole fix regardless: they hide the leak
      // instead of removing it, and a process outliving its test can race the NEXT
      // fixture. CEILING: Node sleeps i*retryDelay for i=1..maxRetries, so 50 x 55 =
      // 2750 ms per stuck directory — NOT maxRetries*retryDelay, and a linear back-off
      // cannot have a product ceiling. A tree stuck at several levels multiplies it.
      fs.rmSync(base, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
      ledger("removed", base);
    },
  };
}
