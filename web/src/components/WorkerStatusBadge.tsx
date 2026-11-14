import type { Worker } from "../lib/api";
import { hibernationPresentation } from "../lib/workerHibernation";
import { stripUnsafeChars } from "../lib/safeText";
import { Badge } from "./ui";

/**
 * The worker's heartbeat status chip. A hibernating worker is scaled down on purpose, so it
 * shows hibernating/hibernated/waking in place of "offline" (PRD #4205): it is neither broken
 * nor cordoned. A failed wake adds a "wake failed" chip whose tooltip carries the server
 * message as plain text.
 */
export function WorkerStatusBadge({ worker, title }: { worker: Worker; title?: string }) {
  const h = hibernationPresentation(worker);
  if (!h) {
    return (
      <Badge tone={worker.status === "online" ? "ok" : "neutral"} dot title={title}>
        {worker.status}
      </Badge>
    );
  }
  const wakeError = stripUnsafeChars(worker.wake_error ?? "").trim();
  return (
    <>
      <Badge tone="info" dot title={h.title}>
        {h.label}
      </Badge>
      {wakeError && (
        <Badge tone="danger" title={wakeError}>
          wake failed
        </Badge>
      )}
    </>
  );
}
