# PRD #3876: Per-user Claude / Codex split in admin usage

**Status**: Done (2026-11-10, issue #3876). Child of epic #3873. Was blocked by #3881 (the by-user table the column lives in), which landed first.

**Design brief**: `prds/3873-overview-redesign.md` (data readiness, constraints, mock contract). This PRD wins where it is more specific.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html#factory`, the "Claude / Codex" column of the By user table. The brief's mock contract applies; the PR carries screenshots.

## Problem

Admins cannot see how each user's runs split between the Claude and Codex harnesses. Per-user usage is one aggregate today (`AdminUsageUser`).

## Outcome

- `GET /api/admin/usage` (`api/internal/handler/usage.go:169`) adds, per user, run counts by harness for both windows: `harness_runs` and `last7_harness_runs`, each `{ "claude": n, "codex": n }`. Additive and optional: an older server omits them.
- The split counts the same population as `run_count` / `last7_run_count`, so the two harness counts sum to Runs.
- `uzi admin usage` shows a Claude / Codex column in its table output and passes the fields through `--json`.
- The Overview By user table (#3881) gains the "Claude / Codex" column: a small two-part bar and "70 / 30" (share of runs). Hidden when the server does not send the fields.

Acceptance examples:

- A user with 7 Claude runs and 3 Codex runs in the last 7 days shows "70 / 30" in that window; the two counts sum to their Runs value.
- Against an older server without the fields, the column is absent and nothing else changes.
- `uzi admin usage --json` carries `harness_runs` per user.

## Out of scope

A cost split by harness (would need per-model cost attribution); a model-level split; non-admin views.

## Modules and seams

- **Store query**: extend `AdminUsagePerUser` (`-- name: AdminUsagePerUser` in `api/internal/store/queries/runtime.sql`) with counts per `runs.harness` (the column exists since migration 00226), for both windows, over the same row set as `run_count`. Regenerate with `sqlc generate`. Add the counts as `FILTER (WHERE r.harness = ...)` aggregates on the existing per-user scan; a new `GROUP BY` key would break one row per user. Bounds: admin-only endpoint, the same scan as today; no new unbounded work. Measure the query on a realistic row count before and after and put the numbers in the PR.
- **DTO**: the three-file edit `.claude/rules/web.md` describes: the Go struct, `fixtures/api-contract/admin_user_usage.{zero,full}.json` and `admin_usage.{zero,full}.json` (it embeds a user row) re-recorded from the contract test output (never hand-written), and `web/src/lib/apiTypes.ts`.
- **CLI**: `api/cmd/uzi/` admin usage table and JSON; update the admin usage section of `docs/cli.md` and run `task docs:sync`.
- **Mock mode**: `getAdminUsage` in `web/src/mocks/mockApi/runs.ts` returns the new fields.
- **Web**: one column in #3881's `ByUserTable`, hidden on absence.

## Testing decisions

- A live-DB store test that the harness counts sum to `run_count` for each user and window, including a user with only one harness.
- Handler and api-contract tests for the additive fields, including users with zero usage in the existing handler merge tests; a web test that the column hides on absence; a CLI test of the table and `--json` output.
- Screenshot of the Factory scope against the mock.

## Milestones

### M1: the split, end to end

**Blocked by**: none inside this PRD (the PRD is blocked by #3881).

Query, DTO and fixtures, CLI, the web column, tests.

Acceptance: counts sum to Runs per window; older-server behaviour unchanged; mock parity for the column.

**Delivered** (2026-11-10):
- `AdminUsagePerUser` (`api/internal/store/queries/runtime.sql`) adds `claude_run_count`, `codex_run_count`, `last7_claude_run_count` and `last7_codex_run_count` as `FILTER` aggregates over the same `count(t.run_id)` population as `run_count`. A live-DB test (`api/internal/store/admin_usage_harness_runs_livedb_test.go`) asserts that the counts sum to Runs per user and window, including single-harness users.
- `AdminUserUsageDTO` carries `harness_runs` / `last7_harness_runs` (`HarnessRunsDTO`, pointer with omitempty). The handler sets both on every row, `{0,0}` on zero-usage rows. The contract fixtures were re-recorded and `apiTypes.ts` updated.
- Query cost at 100k usage-bearing runs over 50 users: median 2986 ms before, 3174 ms after (+6%), with an identical plan. The time is dominated by the existing `run_usage_totals` view.
- `uzi admin usage` shows a `CLAUDE/CODEX` column (e.g. `7/3`) and `--json` passes the fields through. `docs/cli.md` is updated and synced.
- The Overview preview's By user table shows the "Claude / Codex" column (a two-part bar and "70 / 30"), hidden when any row lacks the selected window's split. The mock API returns the fields.

## Decision Log

- **Split runs, not cost** (brief D23): run counts are exact today; a cost split needs per-model attribution. Rejected: cost share by harness.
- **Same population as Runs**: the shares must sum to the Runs column beside them.
- **Additive, optional fields**: older web and CLI builds ignore them; a newer web build hides the column against an older server.
- **Pointer with omitempty** (`harness_runs,omitempty`): an older server's absence decodes to nil, so the CLI can tell "not sent" from 0/0, and `--json` re-emits it absent rather than `null`. This follows the existing optional-field pattern (`agent_source.go` `Counts`). Rejected: a plain struct, which reads as a fabricated 0/0 against an older server.
- **Hidden on absence, in both clients**: the column shows only when every row carries the selected window's split, the same rule as the PR counts. The CLI omits the column the same way.
- **On the web, "—" for a user with no runs in the window**, never "0 / 0" or an empty bar; the CLI prints the raw counts (`0/0`).
- **Rounding**: Claude's share is rounded and Codex takes the remainder, so the pair always sums to 100. A minority harness can therefore read 0 (1 Codex run of 201 shows "100 / 0"); the cell title keeps the exact counts.
- **Cross-check children count under the harness they ran on**: a cross-check child runs on the lead's opposite harness under the same user, and `AdminUsagePerUser` counts children, so a Claude-only lead can show Codex runs. The header tooltip says so.
- **CLI column order deviates from the web**: `CLAUDE/CODEX` sits after `RUNS` in the CLI but last on the web, because the CLI's COST cell carries a long disclosure string that would push a trailing column off-screen. Recorded in `api/cmd/uzi/admin_usage_test.go`.
- **Current Overview unchanged**: `PerUserUsageTable` (`web/src/components/UsageCards.tsx`) does not gain the column; #3877 decides whether it moves there or is cut.
- **Table floor**: with the column the By user table's minimum width is 780px, as in the mock (720px squeezed Cost at 400px); without it, 640px as before.
