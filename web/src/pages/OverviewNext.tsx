// The Overview preview at /overview/next (PRD #3878): the same data as Dashboard through
// the shared useOverviewData hook, with a header that carries a status sentence (and, for an
// admin, the health chip) above the app-wide banner stack. It is a preview route, linked
// from and back to the current Overview at /dashboard.

import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { isTerminalRun } from "../lib/api";
import { useNow } from "../lib/useNow";
import { useDemoMode } from "../lib/demoMode";
import { maskName } from "../lib/demoMask";
import { useOverviewData } from "../lib/useOverviewData";
import {
  mergeOverviewRows,
  overviewCounts,
  ownRecoveryWaitCount,
  setupComplete,
  statusSentence,
  workerJoined,
} from "../lib/overview";
import { UsageCard } from "../components/UsageCards";
import { OnboardingChecklist } from "../components/OnboardingChecklist";
import { OverviewRunRow } from "../components/OverviewRunRow";
import { WorkerLoadCard } from "../components/WorkerLoadCard";
import { CustodyBoardAlert } from "../components/CustodyBoardAlert";
import { HealthPlatformLine } from "../components/HealthPlatformLine";
import { HealthChip } from "../components/HealthChip";
import { InShellHeader } from "../components/PageHeaderSlot";
import { Card, PageHeader, SectionTitle, Skeleton } from "../components/ui";

export function OverviewNext() {
  const demo = useDemoMode();
  const { user } = useAuth();
  const { data, usageWindow, changeUsageWindow, adminRuns } = useOverviewData({ includeAdminRuns: true });
  const now = useNow(30_000);

  if (!user) return null;

  const isAdmin = user.is_admin === true;
  // The viewer's OWN runs: what the custody alert, the platform line and (from M2) the
  // decisions read. Never the admin rows, for an admin too.
  const ownRuns = data?.runs ?? [];
  // The counts follow the viewer's scope: own runs, or for an admin every user's runs
  // while the admin list is readable. An unreadable admin list falls back to the viewer's
  // own runs and the sentence says so.
  const adminUnavailable = isAdmin && adminRuns.failed;
  const factoryRows =
    isAdmin && adminRuns.runs != null && !adminRuns.failed
      ? mergeOverviewRows(ownRuns, adminRuns.runs, user.email)
      : null;
  const sentence = data
    ? statusSentence({
        // M2 wires the viewer's decisions and open pull requests.
        decisions: 0,
        prs: 0,
        counts: overviewCounts(factoryRows ?? ownRuns),
        acrossFactory: factoryRows != null,
        adminUnavailable,
      })
    : undefined;
  const active = ownRuns.filter((r) => !isTerminalRun(r.status));
  const joined = workerJoined(data?.workers ?? []);
  const ready = data ? setupComplete(data, joined) : false;

  return (
    <div className="space-y-6">
      {/* The header portals into the shell's header slot, above the one banner stack. */}
      <InShellHeader>
        <PageHeader
          title={`Welcome${user.display_name ? `, ${maskName(user.display_name, demo)}` : ""}`}
          description={sentence}
          actions={
            <>
              {isAdmin && <HealthChip />}
              <Link to="/dashboard" className="text-sm font-medium text-brand hover:text-brand-hover">
                Back to the current Overview
              </Link>
            </>
          }
        />
      </InShellHeader>

      {data && !ready && (
        <OnboardingChecklist
          hasToken={data.hasToken}
          hasForge={data.hasForge}
          reposEnabled={data.reposEnabled}
          workerJoined={joined}
        />
      )}

      {/* M2 inserts the "Needs your decision" card here. */}

      {/* Same inputs as Dashboard: the viewer's own runs, never the admin rows. */}
      <CustodyBoardAlert recoveryWaitCount={ownRecoveryWaitCount(ownRuns)} />
      <HealthPlatformLine isAdmin={isAdmin} runs={ownRuns} workers={data?.workers ?? []} />

      {/* Temporary Runs card: M3 replaces it with the grouped Yours / Other users widget. */}
      <Card>
        <div className="flex items-center justify-between">
          <SectionTitle>Runs</SectionTitle>
          <Link to="/runs" className="text-xs font-medium text-brand hover:text-brand-hover">
            All runs →
          </Link>
        </div>
        {!data ? (
          <div className="mt-3 space-y-2">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : active.length === 0 ? (
          <p className="mt-3 text-sm text-faint">No active runs. Start one from a board card.</p>
        ) : (
          <ul className="mt-2 divide-y divide-edge">
            {active.map((r) => (
              <OverviewRunRow key={r.id} run={r} now={now} demo={demo} />
            ))}
          </ul>
        )}
      </Card>

      {data && <WorkerLoadCard workers={data.workers} />}

      {/* Factory usage is rendered only from the admin-gated response. */}
      {data?.selfUsage && (
        <UsageCard self={data.selfUsage} admin={data.adminUsage ?? undefined} window={usageWindow} onWindowChange={changeUsageWindow} />
      )}
    </div>
  );
}
