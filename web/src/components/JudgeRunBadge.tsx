import { Badge } from "./ui";
import { judgeBadge, type JudgeBadgeable } from "../lib/judgeBadge";

// JudgeRunBadge is the per-run judge pill on the runs list (PRD #98 M4). It renders the
// ONE badge grammar — verdict-first, count appended only when > 0 — and nothing at all
// for an unjudged run, mirroring RunHealthBadge's "absent unless it has something to
// say" behaviour so the row does not grow a column of placeholder pills.
export function JudgeRunBadge({ run }: { run: JudgeBadgeable }) {
  const badge = judgeBadge(run);
  if (!badge) return null;
  return (
    <Badge tone={badge.tone} title={badge.title}>
      {badge.label}
    </Badge>
  );
}
