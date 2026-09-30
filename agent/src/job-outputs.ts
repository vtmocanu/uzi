// The output files of a job (PRD #1909 M4, D4/D6). Before it posts the result, the JobRunner
// uploads what the job produced so the caller can download it: `report.md` (the report text),
// `findings.json` (the findings) and every file the model listed in submit_job_result's
// `output_files` (workspace-relative paths under `outputs/` or `sources/`).
//
// Nothing here can fail the job. A file the api refuses (413 limit, 415 type, 422 integrity, 507
// quota) is logged and the next one is tried; a transport failure is retried with a short bounded
// backoff and then counts as a refusal; a stale claim or a finished run (409), or a run the api
// will not take files for (403/404/405, files_unavailable), stops the uploads, because every later
// one would be refused the same way (the result post that follows is refused the same way too).
//
// Files are streamed from disk, never buffered whole: a read pass computes the SHA-256 the api
// verifies, and the upload re-opens the file. The report and findings are already in memory.

import { createHash } from "node:crypto";
import { constants as fsc, lstatSync, realpathSync } from "node:fs";
import { open } from "node:fs/promises";
import path from "node:path";
import type { Readable } from "node:stream";

import { isTransientStatus, RequestError, type WorkerClient } from "./client.js";
import { screenToolPath } from "./guardrails.js";
import type { Logger } from "./log.js";
import type { JobFileUploadMeta } from "./protocol.js";
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

/** Read the file without following a final symlink. */
const READ_FLAGS = fsc.O_RDONLY | fsc.O_NOFOLLOW;

const byteLen = (s: string): number => Buffer.byteLength(s, "utf8");

/** The lexical (no filesystem) check of the `output_files` a model submits: an array of at most
 *  OUTPUT_FILES_MAX workspace-relative POSIX paths, each under `outputs/` or `sources/` with at
 *  least one more segment, no absolute or home-relative form, no `.` or `..` or empty segment, no
 *  backslash, control character or NUL. Duplicates are dropped (first wins, order kept). */
export function validateOutputFilePaths(raw: unknown): { ok: true; value: string[] } | { ok: false; error: string } {
  if (raw === undefined || raw === null) return { ok: true, value: [] };
  if (!Array.isArray(raw)) return { ok: false, error: "output_files must be an array of workspace-relative paths" };
  if (raw.length > OUTPUT_FILES_MAX) return { ok: false, error: `output_files lists at most ${OUTPUT_FILES_MAX} files` };
  const out: string[] = [];
  const seen = new Set<string>();
  for (const [i, p] of raw.entries()) {
    if (typeof p !== "string" || p === "") return { ok: false, error: `output_files[${i}] must be a non-empty string` };
    if (byteLen(p) > OUTPUT_PATH_MAX_BYTES) return { ok: false, error: `output_files[${i}] is longer than ${OUTPUT_PATH_MAX_BYTES} bytes` };
    // eslint-disable-next-line no-control-regex
    if (/[\\\u0000-\u001f\u007f]/.test(p)) return { ok: false, error: `output_files[${i}] must not contain a backslash or a control character` };
    if (p.startsWith("/") || p.startsWith("~")) return { ok: false, error: `output_files[${i}] must be relative to the workspace, not absolute` };
    const segments = p.split("/");
    if (segments.some((s) => s === "" || s === "." || s === "..")) {
      return { ok: false, error: `output_files[${i}] must not contain '.', '..' or empty path segments` };
    }
    if (segments.length < 2 || !OUTPUT_DIRS.includes(segments[0]!)) {
      return { ok: false, error: `output_files[${i}] must be a file under outputs/ or sources/` };
    }
    if (!seen.has(p)) {
      seen.add(p);
      out.push(p);
    }
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
 *  REAL path (every symlink resolved) must lie inside `<work>/outputs` or `<work>/sources` and be a
 *  regular file. A symlink that points anywhere else, a directory, a FIFO or a device is refused.
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

/** Open a stream over a file's bytes without following a final symlink (the handle closes with the
 *  stream). */
async function openReadStream(real: string): Promise<Readable> {
  return (await open(real, READ_FLAGS)).createReadStream();
}

/** Stream a file once for its SHA-256 and exact size (the api verifies both). */
async function hashFile(real: string): Promise<{ sha256: string; size: number }> {
  const hash = createHash("sha256");
  let size = 0;
  for await (const chunk of await openReadStream(real)) {
    hash.update(chunk);
    size += chunk.length;
  }
  return { sha256: hash.digest("hex"), size };
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

/** The statuses that are the api refusing THIS file for its content or size: logged, then the next
 *  file is tried. */
const FILE_REFUSAL_STATUSES: ReadonlySet<number> = new Set([413, 415, 422, 507]);

/** How many times one upload is attempted, and the waits between attempts (ms). Bounded: a file
 *  costs at most (attempts x its own timeout) + the waits, and the whole upload phase stops at the
 *  job's wall-clock deadline. */
const OUTPUT_UPLOAD_ATTEMPTS = 3;
const OUTPUT_UPLOAD_RETRY_DELAYS_MS: readonly number[] = [1000, 3000];

/** The floor of one upload's timeout and the slowest rate it assumes (bytes per second): a file's
 *  timeout is max(floor, size / rate), bounded by the time left to the deadline. */
const UPLOAD_MIN_TIMEOUT_MS = 300_000;
const UPLOAD_MIN_RATE_BPS = 100 * 1024;

export interface UploadJobOutputsArgs {
  client: Pick<WorkerClient, "uploadJobFile">;
  log: Logger;
  runId: string;
  generation: number;
  /** The workspace `work` dir: the base of `output_files` paths and the path-guard root. */
  workDir: string;
  secretPaths: readonly string[];
  /** The report text as it will be posted (already redacted). */
  reportMd: string;
  /** The findings as JSON text (already redacted). */
  findingsJson: string;
  /** The validated `output_files` the model listed. */
  outputFiles: readonly string[];
  /** Epoch ms after which no upload starts or continues (the job's wall-clock deadline). */
  deadlineAt: number;
  retryDelaysMs?: readonly number[];
}

/** What the upload phase did. */
export interface UploadJobOutputsSummary {
  stored: number;
  refused: number;
  /** Why the phase stopped early, if it did. */
  stopped?: string;
}

type Candidate =
  | { label: string; displayName: string; buf: Buffer }
  | { label: string; displayName: string; real: string };

/** Upload report.md, findings.json and the listed files. Never throws. */
export async function uploadJobOutputs(args: UploadJobOutputsArgs): Promise<UploadJobOutputsSummary> {
  const { log } = args;
  const summary: UploadJobOutputsSummary = { stored: 0, refused: 0 };
  const candidates: Candidate[] = [];
  const report = Buffer.from(args.reportMd, "utf8");
  if (report.length > 0) candidates.push({ label: "report.md", displayName: "report.md", buf: report });
  candidates.push({
    label: "findings.json",
    displayName: "findings.json",
    buf: Buffer.from(args.findingsJson, "utf8"),
  });
  for (const rel of args.outputFiles) {
    const r = resolveOutputFile(args.workDir, rel, args.secretPaths);
    if (!r.ok) {
      summary.refused++;
      log.warn("job output file skipped", { error: r.error.slice(0, 300) });
      continue;
    }
    if (candidates.some((c) => "real" in c && c.real === r.value.real)) continue; // the same file by two paths
    candidates.push({ label: rel, displayName: r.value.displayName, real: r.value.real });
  }

  let files = 0;
  let totalBytes = 0;
  for (const c of candidates) {
    if (Date.now() >= args.deadlineAt) {
      summary.stopped = "the job's wall-clock budget is spent";
      break;
    }
    try {
      let size: number;
      let sha256: string;
      if ("buf" in c) {
        size = c.buf.length;
        sha256 = createHash("sha256").update(c.buf).digest("hex");
      } else {
        ({ sha256, size } = await hashFile(c.real));
      }
      const skip =
        size === 0
          ? "it is empty"
          : size > JOB_OUTPUT_CEILINGS.fileBytes
            ? `it is larger than the worker's per-file limit of ${JOB_OUTPUT_CEILINGS.fileBytes} bytes`
            : files >= JOB_OUTPUT_CEILINGS.files
              ? `the worker uploads at most ${JOB_OUTPUT_CEILINGS.files} files per job`
              : totalBytes + size > JOB_OUTPUT_CEILINGS.totalBytes
                ? `the worker uploads at most ${JOB_OUTPUT_CEILINGS.totalBytes} bytes per job`
                : "";
      if (skip) {
        summary.refused++;
        log.warn("job output file skipped", { file: c.displayName, reason: skip });
        continue;
      }
      const meta: JobFileUploadMeta = { claim_generation: args.generation, display_name: c.displayName, size, sha256 };
      const body: UploadBody = "buf" in c ? c.buf : () => openReadStream(c.real);
      const outcome = await uploadWithRetry(args, meta, body, c.label);
      if (outcome.kind === "stored") {
        summary.stored++;
        files++;
        totalBytes += size;
      } else if (outcome.kind === "refused") {
        summary.refused++;
      } else {
        summary.stopped = outcome.reason;
        break;
      }
    } catch (err) {
      // A local read failure (the file vanished, unreadable): this file only.
      summary.refused++;
      log.warn("job output file could not be read; skipped", { file: c.displayName, error: errMessage(err).slice(0, 300) });
    }
  }
  if (summary.stopped) log.warn("job output uploads stopped early", { reason: summary.stopped });
  log.info("job output uploads done", { stored: summary.stored, refused: summary.refused });
  return summary;
}

/** A Buffer (in memory already) or a factory opening a fresh stream per attempt. */
type UploadBody = Buffer | (() => Readable | Promise<Readable>);

type UploadOutcome = { kind: "stored" } | { kind: "refused" } | { kind: "stop"; reason: string };

/** One file's upload with the bounded retry: a transport failure or a transient status (5xx incl.
 *  503 uploads_busy, 408, 429) is retried up to OUTPUT_UPLOAD_ATTEMPTS times, then counts as a
 *  refusal. A content refusal is final. A 409, or a status meaning no upload of this run can
 *  succeed (403, 404, 405, 503 files_unavailable), stops the whole phase. */
async function uploadWithRetry(
  args: UploadJobOutputsArgs,
  meta: JobFileUploadMeta,
  body: UploadBody,
  label: string,
): Promise<UploadOutcome> {
  const { log } = args;
  const delays = args.retryDelaysMs ?? OUTPUT_UPLOAD_RETRY_DELAYS_MS;
  for (let attempt = 0; attempt < OUTPUT_UPLOAD_ATTEMPTS; attempt++) {
    const remaining = args.deadlineAt - Date.now();
    if (remaining <= 0) return { kind: "stop", reason: "the job's wall-clock budget is spent" };
    const timeoutMs = Math.max(1, Math.min(Math.max(UPLOAD_MIN_TIMEOUT_MS, Math.ceil(meta.size / UPLOAD_MIN_RATE_BPS) * 1000), remaining));
    try {
      const res = await args.client.uploadJobFile(args.runId, meta, body, undefined, timeoutMs);
      log.info("job output stored", { file: meta.display_name, bytes: meta.size, existing: res.status === 200 });
      return { kind: "stored" };
    } catch (err) {
      if (err instanceof RequestError) {
        const reason = reasonOf(err);
        if (err.status === 409) return { kind: "stop", reason: "the claim is stale or the run already finished" };
        if (err.status === 403 || err.status === 404 || err.status === 405 || (err.status === 503 && reason === "files_unavailable")) {
          return { kind: "stop", reason: `the api does not take job files for this run (${err.status}${reason ? ` ${reason}` : ""})` };
        }
        if (FILE_REFUSAL_STATUSES.has(err.status)) {
          log.warn("job output file refused", { file: meta.display_name, status: err.status, reason });
          return { kind: "refused" };
        }
        if (!isTransientStatus(err.status)) {
          log.warn("job output file rejected", { file: meta.display_name, status: err.status, reason });
          return { kind: "refused" };
        }
      }
      log.warn("job output upload failed", { file: meta.display_name, attempt: attempt + 1, error: errMessage(err).slice(0, 300) });
      if (attempt + 1 >= OUTPUT_UPLOAD_ATTEMPTS) break;
      await sleep(Math.min(delays[attempt] ?? delays[delays.length - 1] ?? 0, Math.max(0, args.deadlineAt - Date.now())));
    }
  }
  log.warn("job output file not uploaded after retries", { file: label });
  return { kind: "refused" };
}
