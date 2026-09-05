# PRD #1136: TUI milestone marker — tungsten in-progress cell + half-circle rail rows

## Problem

In the `uzi` TUI, the in-progress milestone marker diverges from its siblings on two axes, both introduced by PRD #1064 (D4) and both surfaced in review of that work:

1. **Color.** The in-progress marker is drawn in the palette's `wait` hue
   (`ld(#0369a1, #38bdf8)`, blue), the same color reserved for *paused* states
   (rate-limited, crew-waiting, reconnecting; `tui_render.go` maps `crewWaiting`
   to it). So an *actively working* milestone wears the *paused* color, while the
   completed marker uses `tungsten` (`ld(#7c5200, #c9a061)`, the warm accent).
2. **Glyph.** In the crew-rail milestone checklist, the in-progress row is a
   parallelogram (`▰`/`▱`), a different visual family from its `○` (not started)
   and `✓` (done) neighbors, and different from the **web** run view, which draws
   the in-progress milestone as `◐` (a half-circle, `RunView.tsx` `MilestoneMark`,
   `text-info`).

The result: on the TUI the in-progress milestone reads as "paused" by color and
as an out-of-family shape in the checklist, and the TUI and web disagree on the
in-progress glyph.

## Solution

A TUI-only presentation change (web and CLI untouched by deliberate decision, see
D3):

- **Recolor the in-progress micro-bar cell** from `wait` to `tungsten`. This is
  the blinking `▰`/`▱` cell in both the crew-rail eyebrow micro-bar and the board
  per-run micro-bar. The blink itself **stays** — only the color changes. Done
  cells are already `tungsten`, so on the lit (`▰`) frame the in-progress cell now
  matches the done fill; the blink and the empty (`▱`) off-frame are what set it
  apart. This is the accepted trade-off (D2).
- **Change the crew-rail checklist rows** to render the in-progress milestone as a
  blinking `◐ ⇄ ○` half-circle in the **`faint` (grey) color, the SAME color as the
  not-started `○`** (`faintC`, `ld(#6c6c6c, #8a8a8a)`), so the empty (`○`) phase of
  the blink is indistinguishable in color from its not-yet-started siblings and the
  row reads as one grey circle pulsing between half-filled and empty. Static /
  reduced-motion frame is `◐` (see D4). The in-progress row still stands apart from a
  not-started row by its **brighter title** (plain fg vs the not-started row's faint
  title, an existing difference), the `◐` **shape**, and the motion — never by glyph
  color. `✓` (done, `sage`) and `○` (not started, `faint`) rows are otherwise
  unchanged.

Net after this PRD, per TUI surface:

| surface | not started | in progress | done |
|---|---|---|---|
| crew-rail eyebrow micro-bar | `▱` faint | `▰`/`▱` **tungsten** (blinks) | `▰` tungsten |
| board per-run micro-bar | `▱` faint | `▰`/`▱` **tungsten** (blinks) | `▰` tungsten |
| crew-rail checklist row | `○` faint | **`◐ ⇄ ○` faint grey (blinks)** | `✓` sage |

The two surfaces use different in-progress colors on purpose (D2/D5): the micro-bar
is a progress *bar* where in-progress counts toward the tungsten fill; the checklist
is a *todo list* where an in-progress item is a grey circle animating toward its
green `✓`.

## Technical scope

All changes are in `api/cmd/uzi/` (the TUI). No API, DB, web, or CLI-command
changes. The blink machinery (`blinkOn`, `blinkArmed`, `blinkTickMsg`,
`UZI_TUI_NO_BLINK`) is reused as-is; nothing about *when* the tick is armed
changes.

### Code anchors (current line numbers; verify against the tree at implementation)

1. **`api/cmd/uzi/tui_detail_rail.go`**
   - `milestoneCell` (~237-243): the blinking micro-bar cell. Change its color
     from `m.pal.wait` to `m.pal.tungsten`. This single change recolors **both**
     micro-bars, because the cell is shared (see anchor 2). Update its doc comment
     (it says "in the wait colour").
   - `renderMilestones` (~299-385):
     - The eyebrow micro-bar (`mid = m.milestoneCell(nil)`, ~341) needs no further
       change once `milestoneCell` is tungsten.
     - The static `waitCell := paintSeg(m.pal.wait, nil, false, "▱")` (~329) and
       the per-row in-progress switch case (~368-376) currently render the row as
       the blink cell (selected) or the static `waitCell` (others). Replace the
       **row** rendering with a blinking `◐ ⇄ ○` cell in the **`faint` (grey)**
       color for every in-progress row (both the selected/first-in-progress row and
       any additional in-progress rows). The row cell must:
       - alternate `◐` / `○` on `blinkOn` (the same tick the micro-bar consumes),
       - render `m.pal.faint` / `faintC` — the SAME color the not-started `○` uses
         (~361), NOT tungsten,
       - and render `◐` (not `○`) in the static / non-tty / `UZI_TUI_NO_BLINK`
         frame — see D4 for why the presence frame, not the empty one, is the
         static frame (doubly important now the glyph color no longer separates it
         from a not-started `○`).
       Keep the in-progress row's **title** style as it is today (plain terminal fg,
       ~376) — that brighter title is what still separates an in-progress row from a
       faint not-started row at a glance. Only the glyph color changes.
     - **The row cell is a NEW span, not `milestoneCell`, and its blink polarity is
       INVERTED relative to it.** `milestoneCell` maps the initial `blinkOn == false`
       to the *empty* glyph `▱` (its static frame is the absence frame, gated on
       color). The row must instead map `blinkOn == false → ◐` (the presence frame,
       per D4) and `blinkOn == true → ○`. So do NOT reuse `milestoneCell` or copy its
       `if m.blinkOn { g = filled }` shape for the row — write a separate cell with
       the opposite mapping, or the static `◐` lands on the wrong phase. A
       consequence, expected not a bug: the eyebrow/board micro-bar and the row then
       blink in ANTI-PHASE (on `blinkOn == true` the micro-bar shows filled `▰` while
       the row shows empty `○`; on `false` the micro-bar is empty `▱` while the row is
       filled `◐`). See D4.
     - The eyebrow `· <id>` suffix (~350, `m.pal.faint`) is unchanged.

2. **`api/cmd/uzi/tui_board_rows.go`**
   - `milestoneMarker` (~443-461): done fill is already `m.pal.tungsten`; the
     in-progress cell is `m.milestoneCell(bg)` (~458), so recoloring
     `milestoneCell` (anchor 1) recolors the board micro-bar too. Update the
     comment at ~451 ("blinks `▰`/`▱` in the wait colour").
   - **Out of scope, leave as-is:** the board second-line `· <id>` id text at ~487
     uses `m.pal.wait`. That is the in-progress *milestone id text*, not the
     marker glyph, and `wait` there is a separate choice; this PRD does not touch
     it. (Recoloring it is a possible follow-up, not part of this change.)

### Tests to update (flip, do not just delete)

- **`api/cmd/uzi/tui_model_test.go` (~1118-1154)**, the crew-rail detail block
  test, currently asserts the *opposite* of the new behavior:
  - `~1133-1135` asserts the output **must not contain `◐`** ("the retired glyph").
    Flip it: the in-progress **row must now contain `◐`** (in the `faint`/grey
    color, the same as a not-started `○` — so assert the `◐` by shape, not color).
  - `~1136-1141` asserts a static `▱` in the **wait** color. After the change:
    the eyebrow micro-bar's static in-progress cell is `▱` in **tungsten**
    (`newPalette(true).tungsten`), and the row's static in-progress cell is
    **`◐` in `faint`** (`newPalette(true).faintC` / the `faint` style — the SAME
    color as a not-started `○`). Re-point each assertion at the correct color AND
    glyph per surface (micro-bar `▱` tungsten, row `◐` faint). Because the row `◐`
    and the not-started `○` are now the same color, add an assertion that the
    in-progress row is the `◐` **shape** (a not-started row is `○`); do not rely on
    color to tell them apart. Also update the D4 comment at `~1126-1127`.
- **`api/cmd/uzi/tui_blink_test.go` (~104-127)**, the board micro-bar Ascii
  (NO_COLOR) blink test, asserts **shapes** (`▰▱▱▱` off, `▰▰▱▱` on). Recoloring
  does not change shapes under an Ascii profile, so the assertions hold; only the
  comments mentioning "wait tint / wait colour" (~104-105) need updating. Confirm
  by running it.
- **`api/cmd/uzi/tui_detail_fixes_test.go` (~285-292)** references the micro-bar
  `▰▰▱▱`; verify it is shape-only and unaffected, and check it does not assert the
  old rail row glyph. Fix if it does.
- Grep `api/cmd/uzi/*_test.go` for any remaining `pal.wait` / `newPalette(...).wait`
  used in a **milestone** context (as opposed to a genuine wait-state assertion)
  and re-point it; and for any comment describing the in-progress milestone as
  "wait colour".
- **Two NEW positive tests are required — flipping the existing static-frame
  assertions is not enough** (every current rail test renders at the default
  `blinkOn == false`, i.e. the static frame, so nothing today proves the alternation
  or the row's Ascii shape):
  1. **Rail-row blink alternation.** Add a test that renders the crew rail at
     `blinkOn == true` and asserts the in-progress row is `○` (faint), and at
     `blinkOn == false` asserts it is `◐` (faint) — pinning the `◐ ⇄ ○` toggle and
     its inverted polarity (SC1). Without this the blink is unproven and the gate
     is green.
  2. **Rail-row under Ascii / NO_COLOR.** `tui_blink_test.go`'s existing Ascii test
     covers only the board *micro-bar*. Add a rail-row Ascii (NO_COLOR profile)
     assertion that the in-progress row's static frame is the `◐` *shape* and a
     not-started row is `○` — proving SC4's row legibility when the tint is stripped
     (the case D4 exists for).
- Follow the repo's mutation-testing discipline (`.claude/rules/go.md`): each
  flipped or new assertion must be shown to fail on the pre-change renderer and pass
  after — the assertion's *channel* is the glyph+color span, so mutate the color
  or the glyph and confirm the intended assertion (not a neighbor) reddens.

### Docs to update, then sync

- **`docs/run-activity.md` (~294-301)** currently says "On the TUI, there's no
  `◐` glyph — the in-progress milestone is a cell that blinks `▰`/`▱` in the wait
  colour ... other milestones ... static `▱`". Rewrite to the new reality: the
  crew-rail checklist rows now show a `◐` that blinks `◐`/`○` in the **`faint` (grey)
  colour, the same as the not-started `○`** (static `◐` when non-tty or
  `UZI_TUI_NO_BLINK=1`); the board and rail **micro-bars** blink `▰`/`▱` in the
  **tungsten** colour. Line ~276 (the web `◐` description) is unchanged and still
  correct.
- **`docs/cli.md` (~718-766)** describes the micro-bar in-progress cell blinking
  "in the wait colour" (~723) and the crew-rail milestone in progress (~765).
  Update the micro-bar color to **tungsten**, and note the rail checklist rows
  blink `◐`/`○` in the **faint/grey** colour (static `◐`).
- **Run `task docs:sync`** and commit the mirror under
  `api/internal/uzidocs/embed/`. `TestEmbeddedDocsMatchSource` (in `test:api`)
  byte-checks it, so a docs edit without the sync reddens `gate:api`.
- **`specs/ai.md`**: add one decision entry recording that this supersedes the
  TUI half of PRD #1064's D4 (rail rows return to a `◐` glyph, blinking `◐`/`○` in
  the faint/grey colour; micro-bars keep `▰`/`▱` but recolored to tungsten).
  `specs/human.md` is hygiene-only (this is presentation, not a new user-stated
  requirement).

## Milestones

- [ ] **M1 — Micro-bar recolor.** `milestoneCell` renders `tungsten` (not `wait`),
      recoloring both the crew-rail eyebrow micro-bar and the board per-run
      micro-bar; blink behavior unchanged. Comments referencing "wait colour"
      updated at the two call sites.
- [ ] **M2 — Rail rows to blinking `◐ ⇄ ○`.** The crew-rail milestone checklist
      renders every in-progress row as a `◐`/`○` blink in the **`faint`/grey** color
      (same as the not-started `○`), static `◐` under non-tty / `UZI_TUI_NO_BLINK`,
      replacing the `▰`/`▱` cell. The in-progress title keeps its brighter (plain fg)
      style. `✓`/`○`/`—` states and the `· <id>` eyebrow unchanged.
- [ ] **M3 — Tests flipped, ADDED, and pinned.** `tui_model_test.go` asserts the row
      `◐` (faint) by shape (distinct from the not-started `○`) and the tungsten (not
      wait) micro-bar static frame. **Add** the two new positive tests: a rail-row
      blink-alternation test (`blinkOn=true → ○`, `false → ◐`) and a rail-row
      Ascii/NO_COLOR shape test — flipping the static-frame assertions alone leaves
      SC1's alternation and SC4's row-Ascii unproven. `tui_blink_test.go` and
      `tui_detail_fixes_test.go` confirmed/updated. Each flipped or new assertion
      shown red on the old renderer, green on the new (mutation discipline).
- [ ] **M4 — Docs + specs synced.** `docs/run-activity.md` and `docs/cli.md`
      updated to tungsten + the rail `◐`/`○`; `task docs:sync` run and the embed
      mirror committed; `specs/ai.md` decision entry added.
- [ ] **M5 — Gate green, render verified.** `task gate:api` passes (fmt, vet,
      build, lint ratchet, deadcode, `-race` tests). The uxlab render harness
      (`api/cmd/uzi/uxlab`, per `.claude/rules/tui.md`) regenerated and confirmed in
      both light and dark PNGs: the rail in-progress row is a grey `◐`, the
      micro-bar in-progress cell is tungsten `▰`/`▱`, and the grey milestone `◐` is
      visually distinct from the `crewWaiting` lane-dot `◐` above it (R4). Confirm
      the env-gated `uxlab_gen_test.go` harness hard-codes no wait-color span. No
      web, CLI-command, API, or DB change in the diff.

## Success criteria

- SC1: In the crew-rail milestone checklist, an in-progress milestone renders as a
  `◐`/`○` blink in the **faint/grey** color (the same color as a not-started `○`),
  distinguished from a not-started row by the `◐` shape and its brighter title; `✓`
  stays sage, `○` stays faint.
- SC2: The crew-rail eyebrow micro-bar and the board per-run micro-bar render the
  in-progress cell in tungsten and still blink `▰`/`▱`.
- SC3: With `UZI_TUI_NO_BLINK=1` (or a non-tty/offline render), the in-progress
  row shows a static `◐` in the faint color (never a bare `○`, which — being the
  same color as a not-started `○` — would then be indistinguishable from one) and
  the micro-bar shows a static `▱` in tungsten.
- SC4: Under an Ascii / NO_COLOR profile the shapes still distinguish state (row
  `◐` vs `○` vs `✓`; micro-bar `▰` vs `▱`), i.e. the signal never depends on color
  alone.
- SC5: `task gate:api` is green, including the flipped tests, each demonstrated to
  fail on the pre-change renderer.
- SC6: Web (`RunView.tsx`, blue `◐`) and the `uzi run get` CLI (text `done`/`in
  progress`/`left`) are byte-unchanged.

## Decisions

- **D1 — TUI only; web and CLI unchanged.** Chosen 2026-09-05. Recoloring the web
  `◐` (currently `text-info`/blue) to match tungsten was considered and declined:
  the change is scoped to the TUI, and web/TUI diverging on the in-progress color
  is accepted. The CLI `uzi run get` renders milestone state as text words, not a
  glyph, so it is unaffected.
- **D2 — Done and in-progress micro-bar cells share tungsten; blink separates
  them.** On the lit (`▰`) frame the in-progress cell is now the same tungsten
  `▰` as a done cell, distinguished by the blink and by the empty (`▱`) off-frame
  rather than by hue. The user saw a before/after mock and accepted this trade-off
  in preference to a distinct in-progress color.
- **D3 — Keep the blink; do not remove motion.** Only the color of the micro-bar
  cell changes; the `▰`/`▱` tick is retained. The rail row also blinks (D4).
- **D4 — Rail row is a blinking `◐ ⇄ ○`, static frame `◐`.** The row uses the same
  `blinkOn` tick as the micro-bar. The static / reduced-motion / non-tty frame is
  the **filled** `◐` (the presence frame), not the empty `○`: since the row is the
  faint/grey color (D5), a static `○` would be identical to a not-started `○` in
  BOTH glyph and color. Rendering `◐` statically keeps the in-progress state legible
  by **shape** in every profile, honoring the same "shape survives NO_COLOR"
  contract the micro-bar's `▰`/`▱` relies on. This supersedes the TUI half of PRD
  #1064 D4 (which had retired `◐` for the TUI in favor of the parallelogram); the
  parallelogram survives only in the micro-bars.
- **D5 — The rail row is the faint/grey color, NOT tungsten; the two surfaces use
  different in-progress colors on purpose.** User decision 2026-09-05, walking back
  an earlier tungsten choice for the row. The `◐ ⇄ ○` row blink uses `faintC`, the
  same color as a not-started `○`, so the `○` phase reads as one of the row's
  not-yet-started siblings and the row reads as a single grey circle pulsing toward
  done. The micro-bar in-progress cell stays tungsten (D2). Rationale for the split:
  the micro-bar is a progress *bar* where in-progress is part of the tungsten fill;
  the checklist is a *todo list* where in-progress is a grey item animating toward
  its green `✓`. The in-progress row is told apart from a not-started row by its
  brighter (plain-fg) title, the `◐` shape, and the motion — never by glyph color.

## Risks

- **R1 — A test asserts the old glyph/color and passes vacuously if only deleted.**
  Mitigation: flip each assertion and prove it red on the old renderer (M3,
  mutation discipline). A negative assertion (`must not contain ◐`) that is simply
  removed hides a regression; it must become a positive assertion on the new glyph.
- **R2 — Blink as a reduced-motion / accessibility concern.** The row now blinks
  too (previously only the micro-bar cell did). Mitigation: the existing
  `UZI_TUI_NO_BLINK=1` opt-out and the static-frame path cover the row cell as
  well (SC3); one cell per in-progress milestone, 1 Hz, is the same cadence the
  micro-bar already used.
- **R3 — Docs drift reddens `gate:api`.** The embedded docs mirror is byte-checked.
  Mitigation: `task docs:sync` and commit the mirror in the same change (M4).
- **R4 — `◐` now means two things in the same crew rail.** The rail already draws
  `◐` as the `crewWaiting` **lane dot** (`tui_lanes.go:399`, colored `wait`/blue) in
  the lane rows; this PRD adds `◐` as the in-progress **milestone** row (faint grey)
  in the MILESTONES block just below. Under color they differ by hue and by
  sub-block, but under an Ascii/NO_COLOR profile both are a bare `◐`. This is a
  latent legibility overlap, not a correctness bug (the two live in different
  labeled sub-blocks). Mitigation: leave both as-is (neither is worth churning), but
  M5's render check and a `tui-ux` pass must eyeball the rail with a lane in
  `crewWaiting` beside an in-progress milestone, in both a color and an Ascii frame,
  and confirm the two `◐`s are not confusable. If they are, that is a follow-up, not
  a blocker on this change.

## Constraints

- **No `.github/workflows/**` changes** in the implementation *or* the validation.
  The uzi worker PAT lacks `workflow` scope, so any workflow-file touch in the
  branch diff is an atomic push rejection that loses the whole branch
  (`.claude/rules/prds.md`). This change needs none.
- The uzi worker has no open-web egress. This PRD is fully self-contained: every
  fact above (palette hex values, glyphs, file/line anchors, test locations, doc
  locations) is read from this repo's own source and needs no internet lookup to
  implement or verify.

## Out of scope

- Recoloring the web `◐` (D1).
- Recoloring the board second-line `· <id>` in-progress id text
  (`tui_board_rows.go` ~487, still `pal.wait`).
- Any change to the "now" line, the crew rail's lane rows, or the milestone
  count/summary logic.
