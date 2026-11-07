import { useState, type ReactNode } from "react";
import type { SelfUsage, AdminUsage, RunOutcomes } from "../lib/api";
import { formatTokens } from "../lib/formatTokens";
import { aggregateDisclosure } from "../lib/costStatus";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { breakdown, perUserRows } from "../lib/usageByUser";
import { Card, SectionTitle, cx } from "./ui";
import type { UsageWindow } from "./UsageCards";
import {
  failRate, failRateDelta, formatSince, summaryCost, perPrText, failureAgo,
  FailedRunsBlock, TopCauses, SinceLastFailedRun, SegmentedToggle, WindowToggle,
} from "./usageParts";

// PRD #3881: the Overview preview's one Usage card. A You / Factory scope toggle (admins
// only) and the window toggle switch one set of figures, instead of the current Overview's
// side-by-side columns; the Factory scope adds a By user table below.

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

const LAST_FAILURE_TITLE =
  "All time: the API serves the last failure time for all time only, so this column is the same in both windows";

function Th({ children, left, title }: { children: ReactNode; left?: boolean; title?: string }) {
  return (
    <th
      scope="col"
      title={title}
      className={cx("whitespace-nowrap border-b border-edge px-2.5 py-2 text-[11px] font-medium uppercase tracking-[0.04em] text-faint", left ? "text-left" : "text-right")}
    >
      {children}
    </th>
  );
}

function Td({ children, left, total, title }: { children?: ReactNode; left?: boolean; total?: boolean; title?: string }) {
  return (
    <td
      title={title}
      className={cx(
        "whitespace-nowrap px-2.5 py-2 align-top tabular-nums",
        left ? "text-left" : "text-right",
        total ? "border-t-2 border-edge-strong font-semibold text-fg" : "border-b border-edge/50 text-fg",
      )}
    >
      {children}
    </td>
  );
}

function FailRateCell({ outcomes, total }: { outcomes: RunOutcomes; total?: boolean }) {
  return (
    <Td total={total} title={`${outcomes.failed} of ${outcomes.finished} finished runs`}>
      {failRate(outcomes)}
      <span className="block text-[11px] font-normal text-faint">of {outcomes.finished.toLocaleString("en-US")} finished</span>
    </Td>
  );
}

function CostCell({ cost, disclosure, total }: { cost: number; disclosure: ReturnType<typeof aggregateDisclosure>; total?: boolean }) {
  return (
    <Td total={total}>
      <span className="font-mono">{summaryCost(cost)}</span>
      {disclosure.incomplete && <span className="block text-[11px] font-normal text-faint" title={disclosure.title}>{disclosure.text}</span>}
    </Td>
  );
}

// PRD #3881 M2: the Factory scope's per-user table. Columns are fixed on every API version;
// a PR count the API does not report reads "—", never 0.
function ByUserTable({ admin, window, viewerId }: { admin: AdminUsage; window: UsageWindow; viewerId: string }) {
  const now = useNow(30_000);
  const demo = useDemoMode();
  const { available, rows, factoryRuns, factoryPRs, factoryOutcomes, factory, factoryDisclosure } = perUserRows(admin, window);
  const recent = window === "last_7_days";
  return (
    <section aria-label="By user" className="mt-5 border-t border-edge pt-4">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <h3 className="text-sm font-semibold text-fg">By user</h3>
        <span className="text-xs text-faint">{recent ? "last 7 days" : "all time"}, by cost</span>
      </div>
      {recent && !available ? (
        <p className="mt-2 text-sm text-muted">Seven-day per-user usage is unavailable. Upgrade the API to view this period.</p>
      ) : rows.length === 0 ? (
        <p className="mt-2 text-sm text-faint">No usage across the factory yet.</p>
      ) : (
        <div data-testid="by-user-scroll" className="mt-2 overflow-x-auto">
          <table className="w-full min-w-[640px] border-collapse text-[13px]">
            <thead>
              <tr>
                <Th left>User</Th>
                <Th title="Runs with recorded usage in the period">Runs</Th>
                <Th>PRs</Th>
                <Th>Failed</Th>
                <Th title="Failed of finished runs in the period">Fail rate</Th>
                <Th title={LAST_FAILURE_TITLE}>Last failure</Th>
                <Th>Cost</Th>
              </tr>
            </thead>
            <tbody>
              {rows.map((u) => (
                <tr key={u.user_id}>
                  <Td left>
                    {stripUnsafeChars(maskEmail(u.email, demo))}
                    {u.user_id === viewerId && <span className="font-normal text-faint"> (you)</span>}
                  </Td>
                  <Td>{u.selectedRuns.toLocaleString("en-US")}</Td>
                  <Td>{u.selectedPRs == null ? "—" : u.selectedPRs.toLocaleString("en-US")}</Td>
                  <Td>{u.selectedOutcomes.failed.toLocaleString("en-US")}</Td>
                  <FailRateCell outcomes={u.selectedOutcomes} />
                  <Td>{failureAgo(u.outcomes, now)}</Td>
                  <CostCell cost={u.b.cost} disclosure={aggregateDisclosure(u.subscription, u.unreported)} />
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr>
                <Td left total>Factory</Td>
                <Td total>{factoryRuns.toLocaleString("en-US")}</Td>
                <Td total>{factoryPRs == null ? "—" : factoryPRs.toLocaleString("en-US")}</Td>
                <Td total>{factoryOutcomes.failed.toLocaleString("en-US")}</Td>
                <FailRateCell outcomes={factoryOutcomes} total />
                <Td total>{failureAgo(admin.factory.outcomes.lifetime, now)}</Td>
                <CostCell cost={factory.cost} disclosure={factoryDisclosure} total />
              </tr>
            </tfoot>
          </table>
        </div>
      )}
    </section>
  );
}

export function OverviewUsageCard({ self, admin, window, onWindowChange, viewerId }: {
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
      {scope === "factory" && admin && <ByUserTable admin={admin} window={window} viewerId={viewerId} />}
    </Card>
  );
}
