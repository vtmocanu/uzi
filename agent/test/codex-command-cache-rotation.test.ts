import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";

import {
  HeldRunCommandCache,
  boundaryProcessSpawnerForTest,
  makeDefaultSpawnCommand,
  type CacheRotationOptions,
} from "../src/codex/codex-executor.js";
import { ExecutorLifetime } from "../src/codex/executor-lifetime.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import {
  CODEX_COMMAND_CACHE_ROOT,
  CommandCacheStartError,
  createMeasureLimiter,
  measureCommandCache,
  type CacheCleanupResult,
  type CacheMeasureResult,
  type CodexRootHandle,
  type CommandCacheHolder,
  type SpawnStandaloneMode,
  type StandaloneModeProcess,
} from "../src/codex/launcher.js";

// Issue #3675 m2: rotation of a run's command cache, driven at the HeldRunCommandCache seam with
// fake holders, controllable close promises and a scripted measurement. Nothing sleeps: every
// ordering is a gate (a deferred promise) and `flush` only yields the event loop.

const BUDGET = 1000;
const WAITS = { releaseMs: 25, removeMs: 25 };
const REMOVED: CacheCleanupResult = { state: "removed", reason: "" };
/** A lost holder's actual close: the helper exited without a cleanup line. */
const EXITED: CacheCleanupResult = { state: "error", reason: "exited" };
const flush = (): Promise<void> => new Promise<void>((r) => setImmediate(r));
const flushMany = async (n = 4): Promise<void> => { for (let i = 0; i < n; i++) await flush(); };

interface Deferred<T> { promise: Promise<T>; resolve(value: T): void; reject(error: unknown): void }
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

/** Yield the event loop until `cond` holds (bounded by iterations, not by wall time). */
async function until(cond: () => boolean, label: string): Promise<void> {
  for (let i = 0; i < 20_000 && !cond(); i++) await flush();
  assert.ok(cond(), `timed out waiting for ${label}`);
}

async function pending(p: Promise<unknown>): Promise<boolean> {
  let done = false;
  void p.then(() => { done = true; }, () => { done = true; });
  await flushMany(3);
  return !done;
}

interface FakeHolder {
  readonly holder: CommandCacheHolder;
  readonly token: string;
  readonly dir: string;
  /** Every release(drained) call, in order. */
  readonly releases: boolean[];
  /** The helper's actual close: resolves with the release outcome (a real holder closes after it). */
  readonly close: Promise<CacheCleanupResult>;
  die(): void;
}
function fakeHolder(result: () => Promise<CacheCleanupResult> | CacheCleanupResult = () => REMOVED): FakeHolder {
  const token = randomUUID();
  const dir = `${CODEX_COMMAND_CACHE_ROOT}/${token}`;
  let alive = true;
  const releases: boolean[] = [];
  const closed = deferred<CacheCleanupResult>();
  return {
    token,
    dir,
    releases,
    close: closed.promise,
    die: () => { alive = false; },
    holder: {
      token,
      path: dir,
      alive: () => alive,
      release: async (drained) => {
        releases.push(drained);
        alive = false;
        const outcome = await result();
        closed.resolve(outcome);
        return outcome;
      },
    },
  };
}

/** A minimal supervised root: only what HeldRunCommandCache's wrapper reads. */
function fakeRoot(clean = true): CodexRootHandle {
  return {
    started: { event: "started" },
    supervisorPid: 1,
    transport: { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
    snapshot: async () => ({}),
    observe: async () => ({}),
    waitChild: async () => ({ event: "child_exit", code: 0 }),
    dispose: async () => (clean ? { clean: true, event: { event: "dispose" } } : { clean: false, reason: "dispose unconfirmed" }),
    failed: undefined,
    whenFailed: new Promise<Error>(() => undefined),
  } as unknown as CodexRootHandle;
}
const SPEC = {} as Parameters<ReturnType<NonNullable<ReturnType<HeldRunCommandCache["reserve"]>>["launch"]>>[0];

interface MeasureCall { token: string; signal: AbortSignal; resolve(result: CacheMeasureResult): void }
interface StartCall {
  token: string;
  opts: { onSpawned(settlement: Promise<CacheCleanupResult>): void };
  /** Resolve with a holder (also announcing its settlement). */
  ready(holder: FakeHolder, close?: Promise<CacheCleanupResult>): void;
  fail(error: unknown): void;
}

function rig(config: {
  budget?: number;
  minIntervalMs?: number;
  holderResult?: () => Promise<CacheCleanupResult> | CacheCleanupResult;
  registry?: ExecutionRegistry;
} = {}) {
  const g1 = fakeHolder(config.holderResult);
  const lifetime = new ExecutorLifetime();
  const measures: MeasureCall[] = [];
  const starts: StartCall[] = [];
  const removals: string[] = [];
  const lines: string[] = [];
  const clock = { now: 1_000_000 };
  const capture = { closed: false };
  const options: CacheRotationOptions = {
    budgetBytes: config.budget ?? BUDGET,
    measure: (token, signal) => new Promise<CacheMeasureResult>((resolve) => {
      measures.push({ token, signal, resolve });
      signal.addEventListener("abort", () => resolve({ state: "cancelled" }), { once: true });
    }),
    startHolder: (token, opts) => new Promise<CommandCacheHolder>((resolve, reject) => {
      starts.push({
        token,
        opts,
        ready: (holder, close) => {
          if (close) opts.onSpawned(close);
          resolve(holder.holder);
        },
        fail: reject,
      });
    }),
    admitCapture: () => { if (capture.closed) throw new Error("capture admission closed"); },
    remove: async (token) => { removals.push(token); return REMOVED; },
    log: {
      info: (msg: string) => { lines.push(`info:${msg}`); },
      warn: (msg: string) => { lines.push(`warn:${msg}`); },
    },
    waits: WAITS,
    minIntervalMs: config.minIntervalMs ?? 0,
    now: () => clock.now,
  };
  const cache = new HeldRunCommandCache(g1.holder, undefined, lifetime, config.budget === 0 ? { ...options, budgetBytes: 0 } : options);
  if (config.registry) cache.watchRegistry(config.registry);
  const settleLog = {
    info: (msg: string) => { lines.push(`info:${msg}`); },
    warn: (msg: string) => { lines.push(`warn:${msg}`); },
  };
  const settle = (): Promise<void> => cache.settle(settleLog, async (token) => { removals.push(token); return REMOVED; }, WAITS);
  const seal = () => lifetime.seal({}, settle, () => true);
  return { cache, g1, lifetime, measures, starts, removals, lines, clock, capture, options, settle, seal };
}

/** Launch and keep open one reservation-launched root. */
async function openRoot(cache: HeldRunCommandCache, clean = true) {
  const reservation = cache.reserve();
  assert.ok(reservation, "a reservation is available");
  const handle = await reservation.launch(async () => fakeRoot(clean))(SPEC);
  return { dir: reservation.dir, dispose: () => handle.dispose(10) };
}
/** Launch and dispose one root: the measurement trigger. */
async function runRoot(cache: HeldRunCommandCache, clean = true): Promise<string> {
  const root = await openRoot(cache, clean);
  await root.dispose();
  return root.dir;
}

const OVER: CacheMeasureResult = { state: "complete", bytes: BUDGET };
const UNDER: CacheMeasureResult = { state: "complete", bytes: BUDGET - 1 };

describe("command cache rotation (issue #3675)", () => {
  describe("measurement and rotation-pending", () => {
    it("an under-budget measurement never rotates; an at-budget one starts exactly one replacement", async () => {
      const r = rig();
      await runRoot(r.cache);
      assert.equal(r.measures.length, 1);
      assert.equal(r.measures[0]!.token, r.g1.token);
      r.measures[0]!.resolve(UNDER);
      await flushMany();
      assert.equal(r.starts.length, 0);
      await runRoot(r.cache);
      r.measures[1]!.resolve(OVER);
      await flushMany();
      assert.equal(r.starts.length, 1, "bytes >= budget starts one replacement");
      assert.notEqual(r.starts[0]!.token, r.g1.token, "a fresh token");
      assert.ok(r.lines.includes("info:codex command cache rotation pending"));
    });

    it("an unknown measurement is rotation-pending exactly like an over-budget one", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve({ state: "unknown", reason: "io" });
      await flushMany();
      assert.equal(r.starts.length, 1);
    });

    it("a cancelled measurement changes nothing", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve({ state: "cancelled" });
      await flushMany();
      assert.equal(r.starts.length, 0);
      assert.equal(r.lines.includes("info:codex command cache rotation pending"), false);
    });

    it("a result applies only to the generation it measured", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      const g2 = fakeHolder();
      r.starts[0]!.ready(g2);
      await flushMany();
      // G2 is admitting and not pending: its own under-budget measurement does not rotate it.
      assert.equal(await runRoot(r.cache), g2.dir);
      assert.equal(r.measures[1]!.token, g2.token);
      r.measures[1]!.resolve(UNDER);
      await flushMany();
      assert.equal(r.starts.length, 1, "G1's over-budget result never marked G2");
    });

    it("a result that arrives after the run began settling is ignored", async () => {
      const r = rig();
      await runRoot(r.cache);
      const settling = r.settle();
      r.measures[0]!.resolve(OVER);
      await settling;
      assert.equal(r.starts.length, 0);
      assert.deepEqual(r.g1.releases, [true]);
    });

    it("admission stays on the pending generation until the replacement is ready, then switches", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      assert.equal(await runRoot(r.cache), r.g1.dir, "still G1 while the replacement starts");
      const g2 = fakeHolder();
      r.starts[0]!.ready(g2);
      await flushMany();
      assert.equal(await runRoot(r.cache), g2.dir);
      assert.ok(r.lines.includes("info:codex command cache rotated"));
    });

    it("budget 0 disables measurement and rotation entirely; reserve() is never undefined for size", async () => {
      const r = rig({ budget: 0 });
      for (let i = 0; i < 3; i++) assert.equal(await runRoot(r.cache), r.g1.dir);
      assert.equal(r.measures.length, 0, "measure is never called");
      assert.equal(r.starts.length, 0);
    });

    it("no rotation options behaves as before: no measurement, a single holder", async () => {
      const g = fakeHolder();
      const cache = new HeldRunCommandCache(g.holder);
      assert.equal(await runRoot(cache), g.dir);
      await cache.settle({ info() {}, warn() {} }, async () => REMOVED, WAITS);
      assert.deepEqual(g.releases, [true]);
    });

    it("measurement is skipped (not an error) while capture admission is closed", async () => {
      const r = rig();
      r.capture.closed = true;
      await runRoot(r.cache);
      assert.equal(r.measures.length, 0);
      r.capture.closed = false;
      await runRoot(r.cache);
      assert.equal(r.measures.length, 1);
    });

    it("coalesces triggers while one measurement is in flight and honours the minimum interval", async () => {
      const r = rig({ minIntervalMs: 10_000 });
      await runRoot(r.cache);
      await runRoot(r.cache);
      await runRoot(r.cache);
      assert.equal(r.measures.length, 1, "one in flight per generation");
      r.clock.now += 10_000;
      r.measures[0]!.resolve(UNDER);
      await flushMany();
      assert.equal(r.measures.length, 2, "the coalesced triggers run once more, after the interval");
      r.measures[1]!.resolve(UNDER);
      await flushMany();
      await runRoot(r.cache);
      assert.equal(r.measures.length, 2, "a trigger inside the interval does not start another");
      r.clock.now += 10_000;
      await runRoot(r.cache);
      assert.equal(r.measures.length, 3);
    });
  });

  describe("replacement startup", () => {
    it("a failed start latches: no second start, current keeps admitting, the token is never removed", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      r.starts[0]!.fail(new CommandCacheStartError("lock"));
      await flushMany();
      assert.ok(r.lines.includes("warn:codex command cache replacement failed; rotation blocked"));
      for (let i = 0; i < 3; i++) {
        assert.equal(await runRoot(r.cache), r.g1.dir, "the current generation keeps admitting");
        r.measures.at(-1)!.resolve(OVER);
        await flushMany();
      }
      assert.equal(r.starts.length, 1, "start called exactly once across further over-budget results");
      await r.settle();
      assert.deepEqual(r.removals, []);
      assert.deepEqual(r.g1.releases, [true]);
    });

    it("an adopted holder's unresolved close neither blocks the next rotation nor distrusts", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      const g2 = fakeHolder();
      const g2Close = deferred<CacheCleanupResult>();
      r.starts[0]!.ready(g2, g2Close.promise);
      await flushMany();
      assert.deepEqual(r.g1.releases, [true], "G1 retires once it is idle");
      await runRoot(r.cache);
      r.measures[1]!.resolve(OVER);
      await flushMany();
      assert.equal(r.starts.length, 2, "G2's close is still unresolved and does not block G3");
      const g3 = fakeHolder();
      r.starts[1]!.ready(g3, Promise.resolve(REMOVED));
      await flushMany();
      assert.equal(await runRoot(r.cache), g3.dir);
      // Resolve G2's close with a verdict that WOULD distrust if it were judged: it is only joined.
      g2Close.resolve({ state: "retained", reason: "unattested" });
      await flushMany();
      assert.deepEqual(g2.releases, [true]);
      assert.equal((await r.seal()).kind, "sealed", "a joined (not judged) adopted close does not distrust the seal");
    });

    it("a capture-admission refusal before any start clears the latch: a later root disposal re-measures and rotates", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.capture.closed = true;
      r.measures[0]!.resolve(OVER);
      await flushMany();
      assert.equal(r.starts.length, 0, "admission refused before anything was spawned");
      r.capture.closed = false;
      assert.equal(await runRoot(r.cache), r.g1.dir, "the pending generation still admits");
      assert.equal(r.measures.length, 2, "the next root disposal measures again");
      r.measures[1]!.resolve(OVER);
      await flushMany();
      assert.equal(r.starts.length, 1);
      const g2 = fakeHolder();
      r.starts[0]!.ready(g2, Promise.resolve(REMOVED));
      await flushMany();
      assert.equal(await runRoot(r.cache), g2.dir, "the replacement was adopted");
      assert.deepEqual(r.g1.releases, [true]);
    });

    it("a failed replacement start that created no process keeps the seal provable", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      // The holder never spawned: the launcher settles that as `absent`.
      const none = Promise.resolve<CacheCleanupResult>({ state: "absent", reason: "" });
      r.starts[0]!.opts.onSpawned(none);
      r.starts[0]!.fail(new CommandCacheStartError("spawn", none));
      await flushMany();
      assert.ok(r.lines.includes("warn:codex command cache replacement failed; rotation blocked"));
      assert.equal(await runRoot(r.cache), r.g1.dir);
      r.measures.at(-1)!.resolve(OVER);
      await flushMany();
      assert.equal(r.starts.length, 1, "the failed start is not retried");
      assert.equal((await r.seal()).kind, "sealed");
    });

    it("settling with no executor lifetime still chains the release after an unresolved measurement helper's close", async () => {
      const r = rig();
      const cache = new HeldRunCommandCache(r.g1.holder, undefined, undefined, r.options);
      await runRoot(cache);
      const helperClose = deferred<void>();
      r.measures[0]!.resolve({ state: "unknown", reason: "timeout", settlement: helperClose.promise });
      await flushMany();
      r.starts[0]!.fail(new CommandCacheStartError("timeout"));
      await cache.settle({ info: () => undefined, warn: () => undefined }, async () => REMOVED, WAITS);
      assert.deepEqual(r.g1.releases, [], "nothing is released while the helper may still read the directory");
      helperClose.resolve();
      await until(() => r.g1.releases.length === 1, "the chained release");
      assert.deepEqual(r.g1.releases, [true]);
    });

    it("a replacement that is ready after the run began settling is abandoned, released and judged by its own close", async () => {
      for (const verdict of ["removed", "retained"] as const) {
        const r = rig();
        await runRoot(r.cache);
        r.measures[0]!.resolve(OVER);
        await flushMany();
        const settling = r.settle();
        const g2 = fakeHolder();
        const close = deferred<CacheCleanupResult>();
        r.starts[0]!.ready(g2, close.promise);
        await settling;
        await flushMany();
        assert.deepEqual(g2.releases, [true], "the never-used replacement is released drained");
        assert.deepEqual(r.g1.releases, [true]);
        assert.ok(r.lines.includes("warn:codex command cache replacement abandoned"));
        const sealing = r.seal();
        assert.equal(await pending(sealing), true, "the abandoned holder's actual close keeps the seal pending");
        close.resolve(verdict === "removed" ? REMOVED : { state: "retained", reason: "unattested" });
        assert.equal((await sealing).kind, verdict === "removed" ? "sealed" : "unproven", verdict);
        assert.equal(r.starts.length, 1);
        assert.deepEqual(r.removals, [], "only own tokens, never removed here");
      }
    });

    it("a replacement ready after the current holder was lost is abandoned and commands stay uncached", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      r.g1.die();
      const g2 = fakeHolder();
      r.starts[0]!.ready(g2, Promise.resolve(REMOVED));
      await flushMany();
      assert.deepEqual(g2.releases, [true]);
      assert.equal(r.cache.reserve(), undefined, "rotation never revives a lost holder");
    });

    it("a replacement start does not run once the executor lifetime has closed", async () => {
      const r = rig();
      await runRoot(r.cache);
      const sealing = r.seal();
      r.measures[0]!.resolve(OVER);
      await sealing;
      assert.equal(r.starts.length, 0);
    });
  });

  describe("finite retirement", () => {
    async function rotated(config: Parameters<typeof rig>[0] = {}) {
      const r = rig(config);
      return r;
    }
    async function switchTo(r: Awaited<ReturnType<typeof rotated>>, g2: FakeHolder, index = 0): Promise<void> {
      r.measures.at(-1)!.resolve(OVER);
      await flushMany();
      r.starts[index]!.ready(g2, g2.close);
      await flushMany();
    }

    it("removal waits for a live root, then releases drained exactly once", async () => {
      const r = await rotated();
      const live = await openRoot(r.cache);
      await runRoot(r.cache);
      const g2 = fakeHolder();
      await switchTo(r, g2);
      assert.deepEqual(r.g1.releases, [], "G1 still has a live root");
      await live.dispose();
      await flushMany();
      assert.deepEqual(r.g1.releases, [true]);
    });

    it("removal waits for an outstanding reservation, and for a prepared but never invoked launch", async () => {
      for (const prepared of [false, true]) {
        const r = await rotated();
        const held = r.cache.reserve()!;
        const wrapped = prepared ? held.launch(async () => fakeRoot()) : undefined;
        await runRoot(r.cache);
        const g2 = fakeHolder();
        await switchTo(r, g2);
        assert.equal(wrapped === undefined || typeof wrapped === "function", true, "a wrapped launch was prepared but never invoked");
        assert.deepEqual(r.g1.releases, [], `prepared=${String(prepared)}: the reservation pins G1`);
        held.cancel();
        await flushMany();
        assert.deepEqual(r.g1.releases, [true], "released only after the reservation was cancelled");
      }
    });

    it("removal waits for an unresolved measurement helper, not for its delivery", async () => {
      const r = await rotated();
      await runRoot(r.cache);
      const helperClose = deferred<void>();
      r.measures[0]!.resolve({ state: "unknown", reason: "timeout", settlement: helperClose.promise });
      await flushMany();
      assert.equal(r.starts.length, 1, "the unknown delivery rotates");
      r.starts[0]!.ready(fakeHolder(), deferred<CacheCleanupResult>().promise);
      await flushMany();
      assert.deepEqual(r.g1.releases, [], "the helper may still be reading G1's directory");
      assert.ok(r.lines.includes("warn:codex command cache measurement helper unresolved"));
      helperClose.resolve();
      await flushMany();
      assert.deepEqual(r.g1.releases, [true]);
    });

    it("a stalled retirement (release retained) suppresses rotation while the current generation still admits", async () => {
      const r = await rotated({ holderResult: () => ({ state: "retained", reason: "mismatch" }) });
      await runRoot(r.cache);
      const g2 = fakeHolder();
      await switchTo(r, g2);
      assert.deepEqual(r.g1.releases, [true]);
      assert.ok(r.lines.includes("warn:codex command cache retirement stalled; rotation suppressed"));
      assert.equal(await runRoot(r.cache), g2.dir);
      r.measures.at(-1)!.resolve(OVER);
      await flushMany();
      assert.equal(r.starts.length, 1, "no further replacement while the retiring generation is stalled");
      assert.equal(await runRoot(r.cache), g2.dir, "G2 keeps admitting");
      assert.equal((await r.seal()).kind, "unproven", "a stalled retirement is a retained cleanup");
    });

    it("a pending release clears the retiring generation once its settlement is removed, and rotation resumes", async () => {
      const close = deferred<CacheCleanupResult>();
      let first = true;
      const r = await rotated({ holderResult: () => {
        if (!first) return REMOVED;
        first = false;
        return { state: "pending", reason: "", settlement: close.promise };
      } });
      await runRoot(r.cache);
      const g2 = fakeHolder(() => REMOVED);
      await switchTo(r, g2);
      assert.equal(await runRoot(r.cache), g2.dir);
      r.measures.at(-1)!.resolve(OVER);
      await flushMany();
      assert.equal(r.starts.length, 1, "blocked while G1's release is still pending");
      close.resolve(REMOVED);
      await flushMany();
      assert.equal(r.starts.length, 2, "rotation resumes once the retirement cleared");
    });

    it("a lost retiring holder is removed by token once drained", async () => {
      const r = await rotated();
      const live = await openRoot(r.cache);
      await runRoot(r.cache);
      await switchTo(r, fakeHolder());
      r.g1.die();
      await live.dispose();
      await flushMany();
      assert.deepEqual(r.removals, [r.g1.token]);
      assert.deepEqual(r.g1.releases, []);
    });

    it("a retiring generation that cannot be proven drained is left for the terminal settle, with no mid-run release or distrust", async () => {
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(3675));
      const r = rig({ registry });
      await runRoot(r.cache);
      registry.poison({ category: "protocol", message: "poisoned" });
      const g2 = fakeHolder();
      await switchTo(r, g2);
      assert.deepEqual(r.g1.releases, [], "nothing is released mid-run");
      assert.deepEqual(r.removals, []);
      assert.ok(r.lines.includes("warn:codex command cache retirement deferred to terminal settle"));
      const sealing = r.seal();
      assert.equal((await sealing).kind, "sealed", "a deferral distrusts nothing; the terminal settle judges with today's predicate");
      assert.deepEqual(r.g1.releases, [false]);
      assert.deepEqual(g2.releases, [false], "the new generation inherited the live poisoned registry");
    });

    it("registries watched after a rotation reach both the retiring and the new generation", async () => {
      const r = rig();
      const live = await openRoot(r.cache);
      await runRoot(r.cache);
      const g2 = fakeHolder();
      await switchTo(r, g2);
      const late = new ExecutionRegistry(newLocalExecutionEpoch(3676));
      r.cache.watchRegistry(late);
      late.poison({ category: "protocol", message: "late poison" });
      await live.dispose();
      await flushMany();
      assert.deepEqual(r.g1.releases, [], "G1 answers for the late registry too, so it is not drained");
      await r.settle();
      assert.deepEqual(r.g1.releases, [false]);
      assert.deepEqual(g2.releases, [false]);
    });

    it("a lost G2 holding an unproven poisoned registry is retained without a removal; the seal is unproven", async () => {
      const r = rig();
      await runRoot(r.cache);
      const g2 = fakeHolder();
      const g2Close = deferred<CacheCleanupResult>();
      r.measures.at(-1)!.resolve(OVER);
      await flushMany();
      r.starts[0]!.ready(g2, g2Close.promise);
      await flushMany();
      assert.deepEqual(r.g1.releases, [true], "G1 retired cleanly before the poison");
      const late = new ExecutionRegistry(newLocalExecutionEpoch(3678));
      r.cache.watchRegistry(late);
      late.poison({ category: "protocol", message: "poisoned after rotation" });
      g2.die();
      g2Close.resolve(EXITED);
      assert.equal(r.cache.reserve(), undefined, "holder loss is the uncached fallback");
      assert.equal((await r.seal()).kind, "unproven", "lost and not drained: retained and distrusted, as today");
      assert.deepEqual(r.removals, [], "a generation without accepted proof is never removed");
      assert.deepEqual(g2.releases, []);
    });

    it("after G2 holder loss, a verified removal seals clean, but an independent distrust before terminal cleanup still wins", async () => {
      for (const distrust of [false, true]) {
        const r = rig();
        await runRoot(r.cache);
        const g2 = fakeHolder();
        const g2Close = deferred<CacheCleanupResult>();
        r.measures.at(-1)!.resolve(OVER);
        await flushMany();
        r.starts[0]!.ready(g2, g2Close.promise);
        await flushMany();
        assert.deepEqual(r.g1.releases, [true]);
        assert.equal(await runRoot(r.cache), g2.dir);
        r.measures.at(-1)!.resolve(UNDER);
        await flushMany();
        g2.die();
        g2Close.resolve(EXITED);
        assert.equal(r.cache.reserve(), undefined, "commands after the loss run uncached");
        if (distrust) r.lifetime.distrust();
        const sealed = await r.seal();
        assert.deepEqual(r.removals, [g2.token], `distrust=${String(distrust)}: the drained lost generation is removed by token`);
        assert.equal(sealed.kind, distrust ? "unproven" : "sealed", "a successful removal never clears an independent distrust");
      }
    });

    it("G1 to G2 to G3: each is retired once, only own tokens are touched", async () => {
      const r = rig();
      await runRoot(r.cache);
      const g2 = fakeHolder();
      await switchTo(r, g2);
      await runRoot(r.cache);
      const g3 = fakeHolder();
      await switchTo(r, g3, 1);
      assert.equal(r.starts.length, 2);
      assert.deepEqual(r.g1.releases, [true]);
      assert.deepEqual(g2.releases, [true]);
      assert.deepEqual(g3.releases, []);
      await r.settle();
      assert.deepEqual(g3.releases, [true]);
      assert.deepEqual(r.removals, []);
    });
  });

  describe("reservations", () => {
    it("a launch that never reaches the real launch (pre-spawn abort) is a cancel, not an unclean root", async () => {
      const g = fakeHolder();
      const cache = new HeldRunCommandCache(g.holder);
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(3677));
      const aborted = new AbortController();
      aborted.abort();
      const spawn = makeDefaultSpawnCommand(
        registry, async () => fakeRoot(), 1000, "/work/repo", { PATH: "/usr/bin:/bin" }, "required", undefined, cache,
      );
      await assert.rejects(
        spawn(["/bin/true"], { cwd: "/work/repo", deadlineAt: Date.now() + 60_000, signal: aborted.signal }),
        /aborted before launch/,
      );
      assert.equal(cache.drained(), true, "the reservation was cancelled, nothing became unclean");
      await cache.settle({ info() {}, warn() {} }, async () => REMOVED, WAITS);
      assert.deepEqual(g.releases, [true]);
    });

    it("a command-identity boundary spawn whose spec fails to build before the launch releases its reservation", async () => {
      const r = rig();
      const env: NodeJS.ProcessEnv = {};
      Object.defineProperty(env, "UZI_UNREADABLE", { enumerable: true, get: () => { throw new Error("env unreadable"); } });
      let launched = 0;
      const spawner = boundaryProcessSpawnerForTest("required", async () => { launched++; throw new Error("unused"); }, undefined, r.cache);
      await assert.rejects(
        spawner({ argv: ["/usr/bin/git", "status"], cwd: "/data/runner/x", env, identity: "command" }, 1000),
        /env unreadable/,
      );
      assert.equal(launched, 0, "the real launch was never invoked");
      assert.equal(r.cache.drained(), true, "the reservation was cancelled, so the generation is not pinned");
    });

    it("launch is usable once; cancel is idempotent, releases, and is a no-op after the launch ran", async () => {
      const g = fakeHolder();
      const cache = new HeldRunCommandCache(g.holder);
      const first = cache.reserve()!;
      assert.equal(cache.drained(), false, "an outstanding reservation is not drained");
      const launch = first.launch(async () => fakeRoot());
      assert.throws(() => first.launch(async () => fakeRoot()), /already wrapped/);
      const handle = await launch(SPEC);
      assert.throws(() => launch(SPEC), /no longer available/, "a second invocation throws");
      first.cancel();
      assert.equal(cache.drained(), false, "cancel after the launch ran does not release the open root");
      await handle.dispose(10);
      assert.equal(cache.drained(), true);

      const second = cache.reserve()!;
      second.cancel();
      second.cancel();
      assert.equal(cache.drained(), true);
      const third = cache.reserve()!;
      const wrapped = third.launch(async () => fakeRoot());
      third.cancel();
      assert.throws(() => wrapped(SPEC), /no longer available/, "a cancelled reservation cannot launch");
      assert.equal(cache.drained(), true);
    });

    it("once undefined after settle or holder loss, reserve() stays undefined", async () => {
      const lost = fakeHolder();
      const cache = new HeldRunCommandCache(lost.holder);
      lost.die();
      assert.equal(cache.reserve(), undefined);
      assert.equal(cache.reserve(), undefined);
      const settled = new HeldRunCommandCache(fakeHolder().holder);
      void settled.settle({ info() {}, warn() {} }, async () => REMOVED, WAITS);
      assert.equal(settled.reserve(), undefined);
    });

    it("a rotation-capable cache after holder loss removes the lost token when drained, as today", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.g1.die();
      await r.settle();
      assert.deepEqual(r.removals, [r.g1.token]);
    });
  });

  describe("terminal settle", () => {
    it("cancels a queued measurement and still releases", async () => {
      const r = rig();
      await runRoot(r.cache);
      assert.equal(r.measures[0]!.signal.aborted, false);
      await r.settle();
      assert.equal(r.measures[0]!.signal.aborted, true, "the queued measurement was aborted");
      assert.deepEqual(r.g1.releases, [true]);
    });

    it("adopting a replacement aborts the replaced generation's queued measurement", async () => {
      const r = rig();
      await runRoot(r.cache);
      const helperClose = deferred<void>();
      r.measures[0]!.resolve({ state: "unknown", reason: "timeout", settlement: helperClose.promise });
      await flushMany();
      assert.equal(r.measures[0]!.signal.aborted, false);
      r.starts[0]!.ready(fakeHolder(), deferred<CacheCleanupResult>().promise);
      await flushMany();
      assert.equal(r.measures[0]!.signal.aborted, true);
      helperClose.resolve();
    });

    it("a measurement helper still running at settle chains the release after its actual close; nothing is deleted before", async () => {
      const r = rig();
      await runRoot(r.cache);
      const helperClose = deferred<void>();
      r.measures[0]!.resolve({ state: "unknown", reason: "timeout", settlement: helperClose.promise });
      await flushMany();
      // The unknown rotates; hold the replacement so the terminal settle meets G1 as current.
      const sealing = r.seal();
      await until(() => r.lines.some((l) => l.startsWith("warn:codex command cache measurement helper unresolved at settle")), "the settle wait to expire");
      assert.deepEqual(r.g1.releases, [], "no release before the helper closed");
      assert.deepEqual(r.removals, []);
      assert.equal(await pending(sealing), true, "the seal waits for the helper's actual close");
      // The held-back replacement start finally gives up (its own readiness bound).
      r.starts[0]!.fail(new CommandCacheStartError("timeout"));
      helperClose.resolve();
      assert.equal((await sealing).kind, "sealed");
      assert.deepEqual(r.g1.releases, [true]);
    });
  });

  describe("live helpers", () => {
    it("a replacement start timeout keeps the helper's settlement joined; the seal waits for its close and is unproven", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      const close = deferred<CacheCleanupResult>();
      // The helper is announced, then readiness times out while the process is still alive.
      r.starts[0]!.opts.onSpawned(close.promise);
      const sealing = r.seal();
      r.starts[0]!.fail(new CommandCacheStartError("timeout", close.promise));
      await flushMany();
      assert.ok(r.lines.includes("warn:codex command cache replacement failed; rotation blocked"));
      assert.equal(r.starts.length, 1, "no second replacement");
      assert.deepEqual(r.removals, [], "that token is never removed");
      assert.equal(await pending(sealing), true, "the live replacement helper keeps the seal pending");
      // It saw EOF without readiness and retained its directory.
      close.resolve({ state: "retained", reason: "unattested" });
      assert.equal((await sealing).kind, "unproven");
      assert.deepEqual(r.removals, []);
    });

    it("the same timeout while the current generation still admits leaves admission unchanged", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      const close = deferred<CacheCleanupResult>();
      r.starts[0]!.opts.onSpawned(close.promise);
      r.starts[0]!.fail(new CommandCacheStartError("timeout", close.promise));
      await flushMany();
      assert.equal(await runRoot(r.cache), r.g1.dir);
      close.resolve({ state: "error", reason: "exited" });
      await flushMany();
      assert.equal(await runRoot(r.cache), r.g1.dir);
      assert.equal(r.starts.length, 1);
    });

    it("a late ready after the lifetime closed is never adopted; the abandoned holder's close is judged", async () => {
      const r = rig();
      await runRoot(r.cache);
      r.measures[0]!.resolve(OVER);
      await flushMany();
      const sealing = r.seal();
      await flushMany();
      const g2 = fakeHolder();
      const close = deferred<CacheCleanupResult>();
      r.starts[0]!.ready(g2, close.promise);
      await flushMany();
      assert.deepEqual(g2.releases, [true]);
      assert.equal(await pending(sealing), true);
      close.resolve(REMOVED);
      assert.equal((await sealing).kind, "sealed");
      assert.equal(r.cache.reserve(), undefined);
    });
  });
});

// ─── real measurement helper (fake process) behind the real worker-wide limiter ─────────

class FakeHelper extends EventEmitter {
  readonly pid = 4242;
  readonly stdin = new PassThrough();
  readonly stdout = new PassThrough();
  killed = 0;
  kill(): boolean { this.killed += 1; return true; }
  finish(code: number): void {
    this.emit("exit", code, null);
    this.stdout.end();
    setImmediate(() => this.emit("close", code, null));
  }
}

describe("command cache rotation with the real measurement helper (issue #3675)", () => {
  function helpers() {
    const procs: FakeHelper[] = [];
    const spawn: SpawnStandaloneMode = () => {
      const p = new FakeHelper();
      procs.push(p);
      return p as unknown as StandaloneModeProcess;
    };
    return { procs, spawn };
  }

  function twoCaches(timeoutMs = 10) {
    const h = helpers();
    const deliveries: Promise<CacheMeasureResult>[] = [];
    const limiter = createMeasureLimiter();
    // Record kills instead of the production command-uid kill: the fake pid is not ours.
    const kills: number[] = [];
    const killAsCommandUid = (pid: number): boolean => { kills.push(pid); return true; };
    const make = () => {
      const base = rig();
      const options: CacheRotationOptions = {
        ...base.options,
        measure: (token, signal) => {
          const delivery = measureCommandCache(token, { spawn: h.spawn, signal, limiter, timeoutMs, killAsCommandUid });
          deliveries.push(delivery);
          return delivery;
        },
      };
      const lifetime = new ExecutorLifetime();
      const cache = new HeldRunCommandCache(base.g1.holder, undefined, lifetime, options);
      const settle = (): Promise<void> => cache.settle(
        { info: (m: string) => { base.lines.push(`info:${m}`); }, warn: (m: string) => { base.lines.push(`warn:${m}`); } },
        async (t) => { base.removals.push(t); return REMOVED; }, WAITS);
      return { ...base, cache, lifetime, options, settle, seal: () => lifetime.seal({}, settle, () => true) };
    };
    return { h, a: make(), b: make(), deliveries, kills };
  }

  it("a measurement delivery timeout holds the worker slot and pins its generation until the helper really closes", async () => {
    const { h, a, b, deliveries, kills } = twoCaches();
    await runRoot(a.cache);
    await runRoot(b.cache);
    await flushMany();
    assert.equal(h.procs.length, 1, "the second cache's measurement is queued behind the first helper");
    // The first delivery times out (10 ms), the unknown rotates cache A; the helper is still open.
    assert.equal((await deliveries[0]!).state, "unknown");
    await flushMany();
    assert.equal(a.starts.length, 1, "unknown rotates");
    assert.equal(h.procs.length, 1, "the slot is held past the delivery timeout");
    assert.deepEqual(kills, [h.procs[0]!.pid], "the timed-out helper is killed through the injected command-uid kill");
    const g2 = fakeHolder();
    a.starts[0]!.ready(g2, deferred<CacheCleanupResult>().promise);
    await flushMany();
    assert.deepEqual(a.g1.releases, [], "G1 is not retired while its measurement helper is open");
    assert.deepEqual(a.removals, []);
    h.procs[0]!.finish(0);
    await until(() => h.procs.length === 2, "the queued measurement to spawn");
    await flushMany();
    assert.equal(h.procs.length, 2, "the queued measurement spawns only after the first helper closed");
    assert.deepEqual(a.g1.releases, [true]);
    h.procs[1]!.finish(0);
    await flushMany();
    assert.equal(h.procs.length, 2);
  });

  it("a measurement timeout followed by terminal teardown chains the release after the close; the seal stays pending until then", async () => {
    const { h, a, deliveries, kills } = twoCaches();
    await runRoot(a.cache);
    assert.equal((await deliveries[0]!).state, "unknown");
    await flushMany();
    assert.equal(h.procs.length, 1);
    const sealing = a.seal();
    await until(() => a.lines.some((l) => l.startsWith("warn:codex command cache measurement helper unresolved at settle")), "the settle wait to expire");
    assert.equal(await pending(sealing), true);
    assert.deepEqual(a.g1.releases, [], "nothing is deleted before the helper closes");
    assert.deepEqual(a.removals, []);
    assert.equal(await pending(sealing), true);
    assert.deepEqual(kills, [h.procs[0]!.pid], "the kill goes only through the injected command-uid kill");
    assert.equal(h.procs[0]!.killed, 0, "never through ChildProcess.kill");
    a.starts[0]!.fail(new CommandCacheStartError("timeout"));
    h.procs[0]!.finish(0);
    assert.equal((await sealing).kind, "sealed");
    assert.deepEqual(a.g1.releases, [true]);
  });
});
