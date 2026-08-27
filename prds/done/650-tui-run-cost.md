# PRD #650: TUI run cost — floor board column + run view SPEND block

**Status**: Done
**Priority**: Medium
**Depends on / relates to**: PRD #40 (run usage rollup — shipped `UsageDTO`, `cost_usd`, and the web usage surfaces this mirrors into the terminal). PRD #295 / #111 (the board credential column this sits beside). PRD #623 (moved the run's spending account into the detail rail ACCOUNTS block — the SPEND block lands directly above it). This PRD is display-only: it renders cost figures that are already on the wire and adds no API, SQL, or DTO change.

## Problem

The `uzi` TUI never shows what a run cost. The data is already fetched: `Usage.CostUSD` rides on `RunDTO` (`api/internal/apitypes/run.go:315`, `UsageDTO.CostUSD` at `api/internal/apitypes/usage.go:16`), attached on the own-board list read (`api/internal/handler/runs.go:75`, `item.Usage = usageFromListRow(row)`) and on `GetRun`. The web UI surfaces it (runs list per-row cost, run view usage panel), but the terminal board and run view do not — so a user watching the factory from the terminal cannot see spend at a glance, and has to leave for the web to answer "which run is expensive?" or "what did this run cost?".

## What already exists (verified against the tree 2026-08-24)

**Cost data on the wire, own board + detail:**
- `RunDTO.Usage *UsageDTO` (`run.go:315`), nil for a pre-#40 or unclaimed run. `UsageDTO` fields (`usage.go:11-17`): `InputTokens`, `CacheReadTokens`, `CacheCreationTokens`, `OutputTokens` (all `int64`), `CostUSD float64`.
- Own board list: `ListRuns` attaches it (`runs.go:75`). Detail: `GetRun` attaches it.
- **Admin board does NOT:** `AdminListRuns` (`runs.go:86-107`) builds each `RunListItemDTO` from `runToDTO(...)` and never calls `usageFromListRow` — `Usage` is nil on every admin row, exactly like `JudgeVerdict`. This is why admin-board cost is out of scope (it needs a query + DTO change; see Out of scope).

**Web formatting to mirror (parity source, baked in here so the worker needs no web access):** `web/src/lib/formatTokens.ts`.
- `formatCost(usd)`: `usd >= 1000` → `"$"+round` (drops cents, e.g. `$1119`); else `"$"+usd.toFixed(2)` (e.g. `$1.87`). No thousands separator.
- A **`$0` cost with nonzero tokens is a subscription-auth run the SDK prices at $0** — callers render `"—"`, never `"$0.00"` (`web/src/components/RunUsage.tsx:112`, `money()` = `usd > 0 ? formatCost(usd) : "—"`). This em-dash convention is load-bearing and must be preserved.
- `formatTokens(n)`: adaptive ladder — `<1000` bare int; `<1e6` → `"NN.Nk"` (one decimal); `<1e9` → `"N.NNM"`; `<1e12` → `"N.NNB"`; else `"N.NNT"` (two decimals). Reference values: `999→"999"`, `48200→"48.2k"`, `188000→"188.0k"`, `1280000→"1.28M"`, `5.4e9→"5.40B"`, `2.3e12→"2.30T"`.

**Board render (`api/cmd/uzi/tui_board.go`):**
- Column consts `~412-425` (`boardIDWidth`, `boardStatusWordWidth`, `boardAgeWidth`, `boardMileWidth`, `boardCredWidth`, `boardTitleMax`, `boardMileMinWidth`).
- `boardRow` (`~760`) draws the row; `boardRowPrefixWidth(admin, mile, cred)` (`~883`) is the width of every column before TITLE, used to size the title.
- `boardShowMile()` (`~430`) gates the milestone micro-bar on terminal width, dropping it before the title is squeezed (issue #379 discipline); `boardShowCred()` (`~444`) gates the credential column.
- `boardSummary()` (`~515`) is the top-right glyph cluster (`⚑ N · ✎ N · … · N runs`), computed over `m.board.runs` (the whole board, stable under the `/` filter). `boardCredSeg` (`~871`) shows the empty-cell convention to copy for a no-usage run.

**Detail render (`api/cmd/uzi/tui_detail.go`):**
- `detailHeaderLines()` (`~341`) builds the status tag at `~360-367`: `glyph + " " + word` then `" · " + dur`. It has a **combined one-line path** (`~378`) and a **split two-line path** (`~386-405`); cost added to the tag widens `combinedRight`, shifting the combine/split threshold — `padVisual` never truncates and the title truncates first, so the status+cost field is never the one cut.
- `renderLaneRail()` (`~610`) renders in order: crew lanes → `renderMilestones()` (`~807`) → `railRateMeters()` (`~863`, the `ACCOUNTS` block, header emitted at `~948`). The SPEND block inserts between milestones and ACCOUNTS.
- `railRateMeters` appends **whole account entries only while they fit** the remaining rail height (`transcriptViewport() - usedRows - 1`), because `joinColumns` clamps the rail by dropping bottom lines — the SPEND block must obey the same whole-block-or-nothing budgeting so it never half-draws.
- `laneRailWidth = 26` (`~17`) is the rail's fixed column budget.

**Precedent for a Go cost format:** `api/cmd/uzi/admin.go:443` already does `fmt.Sprintf("$%.2f", row.Usage.CostUSD)` for the admin usage table — this PRD adds shared, tested formatters and does not touch that call.

**Test + review seams (`.claude/rules/tui.md`):** the deterministic gate is the `Update→msg` / `View()→string` seam (`tui_render_test.go` / `tui_model_test.go`) with substring / SGR assertions — a screenshot cannot gate. D7 untrusted-field guard is `tui_d7_guard_test.go` + the hostile-render test in `tui_model_test.go`. Offline ANSI→PNG screenshots via `api/cmd/uzi/uxlab` (`devbox run build`, gitignored `png/`). The `tui-ux` agent (`.claude/agents/tui-ux.md`) owns TUI review.

## Solution overview

Surface cost in two terminal places, display-only.

**A — Factory floor (own board), whole dollars.** A right-aligned per-run **COST** column, and a rounded **floor total** appended to the summary cluster. Per the design decision (issue #650 discussion): **no decimals anywhere on the board**, to keep the column width stable, and the floor total is a **rounded** aggregate.
- Per-row cell: `Usage == nil` → blank (never `$0`, matching `boardCredSeg`'s empty convention); `CostUSD == 0` with tokens → `—`; `0 < CostUSD < 0.5` (would round to `$0`) → `<$1` (so a real cost never displays as `$0`, keeping `—` unambiguously "subscription/free"); else `"$" + round(CostUSD)`.
- Floor total: sum the **raw** `CostUSD` over `m.board.runs` where `Usage != nil` (same scope as `boardSummary`, so it does not shrink under a filter), round the **sum** for display (`"$" + round(total)`), drop the segment when the total is 0. Deliberately computed from the raw sum, **not** by adding the rounded per-row cells — so the total is accurate; individual rounded rows may not visibly add up to it (standard dashboard behaviour, documented in `specs/ai.md`).
- Own board only. The admin board keeps no cost column/total (Usage is nil there), the same way it carries no judge marker.

**B — Run view (detail), with cents.** A single run's precision matters and its width is not column-constrained, so B keeps two-decimal cost.
- Headline cost folded into the status tag: `◐ running · 41m · $9.55` (faint, like the duration). `—` when `CostUSD == 0` with tokens; nothing appended when `Usage == nil`.
- A **SPEND** block in the crew rail directly above ACCOUNTS: the total (via the cents formatter, tungsten) over an in/out/cache token breakdown, e.g.
  ```
  SPEND  $9.55
  in 2.4M  out 88.4k
  cache 14.2M 96%
  ```
  where cache% mirrors the web "Tokens in" cache badge's **`cacheDisplayPct`** semantics (`web/src/lib/runUsage.ts:257-274`), not a plain round: the ratio is `CacheReadTokens / (InputTokens + CacheReadTokens + CacheCreationTokens)`, but when both the cached and fresh parts are positive the display is **clamped into `[1,99]`** — `100%` renders only when the fresh part (`InputTokens + CacheCreationTokens`) is 0, and `0%` only when there are no cache reads. This is deliberate: a plain round shows `100%` for the common 97-99.6% run, the exact artifact the web code's 7-line comment (`runUsage.ts:257-263`) forbids. The block is omitted entirely when `Usage == nil`, and obeys the rail height budget (whole block or nothing).

**Why this placement.** The per-row column / headline number answers "which run costs / what did this run cost" on the scan; the floor total / SPEND breakdown answer "what is the floor spending / where did it go" on the drill-in. The SPEND block sits above ACCOUNTS because "what it cost" and "which account paid" (PRD #623) are the same question asked twice — co-locating them is the natural pairing that PRD half-built.

**NO_COLOR / D7.** Cost and token figures are derived numerics, not model-authored text, so they need no `renderer.Plain` (D7) sanitising — but the render test still asserts the D7 guard is unaffected. The `$`, digits, `—`, and `<$1` are plain text and survive an Ascii/NoTTY colorprofile downgrade; the tungsten/faint accent is the only thing stripped, so colour is never the sole cue (verified via the colorprofile downgrade, not the truecolor frame).

## Milestones

- [x] **M1 — Cost/token presentation layer + web-parity tests.** Add shared formatters to `api/cmd/uzi` (a new `tui_cost.go`, or alongside the render primitives in `tui_render.go`): `fmtCostCents(usd float64) string` mirroring web `formatCost` (`$1.87`; `≥1000` drops cents → `$1119`); `fmtCostWhole(usd float64) string` for the board (`0 < usd < 0.5` → `<$1`, else `"$"+round`); `fmtTokens(n int64) string` mirroring web `formatTokens` (the k/M/B/T ladder); and `boardCostTotal(runs []apitypes.RunListItemDTO) (string, bool)` that sums raw `CostUSD` over runs with non-nil `Usage`, rounds the sum, and returns `("", false)` when 0. The `—` (subscription $0) and blank (nil Usage) decisions stay with the **callers**, not the formatters, so the board and detail can differ. **Success:** `task gate:api` green; unit tests pin the exact web-parity reference values (`48200→"48.2k"`, `1280000→"1.28M"`, `5.4e9→"5.40B"`, `999→"999"`; `$1.87`, `$1119`), the board-only cases (`<$1`, whole-dollar rounding), and that `boardCostTotal` rounds the **raw sum** (a fixture where `round(a)+round(b) != round(a+b)` proves it does not sum rounded rows).

- [x] **M2 — Factory floor: COST column + floor total (own board).** Add `boardCostWidth` (6 — fits `$9999` / `<$1` right-aligned) and a `boardShowCost()` width gate, and add a `cost bool` param to `boardRowPrefixWidth` so its width math accounts for the new column. In `boardRow`, draw a right-aligned cost cell after the milestone micro-bar (own board only; blank when `Usage == nil`, `—` when `CostUSD == 0` with tokens, else `fmtCostWhole`). In `boardSummary`, append the `boardCostTotal` segment to the cluster (dropped when 0). **Drop precedence (states the #379 invariant explicitly, since `boardShowMile` already derives its threshold from `boardRowPrefixWidth`, creating a mile↔cost interdependency):** on a narrowing terminal the **milestone micro-bar drops first, then COST** — COST has higher retention priority than the mile bar, and the title is never squeezed. Implement the two gates as one ordered ladder (decide mile, then cost given mile's result) rather than two independent thresholds that can disagree or go circular. (This order is a judgment call — cost is the new headline signal — reversible in review if the mile bar is deemed more valuable.) **Success:** `task gate:api` green; `tui_render_test.go` / `tui_model_test.go` assert — a cost-bearing run renders `$N` (no decimal); a $0-with-tokens run renders `—`; a nil-Usage run renders a blank cell; the summary shows the rounded total; a narrow board drops the COST column before clipping the title, and drops the mile bar before COST; the admin board (`admin=true`) shows neither column nor total.

- [x] **M3 — Run view: headline cost + SPEND block.** In `detailHeaderLines`, append the cost to the status tag on **both** the combined and split paths (faint `· $9.55`; `—` for $0-with-tokens; nothing for nil Usage) and include its width in the combine/split threshold. Add a `renderSpend()` block inserted in `renderLaneRail` between `renderMilestones` and `railRateMeters` **at BOTH call sites** — the no-lanes path (`tui_detail.go:632-637`) and the has-lanes path (`665-670`); a queued/just-claimed run with usage otherwise misses SPEND. Write SPEND into `sb` **before** the `railRateMeters` call so its `usedRows` arg (`strings.Count(sb.String(),"\n")+1`) auto-shrinks the ACCOUNTS budget — do not compute SPEND out-of-band. The block: `SPEND <fmtCostCents or —>` (tungsten total) over `in <fmtTokens> out <fmtTokens>` and `cache <fmtTokens> <cache%>`, omitted when `Usage == nil`, and budgeted whole-block-or-nothing against the rail height like the ACCOUNTS entries. **Success:** `task gate:api` green; tests assert — the header carries the cost substring for a cost-bearing run, `—` for a $0 run, and no cost token for a nil-Usage run; the SPEND block renders above ACCOUNTS with the right total/in/out/cache figures and is absent for a nil-Usage run; the block drops whole (never half-drawn) when the rail is short.

- [x] **M4 — Robustness: NO_COLOR, width, screenshots + tui-ux review.** Assert (in the model test) that the cost `$`/digits/`—` survive an Ascii/NoTTY colorprofile downgrade on both surfaces (colour is not the only cue). Confirm the COST column drop and the SPEND whole-block drop behave under narrow/short terminals. Where the environment supports it (dev machine/reviewer — the worker may lack devbox/nix and this step is not a gate), regenerate the uxlab screenshots (`cd api/cmd/uzi/uxlab && devbox run build`, confirm PNG mtimes newer than the frames) and dispatch the `tui-ux` agent, addressing findings. **Success:** the colorprofile-downgrade assertions are green in `task gate:api`; screenshots regenerated and tui-ux review clean where the environment allows.

- [x] **M5 — Docs, specs, gate.** If `docs/cli.md` documents the TUI board / run view, add a line describing the COST column, floor total, and SPEND block; add a `specs/ai.md` decision note (own-board + detail scope; whole-dollar board vs cents detail; floor total from the raw sum, not summed rounded rows; the `—` / `<$1` / blank conventions; admin-board cost as a follow-up; web `formatCost`/`formatTokens` parity). **Success:** `web/scripts/check-docs.mjs` passes if a user doc was touched; the full `task gate:api` is green.

## Out of scope (stated so it is not silently folded in)

- **Admin/factory board cost.** `AdminListRuns` (`runs.go:86-107`) does not attach `Usage`; adding it needs a `ListActiveRunsAll` query change, sqlc regen, and live-DB tests (the heavier Go gate path). Filed as a follow-up; this PRD keeps the admin board unchanged (no cost column, no total), matching how it already omits the judge marker.
- **Plain-text `uzi run get` / `uzi run list` cost.** Those textual commands also omit cost today; adding it there is a separate CLI-consistency task. This PRD is the interactive TUI board + run view only. (Conventions "does `api/cmd/uzi` need a matching CLI change?" resolves to a documented **follow-up**, not a silent no.)
- **Per-phase / per-agent cost tables in the TUI.** The web usage panel breaks cost down per phase/agent; the TUI SPEND block shows the rolled-up total + in/out/cache only. A deeper breakdown is future work.

## Risks & open questions

- **Rounded rows vs rounded total disagree visually.** By design (accuracy of the total wins). Documented; if it reads as a bug in review, the fallback is to drop the floor total and keep only the per-row column — the per-row column is the higher-value half.
- **Board width pressure.** COST adds ~6 columns to the own-board prefix; on a narrow terminal it and the mile bar both drop before the title clips. The `boardShowCost` gate must be ordered so the title is never squeezed (the #379 invariant). Verified by an M2 narrow-width test.
- **`—` vs `<$1` vs blank must stay distinct.** Three states carry three meanings: blank = no usage recorded, `—` = subscription/$0, `<$1` = a real sub-dollar cost. Conflating any two misleads. Pinned by M1/M2 tests.

## Offline-worker readiness

Every fact this PRD depends on is in-repo: the DTO fields and their nil semantics (`apitypes`, `handler/runs.go`), the render functions and width discipline (`tui_board.go`, `tui_detail.go`), and the web formatting parity (`web/src/lib/formatTokens.ts`) — the reference values are copied into "What already exists" so the worker needs no web access to reproduce them. No open-web lookup is required; there is no external API or docs dependency. Screenshot regeneration (M4) is a reviewer aid gated behind devbox/nix and is explicitly **not** a gate — the deterministic verification is the `tui_render_test.go` / `tui_model_test.go` seam, which runs under `task gate:api` with no extra tooling.

**Workflow-scope guardrail (`.claude/rules/prds.md`).** This PRD touches only `api/cmd/uzi/**`, its tests, and optionally `docs/cli.md`, `specs/ai.md` — **never** `.github/workflows/**` (the worker PAT lacks `workflow` scope; a single touch is an atomic push rejection that loses the whole branch). Before finalize, `git diff --name-only <base>..HEAD` must show zero entries under `.github/workflows/`.

## Decision log

- **Own board + detail only, admin board deferred** — the admin board has no `Usage` on the wire (`runs.go:86-107`); wiring it is an API/SQL change out of proportion to a display feature. Keeps this PRD purely display-side, offline, and small. (issue #650 discussion, 2026-08-24.)
- **Whole dollars on the board, cents in the detail** — per the requester: no decimals in the board table to preserve column width, and round the summed total; the detail view is a single run where precision matters and width is not column-constrained, so it keeps cents. (issue #650 discussion, 2026-08-24.)
- **Floor total from the raw sum, not summed rounded rows** — accuracy of the aggregate over per-row visual reconciliation; documented so the discrepancy is understood, not filed as a bug.
- **`—` / `<$1` / blank are three distinct states** — preserves the web `formatCost` `—`-for-$0 subscription convention while keeping decimal-free board cells, without ever showing a real cost as `$0`.
- **SPEND above ACCOUNTS** — co-locates "what it cost" with "which account paid" (PRD #623), the natural pairing that PRD half-built.
