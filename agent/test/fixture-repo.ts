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
 * Build a throwaway git "origin" on disk. A bare clone of a local path needs no
 * network and no auth, so the whole worktree lifecycle is exercisable offline.
 */
export function makeFixture(): Fixture {
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
  fs.writeFileSync(path.join(originPath, "README.md"), "# fixture\n");
  git(["add", "."]);
  git(["commit", "-m", "init"]);

  return {
    originPath,
    dataDir,
    cleanup() {
      fs.rmSync(base, { recursive: true, force: true });
    },
  };
}
