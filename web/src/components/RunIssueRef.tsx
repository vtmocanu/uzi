import { forgePlatform } from "../lib/forgeNoun";
import { runKindLabel } from "../lib/runKind";
import { ForgeIssueAnchor } from "./ForgeIssueAnchor";

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
  raised = false,
  tone = "default",
}: {
  issueIid: number | null;
  issueWebUrl: string | null;
  kind: string;
  forgeType: string | null;
  className?: string;
  // raised lifts ONLY the interactive forge anchor (branch 2) above a stretched-link
  // card overlay via `relative z-10` (issue #485 NB1). It is deliberately NOT applied
  // to the kind chip (branch 1) or the cached-out #iid span (branch 3): those are
  // non-interactive, so they stay below the overlay and a click on them navigates to
  // the run rather than landing on a dead zone.
  raised?: boolean;
  // tone controls the ref's own text colour (issue #485 NB2). "default" (everywhere
  // else) forces the muted/faint neutral colours. "inherit" drops those explicit
  // colours so the ref reads the PARENT's colour — used by the Board needs-attention
  // strip, whose pill themes everything `text-warn`; a forced faint/muted number there
  // mismatches the amber pill. In "inherit" the branch-2 anchor also swaps its
  // hover:text-brand (which would break the warn theme) for a warn-safe hover:underline.
  tone?: "default" | "inherit";
}) {
  const inherit = tone === "inherit";
  // Branch 1: no issue → kind chip. Never raised (stays below the card overlay).
  if (issueIid == null) {
    // In "inherit" the chip drops text-muted so it reads the parent's colour (warn in
    // the strip); border/bg/padding/size are unchanged.
    return (
      <span
        className={`inline-flex items-center rounded border border-edge bg-raised/60 px-1.5 py-0.5 text-[11px]${
          inherit ? "" : " text-muted"
        }${className ? ` ${className}` : ""}`}
      >
        {runKindLabel(kind)}
      </span>
    );
  }

  // Branches 2 & 3: cached issue → guarded forge link, or plain #iid when no valid https url.
  const label = `Open issue #${issueIid} on ${forgePlatform(forgeType)}`;
  // Branch 2 colour: default forces text-faint with a hover-to-brand affordance; inherit
  // reads the parent colour and uses a warn-safe hover:underline (no non-warn colour).
  const anchorTone = inherit
    ? "transition-colors hover:underline"
    : "text-faint transition-colors hover:text-brand";
  return (
    <ForgeIssueAnchor
      webUrl={issueWebUrl}
      iid={issueIid}
      label={label}
      className={`${anchorTone}${raised ? " relative z-10" : ""}${
        className ? ` ${className}` : ""
      }`}
      fallbackClassName={className}
    />
  );
}
