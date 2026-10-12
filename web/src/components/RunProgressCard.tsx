import type { CSSProperties, ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Run, RunActivity } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";
import { formatAgo } from "../lib/rateLimits";
import { parkLabel } from "../lib/runBadge";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { stallTitle, waitReason } from "./RunProgressCell";
import { MilestoneRows } from "./RunMilestoneRows";
import { milestoneBadge } from "../lib/runBadge";
import { cacheDisplayPct, hasSideUsage, sideUsage, usagePanelVisible, type RunUsage } from "../lib/runUsage";
import { costDisplay, costHeadline, costSubLabel } from "../lib/costStatus";
import { formatTokens } from "../lib/formatTokens";
import { tailCostText, tailVisible } from "../lib/usageTail";
import { Card, cx } from "./ui";

type CardRun = Pick<Run, "id" | "status" | "health_since" | "milestones" | "milestones_completed" | "retry_not_before" | "recovery_retry_not_before" | "progress"> & Partial<Run>;
type Tone = "danger" | "warn" | "info" | "dim";
const FLAG_TONES: Record<Tone, string> = {
  danger: "bg-danger/10 text-danger",
  warn: "bg-warn/10 text-warn",
  info: "bg-info/10 text-info",
  dim: "bg-raised text-muted",
};

function Flag({ tone, title, children }: { tone: Tone; title?: string; children: ReactNode }) {
  return (
    <span
      title={title}
      className={cx(
        "inline-flex w-fit items-center gap-1.5 rounded px-2 py-0.5 font-mono text-xs font-medium",
        FLAG_TONES[tone],
      )}
    >
      {children}
    </span>
  );
}

// The resume instant a park carries, when it names one: a usage-limit park its
// retry_not_before, a transient-recovery park its recovery_retry_not_before. Other parks
// (owner pause, pool, credential) have no clock, and a stale stamp from an earlier park must
// not read as a promise, so only the matching status is consulted and only a future instant.
function resumeAt(run: CardRun, now: number): string | null {
  const iso =
    run.status === "limit_wait"
      ? run.retry_not_before
      : run.status === "recovery_wait"
        ? run.recovery_retry_not_before
        : null;
  if (!iso) return null;
  const t = Date.parse(iso);
  return Number.isFinite(t) && t > now ? formatLocalTime(iso) : null;
}

function StateFlag({ run, now }: { run: CardRun; now: number }) {
  const p = run.progress;
  if (!p) return null;
  switch (p.state) {
    case "stalled": {
      const since = formatLocalTime(run.health_since);
      return (
        <Flag tone="danger" title={stallTitle(run)}>
          <span aria-hidden="true">◼</span>
          <span>
            stalled{run.health === "looping" && <span className="sr-only"> (looping)</span>}
            {since && ` · since ${since}`}
          </span>
        </Flag>
      );
    }
    case "waiting": {
      const since = formatLocalTime(run.status_since);
      const sub = [waitReason(run.status), since && `since ${since}`].filter(Boolean).join(" ");
      return (
        <Flag tone="warn" title="This run is waiting on you: a plan approval, a question or a follow-up.">
          <span aria-hidden="true">●</span>
          <span>
            waits on you{sub && ` · ${sub}`}
          </span>
        </Flag>
      );
    }
    case "parked": {
      const resumes = resumeAt(run, now);
      return (
        <Flag tone="dim">
          <span aria-hidden="true">⏸</span>
          <span>
            {parkLabel(run)}
            {resumes && ` · resumes ${resumes}`}
          </span>
        </Flag>
      );
    }
    case "queued":
      return <Flag tone="dim">queued</Flag>;
    case "planning":
      return <Flag tone="dim">planning</Flag>;
    default:
      return null;
  }
}

type SegState = "done" | "now" | "pending";

function Segment({ state, title, plan = false }: { state: SegState; title: string; plan?: boolean }) {
  const bar: CSSProperties | undefined =
    state === "now"
      ? {
          backgroundImage:
            "repeating-linear-gradient(135deg, rgb(var(--brand)) 0 3px, transparent 3px 6px)",
        }
      : undefined;
  return (
    <div className="grid min-w-0 gap-1.5" data-seg={state} title={title}>
      <div
        style={bar}
        className={cx(
          "h-2.5 rounded-[3px] border",
          state === "done" && (plan ? "border-plan bg-plan" : "border-brand bg-brand"),
          state === "now" && "border-brand",
          state === "pending" && "border-edge bg-raised",
        )}
      />

    </div>
  );
}

// Shared presence rule so the header retains the stage only when no card shows it.
export function runProgressCardVisible(run: CardRun, activity: RunActivity | null, usage?: RunUsage): boolean {
  return Boolean(run.milestones?.length || (run.progress && ["percent", "waiting", "parked", "queued", "stalled", "planning"].includes(run.progress.state)) || activity || (usage && (usage.hasConfirmed || usage.hasLiveTokens || hasSideUsage(usage))) || tailVisible(run.usage_estimated_tail));
}

export function RunProgressCard({ run, activity = null, usage, stage = null }: {
  run: CardRun; activity?: RunActivity | null; usage?: RunUsage; stage?: string | null;
}) {
  const now = useNow(30_000);
  const p = run.progress;
  const milestones = run.milestones ?? [];
  const badge = milestoneBadge(run);
  const percent = p?.state === "percent" && typeof p.pct === "number" && Number.isFinite(p.pct)
    ? Math.max(0, Math.min(100, Math.round(p.pct))) : null;
  const active = p?.active_milestone_id;
  const hasActiveRow = milestones.some(m => m.id === active);
  const terminal = ["completed", "failed", "cancelled"].includes(run.status);
  const live = terminal || run.status === "paused" ? null : activity;
  const note = !terminal && p?.state === "percent" ? stripUnsafeChars(p.now_note?.text ?? "").trim() : "";
  const completed = new Set(run.milestones_completed ?? []);
  const knownState = p && ["percent", "waiting", "parked", "queued", "stalled", "planning"].includes(p.state);
  const hasUsage = usage && usagePanelVisible(usage);
  if (!runProgressCardVisible(run, live, usage)) return null;
  const nowLine = note ? <p data-progress-now className="my-2 rounded-md bg-raised px-2 py-1 text-sm [overflow-wrap:anywhere]">
    <b>Now: </b>{note}{" "}<span className="ml-2 text-xs text-faint">· model summary{p?.now_note?.at && ` · ${formatAgo(p.now_note.at, now)}`}</span>
  </p> : null;
  const safeStage = run.status === "running" ? stripUnsafeChars(stage ?? "") : "";
  const phase = !terminal && p?.state === "percent" && ["review", "validate", "implement"].includes(p.phase) ? p.phase : "";
  const iteration = !terminal && (run.iteration_count ?? 0) > 0 ? `iteration ${run.iteration_count}` : "";
  const contextLine = [phase && `phase: ${stripUnsafeChars(phase)}`, iteration, safeStage && `run: ${safeStage}`].filter(Boolean).join(" · ");
  const phaseLine = contextLine ? <div data-progress-context data-progress-phase={phase ? "" : undefined} className="mt-2 inline-flex rounded bg-raised px-2 py-1 text-xs text-muted" aria-current={phase ? "step" : undefined}>{contextLine}</div> : null;
  const trackLabel = badge ? `Plan approved; ${badge.reported ? `${badge.done} of ${badge.total} milestones reported complete` : `– of ${badge.total} reported; no milestone completion reported`}${active ? `; active milestone: ${stripUnsafeChars(milestones.find(m => m.id === active)?.title ?? active)}` : ""}` : "";
  const hasRecorded = usage && (usage.hasConfirmed || hasSideUsage(usage));
  const cost = usage ? costDisplay(usage.hasConfirmed ? run.usage?.cost_status ?? "" : sideUsage(usage).status, usage.total.costUsd) : null;
  return <Card className="p-4">
    <section aria-label="Run progress" data-run-progress-card={stripUnsafeChars(p?.state ?? "history")} className="grid min-w-0 grid-cols-[minmax(0,1fr)] gap-4">
      <div data-progress-head className="flex flex-wrap items-baseline gap-3">
        {percent !== null ? <span data-progress-pct className="font-mono text-[28px] font-semibold tabular-nums" title="Estimate through the approved plan, not a time estimate"><span aria-hidden>≈</span><span className="sr-only">about </span>{percent}%</span>
          : knownState ? <StateFlag run={run} now={now} /> : null}
        {percent !== null && badge && <span aria-hidden="true">·</span>}
        {p?.state === "planning" && !badge && <span className="text-sm text-muted">no milestones frozen yet</span>}
        {badge && <span data-progress-what title={badge.reported ? "Milestones reported complete of the approved plan" : "No milestone completion reported for this run"} className="min-w-0 text-sm text-muted [overflow-wrap:anywhere]">{badge.reported ? badge.done : "–"} of {badge.total} {badge.reported ? "reported complete" : "reported"}</span>}
        {!hasActiveRow && phaseLine}
      </div>
      {!hasActiveRow && nowLine}
      {p?.maybe_blocked_by_run_id && <Link className="text-sm text-info" title="A question mentions this run; this is a hint, not a recorded dependency." to={`/runs/${encodeURIComponent(stripUnsafeChars(p.maybe_blocked_by_run_id))}`}><span aria-hidden="true">⧗</span>may be blocked by {stripUnsafeChars(p.maybe_blocked_by_run_id).slice(0,8)}</Link>}
      {milestones.length > 0 && <div role="img" aria-label={trackLabel} className="grid gap-1" style={{gridTemplateColumns: `minmax(0, 0.6fr) repeat(${milestones.length}, minmax(0, 1fr))`}}>
        <Segment state="done" title="Plan approved" plan />
        {milestones.map(m => <Segment key={m.id} title={`${stripUnsafeChars(m.title)} · ${completed.has(m.id) ? "reported complete" : m.id === active ? "in progress" : "not started"}`} state={completed.has(m.id) ? "done" : m.id === active ? "now" : "pending"} />)}
      </div>}
      {milestones.length > 0 ? <MilestoneRows run={run} activity={live} activeContent={<>{phaseLine}{nowLine}</>} />
        : live && (run.status === "running" || ["limit_wait", "pool_wait", "recovery_wait"].includes(run.status)) && !run.is_planning && p?.state !== "planning" && <p data-progress-activity className="text-sm text-muted">{stripUnsafeChars(live.agent)} · {stripUnsafeChars(live.agent_label || live.tool)}</p>}
      {(hasUsage || tailVisible(run.usage_estimated_tail)) && <div data-progress-usage className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-edge pt-3 text-xs text-muted">
        {hasRecorded && usage && cost && <>{!usage.hasConfirmed && <span>Summary usage</span>}<span className="font-semibold text-fg">{costHeadline(cost)}</span><span>{costSubLabel(cost, run.harness)}</span><span>{formatTokens(usage.total.fresh + usage.total.cached)} in ({cacheDisplayPct(usage.total)}% cached)</span><span>{formatTokens(usage.total.out)} out</span></>}
        {usage?.hasLiveTokens && !usage.hasConfirmed && <span>In-flight tokens · live, not billed yet: {formatTokens(usage.liveTotal.fresh + usage.liveTotal.cached)} in ({cacheDisplayPct(usage.liveTotal)}% cached) · provisional input</span>}
        {tailVisible(run.usage_estimated_tail) && <span>{run.usage_estimated_tail.cost_status === "unpriced" ? "+ unpriced tail" : `+ ${tailCostText(run.usage_estimated_tail.cost_usd, run.usage_estimated_tail.cost_status)} unrecorded`}</span>}
        {hasUsage && <a href="#run-full-usage" className="ml-auto text-brand hover:underline">Full usage</a>}
      </div>}
    </section>
  </Card>;
}
