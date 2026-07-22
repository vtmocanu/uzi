# PRD #117 — Your-usage card orphans the "see per-run detail →" arrow

**Issue**: [#117](https://gitlab.example.com/vtmocanu/uzi/-/issues/117) · **Label**: PRD · **Priority**: Low
**Area**: `web/` dashboard usage cards (PRD #40 lineage).

## Problem

In the **Your usage** card, the last-7-days kicker ends with a link:

> Across 11 runs, all time · 161.43M tok / $164.13 in the last 7 days · **see per-run detail →**

The kicker is an inline-flowing `<p>` and the link text carries an ordinary
space before the arrow. When the paragraph wraps at a narrow container width,
the break can land between `detail` and `→`, leaving the **arrow stranded alone
on its own line** (observed 2026-07-22, screenshot on the issue). It reads as a
rendering glitch — a dangling glyph with no anchor text.

## Solution

Make the link an **atomic wrap unit** so the whole `see per-run detail →` moves
to the next line together, and the arrow can never separate from `detail`:

1. Add **`whitespace-nowrap`** to the `<Link>` in `YourUsageCard` — the link
   never breaks internally; it wraps as one block.
2. Belt-and-suspenders: replace the space before the arrow with a **non-breaking
   space**, written in JSX as the explicit escape `{"\u00A0"}` (never a literal invisible glyph), so the arrow stays glued to `detail` even if the nowrap
   class is ever dropped or overridden.

The link text is short (`see per-run detail →`, ~20 chars) and the card is
comfortably wide, so forcing it to wrap as a unit costs no horizontal room in
practice and cannot itself cause overflow.

### Approach chosen (and rejected)

- **Chosen — `whitespace-nowrap` on the `<Link>` + NBSP before `→`.** Robust to
  any container width; keeps the arrow semantically part of the link label.
- **Rejected — NBSP only.** Fixes the orphaned arrow but still lets the link
  break between `see` / `per-run` / `detail`, which is acceptable but less tidy
  than moving the whole call-to-action together.
- **Rejected — a flex/`inline-block` restructure of the kicker.** Over-engineered
  for a one-line cosmetic fix; the kicker is deliberately inline-flowing text.

## User journey

- A user views the dashboard on a narrow window (or the sidebar-compressed
  layout). The "see per-run detail →" link now wraps as a whole to the next line
  when it doesn't fit, with the arrow attached — no lone `→` on its own line.
- At wide widths nothing changes: the kicker renders exactly as today.

## Technical scope

Single file, one component.

- **`web/src/components/UsageCards.tsx`** — `YourUsageCard`, the kicker `<p>`
  (currently lines ~65–72). Add `whitespace-nowrap` to the `<Link to="/runs">`
  className (alongside `text-info hover:underline`) and set the label to
  `see per-run detail{"\u00A0"}→` so the space before the glyph is non-breaking.
  This is the **only** arrow-suffixed link in the file — `FactoryTotalCard` and
  `PerUserUsageTable` have no such link, so no other card needs the change.
- **CLI** — no change. This is a web-only rendering concern; `api/cmd/uzi/` has
  no equivalent link (convention check per CLAUDE.md: CLI is a second API
  consumer, but there is nothing to mirror here).
- **Mock** — `prds/mockups/40-token-usage-mock.html` line 329 carries the same
  `see per-run detail →` pattern in a `<p class="kicker">`. Applying the same
  `white-space: nowrap` on its `.faux` link keeps the design reference faithful.
  Low priority (mocks are frozen historical artifacts); do it if cheap, note it
  as superseded otherwise.

## Milestones

- [ ] **M1 — Fix + test.** Apply `whitespace-nowrap` + NBSP to the `YourUsageCard`
  link. Add a `web/src/components/UsageCards.test.tsx` assertion that the
  rendered link element (a) carries the `whitespace-nowrap` class and (b) its
  `textContent` **matches `/detail\u00A0→/`** — a *positive* pin asserting the
  arrow is glued to `detail` by a non-breaking space. (Positive, not "no plain
  space before `→`": a negative pin can pass vacuously if the label text later
  changes, whereas the positive form fails loudly the moment the escape is
  dropped.) `npm test` + `npm run typecheck` green.
- [ ] **M2 — Verify in-app + mock fidelity.** Confirm in mock mode at a narrow
  width that the arrow no longer orphans (the whole link wraps together). Apply
  the same `white-space: nowrap` to the mock's `.faux` link (or note it left
  superseded). `npm run build` green (runs `check-docs` + `tsc`).

## Risks & mitigations

- **Regression surface is effectively nil.** One className + one whitespace
  character in one component; no API, DTO, or state change. The test in M1 pins
  the fix so a future refactor of the kicker can't silently reintroduce the split.
- **NBSP in source readability.** Written as `{"\u00A0"}` (not a literal
  invisible glyph) so it's greppable and obvious in review.

## Success criteria

- The `see per-run detail →` link wraps as a single unit; the arrow never
  appears alone on a new line at any container width.
- `YourUsageCard`'s link carries `whitespace-nowrap` and a non-breaking space
  before `→`; a test asserts both.
- Wide-width rendering is unchanged. All web tests + typecheck + build green.
