# PRD #60: Legible strikethrough on completed checklist steps

**GitLab Issue**: [#60](https://github.com/vtmocanu/uzi/-/issues/60)
**Status**: Complete (created 2026-07-16, shipped 2026-07-27)
**Priority**: Low
**Mock**: https://claude.ai/code/artifact/2347a25a-a83c-48cb-9a0a-a460eff1c2fa
(Option A is the accepted treatment)

## Problem

Completed steps in the dashboard "Get the factory running" checklist draw
their strikethrough in `decoration-edge-strong`
(`web/src/pages/Dashboard.tsx:66`). `--edge-strong` is a border token
(#3A4256 in ember) sitting at ~1.9:1 against the card surface, while the
struck text itself is `text-muted` at ~6.9:1 — so the line is barely
visible and the "crossed off" affordance is lost. The mission theme has
the same defect (`--edge-strong` is 71 85 105 there).

## Solution Overview

Delete the `decoration-edge-strong` class. `text-decoration-color`
defaults to `currentColor`, so the strikethrough inherits the muted text
color (~6.9:1) and stays legible in ember, mission, and any future theme
for free — the line derives from the text color instead of a border
token.

This is the only site with the defect: the other `line-through` usages
(`RunView.tsx:312,357`, `MrChip.tsx:58`) already rely on currentColor.

## Design Decisions

1. **Option A (currentColor) over a softened `decoration-muted/55` or
   dropping the strikethrough** (user choice, 2026-07-16, from the mock's
   three treatments). One-class deletion, theme-portable, keeps the
   skimmable crossed-off affordance.
2. **No new token.** The fix removes a token misuse rather than adding a
   decoration-color slot; browser default behavior is the mechanism.

## Milestones

- [x] **M1 — Fix + verify**: `decoration-edge-strong` removed from
      `Dashboard.tsx:66`; `npm run typecheck` + `npm test` green; visual
      check in both themes matches the mock's Option A.
      - Class dropped, leaving `done ? "text-muted line-through" : "text-fg"`.
        A test in `Dashboard.test.tsx` guards the whole `decoration-` prefix,
        so any decoration utility reintroduced there fails.
      - `npx tsc --noEmit` exit 0; `npm test` 93 files / 1066 tests passed;
        `npm run build` succeeded; `grep -rn decoration-edge-strong web/`
        returns nothing.
      - Visual check done in headless Chromium against
        `VITE_UZI_MOCK=1 npm run dev`, at 3/4 steps done. In both themes the
        struck text's `color` and `text-decoration-color` measure identical —
        ember `rgb(148,158,176)` on `rgb(15,18,26)` (~6.9:1), mission
        `rgb(148,163,184)` on `rgb(11,17,32)` (~7.4:1), against ~1.9:1 and
        ~2.5:1 before. The line is text presentation, so SC 1.4.3 governs at
        4.5:1: the old values missed it, both new ones clear it.

## Success Criteria

- Strikethrough on done checklist steps renders in the muted text color
  (~6.9:1) in ember and mission.
- `rg decoration-edge-strong web/` returns nothing.

## Out of Scope

- Restyling the checklist beyond the decoration color.
- The other `line-through` sites (already correct).
