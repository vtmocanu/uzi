# PRD #3878: Overview preview, decisions first

**Status**: Draft, parked (`Later`). Child of epic #3873. Blocked by: none.

**Design brief**: `prds/3873-overview-redesign.md` (model-fidelity rules, data readiness, reuse map, constraints, mock contract). This PRD wins where it is more specific.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html`. Open it in a browser; append `#factory` for the Factory usage scope. The mock contract in the brief applies: implement the mock for this PRD's surfaces (page header, banner placement, Needs your decision, Runs), small consistency tweaks are fine, any larger deviation goes in the PR body under `Mock deviations` with its reason, and the PR carries side-by-side screenshots.

## Problem

The Overview buries what needs the owner. "2 need you" is a hint under the Active runs tile; the named runs appear only in Recent runs at the bottom, below inventory tiles, worker load and usage. An admin sees only their own runs there, although the Runs page already shows other users' runs to admins.

## Outcome

A new Overview at `/overview/next` (protected route), linked from the current Overview ("Try the new Overview") and back ("Back to the current Overview"). The current Overview at `/dashboard` is unchanged apart from that link. The new page shows, top to bottom:

1. **Page header**: "Welcome, <name>" and one status sentence: "N decisions and M pull requests are waiting on you." or "Nothing needs you right now.", then "X runs are executing, Y queued and Z parked." The counts follow the viewer's scope: own runs for a user; every user's runs for an admin ("across the factory"). An admin also gets a compact health chip mirroring the instance health severity.
2. **The app-wide banner stack, under the header** (Overview only): vault required and vault locked, maintenance, update escalation, health danger, health attention, in today's order and with today's gating.
3. **Onboarding checklist**, unchanged, while setup is incomplete.
4. **Needs your decision** card, self-hiding when empty: the viewer's own plan reviews, questions and follow-ups, owner-action parks, and PRs open for review, each with one verb (Review plan, Answer, Resume, Review PR) and its age. Never another user's items, for admins too.
5. **Runs** widget: "Yours", then for admins only "Other users (admin)" with other users' active runs and the owner's email, as the Runs page does today. Today's row anatomy.
6. **Carried over unchanged** until their PRDs replace them on this page: the custody alert (`CustodyBoardAlert`, replaced by #3880), the non-admin health platform line (`HealthPlatformLine`, stays), the worker-load card (replaced by #3880), the usage card (replaced by #3881).

Acceptance examples:

- A user with one plan awaiting approval opens `/overview/next`: Needs your decision lists it with Review plan, and the sentence starts "1 decision is waiting on you." After approving, the card disappears and the sentence reads "Nothing needs you right now."
- An admin sees Yours and Other users (admin), and another user's run awaiting approval appears only in Other users, with no button. A non-admin sees only Yours, and the page sends no `/api/admin/*` request.
- With the vault locked, the vault banner renders under the greeting on `/overview/next` and above the content on `/runs`, once per page.

## Out of scope

The issue-work line (#3879), the capacity rail and Held work (#3880), the usage recomposition (#3881), the command menu and the header Search button (#3875), promotion to `/dashboard` (#3877), the provider split (#3876). Banners stay in the shell slot on every page except this one.

## Modules and seams

- **`useOverviewData`** (new hook, the prefactor): today's `Dashboard` data loading (`web/src/pages/Dashboard.tsx:148`: runs summary, workers, repos, templates, secrets, connections, usage, plus the 10 s and 30 s visible polls) moved into one hook that both pages use. Admin runs are **opt-in** (`useOverviewData({ includeAdminRuns: true })`, used only by the preview): for an admin the preview also loads `api.adminListRuns({ view: "summary" })` (`web/src/lib/api.ts:486`), while `Dashboard` calls the hook without it and sends exactly today's requests. Personal and admin data load and fail **independently**: an admin-fetch failure keeps every personal section and shows "Other users' runs are unavailable" with the time of the last good read, never an empty group, and the status sentence then counts only the viewer's runs and says so. Invariants: a non-admin never calls an admin endpoint; a poll failure keeps the last good data; skeletons only before the first load. Population limits are in the brief's Run populations section.
- **`parkDisposition(run)`** (pure, `web/src/lib`, shared with #3879): `automatic` or `owner` for every parked run, keyed on `effectiveRunStatus` (`web/src/lib/runBadge.ts:202`). That function returns a `string`, not a union, so write an explicit mapping table over the values it can return today, test it value by value, and map anything unlisted or unknown to `owner`. Automatic: `limit_wait`, `pool_wait`, an account cooldown (`isAccountCooldownWaiting`, `runBadge.ts:533`), a `paused` run held by the weekly reserve (`isReserveHeldPause`, `runBadge.ts:605`; `hold_reason` `weekly_reserve`, the one pause that releases itself), and a `recovery_wait` with a scheduled retry. Owner: every other `paused` hold (`budget_exhausted` included) and a `recovery_wait` whose cause is `worker_requeue_exhausted`, `retained_recovery_adoption_failed`, `worker_memory_pressure`, or `retained_recovery_exhausted` with a null retry stamp. Pin it with a test against the server's `needs_owner` / `progressing` classification in `ListMaintenanceHeldRuns` (`api/internal/store/queries/intake_pause.sql:72`); the server tags every `paused` run `needs_owner`, and the weekly-reserve pause is the deliberate exception.
- **`openPrs(runs)`** (pure, shared with #3879): the viewer's completed issue runs with `mr_iid != null` and `mr_state === "opened"`, deduplicated by `(repo_id, mr_iid)`.
- **`overviewDecisions(ownRuns)`** (pure, `web/src/lib`): ordered decision items from the viewer's own runs only (the `listRuns` rows, never the admin rows): plan reviews (`awaiting_approval` without `is_revising`), questions and follow-ups, parks whose `parkDisposition` is `owner`, and `openPrs`. #3880 adds a second input, the holds aggregate.
- **`overviewCounts(runs)`** (pure): executing, queued and parked counts that bucket on the **raw** status, as today's `activeRunBuckets` does (`Dashboard.tsx:42`), with an account cooldown counted as waiting; feeding it effective statuses would drop runs. For an admin the row set is the viewer's own runs plus the admin rows whose `owner_email` differs from the viewer's, merged by run id.
- **`AppBanners`**: the stack now inline in `AppShell` (`web/src/components/AppShell.tsx:1403` to `:1418`) extracted unchanged into one component. The shell renders it on every route except `/overview/next`; the new page renders it under its header. Invariant: exactly one stack per page, same order, same self-gating.
- **Runs widget**: extract today's Overview Recent-runs row (`Dashboard.tsx`) into one shared row component with an optional owner field (masked with `maskEmail` in demo mode) and use it on both pages. Not `RunRow` from the Runs page (`web/src/pages/RunsList.tsx:285`): it adds duration, tokens, cost and Expedite, and its "now" line lacks the Overview's held-status check, so a parked run would show a stale, pulsing activity line. The owner split copies `RunsList.tsx:664`. Yours lists the viewer's active runs only (today's Recent runs also shows finished ones); its empty state reads "No active runs. Start one from a board card."
- Untrusted text (issue titles, question text, agent activity) goes through `stripUnsafeChars`; demo mode masks names, emails and repo paths as today.

## Testing decisions

- Unit tests for `overviewDecisions` and `overviewCounts`, one per brief rule (revising, stalled, weekly-reserve pause, owner-action park, PR sourcing, own-only for admins). Follow the style of `web/src/lib/runBadge.test.ts`.
- Page tests (Vitest + Testing Library) in the style of `web/src/pages/Dashboard.test.tsx`: admin vs non-admin (assert `adminListRuns` is never called for a non-admin, and never by `Dashboard`), an admin-fetch failure that keeps the personal sections and shows the unavailable note, the self-hiding card, the calm sentence.
- An `AppShell` test that the banner stack is absent from the shell on `/overview/next` and present on another route, and a page test that it renders under the header.
- A mock-mode scenario seeded like the design mock (decisions, own and other users' runs, banners) so screenshots compare like for like: `web/src/mocks/mockScenario.ts` only reads the `?mock=` name, so the seeding goes in `web/src/mocks/mockApi/` and `web/src/mocks/data/`; add it to the known-scenario list in `docs/dev-conventions.md` and run `task docs:sync`.
- A test that the owner email is masked in demo mode, asserting on the rendered attribute where it reaches `title=` (`.claude/rules/web.md`, untrusted values in attributes).
- A browser test at 400 px width (`*.browser.test.tsx`) for layout and no horizontal scroll.

## Milestones

### M1: the preview page on shared data

**Blocked by**: none.

`useOverviewData` extracted and adopted by `Dashboard` (behaviour unchanged), together with the pieces both pages need from today's page: the worker-load card, the onboarding `Step` list and `readUsageWindow`; the `/overview/next` route with the header (greeting, status sentence, admin health chip), `AppBanners` under the header there and in the shell elsewhere, and the carried-over sections; links between the two pages; the mock-mode scenario.

Acceptance: existing `Dashboard` and `AppShell` tests pass unchanged; banners render once per page in today's order; a non-admin sends no admin request; header and banner placement match the mock (screenshots, desktop and 400 px, Ember and Dawn).

### M2: Needs your decision

**Blocked by**: M1.

`overviewDecisions` plus the card: self-hiding, own items only, verbs, ages, the status sentence's decision clause.

Acceptance: a test per rule; the card hides when empty; mock parity for the card, including the calm state.

### M3: Runs grouped Yours / Other users (admin)

**Blocked by**: M1.

The Runs widget replaces Recent runs on the preview: Yours first, Other users (admin) for admins, today's row anatomy via the row extracted from `Dashboard`, owner email masked in demo mode, an empty group hides.

Acceptance: admin and non-admin tests; mock parity for the widget.

## Decision Log

- **Preview route, not a per-user setting** (brief option B): no migration, nothing persisted, reversible; #3877 promotes or drops it. Rejected: a layout preference now (option C), which doubles maintenance before the trial says it is wanted.
- **Prefactor the data loading first**: two Overviews reading one hook cannot drift in what they fetch or how they poll. Rejected: copying `Dashboard`'s effects into the new page.
- **Banners under the header on the Overview only** (maintainer call, brief D20), through one shared component. Rejected: moving them under every page's header, which touches every page for no requested gain.
- **The held-work decision row waits for #3880**, which retires the custody banner at the same time, so custody coverage never has a gap on the preview. Until then the preview keeps `CustodyBoardAlert`.
- **No Search button yet**: #3875 adds it with the menu it opens, so this PRD ships no dead control.
- **Health**: admins get the compact chip from the shared health state; non-admins keep `HealthPlatformLine`. Rejected: a second health card on the page.
