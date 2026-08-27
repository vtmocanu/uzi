# PRD #344: Schedule repo repoint on edit + enabled/disabled control at create (UI + CLI)

**GitHub Issue**: [#344](https://github.com/vtmocanu/uzi/issues/344)
**Status**: Draft (created 2026-08-17)
**Priority**: Medium
**Related**:
- [#241](https://github.com/vtmocanu/uzi/issues/241) — the `run_schedules` surface this PRD extends (`00103_run_schedules.sql`, `api/internal/handler/schedules.go`, `web/src/components/ScheduleModal.tsx`).
- The schedule create/edit CLI (`api/cmd/uzi/schedule.go`) and its embedded skill doc (`api/internal/uzicli/skill/SKILL.md`), the source of truth regenerated into `~/.claude/skills/uzi-cli/`.

## Problem

Two independent gaps in schedule management, both hit live on 2026-08-17 while repointing the six uzi schedules from a retired repo to a new one:

1. **A schedule's repo cannot be changed after creation.** `uzi schedule edit` has no `--repo` flag, and the web schedule editor renders the repo picker only in create mode (`web/src/components/ScheduleModal.tsx:504-515`, gated `{!isEdit && !pinned}`). The edit route is `PATCH /api/schedules/{id}` (`api/internal/handler/handler.go:644`), keyed on the schedule id with **no repo in the path**, and `apitypes.ScheduleRequest` has no `repo_id` field (`api/internal/apitypes/schedule.go:31-52`), so the server literally cannot move a schedule's repo. Moving a schedule therefore forces delete-and-recreate, which loses the schedule id and its run-history association and requires re-entering the prompt/guidance/cron/model by hand. (This is exactly what the six-schedule move had to do.)

2. **A schedule cannot be created disabled.** `uzi schedule create` never sets `req.Enabled` (`buildScheduleRequest`, `api/cmd/uzi/schedule.go:103-218`), so the server default (`enabled=true`) always applies, and the web create form has no enabled/disabled toggle (`buildInput` sends no `enabled`, `ScheduleModal.tsx:377-404`). To end up with a disabled schedule you must create it — which makes it live — and then immediately `schedule pause` it, a two-step dance with a brief window in which a due schedule could fire.

The server is closer to done than it looks for gap 2 and further for gap 1:
- **Gap 2 server support already exists.** `applyCreateDefaults` (`schedules.go:476-493`) already honors a caller-supplied `Enabled` (nil → true, an explicit `false` respected, `:485-488`) and `CreateRunSchedule` already passes it (`schedules.go:198`). Feature B is CLI + web + docs + tests only.
- **Gap 1 needs a real server change.** The PATCH handler `mergeSchedule` (`schedules.go:510-596`) changes every mutable field **except** `repo_id`, and `UpdateRunSchedule` (`api/internal/store/queries/schedules.sql:34-64`) does not set `repo_id`. Feature A adds a `repo_id` to the DTO, the update query, and the merge, guarded by the same ownership check create uses.

**No migration is needed.** `enabled boolean NOT NULL DEFAULT true` and `repo_id uuid NOT NULL REFERENCES repos(id) ON DELETE CASCADE` both already exist (`00103_run_schedules.sql:17,30`).

## Solution

### Feature A — repoint a schedule's repo on `edit`

Because the edit route is keyed on the schedule id (no repo in the URL), the new repo travels in the request **body**:

- **DTO:** add `RepoID string \`json:"repo_id"\`` to `apitypes.ScheduleRequest` (`schedule.go:31-52`). The PATCH decoder uses `DisallowUnknownFields`, and the CLI edit path already rebuilds a typed `ScheduleRequest` (documented at `api/cmd/uzi/schedule.go:346-357`), so a new struct field is compatible.
- **Store:** add `repo_id = @repo_id` to `UpdateRunSchedule` (`schedules.sql:45-64`), then `sqlc generate` (`.claude/rules/go.md`) to regenerate `UpdateRunScheduleParams`.
- **Handler:** in `PatchSchedule` (`schedules.go:253-318`), when `RepoID` is set, validate the new repo with `GetRepoForUser(newRepoID, user.ID)` and treat not-found as **404** — mirroring exactly what create does via `repoForRequest` (create-time eligibility is **ownership only**; it does not check `enabled`/guardrail/blocked state, which run at fire time). Seed/overlay `repo_id` in `mergeSchedule`, pass it to `UpdateRunSchedule`, and **add `req.RepoID == ""` to `onlyEnabled`** (`schedules.go:497-504`) so a pause/resume that happens to carry `repo_id` still short-circuits correctly.
- **CLI:** add `--repo` to `newScheduleEditCmd` (`schedule.go:331-343`) and set `req.RepoID` when the flag is `Changed()` (`buildScheduleEditRequest`, `:358-492`).
- **Web:** relax the create-only gate on the repo `<Select>` so it renders in edit mode, load repos in edit too (`ScheduleModal.tsx:269-283`, `:504-515`), and add `repo_id` to `buildInput` on edit; add `repo_id?: string` to `ScheduleInput` (`web/src/lib/api.ts:962-984`) and merge it in the mock `updateSchedule` (`web/src/mocks/mockApi.ts:3681-3705`, refreshing `repo_path`).

Schedule id and run history are preserved by construction (edit is an UPDATE, not delete+recreate); past runs keep their own frozen `repo_id`, so nothing is rewritten and no FK breaks.

### Feature B — enabled/disabled control at `create`

Server already supports it, so:
- **CLI:** add `--enabled=<bool>` to `newScheduleCreateCmd` and set `req.Enabled` only when the flag is `Changed()` (omit → nil → server default `true`, preserving today's behavior; mirror the existing `--auto-approve` pointer handling).
- **Web:** add an enabled/disabled toggle to the create form (the `Toggle` component is already imported, `ScheduleModal.tsx:28`) and include `enabled` in `buildInput`. (`ScheduleInput.enabled` and the mock already exist.) Edit-mode enable/disable stays the domain of `pause`/`resume`, unchanged.

### Correct a stale doc found in-flight

`apitypes/schedule.go:8` and `web/src/lib/api.ts:959-960` say the create default is `wait_on_limit=false`, but `applyCreateDefaults` (`schedules.go:481-483`, pinned by `TestApplyCreateDefaults` at `schedules_test.go:189-190`) sets it **true**. (Note `applyCreateDefaults`' own doc comment at `schedules.go:466-475` is already correct — only these two DTO comments are stale, and `applyCreateDefaults` is not edited by either feature.) Fix each stale comment in the milestone that already opens its file: the `apitypes/schedule.go` one in **M3** (which adds `RepoID` there) and the `api.ts` one in **M6** (which adds `repo_id` to `ScheduleInput`), per the repo's fix-the-doc-when-you-disprove-it rule.

## Milestones

Grouped so Feature A and Feature B are distinguishable. Feature B first (its server is ready).

- [x] **M1 — Feature B, CLI.** `--enabled=<bool>` on `uzi schedule create`, wired into `buildScheduleRequest` by setting `req.Enabled` only when the flag is `Changed()` (omit → nil → server default `true`; this is the `--model`/`--guidance` pattern, **not** the always-send `--auto-approve` one — both yield omit→enabled, but the `Changed()` form avoids duplicating the default client-side). Update `SKILL.md` create synopsis (`:127`) + prose (`:386-389`). CLI test (`schedule_test.go`) + a create-defaults assertion (`TestApplyCreateDefaults`, `schedules_test.go:182`); a live-DB test confirming create honors an explicit `enabled:false`. No server change.
- [x] **M2 — Feature B, Web.** Enabled/disabled toggle on the create form; `buildInput` sends `enabled`; `ScheduleModal.test.tsx` / `Schedules.test.tsx` coverage. (`ScheduleInput.enabled` + mock already support it.)
- [x] **M3 — Feature A, server core.** Add `RepoID` to `ScheduleRequest` (and fix the stale `wait_on_limit` comment at `apitypes/schedule.go:8` while there); add `repo_id` to `UpdateRunSchedule` (+ `sqlc generate`). In `PatchSchedule`, on a non-empty `RepoID`: `uuid.Parse` it first — **400 on malformed**, distinct from the **404** `GetRepoForUser` returns on a foreign/absent repo, so a garbage id never reaches the store as a zero UUID — then the ownership-mirror check. Update `onlyEnabled` (+`RepoID==""`), and **seed `repo_id` from the current row in `mergeSchedule` (keep-on-empty, NOT the replace-semantics used for `max_issues`/`guidance`/`model`)** so a config edit that omits `--repo` preserves the stored repo (see the S1 risk). Go unit tests: `TestOnlyEnabled` extended for `{enabled, repo_id}`; a merge test asserting a config PATCH **without** `repo_id` preserves the stored `repo_id`; a repoint merge test.
- [x] **M4 — Feature A, server integration tests.** Split out from M3 deliberately (not a convention violation): live-DB tests run through a distinct harness (`./e2e/run-store-it.sh`) and CI lane (`test:api-store-it`), per `.claude/rules/go.md`. A repoint round-trip that moves `repo_id` while preserving the schedule id + history; an owner-isolation test where repointing to a foreign/absent repo returns 404; a malformed-`repo_id` → 400 case.
- [x] **M5 — Feature A, CLI.** `--repo` on `uzi schedule edit`, wired into `buildScheduleEditRequest`; `SKILL.md` edit synopsis (`:130`) + prose (`:411-421`); CLI tests; `skill_drift_test.go` green.
- [x] **M6 — Feature A, Web.** Repo selector in edit mode (`ScheduleModal`), `ScheduleInput.repo_id` (and fix the stale `wait_on_limit` comment at `web/src/lib/api.ts:959-960` while there), mock `updateSchedule` repoint (+ `repo_path` refresh), component tests. `task gate:web` green. The repo `<Select>` is disabled with a hint for an issue target, and `buildInput` gates `repo_id` on `target !== "issue"` so the UI never provokes the 422.

Documentation (SKILL.md) and unit tests are folded into each milestone, per this repo's milestone convention; M4 is the one deliberate exception (live-DB integration tests run in a separate harness + CI lane — see M4).

## Success criteria

1. `uzi schedule create --enabled=false <...>` creates a schedule that is immediately disabled, with no separate `pause` step; omitting the flag preserves today's always-enabled default (both asserted, so the test is not vacuous). The web create form offers the same toggle.
2. `uzi schedule edit <id> --repo <new-repo-id>` moves the schedule to the new repo in place, preserving the schedule id and its run history; the web editor offers a repo selector in edit mode.
3. Repointing to a repo the caller does not own returns 404 (ownership mirror), and no `enabled`/guardrail check is newly imposed at repoint (parity with create).
4. `pause`/`resume` still short-circuit correctly (the `onlyEnabled` fast path is not broken by the new field).
5. Both Go gates and `task gate:web` are green; `sqlc generate` is a no-op after M3; the CLI skill-drift test passes.

## Decision Log

- **D1 — Feature B is client-only; the server already honors `Enabled`.** `applyCreateDefaults` (`schedules.go:485-488`) and `CreateRunSchedule` (`:198`) already pass a supplied `Enabled`; adding a server change for B would be redundant. B's server "work" is one live-DB test proving the existing behavior, not new code.
- **D2 — Repoint travels in the body, not the URL.** Edit is `PATCH /api/schedules/{id}` with no repo in the path (`handler.go:644`), so a repo selector in the URL is not an option; the DTO gains `repo_id`. The PATCH `DisallowUnknownFields` decoder tolerates the new struct field (the CLI edit rebuild already emits a typed `ScheduleRequest`).
- **D3 — Repoint eligibility mirrors create exactly: ownership only.** Create checks just `GetRepoForUser` (owner-scoped, `forge.sql:80-92`); it does not gate on `repo.enabled`, guardrail/override, or blocked-repos, because those run at fire time via the shared run-creation seam. So repoint re-runs `GetRepoForUser` for the new `repo_id` (404 on foreign/absent) and adds no new gate — matching create keeps the two paths consistent.
- **D4 — Repointing an `issue`-target schedule is a real silent-wrong-work hazard (Open Decision 1).** `issue_iid` is repo-relative (`00103:19`) and forge IIDs are small sequential per-project integers, so the target repo very likely already has an issue at the same IID — a *different, unrelated* issue. The fire path `GetIssue(repo.ForgeProjectID, iid)` (`schedsvc/scheduler.go:259`) then **succeeds against the wrong issue** rather than skipping as `fetch_failed` (`SkipFetchFailed` covers only the fewer-issues case). With `auto_approve` defaulting **true**, a repointed issue schedule can auto-run an unrelated issue all the way to an MR. It is not an FK/data-integrity problem (`issue_iid` is a bare bigint), but it is silent wrong work — materially riskier than "points at a possibly-nonexistent issue." See Open Decisions; the recommendation there is now to **restrict** issue-target repoint, not allow it unconditionally.
- **D5 — No migration.** Both `enabled` and `repo_id` columns already exist on `run_schedules` (`00103:17,30`); Feature A only makes the previously-immutable `repo_id` settable in the UPDATE. Do not add a migration.
- **D6 — `onlyEnabled` must learn about `repo_id`.** The pause/resume fast path (`schedules.go:497-504`) short-circuits when only `enabled` changed. If `RepoID` is added to the DTO without adding `req.RepoID == ""` there, a pause/resume carrying `repo_id` would silently skip the config UPDATE. This is the one non-obvious regression the change must avoid (covered by extending `TestOnlyEnabled`).
- **D7 — Fix the stale `wait_on_limit` doc in the same change.** `apitypes/schedule.go:8` and `web/src/lib/api.ts:959` claim create defaults `wait_on_limit=false`; `applyCreateDefaults` sets it `true`. Feature B edits that function, so the comments are corrected in M4 rather than left to rot.

## Risks & mitigations

- **`onlyEnabled` regression** (D6) — mitigated by the explicit `RepoID == ""` guard and an extended `TestOnlyEnabled`.
- **`mergeSchedule` must SEED `repo_id` from the current row, not write `req.RepoID` blindly — co-equal to D6, and worse if missed (S1).** `run_schedules.repo_id` is `NOT NULL` + FK (`00103:17`), and every config edit (`!onlyEnabled`) flows through `UpdateRunSchedule`. Once that query takes a `repo_id` param, a config edit that omits `--repo` must carry the stored `repo_id` through `mergeSchedule` (which holds none today); writing `req.RepoID` directly → `""` → `uuid.Nil` → `SET repo_id='00000000-…'` → FK violation → **500 on EVERY config edit, not just repoints**. Mitigated by keep-on-empty seed semantics (M3) and the "config PATCH without repo_id preserves stored repo_id" test (M3).
- **Adding `RepoID` to the shared `ScheduleRequest` relaxes the CREATE path (S3).** `CreateSchedule` takes the repo from the URL and ignores `req.RepoID`; today a create body carrying `repo_id` is 400-rejected as an unknown field, but once the field exists it becomes accepted-and-silently-ignored. Low-severity footgun; either document it or have `CreateSchedule` reject a non-empty body `repo_id`. Recommend documenting — a create caller has no reason to send `repo_id`.
- **Prompt-run dedup after a repoint.** `HasActiveRunForSchedule` keys on `schedule_id` (`schedules.sql:152-160`), not repo, so a still-active prompt run from the old repo would dedup a fire on the new repo. Acceptable (one active prompt run per schedule) and noted, not fixed.
- **CLI skill-drift catches doc-before-flag, NOT flag-without-doc.** `skill_drift_test.go` enforces only the forward direction — every flag named in `SKILL.md` must exist in the cobra tree — so a `SKILL.md` line added before the flag reddens it, but a `--enabled`/`--repo` wired and left **undocumented** does NOT (its header comment claims reverse-completeness; the implementation at `:43-47` does not enforce it — a pre-existing gap, out of scope here). So M1/M5 must add the `SKILL.md` lines by hand; do not lean on the drift test to catch a missing doc.
- **sqlc field-order assumptions.** The regenerated `UpdateRunScheduleParams` gains a `RepoID` field; regenerate with `sqlc generate` and let the compiler confirm the call site, rather than hand-editing `schedules.sql.go`.

## Out of scope

- Any schedule migration or new column (none needed — D5).
- Edit-mode enable/disable via the `enabled` field: that stays the job of `pause`/`resume` (this PRD adds `enabled` only to **create**).
- Validating that an `issue` schedule's `issue_iid` exists in the target repo on repoint (create does not validate it either; fire-time handles the miss).
- Any change to the fire-time run-creation seam, dedup, or eligibility gates.

## Open decisions (RESOLVED)

1. **Repointing an `issue`-target schedule (D4): RESOLVED → restrict.** The options were: restrict it (reject a repoint when `target=issue`; the user deletes+recreates for that rare case), require the `issue_iid` to be re-supplied alongside `--repo`, or allow it unconditionally. Because a same-IID issue almost always exists in the target repo, unconditional allow means silently running an **unrelated** issue to an MR under auto-approve (D4). **Decision: restrict.** The human was unavailable at the approval gate, so this was decided on best judgment following the PRD's own recommendation — restrict is the smallest surface and removes the silent-wrong-work hazard entirely; it is easily revisited later if a re-supplied-`issue_iid` flow is wanted. Implemented as the pure `scheduleRepoChange` classifier in `PatchSchedule` returning **422** when the merged `target=issue` and `repo_id` changes (M3), the CLI surfacing that 422 (M5, no client mirror to avoid message drift), and the web disabling the repo selector for issue targets plus gating `buildInput`'s `repo_id` on `target !== "issue"` so the UI never provokes the 422 (M6). Covered by unit (`TestScheduleRepoChange`), live-DB (`TestScheduleRepointIssueTargetRestrictedLiveDB`), and component tests.

## Milestone parallelization

Feature A and Feature B genuinely share only three files — `api/cmd/uzi/schedule.go` (B: `buildScheduleRequest`; A: `buildScheduleEditRequest`), `web/src/components/ScheduleModal.tsx` (B: create toggle; A: relax the edit gate), and `SKILL.md` (create vs edit prose); `schedules.go`, `apitypes/schedule.go`, and `mockApi.ts` are **Feature-A-only** (B needs no non-test server change and the mock already honors `enabled`). A strict by-file split is therefore more feasible than a six-file overlap would suggest, but the three real overlaps still favor sequential **B (M1→M2) then A (M3→…→M6)**. Within Feature A the server (M3) must precede the clients (M5, M6), which depend on the new DTO field; M4 (integration tests) does not gate M5/M6. Single repo, single MR.

---

*Drafted 2026-08-17 against code at current `main` HEAD, from a codebase investigation this session. File anchors cite symbols (`buildScheduleRequest`, `PatchSchedule`, `mergeSchedule`, `onlyEnabled`, `UpdateRunSchedule`, `applyCreateDefaults`, `ScheduleModal`) as well as lines, since line numbers drift. No open-web dependency: cobra CLI, `schedsvc` validation, the sqlc store, chi routing, and the React modal + mock are all in-repo, so an offline uzi worker can implement all six milestones from the sources cited.*
