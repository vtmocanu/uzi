import { AssertionError } from "node:assert/strict";
import { execFileSync, spawn, spawnSync, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { TestContext } from "node:test";
import { fileURLToPath } from "node:url";
import type { AuthorityLockOptions, FlockResult } from "../src/authority-lock.js";

// Shared support for the completion-authority tests. The real kernel flock is always used.
// A host whose flock is not util-linux (BusyBox) gets an argument-translating adapter, as
// test/pod-epoch-lock.test.ts does; the production trust check against that host is
// asserted separately by the tests.

const host = spawnSync("/usr/bin/flock", ["--version"], { encoding: "utf8" });
export const utilLinux = host.status === 0 && host.stdout.startsWith("flock from util-linux ");

function adapter(args: string[], fd: number | undefined): Promise<FlockResult> {
  if (args[0] === "--version") return Promise.resolve({ code: 0, output: "flock from util-linux 2.41\n" });
  return new Promise((resolve, reject) => {
    const child = spawn("/usr/bin/flock", ["-x", "-n", "3"], { stdio: ["ignore", "ignore", "ignore", fd!] });
    child.once("error", reject);
    child.once("close", (code) => resolve({ code: code === 1 ? 200 : (code ?? 2), output: "" }));
  });
}

/** Options for a test host: tmp is rarely ext4/XFS, so the statfs check is injected. */
export function testLockOptions(extra: AuthorityLockOptions = {}): AuthorityLockOptions {
  return { supportedFs: async () => true, ...(utilLinux ? {} : { run: adapter }), ...extra };
}

export function makeTmp(t: TestContext, prefix = "authority-"): string {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), prefix)));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// ---- issue #4091: bounded waits, a manual clock and FIFO directories that always release their blocked opens ----

/** A pid is alive unless /proc says it is gone or a zombie. */
function pidAlive(pid: number): boolean {
  try {
    const stat = fs.readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3) !== "Z";
  } catch { return false; }
}

/**
 * Wait for `work`, failing with a named AssertionError after `ms` of REAL time or as soon as any of `raceWith` rejects.
 * The timer is unref'd and cleared; a `work` left pending is never an unhandled rejection.
 */
export async function withinBound<T>(work: Promise<T>, ms: number, message: string, ...raceWith: Promise<never>[]): Promise<T> {
  work.catch(() => undefined);
  let timer: NodeJS.Timeout | undefined;
  const expired = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => reject(new AssertionError({ message })), ms);
    timer.unref();
  });
  try { return await Promise.race([work, expired, ...raceWith]); }
  finally { clearTimeout(timer); }
}

/** Read a FIFO to EOF; fails when no writer reaches it within `ms`. fifoDir teardown releases the blocked open. */
export function readFifoBounded(fifo: string, ms: number, what: string, ...raceWith: Promise<never>[]): Promise<string> {
  return withinBound(fsp.readFile(fifo, "utf8"), ms, `no writer reached ${what} within ${ms}ms`, ...raceWith);
}

/** Write to a FIFO; fails when no reader opens it within `ms`. fifoDir teardown releases the blocked open. */
export function writeFifoBounded(fifo: string, data: string, ms: number, what: string): Promise<void> {
  return withinBound(fsp.writeFile(fifo, data), ms, `no reader for ${what} within ${ms}ms`);
}

export interface ManualClock {
  now(): number;
  schedule(fn: () => void, ms: number): () => void;
  /** Move now forward, firing every due callback in due-time order (including ones scheduled while firing). */
  advance(ms: number): void;
  /** Resolves once a schedule() call (past or future) asked for an ms matching `pred`; else fails after `realBoundMs` of real time. */
  scheduled(pred: (ms: number) => boolean, realBoundMs: number): Promise<void>;
}

/** A frozen clock seeded from the real one: time passes only through advance(). */
export function manualClock(): ManualClock {
  let current = Date.now();
  let seq = 0;
  const timers = new Map<number, { due: number; fn: () => void }>();
  const requested: number[] = [];
  const watchers = new Set<{ pred: (ms: number) => boolean; wake: () => void }>();
  return {
    now: () => current,
    schedule(fn, ms) {
      const id = seq++;
      timers.set(id, { due: current + ms, fn });
      requested.push(ms);
      for (const watcher of watchers) if (watcher.pred(ms)) watcher.wake();
      return () => { timers.delete(id); };
    },
    advance(ms) {
      const target = current + ms;
      for (;;) {
        let next: { id: number; due: number } | undefined;
        for (const [id, timer] of timers) {
          if (timer.due <= target && (next === undefined || timer.due < next.due)) next = { id, due: timer.due };
        }
        if (next === undefined) break;
        const timer = timers.get(next.id)!;
        timers.delete(next.id);
        current = Math.max(current, timer.due);
        timer.fn();
      }
      current = target;
    },
    scheduled(pred, realBoundMs) {
      if (requested.some(pred)) return Promise.resolve();
      return new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => {
          watchers.delete(watcher);
          reject(new AssertionError({ message: `no timer matching ${pred.toString()} was scheduled within ${realBoundMs}ms` }));
        }, realBoundMs);
        timer.unref();
        const watcher = { pred, wake: () => { clearTimeout(timer); watchers.delete(watcher); resolve(); } };
        watchers.add(watcher);
      });
    },
  };
}

/** Release every open of `fifo` that is blocked waiting for a partner (either side); never blocks itself. */
function releaseFifoOpens(fifo: string): void {
  const open = (flags: number): number | undefined => {
    try { return fs.openSync(fifo, flags); } catch { return undefined; } // ENXIO (no reader) or ENOENT: nothing is waiting
  };
  // A non-blocking read open always succeeds and wakes a writer blocked in open. Keeping it open while the write-side
  // open runs lets that open succeed too, which wakes a reader blocked in open; both ends close afterwards.
  const reader = open(fs.constants.O_RDONLY | fs.constants.O_NONBLOCK);
  const writer = open(fs.constants.O_WRONLY | fs.constants.O_NONBLOCK);
  for (const fd of [writer, reader]) if (fd !== undefined) fs.closeSync(fd);
}

export interface FifoDir { dir: string; path(name: string): string; pid?: string }

/**
 * A temp directory of named FIFOs. Teardown (one t.after, so the order is fixed): SIGKILL the helper whose pid is in the
 * pid file, release every open still blocked on a FIFO (a FIFO open with no partner otherwise outlives the test and keeps
 * the test process alive), then remove the directory. Deliberately not makeTmp: that registers its own rmSync first.
 */
export function fifoDir(t: TestContext, prefix: string, fifos: string[], opts: { pidFile?: boolean } = {}): FifoDir {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), prefix)));
  const at = (name: string): string => path.join(dir, name);
  for (const name of fifos) execFileSync("mkfifo", [at(name)]);
  const pid = opts.pidFile ? at("pid") : undefined;
  t.after(() => {
    if (pid !== undefined) {
      try {
        const helper = Number(fs.readFileSync(pid, "utf8").trim());
        if (Number.isInteger(helper) && helper > 1 && pidAlive(helper)) process.kill(helper, "SIGKILL");
      } catch { /* never armed, or already gone */ }
    }
    for (const name of fifos) releaseFifoOpens(at(name));
    fs.rmSync(dir, { recursive: true, force: true });
  });
  return { dir, path: at, ...(pid === undefined ? {} : { pid }) };
}

export const agentRoot =path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const supportUrl = new URL(import.meta.url).href;
const lockUrl = new URL("../src/authority-lock.ts", import.meta.url).href;

export interface Holder {
  child: ChildProcess;
  /** Resolves with the first stdout line (READY or REFUSED:...). */
  first: Promise<string>;
  exited: Promise<number | null>;
}

/** Spawn a real second node process that tries to take W on `dataDir`, then idles. */
export function spawnWorkerLockHolder(dataDir: string): Holder {
  const code = `(async () => {
    const lock = await import(process.argv[1]);
    const support = await import(process.argv[2]);
    try {
      await lock.acquireWorkerLifetime(process.argv[3], support.testLockOptions());
      process.stdout.write("READY\\n");
      process.stdin.resume();
    } catch (error) {
      process.stdout.write("REFUSED:" + error.message + "\\n");
      process.exit(3);
    }
  })();`;
  const child = spawn(process.execPath, ["--import", "tsx", "-e", code, lockUrl, supportUrl, dataDir], {
    cwd: agentRoot, stdio: ["pipe", "pipe", "inherit"],
  });
  let buffer = "";
  const first = new Promise<string>((resolve, reject) => {
    child.stdout!.on("data", (chunk: Buffer) => {
      buffer += chunk.toString("utf8");
      const newline = buffer.indexOf("\n");
      if (newline >= 0) resolve(buffer.slice(0, newline));
    });
    child.once("error", reject);
    child.once("close", () => reject(new Error("holder exited before reporting")));
  });
  first.catch(() => undefined);
  const exited = new Promise<number | null>((resolve) => child.once("close", (c) => resolve(c)));
  return { child, first, exited };
}
