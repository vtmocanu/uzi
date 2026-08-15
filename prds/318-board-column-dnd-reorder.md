# PRD #318: Board columns — grip-handle drag-and-drop reorder (replace ↑/↓ arrows)

**GitLab Issue**: [vtmocanu/uzi#318](https://gitlab.example.com/vtmocanu/uzi/-/issues/318)
**Status**: Draft — created 2026-08-15. Design analysis by a `ux-designer` agent (recommendation was a *hybrid* keeping the arrows; the owner overrode it to **drop the arrows entirely**, accepting the a11y residual below). An interactive mock was built and reviewed in-browser before this PRD (Claude artifact "Column Grip"; illustrative only — the full interaction is specified in text here, since a uzi worker cannot fetch it).
**Priority**: Medium
**Depends on**: nothing new. Reuses the board cards' existing native-HTML5 DnD pattern already in `web/src/pages/Board.tsx`.
**Related**: the board-card drag-and-drop reorder in the same file (the pattern this milestone copies); PRD #304 (board search + paged lanes, done) which last touched this area.

## Problem

The board Settings → **COLUMNS** editor (`ColumnSettings` in `web/src/pages/Board.tsx`)
reorders columns with two per-row **↑/↓ arrow buttons** plus **Remove**. Two costs:

1. **Slow for multi-position moves** — moving a column three places is three clicks,
   each re-rendering and shifting focus.
2. **Inconsistent with the rest of the board** — the board *cards* already have a
   polished pointer drag-and-drop reorder (dragged element dims, an inset brand-colored
   shadow marks the insertion point with no mid-drag reflow). The column editor is the
   one reorder surface that does not use it.

## Solution Overview

Replace the ↑/↓ arrows on each column row with a **vertical 6-dot grip handle** on the
row's left edge and **native HTML5 drag-and-drop** reorder, reusing the cards' existing
idiom verbatim (payload via `dataTransfer.setData("text/plain", …)`, `onDragOver`
top/bottom-half edge detection, drop through one order-computing function, dragged row at
`opacity-40`, insertion via `shadow-[inset_0_±2px_0_0_rgb(var(--brand))]`). **No new
dependency** — `web/package.json` has no DnD library and the cards are hand-rolled; this
matches them.

Per owner decision the arrows are **removed, not retained**. This is a deliberate
accessibility tradeoff (see Accepted residuals): native HTML5 drag has no keyboard or
touch initiation path, so removing the arrows removes the only keyboard/screen-reader/touch
reorder affordance. It is owned by the owner and recorded, not smuggled in.

The column-order data path is untouched: reorder mutates local component state only;
nothing persists until **Save columns** (`api.configureColumns`).

## Design Decisions

1. **Reuse the cards' native HTML5 DnD; no library.** The cards use `draggable` +
   `onDragStart`/`onDragOver`/`onDrop`/`onDragEnd`, payload via
   `e.dataTransfer.setData("text/plain", …)` / `getData("text/plain")`
   (`Board.tsx:1419-1423`, `:1286`), insertion as an inset brand shadow so the list never
   reflows mid-drag (`:1777-1778`), dragged element at `opacity-40` (`:1772`), and inner
   interactive elements marked `draggable={false}` so they don't hijack the drag. The
   column editor is a ≤~8-row, single-container, vertical-only list; a DnD library
   (dnd-kit etc.) would add a second DnD system next to the hand-rolled one for no gain.
   **Copy the card classes verbatim** so the two surfaces stay visually identical.
2. **One order-computing function drives the drop.** Mirror the cards' single-order-path
   doctrine (`applyDrop`, `Board.tsx:882-884`: "THE single order-computing path … if a
   reviewer finds a second one, that is the defect"). Introduce a `moveTo(from, to)` over
   `names` and route the drop through it. The existing `swap(i, j)` (`Board.tsx:2288`) is
   removed with the arrows.
3. **Grip icon, not a hamburger.** The reorder affordance is the **vertical 6-dot grip**
   (⠿, 2×3), the convention in GitHub Projects / Notion / Linear. A hamburger (☰)
   conventionally means "menu" and invites a wrong click. No grip icon exists in
   `web/src/components/icons.tsx` yet — add a `GripVerticalIcon` there in the existing icon
   style (see Technical Design). Treatment: left edge of the row, `text-faint`,
   `hover:text-fg`, `cursor-grab active:cursor-grabbing` (the exact cursor pair the cards
   use, `Board.tsx:1771`).
4. **The handle is a visual affordance, not a control.** `aria-hidden="true"`, not
   focusable, no ARIA role. Do **not** use `aria-grabbed` (deprecated in ARIA 1.1+). The
   whole row is the drag source (`draggable` on the `<li>`); the grip communicates
   *that it drags*. Keep **Remove** as a real button, marked `draggable={false}` so it
   doesn't start a drag.
5. **Positional accent dots are expected to recolor after a drop.** The colored dot uses
   `COLUMN_ACCENTS[i % COLUMN_ACCENTS.length]` (`Board.tsx:79` =
   `["bg-info","bg-brand","bg-warn","bg-ok","bg-danger"]`), i.e. color follows **position,
   not identity**. So after a reorder the dots recolor — this is the existing behavior,
   preserved, and is correct, not a bug. Do not switch the dot to a per-label color as part
   of this change.
6. **Reorder stays local until Save.** `moveTo` only calls `setNames`; persistence remains
   `api.configureColumns` on **Save columns** (`ColumnSettings.save`). No API, DTO, or
   backend change — this is entirely within `ColumnSettings`.

## Technical Design

All changes are in **`web/` only**. No api/, controller/, agent/, migration, DTO, or route
change.

### `web/src/components/icons.tsx`

- Add `GripVerticalIcon` matching the existing icon components (same `props`/`className`
  pattern, `currentColor`, `aria-hidden` handled by the caller). A standard 6-dot grip:
  two columns × three rows of small circles (or the Lucide `grip-vertical` path), sized to
  the other icons in the file.

### `web/src/pages/Board.tsx` — `ColumnSettings` (~line 2251)

- **Remove** the two ↑/↓ `Button`s (`aria-label="Move … up/down"`, `Board.tsx:2326-2338`)
  and the `swap(i, j)` helper (`:2288`).
- **Add reorder state**: `dragName: string | null` and
  `insertion: { name: string; edge: "top" | "bottom" } | null` (drive visuals from state,
  not from reading `e.dataTransfer` in `onDragOver` — the drag data store is protected
  during dragover, exactly as the cards' comment at `Board.tsx:291-292` notes).
- **Add `moveTo(from: number, to: number)`** over `names` (array move, then `setNames`);
  route the drop through it (Decision 2).
- **Make each `<li>` the drag source**:
  - `draggable`, `onDragStart` → set `dragName`, and call
    `e.dataTransfer.setData("text/plain", name)` (**required** — Firefox will not start a
    drag without it), `e.dataTransfer.effectAllowed = "move"`.
  - `onDragOver` → `e.preventDefault()`; compute the top/bottom edge with the existing
    **`insertionEdgeFor(top, height, clientY)`** helper (`web/src/lib/boardOrder.ts:213`,
    already imported into `Board.tsx:50` and unit-tested — do **not** re-implement the math
    inline; it is the single geometry path per Decision 2) from the row's
    `getBoundingClientRect()` and `e.clientY`; set `insertion`. `insertion` drives **only
    the visual marker**, never the drop math (below).
  - `onDrop` → **copy the cards' `onCardDrop` verbatim** (`Board.tsx:1388-1393`):
    `e.preventDefault()`, then **recompute** the edge at drop time via `insertionEdgeFor`
    from the target row's rect + `e.clientY` (do not read `insertion` state — recomputing
    is what lets a `drop` with no preceding `dragOver` resolve, which the M2 test precedent
    relies on); drop on the tail's bottom half appends; call `moveTo`; clear
    `dragName`/`insertion`.
  - `onDragEnd` → clear `dragName`/`insertion`.
  - Dragged row: `opacity-40` when `name === dragName`.
  - Insertion marker: `shadow-[inset_0_2px_0_0_rgb(var(--brand))]` when the insertion edge
    is `top` for this row, `shadow-[inset_0_-2px_0_0_rgb(var(--brand))]` for `bottom` —
    the card classes verbatim (`Board.tsx:1777-1778`).
  - Grip: leading `<span aria-hidden="true" className="… text-faint hover:text-fg
    cursor-grab active:cursor-grabbing">` wrapping `<GripVerticalIcon/>`.
  - **Remove** `<Button>` gets `draggable={false}`.
- `key={name}` remains valid as drag identity: `add()` dedupes and column labels are
  unique.
- **Empty / single-item lists**: the `<li>No columns.</li>` placeholder (`Board.tsx:2343`)
  sits outside `names.map`, so it gets no drag handlers — leave it as is. A single-column
  list has nothing to reorder; the grip renders but a self-drop is a no-op (`moveTo(i, i)`
  returns unchanged) — no special-casing needed.

### Tests — `web/src/pages/Board.test.tsx`

- Precedent exists in this file: `fireEvent.dragStart(…, { dataTransfer: { setData: vi.fn(),
  getData: () => "…" } })` and `fireEvent.drop(…, { dataTransfer: { getData: () => "…" },
  clientY: 0 })` (`Board.test.tsx:784`, `:558`, `:663`). Reuse that hand-forged
  `dataTransfer` stub.
- **jsdom limitation to design around** (already documented at `Board.test.tsx:406`):
  `getBoundingClientRect()` returns zeroes in jsdom, so top/bottom-half detection via
  `clientY` vs rect cannot be meaningfully exercised there. jsdom's zero rect makes
  `clientY: 0` resolve deterministically to the "bottom" edge (`0 < 0` is false — the pinned
  boundary in `insertionEdgeFor`), which is exactly why the card tests pass with a bare
  `drop` and no `dragOver`. Because `onDrop` recomputes the edge at drop (above), the same
  precedent transfers: test the **outcome** — after a simulated `dragStart` on row A and
  `drop` on row C the `names` order changes as expected and the persisted payload to
  `api.configureColumns` (on Save) reflects it — rather than the pixel edge math.
- **Arrow-absence assertion must be scoped.** The board **cards** carry
  `aria-label="Move issue #N up in <lane>"` (`Board.tsx:1831`), present in the same DOM tree
  regardless of hover, so a loose `queryByLabelText(/Move .* up/)` matches those and either
  throws (multiple matches) or returns a card button. Assert arrow absence **scoped to the
  settings panel** — `within(settingsPanel).queryByLabelText(...)` — and/or anchor the regex
  (`/^Move .+ up$/`; card labels end in "in <lane>", not "up"). Also assert the grip
  renders.

## Milestones

- [ ] **M1 — Grip icon + DnD reorder in `ColumnSettings`**: add `GripVerticalIcon`; replace
  the ↑/↓ arrows and `swap` with the grip handle + native HTML5 drag-and-drop through a
  single `moveTo`; dragged-row dim + inset-shadow insertion marker copied from the cards;
  Remove button kept and `draggable={false}`. Validation: in the app (or mock scenario)
  dragging a column by the grip reorders the list with the insertion line showing and no
  mid-drag reflow; **Save columns** persists the new order.
- [ ] **M2 — Tests + gate green**: `Board.test.tsx` cases for a drag-reorder outcome, grip
  presence, and arrow-absence, using the existing `dataTransfer` stub idiom and asserting
  on order/`configureColumns` payload (not jsdom pixel math). Run `task gate:web` (oxlint,
  typecheck, vitest, knip) to green. Validation: `task gate:web` passes; new tests fail if
  the reorder wiring regresses.
- [ ] **M3 — Docs/specs**: record the interaction in `specs/ai.md` (drop-arrows decision +
  the accepted a11y residual). If a user-facing board doc names the arrow reorder, update
  it; otherwise none. Validation: `specs/ai.md` carries the decision; `web/scripts/check-docs.mjs`
  (via `npm run build`) stays green if any `docs/*.md` was touched.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1 | — | `web/src/components/icons.tsx`, `web/src/pages/Board.tsx` |
| 2 | M2 | M1 | `web/src/pages/Board.test.tsx` |
| 3 | M3 | M1 | `specs/ai.md` (+ a board doc only if one names the arrows) |

Single-component, mostly sequential; M1 is the substance.

## Out of Scope

- Retaining the ↑/↓ arrows or adding any keyboard/touch reorder path (owner chose to drop
  them — see Accepted residuals). A later PRD may reintroduce a keyboard affordance if the
  a11y regression proves painful.
- A DnD library (dnd-kit or similar) — explicitly rejected (Decision 1).
- Changing the column persistence path, the `configureColumns` API, or the positional
  accent-dot coloring (Decision 5).
- Touching the board **cards'** DnD (already shipped; this only borrows its idiom).

## Accepted residuals

- **WCAG 2.1.1 (Keyboard) regression, owner-accepted.** Removing the arrows removes the
  only keyboard-, screen-reader-, and touch-operable reorder path: native HTML5 drag is
  mouse/pointer-only and has no keyboard initiation in any mainstream browser, and HTML5
  drag events do not fire from touch input on mobile browsers. After this change, reordering
  columns requires a mouse. The board *cards* deliberately keep an ↑/↓ keyboard/touch
  fallback for exactly this reason (`Board.tsx:1796-1824`); the column **editor** is
  intentionally diverging from that here at the owner's direction. Recorded in `specs/ai.md`.
  Mitigation if revisited: reintroduce a keyboard/touch affordance (roving-tabindex move
  buttons or a keyboard drag mode) without bringing back the visual arrow clutter.
- **No aria-live announcement.** The editor has none today and this change does not add one;
  with the reorder now mouse-only, an announcement would have no keyboard trigger to fire on.
  Noted so a future keyboard-path PRD adds it alongside.

## Success Criteria

- The column editor reorders by dragging a row's grip handle; the interaction (dim + inset
  insertion line, no reflow) is visually identical to the board cards.
- The ↑/↓ arrow buttons are gone; **Remove** still works and does not start a drag.
- Reordering changes only local state; **Save columns** persists the new order via
  `api.configureColumns`, unchanged.
- `task gate:web` is green with new tests covering reorder outcome, grip presence, and
  arrow absence.
- No new runtime dependency is added.
- The accepted a11y residual is recorded in `specs/ai.md`.
