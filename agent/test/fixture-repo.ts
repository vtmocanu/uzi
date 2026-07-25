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
}

/**
 * Build a throwaway git "origin" on disk. A bare clone of a local path needs no
 * network and no auth, so the whole worktree lifecycle is exercisable offline.
 *
 * `files` (repo-relative path → contents) are committed alongside the README, so
 * a test can ship a repo that carries e.g. its own `.claude/agents/`.
 */
export function makeFixture(files: Record<string, string> = {}): Fixture {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-agent-test-"));
  const originPath = path.join(base, "origin");
  const dataDir = path.join(base, "data");
  fs.mkdirSync(originPath);
  fs.mkdirSync(dataDir);

  // Isolate from host/global git config so init.defaultBranch, gpg signing, or a
  // missing user identity on the runner can't perturb the fixture.
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
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
      // above, which stops any detached git process existing in `origin` at all. The
      // retry covers what the fixture CANNOT pre-configure: the repos the code under
      // test creates under `dataDir` (the worker bare + the runner clone), which also
      // live inside `base` and whose own commit/fetch/push each spawn a detached
      // `git maintenance` — three of them back to back just before a run returns.
      // Retries alone would have been the wrong sole fix: they hide the leak instead
      // of removing it, and a process outliving its test can also race the NEXT
      // fixture. maxRetries*retryDelay is the ceiling (Node backs off linearly).
      fs.rmSync(base, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    },
  };
}
