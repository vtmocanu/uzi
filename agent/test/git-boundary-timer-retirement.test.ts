import { afterEach, beforeEach, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PassThrough, Writable } from "node:stream";
import { GitCache, type BoundaryProcessSpawner } from "../src/git.js";
import { CheckpointSoftDeadlineError, CheckpointUnsafeCleanupError, GitBoundaryAbortError } from "../src/checkpoint-stop.js";
import { CodexBoundaryError, type SpawnedBoundaryProcess } from "../src/codex/safety.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import type { ReapOutcome } from "../src/codex/registry.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { codexRig, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";

// Issue #3304: inside a boundary scope, the 600 s read timers of `readBare` and `streamBundleWithCap` retire
// when the scope stops (hard signal, or the soft signal of an unmasked scope). A retired timer never settles,
// abandons or kills anything and is never re-armed: the owner settles the read with its typed error once the
// child's cancellation and completion have settled.
//
// The scope is the real one: `codexRig`'s Codex safety facade (permit, registry, reap/dispose ownership) with
// `GitCache.withBoundaryProcessSpawner` bound to `safety.spawnBoundaryProcess(permit, request)`. Only the child
// is faked: its stdout never ends and its root's dispose/reap stay pending until the test releases them. The
// hard expiry is the rig's virtual boundary clock; Node MockTimers stand in for the 600 s read wait.
//
// Under that facade the Codex handle's completion settles the bundle stream on its own, so the real-facade
// bundle cases show the typed outcome, not the timer retirement. The bundle retirement is pinned by the
// TickSpawner-shaped cases at the end (a spawner whose completion stays pending until the test releases it).

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const UNUSED_BARE = "/nonexistent-3304/bare.git";
const TIMEOUT_MS = 600_000;
const BOUNDARY_MS = 1_800_000;

type CleanupMode = "clean" | "reap-fails" | "dispose-throws";

interface HeldChild {
  ready: Promise<void>;
  spawned: () => SpawnedBoundaryProcess;
  spawns: () => number;
  disposes: () => number;
  reaps: () => number;
  /** Let the root's pending dispose/reap finish: cleanly, or with the named failure. */
  release: (mode?: CleanupMode) => void;
}

function heldChild(): HeldChild {
  let markReady!: () => void;
  const ready = new Promise<void>(resolve => { markReady = resolve; });
  let spawns = 0, disposes = 0, reaps = 0;
  let open!: (mode: CleanupMode) => void;
  const gate = new Promise<CleanupMode>(resolve => { open = resolve; });
  return {
    ready,
    spawns: () => spawns, disposes: () => disposes, reaps: () => reaps,
    release: mode => open(mode ?? "clean"),
    spawned: () => {
      spawns++;
      markReady();
      const stdout = new PassThrough(), stderr = new PassThrough();
      for (const stream of [stdout, stderr]) stream.on("error", () => undefined);
      return {
        root: {
          kind: "boundary_action",
          dispose: async () => {
            disposes++;
            if (await gate === "dispose-throws") throw new Error("fixture dispose failed");
          },
          reap: async (): Promise<ReapOutcome> => {
            reaps++;
            return await gate === "reap-fails"
              ? { ok: false, error: { category: "timeout", message: "fixture root did not reap" } }
              : { ok: true };
          },
        },
        stdin: new Writable({ write: (_chunk, _enc, cb) => cb() }), stdout, stderr,
        waitChild: () => new Promise(() => undefined),
      };
    },
  };
}

/** Real macrotask turns: MockTimers owns setTimeout here, so never wait on a timer. */
async function turns(count = 60): Promise<void> {
  for (let i = 0; i < count; i++) await new Promise<void>(resolve => setImmediate(resolve));
}

type Outcome = { ok: unknown } | { error: unknown };
function track<T>(promise: Promise<T>): { settled: () => boolean; result: () => Promise<Outcome> } {
  let done = false;
  const result: Promise<Outcome> = promise.then(ok => ({ ok }), error => ({ error }));
  void result.then(() => { done = true; });
  return { settled: () => done, result: () => result };
}

let fx: Fixture;
let cache: GitCache;
beforeEach(() => { fx = makeFixture(); cache = new GitCache(fx.dataDir, nullLogger()); });
afterEach(() => fx.cleanup());

interface Scope {
  child: HeldChild;
  soft: AbortController;
  /** Expire the boundary's hard deadline on the rig's virtual clock. */
  expireHard: () => void;
  /** Runs `fn` inside a real finalize permit; `inner` is `fn`'s own outcome, `outer` the boundary's. */
  run: <T>(fn: () => Promise<T>) => { inner: ReturnType<typeof track>; outer: ReturnType<typeof track> };
  rig: ReturnType<typeof codexRig>;
}

function scope(match: (request: BoundaryProcessRequest) => boolean, beforeLaunch?: (scope: Scope) => void): Scope {
  const clock = virtualBoundaryClock();
  const child = heldChild();
  const soft = new AbortController();
  const self = {} as Scope;
  const rig = codexRig({
    armDeadline: clock.armDeadline, now: clock.now,
    boundaryProcess: request => {
      if (!match(request)) return undefined;
      beforeLaunch?.(self);
      return child.spawned();
    },
  });
  Object.assign(self, { child, soft, rig, expireHard: () => clock.advance(BOUNDARY_MS + 1),
    run: <T>(fn: () => Promise<T>) => {
      let resolveInner!: (outcome: Promise<T>) => void;
      const innerPromise = new Promise<T>(resolve => { resolveInner = resolve; });
      const outer = rig.safety.withBoundary({ boundary: "finalize", deadlineMs: BOUNDARY_MS }, permit =>
        cache.withBoundaryProcessSpawner(request => rig.safety.spawnBoundaryProcess(permit, request),
          permit.signal, () => { const p = fn(); resolveInner(p); return p; }, { softSignal: soft.signal }));
      return { inner: track(innerPromise), outer: track(outer) };
    } });
  return self;
}

const isRevParse = (request: BoundaryProcessRequest): boolean => request.argv.includes("rev-parse");
const isBundle = (request: BoundaryProcessRequest): boolean => request.argv.includes("bundle");
const readHead = (): Promise<unknown> => cache.readBare(UNUSED_BARE, ["rev-parse", "HEAD"], { maxBytes: 1024 });

function mockTimers(t: TestContext): { pastReadDeadline: () => void } {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  return { pastReadDeadline: () => t.mock.timers.tick(TIMEOUT_MS + 1) };
}

function errorOf(outcome: Outcome): unknown {
  assert.ok("error" in outcome, "the stopped read fails");
  return outcome.error;
}

function assertTyped(error: unknown, expected: abstract new (...args: never[]) => Error): void {
  assert.ok(error instanceof expected, `expected ${expected.name}, got ${String(error)}`);
  assert.notEqual((error as Error).constructor, Error, "never a plain Error");
  assert.equal(error instanceof CheckpointUnsafeCleanupError, expected === CheckpointUnsafeCleanupError,
    `unsafe-cleanup classification: ${String(error)}`);
  assert.ok(!/exceeded \d+ms/.test((error as Error).message), `plain deadline error leaked: ${String(error)}`);
}

describe("readBare 600 s timer inside a real boundary scope", () => {
  it("retires on the hard stop: nothing settles while cleanup is pending, then the typed hard outcome", async t => {
    const timers = mockTimers(t);
    const s = scope(isRevParse);
    const { inner, outer } = s.run(readHead);
    await turns();
    assert.equal(s.child.spawns(), 1);
    assert.equal(inner.settled(), false);
    s.expireHard();
    await turns();
    assert.equal(s.child.disposes() >= 1, true, "the real safety disposes the root on the permit abort");
    assert.equal(inner.settled(), false, "the read waits for the real cleanup");
    timers.pastReadDeadline();
    await turns();
    assert.equal(inner.settled(), false, "the retired timer did not settle the read with its plain deadline error");
    assert.equal(outer.settled(), false);
    s.child.release("clean");
    const refusal = errorOf(await inner.result());
    assertTyped(refusal, TrustedExecutionRefusal);
    assert.match((refusal as Error).message, /boundary process deadline exceeded/);
    assert.ok(errorOf(await outer.result()) instanceof CodexBoundaryError, "the boundary owner reports its typed failure");
    assert.equal(s.rig.registry.isPoisoned(), true);
  });

  it("retires on the soft stop: the typed soft outcome after the cleanup", async t => {
    const timers = mockTimers(t);
    const s = scope(isRevParse);
    const { inner } = s.run(readHead);
    await turns();
    s.soft.abort();
    await turns();
    assert.equal(s.child.disposes() >= 1, true, "the soft stop cancelled the child through the real safety");
    assert.equal(inner.settled(), false);
    timers.pastReadDeadline();
    await turns();
    assert.equal(inner.settled(), false, "the retired timer did not settle the read");
    s.child.release("clean");
    assertTyped(errorOf(await inner.result()), CheckpointSoftDeadlineError);
    assert.equal(s.rig.registry.isPoisoned(), false, "a clean soft cleanup leaves the registry unpoisoned");
  });

  for (const mode of ["reap-fails", "dispose-throws"] as const) {
    it(`hard stop with ${mode}: the unclean outcome, still without the plain deadline error`, async t => {
      const timers = mockTimers(t);
      const s = scope(isRevParse);
      const { inner, outer } = s.run(readHead);
      await turns();
      s.expireHard();
      await turns();
      timers.pastReadDeadline();
      await turns();
      assert.equal(inner.settled(), false);
      s.child.release(mode);
      assertTyped(errorOf(await inner.result()), CheckpointUnsafeCleanupError);
      assert.ok(errorOf(await outer.result()) instanceof CodexBoundaryError);
      assert.equal(s.rig.registry.isPoisoned(), true, "an unclean cleanup poisons the registry");
    });
  }

  it("never arms the timer when the scope was already stopped as the child launched", async t => {
    const timers = mockTimers(t);
    const s = scope(isRevParse, self => self.expireHard());
    const { inner } = s.run(readHead);
    await turns();
    timers.pastReadDeadline();
    await turns();
    assert.equal(inner.settled(), false, "no timer was armed to fire the plain deadline error");
    s.child.release("clean");
    const refusal = errorOf(await inner.result());
    assertTyped(refusal, TrustedExecutionRefusal);
    assert.match((refusal as Error).message, /boundary process deadline exceeded/);
    assert.equal(s.child.disposes(), 1);
    assert.equal(s.child.reaps(), 1);
  });

  it("control: without a scope stop the timer still fails the read with the plain deadline error", async t => {
    const timers = mockTimers(t);
    const s = scope(isRevParse);
    const { inner } = s.run(readHead);
    await turns();
    assert.equal(inner.settled(), false);
    timers.pastReadDeadline();
    await turns();
    s.child.release("clean");
    const error = errorOf(await inner.result());
    assert.equal((error as Error).constructor, Error);
    assert.match((error as Error).message, new RegExp(`exceeded ${TIMEOUT_MS}ms`));
  });
});

describe("streamBundleWithCap 600 s timer inside a real boundary scope (via produceRecoveryBundle)", () => {
  function bundleOutput(t: TestContext): string {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3304-bundle-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    return path.join(dir, "recovery.bundle");
  }

  async function bundleScope(t: TestContext) {
    const bare = await cache.ensureClone(fx.originPath);
    const sourceSha = execFileSync("git", ["-C", bare, "rev-parse", "refs/remotes/origin/main"], { encoding: "utf8", env: GIT_ENV }).trim();
    const out = bundleOutput(t);
    const s = scope(isBundle);
    return { s, produce: () => s.run(() => cache.produceRecoveryBundle(bare, { sourceSha, outPath: out })) };
  }

  for (const stop of ["hard", "soft"] as const) {
    it(`streamBundleWithCap under the real Codex facade: the pipeline settles with the typed ${stop} error`, async t => {
      const { s, produce } = await bundleScope(t);
      const timers = mockTimers(t);
      const { inner } = produce();
      await s.child.ready;
      assert.equal(s.child.spawns(), 1, "the bundle child launched through the real safety");
      await turns();
      if (stop === "hard") s.expireHard(); else s.soft.abort();
      await turns();
      assert.equal(s.child.disposes() >= 1, true, "the real safety disposed the root");
      timers.pastReadDeadline();
      await turns();
      s.child.release("clean");
      const error = errorOf(await inner.result());
      assertTyped(error, stop === "hard" ? GitBoundaryAbortError : CheckpointSoftDeadlineError);
    });
  }

  // TickSpawner-shaped, completion-pending spawner: a non-Codex boundary spawner (hard-only in the runner)
  // whose handle `completed` stays pending until the test releases it. Nothing else settles the stream, so
  // only a retired (not re-armed) timer keeps the plain deadline error and the kill away.
  type Terminal = { code: number; softTimedOut?: true };
  function pendingCompletionChild() {
    const stdout = new PassThrough(), stderr = new PassThrough();
    for (const stream of [stdout, stderr]) stream.on("error", () => undefined);
    let cancels = 0;
    let release!: (outcome?: Terminal) => void;
    const completed = new Promise<Terminal>(resolve => { release = outcome => resolve(outcome ?? { code: -1 }); });
    const handle: BoundaryProcessHandle = {
      stdin: new Writable({ write: (_chunk, _enc, cb) => cb() }), stdout, stderr, completed,
      cancel: async () => { cancels++; await completed; },
    };
    return { handle, cancels: () => cancels, release };
  }

  for (const stop of ["hard", "soft", "none"] as const) {
    it(`readBare nonzero TickSpawner-shaped completion after ${stop} stop preserves its classification`, async () => {
      const child = pendingCompletionChild();
      const hard = new AbortController(), soft = new AbortController();
      // TickSpawner owns hard cancellation and resolves completed after verified group cleanup.
      hard.signal.addEventListener("abort", () => { void child.handle.cancel(); }, { once: true });
      const run = track(cache.withBoundaryProcessSpawner(async () => child.handle, hard.signal, readHead,
        { softSignal: soft.signal }));
      await turns();
      if (stop !== "none") (stop === "hard" ? hard : soft).abort();
      await turns();
      assert.equal(run.settled(), false, "owned completion is still pending");
      if (stop !== "none") assert.equal(child.cancels(), 1, "the owner requested cancellation");
      child.release({ code: 128 });
      const error = errorOf(await run.result());
      if (stop === "none") {
        assert.equal((error as Error).constructor, Error);
        assert.match((error as Error).message, /exited 128/);
      } else assertTyped(error, stop === "hard" ? GitBoundaryAbortError : CheckpointSoftDeadlineError);
    });
  }

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

  for (const stop of ["hard", "soft"] as const) {
    it(`TickSpawner-shaped completion-pending spawner, ${stop} stop: the timer retires, the owner settles after release`, async t => {
      const bare = await cache.ensureClone(fx.originPath);
      const sourceSha = execFileSync("git", ["-C", bare, "rev-parse", "refs/remotes/origin/main"], { encoding: "utf8", env: GIT_ENV }).trim();
      const out = bundleOutput(t);
      const child = pendingCompletionChild();
      let markSpawned!: () => void;
      const spawned = new Promise<void>(resolve => { markSpawned = resolve; });
      const hard = new AbortController(), soft = new AbortController();
      const spawner: BoundaryProcessSpawner = request => {
        if (!request.argv.includes("bundle")) return realPipes(request);
        markSpawned();
        return Promise.resolve(child.handle);
      };
      const timers = mockTimers(t);
      const run = track(cache.withBoundaryProcessSpawner(spawner, hard.signal,
        () => cache.produceRecoveryBundle(bare, { sourceSha, outPath: out }), { softSignal: soft.signal }));
      await spawned;
      await turns();
      (stop === "hard" ? hard : soft).abort();
      await turns();
      const cancelsAtStop = child.cancels();
      timers.pastReadDeadline();
      await turns();
      assert.equal(child.cancels(), cancelsAtStop, "the retired timer killed nothing");
      assert.equal(run.settled(), false, "the retired timer did not fail the pipeline with its plain deadline error");
      child.release(stop === "hard" ? { code: 0 } : undefined);
      const error = errorOf(await run.result());
      assertTyped(error, stop === "hard" ? GitBoundaryAbortError : CheckpointSoftDeadlineError);
    });
  }

  it("control: without a scope stop the timer still kills the child and fails with the plain deadline error", async t => {
    const { s, produce } = await bundleScope(t);
    const timers = mockTimers(t);
    const { inner } = produce();
    await s.child.ready;
    await turns();
    assert.equal(s.child.disposes(), 0);
    timers.pastReadDeadline();
    await turns();
    assert.equal(s.child.disposes() >= 1, true, "the deadline kills the child");
    s.child.release("clean");
    const error = errorOf(await inner.result());
    assert.match((error as Error).message, new RegExp(`exceeded ${TIMEOUT_MS}ms`));
  });
});
