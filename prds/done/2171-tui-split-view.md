# PRD #2171: TUI split view, floor on top and CI or pulls below on tall terminals

**Status**: Done. Resolved facts below were read at `main` `43161ccf`; implementation and automated checks completed on issue #2171.

## Problem

A maintainer running `uzi tui` watches two things at once: runs on the floor and the CI runs or pull requests those runs produce. Today the TUI shows exactly one screen (`tuiModel.view`, `api/cmd/uzi/tui.go`), so following both means pressing `tab` or `1`/`2`/`3` back and forth. On a tall terminal the floor rarely fills the screen, so the missing information sits next to empty rows.

## Outcome

When the terminal is tall and wide enough, `uzi tui` splits horizontally: the floor in the top pane, a forge pane in the bottom pane showing CI by default and pulls on demand. One pane has focus at a time. The split turns off per session with a key and permanently with a config value.

Acceptance examples:

1. A 60×120 terminal, no config: `uzi tui` opens with one shared header (wordmark, floor tab, rate-limit meters), the floor in the top pane with focus, a separator line naming the bottom pane's tabs and scoped repo, the CI list in the bottom pane, and one footer. Both lists refresh on their own. `tab` focuses the bottom pane on CI, `tab` again switches it to pulls, `tab` again returns focus to the floor; `shift+tab` walks the same cycle backwards.
2. With the split showing and the floor focused, `enter` on a run opens run detail full-screen; `esc` returns to the split with the floor focused and its cursor where it was. Same for `enter` on a CI run or a PR from the focused bottom pane: `esc` returns to the split with the bottom pane focused on the same tab, cursor and repo.
3. The window shrinks below the split threshold while the bottom pane has focus: the TUI shows the floor full-screen. Growing back restores the split with the bottom pane on the tab, cursor and repo it had, floor focused. `s` collapses the split the same way and keeps it collapsed through later resizes until `s` is pressed again; while collapsed on a tall-enough terminal the footer shows a faint `s split` hint. With `[tui] split = "off"` in `~/.config/uzi/config.toml` the TUI behaves exactly as today at any size.

Collapsed restore hints and size notes yield to the existing footer when they do not fit; they never truncate keys or the version readout.

## Out of scope

- A vertical (side-by-side) split, more than two panes, user-resizable pane ratios, a configurable default bottom tab.
- Opening drill-ins inside a pane: every drill-in stays full-screen.
- Making the bottom pane follow the floor cursor's repo. The bottom pane keeps the existing single scoped repo (`R` cycles it when the bottom is focused).
- A slower refresh cadence for an unfocused bottom pane. The displayed tab keeps today's cadence (D6); a different cadence waits for measured forge load (see Acceptance).
- Applying `[tui] split` to `uzi tui --demo` / `--sketch`: they build the model without loading config, so they always use `auto` (the demo then shows the split on a tall terminal).
- Mouse focus.

## Modules and seams

### Height-parameterised rendering (`tui_board.go`, `tui_ci.go`, `tui_pulls.go`)

- `renderBoard` / `renderCI` / `renderPulls` today read `m.height` and each draws `tabStrip()` (`tui_board.go:538`, `tui_ci.go:352`, `tui_pulls.go:523`). They gain a body renderer that takes the pane height and whether to draw full-screen chrome; the full-screen path calls it with `m.height` and draws exactly today's frame.
- Capacity: `boardCapacityWith` (`tui_board.go:949`), `ciCapacity` (`tui_ci.go:435`), `pullsCapacity` (`tui_pulls.go:611`) gain pane-height variants. The zero-arg forms stay as full-screen wrappers (about ten test call sites use them). The key-handler scroll paths (`syncedScroll`, `ciSyncedScroll`, `pullsSyncedScroll`) must use the pane capacity of the pane they scroll. Invariant: the scroll math and the renderer use the same capacity for the same pane, focused or not, so the selected row (and its reserved second line) is never drawn outside its pane.
- `tabStrip()` (`tui_board.go:377`) reads `m.view` for three things: the admin "active runs" relabel (`:379`), the active-tab marks (`:386-388`) and the repo suffix (`:401`). It takes those as explicit inputs, so the split header can show the admin relabel while the bottom pane has focus and omit the repo suffix (the separator carries it).

### Split state and the focus invariant (`tui.go`)

- New `tuiModel` fields: session-off flag, config mode, bottom tab (`viewCI` or `viewPulls`, default `viewCI`), split-eligible latch.
- `m.view` keeps meaning "the screen with focus". With the floor focused it is `viewBoard`; with the bottom focused it is the bottom tab. Invariant: whenever `m.view` is `viewCI` or `viewPulls`, the bottom tab equals `m.view`. One setter enforces it and every path that sets `m.view` to a list uses it (`gotoCI`, `gotoPulls`, `exitToBoard`, the PR view's esc in `tui_pr.go`, the CI-run esc in `tui_cirun.go`), so `2` / `3` / `tab` can never desync the bottom pane from the focused list.
- `splitEligible`: config not `off`, session-off unset, latch on. `splitDrawn`: eligible, `m.view` is a list (`viewBoard`, `viewCI`, `viewPulls`), and no modal or help overlay (`quitting`, `showHelp`, `updatePrompt.showing`).

### Threshold and pane sizes

- Worst-case chrome, from named per-band constants rather than live state: shared header = wordmark/tab line + meters (2 lines max, `boardMeterLayout`) + vault hint (1) + admin-denied (1) + board error (1); floor pane chrome = pane heading + selected-row second line (1) + three band headings + two band spacers; bottom pane chrome = forge header note (1) + pane heading + selected-row second line (1) + three band headings + two band spacers; plus separator (1) and footer (1). The band allowance is threshold-only: live list capacities still count band headings and spacers as display items.
- `minHeight = sharedChrome + separator + footer + 2 × (max(floorPaneChrome, bottomPaneChrome) + 8)`. The split leaves at least 8 actual list rows per pane in the worst case.
- Hysteresis: the latch turns on when height ≥ `minHeight + 2` and off when height < `minHeight`. It also needs width ≥ 80 (the board footer's width). The latch updates only in the `tea.WindowSizeMsg` case (`tui.go:884`), never in `View`, so a cursor move, a new error line or a meter tick can never flip the layout.
- Pane sizes come from the rows left after the live shared header, separator and footer (the resize threshold still uses worst-case shared chrome): each pane gets its worst-case chrome + 8, the rest is halved, the odd row to the floor. Each pane's live chrome is subtracted inside its own fixed pane height.

### View composition (`tuiModel.View`, `tui.go:1571`)

When `splitDrawn`:

- **Shared header** once: wordmark plus only the floor tab (admin relabel kept), the meters band, the vault hint and board error lines as today.
- **Top pane**: the floor body in its pane height. Its own top line keeps the board's filter readout and summary as today.
- **Separator**: one line. In priority order (cut from the end at narrow widths, never cutting the first two):
  1. the focus marker when the bottom is focused;
  2. the bottom tabs in key order, `pulls · ci`, with the shown one in the title style;
  3. the bottom list's filter readout (`/text▌` while typing);
  4. its summary and position (`ciSummary` · `lo–hi`, the pulls twin), moved here from the list's brand line (`tui_ci.go:353-367`);
  5. the scoped repo name (forge-authored, through `m.renderer.Plain`, D7), ellipsised down to 16 columns before it is dropped;
  6. rule glyphs to fill the line.
- **Bottom pane**: the forge header note (rate-limit / error) and the list body in its pane height.
- **Footer**: the focused pane's hints with the version readout kept on every focus (today only `boardFooterLine`, `tui_board.go:732-753`, carries it). Split adds one hint, `tab pane`; `s split` appears only while the split is eligible-but-off. Drop order when the line is too long: `r refresh`, `h fold done`, `a factory`, `/ filter`; `? keys` and `q quit` are never dropped.
- **Focus marker**: the focused pane's label is wrapped in brackets (`[floor]` in the header, `[ci]` / `[pulls]` in the separator), in the title style. Exactly one bracketed label per frame. It does not use `▸`, which every list already draws as the row cursor (`tui_board_rows.go:262`, `tui_ci.go:488`, `tui_pulls.go:662`).
- **Unfocused pane**: rows keep full colour (the floor's NEEDS YOU amber and status glyphs never dim). The selected row loses its highlight band and bold and shows a hollow cursor `›` instead of `▸`; its reserved second line is still drawn (capacity parity).
- Modals (`quitting`, `showHelp`, `updatePrompt`) keep their current precedence over everything.

### Keys (`handleKey`, `tui.go:1504-1555`)

The split keys are handled in `handleKey` after the modal and `filtering()` checks and before the per-view switch, and only when `m.view` is a list screen. They are never global: run detail's steer and review inputs are not covered by `filtering()` (`tui_detail.go:467-476`). When `splitDrawn`:

- `tab`: floor → bottom:ci → bottom:pulls → floor (focus and bottom tab move together). `shift+tab`: the same cycle backwards. Today's full-screen `tab` cycle is floor → pulls → ci (`tui_board.go:359`, `tui_ci.go:867`); the split cycle visits CI first because CI is the default bottom tab (D3).
- `ctrl+w`: toggle focus between the panes (alias; some browser-hosted terminals intercept it, so it is never the only route).
- `esc` with the bottom focused: focus the floor (after `esc` has dismissed a filter, as today).
- `1`: focus the floor. `2` / `3`: set the bottom tab to pulls / CI and focus it.
- `R`: cycles the scoped repo only with the bottom focused.
- `?`: help lists the split keys.

`s`, on the list screens whether split or not:

- with the split drawn: session-off, collapse to the floor full-screen;
- session-off: clear it; the split returns if eligible;
- latch off (too short or narrow) and session-off unset: no state change, footer note `terminal too small to split` until the next key, when it fits alongside the existing keys and version readout;
- config `off`: no effect at all (config wins).

When the split is not drawn, every other key behaves exactly as today.

### Collapse and return

- **Collapse**: when the split stops being drawn because the latch turned off or `s` set session-off, while a list screen has focus, the TUI shows the floor full-screen focused (`m.view = viewBoard`), D4. An open bottom filter is committed first (`filtering = false`, text kept, as `enter` does), so the pane never reopens mid-input. The bottom tab, cursor, scroll, filter text and repo stay in state.
- **While collapsed**, the full-screen screens behave as today; if the user focuses CI or pulls full-screen (`2` / `3` / `tab`) and the split later returns, it opens with that list focused in the bottom pane (it follows from `m.view`). D4 applies at the moment of collapse only.
- **Drill-in return**: run detail, the PR view and the CI-run view record whether they were opened from a drawn split (`fromSplit`, carried through the run ↔ PR jumps `u` / `m`). On `esc`:
  - opened from the split and the split is eligible: the split, with the originating pane focused on its tab, cursor and scroll (the existing targets already name the pane: `detailReturn` defaults to the floor, `prReturn` to pulls, CI-run esc to CI, `tui_cirun.go:207-211`);
  - opened from the split and the split is no longer eligible: the floor full-screen (D4);
  - opened unsplit: the existing target, unchanged.

### Repo scope in split mode (`tui.go`, `tui_pulls.go`)

- Today the default repo is resolved only by `gotoPulls` / `gotoCI` and the `reposMsg` switch on `m.view` (`tui.go:1002-1013`), and `pullsRepoReady()` needs `repoChosen` (`tui_pulls.go:257`). With the floor focused the bottom pane would never resolve a repo and would sit on loading.
- So the `reposMsg` switch keys on "displayed" instead of `m.view`, and the split becoming drawn (startup, a latch-on resize, `s` on) resolves the repo and starts the shown list's fetch. The `WindowSizeMsg` case returns that Cmd; today it returns nil.
- `resolveDefaultRepo` (`tui_pulls.go:302-307`) sets `repoChosen` on its first call, so it must not run before the board can inform it. Split activation calls it only once both the repos reply and the first board reply (success or failure) have landed; whichever lands second triggers it. Until then the bottom pane shows its loading state. There is no later re-resolution, so no cache invalidation is needed. An `R` press before that sets `repoChosen` as today and the deferred call becomes a no-op (D9).

### Polling (`tui.go` tick handlers)

- The CI and pulls tick chains are armed at Init (`tui.go:529,533`) and keep ticking. Their fetch gates and the repo self-heal blocks (`tui.go:1033`, `:1047`, `:1081`, `:1093`) change from "this view has focus" to "this list is displayed": focused full-screen, or the bottom tab while `splitDrawn`.
- So the hidden bottom tab does not poll, neither list polls behind a full-screen drill-in, and neither polls while the quit, help or update modal shows (today only the board, strip and skew ticks pause for the update prompt, `tui.go:922-925`).
- Every existing guard stays: request generation (`reqSeq` / `waitID`), in-flight, `tickGen` chain, error-streak backoff (`ciTickInterval` / `pullsTickInterval`), stale-reply drop, `reposInFlight`. A bottom-tab switch starts the newly shown list's fetch the way switching screens does today. The board poller is unchanged.

### Config (`api/internal/uzicli/config.go`)

- `Config` gains `TUI TUIConfig \`toml:"tui,omitempty"\`` with `Split string \`toml:"split,omitempty"\``. Accepted: `""` / `"auto"` (default) and `"off"`.
- Every CLI write path round-trips the whole `Config` through `LoadConfig` / `SaveConfig` (`context.go:79/285/340`, `login.go:168`), and the TOML struct round-trip drops unknown tables, so the field must exist on the struct or `uzi login` / `uzi context …` would silently erase a user's `[tui]` table.
- `uzi tui` (the client path, not `--demo` / `--sketch`) validates it before opening the screen: any other value exits 2 naming `config.toml`, the key and the accepted values. No other command reads it.

### Docs

`docs/cli.md` "Watching runs live: `uzi tui`" (line 1202) gains the split, its keys, the threshold behaviour and the config key; then `task docs:sync`. `CHANGELOG.md` `[Unreleased]` line.

No new API route, DTO, migration, forge call shape or privilege. The only server-visible change is that the displayed bottom tab's existing list endpoints are called while the floor has focus (D6). No file under `.github/workflows/` is touched.

## Testing decisions

All automated gates are at the `Update→msg` / `View()→string` seam (`tui_model_test.go`, `tui_render_test.go`, `tui_ci_test.go`, `tui_pulls_test.go` patterns; split tests in a new `tui_split_test.go`). Screenshots cannot gate.

- **Threshold**: at `minHeight - 1`, `minHeight`, `minHeight + 1`, `minHeight + 2`, growing from below and shrinking from above, the second pane appears or not per the latch; width 79 never splits. At exactly `minHeight` with the latch on, moving the cursor onto a row with a second line, setting `board.err`, showing the vault hint and two meter lines never collapses the split, and each pane still has ≥ 8 list rows. Rendered lines never exceed `m.height`; the header appears once.
- **Capacity parity**: per list, focused and unfocused, scrolling to the last row keeps the selected row and its second line inside the pane's slice of `View()` lines.
- **Keys**: the `tab` / `shift+tab` cycle, `ctrl+w`, `esc` from the bottom, `1` / `2` / `3`, `R` only with the bottom focused; the bottom tab and `m.view` never disagree after any sequence; filter input and the quit / help / update modals capture keys first; `s` in run detail's steer input types an `s`; existing unsplit key tests pass untouched.
- **`s`**: drawn → off → collapsed, survives a grow; off → on restores; too small → no state change plus the note; config `off` → no effect; eligible-but-off footer shows `s split`.
- **Collapse and return**: shrink with the bottom focused and a CI filter open → floor focused, filter committed, bottom state kept on regrow; drill-in `esc` from each pane while eligible restores that pane; split-origin drill-in `esc` after a collapse → floor; unsplit drill-in `esc` unchanged; run → PR → run (`m`, `u`) from the split returns to the split.
- **Polling** (follow `tui_poll_guard_test.go`, `tui_poll_backoff_test.go`): floor focused and split drawn, a `ciTickMsg` fetches; the hidden tab's tick does not; neither fetches behind run detail or with the update prompt / help / quit modal showing; a stale `ciMsg` after `R` is dropped; split activation with no repo chosen fetches repos and then CI; with repos landing before the first board reply and the newest board run on a non-first repo, the bottom pane resolves to that run's repo; with the board reply failing, to the first enabled repo; `R` before the board reply keeps the user's repo.
- **Header and footer**: with the admin board and the bottom focused, the header reads "active runs"; with the server version ahead and the bottom focused, the footer carries the skew readout; at 80 columns the split footer and separator fit on one line each and contain `q quit` / the bracketed focus label.
- **Monochrome**: through the Ascii colorprofile, exactly one bracketed focus label per frame, in the focused pane; the unfocused pane's selected row carries `›`, never `▸`; an awaiting run in the unfocused floor keeps its amber SGR in the truecolor frame.
- **D7**: the separator's repo name and every pane cell go through `m.renderer.Plain`; extend the hostile-value render test (`tui_model_test.go`) to a split frame.
- **Config**: `LoadConfig` / `SaveConfig` round-trip keeps `[tui] split`; `uzi context set` on a file holding `[tui]` keeps it; an invalid value fails `uzi tui` with exit 2 naming the key.
- **uxlab** (`api/cmd/uzi/uxlab_gen_test.go` renders every scene at 100×34, `:34-35`): split scenes get a per-scene size override. Add `split-floor-focus` and `split-ci-focus` and `split-pulls-focus` at 100×60, `split-min` at exactly the latch-on height, `split-80col`, `split-needs-you-unfocused`, `split-ci-empty`, `split-filtering`.

**Visual review gate (before merge, outside the run).** The landing session runs the full uxlab build (`cd api/cmd/uzi/uxlab && devbox run build`), confirms the PNGs are newer than the frames, inspects the split scenes in light and dark, and has the `tui-ux` agent review them. Findings are fixed before merge.

## Milestones

### M1: lists and the tab strip render at a given pane height (prefactor)

- [x] Board, CI and pulls bodies and capacity functions take a pane height; the scroll helpers take the pane capacity; zero-arg full-screen wrappers remain.
- [x] `tabStrip` takes its admin-relabel, marked-tab and repo-suffix inputs explicitly.
- [x] Capacity-parity tests at a smaller-than-screen height; existing TUI tests pass untouched.

Blocked by: none. Acceptance: `task gate:api` green; full-screen frames unchanged (the existing render tests are the oracle).

### M2: the split, with CI and pulls in the bottom pane

- [x] Split state, the bottom-tab/`m.view` invariant and its setter, `splitEligible` / `splitDrawn`, worst-case threshold with the `WindowSizeMsg`-only latch, pane sizing.
- [x] Shared header, separator, bottom pane, footer with the version readout, focus marker, unfocused-pane rendering.
- [x] Keys `tab` / `shift+tab` / `ctrl+w` / `esc` / `1` / `2` / `3` / `R`; collapse to the floor with filter commit; drill-in `fromSplit` return.
- [x] Repo resolution on activation; `reposMsg`, tick and self-heal gates on "displayed"; modals pause the forge polls.
- [x] Help overlay entries; the tests above for threshold, parity, keys, collapse/return, polling, header/footer, monochrome, D7; uxlab scenes.
- [x] `docs/cli.md` section, `task docs:sync`, CHANGELOG line.

Blocked by: M1. Acceptance: examples 1 and 2 and the resize half of example 3 hold under tests.

### M3: turning the split off

- [x] `s` with the behaviour above; `[tui] split = "auto" | "off"` on `Config`, round-trip, exit-2 validation in `uzi tui`.
- [x] The `s` and config tests above.
- [x] `docs/cli.md` config key and `s`, `task docs:sync`.

Blocked by: M2. Acceptance: example 3 in full.

## Acceptance (live, maintainer)

A linked `acceptance` issue: on a real terminal against the hosted server, check the split at a few sizes in light and dark and inside tmux, and compare the server's forge load over an hour with an idle split floor versus `split = "off"`, to decide whether an unfocused-pane cadence is needed. It does not block merge.

## Decision Log

- **D1: horizontal split, floor on top, CI below by default.** User request. CI is the default because runs produce CI first; pulls are one `tab` away.
- **D2: one focused pane; drill-ins full-screen.** Every drill-in (run detail, PR, CI run) needs the whole screen. Rejected: a drill-in inside the pane.
- **D3: `tab` is a three-stop focus cycle floor → CI → pulls → floor, `shift+tab` reverses, `ctrl+w` toggles panes as an alias.** User decision after review. The first draft (tab from the floor focuses the bottom; tab inside the bottom flips CI/pulls) trapped focus in the bottom pane, and `ctrl+w` alone is intercepted by some browser-hosted terminals. The cycle keeps the user's original "tab from the floor goes to the bottom" and matches today's full-screen cycle in spirit. Rejected: keep the first draft and make `shift+tab` a pane toggle (buddy's recommendation, declined by the user).
- **D4: collapse always lands on the floor.** User decision: the floor is the TUI's home, so a shrink or `s` is predictable. Bottom state is kept for when the split returns. Applies to a split-origin drill-in's `esc` after a collapse too. Rejected: keep the focused pane full-screen.
- **D5: config values `auto` and `off` only.** An `on` value could not split a short terminal either, so it would equal `auto`. Rejected: `auto | on | off`.
- **D6: the displayed bottom tab polls at today's cadence even with the floor focused.** User decision: accept the added idle forge traffic and measure it before building another cadence. Today an idle floor makes no forge list calls (PRD #1255 D4). The server memoises forge reads and charges them to the connection budget (`api/internal/handler/forgeview.go`), so client polls do not map one-to-one to forge calls. Rejected: a slower unfocused cadence now (unmeasured), polling only on focus (a stale pane).
- **D7: the threshold charges three band headings and two band spacers per pane, leaving ≥ 8 actual list rows per pane, latch on at `minHeight + 2`, off below `minHeight`, updated only on resize.** Live chrome changes with the cursor, errors and meter ticks; a threshold built from it would flip the layout without a resize. The exit bound keeps the 8-row minimum; the 2-row entry margin stops one-row resizes from flapping. Width ≥ 80 keeps the footer intact.
- **D8: `s` toggles the split per session, only on list screens, and is a no-op with a note when the split cannot be drawn.** Free today and mnemonic. A global key would eat text in run detail's steer input. Silently recording "off" on a short terminal would leave a user wondering why growing it does nothing. Rejected: `|`.
- **D9: the bottom pane keeps the single scoped repo, resolved once from the newest board run after both the repos reply and the first board reply land.** `resolveDefaultRepo` marks the choice final, so resolving before the board reply would lock in the first enabled repo; waiting one reply is simpler than a provisional choice plus a correction that must invalidate both list caches. The floor is cross-repo; following the floor cursor would refetch the forge on every cursor move. The separator names the repo.
- **D10: focus is a bracketed pane label, the unfocused cursor is `›`.** `▸` already means row cursor in every list, and a colour-only cue disappears under the Ascii profile (`.claude/rules/tui.md`). Unfocused rows keep full colour so NEEDS YOU stays loud on the floor.
- **D11: the separator carries the bottom list's tabs, filter, summary and repo; the header carries only the floor tab.** Two bold tabs in one strip could not say which has focus; the list brand line those readouts used to sit on does not exist in a pane.
- **D12: no sketch-harness milestone; uxlab scenes plus a visual review gate at landing.** The uxlab generator drives the shipped `tuiModel`, so its scenes preview the real thing; a sketch would be thrown away (`.claude/rules/tui.md`). The PNG review runs in the landing session because it needs the devbox render toolchain.
- **D13: pulls ships with the split, not as its own milestone.** Without it `2` in the split has no defined meaning and the pulls poll gate stays focus-only; it has no value on its own.
- **D14: collapsed split notes never cost existing footer content.** Drop a note when it cannot fit alongside the complete footer. With no room for the note, the frame matches config `off` at the same size. Pre-existing full-screen hint clipping is separate work.
