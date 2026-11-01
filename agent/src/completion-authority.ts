import { createHash, createHmac, randomUUID, timingSafeEqual } from "node:crypto";
import { constants as fsc } from "node:fs";
import * as fsp from "node:fs/promises";
import path from "node:path";
import {
  AuthorityRefusal, isPrivateDirMode, mkdirPrivate, requireSupportedStorage, transactionContext, type AuthorityTransaction,
  type WorkerLifetime,
} from "./authority-lock.js";
import {
  type CodexSealInput, codexSealFields, type CompletionSourceV2, parseCompletionSourceV2, sourceCoreDigest,
} from "./completion-source.js";

// Completion-authority enrollment and durable source-use slots (issue #3404 M1a).
// Production reaches it through authority-runtime.ts (main.ts builds the runtime). Every operation needs an active authority transaction (A) from
// authority-lock.ts, so a stale or foreign caller cannot write.
//
// Harness and the sticky codex marker: the harness is bound when a lease is minted (an executor
// present means codex, absent means claude) and recorded on the lease. A slot admitted with an
// executor, at any time, carries `codexMarked` in its MAC'd body; it survives re-admission,
// ineligible, retiring and the retired tombstone, so within this process (see Rollback) a reused
// path cannot publish claude_ready.
// Publication takes the CERTIFIED source itself (see transition) and derives the digest and the
// run/generation of the cohort from it.
//
// Files (all 0600 under the 0700 (S_ISGID tolerated, see Directory modes) `<dataDir>/.uzi-authority/`, never persisting st_dev/st_ino):
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
// shard file is never read as empty: all 256 empty shards are created in one atomic step when
// the store is enrolled, so the seq-1 crash window still has a shard to complete, and a missing
// shard (or a heads/ directory that is not a private directory with every shard) refuses.
//
// Enrollment order: the heads are created FIRST (under the storage id the enrollment will carry),
// then enrollment.json is written. An enrolled store whose heads/ is absent therefore refuses
// unconditionally, whether or not slots exist (deleting heads/ and slots/ cannot reset it). A
// crash between the heads rename and the enrollment write leaves heads/ with no enrollment: that
// is adopted (its storage id is reused) only when there are no slots and every shard is an
// authentic empty shard of one storage id; anything else refuses.
//
// Directory modes: a directory created here is chmod'd to exactly 0700 (clearing the S_ISGID a
// setgid parent such as a Kubernetes fsGroup volume would pass down); an existing one is accepted
// with or without S_ISGID. No check compares a gid.
//
// Rollback. Within one process, any restore or reset is detected: the high-water mark (module
// level, keyed by storage id, so reopening the store per transaction keeps it) catches a restored
// slot or shard, and a module-level latch per canonical data root records the storage id this
// process opened, so a later open that finds no enrollment (a full reset of enrollment.json,
// heads/ and slots/) or another storage id refuses instead of re-enrolling. Across a restart
// nothing remembers the high-water mark or the latch: restoring a head shard together with the
// slot(s) of the keys being read, up to and including the whole store (a volume snapshot restore
// or a full reset), is NOT detected and is out of scope. A deleted shard refuses, as does an
// older shard with a newer slot (more than one ahead) or an older slot under a newer shard.

/** Authority layout identity; exposed as `AuthorityStore.layout` for completion-source.ts. */
const AUTHORITY_LAYOUT = "uzi-authority-layout/1";
const ENROLLMENT_VERSION = "uzi-authority-enrollment/1";
const SLOT_VERSION = "uzi-authority-slot/3";
const CERTIFICATE_LABEL = "uzi completion source certificate v2\0";
const MAX_CAPTURE_IDS = 256;
const HEAD_VERSION = "uzi-authority-head/2";
const HEADS_DIR = "heads";
const SHARD = /^[0-9a-f]{2}$/;
const KEY_LABEL = "uzi completion authority v1";
const ENROLLMENT_FILE = "enrollment.json";
const SLOTS_DIR = "slots";
const AUTHORITY_DIR_NAME = ".uzi-authority";
const MAX_ENROLLMENT_BYTES = 4096;
const MAX_SLOT_BYTES = 1 << 20;
const MAX_FOOTPRINT = 1024;
const MAX_ITEM = 512;
const MAX_ITEMS = 1024;
/** A slot exactly one ahead of its head shard: the crash window only a transaction may complete. */
const SLOT_AHEAD_OF_HEAD = "authority slot is one ahead of its head (crash window); a transaction must complete it";
const HEX64 = /^[0-9a-f]{64}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
/** recovery.ts capture ids: randomUUID(), or "maintenance-v1-" + randomUUID() for a maintenance capture. */
const CAPTURE_ID = /^(?:maintenance-v1-)?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

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
/** The authenticated cohort manifest a final slot binds: one run generation's capture ids. */
export interface SlotCohort {
  runId: string;
  generation: number;
  /** Strictly ascending, so a cohort has exactly one spelling. */
  captureIds: string[];
}
/** What a sealed (codex) or claude_ready (claude) slot publishes about its source. */
export interface SlotPublication {
  harness: "codex" | "claude";
  sourceCoreDigest: string;
  cohort: SlotCohort;
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
  /** Present exactly for sealed and claude_ready; kept through ineligible and retiring. */
  publication: SlotPublication | null;
  /** Sticky: set once the slot was admitted with an executor (codex); never cleared, even by retire. */
  codexMarked: boolean;
}
/** The durably saved identity of a slot `transition(..., "ineligible")` just ended. */
export interface EndedSlotReceipt {
  key: string;
  epoch: string;
  seq: number;
  worker: string;
}
/** What `readPublication` returns for retirement verification. */
export interface SlotPublicationRead extends SlotPublication {
  state: SlotState;
  epoch: string;
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

function normalizePublication(raw: unknown): SlotPublication {
  const p = exact(raw, ["harness", "sourceCoreDigest", "cohort"]);
  if (p.harness !== "codex" && p.harness !== "claude") throw new AuthorityRefusal("malformed authority record");
  const c = exact(p.cohort, ["runId", "generation", "captureIds"]);
  if (!Array.isArray(c.captureIds) || c.captureIds.length > MAX_CAPTURE_IDS) throw new AuthorityRefusal("malformed authority record");
  const captureIds = c.captureIds.map((id) => {
    if (typeof id !== "string" || !CAPTURE_ID.test(id)) throw new AuthorityRefusal("malformed authority record");
    return id;
  });
  for (let i = 1; i < captureIds.length; i++) {
    if (!((captureIds[i - 1] as string) < (captureIds[i] as string))) throw new AuthorityRefusal("malformed authority record");
  }
  return {
    harness: p.harness, sourceCoreDigest: hex64(p.sourceCoreDigest),
    cohort: { runId: text(c.runId, 128), generation: uint(c.generation, 1, Number.MAX_SAFE_INTEGER), captureIds },
  };
}

function normalizeSlot(raw: unknown): SlotSnapshot {
  const o = exact(raw, ["version", "key", "state", "epoch", "seq", "owner", "pending", "retiring", "publication", "codexMarked"]);
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
  const publication = o.publication === null ? null : normalizePublication(o.publication);
  if (state === "active" && publication !== null) throw new AuthorityRefusal("malformed authority record");
  if (state === "sealed" && publication?.harness !== "codex") throw new AuthorityRefusal("malformed authority record");
  if (state === "claude_ready" && publication?.harness !== "claude") throw new AuthorityRefusal("malformed authority record");
  if (state === "retired" && publication !== null) throw new AuthorityRefusal("malformed authority record");
  if (typeof o.codexMarked !== "boolean") throw new AuthorityRefusal("malformed authority record");
  const codexMarked = o.codexMarked;
  // A codex executor or a codex publication can never sit in an unmarked slot, nor a claude_ready one in a marked slot.
  if ((owner.executor !== null || publication?.harness === "codex") && !codexMarked) throw new AuthorityRefusal("malformed authority record");
  if (state === "claude_ready" && codexMarked) throw new AuthorityRefusal("malformed authority record");
  return {
    version: SLOT_VERSION, key: hex64(o.key), state: state as SlotState, epoch: uuid(o.epoch),
    seq: uint(o.seq, 1, Number.MAX_SAFE_INTEGER),
    owner: { worker: uuid(owner.worker), executor: owner.executor === null ? null : text(owner.executor, 128) },
    pending, retiring, publication, codexMarked,
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
  let handle: fsp.FileHandle;
  try { handle = await fsp.open(dir, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW); }
  catch (error) {
    // A missing directory, a symlink (ELOOP) and a non-directory (ENOTDIR) are all refusals, not raw errors.
    // Any other errno (EMFILE, EIO, ...) is also fail-closed but named, so operators can tell a transient
    // resource failure from a hostile directory. The refusal is per operation: it is thrown before this
    // guard's callback runs, so nothing durable has been written by it.
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT") throw new AuthorityRefusal("authority directory missing");
    if (code === "ELOOP" || code === "ENOTDIR") throw new AuthorityRefusal("unsafe authority directory");
    throw new AuthorityRefusal(`authority directory unavailable: ${code ?? "error"}`);
  }
  try {
    const held = await handle.stat({ bigint: true });
    if (!held.isDirectory() || held.uid !== BigInt(uid) || !isPrivateDirMode(held.mode)) {
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

/**
 * The authenticated head shard. Every shard exists from store creation on, so a missing one
 * refuses: reading it as empty would let a deleted shard roll back every key in it.
 */
async function readHead(init: StoreIdentity, shard: string): Promise<Map<string, number>> {
  const source = await withDirGuard(path.join(init.dir, HEADS_DIR), init.uid, async (_handle, check) => {
    const read = await readRegular(path.join(init.dir, HEADS_DIR, `${shard}.json`), init.uid, init.maxHeadBytes);
    await check();
    return read;
  }).catch((error: unknown) => {
    if (error instanceof AuthorityRefusal && error.message === "authority directory missing") throw new AuthorityRefusal("authority head missing");
    throw error;
  });
  if (source === null) throw new AuthorityRefusal("authority head missing");
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
  const created = await mkdirPrivate(dir, uid);
  const stat = await fsp.lstat(dir);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== uid || !isPrivateDirMode(stat.mode)) {
    throw new AuthorityRefusal("unsafe authority directory");
  }
  if (created) {
    const handle = await fsp.open(parent, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
    try { await ops.syncDir(handle); } finally { await handle.close(); }
  }
}

/**
 * Storage id this process opened (or created) per canonical data root, for the life of the
 * process. The high-water marks and the codex marker are keyed by storage id, so a reset that
 * mints a fresh id would otherwise start them over; a later open that finds no enrollment, or
 * another id, refuses instead.
 */
const enrolledStorage = new Map<string, string>();

const SHARD_NAMES: readonly string[] = Array.from({ length: 256 }, (_, i) => i.toString(16).padStart(2, "0"));
const STALE_HEADS_TMP = /^\.heads\.[0-9a-f-]{36}\.tmp$/;

/**
 * Create all 256 empty MAC'd shards in one atomic step: stage them in a tmp directory, fsync
 * each file and the directory, rename it to heads/, fsync the parent. A crash leaves either
 * no heads/ or a complete one (plus an unreferenced tmp directory, removed on the next create).
 */
async function createHeads(ops: AuthorityFsOps, init: StoreIdentity): Promise<void> {
  for (const name of await fsp.readdir(init.dir)) {
    if (STALE_HEADS_TMP.test(name)) await fsp.rm(path.join(init.dir, name), { recursive: true, force: true });
  }
  const staging = path.join(init.dir, `.${HEADS_DIR}.${randomUUID()}.tmp`);
  await fsp.mkdir(staging, { mode: 0o700 });
  try {
    await fsp.chmod(staging, 0o700);
    for (const shard of SHARD_NAMES) {
      const handle = await fsp.open(path.join(staging, `${shard}.json`), fsc.O_WRONLY | fsc.O_CREAT | fsc.O_EXCL | fsc.O_NOFOLLOW, 0o600);
      try {
        await handle.chmod(0o600);
        await handle.writeFile(sealHead(init, shard, new Map()), "utf8");
        await ops.syncFile(handle);
      } finally { await handle.close(); }
    }
    const dirHandle = await fsp.open(staging, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
    try { await ops.syncDir(dirHandle); } finally { await dirHandle.close(); }
    await ops.rename(staging, path.join(init.dir, HEADS_DIR));
  } catch (error) {
    await fsp.rm(staging, { recursive: true, force: true }).catch(() => undefined);
    throw error;
  }
  const parent = await fsp.open(init.dir, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
  try { await ops.syncDir(parent); } finally { await parent.close(); }
}

/** An existing heads/ must be a private directory holding every shard; none may be missing. */
async function requireCompleteHeads(init: { dir: string; uid: number }): Promise<void> {
  const names = await withDirGuard(path.join(init.dir, HEADS_DIR), init.uid, async (_handle, check) => {
    const listed = await fsp.readdir(path.join(init.dir, HEADS_DIR));
    await check();
    return new Set(listed);
  });
  if (SHARD_NAMES.some((shard) => !names.has(`${shard}.json`))) throw new AuthorityRefusal("authority head missing");
}

/**
 * heads/ with no enrollment is the crash window between the heads rename and the enrollment write.
 * Return its storage id only when every one of the 256 shards is an authentic EMPTY shard of one
 * storage id; any other content (or a failed MAC) refuses rather than resetting the store.
 */
async function adoptEmptyHeads(init: Omit<StoreIdentity, "storageId">): Promise<string> {
  await requireCompleteHeads(init);
  const ids = new Set<string>();
  await withDirGuard(path.join(init.dir, HEADS_DIR), init.uid, async (_handle, check) => {
    for (const shard of SHARD_NAMES) {
      const source = await readRegular(path.join(init.dir, HEADS_DIR, `${shard}.json`), init.uid, init.maxHeadBytes);
      if (source === null) throw new AuthorityRefusal("authority head missing");
      const head = unseal(init.key, "head", source, normalizeHead);
      if (head.shard !== shard || Object.keys(head.slots).length > 0) {
        throw new AuthorityRefusal("authority heads exist without an enrollment");
      }
      ids.add(head.storageId);
    }
    await check();
  });
  if (ids.size !== 1) throw new AuthorityRefusal("authority heads exist without an enrollment");
  return [...ids][0]!;
}

// ---- store ----------------------------------------------------------------------------

const storeBrand = Symbol("authority-store-token");
const leaseBrand = Symbol("source-use-lease-token");
type LeaseHarness = "codex" | "claude";
const leaseState = new WeakMap<SourceUseLease, { store: AuthorityStore; key: string; epoch: string; harness: LeaseHarness }>();

/** Opaque exact-owner lease for one source-use epoch. Only admitSourceUse can mint one. */
export class SourceUseLease {
  constructor(token: symbol, store: AuthorityStore, key: string, epoch: string, harness: LeaseHarness) {
    if (token !== leaseBrand) throw new TypeError("SourceUseLease cannot be constructed");
    leaseState.set(this, { store, key, epoch, harness });
  }
}

function heldLease(lease: SourceUseLease): { store: AuthorityStore; key: string; epoch: string; harness: LeaseHarness } {
  const held = leaseState.get(lease);
  if (!held) throw new AuthorityRefusal("not a source-use lease");
  return held;
}
/** The epoch a lease was minted for; fences bind it. Opaque leases expose no structural fields. */
export function leaseEpoch(lease: SourceUseLease): string { return heldLease(lease).epoch; }
/** The slot key a lease is bound to. */
export function leaseSlotKey(lease: SourceUseLease): string { return heldLease(lease).key; }
/** The harness bound at admission: codex when an executor was presented, claude otherwise. */
export function leaseHarness(lease: SourceUseLease): LeaseHarness { return heldLease(lease).harness; }

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
  const maxHeadBytes = options.maxHeadBytes ?? MAX_SLOT_BYTES;
  const headsDir = path.join(ctx.authorityDir, HEADS_DIR);
  let headsExist = true;
  try { await fsp.lstat(headsDir); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; headsExist = false; }
  let enrollment: Enrollment;
  const latched = enrolledStorage.get(ctx.dataDir);
  if (existing === null) {
    // This process already opened an enrolled store here: a store without an enrollment now was
    // reset (enrollment, heads and slots removed), and re-enrolling would drop every high-water mark.
    if (latched !== undefined) throw new AuthorityRefusal("authority store was reset after this process enrolled it");
    let entries: string[] = [];
    try { entries = await fsp.readdir(slotsDir); }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    if (entries.length > 0) throw new AuthorityRefusal("authority slots exist without an enrollment");
    // Heads first, then the enrollment that names their storage id (see the header comment).
    const base = { dir: ctx.authorityDir, uid: ctx.uid, key, maxHeadBytes };
    let storageId: string;
    if (headsExist) storageId = await adoptEmptyHeads(base);
    else {
      storageId = randomUUID();
      await createHeads(ops, { ...base, storageId });
    }
    enrollment = { version: ENROLLMENT_VERSION, layout: AUTHORITY_LAYOUT, storageId, dataDir: ctx.dataDir };
    await writeDurable(ops, ctx.authorityDir, ctx.uid, ENROLLMENT_FILE, seal(key, "enrollment", enrollment));
    const written = await readRegular(enrollmentFile, ctx.uid, MAX_ENROLLMENT_BYTES);
    if (written === null) throw new AuthorityRefusal("authority enrollment vanished");
    enrollment = unseal(key, "enrollment", written, normalizeEnrollment);
  } else {
    enrollment = unseal(key, "enrollment", existing, normalizeEnrollment);
    if (enrollment.layout !== AUTHORITY_LAYOUT) throw new AuthorityRefusal("unsupported authority layout");
    if (enrollment.dataDir !== ctx.dataDir) throw new AuthorityRefusal("authority enrolled under a different data root spelling");
    // An enrolled store always has its heads: absent means deleted, never "not created yet".
    if (!headsExist) throw new AuthorityRefusal("enrolled authority store has no heads");
    await requireCompleteHeads({ dir: ctx.authorityDir, uid: ctx.uid });
    if (latched !== undefined && latched !== enrollment.storageId) {
      throw new AuthorityRefusal("authority store was replaced after this process enrolled it");
    }
  }
  enrolledStorage.set(ctx.dataDir, enrollment.storageId);
  return new AuthorityStore(storeBrand, {
    key, storageId: enrollment.storageId, ctxDir: ctx.authorityDir, dataDir: ctx.dataDir, uid: ctx.uid,
    incarnation: options.workerIncarnation ?? PROCESS_INCARNATION, ops,
    maxHeadBytes: options.maxHeadBytes ?? MAX_SLOT_BYTES,
  });
}

/** The worker incarnation a store opened without an explicit `workerIncarnation` writes its slots under. */
export function processWorkerIncarnation(): string { return PROCESS_INCARNATION; }

/**
 * Read one slot with NO authority transaction and no write (see {@link AuthorityStore.peekSlot}). An
 * unenrolled store reads as "nothing recorded" (null) only when it has no slots and no heads either; a
 * store this process already opened that has since lost its enrollment, an unauthenticated or other-layout
 * enrollment, missing heads and a half-created enrollment all refuse (the next transaction adopts or
 * rejects them). The storage-type probe is skipped: W acquisition already required a supported filesystem.
 */
export async function peekAuthoritySlot(
  options: { dataDir: string; workerToken: string; uid?: number }, sourcePath: string,
): Promise<SlotSnapshot | null> {
  if (options.workerToken === "") throw new AuthorityRefusal("worker token required to authenticate authority state");
  const dataDir = options.dataDir;
  const uid = options.uid ?? process.getuid?.() ?? -1;
  const key = createHmac("sha256", options.workerToken).update(KEY_LABEL).digest();
  if (path.resolve(dataDir) !== dataDir || await fsp.realpath(dataDir) !== dataDir) {
    throw new AuthorityRefusal("authority data root is not the canonical spelling");
  }
  const authorityDir = path.join(dataDir, AUTHORITY_DIR_NAME);
  const existing = await readRegular(path.join(authorityDir, ENROLLMENT_FILE), uid, MAX_ENROLLMENT_BYTES);
  const latched = enrolledStorage.get(dataDir);
  if (existing === null) {
    if (latched !== undefined) throw new AuthorityRefusal("authority store was reset after this process enrolled it");
    for (const name of [SLOTS_DIR, HEADS_DIR]) {
      try { await fsp.lstat(path.join(authorityDir, name)); }
      catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") continue;
        throw error;
      }
      throw new AuthorityRefusal("authority state exists without an enrollment");
    }
    return null;
  }
  const enrollment = unseal(key, "enrollment", existing, normalizeEnrollment);
  if (enrollment.layout !== AUTHORITY_LAYOUT) throw new AuthorityRefusal("unsupported authority layout");
  if (enrollment.dataDir !== dataDir) throw new AuthorityRefusal("authority enrolled under a different data root spelling");
  await requireCompleteHeads({ dir: authorityDir, uid });
  if (latched !== undefined && latched !== enrollment.storageId) {
    throw new AuthorityRefusal("authority store was replaced after this process enrolled it");
  }
  enrolledStorage.set(dataDir, enrollment.storageId);
  const store = new AuthorityStore(storeBrand, {
    key, storageId: enrollment.storageId, ctxDir: authorityDir, dataDir, uid,
    incarnation: PROCESS_INCARNATION, ops: realOps, maxHeadBytes: MAX_SLOT_BYTES,
  });
  return store.peekSlot(sourcePath);
}

interface StoreInit {
  key: Buffer; storageId: string; ctxDir: string; dataDir: string; uid: number; incarnation: string; ops: AuthorityFsOps;
  maxHeadBytes: number;
}

const NEXT_STATES: Record<"claude_ready" | "sealed" | "ineligible", SlotState[]> = {
  claude_ready: ["active"],
  sealed: ["active"],
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

/** The published digest a slot may retire from: sealed or claude_ready, nothing pending, and the expected digest if given. */
function retirablePrior(body: SlotSnapshot, expectedDigest: string | undefined): string {
  if (body.state === "retiring" || body.state === "retired") throw new AuthorityRefusal("source already retiring");
  if (body.pending !== null) throw new AuthorityRefusal("unresolved pending footprint");
  if (body.state !== "sealed" && body.state !== "claude_ready") throw new AuthorityRefusal("only a published source can retire");
  if (body.publication === null) throw new AuthorityRefusal("only a published source can retire");
  if (expectedDigest !== undefined && expectedDigest !== body.publication.sourceCoreDigest) {
    throw new AuthorityRefusal("expected digest does not match the published source");
  }
  return body.publication.sourceCoreDigest;
}
/**
 * Whether a pending record refuses a new use or a new pending operation of its slot. A running
 * record always does, and so does a settled_failed destructive record (its residue is the only
 * authority for the bounded retry). A settled_failed bare_mutation record was positively joined:
 * it releases only the mutator-running exclusion, so it must not wedge every later mutation of
 * that bare for the live owner. A new use or operation replaces it.
 */
function blocksNewOperation(pending: PendingFootprint | null): boolean {
  return pending !== null && !(pending.kind === "bare_mutation" && pending.status === "settled_failed");
}
/**
 * The bounded residue retry: a retiring slot whose destructive record settled failed (a joined, accounted
 * failure) may register a NEW destructive record, but only for items the authenticated retiring manifest
 * names. The manifest is the sole authority for residue cleanup, so a footprint outside it (or one too
 * large to read back as items) refuses. A running record never qualifies.
 */
function isResidueRetry(body: SlotSnapshot, input: { kind: string; footprint: string }): boolean {
  const { pending, retiring } = body;
  if (body.state !== "retiring" || !retiring || pending?.kind !== "destructive" || pending.status !== "settled_failed") return false;
  if (input.kind !== "destructive") return false;
  const manifest = new Set(retiring.manifest);
  const items = input.footprint.split("\n");
  return items.length > 0 && items.every((item) => manifest.has(item));
}
function retiringRecord(prior: string, manifest: readonly string[]): RetiringRecord {
  return { prior, operation: randomUUID(), manifest: strings(manifest, MAX_ITEMS), deleted: 0 };
}
/**
 * The `prior` of an operator-discard retiring record. A discarded source has no published proof to cite, so
 * the record names a digest derived from its own slot and epoch: only a record written by admitOperatorDiscard
 * can carry it, and endOperatorDiscard refuses any other retiring record.
 */
function operatorDiscardPrior(key: string, epoch: string): string {
  return createHash("sha256").update(`uzi operator discard\0${key}\0${epoch}`).digest("hex");
}

/** The exact slot an operator-discard takeover replaced (null: none), keyed by the lease the takeover minted. */
const discardPriors = new WeakMap<SourceUseLease, SlotSnapshot | null>();

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
  get layout(): string { return AUTHORITY_LAYOUT; }
  get dataDir(): string { return this.#init.dataDir; }
  /** The worker incarnation this store writes slots under. */
  get workerIncarnation(): string { return this.#init.incarnation; }

  /**
   * Certify a source core. The key never leaves this module, and only the exact live lease of
   * the slot can obtain a certificate: the slot must be active and owned by this worker under
   * the lease's epoch, and the source must carry that epoch, name this slot, this worker, the
   * lease's harness and (codex) the slot's executor. A claude source is refused for a slot that
   * was ever admitted as codex. A codex source additionally needs the actual terminal seal: the
   * store derives the executor incarnation and registry/facade pairs from `terminalSealMaterial`
   * itself (the seal must be for this lease and still accepted) and requires the source to carry
   * exactly those, so a hand-built codex core cannot be certified. A claude lease takes no seal.
   */
  async certifySource(
    tx: AuthorityTransaction, lease: SourceUseLease, value: unknown, codex?: CodexSealInput,
  ): Promise<CompletionSourceV2> {
    const { body, held } = await this.#owned(tx, lease);
    const source = await this.#bindSource(tx, held, body, value);
    if (held.harness === "codex") {
      if (codex === undefined) throw new AuthorityRefusal("codex source needs its terminal seal");
      const derived = codexSealFields(lease, codex);
      if (derived.executorIncarnation !== body.owner.executor ||
          source.executorIncarnation !== derived.executorIncarnation ||
          JSON.stringify(source.registryFacadePairs) !== JSON.stringify(derived.registryFacadePairs)) {
        throw new AuthorityRefusal("source does not match the terminal seal");
      }
    } else if (codex !== undefined) throw new AuthorityRefusal("a claude source takes no terminal seal");
    const { certificate: _old, ...bare } = source;
    return { ...bare, certificate: this.#certificate(sourceCoreDigest(bare)) };
  }

  async #bindSource(
    tx: AuthorityTransaction, held: { key: string; epoch: string; harness: LeaseHarness }, body: SlotSnapshot, value: unknown,
  ): Promise<CompletionSourceV2> {
    if (body.state !== "active") throw new AuthorityRefusal("source slot is not active");
    const source = parseCompletionSourceV2(value, { dataDir: this.#init.dataDir, storageId: this.#init.storageId, layout: AUTHORITY_LAYOUT });
    if (source.sourceUseEpoch !== body.epoch || body.epoch !== held.epoch) throw new AuthorityRefusal("source belongs to another source-use epoch");
    if (await this.slotKey(tx, source.clonePath) !== held.key) throw new AuthorityRefusal("source belongs to another slot");
    if (source.workerIncarnation !== this.#init.incarnation) throw new AuthorityRefusal("source belongs to another worker incarnation");
    if (source.harness !== held.harness) throw new AuthorityRefusal("source harness does not match the lease");
    if (source.harness === "claude") {
      if (body.codexMarked) throw new AuthorityRefusal("slot was admitted as codex");
    } else if (body.owner.executor === null || source.executorIncarnation !== body.owner.executor) {
      throw new AuthorityRefusal("source executor does not match the slot owner");
    }
    return source;
  }

  /** Constant-time check of a certificate against a digest, under a live transaction. */
  verifySourceCertificate(tx: AuthorityTransaction, coreDigest: string, certificate: unknown): boolean {
    this.#check(tx);
    return this.#verifies(coreDigest, certificate);
  }

  #verifies(coreDigest: string, certificate: unknown): boolean {
    if (typeof certificate !== "string" || !HEX64.test(certificate) || !HEX64.test(coreDigest)) return false;
    return timingSafeEqual(Buffer.from(this.#certificate(coreDigest), "hex"), Buffer.from(certificate, "hex"));
  }

  #certificate(coreDigest: string): string {
    return createHmac("sha256", this.#init.key).update(`${CERTIFICATE_LABEL}${this.#init.storageId}\0${coreDigest}`).digest("hex");
  }

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
    return this.#slotKeyOf(sourcePath);
  }

  async #slotKeyOf(sourcePath: string): Promise<string> {
    const rel = await this.#relative(sourcePath);
    return createHmac("sha256", this.#init.key).update(`slot\0${this.#init.storageId}\0${rel}`).digest("hex");
  }

  /**
   * Read a slot WITHOUT an authority transaction and without writing anything. Meant for a reader whose
   * record is written either by an owner that is necessarily dead (W excludes another compatible worker)
   * or serialized in this process by the caller's own lock. It never creates a directory, never completes
   * the crash window between a slot rename and its head update (a slot exactly one ahead of its head is
   * refused, after a short bounded re-read in case a writer in this process is between the two renames), and
   * never moves the in-process high-water mark. Not for any decision that must be atomic with a write.
   */
  async peekSlot(sourcePath: string): Promise<SlotSnapshot | null> {
    const key = await this.#slotKeyOf(sourcePath);
    const dir = path.join(this.#init.ctxDir, SLOTS_DIR);
    for (let attempt = 0; ; attempt++) {
      try { return await this.#load(dir, key, true); }
      catch (error) {
        if (!(error instanceof AuthorityRefusal) || error.message !== SLOT_AHEAD_OF_HEAD || attempt >= 3) throw error;
        await new Promise((resolve) => setTimeout(resolve, 25));
      }
    }
  }

  #identityOf(): StoreIdentity {
    return {
      dir: this.#init.ctxDir, uid: this.#init.uid, key: this.#init.key, storageId: this.#init.storageId,
      maxHeadBytes: this.#init.maxHeadBytes,
    };
  }

  async #load(dir: string, key: string, readOnly = false): Promise<SlotSnapshot | null> {
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
      // A read-only load never completes the window: it refuses, and the next transaction finishes it.
      if (readOnly) throw new AuthorityRefusal(SLOT_AHEAD_OF_HEAD);
      head.set(key, body.seq);
      await writeHead(this.#init.ops, this.#identityOf(), shardOf(key), head);
    }
    if (!readOnly) this.#highWater.set(key, body.seq);
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
    tx: AuthorityTransaction, sourcePath: string,
    options: { executor?: string; lease?: SourceUseLease; destructive?: boolean; consumePublication?: boolean } = {},
  ): Promise<SourceUseLease> {
    this.#check(tx);
    if (options.executor !== undefined &&
        (typeof options.executor !== "string" || options.executor.length < 1 || options.executor.length > 128)) {
      throw new AuthorityRefusal("invalid executor");
    }
    const key = await this.slotKey(tx, sourcePath);
    const dir = await this.#slotsDir();
    const old = await this.#load(dir, key);
    const harness: LeaseHarness = options.executor !== undefined ? "codex" : "claude";
    if (old) {
      if (blocksNewOperation(old.pending)) throw new AuthorityRefusal("source has an unresolved pending footprint");
      if (old.state === "retiring") throw new AuthorityRefusal("source is being retired", "retiring");
      if (old.state === "active" && old.owner.worker !== this.#init.incarnation) {
        throw new AuthorityRefusal("source is active under another worker incarnation");
      }
      // A fresh epoch writes `publication: null`, so minting one over a live publication (sealed/claude_ready)
      // would erase the published proof with no retiring record. Only a caller that passes the explicit
      // `consumePublication` purpose may do that: the canonical/attempt seeds in git.ts, which run after the
      // journal guards found no journal naming the path. A destructive purpose never consumes (retirement via
      // beginRetirement is the way to destroy a published source), and bind, predecessor, normalize and
      // discard admissions pass no purpose, so they refuse.
      if (options.destructive === true && old.publication !== null) {
        throw new AuthorityRefusal("source is published; a destructive operation needs its retiring lease");
      }
      if ((old.state === "sealed" || old.state === "claude_ready") && old.publication !== null && options.consumePublication !== true) {
        throw new AuthorityRefusal("source is published; a new epoch needs an explicit purpose that consumes the publication");
      }
    }
    if (options.lease) {
      const held = leaseState.get(options.lease);
      if (!held || held.store !== this || held.key !== key) throw new AuthorityRefusal("lease does not belong to this source");
      if (old?.state !== "active" || held.epoch !== old.epoch) throw new AuthorityRefusal("stale source-use lease");
      if (options.executor !== undefined && options.executor !== old.owner.executor) {
        throw new AuthorityRefusal("lease was admitted under another executor");
      }
      return options.lease;
    }
    const body: SlotSnapshot = {
      version: SLOT_VERSION, key, state: "active", epoch: randomUUID(), seq: (old?.seq ?? 0) + 1,
      owner: { worker: this.#init.incarnation, executor: options.executor ?? null }, pending: null, retiring: null,
      publication: null, codexMarked: harness === "codex" || (old?.codexMarked ?? false),
    };
    await this.#save(dir, body);
    return new SourceUseLease(leaseBrand, this, key, body.epoch, harness);
  }

  async #owned(tx: AuthorityTransaction, lease: SourceUseLease): Promise<{
    dir: string; body: SlotSnapshot; held: { key: string; epoch: string; harness: LeaseHarness };
  }> {
    this.#check(tx);
    const held = leaseState.get(lease);
    if (!held || held.store !== this) throw new AuthorityRefusal("lease does not belong to this store");
    const dir = await this.#slotsDir();
    const body = await this.#load(dir, held.key);
    if (!body) throw new AuthorityRefusal("authority slot missing");
    if (body.epoch !== held.epoch || body.owner.worker !== this.#init.incarnation) {
      throw new AuthorityRefusal("stale source-use lease");
    }
    return { dir, body, held: { key: held.key, epoch: held.epoch, harness: held.harness } };
  }

  /**
   * Move the slot on under the exact live lease. claude_ready needs a claude-admitted lease and a
   * slot never admitted as codex; sealed needs a codex-admitted lease. Both take the CERTIFIED
   * source itself plus the cohort's capture ids: the source is parsed, its certificate verified,
   * and it must carry this lease's epoch, this slot, this worker, the lease's harness and (codex)
   * the slot owner's executor. The store computes the core digest and takes the cohort's run and
   * generation from the source, so a caller cannot supply a digest or a foreign cohort. The
   * publication is written in the same step and kept until retirement.
   */
  async transition(tx: AuthorityTransaction, lease: SourceUseLease, to: "ineligible"): Promise<EndedSlotReceipt>;
  async transition(
    tx: AuthorityTransaction, lease: SourceUseLease, to: "claude_ready" | "sealed", input: { source: unknown; captureIds: readonly string[] },
  ): Promise<void>;
  async transition(
    tx: AuthorityTransaction, lease: SourceUseLease, to: "claude_ready" | "sealed" | "ineligible",
    input?: { source: unknown; captureIds: readonly string[] },
  ): Promise<EndedSlotReceipt | void> {
    const { dir, body, held } = await this.#owned(tx, lease);
    if (!NEXT_STATES[to].includes(body.state)) throw new AuthorityRefusal(`illegal transition ${body.state} -> ${to}`);
    if (to === "ineligible") {
      if (input !== undefined) throw new AuthorityRefusal("ineligible carries no publication");
      const seq = body.seq + 1;
      // #save returns only after tmp -> fsync -> rename -> dir fsync -> readback -> head, so this is a durable end.
      await this.#save(dir, { ...body, state: to, seq });
      return { key: body.key, epoch: body.epoch, seq, worker: body.owner.worker };
    }
    if (body.pending !== null) throw new AuthorityRefusal("unresolved pending footprint");
    if (input === undefined) throw new AuthorityRefusal("publication data required");
    const wanted = to === "sealed" ? "codex" : "claude";
    if (held.harness !== wanted) throw new AuthorityRefusal(`${to} requires harness ${wanted}`);
    const source = await this.#bindSource(tx, held, body, input.source);
    const digest = sourceCoreDigest(source);
    if (!this.#verifies(digest, source.certificate)) throw new AuthorityRefusal("completion source certificate does not verify", "certificate_invalid");
    const publication = normalizePublication(JSON.parse(JSON.stringify({
      harness: wanted, sourceCoreDigest: digest,
      cohort: { runId: source.context.runId, generation: source.context.generation, captureIds: input.captureIds },
    })));
    await this.#save(dir, { ...body, state: to, publication, seq: body.seq + 1 });
  }

  /**
   * The published digest and cohort of a sealed or claude_ready source, for retirement
   * verification. Null when the slot is absent or never published; a pending footprint or a
   * malformed or replayed slot refuses.
   */
  async readPublication(tx: AuthorityTransaction, sourcePath: string): Promise<SlotPublicationRead | null> {
    const key = await this.slotKey(tx, sourcePath);
    const slot = await this.#load(await this.#slotsDir(), key);
    if (!slot) return null;
    if (slot.pending !== null) throw new AuthorityRefusal("source has an unresolved pending footprint");
    if (slot.publication === null) return null;
    return { ...structuredClone(slot.publication), state: slot.state, epoch: slot.epoch };
  }

  /** Durably record intent BEFORE a bare mutation or destructive operation begins. */
  async beginPending(
    tx: AuthorityTransaction, lease: SourceUseLease, input: { kind: "bare_mutation" | "destructive"; footprint: string },
  ): Promise<string> {
    const { dir, body } = await this.#owned(tx, lease);
    if (body.state === "retired") throw new AuthorityRefusal("source is retiring");
    // Post-publication, any source operation needs a fresh admitSourceUse epoch first.
    if (body.state === "sealed" || body.state === "claude_ready") throw new AuthorityRefusal("source is published; admit a new epoch first");
    if (blocksNewOperation(body.pending) && !isResidueRetry(body, input)) {
      throw new AuthorityRefusal("a pending footprint is already recorded");
    }
    const operation = randomUUID();
    const pending: PendingFootprint = {
      operation, kind: input.kind, footprint: text(input.footprint, MAX_FOOTPRINT), status: "running", residue: [],
    };
    await this.#save(dir, { ...body, pending, seq: body.seq + 1 });
    return operation;
  }

  /**
   * Admit a bare-mutator epoch and record its running pending footprint in ONE slot write, so a
   * crash cannot leave an active slot with nothing pending. Refuses exactly when admitSourceUse (a
   * pending record that blocks, a retiring slot, an active slot of another incarnation) followed by
   * beginPending would. A bare-mutator slot an older build left active with nothing pending is a
   * foreign active slot and so keeps refusing. Returns the lease and the pending operation id.
   */
  async admitBareMutation(
    tx: AuthorityTransaction, sourcePath: string, input: { footprint: string },
  ): Promise<{ lease: SourceUseLease; operation: string }> {
    this.#check(tx);
    const key = await this.slotKey(tx, sourcePath);
    const dir = await this.#slotsDir();
    const old = await this.#load(dir, key);
    if (old) {
      if (blocksNewOperation(old.pending)) throw new AuthorityRefusal("source has an unresolved pending footprint");
      if (old.state === "retiring") throw new AuthorityRefusal("source is being retired", "retiring");
      if (old.state === "active" && old.owner.worker !== this.#init.incarnation) {
        throw new AuthorityRefusal("source is active under another worker incarnation");
      }
    }
    const operation = randomUUID();
    const pending: PendingFootprint = {
      operation, kind: "bare_mutation", footprint: text(input.footprint, MAX_FOOTPRINT), status: "running", residue: [],
    };
    const body: SlotSnapshot = {
      version: SLOT_VERSION, key, state: "active", epoch: randomUUID(), seq: (old?.seq ?? 0) + 1,
      owner: { worker: this.#init.incarnation, executor: null }, pending, retiring: null,
      publication: null, codexMarked: old?.codexMarked ?? false,
    };
    await this.#save(dir, body);
    return { lease: new SourceUseLease(leaseBrand, this, key, body.epoch, "claude"), operation };
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

  /**
   * Retire the source this incarnation published. Only a sealed or claude_ready slot with no
   * pending record can retire, and `prior` is never caller-supplied: it is the slot's own
   * published source-core digest, so retiring persists exactly what was proven. `expectedDigest`
   * is an optional guard that must equal it.
   */
  async markRetiring(tx: AuthorityTransaction, lease: SourceUseLease, input: { expectedDigest?: string; manifest: string[] }): Promise<void> {
    const { dir, body } = await this.#owned(tx, lease);
    const prior = retirablePrior(body, input.expectedDigest);
    await this.#save(dir, {
      ...body, state: "retiring", retiring: retiringRecord(prior, input.manifest), seq: body.seq + 1,
    });
  }

  /**
   * Begin retirement of a slot published by ANY worker incarnation, including a dead earlier one
   * (restart retirement). The caller names the exact epoch and published digest it verified; both
   * must equal the slot's. The slot must be sealed or claude_ready with no pending record, so an
   * active foreign epoch (an unfinished source use) still refuses. The slot moves to retiring under
   * this worker, same epoch, publication kept, and the returned lease drives
   * advanceRetiring / markRetired / beginPending / settlePending.
   *
   * Resume: a slot already `retiring` under ANOTHER incarnation (its owner died mid-retirement) is
   * taken over when its pending record is absent or settled_failed (never running), the epoch is the
   * same and `expectedDigest` equals the retiring record's `prior`. The retiring record (prior,
   * manifest, deleted progress, operation) is kept as it is; only the owner changes. A retiring slot
   * this incarnation already owns still refuses, and `input.manifest` is not consulted on a resume.
   */
  async beginRetirement(
    tx: AuthorityTransaction, sourcePath: string, input: { expectedEpoch: string; expectedDigest: string; manifest: string[] },
  ): Promise<SourceUseLease> {
    this.#check(tx);
    const key = await this.slotKey(tx, sourcePath);
    const dir = await this.#slotsDir();
    const body = await this.#load(dir, key);
    if (!body) throw new AuthorityRefusal("authority slot missing");
    if (body.epoch !== uuid(input.expectedEpoch)) throw new AuthorityRefusal("source belongs to another source-use epoch");
    if (body.state === "retiring" && body.retiring && body.owner.worker !== this.#init.incarnation) {
      if (body.pending?.status === "running") throw new AuthorityRefusal("unresolved pending footprint");
      if (hex64(input.expectedDigest) !== body.retiring.prior || body.publication === null) {
        throw new AuthorityRefusal("expected digest does not match the retiring source");
      }
      await this.#save(dir, {
        ...body, owner: { worker: this.#init.incarnation, executor: body.owner.executor }, seq: body.seq + 1,
      });
      return new SourceUseLease(leaseBrand, this, key, body.epoch, body.publication.harness);
    }
    const prior = retirablePrior(body, hex64(input.expectedDigest));
    const retiring = retiringRecord(prior, input.manifest);
    await this.#save(dir, {
      ...body, state: "retiring", owner: { worker: this.#init.incarnation, executor: body.owner.executor }, retiring, seq: body.seq + 1,
    });
    return new SourceUseLease(leaseBrand, this, key, body.epoch, (body.publication as SlotPublication).harness);
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
    if (body.pending?.status === "running") throw new AuthorityRefusal("unresolved pending footprint");
    requireResidueCovered(body.pending, body.retiring.manifest);
    await this.#save(dir, { ...body, state: "retired", retiring: null, pending: null, publication: null, seq: body.seq + 1 });
  }

  /**
   * issue #3404 M2 U5 - the ONE operator-discard takeover. The existing guarded operator discard (git.ts
   * discardRetainedSource, reached through retained-source-server) has already authorized exactly the paths in
   * `footprint` (the source and its paired skills dir); this stops authority state from blocking it for those
   * footprints. It admits a fresh epoch owned by this incarnation in state `retiring`, whatever the slot held:
   * an `active` slot a crashed worker left behind or an `ineligible` slot. It refuses a published slot and a
   * `retiring` one that is not an earlier discard's own (a restart retirement owns those; see the checks below).
   * The retiring record is the discard's own transaction (manifest = footprint,
   * `prior` = operatorDiscardPrior), so destructive records for the footprint are residue retries inside the
   * manifest and {@link endOperatorDiscard} tombstones the slot once the deletion is confirmed.
   *
   * Refuses a `running` pending record (an unresolved helper may still be walking the footprint) and a
   * `settled_failed` record whose residue lies outside `footprint`: the discard never clears evidence about
   * anything it does not delete. A settled_failed record is KEPT in the new slot, so its residue stays on
   * record until a confirmed deletion retires it.
   */
  async admitOperatorDiscard(tx: AuthorityTransaction, sourcePath: string, footprint: readonly string[]): Promise<SourceUseLease> {
    this.#check(tx);
    const key = await this.slotKey(tx, sourcePath);
    const dir = await this.#slotsDir();
    const old = await this.#load(dir, key);
    const items = strings([...footprint], MAX_ITEMS);
    if (items.length === 0) throw new AuthorityRefusal("operator discard needs a footprint");
    // A published source (sealed/claude_ready, or any slot still carrying a publication) and a restart retirement in
    // progress belong to restart retirement: the discard authorizes neither its proof nor its manifest, and taking
    // either over would replace the authenticated retirement with its own transaction. Only a discard's own earlier
    // retiring record (its `prior` is derived from the slot and epoch) may be retaken.
    if (old?.state === "retiring" && old.retiring?.prior !== operatorDiscardPrior(key, old.epoch)) {
      throw new AuthorityRefusal("source is being retired; restart retirement owns it");
    }
    if (old && (old.state === "sealed" || old.state === "claude_ready" || old.publication !== null)) {
      throw new AuthorityRefusal("source is published; restart retirement owns it");
    }
    if (old?.pending?.status === "running") throw new AuthorityRefusal("source has an unresolved pending footprint");
    if (old?.pending && !old.pending.residue.every((item) => items.includes(item))) {
      throw new AuthorityRefusal("pending residue is not covered by the discard footprint");
    }
    const epoch = randomUUID();
    const body: SlotSnapshot = {
      version: SLOT_VERSION, key, state: "retiring", epoch, seq: (old?.seq ?? 0) + 1,
      owner: { worker: this.#init.incarnation, executor: null }, pending: old?.pending ?? null,
      retiring: { prior: operatorDiscardPrior(key, epoch), operation: randomUUID(), manifest: items, deleted: 0 },
      publication: null, codexMarked: old?.codexMarked ?? false,
    };
    await this.#save(dir, body);
    const lease = new SourceUseLease(leaseBrand, this, key, epoch, "claude");
    discardPriors.set(lease, old);
    return lease;
  }

  /**
   * End the discard {@link admitOperatorDiscard} opened. `allGone` is the caller's confirmed finding that every
   * footprint item is absent: the slot is then tombstoned (`retired`, pending and publication dropped; the
   * residue it recorded was inside the footprint, so it is gone with it). Otherwise a slot with nothing
   * pending leaves `retiring` for `ineligible` (the source stays as it was, usable by a retained recovery) and
   * a slot with a settled_failed record stays `retiring`, keeping its residue as evidence. A running pending
   * record refuses. Only a record this method's admission wrote qualifies.
   *
   * `nothingDeleted` is the caller's finding that every footprint item that existed at the takeover still exists.
   * The slot is then put back EXACTLY as the takeover found it (a crashed worker's foreign `active` epoch keeps
   * guarding the source, a settled_failed record stays evidence), provided no operation changed the pending
   * record meanwhile; a discard that deleted nothing never weakens the prior state.
   */
  async endOperatorDiscard(
    tx: AuthorityTransaction, lease: SourceUseLease, outcome: { allGone: boolean; nothingDeleted?: boolean },
  ): Promise<"retired" | "ineligible" | "retained" | "restored"> {
    const { dir, body } = await this.#owned(tx, lease);
    if (body.state !== "retiring" || !body.retiring || body.retiring.prior !== operatorDiscardPrior(body.key, body.epoch)) {
      throw new AuthorityRefusal("source is not an operator discard");
    }
    if (body.pending?.status === "running") throw new AuthorityRefusal("unresolved pending footprint");
    if (outcome.allGone) {
      requireResidueCovered(body.pending, body.retiring.manifest);
      await this.#save(dir, { ...body, state: "retired", retiring: null, pending: null, publication: null, seq: body.seq + 1 });
      return "retired";
    }
    const prior = discardPriors.get(lease);
    // An `active` prior owned by THIS incarnation is never put back: the discard's quiescence proof ended that
    // epoch's use, its in-memory lease is released with the discard (so nobody in this process holds it any more),
    // and restoring it would leave an active epoch no process can end, which a restart would read as a foreign
    // worker's. It ends as `ineligible` below. A foreign active, an ineligible slot and settled_failed evidence are
    // restored exactly.
    const ownActive = prior?.state === "active" && prior.owner.worker === this.#init.incarnation;
    if (outcome.nothingDeleted === true && prior && !ownActive && JSON.stringify(prior.pending) === JSON.stringify(body.pending)) {
      await this.#save(dir, { ...prior, seq: body.seq + 1 });
      return "restored";
    }
    if (body.pending !== null) return "retained";
    await this.#save(dir, { ...body, state: "ineligible", retiring: null, seq: body.seq + 1 });
    return "ineligible";
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
