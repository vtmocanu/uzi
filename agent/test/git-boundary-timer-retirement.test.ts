import { afterEach, beforeEach, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PassThrough, Writable } from "node:stream";
import { GitCache, type BoundaryProcessSpawner } from "../src/git.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { CheckpointSoftDeadlineError, CheckpointUnsafeCleanupError, GitBoundaryAbortError } from "../src/checkpoint-stop.js";
import type { BoundaryProcessHandle } from "../src/harness.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

// Issue #3304: inside a boundary scope, the 600 s read timers of `readBare` and `streamBundleWithCap` retire
// when the scope stops (hard signal, or the soft signal of an unmasked scope). A retired timer never settles,
// abandons or kills anything and is never re-armed: the owner settles the read with its typed error once the
// child's cancellation and completion have settled. Node MockTimers stand in for the 600 s wait, so no test
// waits in real time. The child is a held fake: its `completed` stays pending until the test releases it.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const UNUSED_BARE = "/nonexistent-3304/bare.git";
const TIMEOUT_MS = 600_000;

type Terminal = { code: number; softTimedOut?: true };
interface HeldChild {
  handle: BoundaryProcessHandle;
  cancels: () => number;
  /** Cleanup finished cleanly (the root reaped), or failed with a typed error. */
  release: (outcome?: Terminal | Error) => void;
}

function heldChild(): HeldChild {
  const stdout = new PassThrough(), stderr = new PassThrough();
  for (const stream of [stdout, stderr]) stream.on("error", () => undefined);
  let cancels = 0;
  let release!: (outcome?: Terminal | Error) => void;
  const completed = new Promise<Terminal>((resolve, reject) => {
    release = outcome => outcome instanceof Error ? reject(outcome) : resolve(outcome ?? { code: -1 });
  });
  completed.catch(() => undefined);
  const handle: BoundaryProcessHandle = {
    stdin: new Writable({ write: (_chunk, _enc, cb) => cb() }), stdout, stderr, completed,
    cancel: async () => { cancels++; await completed.catch(() => undefined); },
  };
  return { handle, cancels: () => cancels, release };
}

/** Real macrotask turns: MockTimers owns setTimeout here, so never wait on a timer. */
async function turns(count = 60): Promise<void> {
  for (let i = 0; i < count; i++) await new Promise<void>(resolve => setImmediate(resolve));
}

function track<T>(promise: Promise<T>): { settled: () => boolean; result: () => Promise<{ ok: T } | { error: unknown }> } {
  let done = false;
  const result = promise.then(ok => ({ ok }), error => ({ error }));
  void result.then(() => { done = true; });
  return { settled: () => done, result: () => result };
}

let fx: Fixture;
let cache: GitCache;
beforeEach(() => { fx = makeFixture(); cache = new GitCache(fx.dataDir, nullLogger()); });
afterEach(() => fx.cleanup());

interface Scope {
  hard: AbortController;
  soft: AbortController;
  run: <T>(fn: () => Promise<T>) => Promise<T>;
}
function scopeFor(spawner: BoundaryProcessSpawner): Scope {
  const hard = new AbortController(), soft = new AbortController();
  return { hard, soft,
    run: fn => cache.withBoundaryProcessSpawner(spawner, hard.signal, fn, { softSignal: soft.signal }) };
}

const readHead = (): Promise<unknown> => cache.readBare(UNUSED_BARE, ["rev-parse", "HEAD"], { maxBytes: 1024 });

function mockTimers(t: TestContext): { pastReadDeadline: () => void } {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  return { pastReadDeadline: () => t.mock.timers.tick(TIMEOUT_MS + 1) };
}

describe("readBare 600 s timer inside a boundary scope", () => {
  for (const stop of ["hard", "soft"] as const) {
    it(`retires on the ${stop} stop: no plain error, the owner settles it after a clean cleanup`, async t => {
      const timers = mockTimers(t);
      const child = heldChild();
      const scope = scopeFor(async () => child.handle);
      const read = track(scope.run(readHead));
      await turns();
      assert.equal(read.settled(), false);
      (stop === "hard" ? scope.hard : scope.soft).abort();
      await turns();
      // Soft: the handle's own soft listener requests the cancellation. Hard: the safety handle stops on the
      // permit abort itself, so nothing here requests an extra cancel (it would mirror a typed rejection).
      if (stop === "soft") assert.equal(child.cancels() >= 1, true, "the soft stop requested the child's cancellation");
      else assert.equal(child.cancels(), 0, "a hard stop requests no extra cancellation");
      assert.equal(read.settled(), false, "the read waits for the child's cleanup");
      timers.pastReadDeadline();
      await turns();
      assert.equal(read.settled(), false, "the retired timer did not settle the read with its plain deadline error");
      // A hard cancel that reaped cleanly and a soft stop (the handle reports a soft timeout once cancelled).
      child.release(stop === "hard" ? { code: 0 } : undefined);
      const outcome = await read.result();
      assert.ok("error" in outcome, "the stopped read fails");
      assert.ok(outcome.error instanceof (stop === "hard" ? GitBoundaryAbortError : CheckpointSoftDeadlineError),
        `typed ${stop} error, got ${String(outcome.error)}`);
      if (stop === "hard") assert.match((outcome.error as Error).message, /boundary deadline exceeded/);
    });
  }

  it("hard stop with a typed completion rejection that cancel() mirrors: the typed refusal, not an unsafe-cleanup error", async t => {
    const timers = mockTimers(t);
    const stdout = new PassThrough(), stderr = new PassThrough();
    for (const stream of [stdout, stderr]) stream.on("error", () => undefined);
    let cancels = 0;
    const scope = scopeFor(async () => {
      // Safety-like handle: the permit abort stops it, completion rejects a few turns later, and cancel()
      // returns the same completion (so it rejects with the same refusal).
      let reject!: (reason: unknown) => void;
      const completed = new Promise<Terminal>((_resolve, rej) => { reject = rej; });
      completed.catch(() => undefined);
      scope.hard.signal.addEventListener("abort", () => {
        void turns(5).then(() => reject(new TrustedExecutionRefusal("boundary process deadline exceeded")));
      }, { once: true });
      return { stdin: new Writable({ write: (_c, _e, cb) => cb() }), stdout, stderr, completed,
        cancel: () => { cancels++; return completed.then(() => undefined); } };
    });
    const read = track(scope.run(readHead));
    await turns();
    scope.hard.abort();
    timers.pastReadDeadline();
    const outcome = await read.result();
    assert.ok("error" in outcome);
    assert.ok(!(outcome.error instanceof CheckpointUnsafeCleanupError), `reclassified: ${String(outcome.error)}`);
    assert.ok(outcome.error instanceof TrustedExecutionRefusal, `got ${String(outcome.error)}`);
    assert.equal(cancels, 0, "a hard stop requested no cancellation");
  });

  it("surfaces an unclean cleanup as CheckpointUnsafeCleanupError, still without the plain deadline error", async t => {
    const timers = mockTimers(t);
    const child = heldChild();
    const scope = scopeFor(async () => child.handle);
    const read = track(scope.run(readHead));
    await turns();
    scope.hard.abort();
    await turns();
    timers.pastReadDeadline();
    await turns();
    assert.equal(read.settled(), false);
    const unsafe = new CheckpointUnsafeCleanupError("fixture root did not reap");
    child.release(unsafe);
    const outcome = await read.result();
    assert.ok("error" in outcome && outcome.error === unsafe);
  });

  it("never arms the timer when the scope was already stopped as the child launched", async t => {
    const timers = mockTimers(t);
    const child = heldChild();
    const scope = scopeFor(async () => { scope.hard.abort(); return child.handle; });
    const read = track(scope.run(readHead));
    await turns();
    assert.equal(child.cancels(), 0, "a hard stop requests no extra cancellation");
    timers.pastReadDeadline();
    await turns();
    assert.equal(read.settled(), false, "no timer was armed to fire the plain deadline error");
    child.release({ code: 0 });
    const outcome = await read.result();
    assert.ok("error" in outcome && outcome.error instanceof GitBoundaryAbortError, String("error" in outcome && outcome.error));
  });

  it("control: without a scope stop the timer still fails the read with the plain deadline error", async t => {
    const timers = mockTimers(t);
    const child = heldChild();
    const scope = scopeFor(async () => child.handle);
    const read = track(scope.run(readHead));
    await turns();
    assert.equal(read.settled(), false);
    timers.pastReadDeadline();
    const outcome = await read.result();
    assert.ok("error" in outcome);
    assert.equal((outcome.error as Error).constructor, Error);
    assert.match((outcome.error as Error).message, new RegExp(`exceeded ${TIMEOUT_MS}ms`));
    child.release({ code: 0 });
  });
});

describe("streamBundleWithCap 600 s timer inside a boundary scope (via produceRecoveryBundle)", () => {
  const realPipes: BoundaryProcessSpawner = async request => {
    const [command, ...args] = request.argv;
    const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => resolve({ code: code ?? 128 }));
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; } };
  };

  async function bundleScope(): Promise<{ scope: Scope; child: HeldChild; spawned: Promise<void>; produce: () => Promise<unknown> }> {
    const bare = await cache.ensureClone(fx.originPath);
    const sourceSha = execFileSync("git", ["-C", bare, "rev-parse", "refs/remotes/origin/main"], { encoding: "utf8", env: GIT_ENV }).trim();
    const out = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3304-bundle-")), "recovery.bundle");
    const child = heldChild();
    let markSpawned!: () => void;
    const spawned = new Promise<void>(resolve => { markSpawned = resolve; });
    const scope = scopeFor(request => {
      if (!request.argv.includes("bundle")) return realPipes(request);
      markSpawned();
      return Promise.resolve(child.handle);
    });
    return { scope, child, spawned, produce: () => scope.run(() => cache.produceRecoveryBundle(bare, { sourceSha, outPath: out })) };
  }

  for (const stop of ["hard", "soft"] as const) {
    it(`retires on the ${stop} stop: the pipeline is settled by the owner, not the plain deadline error`, async t => {
      const { scope, child, spawned, produce } = await bundleScope();
      const timers = mockTimers(t);
      const run = track(produce());
      await spawned;
      await turns();
      (stop === "hard" ? scope.hard : scope.soft).abort();
      await turns();
      const cancelsAtStop = child.cancels();
      if (stop === "soft") assert.equal(cancelsAtStop >= 1, true, "the soft stop requested the child's cancellation");
      timers.pastReadDeadline();
      await turns();
      assert.equal(child.cancels(), cancelsAtStop, "the retired timer killed nothing");
      assert.equal(run.settled(), false, "the retired timer did not fail the pipeline with its plain deadline error");
      child.release(stop === "hard" ? { code: 0 } : undefined);
      const outcome = await run.result();
      assert.ok("error" in outcome, "the stopped bundle fails");
      assert.ok(!/exceeded \d+ms/.test(String((outcome.error as Error).message)), `plain deadline error leaked: ${String(outcome.error)}`);
      assert.ok(outcome.error instanceof (stop === "hard" ? GitBoundaryAbortError : CheckpointSoftDeadlineError),
        `typed ${stop} error, got ${String(outcome.error)}`);
    });
  }

  it("control: without a scope stop the timer still kills the child and fails with the plain deadline error", async t => {
    const { child, spawned, produce } = await bundleScope();
    const timers = mockTimers(t);
    const run = track(produce());
    await spawned;
    await turns();
    assert.equal(child.cancels(), 0);
    timers.pastReadDeadline();
    await turns();
    assert.equal(child.cancels() >= 1, true, "the deadline kills the child");
    child.release({ code: 137 });
    const outcome = await run.result();
    assert.ok("error" in outcome);
    assert.match((outcome.error as Error).message, new RegExp(`exceeded ${TIMEOUT_MS}ms`));
  });
});
