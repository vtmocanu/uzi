// RunProgressCell renders compact state flags after the milestone badge in Dashboard
// "Recent runs" and Runs rows. Percentages live in the adjacent running StatusPill.
// It only renders what the server derived (RunDTO.progress) — never re-derives a percent
// or a flag client-side (PRD D3) — and every string it draws is enum-derived copy or a
// number, so no untrusted text reaches it.
//
// States (PRD rules 1-8; the board shows `70%` without the ≈ the run page carries):
//   percent  → nothing here; the adjacent running StatusPill carries the estimate
//   waiting  → `● waits on you · plan gate|question|follow-up · 6m`, warn, one line: the
//              reason from the status, the elapsed time from status_since (the clock time
//              rides in the tooltip and the screen-reader text)
//   stalled  → `◼ stalled · 14m`, danger, one line: the elapsed time from health_since;
//              its tooltip and accessible name carry the health word (stalled vs looping),
//              because the row drops its RunHealthBadge while this flag shows (see
//              progressShowsStall)
//   parked / queued / planning → screen-reader-only `Progress: <word>`: the status pill
//              beside the cell already names the state, so a visible flag would repeat it
//              (the TUI also omits flags already carried by its status word)
//   none     → screen-reader-only `Progress: no estimate`, no visible placeholder
// An absent/null progress (a pre-feature server, or a terminal run) or an unknown state
// (a newer server's enum member) renders nothing, so the row reads exactly as before.
import type { Run, RunProgress } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";
import { accountCooldownText, codexAdmissionText, formatElapsed, isMaintenanceQueued, isReserveQueued, parkLabel, queuedBlockerPrecedesCodexAdmission } from "../lib/runBadge";
import { stripUnsafeChars } from "../lib/safeText";
import { Badge } from "./ui";

// The fields the cell reads: the progress DTO, the two "since" instants, the health flag
// (the stalled flag's wording), and whatever parkLabel folds through effectiveRunStatus
// to name a park.
type ProgressRun = Pick<Run, "progress" | "status_since" | "health_since"> &
  Partial<Pick<Run, "health" | "health_reason" | "queue_reason" | "codex_admission_class" | "codex_admission_eligible_at" | "account_cooldown_class" | "account_cooldown_eligible_at">> &
  Parameters<typeof parkLabel>[0];

// progressShowsStall reports whether the cell shows the stalled flag. A row whose cell is
// visible suppresses its RunHealthBadge then, so one stall is not drawn twice (`◼ stalled`
// beside `⚠ stalled · Nm`); the cell's tooltip and accessible name keep the health word.
export function progressShowsStall(run: { progress?: RunProgress | null; health?: string }): boolean {
  return run.progress?.state === "stalled" && run.health !== "waiting_account_cooldown";
}

// The waiting reason, named from the raw status the server derived `waiting` from.
const WAIT_REASONS: Record<string, string> = {
  awaiting_approval: "plan gate",
  awaiting_input: "question",
  awaiting_followup: "follow-up",
};

const WAIT_ACTIONS: Record<string, string> = {
  awaiting_approval: "your plan approval",
  awaiting_input: "your answer",
  awaiting_followup: "your follow-up",
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
  return reason === "" ? base : `${base} Reason: ${reason}`;
}

// Since renders the elapsed time inline (` · 14m`, hidden from assistive tech) plus a
// screen-reader sentence carrying the same duration and the clock time. Nothing when the
// instant is missing or unparseable.
function Since({ iso, clock }: { iso: string | null | undefined; clock: number }) {
  const at = formatLocalTime(iso);
  const ms = iso ? Date.parse(iso) : NaN;
  if (at === null || !Number.isFinite(ms)) return null;
  const dur = formatElapsed(clock - ms);
  return (
    <>
      <span aria-hidden="true"> · {dur}</span>
      <span className="sr-only">
        , for {dur}, since {at}
      </span>
    </>
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

export function RunProgressCell({ run, now }: { run: ProgressRun; now?: number }) {
  const p = run.progress;
  if (!p) return null;
  const clock = now ?? Date.now();
  if (run.health === "waiting_account_cooldown") {
    return <Quiet state="waiting" word={accountCooldownText(run, clock) ?? "no estimate"} />;
  }
  switch (p.state) {
    case "percent":
      // Running rows render the estimate inside StatusPill instead of a second bar.
      return null;
    case "waiting": {
      const reason = waitReason(run.status);
      const at = formatLocalTime(run.status_since);
      const action = WAIT_ACTIONS[run.status];
      const title =
        action && at
          ? `Waiting for ${action} since ${at}.`
          : "This run is waiting on you: a plan approval, a question or a follow-up.";
      return (
        <span data-run-progress="waiting" className="inline-flex">
          <Badge tone="warning" dot title={title}>
            <span>
              waits on you{reason && ` · ${reason}`}
              <Since iso={run.status_since} clock={clock} />
            </span>
          </Badge>
        </span>
      );
    }
    case "stalled": {
      const looping = run.health === "looping";
      const at = formatLocalTime(run.health_since);
      return (
        <span data-run-progress="stalled" className="inline-flex">
          <Badge tone="danger" title={at ? `Since ${at}. ${stallTitle(run)}` : stallTitle(run)}>
            <span>
              <span aria-hidden="true">◼</span> stalled
              {looping && <span className="sr-only"> (looping)</span>}
              <Since iso={run.health_since} clock={clock} />
            </span>
          </Badge>
        </span>
      );
    }
    case "parked":
      return <Quiet state="parked" word={parkLabel(run)} />;
    case "queued":
      // PRD #3454: the adjacent RunHealthBadge draws the visible maintenance pill; the
      // screen-reader word names why the run waits.
      // PRD #3522: the same for the weekly usage reserve (maintenance wins; the reserve outranks
      // Codex account admission, as in the server's health ladder).
      return (
        <Quiet
          state="queued"
          word={
            isMaintenanceQueued(run)
              ? "queued: uzi is in maintenance mode"
              : isReserveQueued(run)
                ? "queued: weekly usage reserve"
                : (!queuedBlockerPrecedesCodexAdmission(run) && codexAdmissionText(run, clock)) || "queued"
          }
        />
      );
    case "planning":
      return <Quiet state="planning" word="planning" />;
    case "none":
      return <Quiet state="none" word="no estimate" />;
    default:
      return null;
  }
}
