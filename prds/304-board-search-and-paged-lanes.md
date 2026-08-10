# PRD #304: Board search + per-lane show-more paging

**GitLab Issue**: [#304](https://gitlab.example.com/vtmocanu/uzi/-/issues/304)
**Status**: Draft (created 2026-08-10)
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
   cards, then a `Show 50 more · N left` expander that reveals **one page (50) at a
   time** — never the whole pile into the DOM. An expanded lane scrolls internally
   (bounded height) so it cannot grow the page to hundreds of rows tall; a quiet
   `Collapse` returns it to the default. Past two pages of remainder, a nudge points
   at search instead of a "show all" that would rebuild the wall.

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

- [ ] **M1 — Per-lane cap + paged `Show more`.** A pure, unit-tested helper (the
  `boardColumns.ts`/`runBadge.ts` discipline: logic out of the DOM) computes, per
  lane, how many cards to render given `(matchCount, shownCount, cap, page)` and
  whether a `Show more` / `Collapse` affordance is offered and with what counts.
  `Board.tsx` renders `cap` (default 10) cards then a `Show 50 more · N left`
  expander that advances `shownCount` by `page`; an expanded lane gets a bounded
  `max-height` internal scroll; a `Collapse` resets to `cap`. The `Closed` lane and
  the drag-reveal path are unaffected.

- [ ] **M2 — Board search (render-only filter).** A pure predicate
  `matchesQuery(card, q)` over title, `#iid`, and labels (case-insensitive),
  applied to `renderCards` **after** `visibleCards`/sort and **before**
  `cardsByColumn`, so search composes with membership and sort. Matched substring
  highlighted in the card title and label chips. While a query is active: caps lift
  (start at one page, still paged), empty lanes drop, capped lanes show `N/M`, and a
  board-level result count renders. `payloadCards` is untouched (freeze safety).

- [ ] **M3 — Sticky toolbar + scroll region.** The search field and existing
  controls (Sort, Issues, Hide empty, Create, Columns, Refresh) pin to the top of
  the board's scroll container. **Verify against the real app shell** — the mock
  scrolls the window; the app renders the board inside its layout, and `position:
  sticky` fails silently under an `overflow` ancestor. The change is likely: give
  the board its own scroll region and pin the bar to it. Confirm the pin holds in a
  browser before this milestone is done.

- [ ] **M4 — Per-lane default preference.** A `Per lane` control persists to
  `prefs` as `uzi.board.${repoId}.perLane` (default **10**), mirroring `hideEmpty`
  and `sortMode` exactly — including the `useEffect` re-read on `repoId` change,
  because the route swaps `:id` without remounting (the trap those two document and
  that has been fixed in this file once already). Changing it re-baselines every
  lane's `shownCount`. A hand-edited or missing value falls back to 10.

- [ ] **M5 — Accessibility & keyboard.** `/` focuses search (when not already in an
  input), `Esc` clears and blurs. Search input labeled; `Show more`/`Collapse`
  buttons carry counts in their accessible names. The board-level result count is
  announced through the **existing** always-mounted `sr-only role=status` live
  region (the S5 region in `Board.tsx`), not a new one. Highlight uses a real
  `<mark>` (or token styling), never color alone. Reduced-motion respected on the
  expander chevron.

- [ ] **M6 — Tests.** Unit: the M1 paging helper (cap/page/collapse edges,
  `Show N more` counts, the "fewer than a page remain" case) and the M2
  `matchesQuery` predicate (title/iid/label hits and misses, case-insensitivity).
  Component (`Board.test.tsx`): show-more reveals a page and Collapse resets; search
  filters, highlights, drops empty lanes, and shows `N/M`; the per-lane pref
  persists and re-reads on repo change; **the reorder freeze still uses the
  unfiltered payload while a search hides cards** (the Decision 7b guard). Follow the
  negative-assertion discipline (`.claude/rules/web.md`) for any copy the tests
  assert absent.

- [ ] **M7 — Docs.** `docs/board.md` (audience: user) documents search, the
  per-lane cap + `Show more`, and the per-lane default; the board's own on-screen
  description copy is updated if the controls change how a user should read it. Grep
  the old description string across the tree per the copy-change rule.

## Decision Log

1. **Filter at render, never at fetch.** Both features narrow `renderCards`, the
   same place `visibleCards` filters (PRD #196 Decision 12). The payload already
   carries every card, so search/cap are instant view state, not sync settings with
   a poll-cycle delay, and need no server change.

2. **Freeze safety is the first rule, not a footnote.** Search and cap filter and
   slice `renderCards` only. `payloadCards` stays unfiltered because it drives the
   reorder freeze (PRD #102 Decision 7b) — filtering it would NULL the position of
   every hidden card. M6 asserts this directly.

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

6. **Sort → filter → cap is one ordered pipeline.** `visibleCards` (membership) →
   search predicate → `sortCards` → per-lane cap slice. Search changes *which* cards
   a lane holds; sort orders them; the cap slices the ordered result. Keeping the
   order fixed is what makes all three predictable together.

7. **Virtualization is deferred, not designed out.** Paged reveal + internal scroll
   covers expected lane sizes without a windowing dependency. If a lane routinely
   sits expanded past a few hundred cards, revisit with windowed rendering
   (react-window or an IntersectionObserver sentinel) behind the same `shownCount`
   affordance — an implementation swap, not a UX change. Recorded as a risk below.

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

1. A lane with more than the per-lane default shows exactly the default, then
   `Show 50 more · N left`; clicking reveals up to 50 more; an expanded lane scrolls
   internally rather than growing the page; `Collapse` returns it to the default.
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
the graph is mostly sequential. The one clean parallel split is **M1 (cap/paging)**
and **M2 (search)**: their pure helpers (`boardColumns.ts` paging helper,
`boardCards.ts` `matchesQuery`) are independent files with independent unit tests
and can be built and tested in parallel, then integrated into `Board.tsx` together.
M3 (sticky) is independent of both. M4/M5/M6/M7 depend on M1+M2 landing in
`Board.tsx` and are sequential after the integration.

| Phase | Milestones | Depends on | Touches |
|---|---|---|---|
| 1 (parallel) | M1 helper, M2 predicate, M3 sticky | — | `boardColumns.ts`, `boardCards.ts`, board layout/CSS |
| 2 (integrate) | M1+M2 wired into `Board.tsx` | Phase 1 | `Board.tsx` |
| 3 (sequential) | M4 pref, M5 a11y, M6 tests, M7 docs | Phase 2 | `Board.tsx`, `Board.test.tsx`, `docs/board.md` |
