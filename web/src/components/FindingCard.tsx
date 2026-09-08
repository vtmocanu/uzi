import { useState } from "react";
import { api, ApiError, type IncidentalFindingFiledIssue } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Badge } from "./ui";
import { BugIcon } from "./icons";
import { stripUnsafeChars } from "../lib/safeText";
import { TriageActions } from "./triage/TriageActions";
import { IssueDraftCard, type IssueDraftSeed, type IssueDraftValues } from "./triage/IssueDraftCard";
import { TriageStateChip } from "./triage/TriageStateChip";
import { findingState } from "./triage/triageCopy";

// FindingCard renders an incidental-finding card in the run stream (PRD #333 M7, rebuilt on the
// shared triage row in PRD #1183 M2).
//
// It is the headless equivalent of Claude Code's "want me to file these?" prompt: a worker
// mid-run flagged an OFF-TASK bug, and this card lets the human file it or dismiss it — on their
// own schedule, on their own forge connection. The write happens only on the human's click; the
// card holds no forge tool of its own. It drives the SINGLE-finding endpoints only
// (findingIssueDraft / fileFinding / dismissFinding), never the Findings-page backlog fields.
//
// It now composes the shared triage set instead of its own controls:
//   * TriageActions — File issue · Dismiss ▾ ONLY (no Mark done: a finding's done comes only from
//     its filed issue closing, which this run-stream card never observes). dismissCopy="finding"
//     gives the worker-voiced dismiss sublines.
//   * IssueDraftCard — the shared "Draft issue" card in fixed-repo / no-selector mode (the finding
//     draft carries no repo; the server resolves the coordinate's repo at file time). Replaces the
//     old inline editor, the one-click "File" button and the green "Issue filed." box.
//   * TriageStateChip — the one triage ladder (To triage → Filed #N ↗ → Dismissed · <reason>),
//     fed the normalised findingState() adapter.
//
// TWO load-bearing rules survive the rebuild:
//   1. INFO/BLUE accent (D10), distinct from the amber gate cards (question/plan, which park the
//      run) and the brand primary actions. A finding is a non-blocking side note.
//   2. title/location/confidence/labels are MODEL-authored + untrusted, so they render as escaped
//      INERT JSX text — never through <Markdown> — and each passes through stripUnsafeChars first,
//      because escaping does not touch a bidi override / zero-width (issue #124). The actions post
//      the card's {id}, never these strings, so the raw values still round-trip.
//
// BEST-EFFORT / ADVISORY (the backlog is the source of truth): this persisted card is a historical
// record, so an OLD card may still offer File/Dismiss for a coordinate already filed or dismissed
// from the /findings backlog. Acting then gets the 409, which this card renders as its own
// friendly "already filed or resolved" advisory — outside the 4-state chip, never a crash.
type FindingCardState =
  | { kind: "open" }
  | { kind: "drafting" }
  | { kind: "filed"; issue: IncidentalFindingFiledIssue; warning: string }
  | { kind: "dismissed"; reason: "wont_do" | "not_an_issue" }
  | { kind: "resolved" };

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
  const [state, setState] = useState<FindingCardState>({ kind: "open" });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  // loadDraft maps the deterministic, owner-scoped finding draft (D4) onto the shared card's seed.
  // The draft carries no repo (the coordinate fixes it server-side), so no defaultRepoId/note is
  // set and the card renders in no-selector mode. The seed's title/description are stripped inside
  // IssueDraftCard (issue #124); the read never 409s (a claim happens only at file time).
  const loadDraft = async (): Promise<IssueDraftSeed> => {
    const draft = await api.findingIssueDraft(id);
    return {
      title: draft.title,
      description: draft.description,
      labels: draft.labels,
      provenance: draft.provenance,
    };
  };

  // onCreate is the owned-mode write: post the user's edits to the single-finding endpoint and flip
  // to the filed state (the "Filed #N ↗" chip). A 409 means the coordinate was already filed or
  // dismissed from the backlog — swallow it into this card's own resolved advisory rather than let
  // it surface as an inline draft error. Any other error re-throws so IssueDraftCard keeps the draft
  // open with the user's edits intact and shows the inline error.
  const onCreate = async (values: IssueDraftValues) => {
    try {
      const res = await api.fileFinding(id, {
        title: values.title,
        description: values.description,
        labels: values.labels,
      });
      setState({ kind: "filed", issue: res.issue, warning: res.warning ?? "" });
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setState({ kind: "resolved" });
        return;
      }
      throw e;
    }
  };

  const dismiss = async (reason: "wont_do" | "not_an_issue") => {
    setErr("");
    setBusy(true);
    try {
      await api.dismissFinding(id, reason);
      setState({ kind: "dismissed", reason });
    } catch (e) {
      // A dismiss 409 means the coordinate is already filed/being filed/dismissed from the
      // backlog — same advisory story as File: show the resolved state, not an error.
      if (e instanceof ApiError && e.status === 409) {
        setState({ kind: "resolved" });
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
        <FindingStateSlot state={state} />
      </div>

      <div className="space-y-3 px-3 py-3">
        {/* Every field below is inert model text: escaped JSX (never Markdown, so a link in the
            title/location is not clickable), passed through stripUnsafeChars first because escaping
            does not touch a bidi override (issue #124). Display only. */}
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

        {state.kind === "open" && (
          <div className="pt-0.5">
            <TriageActions
              onFile={() => {
                setErr("");
                setState({ kind: "drafting" });
              }}
              onDismiss={dismiss}
              dismissCopy="finding"
              busy={busy}
            />
          </div>
        )}

        {state.kind === "drafting" && (
          <IssueDraftCard loadDraft={loadDraft} onCreate={onCreate} onCancel={() => setState({ kind: "open" })} />
        )}

        {/* Filed shows only the "Filed #N ↗" chip in the state slot; a created-with-warning line
            (the issue WAS created but its local disposition could not settle) stays under it. */}
        {state.kind === "filed" && state.warning && <p className="text-xs text-muted">{state.warning}</p>}

        {state.kind === "dismissed" && (
          <p className="text-sm text-faint">Nothing was written to the forge.</p>
        )}

        {state.kind === "resolved" && (
          <p className="text-sm text-faint">
            Already filed or resolved from the Findings backlog — the backlog is the source of truth
            for this coordinate.
          </p>
        )}
      </div>
    </div>
  );
}

// FindingStateSlot renders the shared TriageStateChip for the three ladder states this card
// reaches (To triage / Filed / Dismissed), fed through the findingState adapter so the wording
// matches every other triage surface. The 409 "resolved" advisory is FindingCard's own concern,
// outside the four-state ladder, so it renders a plain neutral badge here and its explanation in
// the body.
function FindingStateSlot({ state }: { state: FindingCardState }) {
  switch (state.kind) {
    case "filed":
      return (
        <TriageStateChip
          {...findingState({ status: "filed", filed_issue_iid: state.issue.iid, filed_issue_url: state.issue.web_url })}
        />
      );
    case "dismissed":
      return <TriageStateChip {...findingState({ status: "dismissed", dismiss_reason: state.reason })} />;
    case "resolved":
      return <Badge tone="neutral">resolved</Badge>;
    default:
      // open + drafting are still the open rung until a write lands.
      return <TriageStateChip {...findingState({ status: "to_file" })} />;
  }
}
