import { useRef, useState } from "react";

import { api, type FreshResumePreview, type FreshResumeStart, type Run } from "../lib/api";
import { ApiError } from "../lib/apiError";
import {
  FRESH_RESUME_CUTOFF_TEXT,
  freshResumeStatusText,
  freshResumeUncoveredText,
} from "../lib/freshResumeText";
import { Alert, Button } from "./ui";

// FreshResumeSection is the owner's fresh resume confirm inside the worker decision panel (issue
// #4628): it loads the preview on demand, shows the evidence and the cutoff text, and starts with ONE
// request id per confirm action. A retry of the same confirm (after a lost reply or a server error)
// reuses the id; a definite refusal, a success, or reviewing a fresh preview discards it, so the next
// confirm makes a new one.
type Phase =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "preview"; preview: FreshResumePreview }
  | { kind: "started"; started: FreshResumeStart };

function refusalReason(err: unknown): string | undefined {
  if (!(err instanceof ApiError) || err.status !== 409) return undefined;
  const reason = (err.body as { reason?: unknown } | null)?.reason;
  return typeof reason === "string" ? reason : "";
}

export function FreshResumeSection({ run, busy }: { run: Run; busy: boolean }) {
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });
  const [notice, setNotice] = useState<string | null>(null);
  const [retryable, setRetryable] = useState(false);
  const [sending, setSending] = useState(false);
  const requestId = useRef<string | null>(null);

  const review = async () => {
    requestId.current = null;
    setRetryable(false);
    setNotice(null);
    setPhase({ kind: "loading" });
    try {
      setPhase({ kind: "preview", preview: await api.getFreshResumePreview(run.id) });
    } catch {
      setPhase({ kind: "idle" });
      setNotice("Could not load the fresh resume preview. Try again.");
    }
  };

  const confirm = async (preview: FreshResumePreview) => {
    if (!preview.selection_token) return;
    requestId.current ??= crypto.randomUUID();
    const request_id = requestId.current;
    setSending(true);
    setNotice(null);
    try {
      const started = await api.startFreshResume(run.id, { selection_token: preview.selection_token, request_id });
      requestId.current = null;
      setRetryable(false);
      setPhase({ kind: "started", started });
    } catch (err) {
      const reason = refusalReason(err);
      if (reason !== undefined) {
        // A definite refusal: nothing was started and this id is spent.
        requestId.current = null;
        setRetryable(false);
        setNotice(freshResumeStatusText(reason));
        setPhase({ kind: "idle" });
      } else if (!(err instanceof ApiError) || err.status >= 500) {
        // Unknown outcome (network failure or a server error): the same confirm may be retried with the
        // same id.
        setRetryable(true);
        setNotice("The start may or may not have been recorded. Retry to confirm with the same request.");
      } else {
        // A definite client error (400, 401, 404 ...): the server did not accept it, so a retry of the
        // same request cannot help; spend the id and review a fresh preview.
        requestId.current = null;
        setRetryable(false);
        setNotice("The start was not accepted. Reload the run and review a new preview.");
        setPhase({ kind: "idle" });
      }
    } finally {
      setSending(false);
    }
  };

  return (
    <div className="mt-3 border-t border-warn/30 pt-3" data-testid="fresh-resume">
      {phase.kind === "idle" && (
        <Button size="sm" variant="ghost" disabled={busy} onClick={review}>Fresh resume on another worker…</Button>
      )}
      {phase.kind === "loading" && <p className="text-xs text-muted" role="status">Loading the fresh resume preview…</p>}
      {phase.kind === "preview" && (phase.preview.status !== "available" || !phase.preview.selection_token ? (
        <p className="text-xs text-muted">{freshResumeStatusText(phase.preview.status)}</p>
      ) : (
        <div className="space-y-2">
          <p className="text-sm font-semibold">Fresh resume on a separate hosted worker</p>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 text-xs">
            <dt className="text-muted">Evidence generation (Ge)</dt>
            <dd>{phase.preview.evidence_generation} (latest claim generation {phase.preview.claim_generation})</dd>
            <dt className="text-muted">Evidence kind</dt>
            <dd>{phase.preview.kind === "capture" ? "recovery capture" : phase.preview.kind === "checkpoint_ref" ? "checkpoint ref" : "unknown"}</dd>
            <dt className="text-muted">Commit</dt>
            <dd className="break-all font-mono">{phase.preview.sha}</dd>
          </dl>
          <ul className="list-disc space-y-1 pl-5 text-xs text-muted">
            {FRESH_RESUME_CUTOFF_TEXT.map((t) => <li key={t}>{t}</li>)}
            {phase.preview.uncovered_generations.length > 0 &&
              <li>{freshResumeUncoveredText(phase.preview.uncovered_generations)}</li>}
          </ul>
          <div className="flex gap-2">
            <Button size="sm" disabled={busy || sending} onClick={() => void confirm(phase.preview)}>
              {retryable ? "Retry fresh resume" : "Confirm fresh resume"}
            </Button>
            <Button size="sm" variant="ghost" disabled={sending} onClick={() => setPhase({ kind: "idle" })}>Not now</Button>
          </div>
        </div>
      ))}
      {phase.kind === "started" && (
        <p className="text-xs text-muted" role="status">
          {phase.started.replay ? "Fresh resume was already started by this request." : "Fresh resume started."} A separate worker is taking the run.
        </p>
      )}
      {notice && <div className="mt-2"><Alert message={notice} tone="warning" /></div>}
    </div>
  );
}
