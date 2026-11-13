// One attention row (severity badge, title, summary, evidence, docs link, "for 38m"),
// shared by the Overview HealthOverviewCard and the HealthChip popover. The panel opts into
// two-column details at sm+; the card keeps its existing single-column DOM and classes.

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

export function HealthCheckRow({ check: c, twoColumnDetails = false }: { check: HealthCheck; twoColumnDetails?: boolean }) {
  const evidence = c.evidence.map((e, i) => (
    <span
      key={i}
      data-health-evidence-entry={twoColumnDetails ? "" : undefined}
      className={twoColumnDetails
        ? `block min-w-0 break-words text-xs text-muted${e.value.length > 40 ? " sm:col-span-2" : ""}`
        : "block break-words text-xs text-muted"}
      style={twoColumnDetails ? { overflowWrap: "anywhere" } : undefined}
    >
      <span className="font-medium"><HealthEvidenceLabel evidence={e} />:</span>{" "}
      <HealthEvidenceValue evidence={e} />
    </span>
  ));
  const docs = c.doc && (
    <span
      data-health-evidence-entry={twoColumnDetails ? "" : undefined}
      className={twoColumnDetails ? "block min-w-0 text-xs" : "block text-xs"}
      style={twoColumnDetails ? { overflowWrap: "anywhere" } : undefined}
    >
      <DocLink slug={c.doc}>Docs: {c.doc}</DocLink>
    </span>
  );
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
      <SeverityBadge severity={c.severity} />
      <span className="min-w-0 flex-1 text-sm">
        <span className="font-medium text-fg">{c.title}</span>{" "}
        <span className="text-muted">{c.summary}</span>
        {twoColumnDetails ? (
          <span data-health-evidence-grid className="contents sm:grid sm:grid-cols-2 sm:gap-x-8 sm:gap-y-1">
            {evidence}
            {docs}
          </span>
        ) : (
          <>
            {evidence}
            {docs}
          </>
        )}
      </span>
      {c.since && <span className="shrink-0 text-xs tabular-nums text-faint">{sinceLabel(c.since)}</span>}
    </li>
  );
}
