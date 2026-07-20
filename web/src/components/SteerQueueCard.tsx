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
// on it — see the PRD's consumed-but-dropped caveats). The gate copy needs the run's
// awaiting_approval status; RunView passes it via `status` (optional so the card still
// renders without it — the gate case then degrades to a plain "Delivered").

type Delivery = { label: string; tone: BadgeTone; title: string };

// deliveryFor maps one follow-up to its chip. Order matters: the unconsumed→terminal
// "Not delivered" case must be checked before the generic Queued.
function deliveryFor(input: SteerInput, terminal: boolean, atGate: boolean): Delivery {
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
  if (atGate) {
    // Consumed while the run is blocked on the human's plan approval (S3): a follow-up
    // submitted at a gate IS consumed immediately, but only takes effect once the plan
    // is approved. The qualified copy is honest; a bare "Delivered" would mislead.
    return {
      label: "Delivered — applies after approval",
      tone: "warning",
      title: "Handed to the worker, but it takes effect only after you approve the plan.",
    };
  }
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
  busy,
  onStop,
  onSend,
}: {
  inputs: SteerInput[];
  terminal: boolean;
  // The run status, used only to render the gate copy ("Delivered — applies after
  // approval") when awaiting_approval. Optional: absent degrades that case to "Delivered".
  status?: string;
  busy: boolean;
  onStop: () => void;
  onSend: (text: string) => void;
}) {
  // Nothing to steer and nothing to show: on a finished run with an empty queue, render
  // no card at all (matching the pre-v2 "no composer once terminal" behavior). A live run
  // always shows the card (it carries the composer); a finished run shows it only to
  // display a non-empty queue read-only.
  if (terminal && inputs.length === 0) return null;

  const atGate = status === "awaiting_approval";

  return (
    <Card className="space-y-3 p-4">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Steer this run</h2>

      {inputs.length > 0 && (
        <ul className="space-y-1.5">
          {inputs.map((i) => {
            const d = deliveryFor(i, terminal, atGate);
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

      {/* Composer + Stop are live-run-only steering; the queue above persists past terminal. */}
      {!terminal && (
        <>
          <FollowUpComposer busy={busy} onSend={onSend} />
          <Button variant="danger" disabled={busy} onClick={onStop}>
            Stop run
          </Button>
        </>
      )}
    </Card>
  );
}
