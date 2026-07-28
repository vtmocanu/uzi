import type { SteerInput } from "../lib/api";
import { Badge, Button, Card, type BadgeTone } from "./ui";
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
};

// deliveryFor maps one follow-up to its chip. Order matters: the unconsumed→terminal
// "Not delivered" case must be checked before the generic Queued.
function deliveryFor(input: SteerInput, terminal: boolean, parked: Delivery | undefined): Delivery {
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

export function SteerQueueCard({
  inputs,
  terminal,
  status,
  canSteer = true,
  busy,
  onStop,
  onSend,
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
                <span className="min-w-0 whitespace-pre-wrap break-words">{i.body}</span>
                <span className="shrink-0" title={d.title}>
                  <Badge tone={d.tone}>{d.label}</Badge>
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
          <FollowUpComposer busy={busy} onSend={onSend} parked={status === "limit_wait"} />
          <Button variant="danger" disabled={busy} onClick={onStop}>
            Stop run
          </Button>
        </>
      )}
    </Card>
  );
}
