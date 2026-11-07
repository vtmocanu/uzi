import { createHash } from "node:crypto";
import { constants } from "node:fs";
import fs from "node:fs/promises";

/**
 * Bounded, read-only parser for a Git index file (`.git/index`), used by the automatic epoch
 * worktree proof so that nothing Git does is the first reader of a hostile index. It admits the
 * plain SHA-1 formats the runner clones use (versions 2, 3 and 4; version 4 prefix-compresses names)
 * and REFUSES everything else rather than interpreting it: split indexes (`link`), sparse
 * directories (`sdir`), the untracked cache (`UNTR`), fsmonitor (`FSMN`) and every unknown extension.
 * The `TREE` cache-tree extension is only skipped, never trusted: a forged one can claim an
 * unchanged subtree, so index-versus-HEAD must be compared from the entries themselves. Per entry it also
 * refuses the flags that hide a worktree difference from `git status` (assume-valid,
 * skip-worktree, intent-to-add), any merge stage, and any mode other than a regular file,
 * executable, symlink or gitlink. Paths stay raw bytes: Git paths need not be UTF-8.
 */

/** A refusal (`refused`: the index is not one we interpret) or an exceeded bound (`limit`). */
export class GitIndexError extends Error {
  constructor(readonly kind: "refused" | "limit", message: string) { super(message); }
}

export interface GitIndexEntry {
  path: Buffer;
  /** Full st_mode as stored (for example 0o100644). */
  mode: number;
  /** Lowercase hex object id. */
  sha: string;
  stage: number;
  assumeValid: boolean;
  skipWorktree: boolean;
  intentToAdd: boolean;
  /** Raw stat data as stored, kept so a caller can later detect racily-clean entries. */
  ctimeS: number;
  ctimeNs: number;
  mtimeS: number;
  mtimeNs: number;
  dev: number;
  ino: number;
  uid: number;
  gid: number;
  size: number;
}

export interface ParsedGitIndex {
  version: 2 | 3 | 4;
  entries: GitIndexEntry[];
  /** The verified SHA-1 trailer. */
  trailer: Buffer;
  extensions: string[];
}

export interface GitIndexLimits {
  /** Largest entry count the caller will walk. */
  maxEntries: number;
}

const HEADER_BYTES = 12;
const TRAILER_BYTES = 20;
const FIXED_ENTRY_BYTES = 62;
const NAME_SATURATED = 0xfff;
const FLAG_ASSUME_VALID = 0x8000;
const FLAG_EXTENDED = 0x4000;
const EXT_SKIP_WORKTREE = 0x4000;
const EXT_INTENT_TO_ADD = 0x2000;
/** Extensions that carry only derived caches; any other signature changes what Git would read. REUC
 *  (resolve-undo) is refused: it names conflict-side blobs Git keeps alive, so it can be a clone's only
 *  pointer to work the epoch proof would otherwise not require. */
const ALLOWED_EXTENSIONS: ReadonlySet<string> = new Set(["TREE"]);
const ALLOWED_MODES: ReadonlySet<number> = new Set([0o100644, 0o100755, 0o120000, 0o160000]);

/** A path longer than this cannot exist; it bounds every index path of every version and each rebuilt version 4 name. */
const MAX_INDEX_PATH = 4096;

const refused = (message: string): never => { throw new GitIndexError("refused", message); };
const limit = (message: string): never => { throw new GitIndexError("limit", message); };

/** A tracked path Git could resolve outside the worktree or into its own metadata is refused. */
function checkTrackedPath(name: Buffer): void {
  if (name.length === 0 || name[0] === 0x2f || name[name.length - 1] === 0x2f) refused("unsafe tracked path");
  let start = 0;
  for (let i = 0; i <= name.length; i++) {
    if (i !== name.length && name[i] !== 0x2f) continue;
    const part = name.subarray(start, i);
    start = i + 1;
    if (part.length === 0 || part.equals(Buffer.from(".")) || part.equals(Buffer.from("..")) ||
        part.toString("latin1").toLowerCase() === ".git") refused("unsafe tracked path component");
  }
}

/**
 * Parse and verify one index image. `strict` (the default) applies the full refusal policy.
 * `inspect` keeps the structural checks but reports per-entry flags, stages and modes instead of
 * refusing them, and walks any extension by its length alone; it exists so a differential test can
 * compare the parser with `git ls-files`.
 */
export function parseGitIndex(buf: Buffer, limits: GitIndexLimits, policy: "strict" | "inspect" = "strict"): ParsedGitIndex {
  const strict = policy === "strict";
  if (buf.length < HEADER_BYTES + TRAILER_BYTES) refused("index is too short");
  if (buf.toString("latin1", 0, 4) !== "DIRC") refused("index signature");
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3 && version !== 4) refused("unsupported index version");
  const count = buf.readUInt32BE(8);
  if (count > limits.maxEntries) limit("index entry limit");
  const end = buf.length - TRAILER_BYTES;
  const trailer = Buffer.from(buf.subarray(end));
  // An all-zero trailer means "unchecked" to Git; it proves nothing about the content.
  if (trailer.every(b => b === 0)) refused("index checksum absent");
  if (!createHash("sha1").update(buf.subarray(0, end)).digest().equals(trailer)) refused("index checksum mismatch");

  const entries: GitIndexEntry[] = [];
  let p = HEADER_BYTES;
  let previous: Buffer | undefined;
  for (let i = 0; i < count; i++) {
    if (p + FIXED_ENTRY_BYTES > end) refused("truncated index entry");
    const mode = buf.readUInt32BE(p + 24);
    const flags = buf.readUInt16BE(p + 60);
    let nameOffset = p + FIXED_ENTRY_BYTES;
    let extended = 0;
    if (flags & FLAG_EXTENDED) {
      if (version < 3 || nameOffset + 2 > end) refused("extended index entry");
      extended = buf.readUInt16BE(nameOffset);
      nameOffset += 2;
    }
    let name: Buffer;
    let size: number;
    if (version === 4) {
      // Prefix-compressed: a varint count of bytes to strip from the previous name, then the
      // NUL-terminated suffix. No padding follows. The flags' length field must agree with the rebuilt name (checked below).
      let at = nameOffset;
      let strip = 0;
      for (let first = true, byte = 0x80; byte & 0x80; first = false) {
        if (at >= end) refused("truncated version 4 path");
        byte = buf[at++]!;
        strip = first ? byte & 0x7f : ((strip + 1) * 128) + (byte & 0x7f);
        if (strip > MAX_INDEX_PATH) refused("version 4 strip length");
      }
      const previousName = previous ?? Buffer.alloc(0);
      if (strip > previousName.length) refused("version 4 strip length exceeds the previous path");
      const nul = buf.indexOf(0, at);
      if (nul < 0 || nul >= end) refused("unterminated index path");
      const keep = previousName.length - strip;
      if (keep + (nul - at) > MAX_INDEX_PATH) refused("version 4 path exceeds PATH_MAX");
      name = Buffer.concat([previousName.subarray(0, keep), buf.subarray(at, nul)]);
      // Git sizes the entry from the flags' length field when it is not saturated, so a name
      // that disagrees would be walked differently by Git than by this parser.
      const declared = flags & NAME_SATURATED;
      if (declared !== NAME_SATURATED ? name.length !== declared : name.length < NAME_SATURATED)
        refused("version 4 path length disagrees with the entry flags");
      size = nul + 1 - p;
    } else {
      let nameLength = flags & NAME_SATURATED;
      if (nameLength === NAME_SATURATED) {
        const nul = buf.indexOf(0, nameOffset);
        if (nul < 0 || nul >= end) refused("unterminated index path");
        nameLength = nul - nameOffset;
      } else if (nameOffset + nameLength >= end || buf[nameOffset + nameLength] !== 0) {
        refused("index path length");
      }
      if (nameLength > MAX_INDEX_PATH) refused("index path exceeds PATH_MAX");
      name = Buffer.from(buf.subarray(nameOffset, nameOffset + nameLength));
      if (name.includes(0)) refused("index path contains NUL");
      size = (nameOffset + nameLength + 8 - p) & ~7;
    }
    if (p + size > end) refused("index entry padding");
    const entry: GitIndexEntry = {
      path: name, mode, sha: buf.toString("hex", p + 40, p + 60), stage: (flags >> 12) & 3,
      assumeValid: (flags & FLAG_ASSUME_VALID) !== 0,
      skipWorktree: (extended & EXT_SKIP_WORKTREE) !== 0, intentToAdd: (extended & EXT_INTENT_TO_ADD) !== 0,
      ctimeS: buf.readUInt32BE(p), ctimeNs: buf.readUInt32BE(p + 4), mtimeS: buf.readUInt32BE(p + 8),
      mtimeNs: buf.readUInt32BE(p + 12), dev: buf.readUInt32BE(p + 16), ino: buf.readUInt32BE(p + 20),
      uid: buf.readUInt32BE(p + 28), gid: buf.readUInt32BE(p + 32), size: buf.readUInt32BE(p + 36),
    };
    if (strict) {
      if (entry.assumeValid) refused("assume-valid entry");
      if (extended !== 0) refused("extended entry flags");
      if (entry.stage !== 0) refused("unmerged entry");
      if (!ALLOWED_MODES.has(mode)) refused("unsupported entry mode");
      checkTrackedPath(name);
      if (previous && Buffer.compare(previous, name) >= 0) refused("index entries are not strictly sorted");
    }
    previous = name;
    entries.push(entry);
    p += size;
  }

  const extensions: string[] = [];
  while (p < end) {
    if (p + 8 > end) refused("truncated index extension");
    const signature = buf.toString("latin1", p, p + 4);
    const length = buf.readUInt32BE(p + 4);
    if (length > end - p - 8) refused("index extension length");
    if (strict && !ALLOWED_EXTENSIONS.has(signature)) {
      refused(`unsupported index extension ${/^[A-Za-z]{4}$/.test(signature) ? signature : "(non-alphabetic)"}`);
    }
    extensions.push(signature);
    p += 8 + length;
  }
  return { version: version as 2 | 3 | 4, entries, trailer, extensions };
}

/** Identity and size of a file as lstat saw it; any difference later means the file changed. */
export function statSignature(st: import("node:fs").BigIntStats): string {
  return [st.dev, st.ino, st.mode, st.size, st.mtimeNs, st.ctimeNs].join(":");
}

export interface BoundedIndex extends ParsedGitIndex {
  /** {@link statSignature} of the file that was read. */
  signature: string;
  /** Bytes read; the caller charges them to its metadata budget. */
  size: number;
  /** The exact image that was parsed, kept so {@link indexUnchanged} can compare a second read with it. */
  bytes: Buffer;
}

type Step = <T>(action: () => Promise<T>) => Promise<T>;
const direct: Step = action => action();

/**
 * Open a descriptor or directory handle through `step`, run `use` on it and always close it. The handle is
 * captured INSIDE the stepped action, so a `step` that throws after the open completed (a liveness check in
 * its finally) still leaves the acquired handle to be closed here.
 */
export async function withClosingHandle<H extends { close(): Promise<void> }, T>(step: Step,
  open: () => Promise<H>, use: (handle: H) => Promise<T>): Promise<T> {
  const held: { handle?: H } = {};
  try {
    const handle = await step(async () => (held.handle = await open()));
    return await use(handle);
  } finally {
    await held.handle?.close();
  }
}

/**
 * lstat a regular file, refuse it above `maxBytes`, read exactly its size through one descriptor
 * (opened non-blocking and without following links, so a swapped-in FIFO or link cannot hang or
 * redirect the read), verify the descriptor still names the lstat'd file and that nothing was
 * appended, then parse it. `step` wraps every awaited filesystem call (liveness checks).
 */
export async function readBoundedIndex(file: string,
  options: GitIndexLimits & { maxBytes: number; step?: Step }): Promise<BoundedIndex> {
  const step = options.step ?? direct;
  const before = await step(() => fs.lstat(file, { bigint: true }));
  if (!before.isFile()) refused("index is not a regular file");
  if (before.size > BigInt(Math.max(0, options.maxBytes))) limit("index byte limit");
  const size = Number(before.size);
  return withClosingHandle(step, () => fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK), async handle => {
    const opened = await step(() => handle.stat({ bigint: true }));
    if (!opened.isFile() || opened.dev !== before.dev || opened.ino !== before.ino || opened.size !== before.size)
      refused("index changed while opening");
    const buf = Buffer.allocUnsafe(size);
    let read = 0;
    while (read < size) {
      const { bytesRead } = await step(() => handle.read(buf, read, size - read, read));
      if (bytesRead === 0) refused("index truncated while reading");
      read += bytesRead;
    }
    if ((await step(() => handle.read(Buffer.alloc(1), 0, 1, size))).bytesRead !== 0) refused("index grew while reading");
    const after = await step(() => handle.stat({ bigint: true }));
    if (statSignature(after) !== statSignature(opened)) refused("index changed while reading");
    return { ...parseGitIndex(buf, options), signature: statSignature(before), size, bytes: buf };
  });
}

/**
 * Read one small regular file whole: lstat it as a regular file at most `maxBytes` long, open it
 * without following links and without blocking, check the descriptor is that file and that nothing
 * changed or grew while reading. Returns the bytes and the lstat {@link statSignature}.
 */
export async function readSmallRegularFile(file: string, maxBytes: number,
  step: Step = direct): Promise<{ bytes: Buffer; signature: string }> {
  const before = await step(() => fs.lstat(file, { bigint: true }));
  if (!before.isFile()) refused("not a regular file");
  if (before.size > BigInt(Math.max(0, maxBytes))) limit("file byte limit");
  const size = Number(before.size);
  return withClosingHandle(step, () => fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK), async handle => {
    const opened = await step(() => handle.stat({ bigint: true }));
    if (!opened.isFile() || opened.dev !== before.dev || opened.ino !== before.ino || opened.size !== before.size)
      refused("file changed while opening");
    const bytes = Buffer.allocUnsafe(size);
    let read = 0;
    while (read < size) {
      const { bytesRead } = await step(() => handle.read(bytes, read, size - read, read));
      if (bytesRead === 0) refused("file truncated while reading");
      read += bytesRead;
    }
    if ((await step(() => handle.read(Buffer.alloc(1), 0, 1, size))).bytesRead !== 0) refused("file grew while reading");
    if (statSignature(await step(() => handle.stat({ bigint: true }))) !== statSignature(opened)) refused("file changed while reading");
    return { bytes, signature: statSignature(before) };
  });
}

/**
 * True only when the file still has the read signature (dev, ino, mode, size, mtime, ctime) AND a
 * second bounded read returns exactly the bytes the first read parsed. A trailer-only comparison
 * would miss a rewrite that kept the trailer. The second read costs `original.size` bytes, which
 * the caller must charge.
 */
export async function indexUnchanged(file: string, original: BoundedIndex, step: Step = direct): Promise<boolean> {
  try {
    const again = await readSmallRegularFile(file, original.size, step);
    return again.signature === original.signature && again.bytes.equals(original.bytes);
  } catch (error) {
    if (error instanceof GitIndexError) return false;
    throw error;
  }
}
