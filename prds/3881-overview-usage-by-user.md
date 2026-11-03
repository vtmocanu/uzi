# PRD #3881: Overview preview, usage with a by-user table

**Status**: Draft, parked (`Later`). Child of epic #3873. Blocked by: #3878 (the preview page and its data hook).

**Design brief**: `prds/3873-overview-redesign.md` (model-fidelity rules, data readiness, reuse map, constraints, mock contract). This PRD wins where it is more specific.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html`, the "Usage" card (append `#factory` to open the Factory scope). The brief's mock contract applies: implement the mock, small consistency tweaks are fine, larger deviations go under `Mock deviations` in the PR body with a reason, and the PR carries side-by-side screenshots. The mock's Claude / Codex column is #3876, not this PRD.

## Problem

Today's usage card shows You and Factory side by side, doubling the load, and its admin per-user table carries more columns than an overview needs. Large API-equivalent dollar figures read like real spend.

## Outcome

On `/overview/next`, one **Usage** card replacing the usage card the preview carried over:

- **Scope toggle** You / Factory · all users (admins only) and the existing window toggle All time / Last 7 days, remembered in the existing `uzi.usageWindow` preference.
- **Figures**: Cost (API-equivalent) with today's incomplete-data disclosure; Pull requests opened with cost per PR ("at least" when cost is incomplete); Failed runs rate (failed of finished, with the change against all time); Tokens with in, cached and out.
- **Outcomes**: today's finished-run bar (completed, cancelled, rejected, failed), where rejected is plan rejections plus base divergence, as today; today's descriptive recoverable-work note, not a segment (issue #1418 removed the needs-landing hatch, `web/src/components/UsageCards.tsx:96`); "No finished runs in this period." when nothing finished; top failure causes; and "Latest failure (all time)" with the run link.
- **By user** (Factory scope, admins only): User, Runs, PRs, Failed, Fail rate, Last failure, Cost, sorted by cost, with a Factory total row, following the window toggle. Runs is `run_count` / `last7_run_count` (runs with recorded usage). The Factory row's lifetime Runs is the factory `run_count`; its 7-day Runs is the sum of the user rows' `last7_run_count`, because `SelfUsage` has no 7-day run count; Fail rate divides failed by finished runs; Last failure is lifetime-only (`last_failed_at`, `web/src/lib/apiTypes.ts:4306`), shown as "3h ago" in both windows, with a header tooltip saying so; today's table already shows the lifetime value in both windows under the title "Since last failure" (`UsageCards.tsx:370`), which this column renames. The viewer's own row is marked "(you)".
- No cost-over-time chart (brief D12).

Acceptance examples:

- An admin switches to Factory: the By user table appears and its rows sum to the Factory row for Runs, PRs, Failed and Cost; a non-admin sees no scope toggle and no table, and sends no admin request.
- In Last 7 days, the Last failure column shows the same values as in All time, and its tooltip says the value is all time.
- With incomplete cost data, the cost per PR reads "at least $X per PR", as today.

## Out of scope

The per-user Claude / Codex split (#3876). Today's Tokens, Out and Cost share columns are not on the preview; the current Overview keeps them until #3877 decides.

## Modules and seams

- **Shared usage presentation**: split `web/src/components/UsageCards.tsx` so both Overviews use the same pieces (`FailedRunsBlock`, `TopCauses`, `SinceLastFailedRun`, the window toggle, `aggregateDisclosure` from `web/src/lib/costStatus`). Rejected: copying them.
- **`ByUserTable`**: built from `PerUserUsageTable`'s row computation (`UsageCards.tsx:321`), with the trimmed columns. Reads `AdminUsage.users` (`AdminUsageUser`: `run_count` / `last7_run_count`, `outcomes` / `last7_outcomes`, `pr_count` / `last7_pr_count`, `usage` / `last_7_days`). An older API without the 7-day fields falls back as today's table does.
- Admin data only for admins: the hook from #3878 already gates `getAdminUsage`.

## Testing decisions

- Unit tests for the by-user rows: the sums equal the factory totals per window; the fail rate uses finished runs, not Runs; Last failure is the same in both windows.
- Component tests for the toggles, the admin gate, the "(you)" marker and the incomplete-cost disclosure, following `web/src/components/UsageCards.test.tsx` if present, else `Dashboard.test.tsx`.
- Screenshots against the mock (You and Factory, both windows, desktop and 400 px with the table scrolling inside its container, Ember and Dawn).

## Milestones

### M1: the recomposed usage card

**Blocked by**: none inside this PRD (the PRD is blocked by #3878).

Shared presentation pieces, the scope and window toggles, figures, outcomes, top causes and the latest failure; the current Overview keeps rendering identically through the shared pieces.

Acceptance: the current Overview's usage tests pass unchanged; mock parity for the You scope.

### M2: the by-user table

**Blocked by**: M1.

`ByUserTable` under the Factory scope with the trimmed columns, totals and the "(you)" marker.

Acceptance: sum and fail-rate tests; admin gate; mock parity for the Factory scope.

## Decision Log

- **A toggle, not side-by-side columns** (brief design, Usage): halves the load. Rejected: today's two columns.
- **Columns: Runs, PRs, Failed, Fail rate, Last failure, Cost** (maintainer calls D13 and later): volume, then quality, then spend.
- **No chart** (maintainer call D12), which also removes the only usage time-series dependency.
- **Share the presentation pieces with the current Overview** so the two cannot drift before #3877.
