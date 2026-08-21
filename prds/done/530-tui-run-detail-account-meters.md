# PRD #530 — TUI run detail: multi-account rate-limit meters under milestones

**Issue**: #530
**Priority**: Medium
**Status**: Complete (tui-ux narrow-rail legibility review is a post-merge human step, per M4)
**Surface**: `uzi tui` run **detail** view only (no web change, no API change)

## Problem

The TUI **factory-floor board** gained a multi-account rate-limit meter strip
(`boardRateLimitStrip`, landed as commit `e0b7d07d` = issue #519 / PR #528), mirroring the
web sidebar's account meters. The TUI **run detail**
view was not touched: it renders the milestone block (`renderMilestones`) but shows no
per-account headroom. So while watching a single run you cannot see how much 5h/7d budget
each of your accounts has left without leaving the detail view. The web already surfaces
account meters (the sidebar `SidebarRateLimits`); this brings the same information to the
TUI run detail, positioned directly under the milestones.

## Solution

Under the milestone block in the run detail crew rail (`renderLaneRail` in
`api/cmd/uzi/tui_detail.go`), render the same multi-account selection as the board and web
sidebar: the **default** token plus any token whose `SecretID` is in the user's
`sidebar_token_ids`, dropping tokens whose `Limits.Status != "ok"`, each shown with its 5h
and 7d windows as `label <bar> NN%`. This is a **presentation-only** change that reuses code
and data already present after #519.

### What already exists (verified against `origin/main`; board meters = commit `e0b7d07d`)

> Line numbers below are from the clone base and **drift** — re-verify each anchor by
> **symbol name** (grep the function) before editing, do not trust the `:NNN`.

- **Model already carries the data — no new fetch, no API/DTO change.**
  `tuiModel` has `rateLimits []apitypes.TokenRateLimitDTO` (`api/cmd/uzi/tui.go:153`) and
  `sidebarTokenIds []string` (`tui.go:154`), populated in `Init()` via `fetchRateLimitsCmd()`
  (calls `SelfRateLimits`) and `fetchSettingsCmd()` (calls `GetMySettings`, reads
  `sidebar_token_ids`). The detail view runs on the same model, so the data is in hand the
  moment the detail view is open; nothing new is fetched.
- **Selection + per-window rendering already exist and are reusable.**
  `boardRateLimitStrip()` (`api/cmd/uzi/tui_board.go:556`) holds the exact selection rule:
  keep `Limits.Status == "ok"` → `readable`; show a token iff `IsDefault || SecretID ∈
  sidebarTokenIds`; render a per-token label only when `len(readable) > 1`; empty selection →
  return `""` (render nothing). `rateWindowCell(label, w)` (`tui_board.go:595`) renders one
  window as `label <tone-coloured bar> NN%`, and a nil window as `label -`.
- **The milestone block placement is known.** `renderMilestones()` (`tui_detail.go:775`) is
  appended inside `renderLaneRail()` (`:600`) at two branches: the no-lanes branch
  (queued/just-claimed, `tui_detail.go:622`) and the with-lanes branch (`tui_detail.go:649`).
  The account meters go **directly after** the milestone block in both branches so "under
  milestones" holds whether or not any lane has appeared yet. Rail constants:
  `laneRailWidth=26` (`:18`), `milestoneTitleCap=22` (`:719`), the no-scroll invariant comment
  at `:48-51`.
- **D7 is already satisfied by reuse.** The token `Label` is already in `d7UntrustedFields`
  and is drawn through `renderer.Plain` inside the strip helper, so reusing that path carries
  the untrusted-text obligation with it. No new untrusted field is introduced.

### The one real design decision: the rail is narrow and does not scroll

`boardRateLimitStrip()` lays the accounts out **horizontally** and clamps to the full terminal
width (`clampVisual(strip, m.width)`). The detail milestone block lives in the **crew rail**, a
fixed ~26-column column (`laneRailWidth`, `milestoneTitleCap = 22`) that **does not scroll** —
there is already an explicit invariant that "a tall roster cannot push the milestone block below
the fold" (`tui_detail.go:49`). A full-width horizontal strip will not fit 26 columns, and adding
rows competes with the lane roster and milestone rows for the same non-scrolling height budget.

So the implementer must render a **rail-width, vertically-stacked** variant rather than reuse the
horizontal board strip verbatim:

- One account per line (its label as an eyebrow/prefix), with its 5h and 7d cells either on that
  line if they fit the rail width or stacked on two short lines. Reuse `rateWindowCell` for each
  window so the bar/percent/tone and the nil-window `-` stay identical to the board and remain
  legible under `NO_COLOR` (the `NN%` text is the fallback signal when the bar colour is stripped).
- Factor the **selection** out of `boardRateLimitStrip()` into a shared helper so the board and
  the detail view cannot drift on which accounts show. **The helper must also expose the
  `showLabel` bool**, not just the shown slice: `showLabel = len(readable) > 1` is keyed off the
  **readable** count, and a helper returning only `shown` would make the detail view recompute it
  from `len(shown)`, which diverges from the board whenever a readable-but-unlisted token exists.
  So return `(shown []apitypes.TokenRateLimitDTO, showLabel bool)` (or a small struct). The board
  keeps its horizontal renderer; the detail view gets a stacked renderer; both consume the one
  selection **and** the one `showLabel`.
- Do **not** modify `transcriptViewport()` (`tui_detail.go:1110`, chrome counted `:1117-1131`).
  That function sizes the **transcript pane** from **body** chrome only; the rail is a separate
  LEFT column that `joinColumns()` truncates to the transcript's height (it never feeds
  `transcriptViewport`). Editing it to "make room" for a rail block would shrink the transcript
  pane and desync the scroll clamp, breaking the full-height fill (issue #379). Instead, the
  account block **reads** `transcriptViewport()` (minus the rows the roster + milestones already
  used) and caps itself to what remains, so it truncates cleanly at a row boundary.
- Respect the no-scroll fold: the block is appended strictly **after** the milestone rows, and
  `joinColumns` truncates the rail from the **bottom** — so overflow eats the account block first
  and it **cannot** push milestones down. The real hazard is the block being **silently or mid-row
  truncated** when roster + milestones already fill the rail; it must drop whole account lines
  cleanly, never render a half-cut row.
- The stacked renderer must draw each token `Label` through `renderer.Plain` (as the board strip
  does) so the untrusted label stays control-byte-stripped in the new eyebrow/prefix.
- Empty/again-empty selection → render nothing (same as the board): no eyebrow, no blank line.

## Dependency / deploy note (resolved, not a worker task)

The meters populate only when the server answers `GET /api/me/settings` and `GET
/api/me/rate-limits` over the CLI `uzc_` Bearer token. The `/me/settings` GET route was moved to
`RequireUser` in **#519**; against a server that predates that (the settings fetch 401s, error
swallowed) the strip falls back to **default-token-only**, exactly as the board does. This is a
deployment ordering fact, not a code task: this PRD requires no server change, and #519 is already
on `main`. State it in the code comment where the strip is added; do not add a new route.

## Milestones

- [x] **M1 — Shared selection seam.** Extract the account-selection rule (status ok; default or in
  `sidebar_token_ids`; `showLabel = len(readable) > 1`) out of `boardRateLimitStrip()` into a shared
  `tuiModel` helper that returns **both** the shown tokens **and** the `showLabel` bool, with the
  board refactored to consume it and its existing tests still green (no behaviour change on the
  board). A test pins that board and detail derive the **same set and the same `showLabel`** from
  one fixture.
- [x] **M2 — Rail-width stacked account block in the detail view.** Render the selected accounts,
  vertically stacked at rail width, directly under the milestone block in both `renderLaneRail`
  branches, reusing `rateWindowCell`. Cap the block by **reading** `transcriptViewport()` (minus the
  roster + milestone rows already drawn) so it drops whole account lines cleanly rather than
  rendering a half-cut row; **do not modify `transcriptViewport()`**.
- [x] **M3 — Tests.** Drive the detail view through the model with seeded `rateLimits` +
  `sidebarTokenIds` and assert: a `status != "ok"` token is dropped; default + a listed token both
  show; an unlisted non-default token is hidden; the label appears only when `len(readable) > 1`;
  the nil window renders `-`; `NO_COLOR` keeps the `NN%` text in the **detail** block; empty
  selection renders no block and no stray blank line; and an over-full rail truncates the block at a
  whole-row boundary (never mid-row). Fold a hostile token `Label` into the existing
  control-byte-stripping detail test. Mirror the board patterns in `api/cmd/uzi/tui_ratelimit_test.go`.
- [x] **M4 — Demo + screenshot regen (worker-verifiable).** Extend the demo detail fixture so
  `uzi tui --demo` shows the account block on a run's detail view (the demo client already seeds
  `SelfMeters` + `SidebarTokenIds` for the board — reuse them), and regenerate the uxlab screenshots
  (`uxlab_gen_test.go`). This milestone is the worker's gate; the **tui-ux legibility review** of the
  narrow rail across light/dark and `NO_COLOR` is a **post-merge human step**, not a worker success
  criterion (an offline worker cannot self-complete a subjective visual review).

## Success criteria

- Opening a run's detail view shows, under the milestones, the same accounts the board and web
  sidebar show for that user (default + `sidebar_token_ids`, `status == "ok"`), each with 5h/7d
  bars + %, and nothing when the selection is empty.
- Board and detail view never disagree on which accounts show **or** on `showLabel` (shared
  selection helper returns both).
- The account block appears strictly **after** the milestone rows and truncates at a whole-row
  boundary on a short terminal (never a half-cut row); the transcript pane height is unchanged
  (`transcriptViewport()` untouched).
- `NO_COLOR` keeps the `NN%` text legible; untrusted labels are control-byte-stripped via
  `renderer.Plain`.
- `task gate:api` (fmt, lint ratchet vs `origin/main`, deadcode, `go test -race`) + the D7 guard
  green.

## Risks

- **Rail real estate.** The rail is small and non-scrolling; when roster + milestones already fill
  it, the appended account block is what gets truncated (it sits below them and `joinColumns` cuts
  from the bottom). Mitigation: cap the block against the remaining rail height read from
  `transcriptViewport()` and drop whole account lines, never a half-cut row; tui-ux validates on a
  short terminal post-merge.
- **Selection drift board↔detail.** Two copies of the selection rule — or of the `showLabel`
  computation — would diverge. Mitigation: the M1 shared helper returns both the set and
  `showLabel`; a test pins that both surfaces derive the same from one fixture.
- **Looks empty against an un-upgraded server.** Same 401-swallow fallback as the board (default
  only). Documented above as a deploy-ordering fact, not fixed here.

## Out of scope

- No web change (the web sidebar already shows account meters).
- No API/DTO/route/migration change.
- No new fetch or polling cadence (reuses the Init-time fetch; refresh follows the existing `r` key,
  as the board strip does).
- Per-account select-reason/headroom detail (that lives on `uzi run <id>`); this is the meter strip
  only.
