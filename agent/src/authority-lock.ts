import { spawn } from "node:child_process";
import { AsyncLocalStorage } from "node:async_hooks";
import { constants as fsc } from "node:fs";
import * as fsp from "node:fs/promises";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { setTimeout as delay } from "node:timers/promises";

// Completion-authority kernel locks (issue #3404 M1a). Worker-private; nothing outside
// this unit and its tests may reach the descriptors.
//
//   W  `<dataDir>/.uzi-authority/worker.lock`     process-lifetime EXCLUSIVE, nonblocking.
//   A  `<dataDir>/.uzi-authority/authority.lock`  per-transaction EXCLUSIVE, bounded wait.
//
// Both are permanent validated inodes: never unlinked or replaced. A is a fresh OFD per
// outer acquisition (a kernel lock follows the open file description), serialized
// in-process by a keyed async mutex. The pod-epoch shared lock is unrelated to W.
// st_dev/st_ino are compared in memory within one live acquisition and never persisted.

export class AuthorityRefusal extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AuthorityRefusal";
  }
}

export interface FlockResult { code: number; output: string }

/** Narrow injection points. Production callers pass nothing. */
export interface AuthorityLockOptions {
  flockPath?: string;
  uid?: number;
  /** Replaces the statfs ext4/XFS check for the given directory. */
  supportedFs?: (dir: string) => Promise<boolean>;
  /** Replaces spawning the flock helper (tests only). */
  run?: (args: string[], fd: number | undefined, signal: AbortSignal | undefined, timeoutMs: number) => Promise<FlockResult>;
}

const FLOCK = "/usr/bin/flock";
const EXT4_MAGIC = 0xef53;
const XFS_MAGIC = 0x58465342;
const W_NAME = "worker.lock";
const A_NAME = "authority.lock";
const AUTHORITY_DIR = ".uzi-authority";
const HELPER_TIMEOUT_MS = 10_000;
const DEFAULT_A_TIMEOUT_MS = 30_000;
const POLL_MS = 50;

interface Context {
  readonly dataDir: string;
  readonly authorityDir: string;
  readonly uid: number;
  readonly flockPath: string;
  readonly run: NonNullable<AuthorityLockOptions["run"]>;
  readonly supportedFs: (dir: string) => Promise<boolean>;
  verified: boolean;
  /** The held W descriptor and its name, set once W is acquired (memory only). */
  wHandle?: fsp.FileHandle;
  wFile?: string;
}

async function defaultSupportedFs(dir: string): Promise<boolean> {
  const type = Number((await fsp.statfs(dir)).type);
  return type === EXT4_MAGIC || type === XFS_MAGIC;
}

function runFlock(flockPath: string, args: string[], fd: number | undefined,
  signal: AbortSignal | undefined, timeoutMs: number): Promise<FlockResult> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(signal.reason ?? new Error("aborted")); return; }
    let child;
    try {
      child = spawn(flockPath, args, {
        stdio: ["ignore", "pipe", "pipe", ...(fd === undefined ? [] : [fd])],
        env: { PATH: "/usr/bin:/bin", LC_ALL: "C" },
      });
    } catch (error) { reject(error); return; }
    let output = "";
    let failure: unknown;
    let settled = false;
    const finish = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      fn();
    };
    // Abort/timeout kills only this finite helper and joins its close event before the
    // caller may close the descriptor it was handed.
    const stop = (error: unknown): void => { failure ??= error; child.kill("SIGKILL"); };
    const timer = setTimeout(() => stop(new Error("flock helper timeout")), Math.max(1, timeoutMs));
    const onAbort = (): void => stop(signal?.reason ?? new Error("aborted"));
    signal?.addEventListener("abort", onAbort, { once: true });
    const collect = (chunk: Buffer): void => {
      if (output.length + chunk.length > 4096) stop(new Error("flock output limit"));
      else output += chunk.toString("utf8");
    };
    child.stdout?.on("data", collect);
    child.stderr?.on("data", collect);
    child.once("error", (error) => {
      failure ??= error;
      // A spawn failure (ENOENT/EACCES) may never produce a close event.
      if (child.pid === undefined) finish(() => reject(failure));
    });
    child.once("close", (code, sig) => {
      finish(() => {
        if (failure) reject(failure);
        else if (sig || code === null) reject(new Error("flock helper terminated"));
        else resolve({ code, output });
      });
    });
  });
}

function makeContext(dataDir: string, options: AuthorityLockOptions): Context {
  if (!path.isAbsolute(dataDir) || path.resolve(dataDir) !== dataDir) {
    throw new AuthorityRefusal("authority data root must be a normalized absolute path");
  }
  const flockPath = options.flockPath ?? FLOCK;
  return {
    dataDir,
    authorityDir: path.join(dataDir, AUTHORITY_DIR),
    uid: options.uid ?? process.getuid?.() ?? -1,
    flockPath,
    run: options.run ?? ((args, fd, signal, timeoutMs) => runFlock(flockPath, args, fd, signal, timeoutMs)),
    supportedFs: options.supportedFs ?? defaultSupportedFs,
    verified: false,
  };
}

/** Refuse unless `dir` is on a supported (ext4/XFS) filesystem. */
export async function requireSupportedStorage(
  supportedFs: (dir: string) => Promise<boolean>, dir: string,
): Promise<void> {
  let ok: boolean;
  try { ok = await supportedFs(dir); }
  catch (error) { throw new AuthorityRefusal(`authority storage type unreadable: ${errorText(error)}`); }
  if (!ok) throw new AuthorityRefusal("authority storage requires a local ext4 or XFS filesystem");
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

async function syncDirectory(dir: string): Promise<void> {
  const handle = await fsp.open(dir, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
  try { await handle.sync(); } finally { await handle.close(); }
}

async function ensureAuthorityDir(ctx: Context): Promise<void> {
  const root = await fsp.lstat(ctx.dataDir);
  if (!root.isDirectory() || root.isSymbolicLink()) throw new AuthorityRefusal("authority data root is not a real directory");
  let created = false;
  try { await fsp.mkdir(ctx.authorityDir, { mode: 0o700 }); created = true; }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error; }
  const stat = await fsp.lstat(ctx.authorityDir);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== ctx.uid || (stat.mode & 0o7777) !== 0o700) {
    throw new AuthorityRefusal("unsafe authority directory");
  }
  if (created) await syncDirectory(ctx.dataDir);
}

/** Validate that the held descriptor and the named inode are one private regular file. */
async function verifyLockFile(ctx: Context, handle: fsp.FileHandle, file: string): Promise<void> {
  const held = await handle.stat({ bigint: true });
  const named = await fsp.lstat(file, { bigint: true });
  for (const stat of [held, named]) {
    if (!stat.isFile() || stat.uid !== BigInt(ctx.uid) || (stat.mode & 0o7777n) !== 0o600n || stat.nlink !== 1n) {
      throw new AuthorityRefusal("unsafe authority lock metadata");
    }
  }
  if (held.dev !== named.dev || held.ino !== named.ino) throw new AuthorityRefusal("authority lock inode changed");
}

async function openLockFile(ctx: Context, name: string): Promise<{ handle: fsp.FileHandle; file: string }> {
  await ensureAuthorityDir(ctx);
  const file = path.join(ctx.authorityDir, name);
  const base = fsc.O_RDWR | fsc.O_NOFOLLOW | fsc.O_NONBLOCK;
  let handle: fsp.FileHandle;
  let created = false;
  try {
    handle = await fsp.open(file, base | fsc.O_CREAT | fsc.O_EXCL, 0o600);
    created = true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    handle = await fsp.open(file, base);
  }
  try {
    if (created) {
      await handle.chmod(0o600);
      await handle.sync();
      await syncDirectory(ctx.authorityDir);
    }
    await verifyLockFile(ctx, handle, file);
    return { handle, file };
  } catch (error) {
    await handle.close().catch(() => undefined);
    throw error;
  }
}

async function requireTrustedFlock(ctx: Context, signal: AbortSignal | undefined): Promise<void> {
  if (ctx.verified) return;
  let result: FlockResult;
  try { result = await ctx.run(["--version"], undefined, signal, HELPER_TIMEOUT_MS); }
  catch (error) { throw new AuthorityRefusal(`trusted util-linux flock required: ${errorText(error)}`); }
  if (result.code !== 0 || !/^flock from util-linux [^\r\n]+\n?$/.test(result.output)) {
    throw new AuthorityRefusal("trusted util-linux flock required");
  }
  ctx.verified = true;
}

/**
 * Acquire an exclusive lock on the inherited descriptor. `once` makes one nonblocking
 * attempt (false on contention). Otherwise contention alone retries until the deadline,
 * at most ceil(timeout/POLL)+1 attempts; every other failure aborts.
 */
async function flockExclusive(ctx: Context, handle: fsp.FileHandle, timeoutMs: number,
  signal: AbortSignal | undefined, once: boolean): Promise<boolean> {
  const deadline = performance.now() + timeoutMs;
  await requireTrustedFlock(ctx, signal);
  for (let attempts = 0; attempts <= Math.ceil(timeoutMs / POLL_MS); attempts++) {
    signal?.throwIfAborted();
    if (performance.now() >= deadline) break;
    const remaining = Math.min(HELPER_TIMEOUT_MS, deadline - performance.now());
    const result = await ctx.run(["-x", "-n", "-E", "200", "3"], handle.fd, signal, remaining);
    if (result.code === 0) return true;
    if (result.code !== 200) throw new AuthorityRefusal(`flock failed: ${result.code}`);
    if (once) return false;
    const wait = Math.min(POLL_MS, deadline - performance.now());
    if (wait <= 0) break;
    await delay(wait, undefined, { signal });
  }
  throw new AuthorityRefusal("authority lock timeout");
}

// ---- W: process-lifetime ownership ----------------------------------------------------

const livenessBrand = Symbol("worker-lifetime-token");
const lifetimeContexts = new WeakMap<WorkerLifetime, Context>();
// Strong references keep the descriptors (and so the kernel locks) alive until exit.
const retainedHandles = new Set<fsp.FileHandle>();

/** Opaque proof that this process holds W. Only acquireWorkerLifetime can mint one. */
export class WorkerLifetime {
  readonly #dataDir: string;
  constructor(token: symbol, dataDir: string) {
    if (token !== livenessBrand) throw new TypeError("WorkerLifetime cannot be constructed");
    this.#dataDir = dataDir;
  }
  get dataDir(): string { return this.#dataDir; }
}

/** Acquire W or refuse startup. The returned capability has no release. */
export async function acquireWorkerLifetime(
  dataDir: string,
  options: AuthorityLockOptions & { signal?: AbortSignal; timeoutMs?: number } = {},
): Promise<WorkerLifetime> {
  const ctx = makeContext(dataDir, options);
  await requireSupportedStorage(ctx.supportedFs, ctx.dataDir);
  const { handle, file } = await openLockFile(ctx, W_NAME);
  try {
    let got: boolean;
    try { got = await flockExclusive(ctx, handle, options.timeoutMs ?? HELPER_TIMEOUT_MS, options.signal, true); }
    catch (error) {
      if (error instanceof AuthorityRefusal) throw error;
      throw new AuthorityRefusal(`worker lifetime lock failed: ${errorText(error)}`);
    }
    if (!got) throw new AuthorityRefusal("another compatible worker already holds the worker lifetime lock");
    // The helper has exited (awaited above); this OFD alone now holds the lock. Recheck.
    await verifyLockFile(ctx, handle, file);
  } catch (error) {
    await handle.close().catch(() => undefined);
    throw error;
  }
  retainedHandles.add(handle);
  ctx.wHandle = handle;
  ctx.wFile = file;
  const capability = new WorkerLifetime(livenessBrand, dataDir);
  lifetimeContexts.set(capability, ctx);
  return capability;
}

/**
 * W exclusivity holds only while the named worker.lock is still the inode this process
 * locked (a successor opening the name would otherwise lock a different inode). Compare the
 * held descriptor with the name at each outer A acquisition; memory only, never persisted.
 */
async function verifyWorkerLock(ctx: Context): Promise<void> {
  if (!ctx.wHandle || !ctx.wFile) throw new AuthorityRefusal("live worker lifetime capability required");
  try { await verifyLockFile(ctx, ctx.wHandle, ctx.wFile); }
  catch (error) {
    if (error instanceof AuthorityRefusal) throw error;
    throw new AuthorityRefusal(`worker lifetime lock unreadable: ${errorText(error)}`);
  }
}

function lifetimeContext(lifetime: unknown): Context {
  const ctx = lifetime instanceof WorkerLifetime ? lifetimeContexts.get(lifetime) : undefined;
  if (!ctx) throw new AuthorityRefusal("live worker lifetime capability required");
  return ctx;
}

// ---- A: authority transaction ---------------------------------------------------------

interface TransactionState { readonly ctx: Context; active: boolean }
const transactionStates = new WeakMap<AuthorityTransaction, TransactionState>();
const transactionBrand = Symbol("authority-transaction-token");
const tails = new Map<string, Promise<void>>();
const current = new AsyncLocalStorage<AuthorityTransaction>();

/** Opaque active-transaction token. Valid only inside its withAuthorityTransaction callback. */
export class AuthorityTransaction {
  constructor(token: symbol) {
    if (token !== transactionBrand) throw new TypeError("AuthorityTransaction cannot be constructed");
  }
}

export interface AuthorityTransactionContext {
  readonly dataDir: string;
  readonly authorityDir: string;
  readonly uid: number;
  readonly supportedFs: (dir: string) => Promise<boolean>;
}

/** Context of an ACTIVE transaction; throws once its callback has settled. */
export function transactionContext(tx: unknown): AuthorityTransactionContext {
  const state = tx instanceof AuthorityTransaction ? transactionStates.get(tx) : undefined;
  if (!state?.active) throw new AuthorityRefusal("active authority transaction required");
  const { dataDir, authorityDir, uid, supportedFs } = state.ctx;
  return { dataDir, authorityDir, uid, supportedFs };
}

/**
 * Run `fn` holding A. Pass an existing transaction instead of a lifetime to BORROW it
 * (no reacquisition). Calling with a lifetime from inside a callback that already holds
 * A in this async context is refused rather than left to deadlock. A is released only
 * after the callback's promise settles.
 */
export async function withAuthorityTransaction<T>(
  from: WorkerLifetime | AuthorityTransaction,
  options: { signal?: AbortSignal; timeoutMs?: number },
  fn: (tx: AuthorityTransaction) => Promise<T>,
): Promise<T> {
  if (from instanceof AuthorityTransaction) {
    transactionContext(from);
    return fn(from);
  }
  const ctx = lifetimeContext(from);
  const held = current.getStore();
  if (held && transactionStates.get(held)?.active && transactionStates.get(held)?.ctx === ctx) {
    throw new AuthorityRefusal("nested authority transaction must borrow the active transaction");
  }
  const { signal } = options;
  // One deadline covers the in-process queue wait and the cross-process flock wait.
  const timeoutMs = options.timeoutMs ?? DEFAULT_A_TIMEOUT_MS;
  const deadline = performance.now() + timeoutMs;
  const key = ctx.authorityDir;
  const previous = tails.get(key) ?? Promise.resolve();
  let release!: () => void;
  const mine = new Promise<void>((resolve) => { release = resolve; });
  const tail = previous.then(() => mine);
  tails.set(key, tail);
  const done = (): void => { release(); if (tails.get(key) === tail) tails.delete(key); };
  try {
    await waitFor(previous, signal, timeoutMs);
  } catch (error) {
    // Free our queue slot only after the predecessor settles so ordering holds.
    void previous.then(done);
    throw error;
  }
  try {
    await verifyWorkerLock(ctx);
    const { handle, file } = await openLockFile(ctx, A_NAME);
    try {
      await requireSupportedStorage(ctx.supportedFs, ctx.authorityDir);
      await flockExclusive(ctx, handle, Math.max(0, deadline - performance.now()), signal, false);
      await verifyLockFile(ctx, handle, file);
      const tx = new AuthorityTransaction(transactionBrand);
      const state: TransactionState = { ctx, active: true };
      transactionStates.set(tx, state);
      try { return await current.run(tx, () => fn(tx)); }
      finally { state.active = false; }
    } finally {
      await handle.close();
    }
  } finally {
    done();
  }
}

/** Wait for the in-process predecessor, bounded by the caller's deadline and signal. */
function waitFor(promise: Promise<void>, signal: AbortSignal | undefined, timeoutMs: number): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(signal.reason ?? new Error("aborted")); return; }
    const cleanup = (): void => { clearTimeout(timer); signal?.removeEventListener("abort", onAbort); };
    const onAbort = (): void => { cleanup(); reject(signal?.reason ?? new Error("aborted")); };
    const timer = setTimeout(() => { cleanup(); reject(new AuthorityRefusal("authority lock timeout")); }, Math.max(1, timeoutMs));
    signal?.addEventListener("abort", onAbort, { once: true });
    void promise.then(() => { cleanup(); resolve(); });
  });
}
