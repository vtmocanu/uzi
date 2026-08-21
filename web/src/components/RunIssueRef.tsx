import { forgePlatform } from "../lib/forgeNoun";
import { ForgeIssueAnchor } from "./ForgeIssueAnchor";

// runKindLabel maps an issue-less run's kind to a short human label for the chip
// (PRD #411). The three issue-less kinds that reach the runs list are task, ci_fix,
// prompt; others degrade to the raw kind with underscores spaced.
export function runKindLabel(kind: string): string {
  switch (kind) {
    case "ci_fix":
      return "ci fix";
    default:
      // Equivalent to kind.replaceAll("_", " ") — a regex global replace because
      // the web tsconfig's lib target predates String.prototype.replaceAll (ES2021).
      return kind.replace(/_/g, " ");
  }
}

// RunIssueRef renders a run's originating issue reference (PRD #411, Design
// Decision 4). Three-way rule:
//   1. issue_iid == null  → a muted neutral KIND chip (never a link, never a "#").
//   2. valid https issue_web_url → an external anchor to the forge issue.
//   3. else (issue cached-out) → a plain "#<iid>" span.
// The board-card forge anchor (Board.tsx) is the visual model for branch 2.
export function RunIssueRef({
  issueIid,
  issueWebUrl,
  kind,
  forgeType,
  className,
}: {
  issueIid: number | null;
  issueWebUrl: string | null;
  kind: string;
  forgeType: string | null;
  className?: string;
}) {
  // Branch 1: no issue → kind chip.
  if (issueIid == null) {
    return (
      <span
        className={`inline-flex items-center rounded border border-edge bg-raised/60 px-1.5 py-0.5 text-[11px] text-muted${
          className ? ` ${className}` : ""
        }`}
      >
        {runKindLabel(kind)}
      </span>
    );
  }

  // Branches 2 & 3: cached issue → guarded forge link, or plain #iid when no valid https url.
  const label = `Open issue #${issueIid} on ${forgePlatform(forgeType)}`;
  return (
    <ForgeIssueAnchor
      webUrl={issueWebUrl}
      iid={issueIid}
      label={label}
      className={`text-faint transition-colors hover:text-brand${
        className ? ` ${className}` : ""
      }`}
      fallbackClassName={className}
    />
  );
}
