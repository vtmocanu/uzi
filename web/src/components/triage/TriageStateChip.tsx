import { isHttpsUrl } from "../../lib/api";
import { Badge } from "../ui";
import { ExternalLinkIcon } from "../icons";
import {
  TRIAGE_OPEN_LABEL,
  dismissedLabel,
  doneLabel,
  filedLabel,
  type DismissReason,
  type FiledRef,
  type TriageState,
} from "./triageCopy";

// TriageStateChip renders the one triage ladder — To triage → Filed → Done → Dismissed — in a
// single place, so the Judge page, the run-page panel and the Findings page cannot show three
// different chips for one state (PRD #1183). It takes a NORMALISED `state`, because the two
// wires key the open rung differently (`todo` vs `to_file`); the surface's adapter
// (judgeState / findingState in triageCopy) does the mapping before this renders.
//
// The Filed chip becomes a link ONLY when the issue URL is a real https URL — the same guard
// `isHttpsUrl` (lib/api) that every filed-issue link in the app already routes through, so a
// hostile or malformed forge URL (javascript:, data:) is never made clickable. Imported, never
// reimplemented — the three private copies this file replaces are being deleted by the wiring.
export function TriageStateChip({
  state,
  reason,
  setVia,
  filed,
}: {
  state: TriageState;
  reason?: DismissReason;
  setVia?: string;
  filed?: FiledRef;
}) {
  switch (state) {
    case "to_triage":
      return <Badge tone="neutral">{TRIAGE_OPEN_LABEL}</Badge>;

    case "filed":
      if (filed && isHttpsUrl(filed.issue_url)) {
        return (
          <a
            href={filed.issue_url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-xs font-medium text-info underline underline-offset-2 hover:text-info"
          >
            {filedLabel(filed.issue_iid)} <ExternalLinkIcon />
          </a>
        );
      }
      // A settled coordinate with no linkable URL still states the fact, as inert text.
      return <Badge tone="info">{filed ? filedLabel(filed.issue_iid) : "Filed"}</Badge>;

    case "done":
      // The ✓ is decorative — aria-hidden so a screen reader reads "Done", not "check mark
      // Done". "Done via #N" (title explaining the automatic close) only when the sync did it.
      return (
        <Badge
          tone="ok"
          title={setVia === "issue_close" ? "Marked done automatically when the filed issue was closed" : undefined}
        >
          <span aria-hidden="true">✓</span> {doneLabel(setVia, filed?.issue_iid)}
        </Badge>
      );

    case "dismissed":
      // not_an_issue (a false positive) reserves the only warm/red chip; wont_do and the
      // system's barred-CLI auto-dismissal read the quiet neutral grey (a valid-but-parked or
      // policy call is not a warning).
      return (
        <Badge
          tone={reason === "not_an_issue" ? "danger" : "neutral"}
          title={
            reason === "barred_cli"
              ? "Automatically dismissed: this recommended a credential-bearing CLI that policy permanently bars (e.g. glab, gh, aws)"
              : undefined
          }
        >
          {dismissedLabel(reason)}
        </Badge>
      );
  }
}
