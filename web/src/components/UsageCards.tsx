import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { SelfUsage, AdminUsage, RunUsage } from "../lib/api";
import { formatTokens, formatCost } from "../lib/formatTokens";
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
            <span className="tabular-nums text-muted">{formatCost(last7.cost)}</span> in the last 7 days
            {last7Disclosure.incomplete && <> ({last7Disclosure.text})</>} ·{" "}
            <Link to="/runs" className="text-info hover:underline whitespace-nowrap">
              see per-run detail{"\u00A0"}→
            </Link>
          </p>
        </>
      )}
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

function Td({ children, left, total, cost }: { children?: ReactNode; left?: boolean; total?: boolean; cost?: boolean }) {
  const cls = ["px-2.5 py-1.5"];
  cls.push(left ? "text-left font-sans" : "text-right font-mono tabular-nums");
  if (total) cls.push("font-semibold text-fg");
  else cls.push(cost ? "text-brand" : left ? "text-fg" : "text-muted", "border-b border-edge/50");
  return <td className={cls.join(" ")}>{children}</td>;
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
          <table className="w-full min-w-[560px] border-collapse text-xs">
            <thead>
              <tr>
                <Th left>User</Th>
                <Th>Runs</Th>
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
