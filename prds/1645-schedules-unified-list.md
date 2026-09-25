# PRD #1645: Schedules page, one operational list plus a Job catalog tab

**Issue**: [#1645](https://github.com/vtmocanu/uzi/issues/1645) | **Priority**: Medium
**Status**: Draft; design direction approved by the maintainer on 2026-09-25 after a two-agent UX brainstorm
**Evidence baseline**: `c6568a47` (2026-09-25); refresh file anchors on the implementation base
**Mock**: `prds/mockups/1645-schedules-unified-list-mock.html` (open locally). Frame 1 is today's Default jobs tab with its pain points; frames 2 and 3 are the accepted target. Its data is fixture data. Where the mock and the *Binding design decisions* below disagree, the decisions win.

## Problem and outcome

The Schedules page (`web/src/pages/Schedules.tsx`) has two tabs, **Default jobs** and **My schedules**. The Default jobs tab (`web/src/components/DefaultJobs.tsx`) renders one summary row per catalog entry through `ScheduleGroupRow`, and every per-repo control lives in an expanded sub-row (`ScheduleSubRow`). For the common case, a default enabled on exactly one repo, every action costs an expand first:

1. **The summary row shows the wrong cadence.** Its When cell renders the catalog's `entry.cron` and `entry.timezone` (UTC). The user's customized schedule (for example Europe/Bucharest) appears only in the sub-row, and even there without its timezone.
2. **No controls on the row.** Run now, Edit, Clone, Reset, Remove, the pause toggle and the last-fire outcome (matched/examined/started) are all in the sub-row.
3. **"Is it on?" needs a click.** A paused default shows only a faint "1 paused" line under the repo-count pill; there is no toggle on the row.
4. **Two jobs on one tab.** Discovering and enabling catalog jobs (the page-wide `RepoMultiSelect` bar plus per-row Enable) is mixed with operating jobs that already run.

Meanwhile **My schedules** already renders a flat row per schedule with inline controls, but excludes catalog-derived rows (`s.origin === "user"` filter), so an owner operates their schedules in two differently shaped places.

**Outcome.** Operating and discovering are separated:

- **Schedules** tab: every schedule the owner has, catalog-derived and user-authored, one flat row per schedule (one per repo), each showing its own effective cadence and timezone, next run, last-fire outcome, inline actions and toggle. Nothing to expand to act.
- **Job catalog** tab: one card per catalog entry for discovery, each with its own repo picker for enabling, and a link to where it already runs.

This is a web change plus one small CLI parity column. **No API, DTO, store or migration change**: `GET /api/schedules` already returns default-origin rows with `origin`, `catalog_slug`, `customized`, `cron_expr`, `timezone`, `next_fires`, `last_fire`, `sibling_group_id` and `enabled` (`web/src/lib/apiTypes.ts`, the `Schedule` type), and the catalog view (`ScheduleCatalog`) already carries per-entry metadata.

## Related work

| Item | Relationship |
|---|---|
| PRD #589 (`prds/done/`) | Shipped the default-jobs catalog and the Default jobs tab this PRD replaces. |
| PRD #636 | Extracted the neutral `ScheduleGroupRow`/`ScheduleSubRow` shell and introduced sibling grouping on My schedules. This PRD retires the grouped display (Decision 2); the `sibling_group_id` data stays untouched. |
| Issue #690 | Per-repo last-run parity in sub-rows; its `LastRunOutcome`/`LastFireDetail` components are reused on the flat rows. |
| PRD #1093 | Pause-all; unchanged here (Decision 9). |

## Scope

### In

- Rename and restructure the two tabs (**Schedules**, **Job catalog**), landing tab and `?tab=` deep link.
- The unified flat Schedules list: row anatomy, per-origin action sets, sort, filters, folding of fired one-shots.
- The Job catalog card grid with a per-card enable picker and the pre-enable sweep-label warning.
- Retiring the grouped display code the new layout no longer uses (dead-code gates must stay green).
- `uzi schedule list` gains a `SOURCE` column; CLI docs and the embedded CLI skill source are updated.
- Mock-mode fixtures that exercise every row state; tests; user docs (`docs/scheduling.md`, `docs/cli.md`) and the docs mirror.

### Out

- Any API, DTO, SQL, sqlc or migration change. If an implementer finds the UI needs server data it does not have, stop and record it in the Decision Log rather than adding an endpoint.
- Changing what any action does server-side (enable, reset, clone, add-repo, delete, pause, run-now keep their existing API calls and confirm flows).
- A "new since last visit" badge on catalog jobs (needs seen-state tracking; rejected in the brainstorm).
- A jobs × repos matrix view.
- Changing `sibling_group_id` semantics or the CLI's grouped-create behaviour.
- Any creation, modification or validation write under `.github/workflows/**`.
- Any edit to frozen `specs/ai.md`.

## Verified current behavior

Resolved local-code facts at the evidence baseline; the offline worker need not look anything up online.

- `Schedules.tsx` owns the tab state (`useState<Tab>("defaults")`), an APG roving-tabindex tablist (`TAB_ORDER`, `tabId`, `panelId`), the pause-all control and banner, and all mutation handlers (`removeSchedule`, clone, add-repo, toggle, run-now, reset, enable). The inactive tabpanel is unmounted.
- My schedules filters `schedules.filter((s) => s.origin === "user")` and groups rows by `sibling_group_id` via `groupMine()` into `ScheduleRow` (standalone) or `MyScheduleGroup` (≥2 siblings, rendered through `ScheduleGroupRow`).
- The Default jobs tab count is `enabledDefaults` (default rows with `enabled`); the My schedules count is the user-row total.
- `DefaultJobs.tsx` renders the repo-guardrail bar (`RepoMultiSelect`), one `CatalogRow` per `catalog.entries` item, per-repo `SubRow`s and `EnableAnotherRepo`. Enabling fans out client-side through `onEnable(entry, repoIds)` and then arms `SweepLabelWarn` rows.
- `SweepLabelWarn` (`web/src/components/SweepLabelWarn.tsx`) is self-contained: given `{repoId, repoPath, labels}` it checks the forge for missing selector labels and offers "Create label". It can render before an enable.
- `LastRunOutcome` and `LastFireDetail` (`web/src/components/LastRun.tsx`) render a row's last-fire badge and its expandable detail; `formatStamp` formats stamps.
- Menu-button precedent: `web/src/components/triage/TriageActions.tsx` handles Escape and focus return but has **no** arrow-key navigation. Dialog-popover precedent (focus in, Escape, focus return): `web/src/components/ExtendTimePopover.tsx`. Reuse these patterns rather than inventing new ones.
- Handlers that assume the old layout: `cloneSchedule` ends with `setTab("mine")`; `addRepo` auto-expands the new sibling's group via `setExpandedGroups`; `removeSchedule` calls `api.deleteSchedule` directly with **no confirmation**; `enableDefault` fans out with `Promise.allSettled` and, on partial failure, sets an error ("Enabled … on X of N repos; K failed.") but still resolves, so a caller cannot tell which repos failed.
- `SweepLabelWarn` debounces its check (300 ms) and exposes no "check complete" state to its parent.
- Reset (`ResetDefaultSchedule` in `api/internal/store/schedules.sql.go`, behind `uzi schedule reset` and the web Reset action) restores cron, timezone, model, auto-approve, wait-on-limit, MR rework, max issues and output mode to the catalog values, sets `override_subagent_model = false`, **clears** guidance, the harness pin and the credential override, and clears `customized`. The sealed prompt and labels are not touched. The `schedule reset` help text in `api/cmd/uzi/schedule.go` and the embedded CLI skill describe only part of this (they omit guidance, harness, credential override and output mode); M5 corrects them.
- The `Schedule` DTO carries every effective value a default row needs: `cron_expr`, `timezone`, `model`, `override_subagent_model`, `auto_approve`, `wait_on_limit`, `max_issues`, `labels`, `next_fires`, `next_fire_at`, `last_fire`, `status`, `timing`.
- Mock mode: `web/src/mocks/mockApi/schedules.ts` serves schedules and the catalog under `VITE_UZI_MOCK=1`.
- CLI: `uzi schedule list` prints `ID, TARGET, REPO, WHEN, NEXT, ON, HARNESS` (`api/cmd/uzi/schedule.go`, the `p.Table(...)` call near line 336); `--json` already carries `origin` and `catalog_slug`.
- Docs that name the old tabs: `docs/scheduling.md` ("Where to manage them" under *Default jobs*, and the grouping paragraph under *Running a schedule on several repos*); the empty-state copy in `Schedules.tsx` ("shipped default from the Default jobs tab"); the embedded CLI skill `api/internal/uzicli/skill/SKILL.md` lists the `schedule list` columns.

## Binding design decisions

**D1. Tabs.** Two tabs, in this order: **Schedules · N** (N = every schedule the owner has, both origins, including fired one-shots) and **Job catalog · M** (M = catalog entries). The catalog tab additionally shows a neutral pill "K not enabled" when K > 0 catalog entries are enabled on none of the owner's repos; hidden at 0. `?tab=schedules|catalog` selects a tab on load and the URL follows tab changes (replace, not push). Without a valid `?tab=`, land on **Schedules** when the owner has ≥1 schedule, else **Job catalog**. Keep the APG tablist keyboard behaviour.

**D2. One row per schedule; no group summary rows.** The Schedules tab renders every schedule as its own row, whatever its origin or `sibling_group_id`. The grouped/expandable display (`MyScheduleGroup`, `CatalogRow`, `ScheduleGroupRow`, `ScheduleSubRow`) is retired from the page. Sibling data is untouched; siblings simply render as independent rows, which is what they already are server-side.

**D3. Sort.** A row's next fire is `next_fires?.[0] ?? next_fire_at`. Order by band, then within band: (1) parked rows (`status === "error"`), they need attention; (2) enabled rows with a next fire, by next fire ascending; (3) enabled rows with no next fire; (4) paused rows. Within bands 1, 3 and 4, and as the tie-break in band 2: display name, then repo path, then `id` (stable). Fired one-shots are folded (D6). This sort does **not** keep a multi-repo job's siblings adjacent when their next fires differ; that is an accepted tradeoff, and the Job and Repo filters (D5) are the way to see one job across repos.

**D4. Row anatomy** (columns: *Target · repo*, *When*, *Next run*, *Last run*, *Options*, *Actions · On*):

- *Target · repo*: display name (the catalog entry's name for `origin === "default"`, today's `targetTitle(s)` for user rows), then the lock marker (default rows except `self_improve`, same tooltip and aria-label as today), the kind pill (sweep / prompt / self-improve), today's `once` and output-mode badges for user rows, and `customized` when `s.customized`. Second line: the repo path in mono (via `maskRepoPath` for demo mode) and, for default rows, a **from catalog** link that switches to the Job catalog tab and moves focus to that entry's card.
- *When*: the row's **own** `cron_expr` (or one-shot time) with `humanizeCron` and the row's **own** `timezone`. Never the catalog's.
- *Next run*: absolute next fire plus relative ("in 2d 22h"); "paused" when the row is off; "fired" for a fired one-shot; today's pause-all note (`pauseNote`) replaces the relative line when pause-all is active and the row would otherwise fire.
- *Last run*: `LastRunOutcome` with its expandable `LastFireDetail` row beneath (existing issue #690 behaviour), falling back to the bare stamp or "never fired" as today.
- *Options*: chips from the **row's own effective values**, never from `CatalogEntry`: model (or "inherit model"), max issues, auto-approve/wait-on-limit as today's `ScheduleRow` shows them, and the sweep labels (`s.labels`; sealed for defaults, so they equal the catalog's). User rows keep today's `ScheduleRow` chips.
- *Actions · On*, left to right: **Run now**, **Edit**, **Reset to catalog defaults** (default rows with `customized` only, warning-tinted; aria-label names the job and repo; its tooltip states the scope: "Restores all editable settings to the catalog defaults and clears your guidance, harness and token overrides. The shipped prompt and labels are unchanged."), a **More actions** menu, then the pause/resume toggle (aria-label "Pause <name> on <repo>" / "Resume …", as today). The menu holds: **Clone to an editable copy** (all rows); **Add to another repo** (user rows; the existing issue-target disabled reason carries over as a disabled item with its reason as description); **Enable on another repo** (default rows; opens the D7 picker scoped to that entry); **Remove** (all rows, calling the existing `removeSchedule` unchanged; today it deletes with no confirmation, and this PRD keeps that behaviour: moving Remove into the menu already makes it a deliberate two-step action). Reset for a non-customized default is not offered (it would be a no-op).
- Off rows render at reduced opacity as today; a parked row (`status === "error"`) keeps today's treatment and is never folded.

**D5. Filters.** Above the table: a single-select chip group **All · From catalog · Mine · Paused** with counts, and a **Repo** select shown only when schedules span ≥2 repos. Following a card's "Enabled on N repos" link (D7) applies a removable **Job: <name>** chip. Order of operations: filters apply first, then D6 folds within the filtered set, then D3 sorts. Chip counts are computed over **all** schedules (they describe what each chip would show, not the current intersection). Filters live in page-level state: they survive switching tabs and back, but are not persisted or put in the URL. An empty filtered result shows "No schedules match" with a Clear filters button.

**D6. Fired one-shots fold away.** Rows with `timing === "once" && status === "fired"` that survive the filters collapse into one disclosure row at the bottom of the table: "N one-time schedules already fired" plus their issue refs; expanding shows them as normal rows. A single fired one-shot still folds. When the **only** rows matching the active filters are folded, the disclosure starts expanded, so a filter never looks empty while hiding its matches. Folding is presentation only.

**D7. Job catalog tab.** A responsive card grid (4 columns at ≥1280px, down to 1 on phone width). The page-wide repo bar is removed. Each card shows: name, lock marker, kind pill, description, the catalog default cadence humanized with its timezone labelled "catalog default", option chips, and a footer with a status and an **Enable on…** button. Status: **Not enabled** as plain text when no repo runs the entry; otherwise a link **Enabled on N repos** (", M paused" when any are off) that switches to Schedules with the D5 Job filter. The button opens a popover dialog (the `ExtendTimePopover` pattern: focus moves in, Escape closes, focus returns to the button) listing the owner's repos as checkboxes: repos already enabled for this entry show checked, disabled and labelled "enabled", so Enable can never be a no-op. The list scrolls within a max height under a fixed action footer, and gains a text filter when the owner has more than 8 repos.

- **Pre-enable label check.** For a sweep entry with labels, a `SweepLabelWarn` renders for each newly selected repo. Add an optional `onCheckStateChange(state: "checking" | "done")` prop to `SweepLabelWarn`. It reports `checking` **synchronously on mount and whenever its repo or labels change** (before the 300 ms debounce, so there is no clickable window between selecting a repo and its check starting), and `done` only when the **current** check settles, success or error (a superseded check's settlement is ignored). While any selected repo is `checking`, the primary button is disabled and reads "Checking labels…"; once all are `done` it is enabled **whatever the result**, so the guardrail stays advisory (a missing label or a failed check never blocks).
- **Enable and partial failure.** Change `enableDefault` to return per-repo results (`{ enabled: string[]; failed: { repoId: string; message: string }[] }`) while keeping its existing page-level error text. The primary button reads **Enable N** and calls it. Full success: close the dialog; the page notice links to the Schedules tab filtered to that job. Partial or total failure: keep the dialog open; repos that succeeded flip to checked-disabled "enabled" (from the refreshed schedule list), failed repos stay checked with their error shown inline, and Enable retries only those.

**D8. Empty states.** Schedules tab with no schedules: "Nothing runs on a clock yet. Enable a shipped job from the Job catalog, or create your own." with a link to the catalog tab and the New schedule button. Replace the old copy that names "Default jobs".

**D9. Pause-all is unchanged** in position, semantics and banner; it still covers both origins.

**D10. CLI parity.** `uzi schedule list` gains a `SOURCE` column after `TARGET`: the `catalog_slug` for `origin == "default"`, else `custom`. `--json` output is unchanged. No other CLI verb changes.

**D11. Narrow screens.** Below the `md` breakpoint (768px) the table renders each schedule as a stacked card instead of a table row, with no horizontal scroll: line 1 name, badges and the toggle; line 2 repo and "from catalog"; line 3 When and Next run; line 4 Last run; line 5 the action buttons (Run now, Edit, Reset when applicable, More actions). Options chips wrap below. The filter chips wrap; the Repo select goes full width.

**D12. Revealing a new row after clone or add-repo.** Replace `cloneSchedule`'s `setTab("mine")` and `addRepo`'s group auto-expand: after either creates a row, switch to the Schedules tab, clear any active filter that would hide the new row (source, paused, repo, job), expand the fired fold if the row lands in it, and scroll the row into view. Add-repo then moves focus to the new row's name cell. Clone opens the edit modal exactly as today, which holds focus while open; when the modal closes, focus goes to the cloned row's name cell. Enabling from the catalog does not jump tabs (D7 closes the dialog and offers the link instead).

**D13. Accessibility.** Every icon-only button keeps an aria-label naming the job and repo. The More actions menu follows the APG menu-button pattern: Enter/Space/ArrowDown open it with focus on the first item, ArrowUp/ArrowDown move between items (wrapping), Home/End jump, Escape closes, and focus returns to the button. Copy Escape and focus return from `TriageActions`; arrow-key navigation is **new** and must be implemented and tested here. The toggle keeps `role="switch"` semantics of the shared `Toggle`. Filter chips are real buttons with `aria-pressed`. The catalog card status link and "from catalog" link are real links or buttons, never clickable spans.

**D14. Delete what the layout no longer uses.** `ScheduleGroupRow.tsx`, the table half of `DefaultJobs.tsx`, `MyScheduleGroup`, `groupMine` and their tests go if unused; knip (`task deadcode:web`) gates unused exports at `error`, so leftovers redden the gate. Keep `AddAnotherRepo`'s repo-picker logic only if the menu item reuses it.

## Milestone dependency graph

| Phase | Milestone | Depends on | Main files |
|---|---|---|---|
| 1 | M1 Unified Schedules list | none | `web/src/pages/Schedules.tsx`, new row component under `web/src/components/` |
| 1 | M5 CLI SOURCE column | none | `api/cmd/uzi/schedule.go`, its test, `api/internal/uzicli/skill/SKILL.md`, `docs/cli.md` |
| 2 | M2 Filters and fold | M1 | `Schedules.tsx` / the list component |
| 2 | M3 Job catalog tab | M1 (tab structure) | `web/src/components/DefaultJobs.tsx` (rewritten as the catalog grid) |
| 3 | M4 Retire old code, fixtures, tests | M1–M3 | `ScheduleGroupRow.tsx`, tests, `web/src/mocks/mockApi/schedules.ts` |
| 3 | M6 Docs | M1–M3, M5 | `docs/scheduling.md`, `docs/cli.md`, docs mirror |
| 4 | M7 Browser acceptance | all | none (validation) |

M1 and M5 touch disjoint files and can run in parallel; M2 and M3 both touch the page shell and should be sequential within one run.

## Milestones

- [ ] **M1 Unified Schedules list.** Tabs renamed and landing/deep-link logic per D1; every schedule rendered as a flat row per D2–D4 with per-origin actions and the More actions menu; D11 narrow-screen cards; D12 reveal after clone and add-repo. Tests assert: a default row shows its own `timezone`, `cron_expr` and `model` (not the catalog's, using a fixture where they differ); a customized default shows Reset inline and a non-customized one does not; the toggle, Run now and Edit are reachable without any expand; user and default rows share one table; the D3 band order including a parked row; clone and add-repo each switch to Schedules, clear a hiding filter and reveal the new row (add-repo focuses it; clone focuses it after the edit modal closes); the More actions menu's open keys, arrow-key wrap, Home/End and Escape focus return.
- [ ] **M2 Filters and fold.** D5 chips with counts, the conditional repo select, the Job chip, the empty-filter state, filter state surviving a tab round-trip; D6 fold of fired one-shots. Tests cover each filter, chip counts over the full set, the fold count within a filtered set, the auto-expanded fold when only folded rows match, and the never-folded parked row.
- [ ] **M3 Job catalog tab.** D7 card grid, per-card enable dialog with enabled repos locked, `SweepLabelWarn`'s new `onCheckStateChange`, the `enableDefault` per-repo result, "K not enabled" tab pill, status link into the filtered Schedules tab, D8 empty state. Tests: enabling only offers repos not yet enabled; the fan-out receives exactly the newly checked ids; Enable is already disabled on the render right after a sweep repo is checked (inside the debounce, before any network call); Enable is disabled while a label check is in flight and enabled after it settles with a missing label; a partial failure keeps the dialog open, locks the succeeded repos and retries only the failed ones; the dialog's focus and Escape behaviour; "Not enabled" is not a link; the status link applies the Job filter.
- [ ] **M4 Retire old code, fixtures and tests.** D14 deletions; mock fixtures include a default enabled on 2 repos, a customized default, a paused default, a paused user sweep, ≥2 fired one-shots, one parked (`error`) row and one catalog entry enabled nowhere; rewrite `Schedules.test.tsx` / `DefaultJobs.test.tsx` expectations, following the copy-change rule in `.claude/rules/web.md` (grep the retired strings "Default jobs", "My schedules", "Enable on another repo:", "Pick at least one repo" across the test tree and repoint negative assertions). `task gate:web` green.
- [ ] **M5 CLI SOURCE column and reset help.** D10 plus a table test; update `docs/cli.md` and the column list in `api/internal/uzicli/skill/SKILL.md`. Correct the `schedule reset` scope everywhere it is described (the help text in `api/cmd/uzi/schedule.go`, the embedded skill's `uzi schedule reset` entry, `docs/cli.md`, `docs/scheduling.md`) to match `ResetDefaultSchedule` as stated in *Verified current behavior*. `task gate:api` green.
- [ ] **M6 Docs.** Rewrite `docs/scheduling.md`'s "Where to manage them" and the multi-repo grouping paragraph for the new tabs and flat rows; run `task docs:sync` and commit the mirror (`TestEmbeddedDocsMatchSource`); `task check-docs:web` green.
- [ ] **M7 Browser acceptance in mock mode.** With `VITE_UZI_MOCK=1`, capture both tabs in light and dark themes at 1440px and 390px wide, the open More actions menu and the open enable dialog; keyboard-walk the tablist, a row's actions, the menu and the dialog; at 390px, operate Run now, the toggle and More actions on a card and confirm the page has no horizontal scroll (`document.documentElement.scrollWidth <= innerWidth`). Attach the screenshots to the PR. Remember the blind-instrument limits in `.claude/rules/web.md` (native `title` tooltips and `<select>` popups do not appear in screenshots).

## Success criteria

1. For a default enabled on one repo, Run now, Edit, the toggle and the last-fire outcome are operable with zero expand clicks.
2. The Schedules tab never shows a catalog cadence or timezone for an enabled row; every row's When matches `uzi schedule get <id>`.
3. A catalog-derived and a user-authored schedule on the same repo render in the same table with the same columns and control positions.
4. Enabling a catalog job cannot target a repo where it is already enabled, and a missing sweep label is shown before the enable, not after.
5. `task gate:web` and `task gate:api` are green; `task deadcode:web` reports no unused exports.
6. No API, SQL or migration file changes in the diff.
7. At 390px every schedule's Run now, toggle and More actions are operable without horizontal scrolling.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Owners with many repos get a long list (one row per repo) | D5 filters (source, paused, repo, job), D6 fold, D3 sort puts what fires next on top. |
| Losing the grouped view some multi-repo users relied on | The next-fire sort does not keep siblings adjacent; the Job and Repo filters replace the drill-down. Recorded as a deliberate reversal of PRD #636's display (not its data). |
| Enabling while a label check is still in flight | D7 disables Enable only while checks run, never on their result. |
| A partial enable failure hidden by a closing dialog | D7 keeps the dialog open with per-repo errors and retries only the failed repos. |
| Negative test assertions silently rot on the copy change | M4 applies the `.claude/rules/web.md` retire-a-string sweep. |
| Menu moves Clone/Remove one click deeper | Accepted: they are rarer than Run/Edit/toggle; the brainstorm ranked inline access to the frequent actions higher. |
| Dead code left behind by the grouped layout | knip gates unused exports at `error`; D14 lists the expected deletions. |

## Decision and progress log

- **2026-09-25, direction.** Four directions were mocked and compared by two agents (Claude and a Codex peer): A (flatten single-repo defaults in place), B (Running section plus catalog cards on one tab), C (jobs × repos matrix), D (one operational list plus a catalog tab). Both recommended D: A reverts to hidden controls as soon as a job runs on two repos; B keeps user schedules on another tab and grows long with many repos; C cannot carry cadence, outcomes and per-repo actions. The maintainer chose D.
- **2026-09-25, review round 1 (Codex peer).** Resolved: Reset's real scope and effective-value Options (D4); Remove keeps today's no-confirm behaviour, stated rather than assumed (D4); pre-enable label check state and partial-failure handling (D7); clone/add-repo reveal (D12); honest sort tradeoff and stable ordering (D3); filter/fold order of operations (D5, D6); plain "Not enabled" status and a scalable picker (D7); narrow-screen cards (D11); corrected menu/dialog precedents.
- **2026-09-25, review round 2 (Codex peer).** Resolved: the label-check race (`checking` reported synchronously before the debounce, D7); Reset's full scope verified in `ResetDefaultSchedule`, with the stale CLI/skill reset text added to M5; arrow-key menu navigation is new work (D13); clone focus order (D12). Peer verdict: LGTM once the first two were fixed.
- **2026-09-25, no "new" badge.** A catalog "new" badge would need per-user seen-state; the "K not enabled" pill (D1) gives discovery without new storage.
