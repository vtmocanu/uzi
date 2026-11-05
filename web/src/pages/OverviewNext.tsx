// The Overview preview at /overview/next (PRD #3878): the same data as Dashboard through
// the shared useOverviewData hook, with a header that carries a status sentence (and, for an
// admin, the health chip) above the app-wide banner stack. It is a preview route, linked
// from and back to the current Overview at /dashboard.

import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { useNow } from "../lib/useNow";
import { useDemoMode } from "../lib/demoMode";
import { maskName } from "../lib/demoMask";
import { useOverviewData } from "../lib/useOverviewData";
import {
  mergeOverviewRows,
  overviewCounts,
  overviewDecisions,
  ownRecoveryWaitCount,
  setupComplete,
  statusSentence,
  workerJoined,
} from "../lib/overview";
import { UsageCard } from "../components/UsageCards";
import { OnboardingChecklist } from "../components/OnboardingChecklist";
import { NeedsDecisionCard } from "../components/NeedsDecisionCard";
import { OverviewRunsWidget } from "../components/OverviewRunsWidget";
import { CapacityRail } from "../components/CapacityRail";
import { CustodyBoardAlert } from "../components/CustodyBoardAlert";
import { HealthPlatformLine } from "../components/HealthPlatformLine";
import { HealthChip } from "../components/HealthChip";
import { InShellHeader } from "../components/PageHeaderSlot";
import { PageHeader } from "../components/ui";

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
  const decisions = overviewDecisions(ownRuns);
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
        decisions: decisions.filter((d) => d.kind !== "pr").length,
        prs: decisions.filter((d) => d.kind === "pr").length,
        counts: overviewCounts(factoryRows ?? ownRuns),
        acrossFactory: factoryRows != null,
        adminUnavailable,
      })
    : undefined;
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

      {/* The viewer's own items only (decisions is computed from ownRuns). */}
      <NeedsDecisionCard items={decisions} now={now} demo={demo} />

      {/* Same inputs as Dashboard: the viewer's own runs, never the admin rows. */}
      <CustodyBoardAlert recoveryWaitCount={ownRecoveryWaitCount(ownRuns)} />
      <HealthPlatformLine isAdmin={isAdmin} runs={ownRuns} workers={data?.workers ?? []} />

      {/* Runs on the left, the right-hand rail beside it; the rail stacks below at narrow widths. */}
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="min-w-0">
          <OverviewRunsWidget
            ownRuns={ownRuns}
            adminRuns={isAdmin ? adminRuns : null}
            isAdmin={isAdmin}
            viewerEmail={user.email}
            now={now}
            demo={demo}
            loading={!data}
          />
        </div>
        <div className="min-w-0 space-y-6">{data && <CapacityRail workers={data.workers} />}</div>
      </div>

      {/* Factory usage is rendered only from the admin-gated response. */}
      {data?.selfUsage && (
        <UsageCard self={data.selfUsage} admin={data.adminUsage ?? undefined} window={usageWindow} onWindowChange={changeUsageWindow} />
      )}
    </div>
  );
}
