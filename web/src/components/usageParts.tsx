import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { failureRecency } from "../lib/failureRecency";
import { useNow } from "../lib/useNow";
import { stripUnsafeChars } from "../lib/safeText";
import type { RunUsage, RunOutcomes } from "../lib/api";
import { failOriginLabel } from "../lib/failOriginLabel";
import type { aggregateDisclosure } from "../lib/costStatus";
import { cx } from "./ui";
import type { UsageWindow } from "./UsageCards";

// Presentation pieces shared by the current Overview's UsageCard (UsageCards.tsx) and the
// Overview preview's OverviewUsageCard, so the two cannot drift before PRD #3877.

// A RunUsage bundle split the way the run view splits it: fresh = fresh input +
// cache creation, cached = cache reads, out = output; total = all three.
export function breakdown(u: RunUsage): { fresh: number; cached: number; out: number; total: number; cost: number } {
  const fresh = u.input_tokens + u.cache_creation_tokens;
  const cached = u.cache_read_tokens;
  const out = u.output_tokens;
  return { fresh, cached, out, total: fresh + cached + out, cost: u.cost_usd };
}

// PRD #1293 D6: the failed-run rate as failed / finished, one decimal, "—" when the scope
// has no finished runs (never a fabricated 0%). One helper for both cards' windows and the
// per-user table's Fail rate cell, so every surface rounds identically.
export const failRate = (o: RunOutcomes): string =>
  o.finished === 0 ? "—" : `${((o.failed / o.finished) * 100).toFixed(1)}%`;

// The change of a window's failed-run rate against all time, as "+1.2 pp vs all-time 10.0%".
// null when either side has no finished runs.
export function failRateDelta(outcomes: RunOutcomes, lifetime: RunOutcomes): string | null {
  if (lifetime.finished <= 0 || outcomes.finished <= 0) return null;
  const delta = (outcomes.failed / outcomes.finished - lifetime.failed / lifetime.finished) * 100;
  return `${delta < 0 ? "−" : delta > 0 ? "+" : ""}${Math.abs(delta).toFixed(1)} pp vs all-time ${failRate(lifetime)}`;
}

// formatSince renders the factory's earliest-run ISO timestamp as "12 May 2026" for
// the "since <date>" line; "" (falsy → the clause is dropped) when absent/unparseable.
export function formatSince(iso: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

// Group dollars only in the summary cards; other usage surfaces keep their formatter.
export function summaryCost(usd: number): string {
  const value = Number.isFinite(usd) && usd >= 0 ? usd : 0;
  return "$" + new Intl.NumberFormat("en-US", {
    minimumFractionDigits: value >= 1000 ? 0 : 2,
    maximumFractionDigits: value >= 1000 ? 0 : 2,
  }).format(value);
}

// Issue #3686: the "$X per PR" half of the PR summary for one window. The per-PR cost
// divides the column's displayed cost, so an incomplete cost reads "at least $X per PR"
// with the cost disclosure's own title. "— per PR" at zero PRs.
export function perPrText(count: number, cost: number, disclosure: ReturnType<typeof aggregateDisclosure>): ReactNode {
  if (count === 0) return "— per PR";
  const per = summaryCost(cost / count);
  if (disclosure.incomplete) return <span title={disclosure.title}>at least {per} per PR</span>;
  return `${per} per PR`;
}

// PRD #1293: outcomes use the selected window and failed / finished denominator.
// Issue #1418: remove the needs_landing hatch because an aggregate cannot know
// whether recoverable work was landed later. It is a descriptive note, not backlog.
export function FailedRunsBlock({ outcomes, personal, framed = true }: { outcomes: RunOutcomes; personal: boolean; framed?: boolean }) {
  if (outcomes.finished === 0) return <p className="text-sm text-muted">No finished runs in this period.</p>;
  const segments = [
    { label: "completed", count: outcomes.completed, background: "rgb(var(--ok))" },
    { label: "cancelled", count: outcomes.cancelled, background: "rgb(var(--edge-strong))" },
    { label: "rejected", count: outcomes.plan_rejected + (outcomes.base_diverged ?? 0), background: "repeating-linear-gradient(135deg, rgb(var(--edge-strong)) 0 2px, rgb(var(--surface)) 2px 4px)" },
    { label: "failed", count: outcomes.failed, background: "rgb(var(--danger))" },
  ].filter((s) => s.count > 0);
  return (
    <div className={cx(framed && "border-t border-edge pt-3", "space-y-2.5")}>
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

// "inline" is the current Overview's one-line summary; "list" is the preview's top-3 rows
// with a bar per cause and an "other causes" total.
export function TopCauses({ outcomes, personal, variant = "inline" }: { outcomes: RunOutcomes; personal: boolean; variant?: "inline" | "list" }) {
  const causes = Object.entries(outcomes.fail_origins)
    .filter(([, count]) => count > 0)
    .sort(([a, ac], [b, bc]) => bc - ac || a.localeCompare(b));
  const hiddenCauses = Math.max(0, causes.length - 3);
  if (variant === "list") {
    if (outcomes.failed === 0 || causes.length === 0) {
      return (
        <div>
          <h4 className="text-[13px] font-medium text-fg">Top causes</h4>
          <p className="mt-2 text-xs text-muted">No failures in this period.</p>
        </div>
      );
    }
    const top = causes.slice(0, 3);
    const max = top[0][1];
    const otherTotal = causes.slice(3).reduce((sum, [, count]) => sum + count, 0);
    const row = "grid grid-cols-[minmax(0,1fr)_84px_30px] items-center gap-x-2.5";
    return (
      <div>
        <h4 className="text-[13px] font-medium text-fg">Top causes</h4>
        <ul className="mt-2.5 space-y-1.5 text-[13px]">
          {top.map(([origin, count]) => {
            const label = stripUnsafeChars(failOriginLabel(origin));
            return (
              <li key={origin} className={row}>
                <span className="min-w-0 truncate text-fg" title={label}>{label}</span>
                <span aria-hidden="true" className="h-[5px] overflow-hidden rounded-[3px] bg-raised">
                  <span className="block h-full bg-danger/80" style={{ width: `${(count / max) * 100}%` }} />
                </span>
                <span className="text-right font-mono text-xs tabular-nums text-muted">{count.toLocaleString("en-US")}</span>
              </li>
            );
          })}
          {hiddenCauses > 0 && (
            <li className={row}>
              <span className="min-w-0 truncate text-muted">
                {personal ? <Link className="text-brand hover:underline" to="/runs/history?status=failed">other causes</Link> : "other causes"}
              </span>
              <span aria-hidden="true" />
              <span className="text-right font-mono text-xs tabular-nums text-muted">{otherTotal.toLocaleString("en-US")}</span>
            </li>
          )}
        </ul>
      </div>
    );
  }
  if (outcomes.failed === 0 || causes.length === 0) return null;
  return (
    <p className="text-xs text-muted">
      Top causes: {causes.slice(0, 3).map(([origin, count]) => `${stripUnsafeChars(failOriginLabel(origin))} ${count.toLocaleString("en-US")}`).join(" · ")}
      {hiddenCauses > 0 && <>{" "}{personal ? <Link className="whitespace-nowrap text-brand hover:underline" to="/runs/history?status=failed">+{hiddenCauses} more</Link> : <span className="whitespace-nowrap">+{hiddenCauses} more</span>}</>}
    </p>
  );
}

export function SinceLastFailedRun({ outcomes, owner, label = "Latest failure" }: { outcomes: RunOutcomes; owner?: string; label?: string }) {
  const now = useNow(30_000);
  const value = failureRecency(outcomes, now);
  const hasFailure = outcomes.last_failed_at != null && value !== "Unavailable";
  const originLabel = stripUnsafeChars(failOriginLabel(outcomes.last_failed_origin ?? "unknown"));
  const ownerLabel = owner ? stripUnsafeChars(owner) : undefined;
  return (
    <div className="min-w-0 border-t border-edge pt-3 text-xs text-muted">
      {hasFailure ? (
        <p className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-1">
          <span className="shrink-0 text-fg">{label} <span className="font-mono tabular-nums">{value}</span> ago</span>
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

export function SegmentedToggle<T extends string>({ label, options, value, onChange, className = "ml-auto" }: {
  label: string;
  options: readonly { value: T; label: string }[];
  value: T;
  onChange: (value: T) => void;
  className?: string;
}) {
  return (
    <div role="group" aria-label={label} className={cx(className, "inline-flex shrink-0 rounded-full border border-edge bg-raised p-0.5")}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          aria-pressed={value === option.value}
          onClick={() => onChange(option.value)}
          className={cx(
            "min-h-11 rounded-full border px-2.5 text-xs focus-visible:outline-solid focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring sm:min-h-8",
            value === option.value ? "border-edge-strong bg-surface font-semibold text-fg shadow-sm" : "border-transparent font-normal text-muted hover:text-fg",
          )}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

const WINDOW_OPTIONS = [
  { value: "lifetime", label: "All time" },
  { value: "last_7_days", label: "Last 7 days" },
] as const;

export function WindowToggle({ value, onChange }: { value: UsageWindow; onChange: (window: UsageWindow) => void }) {
  return <SegmentedToggle label="Usage reporting period" options={WINDOW_OPTIONS} value={value} onChange={onChange} />;
}
