# PRD #2344: Remove the sweep label on dispatch, and the on-deck backlog drain

**Status**: Done — M1 and M2 implemented and verified (#2343 merged as `fc246e2d`)
**Issue**: [#2344](https://github.com/vtmocanu/uzi/issues/2344) (part of umbrella #2342)
**Priority**: Medium
**Design mock**: `prds/mockups/2342-capacity-gated-sweeps-mock.html` (shared with #2343; open it in a browser). This PRD implements the removal toggle and the label-removal last-fire states in frames 1 and 2, the minutes cadence in frame 1, and frame 3.

## Problem

A label sweep never consumes its selector label. A `Planned` issue whose run fails is picked again on the next fire, as the oldest candidate, every time; for a backlog drain that retries a broken issue forever. And there is no ready-made job for draining a triaged backlog: an owner has to assemble a sweep, a gate (#2343) and a frequent cadence by hand, and the cadence presets stop at "every N hours".

## Outcome

A single-label sweep can remove its selector label from an issue once that issue's run is created (best effort, Decision 1). A new catalog default, `ondeck-sweep`, drains `on-deck` issues: every 10 minutes, `N=1`, `C=4`, `K=2` (the #2343 gate), label removed on dispatch. The schedule modal gains an "Every N minutes" cadence preset.

Acceptance examples:

1. A sweep with `remove_label_on_dispatch` and selector `["on-deck"]` starts a run on an issue carrying `on-deck` and `uzi`; afterwards the forge and uzi's cached issue both lack `on-deck` and still carry `uzi`. If that run fails, the next fire does not pick the issue.
2. If the forge label write fails, the run stands and the fire records `label_remove_failed: true` for it; the UI shows "on-deck could not be removed (the run started; remove it by hand)".
3. Create or edit refuses (400) the removal flag on a non-sweep target, a one-time schedule, an `assigned` selector, or a resolved selector that is empty, has more than one label, or contains the configured `uzi` label.
4. `uzi schedule catalog enable ondeck-sweep --repo <id>` creates a schedule with cron `*/10 * * * *`, `max_issues 1`, limit 4, room 2, removal on; an owner edit of C succeeds and marks it customized; `schedule reset` restores the catalog values.
5. The modal offers "Every N minutes" for N in 1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30, producing `*/N * * * *`; `*/40 * * * *` stays a custom cron.

## Out of scope

- Strict once-only dispatch: no per-schedule dispatch ledger (Decision 1).
- Automatic retry of failed `on-deck` issues; the owner re-adds the label.
- Removing labels for multi-label or `uzi` selectors (Decision 2).
- Changing existing sweeps or catalog entries: existing schedules keep `remove_label_on_dispatch = false`.

## Modules and seams

### Store

- Migration, additive only, number assigned at landing (`task migration:renumber`): `run_schedules.remove_label_on_dispatch boolean NOT NULL DEFAULT false`.

### Scheduler (`api/internal/schedsvc/scheduler.go`)

- **Supported configuration**: the removal flag needs a recurring, label-selected sweep with exactly one effective selector label (after the catalog overlay).
- **Fail closed on effective config**: extend #2343's pre-list check: a removal-enabled row whose resolved selector kind is not `label`, or whose effective selector is not exactly one label (a catalog update after enable), starts nothing and records `config_not_supported` before listing candidates or creating any run.
- **Label removal**: after `createIssueRun` returns a started run and the flag is on, remove the single selector label through `forgesvc.Service.SetIssueLabel(ctx, f, projectID, cachedIssue, label, "", false)` (`api/internal/forgesvc/service.go`): forge first, then the cached `issues` row, so the next fire's `ListSweepCandidateIssues` no longer sees it. Widen the scheduler's forge dependency with a narrow interface for this method (`*forgesvc.Service` already backs `ForgeBuilder`). At fire time the label is dropped from the remove set if it equals the live configured `uzi` label (an admin setting that can change after create); then nothing is removed and the started entry records `label_removed: false` without a failure. On a removal error the run stands, the started entry records `label_remove_failed: true`, and the error is logged with the schedule id.
- **Bounds**: label writes per fire are at most the number of runs started, which is the existing started cap (`max_issues`, bounded by `MaxSweepIssues`, lowered further by a #2343 gate, or unlimited on an uncapped sweep exactly as today's fan-out). Every forge write targets the schedule owner's own repo through the owner's connection, as every sweep write does.

### Outcome, last-fire and run-now contracts

- `Started` gains `LabelRemoved bool`, `LabelRemoveFailed bool` and an optional `SelectorLabel string` fire-time snapshot; persisted `last_fire` gains `started[].label_removed`, `started[].label_remove_failed` and `started[].selector_label` (omitempty); `handler.runNowResponse` carries all three. Mirrors in `apitypes` (`schedule.go`) and `web/src/lib/apiTypes.ts` (`LastFireStarted`, run-now type). CLI and web history use the snapshot, with generic label wording for legacy records.

### API

- Schedule DTO and create/patch requests gain `remove_label_on_dispatch`, preserved on PATCH omission (presence-aware decoding as #2343 does for the gate).
- Validation (`api/internal/handler/schedules_request.go`), 400: the refusals in example 3. Default rows validate against the catalog-resolved selector kind and labels, not the row's NULL `labels`.
- Default-origin rows: add the flag to `patchDefaultScheduleConfig`'s editable set and `defaultEditableDiverges` (`api/internal/handler/schedules_catalog.go`); persist the catalog values for the gate and the flag at enable, as `max_issues` is (`catalogMaxIssues`); restore them on `schedule reset`; copy the flag in `api/internal/handler/schedules_clone.go`.

### Catalog

- `api/internal/schedtmpl` parses frontmatter keys `capacity_limit`, `capacity_room_needed`, `remove_label_on_dispatch`, and rejects at parse time a gate or removal on a non-sweep or `assigned`-selector entry, and removal on an entry without exactly one label (a gate on a multi-label entry is allowed). `CatalogEntryDTO` (`apitypes/schedule.go`) and its web mirror expose them.
- New entry `api/internal/schedtmpl/catalog/ondeck-sweep.md`: `target: sweep`, `name: On-deck sweep`, `labels: on-deck`, `cron: */10 * * * *`, `timezone: UTC`, `max_issues: 1`, `capacity_limit: 4`, `capacity_room_needed: 2`, `remove_label_on_dispatch: true`, a short body like `planned-sweep.md`. The enable path's advisory missing-label guardrail covers `on-deck`.

### CLI

- `schedule create|edit`: `--remove-label-on-dispatch[=false]`. `schedule get` and `run-now`: label removal per started run, and the failure line. Update `api/internal/uzicli/skill/SKILL.md`.

### Web

- `web/src/components/ScheduleModal.tsx`: the removal toggle per mock frame 1, hidden for an `assigned` selector and disabled with its reason for an empty, multi-label or `uzi` selector.
- `LastRun.tsx`: "label removed" and "label could not be removed" per mock frame 2; list-row pill "removes label".
- Cadence: a new "Every N minutes" preset in `web/src/lib/schedulePresets.ts` limited to divisors of 60 (`1,2,3,4,5,6,10,12,15,20,30`), producing `*/N * * * *` and round-tripping through `presetFromCron`; any other minute step stays a custom cron. Extend the Go twin `api/internal/schedsvc/presets.go` in step, with a parity test.
- Default jobs card per mock frame 3; mock-mode fixtures in `web/src/mocks/mockApi/schedules.ts`.

### Docs

- `docs/scheduling.md`: label removal, the `ondeck-sweep` row in Default jobs, the minutes preset. Each milestone that edits `docs/*.md` runs `task docs:sync` and commits the mirror (`TestEmbeddedDocsMatchSource`).

## Testing decisions

- **Live-DB evidence** as in #2343: `./e2e/run-store-it.sh` (or a throwaway `UZI_TEST_DATABASE_URL` outside the `uzi-` namespace), named tests shown as run, not skipped.
- **Label removal** (live-DB plus a `forgetest.BaseFake` overriding `UpdateIssueLabels`): removed on forge and in cache; not called with the flag off; forge failure keeps the run and sets `label_remove_failed`; never removes the live `uzi` label even after the admin setting changes; a run that fails after successful removal is not re-picked on the next fire; a removal-enabled default whose catalog labels became multi-label records `config_not_supported` with no run created.
- **Mutations to watch fail**: skip the cache write; drop the fire-time `uzi` filter; remove before creating the run.
- **Handler**: 400 for each refusal in example 3; PATCH omission preserves the flag; default-row edit validates against catalog labels and flips `customized`; enable persists and reset restores the gate and flag; clone copies.
- **Catalog**: `schedtmpl_test.go` parses the new keys, refuses invalid combinations, loads `ondeck-sweep`.
- **CLI**: `schedule_render_test.go` and `schedule_test.go` for the flag and render lines.
- **Web**: vitest for the toggle states, `schedulePresets` round-trip for each divisor and refusal of `*/40` (with the Go parity test), `LastRun` removal states.
- **Mock parity**: screenshots beside the removal parts of frames 1 and 2, the cadence control, and frame 3.
- **Gates**: `task gate:api` + live-DB run, `task gate:web`, `task deadcode`, `task check-docs:web`, `task gate:repo`.

## Milestones

### M1: Remove the selector label on dispatch

Migration, removal through `forgesvc` (forge then cache), fire-time `uzi` filter, the removal arm of `config_not_supported`, `label_removed`/`label_remove_failed`, validation, default-row edit/clone, CLI flag and render, web toggle and last-fire states, docs plus `docs:sync`.

- Dependency: #2343 completed in `fc246e2d`.
- [x] M1 implemented and verified: examples 1-3 pass with a custom single-label sweep; removal parts of mock frames 1 and 2 inspected in Chromium.
- Evidence: `task gate:api` and `TMPDIR="$PWD/.uzi/scratch" ./e2e/run-store-it.sh` passed (2,207 LiveDB tests, zero skips, including removal and handler cases). `task gate:web` passed (5,612 tests and eight browser tests); `task gate:repo`, `TestEmbeddedDocsMatchSource`, mock build and removal screenshot assertions passed. Required removal mutations failed their intended assertions. Backend, CLI and web commits received read-only reviews; mandatory findings were fixed and re-reviewed.
- Approved implementation details: the removal-only `RemoveCachedIssueLabel` query filters the current repo/IID-scoped cached row after forge success, preserving unrelated labels and metadata; apply/AutoMove retain their existing paths. Historical label snapshots avoid incorrect text after schedule or catalog edits. Later authoritative sync or same-label re-add remains outside the best-effort guarantee.

### M2: `ondeck-sweep` default and the minutes cadence

Catalog keys and parse-time validation, `CatalogEntryDTO`, enable/reset persistence of the gate and flag, the `ondeck-sweep` entry, the "Every N minutes" preset (web and Go), Default jobs card, docs plus `docs:sync`.

- Dependency: M1 completed.
- [x] M2: examples 4 and 5 pass; example 1 passes against an enabled `ondeck-sweep` row in a live-DB test; cadence control and card match the relevant mock parts.
- Evidence: `task gate:api` passed. The full `e2e/run-store-it.sh` run passed with 2,208 LiveDB tests and zero skips, including `TestOndeckCatalogEnableRunDrainLiveDB` for enable, drain, edits, exact restoration, reset, stored fire-time controls and repeat enable after a live eligibility-label change. The runner's initial database-readiness failure ran no tests; the retry used `UZI_STORE_IT_PG_WAIT_SECS=240`.
- Web evidence: the full `task gate:web` passed with 5,648 tests and eight browser tests; shared canonical fixtures prove Go/web minute-preset parity. `task deadcode`, `task gate:repo`, mock build and Chromium screenshot assertions passed. The initial global dead-code run skipped agent Knip; after `cd agent && npm ci`, `task deadcode:agent` executed and passed, closing that coverage gap. The final web gate used `VITEST_MAX_WORKERS=1` after route-import hook timeouts; tests and deadlines were unchanged. A scratch Chromium launcher used a relative scratch temporary path to avoid the Unix-socket path limit while keeping artifacts inside the worktree.
- Presentation: the On-deck catalog card, all eleven minute choices and `*/40` staying Custom were checked in the rendered mock app and inspected beside reference frames 1 and 3. Standard app cards and select controls preserve the reference behavior; no material mock deviation was necessary. Backend and web fixed commits received read-only review; the repeat-enable defect and the temporary docs-before-web mismatch were resolved and verified.

## Design mock contract

- `prds/mockups/2342-capacity-gated-sweeps-mock.html` is the presentation reference for the parts this PRD implements. Implement it, not only this text.
- Small deviations for consistency with the app are expected. A larger deviation only when necessary, listed in the PR body under `Mock deviations` with its reason; an absent list claims parity.
- This PRD's text wins where it specifies; the mock governs what the text leaves out.

## Decision Log

1. **Label removal is best effort, after run creation, forge then cache.** Remove-first risks dropping an issue when run creation then fails. Remove-after can leave the label if the forge write fails or the api stops between the two steps; the active-run dedup blocks a re-dispatch while that run is live, and a failed removal is shown on the fire. The user accepted this exception over a strict per-schedule dispatch ledger (more moving parts). The cache write is required: candidates come from the cached `issues` table, so a forge-only removal would re-pick a failed issue before the next sync.
2. **Removal only for a single-label, non-`uzi` selector.** The selector is AND containment, so removing every selector label from a multi-label sweep would strip unrelated labels; removing `uzi` would make the issue ineligible. The live `uzi` label is re-checked at fire time because it is an admin setting.
3. **Defaults `C=4, K=2, N=1`, every 10 minutes** (user decision). The scheduler wakes every minute (`UZI_SCHEDULER_CHECK_INTERVAL`, default 1m), so a 10-minute cron is honoured. The minutes preset is limited to divisors of 60 because `*/N` restarts each hour, so `*/40` would alternate 40- and 20-minute gaps.
4. **Names** (user decision): catalog slug `ondeck-sweep` (fits `*-sweep` and its label), label `on-deck`. Rejected: `backlog` (used loosely for "someday"), `queued` (collides with a run status).
5. **Split from umbrella #2342** (user decision with peer agreement, 2026-10-06): this PRD reuses the #2343 gate and keeps label-removal failure handling in its own review.
