# PRD #3876: Per-user Claude / Codex split in admin usage

**Status**: Draft, parked (`Later`). Child of epic #3873. Blocked by: #3881 (the by-user table the column lives in).

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

- **Store query**: extend `AdminUsagePerUser` (`api/internal/store/queries/runtime.sql:8755`) with counts per `runs.harness` (the column exists since migration 00226), for both windows, over the same row set as `run_count`. Regenerate with `sqlc generate`. Add the counts as `FILTER (WHERE r.harness = ...)` aggregates on the existing per-user scan; a new `GROUP BY` key would break one row per user. Bounds: admin-only endpoint, the same scan as today; no new unbounded work. Measure the query on a realistic row count before and after and put the numbers in the PR.
- **DTO**: the three-file edit `.claude/rules/web.md` describes: the Go struct, `fixtures/api-contract/admin_user_usage.zero.json` and `admin_user_usage.full.json` re-recorded from the contract test output (never hand-written), and `web/src/lib/apiTypes.ts`.
- **CLI**: `api/cmd/uzi/` admin usage table and JSON; update the admin usage section of `docs/cli.md` and run `task docs:sync`.
- **Mock mode**: `getAdminUsage` in `web/src/mocks/mockApi/runs.ts:598` returns the new fields.
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

## Decision Log

- **Split runs, not cost** (brief D23): run counts are exact today; a cost split needs per-model attribution. Rejected: cost share by harness.
- **Same population as Runs**: the shares must sum to the Runs column beside them.
- **Additive, optional fields**: older web and CLI builds ignore them; a newer web build hides the column against an older server.
