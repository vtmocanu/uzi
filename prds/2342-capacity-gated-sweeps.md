# PRD #2342: Capacity-gated label sweeps and the on-deck backlog drain

**Status**: Draft
**Issue**: [#2342](https://github.com/vtmocanu/uzi/issues/2342)
**Priority**: Medium
**Design mock**: `prds/mockups/2342-capacity-gated-sweeps-mock.html` (open it in a browser; the preview strip in frame 1 is a mock-only aid, see the mock contract)

## Problem

An owner who triages a large backlog (say 30 issues) has no way to feed it to uzi gradually. A label sweep fires on its cron and starts up to `max_issues` runs regardless of how much work the owner already has unfinished. Draining a backlog today means one of two bad options: a big `max_issues` that floods the factory and the owner's token, or hand-dispatching issues one at a time as runs finish.

The existing sweep also never consumes its selector label. A `Planned` issue whose run fails is picked again on the next fire, as the oldest candidate, every time; for a backlog drain that retries a broken issue forever.

## Outcome

Any label-selected sweep can carry an optional **capacity gate**, shown as "When to send issues": a ceiling `C` on the owner's unfinished runs and a threshold `K`. On each fire (cron tick or Run now) the sweep counts the owner's unfinished runs; when `max(0, C - in_flight) < K` it starts nothing and records a schedule-level "Waiting for room" outcome; otherwise it starts up to `min(N, C - in_flight)` eligible issues, oldest first, where `N` is the existing `max_issues`. A single-label sweep can also remove its selector label from an issue once that issue's run is created (best effort, see Decision 4). A new catalog default, `ondeck-sweep`, combines both: label `on-deck`, every 10 minutes, `N=1`, `C=4`, `K=2`, label removed on dispatch.

Acceptance examples:

1. A custom gated sweep (`C=4, K=2, N=1`) on a repo with 5 open eligible issues; the owner has 3 unfinished runs. A fire records `capacity: {in_flight: 3, limit: 4, room_needed: 2, blocked: true}`, never calls the forge or lists candidates, records `matched: 0`, and the schedule advances to its next cron fire.
2. Same sweep, the owner has 1 unfinished issue run and 2 open chat runs. Chat is excluded, so `in_flight = 1`; room 3 >= 2, and with `N=1` the fire starts one run on the oldest candidate.
3. A sweep with `C=6, K=2, N=3` and 4 unfinished runs starts 2 (room 2, not 3). With 7 unfinished runs, room is clamped to 0 and the fire is blocked.
4. `uzi schedule run-now` on a blocked gated sweep prints "Waiting for room: space for 1 more run; needs 2" and starts nothing; the JSON response carries `capacity`.
5. A sweep with `remove_label_on_dispatch` and selector `["on-deck"]` starts a run on an issue carrying `on-deck` and `uzi`; afterwards the forge and uzi's cached issue both lack `on-deck` and still carry `uzi`. If that run fails, the next fire does not pick the issue.
6. Create or edit refuses (400) the removal flag when the resolved selector is empty, has more than one label, or contains the configured `uzi` label; and refuses a gate or the flag on an `assigned` selector or a one-time schedule.

## Out of scope

- An exact, serialized admission cap. The gate is an advisory snapshot: two gated schedules firing in the same tick, or a manual start, can push the owner past `C`. Documented, not prevented (Decision 3).
- Gating targets other than label-selected sweeps: pinned issue, prompt, self-improve, the `assigned` selector kind, and one-time (`timing=once`) schedules.
- Worker-capacity gating (fleet slots). The gate counts the owner's runs, not worker slots (Decision 1).
- Strict once-only dispatch. Label removal is best effort (Decision 4); no per-schedule dispatch ledger.
- Automatic retry of failed `on-deck` issues; the owner re-adds the label.
- Changing existing sweeps' behaviour: existing schedules keep a NULL gate and `remove_label_on_dispatch = false`, and existing catalog entries are unchanged.

## Modules and seams

### Store

- Migration(s), additive only, numbers assigned at landing (`task migration:renumber`; M1 and M3 each add one, so landing may renumber twice):
  - M1: `run_schedules.capacity_limit integer NULL`, `run_schedules.capacity_room_needed integer NULL`, CHECK both NULL, or both set with `1 <= capacity_room_needed <= capacity_limit <= 50`.
  - M3: `run_schedules.remove_label_on_dispatch boolean NOT NULL DEFAULT false`.
- In-flight count: **reuse** `CountInProgressRunsForUser` (`api/internal/store/queries/runtime.sql`, the Runs nav badge count): the user's runs with `kind NOT IN ('chat','judge')` and a non-terminal status. Its kind filter is already pinned to `runkind` by `TestRuntimeSQLKindFilterMatchesListed` (`api/internal/runkind/runkind_sql_test.go`). It counts every origin and repo, queued and parked states, and `job` runs (Decision 2). Existing indexes `idx_runs_user (user_id)` and `idx_runs_claimable (user_id, status, created_at)` cover it; no new index.

### Scheduler (`api/internal/schedsvc/scheduler.go`)

- **Gate placement**: in `fireSweep`, after the default-origin overlay has resolved the selector kind and labels (C, K and the removal flag are row values persisted at enable time, not overlaid), and BEFORE `ListSweepCandidateIssues`, so a blocked fire spends no forge call and no candidate query. The tick path and `RunNow` both reach it through `fireOne`.
- **Supported configurations**: a gate needs a recurring, label-selected sweep (one or more selector labels; AND containment is fine for gating). The removal flag additionally needs exactly one effective selector label.
- **Fail closed on effective config**: before listing candidates or creating any run, if a row has a gate or the removal flag but its resolved selector kind is not `label`, or has the removal flag but its effective selector (after the catalog overlay) is not exactly one label, the fire starts nothing and records skip reason `config_not_supported` (new typed `SkipReason`, schedule-level, no issue). This catches a catalog update that changes a default's labels after enable. The row stays active so the owner can fix it. The live `uzi`-label protection below still applies.
- **Blocked**: return `FireOutcome{Matched: 0, Capacity: &CapacityCheck{InFlight, Limit, RoomNeeded, Room, Blocked: true}}`, nil error; `advance` records it and moves `next_fire_at`. No per-issue skip is fabricated, so `Matched == len(Started) + len(Skips)` still holds (`Matched` is computed from both lists).
- **Not blocked**: started cap = `min(max_issues, room)` (NULL `max_issues` becomes `room`); scan window = that cap + `backfillHeadroom`; `Capacity` carried with `Blocked: false`.
- **Count error**: transient; return the error, do not advance (existing transient path).
- **Label removal (M3)**: after `createIssueRun` returns a started run and the row's flag is on, remove the single selector label through `forgesvc.Service.SetIssueLabel(ctx, f, projectID, cachedIssue, label, "", false)` (`api/internal/forgesvc/service.go`): forge first, then the cached `issues` row, so the next fire's `ListSweepCandidateIssues` no longer sees it. Widen the scheduler's forge dependency with a narrow interface for this method (`*forgesvc.Service` already backs `ForgeBuilder`). At fire time the label is dropped from the remove set if it equals the live configured `uzi` label (an admin setting that can change after create); then nothing is removed and the started entry records `label_removed: false` without a failure. On a removal error the run stands, the started entry records `label_remove_failed: true`, and the error is logged with the schedule id.
- **Bounds**: gate inputs are owner integers bounded by CHECK and handler validation (1..50); the count is one indexed query per gated fire; label writes per fire are at most the number of runs started, which is the existing started cap (`max_issues`, bounded by `MaxSweepIssues`, or unlimited on an ungated sweep exactly as today's fan-out). Every forge write targets the schedule owner's own repo through the owner's connection, as every sweep write does.

### Outcome, last-fire and run-now contracts

- `FireOutcome` gains `Capacity *CapacityCheck`; `Started` gains `LabelRemoved bool` and `LabelRemoveFailed bool` (M3).
- Persisted `last_fire` (`api/internal/schedsvc/last_fire.go`): `capacity` (omitempty `{in_flight, limit, room_needed, room, blocked}`), `started[].label_removed`, `started[].label_remove_failed` (omitempty). Older rows render exactly as today.
- Run now: `handler.runNowResponse` (`api/internal/handler/schedules.go`) maps `FireOutcome` into `apitypes.RunNowResponse` separately and persists nothing; it gains the same `capacity` and started fields.
- Mirrors: `apitypes` (`schedule.go`: `LastFire`, `RunNowResponse`, schedule DTO, `CatalogEntryDTO`), web `web/src/lib/apiTypes.ts` (`LastFire`, `LastFireStarted`, run-now and catalog types), and the skip label maps `web/src/lib/scheduleSkipReasons.ts` and `api/cmd/uzi/schedule_render.go` for `config_not_supported`.

### API

- Schedule DTO and create/patch requests gain `capacity_limit`, `capacity_room_needed` and (M3) `remove_label_on_dispatch`.
- PATCH semantics: an omitted field preserves the stored value (a retime must not drop the gate); an explicit clear of BOTH capacity fields removes the gate; one cleared and one set is a 400; the removal flag preserves on omission. Do NOT copy `max_issues`: `mergeSchedule` assigns `m.MaxIssues = req.MaxIssues` directly, so an omitted `max_issues` clears it, which would drop the gate on an unrelated edit. Use presence-aware decoding like `OptionalHarness` / `OptionalCredentialOverride` (`api/internal/apitypes/schedule.go`): seed from the stored row and replace only when the field is present. Pin omission versus clear with tests on both the custom and the default-row PATCH paths.
- Validation (`api/internal/handler/schedules_request.go`), all 400 to match the sibling range checks: gate or flag on a non-sweep target, on `timing=once` (a blocked once-schedule would be consumed by `advance`), or on a resolved `assigned` selector; half-set gate; `K > C`; values outside 1..50; removal flag with an empty, multi-label or `uzi`-containing resolved selector. Default rows validate against the catalog-resolved selector kind and labels, not the row's NULL `labels`.
- Default-origin rows: add the new fields to `patchDefaultScheduleConfig`'s editable set and to `defaultEditableDiverges` (`api/internal/handler/schedules_catalog.go`) so an owner edit marks the row customized; persist catalog values at enable as `max_issues` is (`catalogMaxIssues`); restore them on `schedule reset`; copy them in `api/internal/handler/schedules_clone.go`.

### Catalog (M4)

- `api/internal/schedtmpl` parses frontmatter keys `capacity_limit`, `capacity_room_needed`, `remove_label_on_dispatch`, and rejects at parse time a gate or removal on a non-sweep or `assigned`-selector entry, and removal on an entry without exactly one label (a gate on a multi-label entry is allowed). `CatalogEntryDTO` exposes them.
- New entry `api/internal/schedtmpl/catalog/ondeck-sweep.md`: `target: sweep`, `name: On-deck sweep`, `labels: on-deck`, `cron: */10 * * * *`, `timezone: UTC`, `max_issues: 1`, `capacity_limit: 4`, `capacity_room_needed: 2`, `remove_label_on_dispatch: true`, a short body like `planned-sweep.md`. The enable path's advisory missing-label guardrail covers `on-deck`.

### CLI (`api/cmd/uzi/schedule*.go`, `api/internal/uzicli`)

- `schedule create|edit`: `--capacity-limit <n> --room-needed <k>` (together), `--clear-capacity` on edit, (M3) `--remove-label-on-dispatch[=false]`.
- `schedule get`: a `WHEN TO SEND` row (`limit 4 · room 2` or `off`), the last fire's capacity line, label removal per started run. `run-now`: the blocked line from example 4.
- Help and render text say "issues to send at a time" for `--max-issues`. Update the embedded skill source `api/internal/uzicli/skill/SKILL.md`.

### Web (M2, M3, M4)

- `web/src/components/ScheduleModal.tsx`: the "When to send issues" fieldset per mock frame 1: a toggle, then one sentence with three inline number inputs (C, K, N). While the gate is on, the standalone N field is hidden and the sentence's N input edits the same `max_issues` value; while off, the standalone field shows, labelled "Issues to send at a time". The helper line names the schedule's actual cadence ("Checked every 10 minutes", "Checked daily at 02:00").
- Removal toggle (M3), hidden for an `assigned` selector and disabled with its reason for an empty, multi-label or `uzi` selector.
- `ScheduleListRow.tsx` pills and `LastRun.tsx` "Waiting for room", started-run "label removed" and "label could not be removed" per mock frame 2; mock-mode fixtures in `web/src/mocks/mockApi/schedules.ts`.
- Cadence (M4): a new "Every N minutes" preset in `web/src/lib/schedulePresets.ts` limited to divisors of 60 (`1,2,3,4,5,6,10,12,15,20,30`), producing `*/N * * * *` and round-tripping through `presetFromCron`; any other minute step stays a custom cron. Extend the Go twin `api/internal/schedsvc/presets.go` in step, with a parity test.
- Default jobs card per mock frame 3 (M4).

### Docs

- `docs/scheduling.md`: a "When to send issues" section (M1), label removal (M3), the `ondeck-sweep` row in Default jobs and the minutes preset (M4). Every milestone that edits `docs/*.md` runs `task docs:sync` and commits the mirror, because `TestEmbeddedDocsMatchSource` in `test:api` asserts byte equality.

## Testing decisions

- **Live-DB tests are mandatory evidence.** `task gate:api` skips live-DB tests without a database, so each Go milestone also runs `./e2e/run-store-it.sh` (or `go test` with `UZI_TEST_DATABASE_URL` pointing at a throwaway Postgres named outside the `uzi-` namespace) and the PR shows the named tests ran, not skipped.
- **Scheduler gate** (live-DB, beside `credential_disabled_fire_livedb_test.go`): seed runs across two repos, every origin, kinds (issue, chat, judge, job), and statuses (queued, running, awaiting_approval, limit_wait, paused, completed). Cases: blocked (a counting fake forge asserts zero `ListIssues`/`GetIssue` calls and no run created; `matched 0`; schedule advanced); partial batch `min(N, room)`; over-limit WIP clamps room to 0; chat and judge excluded, job counted; terminal excluded; Run now blocked; a gated multi-label sweep accepted and gated normally; `config_not_supported` for a gated row resolving to `assigned`, and for a removal-enabled default whose catalog labels became multi-label (no run created).
- **Label removal** (live-DB plus a `forgetest.BaseFake` overriding `UpdateIssueLabels`): removed on forge and in cache; not called with the flag off; forge failure keeps the run and sets `label_remove_failed`; never removes the live `uzi` label even after the admin setting changes; a run that fails after successful removal is not re-picked on the next fire.
- **Mutations to watch fail** (per `.claude/rules/go.md`): drop the gate; use `N` instead of `min(N, room)`; drop the kind exclusion; skip the cache write in removal; drop the fire-time `uzi` filter.
- **Handler**: table tests pinning 400 for each refusal in example 6; PATCH omission preserves, both-null clears, half-null refuses; default-row edit validates against catalog labels and flips `customized`; reset restores; clone copies; `runNowResponse` carries `capacity`.
- **Catalog**: `schedtmpl_test.go` parses the new keys, refuses invalid combinations, loads `ondeck-sweep`; catalog enable persists the three fields.
- **CLI**: `schedule_render_test.go` and `schedule_test.go` for flags, `WHEN TO SEND`, the blocked run-now line and the renamed help text.
- **Web**: vitest for `ScheduleModal` (sentence inputs, shared N, toggle disabled states, request body, cadence-aware helper), `schedulePresets` round-trip for each divisor and refusal of `*/40`, `LastRun` states.
- **Mock parity**: screenshots of the modal, list row, last-fire detail and catalog card beside the mock frames, in the PR.
- **Gates per milestone**: `task gate:api` + live-DB run (Go milestones), `task gate:web` (web milestones), `task deadcode` (new exported Go and web types are knip/deadcode surface), `task check-docs:web`, `task gate:repo` (migration numbering and additivity).

## Milestones

### M1: Capacity gate for label sweeps through the API and CLI

Migration (capacity columns), reuse of the in-flight count, the `fireSweep` gate, `config_not_supported`, `capacity` in `FireOutcome`/`last_fire`/run-now, DTO/PATCH/validation including default rows, reset and clone, CLI flags and render, docs section plus `docs:sync`.

- Blocked by: none
- Acceptance: examples 1-4 and the gate parts of 6 pass in tests; an ungated schedule behaves byte-for-byte as before (existing schedsvc and handler tests unchanged and green); live-DB evidence in the PR.

### M2: "When to send issues" in the web UI, and the field rename

Web types, the modal fieldset with the shared N input and cadence-aware helper, list-row pills and "Waiting for room" in the last-fire view, mock fixtures; "Max issues per run" renamed "Issues to send at a time" in both modal sites.

- Blocked by: M1
- Acceptance: vitest cases above; mock parity for the gate parts of frames 1 and 2.

### M3: Remove the selector label on dispatch

Migration (`remove_label_on_dispatch`), removal through `forgesvc` (forge then cache), fire-time `uzi` filter, `label_removed`/`label_remove_failed`, validation, CLI flag and render, web toggle and last-fire states, docs plus `docs:sync`.

- Blocked by: M1, M2
- Acceptance: examples 5 and the removal parts of 6 pass with a custom single-label sweep; mock parity for the removal parts of frames 1 and 2.

### M4: `ondeck-sweep` default and the minutes cadence

Catalog keys and parse-time validation, `CatalogEntryDTO`, the `ondeck-sweep` entry, the "Every N minutes" preset (web and Go, divisors of 60), Default jobs card, docs plus `docs:sync`.

- Blocked by: M1, M2, M3
- Acceptance: `uzi schedule catalog enable ondeck-sweep --repo <id>` creates a schedule with cron `*/10 * * * *`, `max_issues 1`, limit 4, room 2, removal on; example 5 passes against that row in a live-DB test; an owner edit of C on the enabled default succeeds and marks it customized; the card matches mock frame 3.

## Design mock contract

- `prds/mockups/2342-capacity-gated-sweeps-mock.html` is the presentation reference for layout, order, wording and colour roles. Implement it, not only this text.
- The "Preview only" strip under the sentence (busy/free/send boxes and the verdict line) is a mock-only aid for reviewers; do not implement it.
- Small deviations for consistency with the app (its tokens, components, spacing, the existing Schedules and Default jobs layouts) are expected. A larger deviation (a dropped element, different layout or information) only when necessary, listed in the PR body under `Mock deviations` with its reason; an absent list claims parity.
- This PRD's text wins where it specifies; the mock governs what the text leaves out.

## Decision Log

1. **Count the owner's unfinished runs, not worker slots.** A worker's `max_concurrent_runs` is self-reported and hosted burst workers make fleet capacity elastic (bounded per user by `UZI_EPHEMERAL_MAX_PER_USER`), so "free slots" would differ between compose and k8s. A per-owner limit is deterministic, testable and topology-independent. Rejected: fleet slot arithmetic; an "empty queue" check (no `K` control). (User decision with peer review, 2026-10-06.)
2. **Scope is user-wide across repos and run origins, excluding chat and judge, counting job runs.** The owner's token and attention are shared across repos; chat is interactive and judge is a background review, so counting them would stall the drain while the owner chats. Job runs use the owner's token and a worker slot, so they count (user decision). Parked and waiting states count because they are outstanding commitments. Reusing `CountInProgressRunsForUser` gives exactly this set, already pinned to `runkind`, and keeps the gate consistent with the Runs badge. Rejected: a new query (duplicates an existing pinned one); repo-wide or this-schedule-only counts.
3. **`K` is a trigger threshold, not a reserve; partial batches are allowed; the cap is advisory.** Start nothing when room < `K`, else `min(N, room)`. An exact cap would need a lock shared by every run-creation path (manual, autopilot, sweeps), out of proportion for a pacing control; the UI says "other activity can exceed this limit".
4. **Label removal is best effort, after run creation, forge then cache.** Remove-first risks dropping an issue when run creation then fails. Remove-after can leave the label if the forge write fails or the api stops between the two steps; the active-run dedup blocks a re-dispatch while that run is live, and a failed removal is shown on the fire. The user accepted this exception over a strict per-schedule dispatch ledger (more moving parts). The cache write is required: candidates come from the cached `issues` table, so a forge-only removal would re-pick a failed issue before the next sync.
5. **Removal only for a single-label, non-`uzi` selector.** The selector is AND containment, so removing every selector label from a multi-label sweep would strip unrelated labels; removing `uzi` would make the issue ineligible. The live `uzi` label is re-checked at fire time because it is an admin setting.
6. **A setting on the existing sweep target plus a catalog default, not a new target type.** The sweep already has the selector, oldest-first cap, backfill, eligibility gate, auto-approve, wait-on-limit and typed outcomes; only admission and label consumption are new.
7. **Blocked fires are a schedule-level outcome, not per-issue skips**, and are refused on one-time schedules. Fabricated skips would break `Matched == Started + Skips` and spend forge calls; a one-time schedule is consumed by any benign advance, so a blocked fire would end it.
8. **400 for validation errors**, matching the sibling range checks in `schedules_request.go`.
9. **Defaults `C=4, K=2, N=1`, every 10 minutes** (user decision). The scheduler wakes every minute (`UZI_SCHEDULER_CHECK_INTERVAL`, default 1m), so a 10-minute cron is honoured. The minutes preset is limited to divisors of 60 because `*/N` restarts each hour, so `*/40` would alternate 40- and 20-minute gaps.
10. **Wording** (peer-reviewed, user-chosen): "Use [C] unfinished runs as my limit. Wait until there's room for at least [K] more, then send up to [N] issue(s)." "Keep at most" was rejected because it promises a hard cap; "going" because it hides parked work. The user approved renaming "Max issues per run" to "Issues to send at a time", because each issue gets its own run; the API field stays `max_issues`.
11. **Names** (user decision): catalog slug `ondeck-sweep` (fits `*-sweep` and its label), label `on-deck`. Rejected: `backlog` (used loosely for "someday"), `queued` (collides with a run status).
