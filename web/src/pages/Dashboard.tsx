// The overview: stat tiles, an onboarding checklist derived from the same
// preconditions the board's start-run gate checks (lib/runStream.ts
// startRunGate), and the most recent runs. Replaces the old account-metadata
// table (moved to Settings), which told a new user nothing about what to do
// next.

import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { isTerminalRun } from "../lib/api";
import { isAccountCooldownWaiting } from "../lib/runBadge";
import { useNow } from "../lib/useNow";
import { UsageCard } from "../components/UsageCards";
import { OnboardingChecklist } from "../components/OnboardingChecklist";
import { OverviewRunRow } from "../components/OverviewRunRow";
import { WorkerLoadCard } from "../components/WorkerLoadCard";
import { CustodyBoardAlert } from "../components/CustodyBoardAlert";
import { HealthOverviewCard } from "../components/HealthOverviewCard";
import { HealthPlatformLine } from "../components/HealthPlatformLine";
import { activeRunBuckets, ownRecoveryWaitCount, setupComplete, workerJoined } from "../lib/overview";
import { useOverviewData } from "../lib/useOverviewData";
import { Card, PageHeader, SectionTitle, Skeleton, StatTile } from "../components/ui";
import { useDemoMode } from "../lib/demoMode";
import { maskName } from "../lib/demoMask";

export function Dashboard() {
  const demo = useDemoMode();
  const { user } = useAuth();
  // The data loading and both polls live in useOverviewData, shared with the
  // /overview/next preview. Dashboard asks for no admin runs, so it sends exactly the
  // requests it always has.
  const { data, usageWindow, changeUsageWindow } = useOverviewData();
  // PRD #1064 M3: a slow clock to age the board cards' "now" line client-side. Declared
  // with the other hooks, above the early return, so the hook order is stable.
  const now = useNow(30_000);

  if (!user) return null;

  // The tile counts every non-terminal run. Its hint separates execution,
  // owner input (including an interactive follow-up), queued work and resume holds.
  // Raw running includes planning and review iterations; it counts runs, not agents.
  const active = data?.runs.filter((r) => !isTerminalRun(r.status)) ?? [];
  const activeCounts = { running: 0, needsYou: 0, queued: 0, waiting: 0 };
  for (const run of active) {
    const bucket = isAccountCooldownWaiting(run) ? "waiting" : activeRunBuckets[run.status];
    if (bucket) activeCounts[bucket]++;
  }
  // PRD #1349 M6 (D8/D10): recovery_wait_count, derived CLIENT-SIDE from the runs the
  // dashboard already polls — the count of the owner's runs currently self-healing in the
  // recovery_wait park. Surfaced in the custody alert for diagnosis (it explains why holds
  // may be accumulating); no backend field and no lifetime cap on the count.
  const recoveryWaitCount = ownRecoveryWaitCount(data?.runs ?? []);
  const activeHint = active.length
    ? [
        activeCounts.running ? `${activeCounts.running} running` : null,
        activeCounts.needsYou ? `${activeCounts.needsYou} ${activeCounts.needsYou === 1 ? "needs" : "need"} you` : null,
        activeCounts.queued ? `${activeCounts.queued} queued` : null,
        activeCounts.waiting ? `${activeCounts.waiting} waiting to resume` : null,
      ].filter(Boolean).join(" · ")
    : "nothing in flight";
  const recent = data?.runs.slice(0, 5) ?? [];
  const joined = workerJoined(data?.workers ?? []);
  const ready = data ? setupComplete(data, joined) : false;

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Welcome${user.display_name ? `, ${maskName(user.display_name, demo)}` : ""}`}
        description="The factory floor at a glance."
        actions={
          <Link to="/overview/next" className="text-sm font-medium text-brand hover:text-brand-hover">
            Try the new Overview
          </Link>
        }
      />

      {/* PRD #1349 M6 (D8): the conditional custody-pressure alert. Self-hides unless a hold
          needs a decision, a run is blocked, or the admission limit is reached; it fetches
          and refreshes its own aggregate, so it can render before the rest of the dashboard
          settles. */}
      <CustodyBoardAlert recoveryWaitCount={recoveryWaitCount} />

      {/* Instance health on Overview (PRD #1484 M5). Admins get the self-hiding health card
          (fed by the shared HealthStatusProvider — no fetch for a non-admin). A non-admin
          whose own hosted fleet is stuck gets the platform line instead, derived only from
          their own runs + workers already fetched above. The two are mutually exclusive. */}
      <HealthOverviewCard />
      <HealthPlatformLine
        isAdmin={user.is_admin === true}
        runs={data?.runs ?? []}
        workers={data?.workers ?? []}
      />

      {!data ? (
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-24" />
          ))}
        </div>
      ) : (
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <StatTile
            label="Active runs"
            value={active.length}
            hint={activeHint}
            to="/runs"
          />
          <StatTile
            label="Workers online"
            value={`${data.workersOnline}/${data.workersTotal}`}
            hint="your fleet"
            to="/workers"
          />
          <StatTile label="Repos" value={data.reposEnabled} hint="enabled repos" to="/repos" />
          <StatTile label="Agent templates" value={data.templates} hint="roles available" to="/agents" />
        </div>
      )}

      {/* Per-worker resource load (PRD #49): a compact "cpu · mem" line per worker
          that has reported a sample, so the factory floor shows live pressure at a
          glance. Hidden until at least one worker reports (no empty card churn). */}
      {data && <WorkerLoadCard workers={data.workers} />}

      {data && !ready && (
        <OnboardingChecklist
          hasToken={data.hasToken}
          hasForge={data.hasForge}
          reposEnabled={data.reposEnabled}
          workerJoined={joined}
        />
      )}

      {/* Factory usage is rendered only from the admin-gated response. */}
      {data?.selfUsage && (
        <UsageCard self={data.selfUsage} admin={data.adminUsage ?? undefined} window={usageWindow} onWindowChange={changeUsageWindow} />
      )}

      <Card>
        <div className="flex items-center justify-between">
          <SectionTitle>Recent runs</SectionTitle>
          <Link to="/runs" className="text-xs font-medium text-brand hover:text-brand-hover">
            All runs →
          </Link>
        </div>
        {!data ? (
          <div className="mt-3 space-y-2">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : recent.length === 0 ? (
          <p className="mt-3 text-sm text-faint">
            No runs yet — start one from a board card once the checklist above is green.
          </p>
        ) : (
          <ul className="mt-2 divide-y divide-edge">
            {recent.map((r) => (
              <OverviewRunRow key={r.id} run={r} now={now} demo={demo} />
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}
