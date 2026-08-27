# PRD #623 — Highlight the run's account in the TUI run view

**Issue**: [#623](https://github.com/vtmocanu/uzi/issues/623)
**Priority**: Medium
**Status**: Complete (2026-08-23) — shipped in PR #626 (`4b5805a2`), main CI green.
**Component**: `uzi` CLI TUI (`api/cmd/uzi/tui_*.go`) — single Go module (`api/`), no server/DB/web/controller change.

## Problem

In the `uzi tui` run-detail view, which Anthropic account a run is spending is shown as a compact credential tag pinned to the right of the header's **first line** (PRD #295, `detailCredTag`). Two issues:

1. That tag shares the top row with the run breadcrumb and (in the one-line layout) competes for width with the run **title**, so a long title is more likely to be pushed onto a second row or clipped.
2. The run-detail crew rail already renders per-account rate-limit meters under an `ACCOUNTS` eyebrow, but the account the run is actually using is **not called out** there, and if the user has deselected that account in settings it does not appear in the rail at all.

## Solution

In the run-detail crew rail (`ACCOUNTS` block):

- **Always show the account the run is running under**, as the **first** entry in the `ACCOUNTS` list, even when that account is deselected in settings (force-show — we are spending it regardless).
- **Highlight** that account's label in **tungsten, normal weight** (`#c9a061` dark / `#7c5200` light) — no bold, no cursor/dot marker, no left-gutter glyph. Sibling accounts keep their current faint-grey label. Tungsten-normal reads as "accented" against faint siblings and stays distinct from the bold-tungsten focused pane title.
- **Drop the credential tag from the header's first line** (`detailCredTag`), freeing that row so the title flows into the reclaimed width (the one-line header combine budget subtracts the right-block width, so removing the tag lets more titles stay on one line).

The account identity is user-visible in exactly one place in the run view (the rail), which is the intent.

### Design decision (locked with the user before this PRD)

The user reviewed a rendered bash mock of 13 highlight treatments and chose **variant 0: tungsten, normal weight, no bold, no marker**, plus **always force-show the run's account first** and **drop the header cred tag**. Do not reintroduce a bold weight, a `▸`/`●` left marker, a background band, or a right-side "· this run" tag — those were the rejected alternatives.

## Current state (baked-in facts — anchors as of this PRD's commit)

All file:line anchors below are for the tree at this PRD's commit; re-read the named function if a line has drifted (cite by function name, not a bare line).

### Header (what to remove and why it frees the title)

`detailHeaderLines()` in `api/cmd/uzi/tui_detail.go` (~L341-405) builds the priority header. The credential tag is woven in at three points:

- `credTag := m.detailCredTag()` (~L355).
- One-line layout: `combinedRight := joinTags(sep, statusTag, credTag, transportTag)` (~L369) — the block pinned right; its width is subtracted from the title's one-line budget at ~L377 (`m.width - visualWidth(crumb) - 3 - visualWidth(combinedRight) - 1 >= titleW`).
- Split layout, line 1: `joinTags(sep, credTag, transportTag)` (~L387) pins credential+transport to the right of the breadcrumb.

Removing `credTag` from L369 and L387 (and deleting L355) shrinks `combinedRight`, which **widens** the one-line title budget — this is the "room for the title to flow" the user asked for. No other header math changes.

`detailCredTag()` (~L481) becomes unreferenced after this and **must be deleted** (the Go dead-code gate holds both modules at zero against an empty baseline — an unused method reddens `task deadcode:api`; the routine fix is deletion, not a baseline entry — see root `CLAUDE.md` Commands section). Its doc comment currently reads: renders `run.AnthropicSecretLabel` as a compact muted label, empty when no credential recorded, drawn through `m.renderer.Plain` (D7).

### The rail ACCOUNTS block (where the account moves to)

`railRateMeters(now, usedRows)` in `api/cmd/uzi/tui_detail.go` (~L876-911) renders the stacked per-account meters:

- It calls `m.selectedRateMeters()` (`api/cmd/uzi/tui_board.go` ~L576) → `(shown []apitypes.TokenRateLimitDTO, showLabel bool)`.
- `selectedRateMeters`: `readable` = tokens with `Limits.Status == "ok"`; `showLabel = len(readable) > 1`; `shown` = readable filtered by `t.IsDefault || slices.Contains(m.sidebarTokenIds, t.SecretID)` (the settings selection). Empty selection → `(nil, false)`.
- Each shown entry is a `\n`-joined string: an optional faint label eyebrow (`m.pal.faint.Render(m.renderer.Plain(t.Label, laneRailWidth))`, only when `showLabel`), then two window cells (`m.rateWindowCell("5h", …)`, `m.rateWindowCell("7d", …)`), each `railRateBarWidth`=10 wide.
- Entries are added **whole, top-down, while they fit** under a height budget (`m.transcriptViewport() - usedRows - 1`), so an entry that does not fit is dropped from the **bottom**. Block is `m.pal.faint.Render("ACCOUNTS") + "\n" + join(fitted)`.

**`selectedRateMeters` is SHARED** with the board strip (`boardRateLimitStrip`, `tui_board.go` ~L613) — the board has no single run, so **do not** add force-show/highlight there. The run-aware logic must live in the run-detail path only (`railRateMeters`, or a new detail-only selector), keyed off the run.

### How to identify the run's account

The run DTO carries both fields (`api/internal/apitypes/run.go` ~L268-269):

- `AnthropicSecretID *string` — match against `apitypes.TokenRateLimitDTO.SecretID` (`api/internal/apitypes/ratelimit.go` L32).
- `AnthropicSecretLabel *string` — the user-authored account label (same string `detailCredTag` renders today; **untrusted**, already in `d7UntrustedFields`).

`m.detail.run` is the run in the detail view; `m.rateLimits` is `[]apitypes.TokenRateLimitDTO`. Nil `AnthropicSecretID` = a run claimed before PRD #111 M1 or not yet claimed → no forced account (behave as today).

### D7 (terminal-injection) obligation

`AnthropicSecretLabel` is in `d7UntrustedFields` (`api/cmd/uzi/tui_d7_guard_test.go` L115) because it is user-authored. It stays there — the board still renders it via `boardCredSeg`, and the rail must render the highlighted label through `m.renderer.Plain(label, laneRailWidth)` (never a bare `capCell`; `Plain` = `capCell(cellText(s))` strips control bytes). Removing `detailCredTag` does not remove the field's D7 coverage; the rail becomes the detail-view render site.

## Proposed rail selection + render (detail view only)

Build the detail rail's account list from the shared selection, then fold in the run's account:

1. Start from `shown, showLabel := m.selectedRateMeters()`.
2. Let `runID := m.detail.run.AnthropicSecretID`. If `runID == nil`, render exactly as today (no change) but with the header tag already gone.
3. If `runID != nil`, produce the run's account entry and make it **first**:
   - If a token with `SecretID == *runID` is already in `shown`, **remove it from its current position and prepend it** (an in-place "move to front" — a bare prepend without the remove double-lists it, and the run's account is often `IsDefault`, which is *always* in `shown`, so this dedupe is the common path, not the edge case).
   - Else if a token with `SecretID == *runID` exists anywhere in `m.rateLimits` (even `Status != "ok"`), prepend it (force-show); its windows render normally (a nil window → `"5h -"`, matching `rateWindowCell`'s nil branch).
   - Else (no rate-limit entry for the run's secret — e.g. rate limits not fetched, or an older server per the #519 deploy-ordering note in `railRateMeters`'s doc) synthesize a minimal entry from `*runID` + `*m.detail.run.AnthropicSecretLabel` with nil `FiveHour`/`SevenDay`, so the label still shows with `"5h -" / "7d -"`. This guarantees the account is shown even with no rate data, which is what makes dropping the header tag safe.
4. **Render the run's account label always** (even when `showLabel` is false — a single-account run must still show its highlighted name) in **tungsten normal**: `lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(m.renderer.Plain(label, laneRailWidth))`. Sibling account labels keep `m.pal.faint` and still obey `showLabel`.
5. Keep the existing top-down height-budget fitting. Because the run's account is first, it is the **last** entry to be dropped when the rail is squeezed (drop-priority is a free win of first-ordering). **Account for the run entry's always-present label line in the row budget**: the existing fit loop (`railRateMeters` ~L890-906) only counts a label row `if showLabel`, so the run entry — whose label renders unconditionally — is one row taller than that loop assumes; compute its `rows` with the label line included, or the fit budget under-counts by one.

> **🔴 M1 TRAP — the early `if len(shown) == 0 { return "" }` at the top of `railRateMeters` (~L878-880) fires BEFORE any force-show, so it defeats Success Criterion 2.** When the run's account is deselected in settings (and no other account is selected), or when every token is non-`ok`, `selectedRateMeters` returns `(nil, false)` and the current early return yields no `ACCOUNTS` block at all — exactly the deselected-account case this PRD exists to fix. Fold in the run's account (step 3) **first**, then apply the empty-check to the *final* list. Either move the empty-check after the fold, or give the detail rail its own selector that owns this ordering.

Empty/degenerate cases: no run (`AnthropicSecretID` nil) and no selected meters → block is `""` as today; a run with an account but zero rate data → a one-entry `ACCOUNTS` block with the highlighted label and `-` meters.

## Milestones

- [x] **M1 — Rail shows the run's account, force-shown first + highlighted.** In the run-detail path, fold the run's account (matched by `AnthropicSecretID` → `SecretID`) into the `ACCOUNTS` list as the first entry, force-showing it when deselected in settings and synthesizing a label-only entry when no rate-limit row exists. Render its label in tungsten normal weight (through `renderer.Plain`); siblings unchanged. **Move the `len(shown)==0` early return to after the fold (the M1 TRAP above), dedupe the already-shown/`IsDefault` case by remove-then-prepend, and count the run entry's always-present label row in the fit budget.** Board strip (`boardRateLimitStrip`) behavior unchanged — verify `selectedRateMeters` semantics for the board are untouched.
- [x] **M2 — Drop the header credential tag.** Remove `credTag` from `detailHeaderLines` (the one-line `combinedRight` join and the split line-1 join), delete the now-unused `detailCredTag()` method, and confirm the reclaimed width flows to the title (one-line combine budget grows). The deleted method must not be left unreferenced: an unused *unexported* symbol is caught by `lint:api`'s `unused` linter, which runs **earlier** in `gate:api` than `deadcode` (it fails fast at lint) — so the fix is deletion, and `gate:api` stays green through both slots.
- [x] **M3 — Tests updated and passing.** Repoint the D7 render coverage of `AnthropicSecretLabel` in the detail view from the header tag to the rail label (`tui_model_test.go` ~L752/L808 comments + assertions; `tui_d7_guard_test.go` L113 comment). **The detail-view D7 fixture must set `AnthropicSecretID` (not just `AnthropicSecretLabel`) to a value that matches a rate-limit token or triggers the 3c synthesis path — otherwise, after M2 removes the header tag, the hostile label renders nowhere in the detail view and the existing bare `assertNoRawControls(t, "detail", …)` passes VACUOUSLY, silently dropping the field's detail-view coverage. Pair it with a POSITIVE assertion that the sanitized label actually appears in the rail** (the codebase already pairs `assertNoRawControls` with a positive `"safe"` check on the admin path — follow that shape). Add `Update→msg`/`View()→string` assertions (the deterministic seam, per `.claude/rules/tui.md`): (a) the run's account label appears in the `ACCOUNTS` block, first, with a tungsten (non-faint) SGR; (b) it appears even when its `SecretID` is not in `sidebarTokenIds` (force-show); (c) the header first line no longer contains the credential label; (d) a hostile `AnthropicSecretLabel` (control bytes) is stripped in the rail render. Keep the test viewport tall (the default 120x40 is fine; a short viewport drops the ACCOUNTS block and re-vacuates these assertions). Keep `AnthropicSecretLabel` in `d7UntrustedFields`. Run `task gate:api` (fmt-check + vet + build + lint + deadcode + test -race -count=1).
- [x] **M4 — TUI screenshots regenerated + reviewed.** _(Verified visually by the maintainer on 2026-08-23 in the running TUI: the run's account renders highlighted in the rail with no header cred tag, confirmed OK. The offline worker lacked the uxlab devbox/render toolchain, so automated PNG regen was skipped; the deterministic `View()`/`railRateMeters` SGR assertions in M3 gate this behavior in CI.)_ Regenerate the uxlab frames/PNGs (`cd api/cmd/uzi/uxlab && devbox run build`) and confirm the run-detail scene shows the account highlighted in the rail and no cred tag in the header, in both light and dark, and under the NO_COLOR/Ascii downgrade (tungsten stripped → the label must still be legible; it is plain text, so it is). Confirm PNG mtimes are newer than the frames (a timed-out render leaves stale PNGs). Note: the offline worker may not have the uxlab devbox/render toolchain; if the render cannot run in the worker environment, complete M3's deterministic View() assertions (which DO gate) and record in the report that the screenshot step was deferred to local review rather than claiming it ran.

## Success criteria

1. In the run-detail view, the account the run is spending is the first entry under `ACCOUNTS`, its label in tungsten normal weight, visually distinct from faint sibling accounts — verified in a rendered screenshot (dark + light) and by a `View()` SGR assertion.
2. That account appears even when it is deselected in settings (not in `sidebarTokenIds` and not `IsDefault`) — verified by a `View()` assertion with a selection that excludes it.
3. The run-detail header's first line no longer renders the credential label; a long run title has at least as much one-line width as before (strictly more when a credential tag would have been present) — verified by a `View()` assertion and a screenshot.
4. `task gate:api` green (including `deadcode:api` at zero after `detailCredTag` deletion) and `task gate:web`/others unaffected (no non-Go change). Board credential column and board rate strip unchanged.
5. A hostile `AnthropicSecretLabel` is control-stripped in the rail (D7), same guarantee the header tag gave.

## Risks & mitigations

- **Losing account identity when rate limits are unavailable.** The header tag came from `run.AnthropicSecretLabel` (always present); the rail meters come from `m.rateLimits` (can be empty — older server, not-yet-fetched). Mitigation: step 3c synthesizes a label-only rail entry from `AnthropicSecretLabel` so the account name always shows even with no rate data; the run's entry is first so it is the last dropped under the height budget.
- **Extremely short terminal drops the whole ACCOUNTS block.** If not even one entry fits the rail height budget, the account is not shown anywhere in the TUI (same as any rail content today). Acceptable: the full account identity is always on `uzi run <id>` and `uzi run get <id> --json`. Do not special-case this; matches existing rail behavior.
- **Touching the shared `selectedRateMeters` would regress the board.** Mitigation: keep force-show/highlight in the run-detail path only; add a detail-specific selector or post-process `shown` inside `railRateMeters`. Add/keep a board test asserting the board strip is unchanged.
- **Lint/dead-code on the removed method.** `detailCredTag` must be deleted, not left unreferenced. As an unused *unexported* method it is caught by `lint:api`'s `unused` linter (which runs earlier in `gate:api` than `deadcode`), so the gate fails fast at lint until it is removed. Grep confirms its only non-test caller is `detailHeaderLines`; `strOr`, which it calls, stays used by `boardCredSeg`/`boardRow`, so no cascading dead code.

## Out of scope

- The board (`floor`) view's per-run credential column (`boardCredSeg`) and the board rate strip — unchanged.
- The web UI, CLI `uzi run` text output, and `--json` shapes — unchanged (they already surface the credential).
- Any change to which accounts are readable/selected, rate-limit fetching, or settings.

## Notes for an offline worker

This is entirely a codebase change; no open-web lookup is required. Prior-art inspiration projects are optional and, if consulted, must be cloned from their URLs (the worker has no host `inspiration/` symlink) — but no prior-art check is load-bearing here. All facts needed are in this repo, anchored above. Do not touch `.github/workflows/**` (worker PAT lacks `workflow` scope; any workflow-file change in the branch diff is an atomic push rejection). This PRD needs none.
