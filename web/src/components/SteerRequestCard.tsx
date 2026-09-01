import { useState } from "react";
import { api, type SteerRequest } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Button, Textarea } from "./ui";
import { ChevronsRightIcon } from "./icons";
import { stripUnsafeChars } from "../lib/safeText";

// SteerRequestCard renders a chat agent's steer_run REQUEST (PRD #322 M3) as a
// human-gated card. Like CancelRequestCard, the load-bearing rule is that the model
// text (run_id and the PROPOSED message) comes from the immutable, MODEL-authored
// run-message payload and is NEVER rendered as Markdown — the run_id is inert JSX text
// and the message is the INITIAL value of a plain controlled textarea. The human edits
// and confirms it; the follow-up is delivered ONLY on their Send click, and run_id is
// re-resolved server-side by SubmitInput (ownership + terminality + issue-run-only): a
// forged/foreign run is refused "run not found", a finished one "run has already
// finished", and a chat-run target "steering applies to issue runs, not chats".
export function SteerRequestCard({ request }: { request: SteerRequest }) {
  // The initial value is the model's proposed text, sanitized (stripUnsafeChars removes
  // bidi/zero-width control chars). After that it is an ordinary controlled input the
  // human edits — no Markdown, no anchors, nothing model-authored is clickable.
  const [message, setMessage] = useState(() => stripUnsafeChars(request.message));
  const [state, setState] = useState<"pending" | "sent" | "dismissed">("pending");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const send = async () => {
    setErr("");
    setBusy(true);
    try {
      // Send the CURRENT textarea value — the human's edit, not the model's proposal.
      await api.steerRunFromChat(request.run_id, message);
      setState("sent");
    } catch (e) {
      // The server maps 404 → "run not found", 409 → terminal or "issue runs only".
      setErr(errorMessage(e, "Action failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="overflow-hidden rounded-xl border border-brand/40 bg-brand/[0.06]">
      <div className="flex items-center gap-1.5 border-b border-brand/20 bg-brand/10 px-3 py-2 text-xs font-semibold text-brand">
        <span aria-hidden="true">
          <ChevronsRightIcon />
        </span>
        Steer this run
      </div>

      <div className="space-y-3 px-3 py-3">
        {/* Inert model text: escaped JSX, never Markdown, passed through stripUnsafeChars.
            Display only — Send posts run_id + the edited message, re-resolved server-side. */}
        <p className="font-mono text-[11px] text-faint">{stripUnsafeChars(request.run_id)}</p>

        {state === "pending" && (
          <Textarea
            rows={3}
            value={message}
            disabled={busy}
            onChange={(e) => setMessage(e.target.value)}
            aria-label="Follow-up message"
            placeholder="Follow-up instruction for the run…"
          />
        )}

        {err && <p className="text-xs text-danger">{err}</p>}

        {state === "pending" && (
          <div className="flex flex-wrap gap-2 pt-0.5">
            <Button size="sm" disabled={busy || message.trim() === ""} onClick={send}>
              Send
            </Button>
            <Button size="sm" variant="secondary" disabled={busy} onClick={() => setState("dismissed")}>
              Dismiss
            </Button>
          </div>
        )}

        {state === "sent" && (
          <div className="rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-sm text-ok">
            <span className="font-medium">Follow-up sent.</span>
          </div>
        )}

        {state === "dismissed" && <p className="text-sm text-faint">Dismissed. No follow-up was sent.</p>}
      </div>
    </div>
  );
}
