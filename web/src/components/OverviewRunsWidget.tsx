// The Overview preview's Runs card (PRD #3878 M3): the viewer's own active runs ("Yours")
// and, for an admin, every other user's active runs ("Other users (admin)"). Other users'
// rows are read-only: OverviewRunRow carries no action buttons, and the decision card never
// reads these rows.

import { Link } from "react-router-dom";
import { isTerminalRun, type RunSummaryItem } from "../lib/api";
import { issueLineStage, type IssueStage } from "../lib/issueLine";
import { formatAgo } from "../lib/rateLimits";
import type { AdminRunsState } from "../lib/useOverviewData";
import { OverviewRunRow } from "./OverviewRunRow";
import { Card, SectionTitle, Skeleton } from "./ui";

// The stage names the card head shows while a stage is picked on the issue-work line.
const STAGE_NAMES: Record<IssueStage, string> = {
  queued: "Queued",
  planning: "Planning",
  review: "Plan review",
  building: "Building",
  pr: "Open pull requests",
  asking: "Asking you",
  parked: "Parked",
  other: "Other work",
  none: "Active runs",
};

function GroupHeading({ children, admin }: { children: string; admin?: boolean }) {
  // The admin group takes the brand role, like RunsList's "Factory status (admin)" title.
  return <h3 className={`mt-3 text-xs font-medium ${admin ? "text-brand" : "text-muted"}`}>{children}</h3>;
}

export function OverviewRunsWidget({
  ownRuns,
  adminRuns,
  isAdmin,
  viewerEmail,
  now,
  demo,
  loading,
  stage = null,
  prRuns = [],
}: {
  ownRuns: RunSummaryItem[];
  adminRuns: AdminRunsState | null;
  isAdmin: boolean;
  viewerEmail: string;
  now: number;
  demo: boolean;
  // True only before the first load; a later poll never brings the skeletons back.
  loading: boolean;
  // The stage picked on the issue-work line (null: every active run), and the viewer's open
  // pull requests, openPrs(ownRuns) as the line computed them, so the counts match. The PR
  // station lists those runs; no other user's PR is ever on the line.
  stage?: IssueStage | null;
  prRuns?: RunSummaryItem[];
}) {
  const openPrIds = new Set(prRuns.map((r) => r.id));
  const onStage = (r: RunSummaryItem) => stage == null || issueLineStage(r, openPrIds) === stage;
  const filtered = stage != null;
  const yours = stage === "pr" ? prRuns : ownRuns.filter((r) => !isTerminalRun(r.status) && onStage(r));
  const adminFailed = isAdmin && adminRuns?.failed === true;
  // A failed admin read keeps its last good rows, but the line is drawn without them
  // (otherRuns is empty), so they must not shape the label or the empty sentence either.
  const others =
    isAdmin && !adminFailed
      ? (adminRuns?.runs ?? []).filter((r) => r.owner_email !== viewerEmail && !isTerminalRun(r.status) && onStage(r))
      : [];
  // "Questions" when another user's question is in the group, as the line's bay says.
  const scope = stage == null ? "Active runs" : stage === "asking" && others.length > 0 ? "Questions" : STAGE_NAMES[stage];

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle>Runs</SectionTitle>
        <span className="flex items-center gap-3 text-xs">
          <span className="text-faint">{scope}</span>
          <Link to="/runs" className="font-medium text-brand hover:text-brand-hover">
            All runs →
          </Link>
        </span>
      </div>
      {loading ? (
        <div className="mt-3 space-y-2">
          <Skeleton className="h-10" />
          <Skeleton className="h-10" />
        </div>
      ) : (
        <>
          {/* Under a stage filter an empty group hides; one sentence covers both being empty. */}
          {(!filtered || yours.length > 0) && <GroupHeading>Yours</GroupHeading>}
          {yours.length > 0 ? (
            <ul aria-label="Your runs" className="mt-1 divide-y divide-edge">
              {yours.map((r) => (
                <OverviewRunRow key={r.id} run={r} now={now} demo={demo} />
              ))}
            </ul>
          ) : (
            !filtered && <p className="mt-2 text-sm text-faint">No active runs. Start one from a board card.</p>
          )}
          {adminFailed ? (
            // The last good rows are not shown: stale rows must not read as live ones.
            <>
              <GroupHeading admin>Other users (admin)</GroupHeading>
              {/* Only the static sentence is a live region: the ticking read time sits outside it. */}
              <p className="mt-2 text-sm text-faint">
                <span role="status">Other users' runs are unavailable</span>
                {adminRuns?.lastOkAt != null &&
                  ` (last read ${formatAgo(new Date(adminRuns.lastOkAt).toISOString(), now)})`}
              </p>
            </>
          ) : (
            isAdmin &&
            others.length > 0 && (
              <>
                <GroupHeading admin>Other users (admin)</GroupHeading>
                <ul aria-label="Other users' runs" className="mt-1 divide-y divide-edge">
                  {others.map((r) => (
                    <OverviewRunRow key={r.id} run={r} now={now} demo={demo} owner={r.owner_email} />
                  ))}
                </ul>
              </>
            )
          )}
          {filtered && yours.length === 0 && others.length === 0 && (
            <p className="mt-2 text-sm text-faint">No runs at this stage.</p>
          )}
        </>
      )}
    </Card>
  );
}
