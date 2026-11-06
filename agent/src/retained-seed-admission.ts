// issue #3865: the admission rule for seeding a fresh successor during a retained recovery.
//
// Pure arithmetic over one raw statfs sample, the hard governor's effective threshold and an
// estimate of what the seed will occupy. It judges pressure the way the heartbeat and the hard
// governor do (the max valid byte or inode used fraction, `dataVolumeUsedFraction`, against
// `atHardStopPressure`), so a seed is never admitted into a volume the governor would already stop
// runs on. The projection is a conservative estimate, not a quota or a reservation.

import { atHardStopPressure } from "./cache-cap.js";
import { dataVolumeStatsFromStatfs, dataVolumeUsedFraction, type StatfsSample } from "./stats.js";

const GIB = 1024 * 1024 * 1024;

/** What the successor seed is expected to occupy; `complete: false` means the numbers are lower bounds. */
export interface RetainedSeedEstimate {
  bytes: number;
  inodes: number;
  complete: boolean;
}

export interface RetainedSeedAdmissionPolicy {
  /** Fraction added on top of a measured or fallback estimate (drift, provisioning refill). */
  marginRatio: number;
  /** Fraction of the volume reserved for HOME caches the provisioning step refills. */
  homeAllowanceFraction: number;
  /** Fallback estimate floors, used when the measurement is absent or cut short. */
  fallbackMinBytes: number;
  fallbackMinInodes: number;
  /** Fallback estimate as a fraction of the volume's size; the larger of this and the floor applies. */
  fallbackFraction: number;
}

export const RETAINED_SEED_ADMISSION_POLICY: RetainedSeedAdmissionPolicy = {
  marginRatio: 0.25,
  homeAllowanceFraction: 0.01,
  fallbackMinBytes: GIB,
  fallbackMinInodes: 100_000,
  fallbackFraction: 0.02,
};

export type RetainedSeedAdmission =
  | { kind: "unknown" }
  | { kind: "refuse_current"; current: number; threshold: number }
  | {
      kind: "admit" | "refuse_projected";
      current: number;
      projected: number;
      threshold: number;
      estimateComplete: boolean;
    };

/**
 * Decide one seed. `unknown` (no valid byte or inode pair) admits as today. Current pressure at or
 * over `threshold` refuses before any estimate is consulted. Otherwise each valid pair projects
 * `(used + est x (1 + margin) + homeAllowance x total) / total`, where `est` is the measurement
 * when complete, else the larger of the partial reading and the fallback (also used when there is
 * no estimate); the projection is the max over valid pairs and the seed is admitted only when it
 * is strictly below `threshold`.
 */
export function decideRetainedSeedAdmission(input: {
  raw: StatfsSample;
  threshold: number;
  estimate: RetainedSeedEstimate | undefined;
  policy?: RetainedSeedAdmissionPolicy;
}): RetainedSeedAdmission {
  const { raw, threshold, estimate, policy = RETAINED_SEED_ADMISSION_POLICY } = input;
  const stats = dataVolumeStatsFromStatfs(raw);
  const current = dataVolumeUsedFraction(stats);
  if (current === undefined) return { kind: "unknown" };
  if (atHardStopPressure(current, threshold)) return { kind: "refuse_current", current, threshold };
  const complete = estimate?.complete === true;
  const pairs: Array<{ used?: number; total?: number; measured?: number; floor: number }> = [
    { used: stats.disk_data_bytes, total: stats.disk_data_total_bytes, measured: estimate?.bytes, floor: policy.fallbackMinBytes },
    { used: stats.disk_data_inodes, total: stats.disk_data_total_inodes, measured: estimate?.inodes, floor: policy.fallbackMinInodes },
  ];
  let projected = 0;
  for (const { used, total, measured, floor } of pairs) {
    if (used === undefined || total === undefined || !(total > 0)) continue;
    const fallback = Math.max(floor, policy.fallbackFraction * total);
    const est = estimate !== undefined && complete ? measured! : Math.max(measured ?? 0, fallback);
    projected = Math.max(projected, (used + est * (1 + policy.marginRatio) + policy.homeAllowanceFraction * total) / total);
  }
  return {
    kind: projected < threshold ? "admit" : "refuse_projected",
    current,
    projected,
    threshold,
    estimateComplete: complete,
  };
}
