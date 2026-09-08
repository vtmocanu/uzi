import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { WIP_PARK_COMMIT_PREFIX } from "../src/git.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import {
  api,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// #1197: verified capture must precede the promotable recovery_wait report.
// Failure/retry/restart cases live in runner-recovery-capture-failure.test.ts.

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
      // The park was reported after the best-effort capture (the causal fence); with a real
      // git the local restore point verified, so the feed line is the full-durability one.
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
      // A truthful, auto-resume park notice — the FULL-DURABILITY line (a verified local
      // restore point), distinct from the degraded "last durable checkpoint" wording the
      // capture-shortfall path emits.
      const texts = feedTexts(parked.runId);
      assert.ok(
        texts.some((t) => /resumes automatically/.test(t)),
        "a truthful auto-resume park notice is emitted",
      );
      assert.ok(
        texts.some((t) => /the recovery checkpoint is saved/.test(t)),
        "a verified capture emits the full-durability park line",
      );
      assert.strictEqual(
        texts.some((t) => /last durable checkpoint/.test(t)),
        false,
        "a verified capture does NOT emit the degraded 'last durable checkpoint' line",
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
      // The worker still ATTEMPTED the report (the best-effort capture ran first, the park is
      // requested regardless), but keyed the park DECISION on the status literal, so a 200
      // that acked a different status is NOT parked — clean up as unparked.
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

  it("ack error (409 refusal): the run moved on under the worker → NOT parked, cleaned up", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-rec-409-"));
    try {
      const iid = 307;
      const { factory, paths } = recoveryFactory(homeRoot, iid, (wt) => {
        commitInTree(wt, "WORK.txt", "work\n");
      });
      const claim = gitlabClaim(iid);
      // The recovery_wait report is refused with a 409 carrying a DIFFERENT run status (the
      // run was cancelled/parked concurrently under the worker). reportState reads a 409
      // inline as { applied: false, status: "cancelled" }, so ack.status !== "recovery_wait"
      // → handleRecoveryExhausted returns false → NOT parked. Mirrors the limit_wait suite's
      // "cleans up fully when the park is refused with a 409".
      api.refuseStateWith409(claim.run_id);
      await runnerWith(factory, gitlab).execute(claim);
      const p = paths();
      // Capture precedes the refused park report; the saved tracking tip proves
      // this ran through recovery rather than failing before the capture path.
      assert.ok(shaInBare(git.barePathFor(fx.originPath), trackingRef(iid)));

      // NOT parked → the whole session is cleaned up: worktree, plugin dir and run HOME.
      assert.strictEqual(fs.existsSync(p.worktree), false, "clone removed (not parked)");
      assert.strictEqual(fs.existsSync(p.pluginDir), false, "plugin dir removed (not parked)");
      assert.strictEqual(fs.existsSync(p.runHome), false, "run HOME removed (not parked)");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
