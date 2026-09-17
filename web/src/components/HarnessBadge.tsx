// HarnessBadge — the run/schedule harness indicator (PRD #1429 M4a).
//
// Claude stays visually UNMARKED where a badge would add no information: it was
// every run's harness before this milestone, so a badge on every Claude run would be
// noise on every single row. Codex is EXPLICIT — every Codex run/schedule renders a
// small badge so the fact is visible on sight (run header, list rows), not buried in
// a detail sentence a reader has to go looking for.
//
// Modeled on RunCredential.tsx: a small, self-hiding Badge, no link (there is nothing
// to click through to — the harness fact needs no further drill-in).

import type { Harness } from "../lib/api";
import { Badge } from "./ui";

export function HarnessBadge({
  harness,
  title = "This run uses the Codex harness",
}: {
  harness: Harness | null | undefined;
  title?: string;
}) {
  if (harness !== "codex") return null;
  return (
    <Badge tone="info" title={title}>
      Codex
    </Badge>
  );
}
