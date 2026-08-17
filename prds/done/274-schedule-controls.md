# PRD #274: Scheduled runs — safer defaults and controls

**GitLab Issue**: [#274](https://github.com/vtmocanu/uzi/-/issues/274)
**Status**: **Complete** (created 2026-08-09; architect-reviewed 2026-08-09 — blocker + should-fixes folded in; owner resolved both open decisions 2026-08-09: Decision 1 = **1a** (thread + default on), sweep default = **bounded 10**; implemented and landed 2026-08-09, all milestones M1–M5 done)
**Priority**: Medium
**Consolidates**: [#271](https://github.com/vtmocanu/uzi/-/issues/271) (wait-on-limit default), [#272](https://github.com/vtmocanu/uzi/-/issues/272) (sweep cap), [#273](https://github.com/vtmocanu/uzi/-/issues/273) (guiding prompt). Those three are closed in favour of this PRD; their full content is captured below.

## Problem

The schedules feature (PRD #241) shipped the time-driven run origin, but three gaps make unattended scheduled runs harder to trust than they should be:

1. **A fired run fails instead of parking on a usage limit.** `run_schedules.wait_on_limit` defaults `false` (`api/internal/store/migrations/00103_run_schedules.sql`), and — more importantly — for the common `auto_approve=true` schedule the column is not even consulted (see Decision 1). A schedule is unattended by definition (typically off-hours), so a run that dies on the Anthropic usage window is silently lost until a human notices and re-fires.

2. **A label sweep has no upper bound.** `fireSweep` (`api/internal/schedsvc/scheduler.go:253`) iterates **every** open issue matching the selector, and `ListSweepCandidateIssues` (`api/internal/store/queries/schedules.sql`) has no `LIMIT`. A repo with 100 `bug` issues attempts ~100 runs in one fire (skipping only issues that already have an active run). Downstream limits pace *execution*, but nothing bounds the *count*.

3. **No way to steer an issue/sweep run without editing every issue.** `--prompt` is a standalone, issue-less target; there is no field to attach owner guidance ("always add a failing test first", "keep the diff small, no new deps") to an issue-driven or sweep-driven run.

## Solution Overview

Three focused, additive changes, all inside the existing schedules stack (`schedsvc`, the `run_schedules` table, the schedule API/DTOs, the `uzi schedule` CLI, and the web `ScheduleModal`). **No new service, no new trust boundary, no new run kind.** A schedule still fires as its owner, on the owner's token and PAT, under the four main-protection guardrails.

1. **wait-on-limit takes effect for scheduled runs, defaulting on** (Decision 1 — an additive seam, not a default flip).
2. **Per-schedule sweep cap** — a new nullable `max_issues` bound on how many issues a single sweep fire fans out to, oldest-first (Decision 2).
3. **Optional guiding prompt** on issue and sweep targets — a new nullable `guidance` field, injected into the run instruction as a clearly delineated, owner-provided section, without affecting any eligibility gate (Decision 3).

Schema footprint: **one migration** (`00108`, draft) adding two nullable columns to `run_schedules` (`max_issues`, `guidance`), both pure `ADD COLUMN` with **no CHECK change**. Decision 1 needs **no migration** (it is a Go seam + create-time default).

## Design Decisions

### Decision 1 — wait-on-limit for scheduled runs (✅ RESOLVED 2026-08-09: option 1a; revisits PRD #241 Decision 2)

**Mechanism today (verified).** `createIssueRun` (`scheduler.go:342`) threads the schedule's `wait_on_limit` **only on the non-auto-approve path** (`CreateScheduledRun`, `scheduler.go:355`). For `auto_approve=true` schedules it calls `CreateAutopilotRun` (`scheduler.go:348`), which flows into `createRun(..., waitOnLimit=nil)` → `resolveWaitOnLimit` (`api/internal/workersvc/limitwait.go:609`) → the **owner's** `users.wait_on_limit`, which itself defaults **false** (`00091_run_limit_wait.sql:126`). The code comment at `scheduler.go:338-341` states ignoring the schedule's value on the autopilot path is intentional. The `prompt` target already honours the schedule's `wait_on_limit` directly (`CreatePromptRun` → `prompt.go:34`).

**Consequence.** Since schedules default `auto_approve=true` (PRD #241 Decision 4), the schedule's `wait_on_limit` toggle is **silently inert** for the most common scheduled run. This is why the earlier manual test (a `wait_on_limit=true` auto-approve sweep) was a no-op. Flipping the column default alone would not change behaviour.

**Options:**
- **1a (recommended): add an additive `CreateScheduledAutopilotRun` seam and default the schedule's `wait_on_limit` true.** A schedule is an explicit, persisted per-schedule intent; honouring it is what the user asked for. Scoped to *scheduled* runs only.
- **1b: change the owner-level `users.wait_on_limit` default to true.** No seam change, but it affects **all** autopilot runs (label-driven autopilot included), which is broader than the ask.
- **1c: default the schedule column true, leave threading as-is.** Honest but near-cosmetic; documents the limitation.

**Recommendation: 1a**, default on, overridable per schedule. **Owner chose 1a (2026-08-09).**

**How 1a is built (this is the blocker the architect flagged — do NOT mutate the shared seam):** `CreateAutopilotRun` is a **shared** seam with two independent interfaces + fakes: `schedsvc.RunCreator` (`scheduler.go:83`, fake `scheduler_test.go:115`) **and** `poller`'s own (`poller/autopilot.go:27`, fake `poller/autopilot_test.go:105`, live call `autopilot.go:210`). Adding a `waitOnLimit` param to `CreateAutopilotRun` would touch the poller path and break the "autopilot-from-label untouched" guarantee. Instead:
- Add a **new** method `CreateScheduledAutopilotRun(ctx, userID, repoID, issueIID, description, allowWithoutPRD, waitOnLimit *bool)` on `workersvc.Service`, delegating to `createRun(..., autoApprove=true, allowWithoutPRD, allowLinkWaiver=false, waitOnLimit, nil)`.
- Add it **only** to `schedsvc.RunCreator` (+ its fake, which must *capture* `waitOnLimit` — today the fake drops it).
- Route the `sched.AutoApprove` branch of `createIssueRun` to it with `&waitOnLimit`.
- Leave `poller`'s `CreateAutopilotRun`, its interface, its fake, and `autopilot.go:210` **untouched** — a *structural* guarantee, not a passed-nil convention.
- The create-time default flips in `applyCreateDefaults` (`schedules.go:362`); the create path always sends an explicit value, so **no migration** is needed for the default.

Files: `workersvc/service.go`, `schedsvc/scheduler.go` (interface + call site), `schedsvc/scheduler_test.go` (fake gains `waitOnLimit` capture), `api/cmd/uzi/schedule.go` (flag default), `web/src/components/ScheduleModal.tsx` (toggle default).

### Decision 2 — sweep cap storage, enforcement, and semantics

- New **nullable `max_issues` integer** column on `run_schedules` (NULL = unlimited, preserving current behaviour). Sweep target only.
- **Enforcement is a SQL `LIMIT`, not a Go counter.** `ListSweepCandidateIssues` gains `LIMIT sqlc.narg('max_issues')` — sqlc renders a NULL narg as `LIMIT NULL`, i.e. unlimited, giving NULL=unlimited for free. It already `ORDER BY forge_issue_iid ASC`, so `LIMIT N` yields deterministic **oldest-first** batches.
- **Test strategy (the scheduler unit tests drive a fake `Store` that runs no SQL, so a `LIMIT` cannot be unit-verified):**
  - Unit (fake): assert the scheduler threads `MaxIssues` into `ListSweepCandidateIssuesParams`.
  - **LiveDB test (mandatory):** assert the real query limits to N oldest, and that NULL returns all rows — per the repo rule that a new/edited query is unverified until a live-DB test executes it (doubly so for the `LIMIT sqlc.narg` type-inference idiom; see `.claude/rules/go.md`).
- **Semantics (deliberate, state it so it is not filed as a bug):** `fireSweep` checks `HasActiveRunForIssue` *before* `GetIssue` (`scheduler.go:271-282`), so created ≤ examined ≤ N ("at most N runs" holds, Success Criterion 2). Consequence: if the oldest N candidates are all still running from a prior fire, this fire creates zero runs and does not reach newer issues. Bounded and self-correcting on the next fire; the completed-but-still-labeled re-pick stays out of scope.
- **DTO tri-state:** `MaxIssues *int` in `ScheduleRequest` (not a plain `int` — a plain zero would be indistinguishable from "keep"). `mergeSchedule` branches on `!= nil` (present, even zero/empty = set/clear). Web/CLI send explicit `null` to clear to unlimited.
- **Validation** (`validateScheduleConfig`, `schedules.go:39`): reject non-positive `max_issues`; scope it to the `sweep` target (reject on issue/prompt, mirroring the CLI's "`--label` only valid with `--sweep`").
- CLI `--max-issues`; web numeric field for the sweep target; a `MAX_ISSUES` row in `renderScheduleDetail` (`schedule.go:418`).
- **Default for new sweeps (✅ RESOLVED 2026-08-09: bounded default 10):** new sweeps default `max_issues=10`, overridable and clearable to unlimited; existing sweeps stay unbounded (Decision 4). The server/CLI/web defaults must agree (or the modal shows "unlimited" while the server applies 10). **Owner chose bounded 10 (2026-08-09).**

### Decision 3 — guiding prompt storage and composition

- New **nullable `guidance` text** column on `run_schedules`, allowed on **all** targets. It is a **pure `ADD COLUMN guidance text`** — the `run_schedules_target_shape` CHECK references only `issue_iid` and `prompt` (`00103_run_schedules.sql:37-41`), so guidance is unconstrained by it and **the CHECK is not touched** (an earlier draft's "relax the CHECK" was wrong and is dropped — a needless DROP/ADD of that CHECK risks weakening the issue/prompt invariants).
- `createIssueRun` composes the run description as: the issue body (the task) followed by a clearly labelled **owner guidance** section. Framing must make the model treat guidance as *how*, not *what*; the issue body stays the task (and is untrusted forge content, whereas guidance is owner-authored). Guidance is **purely additive** and must not change any eligibility gate (`allowWithoutPRD`, active-run dedup) or the plan gate.
- **Composed-total cap (the architect's silent-skip hazard):** `createRun` rejects `len(description) > MaxIssueDescriptionBytes` with `ErrDescriptionTooLarge` (`service.go:3294`), which `createIssueRun` treats as a **benign skip that advances the schedule without firing** (`scheduler.go:361-367`). So appending guidance can push a previously-runnable issue over the cap and it then **silently stops running**. Cap the `guidance` field well below the description cap (a few KB), and on `body + guidance` over the cap, **truncate the guidance section** in the composition rather than skip the issue. A test must exercise a near-cap body + guidance.
- **DTO tri-state:** `Guidance *string` in `ScheduleRequest`; `mergeSchedule` branches on `!= nil` so a cleared web textarea (`""`) removes guidance instead of being read as "keep". CLI `--guidance` (distinct from the `--prompt` target selector); web optional textarea for issue/sweep targets; a `GUIDANCE` row in `renderScheduleDetail`.

### Decision 4 — existing schedules

Default changes (Decision 1's `wait_on_limit`, Decision 2's `max_issues`) apply at **create time only**. Existing `run_schedules` rows are not rewritten by a data migration.

## Milestones

- [x] **M1 — wait-on-limit for scheduled runs (Decision 1 = 1a, owner-approved).** Additive `CreateScheduledAutopilotRun` seam; route the auto-approve branch of `createIssueRun`; create-time default on; CLI flag + web toggle default on. **No migration.** Tests: an auto-approve scheduled run honours the schedule's `wait_on_limit`; default is on for a new schedule; existing rows unchanged; **a structural assertion that `poller`'s `CreateAutopilotRun` interface/fake/call site are untouched** (label-autopilot behaviour unchanged). Done (commit `5c29bbc5`): `workersvc.CreateScheduledAutopilotRun` delegates to `createRun(..., autoApprove=true, allowLinkWaiver=false, waitOnLimit, seed=nil)`; `createIssueRun` routes the auto-approve branch to it with `&sched.WaitOnLimit`; `applyCreateDefaults`/CLI `--wait-on-limit`/web toggle all default on; `schedsvc/guard_test.go` compile-asserts `*workersvc.Service` still satisfies `poller.RunStarter` (poller diff empty).
- [x] **M2 — sweep cap.** Shared migration `00109` (`max_issues`), `LIMIT sqlc.narg('max_issues')` in `ListSweepCandidateIssues`, scheduler threads the param, DTO `*int` + `mergeSchedule` + `validateScheduleConfig` (non-positive reject, sweep-only scope), CLI `--max-issues` + detail row, web field + `mockApi.ts` parity. Tests: **unit** (param threaded), **LiveDB** (real LIMIT limits to N oldest; NULL = all), validation rejects non-positive / non-sweep. Done (commits `484ae36d`, `3d2e2138`): `LIMIT sqlc.narg('max_issues')` (NULL=unlimited) over the existing `ORDER BY forge_issue_iid ASC`; new sweeps default 10 across server/CLI/web; `mergeSchedule` replace-semantics so an explicit `null` clears to unlimited and `onlyEnabled` keeps pause/resume from wiping it; `MaxSweepIssues=10000` ceiling added to block an int32-wrap into a negative `LIMIT` (review follow-up); `TestScheduleMaxIssuesRoundTripLiveDB` verified against Postgres 17.
- [x] **M3 — guiding prompt.** Shared migration `00109` (`guidance`, no CHECK change), `createIssueRun` composition with delineated owner section + guidance-cap truncation, DTO `*string` + `mergeSchedule` + length cap, CLI `--guidance` + detail row, web textarea + `mockApi.ts` parity. Tests: guidance appears delineated from the issue body; absent when unset; gates unaffected; **near-cap body + guidance truncates, does not skip**. Done (commit `cb9fa703`): `composeRunDescription` appends a fixed-header owner-guidance section after the issue body, truncating on a UTF-8 rune boundary to stay ≤ `MaxIssueDescriptionBytes` (empty guidance leaves the body byte-identical); `MaxGuidanceBytes=8*1024` cap, rejected on the `prompt` target, clearable; `TestComposeRunDescriptionTruncatesNearCap` + `TestScheduleGuidanceRoundTripLiveDB` verified.
- [x] **M4 — docs.** Update `docs/scheduling.md` and `docs/cli.md` for the new flags/fields/defaults, following existing patterns; web modal help text. Done: `docs/scheduling.md` gained a "Sweep cap" and a "Guidance" subsection plus a wait-on-limit-default paragraph; `docs/cli.md`'s `schedule create` synopsis and prose bullet document `--max-issues`, `--guidance`, and the `--wait-on-limit` default flip (including the CLI-has-no-`schedule update` caveat for clearing to unlimited); a `[Unreleased]` `CHANGELOG.md` entry was added (#274). Web modal help text for the new fields was already present in the shipped `ScheduleModal.tsx` change. `node web/scripts/check-docs.mjs` passes with no new ERRORs (pre-existing line-budget WARNs only).
- [x] **M5 — gate green.** `task gate:api` and `task gate:web` pass; the migration renumbered to the next free number above the live head at landing (draft `00108`; head is currently `00107`); all new tests pass under `-race`. Done: migration landed as `00109_schedule_controls.sql` (next free above the live head `00108_repo_claudemd_enabled.sql`). Post-merge renumber (2026-08-09): PRD #217 landed `00109_rate_limit_source_limit_report.sql` and `00110_run_limit_dead_secret.sql` concurrently, so this file collided at `00109` once both landed on main; renumbered to `00111_schedule_controls.sql` (next free above the head `00110`) to clear the goose `duplicate version 109` panic. The file is now `00111_*`. `task gate:web` is green (typecheck, check-docs, 1869 web tests). For api, every real gate signal is green — `fmt-check`, `vet`, `build`, `deadcode` (0 findings), `test:api -race`, and `golangci-lint --new-from-merge-base=<branch-base>` (0 issues on the diff); `sqlc generate` is idempotent (`git diff --exit-code` clean). The only residual red is `lint:api`'s whole-files ratchet re-flagging the pre-existing backlog in untouched files because this clone's `origin/main` is a frozen mirror far behind the branch base — an environmental artifact, not a regression of this change.

## Success Criteria

1. A new schedule created via CLI or web defaults to waiting on the usage limit and **the value takes effect for auto-approve runs** (per Decision 1); existing schedules and label-driven autopilot are unchanged.
2. A sweep with `max_issues=N` creates at most N runs per fire, oldest-first; a NULL/absent cap preserves today's unbounded behaviour (verified by a live-DB test).
3. An issue or sweep schedule can carry optional guidance that appears in the run instruction, clearly separated from the issue body, with no change to which issues are eligible to run; a near-cap body plus guidance truncates the guidance rather than silently skipping the issue.
4. Both new nullable fields are clearable from the web (pointer tri-state), and `POST /api/schedules/preview` is unaffected (timing-only; confirmed no change needed).
5. `task gate:api` and `task gate:web` are green.

## Risks / Mitigations

- **Decision 1 could break the poller path if built naively.** Mitigation: the additive `CreateScheduledAutopilotRun` seam (never touch the shared `CreateAutopilotRun`); a test asserts the poller interface/fake/call site are unchanged.
- **Guidance silently over-caps the description and stops an issue running.** Mitigation: cap the guidance field well below `MaxIssueDescriptionBytes` and truncate the composition rather than skip; a near-cap test.
- **Sweep `LIMIT sqlc.narg` type inference.** Mitigation: mandatory live-DB test for both the limited and NULL paths.
- **New nullable fields not clearable.** Mitigation: `*int`/`*string` tri-state, matching the existing `AutoApprove`/`WaitOnLimit` pointers.
- **Default flips (wait-on-limit, max_issues) are behaviour changes.** Mitigation: create-time only, per-schedule overridable, owner sign-off, server/CLI/web defaults kept in agreement, documented in M4.
- **Migration renumber collision** with parallel PRDs. Mitigation: assign at landing per convention (next free above `00107`).

## Parallelization note

M1, M2, M3 all touch the same files (`api/internal/schedsvc/scheduler.go`, `api/internal/store/queries/schedules.sql`, `api/internal/apitypes/schedule.go`, `api/internal/handler/schedules.go`, `api/cmd/uzi/schedule.go`, `web/src/components/ScheduleModal.tsx`, `web/src/lib/api.ts`, `web/src/mocks/mockApi.ts`). They are **not cleanly parallelizable** — separate agents would conflict. Implement as one coherent change (or strictly sequentially: M1 → M2 → M3 → M4 → M5).

## Out of scope

- Cross-night duplicate-MR / re-pick dedup for sweeps (a completed-but-still-labeled issue being re-picked). Related to the sweep cap but a separate mechanism; noted in #272, deferred.
- Guidance on the `prompt` target (it already carries its own prompt text).
- Backfilling existing schedules to the new defaults (Decision 4).
