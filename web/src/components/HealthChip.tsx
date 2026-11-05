// Compact instance-health chip for the /overview/next header (PRD #3878). Admin-only by
// construction: it reads the ONE shared HealthStatusProvider, whose doc is null for a
// non-admin (no fetch) and before the first load, and the chip then renders nothing.
// It mirrors the severity HealthOverviewCard shows, in one line, and links to the Health tab.

import { Link } from "react-router-dom";

import { attentionChecks, healthVerdict } from "../lib/healthView";
import { useHealthStatus } from "../lib/useAdminHealth";
import { SeverityBadge } from "./healthSeverity";

export function HealthChip() {
  const { doc } = useHealthStatus();
  if (!doc) return null;

  const passing = attentionChecks(doc).length === 0;
  const applicable = doc.checks.length - doc.counts.na;
  const text = passing
    ? `${doc.counts.ok} of ${applicable} checks passing`
    : healthVerdict(doc).title;
  // Same severity the card colours by: danger, otherwise warn for any attention item.
  const severity = passing ? "ok" : doc.status === "danger" ? "danger" : "warn";

  return (
    <Link
      to="/admin/health"
      aria-label={`System health: ${text}`}
      className="inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised"
    >
      <SeverityBadge severity={severity} />
      <span className="min-w-0 truncate">{text}</span>
    </Link>
  );
}
