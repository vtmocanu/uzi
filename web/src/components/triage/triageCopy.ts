// triageCopy is the ONE vocabulary shared by the Judge page, the run-page judge panel and
// the Findings page (PRD #1183 M1). Before this file each surface grew its own words for the
// same triage states — "To triage" / "TO DO" / "to-do" / "To do" for the open state,
// "Issue created." / "Filed." / "Filed #N" for a filed one — and the three could drift. Every
// string a triage chip or a dismiss menu shows now comes from here, so a copy change is a
// one-file edit and the surfaces cannot disagree.
//
// It is PURE (no JSX, no React): label helpers map the closed triage enum to fixed UI copy,
// and the two adapters normalise the two different wire shapes (the judge occurrence/disposition
// and the finding) onto one `TriageState`. The untrusted free text a page renders
// (rationale_preview, run_title, target, location) is NOT touched here — it is stripped with
// lib/safeText at each render site, exactly as before (issue #124).

import type { Disposition, JudgeAdminOccurrence, JudgeOccurrence } from "../../lib/api";

// TriageState is the NORMALISED open→settled ladder, the same four rungs on every surface.
// The two wires key the open rung differently — the judge ships `todo`, findings ship
// `to_file` (workersvc/findings_backlog.go) — so the chip never sees a wire bucket: the
// adapters below collapse both onto `to_triage`.
export type TriageState = "to_triage" | "filed" | "done" | "dismissed";

// A dismissed row's reason. `wont_do` / `not_an_issue` come from a human (the judge's
// Disposition, the finding's dismiss_reason); `barred_cli` is the judge's system
// auto-dismissal of a recommendation naming a credential-bearing CLI that policy bars
// (set_via "denied_cli"). Findings never carry `barred_cli`.
export type DismissReason = "wont_do" | "not_an_issue" | "barred_cli";

// FiledRef is the minimum a filed/done chip needs to render its link and its "#N". A wider
// wire ref (JudgeFiledIssueRef carries filed_at too) is structurally assignable to it.
export interface FiledRef {
  issue_iid: number;
  issue_url: string;
}

// TriageStateView is what an adapter returns and what TriageStateChip takes: the normalised
// state plus whichever of reason / set_via / filed the row actually carries.
export interface TriageStateView {
  state: TriageState;
  reason?: DismissReason;
  setVia?: string;
  filed?: FiledRef;
}

// The open-state chip copy, one word for every surface (PRD #1183: "To triage" on the tab,
// the strip, the bridge line and the chip).
export const TRIAGE_OPEN_LABEL = "To triage";

// filedLabel is the chip's text for a filed coordinate; the "↗" the mock shows is the
// ExternalLinkIcon the chip appends when the link is a real https URL, not part of this string.
export function filedLabel(iid: number): string {
  return `Filed #${iid}`;
}

// doneLabel splits done by provenance (issue #167, PRD #1184 M4): a person's plain "Done";
// the issue-close sync's "Done via #N" (or the unnamed "Done via issue close" when the filed
// iid was cascaded away); and an admin cross-user Mark done's "Done by an admin via #N" (or
// "Done by an admin" when no filed iid is present). The `admin` provenance renders on ALL three
// surfaces that share this chip — the Judge occurrence, the run-page disposition and Findings —
// because an owner whose coordinate an admin marked done should read that everywhere it shows.
// The "✓" the chip shows is decorative (aria-hidden), not part of this.
export function doneLabel(setVia?: string, iid?: number): string {
  if (setVia === "admin") {
    return iid != null ? `Done by an admin via #${iid}` : "Done by an admin";
  }
  if (setVia === "issue_close") {
    return iid != null ? `Done via #${iid}` : "Done via issue close";
  }
  return "Done";
}

// dismissedLabel carries the reason where the row knows one. An unknown reason reads the bare
// "Dismissed" (a hand-dismissed judge OCCURRENCE ships no reason; only its Disposition does).
export function dismissedLabel(reason?: DismissReason): string {
  switch (reason) {
    case "not_an_issue":
      return "Dismissed · Not an issue";
    case "barred_cli":
      return "Dismissed · barred CLI";
    case "wont_do":
      return "Dismissed · Won't do";
    default:
      return "Dismissed";
  }
}

// A single Dismiss ▾ menu item: the reason posted, the item label, and its explanatory
// subline. Both surfaces share the "Won't do" line; only the "Not an issue" subline differs,
// because the false positive is the judge's fault on judge surfaces and the worker's on
// findings.
export interface DismissMenuItem {
  reason: "wont_do" | "not_an_issue";
  label: string;
  subline: string;
}

export function dismissMenuItems(surface: "judge" | "finding"): DismissMenuItem[] {
  return [
    { reason: "wont_do", label: "Won't do", subline: "Valid, but not worth acting on" },
    {
      reason: "not_an_issue",
      label: "Not an issue",
      subline: surface === "finding" ? "False positive, the worker got it wrong" : "False positive, the judge got it wrong",
    },
  ];
}

// judgeState normalises a judge OCCURRENCE (bucket-keyed, per-coordinate) OR a run-page
// DISPOSITION (status + reason) onto one TriageStateView. The two are discriminated by the
// `bucket` field, which only the occurrence carries. The occurrence may be an owner
// JudgeOccurrence OR the attribution-hidden JudgeAdminOccurrence (PRD #1184 M4) — the admin one
// carries no filed_issue, so the filed link is read defensively via `"filed_issue" in input`.
//
// An occurrence carries no wont_do/not_an_issue distinction (only its group's Disposition
// does), so a hand-dismissed occurrence reads the bare "Dismissed"; only a `denied_cli`
// occurrence gets a reason. A Disposition, by contrast, always has the wont_do/not_an_issue
// reason, and an empty reason reads as wont_do — the run page's own precedence.
export function judgeState(input: JudgeOccurrence | JudgeAdminOccurrence | Disposition): TriageStateView {
  if ("bucket" in input) {
    const filed = "filed_issue" in input ? input.filed_issue : undefined;
    switch (input.bucket) {
      case "filed":
        return { state: "filed", filed };
      case "done":
        return { state: "done", setVia: input.set_via, filed };
      case "dismissed":
        return input.set_via === "denied_cli"
          ? { state: "dismissed", reason: "barred_cli" }
          : { state: "dismissed" };
      default:
        return { state: "to_triage" };
    }
  }
  // A run-page Disposition now carries set_via too (PRD #1184 M4): pass it through so the chip
  // renders "Done via #N" (issue_close) or "Done by an admin" (admin) on the run page, matching
  // the Judge occurrence chip. Absent set_via reads the plain "Done" as before.
  if (input.status === "done") return { state: "done", setVia: input.set_via };
  return { state: "dismissed", reason: input.reason === "not_an_issue" ? "not_an_issue" : "wont_do" };
}

// FindingStateInput is the structural subset of a finding row this adapter reads. It is
// declared here rather than importing the evolving IncidentalFinding DTO so the adapter stays
// type-sound while Child B adds `dismiss_reason` / `set_via` / the `done` status to that wire;
// a real finding row (present and future) is assignable to it.
export interface FindingStateInput {
  // The finding's open state is `to_file`, not the judge's `todo`; a `done` finding is one
  // whose filed issue closed on the forge (there is no human "done" on a finding).
  status: string;
  dismiss_reason?: "wont_do" | "not_an_issue";
  set_via?: string;
  filed_issue_iid?: number;
  filed_issue_url?: string;
}

// findingState normalises one finding row. `done` is always the issue-close sync's doing, so
// it reports set_via `issue_close`; a filed/done row's link comes from filed_issue_iid/url.
export function findingState(f: FindingStateInput): TriageStateView {
  const filed = f.filed_issue_iid != null ? { issue_iid: f.filed_issue_iid, issue_url: f.filed_issue_url ?? "" } : undefined;
  switch (f.status) {
    case "filed":
      return { state: "filed", filed };
    case "done":
      return { state: "done", setVia: f.set_via ?? "issue_close", filed };
    case "dismissed":
      return { state: "dismissed", reason: f.dismiss_reason };
    default:
      // `to_file` and any transient/unknown state fall to the open rung.
      return { state: "to_triage" };
  }
}
