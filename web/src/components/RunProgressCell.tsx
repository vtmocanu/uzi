// RunProgressCell is the compact Progress cell of a run row (PRD #2602 M1): the Dashboard
// "Recent runs" list and the Runs list both render it right after the milestone badge.
// It only renders what the server derived (RunDTO.progress) — never re-derives a percent
// or a flag client-side (PRD D3) — and every string it draws is enum-derived copy or a
// number, so no untrusted text reaches it.
//
// States (PRD rules 1-8; the board shows `70%` without the ≈ the run page carries):
//   percent  → `70%` over a thin brand-tone bar (a progressbar, aria value 0-100)
//   waiting  → `● waits on you`, warn, with a `plan gate|question|follow-up since HH:MM`
//              sub-line (the reason from the status, the time from status_since)
//   stalled  → `◼ stalled`, danger, with `since HH:MM` from health_since; its tooltip and
//              accessible name carry the health word (stalled vs looping), because the row
//              drops its RunHealthBadge while this flag shows (see progressShowsStall)
//   parked / queued / planning → screen-reader-only `Progress: <word>`: the status pill
//              beside the cell already names the state, so a visible flag would repeat it
//              (the TUI and CLI, which have no such pill, keep the visible flag)
//   none     → faint `—`, titled, with a screen-reader `Progress: no estimate`
// An absent/null progress (a pre-feature server, or a terminal run) or an unknown state
// (a newer server's enum member) renders nothing, so the row reads exactly as before.
import type { ReactNode } from "react";
import type { Run, RunProgress } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";
import { parkLabel } from "../lib/runBadge";
import { stripUnsafeChars } from "../lib/safeText";
import { Badge } from "./ui";

// The fields the cell reads: the progress DTO, the two "since" instants, the health flag
// (the stalled flag's wording), and whatever parkLabel folds through effectiveRunStatus
// to name a park.
type ProgressRun = Pick<Run, "progress" | "status_since" | "health_since"> &
  Partial<Pick<Run, "health" | "health_reason">> &
  Parameters<typeof parkLabel>[0];

// progressShowsStall reports whether the cell shows the stalled flag. A row whose cell is
// visible suppresses its RunHealthBadge then, so one stall is not drawn twice (`◼ stalled`
// beside `⚠ stalled · Nm`); the cell's tooltip and accessible name keep the health word.
export function progressShowsStall(run: { progress?: RunProgress | null }): boolean {
  return run.progress?.state === "stalled";
}

// The waiting reason, named from the raw status the server derived `waiting` from.
const WAIT_REASONS: Record<string, string> = {
  awaiting_approval: "plan gate",
  awaiting_input: "question",
  awaiting_followup: "follow-up",
};

// waitReason names what a `waiting` run waits on (plan gate / question / follow-up), or
// undefined for a status outside the three. Shared with the run page's RunProgressCard.
export function waitReason(status: string): string | undefined {
  return WAIT_REASONS[status];
}

// stallTitle is the stalled flag's tooltip: the health word (stalled vs looping) plus the
// owner-only health reason the replaced health pill carried, sanitised because it is
// model-influenced text. Shared with the run page's RunProgressCard.
export function stallTitle(run: Partial<Pick<Run, "health" | "health_reason">>): string {
  const reason = run.health_reason ? stripUnsafeChars(run.health_reason) : "";
  const base =
    run.health === "looping"
      ? "Looping: the run keeps repeating the same steps; the percentage returns once it moves on."
      : "Stalled: no progress is being made; the percentage returns once activity resumes.";
  return reason === "" ? base : `${base} ${reason}`;
}

// Stacked flag + faint sub-line (`<reason> since HH:MM`, or whichever half is known); the
// sub-line drops when neither is.
function Flagged({
  state,
  reason,
  since,
  children,
}: {
  state: string;
  reason?: string;
  since: string | null;
  children: ReactNode;
}) {
  const sub = [reason, since && `since ${since}`].filter(Boolean).join(" ");
  return (
    <span data-run-progress={state} className="inline-flex flex-col items-start gap-0.5">
      {children}
      {sub && <span className="font-mono text-[10px] leading-none tabular-nums text-faint">{sub}</span>}
    </span>
  );
}

// A state the adjacent status pill already names: kept for assistive tech, not drawn.
function Quiet({ state, word }: { state: string; word: string }) {
  return (
    <span data-run-progress={state} className="sr-only">
      Progress: {word}
    </span>
  );
}

export function RunProgressCell({ run }: { run: ProgressRun }) {
  const p = run.progress;
  if (!p) return null;
  switch (p.state) {
    case "percent": {
      if (typeof p.pct !== "number" || !Number.isFinite(p.pct)) return null;
      const pct = Math.max(0, Math.min(100, Math.round(p.pct)));
      const of =
        p.milestone_total > 0 ? `, ${p.milestone_done} of ${p.milestone_total} milestones done` : "";
      return (
        <span
          data-run-progress="percent"
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={pct}
          aria-label={`Run progress: about ${pct}%${of}`}
          title={`About ${pct}% through the approved plan${of}`}
          className="inline-flex w-16 flex-col gap-1"
        >
          <span
            aria-hidden="true"
            className="font-mono text-[13px] font-semibold leading-none tabular-nums text-fg"
          >
            {pct}%
          </span>
          <span aria-hidden="true" className="relative h-1 w-full overflow-hidden rounded-full bg-edge">
            <span className="absolute inset-y-0 left-0 rounded-full bg-brand" style={{ width: `${pct}%` }} />
          </span>
        </span>
      );
    }
    case "waiting":
      return (
        <Flagged state="waiting" reason={waitReason(run.status)} since={formatLocalTime(run.status_since)}>
          <Badge tone="warning" dot title="This run is waiting on you: a plan approval, a question or a follow-up.">
            waits on you
          </Badge>
        </Flagged>
      );
    case "stalled": {
      const looping = run.health === "looping";
      return (
        <Flagged state="stalled" since={formatLocalTime(run.health_since)}>
          <Badge
            tone="danger"
            title={stallTitle(run)}
          >
            <span aria-hidden="true">◼</span> stalled
            {looping && <span className="sr-only"> (looping)</span>}
          </Badge>
        </Flagged>
      );
    }
    case "parked":
      return <Quiet state="parked" word={parkLabel(run)} />;
    case "queued":
      return <Quiet state="queued" word="queued" />;
    case "planning":
      return <Quiet state="planning" word="planning" />;
    case "none":
      return (
        <span
          data-run-progress="none"
          title="No milestone plan to measure progress against"
          className="font-mono text-[11px] text-faint"
        >
          <span aria-hidden="true">—</span>
          <span className="sr-only">Progress: no estimate</span>
        </span>
      );
    default:
      return null;
  }
}
