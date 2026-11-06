// Run-slot capacity for the Overview preview's Capacity rail (PRD #3880 M1). Pure: it reads
// each worker's advertised run lane (active_runs against max_concurrent_runs) and nothing
// else. "Open" is advertised spare capacity, never a promise that a run will be claimed:
// credentials, cordons and admission still decide that.

import type { Worker } from "./api";
import { stripUnsafeChars } from "./safeText";

export interface CapacitySlots {
  inUse: number;
  /** Spare advertised slots on a worker that takes no new runs. */
  closed: number;
  /** Spare advertised slots on a worker that is not fenced. */
  open: number;
  total: number;
  /** Runs reported beyond a worker's advertised cap; never part of a segment. */
  overCap: number;
  excludedOffline: number;
  excludedUnknownCap: number;
}

/** True when the worker will claim nothing new: draining, cordoned (manual or effective) or
 *  a hosted worker mid-upgrade. The optional cordon fields read as "not cordoned" when absent. */
export function workerTakesNoNewRuns(w: Worker): boolean {
  return (
    w.draining_since != null ||
    w.manual_cordoned_at != null ||
    w.effective_cordoned === true ||
    (w.kind === "hosted" && w.upgrade_status === "upgrading")
  );
}

/** "Rolls to X once fully idle", only with a hosted worker's actual upgrading report, a
 *  non-blank (sanitized) target and a drain. An outdated version or a manual cordon alone is
 *  no rollout promise. */
export function rollNote(w: Worker): string | null {
  if (w.kind !== "hosted" || w.upgrade_status !== "upgrading" || w.draining_since == null) return null;
  const target = stripUnsafeChars(w.upgrade_target ?? "").trim();
  if (target === "") return null;
  return `Rolls to ${target} once fully idle`;
}

export function capacitySlots(workers: Worker[]): CapacitySlots {
  const out: CapacitySlots = { inUse: 0, closed: 0, open: 0, total: 0, overCap: 0, excludedOffline: 0, excludedUnknownCap: 0 };
  for (const w of workers) {
    if (w.status !== "online") {
      out.excludedOffline++;
      continue;
    }
    const cap = w.max_concurrent_runs;
    if (typeof cap !== "number" || !Number.isFinite(cap) || cap < 0) {
      out.excludedUnknownCap++;
      continue;
    }
    const active = Number.isFinite(w.active_runs) ? Math.max(w.active_runs, 0) : 0;
    const used = Math.min(active, cap);
    out.inUse += used;
    out.overCap += Math.max(active - cap, 0);
    const free = cap - used;
    if (workerTakesNoNewRuns(w)) out.closed += free;
    else out.open += free;
    out.total += cap;
  }
  return out;
}
