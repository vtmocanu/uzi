import { Badge } from "./ui";
import { healthBadge, type HealthFlaggable } from "../lib/runBadge";
import { useMaintenance } from "../lib/useMaintenance";

// RunHealthBadge is the run-health warn pill (PRD #47) for the list surfaces
// (dashboard, runs list) that render run status through StatusPill rather than
// runBadge. It shows `⚠ <label> · <elapsed>` beside the status pill when a run is
// flagged and in a flaggable status, and renders nothing otherwise — so a healthy
// or terminal run is visually unchanged. It reads the SAME healthBadge taxonomy the
// board card uses, so the label and gating stay defined in one place. The title
// carries the owner-only reason when present (a non-owner's health_reason is null).
export function RunHealthBadge({ run, nowMs = Date.now() }: { run: HealthFlaggable; nowMs?: number }) {
  // PRD #3454: a maintenance-held run shows the calm "waiting: uzi is in maintenance mode" pill instead,
  // its tooltip carrying the admin's reason from the shared maintenance state.
  const maintenanceReason = useMaintenance().status?.reason;
  const badge = healthBadge(run, nowMs, maintenanceReason);
  if (!badge || badge.kind !== "badge") return null;
  return (
    <Badge tone={badge.tone} title={badge.title} wrap={badge.wrap}>
      {badge.label}
    </Badge>
  );
}
