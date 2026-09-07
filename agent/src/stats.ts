// Container resource collector (PRD #49).
//
// The worker self-reports container-level CPU + memory on its existing heartbeat,
// read from its OWN cgroup v2 files under /sys/fs/cgroup — the one mechanism that
// works identically under docker-compose and kubelet, needs no docker socket, no
// metrics-server, and preserves the worker's outbound-only posture (Decision 1).
// One read covers the worker AND every SDK/git/devbox child, since they all live in
// the same container cgroup (Decision 2). When the v2 files are absent (cgroup v1
// host, or un-containerized dev) or masquerade as the host (cgroupns=host), it falls
// back to process-level counters and marks the sample source:"process".
//
// A collector failure must NEVER fail the heartbeat: collect() is internally guarded
// and returns undefined rather than throwing (the caller also guards).

import fs from "node:fs";
import os from "node:os";
import type { WorkerStats } from "./protocol.js";

/** Where cgroup v2 exposes this process's own limits + usage. */
const DEFAULT_CGROUP_ROOT = "/sys/fs/cgroup";
/** The cgroup v2 membership file. Its `0::<path>` entry tells us whether
 *  /sys/fs/cgroup reflects THIS container (see cgroupIsNamespaceRoot). */
const DEFAULT_PROC_CGROUP = "/proc/self/cgroup";
/** The nix-store volume mount, a fixed path in the worker image (no env var) — the
 *  volume the incident filled (PRD #837). Overridable only so the test can inject. */
const DEFAULT_NIX_PATH = "/nix";
/** The data volume mount when the collector is constructed without a dataDir; the
 *  real worker passes config.dataDir (default /data). */
const DEFAULT_DATA_PATH = "/data";

/** The subset of fs.StatFsBase this collector needs. Injected in tests as a fake so
 *  the disk math is exercised without a real filesystem, mirroring the now/cpuCount
 *  seams. `blocks`/`bfree`/`bavail` are in units of `bsize` bytes. */
export interface StatfsSample {
  bsize: number;
  blocks: number;
  bfree: number;
  bavail: number;
}

export interface StatsCollectorOptions {
  /** cgroup v2 hierarchy root; overridden in tests with a fixture tree. */
  cgroupRoot?: string;
  /** /proc/self/cgroup path; overridden in tests. */
  procCgroupPath?: string;
  /** Monotonic clock in nanoseconds. Injected for deterministic CPU-delta tests. */
  now?: () => bigint;
  /** Allowed-CPU denominator when the cgroup sets no quota (cpu.max = "max"). */
  cpuCount?: () => number;
  /** process.cpuUsage passthrough (µs user+system) for the process fallback. */
  processCpuUsage?: () => { user: number; system: number };
  /** process.memoryUsage().rss passthrough for the process fallback. */
  processRss?: () => number;
  /** statfs passthrough, injected in tests. Defaults to fs.statfsSync (Node 24, the
   *  `node:24-alpine` base image). Throws on a missing mount; each volume is guarded
   *  independently so one failure never drops the other or fails the heartbeat. */
  statfs?: (path: string) => StatfsSample;
  /** The nix-store mount to sample (PRD #837); defaults to the fixed /nix constant. */
  nixPath?: string;
  /** The data volume to sample; the real worker passes config.dataDir (default /data). */
  dataDir?: string;
}

/** A resolved cgroup reading before CPU% (which needs the prior sample). */
interface CgroupReading {
  memBytes: number;
  memLimit: number | null;
  /** Cumulative CPU microseconds (cgroup usage_usec, or process user+system). */
  cpuUsec: bigint;
  /** The allowed-CPU denominator for the CPU% normalization. */
  allowedCpus: number;
}

/**
 * Stateful per-worker collector: it carries the previous CPU counter + timestamp so
 * each tick's CPU% is a delta over the MEASURED monotonic elapsed time (never the
 * nominal heartbeat interval, which drifts with request latency / backoff). The
 * carry-over is kept per-source so a rare cgroup↔process flip re-omits cpu_pct that
 * tick rather than dividing a cgroup delta by a process baseline.
 */
export class StatsCollector {
  private readonly cgroupRoot: string;
  private readonly procCgroupPath: string;
  private readonly now: () => bigint;
  private readonly cpuCount: () => number;
  private readonly processCpuUsage: () => { user: number; system: number };
  private readonly processRss: () => number;
  private readonly statfs: (path: string) => StatfsSample;
  private readonly nixPath: string;
  private readonly dataDir: string;

  private prevSource?: WorkerStats["source"];
  private prevCpuUsec?: bigint;
  private prevTimeNs?: bigint;

  constructor(opts: StatsCollectorOptions = {}) {
    this.cgroupRoot = opts.cgroupRoot ?? DEFAULT_CGROUP_ROOT;
    this.procCgroupPath = opts.procCgroupPath ?? DEFAULT_PROC_CGROUP;
    this.now = opts.now ?? process.hrtime.bigint;
    this.cpuCount = opts.cpuCount ?? (() => os.cpus().length);
    this.processCpuUsage = opts.processCpuUsage ?? (() => process.cpuUsage());
    this.processRss = opts.processRss ?? (() => process.memoryUsage().rss);
    this.statfs = opts.statfs ?? ((p) => fs.statfsSync(p));
    this.nixPath = opts.nixPath ?? DEFAULT_NIX_PATH;
    this.dataDir = opts.dataDir ?? DEFAULT_DATA_PATH;
  }

  /**
   * One sample. Prefers the cgroup source; falls back to process counters when the
   * cgroup files are unavailable/malformed or masquerade as the host. Returns
   * undefined only if even the process fallback throws (it essentially never does) —
   * a defensive last resort so a collector failure can never surface to the caller.
   */
  collect(): WorkerStats | undefined {
    try {
      const cg = this.readCgroupSample();
      const reading: CgroupReading = cg ?? this.readProcessSample();
      const source: WorkerStats["source"] = cg ? "cgroup" : "process";
      const cpuPct = this.cpuPercent(source, reading.cpuUsec, reading.allowedCpus);

      const stats: WorkerStats = {
        mem_bytes: reading.memBytes,
        mem_limit_bytes: reading.memLimit,
        source,
      };
      if (cpuPct !== undefined) stats.cpu_pct = cpuPct;
      // Disk is an additive, independently-guarded sample (PRD #837 M1): a statfs
      // failure or an absent mount OMITS that volume's two fields and never disturbs
      // the mem/cpu reading above nor throws out of collect(). The mem reading is the
      // only thing whose failure returns undefined (drops the whole heartbeat).
      this.attachDisk(stats, "disk_nix_bytes", "disk_nix_total_bytes", this.nixPath);
      this.attachDisk(stats, "disk_data_bytes", "disk_data_total_bytes", this.dataDir);
      return stats;
    } catch {
      // The process fallback should not reach here; if it somehow did, drop the
      // sample rather than let the heartbeat fail.
      return undefined;
    }
  }

  /**
   * Sample one volume via statfs and, on success, set its used/total fields on the
   * stats object. total = blocks×bsize, used = (blocks−bfree)×bsize (both derived
   * from statfs's `bsize`-unit counts). We store used + total only — unprivileged-free
   * (bavail) is not needed by the DTO. On ANY failure (statfs throws for a missing
   * mount, or a non-finite/negative derivation) the two keys are left ABSENT, never
   * emitted as 0, so an absent field is distinct from an empty volume. Never throws.
   */
  private attachDisk(
    stats: WorkerStats,
    usedKey: "disk_nix_bytes" | "disk_data_bytes",
    totalKey: "disk_nix_total_bytes" | "disk_data_total_bytes",
    path: string,
  ): void {
    try {
      const { bsize, blocks, bfree } = this.statfs(path);
      const total = blocks * bsize;
      const used = (blocks - bfree) * bsize;
      if (!Number.isFinite(total) || !Number.isFinite(used) || total < 0 || used < 0) return;
      stats[usedKey] = used;
      stats[totalKey] = total;
    } catch {
      // Missing mount (dev/compose has no /nix) or a malformed statfs → omit the pair.
    }
  }

  /**
   * CPU% = Δ(cumulative CPU µs) ÷ measured elapsed µs ÷ allowed CPUs × 100. Needs a
   * prior sample of the SAME source, so cpu_pct is omitted on the first tick, on the
   * tick after a source flip, and on a backwards counter delta (a cgroup reset) —
   * reporting a meaningless value there is worse than omitting one. Does NOT clamp
   * high: the server clamps to [0, 6400] and the UI clamps the bar width.
   */
  private cpuPercent(source: WorkerStats["source"], cpuUsec: bigint, allowedCpus: number): number | undefined {
    const nowNs = this.now();
    let pct: number | undefined;
    if (
      this.prevSource === source &&
      this.prevTimeNs !== undefined &&
      this.prevCpuUsec !== undefined &&
      allowedCpus > 0
    ) {
      const elapsedNs = nowNs - this.prevTimeNs;
      const deltaUsec = Number(cpuUsec - this.prevCpuUsec);
      if (elapsedNs > 0n && deltaUsec >= 0) {
        const elapsedUsec = Number(elapsedNs) / 1000;
        const raw = (deltaUsec / elapsedUsec / allowedCpus) * 100;
        if (Number.isFinite(raw)) pct = Math.round(raw * 100) / 100;
      }
    }
    this.prevSource = source;
    this.prevCpuUsec = cpuUsec;
    this.prevTimeNs = nowNs;
    return pct;
  }

  /**
   * Read the container's own cgroup v2 sample, or null to signal the process
   * fallback. null when: the process's cgroup is not the namespace root (cgroupns=
   * host masquerade), or any required file is missing/malformed (cgroup v1, dev).
   */
  private readCgroupSample(): CgroupReading | null {
    try {
      if (!this.cgroupIsNamespaceRoot()) return null;
      const current = readNonNegInt(this.readFile("memory.current"));
      const inactiveFile = readMemStatField(this.readFile("memory.stat"), "inactive_file");
      const memLimit = parseMemMax(this.readFile("memory.max"));
      const usageUsec = readCpuStatField(this.readFile("cpu.stat"), "usage_usec");
      const allowedCpus = parseCpuMax(this.readFile("cpu.max"), this.cpuCount);
      return {
        memBytes: Math.max(0, current - inactiveFile),
        memLimit,
        cpuUsec: usageUsec,
        allowedCpus,
      };
    } catch {
      return null;
    }
  }

  /** The process-level fallback: this worker process only, children-blind, no known
   *  container limit (mem_limit null). Honest, and labeled source:"process". */
  private readProcessSample(): CgroupReading {
    const { user, system } = this.processCpuUsage();
    return {
      memBytes: this.processRss(),
      memLimit: null,
      cpuUsec: BigInt(Math.max(0, Math.trunc(user + system))),
      allowedCpus: Math.max(1, this.cpuCount()),
    };
  }

  /**
   * True when /proc/self/cgroup's v2 entry is `0::/` — the process's cgroup IS the
   * namespace root, so /sys/fs/cgroup is mounted at THIS container's own cgroup and
   * its files reflect the container (cgroupns=private, the Docker + kubelet default).
   *
   * A non-root path (`0::/docker/<id>`, `0::/kubepods/...`) means cgroupns=host: the
   * /sys/fs/cgroup mount is an ancestor (host root), whose numbers masquerade as the
   * container's (memory.max reads "max" though the container is capped), so we treat
   * cgroup data as unavailable and use the process fallback.
   *
   * NOTE: this is the INVERSE of PRD #49 Decision 2's literal wording, which had the
   * check backwards (it named 0::/ as the cgroupns=host masquerade case). Verified
   * against Docker 29.4.0: private ns → `0::/` with a real memory.max; host ns →
   * `0::/docker/<id>` with memory.max="max". Implementing it as the PRD reads would
   * force the process fallback for every normal worker and defeat the feature. Flagged
   * to the lead for a PRD text correction.
   */
  private cgroupIsNamespaceRoot(): boolean {
    const raw = fs.readFileSync(this.procCgroupPath, "utf8");
    for (const line of raw.split("\n")) {
      // v2 unified hierarchy line: "0::<path>" (a hybrid host also has "N:ctrl:/…").
      const m = /^0::(.*)$/.exec(line.trim());
      if (m) return m[1] === "/";
    }
    return false; // no v2 entry (pure cgroup v1) → fall back
  }

  private readFile(name: string): string {
    return fs.readFileSync(`${this.cgroupRoot}/${name}`, "utf8");
  }
}

/** Parse a non-negative integer that occupies the whole (trimmed) string. */
function readNonNegInt(raw: string): number {
  const n = Number(raw.trim());
  if (!Number.isFinite(n) || n < 0) throw new Error(`not a non-negative integer: ${JSON.stringify(raw)}`);
  return n;
}

/** memory.max is either "max" (no limit → null) or a positive byte count. */
function parseMemMax(raw: string): number | null {
  const s = raw.trim();
  if (s === "max") return null;
  const n = Number(s);
  if (!Number.isInteger(n) || n <= 0) throw new Error(`malformed memory.max: ${JSON.stringify(raw)}`);
  return n;
}

/**
 * cpu.max is "$MAX $PERIOD": "max PERIOD" (no quota → normalize by host cores) or
 * "QUOTA PERIOD" (allowed CPUs = QUOTA/PERIOD, e.g. "50000 100000" → 0.5). Anything
 * else is malformed and throws (→ the whole cgroup sample falls back).
 */
function parseCpuMax(raw: string, cpuCount: () => number): number {
  const parts = raw.trim().split(/\s+/);
  if (parts[0] === "max") return Math.max(1, cpuCount());
  const quota = Number(parts[0]);
  const period = Number(parts[1]);
  if (!Number.isInteger(quota) || !Number.isInteger(period) || quota <= 0 || period <= 0) {
    throw new Error(`malformed cpu.max: ${JSON.stringify(raw)}`);
  }
  return quota / period;
}

/** Read `inactive_file` from memory.stat (whitespace "key value" lines); 0 when the
 *  field is absent (accounted as no reclaimable cache) — a readable-but-fieldless
 *  file is not a fallback trigger. */
function readMemStatField(raw: string, field: string): number {
  const v = readKeyedField(raw, field);
  if (v === undefined) return 0;
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0) throw new Error(`malformed ${field} in memory.stat`);
  return n;
}

/** Read `usage_usec` from cpu.stat as a bigint (cumulative µs). Throws if absent or
 *  malformed → cgroup sample falls back. */
function readCpuStatField(raw: string, field: string): bigint {
  const v = readKeyedField(raw, field);
  if (v === undefined) throw new Error(`missing ${field} in cpu.stat`);
  if (!/^\d+$/.test(v)) throw new Error(`malformed ${field} in cpu.stat`);
  return BigInt(v);
}

/** First value of a "key value" line in a cgroup stat file, or undefined. */
function readKeyedField(raw: string, field: string): string | undefined {
  for (const line of raw.split("\n")) {
    const parts = line.trim().split(/\s+/);
    if (parts[0] === field && parts[1] !== undefined) return parts[1];
  }
  return undefined;
}
