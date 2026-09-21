import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { SelfUsage, AdminUsage, RunUsage, RunOutcomes } from "../lib/api";
import { formatTokens, formatCost } from "../lib/formatTokens";
import { failOriginLabel } from "../lib/failOriginLabel";
import { aggregateDisclosure, type AggregateDisclosure } from "../lib/costStatus";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { Card, SectionTitle } from "./ui";

// PRD #40 §3–4: the dashboard usage cards. "Your usage" is for everyone; the
// factory total + per-user breakdown are admin-only (the page only fetches
// /api/admin/usage for an admin, so a non-admin never receives the data).

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
// shown here as a genuine figure via formatCost — never hidden behind "—" — and any
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

function BigNum({ tokens }: { tokens: number }) {
  return (
    <div className="mt-1 font-mono text-[26px] font-semibold tabular-nums tracking-tight">
      {formatTokens(tokens)} <em className="text-sm not-italic text-faint">tokens</em>
    </div>
  );
}

// `disclosure` names the subscription/unreported runs folded into this window whose
// dollar contribution is NOT in `usage.cost_usd` (PRD #1429 D7) — optional so a
// per-user table row (which renders its own compact disclosure, see PerUserUsageTable)
// can reuse `breakdown`'s cost figure without duplicating this line.
function Subrow({ usage, disclosure }: { usage: RunUsage; disclosure?: AggregateDisclosure }) {
  const b = breakdown(usage);
  return (
    <div className="mt-2">
      <div className="flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-muted">
        <span>in <span className="tabular-nums text-fg">{formatTokens(b.fresh)}</span></span>
        <span>cached <span className="tabular-nums text-fg">{formatTokens(b.cached)}</span></span>
        <span>out <span className="tabular-nums text-fg">{formatTokens(b.out)}</span></span>
        <span>cost <span className="tabular-nums text-brand">{formatCost(b.cost)}</span></span>
      </div>
      {/* Byte-identical to nothing when the window is fully metered (disclosure.text
          is "" then) — no disclosure noise on the common case. */}
      {disclosure?.incomplete && <p className="mt-1 text-[11px] text-faint">{disclosure.text}</p>}
    </div>
  );
}

// FailedRunsBlock is the PRD #1293 failed-run block folded under the token figures of a
// usage card (mock variant A): the rate, the counts sentence with the 7-day window, the
// stacked outcome bar (completed → cancelled → plan rejected → failed → failed, needs
// landing) with a legend and an aria-label, and the top fail_origin causes. The last two
// segments split the single `failed` bucket (issue #1418) but sum to the same width.
// `lifetime` drives everything except the
// "last 7d" clause, which reads `last7`. Hidden entirely when the scope has no
// finished runs (mirrors the run_count === 0 precedent — never a fabricated 0%). The block
// counts a different population from the card's run_count (D2), so they are never combined
// into one fraction.
function FailedRunsBlock({ lifetime, last7 }: { lifetime: RunOutcomes; last7: RunOutcomes }) {
  if (lifetime.finished === 0) return null;
  // Fixed order; colours are theme tokens only (no literal hex) so the bar reads in both
  // Dawn and Ember. plan rejected is a 45° hatch so it stays distinct without colour.
  const OK = "rgb(var(--ok))";
  const CANCEL = "rgb(var(--edge-strong))";
  const REJECT = "repeating-linear-gradient(135deg, rgb(var(--edge-strong)) 0 2px, rgb(var(--surface)) 2px 4px)";
  const FAIL = "rgb(var(--danger))";
  // issue #1418: "failed, needs landing" is a SUB-CUT of failed — same danger colour, but a
  // 45° hatch (the plan-rejected hatch idiom keyed on --danger) so it reads as "failed, but
  // its committed work is landable/recoverable" while staying visually part of the failed bar.
  const NEEDS_LANDING = "repeating-linear-gradient(135deg, rgb(var(--danger)) 0 2px, rgb(var(--surface)) 2px 4px)";
  // Split the single `failed` bucket into two ADJACENT segments driven by the same
  // `/ lifetime.finished` denominator, so their combined width equals the old `failed` width.
  // Math.max(0, …) is defensive: the server guarantees needs_landing <= failed. The
  // needs-landing segment is added (next to failed) only when there IS something to land, so a
  // scope with no landable failures reads exactly as before — no "0" clutter in the bar/legend.
  const segments = [
    { label: "completed", count: lifetime.completed, background: OK },
    { label: "cancelled", count: lifetime.cancelled, background: CANCEL },
    { label: "plan rejected", count: lifetime.plan_rejected, background: REJECT },
    { label: "failed", count: Math.max(0, lifetime.failed - lifetime.needs_landing), background: FAIL },
    ...(lifetime.needs_landing > 0
      ? [{ label: "failed, needs landing", count: lifetime.needs_landing, background: NEEDS_LANDING }]
      : []),
  ];
  const barLabel = "Finished runs: " + segments.map((s) => `${s.count} ${s.label}`).join(", ");
  // Top causes: up to 4 fail_origin buckets, descending by count. Array.sort is stable, so
  // ties keep the map's insertion (vocabulary) order. Omit the line when there are none.
  const causes = Object.entries(lifetime.fail_origins)
    .sort((a, b) => b[1] - a[1])
    .slice(0, 4);
  return (
    <div className="mt-4 border-t border-edge pt-3.5">
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <SectionTitle>Failed runs</SectionTitle>
        <span className="font-mono text-[22px] font-semibold tabular-nums tracking-tight text-fg">
          {failRate(lifetime)}
        </span>
        <span className="text-xs text-muted">
          <span className="tabular-nums text-fg">{lifetime.failed}</span> of{" "}
          <span className="tabular-nums text-fg">{lifetime.finished}</span> finished runs ·{" "}
          <span className="tabular-nums text-fg">{failRate(last7)}</span> ({last7.failed} of {last7.finished}) last 7d
        </span>
      </div>
      <div className="mt-2.5 flex h-2 gap-0.5 overflow-hidden rounded" role="img" aria-label={barLabel}>
        {segments.map((s) => (
          <span
            key={s.label}
            className="block h-full"
            style={{ width: `${(s.count / lifetime.finished) * 100}%`, background: s.background }}
          />
        ))}
      </div>
      <ul className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1 text-[11.5px] text-muted">
        {segments.map((s) => (
          <li key={s.label} className="inline-flex items-center gap-1.5">
            <span className="inline-block h-[9px] w-[9px] rounded-[2px]" style={{ background: s.background }} />
            {s.label} <span className="tabular-nums text-fg">{s.count}</span>
          </li>
        ))}
      </ul>
      {causes.length > 0 && (
        <p className="mt-2 text-[11.5px] text-faint">
          Top causes{" "}
          {causes.map(([origin, n], i) => (
            <span key={origin}>
              {i > 0 ? " · " : ""}
              <span className="text-muted">{failOriginLabel(origin)}</span>{" "}
              <span className="tabular-nums text-muted">{n}</span>
            </span>
          ))}
        </p>
      )}
    </div>
  );
}

export function YourUsageCard({ usage }: { usage: SelfUsage }) {
  const life = breakdown(usage.lifetime);
  const last7 = breakdown(usage.last_7_days);
  const lifeDisclosure = aggregateDisclosure(usage.lifetime_subscription_run_count, usage.lifetime_unreported_run_count);
  const last7Disclosure = aggregateDisclosure(usage.last7_subscription_run_count, usage.last7_unreported_run_count);
  return (
    <Card>
      <SectionTitle>Your usage</SectionTitle>
      {usage.run_count === 0 ? (
        <p className="mt-2 text-sm text-faint">No usage recorded yet — it appears here once your runs spend tokens.</p>
      ) : (
        <>
          <BigNum tokens={life.total} />
          <Subrow usage={usage.lifetime} disclosure={lifeDisclosure} />
          <p className="mt-2.5 text-[11px] text-faint">
            Across <span className="tabular-nums text-muted">{usage.run_count}</span> run{usage.run_count === 1 ? "" : "s"}, all
            time · <span className="tabular-nums text-muted">{formatTokens(last7.total)}</span> tok /{" "}
            <span className="tabular-nums text-muted">{formatCost(last7.cost)}</span> last 7d
            {last7Disclosure.incomplete && <> ({last7Disclosure.text})</>} ·{" "}
            <Link to="/runs" className="text-info hover:underline whitespace-nowrap">
              see per-run detail{"\u00A0"}→
            </Link>
          </p>
        </>
      )}
      <FailedRunsBlock lifetime={usage.outcomes.lifetime} last7={usage.outcomes.last_7_days} />
    </Card>
  );
}

export function FactoryTotalCard({ admin }: { admin: AdminUsage }) {
  const f = breakdown(admin.factory.lifetime);
  const disclosure = aggregateDisclosure(
    admin.factory.lifetime_subscription_run_count,
    admin.factory.lifetime_unreported_run_count,
  );
  return (
    <Card>
      <SectionTitle>Factory total · all users · admin</SectionTitle>
      {admin.factory.run_count === 0 ? (
        <p className="mt-2 text-sm text-faint">No usage across the factory yet.</p>
      ) : (
        <>
          <BigNum tokens={f.total} />
          <Subrow usage={admin.factory.lifetime} disclosure={disclosure} />
          <p className="mt-2.5 text-[11px] text-faint">
            <span className="tabular-nums text-muted">{admin.factory.run_count}</span> runs by{" "}
            <span className="tabular-nums text-muted">{admin.users.length}</span> user{admin.users.length === 1 ? "" : "s"}
            {formatSince(admin.earliest_run) && <> since {formatSince(admin.earliest_run)}</>}
          </p>
        </>
      )}
      <FailedRunsBlock lifetime={admin.factory.outcomes.lifetime} last7={admin.factory.outcomes.last_7_days} />
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

export function PerUserUsageTable({ admin }: { admin: AdminUsage }) {
  const demo = useDemoMode();
  const factory = breakdown(admin.factory.lifetime);
  const rows = admin.users.map((u) => ({ ...u, b: breakdown(u.usage) }));
  const shares = tokenShares(rows.map((r) => r.b.total), factory.total);
  // The total row sums the per-user rows by construction, so its disclosure sums their
  // counts too — reusing admin.factory's own counts rather than re-summing the rows.
  const factoryDisclosure = aggregateDisclosure(
    admin.factory.lifetime_subscription_run_count,
    admin.factory.lifetime_unreported_run_count,
  );
  return (
    <Card>
      <SectionTitle>Per-user breakdown · admin</SectionTitle>
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
                <Th>Tokens</Th>
                <Th>Out</Th>
                <Th>Cost</Th>
                <Th>Share</Th>
              </tr>
            </thead>
            <tbody>
              {rows.map((u, i) => {
                // Share is by total tokens (not cost) — matches the mock's percentages.
                // Largest-remainder rounding (see tokenShares) so the column sums to 100%.
                const pct = shares[i];
                // PRD #1429 D7: this user's lifetime cost_usd is their metered subset —
                // disclose their own subscription/unreported counts beside it rather than
                // let the dollar figure read as their complete total.
                const rowDisclosure = aggregateDisclosure(u.subscription_run_count, u.unreported_run_count);
                return (
                  <tr key={u.user_id}>
                    <Td left>{maskEmail(u.email, demo)}</Td>
                    <Td>{u.run_count}</Td>
                    <Td>{u.outcomes.failed}</Td>
                    <Td title={`${u.outcomes.failed} of ${u.outcomes.finished} finished runs`}>
                      {failRate(u.outcomes)}
                    </Td>
                    <Td>{formatTokens(u.b.total)}</Td>
                    <Td>{formatTokens(u.b.out)}</Td>
                    <Td cost>
                      {formatCost(u.b.cost)}
                      {rowDisclosure.incomplete && (
                        <div className="whitespace-nowrap text-[9px] font-normal text-faint">{rowDisclosure.text}</div>
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
                <Td total>{admin.factory.run_count}</Td>
                <Td total>{admin.factory.outcomes.lifetime.failed}</Td>
                <Td
                  total
                  title={`${admin.factory.outcomes.lifetime.failed} of ${admin.factory.outcomes.lifetime.finished} finished runs`}
                >
                  {failRate(admin.factory.outcomes.lifetime)}
                </Td>
                <Td total>{formatTokens(factory.total)}</Td>
                <Td total>{formatTokens(factory.out)}</Td>
                <Td total cost>
                  {formatCost(factory.cost)}
                  {factoryDisclosure.incomplete && (
                    <div className="whitespace-nowrap text-[9px] font-normal text-faint">{factoryDisclosure.text}</div>
                  )}
                </Td>
                <Td total> </Td>
              </tr>
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}
