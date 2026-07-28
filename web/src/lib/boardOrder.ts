// Board sort modes and the whole drop decision (PRD #102 M5), as pure functions.
// No DOM, no React, no network — the runBadge.ts / boardColumns.ts / labelChips.ts
// discipline. Board.tsx owns event wiring, state and JSX; everything a drop DECIDES
// lives here, which is what makes the rules testable without jsdom (which has no
// layout, so the geometry half of a drag is untestable there regardless).

import type { Card } from "./api";
import { stripUnsafeChars } from "./safeText";

// SortMode is the board-wide sort. Per-browser, per repo (Decision 8), because the
// choice of whether to honour an order is a view preference while the order itself is
// durable.
export type SortMode = "manual" | "iid" | "run" | "updated" | "title";

export const SORT_MODES: readonly { value: SortMode; label: string }[] = [
  { value: "manual", label: "Manual" },
  { value: "iid", label: "Issue number" },
  { value: "run", label: "Recent run activity" },
  { value: "updated", label: "Last updated" },
  { value: "title", label: "Title" },
];

// isSortMode guards the persisted preference. A stale or hand-edited localStorage
// value must degrade to Manual rather than reach sortCards and select no comparator.
export function isSortMode(v: unknown): v is SortMode {
  return typeof v === "string" && SORT_MODES.some((m) => m.value === v);
}

// byIID is every mode's tiebreak. Every non-manual comparator MUST fall through to it:
// a comparator that can return 0 leaves the render at the mercy of the engine's sort,
// and "stable relative to an order that itself came from somewhere else" is not a
// property worth depending on. forge_issue_iid is unique per repo, so this is total.
const byIID = (a: Card, b: Card) => a.iid - b.iid;

// timeKey parses an RFC3339 stamp to an instant, or null when there is nothing usable.
// Date.parse and NOT a string compare: Go marshals time.Time with trailing zeros
// TRIMMED from the fractional seconds, so "…T10:00:00Z" and "…T10:00:00.5Z" order
// correctly as instants and incorrectly as strings.
function timeKey(v: string | null | undefined): number | null {
  if (!v) return null;
  const t = Date.parse(v);
  return Number.isNaN(t) ? null : t;
}

// descWithNullsLast orders by a numeric key descending, with "no value" rows last in
// iid order — mirroring the NULLS LAST rule the SQL side uses, so the two halves of
// the feature agree about where a card with nothing to sort on belongs.
function descWithNullsLast(key: (c: Card) => number | null) {
  return (a: Card, b: Card) => {
    const ka = key(a);
    const kb = key(b);
    if (ka === null && kb === null) return byIID(a, b);
    if (ka === null) return 1;
    if (kb === null) return -1;
    if (ka !== kb) return kb - ka;
    return byIID(a, b);
  };
}

// sortCards returns a new array; it never mutates its input.
//
// "manual" is the IDENTITY, and that is a design choice rather than a shortcut. The
// board payload arrives ordered by ListIssuesByRepo's
// `ORDER BY board_position ASC NULLS LAST, forge_issue_iid ASC`
// (api/internal/store/queries/forge.sql), and bucketing by column preserves relative
// order, so "render what the server sent" IS the manual order. That is also why
// board_position never rides cardDTO. The cost is a coupling to a SQL clause in
// another language: it is pinned by TestListIssuesByRepoBoardOrderLiveDB, and if that
// ORDER BY changes, this function is what breaks.
export function sortCards(cards: readonly Card[], mode: SortMode): Card[] {
  const out = [...cards];
  switch (mode) {
    case "manual":
      return out;
    case "iid":
      return out.sort(byIID);
    case "run":
      // latest_run is a POINTER and is null on every card that has never run — on a
      // real board, most of them. Never-run cards go last, in iid order among
      // themselves. Note the key is the newest run's updated_at, not the max across
      // the issue's runs: ListLatestRunsForRepo picks by created_at DESC. Intended.
      return out.sort(descWithNullsLast((c) => timeKey(c.latest_run?.updated_at)));
    case "updated":
      // forge_updated_at is NOT NULL server-side, so the null arm here is defence
      // against a malformed value rather than an expected state — one bad row must
      // sink, not scramble the board.
      return out.sort(descWithNullsLast((c) => timeKey(c.forge_updated_at)));
    case "title":
      // Sort the string that is DISPLAYED. The board renders stripUnsafeChars(title)
      // (issue #124), so sorting the raw one puts a title beginning with a bidi or
      // zero-width character in a visibly wrong place. The locale is EXPLICIT because
      // the default is environment-dependent, which would make an exact-order
      // assertion a latent cross-environment flake.
      return out.sort((a, b) => {
        const cmp = stripUnsafeChars(a.title).localeCompare(stripUnsafeChars(b.title), "en", {
          sensitivity: "base",
          numeric: true,
        });
        return cmp !== 0 ? cmp : byIID(a, b);
      });
  }
}

// DropAnchor is the card a drop landed on and which side of it. `before: true` means
// the dragged card goes above the anchor.
export interface DropAnchor {
  iid: number;
  before: boolean;
}

// columnKeyOf mirrors Board.tsx's columnKeyForCard for the bucketing below. Kept local
// so this module imports nothing from a page component; CLOSED_KEY never appears here
// because callers filter closed cards out before the freeze (Decision 7b).
const columnKeyOf = (c: Card) => c.column;

// boardOrderAfterDrop builds the board-GLOBAL iid list to submit.
//
// `openCards` is the payload set minus closed cards, already in DISPLAYED order.
// `columnKeys` is the board's column keys in render order: ["", ...configured].
// Global rather than per-column because that is what makes a cross-column drag
// well-defined: the card carries a number that still means something in its
// destination, where a per-column sequence would hand it a colliding one.
export function boardOrderAfterDrop(
  openCards: readonly Card[],
  columnKeys: readonly string[],
  dragIid: number,
  destColumnKey: string,
  anchor: DropAnchor | null,
): number[] {
  const buckets = new Map<string, Card[]>();
  for (const key of columnKeys) buckets.set(key, []);
  // A card whose column key is not in columnKeys should be impossible (ResolveColumn
  // only ever yields "" or a configured label), but it must never be DROPPED: a card
  // silently missing from the freeze gets cleared to NULL by ClearBoardOrderExcept and
  // relocates to the bottom of its column, which is a data-losing bug that looks like
  // a render glitch. Unknown keys are kept in their own buckets and appended.
  const unknown: string[] = [];
  for (const c of openCards) {
    const key = columnKeyOf(c);
    let bucket = buckets.get(key);
    if (!bucket) {
      bucket = [];
      buckets.set(key, bucket);
      unknown.push(key);
    }
    bucket.push(c);
  }

  const dragged = openCards.find((c) => c.iid === dragIid);
  // A drop on the dragged card ITSELF is a freeze with no positional change, and it
  // needs this explicit arm rather than falling out of the arithmetic below.
  //
  // The tempting reading is that it handles itself: the card is removed from its
  // bucket first, so the anchor lookup finds nothing and the insert falls through to
  // "end of bucket". That is NOT a no-op — it moves the card to the BOTTOM of its
  // column. It happens to look right only when the card was already last, which is
  // exactly the fixture someone would reach for. Measured while writing the unit test:
  // dragging the FIRST card of a lane onto itself returned [10, 30, …] for an input of
  // [30, 10, …].
  //
  // THIS ARM IS LOAD-BEARING AND FULLY REACHABLE. An earlier version of this comment
  // called it "defence rather than a live path" on the grounds that the UI never draws
  // an insertion edge on the dragged card. The edge is only the INDICATOR: onCardDrop
  // is attached to every card with no self-check, and onCardDragOver calls
  // preventDefault() BEFORE its dragIid === card.iid early return, so the browser
  // permits a drop on the dragged card itself. Measured (review M5-3): dragStart card 1
  // then drop on card 1 submits [1,2,3,4,9] with this arm and [2,3,4,1,9] without it.
  // "Defence, not a live path" is the sentence that gets an arm deleted in six months.
  const selfDrop = dragged != null && anchor != null && anchor.iid === dragIid;
  if (dragged && !selfDrop) {
    const from = buckets.get(columnKeyOf(dragged));
    if (from) {
      const i = from.indexOf(dragged);
      if (i >= 0) from.splice(i, 1);
    }
    const dest = buckets.get(destColumnKey) ?? [];
    if (!buckets.has(destColumnKey)) {
      buckets.set(destColumnKey, dest);
      unknown.push(destColumnKey);
    }
    // Looked up after the removal, so the indices below are the post-removal ones and
    // `before` needs no adjustment for a within-column move.
    //
    // An anchor whose iid is NOT in the destination bucket falls through to
    // `at = dest.length`, appending to the bottom. That is the same shape the selfDrop
    // arm above exists to remove, and it is deliberately kept here because the cases
    // differ: a self-drop expresses "no movement", so relocating the card is wrong,
    // whereas a stale anchor expresses "put it near a card that is no longer there",
    // for which the end of the destination column is the only defensible answer. It IS
    // reachable — the 10s poll can replace `board` between render and drop (review
    // M5-6) — so it is asserted rather than assumed.
    let at = dest.length;
    if (anchor) {
      const ai = dest.findIndex((c) => c.iid === anchor.iid);
      if (ai >= 0) at = ai + (anchor.before ? 0 : 1);
    }
    dest.splice(at, 0, dragged);
  }

  const order: number[] = [];
  for (const key of [...columnKeys, ...unknown]) {
    for (const c of buckets.get(key) ?? []) order.push(c.iid);
  }
  return order;
}

// insertionEdgeFor is the one piece of geometry in the drag path, extracted so it is
// the one piece that IS unit-testable — the handler wrapping it is not, because jsdom
// has no layout and getBoundingClientRect returns zeroes there.
//
// Exactly on the midpoint counts as "bottom": an arbitrary but PINNED choice, because
// the boundary is the only line here where an off-by-one is invisible in use.
export function insertionEdgeFor(rectTop: number, rectHeight: number, clientY: number): "top" | "bottom" {
  return clientY < rectTop + rectHeight / 2 ? "top" : "bottom";
}

// DropIntent is everything handleDrop needs before it may touch the network.
export interface DropIntent {
  // True when the drop crosses into a different column, so a forge label write is
  // needed first. Within-column drags never touch the forge.
  columnChanged: boolean;
  // The board-global order to submit.
  iids: number[];
}

// dropIntent performs the whole decision a drop makes, so Board.tsx's handleDrop
// reduces to two awaits and a setBoard.
//
// THE PAYLOAD-SET RULE (Decision 7b) IS THE SIGNATURE. `payloadCards` is
// board.cards — UNFILTERED — never the rendered or sorted list. "Currently displayed"
// is the wrong scope the moment a card-level render filter exists (M6's non-PRD
// toggle): a viewer with it off would freeze only the cards they can see, leaving
// every hidden card NULL and therefore relocated to the bottom of its column on the
// same user's other browser, where the toggle is on. Positions are computed for every
// non-closed card in the payload, hidden ones included.
//
// Closed cards are excluded and keep a NULL order. Closed is not a drop target, so a
// position there is unreachable by drag — but a frozen one would ride an issue that
// later reopens on the forge and drop it at an arbitrary point in its new column.
export function dropIntent(args: {
  payloadCards: readonly Card[];
  columnKeys: readonly string[];
  sortMode: SortMode;
  dragIid: number;
  destColumnKey: string;
  anchor: DropAnchor | null;
}): DropIntent | null {
  const { payloadCards, columnKeys, sortMode, dragIid, destColumnKey, anchor } = args;
  const dragged = payloadCards.find((c) => c.iid === dragIid);
  // Nothing to do: the card is gone from the payload (evicted between render and drop),
  // or it is closed and therefore not draggable.
  if (!dragged || dragged.closed) return null;

  const openCards = payloadCards.filter((c) => !c.closed);
  const displayed = sortCards(openCards, sortMode);
  return {
    columnChanged: columnKeyOf(dragged) !== destColumnKey,
    iids: boardOrderAfterDrop(displayed, columnKeys, dragIid, destColumnKey, anchor),
  };
}

// neighbourAnchor turns a keyboard ↑/↓ press into the same DropAnchor a drop produces,
// so both gestures enter dropIntent identically. There must be no second
// order-computing path: two paths that agree today and drift later is the failure this
// exists to prevent.
//
// `columnCards` is the cards of one lane in DISPLAYED order. Returns null when the
// card is already at that end of its column (the caller disables the button too, but a
// null here means a stray press cannot produce a bogus freeze).
export function neighbourAnchor(
  columnCards: readonly Card[],
  iid: number,
  direction: "up" | "down",
): DropAnchor | null {
  const i = columnCards.findIndex((c) => c.iid === iid);
  if (i < 0) return null;
  if (direction === "up") {
    if (i === 0) return null;
    // Land above the card currently above this one.
    return { iid: columnCards[i - 1].iid, before: true };
  }
  if (i === columnCards.length - 1) return null;
  // Land below the card currently below this one.
  return { iid: columnCards[i + 1].iid, before: false };
}
