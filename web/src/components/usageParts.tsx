import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { failureRecency } from "../lib/failureRecency";
import { hasRecordedLifetimeUsage } from "../lib/usageRecorded";
import { useNow } from "../lib/useNow";
import { stripUnsafeChars } from "../lib/safeText";
import type { RunOutcomes, SelfUsage } from "../lib/api";
import { failOriginLabel } from "../lib/failOriginLabel";
import type { aggregateDisclosure } from "../lib/costStatus";
import { cx } from "./ui";
import { InfoTip } from "./InfoTip";
import type { UsageWindow } from "../lib/usageByUser";

// Presentation pieces for the Overview's OverviewUsageCard.

// Issues #4209 and #4577: the Runs count covers primary work; tokens and cost also include
// helper runs. Shown in the info tip beside the Overview Usage card's title.
export const USAGE_SCOPE_NOTE =
  "Runs count primary work: issues, tasks, prompts, jobs and self-improve. Cost and tokens also include helper runs (judge, cross-check, PR rework, CI fix).";

// Issue #4577: outcomes count every non-chat run kind, unlike the primary-only Runs count.
// Run history does not list judge or cross_check runs, so their failures are explained here.
// Shown in the info tip beside the failed-of-finished line.
export const OUTCOME_SCOPE_NOTE =
  "The Runs count covers primary work with recorded usage. Outcomes also include reworks, CI fixes, cross-checks and judge runs. Judge and cross-check failures show on their parent run.";

// Issue #4209: run_count counts only primary work runs, so a helper-only user has
// run_count 0 yet real spend. "Nothing recorded" is true only when tokens, cost, the
// subscription/unreported counts and the run count are all zero.
export function hasRecordedUsage(usage: SelfUsage): boolean {
  return hasRecordedLifetimeUsage(usage.lifetime, usage.run_count, usage.lifetime_subscription_run_count, usage.lifetime_unreported_run_count);
}

// failureRecency for a cell that reads "<value> ago": only a duration gets " ago"; the
// "Unavailable", "–" and "no failures" words stand alone.
export function failureAgo(outcomes: RunOutcomes, now: number): string {
  const value = failureRecency(outcomes, now);
  return outcomes.last_failed_at != null && value !== "Unavailable" ? `${value} ago` : value;
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
export function FailedRunsBlock({ outcomes, personal }: { outcomes: RunOutcomes; personal: boolean }) {
  if (outcomes.finished === 0) return <p className="text-sm text-muted">No finished runs in this period.</p>;
  const segments = [
    { label: "completed", count: outcomes.completed, background: "rgb(var(--ok))" },
    { label: "cancelled", count: outcomes.cancelled, background: "rgb(var(--edge-strong))" },
    { label: "rejected", count: outcomes.plan_rejected + (outcomes.base_diverged ?? 0), background: "repeating-linear-gradient(135deg, rgb(var(--edge-strong)) 0 2px, rgb(var(--surface)) 2px 4px)" },
    { label: "failed", count: outcomes.failed, background: "rgb(var(--danger))" },
  ].filter((s) => s.count > 0);
  return (
    <div className="space-y-2.5">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 text-xs">
        <p className="flex items-center gap-1.5 text-muted">
          <span><span className="tabular-nums text-fg">{outcomes.failed.toLocaleString("en-US")}</span> failed of <span className="tabular-nums text-fg">{outcomes.finished.toLocaleString("en-US")}</span> finished runs</span>
          <InfoTip label="About run outcomes" tip={OUTCOME_SCOPE_NOTE} />
        </p>
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

// The top-3 failure causes as rows with a bar per cause and an "other causes" total.
export function TopCauses({ outcomes, personal }: { outcomes: RunOutcomes; personal: boolean }) {
  const causes = Object.entries(outcomes.fail_origins)
    .filter(([, count]) => count > 0)
    .sort(([a, ac], [b, bc]) => bc - ac || a.localeCompare(b));
  const hiddenCauses = Math.max(0, causes.length - 3);
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

export function SinceLastFailedRun({ outcomes, owner, label = "Latest failure", wrap = false }: { outcomes: RunOutcomes; owner?: string; label?: string; wrap?: boolean }) {
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
          <span className={cx("flex min-w-0 flex-1 basis-[120px] items-baseline gap-x-3", wrap && "flex-wrap gap-y-1")}>
            <span className={wrap ? "min-w-0 break-words" : "min-w-0 truncate"} title={originLabel}>{originLabel}</span>
            {/* Owner shrinks to an ellipsis before the cause loses its space (wrap: breaks onto its own line instead). */}
            {ownerLabel && <span className={wrap ? "min-w-0 break-all" : "min-w-0 max-w-max grow shrink-0 basis-[1em] truncate"} title={ownerLabel}>{ownerLabel}</span>}
          </span>
          <span className="shrink-0">{outcomes.completed_since_last_failure ?? 0} completed since</span>
          {outcomes.last_failed_run_id && (
            <Link className="shrink-0 whitespace-nowrap text-brand hover:underline" to={`/runs/${outcomes.last_failed_run_id}`}>run {outcomes.last_failed_run_id.slice(0, 8)} →</Link>
          )}
        </p>
      ) : value === "Unavailable" ? (
        <p>{label}: unavailable</p>
      ) : (
        <p className={outcomes.finished > 0 ? "text-ok" : "text-muted"}>
          {outcomes.finished === 0 ? "No finished runs yet" : `No recorded failures · ${outcomes.completed} completed runs, none failed`}
        </p>
      )}
    </div>
  );
}

export function SegmentedToggle<T extends string>({ label, options, value, onChange, className = "ml-auto", steadyWidth = false }: {
  label: string;
  options: readonly { value: T; label: string; disabled?: boolean }[];
  value: T;
  onChange: (value: T) => void;
  className?: string;
  // One weight for both states (#4052): a bolder selected label changes its advance, so the
  // switch changes width and a right-aligned one shifts on every pick.
  steadyWidth?: boolean;
}) {
  return (
    <div role="group" aria-label={label} className={cx(className, "inline-flex shrink-0 rounded-full border border-edge bg-raised p-0.5")}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          aria-pressed={value === option.value}
          disabled={option.disabled}
          onClick={() => onChange(option.value)}
          className={cx(
            "min-h-11 rounded-full border px-2.5 text-xs focus-visible:outline-solid focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:text-muted sm:min-h-8",
            value === option.value ? "border-edge-strong bg-surface text-fg shadow-sm" : "border-transparent text-muted hover:text-fg",
            steadyWidth ? "font-medium" : value === option.value ? "font-semibold" : "font-normal",
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

export function WindowToggle({ value, onChange, className, steadyWidth }: { value: UsageWindow; onChange: (window: UsageWindow) => void; className?: string; steadyWidth?: boolean }) {
  return <SegmentedToggle label="Usage reporting period" options={WINDOW_OPTIONS} value={value} onChange={onChange} className={className} steadyWidth={steadyWidth} />;
}

// Table cells for the Overview's By user table (PRD #3881), whose first column names its row.
// `start` left-aligns a column header or cell without making it a row header, for the
// table's combined cost-share / Claude-Codex bar column.
export function Th({ children, left, start, title }: { children: ReactNode; left?: boolean; start?: boolean; title?: string }) {
  return (
    <th
      scope="col"
      title={title}
      className={cx("whitespace-nowrap border-b border-edge px-2.5 py-2 text-[11px] font-medium uppercase tracking-[0.04em] text-faint", left || start ? "text-left" : "text-right")}
    >
      {children}
    </th>
  );
}

export function Td({
  children,
  left,
  start,
  total,
  title,
}: {
  children?: ReactNode;
  left?: boolean;
  start?: boolean;
  total?: boolean;
  title?: string;
}) {
  const cls = cx(
    "whitespace-nowrap px-2.5 py-2 align-top tabular-nums",
    left || start ? "text-left" : "text-right",
    total ? "border-t-2 border-edge-strong font-semibold text-fg" : "border-b border-edge/50 text-fg",
  );
  if (left) return <th scope="row" title={title} className={cx(cls, !total && "font-normal")}>{children}</th>;
  return <td title={title} className={cls}>{children}</td>;
}
