import type { Change } from "diff";
import { splitUnsafeChars } from "../lib/safeText";

// DIFF_CONTEXT is how many unchanged lines flank a changed one in the prompt-body
// diff. Builtin bodies run to hundreds of lines, so rendering every unchanged one
// would bury the change; collapsing them keeps the hunk readable while still
// saying how much was skipped.
const DIFF_CONTEXT = 3;

// NBSP keeps a blank diff line from collapsing to zero height. Written as an
// escape rather than as a literal on purpose: as a raw U+00A0 it is
// indistinguishable from an ASCII space in every editor and terminal, and it
// already cost one reviewer a mutation run whose pattern silently failed to
// match. An invisible character in source is a trap for the next person folding
// this code, not a formatting detail.
const NBSP = "\u00a0";

// LineDiff renders a line-level diff, collapsing long unchanged runs to a
// "N unchanged lines" marker so a one-line edit in a 300-line prompt stays
// findable. The marker states the count rather than hiding it silently — an
// elided run the reader cannot size is indistinguishable from a diff that missed
// something.
export function LineDiff({ parts, tone, addedLabel, removedLabel }: {
  parts: Change[];
  tone: "drift" | "revision";
  addedLabel: string;
  removedLabel: string;
}) {
  const rows: { tone: "added" | "removed" | "same" | "elided"; text: string }[] = [];

  parts.forEach((p, idx) => {
    if (p.value === "") return;
    const lines = p.value.split("\n");
    // split() on a trailing newline yields a final "" that is not a line.
    if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();

    if (p.added || p.removed) {
      for (const line of lines) {
        rows.push({ tone: p.added ? "added" : "removed", text: line });
      }
      return;
    }
    const first = idx === 0;
    const last = idx === parts.length - 1;
    // Keep context on the side that faces a change; a leading or trailing
    // unchanged run only faces one.
    const head = first ? [] : lines.slice(0, DIFF_CONTEXT);
    const tail = last ? [] : lines.slice(-DIFF_CONTEXT);
    if (lines.length <= head.length + tail.length) {
      for (const line of lines) rows.push({ tone: "same", text: line });
      return;
    }
    for (const line of head) rows.push({ tone: "same", text: line });
    rows.push({ tone: "elided", text: `… ${lines.length - head.length - tail.length} unchanged lines …` });
    for (const line of tail) rows.push({ tone: "same", text: line });
  });

  const TONE = {
    // Drift colors mark edits Reset would take away; revision colors mark
    // insertions as ok and deletions as danger.
    added: tone === "drift" ? "bg-danger/15 text-danger" : "bg-ok/15 text-ok",
    removed: tone === "drift" ? "bg-ok/15 text-ok" : "bg-danger/15 text-danger",
    same: "text-muted",
    elided: "text-faint",
  } as const;

  return (
    <>
      {rows.map((r, i) => (
        <span key={i} className={`block ${TONE[r.tone]}`}>
          <span aria-hidden="true">{r.tone === "added" ? "+ " : r.tone === "removed" ? "- " : r.tone === "same" ? "  " : ""}</span>
          {(r.tone === "added" || r.tone === "removed") && (
            <span className="sr-only">{r.tone === "added" ? addedLabel : removedLabel} </span>
          )}
          {splitUnsafeChars(r.text || NBSP).map((p, j) => p.unsafe ? (
            <span key={j} data-hidden-char="" className="bg-danger/15 text-danger">
              <span className="sr-only">hidden character </span>
              U+{(p.text.codePointAt(0) ?? 0).toString(16).toUpperCase().padStart(4, "0")}
            </span>
          ) : p.text)}
        </span>
      ))}
    </>
  );
}
