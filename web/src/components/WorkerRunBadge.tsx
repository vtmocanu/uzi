import { Badge } from "./ui";
import { workerRunBadge } from "../lib/workerRuns";
import type { Worker } from "../lib/api";

// WorkerRunBadge renders a worker's run-load pill (PRD #42 Decision 10): "N/M runs"
// once the worker runs more than one run or advertises a cap above 1, the legacy
// "busy" pill for a single run, or nothing when idle. A thin presentational wrapper
// over workerRunBadge so WorkersSettings and the admin RunsList render it identically.
export function WorkerRunBadge({
  worker,
}: {
  worker: Pick<Worker, "busy" | "active_runs" | "max_concurrent_runs">;
}) {
  const badge = workerRunBadge(worker);
  if (!badge) return null;
  return (
    <Badge tone={badge.tone} title={badge.title}>
      {badge.label}
    </Badge>
  );
}
