// Issue #4043 M1: the read-only building blocks of the persistent runner-scratch reclaim. Nothing
// here deletes, spawns or is wired into startup; every filesystem and /proc read goes through the
// injectable `ScratchIo` so tests use small fixtures. Refusals are fixed tokens, never free text.
import * as fs from "node:fs";
import fsp, { type FileHandle } from "node:fs/promises";
import path from "node:path";
import { RUNNER_UID, WORKER_UID } from "./runner-uid.js";
import { TREE_NAME_RE } from "./rmtree.js";

const SCRATCH_ROOT_SPLIT = "/data/runner/.tmp/uzi-runner";
const SCRATCH_ROOT_SINGLE = "/data/runner/.tmp";
const SPLIT_WORKER_TMPDIR = "/data/runner/.tmp/uzi-worker";
/** The two names the entrypoint creates under the pod TMPDIR; never removed in either mode. */
const PROTECTED_NAMES: ReadonlySet<string> = new Set(["uzi-worker", "uzi-runner"]);
const ACTIVE_HELPER_PREFIX = "uzi-quiesce-";
/** A top-level name the scratch reclaim never removes: the protected containers and any live quiesce
 * helper leaf. The census classifier and the delete launch validator both use this one rule. */
export const isNeverRemovedScratchName = (name: string): boolean =>
  PROTECTED_NAMES.has(name) || name.startsWith(ACTIVE_HELPER_PREFIX);
const POD_ENTRY = "/data/.uzi-pod-entry";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
/** `O_PATH` (Linux; absent from `fs.constants`), the same idiom as rmtree's PINNED_PRELUDE. */
const O_PATH = 0o10000000;
const PIN = O_PATH | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW;
const SELF_FD = "/proc/self/fd/";
/** Matches the exclusive-idle proof's own PID inventory cap (run-quiescence.ts scanExclusiveIdle). */
const MAX_PIDS = 128;
const MAX_FDS_PER_PID = 4096;
const MOUNTINFO_LIMIT = 256 * 1024;
const FS_EXT4 = 0xef53;
const FS_XFS = 0x58465342;

export type ScratchIo = typeof fs;

// ---- 1a: layout gate and root pin ---------------------------------------------------------------

export interface ScratchLayout {
  mode: "split" | "single";
  /** Literal root: a constant per mode, never derived from the environment. */
  root: string;
  ownerUid: number;
  /** Accepted permission bits of the root (low 12 bits). */
  modes: readonly number[];
}

/** The exact-layout gate. Anything but the two shipped layouts is `layout_refused`. */
export function scratchLayout(env: NodeJS.ProcessEnv): ScratchLayout | { refused: "layout_refused" } {
  if (env.UZI_UID_SPLIT === "1" && env.UZI_RUNNER_TMPDIR === SCRATCH_ROOT_SPLIT && env.TMPDIR === SPLIT_WORKER_TMPDIR)
    return { mode: "split", root: SCRATCH_ROOT_SPLIT, ownerUid: RUNNER_UID, modes: [0o700] };
  if (env.UZI_UID_SPLIT === undefined && env.UZI_RUNNER_TMPDIR === undefined && env.TMPDIR === SCRATCH_ROOT_SINGLE)
    return { mode: "single", root: SCRATCH_ROOT_SINGLE, ownerUid: WORKER_UID, modes: [0o755, 0o3775] };
  return { refused: "layout_refused" };
}

export interface ScratchRootPin {
  fd: number;
  /** Decimal strings of the bigint stat, the format PINNED_SUBTREE_SCRIPT compares `expect` in. */
  dev: string;
  ino: string;
  devIno: string;
  uid: number;
  close(): void;
}

/** Pins `/` then every component of the root through descriptors (O_PATH, O_DIRECTORY, O_NOFOLLOW),
 * each relative to the previous one, so a rename or symlink swap above cannot move the pin. Every
 * component must fstat as a directory; the root must have the layout's owner and mode. */
export function pinScratchRoot(layout: ScratchLayout, io: ScratchIo = fs): ScratchRootPin | { refused: "root_unpinned" | "root_identity" } {
  let fd: number | undefined;
  const drop = () => { if (fd !== undefined) { try { io.closeSync(fd); } catch { /* best effort */ } fd = undefined; } };
  try {
    fd = io.openSync("/", PIN);
    for (const part of layout.root.split("/").filter(Boolean)) {
      const next = io.openSync(`${SELF_FD}${fd}/${part}`, PIN);
      io.closeSync(fd);
      fd = next;
      if (!io.fstatSync(fd, { bigint: true }).isDirectory()) { drop(); return { refused: "root_unpinned" }; }
    }
    const st = io.fstatSync(fd, { bigint: true });
    if (!st.isDirectory()) { drop(); return { refused: "root_unpinned" }; }
    if (st.uid !== BigInt(layout.ownerUid) || !layout.modes.includes(Number(st.mode) & 0o7777)) {
      drop(); return { refused: "root_identity" };
    }
    const pinned = fd;
    return { fd: pinned, dev: String(st.dev), ino: String(st.ino), devIno: `${st.dev}:${st.ino}`, uid: layout.ownerUid,
      close: () => { try { io.closeSync(pinned); } catch { /* already closed */ } } };
  } catch {
    drop();
    return { refused: "root_unpinned" };
  }
}

/** True only when the root re-pins to exactly `expect` (dev:ino) with the same owner and mode rules. */
export function repinScratchRoot(layout: ScratchLayout, expect: string, io: ScratchIo = fs): boolean {
  const pin = pinScratchRoot(layout, io);
  if ("refused" in pin) return false;
  const same = pin.devIno === expect;
  pin.close();
  return same;
}

// ---- 1b v: pod-entry fence ------------------------------------------------------------------------

/** T0 is the pod-entry record's ctime minus 2 s. A leaf with any subtree ctime at or after it is
 * kept (`current_pod`). The record is read O_NOFOLLOW, must be a regular single-link file of at most
 * 128 ASCII bytes in the shape pod-epoch-init.cjs writes, and `current` must be this pod's UID. */
export function readPodEntryFence(podUid: string, io: ScratchIo = fs, file: string = POD_ENTRY): { t0Ms: number } | { refused: "pod_entry_invalid" } {
  const bad = { refused: "pod_entry_invalid" } as const;
  if (!UUID.test(podUid)) return bad;
  let fd: number | undefined;
  try {
    fd = io.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
    const before = io.fstatSync(fd, { bigint: true });
    if (!before.isFile() || before.nlink !== 1n || before.size > 128n) return bad;
    const bytes = Buffer.alloc(129);
    let length = 0;
    while (length < bytes.length) {
      const n = io.readSync(fd, bytes, length, bytes.length - length, length);
      if (!n) break;
      length += n;
    }
    const after = io.fstatSync(fd, { bigint: true });
    if (length > 128 || BigInt(length) !== before.size || after.size !== before.size || after.ino !== before.ino ||
        after.ctimeNs !== before.ctimeNs) return bad;
    if (bytes.subarray(0, length).some(byte => byte > 127)) return bad;
    const contents = bytes.toString("ascii", 0, length);
    let value: unknown;
    try { value = JSON.parse(contents); } catch { return bad; }
    if ((contents.match(/"(?:current|previous)"\s*:/g) ?? []).length !== 2) return bad;
    if (!value || typeof value !== "object" || Array.isArray(value) ||
        Object.keys(value).sort().join(",") !== "current,previous") return bad;
    const { current, previous } = value as { current: unknown; previous: unknown };
    if (typeof current !== "string" || !UUID.test(current) ||
        (previous !== null && (typeof previous !== "string" || !UUID.test(previous)))) return bad;
    if (current !== podUid) return bad;
    return { t0Ms: Number(after.ctimeMs) - 2000 };
  } catch {
    return bad;
  } finally {
    if (fd !== undefined) { try { io.closeSync(fd); } catch { /* best effort */ } }
  }
}

// ---- 1b vi: mount rules ---------------------------------------------------------------------------

/** Linux dev_t -> `major:minor` (the encoding of st_dev, as mountinfo prints it). */
export function devToMajorMinor(dev: bigint): string {
  const major = ((dev >> 8n) & 0xfffn) | ((dev >> 32n) & 0xfffff000n);
  const minor = (dev & 0xffn) | ((dev >> 12n) & 0xffffff00n);
  return `${major}:${minor}`;
}

interface MountEntry { id: string; parent: string; majorMinor: string; point: string }

function unescapeMountPoint(raw: string): string {
  return raw.replace(/\\([0-7]{3})/g, (_, octal: string) => String.fromCharCode(parseInt(octal, 8)));
}

function parseMountInfo(text: string): MountEntry[] | undefined {
  if (Buffer.byteLength(text) > MOUNTINFO_LIMIT) return undefined;
  const entries: MountEntry[] = [];
  for (const line of text.split("\n")) {
    if (line === "") continue;
    const halves = line.split(" - ");
    if (halves.length !== 2) return undefined;
    const left = halves[0]!.split(" "), right = halves[1]!.split(" ");
    if (left.length < 6 || left.some(field => field === "") || right.length !== 3 || right.some(field => field === "")) return undefined;
    if (!/^\d+$/.test(left[0]!) || !/^\d+$/.test(left[1]!) || !/^\d+:\d+$/.test(left[2]!)) return undefined;
    entries.push({ id: left[0]!, parent: left[1]!, majorMinor: left[2]!, point: unescapeMountPoint(left[4]!) });
  }
  return entries;
}

export interface ScratchMountInput {
  /** Text of /proc/self/mountinfo. */
  mountinfo: string;
  /** Text of /proc/self/fdinfo/<pinned root fd>. */
  fdinfo: string;
  /** st_dev of the pinned root, as a bigint. */
  rootDev: bigint;
  root: string;
  leafNames: readonly string[];
}

const SHARED_DAEMON_VIEW = "/data/runner";
const within = (parent: string, child: string): boolean => child === parent || (parent === "/" ? child.startsWith("/") : child.startsWith(parent + "/"));

/** Plan 1b vi rules 1 to 4 as a pure function. Rule 5 (re-reading before each delete) is the caller
 * calling this again with fresh text. Refusal tokens: an enclosing/ancestor topology other than the
 * shipped one is `mount_unexpected_ancestor`; anything unreadable, unparseable, over the cap or not
 * reconcilable is `mount_unknown`. A mount at or inside a leaf only keeps that leaf. */
export function evaluateScratchMounts(input: ScratchMountInput):
    { refused: "mount_unexpected_ancestor" | "mount_unknown" } | { mountedLeaves: Set<string> } {
  const unknown = { refused: "mount_unknown" } as const;
  const ancestor = { refused: "mount_unexpected_ancestor" } as const;
  if (Buffer.byteLength(input.fdinfo) > MOUNTINFO_LIMIT) return unknown;
  const ids = input.fdinfo.split("\n").filter(line => line.startsWith("mnt_id:"));
  const mntId = ids.length === 1 ? /^mnt_id:[ \t]+(\d+)$/.exec(ids[0]!)?.[1] : undefined;
  const mounts = parseMountInfo(input.mountinfo);
  if (mntId === undefined || mounts === undefined) return unknown;
  if (new Set(mounts.map(m => m.id)).size !== mounts.length) return unknown;
  const enclosing = mounts.find(m => m.id === mntId);
  if (!enclosing) return unknown;
  // Rule 1: the enclosing mount is the single mount at /data/runner and is the root's own device.
  if (enclosing.point !== "/data/runner" || mounts.filter(m => m.point === "/data/runner").length !== 1) return ancestor;
  if (enclosing.majorMinor !== devToMajorMinor(input.rootDev)) return unknown;
  // Rule 2: proper-ancestor mount points may only be /, /data and /data/runner.
  const allowed = new Set(["/", "/data", "/data/runner"]);
  for (const m of mounts) if (m.point !== input.root && within(m.point, input.root) && !allowed.has(m.point)) return ancestor;
  const data = mounts.filter(m => m.point === "/data");
  if (data.length !== 1 || data[0]!.id !== enclosing.parent || data[0]!.majorMinor !== enclosing.majorMinor) return ancestor;
  // Rule 3: a mount at or inside a leaf keeps that leaf; one elsewhere inside the root is unknown.
  const mountedLeaves = new Set<string>();
  const leaves = new Set(input.leafNames);
  for (const m of mounts) {
    if (!within(input.root, m.point)) continue;
    // Rule 1 puts the enclosing mount at /data/runner, so a mount AT the root is a shadow of it.
    if (m.point === input.root) return unknown;
    const leaf = m.point.slice(input.root === "/" ? 1 : input.root.length + 1).split("/")[0]!;
    if (leaves.has(leaf)) mountedLeaves.add(leaf); else return unknown;
  }
  return { mountedLeaves };
}

/** Bounded read of a /proc text file (these report size 0): `undefined` over `limit` bytes or on any error. */
function readBoundedText(io: ScratchIo, file: string, limit: number): string | undefined {
  let fd: number | undefined;
  try {
    fd = io.openSync(file, fs.constants.O_RDONLY);
    const bytes = Buffer.alloc(limit + 1);
    let length = 0;
    for (;;) {
      const n = io.readSync(fd, bytes, length, bytes.length - length, null);
      if (!n) break;
      length += n;
      if (length > limit) return undefined;
    }
    return bytes.toString("utf8", 0, length);
  } catch {
    return undefined;
  } finally {
    if (fd !== undefined) { try { io.closeSync(fd); } catch { /* best effort */ } }
  }
}

/** Reads both inputs of {@link evaluateScratchMounts} fresh; call again before each delete. */
export function readScratchMounts(pin: ScratchRootPin, leafNames: readonly string[], root: string, io: ScratchIo = fs):
    ReturnType<typeof evaluateScratchMounts> {
  const mountinfo = readBoundedText(io, "/proc/self/mountinfo", MOUNTINFO_LIMIT);
  const fdinfo = readBoundedText(io, `/proc/self/fdinfo/${pin.fd}`, MOUNTINFO_LIMIT);
  if (mountinfo === undefined || fdinfo === undefined) return { refused: "mount_unknown" };
  return evaluateScratchMounts({ mountinfo, fdinfo, rootDev: BigInt(pin.dev), root, leafNames });
}

// ---- 1e: supported filesystem ---------------------------------------------------------------------

export function checkScratchFilesystem(statfsType: number | bigint): "ok" | "fs_unsupported" {
  const type = Number(statfsType);
  return type === FS_EXT4 || type === FS_XFS ? "ok" : "fs_unsupported";
}

/** statfs of the pinned root through its descriptor link; a failure is `fs_unsupported`. */
export function scratchFilesystemOf(pin: ScratchRootPin, io: ScratchIo = fs): "ok" | "fs_unsupported" {
  try { return checkScratchFilesystem(io.statfsSync(`${SELF_FD}${pin.fd}`).type); } catch { return "fs_unsupported"; }
}

// ---- 1b i/ii: processes ---------------------------------------------------------------------------

const vanished = (e: unknown): boolean => {
  const code = (e as NodeJS.ErrnoException | undefined)?.code;
  return code === "ENOENT" || code === "ESRCH";
};

const RUNNER_PREFIX = "/data/runner/";
const SHARED_PREFIX = "/data/runner-shared/";

/** The worker container mounts the whole data PVC at /data and its runner-shared subPath at
 * /data/runner, so the same scratch inodes are also reachable under /data/runner-shared. */
function rootsWithAlias(root: string): string[] {
  return root.startsWith(RUNNER_PREFIX) ? [root, SHARED_PREFIX + root.slice(RUNNER_PREFIX.length)] : [root];
}

function allWorkerIds(status: string, label: "Uid" | "Gid"): boolean {
  const line = status.split("\n").find(l => l.startsWith(label + ":"));
  if (!line) return false;
  const ids = line.slice(label.length + 1).trim().split(/\s+/);
  return ids.length === 4 && ids.every(id => id === String(WORKER_UID));
}

/** Every visible process must be the worker uid/gid (a vanished pid is skipped, anything unreadable
 * is `not_idle`), and its cwd and every fd link are read: a link equal to or inside `<root>/<leaf>`
 * puts that leaf into `held` (kept `held_open`); an unreadable link refuses. Under the exclusive-idle
 * proof (run separately) only exempt processes exist, so every visible process is the exempt set. */
export function scanScratchProcesses(root: string, leafNames: readonly string[], io: ScratchIo = fs, proc = "/proc"):
    { refused: "not_idle" | "self_descriptor_unreadable" } | { held: Set<string> } {
  let pids: number[];
  try {
    pids = io.readdirSync(proc).filter(n => /^[0-9]+$/.test(n)).map(Number);
  } catch { return { refused: "not_idle" }; }
  if (pids.length > MAX_PIDS) return { refused: "not_idle" };
  const held = new Set<string>();
  const leaves = rootsWithAlias(root).flatMap(r => leafNames.map(name => [name, path.posix.join(r, name)] as const));
  const note = (target: string) => {
    const link = target.replace(/ \(deleted\)$/, "");
    for (const [name, full] of leaves) if (link === full || link.startsWith(full + "/")) held.add(name);
  };
  for (const pid of pids) {
    let status: string;
    try { status = io.readFileSync(`${proc}/${pid}/status`, "utf8"); }
    catch (e) { if (vanished(e)) continue; return { refused: "not_idle" }; }
    if (!allWorkerIds(status, "Uid") || !allWorkerIds(status, "Gid")) return { refused: "not_idle" };
    try { note(io.readlinkSync(`${proc}/${pid}/cwd`)); }
    catch (e) { if (vanished(e)) continue; return { refused: "self_descriptor_unreadable" }; }
    let fds: string[];
    try { fds = io.readdirSync(`${proc}/${pid}/fd`); }
    catch (e) { if (vanished(e)) continue; return { refused: "self_descriptor_unreadable" }; }
    if (fds.length > MAX_FDS_PER_PID) return { refused: "self_descriptor_unreadable" };
    for (const fd of fds) {
      try { note(io.readlinkSync(`${proc}/${pid}/fd/${fd}`)); }
      catch (e) { if (!vanished(e)) return { refused: "self_descriptor_unreadable" }; }
    }
  }
  return { held };
}

// ---- 1b vii: bind references ----------------------------------------------------------------------

type LstatLike = { isDirectory(): boolean; isSymbolicLink(): boolean };

/** Classifies the daemon's bind sources against the root. Normal form is absolute, `normalize`d and
 * without a trailing slash. A source at the root or above it refuses the phase (`bind_ancestor`); one
 * inside a leaf keeps that leaf (`bind_referenced`). `bind_unknown` (a phase refusal): a source outside
 * /data/runner (the daemon resolves it in a filesystem the worker cannot see), a symlink component at
 * or below /data/runner, or a first component under the root that is a file or missing. Other sources
 * inside /data/runner but outside the root are unrelated. */
export function classifyBindSources(input: {
  sources: readonly string[]; root: string;
  lstat?: (p: string) => LstatLike | undefined;
  /** The only path the worker and the daemon see alike (the runner-shared subPath both mount at
   * /data/runner). Test seam; defaults to that literal. */
  shared?: string;
}): { refused: "bind_unknown" | "bind_ancestor" } | { referenced: Set<string> } {
  const shared = input.shared ?? SHARED_DAEMON_VIEW;
  const lstat = input.lstat ?? ((p: string) => {
    try { return fs.lstatSync(p); } catch (e) { if (vanished(e)) return undefined; throw e; }
  });
  const referenced = new Set<string>();
  let ancestor = false;
  for (const source of input.sources) {
    if (!path.posix.isAbsolute(source) || path.posix.normalize(source) !== source || (source.length > 1 && source.endsWith("/")))
      return { refused: "bind_unknown" };
    if (within(source, input.root)) { ancestor = true; continue; }
    // Docker resolves a bind source in the daemon's own filesystem. Outside the shared subPath that is
    // the dind rootfs, which the worker cannot see (a symlink there could name any leaf): unknown.
    if (!within(shared, source)) return { refused: "bind_unknown" };
    // Inside the shared tree both views agree: no component at or below it may be a symlink, since
    // one would let the source resolve into a leaf its literal path does not name. A missing
    // component ends the walk (nothing below it exists); the literal path then decides.
    let prefix = shared;
    let st: LstatLike | undefined;
    try { st = lstat(prefix); } catch { return { refused: "bind_unknown" }; }
    if (!st || st.isSymbolicLink() || !st.isDirectory()) return { refused: "bind_unknown" };
    for (const part of source.slice(shared.length).split("/").filter(Boolean)) {
      prefix += "/" + part;
      let component: LstatLike | undefined;
      try { component = lstat(prefix); } catch { return { refused: "bind_unknown" }; }
      if (!component) break;
      if (component.isSymbolicLink()) return { refused: "bind_unknown" };
    }
    if (!source.startsWith(input.root + "/")) continue;
    const leaf = source.slice(input.root.length + 1).split("/")[0]!;
    try { st = lstat(path.posix.join(input.root, leaf)); } catch { return { refused: "bind_unknown" }; }
    if (!st || st.isSymbolicLink() || !st.isDirectory()) return { refused: "bind_unknown" };
    referenced.add(leaf);
  }
  return ancestor ? { refused: "bind_ancestor" } : { referenced };
}

// ---- 1c: protected recovery records ---------------------------------------------------------------

/** Leaves named by a recorded absolute path: the path (after `path.resolve`) equals or lies inside
 * `<root>/<leaf>`, compared component-wise (never by string prefix). */
export function protectedLeaves(paths: Iterable<string>, root: string, leafNames: readonly string[]): Set<string> {
  const leaves = new Set(leafNames);
  const kept = new Set<string>();
  const roots = rootsWithAlias(root);
  for (const recorded of paths) {
    if (!path.isAbsolute(recorded)) continue;
    const resolved = path.resolve(recorded);
    for (const r of roots) {
      if (!resolved.startsWith(r + "/")) continue;
      const leaf = resolved.slice(r.length + 1).split("/")[0]!;
      if (leaves.has(leaf)) kept.add(leaf);
    }
  }
  return kept;
}

export type ProtectedPathsRead =
  | { state: "read"; paths: string[] }
  | { state: "unknown" | "malformed"; reason: string };

const MAX_NODES = 2_000_000;
const MAX_PATHS = 65_536;
const MAX_JSON_DEPTH = 64;
const MAX_PATH_LENGTH = 4096;
export interface PathSink { paths: Set<string>; nodes: number }
export const newPathSink = (): PathSink => ({ paths: new Set(), nodes: 0 });

function addPath(s: string, sink: PathSink): boolean {
  if (!s.startsWith("/")) return true;
  if (s.length > MAX_PATH_LENGTH) return false;
  sink.paths.add(s);
  return sink.paths.size <= MAX_PATHS;
}

/** Iterative, bounded walk of a parsed JSON value: every absolute-path string value and object key is
 * collected. False on a node, depth, path-count or path-length overflow (the caller refuses). */
export function absorbJsonPaths(value: unknown, sink: PathSink): boolean {
  const stack: Array<[unknown, number]> = [[value, 0]];
  while (stack.length > 0) {
    const [v, depth] = stack.pop()!;
    if (++sink.nodes > MAX_NODES) return false;
    if (typeof v === "string") { if (!addPath(v, sink)) return false; }
    else if (Array.isArray(v)) {
      if (depth >= MAX_JSON_DEPTH) return false;
      for (const item of v) stack.push([item, depth + 1]);
    } else if (v !== null && typeof v === "object") {
      if (depth >= MAX_JSON_DEPTH) return false;
      for (const [key, item] of Object.entries(v)) {
        if (!addPath(key, sink)) return false;
        stack.push([item, depth + 1]);
      }
    }
  }
  return true;
}

const PROTECTED_FAMILY = /^uzi-(?:recovery|retained|attempts|stopped|recoveryowner)\./;

/** One `uzi-*` bare config value: JSON is walked, a non-JSON absolute path counts as a path, an empty
 * value (a cleared journal) is nothing. Any other value is `unknown` for the protected record
 * families (it could be a record this reader cannot interpret) and ignored for other `uzi-*` keys. */
export function absorbBareConfigValue(key: string, value: string, sink: PathSink): "ok" | "unknown" {
  if (value === "") return "ok";
  let parsed: unknown, json = true;
  try { parsed = JSON.parse(value); } catch { json = false; }
  if (json) return absorbJsonPaths(parsed, sink) ? "ok" : "unknown";
  if (value.startsWith("/")) return addPath(value, sink) ? "ok" : "unknown";
  return PROTECTED_FAMILY.test(key) ? "unknown" : "ok";
}

/** Entries, per-file bytes, total bytes and depth the store walk may spend. */
const STORE_LIMITS = { entries: 16_384, fileBytes: 1024 * 1024, totalBytes: 64 * 1024 * 1024, depth: 8 } as const;
const STORE_UUID = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";
/** The exact binary artifacts the stores hold: recovery.ts `<captureId>.bundle` and
 * `<captureId>.<uuid>.bundle.tmp`, quarantine-archive.ts `g<n>.bundle`, `.g<n>.<hex>.bundle.tmp` and
 * `.g<n>.<hex>.publish.tmp`. Everything else must parse as JSON. */
const STORE_BINARY = new RegExp(
  `^(?:(?:maintenance-v1-)?${STORE_UUID}(?:\\.${STORE_UUID}\\.bundle\\.tmp|\\.bundle)|g\\d+\\.bundle|\\.g\\d+\\.[0-9a-f]{12}\\.(?:bundle|publish)\\.tmp)$`);

class StoreStop extends Error {
  constructor(readonly state: "unknown" | "malformed", readonly reason: string) { super(reason); }
}

/** Bounded fd-relative read of the recovery stores (`recovery`, `recovery-maintenance`,
 * `recovery-settlement`, `recovery-archive`): a missing store is empty, a symlink or special file is
 * `unknown`, a file that does not parse as JSON is `malformed`, and any cap is `unknown`. */
export async function readProtectedStores(roots: readonly string[], opts: { interrupt?: () => void } = {}): Promise<ProtectedPathsRead> {
  const sink = newPathSink();
  const spent = { entries: 0, bytes: 0 };
  const walk = async (dir: FileHandle, depth: number): Promise<void> => {
    if (depth > STORE_LIMITS.depth) throw new StoreStop("unknown", "store_depth");
    const handle = await fsp.opendir(`${SELF_FD}${dir.fd}`, { bufferSize: 64 });
    try {
      for (;;) {
        opts.interrupt?.();
        const ent = await handle.read();
        if (!ent) return;
        if (++spent.entries > STORE_LIMITS.entries) throw new StoreStop("unknown", "store_entry_cap");
        if (ent.isFile() && STORE_BINARY.test(ent.name)) continue;
        let child: FileHandle;
        try {
          child = await fsp.open(`${SELF_FD}${dir.fd}/${ent.name}`,
            fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
        } catch { throw new StoreStop("unknown", "store_unreadable"); }
        try {
          const st = await child.stat();
          if (st.isDirectory()) await walk(child, depth + 1);
          else if (st.isFile()) await absorbFile(child, st.size);
          else throw new StoreStop("unknown", "store_special_file");
        } finally { await child.close().catch(() => undefined); }
      }
    } finally { await handle.close().catch(() => undefined); }
  };
  const absorbFile = async (file: FileHandle, size: number): Promise<void> => {
    if (size > STORE_LIMITS.fileBytes) throw new StoreStop("unknown", "store_file_cap");
    spent.bytes += size;
    if (spent.bytes > STORE_LIMITS.totalBytes) throw new StoreStop("unknown", "store_byte_cap");
    const bytes = Buffer.alloc(size + 1);
    let length = 0;
    for (;;) {
      const { bytesRead } = await file.read(bytes, length, bytes.length - length, length);
      if (!bytesRead) break;
      length += bytesRead;
      if (length > size) break;
    }
    if (length !== size) throw new StoreStop("unknown", "store_changed");
    let value: unknown;
    try { value = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes.subarray(0, length))); }
    catch { throw new StoreStop("malformed", "store_file_malformed"); }
    if (!absorbJsonPaths(value, sink)) throw new StoreStop("unknown", "store_path_cap");
  };
  try {
    for (const root of roots) {
      let top: FileHandle;
      try { top = await fsp.open(root, fs.constants.O_RDONLY | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW); }
      catch (e) {
        if ((e as NodeJS.ErrnoException).code === "ENOENT") continue;
        throw new StoreStop("unknown", "store_unreadable");
      }
      try { await walk(top, 0); } finally { await top.close().catch(() => undefined); }
    }
  } catch (e) {
    if (e instanceof StoreStop) return { state: e.state, reason: e.reason };
    return { state: "unknown", reason: "store_unreadable" };
  }
  return { state: "read", paths: [...sink.paths].sort() };
}

// ---- 1d 4-5: census parsing and classification --------------------------------------------------

export interface CensusEntry {
  name: string;
  type: "dir" | "other" | "absent" | "unreadable";
  devIno: string;
  uid: number;
  fsDiffers: boolean;
  foreignUid: boolean;
  bytes: number;
  sharedBytes: number;
  maxCtimeMs: number;
  incomplete: boolean;
}
export interface ParsedCensus { rootDevIno: string; rootUid: number; entries: CensusEntry[]; overflow: boolean }
export type CensusRefusal = "census_identity" | "census_failed" | "census_malformed";

const isCount = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 0;
const isDecimal = (v: unknown): v is string => typeof v === "string" && /^\d{1,20}$/.test(v);

/** Parses the census helper's output. The root identity must be present and equal `rootExpect`; the
 * listing is trusted only with its closing `end` line and a zero exit. Exit 3 is an identity refusal. */
export function parseScratchCensus(result: { code: number; stdout: string }, rootExpect: string):
    ParsedCensus | { refused: CensusRefusal } {
  if (result.code === 3) return { refused: "census_identity" };
  if (result.code !== 0) return { refused: "census_failed" };
  const bad = { refused: "census_malformed" } as const;
  const lines = result.stdout.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  if (lines.length < 2) return bad;
  const parsed: Array<Record<string, unknown>> = [];
  for (const line of lines) {
    let value: unknown;
    try { value = JSON.parse(line); } catch { return bad; }
    if (value === null || typeof value !== "object" || Array.isArray(value)) return bad;
    parsed.push(value as Record<string, unknown>);
  }
  const head = parsed[0]!, tail = parsed[parsed.length - 1]!;
  if (typeof head.root !== "string" || !isCount(head.uid)) return bad;
  if (head.root !== rootExpect) return { refused: "census_identity" };
  if (tail.end !== true || !isCount(tail.entries) || typeof tail.overflow !== "boolean") return bad;
  const entries: CensusEntry[] = [];
  const names = new Set<string>();
  for (const item of parsed.slice(1, -1)) {
    const { name, type } = item;
    if (typeof name !== "string" || name === "" || name.includes("/") || name.includes("\0") || names.has(name)) return bad;
    if (type !== "dir" && type !== "other" && type !== "absent" && type !== "unreadable") return bad;
    names.add(name);
    if (type === "absent" || type === "unreadable") {
      entries.push({ name, type, devIno: "", uid: -1, fsDiffers: false, foreignUid: false, bytes: 0, sharedBytes: 0, maxCtimeMs: 0,
        incomplete: type === "unreadable" });
      continue;
    }
    if (!isDecimal(item.dev) || !isDecimal(item.ino) || typeof item.uid !== "number" || !Number.isSafeInteger(item.uid) ||
        typeof item.fsdiff !== "boolean" || typeof item.fuid !== "boolean" || !isCount(item.bytes) ||
        !isCount(item.shared) || !isCount(item.ctime) || typeof item.inc !== "boolean") return bad;
    entries.push({ name, type, devIno: `${item.dev}:${item.ino}`, uid: item.uid, fsDiffers: item.fsdiff, foreignUid: item.fuid,
      bytes: item.bytes, sharedBytes: item.shared, maxCtimeMs: item.ctime, incomplete: item.inc });
  }
  if (tail.entries !== entries.length) return bad;
  return { rootDevIno: head.root, rootUid: head.uid, entries, overflow: tail.overflow };
}

/** Fixed-token kept reasons, in precedence order: the first that applies is the one counted. */
export const KEPT_REASONS = ["protected_name", "not_directory", "name_refused", "active_helper", "foreign_owner", "mount",
  "incomplete", "current_pod", "protected_record", "bind_referenced", "held_open", "cap"] as const;
export type KeptReason = (typeof KEPT_REASONS)[number];
export const MAX_ADMITTED_LEAVES = 512;

export interface AdmittedLeaf { name: string; devIno: string; bytes: number; sharedBytes: number; maxCtimeMs: number }
export interface ClassifiedCensus {
  examined: number;
  admitted: AdmittedLeaf[];
  kept: Record<KeptReason, number>;
  eligibleBytes: number;
  sharedBytes: number;
}

export function classifyScratchCensus(input: {
  census: ParsedCensus;
  /** T0 from {@link readPodEntryFence}. */
  t0Ms: number;
  mountedLeaves: ReadonlySet<string>;
  boundLeaves: ReadonlySet<string>;
  protectedLeaves: ReadonlySet<string>;
  heldLeaves: ReadonlySet<string>;
}): ClassifiedCensus {
  const kept = Object.fromEntries(KEPT_REASONS.map(r => [r, 0])) as Record<KeptReason, number>;
  const admitted: AdmittedLeaf[] = [];
  const reasonOf = (e: CensusEntry): KeptReason | undefined => {
    if (PROTECTED_NAMES.has(e.name)) return "protected_name";
    if (e.type !== "dir") return "not_directory";
    if (!TREE_NAME_RE.test(e.name)) return "name_refused";
    if (e.name.startsWith(ACTIVE_HELPER_PREFIX)) return "active_helper";
    if (e.uid !== input.census.rootUid || e.foreignUid) return "foreign_owner";
    if (e.fsDiffers || input.mountedLeaves.has(e.name)) return "mount";
    if (e.incomplete) return "incomplete";
    if (e.maxCtimeMs >= input.t0Ms) return "current_pod";
    if (input.protectedLeaves.has(e.name)) return "protected_record";
    if (input.boundLeaves.has(e.name)) return "bind_referenced";
    if (input.heldLeaves.has(e.name)) return "held_open";
    return undefined;
  };
  // Name order makes the 512 cut deterministic.
  for (const e of [...input.census.entries].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))) {
    const reason = reasonOf(e);
    if (reason) kept[reason]++;
    else if (admitted.length >= MAX_ADMITTED_LEAVES) kept.cap++;
    else admitted.push({ name: e.name, devIno: e.devIno, bytes: e.bytes, sharedBytes: e.sharedBytes, maxCtimeMs: e.maxCtimeMs });
  }
  return { examined: input.census.entries.length, admitted, kept,
    eligibleBytes: admitted.reduce((n, l) => n + l.bytes, 0), sharedBytes: admitted.reduce((n, l) => n + l.sharedBytes, 0) };
}

/** The pre-delete re-census must see the admitted leaf unchanged: identity, bytes, ctime, mount
 * flag, uid and completeness. Anything else is `changed`. */
export function censusEntryUnchanged(before: AdmittedLeaf, after: CensusEntry | undefined, rootUid: number): boolean {
  return after !== undefined && after.type === "dir" && after.devIno === before.devIno && after.bytes === before.bytes &&
    after.sharedBytes === before.sharedBytes && after.maxCtimeMs === before.maxCtimeMs && !after.fsDiffers &&
    !after.foreignUid && after.uid === rootUid && !after.incomplete;
}

// ---- 1e: settlement classifier --------------------------------------------------------------------

export type PostCensus =
  | { state: "absent" }
  | { state: "same"; bytes: number; complete: boolean }
  | { state: "other" }
  | { state: "unreadable" };

export interface SettlementInput {
  joined: boolean;
  spawned: boolean;
  code: number | null;
  signal: string | null;
  stdout: string;
  /** The admitted leaf's dev:ino. */
  expect: string;
  rootRepinOk: boolean;
  mountsOk: boolean;
  postCensus: PostCensus;
  /** Allocated bytes the pre-delete re-census revalidated. */
  bytesBefore: number;
}

/** Maps a single-leaf census entry to the settlement input. A missing entry or one the helper could
 * not lstat (`unreadable`) is never read as "another inode". */
export function postCensusOf(entry: CensusEntry | undefined, expect: string): PostCensus {
  if (!entry || entry.type === "unreadable") return { state: "unreadable" };
  if (entry.type === "absent") return { state: "absent" };
  if (entry.type === "dir" && entry.devIno === expect) return { state: "same", bytes: entry.bytes, complete: !entry.incomplete };
  return { state: "other" };
}

export type Settlement =
  | { outcome: "removed"; bytes: number }
  | { outcome: "residue"; bytesLowerBound: number; kind: "changed" | "helper_failed" | "retained"; stopPhase: boolean }
  | { outcome: "unresolved" };

/** The plan 1e table. A receipt (exit 0, exactly `UZI-SCRATCH-REMOVED <expect>`, one trailing newline
 * allowed) settles `removed` only when the name no longer pins `expect`; a missing receipt settles
 * `residue` only when the name still pins `expect` with a complete re-census and the helper wrote
 * nothing to stdout (its only stdout write is the receipt, so any other text is a malformed or
 * mismatched receipt). Everything else, including a receipt next to a name that still pins `expect`,
 * is unresolved. */
export function classifyScratchSettlement(input: SettlementInput): Settlement {
  const unresolved: Settlement = { outcome: "unresolved" };
  if (!input.joined || !input.rootRepinOk || !input.mountsOk) return unresolved;
  const receipt = `UZI-SCRATCH-REMOVED ${input.expect}`;
  const gotReceipt = input.spawned && input.code === 0 && (input.stdout === receipt || input.stdout === receipt + "\n");
  const post = input.postCensus;
  if (gotReceipt) {
    return post.state === "absent" || post.state === "other" ? { outcome: "removed", bytes: input.bytesBefore } : unresolved;
  }
  if (post.state !== "same" || !post.complete || input.stdout !== "") return unresolved;
  const kind = !input.spawned || input.code === null ? "helper_failed"
    : input.code === 2 || input.code === 3 ? "changed"
      : input.code === 4 || input.code === 8 || input.code === 9 ? "retained" : "helper_failed";
  return { outcome: "residue", bytesLowerBound: Math.max(0, input.bytesBefore - post.bytes), kind, stopPhase: kind !== "retained" };
}
