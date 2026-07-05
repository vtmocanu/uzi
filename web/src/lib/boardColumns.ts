// Derived hide-empty-column filtering for the board. Emptiness is recomputed on
// every render from the freshly-polled cardsByColumn (never stored per column),
// so a column that gains a card reappears on the next poll and one that empties
// disappears — there is no unhide event to handle. A live drag reveals every
// hidden empty so it stays a drop target. Pure + unit-tested (the runBadge.ts
// discipline); Board.tsx owns the DOM.

export interface BoardColumn {
  key: string;
  label: string;
  droppable: boolean;
  accent: string;
}

// visibleColumns returns the columns to render. With hideEmpty off, or while a
// drag is active, every column shows. With hideEmpty on and no drag, only
// columns that currently hold at least one card show. `count(key)` yields the
// card count for a column (Board passes `cardsByColumn.get(key)?.length ?? 0`).
// Mirrors the PRD filter: keep col if `!hideEmpty || count > 0 || dragActive`.
export function visibleColumns(
  columns: BoardColumn[],
  count: (key: string) => number,
  hideEmpty: boolean,
  dragActive: boolean,
): BoardColumn[] {
  if (!hideEmpty || dragActive) return columns;
  return columns.filter((col) => count(col.key) > 0);
}
