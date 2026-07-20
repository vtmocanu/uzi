import { useState } from "react";
import { Button, Textarea } from "./ui";

// FollowUpComposer is the textarea + Send affordance for steering a live run (PRD #95).
// Extracted from RunView into the shared seam (Decision 9) so M2 (ActivityFeed) and M3
// (steer queue) touch disjoint files. It owns ONLY composition — the queue that shows
// delivery status lives in its own card (SteerQueueCard), lifted into useRunStream so it
// survives the run going terminal (Decision 7/B1); the Stop-run action is a card-level
// steering control, not a composer concern. On send it clears the textarea; M3 layers the
// optimistic queue entry on top of the same onSend.
export function FollowUpComposer({ busy, onSend }: { busy: boolean; onSend: (text: string) => void }) {
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
        placeholder="Send a follow-up message (resumes the agent as its next turn)"
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <Button disabled={busy || text.trim() === ""} onClick={send}>
        Send follow-up
      </Button>
    </div>
  );
}
