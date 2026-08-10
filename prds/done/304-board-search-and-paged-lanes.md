# PRD #304: Board search + per-lane show-more paging

**GitLab Issue**: [#304](https://gitlab.example.com/vtmocanu/uzi/-/issues/304)
**Status**: Complete (2026-08-10) — all seven milestones implemented, tested
(1940 web tests green), and documented in `docs/board.md`.
**Priority**: Medium
**Labels**: `PRD`, `Night` (nightly 02:00 sweep)
**Related**: PRD #102 (the kanban board and its render/freeze split — `payloadCards` vs `renderCards`, Decision 7b — which this PRD must not break). PRD #196 (board membership `primary ∪ extras` and the render-time `visibleCards` filter this search filters *after*). The `hideEmpty` and `sortMode` view preferences (`web/src/pages/Board.tsx`) are the persistence pattern the new per-lane default copies.

## Problem

Every board lane renders **all** of its cards. `cardsByColumn` (`Board.tsx`) maps
the fully-filtered `renderCards` into lanes and the lane `.map` emits one
`IssueCard` per entry with no bound. On a busy repo a Backlog or Triage lane holds
hundreds of open issues, so:

- The board is an unscannable wall — the working lanes (Planning, In Progress,
  Human Review) are pushed below a tall Backlog, and vertical scanning replaces
  the at-a-glance read a board is for.
- There is **no way to find a specific issue** except eyeballing every card.
  Board membership is already a render-time filter (`visibleCards`, PRD #196), but
  there is no free-text narrowing on top of it.

A design session (mocked and reviewed in-browser) settled the shape: cap each lane
and reveal more in pages, plus a live search that filters across lanes.

## Solution Overview

Two client-side, render-only additions to `Board.tsx`, plus a sticky toolbar. **No
API, DTO, or DB change** — the payload already carries every card (PRD #102/#196
Decision 12), so both features filter the *render* path exactly like `visibleCards`
does, and are instant rather than a poll-cycle away.

1. **Per-lane cap + paged reveal.** Each lane shows a configurable default (**10**)
   cards, then a `Show N more · N left` expander that reveals **one page (up to 50)
   at a time** — never the whole pile into the DOM. An expanded lane scrolls
   internally (bounded height) so it cannot grow the page to hundreds of rows tall; a
   quiet `Collapse` returns it to the default. Past two pages of remainder, a nudge
   points at search instead of a "show all" that would rebuild the wall.

2. **Board search.** A live, client-side filter over the already-loaded cards
   (title, `#iid`, label), matched case-insensitively across every lane, with the
   matched substring highlighted. While a query is active the per-lane caps lift
   (results start at a page and page from there), empty lanes drop out, and capped
   lanes show an `N/M` count.

3. **Sticky toolbar.** The search field and board controls pin to the top of the
   board's scroll region so search stays reachable while scrolling a long lane.

**The load-bearing constraint (PRD #102 Decision 7b):** both features filter and
slice **`renderCards` only**. `payloadCards` — which drives the drag/keyboard
reorder freeze — stays **unfiltered**. Filtering the freeze set would NULL the
board position of every card a search or cap hid. This is the exact trap the
existing `Board.tsx` comments name; the new code sits on the render side of that
line.

## Milestones

- [x] **M1 — Per-lane cap + paged `Show more`.** A pure, unit-tested helper (the
  `boardColumns.ts`/`runBadge.ts` discipline: logic out of the DOM) computes, per
  lane, how many cards to render given `(matchCount, shownCount, cap, page)` and
  whether a `Show more` / `Collapse` affordance is offered and with what counts.
  `Board.tsx` renders `cap` (default 10) cards then a `Show N more · N left` expander
  (a page is 50) that advances `shownCount` by `page`; an expanded lane gets a
  bounded `max-height` internal scroll; a `Collapse` resets to `cap`. Past two pages
  of remainder the lane shows a *search-instead* nudge rather than a "show all".
  **`cardsByColumn` stays FULL — the cap is a render-only slice**, so the reorder
  anchors (`moveCard`, `canMoveUp/Down`, the append-to-end lane drop) still see the
  whole lane; a card moved or dropped **past the cap auto-reveals** its lane (bumps
  `shownCount` to include it) so it can never land in the hidden window and vanish.
  The cap applies to **every** lane including `Closed` (Decision 8); the live
  drag-reveal path is unaffected.

- [x] **M2 — Board search (render-only filter).** A pure predicate
  `matchesQuery(card, q)` over title, `#iid`, and labels (case-insensitive), applied
  to `renderCards` **after** `visibleCards` (membership) and **before** the
  `cardsByColumn` memo — which is where `sortCards` runs (`Board.tsx:762`), so search
  narrows the set the memo then sorts and slices (Decision 6 names the full seam).
  Matched substring highlighted in **both** the card title and any matching label
  chip (threaded through `chipLabels`/`hoistLabels`/`boundedChips`). While a query is
  active: caps lift (start at one page, still paged); **empty lanes drop — the query
  acts as `hideEmpty` for its duration, composing with `visibleColumns` while a live
  drag still reveals lanes** (`boardColumns.ts` `dragActive`); capped lanes show
  `N/M`; and a board-level result count renders. `payloadCards` is untouched (freeze
  safety).

- [x] **M3 — Sticky toolbar.** The search field and existing controls (Sort, Issues,
  Hide empty, Create, Columns, Refresh) — which live in `PageHeader`'s `actions`
  **inside `Board.tsx`** (`:928`), so this milestone edits that file — pin to the top
  while a long lane scrolls. **Verify plain `sticky top-0` first**: the app shell
  scrolls the *window* with no `overflow` ancestor on `<main>` (`AppShell.tsx`), so
  plain sticky is expected to work and a board-local scroll region is the invasive
  fallback (it also interacts with the lanes' own `overflow-x-auto`). On mobile the
  shell already has a sticky `z-20` top bar, so the new bar needs a top offset and a
  z-order below it. Confirm the pin holds in a browser before this milestone is done.

- [x] **M4 — Per-lane default preference.** A `Per lane` control persists to
  `prefs` as `uzi.board.${repoId}.perLane` (default **10**), mirroring `hideEmpty`
  and `sortMode` exactly — including the `useEffect` re-read on `repoId` change,
  because the route swaps `:id` without remounting (the trap those two document and
  that has been fixed in this file once already). Changing it re-baselines every
  lane's `shownCount`. A hand-edited or missing value falls back to 10.

- [x] **M5 — Accessibility & keyboard.** `/` focuses search (when not already in an
  input), `Esc` clears and blurs. Search input labeled; `Show more`/`Collapse`
  buttons carry counts in their accessible names. The board-level result count is
  announced through the **existing** always-mounted `sr-only role=status` live region
  (the S5 region in `Board.tsx`), not a new one — but **debounced and
  last-writer-wins**, because that region is the single-string channel `pushToast`
  also feeds (`Board.tsx:339`), so a per-keystroke count would spam assistive tech
  and race the auto-move toasts. Highlight uses a real `<mark>` (or token styling),
  never color alone. Reduced-motion respected on the expander chevron.

- [x] **M6 — Component tests.** The paging helper and `matchesQuery` predicate are
  unit-tested in M1/M2; this milestone is the `Board.test.tsx` component layer:
  show-more reveals a page and Collapse resets; search filters, highlights (title and
  chip), drops empty lanes, and shows `N/M`; the per-lane pref persists and re-reads
  on repo change; and **the reorder freeze still uses the unfiltered payload while a
  CAP or a search hides cards** — the Decision 7b guard, extended to the cap because
  the cap hides cards from the render set exactly as search does (there is unit
  precedent in `boardOrder.test.ts`, and `Board.test.tsx` already asserts on
  `reorderBoard` calls). Follow the negative-assertion discipline
  (`.claude/rules/web.md`) for any copy the tests assert absent.

- [x] **M7 — Docs.** `docs/board.md` (audience: user) documents search, the
  per-lane cap + `Show more`, and the per-lane default; the board's own on-screen
  description copy is updated if the controls change how a user should read it. Grep
  the old description string across the tree per the copy-change rule.

## Decision Log

1. **Filter at render, never at fetch.** Both features narrow `renderCards`, the
   same place `visibleCards` filters (PRD #196 Decision 12). The payload already
   carries every card, so search/cap are instant view state, not sync settings with
   a poll-cycle delay, and need no server change.

2. **Freeze safety is the first rule, not a footnote.** Search and cap filter and
   slice `renderCards` only; `cardsByColumn` stays full and the cap is a render-only
   slice over it. `payloadCards` stays unfiltered because it drives the reorder
   freeze (PRD #102 Decision 7b) — filtering it would NULL the position of every
   hidden card. M6 asserts this for both the cap and search hiding cards.

3. **Paged reveal + internal scroll, not "show all".** The expander advances by a
   fixed page (50) and an expanded lane scrolls within a bounded height. A single
   "show all" on a 900-deep lane would rebuild the exact wall this PRD removes and
   land hundreds of nodes in the DOM at once.

4. **Per-lane default is a per-browser, per-repo *view* preference — default 10.**
   It is density, not policy, so it lives in `prefs` beside `hideEmpty`/`sortMode`,
   not in admin/server config. 10 was chosen in the design session: almost every
   real working lane fits in full, so the cap only bites on Backlog/Triage — the
   lanes it is for. (5 = uniform-short; 20 = more before the first click; both
   offered by the control, 10 ships as the default.)

5. **Search lifts the caps rather than fighting them.** A query starts a lane at a
   page and pages from there, so a 400-match search never dumps 400 cards either.
   Caps and search compose through one `shownCount` model, not two competing ones.

6. **One ordered pipeline: membership → search → sort → cap.** `visibleCards`
   (membership) and the search predicate narrow `renderCards`; then the
   `cardsByColumn` memo (`Board.tsx:762`) runs `sortCards` and the per-lane cap
   slices the ordered result. Search changes *which* cards a lane holds, sort orders
   them, the cap slices — the sort and the slice both live inside that memo, which is
   the real seam (an earlier draft mis-placed the sort between `renderCards` and
   grouping). Keeping the order fixed is what makes all three predictable together.

7. **Virtualization is deferred, not designed out.** Paged reveal + internal scroll
   covers expected lane sizes without a windowing dependency. If a lane routinely
   sits expanded past a few hundred cards, revisit with windowed rendering
   (react-window or an IntersectionObserver sentinel) behind the same `shownCount`
   affordance — an implementation swap, not a UX change. Recorded as a risk below.

8. **The cap applies to every lane, including `Closed`.** `Closed` accumulates
   forever and is typically the deepest lane, so exempting it would rebuild the wall
   this PRD removes; it is also the safest lane to cap — not a drop target and
   outside the freeze. It gets the same default + `Show more`, ordered by iid like
   today (the Closed-lane sort at `Board.tsx:781`).

9. **Search is scoped to board members — a known, documented surprise.** Search
   filters `renderCards`, so a payload card excluded by membership (`visibleCards`)
   is unfindable: searching an existing `#iid` that is not a board member returns
   nothing. This is deliberate — search narrows what is on the board, it does not
   widen membership — but it reads as a bug, so `docs/board.md` states it. The
   existing "Issues" control is the widen-membership path.

## Risks & Mitigations

- **Sticky bar clipped by an `overflow` ancestor.** The mock scrolls the window;
  the app shell may put the board inside a scroll container that clips `position:
  sticky`. Mitigation: M3 verifies the pin in a real browser and, if needed, gives
  the board its own scroll region rather than assuming window scroll.

- **DOM accretion on repeated `Show more` in a huge lane.** Even paged, clicking
  through a 900-lane accumulates nodes. Mitigation now: internal scroll bounds the
  visible height; Decision 7 keeps windowing as a clean later swap if measured.

- **Freeze corruption (the primary risk).** Any code path that reaches for the
  nearest cards array instead of `payloadCards` for the reorder freeze would silently
  break drag/keyboard ordering for hidden cards. Mitigation: Decision 2 + the M6
  guard test; the `Board.tsx` comments already name this trap.

- **Copy-change / vacuous negative assertions** (`.claude/rules/web.md`). Editing
  the board description or adding search copy can strand `queryByText(...).toBeNull()`
  assertions on retired strings. Mitigation: grep the old string across the test tree
  and repoint each negative assertion; M7 calls this out.

- **Mock mode validates rendering, not data population** (`.claude/rules/web.md`).
  A `VITE_UZI_MOCK=1` browser pass confirms layout/search/paging interaction but not
  data-class behavior. That is fine here — this feature is purely presentational —
  but findings from the mock are rendering findings by construction.

- **Search cost per keystroke.** Filtering a few hundred cards on each keystroke is
  trivial; if a pathological payload makes it visible, debounce the query. Not
  expected to be needed.

## Success Criteria

1. A lane with more than the per-lane default (including `Closed`) shows exactly the
   default, then `Show N more · N left`; clicking reveals up to a page (50) more; an
   expanded lane scrolls internally rather than growing the page; `Collapse` returns
   it to the default; a card moved or dropped past the cap auto-reveals rather than
   vanishing.
2. Typing in search filters every lane to cards matching title, `#iid`, or label
   (case-insensitive), highlights the match, shows `N/M` on capped lanes, drops
   empty lanes, and shows a board-level result count; clearing restores the board.
3. The per-lane default persists per repo across reloads; changing it re-baselines
   the lanes; navigating to another repo reads that repo's value (no stale carry).
4. The search field and controls stay pinned to the top while a long lane scrolls,
   verified in the real app (not only the mock).
5. `/` focuses search, `Esc` clears; the result count is announced to screen
   readers via the existing live region; `Show more`/`Collapse` are named with their
   counts.
6. Drag and keyboard reorder remain correct while a search or cap hides cards — the
   freeze uses the unfiltered payload (asserted in tests).
7. `docs/board.md` documents search, the per-lane cap + `Show more`, and the
   per-lane default.

## Parallelization Notes

Nearly all milestones edit the one file (`Board.tsx`) plus its sibling lib/tests, so
the graph is mostly sequential. The one clean parallel split is the **pure helpers**:
M1's paging helper (`boardColumns.ts`) and M2's `matchesQuery` (`boardCards.ts`) are
independent files with independent unit tests, built and tested in parallel. Their
`Board.tsx` integration — plus M3's sticky toolbar, which **also** edits `Board.tsx`
(the controls live in `PageHeader` actions) — is one serialized phase, not parallel.
M4/M5/M6/M7 follow the integration.

| Phase | Milestones | Depends on | Touches |
|---|---|---|---|
| 1 (parallel) | M1 helper, M2 predicate | — | `boardColumns.ts`, `boardCards.ts` |
| 2 (integrate, serial) | M1+M2 wired in, M3 sticky | Phase 1 | `Board.tsx` |
| 3 (sequential) | M4 pref, M5 a11y, M6 tests, M7 docs | Phase 2 | `Board.tsx`, `Board.test.tsx`, `docs/board.md` |
