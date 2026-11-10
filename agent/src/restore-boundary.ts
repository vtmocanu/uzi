// Issue #3644 Gate B (compose-first independent restore): the per-process boundary record, the compose
// cross-check, the restore eligibility predicate and the complete attribution census.
//
// The record is write-once per process at `<dataDir>/.uzi-authority/boundaries/<sha256(incarnation)>.json`
// and MAC'd with the authority store's derived key under its own domain label. A successor reads exactly the
// predecessor's file (named by the slot's owner worker incarnation) with no fallback: a missing or invalid
// record always refuses. Nothing here reads or stats any source clone.
import { createHash, createHmac, timingSafeEqual } from "node:crypto";
import * as fsc from "node:constants";
import * as fsp from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { isPrivateDirMode, mkdirPrivate } from "./authority-lock.js";
import { parseAttemptLedgerEntry, parseRecoveryJournal, parseRestoreJournal, parseRestoreTombstone, parseRetainedDescriptorShape, parseStoppedMarkerShape,
  type RecoveryJournalEntry } from "./git.js";
import { parseRestoreValue } from "./restore-journal.js";
import { deriveAuthorityKey, processWorkerIncarnation, readRegular, type SlotSnapshot } from "./completion-authority.js";

const AUTHORITY_DIR = ".uzi-authority";
const BOUNDARIES_DIR = "boundaries";
const MAC_LABEL = "uzi restore boundary v1";
const MAX_RECORD_BYTES = 8192;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const DIGITS_RE = /^[0-9]{1,30}$/;

export type BoundarySlot =
  | { kind: "compose"; hostname: string; containerId: string }
  | { kind: "hosted"; podUid: string; container: string };

export interface BoundaryExecution { nsDev: string; nsIno: string; initStartTicks: string; bootId: string }

export interface BoundaryRecord {
  version: 1;
  incarnation: string;
  slot: BoundarySlot;
  crossCheck: "ok" | "failed" | "na";
  execution: BoundaryExecution;
}

/** What the process looks like right now (the record body minus the incarnation). */
export interface LiveBoundaryIdentity { slot: BoundarySlot; crossCheck: "ok" | "na"; execution: BoundaryExecution }

export type BoundaryRead =
  | { state: "missing" }
  | { state: "invalid" }
  | { state: "ok"; record: BoundaryRecord };

// ---- the sealed record ------------------------------------------------------------------------------------------

function bodyOf(r: BoundaryRecord): BoundaryRecord {
  const slot: BoundarySlot = r.slot.kind === "compose"
    ? { kind: "compose", hostname: r.slot.hostname, containerId: r.slot.containerId }
    : { kind: "hosted", podUid: r.slot.podUid, container: r.slot.container };
  return { version: 1, incarnation: r.incarnation, slot, crossCheck: r.crossCheck,
    execution: { nsDev: r.execution.nsDev, nsIno: r.execution.nsIno, initStartTicks: r.execution.initStartTicks, bootId: r.execution.bootId } };
}

function macOf(workerToken: string, record: BoundaryRecord): string {
  return createHmac("sha256", deriveAuthorityKey(workerToken)).update(MAC_LABEL).update("\0")
    .update(JSON.stringify(bodyOf(record))).digest("hex");
}

/** The persisted spelling: fixed field order, then the MAC. */
export function sealBoundaryRecord(workerToken: string, record: BoundaryRecord): string {
  return JSON.stringify({ ...bodyOf(record), mac: macOf(workerToken, record) });
}

function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v) && Object.getPrototypeOf(v) === Object.prototype;
}
function exactKeys(o: Record<string, unknown>, keys: string[]): boolean {
  const have = Object.keys(o);
  return have.length === keys.length && keys.every(k => have.includes(k));
}
const str = (v: unknown, max = 256): v is string => typeof v === "string" && v.length > 0 && v.length <= max && !v.includes("\0");

/** Strict decode + MAC check. Returns undefined for anything that is not exactly a record sealed under `workerToken`. */
export function unsealBoundaryRecord(workerToken: string, raw: string): BoundaryRecord | undefined {
  let parsed: unknown;
  try { parsed = JSON.parse(raw); } catch { return undefined; }
  if (!isObj(parsed) || !exactKeys(parsed, ["version", "incarnation", "slot", "crossCheck", "execution", "mac"])) return undefined;
  if (parsed.version !== 1 || !str(parsed.incarnation, 128) || !str(parsed.mac, 64)) return undefined;
  if (parsed.crossCheck !== "ok" && parsed.crossCheck !== "failed" && parsed.crossCheck !== "na") return undefined;
  const s = parsed.slot;
  let slot: BoundarySlot;
  if (isObj(s) && s.kind === "compose" && exactKeys(s, ["kind", "hostname", "containerId"]) && str(s.hostname) && str(s.containerId)) {
    slot = { kind: "compose", hostname: s.hostname, containerId: s.containerId };
  } else if (isObj(s) && s.kind === "hosted" && exactKeys(s, ["kind", "podUid", "container"]) && str(s.podUid) && str(s.container)) {
    slot = { kind: "hosted", podUid: s.podUid, container: s.container };
  } else return undefined;
  const e = parsed.execution;
  if (!isObj(e) || !exactKeys(e, ["nsDev", "nsIno", "initStartTicks", "bootId"]) || !str(e.bootId, 64) ||
      ![e.nsDev, e.nsIno, e.initStartTicks].every(x => typeof x === "string" && DIGITS_RE.test(x))) return undefined;
  const record: BoundaryRecord = { version: 1, incarnation: parsed.incarnation, slot, crossCheck: parsed.crossCheck,
    execution: { nsDev: e.nsDev as string, nsIno: e.nsIno as string, initStartTicks: e.initStartTicks as string, bootId: e.bootId } };
  const want = Buffer.from(macOf(workerToken, record), "hex");
  const have = Buffer.from(parsed.mac as string, "hex");
  if (want.length !== have.length || !timingSafeEqual(want, have)) return undefined;
  return record;
}

export function boundaryFileName(incarnation: string): string {
  return `${createHash("sha256").update(incarnation).digest("hex")}.json`;
}

// ---- injectable filesystem seams (each durability step is individually failable in tests) -----------------------

export interface BoundaryFsOps {
  syncDir(dir: string): Promise<void>;
  /** mkdir 0700, verified real/owned/private; tolerates an existing private directory. */
  mkdirPrivate(dir: string): Promise<void>;
  /** Create with O_EXCL|O_NOFOLLOW, mode 0600, write, fsync, close. */
  writeTmp(file: string, data: string): Promise<void>;
  /** link(2): refuses with EEXIST rather than replacing. */
  link(from: string, to: string): Promise<void>;
  unlink(file: string): Promise<void>;
  /** The file's text, or null when absent. Refuses a symlink, non-regular file or oversize file. */
  readFile(file: string): Promise<string | null>;
}

export function realBoundaryOps(uid: number = process.getuid?.() ?? -1): BoundaryFsOps {
  return {
    async syncDir(dir) {
      const h = await fsp.open(dir, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
      try { await h.sync(); } finally { await h.close(); }
    },
    async mkdirPrivate(dir) {
      await mkdirPrivate(dir, uid);
      const st = await fsp.lstat(dir);
      if (!st.isDirectory() || st.isSymbolicLink() || st.uid !== uid || !isPrivateDirMode(st.mode)) throw new Error("unsafe boundaries directory");
    },
    async writeTmp(file, data) {
      const h = await fsp.open(file, fsc.O_WRONLY | fsc.O_CREAT | fsc.O_EXCL | fsc.O_NOFOLLOW, 0o600);
      try { await h.writeFile(data, "utf8"); await h.sync(); } finally { await h.close(); }
    },
    link: (from, to) => fsp.link(from, to),
    unlink: (file) => fsp.unlink(file),
    async readFile(file) {
      // The record's directory must itself be a private real directory owned by the worker (a missing one means no record).
      try {
        const dir = await fsp.lstat(path.dirname(file));
        if (!dir.isDirectory() || dir.isSymbolicLink() || dir.uid !== uid || !isPrivateDirMode(dir.mode)) throw new Error("unsafe boundaries directory");
      } catch (e) { if ((e as NodeJS.ErrnoException).code === "ENOENT") return null; throw e; }
      // O_NOFOLLOW|O_NONBLOCK open, then fstat: a regular, 0600, single-link file of ours within the cap, read at most that long.
      return readRegular(file, uid, MAX_RECORD_BYTES);
    },
  };
}

// ---- procfs readers (injectable root) ---------------------------------------------------------------------------

export type IdentityRefusal = "procfs_unreadable" | "timens_refused" | "cross_check_failed" | "procfs_hidden";

export interface LiveIdentityOptions {
  /** Defaults to `/proc`. */
  procRoot?: string;
  /** Defaults to `os.hostname()`. */
  hostname?: () => string;
  /** `UZI_POD_UID` (a UUID means a hosted slot). */
  podUid?: string | undefined;
  /** The hosted container name (only recorded). */
  container?: string;
}

function parseInitStartTicks(text: string): string | undefined {
  const rest = text.slice(text.lastIndexOf(")") + 2).split(" ");
  const ticks = rest[19]; // stat field 22 (starttime); the slice starts at field 3
  return ticks !== undefined && DIGITS_RE.test(ticks) ? ticks : undefined;
}

/** Zero `timens_offsets` only: a shifted time namespace voids the boot/start-time comparison. */
function timensZero(text: string): boolean {
  const lines = text.split("\n").filter(l => l.trim() !== "");
  if (lines.length === 0) return false;
  return lines.every(l => {
    const m = /^(monotonic|boottime)\s+(-?\d+)\s+(-?\d+)\s*$/.exec(l);
    return m !== null && BigInt(m[2]!) === 0n && BigInt(m[3]!) === 0n;
  });
}

/** The mount point field of a mountinfo row, with its `\040`-style escapes decoded. */
function unescapeMount(s: string): string {
  return s.replace(/\\([0-7]{3})/g, (_, o: string) => String.fromCharCode(parseInt(o, 8)));
}

/**
 * Three-way visibility of the procfs mounted at `procRoot`, the rule of the run-procs.ts procMount scan:
 * "hidden" when its row carries `hidepid` other than 0/off or any `subset` (per-mount or per-superblock options);
 * "unknown" for a malformed row, a row at `procRoot` that is not fstype `proc`, or no row at `procRoot`;
 * "ok" otherwise. Unknown fails closed exactly like hidden (run-procs callers fail closed on unknown too).
 */
function procfsVisibility(text: string, procRoot: string): "ok" | "hidden" | "unknown" {
  const want = path.resolve(procRoot);
  let found = false;
  for (const line of text.split("\n")) {
    if (line === "") continue;
    const cut = line.indexOf(" - ");
    if (cut < 0) return "unknown";
    const lf = line.slice(0, cut).split(" ").filter(x => x !== "");
    const rf = line.slice(cut + 3).split(" ").filter(x => x !== "");
    if (lf.length < 6 || rf.length < 3) return "unknown";
    if (unescapeMount(lf[4]!) !== want) continue;
    found = true;
    if (rf[0] !== "proc") return "unknown";
    for (const opts of [lf[5]!, rf[2]!]) {
      for (const opt of opts.split(",")) {
        const eq = opt.indexOf("=");
        const k = eq < 0 ? opt : opt.slice(0, eq);
        const v = eq < 0 ? "" : opt.slice(eq + 1);
        if ((k === "hidepid" && v !== "0" && v !== "off") || k === "subset") return "hidden";
      }
    }
  }
  return found ? "ok" : "unknown";
}

/** The container id behind `/etc/hostname`, or undefined when the topmost row does not look like a container hostname bind. */
function containerIdFromMountinfo(text: string): string | undefined {
  let root: string | undefined;
  for (const line of text.split("\n")) {
    const f = line.split(" ");
    if (f.length < 7 || f[4] === undefined) continue;
    if (unescapeMount(f[4]) !== "/etc/hostname") continue;
    root = unescapeMount(f[3] ?? "");
  }
  if (root === undefined) return undefined;
  const m = /(?:^|\/)containers\/([0-9a-f]{64})\/hostname$/.exec(root);
  return m ? m[1] : undefined;
}

/**
 * Compute the live slot + execution identity. Hosted lanes (a UUID `UZI_POD_UID`) carry no cross-check; a compose
 * candidate must pass item 6: the `/etc/hostname` bind's root is `containers/<64 hex>/hostname` and the hostname
 * equals the first 12 hex characters.
 */
export async function readLiveIdentity(options: LiveIdentityOptions = {}):
  Promise<{ ok: true; identity: LiveBoundaryIdentity } | { ok: false; reason: IdentityRefusal }> {
  const root = options.procRoot ?? "/proc";
  let execution: BoundaryExecution;
  try {
    const ns = await fsp.stat(`${root}/self/ns/pid`, { bigint: true });
    const init = await fsp.readFile(`${root}/1/stat`, "latin1");
    const ticks = parseInitStartTicks(init);
    const bootId = (await fsp.readFile(`${root}/sys/kernel/random/boot_id`, "utf8")).trim();
    if (ticks === undefined || !UUID_RE.test(bootId)) return { ok: false, reason: "procfs_unreadable" };
    execution = { nsDev: ns.dev.toString(), nsIno: ns.ino.toString(), initStartTicks: ticks, bootId: bootId.toLowerCase() };
  } catch { return { ok: false, reason: "procfs_unreadable" }; }
  try {
    if (!timensZero(await fsp.readFile(`${root}/self/timens_offsets`, "utf8"))) return { ok: false, reason: "timens_refused" };
  } catch { return { ok: false, reason: "timens_refused" }; }
  let mountinfo: string | undefined;
  try { mountinfo = await fsp.readFile(`${root}/self/mountinfo`, "utf8"); } catch { mountinfo = undefined; }
  // Same visibility rule as the run-procs scan: a procfs mounted with hidepid/subset hides processes, so the
  // identity fields above cannot be trusted to describe the whole container. Applies to both slot kinds, and an
  // undeterminable visibility (unreadable mountinfo, malformed/non-proc/missing row) fails closed as
  // procfs_unreadable.
  if (mountinfo === undefined) return { ok: false, reason: "procfs_unreadable" };
  const visibility = procfsVisibility(mountinfo, root);
  if (visibility === "hidden") return { ok: false, reason: "procfs_hidden" };
  if (visibility === "unknown") return { ok: false, reason: "procfs_unreadable" };
  if (options.podUid !== undefined && UUID_RE.test(options.podUid)) {
    return { ok: true, identity: { slot: { kind: "hosted", podUid: options.podUid.toLowerCase(), container: options.container ?? "worker" },
      crossCheck: "na", execution } };
  }
  const containerId = containerIdFromMountinfo(mountinfo);
  const hostname = (options.hostname ?? os.hostname)();
  if (containerId === undefined || hostname !== containerId.slice(0, 12)) return { ok: false, reason: "cross_check_failed" };
  return { ok: true, identity: { slot: { kind: "compose", hostname, containerId }, crossCheck: "ok", execution } };
}

// ---- the write-once writer and reader --------------------------------------------------------------------------

export type BoundaryWriteFailure = IdentityRefusal | "durability_failed" | "exists";

export interface WriteBoundaryOptions extends LiveIdentityOptions {
  dataDir: string;
  workerToken: string;
  /** Defaults to this process's incarnation. */
  incarnation?: string;
  ops?: BoundaryFsOps;
}

/**
 * Write this process's boundary record (3.2 durability order): fsync the data root, create `boundaries/` and fsync
 * `.uzi-authority`, write+fsync a tmp file, link it into place refusing EEXIST then unlink the tmp, fsync
 * `boundaries/`, read the record back. No record is left visible half-written; a failed step returns a token and the
 * caller disables restore for the process.
 */
export async function writeBoundaryRecord(options: WriteBoundaryOptions):
  Promise<{ ok: true; record: BoundaryRecord } | { ok: false; reason: BoundaryWriteFailure }> {
  const live = await readLiveIdentity(options);
  if (!live.ok) return live;
  const incarnation = options.incarnation ?? processWorkerIncarnation();
  const record: BoundaryRecord = { version: 1, incarnation, slot: live.identity.slot, crossCheck: live.identity.crossCheck, execution: live.identity.execution };
  const ops = options.ops ?? realBoundaryOps();
  const authorityDir = path.join(options.dataDir, AUTHORITY_DIR);
  const dir = path.join(authorityDir, BOUNDARIES_DIR);
  const name = boundaryFileName(incarnation);
  const final = path.join(dir, name);
  const tmp = path.join(dir, `.${name}.tmp`);
  const sealed = sealBoundaryRecord(options.workerToken, record);
  let linked = false;
  try {
    await ops.syncDir(options.dataDir);
    await ops.mkdirPrivate(dir);
    await ops.syncDir(authorityDir);
    await ops.writeTmp(tmp, sealed);
    try { await ops.link(tmp, final); linked = true; }
    catch (error) {
      await ops.unlink(tmp).catch(() => undefined);
      return { ok: false, reason: (error as NodeJS.ErrnoException).code === "EEXIST" ? "exists" : "durability_failed" };
    }
    await ops.unlink(tmp);
    await ops.syncDir(dir);
    const back = await ops.readFile(final);
    if (back !== sealed) return { ok: false, reason: "durability_failed" };
  } catch {
    if (!linked) await ops.unlink(tmp).catch(() => undefined);
    return { ok: false, reason: "durability_failed" };
  }
  return { ok: true, record };
}

/** Read exactly `boundaries/<sha256(incarnation)>.json`; a record naming another incarnation is invalid. No fallback. */
export async function readBoundaryRecord(
  options: { dataDir: string; workerToken: string; ops?: BoundaryFsOps }, incarnation: string,
): Promise<BoundaryRead> {
  const read = await readBoundaryRecordRaw(options, incarnation);
  return read.state === "ok" ? { state: "ok", record: read.record } : { state: read.state };
}

/** {@link readBoundaryRecord} plus the sha256 (lowercase hex) of the record file's raw bytes, present exactly when the file was
 *  read (also for an `invalid` record, whose bytes are still what a restore basis would pin). */
export async function readBoundaryRecordRaw(
  options: { dataDir: string; workerToken: string; ops?: BoundaryFsOps }, incarnation: string,
): Promise<BoundaryRead & { digest?: string }> {
  const ops = options.ops ?? realBoundaryOps();
  let raw: string | null;
  try { raw = await ops.readFile(path.join(options.dataDir, AUTHORITY_DIR, BOUNDARIES_DIR, boundaryFileName(incarnation))); }
  catch { return { state: "invalid" }; }
  if (raw === null) return { state: "missing" };
  const digest = createHash("sha256").update(Buffer.from(raw, "utf8")).digest("hex");
  const record = unsealBoundaryRecord(options.workerToken, raw);
  return record && record.incarnation === incarnation ? { state: "ok", record, digest } : { state: "invalid", digest };
}

// ---- process-wide state (set once at startup; read by the runner) ----------------------------------------------

let processState: { record: BoundaryRecord } | { disabled: BoundaryWriteFailure | "not_attempted" } = { disabled: "not_attempted" };

/** Write this process's record and remember the outcome. Never throws: a failure disables restore for the process. */
export async function establishProcessBoundary(options: WriteBoundaryOptions): Promise<{ ok: true; record: BoundaryRecord } | { ok: false; reason: BoundaryWriteFailure }> {
  let result: Awaited<ReturnType<typeof writeBoundaryRecord>>;
  try { result = await writeBoundaryRecord(options); }
  catch { result = { ok: false, reason: "durability_failed" }; }
  processState = result.ok ? { record: result.record } : { disabled: result.reason };
  return result;
}

/** This process's written record, or undefined (restore is disabled for the process). */
export function processBoundary(): BoundaryRecord | undefined { return "record" in processState ? processState.record : undefined; }

/** Why this process has no record, or undefined when it has one. */
export function processBoundaryDisabledReason(): BoundaryWriteFailure | "not_attempted" | undefined {
  return "disabled" in processState ? processState.disabled : undefined;
}

export function resetProcessBoundaryForTests(): void { processState = { disabled: "not_attempted" }; }

// ---- the attribution census (bare config only; never touches a source tree) -------------------------------------

export type CensusRefusal = "attribution_unverifiable" | "census_foreign_active" | "census_pending";

export interface ConfigPair { key: string; value: string }

export type CensusPaths = { ok: true; paths: string[] } | { ok: false; reason: "attribution_unverifiable" };

const ATTRIBUTION_PREFIXES = ["uzi-recovery.", "uzi-restore.", "uzi-attempts.", "uzi-stopped.", "uzi-retained."];

class Unverifiable extends Error {}
const bad = (): never => { throw new Unverifiable("attribution_unverifiable"); };

function sourcePath(v: unknown): string {
  if (typeof v !== "string" || v === "" || v.includes("\0") || !path.isAbsolute(v) || path.resolve(v) !== v) return bad();
  return v;
}

/** Every path a decoded recovery journal names: clonePath, retainedSources[], recovery.source / successor and memoryPark.settlement.sources[]. */
function journalPaths(j: RecoveryJournalEntry, out: Set<string>): void {
  out.add(sourcePath(j.clonePath));
  for (const s of j.retainedSources ?? []) out.add(sourcePath(s.clonePath));
  if (j.recovery) {
    out.add(sourcePath(j.recovery.source.clonePath));
    if (j.recovery.successor) out.add(sourcePath(j.recovery.successor.clonePath));
  }
  for (const s of j.memoryPark?.settlement?.sources ?? []) out.add(sourcePath(s.path));
}

/**
 * Enumerate every attributed source path from the bound bare's raw config pairs. Each value is decoded with the SAME
 * strict parser production uses (git.ts), so the census can never accept a value a production reader would refuse. Any
 * key under an attribution prefix that is not one of the five known shapes, any value those parsers reject, or an
 * incomplete read is `attribution_unverifiable`. A `uzi-restore.<branch>.journal` or `uzi-recovery.<branch>.clone`
 * value of "" means cleared (no paths).
 */
export function enumerateAttributedPaths(pairs: readonly ConfigPair[], complete: boolean): CensusPaths {
  if (!complete) return { ok: false, reason: "attribution_unverifiable" };
  const out = new Set<string>();
  try {
    for (const { key, value } of pairs) {
      if (!ATTRIBUTION_PREFIXES.some(p => key.startsWith(p))) continue;
      let m: RegExpExecArray | null;
      if (/^uzi-recovery\..+\.clone$/.test(key)) {
        if (value === "") continue;
        journalPaths(parseRecoveryJournal(value), out);
      } else if (/^uzi-restore\..+\.journal$/.test(key)) {
        if (value === "") continue;
        if (parseRestoreValue(value).kind === "entry") {
          const e = parseRestoreJournal(value);
          out.add(sourcePath(e.basis.predecessor.clonePath));
          journalPaths(e.journal, out);
        } else {
          const t = parseRestoreTombstone(value);
          out.add(sourcePath(t.basis.predecessor.clonePath));
          out.add(sourcePath(t.successor.clonePath));
        }
      } else if (/^uzi-attempts\..+\.entry$/.test(key)) {
        const e = parseAttemptLedgerEntry(value);
        if (!e) bad();
        out.add(sourcePath(e!.clonePath));
      } else if ((m = /^uzi-stopped\.(.+)\.entry$/.exec(key))) {
        out.add(sourcePath(parseStoppedMarkerShape(m[1]!, value).clonePath));
      } else if ((m = /^uzi-retained\.([^.]+)\.journal$/.exec(key))) {
        journalPaths(parseRetainedDescriptorShape(m[1]!, value).journal, out);
      } else bad();
    }
  } catch {
    // A rejected value (Unverifiable or any production-parser error) is the same fail-closed verdict.
    return { ok: false, reason: "attribution_unverifiable" };
  }
  return { ok: true, paths: [...out].sort() };
}

export interface CensusInput {
  pairs: readonly ConfigPair[];
  /** False when the config read hit a cap or an error. */
  complete: boolean;
  selfIncarnation: string;
  /** P's own clonePath: the one slot excused from the foreign-active / pending checks. */
  predecessorClonePath: string;
  /** A read-only slot peek (the authority store's slot-key derivation is the only touch of the path). Throwing means unreadable. */
  peekSlot(clonePath: string): Promise<SlotSnapshot | null>;
}

export type CensusResult = { ok: true; paths: string[] } | { ok: false; reason: CensusRefusal };

/** Peek the slot of every attributed path (except P's own): foreign-active or a non-null pending footprint refuses. */
export async function runAttributionCensus(input: CensusInput): Promise<CensusResult> {
  const named = enumerateAttributedPaths(input.pairs, input.complete);
  if (!named.ok) return named;
  for (const p of named.paths) {
    if (p === input.predecessorClonePath) continue;
    let slot: SlotSnapshot | null;
    try { slot = await input.peekSlot(p); } catch { return { ok: false, reason: "attribution_unverifiable" }; }
    if (slot === null) continue;
    if (slot.pending !== null) return { ok: false, reason: "census_pending" };
    if (slot.state === "active" && slot.owner.worker !== input.selfIncarnation) return { ok: false, reason: "census_foreign_active" };
  }
  return named;
}

// ---- the predicate -----------------------------------------------------------------------------------------------

export type RestoreRefusal =
  | "self_record_invalid" | "predecessor_record_missing" | "predecessor_record_invalid" | "hosted_restore_not_delivered"
  | "slot_mismatch" | "cross_check_failed" | "same_execution" | "predecessor_not_older"
  | "procfs_hidden" | "predecessor_pending" | "predecessor_fenced" | "bare_busy" | "bare_slot_foreign_active" | CensusRefusal;

export interface RestoreVerdict {
  predecessorIncarnation: string;
  slot: BoundarySlot;
  boot: "same_boot" | "different_boot";
}

/** The predecessor classes this release cannot restore from, set only where the route decides one (plan 3.2), never mapped from a reason. */
export type RestoreUnsupported = "canonical" | "hosted" | "missing_record";

export type RestoreDecision = { admit: true; verdict: RestoreVerdict } | { admit: false; reason: RestoreRefusal; unsupported?: RestoreUnsupported };

export interface RestoreEvalInput {
  selfIncarnation: string;
  /** S's own record as read from disk, and S's live identity re-read now (undefined when it could not be). */
  selfRead: BoundaryRead;
  selfLive: LiveBoundaryIdentity | undefined;
  /** Why `readLiveIdentity` refused when selfLive is undefined; a hidden procfs is reported as its own reason. */
  selfLiveRefusal?: IdentityRefusal;
  /** P's slot snapshot (owner.worker, epoch, pending, publication, state) and whether P's journal carries a completionSourceFence. */
  predecessor: SlotSnapshot;
  predecessorFenced: boolean;
  /** Reads `boundaries/<sha256(incarnation)>`; the predicate calls it with P's owner worker only. */
  readBoundary(incarnation: string): Promise<BoundaryRead>;
  /** `assertBareClear` found a running pending footprint on the bound bare. */
  bareBusy: boolean;
  /** The bound bare's own slot (null: none). */
  bareSlot: SlotSnapshot | null;
  census: CensusResult;
  /** TEST-ONLY (issue #3644 PRD #3831 8.1 named unsafe variant): skips the R5 execution comparison. Production never sets it. */
  skipExecutionComparison?: boolean;
}

function sameSlot(a: BoundarySlot, b: BoundarySlot): boolean {
  if (a.kind === "compose" && b.kind === "compose") return a.hostname === b.hostname && a.containerId === b.containerId;
  if (a.kind === "hosted" && b.kind === "hosted") return a.podUid === b.podUid && a.container === b.container;
  return false;
}
function sameExecution(a: BoundaryExecution, b: BoundaryExecution): boolean {
  return a.nsDev === b.nsDev && a.nsIno === b.nsIno && a.initStartTicks === b.initStartTicks && a.bootId === b.bootId;
}

/** R1: S's record is valid and equals its live identity. Returns the record, or undefined when any part differs or could not be read.
 *  The one comparison both {@link evaluateComposeRestore} and the route's hosted classification use. */
export function selfRecordMatchingLive(
  selfRead: BoundaryRead, selfLive: LiveBoundaryIdentity | undefined, selfIncarnation: string,
): BoundaryRecord | undefined {
  if (selfRead.state !== "ok" || selfLive === undefined) return undefined;
  const s = selfRead.record;
  if (s.incarnation !== selfIncarnation || s.crossCheck !== selfLive.crossCheck ||
      !sameSlot(s.slot, selfLive.slot) || !sameExecution(s.execution, selfLive.execution)) return undefined;
  return s;
}

/** R1-R6, R8, R9 of PRD #3644 6(d), compose only. Reads exactly P's boundary file; every uncertainty refuses. */
export async function evaluateComposeRestore(input: RestoreEvalInput): Promise<RestoreDecision> {
  const refuse = (reason: RestoreRefusal, unsupported?: RestoreUnsupported): RestoreDecision =>
    ({ admit: false, reason, ...(unsupported === undefined ? {} : { unsupported }) });
  if (input.selfLiveRefusal === "procfs_hidden") return refuse("procfs_hidden");
  const s = selfRecordMatchingLive(input.selfRead, input.selfLive, input.selfIncarnation);
  if (s === undefined) return refuse("self_record_invalid");
  // R2/R3: exactly P's record, no fallback.
  const pInc = input.predecessor.owner.worker;
  const pRead = await input.readBoundary(pInc);
  if (pRead.state === "missing") return refuse("predecessor_record_missing", "missing_record");
  if (pRead.state !== "ok" || pRead.record.incarnation !== pInc) return refuse("predecessor_record_invalid");
  const p = pRead.record;
  // R4: same compose slot, both cross-checked.
  if (s.slot.kind === "hosted" || p.slot.kind === "hosted") return refuse("hosted_restore_not_delivered");
  if (!sameSlot(s.slot, p.slot)) return refuse("slot_mismatch");
  if (s.crossCheck !== "ok" || p.crossCheck !== "ok") return refuse("cross_check_failed");
  // R5: a distinct, strictly older execution.
  const sameBoot = s.execution.bootId === p.execution.bootId;
  if (sameBoot && input.skipExecutionComparison !== true) {
    if (s.execution.nsDev === p.execution.nsDev && s.execution.nsIno === p.execution.nsIno &&
        s.execution.initStartTicks === p.execution.initStartTicks) return refuse("same_execution");
    if (!(BigInt(p.execution.initStartTicks) < BigInt(s.execution.initStartTicks))) return refuse("predecessor_not_older");
  }
  // P is quiet: no pending footprint, no publication, no completion fence.
  if (input.predecessor.pending !== null || input.predecessor.publication !== null) return refuse("predecessor_pending");
  if (input.predecessorFenced) return refuse("predecessor_fenced");
  // R8: the bound bare, its own slot, and the complete attribution census.
  if (input.bareBusy) return refuse("bare_busy");
  const b = input.bareSlot;
  if (b !== null && b.state === "active" && b.owner.worker !== input.selfIncarnation) return refuse("bare_slot_foreign_active");
  if (!input.census.ok) return refuse(input.census.reason);
  return { admit: true, verdict: { predecessorIncarnation: pInc, slot: s.slot, boot: sameBoot ? "same_boot" : "different_boot" } };
}
