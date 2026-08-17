import { Badge } from "./ui";
import { priorityBadge } from "../lib/runBadge";
import type { RunPriority } from "../lib/api";

// RunPriorityBadge is the queue-priority pill (PRD #320 M6) for the list surfaces
// (Runs list, run view) that render run status through StatusPill. It shows
// "Deprioritized"/"Expedited"/"Restored" beside the status pill and renders NOTHING
// otherwise, so a normal (or pre-#320, priority-absent) run is visually unchanged. It
// reads the SAME priorityBadge taxonomy defined once in runBadge.ts, and is gated to a
// QUEUED run because only a queued run ever carries a non-normal class — a running or
// terminal run must never show a stale priority pill.
export function RunPriorityBadge({
  priority,
  status,
}: {
  priority?: RunPriority;
  status: string;
}) {
  if (status !== "queued") return null;
  const badge = priorityBadge(priority);
  if (!badge) return null;
  return (
    <Badge tone={badge.tone} title={badge.title}>
      {badge.label}
    </Badge>
  );
}
