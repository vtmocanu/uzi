# PRD #2343: "When to send issues" capacity gate for label sweeps

**Status**: Done — M1 and M2 implemented and verified

## Implementation progress

- [x] Capacity storage, scheduler admission, API/PATCH/default/reset/copy behavior, and optional fire outcomes.
- [x] CLI flags and capacity rendering, synchronized client contracts, and scheduling documentation.
- [x] Focused capacity tests executed against throwaway PostgreSQL; four mutation probes detected the intended defects and the restored control passed.
- [x] Web controls, capacity summaries, mock behavior, and visual parity evidence (M2).

**Issue**: [#2343](https://github.com/vtmocanu/uzi/issues/2343) (part of umbrella #2342; followed by #2344)
**Priority**: Medium
**Design mock**: `prds/mockups/2342-capacity-gated-sweeps-mock.html` (shared with #2344; open it in a browser). This PRD implements the gate parts of frames 1 and 2; the removal toggle, label-removal last-fire states, the minutes cadence and frame 3 belong to #2344. The preview strip in frame 1 is a mock-only aid.

## Problem

An owner who triages a large backlog (say 30 issues) has no way to feed it to uzi gradually. A label sweep fires on its cron and starts up to `max_issues` runs regardless of how much work the owner already has unfinished. Draining a backlog today means one of two bad options: a big `max_issues` that floods the factory and the owner's token, or hand-dispatching issues one at a time as runs finish.

## Outcome

Any recurring, label-selected sweep can carry an optional **capacity gate**, shown as "When to send issues": a ceiling `C` on the owner's unfinished runs and a threshold `K`. On each fire (cron tick or Run now) the sweep counts the owner's unfinished runs; when `max(0, C - in_flight) < K` it starts nothing and records a schedule-level "Waiting for room" outcome; otherwise it starts up to `min(N, C - in_flight)` eligible issues, oldest first, where `N` is the existing `max_issues`.

Acceptance examples:

1. A custom gated sweep (`C=4, K=2, N=1`) on a repo with 5 open eligible issues; the owner has 3 unfinished runs. A fire records `capacity: {in_flight: 3, limit: 4, room_needed: 2, room: 1, blocked: true}`, never calls the forge or lists candidates, records `matched: 0`, and the schedule advances to its next cron fire.
2. Same sweep, the owner has 1 unfinished issue run and 2 open chat runs. Chat is excluded, so `in_flight = 1`; room 3 >= 2, and with `N=1` the fire starts one run on the oldest candidate.
3. A sweep with `C=6, K=2, N=3` and 4 unfinished runs starts 2 (room 2, not 3). With 7 unfinished runs, room is clamped to 0 and the fire is blocked.
4. `uzi schedule run-now` on a blocked gated sweep prints "Waiting for room: space for 1 more run; needs 2" and starts nothing; the JSON response carries `capacity`.
5. Create or edit refuses (400) a gate on a non-sweep target, a one-time schedule, or an `assigned` selector; a half-set gate; `K > C`; values outside 1..50. A gate on a multi-label sweep is accepted.

## Out of scope

- An exact, serialized admission cap. The gate is an advisory snapshot: two gated schedules firing in the same tick, or a manual start, can push the owner past `C`. Documented, not prevented (Decision 3).
- Gating targets other than recurring label-selected sweeps: pinned issue, prompt, self-improve, the `assigned` selector kind, one-time schedules.
- Worker-capacity gating (fleet slots) (Decision 1).
- Label removal on dispatch, the `ondeck-sweep` default and the "Every N minutes" cadence: #2344.
- Changing existing sweeps' behaviour: existing schedules keep a NULL gate; catalog entries are unchanged.

## Modules and seams

### Store

- Migration, additive only, number assigned at landing (`task migration:renumber`): `run_schedules.capacity_limit integer NULL`, `run_schedules.capacity_room_needed integer NULL`, CHECK both NULL, or both set with `1 <= capacity_room_needed <= capacity_limit <= 50`.
- In-flight count: **reuse** `CountInProgressRunsForUser` (`api/internal/store/queries/runtime.sql`, the Runs nav badge count): the user's runs with `kind NOT IN ('chat','judge')` and a non-terminal status. Its kind filter is pinned to `runkind` by `TestRuntimeSQLKindFilterMatchesListed` (`api/internal/runkind/runkind_sql_test.go`). It counts every origin and repo, queued and parked states, and `job` runs (Decision 2). Existing indexes `idx_runs_user (user_id)` and `idx_runs_claimable (user_id, status, created_at)` cover it; no new index.

### Scheduler (`api/internal/schedsvc/scheduler.go`)

- **Gate placement**: in `fireSweep`, after the default-origin overlay has resolved the selector kind and labels (C and K are row values persisted at enable time, not overlaid), and BEFORE `ListSweepCandidateIssues`, so a blocked fire spends no forge call and no candidate query. The tick path and `RunNow` both reach it through `fireOne`.
- **Supported configuration**: a recurring, label-selected sweep with one or more selector labels (AND containment is fine for gating).
- **Fail closed on effective config**: before listing candidates or creating any run, if a gated row's resolved selector kind is not `label` (for example after a catalog change), the fire starts nothing and records skip reason `config_not_supported` (new typed `SkipReason`, schedule-level, no issue). The row stays active so the owner can fix it. #2344 extends this check to label removal.
- **Blocked**: return `FireOutcome{Matched: 0, Capacity: &CapacityCheck{InFlight, Limit, RoomNeeded, Room, Blocked: true}}`, nil error; `advance` records it and moves `next_fire_at`. No per-issue skip is fabricated, so `Matched == len(Started) + len(Skips)` still holds (`Matched` is computed from both lists).
- **Not blocked**: started cap = `min(max_issues, room)` (NULL `max_issues` becomes `room`); scan window = that cap + `backfillHeadroom`; `Capacity` carried with `Blocked: false`.
- **Count error**: transient; return the error, do not advance (existing transient path).
- **Bounds**: gate inputs are owner integers bounded by CHECK and handler validation (1..50); the count is one indexed query per gated fire; a gate only lowers the existing started cap.

### Outcome, last-fire and run-now contracts

- `FireOutcome` gains `Capacity *CapacityCheck`.
- Persisted `last_fire` (`api/internal/schedsvc/last_fire.go`): `capacity` (omitempty `{in_flight, limit, room_needed, room, blocked}`). Older rows render exactly as today.
- Run now: `handler.runNowResponse` (`api/internal/handler/schedules.go`) maps `FireOutcome` into `apitypes.RunNowResponse` separately and persists nothing; it gains `capacity`.
- Mirrors: `apitypes` (`schedule.go`: `LastFire`, `RunNowResponse`, schedule DTO), web `web/src/lib/apiTypes.ts` (`LastFire`, run-now type), and the skip label maps `web/src/lib/scheduleSkipReasons.ts` and `api/cmd/uzi/schedule_render.go` for `config_not_supported`.

### API

- Schedule DTO and create/patch requests gain `capacity_limit` and `capacity_room_needed`.
- PATCH semantics: an omitted field preserves the stored value (a retime must not drop the gate); an explicit clear of BOTH capacity fields removes the gate; one cleared and one set is a 400. Do NOT copy `max_issues`: `mergeSchedule` assigns `m.MaxIssues = req.MaxIssues` directly, so an omitted `max_issues` clears it, which would drop the gate on an unrelated edit. Use presence-aware decoding like `OptionalHarness` / `OptionalCredentialOverride` (`api/internal/apitypes/schedule.go`): seed from the stored row and replace only when the field is present. Pin omission versus clear with tests on both the custom and the default-row PATCH paths.
- Validation (`api/internal/handler/schedules_request.go`), all 400 to match the sibling range checks: the refusals in example 5. Default rows validate against the catalog-resolved selector kind, not the row's NULL `labels`.
- Default-origin rows: add the two fields to `patchDefaultScheduleConfig`'s editable set and to `defaultEditableDiverges` (`api/internal/handler/schedules_catalog.go`) so an owner edit marks the row customized; `schedule reset` clears them back to the catalog value (NULL for every current entry); copy them in `api/internal/handler/schedules_clone.go`.

### CLI (`api/cmd/uzi/schedule*.go`, `api/internal/uzicli`)

- `schedule create|edit`: `--capacity-limit <n> --room-needed <k>` (together), `--clear-capacity` on edit.
- `schedule get`: a `WHEN TO SEND` row (`limit 4 · room 2` or `off`) and the last fire's capacity line. `run-now`: the blocked line from example 4.
- Help and render text say "issues to send at a time" for `--max-issues` (Decision 6). Update the embedded skill source `api/internal/uzicli/skill/SKILL.md`.

### Web

- `web/src/components/ScheduleModal.tsx`: the "When to send issues" fieldset per mock frame 1: a toggle, then one sentence with three inline number inputs (C, K, N). While the gate is on, the standalone N field is hidden and the sentence's N input edits the same `max_issues` value; while off, the standalone field shows, labelled "Issues to send at a time" (renamed from "Max issues per run" at both sites). The helper line names the schedule's actual cadence ("Checked daily at 02:00", "Checked every 10 minutes"). The fieldset is hidden for an `assigned` selector and a one-time timing.
- `ScheduleListRow.tsx` pills (`limit 4 · room 2`, `no limit`) and `LastRun.tsx` "Waiting for room" per mock frame 2; mock-mode fixtures in `web/src/mocks/mockApi/schedules.ts`.

### Docs

- `docs/scheduling.md`: a "When to send issues" section, and the renamed field. Each milestone that edits `docs/*.md` runs `task docs:sync` and commits the mirror, because `TestEmbeddedDocsMatchSource` in `test:api` asserts byte equality.

## Testing decisions

- **Live-DB tests are mandatory evidence.** `task gate:api` skips live-DB tests without a database, so each Go milestone also runs `./e2e/run-store-it.sh` (or `go test` with `UZI_TEST_DATABASE_URL` pointing at a throwaway Postgres named outside the `uzi-` namespace) and the PR shows the named tests ran, not skipped.
- **Scheduler gate** (live-DB, beside `credential_disabled_fire_livedb_test.go`): seed runs across two repos, every origin, kinds (issue, chat, judge, job), and statuses (queued, running, awaiting_approval, limit_wait, paused, completed). Cases: blocked (a counting fake forge asserts zero `ListIssues`/`GetIssue` calls and no run created; `matched 0`; schedule advanced); partial batch `min(N, room)`; over-limit WIP clamps room to 0; chat and judge excluded, job counted; terminal excluded; Run now blocked; a gated multi-label sweep accepted and gated normally; `config_not_supported` for a gated row resolving to `assigned` (no run created).
- **Mutations to watch fail** (per `.claude/rules/go.md`): drop the gate; use `N` instead of `min(N, room)`; drop the kind exclusion (by mutating the generated SQL constant); remove the pre-list placement.
- **Handler**: table tests pinning 400 for each refusal in example 5; PATCH omission preserves, both-clear clears, half-clear refuses; default-row edit flips `customized`; reset clears; clone copies; `runNowResponse` carries `capacity`.
- **CLI**: `schedule_render_test.go` and `schedule_test.go` for flags, `WHEN TO SEND`, the blocked run-now line and the renamed help text.
- **Web**: vitest for `ScheduleModal` (sentence inputs, shared N, hidden for assigned/once, request body, cadence-aware helper) and `LastRun` "Waiting for room".
- **Mock parity**: screenshots of the modal, list row and last-fire detail beside the gate parts of mock frames 1 and 2, in the PR.
- **Gates per milestone**: `task gate:api` + live-DB run (M1), `task gate:web` (M2), `task deadcode` (new exported Go and web types are knip/deadcode surface), `task check-docs:web`, `task gate:repo` (migration numbering and additivity).

## Milestones

### M1: Capacity gate through the API and CLI

Migration, reuse of the in-flight count, the `fireSweep` gate, `config_not_supported`, `capacity` in `FireOutcome`/`last_fire`/run-now, DTO/PATCH/validation including default rows, reset and clone, CLI flags, render and help rename, docs section plus `docs:sync`.

- Blocked by: none
- Acceptance: examples 1-5 pass in tests; ungated admission remains unchanged (existing ungated schedsvc and handler assertions retained and green); live-DB evidence in the PR.

### M2: "When to send issues" in the web UI, and the field rename

Web types, the modal fieldset with the shared N input and cadence-aware helper, list-row pills and "Waiting for room" in the last-fire view, mock fixtures; "Max issues per run" renamed "Issues to send at a time" at both modal sites.

- Blocked by: M1
- Acceptance: vitest cases above; mock parity for the gate parts of frames 1 and 2.

## Design mock contract

- `prds/mockups/2342-capacity-gated-sweeps-mock.html` is the presentation reference for layout, order, wording and colour roles of the parts this PRD implements. Implement it, not only this text.
- The "Mock-only preview" strip under the sentence is a reviewer aid; do not implement it.
- Small deviations for consistency with the app (its tokens, components, spacing, the existing Schedules layouts) are expected. A larger deviation only when necessary, listed in the PR body under `Mock deviations` with its reason; an absent list claims parity.
- This PRD's text wins where it specifies; the mock governs what the text leaves out.

## Decision Log

1. **Count the owner's unfinished runs, not worker slots.** A worker's `max_concurrent_runs` is self-reported and hosted burst workers make fleet capacity elastic (bounded per user by `UZI_EPHEMERAL_MAX_PER_USER`), so "free slots" would differ between compose and k8s. A per-owner limit is deterministic, testable and topology-independent. Rejected: fleet slot arithmetic; an "empty queue" check (no `K` control). (User decision with peer review, 2026-10-06.)
2. **Scope is user-wide across repos and run origins, excluding chat and judge, counting job runs.** The owner's token and attention are shared across repos; chat is interactive and judge is a background review, so counting them would stall the drain while the owner chats. Job runs use the owner's token and a worker slot, so they count (user decision). Parked and waiting states count because they are outstanding commitments. Reusing `CountInProgressRunsForUser` gives exactly this set, already pinned to `runkind`, and keeps the gate consistent with the Runs badge. Rejected: a new query (duplicates an existing pinned one); repo-wide or this-schedule-only counts.
3. **`K` is a trigger threshold, not a reserve; partial batches are allowed; the cap is advisory.** Start nothing when room < `K`, else `min(N, room)`. An exact cap would need a lock shared by every run-creation path (manual, autopilot, sweeps), out of proportion for a pacing control; the UI says "other activity can exceed this limit".
4. **A setting on the existing sweep target, not a new target type.** The sweep already has the selector, oldest-first cap, backfill, eligibility gate, auto-approve, wait-on-limit and typed outcomes; only admission is new here.
5. **Blocked fires are a schedule-level outcome, not per-issue skips, and gates are refused on one-time schedules.** Fabricated skips would break `Matched == Started + Skips` and spend forge calls; a one-time schedule is consumed by any benign advance, so a blocked fire would end it. Validation errors are 400, matching the sibling range checks in `schedules_request.go`.
6. **Wording** (peer-reviewed, user-chosen): "Use [C] unfinished runs as my limit. Wait until there's room for at least [K] more, then send up to [N] issue(s)." with the helper "Counts your unfinished work across all repos, excluding chat and judge runs. Checked <cadence>; other activity can exceed this limit." "Keep at most" was rejected because it promises a hard cap; "going" because it hides parked work. The user approved renaming "Max issues per run" to "Issues to send at a time", because each issue gets its own run; the API field stays `max_issues`.
7. **Split from umbrella #2342** (user decision with peer agreement, 2026-10-06): the gate is independently valuable and gives #2344 a proven seam; label removal and the default job follow in #2344.
