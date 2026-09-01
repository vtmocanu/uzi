import { useState } from "react";
import { api, type CancelRequest } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Button } from "./ui";
import { XIcon } from "./icons";
import { stripUnsafeChars } from "../lib/safeText";

// CancelRequestCard renders a chat agent's cancel_run REQUEST (PRD #322 M1) as a
// human-gated card. Like RunRequestCard, the load-bearing rule is that run_id comes
// from the immutable, MODEL-authored run-message payload and renders as plain INERT
// JSX text — never Markdown, no clickable model link. The run is cancelled ONLY on
// the human's Cancel click, and run_id is re-resolved server-side by SubmitInput
// (ownership + terminality): a forged/foreign run is refused with "run not found" and
// an already-finished one with "run has already finished".
export function CancelRequestCard({ request }: { request: CancelRequest }) {
  const [state, setState] = useState<"pending" | "cancelled" | "dismissed">("pending");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const cancel = async () => {
    setErr("");
    setBusy(true);
    try {
      await api.cancelRunFromChat(request.run_id);
      setState("cancelled");
    } catch (e) {
      // The server maps 404 → "run not found", 409 → "run has already finished".
      setErr(errorMessage(e, "Action failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="overflow-hidden rounded-xl border border-danger/40 bg-danger/[0.06]">
      <div className="flex items-center gap-1.5 border-b border-danger/20 bg-danger/10 px-3 py-2 text-xs font-semibold text-danger">
        <span aria-hidden="true">
          <XIcon />
        </span>
        Cancel a run
      </div>

      <div className="space-y-3 px-3 py-3">
        {/* Inert model text: escaped JSX, never Markdown, passed through stripUnsafeChars
            (escaping does not touch a bidi override). Display only — Cancel posts run_id,
            re-resolved server-side. */}
        <p className="font-mono text-[11px] text-faint">{stripUnsafeChars(request.run_id)}</p>

        {err && <p className="text-xs text-danger">{err}</p>}

        {state === "pending" && (
          <div className="flex flex-wrap gap-2 pt-0.5">
            <Button size="sm" variant="danger" disabled={busy} onClick={cancel}>
              Cancel run
            </Button>
            <Button size="sm" variant="secondary" disabled={busy} onClick={() => setState("dismissed")}>
              Dismiss
            </Button>
          </div>
        )}

        {state === "cancelled" && (
          <div className="rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-sm text-ok">
            <span className="font-medium">Run cancelled.</span>
          </div>
        )}

        {state === "dismissed" && <p className="text-sm text-faint">Dismissed. The run was not cancelled.</p>}
      </div>
    </div>
  );
}
