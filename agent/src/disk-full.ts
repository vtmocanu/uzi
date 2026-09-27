// PRD #1809 D6: a full data volume is a recoverable, bounded condition.
//
// This module decides whether a failed write was the WORKER'S DATA VOLUME filling up, as opposed
// to any other write failure. A failure counts as data-volume disk-full only when all three hold:
//
//   (a) a recognised signal: Node `ENOSPC` / `EDQUOT` (on the error or anywhere on its `cause`
//       chain), or git's `No space left on device` / `Disk quota exceeded` / `unable to write`
//       diagnostics in the error text or its captured stderr;
//   (b) the failing operation's destination is on the data volume. The caller names the path the
//       operation writes (the worker bare, the outbox dir, the run HOME). Attribution compares
//       device ids: `stat(destination).dev === stat(dataDir).dev`. A device id, not a path prefix,
//       because a path under `dataDir` can still be a different mount (a volume mounted inside
//       the data dir), and a path outside it can be the same filesystem. A destination that does
//       not exist yet (a bare a failed `git clone` removed again) is attributed through its
//       nearest existing ancestor, which is the directory the write was going into;
//   (c) a statfs of that volume shows bytes available below a floor, or free inodes below a floor.
//
// A statfs failure, or a filesystem that keeps no inode accounting (`files` 0), is "unknown":
// the caller keeps today's handling. No signal, another mount, or a volume with room is
// "not_disk_full". Only the full verdict changes behaviour (the runner then runs the D7 reclaim,
// retries once, and parks the run in recovery_wait with the data_volume_full cause).

import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import { defaultStatfs, sampleVolume, type StatfsSample, type VolumeSample } from "./stats.js";

export type DiskFullVerdict = "data_volume_full" | "not_disk_full" | "unknown";

/** The byte floor: below max(256 MiB, 1% of the volume) available, the volume is full. */
export const DISK_FULL_MIN_FREE_BYTES = 256 * 1024 * 1024;
const DISK_FULL_FREE_FRACTION = 0.01;
/** The inode floor: below max(1000, 1% of the inodes) free, the volume is full. */
export const DISK_FULL_MIN_FREE_INODES = 1000;

const SIGNAL_CODES = new Set(["ENOSPC", "EDQUOT"]);
const SIGNAL_TEXT = [/No space left on device/i, /Disk quota exceeded/i, /unable to write/i];

/** Bound on the `cause` chain walk, so a cyclic chain cannot loop. */
const MAX_CAUSE_DEPTH = 8;

/** Whether `err` carries a recognised disk-full signal (a). Never throws. */
export function hasDiskFullSignal(err: unknown): boolean {
  let cur: unknown = err;
  for (let depth = 0; depth < MAX_CAUSE_DEPTH && cur !== undefined && cur !== null; depth++) {
    if (typeof cur === "object") {
      const e = cur as { code?: unknown; message?: unknown; stderr?: unknown; cause?: unknown };
      if (typeof e.code === "string" && SIGNAL_CODES.has(e.code)) return true;
      for (const text of [e.message, e.stderr]) {
        if (typeof text === "string" && SIGNAL_TEXT.some((re) => re.test(text))) return true;
      }
      cur = e.cause;
    } else {
      return typeof cur === "string" && SIGNAL_TEXT.some((re) => re.test(cur as string));
    }
  }
  return false;
}

/**
 * Whether a volume sample is below either floor (c). Undefined (unknown) when the filesystem
 * keeps no inode accounting (`inodesTotal` 0) or reports a zero size: the verdict then cannot
 * be trusted either way.
 */
export function volumeBelowFloor(s: VolumeSample): boolean | undefined {
  if (!(s.bytesTotal > 0) || !(s.inodesTotal > 0)) return undefined;
  const byteFloor = Math.max(DISK_FULL_MIN_FREE_BYTES, s.bytesTotal * DISK_FULL_FREE_FRACTION);
  const inodeFloor = Math.max(DISK_FULL_MIN_FREE_INODES, s.inodesTotal * DISK_FULL_FREE_FRACTION);
  return s.bytesAvailable < byteFloor || s.inodesFree < inodeFloor;
}

/**
 * PRD #1809 D6: a write to the data volume failed disk-full after a reclaim and one retry. The
 * runner parks the run in `recovery_wait` with the `data_volume_full` cause on it. `operation`
 * names what failed (for logs only); the message is the original error text, never a secret.
 */
export class DataVolumeFullError extends Error {
  constructor(
    readonly operation: string,
    cause?: unknown,
  ) {
    super(
      `the worker's data volume is full (${operation})${cause instanceof Error ? `: ${cause.message}` : ""}`,
      { cause },
    );
    this.name = "DataVolumeFullError";
  }
}

export interface DataVolumeGuardOptions {
  /** The worker's data volume mount (config.dataDir). */
  dataDir: string;
  /** statfs seam (default fs.statfsSync). */
  statfs?: (p: string) => StatfsSample;
  /** stat seam for the device-id attribution (default fs.promises.stat). */
  stat?: (p: string) => Promise<{ dev: number }>;
  /**
   * Run one D7 reclaim pass and wait for it (production: DiskPressureController.reclaimNow).
   * Undefined when the reclaim is off; {@link DataVolumeGuard.reclaim} then returns at once.
   */
  reclaim?: () => Promise<void>;
  log?: Logger;
}

/**
 * PRD #1809 D6: the runner's handle on its data volume: classify a failed write, preflight the
 * volume before a clone, and run the D7 reclaim before the one retry. Every method never throws.
 */
export class DataVolumeGuard {
  private readonly statfs: (p: string) => StatfsSample;
  private readonly stat: (p: string) => Promise<{ dev: number }>;

  constructor(private readonly opts: DataVolumeGuardOptions) {
    this.statfs = opts.statfs ?? defaultStatfs;
    this.stat = opts.stat ?? (async (p) => fs.stat(p));
  }

  get dataDir(): string {
    return this.opts.dataDir;
  }

  /** Classify a failed write whose destination is `destination`. See the module header. */
  async classify(err: unknown, destination: string): Promise<DiskFullVerdict> {
    if (!hasDiskFullSignal(err)) return "not_disk_full";
    let dataDev: number;
    try {
      dataDev = (await this.stat(this.opts.dataDir)).dev;
    } catch {
      return "unknown";
    }
    const destDev = await this.nearestDev(destination);
    if (destDev === undefined) return "unknown";
    if (destDev !== dataDev) return "not_disk_full";
    return this.volumeVerdict();
  }

  /**
   * The preflight at claim and resume: whether the data volume is below a floor right now.
   * "unknown" on a statfs failure or a filesystem without inode accounting.
   */
  preflight(): DiskFullVerdict {
    return this.volumeVerdict();
  }

  /** Run one D7 reclaim pass and wait for it. Never throws. */
  async reclaim(): Promise<void> {
    if (!this.opts.reclaim) return;
    try {
      await this.opts.reclaim();
    } catch (err) {
      this.opts.log?.warn("disk-full reclaim pass failed", { error: err instanceof Error ? err.message : String(err) });
    }
  }

  private volumeVerdict(): DiskFullVerdict {
    const sample = sampleVolume(this.opts.dataDir, this.statfs);
    if (!sample) return "unknown";
    const below = volumeBelowFloor(sample);
    if (below === undefined) return "unknown";
    return below ? "data_volume_full" : "not_disk_full";
  }

  /** The device id of `p`, or of its nearest existing ancestor; undefined when none stats. */
  private async nearestDev(p: string): Promise<number | undefined> {
    let cur = path.resolve(p);
    for (;;) {
      try {
        return (await this.stat(cur)).dev;
      } catch {
        const parent = path.dirname(cur);
        if (parent === cur) return undefined;
        cur = parent;
      }
    }
  }
}
