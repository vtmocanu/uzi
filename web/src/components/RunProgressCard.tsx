// RunProgressCard is the run page's progress card (PRD #2602 M2, web mock section 2): it sits
// under the header's budget facts and says where the run is in its approved plan.
//
//   head   → `≈70%` (the run page carries the ≈; the board cell does not), or the state flag
//            that replaces a misleading number: `◼ stalled · since`, `● waits on you · plan
//            gate since`, `⏸ <park word> · resumes`, `queued`, `planning`
//   what   → `milestone k of M · <active title>`, else `N of M milestones done`
//   track  → one segment for the plan (done once milestones are frozen) plus one per frozen
//            milestone: done (solid), now (striped: the signature mark, it reads as "being
//            laid down"), pending (empty)
//   steps  → review / validate / implement, the active role's phase marked `▸`; only the
//            current phase is known, so none is ever shown as passed. Drawn only in the
//            percent state with a known phase (PRD D6, as the TUI does): a waiting or parked
//            run has no active role, so a marked phase there would be stale
//   role   → the active role line, the same sanitised role + task the now-line shows; only
//            while a role is actually working (percent, stalled), never on an idle run
//   hint   → `⧗ may be blocked by <8>` linking to that run (GetRun only, awaiting_input)
//
// It renders only what the server derived (Run.progress, PRD D3) and nothing at all for an
// absent progress (a terminal run or a pre-feature server), a `none` run (no plan to measure
// against; the board cell's faint dash already says so) or an unknown state. Milestone
// titles/ids and the activity fields are UNTRUSTED model/repo text: every one is folded
// through stripUnsafeChars before it reaches text OR an attribute.
import type { CSSProperties, ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Run, RunActivity } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";
import { parkLabel } from "../lib/runBadge";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { stallTitle, waitReason } from "./RunProgressCell";
import { Card, cx } from "./ui";

type CardRun = Pick<
  Run,
  | "id"
  | "progress"
  | "status"
  | "health_since"
  | "milestones"
  | "milestones_completed"
  | "retry_not_before"
  | "recovery_retry_not_before"
> &
  Partial<Pick<Run, "health" | "health_reason" | "status_since">> &
  Parameters<typeof parkLabel>[0];

const PHASES = ["review", "validate", "implement"] as const;
// The states in which a role is actually at work: the phase and the active-role line belong
// to these only. waiting/parked/queued/planning runs are idle, whatever activity is cached.
const ROLE_STATES = new Set(["percent", "stalled"]);

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

function Segment({ state, label, title }: { state: SegState; label: string; title: string }) {
  const bar: CSSProperties | undefined =
    state === "now"
      ? {
          backgroundImage:
            "repeating-linear-gradient(90deg, rgb(var(--brand)) 0 3px, transparent 3px 6px)",
        }
      : undefined;
  return (
    <div className="grid min-w-0 gap-1.5" data-seg={state} title={title}>
      <div
        style={bar}
        className={cx(
          "h-2.5 rounded-[3px] border",
          state === "done" && "border-brand bg-brand",
          state === "now" && "border-brand",
          state === "pending" && "border-edge bg-raised",
        )}
      />
      <div
        className={cx(
          "truncate font-mono text-[11px]",
          state === "now" ? "text-fg" : "text-faint",
        )}
      >
        {label}
      </div>
    </div>
  );
}

export function RunProgressCard({
  run,
  activity = null,
}: {
  run: CardRun;
  activity?: RunActivity | null;
}) {
  // A slow clock: it only decides whether a park's resume instant is still ahead.
  const now = useNow(30_000);
  const p = run.progress;
  if (!p) return null;
  const known = ["percent", "waiting", "parked", "queued", "stalled", "planning"];
  if (!known.includes(p.state)) return null;

  let pct: number | null = null;
  if (p.state === "percent") {
    if (typeof p.pct !== "number" || !Number.isFinite(p.pct)) return null;
    pct = Math.max(0, Math.min(100, Math.round(p.pct)));
  }

  const milestones = (run.milestones ?? []).map((m) => ({
    id: m.id,
    title: stripUnsafeChars(m.title) || stripUnsafeChars(m.id),
  }));
  const total = milestones.length;
  const completed = new Set(run.milestones_completed ?? []);
  const activeIdx = p.active_milestone_id ? milestones.findIndex((m) => m.id === p.active_milestone_id) : -1;
  const activeTitle =
    activeIdx >= 0 ? milestones[activeIdx].title : stripUnsafeChars(p.active_milestone_id);
  const doneCount = p.milestone_done;
  const totalCount = total > 0 ? total : p.milestone_total;

  let what: string | null = null;
  if (p.state === "planning") {
    what = "no milestones frozen yet";
  } else if (activeIdx >= 0) {
    what = `milestone ${activeIdx + 1} of ${totalCount} · ${activeTitle}`;
  } else if (activeTitle) {
    what = `active milestone · ${activeTitle}`;
  } else if (totalCount > 0) {
    what = `${doneCount} of ${totalCount} milestones done`;
  }

  const segs = milestones.map((m) => ({
    ...m,
    state: (completed.has(m.id) ? "done" : m.id === p.active_milestone_id ? "now" : "pending") as SegState,
  }));
  const trackLabel =
    `Plan progress: planning done, ${segs.filter((s) => s.state === "done").length} of ${total} milestones done` +
    (activeIdx >= 0 ? `, milestone ${activeIdx + 1} (${activeTitle}) in progress` : "");

  const live = activity && ROLE_STATES.has(p.state) ? activity : null;
  const role = live ? stripUnsafeChars(live.agent) : "";
  const roleTask = live ? stripUnsafeChars(live.agent_label) || stripUnsafeChars(live.tool) : "";
  const showPhase = p.state === "percent" && (PHASES as readonly string[]).includes(p.phase);

  const blockedBy = p.maybe_blocked_by_run_id ? stripUnsafeChars(p.maybe_blocked_by_run_id) : "";

  return (
    <Card className="p-4">
      <section aria-label="Run progress" data-run-progress-card={p.state} className="grid grid-cols-[minmax(0,1fr)] gap-3.5">
        <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1.5">
          {pct !== null ? (
            <span
              data-progress-pct
              className="font-mono text-[28px] font-semibold leading-none tabular-nums text-fg"
              title={`About ${pct}% through the approved plan; an estimate from milestones done, not a time`}
            >
              <span aria-hidden="true">≈</span>
              <span className="sr-only">about </span>
              {pct}%
            </span>
          ) : (
            <StateFlag run={run} now={now} />
          )}
          {what && (
            <span
              data-progress-what
              className="min-w-0 [overflow-wrap:anywhere] font-mono text-sm font-medium text-muted"
            >
              {what}
            </span>
          )}
        </div>

        {blockedBy && (
          <div className="grid gap-1">
            <Link
              to={`/runs/${encodeURIComponent(blockedBy)}`}
              className="inline-flex w-fit items-center gap-1.5 rounded bg-info/10 px-2 py-0.5 font-mono text-xs font-medium text-info hover:underline"
              title="Open the run this one may be waiting on. A mention in the question is a hint, not a recorded dependency."
            >
              <span aria-hidden="true">⧗</span>
              <span>may be blocked by {blockedBy.slice(0, 8)}</span>
            </Link>
            <span className="text-xs text-faint">open question mentions an issue that run is working</span>
          </div>
        )}

        {total > 0 && (
          <div
            role="img"
            aria-label={trackLabel}
            className="grid gap-1"
            style={{ gridTemplateColumns: `minmax(0, 0.6fr) repeat(${total}, minmax(0, 1fr))` }}
          >
            <Segment state="done" label="plan" title="plan approved" />
            {segs.map((s) => (
              <Segment key={s.id} state={s.state} label={s.title} title={`${s.title} · ${s.state === "now" ? "in progress" : s.state}`} />
            ))}
          </div>
        )}

        {showPhase && (
          <div data-progress-phase className="flex flex-wrap items-center gap-1.5 font-mono text-xs">
            {PHASES.map((ph) =>
              ph === p.phase ? (
                <span
                  key={ph}
                  aria-current="step"
                  className="rounded bg-raised px-2 py-0.5 text-fg outline outline-1 outline-brand"
                >
                  <span aria-hidden="true">▸ </span>
                  {ph}
                </span>
              ) : (
                <span key={ph} className="rounded bg-raised px-2 py-0.5 text-faint">
                  {ph}
                </span>
              ),
            )}
            <span className="font-sans text-xs text-faint">current phase only, from the active role</span>
          </div>
        )}

        {role && (
          <dl className="m-0 grid gap-0.5 border-t border-edge pt-3">
            <dt className="text-xs text-faint">Active role</dt>
            <dd className="m-0 min-w-0 truncate font-mono text-[13px] text-fg">
              {role}
              {roleTask && <span className="text-muted"> · {roleTask}</span>}
            </dd>
          </dl>
        )}
      </section>
    </Card>
  );
}
