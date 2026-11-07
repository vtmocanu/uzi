import type { ReactNode } from "react";
import { failureRecency } from "../lib/failureRecency";
import { useNow } from "../lib/useNow";
import type { SelfUsage, AdminUsage } from "../lib/api";
import { formatTokens, formatCost } from "../lib/formatTokens";
import { aggregateDisclosure } from "../lib/costStatus";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { Card, SectionTitle, cx } from "./ui";
import {
  failRate, failRateDelta, formatSince, summaryCost, perPrText,
  FailedRunsBlock, TopCauses, SinceLastFailedRun, WindowToggle, Th, Td,
} from "./usageParts";
import { breakdown, perUserRows } from "../lib/usageByUser";

// PRD #40: one usage summary, with personal figures for everyone and factory
// figures plus the selected-window per-user section only from the admin-gated response.

// Largest-remainder (Hamilton) rounding: integer percentages that sum to exactly
// 100. Floor each row's raw share, then award the leftover point(s) to the largest
// fractional remainders. Deterministic tie-break (the leftover often ties): larger
// raw total first, then row order — so per-row output is pinnable. Integer-exact
// (remainder == total*100 % factoryTotal) so ties compare exactly. Guards a
// non-positive factory total to all zeros; forcing 100 is correct only because the
// rows partition the factory total (AdminUsagePerUser and AdminUsageTotals sum the
// same non-chat runs).
function costShares(totals: number[], factoryTotal: number): number[] {
  if (factoryTotal <= 0) return totals.map(() => 0);
  const scaled = totals.map((t) => t * 100);
  const floors = scaled.map((s) => Math.floor(s / factoryTotal));
  const remainders = scaled.map((s, i) => s - floors[i] * factoryTotal); // exact s % factoryTotal
  const leftover = 100 - floors.reduce((a, b) => a + b, 0);
  const order = totals
    .map((_, i) => i)
    .sort((a, b) => {
      if (remainders[b] !== remainders[a]) return remainders[b] - remainders[a]; // largest remainder first
      if (totals[b] !== totals[a]) return totals[b] - totals[a]; // then larger raw total
      return a - b; // then row order
    });
  const shares = floors.slice();
  for (let k = 0; k < leftover && k < shares.length; k++) shares[order[k]] += 1;
  return shares;
}

// PRD #1429 M4b (D7) superseded PRD #40 Decision 8's "$0 renders '—'" heuristic: an
// aggregate's cost_usd retains recorded API-equivalent costs, including partial
// dollars from mixed unreported runs. Legacy subscription rows have no estimate.
// `aggregateDisclosure` names both populations whose cost is incomplete while
// retaining the recorded partial contributions in the displayed dollar total.

export type UsageWindow = "lifetime" | "last_7_days";

// Issue #3686: "N PRs · $X per PR" for one window. null when the server did not
// report the count (older API): never a fabricated 0. The per-PR cost divides the
// column's displayed cost, so an incomplete cost reads "at least $X per PR" with the
// cost disclosure's own title. Wording is "PRs" for every forge (no forgeNoun).
function prSummary(count: number | undefined, cost: number, disclosure: ReturnType<typeof aggregateDisclosure>): ReactNode {
  if (count == null) return null;
  const head = `${count.toLocaleString("en-US")} ${count === 1 ? "PR" : "PRs"}`;
  return <>{head} · {perPrText(count, cost, disclosure)}</>;
}

function UsageColumn({ usage, window, personal, runCountLine, owner, aligned = false }: {
  usage: SelfUsage;
  window: UsageWindow;
  personal: boolean;
  runCountLine: ReactNode;
  owner?: string;
  aligned?: boolean;
}) {
  const selected = breakdown(usage[window]);
  const outcomes = usage.outcomes[window];
  const lifetime = usage.outcomes.lifetime;
  const subscriptionCount = window === "lifetime" ? usage.lifetime_subscription_run_count : usage.last7_subscription_run_count;
  const unreportedCount = window === "lifetime" ? usage.lifetime_unreported_run_count : usage.last7_unreported_run_count;
  const disclosure = aggregateDisclosure(subscriptionCount, unreportedCount);
  const prText = prSummary(window === "lifetime" ? usage.lifetime_pr_count : usage.last7_pr_count, selected.cost, disclosure);
  const showRuns = window === "lifetime";
  const deltaText = failRateDelta(outcomes, lifetime);
  return (
    <section aria-label={personal ? "Your usage" : "Factory usage"} className={cx("min-w-0 space-y-3", aligned && "md:row-span-[7] md:grid md:grid-rows-subgrid md:space-y-0", aligned && !personal && "border-t border-edge pt-5 md:border-t-0 md:border-l md:pl-6 md:pt-0")}>
      <div><h3 className="text-sm font-semibold text-fg">{personal ? "You" : "Factory · all users"}</h3></div>
      <div>
        <div className="grid grid-cols-3 gap-x-4">
          <div className="min-w-0">
            <h4 className="text-xs text-muted">Cost (API-equivalent)</h4>
            <p className="mt-1 whitespace-nowrap font-mono text-[22px] font-semibold lg:text-[26px] tabular-nums tracking-tight text-brand">{summaryCost(selected.cost)}</p>
            {disclosure.incomplete && <p className="mt-1 truncate whitespace-nowrap text-[11px] leading-relaxed text-muted" title={disclosure.title}>{disclosure.text}</p>}
          </div>
          <div className="min-w-0">
            <h4 className="text-xs text-muted">Failed runs rate</h4>
            <p className="mt-1 whitespace-nowrap font-mono text-[22px] font-semibold lg:text-[26px] tabular-nums tracking-tight">{failRate(outcomes)}</p>
            {window === "last_7_days" && deltaText !== null && <p className="mt-1 text-[11px] leading-relaxed text-muted">{deltaText}</p>}
          </div>
          <div className="min-w-0">
            <h4 className="text-xs text-muted">Tokens</h4>
            <p className="mt-1 whitespace-nowrap font-mono text-[22px] font-semibold lg:text-[26px] tabular-nums tracking-tight">{formatTokens(selected.total)}</p>
            <ul className="mt-1 space-y-0.5 text-[11px] text-muted">
              <li className="whitespace-nowrap">in <span className="font-mono tabular-nums text-fg">{formatTokens(selected.fresh)}</span></li>
              <li className="whitespace-nowrap">cached <span className="font-mono tabular-nums text-fg">{formatTokens(selected.cached)}</span></li>
              <li className="whitespace-nowrap">out <span className="font-mono tabular-nums text-fg">{formatTokens(selected.out)}</span></li>
            </ul>
          </div>
        </div>
        {usage.run_count === 0 && <p className="mt-3 text-xs text-muted">No token usage recorded yet.</p>}
      </div>
      <div><FailedRunsBlock outcomes={outcomes} personal={personal} /></div>
      <div><TopCauses outcomes={outcomes} personal={personal} /></div>
      <div>{outcomes.needs_landing > 0 && <p className="text-[11px] text-muted">{outcomes.needs_landing.toLocaleString("en-US")} failed runs with recoverable work. May include work already landed.</p>}</div>
      <div>{(showRuns || prText != null) && <p className="text-[11px] text-muted">{showRuns && runCountLine}{showRuns && prText != null && " · "}{prText}</p>}</div>
      <div><SinceLastFailedRun outcomes={lifetime} owner={owner} /></div>
    </section>
  );
}

export function UsageCard({ self, admin, window, onWindowChange }: {
  self: SelfUsage;
  admin?: AdminUsage;
  window: UsageWindow;
  onWindowChange: (window: UsageWindow) => void;
}) {
  const demo = useDemoMode();
  const failureOwner = admin?.users.find((u) => u.user_id === admin.factory.outcomes.lifetime.last_failed_user_id);
  const owner = failureOwner ? maskEmail(failureOwner.email, demo) : undefined;
  const since = admin ? formatSince(admin.earliest_run) : "";
  return (
    <Card className="min-w-0">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <SectionTitle>Usage</SectionTitle>
        <WindowToggle value={window} onChange={onWindowChange} />
      </div>
      <div className={cx("mt-4 grid gap-x-6 gap-y-3", admin && "md:grid-cols-2 md:grid-rows-[repeat(7,auto)]")}>
        <UsageColumn usage={self} window={window} personal aligned={!!admin} runCountLine={`${self.run_count.toLocaleString("en-US")} ${self.run_count === 1 ? "run" : "runs"} with token usage`} />
        {admin && <UsageColumn usage={admin.factory} window={window} personal={false} aligned owner={owner} runCountLine={`${admin.factory.run_count.toLocaleString("en-US")} runs with token usage by ${admin.users.length} ${admin.users.length === 1 ? "user" : "users"}${since ? ` since ${since}` : ""}`} />}
      </div>
      {admin && (
        <section aria-label={`Per-user usage, ${window === "lifetime" ? "all time" : "last 7 days"}`} className="mt-5 border-t border-edge pt-4">
          <h3 className="text-sm font-semibold text-fg">Per user · {window === "lifetime" ? "all time" : "last 7 days"}</h3>
          <PerUserUsageTable admin={admin} window={window} />
        </section>
      )}
    </Card>
  );
}

function PerUserUsageTable({ admin, window }: { admin: AdminUsage; window: UsageWindow }) {
  const now = useNow(30_000);
  const demo = useDemoMode();
  const { available, rows, showPRs, factoryRuns, factoryPRs, factoryOutcomes, factory, factoryDisclosure } = perUserRows(admin, window);
  if (window === "last_7_days" && !available) {
    return <p className="mt-2 text-sm text-muted">Seven-day per-user usage is unavailable. Upgrade the API to view this period.</p>;
  }
  const factoryMicroUSD = Math.round(factory.cost * 1e6);
  const shares = factoryMicroUSD > 0
    ? costShares(rows.map((r) => Math.round(r.b.cost * 1e6)), factoryMicroUSD)
    : undefined;
  return (
    <>
      {rows.length === 0 ? (
        <p className="mt-2 text-sm text-faint">No usage across the factory yet.</p>
      ) : (
        <div className="mt-2 overflow-x-auto">
          <table className="w-full min-w-[740px] border-collapse text-xs">
            <thead>
              <tr>
                <Th left>User</Th>
                <Th>Runs</Th>
                {showPRs && <Th>PRs</Th>}
                <Th>Failed</Th>
                <Th>Fail rate</Th>
                <Th>Since last failure</Th>
                <Th>Tokens</Th>
                <Th>Out</Th>
                <Th>Cost</Th>
                <Th title="Share of recorded API-equivalent cost in the selected period">Cost share</Th>
              </tr>
            </thead>
            <tbody>
              {rows.map((u, i) => {
                // Rank uses unrounded recorded cost; shares use integer microdollars.
                // Largest-remainder rounding (see costShares) makes shares sum to 100%.
                const pct = shares?.[i];
                // This user's selected cost_usd includes recorded partial costs —
                // disclose their own subscription/unreported counts beside it rather than
                // let the dollar figure read as their complete total.
                const rowDisclosure = aggregateDisclosure(u.subscription, u.unreported);
                return (
                  <tr key={u.user_id}>
                    <Td left>{maskEmail(u.email, demo)}</Td>
                    <Td>{u.selectedRuns}</Td>
                    {showPRs && <Td>{u.selectedPRs}</Td>}
                    <Td>{u.selectedOutcomes.failed}</Td>
                    <Td title={`${u.selectedOutcomes.failed} of ${u.selectedOutcomes.finished} finished runs`}>
                      {failRate(u.selectedOutcomes)}
                    </Td>
                    <Td>{failureRecency(u.outcomes, now)}</Td>
                    <Td>{formatTokens(u.b.total)}</Td>
                    <Td>{formatTokens(u.b.out)}</Td>
                    <Td cost>
                      {formatCost(u.b.cost)}
                      {rowDisclosure.incomplete && (
                        <div className="whitespace-nowrap text-[9px] font-normal text-faint" title={rowDisclosure.title}>{rowDisclosure.text}</div>
                      )}
                    </Td>
                    <Td>
                      {pct === undefined ? (
                        <span title="Cost share unavailable: no recorded factory cost in the selected period">—</span>
                      ) : (
                        <span className="inline-flex items-center justify-end gap-2 whitespace-nowrap">
                          {pct}%
                          <span
                            className="inline-block h-1 w-16 overflow-hidden rounded bg-edge align-middle"
                            role="img"
                            aria-label={`${pct} percent of recorded factory cost`}
                          >
                            <span className="block h-full bg-brand" style={{ width: `${pct}%` }} />
                          </span>
                        </span>
                      )}
                    </Td>
                  </tr>
                );
              })}
              <tr>
                <Td left total>uzi total</Td>
                <Td total>{factoryRuns}</Td>
                {showPRs && <Td total>{factoryPRs}</Td>}
                <Td total>{factoryOutcomes.failed}</Td>
                <Td
                  total
                  title={`${factoryOutcomes.failed} of ${factoryOutcomes.finished} finished runs`}
                >
                  {failRate(factoryOutcomes)}
                </Td>
                <Td total>{failureRecency(admin.factory.outcomes.lifetime, now)}</Td>
                <Td total>{formatTokens(factory.total)}</Td>
                <Td total>{formatTokens(factory.out)}</Td>
                <Td total cost>
                  {formatCost(factory.cost)}
                  {factoryDisclosure.incomplete && (
                    <div className="whitespace-nowrap text-[9px] font-normal text-faint" title={factoryDisclosure.title}>{factoryDisclosure.text}</div>
                  )}
                </Td>
                <Td total> </Td>
              </tr>
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
