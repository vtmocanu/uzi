// RunProgressCell is the compact Progress cell of a run row (PRD #2602 M1): the Dashboard
// "Recent runs" list and the Runs list both render it right after the milestone badge.
// It only renders what the server derived (RunDTO.progress) — never re-derives a percent
// or a flag client-side (PRD D3) — and every string it draws is enum-derived copy or a
// number, so no untrusted text reaches it.
//
// States (PRD rules 1-8; the board shows `70%` without the ≈ the run page carries):
//   percent  → `70%` over a thin brand-tone bar (a progressbar, aria value 0-100)
//   waiting  → `● waits on you`, warn, with `since HH:MM` from status_since when known
//   stalled  → `◼ stalled`, danger, with `since HH:MM` from health_since when known
//   parked   → `⏸ <today's park word>` (runBadge's PARK_LABELS via parkLabel), faint
//   queued   → faint `queued`
//   planning → plan-tone `planning` (plain text, see the parked note below)
//   none     → faint `—`
// An absent/null progress (a pre-feature server, or a terminal run) or an unknown state
// (a newer server's enum member) renders nothing, so the row reads exactly as before.
import type { ReactNode } from "react";
import type { Run } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";
import { parkLabel } from "../lib/runBadge";
import { Badge } from "./ui";

// The fields the cell reads: the progress DTO, the two "since" instants, and whatever
// parkLabel folds through effectiveRunStatus to name a park.
type ProgressRun = Pick<Run, "progress" | "status_since" | "health_since"> &
  Parameters<typeof parkLabel>[0];

// The ⏸ glyph is drawn rather than typed: U+23F8 is missing from many UI fonts (a tofu box
// in headless Chromium) and becomes a colour emoji on macOS, so two bars stand in for it.
function PauseGlyph() {
  return (
    <span aria-hidden="true" data-glyph="pause" className="inline-flex h-2 items-stretch gap-[2px]">
      <span className="w-[2px] rounded-[1px] bg-current" />
      <span className="w-[2px] rounded-[1px] bg-current" />
    </span>
  );
}

// Stacked flag + faint `since HH:MM` sub-line; the sub drops when the instant is absent.
function Flagged({ state, since, children }: { state: string; since: string | null; children: ReactNode }) {
  return (
    <span data-run-progress={state} className="inline-flex flex-col items-start gap-0.5">
      {children}
      {since && (
        <span className="font-mono text-[10px] leading-none tabular-nums text-faint">since {since}</span>
      )}
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
          className="inline-flex w-12 flex-col gap-1"
        >
          <span aria-hidden="true" className="font-mono text-xs font-semibold leading-none tabular-nums text-fg">
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
        <Flagged state="waiting" since={formatLocalTime(run.status_since)}>
          <Badge tone="warning" dot title="This run is waiting on you: a plan approval, a question or a follow-up.">
            waits on you
          </Badge>
        </Flagged>
      );
    case "stalled":
      return (
        <Flagged state="stalled" since={formatLocalTime(run.health_since)}>
          <Badge tone="danger" title="No progress is being made; the percentage returns once activity resumes.">
            <span aria-hidden="true">◼</span> stalled
          </Badge>
        </Flagged>
      );
    // parked/queued/planning are quiet column values, not pills: on a card row the status
    // pill beside them already names the state, so a second bordered badge would read as
    // the same word twice. Mock section 3 draws these as dim flags.
    case "parked":
      return (
        <span
          data-run-progress="parked"
          title="Parked: the run is held and is not working right now."
          className="inline-flex items-center gap-1 whitespace-nowrap font-mono text-[11px] text-faint"
        >
          <PauseGlyph /> {parkLabel(run)}
        </span>
      );
    case "queued":
      return (
        <span data-run-progress="queued" className="font-mono text-[11px] text-faint">
          queued
        </span>
      );
    case "planning":
      return (
        <span
          data-run-progress="planning"
          title="Planning: no milestones are frozen yet, so there is no percentage."
          className="font-mono text-[11px] text-plan"
        >
          planning
        </span>
      );
    case "none":
      return (
        <span
          data-run-progress="none"
          title="No milestone plan to measure progress against"
          className="font-mono text-[11px] text-faint"
        >
          <span aria-hidden="true">—</span>
          <span className="sr-only">no progress estimate</span>
        </span>
      );
    default:
      return null;
  }
}
