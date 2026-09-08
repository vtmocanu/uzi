import { useState } from "react";
import type { Run, SteerInput } from "../lib/api";
import { formatElapsed, milestoneBadge } from "../lib/runBadge";
import { useNow } from "../lib/useNow";
import { Badge, Button, Card, cx, type BadgeTone } from "./ui";
import { FollowUpComposer } from "./FollowUpComposer";

// SteerQueueCard is the run's steering surface (PRD #95): the follow-up queue with
// per-entry delivery status, plus — while the run is live — the composer + Stop control.
//
// Critically, this card is rendered UNCONDITIONALLY by RunView — including for a terminal
// run — and its `inputs` are lifted into useRunStream (Decision 7/B1). That is what lets
// the queue survive the run completing and still show "Not delivered — run finished"; it
// could never do that from inside the !terminal-gated composer, which unmounts on
// completion. The composer + Stop, by contrast, are only meaningful for a live run, so
// they are gated on !terminal.
//
// Delivery state (Decision 7) is derived CLIENT-SIDE from (consumed_at, run.status): a
// stamped consumed_at means the worker has the input for its next turn (not that it acted
// on it — see the PRD's consumed-but-dropped caveats). The parked copy needs the run's
// status (awaiting_approval, or awaiting_input since PRD #88); RunView passes it via
// `status` (optional so the card still renders without it — those cases then degrade to
// a plain "Delivered").

type Delivery = { label: string; tone: BadgeTone; title: string };

// PARKED_COPY is the qualified wording for a follow-up consumed while the run is
// blocked on a human (PRD #95 S3, extended by PRD #88). A follow-up submitted at a park
// IS consumed immediately — the steering channel polls throughout — but the worker only
// applies it on its next turn, which does not come until the human acts. A bare
// "Delivered" would mislead in both cases; naming the WRONG action would too, which is
// why this is keyed on the status rather than on one "parked" boolean.
const PARKED_COPY: Record<string, Delivery> = {
  awaiting_approval: {
    label: "Delivered — applies after approval",
    tone: "warning",
    title: "Handed to the worker, but it takes effect only after you approve the plan.",
  },
  awaiting_input: {
    label: "Delivered — applies after you answer",
    tone: "warning",
    title: "Handed to the worker, but it takes effect only after you answer its question.",
  },
  // PRD #517: an interactive run parked awaiting the owner's next follow-up. Sending
  // one here is the WHOLE point — it is consumed immediately and IS what resumes the
  // run — so the copy names the resuming action rather than a blocking gate.
  awaiting_followup: {
    label: "Delivered — resumes the run",
    tone: "warning",
    title: "Handed to the worker; it picks this up as the next turn and continues the run.",
  },
};

// SCOPE_DISPOSITION maps an operator scope-ceiling directive (PRD #634, kind
// "scope") to its chip. A scope row is NEVER consumed — its state lives entirely
// in `disposition`, not in `consumed_at` — so it does not touch deliveryFor's
// consumed_at logic at all. `null` is the pending state (the directive is set but
// the worker has not yet reached the ceiling to finalize or supersede it).
const SCOPE_DISPOSITION: Record<string, Delivery> = {
  applied: {
    label: "Applied — finalized at the ceiling",
    tone: "ok",
    title: "The worker reached the scope ceiling and finalized the run's committed slice there.",
  },
  superseded: {
    label: "Superseded — a later directive replaced it",
    tone: "neutral",
    title: "A later scope directive replaced this one before it took effect.",
  },
  declined: {
    label: "Declined — not acted on",
    tone: "neutral",
    title: "This scope directive was not acted on.",
  },
};

// scopeDeliveryFor resolves a scope row's chip from its disposition. A null (or
// otherwise unrecognised) disposition is the pending "Active — scope ceiling set"
// state — the directive is in force but the run has not yet finalized against it.
function scopeDeliveryFor(disposition: string | null): Delivery {
  const known = disposition != null ? SCOPE_DISPOSITION[disposition] : undefined;
  if (known) return known;
  return {
    label: "Active — scope ceiling set",
    tone: "queue",
    title: "The scope ceiling is set; the run will finalize its committed slice when it reaches it.",
  };
}

// deliveryFor maps one steer-queue entry to its chip. A scope directive is driven by
// its disposition (checked FIRST, before any consumed_at logic — a scope row is never
// consumed). The follow_up path is unchanged: order matters, the unconsumed→terminal
// "Not delivered" case must be checked before the generic Queued.
function deliveryFor(input: SteerInput, terminal: boolean, parked: Delivery | undefined): Delivery {
  if (input.kind === "scope") return scopeDeliveryFor(input.disposition);
  const consumed = input.consumed_at != null;
  if (!consumed) {
    if (terminal) {
      // The run finished before the worker drained this input — it never got it.
      return {
        label: "Not delivered — run finished",
        tone: "neutral",
        title: "The run reached a terminal state before the worker consumed this follow-up.",
      };
    }
    // Non-terminal (incl. awaiting_approval): the worker will consume it; until it does
    // it is genuinely pending.
    return { label: "Queued", tone: "queue", title: "Waiting for the worker to consume this follow-up." };
  }
  if (parked) return parked;
  return {
    label: "Delivered",
    tone: "ok",
    title: "Handed to the worker for its next turn (whether it acted on it shows in the agent's messages).",
  };
}

// PRD #1190 (D7): pause is meaningful only for the kinds that run for a while and do NOT
// already park between turns. A chat run parks between turns; an interactive task parks at
// awaiting_followup after every turn; judge/mr_rework/ci_fix are short and self-ending. So
// the menu is offered for issue, non-interactive task, prompt and self_improve only — the
// server enforces the same allowlist (a 409 with a per-kind reason), this is the UI
// agreeing with it, not the enforcement.
const PAUSABLE_KINDS = new Set(["issue", "task", "prompt", "self_improve"]);

function isPausableRun(run: Run): boolean {
  // An interactive task parks after each turn already, so it is never pausable even though
  // its kind is in the set above (mirrors CreatePauseInput's `interactive = false` guard).
  if (run.interactive) return false;
  return PAUSABLE_KINDS.has(run.kind);
}

// PauseMenu is the "‖ Pause ▾" dropdown that sits beside Stop run for a running, pausable
// run (PRD #1190). Two items: "After current milestone" (recommended; "After current
// step" on a run with no frozen milestones) and "Pause now" (danger, names what an
// immediate park discards). The milestone item is HIDDEN on the final milestone, since the
// run completes anyway (the mock/server never offer a milestone park that equals finishing).
function PauseMenu({
  run,
  busy,
  onPause,
}: {
  run: Run;
  busy: boolean;
  onPause: (mode: "milestone" | "now") => void;
}) {
  const [open, setOpen] = useState(false);
  // Freshen the checkpoint age only while the menu is open — no ticker for the life of the
  // card. 30s is plenty for a "9m ago" that only needs to be roughly right when read.
  const now = useNow(open ? 30_000 : null);

  const mb = milestoneBadge(run);
  const hasMilestones = mb != null;
  // On the final milestone (only the last one remains, or all are done) a milestone park
  // equals finishing the run, so the item is hidden. With no frozen list there is no
  // "final milestone" to be on, so the "After current step" item always shows.
  const onFinalMilestone = mb != null && mb.done >= mb.total - 1;
  const showMilestoneItem = !onFinalMilestone;
  const milestoneLabel = hasMilestones ? "After current milestone" : "After current step";

  // The age of the last pushed checkpoint, named so the owner knows what "Pause now"
  // costs before choosing it (D5). null checkpoint_tip_at → no age clause.
  const cp = run.checkpoint_tip_at ? Date.parse(run.checkpoint_tip_at) : NaN;
  const checkpointAge = Number.isFinite(cp) ? formatElapsed(now - cp) : null;
  const nowCopy = checkpointAge
    ? `Drops the turn in flight. Work since the last checkpoint (${checkpointAge} ago) is discarded.`
    : "Drops the turn in flight. Work since the last checkpoint is discarded.";

  return (
    <div className="relative">
      <Button
        variant="secondary"
        disabled={busy}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        ‖ Pause ▾
      </Button>
      {open && (
        <div
          role="menu"
          className="absolute left-0 z-10 mt-1 w-72 rounded-md border border-edge bg-raised p-1 shadow-lg"
        >
          {showMilestoneItem && (
            <button
              type="button"
              role="menuitem"
              disabled={busy}
              className="flex w-full flex-col gap-0.5 rounded-sm px-2 py-1.5 text-left hover:bg-edge/40"
              onClick={() => {
                setOpen(false);
                onPause("milestone");
              }}
            >
              <span className="flex items-center gap-1.5 text-sm font-medium text-fg">
                {milestoneLabel}
                <Badge tone="ok">recommended</Badge>
              </span>
              <span className="text-xs text-muted">
                Finishes the work in flight, pushes a checkpoint, then parks. Nothing in flight is lost.
              </span>
            </button>
          )}
          <button
            type="button"
            role="menuitem"
            disabled={busy}
            className={cx(
              "flex w-full flex-col gap-0.5 rounded-sm px-2 py-1.5 text-left hover:bg-danger/10",
            )}
            onClick={() => {
              setOpen(false);
              onPause("now");
            }}
          >
            <span className="text-sm font-medium text-danger">Pause now</span>
            <span className="text-xs text-muted">{nowCopy}</span>
          </button>
          <p className="px-2 py-1 text-[11px] text-faint">
            Paused time does not count toward the budget. Nothing runs and nothing is spent while it waits.
          </p>
        </div>
      )}
    </div>
  );
}

export function SteerQueueCard({
  inputs,
  terminal,
  status,
  canSteer = true,
  busy,
  onStop,
  onSend,
  run,
  onPause,
}: {
  inputs: SteerInput[];
  terminal: boolean;
  // The run status, used only to render the parked copy ("Delivered — applies after
  // approval" / "…after you answer"). Optional: absent degrades those to "Delivered".
  status?: string;
  // canSteer is false for a NON-OWNER viewer (a non-owner admin can open the owner-or-
  // admin run view, but the owner-only /inputs 404s — useRunStream reports that here).
  // Decision 8/N2: hide the composer + Stop so there is no Send affordance that 404s.
  // Defaults true (owner) so a legit owner is never hidden. A non-owner's queue is also
  // empty (the 404 leaves it []), so the whole card collapses to nothing below.
  canSteer?: boolean;
  busy: boolean;
  onStop: () => void;
  onSend: (text: string) => void;
  // PRD #1190: the full run + the pause callback, needed only for the "‖ Pause ▾" menu
  // beside Stop run. OPTIONAL so every existing caller/test that renders the card without a
  // pause affordance is unchanged — the menu renders only when BOTH are supplied AND the
  // run is a running, pausable one with no pause already pending.
  run?: Run;
  onPause?: (mode: "milestone" | "now") => void;
}) {
  // Render nothing when there is no queue to show AND no composer to offer: a finished
  // run with an empty queue (matching the pre-v2 "no composer once terminal" behavior),
  // OR a non-owner viewer (canSteer false, queue always empty → empty/hidden and silent,
  // never a bare "Steer this run" heading with a broken Send). An owner's live run with
  // an empty queue still renders — it carries the composer.
  if (inputs.length === 0 && (terminal || !canSteer)) return null;

  const parked = status ? PARKED_COPY[status] : undefined;

  return (
    <Card className="space-y-3 p-4">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Steer this run</h2>

      {inputs.length > 0 && (
        <ul className="space-y-1.5">
          {inputs.map((i) => {
            const d = deliveryFor(i, terminal, parked);
            return (
              <li
                key={i.id}
                className="flex items-start justify-between gap-3 rounded-md border border-edge bg-raised/40 px-3 py-2 text-sm text-muted"
              >
                <span className="min-w-0 whitespace-pre-wrap break-words">
                  {/* PRD #634: tag a scope directive so an operator can tell it apart from a
                      follow-up. The body itself is a server-authored string and renders as an
                      escaped React text child, never through a raw-HTML sink. */}
                  {i.kind === "scope" && (
                    <span className="mr-1.5 rounded-sm border border-edge-strong bg-raised px-1 py-[1px] align-middle text-[10px] font-semibold uppercase tracking-wider text-faint">
                      scope
                    </span>
                  )}
                  {i.body}
                </span>
                <span className="shrink-0" title={d.title}>
                  {/* Scope-disposition labels are sentence-length (the longest is
                      "Superseded — a later directive replaced it"), so opt into Badge's
                      `wrap` (ui.tsx) to wrap the pill on a narrow viewport instead of
                      crushing the body text. Follow-up chips are short and stay nowrap. */}
                  <Badge tone={d.tone} wrap={i.kind === "scope"}>
                    {d.label}
                  </Badge>
                </span>
              </li>
            );
          })}
        </ul>
      )}

      {/* Composer + Stop are live-run-only, owner-only steering; the queue above persists
          past terminal and is shown read-only to a non-owner only if it is non-empty
          (which it never is — a non-owner's /inputs 404s, leaving the queue empty). */}
      {!terminal && canSteer && (
        <>
          {/* PRD #517: awaiting_followup is non-terminal, so the composer shows and Send
              is enabled — sending a follow-up is exactly how a follow-up park resumes.
              It is deliberately NOT `parked`: unlike limit_wait (which self-resumes hours
              later, so its placeholder says "queued until the run resumes"), a follow-up
              here IS the next turn, so the default "resumes the agent" placeholder is the
              honest one.

              Issue #754: pool_wait sets parked too. It is the same kind of self-resuming
              hold as limit_wait — blocked on a pooled token, resuming on its own — so a
              follow-up sent here will NOT un-park it; it is queued until the run resumes.
              The "queued until the run resumes" placeholder is resource-agnostic and
              stays honest for a pool hold. */}
          <FollowUpComposer
            busy={busy}
            onSend={onSend}
            parked={status === "limit_wait" || status === "pool_wait"}
          />
          {/* PRD #1190: Pause ▾ sits beside Stop run, offered only for a RUNNING, pausable
              run with no pause already pending (a pending request has its own chip + actions
              in the header). Unpausable kinds and any parked status show no menu. */}
          <div className="flex flex-wrap items-center gap-2">
            {run && onPause && status === "running" && !run.pause_requested_at && isPausableRun(run) && (
              <PauseMenu run={run} busy={busy} onPause={onPause} />
            )}
            <Button variant="danger" disabled={busy} onClick={onStop}>
              Stop run
            </Button>
          </div>
        </>
      )}
    </Card>
  );
}
