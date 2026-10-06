import type { AdminWorker } from "../lib/api";
import { Badge } from "./ui";
import { WorkerQuarantineBadge } from "./WorkerQuarantineBadge";
import { hasStats, WorkerDiskLine, WorkerStatLine } from "./WorkerStats";

/** Resource readings, the admin-only pending state and the residue-quarantine latch are independent reports. */
export function AdminWorkerResources({ worker, diskOnly = false }: { worker: AdminWorker; diskOnly?: boolean }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      {diskOnly || !hasStats(worker) ? <WorkerDiskLine worker={worker} /> : <WorkerStatLine worker={worker} />}
      {worker.cleanup_pending && (
        <Badge
          tone="neutral"
          title="Docker disk cleanup is pending. Existing runs may finish before cleanup; some workers report pressure only."
        >
          cleanup pending
        </Badge>
      )}
      <WorkerQuarantineBadge worker={worker} />
    </span>
  );
}
