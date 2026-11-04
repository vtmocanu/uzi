# PRD #3879: Overview preview, the issue-work line

**Status**: Draft, parked (`Later`). Child of epic #3873. Blocked by: #3878 (the preview page, its data hook and the Runs widget).

**Design brief**: `prds/3873-overview-redesign.md` (model-fidelity rules, data readiness, reuse map, constraints, mock contract). This PRD wins where it is more specific.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html`, the "Issue work" card. The brief's mock contract applies: implement the mock, small consistency tweaks are fine, larger deviations go under `Mock deviations` in the PR body with a reason, and the PR carries side-by-side screenshots.

## Problem

Nothing shows at a glance where the factory's issue runs stand between queue, plan, approval gate, build and PR. The owner reads run rows one by one to find out what is waiting and what is moving.

## Outcome

On `/overview/next`, an **Issue work** card below Needs your decision:

- **Stations** along a line: Queued, Planning, Plan review (drawn as a gate), Building, PR open. Each shows a count, one line of state ("Waiting on you", "Nothing waiting", "Implement, review, validate") and one small badge per run (the issue reference; the owner's email in the tooltip for another user's run, masked in demo mode).
- **Bays** under the line: Asking you, Parked (with whether it resumes on its own), Other work (jobs, prompts, tasks, self-improve, CI fixes and reworks; judge, chat and cross-check runs are not in the lists, brief Run populations).
- **Scope** as in #3878: own runs for a user, every user's active runs for an admin. PR open counts the viewer's own PRs (deduplicated by repository and PR number), not runs, for admins too, and never enters the active totals.
- **Whose wait**: for an admin, captions never imply the viewer can act on another user's run. The gate reads "Waiting on you" only when the viewer owns every counted plan, "Waiting on owners" when none, and "1 on you, 2 on others" when mixed; the Asking bay reads "Asking you" or "Questions" with the same split. Counts stay visible either way.
- **Filtering**: clicking a station or bay filters the Runs widget to it (both groups); clicking it again, or Show all, returns to all active runs. The pressed state is visible and announced (`aria-pressed`).
- Colours come from the run status tones (`RUN_STATUS_TONES`, `web/src/components/ui.tsx`): queued `queue`, planning `plan`, awaiting approval `warning`, running `info`, PR open `ok`. The flowing segments into stations with live work stop under `prefers-reduced-motion`. Under Shadow the card carries `data-live` (brief D22).

Acceptance examples:

- A run at `awaiting_approval` with `is_revising` counts under Planning, not at the gate, and the gate reads "Nothing waiting" if it was the only one.
- An admin's Queued station counts another user's queued run, whose badge tooltip names the owner; a non-admin's line never counts it.
- Clicking Plan review shows only plan-review runs in the Runs widget; clicking it again restores all active runs.

## Out of scope

Capacity and held work (#3880), usage (#3881), the command menu (#3875). No new run data: everything derives from the summary rows #3878 already loads.

## Modules and seams

- **`issueLineStage(run)`** (pure, `web/src/lib`): one of `queued`, `planning`, `review`, `building`, `asking`, `parked`, `other`, `pr` or `none`. Rules, in order:
  - terminal (`completed`, `failed`, `cancelled`): `pr` when #3878's `openPrs` includes it (the viewer's own completed issue run with an open PR); otherwise `none`. This comes first, so finished jobs, tasks and self-improve runs never inflate Other work.
  - `kind` other than `issue` → `other` (only the listed kinds reach here, brief Run populations; this includes `ci_fix` and `mr_rework`, brief D21).
  - an account cooldown (`isAccountCooldownWaiting`, `web/src/lib/runBadge.ts:533`) → `parked`. It comes before the rules below because a cooldown run can be `claimed`, `running` or `awaiting_approval`; today's Overview also counts it as waiting, not executing.
  - `queued`, `claimed` → `queued`.
  - effective planning (`is_planning`), or `awaiting_approval` with `is_revising` → `planning`.
  - `awaiting_approval` → `review`.
  - `awaiting_input`, `awaiting_followup` → `asking`.
  - `limit_wait`, `pool_wait`, `recovery_wait`, `paused` → `parked`. For every parked run, #3878's `parkDisposition` decides automatic or owner; no second rule lives here.
  - `running`, including an empty `progress.phase` or `progress.state` `stalled` → `building`.
  - Any non-terminal status this list does not name → `building`, so a run never drops off the line.
- **`IssueLine`** component and the bays; the selected stage is page state on the preview page, shared with the Runs widget.

## Testing decisions

- Unit tests for `issueLineStage`, one per rule above, including terminal runs of every kind (never Other work), the unknown-status fallback and the `ci_fix` / `mr_rework` placement; caption tests for own, others' and mixed waits.
- Component tests for counts, badges, the owner tooltip (asserting the `title` attribute, masked in demo mode), filtering and `aria-pressed`, in the style of `web/src/pages/Dashboard.test.tsx`.
- Extend #3878's mock-mode scenario with one run per stage; screenshots against the mock at desktop and 400 px (the line turns vertical), Ember and Dawn, plus one Shadow screenshot for `data-live`.

## Milestones

### M1: the line, its counts and bays

**Blocked by**: none inside this PRD (the PRD is blocked by #3878).

`issueLineStage`, the card with stations, bays, counts and badges, scope, tones, motion and the Shadow marker.

Acceptance: a test per stage rule; counts match the Runs widget for admin and non-admin; mock parity (screenshots).

### M2: filtering the Runs widget

**Blocked by**: M1.

Station and bay clicks filter both Runs groups; Show all; empty groups hide.

Acceptance: filter tests; keyboard operable; mock parity for the pressed state.

## Decision Log

- **ci_fix and mr_rework go to Other work** (brief D21): they maintain an existing PR rather than moving an issue through its lifecycle. Rejected: Building, which would count a PR's follow-up as new implementation.
- **PR open counts PRs from completed runs and stays out of active totals**: completed runs are not active work, and one PR can have several runs.
- **Unknown non-terminal statuses fall back to Building**: a new status must never hide a run. Rejected: dropping unknown runs.
- **Tones reuse `RUN_STATUS_TONES`**, so all five themes work without new tokens.
