import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { GitCache, WIP_PARK_COMMIT_PREFIX } from "../src/git.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// issue #1197 (D-RC2c): the WORKER side of the recovery_wait park — capture-and-VERIFY
// the local restore point BEFORE reporting a promotable park (the causal fence), with a
// preserve-and-do-not-promote fallback on a local capture/verification failure. Driven on
// the REAL runner + real git end to end (a fake executor throws TransientRecoveryError,
// exactly as SdkExecutor's driveTurnWithEmptyRecovery does on a persistently-empty turn).

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitInTree(treePath: string, file: string, content: string): string {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
  return execFileSync("git", ["-C", treePath, "rev-parse", "HEAD"], {
    env: GIT_ENV,
    encoding: "utf8",
  }).trim();
}

function shaInBare(bare: string, ref: string): string | null {
  try {
    return execFileSync("git", ["-C", bare, "rev-parse", "--verify", ref], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: ["pipe", "pipe", "pipe"],
    }).trim();
  } catch {
    return null;
  }
}

const trackingRef = (iid: number) => `refs/uzi-runner/agent/issue-${iid}`;

/** A factory whose executor materializes the two resume dirs (plugin dir + per-run HOME),
 *  optionally writes/commits work in the runner clone, then throws TransientRecoveryError
 *  — the shape driveTurnWithEmptyRecovery produces on a persistently-empty turn. */
function recoveryFactory(
  homeRoot: string,
  iid: number,
  work: (worktreePath: string) => void,
): { factory: ExecutorFactory; paths: () => Paths } {
  const pluginDir = skillsPluginDir(worktreeDirFor(iid));
  let runHome = "";
  const factory: ExecutorFactory = (runId) => {
    runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(pluginDir, { recursive: true });
          fs.writeFileSync(path.join(pluginDir, "marker"), "x");
          fs.mkdirSync(runHome, { recursive: true });
          fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
          work(ctx.worktreePath);
          throw new TransientRecoveryError();
        },
      },
    };
  };
  return {
    factory,
    paths: () => ({ pluginDir, runHome, worktree: worktreeDirFor(iid) }),
  };
}

interface Paths {
  pluginDir: string;
  runHome: string;
  worktree: string;
}

const feedTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

describe("RunRunner — recovery_wait park (issue #1197 D-RC2c)", () => {
  it("committed work: verifies the local restore point, parks recovery_wait, preserves the session", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-rec-committed-"));
    try {
      const iid = 301;
      let sha = "";
      const { factory, paths } = recoveryFactory(homeRoot, iid, (wt) => {
        sha = commitInTree(wt, "WORK.txt", "recovered work\n");
      });
      await runnerWith(factory, gitlab).execute(gitlabClaim(iid));
      const p = paths();
      // The promotable park was reported ONLY after the local restore point was verified.
      const parked = api.states.find((s) => s.body.status === "recovery_wait");
      assert.ok(parked, "the worker must have reported recovery_wait");
      // No work-destroying terminal.
      assert.strictEqual(
        api.states.some((s) => s.body.status === "failed"),
        false,
        "recovery must NOT report a terminal failed",
      );
      // The tracking ref carries the committed work (the verified restore point a reseed reads).
      const bare = git.barePathFor(fx.originPath);
      assert.strictEqual(
        shaInBare(bare, trackingRef(iid)),
        sha,
        "the tracking ref covers the committed tip after a verified recovery capture",
      );
      // The two resume dirs survive the park (flight.parked carve-out); the clone is removed.
      assert.strictEqual(fs.existsSync(p.pluginDir), true, "plugin dir survives a park");
      assert.strictEqual(fs.existsSync(p.runHome), true, "run HOME survives a park");
      assert.strictEqual(fs.existsSync(p.worktree), false, "the clone is removed on a park");
      // A truthful, auto-resume park notice.
      assert.ok(
        feedTexts(parked.runId).some((t) => /resumes automatically/.test(t)),
        "a truthful auto-resume park notice is emitted",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("uncommitted work: commits a WIP marker, verifies, and parks recovery_wait", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-rec-dirty-"));
    try {
      const iid = 302;
      const { factory } = recoveryFactory(homeRoot, iid, (wt) => {
        // A genuinely dirty, never-committed edit → commitWipMarker must capture it.
        fs.writeFileSync(path.join(wt, "WIP.txt"), "in-progress, uncommitted\n");
      });
      await runnerWith(factory, gitlab).execute(gitlabClaim(iid));
      assert.ok(
        api.states.some((s) => s.body.status === "recovery_wait"),
        "a dirty tree is committed to a WIP marker, verified, and parked",
      );
      // The tracking-ref tip is the wip(park): marker commit carrying the uncommitted work.
      const bare = git.barePathFor(fx.originPath);
      const tip = shaInBare(bare, trackingRef(iid));
      assert.ok(tip, "the tracking ref exists after the WIP capture");
      const subject = execFileSync("git", ["-C", bare, "log", "-1", "--format=%s", tip!], {
        env: GIT_ENV,
        encoding: "utf8",
      }).trim();
      assert.ok(
        subject.startsWith(WIP_PARK_COMMIT_PREFIX),
        `the verified tip is the WIP marker, got ${JSON.stringify(subject)}`,
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("clean tree (no work): the already-committed tip is a verified no-op restore point → parks", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-rec-clean-"));
    try {
      const iid = 303;
      const { factory } = recoveryFactory(homeRoot, iid, () => {
        /* leave the tree exactly as seeded — clean */
      });
      await runnerWith(factory, gitlab).execute(gitlabClaim(iid));
      assert.ok(
        api.states.some((s) => s.body.status === "recovery_wait"),
        "a clean tree whose committed tip already matches is a verified no-op success",
      );
      assert.strictEqual(
        api.states.some((s) => s.body.status === "failed"),
        false,
        "a clean-tree recovery is not a terminal failure",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("LOCAL capture failure: does NOT park; preserves the session, no promotion, no terminal", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-rec-verifyfail-"));
    try {
      const iid = 304;
      // A real GitCache whose ONLY broken method is the restore-point verification — the
      // commit + fetch-back run, but verification never confirms the tracking ref covers
      // HEAD, so the bounded local retry exhausts and the capture is declared FAILED.
      const brokenGit = new GitCache(fx.dataDir, nullLogger());
      brokenGit.verifyRunnerTrackingCovers = async () => false;
      const pluginDir = skillsPluginDir(worktreeDirFor(iid));
      let runHome = "";
      const factory: ExecutorFactory = (runId) => {
        runHome = path.join(homeRoot, runId);
        return {
          homeDir: runHome,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              fs.mkdirSync(pluginDir, { recursive: true });
              fs.writeFileSync(path.join(pluginDir, "marker"), "x");
              fs.mkdirSync(runHome, { recursive: true });
              fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
              commitInTree(ctx.worktreePath, "WORK.txt", "unverifiable work\n");
              throw new TransientRecoveryError();
            },
          },
        };
      };
      const runner = new RunRunner(client, brokenGit, factory, nullLogger(), 20, undefined, {
        pollMs: 5,
        planApprovalTimeoutMs: 0,
        questionTimeoutMs: 600,
        gitlab,
      });
      await runner.execute(gitlabClaim(iid));
      // NO promotable park was reported.
      assert.strictEqual(
        api.states.some((s) => s.body.status === "recovery_wait"),
        false,
        "an unverifiable capture must NOT report a promotable park",
      );
      // And NO work-destroying terminal.
      assert.strictEqual(
        api.states.some((s) => s.body.status === "failed"),
        false,
        "a local capture failure must NOT report a terminal failed",
      );
      // The SESSION is preserved (flight.preserveSession, the shutdown-branch carve-out):
      // the plugin dir + per-run HOME survive so a same-worker retry resumes the SDK
      // transcript rather than cold-starting. The clone leg is removed unconditionally
      // (PRD #218 M6 — the reseed re-clones from the bare), so preserveSession preserves
      // exactly the two dirs the shutdown branch does. Crucially, NO promotable park was
      // reported, so no promotion/reseed fires onto the un-verified restore point.
      assert.strictEqual(fs.existsSync(pluginDir), true, "the plugin dir (session) is preserved");
      assert.strictEqual(fs.existsSync(runHome), true, "the session HOME is preserved");
      assert.strictEqual(
        fs.existsSync(worktreeDirFor(iid)),
        false,
        "the clone leg is removed unconditionally (PRD #218 M6); durability is the session + tracking ref",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("ack discrimination: a 200 that reports a DIFFERENT status than recovery_wait is NOT parked", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-rec-declined-"));
    try {
      const iid = 305;
      const { factory, paths } = recoveryFactory(homeRoot, iid, (wt) => {
        commitInTree(wt, "WORK.txt", "work\n");
      });
      const claim = gitlabClaim(iid);
      // 200 + a status that is NOT recovery_wait: applied is true, but the run is not parked.
      api.overrideStateStatus(claim.run_id, "failed");
      await runnerWith(factory, gitlab).execute(claim);
      const p = paths();
      // The worker still ATTEMPTED the report (capture verified first), but keyed the park
      // decision on the status literal, so it is NOT parked — clean up as unparked.
      assert.ok(
        api.states.some((s) => s.body.status === "recovery_wait"),
        "the worker reported recovery_wait (the capture verified first)",
      );
      assert.strictEqual(fs.existsSync(p.worktree), false, "clone removed (not parked)");
      assert.strictEqual(fs.existsSync(p.pluginDir), false, "plugin dir removed (not parked)");
      assert.strictEqual(fs.existsSync(p.runHome), false, "run HOME removed (not parked)");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
