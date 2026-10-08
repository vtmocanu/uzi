import fs from "node:fs";
import { performance } from "node:perf_hooks";

type UnavailableReason = "absent" | "malformed" | "unlimited" | "unverified" | "oversized";
type Psi = { avg10: number; avg60: number; avg300: number; total: number };
type Sample = {
  sampleId: number;
  startedAtMs: number;
  completedAtMs: number;
} & ({
  available: true;
  currentBytes: number;
  limitBytes: number;
  stat: { anon: number; shmem: number; slab_unreclaimable: number; unevictable: number };
  pressure: { some: Psi; full: Psi };
} | { available: false; reason: UnavailableReason });

const MAX_SAMPLE_BYTES = 64 * 1024;

/** Reads at most maxBytes + one overflow sentinel byte, before decoding text.
 * Short reads retry only while making progress, bounded by maxBytes + 1 iterations.
 * Any failure aborts this sample; no sibling file or process fallback is attempted.
 */
function boundedRead(file: string, maxBytes: number): Uint8Array {
  const fd = fs.openSync(file, "r");
  try {
    const bytes = Buffer.alloc(maxBytes + 1);
    let used = 0;
    while (used < bytes.length) {
      const count = fs.readSync(fd, bytes, used, bytes.length - used, null);
      if (count === 0) break;
      used += count;
    }
    return bytes.subarray(0, used);
  } finally {
    fs.closeSync(fd);
  }
}

function integer(raw: string): number {
  if (!/^[0-9]+$/.test(raw)) throw new Error("malformed");
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < 0) throw new Error("malformed");
  return value;
}

function stat(raw: string): Extract<Sample, { available: true }>["stat"] {
  const required = ["anon", "shmem", "slab_unreclaimable", "unevictable"] as const;
  const fields = new Map<string, number>();
  for (const line of raw.trim().split("\n")) {
    const parts = line.trim().split(/\s+/);
    if (!required.some((key) => key === parts[0])) continue;
    if (parts.length !== 2 || fields.has(parts[0])) throw new Error("malformed");
    fields.set(parts[0], integer(parts[1]));
  }
  for (const key of required) if (!fields.has(key)) throw new Error("malformed");
  return {
    anon: fields.get("anon")!,
    shmem: fields.get("shmem")!,
    slab_unreclaimable: fields.get("slab_unreclaimable")!,
    unevictable: fields.get("unevictable")!,
  };
}

function pressure(raw: string): { some: Psi; full: Psi } {
  const rows = new Map<string, Psi>();
  for (const line of raw.trim().split("\n")) {
    const [kind, ...parts] = line.trim().split(/\s+/);
    if ((kind !== "some" && kind !== "full") || rows.has(kind) || parts.length !== 4) {
      throw new Error("malformed");
    }
    const fields = new Map<string, number>();
    for (const part of parts) {
      const pair = part.split("=");
      const [key, value] = pair;
      if (pair.length !== 2 || fields.has(key)) throw new Error("malformed");
      if (key === "total") fields.set(key, integer(value));
      else {
        if (!["avg10", "avg60", "avg300"].includes(key) || !/^[0-9]+(?:\.[0-9]+)?$/.test(value)) {
          throw new Error("malformed");
        }
        const n = Number(value);
        if (!Number.isFinite(n) || n < 0 || n > 100) throw new Error("malformed");
        fields.set(key, n);
      }
    }
    if (!["avg10", "avg60", "avg300", "total"].every((key) => fields.has(key))) {
      throw new Error("malformed");
    }
    rows.set(kind, {
      avg10: fields.get("avg10")!, avg60: fields.get("avg60")!,
      avg300: fields.get("avg300")!, total: fields.get("total")!,
    });
  }
  if (!rows.has("some") || !rows.has("full")) throw new Error("malformed");
  return { some: rows.get("some")!, full: rows.get("full")! };
}

/** Total cgroup v2 memory, independent of StatsCollector's display accounting.
 * readBytes must enforce its byte budget before allocating/decoding file contents;
 * returning one sentinel byte beyond the budget signals oversized input.
 * Times use the injected monotonic clock's domain. Consumers can measure age from
 * startedAtMs (oldest read), and duration from completedAtMs - startedAtMs.
 * sampleId increases per attempt within this reader, including unavailable samples.
 */
export class WorkerMemoryReader {
  private sequence = 0;
  constructor(private readonly options: {
    readBytes?: (file: string, maxBytes: number) => Uint8Array;
    now?: () => number;
    cgroupRoot?: string;
    procCgroupPath?: string;
  } = {}) {}

  sample(): Sample {
    const now = this.options.now ?? (() => performance.now());
    const startedAtMs = now();
    const sampleId = ++this.sequence;
    let remaining = MAX_SAMPLE_BYTES;
    let reason: UnavailableReason = "malformed";
    const read = (file: string): string => {
      let bytes: Uint8Array;
      try {
        bytes = (this.options.readBytes ?? boundedRead)(file, remaining);
      } catch {
        reason = "absent";
        throw new Error("absent");
      }
      if (bytes.byteLength > remaining) {
        reason = "oversized";
        throw new Error("oversized");
      }
      remaining -= bytes.byteLength;
      return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    };
    try {
      const membership = read(this.options.procCgroupPath ?? "/proc/self/cgroup");
      const unified = membership.split("\n").filter((line) => line.startsWith("0:"));
      if (unified.length !== 1 || unified[0] !== "0::/") {
        reason = "unverified";
        throw new Error("unverified");
      }
      const root = this.options.cgroupRoot ?? "/sys/fs/cgroup";
      const currentBytes = integer(read(`${root}/memory.current`).trim());
      const limit = read(`${root}/memory.max`).trim();
      if (limit === "max") {
        reason = "unlimited";
        throw new Error("unlimited");
      }
      const limitBytes = integer(limit);
      if (limitBytes === 0) throw new Error("malformed");
      const memoryStat = stat(read(`${root}/memory.stat`));
      const memoryPressure = pressure(read(`${root}/memory.pressure`));
      return {
        available: true, sampleId, startedAtMs, completedAtMs: now(),
        currentBytes, limitBytes, stat: memoryStat, pressure: memoryPressure,
      };
    } catch {
      return { available: false, reason, sampleId, startedAtMs, completedAtMs: now() };
    }
  }
}
