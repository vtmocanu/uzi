// Which of an issue's labels are worth showing as a chip (PRD #102 M4, Decision 6).
// Pure + unit-tested, the runBadge.ts / boardColumns.ts discipline; the callers own
// the DOM.
//
// Four kinds of label are excluded, and all four are OPERATOR-CONFIGURABLE, so
// every one of them arrives as a parameter and none is hardcoded here:
//   - the PRD label       (settings.KeyPRDLabel)      — board membership, not content
//   - the PRDLESS label   (settings.KeyPRDLESSLabel)  — an escape hatch, shown as a badge
//   - the autopilot label (settings.KeyAutopilotLabel) — a workflow marker
//   - the configured column labels — a card in Planned must not also wear a Planned chip
//
// The column set is a parameter rather than one module-level source because the two
// callers legitimately exclude against DIFFERENT sets: the board's chip renderer
// excludes the SAVED columns (board.columns), while ColumnSettings' suggester excludes
// the columns being EDITED (its local `names` state, which already holds unsaved adds
// and removals). Reaching for one global would make the suggester re-offer a label the
// user just added.

export interface LabelChipExclusions {
  prdLabel: string;
  prdlessLabel: string;
  autopilotLabel: string;
  // Column labels to exclude. The board passes its saved columns; ColumnSettings
  // passes its in-flight edit state.
  columnLabels: readonly string[];
}

// chipLabels returns the labels worth rendering as chips, in input order. Empty
// strings are dropped (a forge cannot produce one, but `column` is "" for the
// implicit Backlog lane and callers pass it in as a column label — an empty
// exclusion entry must never become an empty chip either).
export function chipLabels(
  labels: readonly string[],
  { prdLabel, prdlessLabel, autopilotLabel, columnLabels }: LabelChipExclusions,
): string[] {
  const columns = new Set(columnLabels);
  return labels.filter(
    (l) =>
      l !== "" &&
      l !== prdLabel &&
      l !== prdlessLabel &&
      l !== autopilotLabel &&
      !columns.has(l),
  );
}

// hoistLabels reorders `labels` so every label that also appears in `hoist` comes
// first — in the order they appear in `labels` — followed by the rest in their
// original order. Stable, no dedup, no mutation.
//
// This exists for PRD #196 Decision 11: a card is on the board because it carries a
// membership extra (e.g. `bug`), and that "why this card is here" chip must not be
// pushed into the "+N" overflow by MAX_CARD_CHIPS on a card with five or more
// labels. Hoisting the matched labels ahead of the cap keeps them among the shown
// chips. `chipLabels` still decides WHICH labels chip at all; this only reorders the
// survivors before boundedChips splits them.
export function hoistLabels(labels: readonly string[], hoist: readonly string[]): string[] {
  const wanted = new Set(hoist);
  const front: string[] = [];
  const rest: string[] = [];
  for (const l of labels) {
    if (wanted.has(l)) front.push(l);
    else rest.push(l);
  }
  return [...front, ...rest];
}

// MAX_CARD_CHIPS bounds how many chips a board card renders. A board column is a
// fixed w-72 lane, so an issue carrying a dozen labels would otherwise push the card
// several rows taller than its neighbours and bury the run badges under them. The
// chips wrap rather than overflow, so this caps HEIGHT, not width; per-chip
// truncation caps width.
export const MAX_CARD_CHIPS = 4;

// BoundedChips is what a card renders: the first `max` chips plus how many were
// withheld, so the caller can show a "+N" affordance instead of silently dropping
// labels.
export interface BoundedChips {
  shown: string[];
  overflow: number;
  // The withheld labels, for the "+N" element's title — the information is still
  // reachable on hover rather than lost.
  hidden: string[];
}

// boundedChips splits an already-filtered chip list at `max`. A list exactly at the
// cap shows in full with no "+N" (an overflow of 0 would otherwise read as a bug).
export function boundedChips(labels: readonly string[], max = MAX_CARD_CHIPS): BoundedChips {
  if (max < 0) max = 0;
  if (labels.length <= max) return { shown: [...labels], overflow: 0, hidden: [] };
  return {
    shown: labels.slice(0, max),
    overflow: labels.length - max,
    hidden: labels.slice(max),
  };
}
