// The Overview preview at /overview/next (PRD #3878): the same data as Dashboard through
// the shared useOverviewData hook, with a header that carries a status sentence (and, for an
// admin, the health chip) above the app-wide banner stack. It is a preview route, linked
// from and back to the current Overview at /dashboard.

import { useState } from "react";
import { Link } from "react-router-dom";
import { isTerminalRun } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { useNow } from "../lib/useNow";
import { useDemoMode } from "../lib/demoMode";
import { maskName } from "../lib/demoMask";
import { useOverviewData } from "../lib/useOverviewData";
import { issueLineModel, type IssueStage } from "../lib/issueLine";
import {
  decisionCount,
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
import { IssueLine } from "../components/IssueLine";
import { OverviewRunsWidget } from "../components/OverviewRunsWidget";
import { CapacityRail } from "../components/CapacityRail";
import { HeldWorkCard } from "../components/HeldWorkCard";
import { HealthPlatformLine } from "../components/HealthPlatformLine";
import { HealthChip } from "../components/HealthChip";
import { InShellHeader } from "../components/PageHeaderSlot";
import { PageHeader } from "../components/ui";

export function OverviewNext() {
  const demo = useDemoMode();
  const { user } = useAuth();
  const { data, usageWindow, changeUsageWindow, adminRuns, holds, holdsFailed } = useOverviewData({
    includeAdminRuns: true,
    includeRecoveryHolds: true,
  });
  const now = useNow(30_000);
  // The stage picked on the issue-work line; the Runs widget filters its groups by it.
  const [lineStage, setLineStage] = useState<IssueStage | null>(null);

  if (!user) return null;

  const isAdmin = user.is_admin === true;
  // The viewer's OWN runs: what the Held work card, the platform line and the decisions
  // read. Never the admin rows, for an admin too. Held work is the owner's own custody.
  const ownRuns = data?.runs ?? [];
  const decisions = overviewDecisions(ownRuns, holds);
  // The counts follow the viewer's scope: own runs, or for an admin every user's runs
  // while the admin list is readable. An unreadable admin list falls back to the viewer's
  // own runs and the sentence says so.
  const adminUnavailable = isAdmin && adminRuns.failed;
  const factoryRows =
    isAdmin && adminRuns.runs != null && !adminRuns.failed
      ? mergeOverviewRows(ownRuns, adminRuns.runs, user.email)
      : null;
  // The issue-work line counts the Runs widget's populations: the viewer's own runs, and for
  // an admin with a readable list every other user's active runs.
  const lineModel = data
    ? issueLineModel({
        ownRuns,
        otherRuns:
          factoryRows != null
            ? (adminRuns.runs ?? []).filter((r) => r.owner_email !== user.email && !isTerminalRun(r.status))
            : [],
      })
    : null;
  const sentence = data
    ? statusSentence({
        decisions: decisionCount(decisions),
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

      {lineModel && (
        <IssueLine model={lineModel} selected={lineStage} onSelect={setLineStage} demo={demo} factory={factoryRows != null} />
      )}

      {/* Same inputs as Dashboard: the viewer's own runs, never the admin rows. */}
      <HealthPlatformLine isAdmin={isAdmin} runs={ownRuns} workers={data?.workers ?? []} />

      {/* Runs on the left, the right-hand rail beside it; the rail stacks below at narrow widths. */}
      <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_22.5rem]">
        <div className="min-w-0">
          <OverviewRunsWidget
            ownRuns={ownRuns}
            adminRuns={isAdmin ? adminRuns : null}
            isAdmin={isAdmin}
            viewerEmail={user.email}
            now={now}
            demo={demo}
            loading={!data}
            stage={lineStage}
            prRuns={lineModel?.prs}
          />
        </div>
        {/* Below xl the rail is full width: Capacity and Held work sit side by side from lg. */}
        <div className="min-w-0 space-y-6 lg:grid lg:grid-cols-2 lg:items-start lg:gap-6 lg:space-y-0 xl:block xl:space-y-6">
          {data && <CapacityRail workers={data.workers} />}
          {/* Not gated on `data`: custody states must show while the personal load is slow or failed. */}
          <HeldWorkCard holds={holds} holdsFailed={holdsFailed} recoveryWaitCount={ownRecoveryWaitCount(ownRuns)} />
        </div>
      </div>

      {/* Factory usage is rendered only from the admin-gated response. */}
      {data?.selfUsage && (
        <UsageCard self={data.selfUsage} admin={data.adminUsage ?? undefined} window={usageWindow} onWindowChange={changeUsageWindow} />
      )}
    </div>
  );
}
