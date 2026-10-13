import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { it } from "node:test";
import { GitCache, gitEnv, runnerGitCarriesWorkerMark, runnerGitSpawnEnv } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

type GitInternals = {
  cloneBare(repoUrl: string, dest: string): Promise<void>;
  runGit(cwd: string | undefined, args: string[]): Promise<string>;
  runGitAsRunner(cwd: string | undefined, args: string[], opts?: { timeoutMs?: number }): Promise<string>;
  disableAutoMaintenance(dest: string): Promise<void>;
  fetch(dest: string): Promise<void>;
};

async function captureBareClone(cache: GitCache, repoUrl: string, dest: string): Promise<string[]> {
  const internals = cache as unknown as GitInternals;
  const clones: string[][] = [];
  internals.runGit = async (_cwd, args) => {
    if (args[0] === "clone") clones.push([...args]);
    return "";
  };
  internals.disableAutoMaintenance = async () => {};
  internals.fetch = async () => {};
  await internals.cloneBare(repoUrl, dest);
  assert.equal(clones.length, 1);
  return clones[0]!;
}

function gitIn(cwd: string, args: string[]): string {
  return execFileSync("git", args, { cwd, env: gitEnv(), encoding: "utf8" }).trim();
}

function captureRunnerClones(cache: GitCache): string[][] {
  const internals = cache as unknown as GitInternals;
  const run = internals.runGitAsRunner.bind(cache);
  const clones: string[][] = [];
  internals.runGitAsRunner = async (cwd, args, opts) => {
    if (args[0] === "clone") clones.push([...args]);
    return run(cwd, args, opts);
  };
  return clones;
}

it("cloneBare terminates options before the repository and destination", async () => {
  const fx = makeFixture();
  try {
    const cache = new GitCache(fx.dataDir, nullLogger());
    const repoUrl = "-repository";
    const dest = path.join(fx.dataDir, "captured.git");
    const args = await captureBareClone(cache, repoUrl, dest);
    assert.deepEqual(args, ["clone", "--bare", "--", repoUrl, dest]);
  } finally {
    fx.cleanup();
  }
});

it("real Git replays cloneBare argv with a dash-leading destination", async () => {
  const fx = makeFixture();
  try {
    const expectedHead = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const cache = new GitCache(fx.dataDir, nullLogger());
    const args = await captureBareClone(cache, fx.originPath, "-clone.git");
    gitIn(fx.dataDir, args);
    const bare = path.join(fx.dataDir, "-clone.git");
    assert.ok(fs.existsSync(bare), "Git must create the dash-leading destination from the captured argv");
    assert.equal(gitIn(bare, ["rev-parse", "--is-bare-repository"]), "true");
    assert.equal(gitIn(bare, ["rev-parse", "HEAD"]), expectedHead);
  } finally {
    fx.cleanup();
  }
});

it("runnerCloneForBranch terminates clone options before both paths", async () => {
  const fx = makeFixture();
  try {
    const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await cache.ensureClone(fx.originPath);
    const clones = captureRunnerClones(cache);
    const result = await cache.runnerCloneForBranch(bare, "agent/issue-2775", "issue-2775", noProofReseed, "run-2775");
    assert.equal(clones.length, 1);
    const args = clones[0]!;
    const clonePath = args.at(-1)!;
    assert.equal(result.path, clonePath);
    assert.deepEqual(args, ["clone", "--shared", "--no-checkout", "--", bare, clonePath]);
    assert.equal(gitIn(clonePath, ["rev-parse", "HEAD"]), gitIn(fx.originPath, ["rev-parse", "HEAD"]));
  } finally {
    fx.cleanup();
  }
});

it("runnerCloneAtCommit terminates clone options before both paths", async () => {
  const fx = makeFixture();
  try {
    const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await cache.ensureClone(fx.originPath);
    const head = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const clones = captureRunnerClones(cache);
    const result = await cache.runnerCloneAtCommit(bare, head, "run-2775");
    assert.equal(clones.length, 1);
    const args = clones[0]!;
    const clonePath = args.at(-1)!;
    assert.equal(result, clonePath);
    assert.deepEqual(args, ["clone", "--shared", "--no-checkout", "--", bare, clonePath]);
    assert.equal(gitIn(clonePath, ["rev-parse", "HEAD"]), head);
  } finally {
    fx.cleanup();
  }
});

it("runnerGitCarriesWorkerMark recognizes separated clone argv including leading -c", () => {
  for (const prefix of [[], ["-c", "gc.auto=0", "-c", "maintenance.auto=false"]]) {
    assert.equal(runnerGitCarriesWorkerMark([...prefix, "clone", "--shared", "--no-checkout", "--", "-bare.git", "-clone"]), true);
    assert.equal(runnerGitCarriesWorkerMark([...prefix, "clone", "--shared", "--", "-bare.git", "-clone"]), false);
  }
});

it("runnerGitSpawnEnv marks separated no-checkout clones without blocking transport including leading -c", () => {
  const base = { PATH: "/fixture/bin" };
  for (const prefix of [[], ["-c", "gc.auto=0", "-c", "maintenance.auto=false"]]) {
    const marked = runnerGitSpawnEnv([...prefix, "clone", "--shared", "--no-checkout", "--", "-bare.git", "-clone"], base);
    assert.ok(marked.UZI_WORKER_SPAWN);
    assert.equal(marked.GIT_ALLOW_PROTOCOL, undefined);
    assert.equal(marked.PATH, base.PATH);
    const unmarked = runnerGitSpawnEnv([...prefix, "clone", "--shared", "--", "-bare.git", "-clone"], marked);
    assert.equal(unmarked.UZI_WORKER_SPAWN, undefined);
    assert.equal(unmarked.GIT_ALLOW_PROTOCOL, undefined);
  }
  assert.deepEqual(base, { PATH: "/fixture/bin" });
});
