import { createHmac, randomUUID, timingSafeEqual } from "node:crypto";
import { constants as fsc } from "node:fs";
import * as fsp from "node:fs/promises";
import path from "node:path";
import {
  AuthorityRefusal, requireSupportedStorage, transactionContext, type AuthorityTransaction, type WorkerLifetime,
} from "./authority-lock.js";

// Completion-authority enrollment and durable source-use slots (issue #3404 M1a).
// DORMANT: nothing in production calls the slot API yet; it changes no release
// eligibility. Every operation needs an active authority transaction (A) from
// authority-lock.ts, so a stale or foreign caller cannot write.
//
// Files (all 0600 under the 0700 `<dataDir>/.uzi-authority/`, never persisting st_dev/st_ino):
//   enrollment.json        authenticated storage UUID + layout version + canonical data root
//   heads/<xx>.json       256 MAC'd head shards (xx = first two hex digits of the slot key):
//                         latest slot seq per slot key in that shard (freshness)
//   slots/<digest>.json   one MAC'd record per source path
// Writes are tmp(O_EXCL|O_NOFOLLOW) -> file fsync -> rename -> directory fsync -> read back.
// Each write holds an O_DIRECTORY|O_NOFOLLOW handle on the target directory and refuses when
// the directory name no longer resolves to that inode, checked before the tmp file is
// created, before the rename and after it (memory only). A pre-rename refusal unlinks the
// synced tmp file through the held directory handle (/proc/self/fd), so it is not leaked in a
// directory that was moved away.
//
// Freshness: a MAC binds the key, not recency, so an authentic OLD slot file could be put back
// to restore a cleared state. Every slot write is therefore followed by a write of that key's
// head shard (the same five-step durable write), so each slot write costs two extra barriers (a
// file fsync and a directory fsync) on top of the slot's own two. The shard name is bound into
// the shard's MAC and every key in it must carry that prefix, so shards cannot be swapped. A
// loaded slot must satisfy slot.seq == shard seq; slot.seq == shard seq + 1 is the crash window
// between the slot rename and the shard update, and is completed under the transaction.
// Anything older refuses and blocks. Retired tombstones are kept (no GC), so the head is sharded
// to keep each file far below MAX_SLOT_BYTES and to rewrite only one shard per operation. A
// shard file that is deleted reads as seq 0, so it is indistinguishable from the crash window
// only for a slot at seq 1; a deleted shard under any later slot refuses.
//
// Rollback: the in-process high-water mark (module level, keyed by storage id, so reopening the
// store per transaction keeps it) catches any restore within this process. Across a restart,
// the one undetected rollback is restoring a head shard together with every slot written since
// it (for example a volume snapshot restore, up to and including the whole store); detection
// of that case is in-process only. An older shard with a newer slot (more than one ahead) or
// an older slot under a newer shard is detected.

const AUTHORITY_LAYOUT = "uzi-authority-layout/1";
const ENROLLMENT_VERSION = "uzi-authority-enrollment/1";
const SLOT_VERSION = "uzi-authority-slot/1";
const HEAD_VERSION = "uzi-authority-head/2";
const HEADS_DIR = "heads";
const SHARD = /^[0-9a-f]{2}$/;
const SLOT_FILE = /^[0-9a-f]{64}\.json$/;
const KEY_LABEL = "uzi completion authority v1";
const ENROLLMENT_FILE = "enrollment.json";
const SLOTS_DIR = "slots";
const AUTHORITY_DIR_NAME = ".uzi-authority";
const MAX_ENROLLMENT_BYTES = 4096;
const MAX_SLOT_BYTES = 1 << 20;
const MAX_FOOTPRINT = 1024;
const MAX_ITEM = 512;
const MAX_ITEMS = 1024;
const HEX64 = /^[0-9a-f]{64}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** Random per process; a successor worker never shares it. */
const PROCESS_INCARNATION = randomUUID();

export interface AuthorityFsOps {
  syncFile(handle: fsp.FileHandle): Promise<void>;
  syncDir(handle: fsp.FileHandle): Promise<void>;
  rename(from: string, to: string): Promise<void>;
}
const realOps: AuthorityFsOps = {
  syncFile: (handle) => handle.sync(),
  syncDir: (handle) => handle.sync(),
  rename: (from, to) => fsp.rename(from, to),
};

export type SlotState = "active" | "claude_ready" | "sealed" | "ineligible" | "retiring" | "retired";
const STATES: readonly SlotState[] = ["active", "claude_ready", "sealed", "ineligible", "retiring", "retired"];

export interface PendingFootprint {
  operation: string;
  kind: "bare_mutation" | "destructive";
  footprint: string;
  status: "running" | "settled_failed";
  residue: string[];
}
export interface RetiringRecord {
  prior: string;
  operation: string;
  manifest: string[];
  deleted: number;
}
export interface SlotSnapshot {
  version: string;
  key: string;
  state: SlotState;
  epoch: string;
  seq: number;
  owner: { worker: string; executor: string | null };
  pending: PendingFootprint | null;
  retiring: RetiringRecord | null;
}

// ---- strict decoding ------------------------------------------------------------------

function isPlain(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    && Object.getPrototypeOf(value) === Object.prototype;
}
function exact(value: unknown, keys: string[]): Record<string, unknown> {
  if (!isPlain(value) || Object.keys(value).sort().join(",") !== [...keys].sort().join(",")) {
    throw new AuthorityRefusal("malformed authority record");
  }
  return value;
}
function text(value: unknown, max: number, min = 1): string {
  if (typeof value !== "string" || value.length < min || value.length > max) throw new AuthorityRefusal("malformed authority record");
  return value;
}
function uuid(value: unknown): string {
  if (typeof value !== "string" || !UUID.test(value)) throw new AuthorityRefusal("malformed authority record");
  return value;
}
function hex64(value: unknown): string {
  if (typeof value !== "string" || !HEX64.test(value)) throw new AuthorityRefusal("malformed authority record");
  return value;
}
function uint(value: unknown, min: number, max: number): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < min || value > max) {
    throw new AuthorityRefusal("malformed authority record");
  }
  return value;
}
function strings(value: unknown, maxItems: number): string[] {
  if (!Array.isArray(value) || value.length > maxItems) throw new AuthorityRefusal("malformed authority record");
  return value.map((item) => text(item, MAX_ITEM));
}

function normalizeSlot(raw: unknown): SlotSnapshot {
  const o = exact(raw, ["version", "key", "state", "epoch", "seq", "owner", "pending", "retiring"]);
  if (o.version !== SLOT_VERSION) throw new AuthorityRefusal("unsupported authority slot version");
  const state = o.state;
  if (typeof state !== "string" || !STATES.includes(state as SlotState)) throw new AuthorityRefusal("malformed authority record");
  const owner = exact(o.owner, ["worker", "executor"]);
  let pending: PendingFootprint | null = null;
  if (o.pending !== null) {
    const p = exact(o.pending, ["operation", "kind", "footprint", "status", "residue"]);
    if (p.kind !== "bare_mutation" && p.kind !== "destructive") throw new AuthorityRefusal("malformed authority record");
    if (p.status !== "running" && p.status !== "settled_failed") throw new AuthorityRefusal("malformed authority record");
    pending = {
      operation: uuid(p.operation), kind: p.kind, footprint: text(p.footprint, MAX_FOOTPRINT),
      status: p.status, residue: strings(p.residue, 64),
    };
  }
  let retiring: RetiringRecord | null = null;
  if (o.retiring !== null) {
    const r = exact(o.retiring, ["prior", "operation", "manifest", "deleted"]);
    const manifest = strings(r.manifest, MAX_ITEMS);
    retiring = { prior: hex64(r.prior), operation: uuid(r.operation), manifest, deleted: uint(r.deleted, 0, manifest.length) };
  }
  if ((state === "retiring") !== (retiring !== null)) throw new AuthorityRefusal("malformed authority record");
  return {
    version: SLOT_VERSION, key: hex64(o.key), state: state as SlotState, epoch: uuid(o.epoch),
    seq: uint(o.seq, 1, Number.MAX_SAFE_INTEGER),
    owner: { worker: uuid(owner.worker), executor: owner.executor === null ? null : text(owner.executor, 128) },
    pending, retiring,
  };
}

interface Enrollment { version: string; layout: string; storageId: string; dataDir: string }
function normalizeEnrollment(raw: unknown): Enrollment {
  const o = exact(raw, ["version", "layout", "storageId", "dataDir"]);
  if (o.version !== ENROLLMENT_VERSION) throw new AuthorityRefusal("unsupported authority enrollment version");
  return { version: ENROLLMENT_VERSION, layout: text(o.layout, 128), storageId: uuid(o.storageId), dataDir: text(o.dataDir, 4096) };
}

interface Head { version: string; storageId: string; shard: string; slots: Record<string, number> }
function normalizeHead(raw: unknown): Head {
  const o = exact(raw, ["version", "storageId", "shard", "slots"]);
  if (o.version !== HEAD_VERSION) throw new AuthorityRefusal("unsupported authority head version");
  if (typeof o.shard !== "string" || !SHARD.test(o.shard)) throw new AuthorityRefusal("malformed authority record");
  const shard = o.shard;
  if (!isPlain(o.slots)) throw new AuthorityRefusal("malformed authority record");
  const entries = Object.entries(o.slots)
    .map(([key, seq]) => [hex64(key), uint(seq, 1, Number.MAX_SAFE_INTEGER)] as const)
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  if (entries.some(([key]) => !key.startsWith(shard))) throw new AuthorityRefusal("malformed authority record");
  return { version: HEAD_VERSION, storageId: uuid(o.storageId), shard, slots: Object.fromEntries(entries) };
}

// ---- authenticated records ------------------------------------------------------------

function macOf(key: Buffer, kind: string, bodyJson: string): string {
  return createHmac("sha256", key).update(`uzi-authority-record\0${kind}\0${bodyJson}`).digest("hex");
}
function seal(key: Buffer, kind: string, body: object): string {
  return JSON.stringify({ ...body, mac: macOf(key, kind, JSON.stringify(body)) }) + "\n";
}
function unseal<T extends object>(key: Buffer, kind: string, source: string, normalize: (raw: unknown) => T): T {
  let parsed: unknown;
  try { parsed = JSON.parse(source); } catch { throw new AuthorityRefusal("malformed authority record"); }
  if (!isPlain(parsed) || typeof parsed.mac !== "string" || !HEX64.test(parsed.mac)) {
    throw new AuthorityRefusal("authority record is not authenticated");
  }
  const { mac, ...rest } = parsed;
  const body = normalize(rest);
  const expected = macOf(key, kind, JSON.stringify(body));
  if (!timingSafeEqual(Buffer.from(expected, "hex"), Buffer.from(mac as string, "hex"))) {
    throw new AuthorityRefusal("authority record failed authentication");
  }
  // Duplicate keys, reordering and whitespace tricks must not decode to the same state.
  if (source !== seal(key, kind, body)) throw new AuthorityRefusal("non-canonical authority record");
  return body;
}

// ---- file IO (async only) -------------------------------------------------------------

async function readRegular(file: string, uid: number, limit: number): Promise<string | null> {
  let handle: fsp.FileHandle;
  try { handle = await fsp.open(file, fsc.O_RDONLY | fsc.O_NOFOLLOW | fsc.O_NONBLOCK); }
  catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT") return null;
    throw new AuthorityRefusal(`unreadable authority file: ${code ?? "error"}`);
  }
  try {
    const stat = await handle.stat({ bigint: true });
    if (!stat.isFile() || stat.uid !== BigInt(uid) || (stat.mode & 0o7777n) !== 0o600n || stat.nlink !== 1n) {
      throw new AuthorityRefusal("unsafe authority file metadata");
    }
    if (stat.size > BigInt(limit)) throw new AuthorityRefusal("authority file exceeds bound");
    const bytes = Buffer.alloc(Number(stat.size));
    let length = 0;
    while (length < bytes.length) {
      const { bytesRead } = await handle.read(bytes, length, bytes.length - length, length);
      if (bytesRead === 0) break;
      length += bytesRead;
    }
    const after = await handle.stat({ bigint: true });
    if (length !== bytes.length || after.size !== stat.size) throw new AuthorityRefusal("authority file changed while read");
    const decoded = bytes.toString("utf8");
    if (!Buffer.from(decoded, "utf8").equals(bytes)) throw new AuthorityRefusal("non-UTF-8 authority file");
    return decoded;
  } finally {
    await handle.close();
  }
}

/**
 * Hold a handle on `dir` and expose `check`, which refuses unless the name still resolves to
 * the directory this handle was opened on. Memory only: st_dev/st_ino are never persisted.
 */
async function withDirGuard<T>(
  dir: string, uid: number, fn: (handle: fsp.FileHandle, check: () => Promise<void>) => Promise<T>,
): Promise<T> {
  const handle = await fsp.open(dir, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
  try {
    const held = await handle.stat({ bigint: true });
    if (!held.isDirectory() || held.uid !== BigInt(uid) || (held.mode & 0o7777n) !== 0o700n) {
      throw new AuthorityRefusal("unsafe authority directory");
    }
    const check = async (): Promise<void> => {
      let named;
      try { named = await fsp.lstat(dir, { bigint: true }); }
      catch { throw new AuthorityRefusal("authority directory identity changed"); }
      if (!named.isDirectory() || named.dev !== held.dev || named.ino !== held.ino) {
        throw new AuthorityRefusal("authority directory identity changed");
      }
    };
    await check();
    return await fn(handle, check);
  } finally {
    await handle.close();
  }
}

async function writeDurable(ops: AuthorityFsOps, dir: string, uid: number, name: string, content: string): Promise<void> {
  await withDirGuard(dir, uid, async (dirHandle, check) => {
    const tmp = path.join(dir, `.${name}.${randomUUID()}.tmp`);
    try {
      // withDirGuard verified the directory identity just before this point, so before the tmp file exists.
      const handle = await fsp.open(tmp, fsc.O_WRONLY | fsc.O_CREAT | fsc.O_EXCL | fsc.O_NOFOLLOW, 0o600);
      try {
        await handle.chmod(0o600);
        await handle.writeFile(content, "utf8");
        await ops.syncFile(handle);
      } finally { await handle.close(); }
      await check(); // before anything is renamed into the store
      await ops.rename(tmp, path.join(dir, name));
    } catch (error) {
      // Unlink through the held handle: after a directory swap the name resolves elsewhere.
      await fsp.unlink(path.join(`/proc/self/fd/${dirHandle.fd}`, path.basename(tmp)))
        .catch(() => fsp.unlink(tmp)).catch(() => undefined);
      throw error;
    }
    await check();
    await ops.syncDir(dirHandle);
    await check();
  });
}

const shardOf = (slotKey: string): string => slotKey.slice(0, 2);

function sealHead(init: StoreIdentity, shard: string, heads: ReadonlyMap<string, number>): string {
  const slots = Object.fromEntries([...heads].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
  const content = seal(init.key, "head", { version: HEAD_VERSION, storageId: init.storageId, shard, slots });
  if (Buffer.byteLength(content, "utf8") > init.maxHeadBytes) throw new AuthorityRefusal("authority head exceeds bound");
  return content;
}

/** The authenticated head shard (empty when its file does not exist yet). */
async function readHead(init: StoreIdentity, shard: string): Promise<Map<string, number>> {
  try { await fsp.lstat(path.join(init.dir, HEADS_DIR)); }
  catch { throw new AuthorityRefusal("authority head missing"); }
  const source = await readRegular(path.join(init.dir, HEADS_DIR, `${shard}.json`), init.uid, init.maxHeadBytes);
  if (source === null) return new Map();
  const head = unseal(init.key, "head", source, normalizeHead);
  if (head.storageId !== init.storageId) throw new AuthorityRefusal("authority head belongs to another storage");
  if (head.shard !== shard) throw new AuthorityRefusal("authority head shard does not match its name");
  return new Map(Object.entries(head.slots));
}

async function writeHead(
  ops: AuthorityFsOps, init: StoreIdentity, shard: string, heads: ReadonlyMap<string, number>,
): Promise<void> {
  const content = sealHead(init, shard, heads);
  const dir = path.join(init.dir, HEADS_DIR);
  await writeDurable(ops, dir, init.uid, `${shard}.json`, content);
  if (await readRegular(path.join(dir, `${shard}.json`), init.uid, init.maxHeadBytes) !== content) {
    throw new AuthorityRefusal("authority head did not read back");
  }
}

interface StoreIdentity { dir: string; uid: number; key: Buffer; storageId: string; maxHeadBytes: number }

/** Highest seq loaded or written per slot key, per storage id, for the life of the process. */
const highWaters = new Map<string, Map<string, number>>();
function highWaterOf(storageId: string): Map<string, number> {
  let marks = highWaters.get(storageId);
  if (!marks) { marks = new Map(); highWaters.set(storageId, marks); }
  return marks;
}

async function ensurePrivateDir(dir: string, parent: string, uid: number, ops: AuthorityFsOps): Promise<void> {
  let created = false;
  try { await fsp.mkdir(dir, { mode: 0o700 }); created = true; }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error; }
  const stat = await fsp.lstat(dir);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== uid || (stat.mode & 0o7777) !== 0o700) {
    throw new AuthorityRefusal("unsafe authority directory");
  }
  if (created) {
    const handle = await fsp.open(parent, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
    try { await ops.syncDir(handle); } finally { await handle.close(); }
  }
}

// ---- store ----------------------------------------------------------------------------

const storeBrand = Symbol("authority-store-token");
const leaseBrand = Symbol("source-use-lease-token");
const leaseState = new WeakMap<SourceUseLease, { store: AuthorityStore; key: string; epoch: string }>();

/** Opaque exact-owner lease for one source-use epoch. Only admitSourceUse can mint one. */
export class SourceUseLease {
  constructor(token: symbol, store: AuthorityStore, key: string, epoch: string) {
    if (token !== leaseBrand) throw new TypeError("SourceUseLease cannot be constructed");
    leaseState.set(this, { store, key, epoch });
  }
}

export interface AuthorityStoreOptions {
  workerToken: string;
  /** Tests only: simulate another worker process. */
  workerIncarnation?: string;
  /** Tests only: inject fsync/rename failures. */
  ops?: AuthorityFsOps;
  /** Tests only: a smaller per-shard head bound than MAX_SLOT_BYTES. */
  maxHeadBytes?: number;
}

/**
 * Verify (or on first use create) the storage enrollment under A and return the store.
 * An existing store is never re-enrolled: a missing, unauthenticated, other-layout or
 * other-spelling enrollment refuses, as do slot files with no enrollment.
 */
export async function openAuthorityStore(tx: AuthorityTransaction, options: AuthorityStoreOptions): Promise<AuthorityStore> {
  const ctx = transactionContext(tx);
  if (options.workerToken === "") throw new AuthorityRefusal("worker token required to authenticate authority state");
  const ops = options.ops ?? realOps;
  // Domain-separated from the join token itself: only this derived key ever MACs state.
  const key = createHmac("sha256", options.workerToken).update(KEY_LABEL).digest();
  if (path.resolve(ctx.dataDir) !== ctx.dataDir || await fsp.realpath(ctx.dataDir) !== ctx.dataDir) {
    throw new AuthorityRefusal("authority data root is not the canonical spelling");
  }
  await requireSupportedStorage(ctx.supportedFs, ctx.authorityDir);
  const slotsDir = path.join(ctx.authorityDir, SLOTS_DIR);
  const enrollmentFile = path.join(ctx.authorityDir, ENROLLMENT_FILE);
  const existing = await readRegular(enrollmentFile, ctx.uid, MAX_ENROLLMENT_BYTES);
  let enrollment: Enrollment;
  if (existing === null) {
    let entries: string[] = [];
    try { entries = await fsp.readdir(slotsDir); }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    if (entries.length > 0) throw new AuthorityRefusal("authority slots exist without an enrollment");
    enrollment = { version: ENROLLMENT_VERSION, layout: AUTHORITY_LAYOUT, storageId: randomUUID(), dataDir: ctx.dataDir };
    await writeDurable(ops, ctx.authorityDir, ctx.uid, ENROLLMENT_FILE, seal(key, "enrollment", enrollment));
    const written = await readRegular(enrollmentFile, ctx.uid, MAX_ENROLLMENT_BYTES);
    if (written === null) throw new AuthorityRefusal("authority enrollment vanished");
    enrollment = unseal(key, "enrollment", written, normalizeEnrollment);
  } else {
    enrollment = unseal(key, "enrollment", existing, normalizeEnrollment);
  }
  if (enrollment.layout !== AUTHORITY_LAYOUT) throw new AuthorityRefusal("unsupported authority layout");
  if (enrollment.dataDir !== ctx.dataDir) throw new AuthorityRefusal("authority enrolled under a different data root spelling");
  // The head shards make slot freshness checkable. Create their directory only for an empty
  // store: slots with no head directory cannot be told from a deleted one, so they refuse.
  const headsDir = path.join(ctx.authorityDir, HEADS_DIR);
  let headsExist = true;
  try { await fsp.lstat(headsDir); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; headsExist = false; }
  if (!headsExist) {
    let entries: string[] = [];
    try { entries = await fsp.readdir(slotsDir); }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    if (entries.some((name) => SLOT_FILE.test(name))) throw new AuthorityRefusal("authority slots exist without a head");
  }
  await ensurePrivateDir(headsDir, ctx.authorityDir, ctx.uid, ops);
  return new AuthorityStore(storeBrand, {
    key, storageId: enrollment.storageId, ctxDir: ctx.authorityDir, dataDir: ctx.dataDir, uid: ctx.uid,
    incarnation: options.workerIncarnation ?? PROCESS_INCARNATION, ops,
    maxHeadBytes: options.maxHeadBytes ?? MAX_SLOT_BYTES,
  });
}

interface StoreInit {
  key: Buffer; storageId: string; ctxDir: string; dataDir: string; uid: number; incarnation: string; ops: AuthorityFsOps;
  maxHeadBytes: number;
}

const NEXT_STATES: Record<"claude_ready" | "sealed" | "ineligible", SlotState[]> = {
  claude_ready: ["active"],
  sealed: ["claude_ready"],
  ineligible: ["active", "claude_ready", "sealed"],
};

/** A failed footprint's residue may only be discarded by a retire that deletes all of it. */
function requireResidueCovered(pending: PendingFootprint | null, manifest: readonly string[]): void {
  if (pending?.status !== "settled_failed") return;
  const covered = new Set(manifest);
  if (!pending.residue.every((item) => covered.has(item))) {
    throw new AuthorityRefusal("pending residue is not covered by the retire manifest");
  }
}

export class AuthorityStore {
  readonly #init: StoreInit;
  /** Highest seq loaded or written per slot key; shared by every store of this storage id. */
  readonly #highWater: Map<string, number>;
  constructor(token: symbol, init: StoreInit) {
    if (token !== storeBrand) throw new TypeError("AuthorityStore cannot be constructed");
    this.#init = init;
    this.#highWater = highWaterOf(init.storageId);
  }

  get storageId(): string { return this.#init.storageId; }

  #check(tx: AuthorityTransaction): void {
    if (transactionContext(tx).authorityDir !== this.#init.ctxDir) {
      throw new AuthorityRefusal("transaction belongs to a different authority root");
    }
  }

  async #slotsDir(): Promise<string> {
    const dir = path.join(this.#init.ctxDir, SLOTS_DIR);
    await ensurePrivateDir(dir, this.#init.ctxDir, this.#init.uid, this.#init.ops);
    return dir;
  }

  /** Canonical relative layout identity of a source path; refuses anything unsafe. */
  async #relative(sourcePath: string): Promise<string> {
    const root = this.#init.dataDir;
    if (typeof sourcePath !== "string" || sourcePath.includes("\0") || !path.isAbsolute(sourcePath) ||
        path.resolve(sourcePath) !== sourcePath) {
      throw new AuthorityRefusal("source path must be a normalized absolute path");
    }
    const rel = path.relative(root, sourcePath);
    if (rel === "" || rel === ".." || rel.startsWith(`..${path.sep}`) || path.isAbsolute(rel)) {
      throw new AuthorityRefusal("source path is outside the data root");
    }
    const parts = rel.split(path.sep);
    if (parts[0] === AUTHORITY_DIR_NAME) throw new AuthorityRefusal("source path is inside authority storage");
    let current = root;
    for (const [index, part] of parts.entries()) {
      current = path.join(current, part);
      let stat;
      try { stat = await fsp.lstat(current); }
      catch (error) {
        // Only the final component may be absent: nothing below a missing ancestor is checkable.
        if ((error as NodeJS.ErrnoException).code === "ENOENT" && index === parts.length - 1) break;
        throw new AuthorityRefusal("source path is not resolvable");
      }
      if (stat.isSymbolicLink()) throw new AuthorityRefusal("source path crosses a symlink");
      if (index < parts.length - 1 && !stat.isDirectory()) throw new AuthorityRefusal("source path ancestor is not a directory");
    }
    return rel;
  }

  async slotKey(tx: AuthorityTransaction, sourcePath: string): Promise<string> {
    this.#check(tx);
    const rel = await this.#relative(sourcePath);
    return createHmac("sha256", this.#init.key).update(`slot\0${this.#init.storageId}\0${rel}`).digest("hex");
  }

  #identityOf(): StoreIdentity {
    return {
      dir: this.#init.ctxDir, uid: this.#init.uid, key: this.#init.key, storageId: this.#init.storageId,
      maxHeadBytes: this.#init.maxHeadBytes,
    };
  }

  async #load(dir: string, key: string): Promise<SlotSnapshot | null> {
    const head = await readHead(this.#identityOf(), shardOf(key));
    const source = await readRegular(path.join(dir, `${key}.json`), this.#init.uid, MAX_SLOT_BYTES);
    const headSeq = head.get(key) ?? 0;
    const floor = Math.max(headSeq, this.#highWater.get(key) ?? 0);
    if (source === null) {
      if (floor > 0) throw new AuthorityRefusal("authority slot missing");
      return null;
    }
    const body = unseal(this.#init.key, "slot", source, normalizeSlot);
    if (body.key !== key) throw new AuthorityRefusal("authority slot does not match its name");
    if (body.seq < floor) throw new AuthorityRefusal("authority slot is older than the recorded head (replay)");
    if (body.seq > headSeq) {
      // Exactly one ahead is a crash between the slot rename and the head update: finish it.
      if (body.seq !== headSeq + 1) throw new AuthorityRefusal("authority slot is ahead of the recorded head");
      head.set(key, body.seq);
      await writeHead(this.#init.ops, this.#identityOf(), shardOf(key), head);
    }
    this.#highWater.set(key, body.seq);
    return body;
  }

  /**
   * Validate through the decoder and the size bound BEFORE anything is written, so the public
   * API can never persist a slot the next load would refuse. The slot is written first, then
   * the key's head shard (which must stay within its own bound, checked up front).
   */
  async #save(dir: string, input: SlotSnapshot): Promise<void> {
    const body = normalizeSlot(JSON.parse(JSON.stringify(input)));
    const content = seal(this.#init.key, "slot", body);
    if (Buffer.byteLength(content, "utf8") > MAX_SLOT_BYTES) throw new AuthorityRefusal("authority slot exceeds bound");
    const shard = shardOf(body.key);
    const head = await readHead(this.#identityOf(), shard);
    if (body.seq !== (head.get(body.key) ?? 0) + 1) throw new AuthorityRefusal("authority slot is not the next head sequence");
    head.set(body.key, body.seq);
    sealHead(this.#identityOf(), shard, head);
    await writeDurable(this.#init.ops, dir, this.#init.uid, `${body.key}.json`, content);
    const back = await readRegular(path.join(dir, `${body.key}.json`), this.#init.uid, MAX_SLOT_BYTES);
    if (back !== content) throw new AuthorityRefusal("authority slot did not read back");
    this.#highWater.set(body.key, body.seq);
    await writeHead(this.#init.ops, this.#identityOf(), shard, head);
  }

  /** Current verified slot, or null when never admitted. A malformed slot refuses. */
  async readSlot(tx: AuthorityTransaction, key: string): Promise<SlotSnapshot | null> {
    this.#check(tx);
    if (!HEX64.test(key)) throw new AuthorityRefusal("invalid slot key");
    return this.#load(await this.#slotsDir(), key);
  }

  /**
   * Durably invalidate older authority for this source and return a lease for a fresh
   * epoch. Presenting this store's still-current lease re-admits without a write; a lease
   * that is not current (stale epoch, other source, other store, slot no longer active) is
   * REFUSED, never silently replaced by a new epoch: call without a lease to mint one.
   */
  async admitSourceUse(
    tx: AuthorityTransaction, sourcePath: string, options: { executor?: string; lease?: SourceUseLease } = {},
  ): Promise<SourceUseLease> {
    this.#check(tx);
    if (options.executor !== undefined &&
        (typeof options.executor !== "string" || options.executor.length < 1 || options.executor.length > 128)) {
      throw new AuthorityRefusal("invalid executor");
    }
    const key = await this.slotKey(tx, sourcePath);
    const dir = await this.#slotsDir();
    const old = await this.#load(dir, key);
    if (old) {
      if (old.pending !== null) throw new AuthorityRefusal("source has an unresolved pending footprint");
      if (old.state === "retiring") throw new AuthorityRefusal("source is being retired");
      if (old.state === "active" && old.owner.worker !== this.#init.incarnation) {
        throw new AuthorityRefusal("source is active under another worker incarnation");
      }
    }
    if (options.lease) {
      const held = leaseState.get(options.lease);
      if (!held || held.store !== this || held.key !== key) throw new AuthorityRefusal("lease does not belong to this source");
      if (old?.state !== "active" || held.epoch !== old.epoch) throw new AuthorityRefusal("stale source-use lease");
      return options.lease;
    }
    const body: SlotSnapshot = {
      version: SLOT_VERSION, key, state: "active", epoch: randomUUID(), seq: (old?.seq ?? 0) + 1,
      owner: { worker: this.#init.incarnation, executor: options.executor ?? null }, pending: null, retiring: null,
    };
    await this.#save(dir, body);
    return new SourceUseLease(leaseBrand, this, key, body.epoch);
  }

  async #owned(tx: AuthorityTransaction, lease: SourceUseLease): Promise<{ dir: string; body: SlotSnapshot }> {
    this.#check(tx);
    const held = leaseState.get(lease);
    if (!held || held.store !== this) throw new AuthorityRefusal("lease does not belong to this store");
    const dir = await this.#slotsDir();
    const body = await this.#load(dir, held.key);
    if (!body) throw new AuthorityRefusal("authority slot missing");
    if (body.epoch !== held.epoch || body.owner.worker !== this.#init.incarnation) {
      throw new AuthorityRefusal("stale source-use lease");
    }
    return { dir, body };
  }

  async transition(tx: AuthorityTransaction, lease: SourceUseLease, to: "claude_ready" | "sealed" | "ineligible"): Promise<void> {
    const { dir, body } = await this.#owned(tx, lease);
    if (!NEXT_STATES[to].includes(body.state)) throw new AuthorityRefusal(`illegal transition ${body.state} -> ${to}`);
    if (to !== "ineligible" && body.pending !== null) throw new AuthorityRefusal("unresolved pending footprint");
    await this.#save(dir, { ...body, state: to, seq: body.seq + 1 });
  }

  /** Durably record intent BEFORE a bare mutation or destructive operation begins. */
  async beginPending(
    tx: AuthorityTransaction, lease: SourceUseLease, input: { kind: "bare_mutation" | "destructive"; footprint: string },
  ): Promise<string> {
    const { dir, body } = await this.#owned(tx, lease);
    if (body.state === "retiring" || body.state === "retired") throw new AuthorityRefusal("source is retiring");
    if (body.pending !== null) throw new AuthorityRefusal("a pending footprint is already recorded");
    const operation = randomUUID();
    const pending: PendingFootprint = {
      operation, kind: input.kind, footprint: text(input.footprint, MAX_FOOTPRINT), status: "running", residue: [],
    };
    await this.#save(dir, { ...body, pending, seq: body.seq + 1 });
    return operation;
  }

  /** Success clears the footprint. Failure keeps it with its residue and forces ineligible. */
  async settlePending(
    tx: AuthorityTransaction, lease: SourceUseLease, operation: string, outcome: { ok: true } | { ok: false; residue: string[] },
  ): Promise<void> {
    const { dir, body } = await this.#owned(tx, lease);
    if (body.pending?.operation !== operation || body.pending.status !== "running") {
      throw new AuthorityRefusal("no matching running pending footprint");
    }
    if (outcome.ok) {
      await this.#save(dir, { ...body, pending: null, seq: body.seq + 1 });
      return;
    }
    const pending: PendingFootprint = { ...body.pending, status: "settled_failed", residue: strings(outcome.residue, 64) };
    const state: SlotState = body.state === "retiring" ? "retiring" : "ineligible";
    await this.#save(dir, { ...body, state, pending, seq: body.seq + 1 });
  }

  async markRetiring(tx: AuthorityTransaction, lease: SourceUseLease, input: { prior: string; manifest: string[] }): Promise<void> {
    const { dir, body } = await this.#owned(tx, lease);
    if (body.state === "retiring" || body.state === "retired") throw new AuthorityRefusal("source already retiring");
    if (body.pending?.status === "running") throw new AuthorityRefusal("unresolved pending footprint");
    const retiring: RetiringRecord = {
      prior: hex64(input.prior), operation: randomUUID(), manifest: strings(input.manifest, MAX_ITEMS), deleted: 0,
    };
    requireResidueCovered(body.pending, retiring.manifest);
    await this.#save(dir, { ...body, state: "retiring", retiring, seq: body.seq + 1 });
  }

  /** Durable deletion progress; monotonic and bounded by the manifest. */
  async advanceRetiring(tx: AuthorityTransaction, lease: SourceUseLease, deleted: number): Promise<void> {
    const { dir, body } = await this.#owned(tx, lease);
    if (body.state !== "retiring" || !body.retiring) throw new AuthorityRefusal("source is not retiring");
    if (!Number.isSafeInteger(deleted) || deleted < body.retiring.deleted || deleted > body.retiring.manifest.length) {
      throw new AuthorityRefusal("invalid retire progress");
    }
    await this.#save(dir, { ...body, retiring: { ...body.retiring, deleted }, seq: body.seq + 1 });
  }

  /** Tombstone after the whole manifest is deleted; the next admit mints a new epoch. */
  async markRetired(tx: AuthorityTransaction, lease: SourceUseLease): Promise<void> {
    const { dir, body } = await this.#owned(tx, lease);
    if (body.state !== "retiring" || !body.retiring || body.retiring.deleted !== body.retiring.manifest.length) {
      throw new AuthorityRefusal("retire is incomplete");
    }
    requireResidueCovered(body.pending, body.retiring.manifest);
    await this.#save(dir, { ...body, state: "retired", retiring: null, pending: null, seq: body.seq + 1 });
  }
}

// ---- process-wide worker lifetime accessor --------------------------------------------

let installed: WorkerLifetime | undefined;

/** main.ts installs the W capability once, right after acquisition. */
export function installWorkerLifetime(lifetime: WorkerLifetime): void {
  if (installed) throw new Error("worker lifetime already installed");
  installed = lifetime;
}

/** Later units require the live capability through this; it throws before startup acquired W. */
export function requireWorkerLifetime(): WorkerLifetime {
  if (!installed) throw new AuthorityRefusal("worker lifetime lock not acquired");
  return installed;
}
