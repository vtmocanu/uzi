// PRD #1809 M6 (D8): two facts a parked run's panel shows about the run's state on its worker.
//
// 1. Checkpoint durability: whether the checkpoint the park published contains the run's latest
//    committed work. "Checkpoint published" only means a ref was pushed; when the recovery pin or
//    the fetch-back failed, the pushed checkpoint is older than the work, and the only copy of the
//    latest commits is on the worker (which keeps it under its custody hold). The server stores the
//    report of the park that carried it and clears it when the run is claimed again or reports
//    running, so it never describes a later park (a server-side park carries none and reads
//    absent). It renders only while the run is parked.
// 2. The run's HOME size on its current worker and, of that, the rebuildable caches, from that
//    worker's report measured in the last 25 minutes (absent otherwise). "at least" when the
//    worker's size walk was truncated.
//
// Both are display-only and render nothing when the server sent nothing.

import type { Run } from "../lib/api";
import { formatBytes } from "./WorkerStats";

const PARKED = new Set(["limit_wait", "recovery_wait", "paused"]);

/** The durability sentence, or null when the run is not parked or the worker did not report it. */
export function checkpointDurabilityText(run: Run): string | null {
  if (run.checkpoint_contains_latest == null || !PARKED.has(run.status)) return null;
  return run.checkpoint_contains_latest
    ? "The published checkpoint contains the latest work."
    : "The published checkpoint does not contain the latest committed work; the worker keeps that work.";
}

/** "Home on the worker: 4.2 GiB, including 3 GiB of rebuildable caches.", or null with no size. */
export function runDiskSizeText(run: Run): string | null {
  if (run.home_bytes == null) return null;
  const home = `${run.disk_truncated ? "at least " : ""}${formatBytes(run.home_bytes)}`;
  const cache = run.cache_bytes == null ? "" : `, including ${formatBytes(run.cache_bytes)} of rebuildable caches`;
  return `Home on the worker: ${home}${cache}.`;
}

export function RunParkDiskFacts({ run }: { run: Run }) {
  const durability = checkpointDurabilityText(run);
  const size = runDiskSizeText(run);
  if (!durability && !size) return null;
  return (
    <>
      {durability && (
        <p className={run.checkpoint_contains_latest ? "mt-1.5 text-xs text-muted" : "mt-1.5 text-xs text-fg"}>
          {durability}
        </p>
      )}
      {size && <p className="mt-1.5 text-xs tabular-nums text-muted">{size}</p>}
    </>
  );
}
