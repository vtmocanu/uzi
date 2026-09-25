# PRD #1653: Provider logos and account names on usage meters and run badges

**Issue**: [#1653](https://github.com/vtmocanu/uzi/issues/1653) | **Priority**: Medium
**Status**: Draft; every decision below was locked by the maintainer on 2026-09-25 after a two-agent brainstorm (Claude + Codex) over before/after mocks
**Evidence baseline**: `98b06216` (2026-09-25); refresh file anchors on the implementation base
**Mocks**: `prds/mockups/1653-usage-provider-icons-mock.html` (web; open locally) and `prds/mockups/1653-usage-provider-icons-tui-mock.sh` (TUI strip; `bash` it, `NO_COLOR=1` for the stripped profile). Fixture names only. The web mock uses literal hex values; the product uses theme tokens. Where a mock and the *Binding design decisions* disagree, the decisions win. The web mock's "Icon options" section and its option C are exploration, not scope.

## Problem and outcome

A user holding both a Claude token and a Codex account sees two naming schemes for the same kind of thing:

1. **Web sidebar.** The Claude block is headed by the account label ("TEAM") only when more than one Claude token is readable; the Codex block is always headed by the provider word "CODEX", and names its account only when more than one Codex account is readable. A lone account is never named, and adding a second one suddenly renames the first. The Claude "+N more tokens in Settings" link sits between the two provider blocks.
2. **TUI board strip.** The Codex section reads `codex ▎codex P ▰▰▰▱▱ 62% 1d10h`: the first `codex` is the provider tag, the second is the default bucket's id (the account alias is hidden for a single account), and `P` is "primary window". The web draws the same window as `7d`.
3. **Runs.** A Codex run carries a "Codex" text badge; a Claude run is unmarked, so the list does not say which agent ran what at a glance.

**Outcome.** Every account is named by its own label everywhere, and the provider is shown by a logo on the web and by a single `codex` tag in the TUI. The sidebar is one list. Every run shows which agent it runs on. Window labels are lengths (`5h`, `7d`) on both providers and both surfaces.

## Related work

| Item | Relationship |
|---|---|
| PRD #1519 (`prds/done/1519-usage-ui-alignment.md`) | Its D3 label-precedence matrix hides a lone account's label and omits the `codex` tag on the two-line fallback. D-T1 and D-T4 below **supersede** those two rules; record the reversal in this PRD's Decision Log. D4's adaptive one-line/two-line layout (`boardMeterLayout`, one snapshot per frame) is kept. |
| PRD #1209 | Codex per-account meters, the sidebar selection (`sidebar_codex_account_ids`), the Settings "Codex limits" card. Selection rules unchanged. |
| PRD #1429 M4a | `HarnessBadge` (Claude unmarked, Codex explicit). D-W5 reverses the "Claude unmarked" choice. |
| PRD #104 / #53 | Per-token Claude meters and the sidebar micro-meters. Selection rules unchanged. |

## Scope

### In

- Two logo components in `web/src/components/icons.tsx`.
- The web sidebar meters merged into one account list with logo + name.
- Settings "Claude limits" and "Codex limits" cards: logo in the title, account always named, default bucket caption dropped.
- The run harness chip on every run, in the run list and the run detail header.
- The TUI board strip and the detail rail's Codex block: account always named, default bucket unnamed, `5h`/`7d` window labels, the `codex` tag kept on the split line.
- Tests, mock-mode fixtures, `docs/rate-limits.md` and its embedded mirror.

### Out

- Any API, DTO, store, migration or poller change. Every field needed is already served: `Run.harness` (always present, `"claude" | "codex"`), token `label` / `is_default`, Codex `aliases` / `is_default` / buckets with `limit_window_seconds`.
- Sidebar/TUI selection rules (which accounts show), the Settings "Show in sidebar and TUI" checkboxes, forecasts, stale dimming, tones and thresholds.
- `uzi usage` / `uzi token` CLI tables (their `PRIMARY`/`SECONDARY` columns stay; they are data tables, not the strip).
- The TUI's Claude-side rail block (`railRateMeters`), except that it keeps working with the shared selection helper.
- Brand logos anywhere in the TUI (a terminal cannot draw them; D-T1).
- Any creation, modification or validation write under `.github/workflows/**`. Any edit to frozen `specs/ai.md`.

## Verified current behavior

Resolved local-code facts at the evidence baseline; the offline worker need not look anything up online.

- **Web sidebar.** `web/src/components/AppShell.tsx` renders `<SidebarRateLimits />` then `<SidebarCodexRateLimits />` under the user block. `SidebarRateLimits` (`web/src/components/RateLimitMeters.tsx`) filters readable tokens (`limits.status === "ok"`), selects via `isShownInSidebar` (default always, extras by `sidebar_token_ids`), labels each `TokenMicroMeters` only when `readable.length > 1`, and renders "+N more token(s) in Settings" (link to `/settings`). `SidebarCodexRateLimits` (`web/src/components/CodexRateLimitMeters.tsx`) does the same for Codex (`hasCodexReading`, `isCodexAccountShownInSidebar`, `sidebar_codex_account_ids`), always renders a "Codex" eyebrow, labels each `CodexAccountMicroMeters` only when `readable.length > 1`, and renders "+N more Codex account(s) in Settings". Both refetch their selection on `onSidebarTokensChanged`. Container aria-labels are "Claude rate limits" and "Codex rate limits". Rows share `MICRO_METER_GRID_COLS` (`web/src/lib/rateLimitLayout.ts`).
- **Codex labels.** `codexAccountLabel` (`web/src/lib/codexRateLimits.ts`) joins deduped aliases, falling back to "Codex account". `formatCodexWindowLabel(limit_window_seconds)` yields `5h` / `7d` / `3h` / … from the server-reported length, `"window"` when missing. `bucketDisplayName` (`CodexRateLimitMeters.tsx`) is the display name or the id.
- **The default Codex bucket.** `api/internal/codexauth/usage.go`: `codexMainBucketID = "codex"`; the top-level `rate_limit` becomes the bucket with id `"codex"` and an **empty** display name; each `additional_rate_limits` entry becomes a bucket whose id and display name are its sanitized limit id. The web mock fixtures (`web/src/mocks/data/codexRateLimits.ts`) use ids like `requests` / `tokens` / `code`, not `"codex"`, so they do not exercise the default-bucket rule today.
- **Settings cards.** `RateLimitCard` / `TokenMeters` (`RateLimitMeters.tsx`) title "Claude limits", name each token only when `tokens.length > 1`. `CodexRateLimitCard` / `CodexAccountBlock` (`CodexRateLimitMeters.tsx`) title "Codex limits", always name the account, render `bucketDisplayName(b)` above each bucket's windows. Both use `SectionTitle` inside `Card`.
- **Harness badge.** `web/src/components/HarnessBadge.tsx` returns `null` unless `harness === "codex"`, then a `Badge tone="info"` reading "Codex". Rendered at `web/src/pages/RunsList.tsx` (run row badge cluster) and `web/src/pages/RunView.tsx` (run header). `Run.harness` is `Harness` (`"claude" | "codex"`), NOT NULL server-side (`web/src/lib/apiTypes.ts`, `interface Run`).
- **Icons.** `web/src/components/icons.tsx` inlines lucide-style stroked icons (`Icon`) and filled brand glyphs (`LogoIcon`, `viewBox="0 0 24 24"`, `fill="currentColor"`, props spread last so a caller-supplied `viewBox` wins). `GitLabIcon` / `GitIcon` are simple-icons paths with a one-line source comment. No package dependency on an icon library (zero-dependency budget).
- **TUI board strip.** `api/cmd/uzi/tui_board.go`: `boardMeterLayout` combines the Claude section (`boardClaudeMeterSegs`, `tui_board_rows.go`) and the Codex accounts section (`boardCodexAccountsSeg`, `tui_codex_meters.go`) onto one line when it fits `m.width`, else two lines; `boardCodexProviderTag` is the faint `"codex "` tag, placed on the combined line and on a Codex-only line, **omitted on the two-line fallback** (PRD 1519 default-omit). `selectedRateMeters` / `selectedCodexRateMeters` return `showLabel = readable > 1`. `boardCodexAccountSeg` draws the label only when `showLabel`, then per bucket `codexBucketLabel(b) + " "` then `codexRateWindowCell("P", …)` and, when present, `"S"`.
- **TUI detail rail.** `railCodexRateMeters` (`tui_codex_meters.go`) draws a faint `CODEX` header, the account label only when `showLabel`, then per bucket its label line and `P` / `S` window lines, fitting whole accounts to the rail height. `railCodexFloorRows` (`api/cmd/uzi/tui_detail_rail.go`) independently counts the same rows (the optional account eyebrow, one name eyebrow per bucket, the window lines) to decide the crew auto-fold; its comment requires it to mirror `railCodexRateMeters` exactly.
- **Untrusted text (D7).** Account labels, aliases and bucket names are user- or provider-authored and must keep going through `m.renderer.Plain` in the TUI; `Aliases`, `DisplayName` and `Label` are registered in `d7UntrustedFields`.
- **Tests pinned to today's text** include `web/src/components/*RateLimit*.test.tsx`, `web/src/pages/Settings*.test.tsx`, `web/src/components/HarnessBadge*` / `RunsList` / `RunView` tests, `api/cmd/uzi/tui_codex_ratelimit_test.go` and the TUI render/model tests that assert `P`/`S`, the provider tag placement and the D3 matrix. Find them with `git grep` on the implementation base.
- **Docs.** `docs/rate-limits.md` (the Codex surfaces table: "provider-labeled next to your Claude meters") and `docs/codex-credentials.md` mention the sidebar. After editing any `docs/*.md`, run `task docs:sync` and commit the mirror.

## Binding design decisions

### Web

**D-W1. Logos.** Add `ClaudeIcon` and `OpenAIIcon` to `icons.tsx` as `LogoIcon` glyphs, copying the path data **verbatim from the web mock's `<symbol id="i-claude">` and `<symbol id="i-codex">`** (the offline source of truth in this repo):
- `ClaudeIcon`: the Claude mark from simple-icons (`claude.svg`, simple-icons 16.32.0, CC0-1.0), `viewBox 0 0 24 24`. Comment in the GitLab style: `// ClaudeIcon: the Claude mark (simple-icons, CC0-1.0) ...`.
- `OpenAIIcon`: the OpenAI mark from Bootstrap Icons 1.13.1 (`openai.svg`, MIT), `viewBox 0 0 16 16` (pass it explicitly). The full MIT copyright and permission notice is already committed in `THIRD_PARTY_NOTICES.md` (repo root, added with this PRD); the icon's comment names the source and points to that file. Do not edit the notices file except to fix a path it names.
- Colour: Claude renders in a fixed Claude orange `#d97757` via a single CSS custom property or token added once (not repeated literals); OpenAI renders in `currentColor` (the text colour), so it works on every theme. Both 16px in the sidebar and card titles.
- Each logo is `aria-hidden`; the provider word is carried by the surrounding accessible name or a `title` (D-W2, D-W5), never colour alone.
- Do not add any other brand mark, and do not use the Codex "cloud with a prompt" mark.

**D-W2. Sidebar: one account list.** Replace the two stacked sidebar blocks with one component (name it in the Decision Log) rendering, in order, the shown Claude tokens (server order, default first) then the shown Codex accounts (server order, default first). Selection rules and the `onSidebarTokensChanged` refetch are unchanged.
- Each account has a header row: the provider logo (16px), then the account name in the existing eyebrow style (uppercase, `text-[10px]`/`text-[11px]`, `text-faint`/`text-muted`, truncating). **Always rendered**, whatever the account count. Claude name = token `label`; Codex name = `codexAccountLabel(account)`.
- Each account (its header plus its meter rows) is a `role="group"` element whose accessible name is "`<Provider>` account `<name>`" (e.g. "Claude account team", "Codex account default"), via `aria-label` or `aria-labelledby` on that group; tests query it with `getByRole("group", { name })`. The logo has `title="<Provider>"` for hover.
- No "Codex" provider eyebrow. No provider words visible in the sidebar.
- One "+N more accounts in Settings" link at the bottom, N = hidden Claude tokens + hidden Codex accounts (singular "account" for N = 1), linking to `/settings`. Hidden when N = 0.
- One container, `aria-label="Usage limits"`. Hidden entirely when neither provider has a readable account (as today).
- Meter rows (`MicroRow` / `CodexMicroRow`), forecasts, dimming, tooltips and the shared grid are unchanged.

**D-W3. Codex buckets.** Anywhere a Codex bucket name would be drawn (sidebar, Settings card), **hide it for the bucket whose id is `"codex"`** (the account's main limit). Every other bucket keeps its name: in the sidebar as a small faint caption line above that bucket's rows (it is unlabelled today), in the Settings card as today. The accessible names of window rows keep the bucket name.

**D-W4. Settings cards.** Keep two cards ("Claude limits", "Codex limits"), each under its own credential card as today. Put the provider logo before the card title (in `SectionTitle`'s row). Do **not** add a logo per account inside the cards. Name every Claude token even when there is only one (drop the `tokens.length > 1` gate). Apply D-W3 to the Codex card's bucket captions.

**D-W5. Run harness chip.** Replace `HarnessBadge`'s output with a small round chip (about 20px, `border-edge`, `bg` surface, 13px logo) on **every** run, Claude included: `ClaudeIcon` for `"claude"`, `OpenAIIcon` for `"codex"`. `title` "Runs on Claude" / "Runs on Codex"; accessible name "Claude" / "Codex" (`aria-label` on the chip, `role="img"`). Same component in `RunsList.tsx` and `RunView.tsx`; placement unchanged. Update the component's header comment (it currently documents "Claude stays visually unmarked"). Anywhere else that renders `HarnessBadge` (re-check with `git grep` on the base) gets the same chip.

### TUI

**D-T1. No logos; one `codex` tag.** The TUI draws no provider glyph. The Codex group keeps the single faint `codex ` provider tag in front of its first account; Claude stays untagged.

**D-T2. Account always named.** On the board strip, every shown Claude token and Codex account renders its label after its accent bar `▎`, regardless of how many are readable (`showLabel` becomes unconditional for the strip; keep the helpers' selection behaviour). Labels stay capped and routed through `m.renderer.Plain`. The detail rail's Codex block names its account(s) likewise; its `CODEX` rail header stays.

**D-T3. Window labels and buckets.** Codex window cells are labelled by `limit_window_seconds` in the same `5h` / `7d` / `3h` form as the web (`formatCodexWindowLabel`'s rules: whole days → `Nd`, whole hours → `Nh`, whole minutes → `Nm`, else `Ns`; unknown/≤0 → `window`), never `P` / `S`, on the board strip and the detail rail. The bucket with id `"codex"` draws no bucket name; any other bucket keeps its name (Plain-routed) before its windows.

**D-T5. Overflow is clipped, as today.** Each meter line stays clamped to `m.width` by `clampVisual`; there is no third line, no wrapping and no reordering. When the Codex line (or the combined line) is wider than the terminal, accounts or buckets past the right edge are clipped exactly as they are today. "Every shown account is named" means every account that is drawn carries its name; D-T2 does not promise that every selected account fits. Test the clip at 80 columns with two Codex accounts plus an extra bucket: the line is exactly `m.width` visual columns and starts with `codex ▎<first alias>`.

**D-T4. Tag on the split line.** When `boardMeterLayout` falls back to two lines, the Codex line keeps the `codex ` tag (it used to rely on `P`/`S` to read as Codex; that cue is gone). The combined line and the Codex-only line keep exactly one tag. The one-snapshot-per-frame rule and the row reservation (`boardCapacityWith`) are unchanged; remeasure the combined-vs-split threshold only through the existing layout code.

## Milestones

Each milestone ends with its component gate green (`task gate:web` for web milestones, `task gate:api` for TUI milestones; run once to a log per the root CLAUDE.md *Run economy*). Tests assert text, roles and accessible names, never colour classes.

- [ ] **M1. Logos.** D-W1: `ClaudeIcon`, `OpenAIIcon`, the Claude colour token. Tests: both render an `svg` with the expected `viewBox`, are `aria-hidden`, and `OpenAIIcon` uses `currentColor`. knip stays green (both are used by M2/M4).
- [ ] **M2. Sidebar account list.** D-W2, D-W3. Tests: one-of-each renders two account groups, Claude first, found by `getByRole("group", { name: "Claude account …" })` / `"Codex account …"`; a single Claude token is named; no "Codex" eyebrow; hidden counts combine into one "+N more accounts in Settings" (and singular form); a Codex account whose only bucket is `"codex"` shows no bucket caption while a second bucket shows its name; nothing renders when neither provider is readable. Add a mock fixture account whose buckets are `"codex"` (empty display name) plus one extra bucket, mirroring `codexauth/usage.go`, and point the relevant mock scenario at it.
- [ ] **M3. Settings cards.** D-W4. Tests: both titles carry the logo; a lone Claude token is named; the Codex card hides the `"codex"` bucket caption and keeps others.
- [ ] **M4. Run harness chip.** D-W5. Mock mode today has no mixed list: default runs are all `"claude"` and the `codex-only` scenario flips every run to `"codex"` (`harnessOverlay`, `web/src/mocks/mockApi/runs.ts`). Seed exactly one existing default-scenario run with `harness: "codex"` in the run fixtures (the same run in both the list and the detail read), so the default mock shows one Codex run among Claude runs; `codex-only` keeps flipping every run. Tests: a Claude run and a Codex run each render their chip with the right accessible name and title, in `RunsList` and `RunView`; the old "Codex" text badge is gone. Sweep and repoint every unpaired negative assertion on the retired strings ("Codex" badge text, "Claude rate limits", "Codex rate limits", "more tokens in Settings", "more Codex account") per `.claude/rules/web.md`.
- [ ] **M5. TUI board strip.** D-T1 to D-T5. Tests at the `View()` seam: a single Claude token is labelled; a single Codex account shows `codex ▎<alias> 7d …` with no `codex` bucket name and no `P`/`S`; an extra bucket keeps its name; a 3-hour window reads `3h`; the two-line fallback's Codex line carries the `codex` tag; the combined line carries exactly one; a colorprofile downgrade (Ascii/NoTTY) check still distinguishes the providers. Update the D3-matrix tests to the new rules rather than deleting them.
- [ ] **M6. TUI detail rail.** D-T2, D-T3 on `railCodexRateMeters` **and** `railCodexFloorRows` together (the floor adds the now-unconditional account eyebrow and drops the name eyebrow for the `"codex"` bucket, so the auto-fold decision and the render stay equal): account named, window lengths, default bucket unnamed; whole-account fitting unchanged. Tests at the same seam, including a short viewport where the floor decides the fold and the drawn rows match it. Regenerate the uxlab render harness frames that include the header strip or the rail (confirm PNG mtimes are newer than the frames) per `.claude/rules/tui.md`.
- [ ] **M7. Docs and CLI check.** Update `docs/rate-limits.md` (the Codex surfaces table and any sidebar/TUI description) and `docs/codex-credentials.md` if its sidebar sentence changes meaning; `task docs:sync`; commit the mirror. Confirm `api/cmd/uzi/` needs no further change beyond M5/M6 (the `uzi usage` table keeps `PRIMARY`/`SECONDARY`) and say so in the PR.
- [ ] **M8. Visual validation in mock mode and bundle build.** Run `cd web && npm run build` once after the web milestones (`task gate:web` does not run `vite build`, per `.claude/rules/web.md`). `cd web && VITE_UZI_MOCK=1 npm run dev`: the sidebar with one of each, with several of each and hidden extras, and with Codex only; Settings; the run list and a run page with one Claude and one Codex run; on Dawn and Ember at 1440px and the sidebar at 390px. The worker checks these views and reports what it saw in the PR description; it does not commit screenshots. The landing session captures and attaches before/after screenshots to the PR (web views above, plus the TUI strip from the uxlab harness at a wide and an 80-column width).

## Success criteria

- No visible "Codex" provider eyebrow in the sidebar; every account drawn in the sidebar, the Settings cards and the TUI strip is named, including a lone one (TUI overflow past the terminal width is clipped per D-T5).
- Every run row and run header shows a Claude or OpenAI logo chip with the provider word in its accessible name.
- No `P` / `S` window labels remain on the TUI strip or rail; a Codex default bucket is never labelled `codex`.
- On a narrow terminal, the split Codex line starts with the `codex` tag.
- `task gate:web` and `task gate:api` green; no new dependency in `web/package.json`.

## Risks

- **Brand marks (accepted by the maintainer, 2026-09-25).** The icon-library licences cover the SVG paths, not the marks: Simple Icons states its CC0 does not waive trademark rights, and OpenAI's brand page asks that its logo be used as provided, while this path is Bootstrap's redraw. Neither source was found to forbid showing a provider mark to identify an account or run, as uzi already does with GitLab. The maintainer chose to ship both marks; `THIRD_PARTY_NOTICES.md` states the trademarks and that uzi is not affiliated. Draw the paths unmodified (no recolouring beyond `currentColor` / the Claude orange, no distortion).
- **TUI width.** Always naming accounts lengthens the combined line, so it splits sooner at 80–100 columns. D4's adaptive layout absorbs this; the tests in M5 pin both layouts.
- **Negative assertions.** Several tests assert the absence of today's strings; a copy change silently disarms them (M4 sweep).

## Decision Log

- 2026-09-25: Web uses real provider logos (Claude mark, OpenAI mark); the TUI uses none, because a terminal cannot draw them and a glyph stand-in needs learning. The Codex "cloud with a prompt" app icon was rejected by the maintainer; no official SVG of it was found.
- 2026-09-25: Licences: the Bootstrap Icons MIT notice ships in full in `THIRD_PARTY_NOTICES.md` (not a one-line comment); the trademark position is stated as a maintainer-accepted risk rather than as settled nominative use (Codex review).
- 2026-09-25: Every account is named by its own label everywhere, including a lone one, so adding an account never renames the others. Supersedes PRD 1519 D3's single-account label suppression.
- 2026-09-25: The sidebar is one list (Claude first); the Settings page keeps two cards with the logo in the title only, because each card is already one provider.
- 2026-09-25: Every run shows its harness chip, Claude included, reversing PRD #1429 M4a's "Claude unmarked"; the run header matches the list.
- 2026-09-25: TUI keeps one `codex` tag (Claude untagged) and keeps it on the two-line fallback, since `5h`/`7d` on both providers removes the `P`/`S` cue PRD 1519 relied on there.
