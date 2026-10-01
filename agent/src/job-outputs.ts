// The output files of a job (PRD #1909 M4, D4/D6). Before it posts the result, the JobRunner
// uploads the files the model listed in submit_job_result's `output_files` (workspace-relative
// paths under `outputs/` or `sources/`) so the caller can download them. `report.md` and
// `findings.json` are NOT uploaded here: the api stores them itself from the result it stored (and
// scrubbed), so a worker upload can neither bypass that scrub nor stand in for them; both names are
// reserved and refused in `output_files` (see validateOutputFilePaths).
//
// Nothing here can fail the job. A file the api refuses (413 limit, 415 type, 422 integrity, 507
// quota) is logged and the next one is tried (the api records that refusal itself); a transport
// failure or a 503 uploads_busy is retried a bounded number of times (waiting the api's
// Retry-After, capped by the deadline) and then counts as a refusal; a stale claim or a finished
// run (409), or a run the api will not take files for (403/404/405, files_unavailable), stops the
// uploads, because every later one would be refused the same way (the result post that follows is
// refused the same way too).
//
// A file the WORKER drops (empty, over the worker's own ceilings, unreadable or changed, retries
// exhausted, out of time) leaves no trace at the api, so the summary lists it in `refused` and the
// runner posts that list with the result (`refused_outputs`), where the api records it.
//
// Files are streamed from disk, never buffered whole. Each file is opened ONCE through a
// Linux descriptor-relative no-follow walk of every directory component: the
// handle is fstat-ed (a regular file, one link, within the ceiling, all before any byte is read),
// hashed by a bounded loop that checks the deadline and the abort signal, and then uploaded from the
// SAME handle, so the path cannot be swapped between the hash pass and the upload.

import { createHash } from "node:crypto";
import { lstatSync, realpathSync } from "node:fs";
import type { FileHandle } from "node:fs/promises";
import path from "node:path";
import type { Readable } from "node:stream";

import { isTransientStatus, RequestError, type WorkerClient } from "./client.js";
import { screenToolPath } from "./guardrails.js";
import { openJobOutputFile } from "./job-workspace.js";
import type { Logger } from "./log.js";
import type { JobFileUploadMeta, JobRefusedOutput } from "./protocol.js";
import { errMessage, sleep } from "./util.js";

/** The directories an output file may come from, relative to the workspace `work` dir. */
const OUTPUT_DIRS: readonly string[] = ["outputs", "sources"];

/** Most files one submit_job_result may list. */
export const OUTPUT_FILES_MAX = 50;
/** Longest workspace-relative path accepted in `output_files`. */
const OUTPUT_PATH_MAX_BYTES = 1024;

/** Fixed worker-side ceilings on the outputs of one job, the mirror of JOB_INPUT_CEILINGS: the
 *  worker never uploads more than this whatever the api would take. The api's own (smaller)
 *  limits refuse the rest per file. */
const JOB_OUTPUT_CEILINGS = {
  fileBytes: 256 * 1024 * 1024,
  files: 64,
  totalBytes: 1024 * 1024 * 1024,
} as const;

/** Longest display name (code points) sent; the api sanitises again and bounds it itself. */
const DISPLAY_NAME_MAX = 100;
const DISPLAY_EXT_MAX = 16;

/** The basenames the api generates itself (report and findings); an `output_files` entry may not
 *  use them, in any case. Mirrors workersvc.IsReservedJobOutputName. */
const RESERVED_OUTPUT_NAMES: ReadonlySet<string> = new Set(["report.md", "findings.json"]);

/** Characters refused in a listed path: backslash, every Unicode "Other" character (control, format,
 *  surrogate, private use, unassigned) and the line and paragraph separators. */
const BAD_PATH_CHARS = /[\\\p{C}\p{Zl}\p{Zp}]/u;

/** A string made safe to put in a log line: every character of BAD_PATH_CHARS (bar the backslash,
 *  which stays readable) as a \uXXXX escape, cut to `max` characters. Untrusted names and error
 *  text go through it. */
export function logSafe(s: string, max = 300): string {
  return s
    .slice(0, max)
    .replace(/[\p{C}\p{Zl}\p{Zp}]/gu, (c) => {
      const cp = c.codePointAt(0)!;
      // Astral code points (private use planes, tag characters) escape whole as \u{...}; the BMP
      // keeps the fixed four-digit \uXXXX form.
      return cp > 0xffff ? `\\u{${cp.toString(16)}}` : `\\u${cp.toString(16).padStart(4, "0")}`;
    });
}

const byteLen = (s: string): number => Buffer.byteLength(s, "utf8");

/** The lexical (no filesystem) check of the `output_files` a model submits: an array of at most
 *  OUTPUT_FILES_MAX workspace-relative POSIX paths, each under `outputs/` or `sources/` with at
 *  least one more segment, no absolute or home-relative form, no `.` or `..` or empty segment, no
 *  backslash and no control, format, line or paragraph separator character. Repeated paths are
 *  dropped (first wins, order kept). Two DIFFERENT paths may not share a display name (their
 *  basename as the caller sees it, compared case-insensitively: outputs/a/x.csv and outputs/b/x.csv
 *  collide), and the names the api generates itself, report.md and findings.json, are reserved: the
 *  model is told so it can rename. */
export function validateOutputFilePaths(raw: unknown): { ok: true; value: string[] } | { ok: false; error: string } {
  if (raw === undefined || raw === null) return { ok: true, value: [] };
  if (!Array.isArray(raw)) return { ok: false, error: "output_files must be an array of workspace-relative paths" };
  if (raw.length > OUTPUT_FILES_MAX) return { ok: false, error: `output_files lists at most ${OUTPUT_FILES_MAX} files` };
  const out: string[] = [];
  const seen = new Set<string>();
  const names = new Set<string>();
  for (const [i, p] of raw.entries()) {
    if (typeof p !== "string" || p === "") return { ok: false, error: `output_files[${i}] must be a non-empty string` };
    if (byteLen(p) > OUTPUT_PATH_MAX_BYTES) return { ok: false, error: `output_files[${i}] is longer than ${OUTPUT_PATH_MAX_BYTES} bytes` };
    if (BAD_PATH_CHARS.test(p)) {
      return { ok: false, error: `output_files[${i}] must not contain a backslash, a control character or an invisible or separator character` };
    }
    if (p.startsWith("/") || p.startsWith("~")) return { ok: false, error: `output_files[${i}] must be relative to the workspace, not absolute` };
    const segments = p.split("/");
    if (segments.some((s) => s === "" || s === "." || s === "..")) {
      return { ok: false, error: `output_files[${i}] must not contain '.', '..' or empty path segments` };
    }
    if (segments.length < 2 || !OUTPUT_DIRS.includes(segments[0]!)) {
      return { ok: false, error: `output_files[${i}] must be a file under outputs/ or sources/` };
    }
    if (seen.has(p)) continue;
    const display = outputDisplayName(p).toLowerCase();
    if (RESERVED_OUTPUT_NAMES.has(display)) {
      return { ok: false, error: `output_files[${i}] is named ${display}, which the system generates from your result; rename the file` };
    }
    if (names.has(display)) {
      return { ok: false, error: `output_files[${i}] has the same file name as an earlier entry (${logSafe(display, 100)}); give each listed file a distinct name` };
    }
    seen.add(p);
    names.add(display);
    out.push(p);
  }
  return { ok: true, value: out };
}

/** A workspace file resolved for upload. */
export interface ResolvedOutputFile {
  /** The real (symlink-free) path, inside `<work>/outputs` or `<work>/sources`. */
  real: string;
  /** The declared name: the basename of the requested path, sanitised. */
  displayName: string;
}

/** Resolve one lexically valid `output_files` entry against the workspace: the path guard
 *  (screenToolPath: outside the workspace, /proc, the secret mount and .git are denied), then the
 *  REAL path must lie inside `<work>/outputs` or `<work>/sources` and be a
 *  regular file. Every symlink component, a directory, a FIFO or a device is refused.
 *  This preflight is not the read authority: openJobOutputFile pins each component at upload.
 *  Synchronous, so the submit tool can run it while the model is still there to fix a mistake, and
 *  the runner runs it again at upload time. */
export function resolveOutputFile(
  work: string,
  rel: string,
  secretPaths: readonly string[] = [],
): { ok: true; value: ResolvedOutputFile } | { ok: false; error: string } {
  const screen = screenToolPath(rel, work, work, secretPaths);
  if (screen.denied) return { ok: false, error: `${rel}: ${screen.reason ?? "denied by the path guard"}` };
  let workReal: string;
  let real: string;
  try {
    // Preflight policy only; openJobOutputFile is the race-free read authority.
    let component = work;
    if (lstatSync(component).isSymbolicLink()) return { ok: false, error: `${rel}: workspace is a symlink` };
    for (const part of rel.split("/")) {
      component = path.join(component, part);
      if (lstatSync(component).isSymbolicLink()) return { ok: false, error: `${rel}: contains a symlink` };
    }
    workReal = realpathSync(work);
    real = realpathSync(path.resolve(work, rel));
  } catch {
    return { ok: false, error: `${rel}: the file does not exist` };
  }
  if (!OUTPUT_DIRS.some((d) => real.startsWith(path.join(workReal, d) + path.sep))) {
    return { ok: false, error: `${rel}: resolves outside outputs/ and sources/` };
  }
  try {
    if (!lstatSync(real).isFile()) return { ok: false, error: `${rel}: is not a regular file` };
  } catch {
    return { ok: false, error: `${rel}: the file does not exist` };
  }
  return { ok: true, value: { real, displayName: outputDisplayName(rel) } };
}

/** The display name sent for a path: its basename with control, format, line and paragraph
 *  separator characters removed, edge whitespace trimmed, bounded to DISPLAY_NAME_MAX code points by
 *  cutting the stem (a short extension is kept), and "output" when nothing is left. The api
 *  sanitises again; this only keeps the request well formed. */
export function outputDisplayName(rel: string): string {
  const base = rel.slice(rel.lastIndexOf("/") + 1);
  const cleaned = Array.from(base.replace(/[\p{C}\p{Zl}\p{Zp}]/gu, "").trim());
  if (cleaned.length <= DISPLAY_NAME_MAX) return cleaned.join("") || "output";
  const dot = cleaned.lastIndexOf(".");
  const ext = dot > 0 && cleaned.length - dot <= DISPLAY_EXT_MAX ? cleaned.slice(dot) : [];
  const stem = cleaned.slice(0, dot > 0 && ext.length ? dot : cleaned.length).slice(0, DISPLAY_NAME_MAX - ext.length);
  return (stem.join("").trim() + ext.join("")) || "output";
}

/** Bytes read per step of the hashing loop; the deadline and the abort signal are checked between
 *  steps. */
const HASH_STEP_BYTES = 64 * 1024;

/** A file the worker refuses to upload, with the reason the api records (WorkerRefusal* in the api
 *  and the `refused_outputs` of the result). */
class OutputRefusal extends Error {
  constructor(
    readonly reason: WorkerRefusalReason,
    message: string,
  ) {
    super(message);
  }
}

/** Thrown inside the hashing loop when the deadline passes or the signal aborts: the phase stops. */
class OutputPhaseStop extends Error {}

/** The reasons the worker may report for an output it dropped (the api's fixed allowlist). */
export type WorkerRefusalReason = JobRefusedOutput["reason"];

/** A file opened for upload: the handle every later read goes through, its exact size and digest. */
interface OpenedOutput {
  fh: FileHandle;
  size: number;
  sha256: string;
}

/** Open the workspace-relative output ONCE through openJobOutputFile (no blocking on a FIFO) and vet the handle before any byte is read:
 *  fstat must say a regular file with exactly one link (a hard link to a file outside outputs/ and
 *  sources/ passes the path checks, so it is refused), not empty, and within the per-file ceiling and
 *  what is left of the job's file and byte ceilings. Then hash it with a loop capped at size + 1
 *  bytes that checks `deadlineAt` and `signal` on every step, and re-fstat: a file whose size or
 *  mtime moved while it was read is refused. The handle stays open for the upload; the caller closes
 *  it. Throws OutputRefusal (drop this file) or OutputPhaseStop (stop the phase). */
async function openAndHash(
  work: string,
  relative: string,
  ceilings: { files: number; totalBytes: number },
  deadlineAt: number,
  signal: AbortSignal | undefined,
): Promise<OpenedOutput> {
  let fh: FileHandle;
  try {
    fh = await openJobOutputFile(work, relative);
  } catch (err) {
    throw new OutputRefusal("worker_unreadable", `could not be opened: ${errMessage(err)}`);
  }
  try {
    const st = await fh.stat();
    if (!st.isFile()) throw new OutputRefusal("worker_unreadable", "is not a regular file");
    if (st.nlink !== 1) throw new OutputRefusal("worker_unreadable", "has more than one hard link");
    const size = st.size;
    if (size === 0) throw new OutputRefusal("worker_empty", "it is empty");
    if (size > JOB_OUTPUT_CEILINGS.fileBytes) {
      throw new OutputRefusal("worker_too_large", `it is larger than the worker's per-file limit of ${JOB_OUTPUT_CEILINGS.fileBytes} bytes`);
    }
    if (ceilings.files >= JOB_OUTPUT_CEILINGS.files) {
      throw new OutputRefusal("worker_too_large", `the worker uploads at most ${JOB_OUTPUT_CEILINGS.files} files per job`);
    }
    if (ceilings.totalBytes + size > JOB_OUTPUT_CEILINGS.totalBytes) {
      throw new OutputRefusal("worker_too_large", `the worker uploads at most ${JOB_OUTPUT_CEILINGS.totalBytes} bytes per job`);
    }
    const hash = createHash("sha256");
    const buf = Buffer.allocUnsafe(HASH_STEP_BYTES);
    let pos = 0;
    while (pos < size + 1) {
      if (signal?.aborted || Date.now() >= deadlineAt) throw new OutputPhaseStop();
      const { bytesRead } = await fh.read(buf, 0, Math.min(buf.length, size + 1 - pos), pos);
      if (bytesRead === 0) break;
      hash.update(buf.subarray(0, bytesRead));
      pos += bytesRead;
    }
    const after = await fh.stat();
    if (pos !== size || after.size !== size || after.mtimeMs !== st.mtimeMs) {
      throw new OutputRefusal("worker_unreadable", "changed while it was being read");
    }
    return { fh, size, sha256: hash.digest("hex") };
  } catch (err) {
    await fh.close().catch(() => undefined);
    throw err;
  }
}

/** What the api said about a refusal (or a failure), reduced to its stable `reason`. */
function reasonOf(err: RequestError): string {
  try {
    const parsed = JSON.parse(err.body) as { reason?: unknown };
    if (typeof parsed.reason === "string") return parsed.reason.slice(0, 100);
  } catch {
    // not JSON: the status alone is the signal
  }
  return "";
}

/** The statuses that are the api refusing THIS file for its content or size: the api records the
 *  refusal itself; here it is logged, then the next file is tried. */
const FILE_REFUSAL_STATUSES: ReadonlySet<number> = new Set([413, 415, 422, 507]);

/** How many times one upload is attempted, and the waits between attempts (ms) when the api gave no
 *  Retry-After. Bounded: a file costs at most (attempts x its own timeout) + the waits, and the
 *  whole upload phase stops at its deadline. */
const OUTPUT_UPLOAD_ATTEMPTS = 3;
const OUTPUT_UPLOAD_RETRY_DELAYS_MS: readonly number[] = [1000, 3000];

/** The floor of one upload's timeout and the slowest rate it assumes (bytes per second): a file's
 *  timeout is max(floor, size / rate), bounded by the time left to the deadline. The api's read
 *  deadline for the same file is min(max(RequestDeadline, size / the same rate), 10 min) (workersvc
 *  UploadDeadline, ceiling MaxUploadDeadline). Both scale with size at the same rate, and the
 *  worker's floor (300 s) is above the api's default RequestDeadline (120 s), so the worker's
 *  per-attempt timeout is never shorter than the api's read deadline (a size whose api deadline
 *  hits the 10 min ceiling has size / rate >= 600 s here too); the api therefore does not cut a
 *  body off that the worker is still sending. The invariant assumes RequestDeadline <= 300 s. */
const UPLOAD_MIN_TIMEOUT_MS = 300_000;
const UPLOAD_MIN_RATE_BPS = 100 * 1024;

/** The upload phase's own slice after the model's session: the phase may run until this long after
 *  the job's wall-clock budget deadline (`budgetDeadlineAt`), so a model that finishes near its
 *  deadline still gets its files uploaded. It stays inside the api's backstop grace past the wall
 *  budget (jobWallBackstopGraceSeconds = 300 s in workersvc/job_sweeps.go), which leaves 60 s for
 *  the result post and the terminal report. */
export const JOB_UPLOAD_ALLOWANCE_MS = 240_000;

/** The epoch ms after which no upload starts or continues: JOB_UPLOAD_ALLOWANCE_MS past the job's
 *  wall-clock budget deadline. */
export function uploadPhaseDeadline(budgetDeadlineAt: number): number {
  return budgetDeadlineAt + JOB_UPLOAD_ALLOWANCE_MS;
}

export interface UploadJobOutputsArgs {
  client: Pick<WorkerClient, "uploadJobFile">;
  log: Logger;
  runId: string;
  generation: number;
  /** The workspace `work` dir: the base of `output_files` paths and the path-guard root. */
  workDir: string;
  secretPaths: readonly string[];
  /** The validated `output_files` the model listed. */
  outputFiles: readonly string[];
  /** Epoch ms after which no upload starts or continues (uploadPhaseDeadline). */
  deadlineAt: number;
  /** Aborts the phase (checked while hashing and passed to every upload). */
  signal?: AbortSignal;
  retryDelaysMs?: readonly number[];
}

/** What the upload phase did. */
export interface UploadJobOutputsSummary {
  stored: number;
  /** Files refused for any reason (the api's own refusals included). */
  refused: number;
  /** The files the WORKER dropped, for the result's `refused_outputs` (the api records them). The
   *  api's own refusals are recorded by the api and are not repeated here. */
  dropped: JobRefusedOutput[];
  /** Why the phase stopped early, if it did. */
  stopped?: string;
}

interface Candidate {
  label: string;
  displayName: string;
}

/** Upload the listed files. Never throws. */
export async function uploadJobOutputs(args: UploadJobOutputsArgs): Promise<UploadJobOutputsSummary> {
  const { log } = args;
  const summary: UploadJobOutputsSummary = { stored: 0, refused: 0, dropped: [] };
  const drop = (displayName: string, reason: WorkerRefusalReason): void => {
    summary.refused++;
    // The api bounds what it records; keep the list small and free of repeats here too.
    if (summary.dropped.length < OUTPUT_FILES_MAX && !summary.dropped.some((d) => d.display_name === displayName && d.reason === reason)) {
      summary.dropped.push({ display_name: displayName, reason });
    }
  };
  const candidates: Candidate[] = [];
  for (const rel of args.outputFiles) {
    if (candidates.some((c) => c.label === rel)) continue;
    candidates.push({ label: rel, displayName: outputDisplayName(rel) });
  }

  let files = 0;
  let totalBytes = 0;
  for (const [index, c] of candidates.entries()) {
    // The phase is over. Out of time or aborted, this file and every later one are dropped and
    // reported; on a stale claim or an api that takes no files nothing is (the result post is
    // refused the same way, or there is nowhere to record it).
    const stopPhase = (reason: string, report: boolean): void => {
      summary.stopped = reason;
      if (report) for (const rest of candidates.slice(index)) drop(rest.displayName, "worker_upload_failed");
    };
    if (args.signal?.aborted) {
      stopPhase("the upload phase was aborted", true);
      break;
    }
    if (Date.now() >= args.deadlineAt) {
      stopPhase("the upload phase's time is spent", true);
      break;
    }
    // Validate and open THIS candidate now. A resolve-all-first pass leaves later
    // candidates exposed to ancestor swaps while earlier uploads are in flight.
    const resolved = resolveOutputFile(args.workDir, c.label, args.secretPaths);
    if (!resolved.ok) {
      drop(c.displayName, "worker_unreadable");
      log.warn("job output file skipped", { error: logSafe(resolved.error) });
      continue;
    }
    let opened: OpenedOutput | undefined;
    try {
      opened = await openAndHash(args.workDir, c.label, { files, totalBytes }, args.deadlineAt, args.signal);
      const meta: JobFileUploadMeta = { claim_generation: args.generation, display_name: c.displayName, size: opened.size, sha256: opened.sha256 };
      const { fh, size } = opened;
      // The upload reads the handle the hash pass read: bounded to the hashed size, from offset 0
      // on every attempt, never closing the handle (closed below).
      const body = (): Readable => fh.createReadStream({ start: 0, end: size - 1, autoClose: false });
      const outcome = await uploadWithRetry(args, meta, body, c.label);
      if (outcome.kind === "stored") {
        summary.stored++;
        files++;
        totalBytes += size;
      } else if (outcome.kind === "refused") {
        summary.refused++;
        if (outcome.report) drop(c.displayName, outcome.report);
      } else {
        stopPhase(outcome.reason, outcome.deadline);
        break;
      }
    } catch (err) {
      if (err instanceof OutputPhaseStop) {
        stopPhase(args.signal?.aborted ? "the upload phase was aborted" : "the upload phase's time is spent", true);
        break;
      }
      if (err instanceof OutputRefusal) {
        drop(c.displayName, err.reason);
        log.warn("job output file skipped", { file: logSafe(c.displayName), reason: err.message });
        continue;
      }
      // A local read failure (the file vanished, unreadable): this file only.
      drop(c.displayName, "worker_unreadable");
      log.warn("job output file could not be read; skipped", { file: logSafe(c.displayName), error: logSafe(errMessage(err)) });
    } finally {
      await opened?.fh.close().catch(() => undefined);
    }
  }
  if (summary.stopped) log.warn("job output uploads stopped early", { reason: summary.stopped });
  log.info("job output uploads done", { stored: summary.stored, refused: summary.refused });
  return summary;
}

type UploadOutcome =
  | { kind: "stored" }
  /** `report`: the reason to tell the api when it did not record the refusal itself. */
  | { kind: "refused"; report?: WorkerRefusalReason }
  | { kind: "stop"; reason: string; deadline: boolean };

/** One file's upload with the bounded retry: a transport failure or a transient status (5xx incl.
 *  503 uploads_busy, 408, 429) is retried up to OUTPUT_UPLOAD_ATTEMPTS times, then counts as a
 *  refusal. A 503 uploads_busy waits the api's Retry-After (capped by the time left to the deadline)
 *  instead of the fixed delay. A content refusal (413, 415, 422, 507) is final and already recorded
 *  by the api. A 409, or a status meaning no upload of this run can succeed (403, 404, 405, 503
 *  files_unavailable), stops the whole phase. */
async function uploadWithRetry(
  args: UploadJobOutputsArgs,
  meta: JobFileUploadMeta,
  body: () => Readable,
  label: string,
): Promise<UploadOutcome> {
  const { log } = args;
  const delays = args.retryDelaysMs ?? OUTPUT_UPLOAD_RETRY_DELAYS_MS;
  let lastBusy = false;
  for (let attempt = 0; attempt < OUTPUT_UPLOAD_ATTEMPTS; attempt++) {
    const remaining = args.deadlineAt - Date.now();
    if (args.signal?.aborted) return { kind: "stop", reason: "the upload phase was aborted", deadline: true };
    if (remaining <= 0) return { kind: "stop", reason: "the upload phase's time is spent", deadline: true };
    const timeoutMs = Math.max(1, Math.min(Math.max(UPLOAD_MIN_TIMEOUT_MS, Math.ceil(meta.size / UPLOAD_MIN_RATE_BPS) * 1000), remaining));
    let retryAfterMs: number | undefined;
    try {
      const res = await args.client.uploadJobFile(args.runId, meta, body, args.signal, timeoutMs);
      log.info("job output stored", { file: logSafe(meta.display_name), bytes: meta.size, existing: res.status === 200 });
      return { kind: "stored" };
    } catch (err) {
      lastBusy = false;
      if (err instanceof RequestError) {
        const reason = reasonOf(err);
        if (err.status === 409) return { kind: "stop", reason: "the claim is stale or the run already finished", deadline: false };
        if (err.status === 403 || err.status === 404 || err.status === 405 || (err.status === 503 && reason === "files_unavailable")) {
          return { kind: "stop", reason: `the api does not take job files for this run (${err.status}${reason ? ` ${reason}` : ""})`, deadline: false };
        }
        if (FILE_REFUSAL_STATUSES.has(err.status)) {
          log.warn("job output file refused", { file: logSafe(meta.display_name), status: err.status, reason });
          return { kind: "refused" }; // the api recorded it
        }
        if (!isTransientStatus(err.status)) {
          log.warn("job output file rejected", { file: logSafe(meta.display_name), status: err.status, reason });
          return { kind: "refused", report: "worker_upload_failed" };
        }
        if (err.status === 503 && reason === "uploads_busy") {
          lastBusy = true;
          retryAfterMs = err.retryAfterHeaderMs;
        }
      }
      log.warn("job output upload failed", { file: logSafe(meta.display_name), attempt: attempt + 1, error: logSafe(errMessage(err)) });
      if (attempt + 1 >= OUTPUT_UPLOAD_ATTEMPTS) break;
      const wait = retryAfterMs ?? delays[attempt] ?? delays[delays.length - 1] ?? 0;
      await sleep(Math.min(wait, Math.max(0, args.deadlineAt - Date.now())), args.signal);
    }
  }
  log.warn("job output file not uploaded after retries", { file: logSafe(label) });
  return { kind: "refused", report: lastBusy ? "worker_busy" : "worker_upload_failed" };
}
