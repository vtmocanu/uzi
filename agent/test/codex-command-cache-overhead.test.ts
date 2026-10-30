import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import { chmodSync, mkdirSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { cpus } from "node:os";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { PassThrough, type Readable } from "node:stream";
import { setTimeout as sleep } from "node:timers/promises";

import { HeldRunCommandCache, type CacheRotationOptions } from "../src/codex/codex-executor.js";
import { ExecutorLifetime } from "../src/codex/executor-lifetime.js";
import {
  CODEX_COMMAND_CACHE_ROOT,
  measureCommandCache,
  removeCommandCache,
  startCommandCacheHolder,
  type CacheCleanupResult,
  type CacheMeasureResult,
  type CodexRootHandle,
  type CommandCacheHolder,
  type SpawnStandaloneMode,
  type StandaloneModeProcess,
} from "../src/codex/launcher.js";

// Issue #3675 m4: overhead and contention EVIDENCE for command-cache measurement and rotation.
// Two env-gated scenarios, skipped by default (CI is unaffected). Each prints one JSON line per
// scenario via console.log; assertions are sanity only (no timing thresholds).
//   UZI_CACHE_OVERHEAD=1                   (a) "synthetic scheduler overhead": fake holders, fake measure.
//   UZI_CACHE_OVERHEAD_SUPERVISOR=<abs>    (b) "integrated real-helper": the production launcher
//     functions against a built uzi-codex-supervisor binary (see the substitution notes below).

const SYNTHETIC = process.env.UZI_CACHE_OVERHEAD === "1";
const SUPERVISOR = process.env.UZI_CACHE_OVERHEAD_SUPERVISOR ?? "";
const REMOVED: CacheCleanupResult = { state: "removed", reason: "" };
const WAITS = { releaseMs: 30_000, removeMs: 30_000 };
const SPEC = {} as Parameters<ReturnType<NonNullable<ReturnType<HeldRunCommandCache["reserve"]>>["launch"]>>[0];

/** The scratch directory the supervisor binary was built into (fixtures live beside it). */
const scratchDir = (): string => (SUPERVISOR === "" ? resolve("../.uzi/scratch") : dirname(SUPERVISOR));
const now = (): number => performance.now();
const round = (v: number): number => Math.round(v * 1000) / 1000;

interface Stats { readonly n: number; readonly median: number; readonly p95: number; readonly max: number }
function stats(values: readonly number[]): Stats {
  if (values.length === 0) return { n: 0, median: 0, p95: 0, max: 0 };
  const s = [...values].sort((a, b) => a - b);
  const at = (q: number): number => s[Math.min(s.length - 1, Math.max(0, Math.ceil(q * s.length) - 1))]!;
  return { n: s.length, median: round(at(0.5)), p95: round(at(0.95)), max: round(s[s.length - 1]!) };
}

function fakeRoot(): CodexRootHandle {
  return {
    started: { event: "started" },
    supervisorPid: 1,
    transport: { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
    snapshot: async () => ({}),
    observe: async () => ({}),
    waitChild: async () => ({ event: "child_exit", code: 0 }),
    dispose: async () => ({ clean: true, event: { event: "dispose" } }),
    failed: undefined,
    whenFailed: new Promise<Error>(() => undefined),
  } as unknown as CodexRootHandle;
}

function environment(): Record<string, unknown> {
  let fsType = "unknown";
  try { fsType = execFileSync("stat", ["-f", "-c", "%T", scratchDir()], { encoding: "utf8" }).trim(); } catch { /* reported as unknown */ }
  const cpu = cpus();
  return {
    event: "cache_overhead_environment",
    cpuModel: cpu[0]?.model ?? "unknown",
    cpuCount: cpu.length,
    node: process.version,
    platform: `${process.platform}/${process.arch}`,
    uid: process.getuid?.(),
    scratchFilesystem: fsType,
  };
}

interface Counters { readonly [message: string]: number }
function countingLog(into: Record<string, number>): { info(msg: string, fields?: unknown): void; warn(msg: string, fields?: unknown): void } {
  const bump = (level: string, msg: string, fields: unknown): void => {
    const state = typeof fields === "object" && fields !== null && "state" in fields ? `:${String((fields as { state: unknown }).state)}` : "";
    const key = `${level}:${msg.replace(/:.*$/, "")}${state}`;
    into[key] = (into[key] ?? 0) + 1;
  };
  return { info: (msg, fields) => bump("info", msg, fields), warn: (msg, fields) => bump("warn", msg, fields) };
}
const countOf = (counts: Counters, prefix: string): number =>
  Object.entries(counts).filter(([k]) => k.startsWith(prefix)).reduce((sum, [, v]) => sum + v, 0);

// ─── (a) synthetic scheduler overhead ──────────────────────────────────────────────

type SyntheticMode = "no-rotation-options" | "budget-0" | "rotation-enabled";
const SYNTHETIC_COMMANDS = 200;
const SYNTHETIC_THINK_MS = 5;
const SYNTHETIC_MEASURE_MS = 50;
const SYNTHETIC_START_MS = 20;
const SYNTHETIC_BYTES_PER_COMMAND = 1000;
const SYNTHETIC_BUDGET = 20 * SYNTHETIC_BYTES_PER_COMMAND;

async function syntheticScenario(mode: SyntheticMode): Promise<Record<string, unknown>> {
  const counts: Record<string, number> = {};
  const lifetime = new ExecutorLifetime();
  const used = new Map<string, number>();
  const holders: { readonly token: string; alive: boolean }[] = [];
  let measures = 0;
  let holderStarts = 0;
  const holderOf = (token: string): CommandCacheHolder => {
    const state = { token, alive: true };
    holders.push(state);
    return {
      token,
      path: `${CODEX_COMMAND_CACHE_ROOT}/${token}`,
      alive: () => state.alive,
      release: async () => { state.alive = false; return REMOVED; },
    };
  };
  const log = countingLog(counts);
  const options: CacheRotationOptions = {
    budgetBytes: mode === "budget-0" ? 0 : SYNTHETIC_BUDGET,
    measure: async (token, signal) => {
      measures += 1;
      await sleep(SYNTHETIC_MEASURE_MS, undefined, { signal }).catch(() => undefined);
      if (signal.aborted) return { state: "cancelled" } as CacheMeasureResult;
      return { state: "complete", bytes: (used.get(token) ?? 0) * SYNTHETIC_BYTES_PER_COMMAND };
    },
    startHolder: async (token, opts) => {
      holderStarts += 1;
      await sleep(SYNTHETIC_START_MS);
      opts.onSpawned(Promise.resolve(REMOVED));
      return holderOf(token);
    },
    admitCapture: () => undefined,
    remove: async () => REMOVED,
    log,
    waits: WAITS,
    minIntervalMs: 0,
  };
  const cache = new HeldRunCommandCache(holderOf(randomUUID()), undefined, lifetime, mode === "no-rotation-options" ? undefined : options);
  const reserveToLaunch: number[] = [];
  const disposeToReturn: number[] = [];
  let completed = 0;
  const start = now();
  for (let i = 0; i < SYNTHETIC_COMMANDS; i++) {
    const t0 = now();
    const reservation = cache.reserve();
    assert.ok(reservation, `command ${i}: a reservation is available`);
    let inner = 0;
    const handle = await reservation.launch(async () => { inner = now(); return fakeRoot(); })(SPEC);
    reserveToLaunch.push(inner - t0);
    const token = reservation.dir.slice(CODEX_COMMAND_CACHE_ROOT.length + 1);
    used.set(token, (used.get(token) ?? 0) + 1);
    await sleep(SYNTHETIC_THINK_MS);
    const t1 = now();
    await handle.dispose(10);
    disposeToReturn.push(now() - t1);
    completed += 1;
  }
  const elapsedMs = now() - start;
  const settleStart = now();
  const sealed = await lifetime.seal({}, () => cache.settle(log, async () => REMOVED, WAITS), () => true);
  const settleMs = now() - settleStart;
  const rotations = counts["info:codex command cache rotated"] ?? 0;
  const result = {
    label: "synthetic scheduler overhead",
    mode,
    commands: SYNTHETIC_COMMANDS,
    thinkMs: SYNTHETIC_THINK_MS,
    fakeMeasureLatencyMs: SYNTHETIC_MEASURE_MS,
    fakeHolderStartMs: SYNTHETIC_START_MS,
    budgetBytes: mode === "no-rotation-options" ? null : options.budgetBytes,
    elapsedMs: round(elapsedMs),
    settleMs: round(settleMs),
    reserveToLaunchMs: stats(reserveToLaunch),
    disposeToReturnMs: stats(disposeToReturn),
    measurements: measures,
    rotations,
    holdersStarted: holderStarts,
    holdersTotal: holders.length,
  };
  assert.equal(completed, SYNTHETIC_COMMANDS, "every command completed");
  assert.equal(sealed.kind, "sealed", "settle sealed cleanly");
  assert.ok(holders.every((h) => !h.alive), "every holder was released");
  if (mode === "rotation-enabled") {
    assert.ok(measures > 0 && rotations > 0, `rotation enabled must measure and rotate: ${JSON.stringify(result)}`);
    assert.equal(holders.length, rotations + 1, "one holder per generation");
  } else {
    assert.equal(measures, 0, "no measurement without a positive budget");
    assert.equal(rotations, 0);
    assert.equal(holders.length, 1);
  }
  return result;
}

describe("command cache overhead evidence (issue #3675 m4)", () => {
  describe("synthetic scheduler overhead", { skip: SYNTHETIC ? false : "UZI_CACHE_OVERHEAD=1 not set" }, () => {
    it("environment", () => { console.log(JSON.stringify(environment())); });
    for (const mode of ["no-rotation-options", "budget-0", "rotation-enabled"] as const) {
      it(`${mode}: ${SYNTHETIC_COMMANDS} fake commands`, async () => {
        console.log(JSON.stringify(await syntheticScenario(mode)));
      });
    }
  });

  describe("integrated real-helper", {
    skip: SUPERVISOR !== "" ? false : "UZI_CACHE_OVERHEAD_SUPERVISOR not set",
  }, () => {
    it("environment", () => { console.log(JSON.stringify(environment())); });
    const scenarios: readonly IntegratedConfig[] = [
      { name: "single-rotation-off", caches: 1, commands: 100, thinkMs: 10, rotate: false },
      { name: "single-rotation-on", caches: 1, commands: 100, thinkMs: 10, rotate: true },
      { name: "burst-rotation-off", caches: 1, commands: 100, thinkMs: 0, rotate: false },
      { name: "burst-rotation-on", caches: 1, commands: 100, thinkMs: 0, rotate: true },
      { name: "two-caches-rotation-off", caches: 2, commands: 100, thinkMs: 10, rotate: false },
      { name: "two-caches-rotation-on", caches: 2, commands: 100, thinkMs: 10, rotate: true },
    ];
    for (const config of scenarios) {
      it(config.name, async () => {
        console.log(JSON.stringify(await integratedScenario(config)));
      });
    }
  });
});

// ─── (b) integrated real-helper ────────────────────────────────────────────────────

const FILES_PER_COMMAND = 20;
const FILE_BYTES = 4096;
const INTEGRATED_BUDGET = 2 * 1024 * 1024;
const INTEGRATED_MIN_INTERVAL_MS = 20;
const MODES = ["--hold-cache", "--remove-cache", "--measure-cache", "--reap-orphans"];

interface IntegratedConfig {
  readonly name: string;
  readonly caches: number;
  readonly commands: number;
  readonly thinkMs: number;
  readonly rotate: boolean;
}

interface HelperRecord {
  readonly mode: string;
  readonly token: string;
  readonly spawnAt: number;
  exitAt?: number;
  closeAt?: number;
  code?: number | null;
}
interface MeasureRequest {
  readonly token: string;
  readonly requestAt: number;
  spawnAt?: number;
  doneAt?: number;
  state?: string;
}

/**
 * In the helper process layer the only substitution against production is the spawn:
 * `StandaloneModeDeps.spawn` execs the built binary directly (no setpriv), rewriting
 * `--expect-uid <N>` to this process's uid and `--cache-root` to the owned 0700 fixture root.
 * Because the helper then prints `cache_ready` with the fixture path while
 * `startCommandCacheHolder` requires `<CODEX_COMMAND_CACHE_ROOT>/<token>`, the --hold-cache stdout
 * has the fixture root text mapped back to the production root (a byte rewrite of the data chunks;
 * the process, its framing and its exit are untouched). The holder, measure, limiter, remove and
 * rotation code are production. The scenario also launches fake roots (no real Codex command; the
 * test writes the files itself) and uses a no-op capture-admission check. Not measured: the
 * setpriv/uid split, real Go/npm caches, large corpora, cold caches.
 */
function makeSpawn(binary: string, root: string, helpers: HelperRecord[], requests: Map<string, MeasureRequest>): SpawnStandaloneMode {
  return (_command, args, options) => {
    const modeAt = args.findIndex((a) => MODES.includes(a));
    assert.ok(modeAt >= 0, `no supervisor mode in argv: ${args.join(" ")}`);
    const argv = args.slice(modeAt);
    for (let i = 0; i < argv.length; i++) {
      if (argv[i] === "--expect-uid") argv[i + 1] = String(process.getuid!());
      if (argv[i] === "--cache-root") argv[i + 1] = root;
    }
    const mode = argv[0]!;
    const tokenAt = argv.indexOf("--cache-token");
    const token = tokenAt >= 0 ? argv[tokenAt + 1]! : "";
    const record: HelperRecord = { mode, token, spawnAt: now() };
    helpers.push(record);
    if (mode === "--measure-cache") {
      const request = requests.get(token);
      if (request && request.spawnAt === undefined) request.spawnAt = record.spawnAt;
    }
    const child = spawn(binary, argv, { cwd: options.cwd, env: options.env, stdio: [...options.stdio] });
    child.once("exit", () => { record.exitAt = now(); });
    child.once("close", (code) => { record.closeAt = now(); record.code = code; });
    if (mode !== "--hold-cache") return child as unknown as StandaloneModeProcess;

    const stdout = child.stdout!;
    const wrappers = new Map<(...a: unknown[]) => void, (chunk: Buffer | string) => void>();
    const mapBack = (chunk: Buffer | string): Buffer => Buffer.from(chunk.toString("utf8").split(root).join(CODEX_COMMAND_CACHE_ROOT));
    const view: Readable = new Proxy(stdout, {
      get(target, prop) {
        if (prop === "on" || prop === "once" || prop === "addListener") {
          return (event: string, fn: (...a: unknown[]) => void) => {
            if (event !== "data") { target.on(event, fn); return view; }
            const wrapper = (chunk: Buffer | string): void => (fn as (c: Buffer) => void)(mapBack(chunk));
            wrappers.set(fn, wrapper);
            target.on("data", wrapper);
            return view;
          };
        }
        if (prop === "off" || prop === "removeListener") {
          return (event: string, fn: (...a: unknown[]) => void) => {
            const wrapper = event === "data" ? wrappers.get(fn) : undefined;
            target.off(event, wrapper ?? fn);
            return view;
          };
        }
        const value: unknown = Reflect.get(target, prop, target);
        return typeof value === "function" ? (value as (...a: unknown[]) => unknown).bind(target) : value;
      },
    });
    return {
      pid: child.pid,
      stdin: child.stdin,
      stdout: view,
      once: child.once.bind(child),
      on: child.on.bind(child),
    } as unknown as StandaloneModeProcess;
  };
}

function cacheRootFixture(name: string): string {
  const root = join(scratchDir(), `overhead-cache-root-${process.pid}-${name}`);
  rmSync(root, { recursive: true, force: true });
  mkdirSync(root, { mode: 0o700 });
  chmodSync(root, 0o700); // scratch is setgid: mkdir yields 02700; the helper requires exactly 0700
  assert.equal(statSync(root).mode & 0o7777, 0o700, "fixture root is exactly 0700");
  return root;
}

function writeCommandWork(dir: string, id: string): void {
  const base = join(dir, "gocache", `cmd-${id}`);
  mkdirSync(base, { recursive: true, mode: 0o700 });
  const payload = Buffer.alloc(FILE_BYTES, 0x61);
  for (let i = 0; i < FILES_PER_COMMAND; i++) writeFileSync(join(base, `f${i}`), payload, { mode: 0o600 });
}

async function integratedScenario(config: IntegratedConfig): Promise<Record<string, unknown>> {
  assert.ok(isAbsolute(SUPERVISOR), "UZI_CACHE_OVERHEAD_SUPERVISOR must be an absolute path");
  const root = cacheRootFixture(config.name);
  try {
    const helpers: HelperRecord[] = [];
    const requests = new Map<string, MeasureRequest>();
    const measureRequests: MeasureRequest[] = [];
    const spawnReal = makeSpawn(SUPERVISOR, root, helpers, requests);
    const counts: Record<string, number> = {};
    const log = countingLog(counts);
    let maxDirs = 0;

    interface Instance { readonly cache: HeldRunCommandCache; readonly lifetime: ExecutorLifetime }
    const instances: Instance[] = [];
    for (let k = 0; k < config.caches; k++) {
      const lifetime = new ExecutorLifetime();
      const rotation: CacheRotationOptions | undefined = config.rotate ? {
        budgetBytes: INTEGRATED_BUDGET,
        // Production measureCommandCache with its module-level worker-wide FIFO limiter (no
        // `limiter` dep): every cache in this process queues on the same one.
        measure: async (token, signal) => {
          const request: MeasureRequest = { token, requestAt: now() };
          requests.set(token, request);
          measureRequests.push(request);
          const result = await measureCommandCache(token, { signal, spawn: spawnReal });
          request.doneAt = now();
          request.state = result.state === "unknown" ? `unknown:${result.reason}` : result.state;
          return result;
        },
        startHolder: (token, opts) => startCommandCacheHolder(token, { ...opts, spawn: spawnReal }),
        admitCapture: () => undefined,
        remove: (token, timeoutMs) => removeCommandCache(token, { timeoutMs, spawn: spawnReal }),
        log,
        waits: WAITS,
        minIntervalMs: INTEGRATED_MIN_INTERVAL_MS,
      } : undefined;
      const first = await startCommandCacheHolder(randomUUID(), { spawn: spawnReal });
      instances.push({ cache: new HeldRunCommandCache(first, undefined, lifetime, rotation), lifetime });
    }

    const reserveToLaunch: number[] = [];
    const disposeToReturn: number[] = [];
    const perCacheElapsed: number[] = [];
    let completed = 0;
    const sequence = async (instance: Instance, index: number): Promise<void> => {
      const start = now();
      for (let i = 0; i < config.commands; i++) {
        const t0 = now();
        const reservation = instance.cache.reserve();
        assert.ok(reservation, `cache ${index} command ${i}: a reservation is available`);
        let inner = 0;
        const handle = await reservation.launch(async () => { inner = now(); return fakeRoot(); })(SPEC);
        reserveToLaunch.push(inner - t0);
        writeCommandWork(reservation.dir.replace(CODEX_COMMAND_CACHE_ROOT, root), `${index}-${i}`);
        if (config.thinkMs > 0) await sleep(config.thinkMs);
        const t1 = now();
        await handle.dispose(10);
        disposeToReturn.push(now() - t1);
        maxDirs = Math.max(maxDirs, readdirSync(root).length);
        completed += 1;
      }
      perCacheElapsed.push(now() - start);
    };
    const wallStart = now();
    await Promise.all(instances.map(sequence));
    const wallMs = now() - wallStart;

    const settleStart = now();
    const sealed = await Promise.all(instances.map((i) =>
      i.lifetime.seal({}, () => i.cache.settle(log, (token, timeoutMs) => removeCommandCache(token, { timeoutMs, spawn: spawnReal }), WAITS), () => true)));
    const settleMs = now() - settleStart;
    const leftover = readdirSync(root);

    const measureHelpers = helpers.filter((h) => h.mode === "--measure-cache" && h.closeAt !== undefined);
    const spawnedRequests = measureRequests.filter((r) => r.spawnAt !== undefined);
    const rotations = counts["info:codex command cache rotated"] ?? 0;
    const result = {
      label: "integrated real-helper",
      scenario: config.name,
      caches: config.caches,
      commandsPerCache: config.commands,
      thinkMs: config.thinkMs,
      rotationEnabled: config.rotate,
      budgetBytes: config.rotate ? INTEGRATED_BUDGET : null,
      minIntervalMs: config.rotate ? INTEGRATED_MIN_INTERVAL_MS : null,
      bytesPerCommand: FILES_PER_COMMAND * FILE_BYTES,
      sequenceWallMs: round(wallMs),
      perCacheSequenceMs: perCacheElapsed.map(round),
      settleMs: round(settleMs),
      reserveToLaunchMs: stats(reserveToLaunch),
      disposeToReturnMs: stats(disposeToReturn),
      helperWork: {
        measureSpawnToCloseMs: stats(measureHelpers.map((h) => h.closeAt! - h.spawnAt)),
        measureSpawnToExitMs: stats(measureHelpers.filter((h) => h.exitAt !== undefined).map((h) => h.exitAt! - h.spawnAt)),
      },
      queueing: {
        requestToSpawnMs: stats(spawnedRequests.map((r) => r.spawnAt! - r.requestAt)),
        requestToResultMs: stats(measureRequests.filter((r) => r.doneAt !== undefined).map((r) => r.doneAt! - r.requestAt)),
      },
      counts: {
        measureRequests: measureRequests.length,
        measureSpawned: spawnedRequests.length,
        measureStates: measureRequests.reduce<Record<string, number>>((acc, r) => {
          const key = r.state ?? "unfinished";
          acc[key] = (acc[key] ?? 0) + 1;
          return acc;
        }, {}),
        holderSpawns: helpers.filter((h) => h.mode === "--hold-cache").length,
        removeCacheSpawns: helpers.filter((h) => h.mode === "--remove-cache").length,
        rotations,
        retiredMidRun: countOf(counts, "info:codex command cache retired"),
        releasedAtSettle: countOf(counts, "info:codex command cache released"),
        warnings: Object.fromEntries(Object.entries(counts).filter(([k]) => k.startsWith("warn:"))),
      },
      maxCacheDirsObserved: maxDirs,
      leftoverEntriesAfterSettle: leftover.length,
    };

    assert.equal(completed, config.caches * config.commands, "every command completed");
    assert.ok(sealed.every((s) => s.kind === "sealed"), `every lifetime sealed: ${JSON.stringify(sealed.map((s) => s.kind))}`);
    assert.deepEqual(leftover, [], "no generation directory remains after settle");
    assert.ok(helpers.every((h) => h.closeAt !== undefined), "every helper process closed");
    assert.equal(result.counts.removeCacheSpawns, 0, "no holder was lost, so no --remove-cache was needed");
    assert.equal(helpers.filter((h) => h.mode === "--hold-cache").length, config.caches + rotations, "one holder per generation");
    assert.ok(maxDirs <= 2 * config.caches, `at most the live + retiring generation per cache existed: ${maxDirs}`);
    if (config.rotate && config.thinkMs > 0) {
      assert.ok(rotations > 0, `rotation enabled must rotate: ${JSON.stringify(result.counts)}`);
      assert.ok(measureHelpers.length > 0, "real measurement helpers ran");
    }
    if (!config.rotate) {
      assert.equal(measureRequests.length, 0);
      assert.equal(rotations, 0);
    }
    return result;
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}
