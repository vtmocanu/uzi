# PRD #379: Surface milestone progress in the CLI TUI

**Issue**: #379
**Priority**: Medium
**Status**: Implemented (follow-ups pending)

> This PRD is **retrospective**: the implementation landed during the session that
> authored it and is gate-clean (`task lint:api`, `task deadcode:api`,
> `go test ./cmd/uzi` all green). The implementation milestones below are checked
> done and cite the code as shipped; only the review/screenshot/docs follow-ups
> remain open. It is written so the change has the same design-rationale record as
> any other, not to plan work that has not happened.

## Problem

A run created from a `PRD`-labelled issue is **milestone-structured** (PRD #122): it
carries a frozen, human-approved milestone list plus live progress (which ids are
reported complete, which is in progress). Two of uzi's three run surfaces already
show that progress:

- **Web** — `MilestoneChecklist` and the compact `MilestoneBadge`
  (`web/src/pages/RunView.tsx`, `web/src/lib/runBadge.ts`).
- **CLI `uzi run get`** — `milestoneRows` prints a `{done}/{total} reported complete`
  block with a done / in-progress / left row per milestone
  (`api/cmd/uzi/run.go:1321`).

The **interactive TUI** (`uzi tui`, `api/cmd/uzi/tui_*.go`) showed **none of it**.
Neither the board (factory floor) nor the run-detail view referenced a run's
milestones — a grep for `milestone` across `tui_*.go` returned only an unrelated
test comment. A user living in the TUI (the board is the primary at-a-glance view,
and `we mostly test in k8s now` makes the TUI a common window onto real runs) could
not tell how far a PRD run had progressed without dropping to `uzi run get` or the
web. The two surfaces that already had it also proved the semantics were settled and
worth mirroring, so this was a visibility gap, not a new-feature design problem.

## Goal

Bring milestone progress to the two TUI surfaces, **reusing the web/CLI semantics
exactly** so the three surfaces cannot disagree:

- **Board**: a compact `M{done}/{total}` badge (the web `MilestoneBadge` twin) in a
  fixed column, blank for a run with no frozen list.
- **Run detail**: a per-milestone checklist (the web `MilestoneChecklist` / CLI
  `milestoneRows` twin) in the crew rail.
- Do it without regressing the existing views: a non-milestone (pre-#122) run must
  render byte-for-byte as before.

While in the board row layout, a secondary ask surfaced and is folded in: **trim
over-long titles** into a tidy column instead of letting them run the full width of
a wide terminal.

## Why this needed no open-web access

Every fact is in this repo's own source, resolved locally during the work: the
milestone DTO fields and their nil-vs-empty contract (`api/internal/apitypes/run.go`,
`api/internal/handler/{runs,workers}.go`), the web/CLI progress semantics
(`web/src/lib/runBadge.ts`, `api/cmd/uzi/run.go`), the TUI palette and render
primitives (`api/cmd/uzi/tui_render.go`), and the D7 untrusted-text discipline
(`.claude/rules/tui.md`, `tui_d7_guard_test.go`). Nothing depended on the internet.

## Technical scope

**Surfaces changed** (all in `api/cmd/uzi/`):

1. **Shared progress helper** (`tui_detail.go`): `milestoneProgress(run) → (done,
   total, reported)` and `milestoneCount(done, total, reported) → "2/4" | "–/4"`,
   the TUI twin of the web's `milestoneBadge`. Both the board badge and the detail
   block read these, so the two surfaces count identically by construction.
   - `done` counts frozen **members** present in the completed set — immune to a
     duplicate id and to a completed id naming a milestone dropped after it was
     ticked (matching `milestoneBadge` and `milestoneRows`).
   - `reported` is `MilestonesCompleted != nil`: a nil completed slice (JSON `null`)
     means nothing was ever reported, so the run reads `–/N` rather than a `0/N`
     that looks like failure (matching the web's PRD #265 M2 treatment).

2. **Run-detail crew rail** (`tui_detail.go`, `renderMilestones`): a titled
   `MILESTONES {count}` line plus one row per milestone in frozen order, glyph-marked
   ✓ done / ◐ in progress / ○ not started (the web `MilestoneMark` glyphs). Appended
   below the crew lanes **and** on the no-lanes ("(no activity yet)") path, so a
   queued/just-claimed milestone run shows the block before its first frame; renders
   nothing (and leaves the rail byte-identical) for a run with no frozen list.
   Untrusted titles drawn through `renderer.Plain`.

3. **Board `MILE` column** (`tui_board.go`, `milestoneMarker` + row/header/prefix):
   a fixed 6-wide column between AGE and TITLE carrying `M{done}/{total}` (or
   `M–/{total}`), blank for a non-milestone run. A **fixed column**, not a float
   after the title, so the badge stays in one place down both the own and admin
   boards. Faint styling: milestones are informational, unlike the own-board judge
   marker or a stalled row.

4. **Board title trim** (`tui_board.go`): titles cap at `boardTitleMax = 60` (was:
   the whole remaining terminal width) and pad to that width, so a long title is
   trimmed to `…` and the trailing judge marker lines up in a column instead of
   floating after each ragged title end.

5. **Guardrails + demo** (`tui_d7_guard_test.go`, `demo.go`): `"Title"` added to
   `d7UntrustedFields`; a milestone-structured run seeded into `uzi tui --demo`.

6. **Full-height run viewer** (`tui_detail.go`, `transcriptViewport` +
   `padLinesToViewport`): the two-pane body now fills the terminal down to the footer.
   Previously the transcript column was only as tall as its content, so the pane divider
   stopped mid-screen and a tall terminal showed dead space below the footer. The viewport
   is now the terminal height minus the *actual* chrome renderDetail draws (header, optional
   park line, transport + blank, pane title, optional banner, optional steer bar, footer),
   and the window is padded with blank lines to that height so the divider reaches the
   footer. The same value drives the scroll clamp, so render and scroll stay consistent.

**Data availability**: no server change. `ListRuns` and `AdminListRuns` already
build each list item via `runToDTO` (`api/internal/handler/workers.go:348`), which
decodes `Milestones` / `MilestonesCompleted` / `MilestonesInProgress`, so the board
list DTO already carried everything the badge needs.

## Milestones

- [x] **M1 — Shared progress helper.** `milestoneProgress` / `milestoneCount` in
  `tui_detail.go`, matching `milestoneBadge` semantics (frozen-member `done`, nil ⇒
  `–/N`). Single source for both surfaces.
- [x] **M2 — Run-detail crew-rail checklist.** `renderMilestones` wired into
  `renderLaneRail`; glyphs ✓/◐/○, frozen order, untrusted titles via `renderer.Plain`,
  non-milestone runs unchanged.
- [x] **M3 — Board `MILE` column + title trim.** Fixed column between AGE and TITLE;
  `boardRowPrefixWidth` and the header updated; `boardTitleMax = 60` with padded
  title so the judge marker aligns.
- [x] **M4 — Guardrails + tests.** `"Title"` in `d7UntrustedFields`; hostile
  milestone title added to `TestTUIViewsStripControlBytesFromUntrustedText`;
  `TestTUIDetailMilestoneBlock` and `TestTUIBoardMilestoneBadge` (glyphs, counts,
  `–/N`, and the back-compat "no block / no badge" assertions). Gate green.
- [x] **M5 — Demo fixture.** A milestone-structured run (2/4, one in progress) seeded
  into `uzi tui --demo` so the block and badge are drivable with no server.
- [x] **M6 — Full-height run viewer.** `transcriptViewport` computes the body height from
  the actual chrome and `padLinesToViewport` pads the window, so the two-pane body and its
  divider fill the terminal to the footer. Guarded by `TestTUIDetailFillsHeight` (rendered
  rows == terminal height; divider extends down the body).
- [x] **M7 — Visual review.** uxlab fixtures seeded with a milestone run and screenshots
  regenerated; `tui-ux` reviewed via the PNGs, a real `NO_COLOR=1` PTY run, and seven
  terminal sizes. Milestone block, MILE column, `M–/N`, NO_COLOR fallback and the full-height
  fill all passed. Two edge-of-envelope regressions were found and **fixed**:
  - **Footer clipped when the rail is taller than the viewport** (a milestone run's rail can
    exceed the transcript budget): `joinColumns` now clamps the two-pane body to the transcript
    (viewport) height, so the rail truncates rather than pushing the footer off-screen. Guard:
    `TestTUIDetailFooterSurvivesTallRail`.
  - **Judge marker clipped on the board at ~80 cols** (the MILE column's 8-col prefix tax
    squeezed marker rows past the edge): the MILE column is now dropped below
    `boardMileMinWidth` (90 cols), reverting to the pre-#379 board layout on narrow terminals;
    milestone progress stays on the detail view. Guard: `TestTUIBoardRowsFitNarrowWidth`.
- [ ] **M8 — Docs + spec note.** If `docs/cli.md` documents the TUI board columns or
  the detail view, add `MILE`, the milestone block, and the full-height body; record the
  design decisions (fixed column over float, `–/N` semantics, `boardTitleMax`, dynamic
  viewport) in `specs/ai.md`.

## Success criteria

1. A milestone-structured run shows `M{done}/{total}` on the board and a per-milestone
   checklist in the detail crew rail; a non-milestone run renders exactly as before
   (verified by `TestTUIBoardMilestoneBadge` / `TestTUIDetailMilestoneBlock`).
2. The board and detail counts always agree (shared `milestoneProgress`).
3. A run that has reported nothing shows `–/N`, never `0/N`.
4. Untrusted milestone titles cannot inject control bytes into a frame (D7 guard +
   hostile-value render test).
5. Long board titles are trimmed to a fixed column; the judge marker aligns.
6. The run viewer fills the terminal height: the two-pane body and pane divider reach the
   footer with no dead space below the content, at any terminal height
   (`TestTUIDetailFillsHeight`).
7. A milestone run with no activity yet still shows the block (regression guard in
   `TestTUIDetailMilestoneBlock`).
8. A tall crew rail (many lanes + the milestone block) never pushes the footer off-screen;
   the body clamps to the viewport (`TestTUIDetailFooterSurvivesTallRail`).
9. No board row overflows the terminal edge at a narrow width, and the judge marker keeps its
   full text (`TestTUIBoardRowsFitNarrowWidth`); the MILE column is dropped below 90 cols.
10. `uzi tui --demo` demonstrates all of the above with no server.
11. `task lint:api`, `task deadcode:api`, `go test ./cmd/uzi` all green.

## Decisions

1. **Reuse web/CLI semantics via one shared helper** rather than re-deriving counts
   per surface — a third independent implementation is a third thing to drift. The
   board badge and the detail block both read `milestoneProgress`.
2. **`reported = MilestonesCompleted != nil`**, so `–/N` (never reported) is distinct
   from `0/N` — matching the web (PRD #265 M2). The Go nil-vs-empty-slice distinction
   survives the JSON round-trip (`null` → nil, `[]` → empty non-nil).
3. **Count says neither "verified" nor "complete"** (PRD #122 Decision 6): the worker
   only *reports* a milestone done and nothing in uzi verifies it. The bare `N/M` and
   the ✓ glyph must not imply verification; the crew-rail header is `MILESTONES` with
   no "complete" word.
4. **Board: a fixed column, not a badge floating after the title.** A column keeps
   the badge in one place down the board and lets non-milestone rows show blank,
   aligned. The user explicitly asked for "a table … after AGE and before TITLE."
5. **Detail done rows are muted, not struck through.** lipgloss emits strikethrough
   as a per-rune SGR run (frame bloat, and it breaks substring assertions) and the ✓
   glyph already carries completion. In-progress uses plain terminal fg (the web's
   `text-fg`); the glyph colour is the signal.
6. **Title trim + pad** (`boardTitleMax = 60`): trimming answers the "titles are very
   long" ask; padding to the cap turns the trailing judge marker from a ragged float
   into an aligned column.
7. **D7**: milestone titles are repo/agent-authored untrusted text, so they route
   through `renderer.Plain` and `"Title"` joins `d7UntrustedFields`; the hostile-value
   render test is extended (the clean-fixture screenshots cannot catch a raw draw).
8. **Full-height body via a dynamic viewport, not a fixed `-11`.** The old
   `transcriptViewport` subtracted a constant, which over-subtracted whenever a banner/steer
   was absent and left the body short. It now subtracts the *actual* rendered chrome (using
   the same helpers renderDetail calls: `detailBanner`, `renderSteerBar`, `limitWaitLine`),
   so the body fills exactly to the footer. One value feeds both the render and the scroll
   clamp, so they cannot disagree. The alternative (pad the whole frame below the footer) was
   rejected: it moves the footer, not the divider, so it would not close the gap the user saw.
9. **Body clamps to the transcript height, rail truncates** (tui-ux finding 1). `joinColumns`
   uses the transcript column's height, not `max(rail, transcript)`, so a rail taller than the
   viewport (a milestone run with many lanes) can never push the total past the terminal and
   clip the footer. The footer carries the only navigation keys, so keeping it always beats
   showing every rail line; rail scrolling is a possible later refinement.
10. **MILE column is width-conditional** (tui-ux finding 2). It adds 8 cols to the fixed
    prefix, which at ~80 cols squeezed marker rows past the edge and clipped the judge marker.
    Below `boardMileMinWidth` (90) the column is dropped and the board is the pre-#379 layout;
    milestone progress is still on the detail view. Chosen over shrinking the title below its
    floor (which would give a 3-char title) or abbreviating the marker.

## Risks and mitigations

- **A long rail overflows a short terminal.** A run with many milestones lengthens
  the crew rail; on a short terminal the tail clips. Bounded and visible; M6's review
  checks it. Not gating — the block is informational and the transcript viewport is
  computed independently of rail height.
- **`boardTitleMax` is a guess.** 60 is a judgement call; too low truncates useful
  titles, too high defeats the trim. Exposed as a single named const, tunable after
  the visual review.
- **Nil-vs-empty relies on the server emitting `null`.** If a future server change
  emitted `[]` for an unreported run, `reported` would flip to true and show `0/N`.
  The web already depends on this exact contract, so the two move together.

## Non-goals

- No change to how milestones are created, frozen, approved, or reported — this is
  read-only presentation.
- No server/DTO change (the data was already on the list DTO).
- No new interactivity (no drill-into-milestone, no filtering by progress).
- No change to the web or CLI surfaces — they are the reference, not in scope.

## Testing

- **Unit / render seam** (`tui_model_test.go`): `TestTUIDetailMilestoneBlock`
  (glyphs, `2/4`, titles; asserts a non-milestone run draws no block, an unreported run
  shows `–/4`, and a milestone run with no activity yet still shows the block);
  `TestTUIBoardMilestoneBadge` (`M2/4`, `M–/2`, no badge for a milestone-less run);
  `TestTUIDetailFillsHeight` (rendered rows == terminal height; divider extends down the
  body); `TestTUIDetailFooterSurvivesTallRail` (a tall rail does not clip the footer);
  `TestTUIBoardRowsFitNarrowWidth` (no row overflow + full judge marker at 80 cols, MILE
  dropped below 90 cols and back at 120).
- **Guard** (`tui_d7_guard_test.go`): `"Title"` in `d7UntrustedFields`, enforced by
  `TestD7UntrustedFieldsNeverReachAWriterUnsanitized`; hostile milestone title in
  `TestTUIViewsStripControlBytesFromUntrustedText`.
- **Manual / visual**: `uzi tui --demo` (the seeded run), and the uxlab screenshot
  harness in M6.
- **Gate**: `task lint:api`, `task deadcode:api`, `go test ./cmd/uzi` green.
