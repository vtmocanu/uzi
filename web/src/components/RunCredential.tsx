// RunCredential renders WHICH Anthropic credential a run's claim spent (PRD #111
// M1) — the answer to "which account paid for this run?", which the usage totals
// alone could never give.
//
// It keys off the LABEL, not the id, and that is the whole reason it is a component
// rather than three lines inline. The two fields go null independently: the label is
// a snapshot taken at claim time, while the id is a live foreign key that the server
// nulls when the token is deleted. So `id === null` with a label is the normal shape
// of a historical run whose credential has since been removed, and rendering it is
// the point — the run still names the account it billed. Gating on the id would make
// exactly that case disappear.
//
// Renders nothing when there is no label: a run claimed before this landed, and a
// run not yet claimed, have nothing truthful to say and must not show a placeholder.
//
// The label is USER-AUTHORED text and is rendered as plain JSX (React escapes it),
// never through <Markdown> and never interpolated into a URL.
//
// PRD #111 M5 extends this to name the MODE alongside the label — `console-key —
// auto, 62% headroom` vs `console-key — default` — because an auto pick and a
// default fallback can name the same token (D20). M1 has no mode to show yet, so it
// shows the label alone rather than guessing one.

import type { Run } from "../lib/api";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Badge } from "./ui";

export function RunCredential({
  run,
}: {
  run: Pick<Run, "anthropic_secret_id" | "anthropic_secret_label">;
}) {
  const label = run.anthropic_secret_label;
  if (!label) return null;
  // The label is user-authored and reaches a renderer without necessarily having
  // passed the server validator — see lib/sanitizeLabel for the three routes. React
  // escaping does not touch a bidi override.
  const safe = sanitizeLabel(label);
  // web-ux F8: a run whose credential was DELETED is otherwise indistinguishable
  // from one whose credential still exists — the DTO says so (the id is null while
  // the snapshotted label survives) and the chip was ignoring it. Saying "deleted"
  // is the difference between "go look at this token" and "this token is gone".
  const deleted = run.anthropic_secret_id == null;
  return (
    <Badge
      tone="neutral"
      title={
        deleted
          ? "The Anthropic credential this run's claim spent. It has since been deleted; the name is the one recorded when the run was claimed."
          : "The Anthropic credential this run's claim spent"
      }
    >
      token {safe}
      {deleted && " (deleted)"}
    </Badge>
  );
}
