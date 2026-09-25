# PRD #1650: Retire the Notifications inbox tab

**Issue**: [#1650](https://github.com/vtmocanu/uzi/issues/1650) | **Priority**: Medium
**Status**: Complete (2026-09-25), implemented on branch agent/issue-1650; direction approved by the maintainer on 2026-09-25 after a two-agent brainstorm (Claude + Codex)
**Evidence baseline**: `e0d64558` (2026-09-25); refresh file anchors on the implementation base

## Problem and outcome

The web **Notifications** tab (bell icon, `/notifications`, `web/src/pages/Notifications.tsx`, PRD #46) is unused by the maintainer and reads as spam:

1. **It duplicates signals that already have a home.** Findings has its page and `findingsOpen` badge; judge reviews have Judge and `judgeTodo`; schedule errors show as "parked" on Schedules; health and custody episodes have the Admin Health tab, the danger banner and the Dashboard alert; a locked vault has the app-wide `VaultLockedBanner`; run failures show on Runs and the run page.
2. **Its read state is disconnected from the real state.** A notification is marked read only by clicking it in the inbox (`MarkNotificationRead`). Triaging the finding, answering the judge, or fixing the schedule never clears it, so the unread badge only grows and means nothing.
3. **Several kinds are pure status**: `ci_autofix_started`, `ci_autofix_landed`, `selfimprove_started`, and an inbox copy of `run_failed` that Slack already covers.

**Outcome.** The Notifications tab, the bell, its unread badge and the inbox read API are gone. Every signal a user must act on still reaches them, through a Slack DM (when linked) and the page where the underlying thing lives. Pure status events are no longer produced. The `notifications` table stays as an internal event log and the incidental-finding Slack de-dup latch; nothing in the product reads it back.

## Related work

| Item | Relationship |
|---|---|
| PRD #46 (`prds/done/`, judge + inbox) | Created the inbox. This PRD retires its UI and read API; the judge is unchanged. |
| PRD #333 D6 | Incidental-finding coalescing (one Slack DM per run) keys on a `notifications` row. Kept working here (D4). |
| PRD #1645 (in flight) | Reworks `web/src/pages/Schedules.tsx`. This PRD must not touch that file (D6). |
| PRD #1648 (in flight) | Reworks the Admin Health tab and the nav count pill anatomy in `AppShell.tsx`. This PRD's `AppShell.tsx` edits (remove the bell, add one badge) are small; resolve any conflict at landing. |
| PRD #70 | Favicon ladder; its `unread` input is removed (D5). |

## Scope

### In

- Remove the Notifications page, route, bell nav item, unread polling and the `uzi:notifications-changed` event plumbing in the web app.
- Remove the inbox HTTP routes (`GET /api/notifications`, `GET /api/notifications/unread_count`, `POST /api/notifications/{id}/read`) and their handlers and now-unused queries.
- Stop producing `run_failed`, `ci_autofix_started`, `ci_autofix_landed` and `selfimprove_started` notifications.
- Give the three actionable inbox-only kinds a Slack DM (`ci_autofix_halted`, `mr_rework_halted`, `guardrail_override_decided`) and give `ci_autofix_halted` a web surface.
- A "Failed" filter on Runs → Past runs, and a parked-schedules count on the Schedules nav item.
- Mocks, tests, e2e phase 35, docs, `specs/human.md`, `ARCHITECTURE.md`.

### Out

- Dropping the `notifications` table or adding a migration (D4).
- Any change to the Slack linking section `web/src/components/SlackNotifications.tsx` ("Settings → Notifications"). That name refers to Slack linking, not the inbox, and stays.
- Any change to `web/src/pages/Schedules.tsx` (owned by PRD #1645).
- Any change to judge, findings, health, custody or vault behaviour beyond removing their inbox copy from the UI.
- Any creation, modification or validation write under `.github/workflows/**`. Any edit to frozen `specs/ai.md`.

## Verified current behavior

Resolved local-code facts at the evidence baseline; the offline worker need not look anything up online.

- **Write seam.** `notifysvc.Service.Notify` (`api/internal/notifysvc/service.go:138-176`) inserts a row, prunes the user's rows to 200 (`:162`), then enqueues a Slack DM only when `n.Slack != nil` (`:171`). The DM path (`api/internal/slacksvc/notifier_notify.go:22-45`) is gated by `GetSlackDeliveryForUser` (`slack_notify = true`, link confirmed, resolved Slack id). Nothing reads the table back for Slack; the DM queue is in memory with no retry.
- **The 16 kinds, their producers, Slack, and existing homes:**

| kind | producer | Slack today | other surface today | this PRD |
|---|---|---|---|---|
| `run_failed` | `notifysvc/run_failure_notifier.go:147` | nil (a separate failed-run DM comes from `slacksvc` `notifier_state.go`) | Runs pill, RunView failure block, red favicon | **stop producing** (D2); Runs "Failed" filter (D7) |
| `ci_autofix_started` | `poller/ci_autofix.go:330` | nil (the ci_fix run's own state DMs cover it) | the ci_fix run in Runs/RunView; forge comment | **stop producing** |
| `ci_autofix_landed` | `forgesvc/pipeline_sync.go:310` | nil | fix-verdict chip `components/CIFixRunHeader.tsx` | **stop producing** |
| `ci_autofix_halted` | `poller/ci_autofix.go:348` (latched by `ci_autofix_attempts.halt_notified`) | nil | **none** (forge comment only when an issue backs the branch) | **add Slack DM + web surface** (D3) |
| `mr_rework_halted` | `poller/mr_review_watch.go:316` | nil | RunView "Automatic rework: N of M cycles used · stopped" | **add Slack DM** |
| `guardrail_override_decided` | `handler/guardrail_override_request.go:551` | nil | Repos row pending/approved/rejected (`pages/Repos.tsx`) | **add Slack DM** |
| `schedule_error` | `schedsvc/scheduler.go:918` | set | Schedules "parked" badge | keep; nav count (D6) |
| `selfimprove_started` | `schedsvc/self_improve.go:215` | set | Runs list, Schedules last fire | **stop producing** |
| `selfimprove_skipped` | `schedsvc/self_improve.go:110,154` | set | Schedules last-fire skip badge | keep (vault lock and open-MR cap are actionable) |
| `vault_locked` | `schedsvc/vault_lock_notice.go:112` | set (only Slack-linked users are eligible) | `VaultLockedBanner` | keep |
| `guard_role_excluded` | `handler/runs_lifecycle.go:980` | set | RunView agent card | keep |
| `health_episode` | `healthsvc/episode.go:196` | set | Admin Health, danger banner, pip | keep |
| `custody_episode` | `slacksvc/custody_episode.go:162` | set | Dashboard `CustodyBoardAlert`, Workers recovery holds | keep |
| `early_limit_reset` | `notifysvc/service.go:311` via `usagepoller/engine.go:399` | set, gated by `users.notify_early_limit_reset` | none (event only) | keep, Slack-only (D8) |
| `judge_review` | `handler/judge_worker.go:225` | set | Judge page + `judgeTodo` | keep |
| `incidental_finding` | `notifysvc/service.go:229` (`NotifyIncidentalFinding`) | set, first finding per run only | Findings page + `findingsOpen` | keep (D4) |

- **Coalescing.** `FindUnreadNotificationForRunKind` (`api/internal/store/queries/notifications.sql:88-101`, `read_at IS NULL`, same user/run/kind) is called only from `service.go:230`. A miss inserts a row and sends one DM; a hit bumps `payload.count` and `finding_ids` (capped at 50) with no DM.
- **Queries** in `notifications.sql` (10): `InsertNotification`, `ListNotificationsForUser`, `CountNotificationsForUser`, `CountUnreadNotificationsForUser`, `ListAllNotifications`, `CountAllNotifications`, `MarkNotificationRead`, `PruneNotificationsForUser`, `FindUnreadNotificationForRunKind`, `UpdateNotificationPayload`. `api/internal/store/query_inventory_test.go` (~203-254) pins the inventory.
- **Routes.** `api/internal/handler/routes_notifications.go` mounts list, `unread_count` and `{id}/read`; handlers in `api/internal/handler/notifications.go` (admin `?all=1` gate at ~127). `route_limiter_mounts_test.go` (~386, ~577) lists them. The notifier is wired at `api/cmd/server/main.go:~607` and `h.SetNotifier` at `~1263`; the run-failure notifier at `~624`.
- **CLI/TUI.** No reference to notifications anywhere in `api/cmd/uzi` or `api/internal/uzicli`.
- **Web consumers.** Route `web/src/App.tsx:35,116`; bell `web/src/components/AppShell.tsx:~778` plus unread state and polling (`~638`, `~1089-1157`); `web/src/lib/notifications.ts`; types `web/src/lib/apiTypes.ts:~3107-3153`; API `web/src/lib/api.ts:~1692-1722`; `BellIcon` in `web/src/components/icons.tsx:~316`; the Judge page reads `JudgeTodoContext` which the inbox also read (`Notifications.tsx:195`). Mocks: `web/src/mocks/data/notifications.ts` (also invents `mr_rework_capped` and `mr_merged`, which no Go code produces), `web/src/mocks/mockApi/notifications.ts`, `web/src/mocks/mockApi/index.ts:13,43`, `web/src/mocks/mockApi.mrRework.test.ts:75-78`.
- **Favicon.** `useFavicon({unread})` (`AppShell.tsx:~1285`); `deriveFaviconState(runs, unread, baseline)` (`web/src/lib/favicon.ts:~58-71`) returns amber when `unread > 0` or a run needs human attention; `web/src/lib/useFavicon.ts:~73-80` forces amber on `unread > 0` before the first runs poll and re-polls on the notifications-changed event (`~131`).
- **Runs page.** `web/src/pages/RunsList.tsx` has only "Active" and "Past runs · N" tabs (`~184-186`); no failed filter. `isStoppedRun(status, stop_kind)` already separates a genuine failure from a deliberate stop (used by `favicon.ts`).
- **Schedules nav badge.** `schedulesEnabled` counts `enabled` schedules from `GET /me/schedules` client-side (`AppShell.tsx:~1202-1217`); a parked schedule has `status = 'error'`.
- **ci_autofix state.** `ci_autofix_attempts (repo_id, ref, attempt_count, last_signature, last_pipeline_id, halt_notified, updated_at)` keyed by `(repo_id, ref)` (`api/internal/store/migrations/00117_ci_autofix_attempts.sql`). No handler exposes it. The board card shows a pipeline badge and Fix CI (`web/src/pages/board/IssueCard.tsx:~441`). The start DM is nil because the ci_fix run's own state DMs would double it (`ci_autofix.go:~320-325`); a halt starts no run, so a halt DM does not double anything.
- **Tests touching the inbox** (update or delete): web `pages/Notifications.test.tsx`, `pages/Notifications.findings.test.tsx`, `lib/notifications.test.ts`, `lib/useFavicon.test.tsx`, `mocks/mockApi.notifications.test.ts`, `components/AppShell*.test.tsx`, `pages/JudgeNavBadge.test.tsx`, `pages/Findings.test.tsx:~39`; Go `notifysvc/{service,findings_notify,run_failure_notifier}_test.go`, `handler/{notifications,judge_notify,guard_role_notify,worker_findings,notifyearlyreset}_test.go`, `handler/findings_e2e_livedb_test.go`, `handler/guardrail_override_request_livedb_test.go`, `handler/route_limiter_mounts_test.go`, `store/notifications_integration_test.go`, `store/query_inventory_test.go`, `forgesvc/pipeline_sync_test.go`, `poller/{ci_autofix,mr_review_watch}_test.go`, `schedsvc/scheduler_test.go`, `usagepoller/engine_test.go`. e2e `e2e/phases/35-judge-funnel.sh:101-105` asserts a `judge_review` row via `GET /api/notifications`.
- **Docs promising the inbox:** `docs/judge.md` (~122, ~360, "The inbox" section ~365-380 incl. the favicon note), `docs/judge-menu.md` (~68, ~84), `docs/findings.md` (~24-26, ~97), `docs/admin-health.md:~40`, `docs/run-limit-wait.md:~101`, `docs/ci-autofix.md` (~84-88, ~109), `docs/scheduling.md` (~439, ~502-507), `docs/slack.md:~235`, `ARCHITECTURE.md:~965`. `specs/human.md:330-332` ("Recommendations land in an inbox/notifications surface…") and `:446` ("Slack DM + inbox").

## Binding design decisions

**D1. Remove the inbox read path end to end.** Delete the page, its route, the bell `NavItem`, `BellIcon` if unused, the unread state and polling in `AppShell`, `web/src/lib/notifications.ts` (keep any helper still used elsewhere by moving it next to its caller), the inbox types and API calls, the mock data and mock handlers. Delete `routes_notifications.go`, `handler/notifications.go`, their mount, and the queries `ListNotificationsForUser`, `CountNotificationsForUser`, `CountUnreadNotificationsForUser`, `ListAllNotifications`, `CountAllNotifications`, `MarkNotificationRead`; regenerate sqlc; update the query inventory test. Visiting `/notifications` falls through to the app's existing not-found handling (no redirect page needed). `knip` and `deadcode` must stay green.

**D2. Stop producing status-only kinds.** Remove the `run_failed` notifier (`RunFailureNotifier` and its wiring; Slack's own failed-run DM in `slacksvc` is untouched), the `ci_autofix_started` and `ci_autofix_landed` calls, and the `selfimprove_started` call. Keep the forge comments those paths already post.

**D3. Actionable inbox-only kinds get a Slack DM.** Set a `SlackRender` on `ci_autofix_halted`, `mr_rework_halted` and `guardrail_override_decided`, following the existing renders' emoji/title/body convention and the notifier's escaping rules (untrusted values such as refs, reasons and repo paths go through the notifier's escape + scrub, closed enums do not). Each render links to the page where the user acts: the run (`/runs/<id>`) for `mr_rework_halted`, the Repos page for `guardrail_override_decided`, the pipeline URL (already in `CIAutofixPayload.PipelineWebURL`) for `ci_autofix_halted`. The existing latches (`halt_notified`, one decision per request) keep each to one DM.

**D3a. `ci_autofix_halted` gets a web surface.** Expose the halt on the board card: in the board's batch read (`api/internal/handler/board.go`), join `ci_autofix_attempts` on `(repo_id, latest_run.branch)` (the card's `latest_run`, `board.go:~87-97`), and carry the result on the card (for example `ci_autofix_halted: boolean` and `ci_autofix_attempts: number` on the card or its `latest_run`). Do **not** join through `card.pipeline.ref`: that badge exists only when a pipeline is cached, so it would hide a halt. The card shows a small "Autofix stopped" marker beside the pipeline badge (or where it would be) with the attempt count in its title, so the user knows the Fix CI button is now theirs to press. Every response that returns a card (the board list and any single-card refresh) must carry the same fields. The marker is also hidden once the card's latest run has a closed or merged MR (it covers the gap until the next reconcile evicts the ledger row; found in M7's visual check). No new endpoint. A ref with no backing issue card (a prompt-schedule MR) relies on the Slack DM alone; say so in `docs/ci-autofix.md`.

**D4. Keep the `notifications` table as an internal event log and de-dup latch.** No migration. `Notify` still inserts and prunes; `InsertNotification`, `PruneNotificationsForUser`, `FindUnreadNotificationForRunKind` and `UpdateNotificationPayload` stay. Since nothing marks a row read any more, rename `FindUnreadNotificationForRunKind` to `FindNotificationForRunKind` and drop its `read_at IS NULL` predicate, so coalescing no longer depends on historical read state. The guarantee stays **best-effort**, exactly as today: the 200-row per-user prune can evict a run's latch row during a long run, and two concurrent first findings can both miss before inserting; either can produce a second DM. Do not claim exactly-once. Leave the `read_at` column in place (a later cleanup PRD may drop it). Update `ARCHITECTURE.md` to describe the table as a pruned, write-only event log (not a durable audit log).

**D5. Favicon.** Remove the `unread` input from `deriveFaviconState` and `useFavicon`; amber means only "a run needs you" (awaiting approval or an answer), red and running are unchanged. Remove the notifications-changed re-poll.

**D6. Parked schedules on the Schedules nav item.** In `AppShell` only (not `Schedules.tsx`): when any of the caller's schedules has `status = 'error'`, the Schedules `NavItem` shows that parked count with `badgeTone="alert"` and an accessible noun such as "parked schedules"; otherwise it keeps today's enabled count. Derived from the same `GET /me/schedules` response already fetched, so it clears on the next nav poll after the schedule is fixed. PRD #1648 adds a `warn` tone to `NavItem.badgeTone` in `AppShell.tsx`; if it has landed, rebase first. Set the props on every sidebar instance (expanded, collapsed rail, mobile).

**D7. Runs → Past runs "Failed" filter.** Add a filter control (chip or segmented option) on the Past runs tab that shows only genuine failures (`status = 'failed'` and not `isStoppedRun`), with its count in the label. No nav badge: a failed run has no resolved state, and a count that never clears is the defect this PRD removes. Client-side over the list the tab already loads. Apply the filter before archive paging and grouping, so the count and pages reflect failures only, and keep the existing history search query when the filter is toggled.

**D8. `early_limit_reset` becomes Slack-only.** No web fallback. Update the toggle copy in `web/src/pages/RunDefaults.tsx` (~552) and `docs/run-limit-wait.md` so they say Slack DM, and note that users without Slack linked see the reset only on the rate-limit meters.

**D9. Specs.** Amend `specs/human.md:330-332` and `:446` to drop the inbox, tagged `(AI-synced 2026-09-25)` with a pointer to #1650. Per the root `CLAUDE.md`, AI has full authority over `human.md`.

## Milestones

Each milestone ends with its component gates green, run once to a log per the root `CLAUDE.md` *Run economy*: `task gate:api` for Go changes, `task gate:web` for web changes, and `task gate:repo` at M6. Every behaviour change carries a test that fails before the change and passes after (watch both directions).

- [x] **M1. Producers.** D2 and D3: remove the four status-only producers and `RunFailureNotifier`; add the three Slack renders. Tests: each removed kind no longer inserts a row (update the existing producer tests to assert absence); each of the three kinds now publishes a Slack render with its link and escaped untrusted fields; `ci_autofix_halted` still fires once per halt.
  - Landed as planned, plus `notifysvc.SafeLinkURL` (the forge pipeline URL is linked only when it is a plain http(s) URL with no userinfo, `<>|`, whitespace, control or format characters) and an optional `SlackRender.LinkLabel`.
- [x] **M2. API removal, store and coalescing.** One milestone, in this order so `gate:api` stays green: first D1's server half (delete the routes, handlers and their tests; update `route_limiter_mounts_test.go`; keep `main.go` wiring of `Notify` for the remaining producers), then D4 (rename and widen the finding lookup, drop the six read-path queries, `sqlc generate`, update `query_inventory_test.go` and `notifications_integration_test.go`). Tests: the three routes return 404 through the real router; a second finding on the same run after the first row has a non-null `read_at` still coalesces (no second DM); a finding on a different run still inserts and DMs.
  - The e2e phase 35 rewrite landed here rather than in M6, so no commit left it calling a removed route. The full e2e harness cannot build images in the uzi worker; phase 35 was checked with `bash -n`, shellcheck and `check:e2e-registry-doc`, not run.
- [x] **M3. Board autofix-halt field.** D3a server half: the board batch join and the card fields, on every card-returning response. Tests: a card whose latest run's branch has `halt_notified = true` carries the halt and the attempt count; a halted ref with no cached pipeline still carries it; a non-halted ref does not.
- [x] **M4. Web removal and favicon.** D1 web half and D5: page, route, bell, polling, event, lib, types, API calls, mocks (including the invented `mr_rework_capped`/`mr_merged` rows and `mockApi.mrRework.test.ts`), tests. Tests: AppShell renders no Notifications nav item; favicon amber only from runs; knip green.
  - `mockApi.mrRework.test.ts` was kept and rewritten against a run-backed fixture (moved to `web/src/mocks/data/mrRework.ts`) instead of deleted. `useJudgeTodo` and its value context were removed with their only reader. `NavItem` now requires `badgeLabel` whenever a badge is set; the Judge badge reads "N to triage".
- [x] **M5. New homes in the web.** D3a web half, D6, D7: the board card "Autofix stopped" marker, the Schedules nav parked count, the Runs Failed filter. Tests: marker shown only when halted, with attempt count; nav shows parked count in alert tone and falls back to the enabled count when none is parked; Failed filter excludes cancelled and plan-rejected runs and shows the right count. Add mock-mode fixtures so each state is visible without a live stack.
  - The marker copy is "Autofix stopped after N attempts: fixing this failure is up to you." (a no-progress halt below the cap still auto-fixes a different failure, so it never promises a Fix CI button or no retry).
- [x] **M6. Docs, specs, e2e, copy sweep.** Update every doc listed in *Verified current behavior* (drop inbox promises; `docs/judge.md` loses "The inbox" section and its favicon note; `docs/ci-autofix.md`, `docs/scheduling.md`, `docs/findings.md`, `docs/run-limit-wait.md`, `docs/admin-health.md`, `docs/judge-menu.md`, `docs/slack.md` say Slack DM and/or the owning page instead), then `task docs:sync` and commit the mirror; `task check-docs:web`. `ARCHITECTURE.md` per D4. `specs/human.md` per D9. Rewrite `e2e/phases/35-judge-funnel.sh:101-105` to assert durable judge-review persistence through the judge API instead of `/api/notifications`, and update the phase title, header comment and `e2e/README.md` row so they no longer claim a persist-first *notification*; `Notify`'s persist-then-Slack ordering stays covered by its focused `notifysvc` service test. Also fix the Judge copy that promises results "in your inbox": `web/src/pages/RunDefaults.tsx:~571` and `web/src/pages/adminSettings/JudgeSettingsCard.tsx:~75` (and their tests). Sweep retired strings ("Notifications" as a nav label, "Mark read", "inbox") across `web/src` tests and repoint any unpaired negative assertion (`.claude/rules/web.md`, *Copy changes disarm negative assertions*); do not touch the Slack-linking "Settings → Notifications" strings. Confirm `api/cmd/uzi` needs no change and say so in the PR.
- [x] **M7. Visual check in mock mode.** `cd web && VITE_UZI_MOCK=1 npm run dev`: the sidebar without the bell (expanded and collapsed rail), the Schedules nav parked count, the Runs Failed filter, the board card marker, on Dawn and Ember at 1440px and the sidebar at 390px. Commit the screenshots under prds/mockups/1650/ and reference them in the PR body; if the run cannot produce them, say so in the PR and the landing session attaches them.
  - 13 screenshots in `prds/mockups/1650/` (sidebar expanded, rail and 390px drawer; Schedules parked badge; Past runs Failed on/off; board marker), Dawn and Ember at 1440px; the 390px drawer on Ember. The check found the marker showing on a closed-MR card; fixed by hiding it once the MR is recorded closed or merged.

## Success criteria

- No Notifications nav item, `/notifications` page, or `/api/notifications*` route exists; `git grep -n 'listNotifications\|unread_count' -- web/src api/internal` returns nothing.
  - As built, the only remaining matches are the 404 regression test `api/internal/handler/notifications_removed_test.go` and a comment in `api/internal/handler/route_limiter_mounts_test.go`, both of which must name the retired routes.
- A halted CI autofix, a halted MR rework and a decided guardrail override each produce exactly one Slack DM for a Slack-linked owner; the halted autofix is also visible on its board card.
- A run with several incidental findings still sends one Slack DM (best-effort, as today; D4).
- The four status-only kinds are no longer inserted.
- All gates green; no migration, no workflow file, no `specs/ai.md` change.

## Risks

| Risk | Mitigation |
|---|---|
| A user without Slack loses `early_limit_reset`, `ci_autofix_halted` on prompt MRs, `mr_rework_halted`, `guardrail_override_decided` push signals | Accepted by the maintainer; each still has a page surface except `early_limit_reset` (meters) and prompt-MR autofix halts (documented). |
| Removing the read path silently breaks finding coalescing and floods Slack | D4 keeps the table and widens the lookup; M2 pins it with a read-row regression test. |
| Merge conflict with PRD #1648 in `AppShell.tsx` and with #1645 on Schedules | This PRD stays out of `Schedules.tsx`; `AppShell` edits are local; resolve at landing. |
| Negative assertions rot when "Notifications" copy disappears | M6 sweep. |

## Decision Log

| Date | Decision | Rationale |
|---|---|---|
| 2026-09-25 | Retire the tab rather than rebuild it as an "Attention" feed | Once each kind is mapped to its existing home, an Attention feed would hold only failed/halted runs, schedule errors and guardrail decisions; too few to justify a destination. A second inbox with its own read state is the anti-pattern being removed. |
| 2026-09-25 | Keep the `notifications` table, no migration | It is the incidental-finding DM latch and a cheap audit log; dropping it needs a replacement latch and a migration for no user-visible gain. |
| 2026-09-25 | Add Slack DMs to three inbox-only actionable kinds; stop producing four status-only kinds | Codex review found these six had no Slack DM, so removing the inbox alone would have silenced the actionable three. |
| 2026-09-25 | Failed runs get a filter, not a nav badge | A failed run has no resolved state; a badge that never clears is the original defect. |
| 2026-09-25 | Codex PRD review: join the halt on `latest_run.branch`, not the optional pipeline badge; merge API removal and sqlc changes into one milestone; state coalescing as best-effort; e2e phase 35 asserts judge-review persistence, not a notification | A pipeline-badge join hides halts; deleting queries before handlers reddens the gate; the prune and a race already allow a second DM; a judge read cannot prove notification ordering. |
| 2026-09-25 | vault_locked needs no new home | The app-wide `VaultLockedBanner` already covers it (maintainer). |
