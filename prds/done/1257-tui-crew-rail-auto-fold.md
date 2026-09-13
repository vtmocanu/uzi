# PRD 1257: TUI crew rail auto-folds when the blocks below it would not fit

- **Issue**: #1257
- **Status**: Done — all milestones M1–M5 landed on `agent/issue-1257` (2026-09-13)
- **Priority**: Medium
- **Surfaces**: the `uzi tui` run detail view only (`api/cmd/uzi/tui_detail_rail.go`, `tui_detail.go`, `tui_keys.go`, their tests, one uxlab scene, `docs/cli.md`, specs). Everything is in the `api` Go module, so `task gate:api` is the gate for every milestone. No route, DTO, migration, web or worker change; the board is untouched.
- **Trust boundary**: none crossed. The fold decision reads terminal geometry and row counts; no new untrusted field is drawn (every cell the rail shows already rides `m.renderer.Plain`, D7 of `.claude/rules/tui.md`).
- **Guardrail**: neither implementation nor validation touches `.github/workflows/**` (worker PAT lacks `workflow` scope; `.claude/rules/prds.md`). Invariant at finalize: `git diff --name-only <base>..HEAD` shows zero entries under `.github/workflows/`.
- **Pairing**: PRD #1255 (TUI forge view) is in flight on the same TUI. It adds screens beside the board and edits `tui.go`, `tui_board.go`, `tui_keys.go` (help lines) and the test files; it does not touch `renderLaneRail`, the detail rail state or the detail key handler. The two overlap only textually (adjacent help lines in `tui_keys.go`, appended tests, a `specs/ai.md` section number), all rebase-level.

## Problem

The run view's crew rail does not scroll: `joinColumns` clamps it to the transcript's height by dropping its bottom rows (`tui_detail_transcript.go:604-621`, issue #379). On a run with many lanes, or in a short terminal, the roster alone fills the rail and the MILESTONES, SPEND and ACCOUNTS blocks below it vanish with no trace. The user has to know the `c` fold key exists and press it per run (the fold resets on every open, `newDetailState`, `tui_detail.go:134`).

Observed on the reference instance (rail is the fixed 26 columns, `laneRailWidth`, `tui_detail.go:17`):

```
 CREW  ▾                        CREW  17 ▸
▸◉ all agents                  ▸◉ all agents
 ○ worker
 ○ lead ▰▱▱▱▱▱▱▱▱▱▱▱  25%      MILESTONES ▰▱▱▱▱▱ 1/6
 ○ researcher·1                · m2, m3 +1
   Research store/SQL lay…      ✓ Store, clock, input, …
 ○ researcher·2                 ◕ Web: budget text, Ext…
   Research workersvc Go …        ↳ coder · 1s
 ○ researcher·3                   Implement M3 CLI ext…
   Research web + CLI sur…      ◕ CLI: uzi run extend +…
 ○ researcher·4                 ◕ Slack near-timeout DM…
   Research Slack + handl…      ○ Docs, specs, changelog
 ○ fact-checker                 ○ Whole-tree gate valid…
   Validate plan citation…
 ○ coder·1                     SPEND  $62.56
   Implement M1 contract …     in 2.32M  out 499.7k
 ○ coder·2                     cache 83.16M 97%
   Write M1 Go tests
 ○ coder·3                     ACCOUNTS
   Write agent wall re-ar…     personal
 ○ reviewer·1                  5h ▰▰▰▰▱▱▱▱▱▱  42%   4h7m
   Review M1 committed ra…     7d ▰▰▱▱▱▱▱▱▱▱  23%  6d18h
 ○ auditor                     meta
   Audit M1 trust boundary     5h ▰▰▱▱▱▱▱▱▱▱  20%     0s
 …(clamped: MILESTONES,        7d ▰▰▰▰▰▰▰▰▰▰ 100%  11h7m
   SPEND, ACCOUNTS gone)
   expanded (today's default)     folded (today: only after `c`)
```

The same run on a taller terminal, or a 6-lane run on this one, fits expanded with rows to spare, so a blanket "always fold" would throw away the roster for nothing:

```
 CREW  ▾
▸◉ all agents
 ○ worker
 ○ lead ▰▱▱▱▱▱▱▱▱▱▱▱   6%
 ○ fact-checker
   Adversarially verify r…
 ● coder·1
   Implement FIX 1 + FIX …
 ○ coder·2
   Implement FIX 2 (web R…
                                <- two blank rows here (a second defect, D6)

 SPEND  $7.13
 in 291.8k  out 85.9k
 cache 5.27M 95%

 ACCOUNTS
 personal
 5h ▰▰▰▰▱▱▱▱▱▱  42%   4h5m
 7d ▰▰▱▱▱▱▱▱▱▱  23%  6d18h
 meta
 5h ▰▰▱▱▱▱▱▱▱▱  20%     0s
 7d ▰▰▰▰▰▰▰▰▰▰ 100%  11h5m
```

The rail already knows the height: `transcriptViewport()` (`tui_detail_transcript.go:401-429`) is the rail's row budget, and SPEND / ACCOUNTS already budget themselves against it whole-block-or-nothing (`tui_detail_meters.go:42`, `:112-142`). Only the fold decision is manual.

## Solution

Decide the fold from the height. At render time the rail computes the rows its **expanded** form would need for the roster plus the protected blocks (whole MILESTONES, SPEND, the run's own ACCOUNTS entry, with their blank separators) and compares that to `transcriptViewport()`. When it does not fit, the rail renders the folded form that `c` produces today (count caret, selected lane only) with no key press; when it fits, the rail is expanded, byte-for-byte as today. `c` becomes a sticky user override (open / closed) that wins over the auto decision until the run is reopened. A resize re-decides by construction. The double blank row between the roster and SPEND on a run with no milestone list is fixed in the same change.

## Resolved facts (current code; an offline worker can verify each by reading the cited file)

- **Rail render**: `renderLaneRail` (`tui_detail_rail.go:17-86`). Caret at `:26-33` (`▾` open; `N ▸` folded, N = `len(d.lanes)` incl. the synthetic `all agents` lane). No-lanes path `:35-49` (`(no activity yet)` + blocks; nothing to fold). Folded branch `:65-70` draws only the selected lane via `laneRow`; expanded `:71-75` draws every lane. Blocks: MILESTONES `:76-78` (`"\n" + block`, block is right-trimmed), SPEND `:79-81` (`"\n\n" + sp`), ACCOUNTS `:82-84` (`"\n\n" + rb`). Each `laneRow` ends in `\n` and adds a second row when the lane has a label (`:155-164`).
- **The double gap (D6)**: after the roster `sb` ends in `\n`; with an empty MILESTONES block nothing is written, then SPEND's `"\n\n"` yields **two** blank rows. With a MILESTONES block present the join is `\n` + block + `\n\n` + SPEND = one blank row each side. The no-lanes path uses `"\n\n"` after a line with no trailing `\n`, so it is one blank row. The extra row is an artifact of the join, not a design choice.
- **Fold state**: `detailState.railCollapsed bool` (`tui_detail.go:64-67`), toggled by `keyCollapseCrew` (`"c"`, `tui_keys.go:28`) in `detailKey` (`tui_detail.go:491-499`), a no-op with zero lanes. Reset on every open by `newDetailState` (`tui_detail.go:134-141`), also from the board (`tui_board.go:282`) and the start-run path (`tui.go:313`). Not touched by `WindowSizeMsg` (`tui.go:613-615` sets `m.width, m.height` and rebuilds the renderer; no fold state).
- **Row budget**: `transcriptViewport()` = `m.height` minus the chrome `renderDetail` emits (header, optional limit-wait / near-timeout row, optional degraded transport row, blank separator, pane title row, banner, steer bar, footer), floor 3. It is pure (calls `detailBanner`, `renderSteerBar`, `transportLine`, none of which call back), so a helper may call it from both `View` and the key handler. `joinColumns(rail, body, laneRailWidth)` (`tui_detail.go:770-772`) iterates the transcript's row count = its title row + `transcriptViewport()` padded rows (`padLinesToViewport`, `tui_detail_transcript.go:434-441`), so the rail shows its `CREW` title plus exactly `transcriptViewport()` content rows beneath it. The existing SPEND/ACCOUNTS budgets count `usedRows` from the title row and compare against `transcriptViewport()`, so they are conservative by one row; that is pre-existing, left alone (M2 pins the boundary unchanged), and is why D2 counts content rows beneath the title.
- **Block budgets already in place**: `renderSpend(usedRows)` returns `""` unless its 3 rows + 1 separator fit in `transcriptViewport() - usedRows` (`tui_detail_meters.go:42`). `railRateMeters(now, usedRows)` adds whole account entries while `header + accumulated + rows <= budget` (`:112-142`), the run's own account forced first (`:71-105`, PRD #623), its label row unconditional (`:125-127`), so the run's entry is 3 rows (label, 5h, 7d) under a 1-row `ACCOUNTS` header; a sibling entry is 2 or 3 rows (`showLabel`). Both take `usedRows` = the rows the caller has emitted so far (`strings.Count(sb.String(), "\n") + 1`).
- **MILESTONES has no budget**: `renderMilestones` (`tui_detail_rail.go:451-581`) emits the eyebrow (plus a continuation row when the in-progress suffix would overflow, `:523-531`), an optional unattached now-line pair (`:536-538`), one row per frozen milestone, and per in-progress milestone a `↳ role · age` row plus an optional italic label row (`railNowLines :588-598`, `railMilestoneAgentLines :611-623`). Its height therefore changes when a milestone starts or finishes, when attribution arrives, or when the live activity gains or loses a label, all event-driven, not per tick.
- **Footer and help**: `detailFooter` shows `c crew` whenever `len(lanes) > 0` (`tui_detail.go:843-845`); the `?` overlay line is `"c          collapse the crew list (keeps the milestone block in view)"` (`tui_keys.go:79`).
- **Tests pinning today's behaviour**: `TestDetailCollapsibleCrewRevealsMilestones` (`tui_detail_fixes_test.go:213-260`) builds 8 lanes + 4 milestones at 100x20, asserts the **expanded** render hides MILESTONES and shows `▾`, then `c` reveals MILESTONES with `9 ▸`. Under this PRD that precondition inverts (the rail opens folded), so the test is rewritten, not deleted (M1). `TestTUIDetailFooterSurvivesTallRail` (`tui_model_test.go:1197-1222`) asserts the clamp keeps the footer on screen for a 5-lane, 4-milestone run at 100x20; that run carries protected blocks, so under this PRD it auto-folds. The test stays valid (the footer must survive folded too) but its docstring ("the rail truncates rather than overflowing") goes stale and is updated in M1; it does **not** guard the empty-required-set case, which gets its own test. The existing `renderSpend` drop test (`tui_model_test.go:2123-2136`) backs M2's budget-boundary claim. Helpers: `tuiTestModel` (`tui_model_test.go:32`), `press` (`:40`), `applyDetail` (`tui_detail_helpers_test.go:17`); a resize is `m.Update(tea.WindowSizeMsg{Width, Height})`.
- **uxlab**: scenes render at 100x34 (`uxlab_gen_test.go:32-36`); the detail scenes (`:88-100`, `detailRunning :381`) use `laneMsgs` (`:346`: lead, coder + label, tester + label, plus the `all agents` row = 6 lane rows) with `milestoneList` (`:228`, 4 milestones) and `boardMeters` (two accounts). Measured at 100x34 (`transcriptViewport()` = 30: 34 minus header, blank, pane title, footer): `detail-running` needs **27** content rows (6 lanes, 1+8 MILESTONES = eyebrow + the `· m3, m4` suffix on its own continuation row (#1176: eyebrow ~19 cols + 9 > 26) + 4 milestone rows + the m3 now-line pair, 1+3 SPEND, 1+7 ACCOUNTS) and `detail-milestones-attributed` **29**, so **no existing scene is expected to fold**, but the attributed scene has one row of headroom (29/30); a banner-carrying scene (`detail-awaiting-approval`) has a smaller viewport. Both must be read off the frame diff in M3, not assumed. The gap fix does not reach them (they all carry a milestone list). `cd api/cmd/uzi/uxlab && devbox run build` regenerates; PNG mtimes must be newer than the frames (`.claude/rules/tui.md`).
- **Docs**: `docs/cli.md` Run detail bullet (`:798-834`) describes the rail, MILESTONES and SPEND but never mentions the fold or `c`; the Keybindings block (`:839-857`) has no `c` row. `docs/run-activity.md` describes lanes and the now line, not the fold. After a `docs/*.md` edit, `task docs:sync` and commit the mirror (`TestEmbeddedDocsMatchSource`).
- **Specs**: the `c` fold is an AI decision (no `specs/human.md` line names it); the TUI's user requirements live under `## Feature #325` (`specs/human.md:714-727`). `specs/ai.md`'s last section is §632 on `main` `e0ca45a6`; the next free number is taken at landing (PRD #1255 may land first).

## Design

### D1 — The fold is a pure function of the render inputs, decided at render time

A helper, `railAutoFolded() bool`, computes the rows the **expanded** rail would need (D2) and returns `true` when that exceeds `transcriptViewport()`. It renders the roster and blocks to count them (`strings.Count(s, "\n")+1`, the arithmetic the rail already uses) rather than re-deriving row counts from run fields, so the decision and the render cannot disagree. No decision is stored: `View` and the `c` handler both call it, a `WindowSizeMsg` re-decides on the next `View`, and a run that grows past the fit folds on the frame that overflows. It takes a `now time.Time` (the rail's `railRateMeters(now, …)` needs one), so the `c` handler passes `time.Now()` as `renderLaneRail` does; the row count itself is clock-independent, so the decision is deterministic. Cost is one extra rail render per frame, bounded by the roster size at a 2 s poll; acceptable.

### D2 — What must fit: the expanded roster plus every *present* protected block, ACCOUNTS reduced to the run's own entry

Required rows (content rows beneath the `CREW` title, compared against `transcriptViewport()`) = every lane row (label rows included) + `1 + rows(MILESTONES)` when the run has a frozen list + `1 + 3` when `run.Usage != nil` + `1 + 1 + rows(run's own ACCOUNTS entry)` when `railRateMeters` would draw anything (each `1 +` is the blank separator). The run's own entry is the floor because PRD #623 already made it the one entry the rail must never drop; sibling accounts stay best-effort and still drop bottom-up as today, so a user with four selected accounts is not forced into a folded crew on a tall terminal. A run with no `AnthropicSecretID` (no own account) contributes **0** for ACCOUNTS even when sidebar-selected siblings would draw: siblings are best-effort everywhere, and a claimed run always carries a secret id, so this is the unclaimed corner, which has no lanes anyway. An absent block contributes nothing, so a run with **no** milestone list, **no** usage and **no** own account has an empty required set and never auto-folds (its roster clips at the bottom exactly as today, `TestTUIDetailFooterSurvivesTallRail`).

### D3 — `c` is a sticky override; auto is the zero value; reopen resets

`railCollapsed bool` becomes `railFold railFoldMode` with `railFoldAuto` (zero) / `railFoldOpen` / `railFoldClosed`. Effective folded = `Closed`, or `Auto && railAutoFolded()`. `c` sets `Open` when the effective state is folded and `Closed` otherwise, so the first press always does what the user sees as "the opposite", and later presses toggle between the two pins. Nothing returns the run to `Auto` except reopening it (`newDetailState`), matching how every other per-run view preference resets. A resize does not clear a pin: a user who pinned the roster open keeps it open and the blocks drop as before. The no-lanes no-op stays.

### D4 — The folded form and the caret are unchanged

Auto and manual folds render identically (`N ▸`, selected lane only). No new glyph for "folded by itself": the help line (D7) explains it, and one vocabulary keeps the rail's row budget and the existing tests' substrings stable.

### D5 — A rail that cannot fit even folded still folds

On a very short terminal the folded roster plus the required set may still exceed the viewport. Fold anyway (it maximises what shows) and let the blocks drop bottom-up through their existing whole-block-or-nothing budgets. No new truncation logic.

### D6 — Exactly one blank row between consecutive rail blocks

One helper appends a block to the rail with exactly one blank separator regardless of which earlier blocks were present, replacing the three hand-written `"\n"` / `"\n\n"` joins. `renderSpend` / `railRateMeters` keep their `usedRows` contract (rows actually emitted so far, separator included), so their budgets do not move. This retires the double gap in the 6-lane frame above.

### D7 — Copy

Footer hint stays `c crew`. The help line becomes `"c          fold / unfold the crew list (folds by itself when the blocks below would not fit)"`. `docs/cli.md` gains one sentence in the Run detail bullet and a `c` row in the Keybindings block.

### D8 — No hysteresis in this PRD

The natural height moves on events (a lane appears, a milestone starts or finishes, SPEND appears at first claim, a now-line label changes), not on the poll tick, so an oscillating fold needs the natural height to sit exactly on the viewport while an event flips it back and forth. M1 pins that the decision is stable across ticks with unchanged inputs. If a live run shows visible flapping, the follow-up is a 1-2 row re-expand latch decided in `Update`; named here, not built.

## Milestones

Each milestone's acceptance is its own behaviour plus a green `task gate:api` (run once to a log, then read it: Run economy in `CLAUDE.md`). Deterministic seam tests (`Update→msg` / `View()→string`, substring + SGR assertions) gate; a screenshot never does. One run.

- [x] **M1 — Auto-fold decision + `c` override (D1-D5).** `railAutoFolded()` and the required-set row count in `tui_detail_rail.go`; `railFold` tri-state replacing `railCollapsed` in `tui_detail.go` (field, `c` handler, caret). Rewrite `TestDetailCollapsibleCrewRevealsMilestones` so the 8-lane/4-milestone run at 100x20 **opens folded** (`9 ▸`, MILESTONES visible, no key pressed), `c` pins it open (`▾`, MILESTONES gone), `c` again pins it closed. New seam tests: the same run at 100x60 opens expanded (`▾`, every lane, MILESTONES, SPEND when `Usage` is set); `WindowSizeMsg` 60→20 folds and 20→60 unfolds with no key; an `Open` pin survives 60→20 (roster stays, blocks drop); reopening the run (`newDetailState`) returns to auto; a run with three accounts where only the run's own entry fits keeps the roster expanded, while one where the run's entry would not fit folds (D2); a run with no milestone list, no usage and no own account never auto-folds however tall the roster (D2, D5; a dedicated test, since `TestTUIDetailFooterSurvivesTallRail` carries milestones and now folds; update that test's docstring to say the footer survives the fold); the fold state (caret `▾` vs `N ▸`) is identical across two consecutive `View()` calls and across a blink tick (`blinkOn` toggled) with no other change (D8; assert the caret, not the whole frame, since `relAge` and the blink phase legitimately move); zero lanes: no caret, `c` no-op, no fold. Mutation check (the `.claude/rules/go.md` discipline, watched both ways): invert the comparison in `railAutoFolded` and confirm the fold tests redden.
- [x] **M2 — Rail block separator (D6).** The append-block helper; property test that exactly one blank row separates any two consecutive rendered blocks, for the four presence combinations (MILESTONES present/absent × SPEND present/absent, ACCOUNTS present), in the expanded, folded and no-lanes paths; `renderSpend` / `railRateMeters` budget tests unchanged and green (their whole-block-or-nothing rows must not move: assert SPEND is still dropped whole at the same `usedRows` boundary as before the change).
- [x] **M3 — Visual review.** One new uxlab scene, `detail-crew-autofold`: `detailRunning`'s run with a roster large enough to overflow 100x34 (extend `laneMsgs` locally in the scene, not the shared fixture), rendered light + dark, showing the auto-folded rail with MILESTONES, SPEND and ACCOUNTS all present; `devbox run build` with PNG mtimes newer than the frames; diff the existing scenes' frames against the pre-change generation and report which changed (expected: none, but `detail-milestones-attributed` sits at 29/30 rows and `detail-awaiting-approval` has a banner-shortened viewport; a scene that now folds is a finding to review with `tui-ux`, not a silent acceptance). Dispatch the `tui-ux` agent on the new and the `detail-running` scenes (both themes, plus the Ascii/`NO_COLOR` fallback: the caret's `N ▸` count carries the state without colour); fold any blocking finding, file the rest as incidental.
- [x] **M4 — Copy and docs (D7).** `tui_keys.go` help line; `docs/cli.md` Run detail bullet gains the auto-fold sentence (what folds, when, that `c` overrides and a reopen resets) and the Keybindings block gains the `c` row; `task docs:sync` run and the embedded mirror committed (`TestEmbeddedDocsMatchSource` reddens `gate:api` otherwise).
- [x] **M5 — Specs and finalize.** `specs/human.md` under `## Feature #325`: `- The run view's crew rail folds by itself when the expanded roster would push MILESTONES, SPEND or the run's own account meters off the rail; a roster that fits stays open; `c` overrides for that run. [user 2026-09-12, #1257]` (wording approved by the user in the authoring session; a change to it routes through the lead). `specs/ai.md` new section, next free number after the highest on `origin/main` at landing, recording D1-D8. Whole-tree `task gate:api`; finalize invariants: zero `.github/workflows/**` entries in `git diff --name-only <base>..HEAD`; `uxlab` sketch registry unchanged.

## Risks

- **R1 — Flapping.** Covered by D8: event-driven height changes, a stability test, and a named follow-up (a re-expand latch) if a live run shows it. The override (D3) is the user's escape hatch in the meantime.
- **R2 — The decision and the render disagree.** Avoided by D1 for the roster, MILESTONES and SPEND: the decision counts a real render of the same functions, with the same `transcriptViewport()`, rather than a parallel arithmetic model. ACCOUNTS is the deliberate exception (D2 counts the run's own entry, the render draws every fitted entry); it is safe because `railRateMeters` forces the run's entry first, so whenever the rail is expanded that entry is the one guaranteed on screen. The test that the same run folds at h=20 and expands at h=60 through the real `View()` is the guard.
- **R3 — Budget contract drift in `renderSpend` / `railRateMeters`.** D6 refactors the joins around them; their `usedRows` meaning must not move or SPEND starts dropping one row early or late. M2's boundary assertion pins it.
- **R4 — Rebase against PRD #1255.** Textual overlap only (help lines, appended tests, the `specs/ai.md` number). If #1255 lands first, renumber the section and re-run `gate:api`; no semantic merge.
- **R5 — Surprise for users who liked the roster.** A 15-lane run in a 40-row terminal now opens folded. `c` pins it open in one press, the footer hint is always visible when there are lanes, and the help line says why it folded.

## Validation strategy

- **Deterministic (CI)**: the M1/M2 seam tests through `tuiTestModel` + `applyDetail` + `press` + `tea.WindowSizeMsg`, substring assertions on `▾` / `N ▸` / `MILESTONES` / `SPEND` / `ACCOUNTS`, the byte-identical re-render, the separator property. `task gate:api` per milestone (`-race -count=1`).
- **Visual (review, not a gate)**: the new uxlab scene plus `detail-running`, light + dark + Ascii, reviewed by `tui-ux`.
- **Live (maintainer, post-merge)**: `uzi tui <run>` on the reference instance against a 15+ lane run and a 6-lane run in the same terminal: the first opens folded, the second expanded; resize the terminal taller and shorter and watch the first flip; press `c`, resize again, confirm the pin holds; reopen the run, confirm auto returns. Then `uzi tui --demo` for the seeded fixtures.

## Out of scope

- A scrolling crew rail (would replace the clamp; a different PRD).
- A partial fold (showing as many lanes as fit): the required set is protected whole; the crew is either the roster or the selected lane.
- Any board change, any web or CLI (`uzi run get`) change: the fold is a rail-geometry concern.
- Persisting the `c` preference across runs or sessions.
- A re-expand hysteresis latch (D8 follow-up, only on evidence).
- `.github/workflows/**` edits (guardrail).

## Decision log

- **D1** Render-time pure decision from a real render's row count; no stored state; resize re-decides by construction. (Authoring session 2026-09-12; alternative rejected: an `Update`-time latch with hysteresis, more state and more seams for a flap not yet observed.)
- **D2** Required set = roster + whole MILESTONES + SPEND + the run's own ACCOUNTS entry; sibling accounts best-effort; empty required set never folds. (User framing: "if milestones and spend and account do not fit"; the run's-own-account floor follows PRD #623.)
- **D3** `c` is a sticky open/closed pin over an auto zero value, reset only by reopening the run; resize never clears it.
- **D4** No distinct caret for the auto fold.
- **D5** Fold even when the folded rail still overflows; existing block budgets handle the rest.
- **D6** One blank row between consecutive blocks via a single append helper; the SPEND/ACCOUNTS `usedRows` contract is preserved.
- **D7** Help line rewritten; footer hint unchanged; `docs/cli.md` gains the sentence and the `c` row.
- **D8** No hysteresis now; stability test; latch named as the follow-up.
- **User-confirmed 2026-09-12**: a 6-lane run that fits must not fold ("for this we would not collapse, no need"); pairing with the in-flight #1255 is acceptable since the two do not collide semantically.
