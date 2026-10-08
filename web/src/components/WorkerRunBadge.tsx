import { Badge } from "./ui";
import { workerRunBadge, workerCrossCheckBadge } from "../lib/workerRuns";
import type { Worker } from "../lib/api";

// Keep the lanes as separate pills wherever worker capacity is shown.
export function WorkerRunBadge({
  worker,
}: {
  worker: Pick<Worker, "busy" | "active_runs" | "max_concurrent_runs" | "active_cross_checks" | "max_cross_check_slots">;
}) {
  const runs = workerRunBadge(worker);
  const checks = workerCrossCheckBadge(worker);
  return (
    <>
      {runs && <Badge tone={runs.tone} title={runs.title}>{runs.label}</Badge>}
      {checks && <Badge tone={checks.tone} title={checks.title}>{checks.label}</Badge>}
    </>
  );
}
