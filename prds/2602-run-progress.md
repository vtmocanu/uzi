# PRD #2602: Run progress estimate (percent and state flags)

**Status**: Draft
**Priority**: Medium
**Umbrella**: #2601
**Design mock**: `prds/mockups/2602-run-progress-mock.html` (open in a browser; light and dark) and `prds/mockups/2602-run-progress-tui-mock.sh` (`bash prds/mockups/2602-run-progress-tui-mock.sh`; prefix `NO_COLOR=1` for the glyph fallback).

## Problem

A run owner looking at the board, the run page, the TUI or `uzi run get` sees the status, a milestone bar and the "now" line, but not how far along a run is, nor whether a run that looks busy is stuck or waiting on something. To answer "how far is it?" and "is it waiting on me or on another run?", the owner reads the transcript. On 2026-10-09 one run sat in `awaiting_input` on a question about issue #2512 while the run working #2512 was itself stalled; nothing on any surface connected the two.

## Outcome

Every non-terminal issue run that is executing against a non-empty frozen milestone list shows an approximate percentage. Every run whose percentage would mislead shows a state flag instead. The run page and TUI detail add the active milestone and the phase of the active role, and for a run waiting on a question, a live run of the same owner that the question mentions.

Acceptance examples:
1. A `running` issue run, health `ok`, 3 frozen milestones, 2 of them in `milestones_completed`: the web board cell and TUI `PROG` column show `70%`, and the run page, TUI detail and `uzi run get` show `≈70%`.
2. A run in `awaiting_input` whose open question text contains `#2512`, where the same owner has a different non-terminal run on the same repo that is either an issue run with `issue_iid = 2512` or an `mr_rework`/`ci_fix` run with `pipeline_ref = agent/issue-2512`: the board shows `waits on you`; the run page, TUI detail and `uzi run get` show `may be blocked by <that run's short id>`, linking to it.
3. A `running` run whose `health` is `stalled`: every surface shows `stalled` instead of a percentage, and shows the percentage again once health returns to `ok`.

## Out of scope

- **Time remaining or a finish time.** A backtest over 232 completed issue runs on the production replica on 2026-10-09 (runs with no parks or requeues; estimate taken at each milestone boundary) put the best estimator within 2x of the actual remaining time 59% of the time, and its 80% band was 0.11x to 2.08x. That is too wide to display. A later PRD may revisit it with a within-milestone signal; it must re-run the backtest first.
- **Per-milestone durations and "open for N minutes".** They need a structured milestone event in the worker's payload, a partial index and worker/api version-skew handling. Deferred to a later PRD.
- **Cross-run or cross-user statistics** ("typical minutes per milestone"): an aggregate over other runs, a trust boundary, for a hint that improved the percentage error by 2.7 points (16.4 to 13.7 mean absolute) in the same backtest.
- **The model-written "Now" summary**: follow-up PRD #2603, blocked by this one.
- A percentage for any run kind other than `issue` (chat, judge, task, prompt, self_improve, mr_rework, ci_fix, job, cross_check), even when it carries a frozen milestone list. Those kinds still get the state flags in rules 1 to 5.

## Modules and seams

### `api/internal/runprogress` (new, pure)

`Derive(in Input) apitypes.RunProgress`, no I/O.

Input: `kind`, `status`, `health`, `is_planning`, the frozen milestone ids (distinguishing a nil list from an empty one), `milestones_completed`, `milestones_in_progress`, and `current_activity.agent` (empty when absent).

Output, `apitypes.RunProgress`, carried on `RunDTO` as `progress` (`null` for terminal runs):

| field | type | meaning |
|---|---|---|
| `state` | closed enum `percent`, `waiting`, `parked`, `queued`, `stalled`, `planning`, `none` | what every surface renders |
| `pct` | `*int` | set only when `state = percent` |
| `milestone_done`, `milestone_total` | `int` | counts over the frozen list; a completed id outside the frozen list is ignored, exactly as `milestoneProgress` (`api/cmd/uzi/tui_detail_rail.go:379`) and `milestoneBadge` (`web/src/lib/runBadge.ts:721`) count today |
| `active_milestone_id` | `string` | the first id, in frozen order, that is in `milestones_in_progress` and not in `milestones_completed` (the same rule as `milestoneInProgress`, `api/cmd/uzi/tui_detail_rail.go`); empty when none |
| `phase` | closed enum `implement`, `review`, `validate`, or empty | set only where `current_activity` is set (see below) |

Rules, first match wins:
1. Terminal status (`completed`, `failed`, `cancelled`): `progress` is `null`.
2. `awaiting_approval`, `awaiting_input`, `awaiting_followup`: `waiting`.
3. `limit_wait`, `pool_wait`, `recovery_wait`, `paused`: `parked` (surfaces keep today's park wording beside it).
4. `queued`, `claimed`: `queued`.
5. `health` is `stalled` or `looping`: `stalled` (surfaces show `health_since`).
6. `is_planning`: `planning`.
7. `kind != issue`, or the frozen list is nil or empty: `none`.
8. Otherwise `percent`, with `pct = min(99, floor(11 + 89 * done / total))`. Planning gets 11% because 11% is the measured median share of a run from its start to its first milestone start (2026-10-09 backtest); finalize after the last milestone measured under one minute, so it gets no weight. The board bar draws `round(pct * 8 / 100)` filled cells of 8.

Phase maps `current_activity.agent` through closed sets, from the product builtins (`api/internal/agenttmpl/builtins/`) plus the dev-team roster: `reviewer`, `auditor`, `fact-checker`, `architect`, `web-ux`, `tui-ux`, `dba` give `review`; `tester` gives `validate`; any other non-empty value (`lead`, `coder`, `documenter`, `ux-designer`, a repo-supplied custom name, a Codex lane's `unknown`) gives `implement`. The role is untrusted, model-influenced text: it is only compared against these constants, never echoed. The phase is the current role's phase only, not a history: `current_activity` is the newest tool frame, so it changes as lanes alternate.

Tests: table tests in `runprogress_test.go`, one row per rule and per precedence collision (for example `awaiting_input` with health `stalled` gives `waiting`), a frozen nil list and a frozen empty list on an issue run, a non-issue kind with a frozen list, a completed id outside the frozen list, two in-progress milestones, an id both in progress and completed (not active), and an empty and an unknown role.

### Attachment

- `state`, `pct`, the counts and `active_milestone_id` are computed inside `runToDTO` (`api/internal/handler/runs_dto.go:208`), whose `store.Run` input already holds every field, so every response that carries a `RunDTO` carries them and no new query is added.
- `phase` is set at the three sites that set `CurrentActivity` today: `ListRuns` and `AdminListRuns` (`api/internal/handler/runs.go`) and `GetRun` (`api/internal/handler/runs_lifecycle.go`).
- A DTO change touches three places (`.claude/rules/go.md`, "DTO changes"): the Go struct in `api/internal/apitypes/run.go`, the recorded fixtures `fixtures/api-contract/run.*.json` and `run_list_item.*.json`, and `web/src/lib/apiTypes.ts`.

### Blocked-by hint (run detail only, M2)

`progress.maybe_blocked_by_run_id` on the `GetRun` response, only while `status = awaiting_input`:
1. Read the open question with `GetLatestRunQuestion` (`api/internal/store/queries/slack.sql:344`). Use it only when its `question_id` equals the run's `open_question_id` and no `run_user_inputs` answer for that question id exists yet (the owner may have answered while the status still reads `awaiting_input`, before the worker consumes it); otherwise there is no hint. Take the question text from its payload.
2. Extract at most 5 `#<digits>` references; drop values that are zero, longer than 9 digits, or equal to this run's own `issue_iid`. The text is untrusted: only these digit runs are used, never the text.
3. Query one non-terminal run where `user_id` = **this run's owner** (not the viewer: admins can view other users' runs), `repo_id` = this run's repo, `id <> this run`, and either (`kind = 'issue'` and `issue_iid` in the references) or (`kind in ('mr_rework', 'ci_fix')` and `pipeline_ref` in `agent/issue-<n>` for the references). Note: a live issue run's `branch` stays NULL until its terminal report, so `branch` is not a usable key. `ORDER BY created_at DESC LIMIT 1`.

Wording is "may be blocked by": a mention is a hint, not a recorded dependency.

Resource bounds: one indexed query per detail read, at most 5 parameters, `LIMIT 1`, only for runs in `awaiting_input`. The list endpoint never runs it.

Tests: handler tests for found, not found, a newest question whose id differs from `open_question_id` (absent), an answer submitted but not yet consumed (absent), a match owned by another user (absent), a match on another repo (absent), a question mentioning the run's own issue (absent), an `mr_rework` match by `pipeline_ref`, an over-long and a zero reference.

### Surfaces (render only)

- **Web**: the Progress cell in the run rows of `web/src/pages/Dashboard.tsx` and `web/src/pages/RunsList.tsx`; the progress card in `web/src/pages/RunView.tsx` under the budget facts.
- **TUI**: a `PROG` column after `MILES` in `boardRow` (`api/cmd/uzi/tui_board_rows.go`), shown when `boardShowMile` is; on narrow widths the bar drops before the percent. A `PROGRESS` block above the milestone list in the detail rail (`api/cmd/uzi/tui_detail_rail.go`). Every state has a text form, so the colorprofile downgrade keeps it. The active milestone's title goes through `m.renderer.Plain`; add any newly drawn untrusted field to `d7UntrustedFields` (`api/cmd/uzi/tui_d7_guard_test.go:93`) and extend the hostile-value render test in `tui_model_test.go`.
- **CLI**: a `PROGRESS` row in `uzi run get` (`api/cmd/uzi/run_render.go`), including the blocked-by hint from M2; `--json` carries `progress` verbatim; `--field progress` stays a usage error (non-scalar), as for `milestones`.

## Testing decisions

- Rules at the pure seam (`runprogress`), not through the UI.
- A parity test: for the same DTO, `progress.milestone_done` equals the TUI's `milestoneProgress` and the web's `milestoneBadge`, and `active_milestone_id` equals the TUI's `milestoneInProgress`, including a completed id outside the frozen list and an id both in progress and completed, so the surfaces cannot disagree.
- Web: component tests beside the existing Dashboard and RunView tests, one per state; follow `RunView.nowline.test.tsx` for the run page.
- TUI: `View()` substring assertions in `tui_model_test.go` / `tui_render_test.go` for each state, including the NO_COLOR profile; render the uxlab scenes and compare with the mock.
- CLI: a `run_render` test for the `PROGRESS` row.

## Design mock contract

The mock is the presentation reference: layout, columns, order, glyphs, colour roles and wording. Implement it, not only this text. Small deviations for consistency with the app (palette tokens, components, spacing, wording) are expected. A larger deviation (a dropped element, a different layout or different information) only when necessary, listed in the PR body under `Mock deviations` with its reason; an absent list claims parity. This PRD's text wins where it specifies; the mock governs what the text leaves out. Validation compares the result with the mock side by side (uxlab PNGs and web screenshots) and the PR carries that evidence. The dotted "Now" line (web section 2, TUI mock 4) belongs to PRD #2603 and is not built here.

## Milestones

### M1: Progress cell on the board, TUI board and CLI
Blocked by: none.
`runprogress.Derive` with its table tests; `RunDTO.progress` (struct, fixtures, `apiTypes.ts`); the web Progress cell in Dashboard and Runs list; the TUI `PROG` column with narrow-width shedding; the CLI `PROGRESS` row; the parity test; docs (`docs/run-activity.md` progress section, `docs/cli.md` row) followed by `task docs:sync`; a `CHANGELOG.md` line.
Acceptance: examples 1 and 3 on every listed surface; NO_COLOR keeps every state; board rendering matches web mock section 1 and TUI mock 1.

### M2: Run page card, TUI detail block and blocked-by hint
Blocked by: M1.
The blocked-by hint with its handler tests; the RunView progress card (percent, segmented plan bar, active milestone, current phase chip, blocked-by link); the TUI detail `PROGRESS` block and its variants; the hint in the CLI `PROGRESS` row; `docs/run-activity.md` and `docs/cli.md` updated, then `task docs:sync`.
Acceptance: example 2 on the run page, TUI detail and CLI; a match owned by another user, on another repo, or the run itself never shows; rendering matches web mock sections 2 and 3 and TUI mocks 2 and 3.

## Decision Log

| # | Decision | Reason | Rejected alternative |
|---|---|---|---|
| D1 | No time remaining or finish time | Backtest: 59% within 2x, 80% band 0.11x to 2.08x | A range (`25m–1h left`): too wide to be honest |
| D2 | Percent = 11 + 89 x done/total, capped at 99 | Measured planning share 11%, finalize under a minute; no history query | Blend with cross-run priors: 2.7 points better, needs a cross-user aggregate |
| D3 | Computed server-side, mostly inside `runToDTO` | One rule; every `RunDTO` response carries it; no new query | Client-side derivation in web and TUI (two implementations to keep in sync) |
| D4 | A flag replaces the percent for waiting, parked, queued and stalled runs | A number on a run that cannot progress misleads | Show both (clutters the cell, hides the actionable state) |
| D5 | Blocked-by on the detail path only, scoped to the run owner and repo, excluding the run itself, worded "may be" | Needs the question text and one lookup; the board already says `waits on you`; owner scope cannot reveal another user's run; a mention is not a dependency | Board-wide lookup (one query per row), viewer-scoped or any-owner match (leaks), "blocked by" wording (overclaims) |
| D6 | Phase is the current role's phase only, from closed sets | `current_activity` is the newest frame and alternates between lanes; a history needs data we do not keep; mapping to an enum keeps the untrusted role inert | A phase history with check marks (cannot be established from the newest frame) |
| D7 | Per-milestone durations deferred | Doing them right needs a worker payload change, an index and version-skew handling | Parsing the worker's display text (fragile, unindexed) |
| D8 | `health = looping` flags like `stalled` | A looping run is not making progress either | Showing a percent on a looping run |
