// issue #3644 (plan 3.12): the retained-home marker. An independent restore leaves the predecessor P's legacy HOME
// (`agent-home/<runId>`) and provision dir (`provision/<runId>`) untouched; this marker, `<dataDir>/retained-homes/<runId>.json`,
// is what keeps the disk reclaimers off them until P's own guarded discard removes it. The restore seed's
// `beforeRestoreAttribution` callback must write it durably before any bare write of the restore (the runner's `restoreFromPin`
// wires it; the retained-source server wires the discard's `hasRetainedHomeMarker` / `removeRetainedHomeMarker` hooks). A marker that is present, malformed, unreadable or whose directory fails with anything but ENOENT counts
// as PRESENT for every reclaimer: when in doubt the directories are protected, never reclaimed.

import { constants as fsc } from "node:fs";
import * as fsp from "node:fs/promises";
import path from "node:path";
import { readRegular } from "./completion-authority.js";
import type { RetainedHomeBinding } from "./git.js";
import { RUN_ID_RE } from "./util.js";

const RETAINED_HOMES_DIR = "retained-homes";
/** A marker is a few hundred bytes; anything larger is not one. */
const MARKER_LIMIT = 8192;

export interface RetainedHomeMarker {
  version: 1;
  runId: string;
  /** The restored predecessor's generation and attempt id. */
  predecessorGeneration: number;
  predecessorAttemptId: string;
  /** The bare repository the restore ran in; the exit's marker checks bind to it (git.ts `RetainedHomeBinding`). */
  barePath: string;
}

export type RetainedHomeRead =
  | { kind: "absent" }
  | { kind: "marker"; marker: RetainedHomeMarker }
  /** Present but not a valid marker for this run, or not readable: still protects the directories. */
  | { kind: "unreadable"; reason: string };

/** The filesystem operations the marker uses; injectable so a test can fail or record each durability step. */
export interface RetainedHomeFs {
  /** Create the directory with mode 0700; an existing directory is accepted. Must refuse a symlink or a non-directory. */
  mkdir(dir: string): Promise<void>;
  /** fsync a directory. */
  syncDir(dir: string): Promise<void>;
  /** O_CREAT|O_EXCL|O_NOFOLLOW write of `data` with mode 0600, then fsync of the file. Rejects EEXIST when the name exists. */
  createExclusive(file: string, data: string): Promise<void>;
  /** fsync an existing regular file. */
  syncFile(file: string): Promise<void>;
  /** The file's contents; null when the name does not exist. Rejects when it is not a safe regular file. */
  read(file: string): Promise<string | null>;
  /** Remove the name; a missing name is fine. */
  unlink(file: string): Promise<void>;
}

export class RetainedHomeMarkerRefusal extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RetainedHomeMarkerRefusal";
  }
}

const errCode = (e: unknown): string | undefined => (e as NodeJS.ErrnoException | undefined)?.code;

async function syncPath(target: string, flags: number): Promise<void> {
  const handle = await fsp.open(target, flags);
  try { await handle.sync(); } finally { await handle.close(); }
}

export const defaultRetainedHomeFs: RetainedHomeFs = {
  async mkdir(dir) {
    try { await fsp.mkdir(dir, { mode: 0o700 }); } catch (e) { if (errCode(e) !== "EEXIST") throw e; }
    const st = await fsp.lstat(dir);
    if (!st.isDirectory() || st.isSymbolicLink()) throw new RetainedHomeMarkerRefusal("the retained-homes path is not a directory");
    if (process.getuid && st.uid !== process.getuid()) throw new RetainedHomeMarkerRefusal("the retained-homes directory is not owned by this uid");
  },
  syncDir: (dir) => syncPath(dir, fsc.O_RDONLY | fsc.O_DIRECTORY),
  async createExclusive(file, data) {
    const handle = await fsp.open(file, fsc.O_WRONLY | fsc.O_CREAT | fsc.O_EXCL | fsc.O_NOFOLLOW, 0o600);
    try {
      await handle.chmod(0o600);
      await handle.writeFile(data, "utf8");
      await handle.sync();
    } finally {
      await handle.close();
    }
  },
  syncFile: (file) => syncPath(file, fsc.O_RDONLY | fsc.O_NOFOLLOW | fsc.O_NONBLOCK),
  read: (file) => readRegular(file, process.getuid?.() ?? 0, MARKER_LIMIT),
  async unlink(file) {
    try { await fsp.unlink(file); } catch (e) { if (errCode(e) !== "ENOENT") throw e; }
  },
};

function markerPaths(dataDir: string, runId: string): { dir: string; file: string } {
  if (!RUN_ID_RE.test(runId)) throw new RetainedHomeMarkerRefusal("the retained-home marker run id is not a UUID");
  const dir = path.join(dataDir, RETAINED_HOMES_DIR);
  return { dir, file: path.join(dir, `${runId}.json`) };
}

const serialize = (marker: RetainedHomeMarker): string =>
  JSON.stringify({
    version: marker.version, runId: marker.runId, predecessorGeneration: marker.predecessorGeneration,
    predecessorAttemptId: marker.predecessorAttemptId, barePath: marker.barePath,
  });

function parseMarker(raw: string, runId: string): RetainedHomeMarker | string {
  let v: unknown;
  try { v = JSON.parse(raw); } catch { return "not JSON"; }
  if (typeof v !== "object" || v === null || Array.isArray(v)) return "not an object";
  const o = v as Record<string, unknown>;
  if (Object.keys(o).sort().join(",") !== "barePath,predecessorAttemptId,predecessorGeneration,runId,version") return "unexpected fields";
  if (o["version"] !== 1) return "unknown version";
  if (o["runId"] !== runId) return "names another run";
  const gen = o["predecessorGeneration"];
  if (typeof gen !== "number" || !Number.isSafeInteger(gen) || gen < 1) return "bad predecessor generation";
  const attempt = o["predecessorAttemptId"];
  if (typeof attempt !== "string" || attempt === "" || attempt.length > 256) return "bad predecessor attempt id";
  const bare = o["barePath"];
  if (typeof bare !== "string" || !path.isAbsolute(bare) || bare.length > 4096) return "bad bare path";
  return { version: 1, runId, predecessorGeneration: gen, predecessorAttemptId: attempt, barePath: bare };
}

/** Read the marker of `runId`. Never throws for the marker's own state: absent, a valid marker, or `unreadable` (present). */
export async function readRetainedHomeMarker(dataDir: string, runId: string, fs: RetainedHomeFs = defaultRetainedHomeFs): Promise<RetainedHomeRead> {
  const { file } = markerPaths(dataDir, runId);
  let raw: string | null;
  try { raw = await fs.read(file); } catch (e) { return { kind: "unreadable", reason: errCode(e) ?? (e instanceof Error ? e.message : "error") }; }
  if (raw === null) return { kind: "absent" };
  const parsed = parseMarker(raw, runId);
  return typeof parsed === "string" ? { kind: "unreadable", reason: parsed } : { kind: "marker", marker: parsed };
}

/**
 * Whether `runId`'s retained-home marker exists. Without `binding` this is the reclaimers' protection question: anything but a
 * clean ENOENT (a malformed or unreadable marker, an unreadable directory) is "present". With `binding` (the restore exit's
 * evidence question) it is true only for a valid marker recording exactly that bare path and predecessor attempt id; a marker of
 * another bare or attempt, or one that cannot be read, is not evidence about that bare. An invalid run id throws.
 */
export async function hasRetainedHomeMarker(
  dataDir: string, runId: string, binding?: RetainedHomeBinding, fs: RetainedHomeFs = defaultRetainedHomeFs,
): Promise<boolean> {
  const read = await readRetainedHomeMarker(dataDir, runId, fs);
  if (binding === undefined) return read.kind !== "absent";
  return read.kind === "marker" && read.marker.barePath === binding.barePath && read.marker.predecessorAttemptId === binding.attemptId;
}

/**
 * Write the marker durably, in this order: create the directory; fsync `<dataDir>`; O_EXCL write and fsync of the file; fsync the
 * directory; read back. An existing identical marker is re-synced (file and directory) before it is accepted; a differing or
 * partial one refuses, and stays in place (it still protects the directories). Any failure throws before the caller writes the bare.
 */
export async function writeRetainedHomeMarker(dataDir: string, marker: RetainedHomeMarker, fs: RetainedHomeFs = defaultRetainedHomeFs): Promise<void> {
  const { dir, file } = markerPaths(dataDir, marker.runId);
  const data = serialize(marker);
  if (typeof parseMarker(data, marker.runId) === "string") {
    throw new RetainedHomeMarkerRefusal("the retained-home marker content is invalid");
  }
  await fs.mkdir(dir);
  await fs.syncDir(dataDir);
  try {
    await fs.createExclusive(file, data);
  } catch (e) {
    if (errCode(e) !== "EEXIST") throw e;
    const existing = await fs.read(file);
    if (existing !== data) throw new RetainedHomeMarkerRefusal("a different or partial retained-home marker already exists");
    await fs.syncFile(file);
  }
  await fs.syncDir(dir);
  const back = await fs.read(file);
  if (back !== data) throw new RetainedHomeMarkerRefusal("the retained-home marker read-back differs");
}

/** Remove `runId`'s marker (a missing one is fine) and make the removal durable. A failed unlink throws with the marker still
 *  present; a failed directory sync throws AFTER the unlink succeeded, so the marker is gone but its removal is not proven
 *  durable (the caller must treat that as unfinished and retry). */
export async function removeRetainedHomeMarker(dataDir: string, runId: string, fs: RetainedHomeFs = defaultRetainedHomeFs): Promise<void> {
  const { dir, file } = markerPaths(dataDir, runId);
  await fs.unlink(file);
  try { await fs.syncDir(dir); } catch (e) { if (errCode(e) !== "ENOENT") throw e; }
}
