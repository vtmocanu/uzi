import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { failureRecency } from "../lib/failureRecency";
import { useNow } from "../lib/useNow";
import { stripUnsafeChars } from "../lib/safeText";
import type { SelfUsage, AdminUsage, RunUsage, RunOutcomes } from "../lib/api";
import { formatTokens, formatCost } from "../lib/formatTokens";
import { failOriginLabel } from "../lib/failOriginLabel";
import { aggregateDisclosure } from "../lib/costStatus";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { Card, SectionTitle, cx } from "./ui";

// PRD #40: one usage summary, with personal figures for everyone and factory
// figures plus the selected-window per-user section only from the admin-gated response.

// A RunUsage bundle split the way the run view splits it: fresh = fresh input +
// cache creation, cached = cache reads, out = output; total = all three.
function breakdown(u: RunUsage): { fresh: number; cached: number; out: number; total: number; cost: number } {
  const fresh = u.input_tokens + u.cache_creation_tokens;
  const cached = u.cache_read_tokens;
  const out = u.output_tokens;
  return { fresh, cached, out, total: fresh + cached + out, cost: u.cost_usd };
}

// Largest-remainder (Hamilton) rounding: integer percentages that sum to exactly
// 100. Floor each row's raw share, then award the leftover point(s) to the largest
// fractional remainders. Deterministic tie-break (the leftover often ties): larger
// raw total first, then row order — so per-row output is pinnable. Integer-exact
// (remainder == total*100 % factoryTotal) so ties compare exactly. Guards a
// non-positive factory total to all zeros; forcing 100 is correct only because the
// rows partition the factory total (AdminUsagePerUser and AdminUsageTotals sum the
// same non-chat runs).
function tokenShares(totals: number[], factoryTotal: number): number[] {
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
// aggregate's cost_usd is the METERED SUBSET's real dollar sum (a subscription or
// unreported run contributes $0 to that stored numeric by construction), so it is
// shown here as a genuine dollar figure, never hidden behind a dash, and any
// non-metered runs folded into the window are disclosed by count instead, via
// `aggregateDisclosure` below. Showing the dollar figure ALONE, with no signal that
// it excludes some runs, would be the aggregate shape of the same false-zero bug.

// PRD #1293 D6: the failed-run rate as failed / finished, one decimal, "—" when the scope
// has no finished runs (never a fabricated 0%). One helper for both cards' windows and the
// per-user table's Fail rate cell, so every surface rounds identically.
const failRate = (o: RunOutcomes): string =>
  o.finished === 0 ? "—" : `${((o.failed / o.finished) * 100).toFixed(1)}%`;

// formatSince renders the factory's earliest-run ISO timestamp as "12 May 2026" for
// the "since <date>" line; "" (falsy → the clause is dropped) when absent/unparseable.
function formatSince(iso: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

export type UsageWindow = "lifetime" | "last_7_days";

// Group dollars only in the summary cards; other usage surfaces keep their formatter.
function summaryCost(usd: number): string {
  const value = Number.isFinite(usd) && usd >= 0 ? usd : 0;
  return "$" + new Intl.NumberFormat("en-US", {
    minimumFractionDigits: value >= 1000 ? 0 : 2,
    maximumFractionDigits: value >= 1000 ? 0 : 2,
  }).format(value);
}

// PRD #1293: outcomes use the selected window and failed / finished denominator.
// Issue #1418: remove the needs_landing hatch because an aggregate cannot know
// whether recoverable work was landed later. It is a descriptive note, not backlog.
function FailedRunsBlock({ outcomes, personal }: { outcomes: RunOutcomes; personal: boolean }) {
  if (outcomes.finished === 0) return <p className="text-sm text-muted">No finished runs in this period.</p>;
  const segments = [
    { label: "completed", count: outcomes.completed, background: "rgb(var(--ok))" },
    { label: "cancelled", count: outcomes.cancelled, background: "rgb(var(--edge-strong))" },
    { label: "plan rejected", count: outcomes.plan_rejected, background: "repeating-linear-gradient(135deg, rgb(var(--edge-strong)) 0 2px, rgb(var(--surface)) 2px 4px)" },
    { label: "failed", count: outcomes.failed, background: "rgb(var(--danger))" },
  ].filter((s) => s.count > 0);
  return (
    <div className="border-t border-edge pt-3 space-y-2.5">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 text-xs">
        <p className="text-muted"><span className="tabular-nums text-fg">{outcomes.failed.toLocaleString("en-US")}</span> failed of <span className="tabular-nums text-fg">{outcomes.finished.toLocaleString("en-US")}</span> finished runs</p>
        {personal && <Link className="text-brand hover:underline" to="/runs/history?status=failed">Your failed runs →</Link>}
      </div>
      <div className="flex h-2 gap-0.5 overflow-hidden rounded" role="img" aria-label={"Finished runs: " + segments.map((s) => `${s.count} ${s.label}`).join(", ")}>
        {segments.map((s) => (
          <span key={s.label} className="block h-full" style={{ width: `${s.count / outcomes.finished * 100}%`, background: s.background }} />
        ))}
      </div>
      <ul className="flex flex-wrap gap-x-3.5 gap-y-1 text-[11px] text-muted">
        {segments.map((s) => (
          <li key={s.label} className="inline-flex items-center gap-1.5">
            <span aria-hidden="true" className="h-2 w-2 rounded-sm" style={{ background: s.background }} />
            {s.label} <span className="tabular-nums text-fg">{s.count.toLocaleString("en-US")}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function TopCauses({ outcomes, personal }: { outcomes: RunOutcomes; personal: boolean }) {
  const causes = Object.entries(outcomes.fail_origins)
    .filter(([, count]) => count > 0)
    .sort(([a, ac], [b, bc]) => bc - ac || a.localeCompare(b));
  const hiddenCauses = Math.max(0, causes.length - 3);
  if (outcomes.failed === 0 || causes.length === 0) return null;
  return (
    <p className="text-xs text-muted">
      Top causes: {causes.slice(0, 3).map(([origin, count]) => `${stripUnsafeChars(failOriginLabel(origin))} ${count.toLocaleString("en-US")}`).join(" · ")}
      {hiddenCauses > 0 && <>{" "}{personal ? <Link className="whitespace-nowrap text-brand hover:underline" to="/runs/history?status=failed">+{hiddenCauses} more</Link> : <span className="whitespace-nowrap">+{hiddenCauses} more</span>}</>}
    </p>
  );
}

function SinceLastFailedRun({ outcomes, owner }: { outcomes: RunOutcomes; owner?: string }) {
  const now = useNow(30_000);
  const value = failureRecency(outcomes, now);
  const hasFailure = outcomes.last_failed_at != null && value !== "Unavailable";
  const originLabel = stripUnsafeChars(failOriginLabel(outcomes.last_failed_origin ?? "unknown"));
  const ownerLabel = owner ? stripUnsafeChars(owner) : undefined;
  return (
    <div className="min-w-0 border-t border-edge pt-3 text-xs text-muted">
      {hasFailure ? (
        <p className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-1">
          <span className="shrink-0 text-fg">Latest failure <span className="font-mono tabular-nums">{value}</span> ago</span>
          <span className="flex min-w-0 flex-1 basis-[120px] items-baseline gap-x-3">
            <span className="min-w-0 truncate" title={originLabel}>{originLabel}</span>
            {/* Owner shrinks to an ellipsis before the cause loses its space. */}
            {ownerLabel && <span className="min-w-0 max-w-max grow shrink-0 basis-[1em] truncate" title={ownerLabel}>{ownerLabel}</span>}
          </span>
          <span className="shrink-0">{outcomes.completed_since_last_failure ?? 0} completed since</span>
          {outcomes.last_failed_run_id && (
            <Link className="shrink-0 whitespace-nowrap text-brand hover:underline" to={`/runs/${outcomes.last_failed_run_id}`}>run {outcomes.last_failed_run_id.slice(0, 8)} →</Link>
          )}
        </p>
      ) : value === "Unavailable" ? (
        <p>Latest failure: unavailable</p>
      ) : (
        <p className={outcomes.finished > 0 ? "text-ok" : "text-muted"}>
          {outcomes.finished === 0 ? "No finished runs yet" : `No recorded failures · ${outcomes.completed} completed runs, none failed`}
        </p>
      )}
    </div>
  );
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
  const delta = lifetime.finished > 0 && outcomes.finished > 0
    ? (outcomes.failed / outcomes.finished - lifetime.failed / lifetime.finished) * 100 : null;
  const deltaText = delta == null ? "" : `${delta < 0 ? "−" : delta > 0 ? "+" : ""}${Math.abs(delta).toFixed(1)} pp vs all-time ${failRate(lifetime)}`;
  return (
    <section aria-label={personal ? "Your usage" : "Factory usage"} className={cx("min-w-0 space-y-3", aligned && "md:row-span-[7] md:grid md:grid-rows-subgrid md:space-y-0", aligned && !personal && "border-t border-edge pt-5 md:border-t-0 md:border-l md:pl-6 md:pt-0")}>
      <div><h3 className="text-sm font-semibold text-fg">{personal ? "You" : "Factory · all users"}</h3></div>
      <div>
        <div className="grid grid-cols-3 gap-x-4">
          <div className="min-w-0">
            <h4 className="text-xs text-muted">Metered cost</h4>
            <p className="mt-1 whitespace-nowrap font-mono text-[22px] font-semibold lg:text-[26px] tabular-nums tracking-tight text-brand">{summaryCost(selected.cost)}</p>
            {disclosure.incomplete && <p className="mt-1 truncate whitespace-nowrap text-[11px] leading-relaxed text-muted" title={disclosure.title}>{disclosure.text}</p>}
          </div>
          <div className="min-w-0">
            <h4 className="text-xs text-muted">Failed runs rate</h4>
            <p className="mt-1 whitespace-nowrap font-mono text-[22px] font-semibold lg:text-[26px] tabular-nums tracking-tight">{failRate(outcomes)}</p>
            {window === "last_7_days" && delta !== null && <p className="mt-1 text-[11px] leading-relaxed text-muted">{deltaText}</p>}
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
      <div>{window === "lifetime" && <p className="text-[11px] text-muted">{runCountLine}</p>}</div>
      <div><SinceLastFailedRun outcomes={lifetime} owner={owner} /></div>
    </section>
  );
}

function WindowToggle({ value, onChange }: { value: UsageWindow; onChange: (window: UsageWindow) => void }) {
  return (
    <div role="group" aria-label="Usage reporting period" className="ml-auto inline-flex shrink-0 rounded-full border border-edge bg-raised p-0.5">
      {(["lifetime", "last_7_days"] as const).map((window) => (
        <button
          key={window}
          type="button"
          aria-pressed={value === window}
          onClick={() => onChange(window)}
          className={cx(
            "min-h-11 rounded-full border px-2.5 text-xs focus-visible:outline-solid focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring sm:min-h-8",
            value === window ? "border-edge-strong bg-surface font-semibold text-fg shadow-sm" : "border-transparent font-normal text-muted hover:text-fg",
          )}
        >
          {window === "lifetime" ? "All time" : "Last 7 days"}
        </button>
      ))}
    </div>
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

function Th({ children, left }: { children: ReactNode; left?: boolean }) {
  return (
    <th
      className={
        "border-b border-edge px-2.5 py-1.5 text-[10.5px] font-semibold uppercase tracking-[0.06em] text-faint " +
        (left ? "text-left" : "text-right")
      }
    >
      {children}
    </th>
  );
}

function Td({
  children,
  left,
  total,
  cost,
  title,
}: {
  children?: ReactNode;
  left?: boolean;
  total?: boolean;
  cost?: boolean;
  title?: string;
}) {
  const cls = ["px-2.5 py-1.5"];
  cls.push(left ? "text-left font-sans" : "text-right font-mono tabular-nums");
  if (total) cls.push("font-semibold text-fg");
  else cls.push(cost ? "text-brand" : left ? "text-fg" : "text-muted", "border-b border-edge/50");
  return (
    <td className={cls.join(" ")} title={title}>
      {children}
    </td>
  );
}

function PerUserUsageTable({ admin, window }: { admin: AdminUsage; window: UsageWindow }) {
  const now = useNow(30_000);
  const demo = useDemoMode();
  const recent = window === "last_7_days";
  const available = admin.users.every((u) =>
    u.last_7_days != null && u.last7_run_count != null && u.last7_outcomes != null &&
    u.last7_subscription_run_count != null && u.last7_unreported_run_count != null,
  );
  if (recent && !available) {
    return <p className="mt-2 text-sm text-muted">Seven-day per-user usage is unavailable. Upgrade the API to view this period.</p>;
  }
  const factory = breakdown(admin.factory[window]);
  const factoryOutcomes = admin.factory.outcomes[window];
  const rows = admin.users.map((u) => ({
    ...u,
    b: breakdown(recent ? u.last_7_days! : u.usage),
    selectedRuns: recent ? u.last7_run_count! : u.run_count,
    selectedOutcomes: recent ? u.last7_outcomes! : u.outcomes,
    subscription: recent ? u.last7_subscription_run_count! : u.subscription_run_count,
    unreported: recent ? u.last7_unreported_run_count! : u.unreported_run_count,
  })).sort((a, b) => b.b.total - a.b.total || b.b.cost - a.b.cost || b.b.out - a.b.out ||
    (a.user_id < b.user_id ? -1 : a.user_id > b.user_id ? 1 : 0));
  const shares = tokenShares(rows.map((r) => r.b.total), factory.total);
  const factoryRuns = recent ? rows.reduce((sum, u) => sum + u.selectedRuns, 0) : admin.factory.run_count;
  const factoryDisclosure = aggregateDisclosure(
    recent ? admin.factory.last7_subscription_run_count : admin.factory.lifetime_subscription_run_count,
    recent ? admin.factory.last7_unreported_run_count : admin.factory.lifetime_unreported_run_count,
  );
  return (
    <>
      {rows.length === 0 ? (
        <p className="mt-2 text-sm text-faint">No usage across the factory yet.</p>
      ) : (
        <div className="mt-2 overflow-x-auto">
          <table className="w-full min-w-[680px] border-collapse text-xs">
            <thead>
              <tr>
                <Th left>User</Th>
                <Th>Runs</Th>
                <Th>Failed</Th>
                <Th>Fail rate</Th>
                <Th>Since last failure</Th>
                <Th>Tokens</Th>
                <Th>Out</Th>
                <Th>Cost</Th>
                <Th>Share</Th>
              </tr>
            </thead>
            <tbody>
              {rows.map((u, i) => {
                // Rank and Share both use total tokens, including subscription runs.
                // Largest-remainder rounding (see tokenShares) so the column sums to 100%.
                const pct = shares[i];
                // PRD #1429 D7: this user's selected cost_usd is their metered subset —
                // disclose their own subscription/unreported counts beside it rather than
                // let the dollar figure read as their complete total.
                const rowDisclosure = aggregateDisclosure(u.subscription, u.unreported);
                return (
                  <tr key={u.user_id}>
                    <Td left>{maskEmail(u.email, demo)}</Td>
                    <Td>{u.selectedRuns}</Td>
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
                      <span className="inline-flex items-center justify-end gap-2 whitespace-nowrap">
                        {pct}%
                        <span
                          className="inline-block h-1 w-16 overflow-hidden rounded bg-edge align-middle"
                          role="img"
                          aria-label={`${pct} percent of factory tokens`}
                        >
                          <span className="block h-full bg-brand" style={{ width: `${pct}%` }} />
                        </span>
                      </span>
                    </Td>
                  </tr>
                );
              })}
              <tr>
                <Td left total>uzi total</Td>
                <Td total>{factoryRuns}</Td>
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
