// Pure, framework-free worker run-load badge logic (PRD #42 Decision 10). Kept out
// of WorkersSettings.tsx / RunsList.tsx so the "N/M runs" mapping is unit-tested in
// isolation (workerRuns.test.ts), the same split runBadge.ts / pipelineBadge.ts use.

import type { Worker } from "./api";
import type { BadgeTone } from "./runBadge";

// WorkerRunBadge is the resolved run-load pill for a worker row, or null when the
// worker is idle and advertises no cap above 1 (render nothing, as before PRD #42).
export interface WorkerRunBadge {
  label: string;
  tone: BadgeTone;
  title: string;
}

// workerRunBadge decides how a worker's active-run load renders. Below the
// concurrency threshold — at most one active run AND no advertised cap above 1 — it
// stays exactly the legacy rendering: a "busy" pill when it holds a run, nothing when
// idle. Once the worker runs more than one run OR advertises a cap above 1, it shows
// the "N/M runs" count so saturation is visible. M is the advertised cap; when the
// worker advertises none (an older image, or before the M2 agent sends it) it falls
// back to the active count, so the badge never shows a meaningless "N/null".
export function workerRunBadge(
  w: Pick<Worker, "busy" | "active_runs" | "max_concurrent_runs">,
): WorkerRunBadge | null {
  const cap = w.max_concurrent_runs;
  const active = w.active_runs;
  if (active > 1 || (cap != null && cap > 1)) {
    const denom = cap ?? active;
    return {
      label: `${active}/${denom} runs`,
      // Amber only while actually running; an idle worker advertising a cap (e.g.
      // "0/2 runs") reads as calm capacity, not a warning.
      tone: active > 0 ? "warning" : "neutral",
      title:
        cap != null
          ? `Running ${active} of ${cap} run slots`
          : `Running ${active} concurrent runs`,
    };
  }
  if (w.busy) return { label: "busy", tone: "warning", title: "Holds an active run" };
  return null;
}
