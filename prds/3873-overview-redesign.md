# Overview redesign: design brief (epic #3873)

**Status**: Parked (`Later`). This is the epic's shared design brief, not a PRD: it has no milestones. Each child PRD implements one slice and cites the sections below; where a child PRD and this brief disagree, the child PRD wins for its own scope.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html` (the design to build). Reference only: `prds/mockups/3873-overview-redesign-exploration-mock.html`.

## Child PRDs and order

| PRD | Outcome | Blocked by |
|---|---|---|
| #3878 (`prds/done/3878-overview-preview-decisions.md`) | The new Overview at `/overview/next`: header with status sentence and health chip, banners under the header, Needs your decision, runs grouped Yours / Other users (admin) | none |
| #3879 (`prds/done/3879-overview-issue-line.md`) | The issue-work line, filtering the runs widget | #3878 |
| #3880 (`prds/done/3880-overview-capacity-held-work.md`) | Capacity rail and Held work; retires the worker-load card and custody banner on the preview | #3878 |
| #3881 (`prds/done/3881-overview-usage-by-user.md`) | Usage card with the You / Factory toggle and the admin by-user table | #3878 |
| #3875 (`prds/done/3875-command-menu.md`) | The Cmd K command menu | #3878, #3880 |
| #3876 (`prds/done/3876-usage-harness-split.md`) | Per-user Claude / Codex split in admin usage (API, CLI, table column) | #3881 |
| #4052 (issue) | Issue work: an admin-only You \| All users scope toggle pinned right (same as the usage card, default You, remembered per browser), and no Show all button; target mock `prds/mockups/3873-overview-issue-work-scope.html` | #3881 |
| #4041 (issue) | Needs your decision: group same-kind decisions and PR reviews into expandable rows, kind icons, plan milestone line, two-column rule; target mock `prds/mockups/4041-needs-decision-after.html` | #3881 |
| #4040 (`prds/done/4040-overview-pending-question.md`) | The pending question quoted inline in Needs your decision (owner-scoped server excerpt) | #3881, #4041 |
| #4042 (issue) | Admin People chip: how many people are active now, with a who-is-doing-what popover; target mock `prds/mockups/3873-overview-people-chip.html` | #3881 |
| #4078 (issue) | Capacity rail: richer worker rows (gauges, problem badges on the collapsed row, runs on the worker, version and uptime); target mock `prds/mockups/3873-overview-worker-rows.html` | #4042 (same page and fixtures) |
| #4082 (issue) | Issue work: cross-check state in the run chip ("cross-checking", "cross-check: revise / blocked / unavailable"); target mock `prds/mockups/3873-overview-issue-work-crosscheck.html` | #4078 (same page and fixtures) |
| #3877 (`prds/3877-overview-promote.md`) | Promote the new Overview to `/dashboard` and retire the old one | #3878, #3879, #3880, #3881, #4052, #4041, #4040, #4042, #4078, #4082, and the maintainer's trial verdict |

Every child edits the preview page, so run them one at a time, in this order: #3878, #3880, #3879, #3881, #4052, #4041, #4040, #4042, #4078, #4082, #3876; #3875 any time after #3880; #3877 last.

## Mock contract (applies to every child PRD)

- The mock is the presentation reference: layout, order, wording, colour roles and states. Implement it, not only the text spec.
- Small deviations for consistency with the app are expected: existing components, tokens, spacing, wording that matches other pages.
- A larger deviation (a dropped or added element, a different layout, navigation or information) only when necessary, listed in the PR body under `Mock deviations` with its reason. An absent list claims parity.
- Validation compares the result with the mock side by side (desktop and 400 px, at least Ember and Dawn) and the PR carries the screenshots.
- Precedence: a child PRD's text wins where it specifies; the mock governs what the text leaves out. Mock sample data (names, numbers, repos) is never a spec.
- Offline workers: the mock loads IBM Plex from Google Fonts and falls back to the system font without network; that is expected.

## The mocks

Both are standalone HTML files with sample data and no backend; open them locally in a browser.

1. **`prds/mockups/3873-overview-redesign-mock.html`, the design to build.** The new Overview structure on uzi's **existing** design: theme tokens copied from `web/src/index.css`, components mirroring `web/src/components/ui.tsx` (Card, SectionTitle, Badge, CountPill, Button), today's sidebar with the account usage meters unchanged. Mock controls (bottom right, not part of the design): theme (System (Ember / Dawn), Ember, Mission control, Dawn, Hall, Shadow), typeface (IBM Plex or system), a "Needs you: Nothing" scenario, and a banner preview (health danger, health attention, vault locked, maintenance). Append `#factory` to the URL to open on the Factory usage scope. It loads IBM Plex from Google Fonts; the product already bundles Plex.
2. **`prds/mockups/3873-overview-redesign-exploration-mock.html`, reference only.** The first exploration, with a new visual language (slate palette, Archivo, a top bar, quota meters moved out of the sidebar). The maintainer prefers the current themes and fonts, so only its structure carries forward; it is kept for the record and for the reasoning behind the layout.

Both mocks land with PR #3874. Interactive in both: line stations and bays filter the Runs list; You / Factory and Last 7 days / All time toggles; worker rows expand; <kbd>⌘K</kbd> / <kbd>Ctrl K</kbd> command menu; navigation drawer below 960 px. Figures are sample data (demo repos `vtmocanu/atlas-api`, `team-beta/billing-service`; demo user Robin Diaz). Elements tagged **Proposed** need data the API does not serve today.

Made by a Claude lead with an independent Codex reviewer: direction agreed, two review rounds on the exploration (5 + 5 findings, all folded; see [Decision log](#decision-log)), then a review of the ported mock. Reviewed from source plus desktop screenshots; phone-width and keyboard behaviour were built to spec but not driven in a browser.

## Problem: what the current Overview gets wrong

Ranked, from `web/src/pages/Dashboard.tsx` and a live instance:

1. **Actionable work is buried.** "2 need you" is a hint under the Active runs tile; the named runs appear only in Recent runs at the bottom, below worker load and usage.
2. **Inventory outranks production.** The Repos and Agent templates tiles carry the same weight as active work. Worker names and cpu / mem / disk text dominate the first screen.
3. **Attention signals blur.** Count badges for enabled schedules, active runs and large backlogs (`99+`) look the same as signals that need action.
4. **Custody needs interpretation.** Custody slots used, open holds and decisions needed sit together without hierarchy.
5. **Usage dominates.** Side-by-side You and Factory columns double the load, and large API-equivalent dollar figures overshadow output and read like actual spend.

## Prior work

- **`prds/mockups/alloy-theme-review.html`** (added 2026-09-08 in `8d78b1137`; no issue, PRD or recorded decision found): an earlier unshipped proposal with the same thesis. No metric hero, human decisions get the only warm surface, work in rows, operations in a quiet side column, structure follows the factory's own sequence. It covers Overview, Board and Run. This issue goes deeper on the Overview only; the PRD should reconcile the two (alloy's Board and Run screens have no counterpart here).
- **PRD #21** (`prds/done/21-mission-control-theme.md`, Decision 1): a theme is tokens; no component file may branch on the active theme; structural ideas need a separate features PRD.
- **PRD #1167** (`prds/done/1167-lights-on-themes.md`): light themes, per-polarity theme pairs, the bundled IBM Plex typeface.
- **Shipped Overview pieces to carry over:** the custody alert (PRD #1349 M6, `web/src/components/CustodyBoardAlert.tsx`), the health card (PRD #1484 M5), worker load (PRD #49), the "now" activity line (PRD #1064 M3), progress flags (PRD #2602), the usage card toggle (#2430), failed-run outcomes (PRD #1293), the onboarding checklist. The Inbox nav item was retired (`web/src/components/AppShell.inboxRetired.test.tsx`); the decisions strip must not bring it back.
- Closed issues and PRs searched for "overview redesign", "dashboard redesign", "overview layout", "command palette" and "cmd-k": no earlier attempt.

## Design

### Information architecture, top to bottom

The page keeps today's shell and content column (`max-w-[68rem]`, `web/src/components/AppShell.tsx:1402`).

0. **App-wide banners.** On the Overview only, the stack appears **under the page header** (below the greeting and status sentence, above Needs your decision; maintainer calls D18 and D20). PR #3952 kept one `AppBanners` instance mounted in `AppShell` on every route and portalled the preview header into a shell slot above it (`web/src/components/PageHeaderSlot.tsx`); snooze and dismissal state therefore survives navigation. Order and behaviour stay as today: vault required prompt, vault locked banner with its inline unlock form (`VaultLockedBanner`, `web/src/components/VaultControls.tsx`), maintenance mode (`MaintenanceBanner`), update escalation (admins), instance-health danger (`HealthDangerBanner`, admins) and health attention (`HealthAttentionBanner`, admins). The Overview adds no second health card; its header chip mirrors the same severity. The sidebar's vault chip stays as today.
1. **Page header** (`PageHeader`): "Welcome, <name>" plus one status sentence ("3 decisions and 2 pull requests are waiting on you. 4 runs are executing, 1 is queued and 1 is parked until its usage window reopens."). Actions on the right: a compact health chip for admins (fed by `HealthOverviewCard`'s data; replaces today's full-width health strip) and a Search <kbd>⌘K</kbd> button. Non-admins keep today's `HealthPlatformLine` (`web/src/components/HealthPlatformLine.tsx:47`) when their own hosted fleet is stuck.
2. **Needs your decision** card: plan review, question, held-work decision, PRs open for review. **Only the viewer's own items, for admins too**: these are owner actions, and nobody can decide for another user. One explicit verb each: Review plan, Answer, Review held work, Review PR. **It self-hides when nothing needs the owner**, as the custody banner does today; the status sentence then reads "Nothing needs you right now." It replaces the custody banner as the place a held-work decision surfaces, but **must keep every state the banner covers today** (`custodyAlertView`, `web/src/lib/recovery.ts:202`): blocked runs (danger tone, "Held work is blocking new runs"), admission at its limit ("Custody capacity reached") and pending decisions, each even when the others are zero. No Review CTA unless `decision_needed > 0`; the Held work card shows the capacity and blocked states with today's headlines.
   - The onboarding checklist ("Get the factory running", shown until token, forge, repo and worker are all set) stays, above the decisions card for a new user.
3. **Issue work**, the production line (the signature element): stations Queued, Planning, Plan review (drawn as a gate), Building, PR open; bays below for Asking you, Parked and Other work. Clicking a station or bay filters the Runs list. **Scope:** a normal user's line counts their own runs; an admin's counts every user's active runs (the factory floor), and other users' pips carry the owner. PR open stays the viewer's own (see Run populations). Another user's run waiting at the gate shows on the line with no action.
4. **Two columns.** Runs: today's Recent-runs row anatomy (title, "now" line, repo and issue, iteration and milestone badges, `StatusPill`) plus the stage filter and a stalled badge, **grouped like today's Runs page**: "Yours" first, then for admins only "Other users (admin)" with other users' active runs and the owner's email in the meta line (masked in demo mode). That is today's `RunsList` pattern (`web/src/pages/RunsList.tsx:664`: `adminListRuns` rows whose `owner_email` differs from the viewer's); the Runs page keeps its "Factory status (admin)" title because that card also lists workers. The stage filter applies to both groups; an empty group hides. Capacity rail: run slots in use / closed / open, then workers (online, runs x/y and draining badges as today, cpu / mem / disk behind a disclosure), then Held work with today's custody pips and three separate figures.
5. **Usage** card: You / Factory (admins) as a toggle instead of side-by-side columns, All time / Last 7 days as today; cost (API-equivalent), PRs opened and cost per PR, failed-run rate, tokens; the finished-run outcome bar; top causes; latest failure (all time). No cost-over-time chart.
   - **By user** (Factory scope only): today's `PerUserUsageTable` (`web/src/components/UsageCards.tsx:321`) trimmed to User, Runs, PRs, Failed, Fail rate, Last failure and Cost, sorted by cost, with a Factory total row, following the same window toggle. Fail rate divides by finished runs, not by Runs (`run_count` counts usage-bearing runs, a different population). Last failure ("3h ago") is lifetime-only (`last_failed_at`, `apiTypes.ts:4306`), so it shows the same value in both windows; the header tooltip says so. A **Claude / Codex split** per user was Proposed here and is delivered by #3876 (`harness_runs` / `last7_harness_runs` on `AdminUsageUser`). Today's Tokens, Out and Cost share columns drop from the Overview (they could live on an admin usage page).

The sidebar stays exactly as today, including the account usage meters under the user block (`web/src/components/SidebarUsageLimits.tsx`).

### Visual language: the existing design

- No new theme and no new font. Every colour is an existing token; every surface is an existing component (Card, SectionTitle, Badge, CountPill, Button sizes and variants from `ui.tsx`).
- Line colours reuse the run status tones (`RUN_STATUS_TONES`, `web/src/components/ui.tsx`): queued `queue`, planning `plan`, awaiting approval `warning`, running `info`, PR open `ok`. Flowing dashes run into stations with live work (Planning, Building) in that station's tone.
- Motion: only the line flow and the existing pulse on live status dots; both stop under `prefers-reduced-motion`.
- The mock renders under all five themes because it only reads tokens. Hall keeps its dark frame. Shadow needs surface markers: the mock puts `data-live` on the issue-work card (a dark live surface) and `data-attention` on the decisions card (the rust rail and tint), following the `[data-theme="shadow"] [data-live]` / `[data-attention]` rules in `web/src/index.css`. The PRD picks the final markers.

### Model-fidelity rules

The mocks were checked against these; the real build needs a test per rule.

- "Active" includes waiting and paused runs; it does not mean executing agents.
- **Stage derivation** goes through `effectiveRunStatus` (`web/src/lib/runBadge.ts:202`). Planning comes from `is_planning` (on `Run`, which the Overview's `RunSummaryItem` derives from: `web/src/lib/apiTypes.ts:3339`). `awaiting_approval` with `is_revising` (`apiTypes.ts:3345`) means actively revising, not waiting on the owner. `progress.state` can be `stalled` and `progress.phase` can be empty (`apiTypes.ts:2959`). The same names at `apiTypes.ts:932` and `:939` belong to the board's `LatestRun` projection, not to the Overview's rows. Keep an executing fallback so a run never drops off the line.
- **The line is scoped to `kind = issue`.** Other kinds (`fixtures/run-kinds/registry.json`: chat, judge, job, prompt, task, self_improve, cross_check, ci_fix, mr_rework) go to Other work; ci_fix and mr_rework sit there too (D21).
- **Questions** (`awaiting_input`, `awaiting_followup`) are not assigned to a stage; they sit in Asking you.
- **Parked** separates runs that resume on their own from runs that need the owner, and shows the reason. Derive this from the run's own evidence, never from the raw status: a `paused` run can auto-release (a weekly-reserve pause, `hold_reason` `weekly_reserve`, `web/src/lib/runBadge.ts:591`), and a `recovery_wait` run can need the owner. Use the effective status (`effectiveRunStatus` reads `hold_reason`, `runBadge.ts:208`; `isReserveHeldPause` detects the weekly-reserve pause, `runBadge.ts:605`) and the recovery fields `StatusPill` reads (`recovery_wait_cause`, `recovery_wait_phase`, `recovery_retry_not_before`; `apiTypes.ts:3759`, `apiTypes.ts:3764`). On `Run`, `hold_reason` is at `apiTypes.ts:3946`. The `progressing` / `needs_owner` tag (`apiTypes.ts:1683`) lives on the admin maintenance readout (`MaintenanceRunItem`, `apiTypes.ts:1676`), not on run summaries: reuse its classification semantics only, and if the run fields cannot express the split, name the owner-scoped API field the PRD needs. An owner-action park also belongs in Needs your decision.
- **PR open shows only PRs opened by the viewer's own uzi runs, for admins too** (see Run populations): a run with `mr_iid != null` and an observed `mr_state === "opened"`, deduplicated by repository and PR number. Human, Renovate and other bot PRs on the same repos are out of scope here; a full forge PR view is a separate feature. The "Review PRs" decision lists only the viewer's own PRs. `mr_state` is a cached hint (`apiTypes.ts:3405` on `Run`), so word it that way ("PR state as last seen by uzi"). Count PRs (deduplicated), not runs, and keep them out of active totals. Completed does not mean merged, and not every run opens a PR. The action is Review PR, never Merge.
- **Custody:** custody slots, open holds and decisions needed stay three separate figures (`RecoveryCustodyHolds`, `apiTypes.ts:5660`; `decision_needed`, `apiTypes.ts:5627`). A hold protects committed but unpublished work per claim generation (`docs/cli.md:1423`), during healthy execution too, not only after a worker is lost.
- **Workers:** online can mean draining, and draining alone is not owner attention. An image roll waits for genuine idle, so never promise "rolls after this run". Ephemeral workers retire after completion once retained work is safe.
- **Slots:** "open" means advertised spare capacity (`max_concurrent_runs` on `Worker` in `web/src/lib/apiTypes.ts`), not guaranteed schedulability: credentials, cordons, capabilities and admission also gate a claim. Dedicated cross-check slots count separately, while legacy cross-checks remain in `active_runs`; unknown caps exist.
- **Usage:** costs are API-equivalent estimates; keep the incomplete-data disclosure (`aggregateDisclosure` from `web/src/lib/costStatus`, used by `web/src/components/UsageCards.tsx`). The failed-run rate is failed / finished, with plan_rejected split out (`RunOutcomes`, `apiTypes.ts:4304`). `last_failed_*` is lifetime-only (`apiTypes.ts:4306`), so Latest failure is labelled all time. Cost per PR divides by PRs opened.
- Factory usage and instance health are admin-scoped; a non-admin sees only their own.
- Findings (bugs in target code) and Judge recommendations (factory improvements) stay distinct.
- Untrusted text (issue titles, agent activity, question text) renders through `stripUnsafeChars`, and demo mode masks names and repo paths (`maskName`, `maskRepoPath`), as `Dashboard.tsx` does today.

## Data readiness

| Mock element | Source today | Gap |
|---|---|---|
| Stages, live activity, milestones, iterations, stalled flag | `api.listRuns({ view: "summary" })` (`Dashboard.tsx:154`), own-scoped (`ListRuns`, `api/internal/handler/runs.go`) | none |
| Admin: other users' runs, line counts | `api.adminListRuns({ view: "summary" })` (`web/src/lib/api.ts:486`) → `GET /api/admin/runs` (`AdminListRuns`, `api/internal/handler/runs.go:137`, `routes_admin.go:48`), same summary projection; `owner_email` is the discriminator since `RunSummaryItem` has no `is_mine` | none; a non-admin must never call it |
| Plan reviews and questions | run statuses | none |
| Held-work decision and custody figures | Owner `GET /api/recovery/holds?state=open` (`api.getRecoveryHolds`, `CustodyBoardAlert.tsx`); PR #3952 still renders that alert on the preview | none; #3880 moves the preview read into an independent shared hook poll |
| PRs open | `mr_iid`, `mr_state` on runs | no freshness timestamp; use cached-state wording or add one |
| Slots, workers, cpu / mem / disk | `api.listWorkers()` through `useOverviewData`; preview currently renders `WorkerLoadCard` only for workers with stats | #3880 must list every owner worker; closed slots derive from cordon, drain or an in-progress hosted upgrade, with offline and unknown-cap workers outside the bar |
| Outcome figures and causes | `SelfUsage` (`apiTypes.ts:4326`) and the admin usage response | none |
| By user: runs, PRs, failed, fail rate, last failure, cost, both windows | `AdminUsage.users` (`AdminUsageUser`: `run_count` / `last7_run_count`, `outcomes` / `last7_outcomes` incl. lifetime `last_failed_at`, `pr_count` / `last7_pr_count`, `usage` / `last_7_days`) | none |
| By user: Claude / Codex split | `AdminUsageUser.harness_runs` / `last7_harness_runs` (delivered by #3876, with the `uzi admin usage` CLAUDE/CODEX column) | none |
| ⌘K search | none | v1 is navigation plus needs-you actions; global search is a separate feature |
| Health chip | `HealthStatusProvider` (`web/src/components/AppShell.tsx:1319`) | none |

### Run populations (what the lists can and cannot show)

- **Own runs**: `ListRunsForUser` (`api/internal/store/queries/runtime.sql:782` for the kind filter): the viewer's newest 200 runs of any status, excluding `chat`, `judge` and `cross_check`.
- **Admin, every user**: `ListActiveRunsAll` behind `AdminListRuns` (`api/internal/handler/runs.go:137`): the newest 500 non-terminal runs, same kind exclusion.
- **Consequences, accepted as scope** (no new endpoint): Other work shows only the listed non-issue kinds (`job`, `prompt`, `task`, `self_improve`, `ci_fix`, `mr_rework`); judge, chat and cross-check runs never appear. PR open shows only the viewer's own PRs, for admins too, because completed runs exist only in the owner list. Counts are as complete as these lists: an active run older than the viewer's newest 200 runs is not counted, the same limit today's Overview has. A count endpoint is out of scope unless the trial shows the limits matter.
- **PR identity**: deduplicate open PRs by repository plus PR number (`repo_id`, `mr_iid`), since several runs can share one PR.
- **Admin rows are thinner**: both handlers serve the same summary keys, but the admin rows leave usage, the judge verdict and to-triage count, the harness decision and the capture-aware landing state unset (`api/internal/handler/runs.go`, `AdminListRuns` against `ListRuns`). The Other users group must not render those fields.

## Reuse map

Build from what exists; do not re-implement these. The original anchors were checked on `ba230ce3`; capacity and held-work seams below were refreshed on `origin/main` `e6cafe3f2` (2026-11-05). Other children's anchors remain historical until their dispatch checks.

| Need | Reuse |
|---|---|
| Effective status, buckets for the status sentence | `effectiveRunStatus` (`web/src/lib/runBadge.ts`), `isAccountCooldownWaiting` (`runBadge.ts`), `activeRunBuckets` (`web/src/lib/overview.ts`, moved there by #3878) |
| Status pill, iteration and milestone badges | `StatusPill` (`web/src/components/ui.tsx:634`), `Badge`, `milestoneBadge` (`runBadge.ts:1029`) |
| Stalled and waiting flags, health word | `RunProgressCell` / `progressShowsStall` (`web/src/components/RunProgressCell.tsx:100`, `:38`), `RunHealthBadge` (`web/src/components/RunHealthBadge.tsx:12`) |
| Issue ref and PR chip | `RunIssueRef` (`web/src/components/RunIssueRef.tsx:11`), `MrChip` (`web/src/components/MrChip.tsx:27`) |
| "Now" line age | `activityAge` (`web/src/lib/runActivity.ts:126`) |
| Worker badges, upgrade evidence and stat line | `WorkerRunBadge`, `WorkerCordonBadge`, `WorkerUpgradeBadge`, `WorkerStatLine` (`web/src/components/WorkerRunBadge.tsx`, `WorkerCordonBadge.tsx`, `WorkerUpgradeBadge.tsx`, `WorkerStats.tsx`); current preview's `WorkerLoadCard` filters to `hasStats` |
| Custody states, admission slots and headlines | `custodyAlertView` and `recoveryCapacityUsed` (`web/src/lib/recovery.ts`), `CustodyBoardAlert` (`web/src/components/CustodyBoardAlert.tsx`) |
| Health | `HealthOverviewCard` (`web/src/components/HealthOverviewCard.tsx:36`), `HealthPlatformLine` (`HealthPlatformLine.tsx:47`) |
| Usage figures, disclosure, window preference | `UsageCard` (`web/src/components/UsageCards.tsx:253`) and its helpers (`FailedRunsBlock`, `TopCauses`, `SinceLastFailedRun`) |
| By-user table | `PerUserUsageTable` (`UsageCards.tsx:321`), trimmed |
| Yours / Other users (admin) run groups | `RunsList` (`web/src/pages/RunsList.tsx:664`) and its row's `showOwner` |
| App-wide banners (unchanged) | `VaultLockedBanner` (`web/src/components/VaultControls.tsx:65`), `MaintenanceBanner` (`MaintenanceBanner.tsx:28`), `HealthDangerBanner` (`HealthDangerBanner.tsx:32`), `HealthAttentionBanner` (`HealthAttentionBanner.tsx:8`) |
| Sidebar account meters (unchanged) | `SidebarUsageLimits` (`web/src/components/SidebarUsageLimits.tsx:107`) |
| Nav count pills | `CountPill` (`ui.tsx:434`) |
| Polling, untrusted text, demo masking | `usePollWhileVisible` (`web/src/lib/usePollWhileVisible.ts:14`), `stripUnsafeChars` (`web/src/lib/safeText.ts:87`), `maskEmail` / `maskName` / `maskRepoPath` (`web/src/lib/demoMask.ts:31`, `:42`, `:54`) |

## Constraints

- **Layout cannot be selected by theme id** (PRD #21 Decision 1; `web/src/index.css:13`): themes recolour one component tree. The new Overview must read tokens only, so every existing theme works unchanged.
- **No new fonts or external assets.** CSP is same-origin for fonts, styles and scripts (`web/nginx.conf:23`; the chart's `deploy/chart/templates/web-configmap.yaml` carries the same header). IBM Plex is already bundled (`web/src/main.tsx:18`, pinned by `web/src/lib/font-imports.test.ts`).
- **Gates (a uzi run):** follow the API/web/repo in-run boundary in `AGENTS.md`: `task check:web` plus the touched and directly affected Vitest files and the browser cases for changed UI (AppShell has a large behaviour suite: run it when the shell changes), plus companion checks for changed files (`docs:sync` + `check-docs:web`, `check:changelog-entry`). Full `test:web`, `test:web-browser` and `gate:repo` stay pending exact-head PR CI.
- **A per-user layout preference** (option C below) adds a migration, a `/me/settings` field with validation, api-contract fixtures, and CLI JSON compatibility (`api/cmd/uzi/settings.go`).
- **CLI check:** new web functionality needs a look at `api/cmd/uzi/`; a per-user harness split in the admin usage API would want the CLI's admin usage output to match.
- A PRD sent to uzi keeps `.github/workflows/**` out of implementation and validation.

## Options considered: keep the current Overview and add this as an alternative

| Option | Size | What you get |
|---|---|---|
| **B. Preview route, then replace** (recommended) | medium, 2 to 4 PRs | The new Overview at a preview route (for example `/overview/next`) sharing the current page's data loading, linked from the current Overview. Time-boxed trial, then promote it to `/dashboard` and delete the old page, or drop it. |
| **C. Permanent per-user layout setting** | largest | B plus a persisted Classic / New preference. Only worth it if both layouts are meant to live on. |
| A. New theme pair | small | Palette only: a theme cannot deliver this structural redesign (PRD #21 Decision 1). Also dropped because the maintainer keeps the current themes and fonts; the exploration mock records the look. |

**Chosen working plan: B** (the child PRDs build to a preview route; #3877 promotes or drops). **Why not C by default:** C doubles the Overview's maintenance: every dashboard change gets built twice or the layouts drift apart, which both reviewers named as the biggest risk. If two layouts ship, share data loading, state interpretation and actions; vary only presentation.

## Decision log

- **D1** Decisions sit above the line; otherwise the hero repeats today's burying of what needs the owner. (review round 1)
- **D2** The line covers issue runs only and is titled "Issue work"; questions and other run kinds sit off the line. (round 1)
- **D3** PR open is not "ready to land": a cached hint, action Review PR, kept out of active totals. (round 1)
- **D4** No merged-PR metric and no per-day series without new aggregation; the chart is tagged Proposed. (round 1)
- **D5** Attention colour only for things that need the owner; a draining worker is not attention. (round 2)
- **D6** Lifecycle copy corrected: holds per claim generation, ephemeral retirement, rolls on genuine idle. (round 2)
- **D7** The mobile drawer is inert when closed and manages focus. (round 2)
- **D8** Layout is not a theme (PRD #21 Decision 1).
- **D9** Out of scope: Board and Run page redesigns (see alloy).
- **D10** Keep the existing themes, typeface and components, and keep the account usage meters in the sidebar (maintainer call). The exploration's palette, Archivo, top bar and rail quota card are dropped; its structure is ported.
- **D11** The decisions card replaces the custody banner only with full state coverage (blocked runs, capacity reached, decisions); the onboarding checklist and the non-admin health platform line stay. Parked splits automatic from owner-action by the run's own evidence (hold reason, recovery fields), not raw status. (ported-mock review)
- **D12** No cost-per-day or cost-per-month chart (maintainer call); this also drops the only usage time-series dependency.
- **D13** The Factory scope shows per-user Runs, PRs, Failed, Fail rate, Last failure (lifetime value) and Cost, in that order, plus a Claude / Codex split (maintainer call; Proposed here, delivered by #3876); provider, not model.
- **D14** The decisions card self-hides when empty (maintainer call).
- **D15** App-wide banners (vault, maintenance, update escalation, health danger and attention) keep their components, order and behaviour, and the sidebar vault chip stays; the Overview never duplicates them.
- **D16** PR open lists only PRs opened by the viewer's own uzi runs, for admins too (the admin list carries no completed runs); human and bot PRs are out of scope.
- **D17** Run scope follows today's Runs page: a normal user sees only their runs; an admin also sees every other user's runs, as a separate "Other users (admin)" group (maintainer call on the name), and the line counts them. Needs your decision stays the viewer's own (maintainer call). A behaviour change from today's Overview, which lists only the viewer's own runs even for admins.
- **D18** On the Overview the banner stack sits under the page header, between the greeting and Needs your decision (maintainer call). Its all-pages variant is superseded by D20.
- **D19** The Search button lives only in the Overview header; the Cmd K shortcut opens the menu on every signed-in page (confirmed by the maintainer 2026-11-10; no sidebar entry).
- **D20** Banners render under the header on the Overview only; other pages keep the shell slot (maintainer call). One shared component, so placement differs but code does not.
- **D21** ci_fix and mr_rework runs sit in Other work: they maintain an existing PR, they are not issue lifecycle stages.
- **D22** Shadow markers: `data-attention` on the decisions card, `data-live` on the issue-work card.
- **D23** The per-user provider split counts runs by harness, the same population as Runs; a cost split would need per-model cost attribution and is out of scope.
- **D24** The new Overview lives at `/overview/next`, linked both ways with the current Overview, until #3877.

## Open decisions

1. After the trial: promote the preview and delete the old Overview (B), or keep both behind a per-user setting (C)? Decided at #3877.
Resolved since the issue was filed: ci_fix and mr_rework placement (D21), Shadow markers (D22), provider split by runs (D23), banners Overview-only (D20), Cmd K on every signed-in page with the button on the Overview only (D19).

## Inspiration (patterns borrowed, not visuals)

- Linear Triage, a decision queue with contextual actions: https://linear.app/docs/triage
- Linear's 2026 UI refresh, a dimmer sidebar so content leads: https://linear.app/changelog/2026-03-12-ui-refresh
- Claude Code agent view, sessions grouped by state with needs-input first: https://dsebastien.net/claude-code-agent-view/
- Trigger.dev live runs, stable rows that update in place: https://trigger.dev/changelog/live-runs-page
- Vercel Observability, a time-scoped summary leading into ranked detail: https://vercel.com/docs/observability
- Railway metrics, telemetry inside the selected service: https://docs.railway.com/observability/metrics
