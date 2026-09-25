// Overview admin health card (PRD #1484 M5). Mounted in the Dashboard beside
// CustodyBoardAlert. Admin-only and self-gating: it reads the ONE shared
// HealthStatusProvider, so a non-admin never fetches the admin endpoint (doc is null for
// them) and the card renders nothing. A non-admin gets the platform line instead
// (HealthPlatformLine) — the two are mutually exclusive.
//
// When every check passes it collapses to one quiet line so it costs no attention on a
// normal day; otherwise it shows the verdict and the top three attention items (worst-first),
// each linking to the Health tab.

import { Link } from "react-router-dom";

import { attentionChecks, healthVerdict } from "../lib/healthView";
import { formatAgo } from "../lib/rateLimits";
import { useHealthStatus } from "../lib/useAdminHealth";
import { SeverityBadge } from "./healthSeverity";
import { cx } from "./ui";

// "for 38m" from an RFC3339 `since`, or empty when the check carries none. Static (no live
// clock): the card refreshes on the dashboard's 10 s poll through the shared provider.
function sinceLabel(since: string | null): string {
  if (!since) return "";
  return `for ${formatAgo(since).replace(/ ago$/, "")}`;
}

function OpenHealthLink() {
  return (
    <Link to="/admin/health" className="shrink-0 text-sm text-info hover:underline">
      Open health
    </Link>
  );
}

export function HealthOverviewCard() {
  const { doc } = useHealthStatus();
  // null doc = a non-admin (no fetch) or before the first load: render nothing.
  if (!doc) return null;

  const attention = attentionChecks(doc);
  const danger = doc.status === "danger";
  // Follow CustodyBoardAlert's conditional role: danger is an alert, everything else a status
  // region. Scoped handle for tests: the aria-label, not a bare getByRole("status").
  const role = danger ? "alert" : "status";

  if (attention.length === 0) {
    // Quiet one-liner: every check uzi can make about itself is passing (na checks cannot
    // apply and are counted separately, never as green).
    return (
      <section
        role={role}
        aria-label="System health"
        className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 rounded-xl border border-edge bg-surface px-4 py-3"
      >
        <span className="flex flex-wrap items-center gap-2">
          <SeverityBadge severity="ok" />
          <span className="text-sm text-fg">
            System health: all {doc.counts.ok} {doc.counts.ok === 1 ? "check" : "checks"} passing
            {doc.counts.na > 0 ? `, ${doc.counts.na} not applicable` : ""}
          </span>
        </span>
        <OpenHealthLink />
      </section>
    );
  }

  const verdict = healthVerdict(doc.status, doc.counts);
  const top = attention.slice(0, 3);

  return (
    <section
      role={role}
      aria-label="System health"
      className={cx(
        "space-y-3 rounded-xl border p-4",
        danger ? "border-danger/50 bg-danger/10" : "border-warn/40 bg-warn/10",
      )}
    >
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <span className="flex min-w-0 flex-wrap items-center gap-2">
          <SeverityBadge severity={danger ? "danger" : "warn"} />
          <span className="text-sm font-semibold text-fg">System health: {verdict.title}</span>
        </span>
        <OpenHealthLink />
      </div>
      <ul className="space-y-1.5">
        {top.map((c) => (
          <li key={c.id} className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
            <SeverityBadge severity={c.severity} />
            <span className="min-w-0 flex-1 text-sm">
              <span className="font-medium text-fg">{c.title}</span>{" "}
              <span className="text-muted">{c.summary}</span>
            </span>
            {c.since && <span className="shrink-0 text-xs tabular-nums text-faint">{sinceLabel(c.since)}</span>}
          </li>
        ))}
      </ul>
      {attention.length > 3 && (
        <p className="text-xs text-faint">and {attention.length - 3} more</p>
      )}
    </section>
  );
}
