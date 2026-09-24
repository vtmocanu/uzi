import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import type { ExecutorFactory, RunnerOptions } from "../src/runner.js";
import { RunRunner } from "../src/runner.js";
import type { RunContext } from "../src/executor.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
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

/** A GitHub-PAT-shaped value ASSEMBLED AT RUNTIME (never a complete token literal in source). */
function runtimeSecret(): string {
  const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  const body = [...randomBytes(36)].map((b) => alnum[b % alnum.length]).join("");
  return "gh" + "p_" + body;
}

// ── tick + publish control ──────────────────────────────────────────────────────────────────

interface Ctl {
  setTimer: NonNullable<RunnerOptions["setTimer"]>;
  now: () => number;
  advance: (ms: number) => void;
  /** Fire the captured tick timer and resolve with the tick's outcome class. */
  fire: () => Promise<string>;
  /** Fire WITHOUT waiting for the outcome (it is still recorded). */
  fireNoWait: () => void;
  nextOutcome: () => Promise<string>;
  outcomes: string[];
  armed: () => boolean;
  hooks: NonNullable<RunnerOptions["checkpointTestHooks"]>;
  pids: number[];
  spawned: string[][];
}

function control(extraHooks: Partial<NonNullable<RunnerOptions["checkpointTestHooks"]>> = {}): Ctl {
  let tickCb: (() => void) | undefined;
  let fakeNow = 0;
  const outcomes: string[] = [];
  const waiters: Array<{ n: number; resolve: (o: string) => void }> = [];
  const pids: number[] = [];
  const spawned: string[][] = [];
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
      if (ms === TICK_MS) {
        tickCb = cb;
        return () => {
          if (tickCb === cb) tickCb = undefined;
        };
      }
      const t = setTimeout(cb, ms);
      t.unref?.();
      return () => clearTimeout(t);
    },
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
      cb();
    },
    nextOutcome: () => waitIdx(outcomes.length),
    outcomes,
    armed: () => tickCb !== undefined,
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

/** A publish that hangs until its signal aborts, then rejects like an aborted fetch. */
async function hangUntilAborted(pack: Readable, signal?: AbortSignal): Promise<never> {
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
    ...(ctl ? { setTimer: ctl.setTimer, setTickTimer: ctl.setTimer, now: ctl.now, checkpointTestHooks: ctl.hooks } : {}),
    ...extra,
  });
}

/** An executor whose single long turn runs `body`, then returns (turn completes). */
function turn(body: (ctx: RunContext) => Promise<void>): ExecutorFactory {
  return () => ({
    executor: {
      run: async (ctx: RunContext) => {
        await body(ctx);
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
        await body(ctx);
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
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "H.txt", "h\n");
          ctl.advance(INTERVAL_MS + 1);
          ctl.fireNoWait();
          await entered.promise; // the tick is now stuck inside the publish RPC
        }),
        ctl,
      ).execute(claim);
      assert.deepEqual(ctl.outcomes, ["aborted"]);
      assert.equal(pub.count(), 1);
      assert.ok(Date.now() - t0 < 20_000, "teardown did not wait out the hang");
      assert.equal(finalStatus(claim.run_id), "completed");
      assert.ok(!statusTexts(claim.run_id).some((t) => t.startsWith("checkpoint publish failed")), "an abort is silent");
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
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
    const secret = runtimeSecret();
    try {
      await mkRunner(
        g,
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "config.env", `TOKEN=${secret}\n`);
          commitB = commitIn(ctx.worktreePath, "config.env", "TOKEN=\n");
          ctl.advance(INTERVAL_MS + 1);
          assert.equal(await ctl.fire(), "secret_found");
          assert.equal(pub.count(), 0, "no publishCheckpoint call");
          assert.equal(refOr(bare, `refs/uzi-runner/agent/issue-${iid}`), commitB, "the fetch-back is kept");
        }),
        ctl,
        {},
        logger,
      ).execute(claim);
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
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "U.txt", "u\n");
          ctl.advance(INTERVAL_MS + 1);
          assert.equal(await ctl.fire(), "secret_scan_untrusted");
        }),
        ctl,
      ).execute(claim);
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
    try {
      await mkRunner(
        mkGit(fx.dataDir, gitleaks),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "deploy.sh", `export GITHUB_TOKEN=${secret}\n`);
          commitIn(ctx.worktreePath, "deploy.sh", "export GITHUB_TOKEN=\n");
          ctl.advance(INTERVAL_MS + 1);
          assert.equal(await ctl.fire(), "secret_found");
        }),
        ctl,
      ).execute(claim);
      assert.equal(pub.count(), 0);
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
      assert.deepEqual(scanned, { tipSha: c1, excludeSha: o1 }, "the scan walked O1..C1");
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
    try {
      await mkRunner(
        mkGit(fx.dataDir, shim),
        turn(async (ctx) => {
          commitIn(ctx.worktreePath, "G.txt", "g\n");
          fs.writeFileSync(path.join(ctx.worktreePath, "WIP.txt"), "uncommitted\n");
          const parked = await ctx.parkForPause!({ completedCount: 0 });
          assert.equal(parked, false);
          assert.equal(
            gitIn(ctx.worktreePath, ["log", "-1", "--format=%s"]).startsWith("wip(park):"),
            false,
            "the marker was undone",
          );
        }),
        ctl,
      ).execute(claim);
      assert.equal(tickOutcome, "gate_busy");
      assert.equal(pub.count(), 1, "only the pause's own publish ran — the tick never published the marker");
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
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
    try {
      await mkRunner(
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
      assert.deepEqual(events, [
        "tick:publish",
        "tick:publish-aborted",
        "milestone:publish (tick children alive: 0)",
        "milestone:done (tick outcome: aborted)",
      ]);
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
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
  try {
    const p = runner.execute(claim);
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
    pub.restore();
    fs.rmSync(tmp, { recursive: true, force: true });
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

describe("mid-turn checkpoint lock custody (issue #1597 M2)", () => {
  it("(i) proven-owned: the SIGKILLed fetch child's own refs/uzi-runner lock is removed and the next fetch-back works", async () => {
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
    try {
      const runner = mkRunner(
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
      const p = runner.execute(claim);
      await waitFor(() => fs.existsSync(lock));
      runner.shutdown();
      await p;
      assert.deepEqual(ctl.outcomes, ["aborted"]);
      assert.equal(fs.existsSync(lock), false, "the proven-owned lock was removed");
      assert.ok(lines.some((l) => l.msg === "mid-turn checkpoint removed a lock file its cancelled child provably owned"));
      assert.equal(refOr(bare, `refs/uzi-runner/${branch}`), sha, "the shutdown fetch-back then worked");
      assert.ok(statusTexts(claim.run_id).includes("shutdown checkpoint published to origin"));
    } finally {
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(i) foreign: a lock a separate process created during the cancellation is RETAINED; later ticks skip; shutdown names bare_lock_retained", async () => {
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
    try {
      const runner = mkRunner(
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
      const p = runner.execute(claim);
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
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("(i) replaced: the child held the lock but after its exit the path is a different inode — RETAINED", async () => {
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
    try {
      const runner = mkRunner(
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
      const p = runner.execute(claim);
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
      pub.restore();
      fs.rmSync(tmp, { recursive: true, force: true });
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
    assert.deepEqual(await g.resolveCheckpointRange(bare, branch), { tipSha: tip, excludeSha: base });
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
