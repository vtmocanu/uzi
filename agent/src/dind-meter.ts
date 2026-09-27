// DinD data-root meter reader (issue #1759 M3).
//
// A docker-tier hosted worker's DinD daemon keeps its data root on the `dind-data`
// volume, which the worker does NOT mount. The controller renders a `dind-meter`
// sidecar that mounts that volume read-only, samples statfs every 30s and publishes
// one line into a private emptyDir the worker mounts read-only. This module parses it.
//
// The file is UNTRUSTED input: in the privileged daemon posture a node-root actor could
// rewrite it, so the reader bounds the read, refuses anything that is not a regular
// file, matches the whole line against one strict pattern, rejects every integer that
// is not exactly representable, rejects impossible statfs relations, and rejects a
// sample that is stale or from the future. Every rejection is `null` (absent), never a
// throw, and the raw line is never logged or returned.

import fs from "node:fs";

/**
 * Where the dind-meter sidecar publishes its sample. COUPLED TO
 * controller/internal/kube/render_dind.go `dindMeterFile` (dindMeterDir + "/statfs"):
 * change both together. The line shape is versioned by its leading `v1` token.
 */
const DIND_METER_FILE = "/run/uzi-dind-meter/statfs";

/** The largest file the reader accepts. A valid v1 line is at most 3 + 6×20 = 123
 *  bytes, so anything past this bound is not a v1 sample. */
const MAX_SAMPLE_BYTES = 256;

/** A sample older than this is stale (three of the meter's 30s intervals, matching the
 *  freshness bound documented beside dindMeterFile): the meter leaves the previous file
 *  in place on a failed statfs, so an old file is "no sample", not "last known". */
const MAX_SAMPLE_AGE_MS = 90_000;

/** Clock skew tolerated between the meter's `date +%s` and the worker's clock. A
 *  sample dated further in the future than this is rejected as forged or broken. */
const MAX_FUTURE_SKEW_MS = 5_000;

/** The whole v1 line: six base-10 non-negative integers without leading zeros, at most
 *  19 digits each, single-space separated, an optional trailing newline, nothing else. */
const V1_LINE = /^v1( (0|[1-9][0-9]{0,18})){6}\n?$/;

const MAX_SAFE = BigInt(Number.MAX_SAFE_INTEGER);

/** A validated, fresh dind-data sample in derived units. Every field is a safe integer. */
export interface DindMeterSample {
  /** The meter's sampling time, epoch seconds. Distinct samples have distinct epochs. */
  epochS: number;
  /** (blocks − bfree) × frsize. */
  bytesUsed: number;
  /** blocks × frsize (> 0). */
  bytesTotal: number;
  /** files − ffree. */
  inodesUsed: number;
  /** files (> 0). */
  inodesTotal: number;
}

export interface ReadDindMeterOptions {
  /** The sample file; defaults to {@link DIND_METER_FILE}. */
  path?: string;
  /** Reads at most `maxBytes + 1` bytes of the file, or returns null when it is missing,
   *  unreadable or not a regular file. Injected in tests. */
  readFile?: (path: string, maxBytes: number) => string | null;
  /** Wall clock, epoch milliseconds. Injected in tests. */
  nowMs?: () => number;
}

/**
 * Read and validate the current dind-data sample. Returns null when the file is missing,
 * unreadable, not a regular file, oversized, malformed, internally inconsistent, stale
 * (older than 90s) or dated more than 5s in the future. Never throws.
 */
export function readDindMeterSample(opts: ReadDindMeterOptions = {}): DindMeterSample | null {
  try {
    const raw = (opts.readFile ?? readBounded)(opts.path ?? DIND_METER_FILE, MAX_SAMPLE_BYTES);
    if (raw === null) return null;
    return parseV1(raw, (opts.nowMs ?? Date.now)());
  } catch {
    return null;
  }
}

/** Parse one v1 line against the clock. null on any violation. */
function parseV1(raw: string, nowMs: number): DindMeterSample | null {
  if (raw.length > MAX_SAMPLE_BYTES || !V1_LINE.test(raw)) return null;
  const nums: bigint[] = [];
  for (const tok of raw.trim().split(" ").slice(1)) {
    const n = BigInt(tok);
    if (n > MAX_SAFE) return null;
    nums.push(n);
  }
  const [epochS, frsize, blocks, bfree, files, ffree] = nums as [bigint, bigint, bigint, bigint, bigint, bigint];
  if (frsize <= 0n || blocks <= 0n || files <= 0n || bfree > blocks || ffree > files) return null;
  const bytesTotal = blocks * frsize;
  if (bytesTotal > MAX_SAFE) return null;
  const epochMs = Number(epochS) * 1000;
  if (epochMs > nowMs + MAX_FUTURE_SKEW_MS) return null;
  if (nowMs - epochMs > MAX_SAMPLE_AGE_MS) return null;
  return {
    epochS: Number(epochS),
    bytesUsed: Number((blocks - bfree) * frsize),
    bytesTotal: Number(bytesTotal),
    inodesUsed: Number(files - ffree),
    inodesTotal: Number(files),
  };
}

/**
 * Read at most `maxBytes + 1` bytes (one past the bound, so an oversized file is
 * detectable). O_NOFOLLOW refuses a planted symlink; O_NONBLOCK plus the regular-file
 * check keep a planted FIFO from blocking the event loop on this synchronous read.
 */
function readBounded(path: string, maxBytes: number): string | null {
  let fd: number;
  try {
    fd = fs.openSync(path, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  } catch {
    return null;
  }
  try {
    if (!fs.fstatSync(fd).isFile()) return null;
    const buf = Buffer.alloc(maxBytes + 1);
    const n = fs.readSync(fd, buf, 0, buf.length, 0);
    return buf.subarray(0, n).toString("utf8");
  } catch {
    return null;
  } finally {
    fs.closeSync(fd);
  }
}
