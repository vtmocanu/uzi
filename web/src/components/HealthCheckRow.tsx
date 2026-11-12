// One attention row (severity badge, title, summary, evidence, docs link, "for 38m"),
// shared by the Overview HealthOverviewCard and the HealthChip popover so both draw a
// check identically.

import type { HealthCheck } from "../lib/api";
import { formatAgo } from "../lib/rateLimits";
import { SeverityBadge } from "./healthSeverity";
import { DocLink } from "./DocLink";
import { HealthEvidenceLabel, HealthEvidenceValue } from "./HealthEvidenceLabel";

// "for 38m" from an RFC3339 `since`, or empty when the check carries none. Static (no live
// clock): both surfaces refresh through the shared health provider's poll.
function sinceLabel(since: string | null): string {
  if (!since) return "";
  return `for ${formatAgo(since).replace(/ ago$/, "")}`;
}

export function HealthCheckRow({ check: c }: { check: HealthCheck }) {
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
      <SeverityBadge severity={c.severity} />
      <span className="min-w-0 flex-1 text-sm">
        <span className="font-medium text-fg">{c.title}</span>{" "}
        <span className="text-muted">{c.summary}</span>
        {c.evidence.map((e, i) => (
          <span key={i} className="block break-words text-xs text-muted">
            <span className="font-medium"><HealthEvidenceLabel evidence={e} />:</span>{" "}
            <HealthEvidenceValue evidence={e} />
          </span>
        ))}
        {c.doc && (
          <span className="block text-xs">
            <DocLink slug={c.doc}>Docs: {c.doc}</DocLink>
          </span>
        )}
      </span>
      {c.since && <span className="shrink-0 text-xs tabular-nums text-faint">{sinceLabel(c.since)}</span>}
    </li>
  );
}
