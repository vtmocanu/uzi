# PRD #276: Filter-by-label "Clear" — no layout shift, button-not-link styling

**GitLab Issue**: [#276](https://github.com/vtmocanu/uzi/-/issues/276)
**Status**: ✅ Done — 2026-08-09 (implemented via Option B: always-mounted, visibility-
toggled Clear; three link-styled actions restyled as on-token ghost buttons with
`min-h-[24px]` meeting WCAG 2.5.8; `task gate:web` green; browser-verified no shift in
both themes)
**Priority**: Low

## Problem

On the Judge recommendations page (`/judge`), the "Filter by label" panel
(`LabelFilter` in `web/src/pages/Judge.tsx`, ~lines 589-663, rendered at ~435-440)
has two rough edges the owner hit while filtering:

1. **The panel resizes when you click a filter.** With no filter active the panel
   is shorter; the moment a chip is selected a "✕ Clear" control appears in the
   heading row and the whole `<section>` grows taller (browser-measured **122px →
   126px, +4px**). The entire delta is in the heading row (`~:606`,
   `flex items-center`) going **16px → 20px**. It is *not* a wrap onto a new line —
   the Clear button is `ml-auto` on the same row and fits. A flex row's height is
   its tallest child: inactive, that is the `text-xs` heading span (line-height
   **16px**); active, the conditionally-mounted (`{active.size > 0 && (…)}`,
   `~:608`) `text-sm` Clear button, whose line-height is **20px**. The driver is
   the `text-sm` vs `text-xs` line-height difference alone — **not** the inline
   `<XIcon/>`, which renders at `1em` (~14px) and is not the tallest child. So
   selecting the first filter shifts the layout, which reads as jank.

2. **"Clear" looks like a hyperlink, not an action.** Its className (`~:612`) is
   `text-sm font-medium text-brand underline underline-offset-2
   hover:text-brand-hover` — the underline + brand-color combo is *literally* this
   repo's link style (`.docs-prose a`, `web/src/index.css:227-228`). By the
   "underline = navigation" convention users read "Clear" as a link, not a
   state-clearing button. **The same link-styled `<button>` pattern recurs at
   three sites on this page**, all in `Judge.tsx`:
   - `~:415` — the run-anchor "Clear filter" button
   - `~:612` — the `LabelFilter` "Clear" button (Problem 1's control)
   - `~:1069` — the `UndoToast` "Undo" button

   Two nearby occurrences are correctly *not* in scope: `MultiSelectBar`'s "Clear"
   (`~:1043`) is already a proper `<Button variant="ghost">`, and the "Filed #N"
   link (`~:937`) is a genuine `<a href>` external link where the underline is the
   right treatment.

Both problems are cosmetic, low-risk, and scoped to a single component/page —
`LabelFilter` is defined inline in `Judge.tsx` (not exported, no other importer),
so the blast radius is the Judge page only.

## Solution Overview

Small frontend changes in `web/src/pages/Judge.tsx` (no API, schema, migration, or
shared-component change):

- **Reserve the heading-row height** so the panel is the same height with and
  without an active filter, so the first chip click causes no shift.
- **Restyle the three link-styled `<button>` actions as subtle on-token buttons**,
  dropping the link underline while keeping their icon + label. Use the repo's
  existing ghost-button treatment (`hover:bg-raised hover:text-fg`) so they read as
  actions, not links.

**Key interaction between the two:** restyling Clear does *not* by itself remove
the shift. The suggested replacement class (Decision 2) is `text-xs … py-0.5` =
16px line-height + 4px vertical padding = **still 20px tall**, so the heading row
still grows 16→20px unless its height is reserved. Decision 1 is therefore
required regardless of the restyle; the two ship together.

## Design Decisions

### Decision 1 — Reserve the heading-row height (recommended: always-mount + toggle visibility)
Make the panel height constant across states. The load-bearing requirement is the
**outcome**: the panel height must not change between inactive and active. Two
viable approaches, both Tailwind-native and on-token:

- **Option B (recommended, decoupled):** keep the Clear button always mounted and
  toggle only its visibility — `invisible pointer-events-none` when inactive, plus
  `aria-hidden={true}` / `tabIndex={-1}` so it is not a hidden focus or
  accessibility target. Layout is then defined by the button itself: self-sizing,
  **no magic number**, and a later target-size bump (Decision 4) "just works". It
  also gives a behaviorally meaningful test (Clear always in the DOM,
  `aria-hidden`/non-focusable when inactive) rather than a class-string check.
- **Option A (alternative, minimal):** pin the heading row to its active height,
  e.g. add a `min-h-*` to `~:606` (`mb-2 flex flex-wrap items-center gap-2`) sized
  to the restyled Clear button, so the inactive panel reserves the same height.
  One-class change, but it introduces a constant that must track the Clear button
  height (and any Decision 4 bump) — the coupling the Risks section calls out.

Option B is preferred because it dissolves that coupling. Either satisfies the
requirement; the implementer may pick A if they prefer the smaller diff and accept
sizing the constant in the same commit.

### Decision 2 — "Clear" is a button, not a link (drop the underline)
Replace the link-style className at `~:612` with a subtle button consistent with
existing patterns in this file (the ghost `hover:bg-raised` treatment). Suggested
class:
`ml-auto inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs
font-medium text-muted transition-colors hover:bg-raised hover:text-fg`.
Keep `<XIcon/> Clear`. This removes the hyperlink read, gives a button-like hover,
and stays entirely on design tokens. Note the resulting control is still ~20px
tall (16px line + `py-0.5`), which is why Decision 1 is still needed.

### Decision 3 — Apply the same styling fix to the other two link-styled actions
For a coherent page, restyle all three link-styled `<button>` actions the same
way (drop underline → on-token button):
- the run-anchor "Clear filter" (`~:412-418`, className `~:415`) — not
  conditionally mounted (inside `{runAnchor && …}`), so styling only, no
  layout-shift component;
- the `UndoToast` "Undo" (`~:1069`) — a toast action; underline-as-link is
  especially misleading for an Undo, so include it.

Explicitly out of scope, confirmed by the audit: `MultiSelectBar`'s "Clear"
(`~:1043`) is already a proper `<Button variant="ghost">`, and "Filed #N"
(`~:937`) is a real `<a href>` where the underline is correct.

### Decision 4 — Address the target-size a11y gap while we are here (should-fix)
The current Clear target is ~20px tall, below **WCAG 2.5.8 Target Size (Minimum),
Level AA** (24×24 CSS px, WCAG 2.2). (The AAA enhanced criterion is 2.5.5 at 44px;
not the bar here.) 2.5.8 also has a **spacing exception** — a target under 24px can
still conform if it has sufficient surrounding spacing — so the current control may
already pass; verify rather than assume a hard failure. Either way, the padded
button from Decision 2 helps; if the implementer bumps the target to ≥24px, Option
B self-sizes to it and Option A's reserved height must be sized to match. If
deferred, record why in the MR.

## Milestones

- [x] **M1 — Stable panel height.** The `/judge` "Filter by label" panel renders
  at the same height with and without an active filter (no shift on first chip
  click), via Decision 1. Test (Option B, the meaningful one): the Clear button is
  always in the DOM and is `aria-hidden`/non-focusable when inactive. (Option A's
  "row carries the reserved-height class" is a near-tautological change-detector —
  another reason to prefer B.) The pixel-height equality itself is validated in the
  browser (M3): jsdom does no layout, so a `getBoundingClientRect` height assertion
  there would be vacuous.
- [x] **M2 — Button-not-link styling.** The three link-styled actions
  (`LabelFilter` Clear, run-anchor "Clear filter", `UndoToast` "Undo") render as
  on-token buttons with no persistent underline (Decisions 2, 3), keeping their
  icon + label; target size addressed or deferral recorded (Decision 4). Existing
  tests select these by accessible name (`Judge.test.tsx` `/Clear/`, `/Clear
  filter/`), which is preserved, so they should stay green. Note a bare "class no
  longer contains `underline`" assertion is a brittle unpaired-negative (per
  `.claude/rules/web.md`) — the real gate is the M3 browser pass. Gate:
  `task gate:web`.
- [x] **M3 — Gate + manual browser check.** `task gate:web` green; manual pass in
  mock mode (`VITE_UZI_MOCK=1 npm run dev`, `/judge`) in **both** themes (ember and
  mission) confirming: (a) panel height is identical inactive vs. active, and (b)
  the Clear/Clear-filter/Undo actions read as buttons, not links.

## Success Criteria

1. On `/judge`, the "Filter by label" panel has identical height whether or not a
   filter is active — clicking the first chip causes no vertical shift.
2. The "Clear" control renders as a button (no persistent underline, not the link
   style), retaining the ✕ icon + "Clear" label.
3. The other two link-styled actions on the page (run-anchor "Clear filter",
   `UndoToast` "Undo") get the same non-link treatment; genuine links are untouched.
4. Clear targets meet WCAG 2.5.8 (≥24px) or its spacing exception, or the deferral
   is recorded in the MR.
5. `task gate:web` is green and the change is confined to `web/` (no API/schema
   change).

## Risks & Mitigations

- **Magic-number coupling (Option A only).** A hard-coded reserved height goes
  stale if the Clear button height later changes. Mitigation: prefer Option B
  (self-sizing); if Option A, size the `min-h-*` to the restyled Clear in the same
  commit.
- **Stale inline comment under Option B.** The comment at `Judge.tsx:576-577` says
  "The Clear control appears only when something is selected"; Option B mounts it
  always (just hidden), making that inaccurate. Mitigation: update it in the same
  commit (repo fix-the-doc rule) — see Documentation Impact.
- **Theme regressions.** Tokens differ between ember and mission. Mitigation: M3
  verifies both themes in the browser (the investigation already confirmed the bug
  in both).
- **Accidental scope creep into shared UI.** `LabelFilter` is inline in
  `Judge.tsx`, so there is no shared component to regress; keep the change there.

## Dependencies

- None. Pure frontend change in `web/src/pages/Judge.tsx`. No new services, schema,
  migration, or API/DTO change. Touches the same `LabelFilter` component as PRD
  #244/#270 (chip counts) but is independent of them.

## Documentation Impact

- No `docs/*` page or user-facing copy changes (the change is cosmetic).
- **If Option B is chosen**, the inline comment at `Judge.tsx:576-577` ("The Clear
  control appears only when something is selected …") becomes inaccurate and must
  be corrected in the same commit (repo fix-the-doc rule). Under Option A the
  comment stays accurate.
