import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  client,
  deferred,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// ── PRD #1030 M4 — publish a FINAL checkpoint on graceful shutdown ─────────────
//
// Before M4 the graceful-shutdown catch branch (`active?.shuttingDown`) was FETCH-BACK
// ONLY, so a roll-while-running / eviction / OOM / node-drain lost up to a whole ~20-min
// checkpoint interval of committed work: a DIFFERENT worker re-claiming the requeued run
// cold-started from the default branch. M4 mirrors the park path's ordering on the
// shutdown branch — commitWipMarker → fetchBackBestEffort → publishCheckpointBestEffort —
// under a client-side budget (shutdownPublishTimeoutMs) that keeps the whole sequence
// well within the k8s termination grace (30s default), and surfaces the outcome on the
// feed (published vs NOT-published) reusing the M1 batcher-emit mechanism. The publish
// stays on the join-token seam (client.publishCheckpoint), never a git push / PAT.

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

/** A recorded publishCheckpoint call: the tip oid and the packfile object count
 *  (bytes 8..12, big-endian; 0 for an empty/no-op pack). */
interface PublishCall {
  runId: string;
  tipOid: string;
  objectCount: number;
}

function drain(stream: Readable): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (c: Buffer) => chunks.push(Buffer.from(c)));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}

/** Spy on client.publishCheckpoint: buffer the pack, parse its object count, record the
 *  call, and return a landed 2xx. Returns [calls, restore]. */
function spyPublish(): { calls: PublishCall[]; restore: () => void } {
  const calls: PublishCall[] = [];
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    runId: string,
    tipOid: string,
    pack: Readable,
  ) => {
    const bytes = await drain(pack);
    const objectCount = bytes.length >= 12 ? bytes.readUInt32BE(8) : -1;
    calls.push({ runId, tipOid, objectCount });
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/agent/issue-x` } };
  };
  return {
    calls,
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

/** Spy on client.publishCheckpoint that drains the pack then returns a non-2xx result,
 *  to drive the shutdown-publish FAILURE feed line. */
function spyPublishHttpError(httpStatus: number): { restore: () => void } {
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    await drain(pack);
    return { ok: false, httpStatus };
  };
  return {
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

/** Spy on client.publishCheckpoint that NEVER resolves — models a slow/unreachable forge
 *  the client-side budget must cap. A bare never-resolving promise (no timer) keeps no
 *  handle alive, so `node --test` still exits. */
function spyPublishHang(): { entered: Promise<void>; restore: () => void } {
  const enter = deferred();
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    pack.resume(); // drain so the pack stream does not itself hold a handle
    enter.resolve();
    return new Promise(() => {}); // never settles
  };
  return {
    entered: enter.promise,
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

interface Paths {
  pluginDir: string;
  runHome: string;
  worktree: string;
}

/** Resolves once `signal` aborts (or immediately if already aborted). */
function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

/** An executor that commits `file` in the runner clone (so there is committed work to
 *  checkpoint), materializes the two dirs a same-worker resume needs, signals it is
 *  mid-run, waits for its controller to abort (worker shutdown), then throws — the shape
 *  a run takes when shutdown() aborts it mid-flight. */
function shutdownFactory(
  homeRoot: string,
  iid: number,
  file: string | null,
): { factory: ExecutorFactory; started: Promise<void>; sha: () => string; paths: () => Paths } {
  const pluginDir = skillsPluginDir(worktreeDirFor(iid));
  const start = deferred();
  let runHome = "";
  let sha = "";
  const factory: ExecutorFactory = (runId) => {
    runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          if (file) sha = commitInTree(ctx.worktreePath, file, "work before the shutdown\n");
          fs.mkdirSync(pluginDir, { recursive: true });
          fs.writeFileSync(path.join(pluginDir, "marker"), "x");
          fs.mkdirSync(runHome, { recursive: true });
          fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
          start.resolve();
          await waitAbort(ctx.signal!);
          throw new Error("aborted mid-run");
        },
      },
    };
  };
  return {
    factory,
    started: start.promise,
    sha: () => sha,
    paths: () => ({ pluginDir, runHome, worktree: worktreeDirFor(iid) }),
  };
}

const terminal = (runId: string) =>
  api.states.some(
    (s) =>
      s.runId === runId &&
      (s.body.status === "failed" || s.body.status === "completed"),
  );

/** The `status`-kind feed lines the worker emitted for a run. */
const statusTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

describe("RunRunner — checkpoint on graceful shutdown (PRD #1030 M4)", () => {
  it("publishes a final checkpoint carrying the committed work and states it on the feed", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1030-shut-ok-"));
    const { calls, restore } = spyPublish();
    try {
      const iid = 1030;
      const { factory, started, sha } = shutdownFactory(homeRoot, iid, "WORK.txt");
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(iid);
      const p = runner.execute(claim);
      await started;
      runner.shutdown();
      await p;

      // The shutdown branch published EXACTLY one checkpoint (fetch-back-only pre-M4 => 0).
      assert.strictEqual(
        calls.length,
        1,
        `the shutdown path must publish exactly one checkpoint, got ${calls.length}`,
      );
      assert.strictEqual(
        calls[0]!.runId,
        claim.run_id,
        "the publish is brokered for this run over its join token",
      );
      assert.strictEqual(
        calls[0]!.tipOid,
        sha(),
        "the checkpoint carries the tracking-ref tip = the committed work (proves the fetch-back ran first)",
      );
      assert.ok(
        calls[0]!.objectCount > 0,
        `the pack is non-empty (carries the new commit + tree + blob), got ${calls[0]!.objectCount}`,
      );
      const feed = statusTexts(claim.run_id);
      assert.ok(
        feed.includes("shutdown checkpoint published to origin"),
        `expected the shutdown-published feed line, got ${JSON.stringify(feed)}`,
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("still preserves the session and requeues (fetch-back + preserveSession not regressed)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1030-shut-preserve-"));
    const { calls, restore } = spyPublish();
    try {
      const iid = 1031;
      const { factory, started, paths } = shutdownFactory(homeRoot, iid, "WORK.txt");
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(iid);
      const p = runner.execute(claim);
      await started;
      runner.shutdown();
      await p;

      const paths_ = paths();
      // The two dirs a same-worker resume needs must SURVIVE the shutdown (PRD #556 M1),
      // proving the shutdown carve-out path (not a plain failure cleanup) was taken.
      assert.strictEqual(
        fs.existsSync(paths_.pluginDir),
        true,
        "skills plugin dir must survive a worker-shutdown interrupt",
      );
      assert.strictEqual(
        fs.existsSync(paths_.runHome),
        true,
        "run HOME must survive a worker-shutdown interrupt",
      );
      // The carve-out is scoped: the runner clone is still removed.
      assert.strictEqual(
        fs.existsSync(paths_.worktree),
        false,
        "the runner clone is still removed on a shutdown",
      );
      // No terminal report — the run stays requeuable.
      assert.strictEqual(
        terminal(claim.run_id),
        false,
        "a shutdown interruption must requeue, never report a terminal state",
      );
      // The fetch-back still ran (a non-empty checkpoint proves the tracking ref was written).
      assert.ok(
        calls.length === 1 && calls[0]!.objectCount > 0,
        "the fetch-back + publish still fire on the shutdown branch",
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("warns on the feed that a resume restarts from default when the publish fails, and still requeues", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1030-shut-fail-"));
    const { restore } = spyPublishHttpError(500);
    try {
      const iid = 1032;
      const { factory, started } = shutdownFactory(homeRoot, iid, "WORK.txt");
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(iid);
      const p = runner.execute(claim);
      await started;
      runner.shutdown();
      await p;

      assert.strictEqual(
        terminal(claim.run_id),
        false,
        "a failed checkpoint publish must not change the requeue outcome (best-effort)",
      );
      const feed = statusTexts(claim.run_id);
      assert.ok(
        feed.some((t) =>
          t.includes(
            "shutdown checkpoint NOT published — a resume on another worker will restart from the default branch",
          ),
        ),
        `expected the shutdown-not-published consequence line, got ${JSON.stringify(feed)}`,
      );
      assert.ok(
        feed.some((t) => t.includes("checkpoint publish failed: HTTP 500")),
        `expected the generic HTTP-status line naming the cause, got ${JSON.stringify(feed)}`,
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("does not hang past the client-side budget when the publish is slow, and states NOT-published", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1030-shut-hang-"));
    const { entered, restore } = spyPublishHang();
    try {
      const iid = 1033;
      const { factory, started } = shutdownFactory(homeRoot, iid, "WORK.txt");
      // A short budget: the hanging publish must be cut off, the run must complete
      // promptly. Kept comfortably above local git-subprocess latency (the pre-publish
      // commitWipMarker/fetch-back/overlay steps race the same budget in runner.ts) yet
      // far below the 5s wall assertion below, so on a healthy host the publish IS
      // reached and the budget caps the *hanging publish* — not an earlier git step.
      // Test-only override; the production default (15s) is untouched.
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), {
        shutdownPublishTimeoutMs: 500,
      });
      const claim = gitlabClaim(iid);
      const started_at = Date.now();
      const p = runner.execute(claim);
      await started;
      runner.shutdown();
      await p; // must resolve despite the never-settling publish
      // Best-effort non-vacuity signal: on a healthy host the budget lets the pre-publish
      // git steps finish so publish IS reached and `entered` resolves. Under CPU contention
      // those real git subprocesses can eat the whole budget before publish is reached, so
      // `entered` may never resolve — that is a legitimately skipped step, NOT a hang. Bound
      // the wait so the test can never deadlock either way; the load-invariant lives in the
      // observable assertions below (prompt return, requeue, NOT-published line). The timer
      // is cleared after the race so it holds no handle open (see spyPublishHang's note).
      let publishReachTimer: ReturnType<typeof setTimeout> | undefined;
      await Promise.race([
        entered,
        new Promise<void>((resolve) => {
          publishReachTimer = setTimeout(resolve, 1_000);
        }),
      ]);
      clearTimeout(publishReachTimer);

      assert.ok(
        Date.now() - started_at < 5_000,
        "the shutdown must not wait out a hanging publish (budget capped it)",
      );
      assert.strictEqual(
        terminal(claim.run_id),
        false,
        "a budget-capped publish still requeues (best-effort)",
      );
      const feed = statusTexts(claim.run_id);
      assert.ok(
        feed.some((t) =>
          t.includes(
            "shutdown checkpoint NOT published — a resume on another worker will restart from the default branch",
          ),
        ),
        `expected the NOT-published line when the budget elapses, got ${JSON.stringify(feed)}`,
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
