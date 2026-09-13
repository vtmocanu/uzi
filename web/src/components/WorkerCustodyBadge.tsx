import { Badge } from "./ui";
import type { Worker } from "../lib/api";

// WorkerCustodyBadge marks a worker that is retaining unpublished committed work for
// durable recovery (PRD #1296 M4/M5 E25, D4). It renders ONLY when the worker holds an
// OPEN custody hold (retaining_unpublished_work), and is deliberately DISTINCT from the
// busy/active-runs load pill (WorkerRunBadge, amber) and the cordon pill (dashed
// neutral): custody consumes no run slot, so it must not read as either. It uses the
// `brand` accent tone so an operator can see at a glance that this worker's teardown is
// deferred until the work is archived or explicitly discarded.
//
// No stripUnsafeChars is needed: the only text is static copy, never a worker-controlled
// string. The field is optional on the wire (mocks/older payloads may omit it); absence
// renders nothing.
export function WorkerCustodyBadge({
  worker,
}: {
  worker: Pick<Worker, "retaining_unpublished_work">;
}) {
  if (!worker.retaining_unpublished_work) return null;
  return (
    <Badge
      tone="brand"
      dot
      title="Retaining unpublished committed work for durable recovery. Teardown is deferred until the work is archived or explicitly discarded; this holds no run slot."
    >
      retaining work
    </Badge>
  );
}
