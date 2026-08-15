# PRD #325: TUI redesign — factory shift board

**GitLab Issue**: [vtmocanu/uzi#325](https://gitlab.example.com/vtmocanu/uzi/-/issues/325)
**Status**: Draft, 2026-08-15. Design validated interactively by the user against a runnable prototype (see **Design reference**); this PRD ports that prototype into the shipped TUI. **Three independent code-verified reviews (architect + reviewer + fact-checker) folded in 2026-08-15** — 2 blocking security/correctness findings and 2 refuted factual claims corrected below; see the review-derived items marked `[review]`.
**Priority**: Medium (developer-facing polish; the TUI is the primary local driver of runs alongside the web UI)
**Created**: 2026-08-15
**Related (shipped code, at `d3aded87`)**: `api/cmd/uzi/tui.go`, `tui_board.go`, `tui_detail.go`, `tui_review.go`, `tui_render.go`, `tui_keys.go`, `tui_lanes.go`, `tui_steer.go`, `tui_d7_guard_test.go`, `tui_model_test.go` (bubbletea v2 + lipgloss v2)
**Related (design reference, on branch `feature/tui-redesign`)**: `api/cmd/uzi/uxlab/` (render harness), `api/cmd/uzi/uxlab/factoryui/` (the redesign as runnable render funcs + model), `api/cmd/uzi/uxlab/demo/` (`go run ./demo`), `api/cmd/uzi/uxlab/png/` (light+dark mock screenshots), `api/cmd/uzi/uxlab_gen_test.go` + `uxlab_mock_test.go` (frame generators)
**Follow-live parity reference**: `web/src/lib/useFollowScroll.ts` (the web's `useFollowScroll`: `paused` on scroll-up, `newCount` = "N new below the fold", `jumpToBottom` to re-attach, plus `useReconnectingBanner`) and `web/src/components/ActivityFeed.tsx` (the opt-in "Follow live" checkbox, default OFF, wired via `useTailOnAppend`). The web already implements exactly the tail/detach model this PRD proposes for the TUI — M5 should match it. `/api/ws` streaming is done by a parent hook, not `ActivityFeed` itself.

> Every file:line citation below was derived at **`d3aded87`** (the shipped TUI) unless
> marked as a `feature/tui-redesign` reference. A line number without a SHA is not a
> citation — re-derive before acting.

## Design reference (read this before implementing)

The redesign is **already prototyped and user-validated**. It is not a fresh design task.

- Runnable: `cd api/cmd/uzi/uxlab && go run ./demo` — the redesigned board + detail on the
  real bubbletea loop, seeded with fixtures (no server/DB/auth), in the terminal's real theme.
- Static: `api/cmd/uzi/uxlab/png/mock-*.png` (light + dark) — the same design rendered offline;
  `factoryui/` is the single source of truth for both, so demo and screenshots cannot drift.
- The design was iterated with the user across several rounds; the interaction model and the
  visual direction below are the **frozen** outcome of that, not proposals to re-open.

The implementer's job is to **port `factoryui`'s design into the shipped `tui_*.go`** so the
real TUI (which talks to the live API) looks and behaves like the prototype. `factoryui` is a
demo re-implementation over fixtures; the shipped views are separate code and must be changed
directly.

**`[review]` factoryui is a fixture-only prototype and omits load-bearing shipped behaviour** —
the steer state machine (`tui_steer.go`), the ownership gate on steer/approve actions, D8
degradation, seq-dedup of stream frames, the limit-wait park line, and (critically) D7 control-
byte sanitization of untrusted cells. A **literal** port therefore drops working, security-
load-bearing code. Port the *design* (layout, colour, interaction), not the prototype's code
verbatim; each milestone below calls out the shipped invariant it must preserve.

## Problem

The shipped TUI reads as undifferentiated grey text. Concretely, each cited against `d3aded87`:

- **No semantic status colour.** The board renders status as plain text (`tui_board.go:186`);
  the detail header wraps the line, status included, in one brand-blue style
  (`tui_detail.go:183`). `failed` / `completed` / `running` / `awaiting_approval` are visually
  identical. The palette (`tui_render.go:114-132`) has no run-status colour axis at all: it is
  brand blue + grey + a purple `boxTitle` + black/white `sel` + **three distinct crew-lane dot
  hues** (green/orange/blue) across five states (idle and done both alias to grey). A status
  board's first job is colour-coded status.
- **The plan gate does not announce itself.** `awaiting_approval` looks like a normal running
  view plus one blue word; approve/reject sit in the same faint grey as every other hint
  (`tui_steer.go:191-195`). The one moment that requires a human is not emphasised.
- **Judge verdict has no severity colour** — `issues` renders in the same brand-blue as `ideal`
  (`tui_review.go:179`; verdict enum `{ideal,ok,issues}`).
- **Inline code renders alarm-red** (glamour v2.0.1 stock style, xterm-256 colour 203 `#ff5f5f`,
  in **both** dark and light), e.g. `pollInterval` — reads as an error in a status UI
  (`tui_render.go:36-52`).
- **Admin board omits OWNER** — the factory-wide board shows the same columns as own-runs
  (`tui_board.go:181-188`); `OwnerEmail` is loaded (`apitypes/run.go:303`) and used by the plain
  `uzi admin` tables (`admin.go:67`) but never shown in the TUI board.
- **Board lacks triage columns** — no age (`relAge`, `run.go:1652`, is used at `tui_steer.go:156`
  but unused on the board), no MR/judge marker; the HEALTH column is empty for health `""`/`ok`
  (`tui_board.go:202-207`, applied at `:187`).
- **Empty state is a dead end** — an early return prints "no runs to show" and returns before the
  footer (`tui_board.go:174-177`), offering no guidance on how to start a run.
- **Detail navigation is unintuitive.** left/right switches the selected agent and up/down
  scrolls (`tui_detail.go:149-160`, `tui_keys.go:43-54`); there is no pane-focus model, so you
  cannot move between agents with up/down. The transcript is **top-anchored** (`scroll` = offset
  from the top, `tui_detail.go:286-293`) with no live-follow (tail -f) affordance.
- **The detail keybinding hint region occupies two lines** — a steer-bar key list
  (`tui_detail.go:234`) then a separate nav footer (`:235`). (`[review]` two explicit render
  calls, not a width-driven wrap; M4 collapses them to one line.)

## Solution

Port the `factoryui` "factory shift board" design into the shipped views. The frozen design:

- **Board**: a solid status colour chip + a left colour spine you scan in one pass; a top
  summary bar that answers "does anything need me?" (`N runs · N needs you · N stalled`) before
  you read a row; an AGE column (from `RunDTO.CreatedAt`); a judge verdict marker
  (⚖ ideal/ok/issues) **on the own-runs board only**; OWNER shown on the admin (all-crews) board;
  a useful empty state that keeps the footer and guides the user.
- **Detail plan gate**: an unmissable amber `PLAN GATE` banner on its own line with promoted
  `[y] approve` `[n] reject` keycaps; detail-header status chips using the same semantic palette.
- **Detail navigation (focusable panes)**: `←`/`→` (and `tab`) select/cycle the focused pane
  (crew rail ↔ transcript); `↑`/`↓` (`j`/`k`) act within the focused pane (move between agents
  when the rail is focused, scroll when the transcript is focused). **Detail opens focused on the
  crew rail** (the leftmost pane); the focused pane is visually distinguished (bright title + a
  bar; the other dimmed). Keybinding footer fits on **one line**.
- **Follow-live (tail -f)**: a live run auto-tails the focused transcript with a green
  `● FOLLOWING` badge; scrolling up detaches to an amber `⏸ PAUSED ↓N new` (N = frames below);
  `g` re-attaches and jumps to newest, and scrolling back to the bottom also re-attaches. This
  mirrors the web's `useFollowScroll` model (`[review]` parity reference above).
- **Judge review overlay**: severity-coloured verdict (red `issues`, etc.) and the inline-code
  fix so code spans stop reading as errors.

**Semantic status → colour, the full map** (`[review]` — required so M2/M3 are deterministic;
the run lifecycle has more states than the six colour buckets):

| Run status | Bucket | Colour |
|---|---|---|
| `running` | running | green |
| `awaiting_approval` | needs-you | amber |
| `awaiting_input` | needs-you (input) | amber (distinct banner — see M3) |
| `limit_wait` | paused | cyan |
| `queued`, `claimed` | queued/pending | blue (or grey) |
| `completed` | completed | teal |
| `failed` | failed | red |
| `unknown` / unrecognised (`docs/cli.md:721`) | default | grey |

- **Status-vs-health precedence on the single-colour spine** (`[review]`): a `running` run with
  `Health != ok` (e.g. `boardHealth` "stalled", PRD #47) must resolve to one colour. **Decision:
  health "stalled" overrides the status bucket → orange spine/chip**, because the whole point of
  the spine is triage ("what needs me") and a stalled run does. Non-stalled health does not
  override. M2 states this and the summary-bar predicates use it.
- **Summary-bar count predicates** (`[review]`): `needs you` = count of `awaiting_approval` +
  `awaiting_input`; `stalled` = count of runs whose health is stalled; `N runs` = total shown.
- **NO_COLOR / limited-terminal fallback** (`[review]`, D3): the spine and the plan-gate banner
  carry **colour-only** signal (a background fill with no text); under `NO_COLOR` or an Ascii
  colour profile lipgloss strips the fill and that signal vanishes (chip *text* like "running"
  survives; the spine and the amber do not). The design must add a text-marker fallback (a glyph/
  letter in the spine column; a bordered banner that does not depend on fill) so SC1 (spine) and
  SC2 (unmissable gate) hold without colour. Honour `NO_COLOR`.

## Milestones

Ordering: **M2 lands the semantic status seam** in `tui_render.go` (enumerated below) that M3/M4/
M6 consume, so it goes first. Board (M2) and the detail track then proceed largely in parallel
(disjoint files); the detail track (M3 → M4 → M5) is sequential because all three edit
`tui_detail.go`; M6 runs after M2 because both touch `tui_render.go`.

**`[review]` Test discipline for every milestone (B2):** each milestone carries the test edits
for the behaviour it changes — a milestone that alters a rendered string also fixes the tests
that assert it (e.g. M2 breaks `TestTUIAdminBoardIsLabelledActiveRuns`, `tui_model_test.go:127`,
which asserts "active runs"). Milestones do NOT land test debt for M7. Each of M2-M6 also carries
at least one **automatable** assertion via the repo's `Update→msg` / `View()→string`
substring/SGR-escape seam (as in `tui_render_test.go` / `tui_model_test.go`), so CI gates
something per milestone rather than only at M7. Run `task gate:api` (build + lint-ratchet +
deadcode + test), not just build, as each milestone's acceptance.

- [x] **M1 — Land the uxlab render harness** (already built on this branch, untracked). Commit
      `api/cmd/uzi/uxlab/` + `uxlab_gen_test.go` + `uxlab_mock_test.go`. This is the
      "agents can see the TUI" deliverable: `cd api/cmd/uzi/uxlab && devbox run build` renders
      every screen × state × {dark,light} to PNGs via charmbracelet/freeze, driving the real
      model offline (no server/PTY). The gen path drives the **shipped** `tuiModel`, so it
      renders the real TUI before AND after this PRD; keep it pointed at the shipped views so it
      becomes the regression surface for M2-M6. (`png/` and `frames/` stay gitignored.)
      **`[review]` Acceptance: `task gate:api` green** — not just `go build`. `uxlab/` enters the
      **api Go module**, which is held at **deadcode ZERO** (empty baseline) under a **whole-files
      lint ratchet** (every line of the new files is "new"). Any exported `factoryui` symbol not
      reached by `uxlab/demo`'s `main` or a `-test` reference fails `task deadcode:api`; any lint
      finding in the new files gates. M1 must prove deadcode + lint clean, since D1 keeps
      `factoryui` alive through M6.
- [x] **M2 — Semantic status seam + board redesign** (kept whole; seam enumerated so M3/M6 can
      start against it). **Seam, in `tui_render.go`** (`[review]`, from factoryui's consumers):
      extend the `palette` struct (`:104-112`) with a `status` lookup (`statusColor(status,
      health) color.Color` applying the precedence rule above), a `chipFg` colour, and a
      `chip(text, bg)` renderer — all populated in `newPalette`. M3 (detail chips) and M6
      (verdict severity) only **read** these, so neither reaches back into
      `tui_render.go`. The crew-lane dot palette (`states`, `:124-130`) is a **separate** axis
      that already carries green/amber/blue — M4's rail needs little change. **Board, in
      `tui_board.go`**: status chip + left colour spine (with the NO_COLOR text-marker fallback),
      the `N runs · N needs you · N stalled` summary bar (predicates above), an AGE column (from
      `CreatedAt`), the ⚖ judge marker **on the own-runs board only**, OWNER on the admin board,
      and a non-dead-end empty state that keeps the footer. **`[review]` Keep the admin honesty
      label**: the admin board is deliberately "active runs (factory-wide)" — `AdminListRuns`
      returns non-terminal runs with no judge/usage columns, enforced by
      `TestTUIAdminBoardIsLabelledActiveRuns`; do not relabel it "all crews", and note the ⚖
      marker cannot appear on the admin board at all (admin rows carry no `JudgeVerdict`), so the
      admin column budget is OWNER + AGE only. **`[review]` D7 (blocking, B1): sanitize OWNER.**
      Route `OwnerEmail` through `m.renderer.Plain` = `capCell(cellText(s), n)` (`cellText` is the
      control-byte stripper; bare `capCell` is NOT sufficient). Add `OwnerEmail` to
      `d7UntrustedFields` (`tui_d7_guard_test.go`) and extend
      `TestTUIViewsStripControlBytesFromUntrustedText` to drive the **admin** board with a hostile
      `OwnerEmail` (`\x1b` / `‮` / `\x07`), asserting the control bytes are absent from the
      frame. The clean-fixture harness cannot catch this (golden-fixture trap — see Risks), so the
      test is the only guard. Regenerate the M1 screenshots and confirm the board matches
      `mock-board-*` / `mock-board-admin-*`.
- [x] **M3 — Plan-gate banner + detail header status chips.** In `tui_detail.go` / `tui_steer.go`:
      render the amber `PLAN GATE` banner on its own line at `awaiting_approval` with promoted
      `[y]/[n]` keycaps (with the NO_COLOR bordered-banner fallback), and switch the detail header
      to semantic status chips (consumes M2's seam). **`[review]` Two distinct banners (S3):**
      `awaiting_approval` → plan-gate banner (y/n approve/reject); `awaiting_input` (PRD #88, a
      clarification park) → a *follow-up* banner, NOT y/n — factoryui conflates the two
      (`needsHuman = approval || input`), which would tell `awaiting_input` users to press keys
      that do nothing. The shipped `atPlanGate` is `awaiting_approval`-only; preserve that.
      **`[review]` Ownership (N1):** the banner is informational and shows regardless of
      ownership, but the promoted `[y]/[n]` keycaps are owner-gated (`steerAccess == steerAllowed`,
      `tui_steer.go:169`) — reconcile (dim/omit the keycaps for non-owners). Matches
      `mock-detail-awaiting-approval-*`.
- [ ] **M4 — Focusable-pane detail navigation.** In `tui_detail.go` / `tui_keys.go`: `←`/`→`
      (and `tab`) select/cycle the focused pane; `↑`/`↓`/`j`/`k` act within it (agents vs scroll);
      default focus = crew rail; focused pane visually distinguished; **one-line footer**.
      **`[review]` Preserve steer/overlay key first-refusal (N2):** the overlay/steer bar take
      keys before lane nav (`tui_detail.go:125-130`) and `steerTyping` swallows all keys incl.
      `tab`/`←`/`→` (`tui_steer.go:249-257`) — free as long as M4 does not reorder that. Do not
      over-invest in the top-anchored scroll model here; M5 replaces it. Matches
      `mock-detail-focus-rail-*` / `mock-detail-focus-transcript-*`.
- [ ] **M5 — Follow-live (tail -f) transcript.** OQ1 is resolved: the shipped detail already
      consumes the live `/api/ws` stream (`tui.go:158-164` → `internal/uzicli/stream.go`), so M5
      does **not** touch ingestion. **`[review]` Real scope (S5): invert the transcript windowing**
      from top-anchored (`out[m.detail.scroll:]`, `tui_detail.go:286-293`) to **bottom-anchored +
      follow**, matching factoryui (`render.go:321-335`, `model.go:226-261`) and the web's
      `useFollowScroll`. Add follow-mode state coupled to M4's scroll handling: `● FOLLOWING`
      auto-tail, detach to `⏸ PAUSED ↓N new` on scroll-up, `g` to go live (jump to newest),
      re-attach on scroll-to-bottom, with the viewport/extent clamp. `g` chosen because `f` is
      already follow-up. Matches `mock-detail-running-*` / `mock-detail-paused-*`.
- [x] **M6 — Judge review overlay severity + inline-code fix.** In `tui_review.go`, colour the
      verdict by severity (consumes M2's seam); in `tui_render.go`, retune the glamour style so
      inline code no longer renders alarm-red (it is colour 203 in both dark and light today).
      Runs after M2 (shared `tui_render.go`). Matches `mock-review-*`.
- [ ] **M7 — New tests, docs, CHANGELOG** (`[review]` shrunk per B2 — per-milestone test edits
      live in their milestones). Here: genuinely-new tests (follow-live attach/detach, pane-focus)
      and the B1 security test if not already landed in M2; **rewrite** the now-stale nav
      description in `docs/cli.md:352-392` (fix-the-doc: the current "left/right switches agent,
      up/down scrolls" prose becomes false after M4) and cross-check `docs/cli.md:721`'s
      `unknown`-status note against the new palette's default bucket; add a CHANGELOG
      `[Unreleased]` entry.

## Decisions

- **D1 — factoryui's fate after the port** (`[review]` updated: **lean retire at M7**). Keeping
  `factoryui/` + demo after the shipped views match creates a second implementation of the same
  views with **no differential test binding it to the shipped output** — it will drift and
  mislead. Keep it through M6 (it is the frozen spec each milestone is checked against), then at
  M7 retire it, leaving only the shipped-model harness (`uxlab_gen_test.go`) as the regression
  surface. Retiring means also removing `uxlab_mock_test.go` (imports `factoryui`) and
  `uxlab/demo` (a dangling import breaks the build; a plain deletion cannot redden deadcode).
  Confirm at M7 before deleting.
- **D2 — Port granularity: incremental** (this plan), not a wholesale swap to `factoryui`'s
  functions. A wholesale swap would delete the shipped steer machine, ownership gate, D8
  degradation, seq-dedup, limit-wait park line, and D7 sanitization (all absent from factoryui).
  Incremental keeps each milestone reviewable and lets the harness prove each step against the
  mocks.
- **D3 — NO_COLOR / limited-terminal fallback: designed now, not deferred.** The spine and
  plan-gate banner are colour-only fills that vanish under `NO_COLOR`/Ascii profile. Fallback:
  a glyph/letter in the spine column and a bordered banner independent of fill (see Solution).
  SC1/SC2 depend on it, so it is scope, not polish.

## Open questions

1. **RESOLVED** — the shipped detail view consumes the live `/api/ws` stream (`tui.go:158-164` →
   `internal/uzicli/stream.go:244-260,308-318`), so M5 is a UX affordance over an existing stream,
   not an ingestion change. (The PRD draft reached this via a wrong indicator: the shipped view
   shows a faint-grey "live" word on a transport line at `tui_detail.go:212`, **not** a "● live"
   header badge — the ● badge is factoryui's.)
2. **Admin column budget** (`[review]` shrunk): since the admin list carries no `JudgeVerdict`, the
   ⚖ marker cannot appear there, so the width question is OWNER + AGE only. Confirm the column set
   degrades gracefully (drop least-important first) at ~100 cols; verify via the harness.

## Parallelization plan

| Phase | Milestones | Depends on | Files touched (distinct) |
|---|---|---|---|
| 1 | **M1** (harness, commit + gate-green) · **M2** (seam + board + OWNER sanitization) | — | `uxlab/**` + gen tests (M1) vs `tui_render.go` + `tui_board.go` + `tui_*_test.go` (M2) |
| 2 | **M3** (plan/​input banners + detail chips) · **M6** (review severity + inline code) | M2 seam | `tui_detail.go`/`tui_steer.go` (M3) vs `tui_review.go`/`tui_render.go` (M6) |
| 3 | **M4** (pane nav) → **M5** (follow-live windowing) | M3 (same file) | `tui_detail.go` + `tui_keys.go` (sequential — same file) |
| 4 | **M7** (new tests + docs + CHANGELOG + retire factoryui) | M1-M6 | test files + `docs/cli.md` + CHANGELOG |

M1 ∥ M2 (disjoint trees). M3 ∥ M6 are file-disjoint **only because M2 lands the complete seam** in
`tui_render.go` (S1) so M3 never edits that file; M6 also edits `tui_render.go`, so it is sequenced
after M2. M4→M5 are sequential (both edit `tui_detail.go`).

## Risks and mitigations

- **`[review]` Golden-fixture trap hides the D7 regression.** Both the shipped-model harness and
  the factoryui mocks use clean fixtures, so `board-*` screenshots match whether or not OWNER
  sanitization survives the port. Mitigate: the B1 hostile-`OwnerEmail` admin-board test (M2) is
  the guard, not the screenshots.
- **`[review]` Gate-green risk from the api module's zero-tolerance.** `uxlab/` lands in the
  deadcode-ZERO, whole-files-lint-ratcheted api module. Mitigate: M1 acceptance is `task gate:api`
  (deadcode + lint), not build; keep every exported `factoryui` symbol reachable from `demo`/tests.
- **Piecemeal port drifts from the frozen prototype.** Mitigate: the M1 harness renders the
  shipped views, so every milestone regenerates screenshots and diffs them against `mock-*`.
- **Colour illegibility on limited terminals / NO_COLOR** (D3). Mitigate: text-marker fallbacks;
  verify at reduced colour profiles via the harness.
- **Follow-live races the live stream** (auto-scroll fighting incoming frames, PAUSED counter
  drift). Mitigate: model follow-mode as explicit state keyed off scroll position, coupled to
  M4; cover attach/detach transitions in tests (factoryui smoke tests + web `useFollowScroll` are
  references).
- **These are TUI/CLI-only changes** — no API, DB, worker, or web change — so blast radius is the
  `uzi` binary's interactive views. The compose/e2e/web paths are untouched.

## Success criteria

1. On the board, run status is colour-coded (chip + spine, with a text-marker fallback under
   `NO_COLOR`) with a distinct colour per state in both light and dark, and the top bar shows
   `N runs · N needs you · N stalled`; verifiable in the harness screenshots + a `View()`
   substring/SGR assertion, and by `go run ./uzi` against a live stack.
2. At `awaiting_approval`, the detail view shows the amber `PLAN GATE` banner with promoted
   approve/reject keycaps (owner-gated), and `awaiting_input` shows a distinct follow-up banner —
   verifiable via a `View()` assertion per state.
3. Detail navigation uses the focusable-pane model: `←`/`→` select the pane, `↑`/`↓` move within
   it, default focus is the crew rail, and the keybinding footer is one line — verifiable via an
   `Update→msg` / `View()` assertion.
4. A live run's transcript follows (tail -f, bottom-anchored) with a `● FOLLOWING` indicator,
   detaches to `⏸ PAUSED ↓N new` on scroll-up, and re-attaches with `g` or scroll-to-bottom.
5. The judge review overlay colours verdict by severity, and inline code no longer renders as an
   error colour.
6. A hostile `OwnerEmail` cannot emit control bytes into the admin board frame (B1 test passes);
   `OwnerEmail` is in `d7UntrustedFields`.
7. `cd api/cmd/uzi/uxlab && devbox run build` regenerates the full light/dark screenshot set from
   the shipped views, and the repo's Go gate (`task gate:api`: build + lint-ratchet + deadcode +
   test) stays green at every milestone.

## Amendments

### M2 review fixes (accepted, 2026-08-15)

M2 (`856d41c0`) passed the review wave — reviewer + tester. D7 OwnerEmail sanitization is
mutation-confirmed (dropping `Plain`/using `capCell`-only reddens the render guard at the right
line, vacuity guard sound); `task gate:api` green (independently run, 94s); NO_COLOR spine glyphs
survive the real Ascii/NoTTY downgrade; seam complete and read-only-consumable by M3/M6. Accepted
follow-up fixes, to land as a follow-up commit on `856d41c0` before M2 is closed:

- **F1 — admin OWNER alignment (reviewer, was framed blocking).** The admin OWNER cell is drawn
  via `Plain` (truncates, does not pad), so STATUS/HEALTH/AGE/TITLE go ragged row-to-row and miss
  the header (`tui_board.go:273`). Wrap in `padCell(m.renderer.Plain(strOr(r.OwnerEmail,""),
  boardOwnerWidth), boardOwnerWidth)`. (factoryui already pads owner to 18 — the shipped board is
  the one that regressed.)
- **F2 — column degradation at ~100 cols (reviewer; PRD Open Question 2).** No column drops/title
  truncation: a long owner+title renders up to ~134 visual cols at `m.width=100` and the terminal
  wraps. Truncate the TITLE (and/or drop least-important columns) so no row exceeds
  `boardRuleWidth`. Verify at width=100 via the harness.
- **F3 — header/data column alignment + stale comment.** Header row sits 1 col left of data rows
  (spine+gutter mismatch, present in factoryui too); align them. Fix the stale header comment
  (`tui_board.go:182-183`) that still describes the pre-HEALTH-column "4 spaces" layout. Keep
  factoryui in sync.
- **F4 — verdict todo count.** `verdictMarker` drops `JudgeTodoCount`; render the DTO's documented
  "⚖ issues · N" grammar when N>0 (`tui_board.go:319`, `apitypes/run.go:304-318`).

Spec correction (done in this amendment's commit range): `statusColor` returns `color.Color`, not
`lipgloss.Style` as §M2 first stated — the impl is correct (chip bg + M6 foreground both want a
`color.Color`), the §M2 line is fixed to match.

Observations, NOT changed (match the stated spec / not server-reachable): a terminal-status row
with `Health="stalled"` would still count as stalled and render orange — matches the "stalled
overrides" precedence and is not produced by the server (stalled is a running-heartbeat concept).
The D7 render guard is presence-class: it catches a direct raw/`capCell`-only draw of a fixtured
field, not a future neutralizing addition or a NEW untrusted field absent from `d7UntrustedFields`
— keep new untrusted board fields covered as they are added.

Hygiene note (not an M2 defect): both validators ran in detached worktrees under the shared
scratchpad and observed one being pruned externally mid-review (results unaffected, captured
first). Watch for cross-agent worktree pruning under the shared scratchpad.

### M3 + M6 review outcomes (accepted, 2026-08-15)

M3 (`fd3cb128`) and M6 (`1cce7e0b`) both passed their review waves with NO blocking findings.
- **M3** — reviewer + tester PASS. Two-banner branching mutation-confirmed (adding `awaiting_input`
  to `atPlanGate` reddens the input-distinct test); the non-owner approve-key guard is
  mutation-confirmed non-vacuous and defense-in-depth (`renderSteerBar` early-return AND `steerKey`
  gating, so leaked keycaps would be inert); seam consumed read-only; NO_COLOR banner border
  survives.
- **M6** — reviewer PASS. The inline-code retune is verified from glamour source NOT to mutate the
  shared package-global `StyleConfig` (value copy + pointer reassignment, not a deref-mutate), so
  other renderers are unaffected. Verdict severity mappings correct (teal ideal/ok, red issues,
  grey default); the board ⚖ marker is refactored onto the same `verdictColor` (byte-for-byte
  extraction, so board and overlay can't disagree).

Accepted nits and dispositions:
- **M3 header stalled cue is colour-only under NO_COLOR** → folded into M4 (which reworks the detail
  view): keep a text-marker/word cue for a stalled run in the detail header so it survives colour
  stripping (consistent with the board keeping health words).
- **M6 inline-code test lacks a light-theme control** → folded into M7's test pass (add a symmetric
  light control; the dark control + shared-source already mitigate).
- **M6 nit: `codeRed` negative could pass on a render-error fallback** → not reachable (the control
  proves the markdown renders); no action.
- The §M2 seam line's "verdict/confidence severity" was corrected to "verdict severity" — M6 colours
  the verdict only; confidence renders neutral (`faint`) by design.

### D1 resolved + M5 done + doc landed (2026-08-15)

- **D1 resolved (user decision): rebuild the demo on the shipped views.** Not "lean retire" (the
  draft default) and not "keep factoryui as a sandbox". M7 **retires `factoryui`** and **rewires
  `go run ./demo` to drive the real shipped `tuiModel`** with seeded fixtures + a simulated stream
  (so follow-live is drivable), so the interactive demo always mirrors what actually ships and the
  unbound-duplicate drift risk is removed. This grows M7's scope beyond the draft text (which said
  retire-only): M7 owns the demo rebuild, not just deletion of `uxlab_mock_test.go`/`factoryui`.
- **M5 landed** (`9e80c89f`): transcript is bottom-anchored with follow — `● FOLLOWING` auto-tail,
  `⏸ PAUSED ↓N new` on scroll-up (view held stable), `g` re-attach, terminal runs static. Windowing
  is a pure function of (lines, scroll, viewport) so the clamp matches the render. Under review.
- **Run-cost doc landed on this branch** (`5f3eb0e8`, cherry-picked): `docs/run-cost.md` +
  README/ARCHITECTURE pointers, explaining why hosted uzi runs cost less than a local agent-team
  run (structural, not model tier). Unrelated to the TUI change; bundled here at the user's request.

### M5 review outcome + fix (2026-08-15)

M5 (`9e80c89f`) passed a single reviewer pass (cost-conscious: follow-live is correctness, not a
trust boundary). Windowing verified sound — render and key-handler clamp share ONE layout
(`buildTranscriptLines` + `transcriptViewport`), so a stale scroll can't blank the view; `N` =
`maxTop − top` is exactly the lines below the fold (not off by one); re-attach is inclusive at
bottom; only live runs follow. One should-fix WITH a failing-execution repro, to land as a
follow-up commit:

- **F-M5a (resize-while-paused re-attach bug).** `detailKey` applies the scroll delta to the stale
  stored `m.detail.scroll` without clamping to the current `[0,maxTop]`, and the `WindowSizeMsg`
  handler (`tui.go`) never reclamps. Repro: 8 frames, height 16 (maxTop 26) → `k` → paused
  scroll=25 → resize height 20 (maxTop 22) → `k` (UP) → **follow re-arms (scroll jumps to 22, the
  live tail)** instead of scrolling to older output; also shows a bare `⏸ PAUSED` (N=0) until then.
  Fix: clamp `m.detail.scroll` to `[0,maxTop]` at the top of the transcript-scroll branch before
  applying the delta (or reclamp in `WindowSizeMsg`). Add a test for exactly this sequence
  (resize-taller while paused, then UP → follow stays false, scroll decreases).
- **F-M5b (test robustness, folded).** `TestTUIDetailFollowLive`'s bottom-anchored assertion is
  coarse — it pins `N`/`top` exactly (catches a top-side off-by-one) but wouldn't catch an end-side
  ±1 in the visible-line count. Add an assertion counting visible frames.

Nits left as-is: `g` on a terminal (non-live) run sets follow but renders no badge (harmless);
`paneTitleBadge` right-align can overflow at ~20 cols (unreachable in practice).
