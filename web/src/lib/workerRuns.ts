// Shared capacity badge logic for the run and dedicated cross-check lanes.
import type { Worker } from "./api";
import type { BadgeTone } from "./runBadge";

export interface WorkerRunBadge {
  label: string;
  tone: BadgeTone;
  title: string;
}

// Lane-aware workers show run capacity even at cap one. Older workers keep the
// legacy busy pill below the concurrency threshold; unknown caps use live load.
export function workerRunBadge(
  w: Pick<Worker, "busy" | "active_runs" | "max_concurrent_runs" | "max_cross_check_slots">,
): WorkerRunBadge | null {
  const cap = w.max_concurrent_runs;
  const active = w.active_runs;
  const laneOn = w.max_cross_check_slots != null && w.max_cross_check_slots > 0;
  if (active > 1 || (cap != null && cap > 1) || (laneOn && (cap != null || active > 0))) {
    const denom = cap ?? active;
    return {
      label: `${active}/${denom} runs`,
      tone: active > 0 ? "warning" : "neutral",
      title:
        cap != null
          ? `Running ${active} of ${cap} run slots`
          : `Running ${active} concurrent runs`,
    };
  }
  if (w.busy && !laneOn) return { label: "busy", tone: "warning", title: "Holds an active run" };
  return null;
}

export function workerCrossCheckBadge(
  w: Pick<Worker, "active_cross_checks" | "max_cross_check_slots">,
): WorkerRunBadge | null {
  const cap = w.max_cross_check_slots;
  const active = w.active_cross_checks;
  if ((cap == null || cap <= 0) && active === 0) return null;
  return {
    label: `${active}/${cap ?? "?"} cross-checks`,
    tone: active > 0 ? "warning" : "neutral",
    title: cap == null
      ? `Running ${active} cross-checks; slot capacity is unadvertised`
      : `Running ${active} of ${cap} cross-check slots`,
  };
}
