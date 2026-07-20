import type { SteerInput } from "../lib/api";
import { Button, Card } from "./ui";
import { FollowUpComposer } from "./FollowUpComposer";

// SteerQueueCard is the run's steering surface (PRD #95): the follow-up queue plus, while
// the run is live, the composer + Stop control. It is the shared-seam home M1 lands so M3
// can build the delivery UI here WITHOUT ever touching RunView again (Decision 9).
//
// Critically, this card is rendered UNCONDITIONALLY by RunView — including for a terminal
// run — and its `inputs` are lifted into useRunStream (Decision 7/B1). That is what lets
// the queue survive the run completing and still show "Not delivered — run finished"; it
// could never do that from inside the !terminal-gated composer, which unmounts on
// completion. The composer + Stop, by contrast, are only meaningful for a live run, so
// they are gated on !terminal.
//
// M1 scope: the card shell, the queue-list skeleton (a plain per-item list), and the
// composer/Stop mounts. Deferred to M3: the five delivery-state chips
// (Queued / Delivered / Delivered — applies after approval / Not delivered — run
// finished), optimistic append-on-send, and reconcile via the returned row id.
export function SteerQueueCard({
  inputs,
  terminal,
  busy,
  onStop,
  onSend,
}: {
  inputs: SteerInput[];
  terminal: boolean;
  busy: boolean;
  onStop: () => void;
  onSend: (text: string) => void;
}) {
  // Nothing to steer and nothing to show: on a finished run with an empty queue, render
  // no card at all (matching the pre-v2 "no composer once terminal" behavior). A live run
  // always shows the card (it carries the composer); a finished run shows it only to
  // display a non-empty queue read-only.
  if (terminal && inputs.length === 0) return null;

  return (
    <Card className="space-y-3 p-4">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Steer this run</h2>

      {/* Queue skeleton (PRD #95 M1). M3 replaces each row with its delivery-state chip
          and the optimistic/reconcile logic; here it is a plain list of the follow-ups. */}
      {inputs.length > 0 && (
        <ul className="space-y-1.5">
          {inputs.map((i) => (
            <li
              key={i.id}
              className="rounded-md border border-edge bg-raised/40 px-3 py-2 text-sm text-muted"
            >
              {i.body}
            </li>
          ))}
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
