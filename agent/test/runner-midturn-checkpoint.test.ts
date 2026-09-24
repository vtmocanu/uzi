import { afterEach, describe, it } from "node:test";
import { AsyncResource } from "node:async_hooks";
import assert from "node:assert/strict";
import { execFile, execFileSync, spawn } from "node:child_process";
import { promisify } from "node:util";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import type { ExecutorFactory, RunnerOptions } from "../src/runner.js";
import { RunRunner } from "../src/runner.js";
import type { RunContext } from "../src/executor.js";
import type { BoundaryPermit, BoundaryProcessRequest, CodexExecutionSafety } from "../src/harness.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { GitCache } from "../src/git.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { defaultGitleaksShim, shimCalls, writeGitleaksShim as writeShim } from "./gitleaks-shim.js";
import type { Logger } from "../src/log.js";
import {
  api,
  client,
  deferred,
  fakeGitlab,
  fx,
  gitlabClaim,
  installHarness,
  isAlive,
} from "./runner-harness.js";

installHarness();

// ── issue #1597 M2 — the MID-TURN checkpoint tick ─────────────────────────────────────────────
//
// While ONE long agent turn is in flight nothing else checkpoints the run, so a SIGKILL / eviction
// mid-turn lost everything since the last milestone. M2 arms a repeating tick around executor.run
// that (1) probes the runner clone for an in-progress git op, (2) opens a cancellable, process-
// group-supervised scope, (3) try-acquires the per-flight sink gate, (4) runs the checkpoint body
// QUIET with reap:false semantics — fetch-back, steer/bridge, time gate — and, when about to publish,
// SCANS a range PINNED to SHAs and packs exactly that range, (5) publishes with the tick signal.
// Teardown (turn end, shutdown) awaits full settlement; a SIGKILLed child's lock file is removed
// only on PROVEN ownership. These tests drive it through the real runner with real git fixtures.

const TICK_MS = 424_242; // the tick interval; the fake setTimer captures exactly this delay
const INTERVAL_MS = 1_000; // CHECKPOINT_INTERVAL for the time gate (fake clock)
const LANDED = { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/x" } };

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitIn(dir: string, args: string[], input?: Buffer | string): string {
  return execFileSync("git", ["-C", dir, ...args], {
    env: GIT_ENV,
    encoding: "utf8",
    ...(input !== undefined ? { input } : {}),
  }).trim();
}

function commitIn(tree: string, file: string, content: string): string {
  fs.mkdirSync(path.dirname(path.join(tree, file)), { recursive: true });
  fs.writeFileSync(path.join(tree, file), content);
  gitIn(tree, ["add", "-A"]);
  gitIn(tree, [...IDENT, "commit", "-m", `work ${file}`]);
  return gitIn(tree, ["rev-parse", "HEAD"]);
}

function refOr(dir: string, ref: string): string | null {
  try {
    return gitIn(dir, ["rev-parse", "--verify", "--quiet", ref]);
  } catch {
    return null;
  }
}

function drain(stream: Readable): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (c: Buffer) => chunks.push(Buffer.from(c)));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}

function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) resolve();
    else signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

const statusTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

const finalStatus = (runId: string): string | undefined =>
  api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status)).filter(
    (st) => st === "completed" || st === "failed",
  )[0];

/** A GitHub-PAT-shaped value ASSEMBLED AT RUNTIME (never a complete token literal in source). The
 *  36-char body is hex (18 random bytes): uniformly distributed, with no modulo bias. */
function runtimeSecret(): string {
  const body = randomBytes(18).toString("hex");
  return "gh" + "p_" + body;
}

// ── tick + publish control ──────────────────────────────────────────────────────────────────

interface Ctl {
  setTimer: NonNullable<RunnerOptions["setTimer"]>;
  /** Captures EVERY tick-timer arm (interval or kick); `armedDelay()` is the latest delay. */
  setTickTimer: NonNullable<RunnerOptions["setTickTimer"]>;
  armedDelay: () => number | undefined;
  now: () => number;
  advance: (ms: number) => void;
  /** Fire the captured tick timer and resolve with the tick's outcome class. */
  fire: () => Promise<string>;
  /** Fire WITHOUT waiting for the outcome (it is still recorded). */
  fireNoWait: () => void;
  nextOutcome: () => Promise<string>;
  outcomes: string[];
  armed: () => boolean;
  /** Resolves once every tick this control STARTED has reported its outcome (i.e. no tick is in
   *  flight); rejects after `ms`. */
  settled: (ms: number) => Promise<void>;
  hooks: NonNullable<RunnerOptions["checkpointTestHooks"]>;
  pids: number[];
  spawned: string[][];
}

function control(extraHooks: Partial<NonNullable<RunnerOptions["checkpointTestHooks"]>> = {}): Ctl {
  let tickCb: (() => void) | undefined;
  let tickDelay: number | undefined;
  let fakeNow = 0;
  const outcomes: string[] = [];
  const waiters: Array<{ n: number; resolve: (o: string) => void }> = [];
  const pids: number[] = [];
  const spawned: string[][] = [];
  // Ticks this control STARTED. The runner reports exactly one outcome per started tick, clears its
  // in-flight marker synchronously just before reporting it, and starts nothing on a fire while a
  // tick is in flight (the timer is only armed while the ticker is not stopped). So a tick is in
  // flight exactly while `started > outcomes.length`, and a fire starts one iff none is.
  let started = 0;
  const onOutcome = (o: string): void => {
    outcomes.push(o);
    for (const w of waiters.slice()) {
      if (outcomes.length > w.n) {
        waiters.splice(waiters.indexOf(w), 1);
        w.resolve(outcomes[w.n]!);
      }
    }
  };
  const waitIdx = (n: number): Promise<string> =>
    outcomes.length > n ? Promise.resolve(outcomes[n]!) : new Promise((resolve) => waiters.push({ n, resolve }));
  const ctl: Ctl = {
    setTimer: (cb, ms) => {
      const t = setTimeout(cb, ms);
      t.unref?.();
      return () => clearTimeout(t);
    },
    setTickTimer: (cb, ms) => {
      // Like a real setTimeout, the callback runs in the async context that ARMED it (not the
      // context of whoever calls fire()), so a tick armed inside a Codex permit is modelled
      // faithfully (issue #1597, MR !1618 review).
      const bound = AsyncResource.bind(cb);
      tickCb = bound;
      tickDelay = ms;
      return () => {
        if (tickCb === bound) {
          tickCb = undefined;
          tickDelay = undefined;
        }
      };
    },
    armedDelay: () => tickDelay,
    now: () => fakeNow,
    advance: (ms) => {
      fakeNow += ms;
    },
    fire: () => {
      const n = outcomes.length;
      ctl.fireNoWait();
      return waitIdx(n);
    },
    fireNoWait: () => {
      const cb = tickCb;
      assert.ok(cb, "the tick timer is armed");
      const starts = started === outcomes.length;
      cb();
      if (starts) started++;
    },
    nextOutcome: () => waitIdx(outcomes.length),
    outcomes,
    armed: () => tickCb !== undefined,
    settled: (ms) => waitFor(() => outcomes.length >= started, ms),
    pids,
    spawned,
    hooks: {
      onTickOutcome: onOutcome,
      tickKillGraceMs: 300,
      ...extraHooks,
      tickSpawn: {
        onSpawn: (pid, argv) => {
          pids.push(pid);
          spawned.push([...argv]);
        },
        ...extraHooks.tickSpawn,
      },
    },
  };
  return ctl;
}

/** Replace client.publishCheckpoint with `impl` (receives the pack + signal). */
function stubPublish(
  impl: (call: number, tipOid: string, pack: Readable, signal?: AbortSignal) => Promise<unknown>,
): { restore: () => void; count: () => number; tips: string[] } {
  const orig = client.publishCheckpoint.bind(client);
  let n = 0;
  const tips: string[] = [];
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    tipOid: string,
    pack: Readable,
    signal?: AbortSignal,
  ) => {
    tips.push(tipOid);
    return impl(n++, tipOid, pack, signal);
  };
  return {
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
    count: () => n,
    tips,
  };
}

/** A publish that hangs until its signal aborts, then rejects like an aborted fetch.
 *
 *  It stops being the pack's consumer while the pack-objects child may still be running, so it
 *  keeps an `error` listener on the pack for good. The abort SIGTERMs that child; a child killed by
 *  a signal completes with code 128, and GitCache.spawnGit then destroys the pack with
 *  `git pack-objects --revs --stdout exited 128`. That destroy reaches the stream only when the
 *  child's `exit` is delivered BEFORE its stdout reaches EOF. On Linux EOF was observed to arrive
 *  first, so the destroy is a no-op there. When `exit` comes first (forced on Linux by leaving a
 *  SIGTERM-ignoring grandchild holding stdout; the macOS `exited 128` failures fit the same
 *  ordering), the destroy emits an `error` nobody listens for and the test fails with an uncaught
 *  exception. */
async function hangUntilAborted(pack: Readable, signal?: AbortSignal): Promise<never> {
  pack.on("error", () => undefined);
  pack.resume();
  await waitAbort(signal!);
  const e = new Error("publish aborted");
  e.name = "AbortError";
  throw e;
}

function mkGit(dataDir: string, gitleaksBin?: string, log: Logger = nullLogger()): GitCache {
  fs.mkdirSync(dataDir, { recursive: true });
  return new GitCache(dataDir, log, undefined, { gitleaksBin: gitleaksBin ?? defaultGitleaksShim() });
}

function mkRunner(
  g: GitCache,
  factory: ExecutorFactory,
  ctl: Ctl | undefined,
  extra: Partial<RunnerOptions> = {},
  log: Logger = nullLogger(),
): RunRunner {
  const { gitlab } = fakeGitlab();
  return new RunRunner(client, g, factory, log, 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    checkpointIntervalMs: INTERVAL_MS,
    checkpointTickIntervalMs: TICK_MS,
    ...(ctl ? { setTimer: ctl.setTimer, setTickTimer: ctl.setTickTimer, now: ctl.now, checkpointTestHooks: ctl.hooks } : {}),
    ...extra,
  });
}

/** Errors thrown by a test's in-turn body. An assert inside the executor turn only FAILS THE RUN
 *  (the runner catches it), so every such error is recorded here and the afterEach below fails the
 *  test on it (review item 1) — in addition to the explicit finalStatus checks. */
const turnErrors: unknown[] = [];
/** Failures of {@link settleAndClean} (a runner or tick that did not settle, a tick child still
 *  alive after settlement). Reported from the afterEach below rather than thrown from a test's
 *  `finally`, so they never replace the test's own assertion error: node:test keeps the first
 *  error a test records. */
const teardownErrors: unknown[] = [];
afterEach(() => {
  const errs = [...turnErrors.splice(0), ...teardownErrors.splice(0)];
  if (errs.length > 0) throw errs[0];
});

async function recordingTurnErrors(body: () => Promise<void>): Promise<void> {
  try {
    await body();
  } catch (e) {
    turnErrors.push(e);
    throw e;
  }
}

/** An executor whose single long turn runs `body`, then returns (turn completes). */
function turn(body: (ctx: RunContext) => Promise<void>): ExecutorFactory {
  return () => ({
    executor: {
      run: async (ctx: RunContext) => {
        await recordingTurnErrors(() => body(ctx));
        return { branch: ctx.branch };
      },
    },
  });
}

/** An executor whose turn runs `body` then blocks until the run's controller aborts (shutdown). */
function blockingTurn(body: (ctx: RunContext) => Promise<void>): ExecutorFactory {
  return () => ({
    executor: {
      run: async (ctx: RunContext) => {
        await recordingTurnErrors(() => body(ctx));
        await waitAbort(ctx.signal!);
        throw new Error("aborted mid-turn");
      },
    },
  });
}

/** Every file under `dir` (relative), for an "unchanged over an interval" comparison. */
function listing(dir: string): string[] {
  const out: string[] = [];
  const walk = (d: string): void => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else out.push(`${path.relative(dir, p)}:${fs.statSync(p).size}`);
    }
  };
  walk(dir);
  return out.sort();
}

/** A node script that ignores SIGTERM, optionally creates+holds `lock`, then idles forever. Once its
 *  SIGTERM handler is installed (and the lock held) it creates `<script>.ready` — see isReady. */
function stubbornScript(dir: string, lock?: string): string {
  const p = path.join(dir, `stubborn-${randomBytes(4).toString("hex")}.cjs`);
  fs.writeFileSync(
    p,
    `const fs = require("fs");
process.on("SIGTERM", () => {});
${lock ? `fs.mkdirSync(require("path").dirname(${JSON.stringify(lock)}), { recursive: true });
fs.openSync(${JSON.stringify(lock)}, "wx");` : ""}
fs.writeFileSync(${JSON.stringify(`${p}.ready`)}, String(process.pid));
setInterval(() => {}, 1000);
`,
  );
  return p;
}

/** True once the stubborn script is guaranteed to ignore SIGTERM (so the test really exercises the
 *  SIGKILL escalation rather than racing node's startup). */
const isReady = (script: string): boolean => fs.existsSync(`${script}.ready`);

/** A tick-spawn rewrite: the tick's `git fetch` becomes `script` (run by node). */
function fetchBecomes(script: string): (r: BoundaryProcessRequest) => BoundaryProcessRequest {
  return (r) => (r.argv.includes("fetch") ? { ...r, argv: [process.execPath, script] } : r);
}

async function sleepMs(ms: number): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
}

async function waitFor(pred: () => boolean, ms = 5_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!pred()) {
    if (Date.now() > deadline) throw new Error("waitFor timed out");
    await sleepMs(10);
  }
}

/** How long teardown waits for each phase: execute() resolving, tick settlement, child death. */
const TEARDOWN_MS = 20_000;

async function bounded<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let t: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      p,
      new Promise<never>((_, reject) => {
        t = setTimeout(() => reject(new Error(`teardown: ${what} did not happen within ${ms}ms`)), ms);
      }),
    ]);
  } finally {
    clearTimeout(t);
  }
}

/** True while the process group `pgid` has any member. */
function groupAlive(pgid: number): boolean {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
}

/** SIGKILL each pid's process group, then the pid itself (ESRCH ignored), and wait for them to go. A
 *  pid whose leader and group are both already gone is skipped; that narrows, but does not remove,
 *  the chance of signalling a pid the OS has since reused. */
async function killAndReap(pids: readonly number[]): Promise<void> {
  const live = pids.filter((pid) => isAlive(pid) || groupAlive(pid));
  for (const pid of live) {
    for (const target of [-pid, pid]) {
      try {
        process.kill(target, "SIGKILL");
      } catch {
        /* ESRCH: already gone */
      }
    }
  }
  await waitFor(() => live.every((pid) => !isAlive(pid) && !groupAlive(pid)), TEARDOWN_MS);
}

/**
 * The teardown every runner-driven test with a tick runs in its `finally`, BEFORE any temp dir it
 * (or the harness afterEach, via fx.cleanup) removes is gone:
 *   1. shut down `runners` and await every `running` execute() promise (a rejection is fine);
 *   2. await `ctl.settled()`: no tick is in flight;
 *   3. every tick child (a group leader) must be dead by now, and a live one is reported as a
 *      failure; any leader or group still alive is SIGKILLed and waited for, so no child outlives
 *      the test;
 *   4. `restore` the publish stub, then remove `paths` (both happen even if a phase above failed).
 * A failed phase is recorded in teardownErrors, never thrown, so it cannot mask the test's own error.
 */
async function settleAndClean(spec: {
  ctl?: Ctl;
  runners?: ReadonlyArray<{ shutdown: () => void }>;
  running?: ReadonlyArray<Promise<unknown> | undefined>;
  restore?: () => void;
  paths?: readonly string[];
}): Promise<void> {
  try {
    try {
      for (const r of spec.runners ?? []) r.shutdown();
      await bounded(
        Promise.all((spec.running ?? []).map((p) => p?.catch(() => undefined))),
        TEARDOWN_MS,
        "the runner's execute()",
      );
      await spec.ctl?.settled(TEARDOWN_MS).catch((e: unknown) => {
        throw new Error(`teardown: a started tick never reported its outcome (${String(e)})`);
      });
    } catch (e) {
      teardownErrors.push(e);
    }
    if (spec.ctl) {
      // Settlement is defined on the whole process GROUP (tick-spawner.ts settled()), so ANY live
      // member of a tick child's group is a failure, not only a live leader; it is reaped below.
      const alive = spec.ctl.pids.filter((p) => isAlive(p) || groupAlive(p));
      if (alive.length > 0) {
        teardownErrors.push(new Error(`teardown: tick child process group(s) ${alive.join(", ")} outlived settlement`));
      }
      await killAndReap(spec.ctl.pids).catch((e: unknown) => teardownErrors.push(e));
    }
  } finally {
    spec.restore?.();
    for (const p of spec.paths ?? []) fs.rmSync(p, { recursive: true, force: true });
  }
}

function scratchDir(tag: string): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), `uzi-1597-m2-${tag}-`));
}

// ── (a) cross-worker recovery ────────────────────────────────────────────────────────────────

/** Worker A commits in one long turn; a tick publishes (when enabled) into the fixture origin's
 *  refs/uzi-checkpoints/<branch>; A is then "SIGKILLed" (no shutdown, timers stopped, data dir
 *  deleted). Worker B, on a FRESH data dir, re-claims the run as a resume carrying the recorded
 *  checkpoint tip. Returns whether B's runner clone contains A's commit. */
async function crossWorker(iid: number, tickEnabled: boolean): Promise<{ recovered: boolean; publishes: number }> {
  const tmp = scratchDir("xw");
  const shim = writeShim(tmp, "detect");
  const ctl = control();
  const branch = `agent/issue-${iid}`;
  let recordedTip: string | null = null;
  const pub = stubPublish(async (_n, tipOid, pack) => {
    const bytes = await drain(pack);
    // What the api broker does: land the pack in origin under refs/uzi-checkpoints/<branch>.
    gitIn(fx.originPath, ["index-pack", "--stdin", "--fix-thin"], bytes);
    gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tipOid]);
    recordedTip = tipOid;
    return LANDED;
  });
  const dataA = path.join(tmp, "worker-a");
  const dataB = path.join(tmp, "worker-b");
  const started = deferred();
  const claim = gitlabClaim(iid);
  const runnerA = mkRunner(
    mkGit(dataA, shim),
    blockingTurn(async (ctx) => {
      commitIn(ctx.worktreePath, "A1.txt", "worker A's committed work\n");
      started.resolve();
    }),
    ctl,
    { checkpointTickIntervalMs: tickEnabled ? TICK_MS : 0 },
  );
  const pA = runnerA.execute(claim);
  try {
    await started.promise;
    if (tickEnabled) {
      ctl.advance(INTERVAL_MS + 1);
      assert.equal(await ctl.fire(), "published");
    } else {
      assert.equal(ctl.armed(), false, "no tick is armed when CHECKPOINT_TICK_INTERVAL=0");
    }
    // SIGKILL: no shutdown(); A's timers are never fired again; A's whole data dir is gone.
    fs.rmSync(dataA, { recursive: true, force: true });

    let recovered = false;
    const claimB = gitlabClaim(iid, {
      run_id: claim.run_id,
      session_id: "sess-a",
      checkpoint_tip: recordedTip,
    });
    const runnerB = mkRunner(
      mkGit(dataB, shim),
      turn(async (ctx) => {
        recovered = fs.existsSync(path.join(ctx.worktreePath, "A1.txt"));
        if (!recovered) commitIn(ctx.worktreePath, "B.txt", "b\n"); // keep B's own finalize non-empty
      }),
      undefined,
      { checkpointTickIntervalMs: 0 },
    );
    await runnerB.execute(claimB);
    return { recovered, publishes: pub.count() };
  } finally {
    pub.restore();
    runnerA.shutdown();
    await pA.catch(() => undefined);
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

describe("mid-turn checkpoint tick (issue #1597 M2)", () => {
  it("(a) cross-worker recovery: a tick-published checkpoint lets a fresh worker resume A's commits; with the tick disabled it cannot", async () => {
    const on = await crossWorker(1597_201, true);
    assert.equal(on.publishes, 1, "the tick published once");
    assert.equal(on.recovered, true, "worker B's runner clone contains worker A's mid-turn commit");
    // Paired control: the SAME scenario with CHECKPOINT_TICK_INTERVAL=0 loses the work.
    const off = await crossWorker(1597_202, false);
    assert.equal(off.publishes, 0, "nothing was published without the tick");
    assert.equal(off.recovered, false, "without the tick worker B cold-starts without A's commit");
  });

  it("(b) same-worker adoption: a failed publish still leaves the tick's fetch-back in the bare, which a re-claim adopts", async () => {
    const tmp = scratchDir("same");
    const shim = writeShim(tmp, "detect");
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return { ok: false, httpStatus: 503 };
    });
    const iid = 1597_203;
    const branch = `agent/issue-${iid}`;
    const dataDir = path.join(tmp, "data");
    const g = mkGit(dataDir, shim);
    const claim = gitlabClaim(iid);
    let sha = "";
    const started = deferred();
    const runnerA = mkRunner(
      g,
      blockingTurn(async (ctx) => {
        sha = commitIn(ctx.worktreePath, "SAME.txt", "same-worker work\n");
        started.resolve();
      }),
      ctl,
    );
    const pA = runnerA.execute(claim);
    try {
      await started.promise;
      ctl.advance(INTERVAL_MS + 1);
      assert.equal(await ctl.fire(), "publish_failed:rejected");
      const bare = g.barePathFor(fx.originPath);
      assert.equal(refOr(bare, `refs/uzi-runner/${branch}`), sha, "the tick fetched the commit into the bare");
      // The runner clone is lost (e.g. an emptyDir on a pod roll); the bare survives.
      fs.rmSync(path.join(dataDir, "runner"), { recursive: true, force: true });
      let adopted = false;
      const runnerB = mkRunner(
        mkGit(dataDir, shim),
        turn(async (ctx) => {
          adopted = fs.existsSync(path.join(ctx.worktreePath, "SAME.txt"));
        }),
        undefined,
        { checkpointTickIntervalMs: 0 },
      );
      await runnerB.execute(claim);
      assert.equal(adopted, true, "the re-claim seeded from refs/uzi-runner/<branch> (the tick's fetch-back)");
    } finally {
      pub.restore();
      runnerA.shutdown();
      await pA.catch(() => undefined);
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(c) sink unavailable (HTTP 503): bounded feed line, the next interval retries, a later success emits the recovery line, the run completes", async () => {
    const tmp = scratchDir("503");
    const shim = writeShim(tmp, "detect");
    const ctl = control();
    const pub = stubPublish(async (n, _tip, pack) => {
      await drain(pack);
      return n < 2 ? { ok: false, httpStatus: 503 } : LANDED;
    });
    const claim = gitlabClaim(1597_204);
    const seen: string[] = [];
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "C.txt", "c\n");
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire()); // 503
          seen.push(await ctl.fire()); // time gate closed (attempted this interval)
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire()); // retry: 503 again (deduped)
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire()); // retry: lands
        }),
        ctl,
      ).execute(claim);
      assert.deepEqual(seen, [
        "publish_failed:rejected",
        "time_gate_closed",
        "publish_failed:rejected",
        "published",
      ]);
      assert.equal(pub.count(), 3, "one publish per open interval");
      const feed = statusTexts(claim.run_id);
      assert.deepEqual(
        feed.filter((t) => t.startsWith("checkpoint publish")),
        ["checkpoint publish failed: HTTP 503", "checkpoint publishing recovered — published to origin"],
        JSON.stringify(feed),
      );
      assert.equal(finalStatus(claim.run_id), "completed", "the run was not failed or stalled");
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(c) sink hangs until aborted: the turn ends, teardown aborts the publish, and the run completes normally", async () => {
    const tmp = scratchDir("hang");
    const shim = writeShim(tmp, "detect");
    const ctl = control();
    const entered = deferred();
    const pub = stubPublish(async (_n, _tip, pack, signal) => {
      entered.resolve();
      return hangUntilAborted(pack, signal);
    });
    const claim = gitlabClaim(1597_205);
    const t0 = Date.now();
    let running: Promise<unknown> | undefined;
    try {
      running = mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "H.txt", "h\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await entered.promise; // the tick is now stuck inside the publish RPC
        }),
        ctl,
      ).execute(claim);
      await running;
      assert.deepEqual(ctl.outcomes, ["aborted"]);
      assert.equal(pub.count(), 1);
      assert.ok(Date.now() - t0 < 20_000, "teardown did not wait out the hang");
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.ok(!statusTexts(claim.run_id).some((t) => t.startsWith("checkpoint publish failed")), "an abort is silent");
    } finally {
      await settleAndClean({ ctl, running: [running], restore: pub.restore, paths: [tmp] });
    }
  });

  it("(d) git busy: a fresh AND a 1h-old index.lock both skip without fetching; the deferred line appears once after 3 busy ticks", async () => {
    const tmp = scratchDir("busy");
    const shim = writeShim(tmp, "detect");
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const iid = 1597_206;
    const claim = gitlabClaim(iid);
    const g = mkGit(fx.dataDir, shim);
    const bare = g.barePathFor(fx.originPath);
    const tracking = `refs/uzi-runner/agent/issue-${iid}`;
    const seen: string[] = [];
    try {
      await mkRunner(
        g,
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "D.txt", "d\n");
          ctl.advance(INTERVAL_MS + 1);
          const before = refOr(bare, tracking);
          const lock = path.join(ctx.worktreePath, ".git", "index.lock");
          fs.writeFileSync(lock, "");
          seen.push(await ctl.fire()); // fresh lock
          const old = new Date(Date.now() - 3_600_000);
          fs.utimesSync(lock, old, old);
          seen.push(await ctl.fire()); // 1h-old lock: STILL busy (no staleness heuristic)
          seen.push(await ctl.fire()); // third consecutive busy → the deferred line
          seen.push(await ctl.fire()); // fourth: still deduped
          assert.equal(refOr(bare, tracking), before, "no fetch-back while busy (tracking ref unchanged)");
          fs.rmSync(lock);
          seen.push(await ctl.fire()); // past the probe → publishes
        }),
        ctl,
      ).execute(claim);
      assert.deepEqual(seen, ["git_busy", "git_busy", "git_busy", "git_busy", "published"]);
      const deferredLine = "mid-turn checkpoint deferred: a git operation is in progress in the working tree";
      assert.equal(statusTexts(claim.run_id).filter((t) => t === deferredLine).length, 1);
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(e) a secret committed and removed within one turn: the scan finds it, nothing is published, the fetch-back stays", async () => {
    const tmp = scratchDir("secret");
    const shim = writeShim(tmp, "detect");
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const iid = 1597_207;
    const claim = gitlabClaim(iid);
    const { logger, lines: rawLines } = recordingLogger();
    const lines = rawLines as Array<Record<string, unknown>>;
    const g = mkGit(fx.dataDir, shim);
    const bare = g.barePathFor(fx.originPath);
    let commitB = "";
    let tickOutcome = "";
    let trackingAfterTick: string | null = null;
    let publishesDuringTurn = -1;
    const secret = runtimeSecret();
    try {
      await mkRunner(
        g,
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "config.env", `TOKEN=${secret}\n`);
          commitB = commitIn(ctx.worktreePath, "config.env", "TOKEN=\n");
          ctl.advance(INTERVAL_MS + 1);
          tickOutcome = await ctl.fire();
          publishesDuringTurn = pub.count();
          trackingAfterTick = refOr(bare, `refs/uzi-runner/agent/issue-${iid}`);
        }),
        ctl,
        {},
        logger,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.equal(tickOutcome, "secret_found");
      assert.equal(publishesDuringTurn, 0, "no publishCheckpoint call");
      assert.equal(trackingAfterTick, commitB, "the fetch-back is kept");
      const feed = statusTexts(claim.run_id);
      assert.ok(feed.includes("checkpoint publish skipped: secret_found"), JSON.stringify(feed));
      assert.ok(!JSON.stringify(api.messages(claim.run_id)).includes(secret), "the secret never reaches the feed");
      const warn = lines.find((l) => l.msg === "checkpoint publish skipped: secret_found") as
        | { findings?: Array<{ rule_id: string; path: string }> }
        | undefined;
      assert.equal(warn?.findings?.[0]?.rule_id, "github-pat");
      assert.equal(warn?.findings?.[0]?.path, "config.env");
      assert.ok(!JSON.stringify(lines).includes(secret), "the secret never reaches the run log");
      assert.ok(shimCalls(shim).length >= 1, "the shim scanned");
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(e) an untrusted scan (the scanner exits nonzero) publishes nothing: secret_scan_untrusted", async () => {
    const tmp = scratchDir("untrusted");
    const shim = writeShim(tmp, "fail");
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_208);
    let tickOutcome = "";
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "U.txt", "u\n");
          ctl.advance(INTERVAL_MS + 1);
          tickOutcome = await ctl.fire();
        }),
        ctl,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.equal(tickOutcome, "secret_scan_untrusted");
      assert.equal(shimCalls(shim).length >= 1, true);
      assert.equal(
        pub.tips.length,
        0,
        "no publish while the scanner is broken (the milestone-less run's finalize does not publish checkpoints)",
      );
      assert.ok(statusTexts(claim.run_id).includes("checkpoint publish skipped: secret_scan_untrusted"));
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(e) real gitleaks: a secret committed then removed within the range is found and not published", async (t) => {
    let gitleaks = "";
    try {
      gitleaks = execFileSync("sh", ["-c", "command -v gitleaks"], { encoding: "utf8" }).trim();
    } catch {
      /* not installed */
    }
    if (!gitleaks || !path.isAbsolute(gitleaks)) {
      t.skip("gitleaks is not on PATH in this environment; the shim variants above cover the logic");
      return;
    }
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_209);
    const secret = runtimeSecret();
    let tickOutcome = "";
    let publishesDuringTurn = -1;
    try {
      await mkRunner(
        mkGit(fx.dataDir, gitleaks),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "deploy.sh", `export GITHUB_TOKEN=${secret}\n`);
          commitIn(ctx.worktreePath, "deploy.sh", "export GITHUB_TOKEN=\n");
          ctl.advance(INTERVAL_MS + 1);
          tickOutcome = await ctl.fire();
          publishesDuringTurn = pub.count();
        }),
        ctl,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.equal(tickOutcome, "secret_found");
      assert.equal(publishesDuringTurn, 0);
    } finally {
      pub.restore();
    }
  });

  it("(f) the published pack is exactly the SCANNED range even when origin/<branch> and the tracking ref move between scan and pack", async () => {
    const tmp = scratchDir("pinned");
    const shim = writeShim(tmp, "detect");
    const iid = 1597_210;
    const branch = `agent/issue-${iid}`;
    // origin carries the branch at O1 (so the exclude floor is refs/remotes/origin/<branch>).
    const main = gitIn(fx.originPath, ["rev-parse", "main"]);
    gitIn(fx.originPath, ["checkout", "-q", "-b", branch]);
    const o1 = commitIn(fx.originPath, "O1.txt", "published O1\n");
    gitIn(fx.originPath, ["checkout", "-q", "main"]);
    const g = mkGit(fx.dataDir, shim);
    const bare = (): string => g.barePathFor(fx.originPath);
    let scanned: { tipSha: string; excludeSha: string } | undefined;
    let c2 = "";
    const ctl = control({
      afterCheckpointScan: async ({ range }) => {
        scanned = { ...range };
        // Widen: move the exclude floor BACK below O1 (O1 was never scanned) …
        gitIn(bare(), ["update-ref", `refs/remotes/origin/${branch}`, main]);
        // … and advance the tracking ref to a NEW unscanned commit.
        const tree = gitIn(bare(), ["rev-parse", `${range.tipSha}^{tree}`]);
        c2 = gitIn(bare(), [...IDENT, "commit-tree", tree, "-p", range.tipSha, "-m", "unscanned C2"]);
        gitIn(bare(), ["update-ref", `refs/uzi-runner/${branch}`, c2]);
      },
    });
    let packBytes: Buffer | undefined;
    const pub = stubPublish(async (_n, _tip, pack) => {
      packBytes = await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(iid);
    let c1 = "";
    try {
      await mkRunner(
        g,
        turn(async (ctx) => {
          c1 = commitIn(ctx.worktreePath, "C1.txt", "c1\n");
          ctl.advance(INTERVAL_MS + 1);
          assert.equal(await ctl.fire(), "published");
        }),
        ctl,
      ).execute(claim);
      assert.deepEqual(scanned && { tipSha: scanned.tipSha, excludeSha: scanned.excludeSha }, { tipSha: c1, excludeSha: o1 }, "the scan walked O1..C1");
      assert.equal(pub.tips[0], c1, "the declared tip is the SCANNED tip, not the moved tracking ref");
      // index-pack the captured pack into a scratch repo and list its commits.
      const scratch = path.join(tmp, "scratch");
      execFileSync("git", ["init", "-q", "--bare", scratch], { env: GIT_ENV });
      gitIn(scratch, ["index-pack", "--stdin"], packBytes!);
      const commits = gitIn(scratch, ["cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype)"])
        .split("\n")
        .filter((l) => l.endsWith(" commit"))
        .map((l) => l.split(" ")[0]);
      assert.deepEqual(commits, [c1], "the pack carries exactly excludeSha..tipSha = {C1} (neither O1 nor C2)");
      assert.ok(c2 && !commits.includes(c2) && !commits.includes(o1));
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(g) sink gate: a tick firing while a pause park holds the gate (wip marker committed) skips gate_busy and never publishes the marker", async () => {
    const tmp = scratchDir("gate");
    const shim = writeShim(tmp, "detect");
    const ctl = control();
    let tickOutcome = "";
    const pub = stubPublish(async (n, _tip, pack) => {
      await drain(pack);
      if (n === 0) {
        // The pause path is INSIDE its publish, holding the gate with a wip marker committed.
        ctl.advance(INTERVAL_MS + 1);
        tickOutcome = await ctl.fire();
        return { ok: false, httpStatus: 500 }; // the pause fails → the marker is undone
      }
      return LANDED;
    });
    const claim = gitlabClaim(1597_211);
    let parked: boolean | undefined;
    let headSubject = "";
    let running: Promise<unknown> | undefined;
    try {
      running = mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "G.txt", "g\n");
          fs.writeFileSync(path.join(ctx.worktreePath, "WIP.txt"), "uncommitted\n");
          parked = await ctx.parkForPause!({ completedCount: 0 });
          headSubject = gitIn(ctx.worktreePath, ["log", "-1", "--format=%s"]);
        }),
        ctl,
      ).execute(claim);
      await running;
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.equal(parked, false);
      assert.equal(headSubject.startsWith("wip(park):"), false, "the marker was undone");
      assert.equal(tickOutcome, "gate_busy");
      assert.equal(pub.count(), 1, "only the pause's own publish ran — the tick never published the marker");
    } finally {
      await settleAndClean({ ctl, running: [running], restore: pub.restore, paths: [tmp] });
    }
  });

  it("(g) sink gate: a milestone checkpoint PREEMPTS a tick stuck in its publish and waits for it to settle", async () => {
    const tmp = scratchDir("preempt");
    const shim = writeShim(tmp, "detect");
    const ctl = control();
    const events: string[] = [];
    const entered = deferred();
    const pub = stubPublish(async (n, _tip, pack, signal) => {
      if (n === 0) {
        events.push("tick:publish");
        entered.resolve();
        try {
          return await hangUntilAborted(pack, signal);
        } finally {
          events.push("tick:publish-aborted");
        }
      }
      await drain(pack);
      events.push(`milestone:publish (tick children alive: ${ctl.pids.filter(isAlive).length})`);
      return LANDED;
    });
    const claim = gitlabClaim(1597_212);
    let running: Promise<unknown> | undefined;
    try {
      running = mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "P.txt", "p\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await entered.promise;
          await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
          events.push(`milestone:done (tick outcome: ${ctl.outcomes.join(",")})`);
        }),
        ctl,
      ).execute(claim);
      await running;
      assert.deepEqual(events, [
        "tick:publish",
        "tick:publish-aborted",
        "milestone:publish (tick children alive: 0)",
        "milestone:done (tick outcome: aborted)",
      ]);
    } finally {
      await settleAndClean({ ctl, running: [running], restore: pub.restore, paths: [tmp] });
    }
  });
});

// ── (h) quiescence ──────────────────────────────────────────────────────────────────────────

type Stuck = "fetch_child" | "bare_lock" | "publish";

async function quiescence(iid: number, stuck: Stuck, teardown: "turn_end" | "shutdown"): Promise<void> {
  const tmp = scratchDir(`q-${stuck}`);
  const shim = writeShim(tmp, "detect");
  const g = mkGit(fx.dataDir, shim);
  const bare = g.barePathFor(fx.originPath);
  const branch = `agent/issue-${iid}`;
  const hang = stubbornScript(tmp);
  let releaseLock: (() => void) | undefined;
  const ctl = control(
    stuck === "fetch_child" ? { tickSpawn: { rewrite: fetchBecomes(hang) } } : {},
  );
  if (stuck === "bare_lock") {
    // Release the held bare lock only once the tick has SETTLED (its outcome recorded), so the
    // shutdown sink's own fetch-back can proceed afterwards.
    const orig = ctl.hooks.onTickOutcome!;
    ctl.hooks.onTickOutcome = (o) => {
      orig(o);
      releaseLock?.();
    };
  }
  const stuckIn = deferred();
  const pub = stubPublish(async (n, _tip, pack, signal) => {
    if (stuck === "publish" && n === 0) {
      stuckIn.resolve();
      return hangUntilAborted(pack, signal);
    }
    await drain(pack);
    return LANDED;
  });
  const claim = gitlabClaim(iid);
  let sha = "";
  const body = async (ctx: RunContext): Promise<void> => {
    sha = commitIn(ctx.worktreePath, "Q.txt", "q\n");
    ctl.advance(INTERVAL_MS + 1);
    if (stuck === "bare_lock") {
      void g.withBareLock(bare, () => new Promise<void>((r) => (releaseLock = r)));
    }
    ctl.fireNoWait();
    if (stuck === "fetch_child") {
      await waitFor(() => isReady(hang));
    } else if (stuck === "publish") {
      await stuckIn.promise;
    } else {
      await sleepMs(200); // the tick is now queued behind the held bare lock
    }
  };
  const factory = teardown === "turn_end" ? turn(body) : blockingTurn(body);
  const runner = mkRunner(g, factory, ctl);
  const started = Date.now();
  let p: Promise<unknown> | undefined;
  try {
    p = runner.execute(claim);
    if (teardown === "shutdown") {
      await waitFor(() => sha !== "" && (stuck !== "fetch_child" || isReady(hang)));
      if (stuck === "publish") await stuckIn.promise;
      else await sleepMs(200);
      runner.shutdown();
    }
    await p;
    // Every tick child is gone, the timer is disarmed, and nothing spawns or publishes afterwards.
    for (const pid of ctl.pids) assert.equal(isAlive(pid), false, `tick pid ${pid} is gone (ESRCH)`);
    assert.equal(ctl.armed(), false, "the tick timer was cancelled");
    assert.deepEqual(ctl.outcomes, ["aborted"]);
    const spawnsAfter = ctl.pids.length;
    const publishesAfter = pub.count();
    const refs = gitIn(bare, ["for-each-ref"]);
    const files = listing(bare);
    await sleepMs(400);
    assert.equal(ctl.pids.length, spawnsAfter, "no tick spawn after teardown");
    assert.equal(pub.count(), publishesAfter, "no publish after teardown");
    assert.equal(gitIn(bare, ["for-each-ref"]), refs, "bare refs unchanged over a following interval");
    assert.deepEqual(listing(bare), files, "bare file listing unchanged over a following interval");
    assert.ok(Date.now() - started < 30_000, "teardown settled within budget");
    const feed = statusTexts(claim.run_id);
    if (teardown === "shutdown") {
      assert.ok(feed.includes("shutdown checkpoint published to origin"), JSON.stringify(feed));
      assert.equal(refOr(bare, `refs/uzi-runner/${branch}`), sha, "the shutdown fetch-back landed the commit");
    } else {
      assert.equal(finalStatus(claim.run_id), "completed");
    }
  } finally {
    releaseLock?.();
    await settleAndClean({ ctl, runners: [runner], running: [p], restore: pub.restore, paths: [tmp] });
  }
}

describe("mid-turn checkpoint quiescence (issue #1597 M2)", () => {
  it("(h) a hung tick fetch child (ignores SIGTERM) is killed and settled before the turn's teardown continues", async () => {
    await quiescence(1597_220, "fetch_child", "turn_end");
  });
  it("(h) shutdown: a hung tick fetch child is settled before the shutdown fetch-back/publish, which then succeed", async () => {
    await quiescence(1597_221, "fetch_child", "shutdown");
  });
  it("(h) shutdown: a tick waiting behind a held bare lock abandons the wait and settles first", async () => {
    await quiescence(1597_222, "bare_lock", "shutdown");
  });
  it("(h) shutdown: a tick stuck in its publish is aborted and settled first", async () => {
    await quiescence(1597_223, "publish", "shutdown");
  });
  it("(h) turn end: a tick stuck in its publish is aborted and settled first", async () => {
    await quiescence(1597_224, "publish", "turn_end");
  });
});

// ── (i) lock ownership ──────────────────────────────────────────────────────────────────────

/** Proven-ownership lock removal, and the `foreign` / `inode_mismatch` reasons for a SIGKILLed
 *  child, need the /proc fd evidence tick-spawner.ts reads only on Linux. Elsewhere every such lock
 *  is retained as `no_proc_evidence`, by design, so these cases do not apply. */
const NEEDS_PROC: string | false =
  process.platform !== "linux"
    ? "lock-ownership proof reads /proc (Linux only); non-Linux retains every lock as no_proc_evidence by design"
    : false;

describe("mid-turn checkpoint lock custody (issue #1597 M2)", () => {
  it("(i) proven-owned: the SIGKILLed fetch child's own refs/uzi-runner lock is removed and the next fetch-back works", { skip: NEEDS_PROC }, async () => {
    const tmp = scratchDir("owned");
    const shim = writeShim(tmp, "detect");
    const iid = 1597_230;
    const branch = `agent/issue-${iid}`;
    const g = mkGit(fx.dataDir, shim);
    const bare = g.barePathFor(fx.originPath);
    const lock = path.join(bare, "refs", "uzi-runner", "agent", `issue-${iid}.lock`);
    const holder = stubbornScript(tmp, lock);
    const ctl = control({ tickSpawn: { rewrite: fetchBecomes(holder) } });
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const { logger, lines: rawLines } = recordingLogger();
    const lines = rawLines as Array<Record<string, unknown>>;
    const claim = gitlabClaim(iid);
    let sha = "";
    let runner: RunRunner | undefined;
    let p: Promise<unknown> | undefined;
    try {
      runner = mkRunner(
        g,
        blockingTurn(async (ctx) => {
          sha = commitIn(ctx.worktreePath, "I.txt", "i\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await waitFor(() => fs.existsSync(lock));
        }),
        ctl,
        {},
        logger,
      );
      p = runner.execute(claim);
      await waitFor(() => fs.existsSync(lock));
      runner.shutdown();
      await p;
      assert.deepEqual(ctl.outcomes, ["aborted"]);
      assert.equal(fs.existsSync(lock), false, "the proven-owned lock was removed");
      assert.ok(lines.some((l) => l.msg === "mid-turn checkpoint removed a lock file its cancelled child provably owned"));
      assert.equal(refOr(bare, `refs/uzi-runner/${branch}`), sha, "the shutdown fetch-back then worked");
      assert.ok(statusTexts(claim.run_id).includes("shutdown checkpoint published to origin"));
    } finally {
      await settleAndClean({ ctl, runners: runner ? [runner] : [], running: [p], restore: pub.restore, paths: [tmp] });
    }
  });

  it("(i) foreign: a lock a separate process created during the cancellation is RETAINED; later ticks skip; shutdown names bare_lock_retained", { skip: NEEDS_PROC }, async () => {
    const tmp = scratchDir("foreign");
    const shim = writeShim(tmp, "detect");
    const iid = 1597_231;
    const g = mkGit(fx.dataDir, shim);
    const bare = g.barePathFor(fx.originPath);
    const foreignLock = path.join(bare, "packed-refs.lock");
    const stubborn = stubbornScript(tmp);
    const ctl = control({ tickSpawn: { rewrite: fetchBecomes(stubborn) }, tickKillGraceMs: 500 });
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return { ok: false, httpStatus: 503 }; // the shutdown publish does not land
    });
    const { logger, lines: rawLines } = recordingLogger();
    const lines = rawLines as Array<Record<string, unknown>>;
    const claim = gitlabClaim(iid);
    const later: string[] = [];
    let runner: RunRunner | undefined;
    let p: Promise<unknown> | undefined;
    try {
      runner = mkRunner(
        g,
        blockingTurn(async (ctx) => {
          commitIn(ctx.worktreePath, "F.txt", "f\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await waitFor(() => isReady(stubborn));
          // A gated path preempts the tick; DURING the child's cancellation a SEPARATE process
          // creates packed-refs.lock (the child never opens it).
          const milestone = ctx.checkpoint!({ reap: false });
          await new Promise<void>((resolve, reject) => {
            const c = spawn(process.execPath, ["-e", `require("fs").writeFileSync(${JSON.stringify(foreignLock)}, "", { flag: "wx" })`]);
            c.once("exit", (code) => (code === 0 ? resolve() : reject(new Error(`exit ${code}`))));
          });
          await milestone;
          assert.equal(ctl.outcomes[0], "bare_lock_retained");
          ctl.advance(INTERVAL_MS + 1);
          later.push(await ctl.fire());
        }),
        ctl,
        {},
        logger,
      );
      p = runner.execute(claim);
      await waitFor(() => later.length === 1, 20_000);
      runner.shutdown();
      await p;
      assert.deepEqual(later, ["bare_lock_retained"], "later ticks skip");
      assert.ok(fs.existsSync(foreignLock), "the foreign lock was never deleted");
      const warn = lines.find((l) => l.msg === "mid-turn checkpoint retained a git lock file in the worker bare") as
        | { retained?: Array<Record<string, unknown>> }
        | undefined;
      const ev = warn?.retained?.find((r) => String(r.path).endsWith("packed-refs.lock"));
      assert.ok(ev, JSON.stringify(warn));
      assert.equal(ev.reason, "foreign");
      assert.equal(ev.argv_class, "git fetch", "the argv class of the tick's (rewritten) fetch — no paths/credentials");
      assert.deepEqual(ev.pre_spawn, { present: false });
      for (const k of ["dev", "ino", "size", "mtime_ms"]) assert.equal(typeof ev[k], "number", k);
      const feed = statusTexts(claim.run_id);
      assert.equal(
        feed.filter((t) => t === "mid-turn checkpoint sink blocked: a git lock file remains in the worker repository").length,
        1,
        JSON.stringify(feed),
      );
      assert.ok(
        feed.some((t) => t.startsWith("shutdown checkpoint NOT published (reason: bare_lock_retained)")),
        JSON.stringify(feed),
      );
    } finally {
      await settleAndClean({ ctl, runners: runner ? [runner] : [], running: [p], restore: pub.restore, paths: [tmp] });
    }
  });

  it("(i) survivor: a tick process group that outlives its SIGKILL blocks ticks, delays gated sinks, and unblocks once gone", async () => {
    const tmp = scratchDir("survivor");
    const shim = writeShim(tmp, "detect");
    const iid = 1597_235;
    const g = mkGit(fx.dataDir, shim);
    const stubborn = stubbornScript(tmp);
    // No real process survives SIGKILL: force the spawner's group probe to say "alive" for the
    // cancelled fetch child, so settlement gives up after its bounded wait and reports it.
    let forceAlive = true;
    const ctl = control({
      tickSpawn: { rewrite: fetchBecomes(stubborn), groupAlive: () => (forceAlive ? true : undefined) },
      tickKillGraceMs: 300,
    });
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return { ok: true };
    });
    const { logger, lines: rawLines } = recordingLogger();
    const lines = rawLines as Array<Record<string, unknown>>;
    const claim = gitlabClaim(iid);
    const later: string[] = [];
    let runner: RunRunner | undefined;
    let p: Promise<unknown> | undefined;
    try {
      runner = mkRunner(
        g,
        blockingTurn(async (ctx) => {
          commitIn(ctx.worktreePath, "S.txt", "s\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await waitFor(() => isReady(stubborn));
          // The milestone preempts the tick (cancelled); the gated milestone sink then WAITS, bounded,
          // for the (forced) survivor before touching the clone, and proceeds with a logged residual.
          const t0 = Date.now();
          await ctx.checkpoint!({ reap: false });
          assert.equal(ctl.outcomes[0], "tick_process_survived", "a survivor names its own tick's class");
          assert.ok(Date.now() - t0 >= 1_500, "the gated sink waited for the survivor");
          assert.ok(
            lines.some((l) => l.msg === "durable sink proceeding while a surviving tick process group is still alive"),
            "the gated sink logged the residual",
          );
          // Still alive: a later tick skips on the survivor.
          ctl.advance(INTERVAL_MS + 1);
          later.push(await ctl.fire());
          // Gone (the flight's probe now falls through to the REAL one: the SIGKILLed child is dead).
          forceAlive = false;
          ctl.advance(INTERVAL_MS + 1);
          later.push(await ctl.fire());
        }),
        ctl,
        {},
        logger,
      );
      p = runner.execute(claim);
      await waitFor(() => later.length === 2, 20_000);
      runner.shutdown();
      await p;
      assert.equal(later[0], "tick_process_survived", "a later tick skips while the survivor lives");
      assert.notEqual(later[1], "tick_process_survived", "the sink unblocks once the surviving group is gone");
      assert.ok(
        lines.some((l) => l.msg === "mid-turn checkpoint: a tick process group survived its SIGKILL; blocking the sink"),
        "the survivor was logged",
      );
      assert.ok(
        lines.some((l) => l.msg === "mid-turn checkpoint sink unblocked: the surviving tick process group(s) are gone"),
        "the unblock was logged",
      );
      const feed = statusTexts(claim.run_id);
      assert.equal(
        feed.filter((t) => t === "mid-turn checkpoint sink blocked: a checkpoint process survived being stopped").length,
        1,
        JSON.stringify(feed),
      );
    } finally {
      forceAlive = false;
      await settleAndClean({ ctl, runners: runner ? [runner] : [], running: [p], restore: pub.restore, paths: [tmp] });
    }
  });

  it("(i) replaced: the child held the lock but after its exit the path is a different inode — RETAINED", { skip: NEEDS_PROC }, async () => {
    const tmp = scratchDir("replaced");
    const shim = writeShim(tmp, "detect");
    const iid = 1597_232;
    const g = mkGit(fx.dataDir, shim);
    const bare = g.barePathFor(fx.originPath);
    const lock = path.join(bare, "refs", "uzi-runner", "agent", `issue-${iid}.lock`);
    const ctl = control({
      tickSpawn: {
        rewrite: fetchBecomes(stubbornScript(tmp, lock)),
        beforeReconcile: async () => {
          const next = `${lock}.next`;
          fs.writeFileSync(next, "someone else");
          fs.renameSync(next, lock);
        },
      },
    });
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const { logger, lines: rawLines } = recordingLogger();
    const lines = rawLines as Array<Record<string, unknown>>;
    const claim = gitlabClaim(iid);
    let runner: RunRunner | undefined;
    let p: Promise<unknown> | undefined;
    try {
      runner = mkRunner(
        g,
        blockingTurn(async (ctx) => {
          commitIn(ctx.worktreePath, "R.txt", "r\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await waitFor(() => fs.existsSync(lock));
        }),
        ctl,
        {},
        logger,
      );
      p = runner.execute(claim);
      await waitFor(() => fs.existsSync(lock));
      runner.shutdown();
      await p;
      assert.deepEqual(ctl.outcomes, ["bare_lock_retained"]);
      assert.ok(fs.existsSync(lock), "a replaced inode is never deleted");
      const warn = lines.find((l) => l.msg === "mid-turn checkpoint retained a git lock file in the worker bare") as
        | { retained?: Array<{ reason: string }> }
        | undefined;
      assert.equal(warn?.retained?.[0]?.reason, "inode_mismatch");
    } finally {
      await settleAndClean({ ctl, runners: runner ? [runner] : [], running: [p], restore: pub.restore, paths: [tmp] });
    }
  });
});

// ── GitCache unit coverage for the M2 primitives ───────────────────────────────────────────

describe("GitCache mid-turn primitives (issue #1597 M2)", () => {
  function bareWithBranch(): { g: GitCache; bare: string; branch: string; tip: string; base: string } {
    const g = mkGit(fx.dataDir);
    const branch = "agent/issue-1";
    const base = gitIn(fx.originPath, ["rev-parse", "main"]);
    const bareDir = path.join(fx.dataDir, "unit.git");
    execFileSync("git", ["clone", "-q", "--bare", fx.originPath, bareDir], { env: GIT_ENV });
    gitIn(bareDir, ["update-ref", `refs/remotes/origin/main`, base]);
    const tree = gitIn(bareDir, ["rev-parse", `${base}^{tree}`]);
    const tip = gitIn(bareDir, [...IDENT, "commit-tree", tree, "-p", base, "-m", "tip"]);
    gitIn(bareDir, ["update-ref", `refs/uzi-runner/${branch}`, tip]);
    return { g, bare: bareDir, branch, tip, base };
  }

  it("checkpointPack with `pinned` writes the SHAs (not ref names) to pack-objects stdin and declares the pinned tip", async () => {
    const { g, bare, branch, tip, base } = bareWithBranch();
    const stdins: string[] = [];
    const argvs: string[][] = [];
    const ac = new AbortController();
    const out = await g.withBoundaryProcessSpawner(
      async (req) => {
        argvs.push([...req.argv]);
        const stdin = new PassThrough();
        let buf = "";
        stdin.on("data", (c: Buffer) => (buf += String(c)));
        stdin.on("end", () => stdins.push(buf));
        const stdout = new PassThrough();
        stdout.end();
        return { stdin, stdout, stderr: new PassThrough().end() as unknown as PassThrough, completed: Promise.resolve({ code: 0 }) };
      },
      ac.signal,
      () => g.checkpointPack(bare, branch, undefined, { tipSha: tip, excludeSha: base }),
    );
    await new Promise((r) => setImmediate(r)); // let the stdin 'end' event land
    assert.equal(out?.tipOid, tip);
    assert.deepEqual(stdins, [`${tip}\n^${base}\n`], "exactly the pinned SHAs, no ref names");
    assert.equal(argvs.length, 1, "no ref was re-resolved (a single pack-objects child)");
    assert.ok(argvs[0]!.includes("pack-objects"));
    await assert.rejects(g.checkpointPack(bare, branch, undefined, { tipSha: "main", excludeSha: base }), /40-hex/);
  });

  it("resolveCheckpointRange pins the tracking tip and the SAME floor checkpointPack excludes", async () => {
    const { g, bare, branch, tip, base } = bareWithBranch();
    assert.deepEqual(await g.resolveCheckpointRange(bare, branch), { tipSha: tip, excludeSha: base, scanFloorShas: [] });
    assert.equal(await g.resolveCheckpointRange(bare, "agent/none"), null, "no tracking ref → null");
  });

  it("secretScanCheckpointRange: an empty range is trusted-clean without running the scanner; a bad SHA is untrusted", async () => {
    const tmp = scratchDir("unitscan");
    try {
      const shim = writeShim(tmp, "fail");
      const g = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: shim });
      const { bare, base } = bareWithBranch();
      assert.deepEqual(await g.secretScanCheckpointRange(bare, { tipSha: base, excludeSha: base }), {
        trusted: true,
        findings: [],
      });
      assert.equal(shimCalls(shim).length, 0);
      assert.deepEqual(await g.secretScanCheckpointRange(bare, { tipSha: "HEAD", excludeSha: base }), {
        trusted: false,
        findings: [],
        reason: "range_invalid",
      });
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("runnerCloneBusy: markers in a .git dir, a .git FILE's gitdir, and an unreadable probe", async () => {
    const g = mkGit(fx.dataDir);
    const clone = path.join(fx.dataDir, "probe-clone");
    execFileSync("git", ["init", "-q", clone], { env: GIT_ENV });
    assert.deepEqual(await g.runnerCloneBusy(clone, "agent/x"), { busy: false, markers: [] });
    fs.mkdirSync(path.join(clone, ".git", "rebase-merge"));
    fs.mkdirSync(path.join(clone, ".git", "refs", "heads", "agent"), { recursive: true });
    fs.writeFileSync(path.join(clone, ".git", "refs", "heads", "agent", "x.lock"), "");
    assert.deepEqual(await g.runnerCloneBusy(clone, "agent/x"), {
      busy: true,
      markers: ["rebase-merge/", "refs/heads/agent/x.lock"],
    });
    // A `.git` FILE pointing elsewhere (a linked worktree / separate gitdir).
    const linked = path.join(fx.dataDir, "linked");
    const gitdir = path.join(fx.dataDir, "separate-gitdir");
    fs.mkdirSync(linked);
    fs.mkdirSync(gitdir);
    fs.writeFileSync(path.join(linked, ".git"), `gitdir: ${path.relative(linked, gitdir)}\n`);
    fs.writeFileSync(path.join(gitdir, "MERGE_HEAD"), "x");
    assert.deepEqual(await g.runnerCloneBusy(linked, "b"), { busy: true, markers: ["MERGE_HEAD"] });
    assert.deepEqual(await g.runnerCloneBusy(path.join(fx.dataDir, "missing"), "b"), {
      busy: true,
      markers: ["unreadable"],
    });
  });
});

// ── review follow-ups (issue #1597 M2 rework) ────────────────────────────────────────────────

const execFileP = promisify(execFile);

/** The absolute gitleaks on PATH, or "" (then the real-binary variants skip explicitly). */
function realGitleaks(): string {
  try {
    const p = execFileSync("sh", ["-c", "command -v gitleaks"], { encoding: "utf8" }).trim();
    return path.isAbsolute(p) ? p : "";
  } catch {
    return "";
  }
}

/** Resolve with `p`, or reject after `ms` — a probe that must never block. */
function within<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let t: ReturnType<typeof setTimeout> | undefined;
  return Promise.race([
    p.finally(() => clearTimeout(t)),
    new Promise<T>((_, reject) => {
      t = setTimeout(() => reject(new Error(`${what} did not complete within ${ms}ms`)), ms);
    }),
  ]);
}

describe("mid-turn checkpoint review follow-ups (issue #1597 M2)", () => {
  // ── item 2: the busy probe never blocks on an agent-controlled `.git` ────────────────────
  it("(item 2) runnerCloneBusy: a FIFO or a symlink at `.git` is `unreadable` at once (never a blocking open)", async () => {
    const g = mkGit(fx.dataDir);
    const clone = path.join(fx.dataDir, "fifo-clone");
    fs.mkdirSync(clone);
    execFileSync("mkfifo", [path.join(clone, ".git")]);
    assert.deepEqual(await within(g.runnerCloneBusy(clone, "b"), 3_000, "probe of a FIFO .git"), {
      busy: true,
      markers: ["unreadable"],
    });
    const linked = path.join(fx.dataDir, "symlink-clone");
    fs.mkdirSync(linked);
    fs.writeFileSync(path.join(fx.dataDir, "gitfile"), "gitdir: /nonexistent\n");
    fs.symlinkSync(path.join(fx.dataDir, "gitfile"), path.join(linked, ".git"));
    assert.deepEqual(await g.runnerCloneBusy(linked, "b"), { busy: true, markers: ["unreadable"] });
  });

  it("(item 2) runnerCloneBusy: a `.git` racing between a gitfile and a FIFO never parks the probe", async () => {
    const g = mkGit(fx.dataDir);
    const clone = path.join(fx.dataDir, "race-clone");
    const gitdir = path.join(fx.dataDir, "race-gitdir");
    fs.mkdirSync(clone);
    fs.mkdirSync(gitdir);
    const dotgit = path.join(clone, ".git");
    let done = false;
    const swapper = (async () => {
      while (!done) {
        await fs.promises.rm(dotgit, { force: true });
        await execFileP("mkfifo", [dotgit]);
        await new Promise((r) => setImmediate(r));
        await fs.promises.rm(dotgit, { force: true });
        await fs.promises.writeFile(dotgit, `gitdir: ${gitdir}\n`);
        await new Promise((r) => setImmediate(r));
      }
    })();
    try {
      for (let i = 0; i < 40; i++) {
        const r = await within(g.runnerCloneBusy(clone, "b"), 3_000, `racing probe #${i}`);
        assert.ok(r.markers.length === 0 || r.markers.includes("unreadable"), JSON.stringify(r));
      }
    } finally {
      done = true;
      await swapper;
    }
  });

  it("(item 2) runner: a FIFO at the runner clone's `.git` makes the tick `git_busy` and the run still finishes", async () => {
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_240);
    let outcome = "";
    try {
      await within(
        mkRunner(
          mkGit(fx.dataDir),
          turn(async (ctx) => {
            commitIn(ctx.worktreePath, "FIFO.txt", "f\n");
            const dotgit = path.join(ctx.worktreePath, ".git");
            fs.renameSync(dotgit, `${dotgit}.real`);
            execFileSync("mkfifo", [dotgit]);
            ctl.advance(INTERVAL_MS + 1);
            outcome = await ctl.fire();
            fs.rmSync(dotgit);
            fs.renameSync(`${dotgit}.real`, dotgit);
          }),
          ctl,
        ).execute(claim),
        60_000,
        "the run",
      );
      assert.equal(outcome, "git_busy");
      assert.equal(finalStatus(claim.run_id), "completed");
    } finally {
      pub.restore();
    }
  });

  it("(items 2/4) a tick held in its pre-scope phase never delays teardown, and never runs after it", async () => {
    let probeCalls = 0;
    const ctl = control({
      beforeBusyProbe: () => {
        probeCalls++;
        return new Promise<void>(() => {}); // never resolves: a stuck pre-scope phase
      },
    });
    const claim = gitlabClaim(1597_241);
    const t0 = Date.now();
    await within(
      mkRunner(
        mkGit(fx.dataDir),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "S.txt", "s\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await waitFor(() => probeCalls === 1);
        }),
        ctl,
      ).execute(claim),
      20_000,
      "teardown with a stuck pre-scope tick",
    );
    assert.ok(Date.now() - t0 < 20_000);
    assert.deepEqual(ctl.outcomes, ["aborted"]);
    assert.equal(ctl.pids.length, 0, "the stopped tick spawned nothing");
    assert.equal(finalStatus(claim.run_id), "completed");
  });

  // ── item 3: bookkeeping follows the PUBLISHED pinned tip ─────────────────────────────────
  it("(item 3) an agent reset+commit between the body's first read and the fetch: the published tip is recorded, no republish, no false steer", async () => {
    const iid = 1597_242;
    const branch = `agent/issue-${iid}`;
    gitIn(fx.originPath, ["checkout", "-q", "-b", branch]);
    const p = commitIn(fx.originPath, "P.txt", "published P\n");
    gitIn(fx.originPath, ["checkout", "-q", "main"]);
    let clonePath = "";
    let rewritten = "";
    const ctl = control({
      tickSpawn: {
        // Just before the tick's fetch child starts (AFTER the body read cloneTip = C1), the agent
        // drops C1 and commits C2' on P — the reviewer's window.
        rewrite: (r) => {
          if (r.argv.includes("fetch") && !rewritten) {
            gitIn(clonePath, ["reset", "-q", "--hard", p]);
            rewritten = commitIn(clonePath, "C2.txt", "c2 replaces c1\n");
          }
          return r;
        },
      },
    });
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(iid);
    const seen: string[] = [];
    try {
      await mkRunner(
        mkGit(fx.dataDir),
        turn(async (ctx) => {
          clonePath = ctx.worktreePath;
          commitIn(ctx.worktreePath, "C1.txt", "c1 (about to be dropped)\n");
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire());
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire());
        }),
        ctl,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.deepEqual(seen, ["published", "no_new_work"], "the second tick sees the PUBLISHED tip as current");
      assert.deepEqual(pub.tips, [rewritten], "one publish, of the tip the fetch actually brought");
      const feed = statusTexts(claim.run_id);
      assert.ok(!feed.some((t) => t.includes("history was rewritten")), `no false #1416 steer: ${JSON.stringify(feed)}`);
    } finally {
      pub.restore();
    }
  });

  // ── item 5: merges ─────────────────────────────────────────────────────────────────────
  async function mergeRun(gitleaksBin: string | undefined): Promise<{ seen: string[]; tips: number }> {
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_243);
    const seen: string[] = [];
    try {
      await mkRunner(
        mkGit(fx.dataDir, gitleaksBin),
        turn(async (ctx) => {
          const w = ctx.worktreePath;
          // A PLAIN merge: side + main-line commits, merged with no extra content.
          gitIn(w, ["checkout", "-q", "-b", "side"]);
          commitIn(w, "side.txt", "side\n");
          gitIn(w, ["checkout", "-q", ctx.branch]);
          commitIn(w, "line.txt", "line\n");
          gitIn(w, [...IDENT, "merge", "-q", "--no-ff", "-m", "plain merge", "side"]);
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire());
          // An EVIL merge: the secret exists ONLY in the merge commit (in neither parent).
          gitIn(w, ["checkout", "-q", "side"]);
          commitIn(w, "side2.txt", "side2\n");
          gitIn(w, ["checkout", "-q", ctx.branch]);
          gitIn(w, [...IDENT, "merge", "-q", "--no-ff", "--no-commit", "side"]);
          fs.writeFileSync(path.join(w, "evil.env"), `TOKEN=${runtimeSecret()}\n`);
          gitIn(w, ["add", "evil.env"]);
          gitIn(w, [...IDENT, "commit", "-q", "-m", "evil merge"]);
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire());
        }),
        ctl,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      return { seen, tips: pub.count() };
    } finally {
      pub.restore();
    }
  }

  it("(item 5) shim: a plain merge publishes; an evil merge (secret only in the merge commit) is secret_found", async () => {
    const r = await mergeRun(undefined);
    assert.deepEqual(r.seen, ["published", "secret_found"]);
    assert.equal(r.tips, 1);
  });

  it("(item 5) real gitleaks: a plain merge publishes; an evil merge is secret_found", async (t) => {
    const gl = realGitleaks();
    if (!gl) {
      t.skip("gitleaks is not on PATH in this environment; the shim variant above covers the logic");
      return;
    }
    const r = await mergeRun(gl);
    assert.deepEqual(r.seen, ["published", "secret_found"]);
    assert.equal(r.tips, 1);
  });

  // ── item 6: size cap ───────────────────────────────────────────────────────────────────
  it("(item 6) a range with a blob over the per-blob cap is untrusted `scan_too_large` without running the scanner", async () => {
    const tmp = scratchDir("toolarge");
    try {
      const shim = writeShim(tmp, "detect");
      const { logger, lines } = recordingLogger();
      const g = new GitCache(fx.dataDir, logger, undefined, { gitleaksBin: shim });
      const bare = path.join(fx.dataDir, "big.git");
      execFileSync("git", ["clone", "-q", "--bare", fx.originPath, bare], { env: GIT_ENV });
      const base = gitIn(bare, ["rev-parse", "main"]);
      const blob = gitIn(bare, ["hash-object", "-w", "--stdin"], randomBytes(9 * 1024 * 1024));
      const tree = gitIn(bare, ["mktree"], `100644 blob ${blob}\tbig.bin\n`);
      const tip = gitIn(bare, [...IDENT, "commit-tree", tree, "-p", base, "-m", "big"]);
      const r = await g.secretScanCheckpointRange(bare, { tipSha: tip, excludeSha: base });
      assert.deepEqual(r, { trusted: false, findings: [], reason: "scan_too_large" });
      assert.equal(shimCalls(shim).length, 0, "gitleaks never ran");
      assert.ok(
        (lines as Array<Record<string, unknown>>).some((l) => l.reason === "scan_too_large" && l.largest_blob === 9 * 1024 * 1024),
      );
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  // ── round 3 item 1: our own deadline; never gitleaks inside a Codex permit ──────────────
  it("(r3 1a) a scanner that ignores --timeout and exits 0 CLEAN after the deadline is `deadline`, never trusted", async () => {
    const tmp = scratchDir("slowclean");
    try {
      const { bare, range } = await (async () => {
        const b = path.join(fx.dataDir, "sc.git");
        execFileSync("git", ["clone", "-q", "--bare", fx.originPath, b], { env: GIT_ENV });
        const base = gitIn(b, ["rev-parse", "main"]);
        const blob = gitIn(b, ["hash-object", "-w", "--stdin"], "text\n");
        const tree = gitIn(b, ["mktree"], `100644 blob ${blob}\tt.txt\n`);
        const tip = gitIn(b, [...IDENT, "commit-tree", tree, "-p", base, "-m", "t"]);
        return { bare: b, range: { tipSha: tip, excludeSha: base } };
      })();
      // Exits 0 with a clean, count-matching report just INSIDE the deadline (within the margin).
      const late = new GitCache(fx.dataDir, nullLogger(), undefined, {
        gitleaksBin: writeShim(tmp, "slowclean", { sleepMs: 1_400 }),
      });
      assert.deepEqual(await late.secretScanCheckpointRange(bare, range, { deadlineMs: 1_500 }), {
        trusted: false,
        findings: [],
        reason: "deadline",
      });
      // Would exit 0 clean long AFTER the deadline: our kill ends it at the deadline.
      const hung = new GitCache(fx.dataDir, nullLogger(), undefined, {
        gitleaksBin: writeShim(tmp, "slowclean", { sleepMs: 30_000 }),
      });
      const t0 = Date.now();
      assert.deepEqual(await hung.secretScanCheckpointRange(bare, range, { deadlineMs: 1_500 }), {
        trusted: false,
        findings: [],
        reason: "deadline",
      });
      assert.ok(Date.now() - t0 < 10_000, "killed at our deadline, not the scanner's");
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(r3 1a) outside a permit: a milestone whose scan outlives the deadline is killed, classed deadline, and not published", async () => {
    const tmp = scratchDir("milestone-deadline");
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const ctl = control({ scanDeadlineMs: 1_500 });
    const { logger, lines } = recordingLogger();
    const claim = gitlabClaim(1597_250);
    const t0 = Date.now();
    try {
      await mkRunner(
        mkGit(fx.dataDir, writeShim(tmp, "slowclean", { sleepMs: 30_000 })),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "M.txt", "m\n");
          await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        }),
        ctl,
        {},
        logger,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.equal(pub.count(), 0, "a scan cut by the deadline never publishes");
      assert.ok(
        (lines as Array<Record<string, unknown>>).some(
          (l) => l.msg === "checkpoint publish skipped: secret_scan_untrusted" && l.why === "deadline",
        ),
      );
      assert.ok(Date.now() - t0 < 25_000);
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(r3 1c) Codex milestone: gitleaks never runs inside the permit — the publish is deferred and the next (kicked) tick publishes it", async () => {
    const tmp = scratchDir("codexdefer");
    const shim = writeShim(tmp, "detect");
    const boundaryErrors: string[] = [];
    let inPermit = false;
    const scansInPermit: number[] = [];
    const safety: CodexExecutionSafety = {
      kind: "codex",
      withBoundary: async (req, action) => {
        const ac = new AbortController();
        const timer = setTimeout(() => ac.abort(new Error("boundary deadline")), req.deadlineMs);
        inPermit = true;
        try {
          const value = await action({ epoch: 1, boundary: req.boundary, signal: ac.signal } as unknown as BoundaryPermit);
          scansInPermit.push(shimCalls(shim).length);
          if (ac.signal.aborted) {
            boundaryErrors.push(req.boundary);
            throw new CodexBoundaryError("action", [{ category: "timeout", message: "late" }]);
          }
          return value;
        } finally {
          inPermit = false;
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
    const pub = stubPublish(async (_n, _tip, pack) => {
      assert.equal(inPermit, false, "never published from inside the permit");
      await drain(pack);
      return LANDED;
    });
    const ctl = control();
    const claim = gitlabClaim(1597_251);
    const { logger, lines } = recordingLogger();
    let milestoneSha = "";
    let kickDelay: number | undefined;
    let tickOutcome = "";
    try {
      const factory: ExecutorFactory = () => ({
        executor: {
          run: async (ctx: RunContext) => {
            await recordingTurnErrors(async () => {
              milestoneSha = commitIn(ctx.worktreePath, "K.txt", "k\n");
              await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
              kickDelay = ctl.armedDelay();
              tickOutcome = await ctl.fire(); // the kicked tick, outside any permit
            });
            return { branch: ctx.branch };
          },
          safety,
        },
      });
      await mkRunner(mkGit(fx.dataDir, shim), factory, ctl, { codexBoundaryDeadlineMs: 5_000 }, logger).execute(claim);
      assert.deepEqual(boundaryErrors, []);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.equal(scansInPermit[0], 0, "no gitleaks call inside the checkpoint permit");
      assert.ok(
        (lines as Array<Record<string, unknown>>).some((l) => l.msg === "checkpoint publish deferred out of the Codex permit (scan_deferred)"),
      );
      assert.equal(kickDelay, 1_000, "a tick was kicked soon after the deferral (not a full interval)");
      assert.equal(tickOutcome, "published", "the deferred publish went out on the next tick, time gate or not");
      assert.deepEqual(pub.tips, [milestoneSha]);
      assert.ok(shimCalls(shim).length >= 1, "the scan ran — outside the permit");
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(r5) a tick KICKED from inside a Codex permit runs outside it: after the permit ended, its lock reconcile still retains a foreign bare lock", async () => {
    const tmp = scratchDir("kickctx");
    const shim = writeShim(tmp, "detect");
    const g = mkGit(fx.dataDir, shim);
    const bare = g.barePathFor(fx.originPath);
    const foreignLock = path.join(bare, "packed-refs.lock");
    // A SIGTERM-honouring stand-in for the tick's FIRST child — the clone `rev-parse` of the
    // branch tip, which runs OUTSIDE any withLock section, so its lock custody happens only in
    // the tick's post-body withBareLock reconcile (the call an inherited permit scope breaks).
    // It dies on SIGTERM (never SIGKILLed), so a new lock is `foreign` without /proc evidence.
    const sleeper = path.join(tmp, "sleeper.cjs");
    fs.writeFileSync(sleeper, "setInterval(() => {}, 1000);\n");
    let rewritten = false;
    const ctl = control({
      tickSpawn: {
        rewrite: (r) => {
          if (rewritten || !r.argv.includes("rev-parse") || !r.argv.some((a) => a.startsWith("refs/heads/"))) return r;
          rewritten = true;
          return { ...r, argv: [process.execPath, sleeper] };
        },
      },
    });
    const permitSignals: AbortSignal[] = [];
    const safety: CodexExecutionSafety = {
      kind: "codex",
      withBoundary: async (req, action) => {
        const ac = new AbortController();
        permitSignals.push(ac.signal);
        try {
          return await action({ epoch: 1, boundary: req.boundary, signal: ac.signal } as unknown as BoundaryPermit);
        } finally {
          // The permit is over: its signal aborts (an expired permit), as anything still bound to
          // it must no longer be able to take the bare lock.
          ac.abort(new Error("permit ended"));
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
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const { logger, lines: rawLines } = recordingLogger();
    const lines = rawLines as Array<Record<string, unknown>>;
    const claim = gitlabClaim(1597_261);
    let kickDelay: number | undefined;
    let tickOutcome = "";
    let lockPresentAtReconcile = false;
    let running: Promise<unknown> | undefined;
    try {
      const factory: ExecutorFactory = () => ({
        executor: {
          run: async (ctx: RunContext) => {
            await recordingTurnErrors(async () => {
              commitIn(ctx.worktreePath, "R5.txt", "r5\n");
              // The milestone defers its publish out of the permit and KICKS a tick from inside it.
              await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
              kickDelay = ctl.armedDelay();
              assert.ok(permitSignals.length >= 1 && permitSignals.every((s) => s.aborted), "every permit has ended");
              // The kicked tick fires after the permit ended; hold it in its first (lock-free) child.
              const outcome = ctl.fire();
              await waitFor(() => rewritten && ctl.pids.length > 0);
              // A SEPARATE process's lock appears while the child runs (absent at its spawn).
              fs.writeFileSync(foreignLock, "", { flag: "wx" });
              // A gated path preempts the tick: the child is SIGTERMed and exits.
              await ctx.checkpoint!({ reap: false });
              tickOutcome = await outcome;
              lockPresentAtReconcile = fs.existsSync(foreignLock);
              // The owner removes its lock so the run can finish normally.
              fs.rmSync(foreignLock, { force: true });
            });
            return { branch: ctx.branch };
          },
          safety,
        },
      });
      running = mkRunner(g, factory, ctl, { codexBoundaryDeadlineMs: 5_000 }, logger).execute(claim);
      await running;
      assert.equal(kickDelay, 1_000, "the milestone kicked a tick");
      assert.ok(
        lines.some((l) => l.msg === "checkpoint publish deferred out of the Codex permit (scan_deferred)"),
        "the milestone deferred its publish out of the permit",
      );
      assert.ok(lockPresentAtReconcile, "the foreign lock was never deleted");
      assert.equal(
        lines.find((l) => l.msg === "mid-turn checkpoint lock reconcile failed"),
        undefined,
        "the reconcile took the bare lock (it did not inherit the ended permit's scope)",
      );
      assert.equal(tickOutcome, "bare_lock_retained");
      const warn = lines.find((l) => l.msg === "mid-turn checkpoint retained a git lock file in the worker bare") as
        | { retained?: Array<Record<string, unknown>> }
        | undefined;
      const ev = warn?.retained?.find((r) => String(r.path).endsWith("packed-refs.lock"));
      assert.ok(ev, JSON.stringify(warn));
      assert.equal(ev.reason, "foreign");
      assert.equal(finalStatus(claim.run_id), "completed");
    } finally {
      // Never leave the (detached) stand-in child behind a failed assertion; the lock and the temp
      // dir are removed even if that wait times out, and a timeout never masks the test's error.
      await settleAndClean({ ctl, running: [running], restore: pub.restore, paths: [foreignLock, tmp] });
    }
  });

  // ── item 8: retention is sticky only while the lock exists ──────────────────────────────
  it("(item 8) a retained foreign lock blocks ticks only while it exists; once its owner removes it the next tick proceeds", async () => {
    const tmp = scratchDir("unstick");
    const iid = 1597_245;
    const g = mkGit(fx.dataDir);
    const bare = g.barePathFor(fx.originPath);
    const foreignLock = path.join(bare, "packed-refs.lock");
    const stubborn = stubbornScript(tmp);
    let rewriteOn = true;
    const ctl = control({
      tickSpawn: { rewrite: (r) => (rewriteOn ? fetchBecomes(stubborn)(r) : r) },
      tickKillGraceMs: 500,
    });
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(iid);
    const seen: string[] = [];
    let running: Promise<unknown> | undefined;
    try {
      running = mkRunner(
        g,
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "U.txt", "u\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await waitFor(() => isReady(stubborn));
          const milestone = ctx.checkpoint!({ reap: false });
          fs.writeFileSync(foreignLock, "", { flag: "wx" }); // the foreign owner, during the cancellation
          await milestone;
          seen.push(ctl.outcomes[0]!);
          rewriteOn = false;
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire()); // still there → skip
          fs.rmSync(foreignLock); // its owner finishes
          commitIn(ctx.worktreePath, "U2.txt", "u2\n");
          ctl.advance(INTERVAL_MS + 1);
          seen.push(await ctl.fire()); // proceeds
        }),
        ctl,
      ).execute(claim);
      await running;
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.equal(seen[0], "bare_lock_retained");
      assert.equal(seen[1], "bare_lock_retained");
      assert.notEqual(seen[2], "bare_lock_retained", JSON.stringify(seen));
      assert.equal(seen[2], "published", JSON.stringify(seen));
    } finally {
      await settleAndClean({ ctl, running: [running], restore: pub.restore, paths: [tmp] });
    }
  });

  // ── item 9: every gated path excludes the tick ─────────────────────────────────────────
  type Gated = "parkForWall" | "enterCompletionHold" | "attemptCredentialSwitch";
  async function gatedPathExcludesTick(which: Gated, iid: number): Promise<string> {
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const g = mkGit(fx.dataDir);
    // All three paths read the clone status (captureHoldContext / captureRecoveryRestorePoint) while
    // holding the gate; the tick never calls worktreeStatus. Fire a tick from inside that read.
    let tickOutcome = "";
    const orig = g.worktreeStatus.bind(g);
    let fired = false;
    (g as unknown as { worktreeStatus: unknown }).worktreeStatus = async (cwd: string) => {
      if (!fired) {
        fired = true;
        ctl.advance(INTERVAL_MS + 1);
        tickOutcome = await ctl.fire();
      }
      return orig(cwd);
    };
    const claim = gitlabClaim(iid);
    try {
      await mkRunner(
        g,
        () => ({
          executor: {
            run: async (ctx: RunContext) => {
              commitIn(ctx.worktreePath, "W.txt", "w\n");
              if (which === "parkForWall") await ctx.parkForWall!({ completedCount: 0 }).catch(() => undefined);
              else if (which === "enterCompletionHold") await ctx.enterCompletionHold!("stall").catch(() => false);
              else await ctx.attemptCredentialSwitch!().catch(() => undefined);
              return { branch: ctx.branch };
            },
          },
        }),
        ctl,
      )
        .execute(claim)
        .catch(() => undefined);
      assert.ok(fired, `${which} reached its gated status read`);
      return tickOutcome;
    } finally {
      pub.restore();
    }
  }

  it("(item 9) parkForWall holds the sink gate: a tick firing inside it is gate_busy", async () => {
    assert.equal(await gatedPathExcludesTick("parkForWall", 1597_246), "gate_busy");
  });
  it("(item 9) enterCompletionHold holds the sink gate: a tick firing inside it is gate_busy", async () => {
    assert.equal(await gatedPathExcludesTick("enterCompletionHold", 1597_247), "gate_busy");
  });
  it("(item 9) attemptCredentialSwitch holds the sink gate: a tick firing inside it is gate_busy", async () => {
    assert.equal(await gatedPathExcludesTick("attemptCredentialSwitch", 1597_248), "gate_busy");
  });
});

// ── round 3 (issue #1597 M2): scan floors, old-side cap, remerge-diff, gitleaks' counting rule ──

/** A throwaway working repo cloned from the fixture origin, for GitCache-level scan tests (every
 *  scan command runs `git -C <repo>`, so a non-bare repo works as the "bare"). */
function workRepo(name: string): { dir: string; base: string; git: (args: string[], input?: string | Buffer) => string } {
  const dir = path.join(fx.dataDir, name);
  execFileSync("git", ["clone", "-q", fx.originPath, dir], { env: GIT_ENV });
  const g = (args: string[], input?: string | Buffer): string => gitIn(dir, [...IDENT, ...args], input);
  return { dir, base: g(["rev-parse", "HEAD"]), git: g };
}

/** Run `fn` once per scanner: the CI shim always, and the real binary when on PATH. */
async function eachScanner(
  t: { skip: (m: string) => void },
  fn: (bin: string, label: string) => Promise<void>,
): Promise<void> {
  const tmp = scratchDir("scanners");
  try {
    await fn(writeShim(tmp, "detect"), "shim");
    const real = realGitleaks();
    if (real) await fn(real, "real gitleaks");
    else t.skip("gitleaks is not on PATH in this environment; only the shim variant ran");
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

describe("mid-turn checkpoint round 3 (issue #1597 M2)", () => {
  it("(r3 5) commits gitleaks does not count (pure mv, --allow-empty, chmod, binary-only, empty file, whole-file delete) do not wedge the scan", async (t) => {
    await eachScanner(t, async (bin, label) => {
      const w = workRepo(`count-${label.replace(/\W/g, "")}`);
      const f = (n: string, c: string | Buffer): void => fs.writeFileSync(path.join(w.dir, n), c);
      f("n.txt", "normal\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "normal"]);
      w.git(["mv", "n.txt", "m.txt"]);
      w.git(["commit", "-q", "-m", "pure mv"]);
      w.git(["commit", "-q", "--allow-empty", "-m", "empty"]);
      fs.chmodSync(path.join(w.dir, "m.txt"), 0o755);
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "chmod"]);
      f("bin.dat", Buffer.from([0x61, 0x00, 0x62]));
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "binary"]);
      f("empty.txt", "");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "empty file"]);
      w.git(["rm", "-q", "README.md"]);
      w.git(["commit", "-q", "-m", "delete a text file"]);
      const tip = w.git(["rev-parse", "HEAD"]);
      const g = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: bin });
      assert.deepEqual(
        await g.secretScanCheckpointRange(w.dir, { tipSha: tip, excludeSha: w.base }),
        { trusted: true, findings: [] },
        label,
      );
    });
  });

  it("(r3 2) the size cap charges the OLD side: deleting a blob over the per-blob cap is scan_too_large", async () => {
    const tmp = scratchDir("oldside");
    try {
      const shim = writeShim(tmp, "detect");
      const w = workRepo("oldside");
      fs.writeFileSync(path.join(w.dir, "big.txt"), randomBytes(9 * 1024 * 1024).toString("base64").slice(0, 9 * 1024 * 1024 + 10));
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "big (already public)"]);
      const floor = w.git(["rev-parse", "HEAD"]);
      w.git(["rm", "-q", "big.txt"]);
      w.git(["commit", "-q", "-m", "delete big"]);
      const tip = w.git(["rev-parse", "HEAD"]);
      const g = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: shim });
      assert.deepEqual(await g.secretScanCheckpointRange(w.dir, { tipSha: tip, excludeSha: floor }), {
        trusted: false,
        findings: [],
        reason: "scan_too_large",
      });
      assert.equal(shimCalls(shim).length, 0);
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(r3 3) merges: an OCTOPUS evil merge is found; a plain merge across a rename is trusted", async (t) => {
    await eachScanner(t, async (bin, label) => {
      const w = workRepo(`octo-${label.replace(/\W/g, "")}`);
      const main = w.git(["rev-parse", "--abbrev-ref", "HEAD"]);
      for (const b of ["s1", "s2"]) {
        w.git(["checkout", "-q", "-b", b, w.base]);
        fs.writeFileSync(path.join(w.dir, `${b}.txt`), `${b}\n`);
        w.git(["add", "-A"]);
        w.git(["commit", "-q", "-m", b]);
      }
      w.git(["checkout", "-q", main]);
      fs.writeFileSync(path.join(w.dir, "line.txt"), "line\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "line"]);
      w.git(["merge", "-q", "--no-commit", "s1", "s2"]);
      fs.writeFileSync(path.join(w.dir, "evil.env"), `TOKEN=${runtimeSecret()}\n`);
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "octopus evil"]);
      const octo = w.git(["rev-parse", "HEAD"]);
      assert.equal(w.git(["rev-list", "--parents", "-n", "1", octo]).split(" ").length, 4, "a real octopus");
      const g = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: bin });
      const r = await g.secretScanCheckpointRange(w.dir, { tipSha: octo, excludeSha: w.base });
      assert.equal(r.findings.length > 0, true, `${label}: ${JSON.stringify(r)}`);
      assert.equal(r.findings[0]!.commit, octo);

      // A plain two-parent merge where one side renamed a file the other side edited.
      w.git(["checkout", "-q", "-b", "ren", octo]);
      w.git(["mv", "line.txt", "renamed.txt"]);
      w.git(["commit", "-q", "-m", "rename"]);
      w.git(["checkout", "-q", main]);
      fs.writeFileSync(path.join(w.dir, "line.txt"), "line\nmore\n");
      w.git(["commit", "-q", "-am", "edit"]);
      w.git(["merge", "-q", "--no-edit", "ren"]);
      const merged = w.git(["rev-parse", "HEAD"]);
      assert.deepEqual(await g.secretScanCheckpointRange(w.dir, { tipSha: merged, excludeSha: octo }), {
        trusted: true,
        findings: [],
      }, label);
    });
  });

  it("(r3 2) resolveCheckpointRange adds the default-branch tip and an ANCESTOR confirmed tip as scan-only floors", async () => {
    const g = mkGit(fx.dataDir);
    const bare = path.join(fx.dataDir, "floors.git");
    execFileSync("git", ["clone", "-q", "--bare", fx.originPath, bare], { env: GIT_ENV });
    const main = gitIn(bare, ["rev-parse", "main"]);
    const tree = gitIn(bare, ["rev-parse", `${main}^{tree}`]);
    const c1 = gitIn(bare, [...IDENT, "commit-tree", tree, "-p", main, "-m", "c1"]);
    const c2 = gitIn(bare, [...IDENT, "commit-tree", tree, "-p", c1, "-m", "c2"]);
    const other = gitIn(bare, [...IDENT, "commit-tree", tree, "-p", main, "-m", "other"]);
    // A default branch AHEAD of the pack floor, so it is a distinct scan floor.
    const newMain = gitIn(bare, [...IDENT, "commit-tree", tree, "-p", main, "-m", "main moved"]);
    gitIn(bare, ["update-ref", "refs/remotes/origin/agent/issue-9", main]);
    gitIn(bare, ["update-ref", "refs/heads/main", newMain]);
    gitIn(bare, ["update-ref", "refs/uzi-runner/agent/issue-9", c2]);
    assert.deepEqual(await g.resolveCheckpointRange(bare, "agent/issue-9", { confirmedTip: c1 }), {
      tipSha: c2,
      excludeSha: main,
      scanFloorShas: [newMain, c1],
    });
    assert.deepEqual(await g.resolveCheckpointRange(bare, "agent/issue-9", { confirmedTip: other }), {
      tipSha: c2,
      excludeSha: main,
      scanFloorShas: [newMain],
    }, "a confirmed tip that is not an ancestor of the tip is not a floor");
  });

  it("(r3 2) runner: content an UNSCANNED pause publish already made public is not re-scanned (no permanent wedge)", async () => {
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_260);
    let outcome = "";
    let parked: boolean | undefined;
    try {
      await mkRunner(
        mkGit(fx.dataDir),
        () => ({
          executor: {
            run: async (ctx: RunContext) => {
              // A secret committed and removed, then made public by the (unscanned, by design) pause
              // publish — which the server confirmed.
              commitIn(ctx.worktreePath, "cfg.env", `TOKEN=${runtimeSecret()}\n`);
              commitIn(ctx.worktreePath, "cfg.env", "TOKEN=\n");
              parked = await ctx.parkForPause!({ completedCount: 0 });
              commitIn(ctx.worktreePath, "after.txt", "after the pause\n");
              ctl.advance(INTERVAL_MS + 1);
              outcome = await ctl.fire();
              return { branch: ctx.branch };
            },
          },
        }),
        ctl,
      )
        .execute(claim)
        .catch(() => undefined);
      assert.equal(parked, true, "the pause publish landed (confirmed)");
      assert.equal(outcome, "published", "the confirmed checkpoint is a scan floor: only the new commit is scanned");
      assert.equal(pub.count(), 2);
    } finally {
      pub.restore();
    }
  });

  it("(r3 2/3) merging default-branch content that carries a secret-shaped fixture does not wedge publishing", async (t) => {
    await eachScanner(t, async (bin, label) => {
      const iid = label === "shim" ? 1597_261 : 1597_262;
      const branch = `agent/issue-${iid}`;
      // The run's branch is published at the current main; main then gains a fixture commit.
      const at = gitIn(fx.originPath, ["rev-parse", "main"]);
      gitIn(fx.originPath, ["branch", "-f", branch, at]);
      const fixture = commitIn(fx.originPath, `fixtures/${iid}.env`, `EXAMPLE_TOKEN=${runtimeSecret()}\n`);
      const ctl = control();
      const pub = stubPublish(async (_n, _tip, pack) => {
        await drain(pack);
        return LANDED;
      });
      const claim = gitlabClaim(iid);
      let outcome = "";
      try {
        await mkRunner(
          mkGit(fx.dataDir, bin),
          turn(async (ctx) => {
            commitIn(ctx.worktreePath, "work.txt", "work\n");
            gitIn(ctx.worktreePath, [...IDENT, "merge", "-q", "--no-edit", fixture]);
            ctl.advance(INTERVAL_MS + 1);
            outcome = await ctl.fire();
          }),
          ctl,
        ).execute(claim);
        assert.equal(finalStatus(claim.run_id), "completed", label);
        assert.equal(outcome, "published", label);
        assert.equal(pub.count(), 1, label);
      } finally {
        pub.restore();
      }
    });
  });

  it("(r3 6) bridged bookkeeping: lastPublishedTip = the fetched H, checkpointFloor = the published bridge B", async () => {
    const iid = 1597_263;
    const branch = `agent/issue-${iid}`;
    const main = gitIn(fx.originPath, ["rev-parse", "main"]);
    gitIn(fx.originPath, ["checkout", "-q", "-b", branch]);
    commitIn(fx.originPath, "P.txt", "published P\n");
    gitIn(fx.originPath, ["checkout", "-q", "main"]);
    const states: Array<{ publishedTip: string; lastPublishedTip?: string; checkpointFloor?: string }> = [];
    const ctl = control({ afterPinnedPublish: (s) => states.push(s) });
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(iid);
    let h = "";
    let second = "";
    try {
      await mkRunner(
        mkGit(fx.dataDir),
        turn(async (ctx) => {
          // Rewrite history BELOW the published floor P: the tick must bridge (B wraps H over P).
          gitIn(ctx.worktreePath, ["reset", "-q", "--hard", main]);
          h = commitIn(ctx.worktreePath, "H.txt", "rewritten work\n");
          ctl.advance(INTERVAL_MS + 1);
          assert.equal(await ctl.fire(), "published");
          ctl.advance(INTERVAL_MS + 1);
          second = await ctl.fire();
        }),
        ctl,
      ).execute(claim);
      assert.equal(states.length, 1, JSON.stringify(states));
      const s = states[0]!;
      assert.notEqual(s.publishedTip, h, "the published tip is the bridge B, not H");
      assert.equal(s.lastPublishedTip, h, "lastPublishedTip is the fetched H (it drives hasNewWork against the clone)");
      assert.equal(s.checkpointFloor, s.publishedTip, "the checkpoint floor is the published bridge B");
      assert.equal(second, "no_new_work", "no republish of the same work");
    } finally {
      pub.restore();
    }
  });

  it("(r3 3) a REJECTED --remerge-diff probe is not cached: a later call probes again", async (t) => {
    const ver = /git version (\d+)\.(\d+)/.exec(execFileSync("git", ["version"], { encoding: "utf8" }));
    if (!ver || Number(ver[1]) < 2 || (Number(ver[1]) === 2 && Number(ver[2]) < 36)) {
      t.skip("local git < 2.36 has no --remerge-diff");
      return;
    }
    const g = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: defaultGitleaksShim() });
    type ExecScoped = (command: string, args: string[], ...rest: unknown[]) => Promise<{ stdout: string; stderr: string }>;
    const priv = g as unknown as { execScoped: ExecScoped; remergeDiffSupported: () => Promise<boolean> };
    const original = priv.execScoped.bind(g);
    let versionCalls = 0;
    priv.execScoped = (command, args, ...rest) => {
      if (command === "git" && args[0] === "version" && ++versionCalls === 1) {
        return Promise.reject(new Error("aborted: tick scan scope ended"));
      }
      return original(command, args, ...rest);
    };
    assert.equal(await priv.remergeDiffSupported(), false, "the rejected probe answers false for its own call");
    assert.equal(await priv.remergeDiffSupported(), true, "a later call re-probes instead of reusing the rejection");
    assert.equal(versionCalls, 2);
    assert.equal(await priv.remergeDiffSupported(), true, "a completed probe is cached");
    assert.equal(versionCalls, 2);
  });
});

// ── round 4 (issue #1597 M2) ────────────────────────────────────────────────────────────────

/** A fake Codex facade enforcing the permit deadline (as the real one does). `inPermit()` says
 *  whether a permit is currently held. */
function fakeCodex(): { safety: CodexExecutionSafety; inPermit: () => boolean; boundaryErrors: string[] } {
  let held = false;
  const boundaryErrors: string[] = [];
  const safety: CodexExecutionSafety = {
    kind: "codex",
    withBoundary: async (req, action) => {
      const ac = new AbortController();
      const timer = setTimeout(() => ac.abort(new Error("boundary deadline")), req.deadlineMs);
      held = true;
      try {
        const value = await action({ epoch: 1, boundary: req.boundary, signal: ac.signal } as unknown as BoundaryPermit);
        if (ac.signal.aborted) {
          boundaryErrors.push(req.boundary);
          throw new CodexBoundaryError("action", [{ category: "timeout", message: "late" }]);
        }
        return value;
      } finally {
        held = false;
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
  return { safety, inPermit: () => held, boundaryErrors };
}

describe("mid-turn checkpoint round 4 (issue #1597 M2)", () => {
  it("(r4 1) an owed publish is settled by a blocked attempt: later ticks obey the time gate (one scan, not one per tick)", async () => {
    const tmp = scratchDir("owed");
    const shim = writeShim(tmp, "detect");
    const codex = fakeCodex();
    const ctl = control();
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_270);
    const seen: string[] = [];
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        () => ({
          executor: {
            run: async (ctx: RunContext) => {
              await recordingTurnErrors(async () => {
                commitIn(ctx.worktreePath, "cfg.env", `TOKEN=${runtimeSecret()}\n`);
                await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } }); // deferred
                seen.push(await ctl.fire()); // the kicked tick: scans, finds the secret
                seen.push(await ctl.fire()); // frozen clock: the time gate governs now
                seen.push(await ctl.fire());
              });
              return { branch: ctx.branch };
            },
            safety: codex.safety,
          },
        }),
        ctl,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.deepEqual(seen, ["secret_found", "time_gate_closed", "time_gate_closed"]);
      assert.equal(shimCalls(shim).filter((c) => c.startsWith("git ")).length, 1, "one gitleaks run, not one per tick");
      assert.equal(pub.count(), 0);
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(r4 2) a CONFLICTED merge of main resolved to main's side does not re-flag main's fixture; an evil merge is still caught", async (t) => {
    await eachScanner(t, async (bin, label) => {
      const iid = label === "shim" ? 1597_271 : 1597_272;
      const file = `conf-${iid}.env`;
      // main and the run's branch start from a commit carrying the file…
      commitIn(fx.originPath, file, "TOKEN=\n");
      const at = gitIn(fx.originPath, ["rev-parse", "main"]);
      gitIn(fx.originPath, ["branch", "-f", `agent/issue-${iid}`, at]);
      // …then main sets the value to a secret-shaped FIXTURE (public on the default branch).
      const fixture = commitIn(fx.originPath, file, `TOKEN=${runtimeSecret()}\n`);
      const ctl = control();
      const pub = stubPublish(async (_n, _tip, pack) => {
        await drain(pack);
        return LANDED;
      });
      const claim = gitlabClaim(iid);
      const seen: string[] = [];
      try {
        await mkRunner(
          mkGit(fx.dataDir, bin),
          turn(async (ctx) => {
            const w = ctx.worktreePath;
            commitIn(w, file, "TOKEN=branch-value\n"); // conflicts with main's edit
            try {
              gitIn(w, [...IDENT, "merge", "-q", "--no-edit", fixture]);
            } catch {
              /* the expected conflict */
            }
            gitIn(w, ["checkout", "--theirs", "--", file]); // resolve to MAIN's side
            gitIn(w, ["add", file]);
            gitIn(w, [...IDENT, "commit", "-q", "--no-edit"]);
            ctl.advance(INTERVAL_MS + 1);
            seen.push(await ctl.fire());
            // An evil merge afterwards must still be caught.
            gitIn(w, ["checkout", "-q", "-b", "side"]);
            commitIn(w, "side.txt", "side\n");
            gitIn(w, ["checkout", "-q", ctx.branch]);
            commitIn(w, "line.txt", "line\n");
            gitIn(w, [...IDENT, "merge", "-q", "--no-ff", "--no-commit", "side"]);
            fs.writeFileSync(path.join(w, "evil.env"), `TOKEN=${runtimeSecret()}\n`);
            gitIn(w, ["add", "evil.env"]);
            gitIn(w, [...IDENT, "commit", "-q", "-m", "evil merge"]);
            ctl.advance(INTERVAL_MS + 1);
            seen.push(await ctl.fire());
          }),
          ctl,
        ).execute(claim);
        assert.equal(finalStatus(claim.run_id), "completed", label);
        assert.deepEqual(seen, ["published", "secret_found"], label);
      } finally {
        pub.restore();
      }
    });
  });

  it("(r4 3) the merge scan is untrusted when gitleaks reports fewer bytes than it was fed", async () => {
    const tmp = scratchDir("stdinlie");
    try {
      const w = workRepo("stdinlie");
      const main = w.git(["rev-parse", "--abbrev-ref", "HEAD"]);
      w.git(["checkout", "-q", "-b", "side"]);
      fs.writeFileSync(path.join(w.dir, "side.txt"), "side\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "side"]);
      w.git(["checkout", "-q", main]);
      fs.writeFileSync(path.join(w.dir, "line.txt"), "line\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "line"]);
      w.git(["merge", "-q", "--no-ff", "--no-commit", "side"]);
      fs.writeFileSync(path.join(w.dir, "own.txt"), "content only the merge adds\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "merge with its own (clean) content"]);
      const tip = w.git(["rev-parse", "HEAD"]);
      const honest = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: writeShim(tmp, "detect") });
      assert.deepEqual(await honest.secretScanCheckpointRange(w.dir, { tipSha: tip, excludeSha: w.base }), {
        trusted: true,
        findings: [],
      });
      const liar = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: writeShim(tmp, "stdinlie") });
      assert.deepEqual(await liar.secretScanCheckpointRange(w.dir, { tipSha: tip, excludeSha: w.base }), {
        trusted: false,
        findings: [],
        reason: "scan_untrusted",
      });
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(r4 6) a merge-added file whose PATH is secret-shaped is trusted: only hunk '+' lines are fed, never the `+++ b/` header", async (t) => {
    await eachScanner(t, async (bin, label) => {
      const w = workRepo(`hdr-${label.replace(/\W/g, "")}`);
      const main = w.git(["rev-parse", "--abbrev-ref", "HEAD"]);
      w.git(["checkout", "-q", "-b", "side"]);
      fs.writeFileSync(path.join(w.dir, "side.txt"), "side\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "side"]);
      w.git(["checkout", "-q", main]);
      fs.writeFileSync(path.join(w.dir, "line.txt"), "line\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "line"]);
      w.git(["merge", "-q", "--no-ff", "--no-commit", "side"]);
      // The MERGE itself adds a file with clean content under a secret-shaped name (so no ordinary
      // commit carries it): its only appearance in the merge's own diff is the file header.
      const name = `${runtimeSecret()}.txt`;
      fs.writeFileSync(path.join(w.dir, name), "clean content\n");
      w.git(["add", "-A"]);
      w.git(["commit", "-q", "-m", "merge adding a secret-named file"]);
      const tip = w.git(["rev-parse", "HEAD"]);
      assert.ok(
        w.git(["diff", "--no-color", `${tip}^1`, tip]).includes(`+++ b/${name}`),
        "the merge diff carries the secret-shaped `+++ b/` header",
      );
      const g = new GitCache(fx.dataDir, nullLogger(), undefined, { gitleaksBin: bin });
      assert.deepEqual(
        await g.secretScanCheckpointRange(w.dir, { tipSha: tip, excludeSha: w.base }),
        { trusted: true, findings: [] },
        label,
      );
    });
  });

  it("(r4 4) CHECKPOINT_TICK_INTERVAL=0: a Codex milestone publishes right after its permit, before the checkpoint returns", async () => {
    const tmp = scratchDir("notick");
    const shim = writeShim(tmp, "detect");
    const codex = fakeCodex();
    const publishedInPermit: boolean[] = [];
    const pub = stubPublish(async (_n, _tip, pack) => {
      publishedInPermit.push(codex.inPermit());
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_273);
    let sha = "";
    let publishesWhenCheckpointReturned = -1;
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        () => ({
          executor: {
            run: async (ctx: RunContext) => {
              await recordingTurnErrors(async () => {
                sha = commitIn(ctx.worktreePath, "N.txt", "n\n");
                await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
                publishesWhenCheckpointReturned = pub.count();
              });
              return { branch: ctx.branch };
            },
            safety: codex.safety,
          },
        }),
        undefined,
        { checkpointTickIntervalMs: 0 },
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.deepEqual(codex.boundaryErrors, []);
      assert.equal(publishesWhenCheckpointReturned, 1, "published before ctx.checkpoint returned");
      assert.deepEqual(pub.tips.slice(0, 1), [sha]);
      assert.deepEqual(publishedInPermit.slice(0, 1), [false], "never from inside the permit");
      assert.ok(shimCalls(shim).length >= 1);
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });
});

describe("mid-turn checkpoint round 4: deferred publish vs shutdown (issue #1597 M2)", () => {
  it("(r4 7) CHECKPOINT_TICK_INTERVAL=0: a shutdown during the Codex permit skips the post-permit scan+publish", async () => {
    const tmp = scratchDir("notickshut");
    const shim = writeShim(tmp, "detect");
    const codex = fakeCodex();
    let runner: RunRunner | undefined;
    const safety: CodexExecutionSafety = {
      ...codex.safety,
      withBoundary: (req, action) =>
        codex.safety.withBoundary(req, async (permit) => {
          const v = await action(permit);
          runner!.shutdown(); // the worker is told to stop while the permit is still held
          return v;
        }),
    };
    const pub = stubPublish(async (_n, _tip, pack) => {
      await drain(pack);
      return LANDED;
    });
    const claim = gitlabClaim(1597_275);
    let publishesWhenCheckpointReturned = -1;
    let scansWhenCheckpointReturned = -1;
    try {
      runner = mkRunner(
        mkGit(fx.dataDir, shim),
        () => ({
          executor: {
            run: async (ctx: RunContext) => {
              await recordingTurnErrors(async () => {
                commitIn(ctx.worktreePath, "S.txt", "s\n");
                await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
                publishesWhenCheckpointReturned = pub.count();
                scansWhenCheckpointReturned = shimCalls(shim).length;
              });
              await waitAbort(ctx.signal!);
              throw new Error("aborted mid-turn");
            },
            safety,
          },
        }),
        undefined,
        { checkpointTickIntervalMs: 0 },
      );
      await runner.execute(claim).catch(() => undefined);
      assert.deepEqual(codex.boundaryErrors, []);
      assert.equal(scansWhenCheckpointReturned, 0, "no post-permit scan once the flight is cancelled");
      assert.equal(publishesWhenCheckpointReturned, 0, "no post-permit publish once the flight is cancelled");
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });
});

describe("mid-turn checkpoint round 4: gate_busy quick retry (issue #1597 M2)", () => {
  it("(r4 5) an owed publish whose tick finds the sink gate busy arms ONE quick retry; a second miss waits for the normal cadence", async () => {
    const tmp = scratchDir("gateretry");
    const shim = writeShim(tmp, "detect");
    const codex = fakeCodex();
    const ctl = control();
    const seen: string[] = [];
    const delays: Array<number | undefined> = [];
    const pub = stubPublish(async (n, _tip, pack) => {
      await drain(pack);
      if (n === 0) {
        // The pause path is INSIDE its publish, holding the sink gate, while the deferred
        // milestone publish is still owed.
        delays.push(ctl.armedDelay()); // the deferral's kick
        seen.push(await ctl.fire()); // the kicked tick: gate busy
        delays.push(ctl.armedDelay()); // → one quick retry armed
        seen.push(await ctl.fire()); // the quick retry: gate still busy
        delays.push(ctl.armedDelay()); // → back to the normal cadence
        return { ok: false, httpStatus: 500 }; // the pause fails; the run carries on
      }
      return LANDED;
    });
    const claim = gitlabClaim(1597_274);
    let sha = "";
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        () => ({
          executor: {
            run: async (ctx: RunContext) => {
              await recordingTurnErrors(async () => {
                sha = commitIn(ctx.worktreePath, "R.txt", "r\n");
                await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } }); // deferred
                assert.equal(await ctx.parkForPause!({ completedCount: 1 }), false);
                seen.push(await ctl.fire()); // gate free again: the owed publish goes out
                // The published attempt SETTLED the owed publish: new work on a frozen clock now
                // waits for the time gate instead of bypassing it on every tick.
                commitIn(ctx.worktreePath, "R2.txt", "r2\n");
                seen.push(await ctl.fire());
              });
              return { branch: ctx.branch };
            },
            safety: codex.safety,
          },
        }),
        ctl,
      ).execute(claim);
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.deepEqual(seen, ["gate_busy", "gate_busy", "published", "time_gate_closed"]);
      assert.deepEqual(delays, [1_000, 1_000, TICK_MS], "one quick retry, then the normal cadence");
      assert.equal(pub.tips.at(-1), sha);
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });
});
