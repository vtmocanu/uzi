import { Badge } from "./ui";
import type { Worker } from "../lib/api";

// WorkerCustodyBadge marks retained unpublished committed work that needs an owner
// decision. It follows the worker list's decision count; run recovery provides the
// available actions. Healthy retained work does not need this badge.
//
// No stripUnsafeChars is needed: the only text is static copy, never a worker-controlled
// string. Older payloads may omit the count; absence renders nothing.
export function WorkerCustodyBadge({
  worker,
}: {
  worker: Pick<Worker, "custody_decisions_needed">;
}) {
  if (!((worker.custody_decisions_needed ?? 0) > 0)) return null;
  return (
    <Badge
      tone="brand"
      dot
      title="Retaining unpublished committed work that needs an owner decision."
    >
      retaining work
    </Badge>
  );
}
