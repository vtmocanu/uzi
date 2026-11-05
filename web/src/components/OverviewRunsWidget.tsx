// The Overview preview's Runs card (PRD #3878 M3): the viewer's own active runs ("Yours")
// and, for an admin, every other user's active runs ("Other users (admin)"). Other users'
// rows are read-only: OverviewRunRow carries no action buttons, and the decision card never
// reads these rows.

import { Link } from "react-router-dom";
import { isTerminalRun, type RunSummaryItem } from "../lib/api";
import { formatAgo } from "../lib/rateLimits";
import type { AdminRunsState } from "../lib/useOverviewData";
import { OverviewRunRow } from "./OverviewRunRow";
import { Card, SectionTitle, Skeleton } from "./ui";

function GroupHeading({ children }: { children: string }) {
  return <h3 className="mt-3 text-xs font-medium text-muted">{children}</h3>;
}

export function OverviewRunsWidget({
  ownRuns,
  adminRuns,
  isAdmin,
  viewerEmail,
  now,
  demo,
  loading,
}: {
  ownRuns: RunSummaryItem[];
  adminRuns: AdminRunsState | null;
  isAdmin: boolean;
  viewerEmail: string;
  now: number;
  demo: boolean;
  // True only before the first load; a later poll never brings the skeletons back.
  loading: boolean;
}) {
  const yours = ownRuns.filter((r) => !isTerminalRun(r.status));
  const others = (adminRuns?.runs ?? []).filter(
    (r) => r.owner_email !== viewerEmail && !isTerminalRun(r.status),
  );
  const adminFailed = isAdmin && adminRuns?.failed === true;

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle>Runs</SectionTitle>
        <Link to="/runs" className="text-xs font-medium text-brand hover:text-brand-hover">
          All runs →
        </Link>
      </div>
      {loading ? (
        <div className="mt-3 space-y-2">
          <Skeleton className="h-10" />
          <Skeleton className="h-10" />
        </div>
      ) : (
        <>
          {isAdmin && <GroupHeading>Yours</GroupHeading>}
          {yours.length === 0 ? (
            <p className="mt-2 text-sm text-faint">No active runs. Start one from a board card.</p>
          ) : (
            <ul aria-label="Your runs" className="mt-1 divide-y divide-edge">
              {yours.map((r) => (
                <OverviewRunRow key={r.id} run={r} now={now} demo={demo} />
              ))}
            </ul>
          )}
          {adminFailed ? (
            // The last good rows are not shown: stale rows must not read as live ones.
            <>
              <GroupHeading>Other users (admin)</GroupHeading>
              <p role="status" className="mt-2 text-sm text-faint">
                Other users' runs are unavailable
                {adminRuns?.lastOkAt != null &&
                  ` (last read ${formatAgo(new Date(adminRuns.lastOkAt).toISOString(), now)})`}
              </p>
            </>
          ) : (
            isAdmin &&
            others.length > 0 && (
              <>
                <GroupHeading>Other users (admin)</GroupHeading>
                <ul aria-label="Other users' runs" className="mt-1 divide-y divide-edge">
                  {others.map((r) => (
                    <OverviewRunRow key={r.id} run={r} now={now} demo={demo} owner={r.owner_email} />
                  ))}
                </ul>
              </>
            )
          )}
        </>
      )}
    </Card>
  );
}
