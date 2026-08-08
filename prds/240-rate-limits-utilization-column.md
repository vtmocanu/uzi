# PRD #240: Rate limits table — stack the two windows into one "Utilization" column

**GitLab Issue**: [#240](https://gitlab.example.com/vtmocanu/uzi/-/issues/240)
**Status**: Draft (created 2026-08-07)
**Priority**: Low
**Mock**: `prds/mockups/240-rate-limits-utilization-mock.html` (Option A of three; shown to owner 2026-08-07, chosen)

## Problem

The Admin › Rate limits table (`web/src/pages/AdminRateLimits.tsx`, PRD #53) renders
six columns: **User · Token · 5-hour window · 7-day window · Status · Updated**. Each
window cell is a fixed grid — `minmax(5rem,9rem)` meter + `3rem` percent + `5.5rem`
reset countdown ≈ 280px — and there are **two** of them side by side. On a laptop
content column (sidebar + padding leaves ~900px) the table is wider than its `Card`,
so the existing `<div className="overflow-x-auto">` wrapper takes over: a **horizontal
scrollbar** appears and the **Status pill clips** (the reported bug — "7d nearly out"
is cut off, "Updated" is off-screen entirely).

The two window columns are the whole cause: ~560px of the table's width is spent on
them, with the reset countdown (`5.5rem` each) and the standalone "Updated" column
adding width for information that does not need its own column.

## Solution Overview

Adopt **Option A**: collapse the two side-by-side window columns into a single
**"Utilization"** column that stacks the 5-hour and 7-day meters as two thin rows,
and fold the "Updated" timestamp under the Status pill. The table goes from **six
columns to four** (User · Token · Utilization · Status), roughly **halving** its
horizontal footprint so it fits its card at the real content width — no scrollbar,
no clipped pill, **and no data removed**: both windows, both percentages, both reset
countdowns, and the sync time all remain on screen.

Each Utilization cell renders, per window, a small mono label chip (`5h` / `7d`), the
existing `MeterTrack` bar, the percentage, and the reset countdown — reusing
`MeterTrack` (`web/src/components/Meter.tsx`), `Badge`, and the `formatCountdown` /
`formatAgo` helpers (`web/src/lib/rateLimits.ts`) verbatim. This is a **pure `web/`,
single-page change**: no API, no DTO, no query, no migration, no trust boundary. The
two other mocked options (B — tighten the existing columns; C — responsive user cards)
were shown and not chosen; they are recorded in Decision 1 for provenance.

## Design Decisions

### Decision 1 — Stack both windows in one column (chosen), not tighten-in-place or cards

Three layouts were mocked at the real content width:

| Option | What it does | Why not (for this PRD) |
|---|---|---|
| **A — stacked Utilization column** (chosen) | 5h/7d become two stacked meter rows in one column; Updated folds under Status | — |
| B — tighten the two columns | keep six columns; % rides the bar, reset → caption, drop Updated | smaller diff, keeps side-by-side compare, but leaves little margin — a very narrow window can still pinch |
| C — responsive user cards | drop the `<table>` for a reflowing card grid | can't scroll by construction and best on mobile, but loses row density and the at-a-glance "nearest a limit first" scan, and is the largest change |

A is chosen as the balance: it **directly** kills the scrollbar (halves the width),
hides nothing, and stays a capacity **table**, so the existing danger→warn→ok→stale→
no-token sort (`sortAdminRows`) still reads top-to-bottom. Cost accepted: rows get
taller (two stacked meters), and 5h-vs-7d is now a **vertical** comparison within a
cell rather than horizontal across two columns.

### Decision 2 — Fold "Updated" under the Status pill, don't keep it a column or drop it

The "Updated Nm ago" value (`formatAgo(t.limits.synced_at, now)`) is worth keeping — it
is how an admin tells a fresh reading from an old one — but it does not warrant a sixth
column's width. It moves to a small sub-line **under** the Status badge in the Status
cell.

**Tone: it stays `text-muted`, not `text-faint`.** This value is not new — it is the
existing **Updated** column relocated, and that column is `text-muted` **today** with an
explicit web-ux comment in `AdminRateLimits.tsx` (search `text-muted (not faint)`)
requiring exactly that: *"the 'updated Xm ago' timestamp must clear WCAG AA at 12px
(web-ux finding)."* Moving the value under the pill does not change its size (still
`text-xs` = 12px) or that finding, so the folded line keeps `text-muted`. The chosen mock
renders `.updated` in `--faint` (`prds/mockups/240-rate-limits-utilization-mock.html`,
`.updated` rule); that is a **mock divergence to correct in M1**, not a licence to
downgrade the tone. Any move to `text-faint` here reverses a documented a11y decision and
needs web-ux / owner sign-off first.

### Decision 3 — Keep `overflow-x-auto` as a defensive wrapper

The wrapper is **not** removed. After Option A the table fits at target widths, so the
scrollbar never renders — but on a genuinely tiny viewport a horizontal scroll is
better graceful degradation than a broken layout. The wrapper is inert when content
fits; the goal is "it never triggers at normal widths", not "the safety net is gone".

### Decision 4 — Preserve every accessibility + test contract the current cell holds

The refactor must not silently weaken what `AdminRateLimits.tsx` already guarantees:

- **`MeterTrack` aria-labels stay the full window names** — `"5-hour window"` /
  `"7-day window"` — even though the visible chip reads `5h` / `7d`. The existing test
  (`AdminRateLimits.test.tsx`) selects bars by `getByRole("progressbar", { name: "5-hour window" })`; that must keep passing.
- **Identity cell unchanged**: `UserCell` still renders once per user via `rowSpan`
  with the name in the first `<div>` (the sort test reads it via `querySelector("div")`,
  and PRD #54's "no name" faint placeholder stays).
- **Tones unchanged**: `toneFor` thresholds (ok <40 ≤ warn <85 ≤ danger), the dimmed
  bar + faint text for a stale row, and the `statusBadge` mapping are all reused as-is.
- **Percent, reset, and the folded "updated" line stay `text-muted`** where the current
  code's web-ux comments require AA at 12px. The "updated" line is the **relocated**
  Updated column, not a new element, and is `text-muted` today for exactly that reason
  (Decision 2) — moving it under the pill must not silently drop it to `text-faint`.
- **The `5h` / `7d` chip is a NEW visible surface, not a preserved one.** It becomes the
  only *visible* window identity (the `MeterTrack` aria-label keeps the full "5-hour
  window" / "7-day window" for screen readers), replacing the full-size `text-muted`
  column headers. The mock renders it in `--faint` at 10.5px (`.wlab`), **below** the
  team's own "faint fails AA at 12px" bar. Pick a tone during M1 so the label a sighted
  low-vision admin relies on clears AA, or record why faint is acceptable — do not carry
  the mock's tone in unexamined. Decision 4 "preserves" contracts; this chip is the one
  thing the refactor *adds*, so it needs a decision, not preservation.

## Milestones

### M1 — Utilization column refactor (`web`)
- [ ] Replace the two window `<th>`/`<td>` columns in `AdminRateLimits.tsx` with one
  **Utilization** column. Introduce a cell that stacks the 5h and 7d meters (either a
  new `UtilizationCell`, or `WindowCell` reworked to render both windows) — each row a
  mono `5h`/`7d` chip + `MeterTrack` + `%` + reset countdown, per the mock.
- [ ] Remove the standalone **Updated** column; render "updated Nm ago" as a sub-line
  under the Status badge (Decision 2).
- [ ] `no_token` and non-`ok` rows: one em-dash Utilization cell (not two), Status pill
  unchanged; keep every user present and countable (PRD #104's one-row-per-token and the
  token-less single row are unchanged).
- [ ] Preserve all Decision 4 contracts (aria-labels, rowSpan identity, tones, AA tones).
- [ ] Keep the `overflow-x-auto` wrapper (Decision 3).

### M2 — Tests + gate (`web`)
- [ ] Update `web/src/pages/AdminRateLimits.test.tsx` for the new shape. The `no_token`
  em-dash count changes: today `getAllByText("—").length` is **4** (token cell + both
  window cells + Updated); after the refactor it is **2** (token cell + the single
  Utilization cell — the Updated dash is gone, since a `no_token` row shows no "updated"
  line). Repoint it to `toBe(2)` and rewrite its comment. Keep the `progressbar`
  by-aria-name assertions (aria-labels stay `"5-hour window"` / `"7-day window"`) and the
  stale-row assertion `getAllByText("stale").length === 2` (the one Utilization cell still
  stacks two reset rows, so both "stale" labels remain; mihai's badge is "🔒 vault locked",
  not "stale", so the count is exactly the two resets). There are **no** existing header
  assertions to update — the four-column-header check below is a net-new addition.
- [ ] Add a regression assertion that pins the fix's intent — e.g. the table renders the
  four expected column headers and no `Updated` header — so a future re-widening is caught.
- [ ] `task gate:web` green (deps-check + oxlint + knip + check-docs + typecheck + vitest).
- [ ] Manual: `VITE_UZI_MOCK=1 npm run dev` on the admin rate-limits scenario — confirm
  no horizontal scrollbar and no clipped pill at a laptop width; spot-check the `mission`
  theme too (tokens differ, layout must still fit).

### M3 — Specs, docs, CLI check
- [ ] `specs/ai.md` records the layout decision (Utilization column; Updated folded) as
  an AI design decision.
- [ ] **CLI check** (mandatory per the "new functionality ⇒ check `api/cmd/uzi/`"
  convention): this is presentation-only for a web admin table with no CLI counterpart —
  record it as deliberately out-of-scope rather than leaving the note unmade.
- [ ] Docs: only if a page enumerates this table's columns; otherwise none. `check-docs`
  stays green.

## Success Criteria

1. At a ~900px content column the Rate limits table shows **no horizontal scrollbar** and
   the Status pill is **fully visible** — the reported bug is gone.
2. The table has **four** columns (User · Token · Utilization · Status); both windows,
   both percentages, both reset countdowns, and the sync time remain on screen — nothing
   is hidden to fix the width.
3. The danger→warn→ok→stale→no-token sort still reads top-to-bottom, and every user
   (including token-less) is present and countable.
4. Accessibility contracts hold: bars keep their `"5-hour window"` / `"7-day window"`
   aria-labels, and AA-critical text keeps `text-muted`.
5. `task gate:web` is green; the mock/demo build renders the new layout.

## Risks & Mitigations

- **Taller rows reduce users-per-screen.** Accepted trade for killing the scroll and
  keeping all data; the sort still surfaces the urgent rows first.
- **Vertical 5h-vs-7d comparison is less direct than side-by-side.** Mitigated by the
  `5h`/`7d` label chips and consistent bar alignment; if it proves a problem, Option B
  is the fallback (recorded in Decision 1).
- **A negative/stale test assertion going vacuous after the shape change.** Mitigated by
  M2 following the copy-change sweep rule (`.claude/rules/web.md`): grep retired
  header/label strings across the test tree and repoint each assertion, not blindly.

## Testing Strategy

- **web**: vitest on `AdminRateLimits.test.tsx` — render/sort/badge/stale/no-token
  behaviour under the new column shape, progressbar-by-aria-name, and the new
  column-count regression assertion.
- **manual**: `VITE_UZI_MOCK=1 npm run dev`, admin rate-limits scenario, at laptop width
  and narrower, in both `ember` and `mission` themes.

## Dependencies

None. Reuses `MeterTrack` (PRD #53), `Badge` (`ui.tsx`), `formatCountdown`/`formatAgo`
(`rateLimits.ts`), and the PRD #104 one-row-per-token structure. No API, DTO, query,
migration, service, or trust-boundary change.

## Parallelization

Single file plus its test — no parallelism to exploit. M1 → M2 → M3 sequential.

| Phase | Milestone | Repo/module | Depends on | Files (primary) |
|---|---|---|---|---|
| 1 | M1 | web | — | `web/src/pages/AdminRateLimits.tsx` |
| 2 | M2 | web | M1 | `web/src/pages/AdminRateLimits.test.tsx` |
| 3 | M3 | web + specs | M1 | `specs/ai.md`, (docs only if a page lists columns) |
