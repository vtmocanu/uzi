# PRD #411 — Clickable issue links on runs

**Issue**: [#411](https://github.com/vtmocanu/uzi/issues/411)
**Priority**: Medium
**Status**: Draft — ready for implementation

> **Path convention**: unless a path starts with `web/`, `deploy/`, `controller/`, or another top-level dir, every Go path in this PRD is relative to **`api/internal/`** (e.g. `store/queries/runtime.sql` = `api/internal/store/queries/runtime.sql`, `handler/workers.go` = `api/internal/handler/workers.go`). Stated once so an offline worker's literal lookups resolve.

## Problem

A run seeded from a forge issue shows the issue number (e.g. `#189`) everywhere it appears — the runs list, the run detail page, the dashboard, the board, and a schedule's last-fire panel — but the number is **plain text**. There is no way to jump from a run to the issue that seeded it. The originating issue is the one piece of context a reviewer most often wants (to read the full description, the discussion, the labels), and today it takes a manual search on the forge to reach it.

The run detail breadcrumb is a partial exception: its `#<iid>` links to uzi's **in-app** issue view, not the forge. Nowhere links to the actual issue on the forge, the way the **Open pull request** button already links to the MR on the forge.

## Solution

Make `#<iid>` a link to the issue **on the forge** everywhere a run's issue number appears, opening in a new tab, matching how PR/MR links already behave. Runs that have **no** issue (kinds `task`, `chat`, `ci_fix`, `judge`, `prompt`) render a muted **`no issue`** flag instead of a dead link.

The server already knows every issue's web URL (the forge returns it and it is cached in the `issues` table), it is simply not plumbed to these surfaces. This PRD plumbs it and renders the links.

---

## Background — current state (resolved facts)

All facts below were verified against the codebase on 2026-08-20 (and re-verified by two reviewers) and are baked in here so the work needs **no** external lookups (the implementing worker runs offline).

### What the number is

`#<iid>` is always the forge issue's per-project number — `runs.issue_iid` (`store/models.go:308` `Run.IssueIid pgtype.Int8`, JSON `issue_iid`). It is **not** the run's own id (`run.id`, a uuid string used only for `/runs/:id` navigation). Nullable, because issue-less run kinds exist (below).

### The gap: no issue web URL on a run

- `runs` has **no** issue-URL column. The only web-URL column ever added to `runs` is `mr_web_url` (migration `store/migrations/00069_forgejo_and_mr_web_url.sql`).
- `RunDTO` (`apitypes/run.go`) serializes `issue_iid`, `issue_title`, `issue_description`, `forge_type`, `mr_iid`, `mr_web_url`, `pipeline_web_url` — but **no** issue web URL.
- Frontend `Run` / `RunListItem` (`web/src/lib/api.ts:1227`, `:1485`) mirror that — **no** `issue_web_url`.

### But the URL exists server-side, unused by runs

- The forge driver returns it: `forge.Issue.WebURL` (`forge/forge.go:223`), populated per forge — GitLab `i.WebURL` (`forge/gitlab.go:736`), Forgejo `i.HTMLURL` (`forge/forgejo.go:733`), GitHub `i.GetHTMLURL()` (`forge/github.go:805`). uzi does **not** construct issue URLs; the forge supplies them. **A single `isHttpsUrl` guard suffices for all three forges — no per-forge logic.**
- It is persisted in the issues cache: table `issues` (`store/migrations/00002_forge.sql`) — `repo_id uuid` (`:49`), `forge_issue_iid bigint` (`:50`), `web_url text NOT NULL` (`:54`), `UNIQUE (repo_id, forge_issue_iid)` (`:59`).
- There is already a ready-made lookup query: `GetIssueByIID(RepoID, ForgeIssueIid) (store.Issue, error)` (`store/forge.sql.go:259`), whose `store.Issue.WebUrl` is the value we want.
- It is otherwise exposed only on the **issues** DTO (`handler/issues.go` `issueDetailDTO.web_url`), never on a run.

So `runs.repo_id` + `runs.issue_iid` map to exactly one `issues` row (when the issue is still cached), whose `web_url` is the link we want.

### Issue-less run kinds

`runs.kind` domain (migration `store/migrations/00134_run_task_kind.sql`, CHECK `runs_kind_shape`): `issue`, `ci_fix`, `chat`, `judge`, `self_improve`, `prompt`, `task`.

- **Issue-shaped** (`issue_iid` NOT NULL): `issue`, `self_improve`.
- **Issue-less** (`issue_iid` NULL): `ci_fix`, `chat`, `judge`, `prompt`, `task`.

The **`task`** kind (PRD #400: `uzi handoff`) is the one that shows in the runs list with a repo but no issue — `repo_id` set, `issue_iid` NULL, `branch` set. These must be **flagged**, never rendered with a broken/empty link. (`chat` has `repo_id` NULL and does not render a repo/issue ref at all; the run-detail breadcrumb already special-cases `kind === "ci_fix"`.)

### Where a run's issue number renders (verified file:line)

| # | Surface | File | Data source | In scope |
|---|---------|------|-------------|----------|
| 1 | Runs list card meta | `web/src/pages/RunsList.tsx:263` | `ListRunsForUser` → `RunListItem` | ✅ M2 |
| 2 | Run detail breadcrumb | `web/src/pages/RunView.tsx:699` | already **in-app** link — unchanged | ❌ |
| 3 | Run detail header (`RunHeading`) | `web/src/pages/RunView.tsx:1571` | `GetRun` → `Run` | ✅ M2 |
| 4 | Dashboard recent runs | `web/src/pages/Dashboard.tsx:348` | `ListRunsForUser` (`Dashboard.tsx:97,151`) | ✅ M2 |
| 5 | Board needs-attention strip | `web/src/pages/Board.tsx:1220` | `ListRunsForUser` (`Board.tsx:468`) | ✅ M2 |
| 6 | Schedule last-fire rows | `web/src/pages/Schedules.tsx:476` (started), `:538` (skips) | persisted `LastFire` jsonb | ✅ M3 |
| 7 | Chat start-run request card | `web/src/components/RunRequestCard.tsx:52` | `RunStartRequest` (no URL source) | ❌ out of scope |
| 8 | Schedule target rows | `web/src/pages/Schedules.tsx:587` | schedule DTO (no URL source) | ❌ out of scope |

Surfaces 1/3/4/5 are all fed by the run-list/detail queries, so one server field (`RunDTO.issue_web_url`) covers them. Board *cards* (not the strip) use `ListLatestRunsForRepo` → `latestRunDTO` (`handler/board.go:496`) and already link to the forge issue — out of scope. Worker/CLI queries (`ListRunsForWorkerUser`, `GetRunForWorkerUser`) are out of scope.

### The schedule last-fire is a **second, separate** data path

The last-fire panel (screenshot: `3 matched · 2 started · 1 skipped`) is rendered by `LastFireDetail` in `Schedules.tsx`. Its rows come from a **persisted jsonb summary** written by `schedsvc`, not from the runs table:

- **Wire/API types** (`apitypes/schedule.go`): `LastFireStarted { IssueIID; RunID; Title }` (`:118`), `LastFireSkip { IssueIID; Title }` (`:127`). Their json tags must stay in lockstep with the schedsvc internal structs (comment at `schedule.go:116`).
- **schedsvc internal**: `Started` / `Skip` (`schedsvc/outcome.go:8`, `:18`) — carry `IssueIID/RunID/Title`, **no** `WebURL`. Marshalled into the persisted `LastFire` by `marshalLastFire` (`schedsvc/last_fire.go:49`), copying from `FireOutcome.Started[]`/`.Skips[]` at `:58-62` (started) and `:65-69` (skips).
- Built in `schedsvc/scheduler.go`: the `Started` element is constructed at `:434`; `createIssueRun` (`:410`) today receives `issue.Title, issue.Description, issue.Labels` but **not** `issue.WebURL`.

Neither struct carries `web_url`, so this surface **cannot** be done frontend-only. **This work is entirely in `api/internal/schedsvc` — the `poller` package is CI-autofix only and is unrelated.**

**Where the URL is / isn't in hand at fire time** (verified): the forge issue is fetched via `f.GetIssue(...)` only on the **started** path (`scheduler.go:259` issue-target, `:331` sweep) and the **one post-create skip** (`:343`), so `issue.WebURL` is available there. The `already_running` skips (`:256`, `:328`, `:369`, `:380`) and the `fetch_failed` pre-check skip (`:324`) deliberately never fetch the issue (comment at `:322`: "a pre-check skip does not pay for an extra forge call"), so `web_url` is unavailable for those exactly as `Title` is today — they degrade to a plain number.

### Reusable frontend building blocks

- `ExternalLinkIcon` — `web/src/components/icons.tsx:230`.
- `isHttpsUrl(url)` — `web/src/lib/api.ts:2285` (guards every external anchor in the app).
- **The model to copy is the board card's forge link** — `Board.tsx:1862`: `<a href target="_blank" rel="noreferrer">` + `ExternalLinkIcon`, `isHttpsUrl`-guarded. (Note: there is **no** reusable `.link` utility class; `index.css:234` is `.docs-prose a`, scoped to rendered docs. Copy the board-card anchor, do not reference a `.link` class.)
- `web/src/lib/forgeUrls.ts` is **GitLab-only** and builds **MR** URLs from an existing issue URL; it does **not** build issue URLs. Do not extend it — the URL comes from the server, not string surgery.

---

## Design decisions

1. **Link target = the forge issue (external, new tab).** Chosen over the in-app issue view, per the request ("link to the initial issue"). Matches how **Open pull request** opens the MR on the forge. The run-detail breadcrumb keeps its existing **in-app** link (unchanged) — the two coexist.

2. **Runs: LEFT JOIN on the two list queries, best-effort lookup on the detail path.** The `issues` cache is `UNIQUE (repo_id, forge_issue_iid)`, so a join is exact (1:1, no fan-out), needs **no migration/backfill**, and covers historical runs.
   - **List queries** — `ListRunsForUser` (`store/queries/runtime.sql:392`) and `ListActiveRunsAll` (`:459`, admin) — already return **Row structs** (they use `sqlc.embed`), so adding `LEFT JOIN issues i ON i.repo_id = runs.repo_id AND i.forge_issue_iid = runs.issue_iid` and selecting `i.web_url` is clean. sqlc generates a nullable `pgtype.Text` for `i.web_url` (same as the existing `LEFT JOIN workers` → `WorkerName pgtype.Text` at `runtime.sql.go:2911`); the handler maps it via the existing `textPtrValue` helper.
   - **Detail queries** — `GetRunByID` (`:376`) and `GetRunByIDForUser` (`:353`) — are bare `SELECT * FROM runs` returning a `store.Run`. **Do NOT add the join here.** Doing so flips their return type to a Row struct and ripples through `runToDTO(store.Run)` (`handler/workers.go:348`), the `workersvc`/`notifysvc` service interfaces, and ~15 direct callers — the repo explicitly rejected exactly this for the sibling `forge_type` field (see the comment at `runtime.sql:384`). Instead, the run-detail handler (`GetRun`, `handler/workers.go:1049`) resolves `issue_web_url` **best-effort** with a separate `GetIssueByIID(repo_id, issue_iid)` call, right beside the existing `GetForgeTypeForRepo` call at `workers.go:1076`, and stamps `dto.IssueWebURL`.

3. **Schedule last-fire: snapshot into the persisted summary.** The fire summary is a persisted jsonb event, so a read-time join is impossible — snapshot at fire time. Add `WebURL` to `schedsvc.Started`/`Skip` (`outcome.go`), thread `issue.WebURL` through `createIssueRun` (or pass the whole `forge.Issue`) at the started + fetched-skip sites, populate in `marshalLastFire` (`last_fire.go:58-69`), and add `web_url` (json `web_url`) to `apitypes.LastFireStarted`/`LastFireSkip`. Skips that never fetch the issue leave it empty → plain number. Old persisted fires have no `web_url` → no link (graceful).

4. **Issue-less flag wording = `no issue`.** A muted chip (uses `--warn` amber, distinct from the orange issue link) shown wherever a run's issue ref would render but `issue_iid` is NULL. The existing kind pill (e.g. `task`) stays. Per-surface rule: `issue_iid == null` → flag; else if `issue_web_url` is a valid https URL → forge link; else → plain number (rare edge: issue cached-out). (Schedule surfaces already render `"prompt"` for null `issue_iid`, so the flag is a runs-surface concern only.)

5. **Render guard is uniform.** Every new anchor is guarded by `isHttpsUrl(...)`, uses `target="_blank" rel="noreferrer"`, and copies the board-card anchor + `ExternalLinkIcon`. On the runs list (`RunsList.tsx`) and board strip (`Board.tsx:1220`) the card is already a router `<Link>`, so the inner issue anchor calls `stopPropagation()` to stop double-navigation. Note this nests an `<a>` inside a `<Link>` (also an `<a>`), which is invalid HTML; prefer rendering the issue ref as a sibling of the card `<Link>` (outside its anchor) rather than a descendant where the layout allows, to keep the markup valid as well as click-correct.

## Scope

**In scope**: surfaces 1/3/4/5 (runs) and 6 (schedule last-fire); the two server data paths; the issue-less `no issue` flag; mock fixtures for both surfaces; tests; a CHANGELOG entry.

**Out of scope**:
- The run-detail breadcrumb's in-app issue link (kept as-is, surface 2).
- **Secondary surfaces 7 (chat start-run request card) and 8 (schedule target rows).** Neither's DTO (`RunStartRequest` at `api.ts:2234`; the schedule DTO) carries an issue `web_url`, so linking them means a *third and fourth* server data-path for a pre-run request card and a config row — low value relative to cost. They keep their plain numbers. Revisit only on request.
- Any new issue-URL *construction* logic (the server already has the URL).
- Board issue cards (already link to the forge), judge/findings "filed issue" links (already exist), CLI output (`api/cmd/uzi`).

## Milestones

Milestones are ordered by dependency: **M2 depends on M1**, and **M3's web half depends on M3's server half**. M1 and M3-server touch disjoint files and can proceed in parallel; M2 and M3-web both edit `web/src/lib/api.ts` and should be sequenced or merged carefully (see Dependencies).

- [ ] **M1 — Runs carry the issue web URL (server + FE type).** List path: `LEFT JOIN issues` on `(repo_id, forge_issue_iid)` in `ListRunsForUser` and `ListActiveRunsAll`, select `i.web_url`; `sqlc generate`; map the nullable column in the list handlers (`handler/runs.go`) where `RepoPath` is already set. Detail path: in `GetRun` (`handler/workers.go:1049`), call `GetIssueByIID(repo_id, issue_iid)` best-effort beside `GetForgeTypeForRepo` (`:1076`) and stamp `dto.IssueWebURL` — **no join on `GetRunByID*`**. Add `IssueWebURL *string` (json `issue_web_url`) to `RunDTO` (`apitypes/run.go`; `RunListItemDTO` embeds it, so list+detail are covered by one field). Add `issue_web_url: string | null` to `Run` (`web/src/lib/api.ts:1227`). **Validate**: list DTO includes `issue_web_url` for an issue-run, `null` for a task-run and for a run whose issue is uncached; detail DTO likewise; the `GetRunByID*` return type is unchanged (no caller ripple).

- [ ] **M2 — Issue links on all run surfaces (web).** Render `#<iid>` as an `isHttpsUrl`-guarded forge anchor (board-card style, `ExternalLinkIcon`, `target=_blank rel=noreferrer`, `stopPropagation` inside card `<Link>`s) at surfaces 1, 3, 4, 5; add the `no issue` flag when `issue_iid == null`. Add `issue_web_url` to the run fixtures in `web/src/mocks/data.ts` (e.g. `:698,:705,:807,:882`) so the links render under `VITE_UZI_MOCK=1` and the tests have a positive fixture. **Validate**: an issue-run row links to its `issue_web_url`; a task-run row shows `no issue`; a run with null `issue_web_url` shows a plain number, no anchor; links render in mock mode.

- [ ] **M3 — Schedule last-fire carries + renders the issue URL.** Server: add `WebURL` to `schedsvc.Started`/`Skip` (`outcome.go:8,:18`), thread `issue.WebURL` through `createIssueRun`/build sites (`scheduler.go:410,:434` + fetched-skip sites), populate in `marshalLastFire` (`last_fire.go:58-69`), add `web_url` (json `web_url`) to `apitypes.LastFireStarted`/`LastFireSkip`, and to the FE `LastFireStarted`/`LastFireSkip` types (`api.ts:884,:892`). Web: render the started (`Schedules.tsx:476`) and skip (`:538`) row `#<iid>` as a guarded forge link; add `web_url` to the mock schedule `last_fire` (`web/src/mocks/mockApi.ts:943-970`). **Validate**: a recorded fire's persisted summary includes `web_url` for a started/fetched-skip issue; the row renders a link; an `already_running`/`fetch_failed` skip and an old summary render a plain number.

- [ ] **M4 — Tests.** Go: a **live-DB/integration** test in the store package (alongside `store/run_usage_integration_test.go` / `schedules_livedb_test.go`) that actually executes the joined `ListRunsForUser`/`ListActiveRunsAll` and asserts `issue_web_url` is populated for a run with a cached issue and NULL for one without — a fake-store handler test proves only mapping, not that the SQL join runs (per `.claude/rules/go.md`). A `schedsvc` unit test (`scheduler_test.go`) that `marshalLastFire` writes `web_url` for a started row and leaves it empty for an unfetched skip. Web (vitest): the issue link renders with the correct href for an issue-run; the `no issue` flag renders for a task-run; **no** anchor renders when `issue_web_url`/`web_url` is null — each negative paired with a positive on the same wording (per `.claude/rules/web.md`), and every new assertion proven non-vacuous by a call-site mutation. **Validate**: `task gate:api` and `task gate:web` pass.

- [ ] **M5 — Docs.** CHANGELOG `[Unreleased]` entry ("run issue numbers now link to the forge issue; issue-less runs flagged"). Check whether any `audience: user` doc under `docs/` describes the runs list / run view / schedules and, if so, add a sentence. No new doc page expected.

## Success criteria

1. From the runs list, run detail, dashboard, and board strip, a run's `#<iid>` opens the originating issue on the forge in a new tab; and from a schedule's last-fire panel, a started (or fetched-skip) issue's `#<iid>` does the same.
2. A `task` (or other issue-less) run never renders a dead issue link — it shows the `no issue` flag.
3. A run whose issue is no longer cached, an unfetched skip, and an old persisted fire summary all degrade to a plain number (no broken link, no crash).
4. No new issue-URL construction logic is introduced; the URL always originates from the forge-supplied value.
5. The `GetRunByID*` queries keep returning `store.Run` (no service-layer ripple).
6. `main` is never touched; delivered on a branch + PR.
7. `task gate:api` and `task gate:web` pass, with the join exercised by a live-DB test and new assertions proven non-vacuous.

## Risks & mitigations

- **Widening the detail queries would ripple through ~15 callers.** Mitigation: detail path uses a best-effort `GetIssueByIID` lookup in the handler, not a join (Design Decision 2); M1 validation asserts the `GetRunByID*` return type is unchanged.
- **Join fan-out / performance.** `issues` is `UNIQUE (repo_id, forge_issue_iid)`, so the LEFT JOIN is 1:1 and cheap. `runs.issue_iid` (`pgtype.Int8`) vs `issues.forge_issue_iid` (`int64`) is a non-issue — both are SQL `bigint` and the comparison is in SQL. Mitigation: the live-DB test (M4) confirms no row multiplication.
- **`web_url` unavailable for unfetched skips (M3).** The `already_running`/`fetch_failed` skips never call `f.GetIssue`, so no URL — same as `Title` today. Mitigation: these degrade to a plain number by design; the M3 validate step asserts it.
- **Mock fixtures gate the sanctioned validation path.** `VITE_UZI_MOCK=1` has no live backend, so without fixture `issue_web_url`/`web_url` the links never render in a browser pass and a validator would falsely read "not working" (the "fixture manufactured the finding" hazard in `.claude/rules/web.md`). Mitigation: M2/M3 include the fixture updates; the positive test assertion depends on them.
- **`self_improve` runs have `issue_iid` but may lack a cached forge issue.** The join/lookup simply yields NULL → plain number, no special-casing.
- **Persisted-fire shape change is additive.** Old summaries unmarshal with empty `web_url`; the FE `isHttpsUrl` guard renders a plain number. Keep `apitypes.LastFire*` json tags in lockstep with the `schedsvc` internal structs (`schedule.go:116`).
- **Nested-anchor HTML validity.** `stopPropagation` fixes double-navigation but not the invalid `<a>`-in-`<a>`; render the ref outside the card `<Link>`'s anchor where layout permits (Design Decision 5).
- **Negative-assertion vacuity.** Pair every "no link when null" negative with a positive "link when present" on the same wording (`.claude/rules/web.md`).

## Dependencies

- **No external / internet dependency.** Every fact is codebase-resolvable and the issue `web_url` is already cached server-side; the offline worker can complete this fully. No forge call at render time.
- **Milestone ordering**: M2 needs M1's `Run.issue_web_url`; M3's web half needs M3's server half. M1 (runs) and M3-server (`schedsvc`) touch disjoint files — parallelizable.
- **Shared-file collisions if parallelized**: `web/src/lib/api.ts` is edited by M1 (`Run`, `:1227`) and M3 (`LastFireStarted`/`Skip`, `:884/:892`); `web/src/pages/Schedules.tsx` by M3 only (surface 8 is out of scope, removing the earlier M3/M4 clash). Sequence the two `api.ts` edits or land them in one branch.
- Touches Go (`api`: `store`, `handler`, `apitypes`, `schedsvc`) and web. No controller or agent changes.

## Decision log

- **2026-08-20**: Link target = forge issue (external), chosen by the user over in-app issue view. Breadcrumb's in-app link kept.
- **2026-08-20**: Runs — **list** queries use a LEFT JOIN (already Row structs); **detail** uses a best-effort `GetIssueByIID` lookup in the handler, NOT a join, to avoid the `store.Run` → Row-struct ripple the repo already rejected for `forge_type` (`runtime.sql:384`). No migration/backfill; covers historical runs.
- **2026-08-20**: Schedules — snapshot `web_url` into the persisted fire summary (`schedsvc`, not the poller); only started + fetched-skip rows get a URL, others degrade to plain number.
- **2026-08-20**: Secondary surfaces 7 (chat request) and 8 (schedule targets) moved out of scope — no `web_url` source, low value.
- **2026-08-20**: Issue-less runs flagged with a `no issue` chip; flagging driven by `issue_iid == null`, link rendering by a valid `issue_web_url`.
- **2026-08-20**: Next step = queue for the uzi **Night-Shift** sweep (currently disabled — the issue waits until that schedule is enabled). PRD authored to be internet-independent for the offline worker.
- **2026-08-20**: Reviewer-driven corrections (two Explore reviewers): fixed the M1 detail-query seam, relocated M3 from poller to `schedsvc` with exact population points, dropped the phantom `.link` class citation, added mock-fixture and live-DB-test requirements, and moved the secondary surfaces out of scope.
