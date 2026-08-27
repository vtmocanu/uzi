import { useState, type KeyboardEvent } from "react";
import { Link } from "react-router-dom";
import { Button, Textarea } from "./ui";
import type { ComposerGate } from "../lib/chat";

// ChatComposer is the conversation's input surface (PRD #39 M4): the send box and
// its disabled states, plus the three honest notices that live with it —
//   - worker-offline banner (Decision 15): shown when no worker is connected. It
//     is UX, NOT a gate — the message still queues and is served when a worker
//     connects — so it never disables the box.
//   - turn-cap notice (Decision 3b): a heads-up as the conversation nears its cap;
//     the hard stop comes from the gate, not this line.
//   - one-live-conversation note (Decision 4): WORKER_CHAT_SESSIONS=1, so a second
//     chat queues until the active one ends.
// The gate (composerGate) is what actually disables input — a terminal chat or a
// reached turn cap.
export function ChatComposer({
  gate,
  busy,
  workerOffline,
  turnNotice,
  queuedBehindActive,
  onSend,
  onEnd,
}: {
  gate: ComposerGate;
  busy: boolean;
  workerOffline: boolean;
  turnNotice: string | null;
  queuedBehindActive: boolean;
  onSend: (text: string) => void;
  onEnd: () => void;
}) {
  const [text, setText] = useState("");

  const send = () => {
    const t = text.trim();
    if (!t || !gate.enabled || busy) return;
    onSend(t);
    setText("");
  };

  // Enter sends; Shift+Enter inserts a newline (the chat-composer convention).
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  };

  return (
    <div className="space-y-2">
      {workerOffline && (
        <div className="rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-sm text-warn">
          No worker connected — chat needs your worker running. Your message will be answered once
          one comes online.{" "}
          <Link to="/workers" className="font-medium underline underline-offset-2">
            Set up a worker
          </Link>
          .
        </div>
      )}

      {queuedBehindActive && (
        <div className="rounded-lg border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
          Another conversation is active. Only one chat runs at a time, so this one starts when the
          other ends.
        </div>
      )}

      {turnNotice && (
        <div className="rounded-lg border border-edge bg-raised/50 px-3 py-2 text-xs text-muted">
          {turnNotice}
        </div>
      )}

      {gate.enabled ? (
        <div className="space-y-2 rounded-xl border border-edge bg-surface p-3">
          <Textarea
            rows={2}
            aria-label="Message uzi"
            placeholder="Ask about uzi, your runs, or an idea… (Enter to send, Shift+Enter for a new line)"
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={onKeyDown}
          />
          <div className="flex items-center justify-between gap-2">
            <Button variant="ghost" size="sm" disabled={busy} onClick={onEnd}>
              End chat
            </Button>
            <Button disabled={busy || text.trim() === ""} onClick={send}>
              Send
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-edge bg-raised/40 px-3 py-3">
          <p className="text-sm text-faint">{gate.reason}</p>
          {/* The composer is only mounted for a non-terminal conversation, so a
              disabled gate here means the turn cap — End chat must stay reachable
              (ending it is the way out, alongside starting a new chat). */}
          <Button variant="ghost" size="sm" disabled={busy} onClick={onEnd}>
            End chat
          </Button>
        </div>
      )}
    </div>
  );
}
