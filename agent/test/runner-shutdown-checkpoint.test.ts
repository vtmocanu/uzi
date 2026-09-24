import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import type { BoundaryPermit, CodexExecutionSafety, HarnessError } from "../src/harness.js";
import { type ExecutorFactory, type RunnerOptions } from "../src/runner.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api,
  client,
  deferred,
  fakeGitlab,
  git,
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

/** Spy on client.publishCheckpoint that does not settle until the test ends — models a
 *  slow/unreachable forge the client-side budget must cap. issue #1597 M2 (review item 11): the
 *  Claude budget race ABANDONS the durability promise, so a hung publish used to outlive its test
 *  and race the next test's teardown (an uncaught `write EPIPE` under load). `restore()` now
 *  RELEASES the hang (a non-2xx result) and `drained` resolves once the pack stream finished, so
 *  the abandoned sequence completes inside the test that started it. */
function spyPublishHang(): { entered: Promise<void>; restore: () => Promise<void> } {
  const enter = deferred();
  const release = deferred();
  const drained = deferred();
  let wasEntered = false;
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    void drain(pack).catch(() => undefined).finally(() => drained.resolve());
    wasEntered = true;
    enter.resolve();
    await release.promise;
    return { ok: false, httpStatus: 599 };
  };
  return {
    entered: enter.promise,
    restore: async () => {
      release.resolve();
      if (wasEntered) await drained.promise;
      // Let the abandoned sequence's continuation run before the harness tears the fixture down.
      await new Promise((r) => setImmediate(r));
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
  opts: {
    /** issue #1597 M1: a Codex-shaped executor (its sinks route through `safety.withBoundary`). */
    safety?: CodexExecutionSafety;
    /** issue #1597 M1: runs after the commit, before the executor signals it is mid-run. */
    midRun?: (ctx: RunContext) => Promise<void>;
  } = {},
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
          if (opts.midRun) await opts.midRun(ctx);
          fs.mkdirSync(pluginDir, { recursive: true });
          fs.writeFileSync(path.join(pluginDir, "marker"), "x");
          fs.mkdirSync(runHome, { recursive: true });
          fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
          start.resolve();
          await waitAbort(ctx.signal!);
          throw new Error("aborted mid-run");
        },
        ...(opts.safety ? { safety: opts.safety } : {}),
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
            "shutdown checkpoint NOT published (reason: publish_rejected) — a resume on another worker will restart from the default branch",
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
            "shutdown checkpoint NOT published (reason: timeout) — a resume on another worker will restart from the default branch",
          ),
        ),
        `expected the NOT-published line when the budget elapses, got ${JSON.stringify(feed)}`,
      );
    } finally {
      await restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});

// ── issue #1597 M1 — a typed shutdown-checkpoint outcome on the feed ─────────────
//
// The shutdown feed line used to be binary ("published" / "NOT published — restart from the
// default branch"), and a Codex boundary error was logged as "nothing published" even when the
// body's publish had already landed. M1 names the CLASS (never an error message / remote text),
// lets a published checkpoint WIN over a late boundary error, keeps a permit/deadline abort off
// the feed, and names the real restart point (the last published checkpoint when one landed).

/** Replace client.publishCheckpoint with `impl` (the pack is drained first). */
function spyPublishWith(
  impl: (call: number, signal: AbortSignal | undefined) => Promise<unknown>,
): { restore: () => void; count: () => number } {
  const orig = client.publishCheckpoint.bind(client);
  let n = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
    signal?: AbortSignal,
  ) => {
    await drain(pack);
    return impl(n++, signal);
  };
  return {
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
    count: () => n,
  };
}

const LANDED = { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };

/** A fake Codex safety whose `shutdown` boundary is scripted. `before` throws the given error
 *  WITHOUT running the action (the boundary could not be established); `after` runs the action
 *  under a held permit, then throws (a late boundary failure); `abortAfterMs` runs the action
 *  under a permit whose signal aborts after the delay (the boundary deadline). `onPermit` hands the
 *  test a function that aborts the shutdown permit ON DEMAND (issue #1597 M2 follow-up: a test
 *  triggers the deadline from inside the publish RPC, deterministically), and — as the real facade
 *  does — a boundary whose permit aborted throws a CodexBoundaryError(timeout) once the action
 *  returns. Other boundaries run their action plainly. Permit-held git subprocesses are spawned
 *  directly. */
function fakeSafety(script: {
  before?: () => Error;
  after?: () => Error;
  abortAfterMs?: number;
  onPermit?: (abort: () => void) => void;
}): CodexExecutionSafety {
  const permitFor = (boundary: string, signal: AbortSignal): BoundaryPermit =>
    ({ epoch: 1, boundary, signal }) as unknown as BoundaryPermit;
  return {
    kind: "codex",
    withBoundary: async (req, action) => {
      const ac = new AbortController();
      if (req.boundary !== "shutdown") return action(permitFor(req.boundary, ac.signal));
      if (script.before) throw script.before();
      let timer: ReturnType<typeof setTimeout> | undefined;
      if (script.abortAfterMs !== undefined) {
        timer = setTimeout(() => ac.abort(new Error("boundary deadline")), script.abortAfterMs);
      }
      script.onPermit?.(() => ac.abort(new Error("boundary deadline")));
      try {
        const value = await action(permitFor(req.boundary, ac.signal));
        if (script.after) throw script.after();
        if (script.onPermit && ac.signal.aborted) {
          throw new CodexBoundaryError("action", [harnessError("timeout")]);
        }
        return value;
      } finally {
        clearTimeout(timer);
      }
    },
    spawnBoundaryProcess: async (_permit, request) => {
      const [command, ...args] = request.argv;
      const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const completed = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, sig) => resolve({ code: code ?? (sig ? 128 : 1) }));
      });
      return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed };
    },
    dispose: async () => ({ kind: "disposed" }),
  };
}

const harnessError = (category: HarnessError["category"]): HarnessError => ({
  category,
  message: `boundary ${category}`,
});

/** Run one shutdown with the given publish spy / safety; return the run's feed + log lines. */
async function shutdownOnce(
  iid: number,
  opts: {
    safety?: CodexExecutionSafety;
    midRun?: (ctx: RunContext) => Promise<void>;
    budgetMs?: number;
    setTimer?: RunnerOptions["setTimer"];
  } = {},
): Promise<{ feed: string[]; lines: Array<Record<string, unknown>>; runId: string }> {
  const { gitlab } = fakeGitlab();
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), `uzi-1597-shut-${iid}-`));
  try {
    const { factory, started } = shutdownFactory(homeRoot, iid, "WORK.txt", opts);
    const { logger, lines } = recordingLogger();
    const runner = runnerWith(
      factory,
      gitlab,
      undefined,
      logger,
      {
        ...(opts.budgetMs !== undefined ? { shutdownPublishTimeoutMs: opts.budgetMs } : {}),
        ...(opts.setTimer ? { setTimer: opts.setTimer } : {}),
      },
    );
    const claim = gitlabClaim(iid);
    const p = runner.execute(claim);
    await started;
    runner.shutdown();
    await p;
    assert.strictEqual(terminal(claim.run_id), false, "a shutdown always requeues (best-effort)");
    return {
      feed: statusTexts(claim.run_id),
      lines: lines as Array<Record<string, unknown>>,
      runId: claim.run_id,
    };
  } finally {
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
}

const DEFAULT_TAIL = "a resume on another worker will restart from the default branch";
const notPublished = (cls: string, tail = DEFAULT_TAIL): string =>
  `shutdown checkpoint NOT published (reason: ${cls}) — ${tail}`;

/** The class the runner logged for this shutdown (runLog.info "shutdown checkpoint outcome"). */
const loggedOutcome = (lines: Array<Record<string, unknown>>): unknown =>
  lines.find((l) => l.msg === "shutdown checkpoint outcome")?.outcome;

describe("RunRunner — typed shutdown checkpoint outcome (issue #1597 M1)", () => {
  it("published: a landed publish states published and logs the class", async () => {
    const { restore } = spyPublishWith(async () => LANDED);
    try {
      const { feed, lines } = await shutdownOnce(1597_01);
      assert.ok(feed.includes("shutdown checkpoint published to origin"), JSON.stringify(feed));
      assert.ok(!feed.some((t) => t.includes("NOT published")), JSON.stringify(feed));
      assert.equal(loggedOutcome(lines), "published");
      // A first-time success had no failure line to recover from: NO recovery line.
      assert.ok(!feed.some((t) => t.includes("recovered")), JSON.stringify(feed));
      assert.ok(!lines.some((l) => l.msg === "checkpoint publishing recovered"), "no recovery log line");
    } finally {
      restore();
    }
  });

  it("an AbortError thrown with NO aborted signal is silent on the feed and names publish_error", async () => {
    // e.g. the client's own request timeout surfacing as an AbortError: publishCheckpointOutcome
    // classes it `aborted` (silent — no "checkpoint publish failed" line, no message on the feed),
    // and the shutdown sink names it publish_error because its permit/budget did not expire.
    const { restore, count } = spyPublishWith(async (_n, signal) => {
      assert.ok(!signal?.aborted, "the caller's signal (if any) is NOT aborted");
      const e = new Error("request aborted boom-secret-remote-text");
      e.name = "AbortError";
      throw e;
    });
    try {
      const { feed, lines } = await shutdownOnce(1597_14);
      assert.equal(count(), 1);
      assert.ok(!feed.some((t) => t.includes("checkpoint publish failed")), JSON.stringify(feed));
      assert.ok(!feed.some((t) => t.includes("boom")), JSON.stringify(feed));
      assert.ok(feed.includes(notPublished("publish_error")), JSON.stringify(feed));
      assert.equal(loggedOutcome(lines), "publish_error");
      assert.ok(lines.some((l) => l.msg === "checkpoint publish aborted"), "runLog records the abort");
    } finally {
      restore();
    }
  });

  it("timeout (Claude): the budget race elapsing names the timeout class", async () => {
    // issue #1597 M2 (review item 11): deterministic — the shutdown budget timer (the only
    // `setTimer` arm of this delay) fires the moment the hung publish is ENTERED, never on a wall
    // clock that races git latency under load; restore() releases the hang inside this test.
    const BUDGET = 54_321;
    const { entered, restore } = spyPublishHang();
    const setTimer = (cb: () => void, ms: number): (() => void) => {
      if (ms === BUDGET) {
        void entered.then(cb);
        return () => {};
      }
      const t = setTimeout(cb, ms);
      t.unref?.();
      return () => clearTimeout(t);
    };
    try {
      const { feed, lines } = await shutdownOnce(1597_02, { budgetMs: BUDGET, setTimer });
      assert.ok(feed.includes(notPublished("timeout")), JSON.stringify(feed));
      assert.equal(loggedOutcome(lines), "timeout");
    } finally {
      await restore();
    }
  });

  it("publish_rejected: a non-2xx names the class and keeps the HTTP-status line", async () => {
    const { restore } = spyPublishWith(async () => ({ ok: false, httpStatus: 500 }));
    try {
      const { feed } = await shutdownOnce(1597_03);
      assert.ok(feed.includes(notPublished("publish_rejected")), JSON.stringify(feed));
      assert.ok(feed.includes("checkpoint publish failed: HTTP 500"), JSON.stringify(feed));
    } finally {
      restore();
    }
  });

  it("publish_skipped: a 2xx skip names the class and the allowlisted skip label", async () => {
    const { restore } = spyPublishWith(async () => ({
      ok: true,
      body: { published: false, ref: "", skipped: "workflow_scope" },
    }));
    try {
      const { feed } = await shutdownOnce(1597_04);
      assert.ok(feed.includes(notPublished("publish_skipped")), JSON.stringify(feed));
      assert.ok(feed.includes("checkpoint publish skipped: workflow_scope"), JSON.stringify(feed));
    } finally {
      restore();
    }
  });

  it("publish_skipped: a non-allowlisted server skip label is folded to `other` on the feed", async () => {
    const { restore } = spyPublishWith(async () => ({
      ok: true,
      body: { published: false, ref: "", skipped: "remote said https://tok@forge/x" },
    }));
    try {
      const { feed } = await shutdownOnce(1597_05);
      assert.ok(feed.includes("checkpoint publish skipped: other"), JSON.stringify(feed));
      assert.ok(!feed.some((t) => t.includes("remote said")), JSON.stringify(feed));
    } finally {
      restore();
    }
  });

  it("publish_error: a throw names the class and NO part of the thrown message reaches the feed", async () => {
    const { restore } = spyPublishWith(async () => {
      throw new Error("boom-secret-remote-text");
    });
    try {
      const { feed, lines } = await shutdownOnce(1597_06);
      assert.ok(feed.includes(notPublished("publish_error")), JSON.stringify(feed));
      assert.ok(feed.includes("checkpoint publish failed: error"), JSON.stringify(feed));
      for (const needle of ["boom", "secret", "remote-text"]) {
        assert.ok(!feed.some((t) => t.includes(needle)), `feed leaked "${needle}": ${JSON.stringify(feed)}`);
      }
      // The operator log keeps the message.
      assert.ok(
        lines.some((l) => l.msg === "checkpoint publish failed: error" && l.error === "boom-secret-remote-text"),
        "runLog.warn keeps the thrown message",
      );
    } finally {
      restore();
    }
  });

  it("no_local_tip: no tracking ref to pack names the class and publishes nothing", async () => {
    const { restore, count } = spyPublishWith(async () => LANDED);
    const origPack = git.checkpointPack.bind(git);
    (git as unknown as { checkpointPack: unknown }).checkpointPack = async () => null;
    try {
      const { feed } = await shutdownOnce(1597_07);
      assert.equal(count(), 0, "nothing was sent to the publish RPC");
      assert.ok(feed.includes(notPublished("no_local_tip")), JSON.stringify(feed));
      assert.ok(!feed.some((t) => t.startsWith("checkpoint publish")), JSON.stringify(feed));
    } finally {
      (git as unknown as { checkpointPack: unknown }).checkpointPack = origPack;
      restore();
    }
  });

  it("boundary_blocked (Codex): a non-timeout CodexBoundaryError names boundary_blocked", async () => {
    const { restore, count } = spyPublishWith(async () => LANDED);
    try {
      const safety = fakeSafety({
        before: () => new CodexBoundaryError("reconcile", [harnessError("protocol")]),
      });
      const { feed, lines } = await shutdownOnce(1597_08, { safety });
      assert.equal(count(), 0, "the blocked boundary never ran the publish");
      assert.ok(feed.includes(notPublished("boundary_blocked")), JSON.stringify(feed));
      assert.equal(loggedOutcome(lines), "boundary_blocked");
    } finally {
      restore();
    }
  });

  it("timeout (Codex): a CodexBoundaryError carrying a timeout-category error names timeout", async () => {
    const { restore } = spyPublishWith(async () => LANDED);
    try {
      const safety = fakeSafety({
        before: () => new CodexBoundaryError("quiesce", [harnessError("transport"), harnessError("timeout")]),
      });
      const { feed, lines } = await shutdownOnce(1597_09, { safety });
      assert.ok(feed.includes(notPublished("timeout")), JSON.stringify(feed));
      assert.equal(loggedOutcome(lines), "timeout");
    } finally {
      restore();
    }
  });

  it("published wins (Codex): a boundary error AFTER the publish landed still states published", async () => {
    const { restore, count } = spyPublishWith(async () => LANDED);
    try {
      const safety = fakeSafety({
        after: () => new CodexBoundaryError("reap", [harnessError("timeout")]),
      });
      const { feed, lines } = await shutdownOnce(1597_10, { safety });
      assert.equal(count(), 1, "the publish ran and landed inside the boundary");
      assert.ok(feed.includes("shutdown checkpoint published to origin"), JSON.stringify(feed));
      assert.ok(!feed.some((t) => t.includes("NOT published")), JSON.stringify(feed));
      assert.equal(loggedOutcome(lines), "published");
      assert.ok(
        !lines.some((l) => typeof l.msg === "string" && l.msg.includes("nothing published")),
        "no log line claims nothing was published",
      );
    } finally {
      restore();
    }
  });

  it("aborted is silent (Codex): a permit-deadline abort emits no publish-failed line and names timeout", async () => {
    // issue #1597 M2 follow-up: the permit is aborted from INSIDE the publish RPC (once it is
    // entered), never on a wall-clock timer — an early timer abort could land before the pack and
    // read as no_local_tip. The fake boundary then throws CodexBoundaryError(timeout) like the real
    // facade.
    let abortPermit: (() => void) | undefined;
    const { restore, count } = spyPublishWith(async (_n, signal) => {
      abortPermit!();
      await new Promise<void>((resolve) => {
        if (signal?.aborted) resolve();
        else signal?.addEventListener("abort", () => resolve(), { once: true });
      });
      throw new Error("upload aborted boom-secret-remote-text");
    });
    try {
      const safety = fakeSafety({ onPermit: (abort) => (abortPermit = abort) });
      const { feed, lines } = await shutdownOnce(1597_11, { safety });
      assert.equal(count(), 1, "the publish RPC was entered (the abort came from inside it)");
      assert.ok(!feed.some((t) => t.includes("checkpoint publish failed")), JSON.stringify(feed));
      assert.ok(!feed.some((t) => t.includes("boom")), JSON.stringify(feed));
      assert.ok(feed.includes(notPublished("timeout")), JSON.stringify(feed));
      assert.equal(loggedOutcome(lines), "timeout");
    } finally {
      restore();
    }
  });

  it("names the last published checkpoint as the restart point when a prior checkpoint landed", async () => {
    // Call 0 = the mid-run milestone checkpoint (lands); call 1 = the shutdown publish (HTTP 500).
    const { restore, count } = spyPublishWith(async (n) => (n === 0 ? LANDED : { ok: false, httpStatus: 500 }));
    try {
      const { feed } = await shutdownOnce(1597_12, {
        midRun: async (ctx) => {
          await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
          commitInTree(ctx.worktreePath, "MORE.txt", "after the checkpoint\n");
        },
      });
      assert.equal(count(), 2, "the mid-run checkpoint and the shutdown publish both ran");
      assert.ok(
        feed.includes(
          notPublished(
            "publish_rejected",
            "a resume on another worker will restart from the last published checkpoint if it is still adoptable, else the branch or default branch",
          ),
        ),
        JSON.stringify(feed),
      );
    } finally {
      restore();
    }
  });

  it("recovery: fail → succeed emits ONE recovered line; a later failure is shown again", async () => {
    // Calls: 0 = HTTP 500 (surfaced), 1 = landed (recovered), 2 = HTTP 500 (surfaced AGAIN).
    const { restore, count } = spyPublishWith(async (n) => (n === 1 ? LANDED : { ok: false, httpStatus: 500 }));
    try {
      const { feed } = await shutdownOnce(1597_13, {
        midRun: async (ctx) => {
          await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } }); // 500
          await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } }); // retry of the same tip: lands
          commitInTree(ctx.worktreePath, "MORE.txt", "after the recovery\n");
        },
      });
      assert.equal(count(), 3, "two mid-run publishes plus the shutdown publish");
      const FAILED = "checkpoint publish failed: HTTP 500";
      const RECOVERED = "checkpoint publishing recovered — published to origin";
      // Exactly one recovered line, between two failure lines: the recovery cleared the dedupe
      // set, so the recurring HTTP 500 is surfaced again instead of being swallowed.
      assert.deepEqual(
        feed.filter((t) => t === FAILED || t === RECOVERED),
        [FAILED, RECOVERED, FAILED],
        JSON.stringify(feed),
      );
    } finally {
      restore();
    }
  });
});
