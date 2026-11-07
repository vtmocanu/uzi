import { useState, type ReactNode } from "react";
import type { SelfUsage, AdminUsage } from "../lib/api";
import { formatTokens } from "../lib/formatTokens";
import { aggregateDisclosure } from "../lib/costStatus";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { Card, SectionTitle } from "./ui";
import type { UsageWindow } from "./UsageCards";
import {
  breakdown, failRate, failRateDelta, formatSince, summaryCost, perPrText,
  FailedRunsBlock, TopCauses, SinceLastFailedRun, SegmentedToggle, WindowToggle,
} from "./usageParts";

// PRD #3881 M1: the Overview preview's one Usage card. A You / Factory scope toggle (admins
// only) and the window toggle switch one set of figures, instead of the current Overview's
// side-by-side columns. The by-user table is M2.

type Scope = "you" | "factory";

const SCOPE_OPTIONS = [
  { value: "you", label: "You" },
  { value: "factory", label: "Factory · all users" },
] as const;

function Kpi({ label, value, valueClass = "", sub }: { label: string; value: ReactNode; valueClass?: string; sub?: ReactNode }) {
  return (
    <div className="min-w-0">
      <h4 className="text-xs text-muted">{label}</h4>
      <p className={`mt-0.5 whitespace-nowrap font-mono text-[28px] font-medium leading-[1.2] tabular-nums ${valueClass}`}>{value}</p>
      {sub != null && sub !== "" && <div className="mt-0.5 text-xs text-faint">{sub}</div>}
    </div>
  );
}

// viewerId is accepted for M2's "(you)" marker in the by-user table.
export function OverviewUsageCard({ self, admin, window, onWindowChange }: {
  self: SelfUsage;
  admin?: AdminUsage;
  window: UsageWindow;
  onWindowChange: (w: UsageWindow) => void;
  viewerId: string;
}) {
  const demo = useDemoMode();
  const [chosen, setScope] = useState<Scope>("you");
  const scope: Scope = admin ? chosen : "you";
  const personal = scope === "you";
  const usage = scope === "factory" && admin ? admin.factory : self;

  const selected = breakdown(usage[window]);
  const outcomes = usage.outcomes[window];
  const lifetime = usage.outcomes.lifetime;
  const recent = window === "last_7_days";
  const disclosure = aggregateDisclosure(
    recent ? usage.last7_subscription_run_count : usage.lifetime_subscription_run_count,
    recent ? usage.last7_unreported_run_count : usage.lifetime_unreported_run_count,
  );
  const prCount = recent ? usage.last7_pr_count : usage.lifetime_pr_count;

  const failureOwner = admin?.users.find((u) => u.user_id === admin.factory.outcomes.lifetime.last_failed_user_id);
  const owner = !personal && failureOwner ? maskEmail(failureOwner.email, demo) : undefined;

  let failSub: ReactNode = null;
  if (recent) failSub = failRateDelta(outcomes, lifetime);
  else if (!personal && admin) {
    const since = formatSince(admin.earliest_run);
    if (since) failSub = `across every user since ${since}`;
  }

  return (
    <Card className="min-w-0">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <SectionTitle>Usage</SectionTitle>
        <div className="flex flex-wrap items-center gap-2">
          {admin && <SegmentedToggle label="Usage scope" options={SCOPE_OPTIONS} value={scope} onChange={setScope} className="" />}
          <WindowToggle value={window} onChange={onWindowChange} className="" />
        </div>
      </div>
      <section aria-label={personal ? "Your usage" : "Factory usage"} className="min-w-0">
        <div className="mt-4 grid grid-cols-2 gap-x-6 gap-y-4 md:grid-cols-4">
          <Kpi
            label="Cost (API-equivalent)"
            valueClass="text-brand"
            value={summaryCost(selected.cost)}
            sub={disclosure.incomplete ? <span title={disclosure.title}>{disclosure.text}</span> : "estimated at API list prices"}
          />
          <Kpi
            label="Pull requests opened"
            value={prCount == null ? "—" : prCount.toLocaleString("en-US")}
            sub={prCount == null ? null : perPrText(prCount, selected.cost, disclosure)}
          />
          <Kpi label="Failed runs rate" value={failRate(outcomes)} sub={failSub} />
          <Kpi
            label="Tokens"
            value={formatTokens(selected.total)}
            sub={`in ${formatTokens(selected.fresh)}, cached ${formatTokens(selected.cached)}, out ${formatTokens(selected.out)}`}
          />
        </div>
        {usage.run_count === 0 && <p className="mt-3 text-xs text-muted">No token usage recorded yet.</p>}
        <div className="mt-5 grid gap-6 border-t border-edge pt-4 md:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)]">
          <div className="min-w-0 space-y-3">
            <FailedRunsBlock outcomes={outcomes} personal={personal} framed={false} />
            {outcomes.needs_landing > 0 && (
              <p className="text-[11px] text-muted">{outcomes.needs_landing.toLocaleString("en-US")} failed runs with recoverable work. May include work already landed.</p>
            )}
          </div>
          <div className="min-w-0 space-y-3">
            <TopCauses outcomes={outcomes} personal={personal} variant="list" />
            <SinceLastFailedRun outcomes={lifetime} owner={owner} label="Latest failure (all time)" wrap />
          </div>
        </div>
      </section>
    </Card>
  );
}
