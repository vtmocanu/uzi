import { useCallback, useEffect, useRef, useState } from "react";

import { api, runArchiveDownloadUrl, type RecoveryArchiveSummary, type Run } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { formatCountdown } from "../lib/limitWait";
import {
  captureView,
  formatArchiveSize,
  recoverySectionKind,
  shortSha,
  sortedArchives,
} from "../lib/recovery";
import { Alert, Badge, Button, Card, SectionTitle } from "./ui";
import { ShieldIcon, TrashIcon } from "./icons";

// RecoveryArchivesPanel is the run page's owner-facing durable-recovery section (PRD
// #1296 M5, D6/D7). It is a TOP-LEVEL section, driven by the aggregate summary rather
// than the failed-card conditional or captures.length, so it is truthful with zero
// captures: it distinguishes legacy/unsupported, an open-hold-still-preparing, and each
// capture lifecycle state (available / needs-action / expired / discarded / preparing).
//
// It fetches its own summary. The endpoint is strict-owner (a non-owner, incl. an admin
// viewing a foreign run, gets 404), so any fetch failure is treated as "no recovery
// data" and the section renders nothing — the section never leaks the existence of an
// archive to a viewer the server would refuse.
export function RecoveryArchivesPanel({ run }: { run: Run }) {
  const [summary, setSummary] = useState<RecoveryArchiveSummary | null>(null);

  // Re-fetch the owner-scoped summary. Used on mount/status-change AND after a Delete
  // archive so the list reflects the deletion (the server drops the capture / flips it to
  // discarded) without an optimistic guess that could disagree with the aggregate counts.
  const reload = useCallback(() => {
    return api
      .getRunArchives(run.id)
      .then((s) => setSummary(s))
      .catch(() => {
        /* keep the last-good summary; a transient failure must not blank the section */
      });
  }, [run.id]);

  useEffect(() => {
    let live = true;
    api
      .getRunArchives(run.id)
      .then((s) => {
        if (live) setSummary(s);
      })
      .catch(() => {
        if (live) setSummary(null);
      });
    return () => {
      live = false;
    };
    // Re-fetch when the run id changes, and when its status flips terminal — the archive
    // is captured at the finalization boundary, so a run that just failed grows one.
  }, [run.id, run.status]);

  if (!summary) return null;
  const kind = recoverySectionKind(summary, run.status);
  if (!kind) return null;

  return (
    // id anchor so the Workers custody surface can deep-link straight to a run's archives.
    <Card id="recovery-archives" className="scroll-mt-20 space-y-3 p-4">
      <div className="flex items-center gap-2">
        <ShieldIcon className="h-4 w-4 text-muted" aria-hidden="true" />
        <SectionTitle>Recovery archives</SectionTitle>
      </div>

      {/* What this is — and, just as importantly, what it is NOT (D7). */}
      <p className="text-sm text-muted">
        This recovers the committed Git history captured at the protected finalization
        boundary. It does not include uncommitted files, and it cannot recover every
        worker that stopped before that boundary was reached.
      </p>

      {kind === "unsupported" && (
        <p className="text-sm text-muted">
          Durable recovery was not available for this run. It ran on a worker that
          predates recovery, so the original committed history was not captured.
        </p>
      )}

      {kind === "unavailable" && (
        <p className="text-sm text-muted">
          No recoverable archive exists for this run. Any committed history was already
          published, released, or could not be captured.
        </p>
      )}

      {kind === "preparing" && (
        <div className="rounded-lg border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
          Preparing the recovery archive. The committed history is being captured and
          stored, and a download appears here once it is ready.
        </div>
      )}

      {kind === "captures" && (
        <>
          {/* The secret warning sits directly above every download control, so it is on
              every download surface (D7). */}
          <Alert
            tone="warning"
            message="Treat every archive as if it contains secrets. Review the history before you publish it anywhere. If it exposed a real credential, revoke and rotate it and remove it from the affected history — deleting the archive alone is not enough."
          />
          <ul className="space-y-3">
            {sortedArchives(summary).map((cap) => (
              <CaptureRow key={cap.id} runId={run.id} capture={cap} onDeleted={reload} />
            ))}
          </ul>
        </>
      )}
    </Card>
  );
}

// CaptureRow renders one capture's metadata + its (state-gated) download control. Every
// worker-authored field — source_sha, attempted_head_sha, prerequisite_shas and the
// bounded reason — is UNTRUSTED and rendered as escaped plain text through
// stripUnsafeChars, never through <Markdown> (D6, auditor R2).
function CaptureRow({
  runId,
  capture,
  onDeleted,
}: {
  runId: string;
  capture: RecoveryArchiveSummary["archives"][number];
  onDeleted: () => void;
}) {
  const now = useNow(60_000);
  const view = captureView(capture.state);
  const expiresIn = capture.expires_at ? formatCountdown(capture.expires_at, now) : null;
  // "Delete archive" (D7/D9) is artifact cleanup, distinct from the "Discard held work"
  // hold discard. It is offered only while the archive artifact meaningfully exists — an
  // available (downloadable) capture, or a needs_action one that still retains recovery —
  // never for an in-flight (preparing/uploading) capture the worker owns, nor an already
  // gone (expired/discarded) one.
  const deletable = capture.state === "available" || capture.state === "needs_action";
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const confirmRef = useRef<HTMLDivElement>(null);
  const deleteBtnId = `delete-archive-btn-${capture.id}`;
  const warningId = `delete-archive-warning-${capture.id}`;
  const [restoreFocus, setRestoreFocus] = useState(false);

  useEffect(() => {
    if (confirming) confirmRef.current?.focus();
  }, [confirming]);
  useEffect(() => {
    if (restoreFocus && !confirming) {
      document.getElementById(deleteBtnId)?.focus();
      setRestoreFocus(false);
    }
  }, [restoreFocus, confirming, deleteBtnId]);

  const dismiss = () => {
    setConfirming(false);
    setError("");
    setRestoreFocus(true);
  };
  const remove = async () => {
    setBusy(true);
    setError("");
    try {
      await api.discardRunArchive(runId, capture.id);
      onDeleted();
    } catch (e) {
      setError(errorMessage(e, "Could not delete the archive."));
      setBusy(false);
    }
  };

  return (
    <li className="rounded-lg border border-edge bg-raised/40 p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <Badge tone={view.tone}>{view.label}</Badge>
          <span className="font-mono text-xs text-muted">
            {stripUnsafeChars(shortSha(capture.source_sha))}
          </span>
          <span className="text-xs text-faint">{formatArchiveSize(capture.byte_size)}</span>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          {view.downloadable ? (
            // An authenticated same-origin attachment link (D6): the browser sends the
            // session cookie, the server sets Content-Disposition/no-store, and no
            // presigned/public URL is minted. `download` asks for a save rather than a
            // navigation; the server-generated filename wins over this hint.
            <a href={runArchiveDownloadUrl(runId, capture.id)} download>
              <Button variant="primary" size="sm">
                Export archive
              </Button>
            </a>
          ) : (
            <Button variant="secondary" size="sm" disabled>
              Export archive
            </Button>
          )}
          {deletable && !confirming && (
            <Button id={deleteBtnId} variant="danger" size="sm" onClick={() => setConfirming(true)}>
              <TrashIcon /> Delete archive
            </Button>
          )}
        </div>
      </div>

      {confirming && (
        <div
          ref={confirmRef}
          tabIndex={-1}
          role="group"
          aria-label="Delete this recovery archive"
          aria-describedby={warningId}
          onKeyDown={(e) => {
            if (e.key === "Escape") dismiss();
          }}
          className="mt-3 space-y-3 rounded-lg border border-danger/40 bg-danger/10 p-3 outline-hidden"
        >
          {/* D9 archive-deletion strength: warn that the server copy may be the only
              remaining recovery artifact, and recommend exporting first. Weaker than the
              possible-only-copy hold discard (no typed confirmation), stronger than a bare
              are-you-sure. */}
          <p id={warningId} className="text-sm text-danger">
            {view.downloadable
              ? "This permanently deletes the archived committed history from the server. It may be the only remaining copy of this work — export it first if you might need it. This cannot be undone."
              : "This permanently deletes the retained recovery for this capture. It may be the only remaining copy of this work, and it cannot be undone."}
          </p>
          {error && <p className="text-xs text-danger">{error}</p>}
          <div className="flex items-center gap-1.5">
            <Button variant="dangerSolid" size="sm" disabled={busy} onClick={remove}>
              {busy ? "Deleting…" : "Delete archive"}
            </Button>
            <Button variant="ghost" size="sm" onClick={dismiss} disabled={busy}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      <p className="mt-2 text-xs text-muted">{view.description}</p>

      <dl className="mt-2 grid grid-cols-1 gap-x-4 gap-y-1 text-xs text-faint sm:grid-cols-2">
        <div>
          <dt className="inline text-faint">Original head: </dt>
          <dd className="inline font-mono text-muted">
            {stripUnsafeChars(shortSha(capture.source_sha))}
          </dd>
        </div>
        {capture.attempted_head_sha && (
          <div>
            <dt className="inline text-faint">Attempted publish: </dt>
            <dd className="inline font-mono text-muted">
              {stripUnsafeChars(shortSha(capture.attempted_head_sha))}
            </dd>
          </div>
        )}
        {capture.prerequisite_shas && capture.prerequisite_shas.length > 0 && (
          <div className="sm:col-span-2">
            <dt className="inline text-faint">Prerequisites: </dt>
            <dd className="inline font-mono text-muted">
              {capture.prerequisite_shas
                .map((s) => stripUnsafeChars(shortSha(s)))
                .join(", ")}
            </dd>
          </div>
        )}
        {expiresIn && (
          <div>
            <dt className="inline text-faint">Expires in: </dt>
            <dd className="inline text-muted">{expiresIn}</dd>
          </div>
        )}
      </dl>

      {/* A bounded, sanitized reason (e.g. a needs_action cause). Untrusted worker text:
          escaped plain text via stripUnsafeChars, whitespace-pre-wrap, never <Markdown>. */}
      {capture.reason && capture.reason.trim() !== "" && (
        <p className="mt-2 whitespace-pre-wrap break-words rounded-md bg-surface/60 px-2 py-1 text-xs text-muted">
          {stripUnsafeChars(capture.reason)}
        </p>
      )}
    </li>
  );
}
