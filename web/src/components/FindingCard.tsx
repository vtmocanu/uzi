import { useState } from "react";
import { api, ApiError, isHttpsUrl, type IncidentalFindingFiledIssue } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Badge, Button } from "./ui";
import { BugIcon, ExternalLinkIcon } from "./icons";
import { stripUnsafeChars } from "../lib/safeText";

// FindingCard renders an incidental-finding card in the run stream (PRD #333 M7, D10).
//
// It is the headless equivalent of Claude Code's "want me to file these?" prompt: a worker
// mid-run flagged an OFF-TASK bug, and this card lets the human file it (File / Edit-and-file)
// or dismiss it — on their own schedule, on their own forge connection. The write happens only
// on the human's click; the card holds no forge tool of its own.
//
// TWO load-bearing rules copied from ProposalCard:
//   1. INFO/BLUE accent (D10), distinct from the amber gate cards (question/plan, which park
//      the run) and the orange primary actions. A finding is a non-blocking side note.
//   2. title/location/confidence are MODEL-authored + untrusted, so they render as escaped
//      INERT JSX text — never through <Markdown> — and each passes through stripUnsafeChars
//      first, because escaping does not touch a bidi override / zero-width (issue #124). The
//      actions post the card's `{id}`, never these strings, so the raw values still round-trip.
//
// BEST-EFFORT / ADVISORY (the backlog is the source of truth): this persisted card is a
// historical record, so an OLD card may still show File for a coordinate already filed or
// dismissed from the /findings backlog. Clicking then gets the M5 409, which this card renders
// as a friendly "already filed or resolved" state — never a crash or a scary error.
type FindingCardState = "idle" | "editing" | "filed" | "dismissed" | "resolved";

export function FindingCard({
  id,
  title,
  location,
  confidence,
  labels,
}: {
  id: string;
  title: string;
  location: string;
  confidence?: string;
  labels: string[];
}) {
  const [state, setState] = useState<FindingCardState>("idle");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [issue, setIssue] = useState<IncidentalFindingFiledIssue | null>(null);
  // Created-with-warning note (the forge issue WAS created but its local disposition could not
  // settle): a success, not a retry signal, so the card shows filed and surfaces the note inline,
  // mirroring what the CLI prints.
  const [warning, setWarning] = useState("");
  // Reason picker toggle (mirrors Judge's Dismiss ▾): closed until the user starts a dismiss.
  const [dismissing, setDismissing] = useState(false);
  // Edit-and-file draft, loaded lazily from findingIssueDraft on the first Edit click.
  const [editTitle, setEditTitle] = useState("");
  const [editDescription, setEditDescription] = useState("");
  const [editLabels, setEditLabels] = useState<string[]>([]);

  // handleFileError maps the M5 409 (a stale card acting on an already-resolved coordinate) to
  // the friendly "resolved" state, and everything else to an inline error line.
  const handleFileError = (e: unknown) => {
    if (e instanceof ApiError && e.status === 409) {
      setState("resolved");
      return;
    }
    setErr(errorMessage(e, "Could not file the finding"));
  };

  const file = async (edits?: { title?: string; description?: string; labels?: string[] }) => {
    setErr("");
    setBusy(true);
    try {
      const res = await api.fileFinding(id, edits);
      setIssue(res.issue);
      setWarning(res.warning ?? "");
      setState("filed");
    } catch (e) {
      handleFileError(e);
    } finally {
      setBusy(false);
    }
  };

  // openEditor loads the deterministic, server-sanitised draft (D4) into the editable panel.
  // The draft read is owner-scoped and never 409s (a claim happens only at file time), so a
  // failure here is a plain inline error.
  const openEditor = async () => {
    setErr("");
    setBusy(true);
    try {
      const draft = await api.findingIssueDraft(id);
      setEditTitle(draft.title);
      setEditDescription(draft.description);
      setEditLabels(draft.labels);
      setState("editing");
    } catch (e) {
      setErr(errorMessage(e, "Could not load the draft"));
    } finally {
      setBusy(false);
    }
  };

  const dismiss = async (reason: "wont_do" | "not_an_issue") => {
    setErr("");
    setBusy(true);
    setDismissing(false);
    try {
      await api.dismissFinding(id, reason);
      setState("dismissed");
    } catch (e) {
      // A dismiss 409 means the coordinate is already filed/being filed/dismissed from the
      // backlog — same advisory story as File: show the resolved state, not an error.
      if (e instanceof ApiError && e.status === 409) {
        setState("resolved");
      } else {
        setErr(errorMessage(e, "Could not dismiss the finding"));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="overflow-hidden rounded-xl border border-info/40 bg-info/[0.06]">
      <div className="flex items-center justify-between gap-2 border-b border-info/20 bg-info/10 px-3 py-2">
        <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-info">
          <span aria-hidden="true">
            <BugIcon />
          </span>
          Incidental finding
        </span>
        <FindingStatusBadge state={state} />
      </div>

      <div className="space-y-3 px-3 py-3">
        {/* Every field below is inert model text: escaped JSX (never Markdown, so a link in
            the title/location is not clickable), passed through stripUnsafeChars first because
            escaping does not touch a bidi override (issue #124). Display only. */}
        <div className="space-y-1">
          {location && (
            <p className="break-all font-mono text-[11px] text-faint">{stripUnsafeChars(location)}</p>
          )}
          <p className="text-sm font-semibold text-fg">{stripUnsafeChars(title)}</p>
        </div>
        {confidence && (
          <p className="text-xs text-muted">
            Confidence: <span className="font-medium">{stripUnsafeChars(confidence)}</span>
          </p>
        )}
        {labels.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {labels.map((l) => (
              <Badge key={l} tone="neutral">
                {stripUnsafeChars(l)}
              </Badge>
            ))}
          </div>
        )}

        {err && <p className="text-xs text-danger">{err}</p>}

        {state === "idle" && !dismissing && (
          <div className="flex flex-wrap gap-2 pt-0.5">
            <Button size="sm" disabled={busy} onClick={() => file()}>
              File
            </Button>
            <Button size="sm" variant="secondary" disabled={busy} onClick={openEditor}>
              Edit &amp; file
            </Button>
            <Button size="sm" variant="secondary" disabled={busy} onClick={() => setDismissing(true)}>
              Dismiss
            </Button>
          </div>
        )}

        {state === "idle" && dismissing && (
          <div className="space-y-1.5 pt-0.5">
            <p className="text-xs text-muted">Dismiss this finding as…</p>
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="secondary" disabled={busy} onClick={() => dismiss("wont_do")}>
                Won&apos;t do
              </Button>
              <Button size="sm" variant="secondary" disabled={busy} onClick={() => dismiss("not_an_issue")}>
                Not an issue
              </Button>
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => setDismissing(false)}>
                Cancel
              </Button>
            </div>
          </div>
        )}

        {state === "editing" && (
          <div className="space-y-2 pt-0.5">
            {/* The draft fields are server-rendered and already sanitised; the user edits them
                and the file POST re-runs the write-boundary sanitisers on whatever they send. */}
            <label className="block space-y-1">
              <span className="text-xs font-medium text-muted">Title</span>
              <input
                type="text"
                value={editTitle}
                onChange={(e) => setEditTitle(e.target.value)}
                className="w-full rounded-md border border-edge bg-surface px-2 py-1 text-sm text-fg"
              />
            </label>
            <label className="block space-y-1">
              <span className="text-xs font-medium text-muted">Description</span>
              <textarea
                value={editDescription}
                onChange={(e) => setEditDescription(e.target.value)}
                rows={5}
                className="w-full rounded-md border border-edge bg-surface px-2 py-1 font-mono text-xs text-fg"
              />
            </label>
            <div className="flex flex-wrap gap-2">
              <Button
                size="sm"
                disabled={busy}
                onClick={() => file({ title: editTitle, description: editDescription, labels: editLabels })}
              >
                File issue
              </Button>
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => setState("idle")}>
                Cancel
              </Button>
            </div>
          </div>
        )}

        {state === "filed" && (
          <div className="rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-sm text-ok">
            <span className="font-medium">Issue filed.</span>{" "}
            {/* The link is app-rendered from the file response, not model text, and only an
                anchor when it is a real https URL. */}
            {issue && isHttpsUrl(issue.web_url) ? (
              <a
                href={issue.web_url}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 font-medium underline underline-offset-2 hover:text-ok"
              >
                #{issue.iid} <ExternalLinkIcon />
              </a>
            ) : (
              issue && <span className="font-medium">#{issue.iid}</span>
            )}
            {/* warning is a server-authored constant (created-with-warning), not model text. */}
            {warning && <p className="mt-1 text-xs text-muted">· {warning}</p>}
          </div>
        )}

        {state === "dismissed" && (
          <p className="text-sm text-faint">Dismissed. Nothing was written to the forge.</p>
        )}

        {state === "resolved" && (
          <p className="text-sm text-faint">
            Already filed or resolved from the Findings backlog — the backlog is the source of
            truth for this coordinate.
          </p>
        )}
      </div>
    </div>
  );
}

function FindingStatusBadge({ state }: { state: FindingCardState }) {
  if (state === "filed") return <Badge tone="ok">filed</Badge>;
  if (state === "dismissed") return <Badge tone="neutral">dismissed</Badge>;
  if (state === "resolved") return <Badge tone="neutral">resolved</Badge>;
  return <Badge tone="info">off-task</Badge>;
}
