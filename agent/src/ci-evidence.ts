import { createHash } from "node:crypto";
import { constants as fsc, promises as fs, type Stats } from "node:fs";
import type { FileHandle } from "node:fs/promises";
import path from "node:path";
import type { Readable } from "node:stream";
import { z } from "zod";

import { validateJobInputFileSpec, writeJobInputFile, type JobWorkspace } from "./job-workspace.js";
import type { ClaimCIRepair, ClaimCIRepairFile } from "./protocol.js";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const integer = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER);
const text = z.string();
const sourceReasons = new Set(["missing", "expired", "denied", "unsupported", "malformed",
  "download_budget", "download_unknown", "deadline", "association", "attempt_provenance_unavailable",
  "scrubber_unavailable", "artifact_limit", "archive_safety", "inflated_budget"]);
const fileSchema = z.object({
  manifest_id: text.regex(UUID), name: text, size: integer.positive().max(4 << 20),
  sha256: text.regex(/^[0-9a-f]{64}$/),
}).strict();
const entrySchema = z.object({
  artifact_id: text.min(1).max(200), entry_ordinal: integer, label: text,
  artifact_name: text.optional(), path: text.optional(), compressed_size: integer.optional(),
  uncompressed_size: integer.optional(), summary: z.boolean().optional(),
  shortened: z.boolean().optional(), status: z.enum(["kept", "skipped"]), reason: text.optional(),
  manifest_id: text.optional(), name: text.optional(), size: integer.optional(), sha256: text.optional(),
}).strict();
const repairSchema = z.object({
  manifest: z.object({
    execution: z.object({
      run_id: integer.positive(), repository_id: integer, head_repository_id: integer.optional(),
      workflow_key: text, workflow: text.optional(), sha: text, branch: text,
      updated_at: text, attempt: integer.nullable().optional(),
    }).strict().nullable().optional(),
    reasons: z.array(text.refine((r) => sourceReasons.has(r))).max(20).nullable().optional(),
    omitted_kept_aliases: integer.optional(), entries: z.array(entrySchema).max(250),
    omitted: integer.nullable().optional(), uninspected: z.boolean().optional(),
  }).strict(),
  input_files: z.array(fileSchema).max(250),
}).strict();

function fail(message: string): never { throw new Error(`CI evidence: ${message}`); }
function digest(bytes: Uint8Array): string { return createHash("sha256").update(bytes).digest("hex"); }
function identity(a: Stats, b: Stats): boolean { return a.dev === b.dev && a.ino === b.ino && a.uid === b.uid; }
function safeIdentity(s: string, max: number): boolean {
  return Buffer.byteLength(s) <= max && !/[\p{Cc}\p{Cf}\p{Cs}]/u.test(s);
}

function validate(raw: unknown): { repair: ClaimCIRepair; files: ClaimCIRepairFile[] } {
  // The byte check precedes schema traversal and every filesystem/network operation.
  let encoded: string | undefined;
  try { encoded = JSON.stringify(raw); } catch { fail("manifest is not JSON"); }
  if (!encoded || Buffer.byteLength(encoded) > 256 << 10) fail("repair byte limit");
  const decoded = JSON.parse(encoded);
  if (!decoded?.manifest || Buffer.byteLength(JSON.stringify(decoded.manifest)) > 128 << 10) fail("manifest byte limit");
  const repair = repairSchema.parse(decoded);
  const ids = new Map<string, ClaimCIRepairFile>();
  const names = new Map<string, string>();
  for (const f of repair.input_files) {
    validateJobInputFileSpec(f);
    if (f.name !== f.sha256 + ".txt") fail("storage name must be text");
    const prior = ids.get(f.manifest_id);
    if (prior && (prior.name !== f.name || prior.sha256 !== f.sha256 || prior.size !== f.size)) fail("conflicting file identity");
    if (names.has(f.name) && names.get(f.name) !== f.manifest_id) fail("conflicting storage name");
    ids.set(f.manifest_id, f);
    names.set(f.name, f.manifest_id);
  }
  const files = [...ids.values()];
  if (files.length > 50 || files.reduce((n, f) => n + f.size, 0) > 16 << 20) fail("file budget exceeded");
  const referenced = new Set<string>();
  const artifacts = new Set<string>();
  let skipped = 0;
  for (const e of repair.manifest.entries) {
    if (!safeIdentity(e.artifact_id, 200) || (e.reason !== undefined && !safeIdentity(e.reason, 100))) fail("invalid entry identity/reason");
    const key = JSON.stringify([e.artifact_id, e.entry_ordinal]);
    if (artifacts.has(key)) fail("duplicate artifact identity");
    artifacts.add(key);
    if (e.status === "skipped") { skipped++; continue; }
    const f = e.manifest_id ? ids.get(e.manifest_id) : undefined;
    if (!f || e.name !== f.name || e.size !== f.size || e.sha256 !== f.sha256) fail("kept reference mismatch");
    referenced.add(f.manifest_id);
  }
  if (skipped > 200 || referenced.size !== ids.size) fail("unmatched files or skipped budget");
  const ex = repair.manifest.execution;
  if (ex && (!safeIdentity(ex.workflow_key, 200) || !safeIdentity(ex.sha, 200))) fail("invalid execution identity");
  return { repair, files };
}

/** Structural match for APIClient.downloadJobFile; one attempt per unique file. */
export interface CIEvidenceDownloader {
  downloadJobFile(run: string, file: string, generation: number,
    sink: (body: Readable) => Promise<void>, signal?: AbortSignal, timeoutMs?: number): Promise<void>;
}

export interface PrepareCIEvidenceOptions {
  claim: { run_id: string; kind?: string; config?: { ci_config_guard?: boolean }; ci_repair?: unknown };
  claimGeneration: number;
  runHome: string;
  cloneRoot: string;
  downloader: CIEvidenceDownloader;
  signal: AbortSignal;
  /** Preparation-only interruption; unlike terminal cancellation this may be rearmed by the runner. */
  downloadSignal?: AbortSignal;
  isCurrent: () => boolean;
  /** Positive integer, clamped to the fixed 60s per-download ceiling. */
  timeoutMs?: number;
}

export interface CIEvidenceExcerpt {
  manifest_id: string;
  offset: number;
  text: string;
  truncated: boolean;
  omitted_lines: number;
  omitted_bytes: number;
}

export interface PreparedCIEvidence {
  /** Bounded JSON data for an untrusted prompt fence; contains no local paths. */
  metadata: string;
  read(request: unknown): Promise<CIEvidenceExcerpt>;
  /** Revokes this claim's reader and releases handles; retains verified files for resume. */
  dispose(): Promise<void>;
}

function metadata(repair: ClaimCIRepair, files: ClaimCIRepairFile[]): string {
  const execution = repair.manifest.execution;
  const base = {
    untrusted: true, files,
    execution: execution ? { ...execution, workflow: execution.workflow?.slice(0, 200),
      branch: execution.branch.slice(0, 200), updated_at: execution.updated_at.slice(0, 100) } : null,
    reasons: repair.manifest.reasons ?? [], omitted: repair.manifest.omitted ?? null,
    omitted_kept_aliases: repair.manifest.omitted_kept_aliases ?? 0,
    uninspected: repair.manifest.uninspected ?? false,
  };
  // Escaping prevents metadata from closing a caller's XML/HTML untrusted fence.
  const quote = (v: unknown): string => JSON.stringify(v).replace(/[<>&\u007f-\u009f]/g,
    (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));
  const rows: unknown[] = [];
  let omittedRows = repair.manifest.entries.length;
  for (const e of repair.manifest.entries) {
    const row = { ...e, label: e.label.slice(0, 512), artifact_name: e.artifact_name?.slice(0, 200), path: e.path?.slice(0, 512),
      shortened: e.shortened || e.label.length > 512 || (e.artifact_name?.length ?? 0) > 200 || (e.path?.length ?? 0) > 512 };
    const candidate = quote({ ...base, entries: [...rows, row], omitted_metadata_rows: omittedRows - 1 });
    if (Buffer.byteLength(candidate) > 64 << 10) break;
    rows.push(row);
    omittedRows--;
  }
  return quote({ ...base, entries: rows, omitted_metadata_rows: omittedRows });
}

/** Validate before the runner clones or creates an executor home. Absent evidence is compatible. */
export function validateCIRepairClaim(claim: PrepareCIEvidenceOptions["claim"], generation: number | undefined): void {
  if (claim.ci_repair === undefined) return;
  if (claim.kind !== "prompt" || claim.config?.ci_config_guard !== true) fail("requires guarded prompt");
  if (!UUID.test(claim.run_id) || !Number.isSafeInteger(generation) || (generation ?? 0) <= 0) fail("invalid claim");
  validate(claim.ci_repair);
}

/**
 * Linux descriptor anchoring keeps the production writer inside the private subtree even if
 * the group-writable runHome is renamed during download. Live identities are rechecked on use.
 * All loops are bounded by validated manifest caps, file sizes, or absolute path depth;
 * failures abort preparation (no sibling downloads/retries).
 */
export async function prepareCIEvidence(opts: PrepareCIEvidenceOptions): Promise<PreparedCIEvidence> {
  const { claim } = opts;
  validateCIRepairClaim(claim, opts.claimGeneration);
  if (claim.kind !== "prompt" || claim.config?.ci_config_guard !== true) fail("requires guarded prompt");
  if (!UUID.test(claim.run_id) || !Number.isSafeInteger(opts.claimGeneration) || opts.claimGeneration <= 0) fail("invalid claim");
  const { repair, files } = validate(claim.ci_repair);
  if (!path.isAbsolute(opts.runHome) || !path.isAbsolute(opts.cloneRoot)) fail("absolute roots required");
  const home = path.resolve(opts.runHome);
  const clone = path.resolve(opts.cloneRoot);
  if (home === clone || home.startsWith(clone + path.sep)) fail("evidence must be outside clone");
  if (opts.timeoutMs !== undefined && (!Number.isSafeInteger(opts.timeoutMs) || opts.timeoutMs <= 0)) fail("invalid timeout");
  let disposed = false;
  const active = (): void => {
    if (disposed || opts.signal.aborted || !opts.isCurrent()) fail("cancelled, stale or disposed");
  };
  active();
  // Logs-only manifests need no filesystem or Linux descriptor support.
  if (files.length === 0) return {
    metadata: metadata(repair, files),
    async read() { active(); fail("unknown manifest_id"); },
    async dispose() { disposed = true; },
  };
  if (process.platform !== "linux") fail("descriptor anchoring unavailable");
  const handles: FileHandle[] = [];
  const dirs: { handle: FileHandle; st: Stats; absolute: string; private: boolean }[] = [];
  const uid = process.getuid!();
  const flags = fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW;
  const checkDir = (st: Stats, privateDir: boolean): void => {
    if (!st.isDirectory() || (privateDir && (st.uid !== uid || (st.mode & 0o7777) !== 0o700))) fail("unsafe private directory");
  };
  const checked = async <T>(op: () => Promise<T>): Promise<T> => {
    active();
    const result = await op();
    active();
    return result;
  };
  const verifyDirs = async (): Promise<void> => {
    for (const d of dirs) {
      const st = await checked(() => fs.lstat(d.absolute));
      checkDir(st, d.private);
      if (st.isSymbolicLink() || !identity(st, d.st)) fail("directory identity changed");
    }
  };
  const close = async (): Promise<void> => {
    // At most path depth + three handles; close every sibling even after a close failure.
    const results = await Promise.allSettled(handles.map((h) => h.close()));
    const rejected = results.find((r) => r.status === "rejected");
    if (rejected?.status === "rejected") throw rejected.reason;
  };
  try {
    const cloneReal = await checked(() => fs.realpath(clone));
    if (home === cloneReal || home.startsWith(cloneReal + path.sep)) fail("evidence must be outside clone");
    if ((await checked(() => fs.statfs("/proc/self/fd"))).type !== 0x9fa0) fail("descriptor anchoring unavailable");
    let parent = await fs.open("/", flags);
    handles.push(parent);
    active();
    dirs.push({ handle: parent, st: await checked(() => parent.stat()), absolute: "/", private: false });
    let absolute = "";
    for (const part of home.split("/").filter(Boolean)) {
      absolute += "/" + part;
      const child = await fs.open(`/proc/self/fd/${parent.fd}/${part}`, flags);
      handles.push(child);
      active();
      const st = await checked(() => child.stat());
      checkDir(st, false);
      dirs.push({ handle: child, st, absolute, private: false });
      parent = child;
    }
    if (dirs.at(-1)!.st.uid !== uid) fail("run home is not worker owned");
    for (const part of ["ci-evidence", "inputs"]) {
      const target = `/proc/self/fd/${parent.fd}/${part}`;
      active();
      let created = false;
      try { await fs.mkdir(target, { mode: 0o700 }); created = true; }
      catch (err) { if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw err; }
      active();
      const child = await fs.open(target, flags);
      handles.push(child);
      active();
      const initial = await checked(() => child.stat());
      if (initial.uid !== uid) fail("private directory is not worker owned");
      // A setgid runHome propagates its group bit. Only a newly created, pinned,
      // worker-owned directory is normalized; resumed trees must already be private.
      if (created) await checked(() => child.chmod(0o700));
      const st = await checked(() => child.stat());
      checkDir(st, true);
      absolute += "/" + part;
      dirs.push({ handle: child, st, absolute, private: true });
      parent = child;
    }
    const workHandle = dirs.at(-2)!.handle;
    const inputsHandle = dirs.at(-1)!.handle;
    const work = `/proc/self/fd/${workHandle.fd}`;
    const inputsDir = `/proc/self/fd/${inputsHandle.fd}`;
    const ws: JobWorkspace = { root: home, home, work, inputsDir, outputsDir: path.join(work, "outputs") };
    const fileStats = new Map<string, Stats>();
    const verifyFile = async (f: ClaimCIRepairFile): Promise<Buffer> => {
      await verifyDirs();
      const h = await fs.open(path.join(inputsDir, f.name), fsc.O_RDONLY | fsc.O_NOFOLLOW | fsc.O_NONBLOCK);
      try {
        active();
        const before = await checked(() => h.stat());
        const known = fileStats.get(f.manifest_id);
        if (!before.isFile() || before.nlink !== 1 || before.uid !== uid ||
            (before.mode & 0o7777) !== 0o400 || before.size !== f.size ||
            (known && !identity(known, before))) fail("unsafe evidence file");
        const bytes = Buffer.alloc(f.size);
        let offset = 0;
        while (offset < bytes.length) {
          const { bytesRead } = await checked(() => h.read(bytes, offset, bytes.length - offset, offset));
          if (!bytesRead) fail("short evidence file");
          offset += bytesRead;
        }
        const after = await checked(() => h.stat());
        if (!identity(before, after) || after.size !== before.size || after.mtimeMs !== before.mtimeMs ||
            after.ctimeMs !== before.ctimeMs || digest(bytes) !== f.sha256) fail("evidence integrity mismatch");
        const live = await checked(() => fs.lstat(path.join(inputsDir, f.name)));
        if (!identity(before, live) || live.isSymbolicLink()) fail("file identity changed");
        await verifyDirs();
        fileStats.set(f.manifest_id, after);
        active();
        return bytes;
      } finally { await h.close(); active(); }
    };
    for (const f of files) {
      await verifyDirs();
      let exists = true;
      try { await checked(() => fs.lstat(path.join(inputsDir, f.name))); }
      catch (err) { if ((err as NodeJS.ErrnoException).code === "ENOENT") exists = false; else throw err; }
      if (!exists) {
        await checked(() => opts.downloader.downloadJobFile(claim.run_id, f.manifest_id, opts.claimGeneration,
          async (body) => {
            active();
            async function* guarded(): AsyncGenerator<Uint8Array> {
              for await (const chunk of body) { active(); yield chunk as Uint8Array; active(); }
              active();
            }
            await checked(() => writeJobInputFile(ws, f, guarded(), false));
          }, opts.downloadSignal ?? opts.signal, Math.min(opts.timeoutMs ?? 60_000, 60_000)));
      }
      await verifyFile(f);
    }
    const byId = new Map(files.map((f) => [f.manifest_id, f]));
    active();
    return {
      metadata: metadata(repair, files),
      async read(request) {
        active();
        const q = z.object({ manifest_id: text.regex(UUID), offset: integer.positive().default(1),
          limit: integer.positive().max(200).default(200) }).strict().parse(request);
        const f = byId.get(q.manifest_id);
        if (!f) fail("unknown manifest_id");
        const bytes = await verifyFile(f);
        const content = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
        const lines = content.split("\n");
        if (lines.at(-1) === "") lines.pop();
        const selected = lines.slice(q.offset - 1, q.offset - 1 + q.limit).join("\n");
        let excerpt = Buffer.from(selected);
        if (excerpt.length > 64 << 10) {
          let end = 64 << 10;
          while (end > 0 && (excerpt[end]! & 0xc0) === 0x80) end--;
          excerpt = excerpt.subarray(0, end);
        }
        const omittedLines = Math.max(0, lines.length - Math.min(lines.length, q.offset - 1 + q.limit));
        const omittedBytes = Buffer.byteLength(selected) - excerpt.length;
        active();
        return { manifest_id: f.manifest_id, offset: q.offset, text: excerpt.toString("utf8"),
          truncated: omittedLines > 0 || omittedBytes > 0, omitted_lines: omittedLines, omitted_bytes: omittedBytes };
      },
      async dispose() { if (!disposed) { disposed = true; await close(); } },
    };
  } catch (err) {
    disposed = true;
    await close();
    throw err;
  }
}
