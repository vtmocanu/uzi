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

// Per-lane paging (PRD #304 M1). Pure + total: given a lane's full `total`, how many
// cards it is currently asked to show (`shownCount`), the `cap` (default 10) and
// `page` (50) the caller owns, and whether a search is active, it returns what to
// render and every affordance count Board.tsx needs. The cap is a RENDER-only slice —
// `cardsByColumn` stays full (Decision 2) — so this decides display, never membership.
export interface LanePaging {
  render: number; // cards to actually render = min(total, effective shownCount)
  showMoreBy: number; // size of the next reveal = min(page, remaining); 0 when nothing left
  remaining: number; // total - render (the "N left")
  canCollapse: boolean; // render > baseline: the lane was expanded past its starting size
  nudgeSearch: boolean; // remaining > 2 * page — past two pages of remainder, hint at search
  countLabel: string; // `${render}/${total}` when capped (render < total), else ""
}

// baseline is the lane's STARTING size: `cap` normally, `page` when a search is active —
// a query lifts the cap so a matched lane opens at one page and pages from there
// (Decision 5). shownCount below baseline is treated as baseline, and every input is
// clamped non-negative, so the function is total for any caller state.
export function lanePaging(opts: {
  total: number;
  shownCount: number;
  cap: number;
  page: number;
  searchActive: boolean;
}): LanePaging {
  const total = Math.max(0, opts.total);
  const cap = Math.max(0, opts.cap);
  const page = Math.max(0, opts.page);
  const baseline = opts.searchActive ? page : cap;
  const shown = Math.max(baseline, opts.shownCount);

  const render = Math.min(total, shown);
  const remaining = total - render;
  return {
    render,
    showMoreBy: Math.min(page, remaining),
    remaining,
    canCollapse: render > baseline,
    nudgeSearch: remaining > 2 * page,
    countLabel: render < total ? `${render}/${total}` : "",
  };
}
