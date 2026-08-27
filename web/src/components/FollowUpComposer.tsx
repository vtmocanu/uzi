import { useState } from "react";
import { Button, Textarea } from "./ui";

// FollowUpComposer is the textarea + Send affordance for steering a live run (PRD #95).
// Extracted from RunView into the shared seam (Decision 9) so M2 (ActivityFeed) and M3
// (steer queue) touch disjoint files. It owns ONLY composition — the queue that shows
// delivery status lives in its own card (SteerQueueCard), lifted into useRunStream so it
// survives the run going terminal (Decision 7/B1); the Stop-run action is a card-level
// steering control, not a composer concern. On send it clears the textarea; M3 layers the
// optimistic queue entry on top of the same onSend.
export function FollowUpComposer({
  busy,
  onSend,
  parked = false,
}: {
  busy: boolean;
  onSend: (text: string) => void;
  parked?: boolean;
}) {
  const [text, setText] = useState("");
  const send = () => {
    const t = text.trim();
    if (!t) return;
    onSend(t);
    setText("");
  };
  return (
    <div className="space-y-3">
      <Textarea
        rows={2}
        // PRD #35 (web-ux F1): the default placeholder promises "resumes the agent as
        // its next turn", which on a run parked on a usage limit is the opposite of
        // what happens — nothing resumes for hours. That mattered more than the
        // wording suggests: this composer's Send is the page's only FILLED primary
        // button, so a user hunting for a way out of a park was being offered the one
        // control that reads like an escape and is not.
        //
        // The queue still accepts the message (it is delivered on resume, which is
        // genuinely useful), so the fix is honesty about WHEN, not disabling it.
        placeholder={
          parked
            ? "Send a follow-up message (queued until the run resumes)"
            : "Send a follow-up message (resumes the agent as its next turn)"
        }
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <Button disabled={busy || text.trim() === ""} onClick={send}>
        Send follow-up
      </Button>
    </div>
  );
}
