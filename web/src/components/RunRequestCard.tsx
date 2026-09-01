import { useState } from "react";
import { Link } from "react-router-dom";
import { api, type RunRequest } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Button } from "./ui";
import { PlayIcon } from "./icons";
import { stripUnsafeChars } from "../lib/safeText";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";

// RunRequestCard renders a chat agent's start_run REQUEST (PRD #191 M5) as a
// human-gated card. Like ProposalCard, the load-bearing rule is that title/repo_path
// come from the immutable, MODEL-authored run-message payload and render as plain
// INERT JSX text — never Markdown, no clickable model link. The run is queued ONLY on
// the human's Start click, gated exactly as the board start button (an issue with no
// PRD is refused with the same message); the run link comes from the app's start
// response, never from model text.
export function RunRequestCard({ request }: { request: RunRequest }) {
  const demo = useDemoMode();
  const [state, setState] = useState<"pending" | "started" | "dismissed">("pending");
  const [runId, setRunId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const start = async () => {
    setErr("");
    setBusy(true);
    try {
      const { run } = await api.startRunFromChat(request.repo_path, request.issue_iid);
      setRunId(run.id);
      setState("started");
    } catch (e) {
      // The server returns the same PRD/gate refusal the board start button produces.
      setErr(errorMessage(e, "Action failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="overflow-hidden rounded-xl border border-brand/40 bg-brand/[0.06]">
      <div className="flex items-center gap-1.5 border-b border-brand/20 bg-brand/10 px-3 py-2 text-xs font-semibold text-brand">
        <span aria-hidden="true">
          <PlayIcon />
        </span>
        Start a run
      </div>

      <div className="space-y-3 px-3 py-3">
        {/* Inert model text: escaped JSX, never Markdown, passed through stripUnsafeChars
            (escaping does not touch a bidi override). Display only — Start posts
            repo_path/issue_iid, re-validated server-side. */}
        <div className="space-y-1">
          <p className="font-mono text-[11px] text-faint">{stripUnsafeChars(maskRepoPath(request.repo_path, demo))}</p>
          <p className="text-sm font-semibold text-fg">
            Issue #{request.issue_iid}
            {request.title ? <span className="text-muted">{" · "}{stripUnsafeChars(request.title)}</span> : null}
          </p>
        </div>

        {err && <p className="text-xs text-danger">{err}</p>}

        {state === "pending" && (
          <div className="flex flex-wrap gap-2 pt-0.5">
            <Button size="sm" disabled={busy} onClick={start}>
              Start run
            </Button>
            <Button size="sm" variant="secondary" disabled={busy} onClick={() => setState("dismissed")}>
              Dismiss
            </Button>
          </div>
        )}

        {state === "started" && (
          <div className="rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-sm text-ok">
            <span className="font-medium">Run started.</span>{" "}
            {runId && (
              <Link
                to={`/runs/${runId}`}
                className="font-medium underline underline-offset-2 hover:text-ok"
              >
                View the run
              </Link>
            )}
          </div>
        )}

        {state === "dismissed" && <p className="text-sm text-faint">Dismissed. No run was started.</p>}
      </div>
    </div>
  );
}
