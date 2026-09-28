import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { runnerGitCarriesWorkerMark } from "../src/git.js";
import { procfsTable, setQuiescenceViewForTests } from "../src/run-quiescence.js";
import { scopedRealView } from "./fake-proc.js";
import { api, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";

// issue #1783 (auditor M1) — an agent-planted git filter driver must not outlive a park.
//
// The runner clone's `.git/config` and `.gitattributes` are agent-writable, and `filter.<name>.*`
// cannot be pinned off. The park sinks run `git status` / `add -A` / `commit` in that clone (the wip
// marker) AFTER their quiescence proof and BEFORE their credentialed fetch-back / overlay / publish.
// A `clean` filter the agent planted runs inside that git and here starts a DETACHED `setsid sleep`
// whose cwd is the clone. Two things must hold, and each fails without its half of the fix:
//   (a) that git carries NO worker spawn mark, so the sleep does not inherit the nonce and stays
//       reapable (a marked plant would be exempt from every reap forever);
//   (b) the sink RE-PROVES process quiescence after the marker, so the sleep is gone before the
//       credentialed publish runs, not merely at some later sink.
// Real processes, real git, the real in-process reaper (single uid: the runner uid is this test's).

installHarness();

const HAS_PROCFS = process.platform === "linux" && fs.existsSync(path.join("/", "proc", "self", "status"));
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-planted-filter-"));
const pidFiles: string[] = [];

function plantedPids(pidFile: string): number[] {
  if (!fs.existsSync(pidFile)) return [];
  return fs
    .readFileSync(pidFile, "utf8")
    .split("\n")
    .filter((l) => /^\d+$/.test(l.trim()))
    .map(Number);
}

// The reaper sees this file's own descendants plus the planted (reparented) sleeps, never the
// host's other processes.
before(() => {
  setQuiescenceViewForTests(
    scopedRealView({ pidFiles: ["limit", "pause", "wall", "align"].map((n) => path.join(scratch, `${n}.pids`)) }),
  );
});

after(() => {
  restoreHermeticView();
  for (const f of pidFiles) {
    for (const pid of plantedPids(f)) {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        /* gone */
      }
    }
  }
  fs.rmSync(scratch, { recursive: true, force: true });
});

/** Alive and not a zombie (a reparented sleep may linger as a zombie until its reaper waits). */
function alive(pid: number): boolean {
  try {
    return !/^State:\s*[ZX]/m.test(procfsTable.readStatus(pid));
  } catch {
    return false;
  }
}

/** Plant `filter.evil.clean` in the clone: the driver backgrounds a detached `setsid sleep` (its pid
 *  appended to `pidFile`) and then passes the content through unchanged. */
function plantFilter(clone: string, pidFile: string): void {
  const driver = path.join(scratch, `evil-${path.basename(pidFile)}.sh`);
  fs.writeFileSync(
    driver,
    `#!/bin/sh\nsetsid sh -c 'echo $$ >> ${JSON.stringify(pidFile).slice(1, -1)}; exec sleep 300' </dev/null >/dev/null 2>&1 &\ncat\n`,
    { mode: 0o755 },
  );
  execFileSync("git", ["-C", clone, "config", "filter.evil.clean", driver], { env: GIT_ENV, stdio: "pipe" });
  fs.writeFileSync(path.join(clone, ".gitattributes"), "*.dat filter=evil\n");
  // Uncommitted work the park's wip marker will `add -A` (running the clean filter).
  fs.writeFileSync(path.join(clone, "DIRTY.dat"), "uncommitted work\n");
}

function commitWork(tree: string): void {
  fs.writeFileSync(path.join(tree, "WORK.txt"), "work\n");
  execFileSync("git", ["-C", tree, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", tree, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

/** Spy the checkpoint publish: record, at the moment it runs, which planted pids are alive. */
function spyPublish(pidFile: string): { aliveAtPublish: number[][] } {
  const aliveAtPublish: number[][] = [];
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    aliveAtPublish.push(plantedPids(pidFile).filter(alive));
    await drain(pack);
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return { aliveAtPublish };
}

const RUNNER_OPTS = { checkpointIntervalMs: 0, recoveryRetryMs: 1 };

describe("issue #1783 M1: the runner-clone git classification", () => {
  it("marks only subcommands that start no clone-configured driver", () => {
    for (const args of [
      ["rev-parse", "--verify", "HEAD^{commit}"],
      ["rev-list", "--count", "a..b"],
      ["update-ref", "refs/x", "abc"],
      ["config", "user.name", "x"],
      ["ls-files", "--cached", "--", "x"],
      ["ls-tree", "HEAD", "--", "x"],
      ["check-ignore", "-q", "--no-index", "--", "x"],
      ["clone", "--shared", "--no-checkout", "/bare", "/clone"],
    ]) {
      assert.equal(runnerGitCarriesWorkerMark(args), true, args.join(" "));
    }
    for (const args of [
      ["status", "--porcelain"],
      ["add", "-A"],
      ["commit", "-m", "x"],
      ["-c", "user.name=x", "commit", "-m", "x"],
      ["checkout", "-b", "b", "sha"],
      ["reset", "--mixed", "HEAD^"],
      ["merge", "--no-edit", "x"],
      ["rebase", "x"],
      ["cherry-pick", "--no-commit", "x"],
      ["rm", "-r", "x"],
      ["clean", "-fd"],
      ["diff", "--cached", "--name-only"],
      ["log", "-1", "--format=%s"],
      ["clone", "--shared", "/bare", "/clone"],
      [],
    ]) {
      assert.equal(runnerGitCarriesWorkerMark(args), false, args.join(" "));
    }
  });
});

describe("issue #1783 M1: a planted filter driver does not survive a park", { skip: !HAS_PROCFS }, () => {
  it("limit park: the filter's detached sleep is reaped before the credentialed publish runs", async () => {
    const { gitlab } = fakeGitlab();
    const pidFile = path.join(scratch, "limit.pids");
    pidFiles.push(pidFile);
    const pub = spyPublish(pidFile);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          plantFilter(ctx.worktreePath, pidFile);
          const { LimitReachedError } = await import("../src/limit.js");
          throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
        },
      },
    });
    const claim = gitlabClaim(1850, { wait_on_limit: true });
    await runnerWith(factory, gitlab, undefined, undefined, RUNNER_OPTS).execute(claim);

    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "limit_wait"), "the run parked");
    const planted = plantedPids(pidFile);
    assert.ok(planted.length >= 1, "the planted clean filter ran inside the park's wip marker");
    assert.ok(pub.aliveAtPublish.length >= 1, "the park published its checkpoint (the clone was proven quiescent)");
    assert.deepEqual(pub.aliveAtPublish, pub.aliveAtPublish.map(() => []), "no planted process was alive while the publish ran");
    assert.deepEqual(planted.filter(alive), [], "no planted process survives the park");
  });

  it("pause park: the filter's detached sleep is reaped before the checkpoint publish runs", async () => {
    const { gitlab } = fakeGitlab();
    const pidFile = path.join(scratch, "pause.pids");
    pidFiles.push(pidFile);
    const pub = spyPublish(pidFile);
    const parked: Array<boolean | undefined> = [];
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          plantFilter(ctx.worktreePath, pidFile);
          const at = { completedCount: 1, total: 2 };
          const p = await ctx.parkForPause?.(at);
          parked.push(p);
          return p ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
        },
      },
    });
    const claim = gitlabClaim(1851);
    await runnerWith(factory, gitlab, undefined, undefined, RUNNER_OPTS).execute(claim);

    assert.deepEqual(parked, [true], "the pause parked");
    const planted = plantedPids(pidFile);
    assert.ok(planted.length >= 1, "the planted clean filter ran inside the pause's wip marker");
    assert.ok(pub.aliveAtPublish.length >= 1, "the pause published its checkpoint");
    assert.deepEqual(pub.aliveAtPublish, pub.aliveAtPublish.map(() => []), "no planted process was alive while the publish ran");
    assert.deepEqual(planted.filter(alive), [], "no planted process survives the pause");
  });

  it("wall park (captureHoldContext): the filter's detached sleep is reaped before the credentialed publish runs", async () => {
    const { gitlab } = fakeGitlab();
    const pidFile = path.join(scratch, "wall.pids");
    pidFiles.push(pidFile);
    const pub = spyPublish(pidFile);
    const outcomes: unknown[] = [];
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          plantFilter(ctx.worktreePath, pidFile);
          const o = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
          outcomes.push(o);
          return o === "parked" || o === "undeliverable"
            ? { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } }
            : { branch: ctx.branch };
        },
      },
    });
    await runnerWith(factory, gitlab, undefined, undefined, RUNNER_OPTS).execute(gitlabClaim(1852));

    assert.deepEqual(outcomes, ["parked"], "the wall park stands");
    const body = api.wallParkRequests[0]!.body as { head: string; published: boolean };
    assert.equal(body.head.length, 40, "a verified capture (the clone was proven quiescent after the marker)");
    const planted = plantedPids(pidFile);
    assert.ok(planted.length >= 1, "the planted clean filter ran inside the capture's wip marker");
    assert.ok(pub.aliveAtPublish.length >= 1, "the capture published its checkpoint");
    assert.deepEqual(pub.aliveAtPublish, pub.aliveAtPublish.map(() => []), "no planted process was alive while the publish ran");
    assert.deepEqual(planted.filter(alive), [], "no planted process survives the wall park");
  });
});

/** Plant `filter.evil.smudge` for `*.yml` via the clone's `.git/info/attributes` (which the
 *  align's `clean -fd` does not remove): the driver backgrounds a detached `setsid sleep` (its pid
 *  appended to `pidFile`) and passes the content through. */
function plantSmudge(clone: string, pidFile: string): void {
  const driver = path.join(scratch, `smudge-${path.basename(pidFile)}.sh`);
  fs.writeFileSync(
    driver,
    `#!/bin/sh\nsetsid sh -c 'echo $$ >> ${JSON.stringify(pidFile).slice(1, -1)}; exec sleep 300' </dev/null >/dev/null 2>&1 &\ncat\n`,
    { mode: 0o755 },
  );
  execFileSync("git", ["-C", clone, "config", "filter.evil.smudge", driver], { env: GIT_ENV, stdio: "pipe" });
  fs.mkdirSync(path.join(clone, ".git", "info"), { recursive: true });
  fs.appendFileSync(path.join(clone, ".git", "info", "attributes"), "*.yml filter=evil\n");
}

function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  execFileSync("git", ["-C", fx.originPath, "add", "."], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", fx.originPath, ...IDENT, "commit", "-m", msg], { env: GIT_ENV, stdio: "pipe" });
}

describe("issue #1783: a planted smudge filter does not survive into the finalize push", { skip: !HAS_PROCFS }, () => {
  it("base-align: the smudge's detached sleep is reaped before the PAT push runs", async () => {
    const pidFile = path.join(scratch, "align.pids");
    pidFiles.push(pidFile);
    commitToOriginMain({ ".github/workflows/ci.yml": "name: ci\non: [push]\njobs: {}\n" }, "seed workflows");
    const { github, calls: prCalls } = fakeGitHub();
    const aliveAtPush: number[][] = [];
    const origPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...a: Parameters<typeof git.pushBranch>) => {
      aliveAtPush.push(plantedPids(pidFile).filter(alive));
      return origPush(...a);
    }) as typeof git.pushBranch;
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          plantSmudge(ctx.worktreePath, pidFile);
          // main moves ahead on .github/workflows: finalize overlays the default's workflow tree,
          // whose `checkout <default> -- .github/workflows` runs the planted smudge.
          commitToOriginMain({ ".github/workflows/ci.yml": "name: ci\non: [pull_request]\njobs: {}\n" }, "main advances");
          return { branch: ctx.branch };
        },
      },
    });
    const claim = gitlabClaim(1853, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    await runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { ...RUNNER_OPTS, github }).execute(claim);

    const planted = plantedPids(pidFile);
    assert.ok(planted.length >= 1, "the planted smudge ran inside the finalize base-align");
    assert.ok(aliveAtPush.length >= 1, "the aligned branch was pushed (the clone was proven quiescent after the align)");
    assert.deepEqual(aliveAtPush, aliveAtPush.map(() => []), "no planted process was alive while the PAT push ran");
    assert.equal(prCalls.length, 1, "the run opened its PR");
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"), "the run completed");
    assert.deepEqual(planted.filter(alive), [], "no planted process survives the run");
  });
});
