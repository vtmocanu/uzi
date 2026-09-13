# PRD #1293: Failed-run rate on the dashboard (global and per user)

**Issue:** [#1293](https://github.com/vtmocanu/uzi/issues/1293)
**Status:** Draft (2026-09-12), queued for the nightly Planned sweep.
**Execution:** Commit & push + queue for the `Planned` sweep (`Planned` + `uzi` labels). API + web + CLI + specs. **No `.github/workflows/**` touched** in implementation or validation (`.claude/rules/prds.md`). No migration.
**Mock:** `prds/mockups/1293-failed-runs-mock.html` (open locally; the dark top bar toggles the two placements, **"Inside the usage cards" is the accepted one**; "Own cards" is kept only as the rejected alternative). Figures in the mock are examples, not measurements.

Use current `main` and a new working branch, never write to `main`.

## Problem

The dashboard tells you what the factory spends (tokens, cost, run counts, per user for admins) and nothing about how often it fails. The only way to gauge reliability today is to scroll `/runs` and count red `failed` pills by hand, per user is impossible without the admin runs list, and neither says *why* runs fail even though every failed run carries a typed `fail_origin`. A maintainer deciding whether the fleet is healthy, or whether one user's runs are timing out more than the rest, has no number to look at.

## Solution

Add a **failed-run rate** to the surfaces that already carry per-scope run figures, so it arrives with the same scopes (you / factory / per user), the same two windows (all-time / last 7 days), and no new endpoint:

- **Your usage** card (every user): a "Failed runs" block under the token figures.
- **Factory total** card (admin): the same block, factory-wide.
- **Per-user breakdown** table (admin): two new columns, **Failed** and **Fail rate**, placed **right after Runs**.
- `uzi admin usage` (CLI): the same figures on the factory line and in the table, mirroring the web column order.

The block shows the rate, the counts behind it, an outcome bar (completed / cancelled / plan rejected / failed), and the top causes by `fail_origin`, so a 10% reads as "pod evictions" or "agent timeouts" rather than a bare number.

## Resolved facts (read locally, no internet needed)

### Where usage is computed and served today

- **Queries** (`api/internal/store/queries/runtime.sql`): `SelfUsage` (`~:2891`), `AdminUsageTotals` (`~:2918`), `AdminUsagePerUser` (`~:2947`). All three read `run_usage_totals JOIN runs`, filter `r.kind <> 'chat'`, and window the 7-day figures on `runs.created_at`. `run_count` is the number of runs *carrying a usage row*, not the number of runs; the client reads `run_count == 0` as "nothing yet" and hides the card body rather than rendering zeros.
- **Handler / DTOs**: `api/internal/handler/usage.go` (`SelfUsage`, `AdminUsage`); `api/internal/apitypes/usage.go` (`UsageDTO`, `SelfUsageDTO{Lifetime, Last7Days, RunCount}`, `AdminUserUsageDTO{UserID, Email, Usage, RunCount}`, `AdminUsageDTO{Factory, Users, EarliestRun}`). The factory card reuses `SelfUsageDTO`.
- **Web**: types at `web/src/lib/apiTypes.ts:2379-2402` (`SelfUsage`, `AdminUsageUser`, `AdminUsage`); the cards in `web/src/components/UsageCards.tsx` (`YourUsageCard`, `FactoryTotalCard`, `PerUserUsageTable`, plus `tokenShares`, the largest-remainder Share% rounding); the only consumer is `web/src/pages/Dashboard.tsx` (`:119-120` fetches both, best-effort with `.catch(() => null)`, admin fetch gated on `user?.is_admin`; renders at `:344-349`).
- **Mock/demo build** (`docs/dev-conventions.md#the-mockdemo-build`): static figures in `web/src/mocks/mockApi/runs.ts:211-233` (`getUsage`, `getAdminUsage`, four example users). Mock mode is the offline browser-verification surface for this PRD.
- **CLI**: `uzi admin usage` (`api/cmd/uzi/admin.go:126`), `renderAdminUsage` (`:439`) prints one factory line (`input= cache_read= cache_creation= output= cost= (runs=)`) then a table with columns `EMAIL INPUT OUTPUT COST RUNS`. `--json` passes `AdminUsageDTO` through untouched. There is no self-scoped `uzi usage` command; nothing to add there.
- **Tests**: `api/internal/handler/usage_test.go` (a fake `runsStore` carrying a `selfUsage` row; `TestSelfUsageReturnsScopedTotals`, `TestAdminUsageShapesFactoryAndUsers`, the two auth tests); `api/internal/store/run_usage_integration_test.go` (`TestUsageRollupsLiveDB`, the live-DB rollup test; `.claude/rules/go.md` for the `PASS=0` discipline); `web/src/components/UsageCards.test.tsx` (the `bundle()` fixture helper, 8 cases incl. the Share% sum-to-100 case).

### Failure vocabulary (the numerator)

- Terminal `runs.status` values: `completed`, `failed`, `cancelled` (the current `runs_status_check`, last re-stated in migration `00206`, which itself added the non-terminal `recovery_wait`). Everything else is in flight and must never enter the denominator.
- A **rejected plan is a failed run**: the reject path writes `status = 'failed'`, `stop_kind = 'plan_rejected'`, `fail_origin = 'plan_rejected'` in one statement (`runtime.sql:~2370`; `workersvc/submit.go:754`).
- `fail_origin` is a closed 12-value vocabulary, server-coerced (`api/internal/workersvc/failorigin.go`; CHECK last widened in migration `00186`): `provisioning_failed`, `credential_unavailable`, `guardrail_blocked`, `rate_limited`, `run_timeout`, `worker_lost`, `agent_failure`, `plan_rejected`, `auto_stopped`, `workflow_scope_missing`, `finalize_base_align_conflict`, `push_secret_blocked`. The column is nullable with no default (`00126`); rows failed before it existed carry NULL. `TestFailOriginVocabularyMatchesCheck` pins the Go list to the migration.
- **Neither the web nor the CLI has a human-label map for `fail_origin` today.** The web only carries the raw string on the run DTO (`apiTypes.ts:2020`); nothing renders it as prose. This PRD adds one small map on each surface (M2, M3), with the vocabulary above as its keys.
- The judge doc (`docs/judge.md:155`) already names a few origins in prose; no other user doc does.

### Run kinds (which runs count)

- Eight kinds (`api/internal/runkind`): `issue`, `ci_fix`, `chat`, `judge`, `self_improve`, `prompt`, `task`, `mr_rework`.
- The Runs page and its nav badge exclude `chat` **and** `judge` (`runtime.sql:~331-337`, `CountInProgressRunsForUser`); the usage rollups exclude only `chat` (the fold never writes chat rows, `usage_fold.go:473`; judge runs do spend the user's token and are summed).
- **Decision (D2):** the failure figures exclude `chat` and `judge`, matching what `/runs` lists, because a chat or judge failure is not a factory failure. The token sums are left exactly as they are. The two populations therefore differ, and the copy must never imply the usage `run_count` and the outcome `finished` count are the same number (they are shown as separate numbers, never as a fraction of each other).

### Why the counts cannot ride the usage join

- `SelfUsage` / `AdminUsageTotals` / `AdminUsagePerUser` start from `run_usage_totals`. A run that fails at provisioning, credential lookup, or guardrail has **no usage row**, so a failure rate computed over that join would systematically hide infra failures, the class the number exists to surface. The outcome counts are a separate aggregate over `runs` (D1) and the handler merges the two per scope.
- `AdminUsagePerUser` yields one row per user *with usage*. A user whose every run died before spending (possible on a fresh install with a broken token) has failures but no usage row. The merged per-user table row is keyed by user id across both aggregates: a user present in the outcome aggregate but absent from usage gets a row with zero usage and `Share 0%` (D5). This keeps the "per-user rows sum to factory lifetime" invariant `TestAdminUsageShapesFactoryAndUsers` asserts (zero adds nothing).
- Indexes that already serve the scan: `idx_runs_user (user_id)` and `idx_runs_claimable (user_id, status, created_at)` (`00020`). No new index; the table is laptop-scale and the usage rollups already scan `run_usage_totals` on every dashboard load.

### Windowing

- The usage 7-day figures window on `runs.created_at`. `runs.finished_at` exists (`00020`) and would be the more literal choice for "failed in the last 7 days".
- **Decision (D3):** window the outcome counts on `created_at` too, so the 7-day usage figure and the 7-day failure figure on the same line describe the same set of runs. A run created 8 days ago that failed yesterday is in neither. Logged as a decision, not a fact; revisit only if both windows move together.

### Specs and docs

- `specs/ai.md` head section is **633** on `main` and on every sibling worktree (checked 2026-09-12); this PRD uses **634**, written as the heading `## 634. PRD #1293 — Failed-run rate on the dashboard (global and per user)` (the file's heading form is `## NNN. PRD #…`; the `§` prefix is only for inline cross-references). Re-confirm at the landing rebase and bump if a parallel PRD landed first (append-only rule).
- `specs/human.md` gains a new `## Feature #1293` heading with the bullet in M4, text approved by the user in the PRD session (2026-09-12); the worker adds it verbatim, no wording changes.
- No `docs/*.md` page describes the dashboard usage cards today, so there is no user-doc page to extend for the web half. `docs/cli.md` is the CLI reference: the worker checks whether it lists the `admin usage` columns and updates that line if it does (then `task docs:sync`, per `CLAUDE.md` Conventions).

### No workflow, migration, or forge impact

- No `.github/workflows/**` change in implementation or validation. No migration: the counts derive from existing `runs` columns (`status`, `fail_origin`, `kind`, `created_at`, `user_id`). No forge call, no worker/agent change, no wire change on the run DTOs.

## Design (accepted from the mock)

Placement A in `prds/mockups/1293-failed-runs-mock.html`, rendered inside each usage card under a divider:

- **Label** `FAILED RUNS` (the card's `SectionTitle` style), then the **rate** in the card's mono numeral style at one size below the token figure, then the counts sentence: `106 of 1024 finished runs · 7.9% (3 of 38) in the last 7 days`. Rate always one decimal.
- **Outcome bar**: one 8px stacked bar of finished runs in fixed order completed → cancelled → plan rejected → failed, 2px surface gaps, colours from the theme tokens: `ok` for completed, `edge-strong` for cancelled, a 45° hatch of `edge-strong` over `surface` for plan rejected (distinguishable without colour), `danger` for failed. A legend line with the four labels and counts follows it; the bar carries an `aria-label` sentence with the same four counts. Text takes text tokens, never a series colour.
- **Top causes**: one line, up to four `fail_origin` buckets by descending count with human labels (`agent failure 48 · run timeout 31 · worker lost 14 · rate limited 9`); NULL origin renders as `unknown`. Ties break on the vocabulary order in `failorigin.go`.
- **Per-user table column order** (user decision, 2026-09-12, binding): **User · Runs · Failed · Fail rate · Tokens · Out · Cost · Share**. The `uzi total` row carries the factory Failed and Fail rate. Failed and Fail rate are lifetime figures (the table is lifetime-only today).
- **No small-sample threshold** (user decision, 2026-09-12): an earlier draft marked a per-user rate over fewer than 30 finished runs with a `†`; dropped, because the counts are already beside the rate (the cards say "N of M finished runs", the table has Failed next to Runs) and any cut-off is arbitrary. Instead the table's Fail rate cell carries a `title` with the exact fraction (`106 of 1024 finished runs`), so the denominator is one hover away.
- **Nothing yet**: a scope with zero finished runs hides the whole block and renders `—` in the table's Fail rate cell. Never a fabricated `0%` (the `run_count == 0` precedent, PRD #40 Decision 8).
- **Light and dark**: token-driven, no literal colours; verified in both polarities in mock mode.

## API contract

`UsageDTO` is unchanged. Two DTO additions, both always present (zeros, never null):

```jsonc
// RunOutcomesDTO
{
  "finished": 1024,        // = completed + cancelled + plan_rejected + failed, i.e. every row with
                           //   status IN ('completed','failed','cancelled'); chat/judge excluded
  "completed": 861,
  "cancelled": 45,
  "plan_rejected": 12,     // status=failed AND fail_origin='plan_rejected'
  "failed": 106,           // status=failed AND fail_origin IS DISTINCT FROM 'plan_rejected'
  "fail_origins": {        // counts over the `failed` rows only; NULL origin keyed "unknown"
    "agent_failure": 48, "run_timeout": 31, "worker_lost": 14, "rate_limited": 9, "unknown": 4
  }
}
```

- `SelfUsageDTO` gains `outcomes: {lifetime: RunOutcomesDTO, last_7_days: RunOutcomesDTO}` (so the factory card gets it for free, since it reuses the type).
- `AdminUserUsageDTO` gains `outcomes: RunOutcomesDTO` (lifetime only, matching the row's lifetime `usage`).
- Invariant, asserted in tests: `finished == completed + cancelled + plan_rejected + failed`, and `sum(fail_origins) == failed`.
- The **rate is client-computed** (`failed / finished`), never sent, so the two surfaces cannot disagree on rounding with the server. Both surfaces format it identically: one decimal, `—` when `finished == 0`.
- `fail_origins` keys are the `failorigin.go` vocabulary plus `unknown`; an unrecognised key from a future migration renders with its raw name rather than being dropped (forward-compatible label map).

## Milestones

- [ ] **1. API: outcome aggregates on the usage endpoints.** Three sqlc queries in `runtime.sql` next to the usage rollups: `SelfRunOutcomes` (per user, both windows), `AdminRunOutcomes` (factory, both windows), `AdminRunOutcomesPerUser` (lifetime, grouped by user, returning `(user_id, email, …)` via `JOIN users` exactly as `AdminUsagePerUser` does, so an outcome-only user's row has an email to render). Each counts over `runs` with `status IN ('completed','failed','cancelled') AND kind NOT IN ('chat','judge')`, windows on `created_at`, and splits `plan_rejected` out of `failed`; every computed column carries an explicit cast (`::bigint`), the file's convention. The per-origin counts come from a **second `:many` query per scope** returning `(user_id?, window, origin, count)` rows with `COALESCE(fail_origin,'unknown') AS origin`, folded into the map in Go: this is the default, because `runtime.sql` has no precedent for returning a jsonb aggregate to Go (jsonb reaches Go only as plain `[]byte` table columns) and an offline worker cannot iterate on sqlc typing. Only if the worker has a concrete reason to prefer `jsonb_object_agg` may it use `COALESCE(jsonb_object_agg(COALESCE(fail_origin,'unknown'), cnt), '{}')::jsonb` so sqlc types it `[]byte` and an empty group yields `{}`, with the reason in the query comment. Run the pinned `sqlc generate` (`.claude/rules/go.md`) after editing `runtime.sql`; the generated store code is committed. Add `RunOutcomesDTO` to `apitypes/usage.go`, thread it through `SelfUsageDTO` / `AdminUserUsageDTO`, and merge per-user rows by user id in `AdminUsage` (D5: a user with outcomes and no usage gets a zero-usage row, appended after the cost-sorted usage rows). Tests: extend the handler fakes and the four existing handler tests for the new fields and the merge; a live-DB query test beside `TestUsageRollupsLiveDB` seeding one run per terminal status, one `plan_rejected`, one NULL-origin failure, one `chat` and one `judge` failure, and asserting the exclusions and both invariants; a mutation check that flipping the `plan_rejected` split or the kind filter turns the test red (`.claude/rules/go.md`). Gate: `task gate:api`.

- [ ] **2. Web: the block, the columns, the label map.** `apiTypes.ts` gains `RunOutcomes` and the two field additions. `UsageCards.tsx`: a `FailedRunsBlock` rendered by `YourUsageCard` and `FactoryTotalCard` (rate, counts sentence, outcome bar with legend and `aria-label`, top causes), hidden when `finished == 0`; `PerUserUsageTable` gains **Failed** and **Fail rate** immediately after **Runs** in the order above, a `title="<failed> of <finished> finished runs"` on every Fail rate cell, `—` on zero, and the total row's factory figures. No small-sample tone, no threshold constant. A `failOriginLabel(origin)` map in `web/src/lib` with the 12 vocabulary entries plus `unknown`, falling back to the raw key. Mock API (`mocks/mockApi/runs.ts`) returns realistic `outcomes` for the caller and each of the four users so mock mode renders the block; the `data.realism` guard is extended only if it covers these figures. Tests in `UsageCards.test.tsx`: block renders rate/counts/legend/causes; hidden at zero; column order asserted by header text sequence; the Fail rate cell's `title` carries the fraction; the Share% sum-to-100 case still passes with a zero-usage row present. Gate: `task gate:web`. Browser pass in mock mode (`.claude/rules/web.md`): both cards and the table at desktop and 390px, lights on and lights off.

- [ ] **3. CLI: `uzi admin usage` mirrors the web.** `renderAdminUsage` factory line appends `finished=%d failed=%d fail_rate=%s` (one decimal, `-` at zero); table columns become `EMAIL RUNS FAILED FAIL% INPUT OUTPUT COST` (the web order, with the token columns the CLI already shows; `SHARE` is web-only today and stays so). A Go `failOriginLabel` map is not needed here because the CLI prints no causes line; keep it out (dead code gates at zero). `--json` carries the new fields untouched. A test in `api/cmd/uzi` asserting the header order and the zero-finished `-` cell. Gate: `task gate:api` (the CLI lives in the api module).

- [ ] **4. Specs and docs.** `specs/ai.md` heading `## 634. PRD #1293 — Failed-run rate on the dashboard (global and per user)` with the D1-D9 contract (terse, the PRD Decision Log carries rationale). `specs/human.md` new `## Feature #1293 — Failed-run rate on the dashboard (global and per user)` with, verbatim: `- The dashboard shows a failed-run percentage: global for admins, per user for everyone, and per user in the admin table. [user 2026-09-12]` and `- In the admin per-user table the Failed and Fail rate columns come right after Runs, and Cost sits last before Share. [user 2026-09-12]`. `docs/cli.md`: update the `admin usage` column list if it is documented there, then `task docs:sync` and commit the mirror (skip both if the page does not list the columns). Gate: `task check-docs:web`, `task gate:repo`.

- [ ] **5. Verification sweep.** `task gate:api`, `task gate:web`, `task gate:repo` green on the branch, each run once to a log (CLAUDE.md "Run economy"). `git diff --name-only <base>..HEAD` shows nothing under `.github/workflows/`. The mock-mode dashboard screenshot pair (lights on / off) attached to the MR. MR body cites `prds/1293-failed-run-rate-dashboard.md` and the mock.

## Success criteria

1. `GET /api/usage` and `GET /api/admin/usage` return `outcomes` for every scope, with the two invariants holding on real data (`finished` sums, `fail_origins` sums), and `chat` / `judge` runs never counted.
2. A run that failed before spending a token (provisioning, credential, guardrail) is counted as failed on the dashboard.
3. The dashboard shows the rate, counts, outcome bar and top causes in both usage cards, and the admin table shows Failed and Fail rate right after Runs, in both polarities and at 390px, with no fabricated `0%` anywhere.
4. `uzi admin usage` prints the same figures, in the web's column order, and `--json` carries them.
5. Existing usage tests, the Share% rounding, and the "nothing yet" states are unchanged in behaviour.

## Risks

- **Two populations on one card.** Usage `run_count` (runs with usage) and outcome `finished` (terminal runs) are different sets by design (D2, infra failures have no usage). Mitigation: they are never combined into one fraction, and the specs section states the difference so a later PRD does not "fix" one to match the other.
- **NULL `fail_origin` history.** Pre-`00126` failures bucket as `unknown`; on a long-lived install that bucket can top the causes line for a while. Acceptable: it is true, and it drains as history ages out of the 7-day window.
- **Per-user merge widens the row set** (D5): a zero-usage user now appears in the admin table. The `tokenShares` largest-remainder rounding must accept a zero total row; the existing sum-to-100 test guards it.
- **Aggregate shape vs sqlc.** The per-origin map is built in Go from a `:many (origin, count)` query by default (M1); `jsonb_object_agg` is allowed only with the explicit `::jsonb` cast and a recorded reason, since the file has no precedent for a jsonb aggregate as an output column.
- **Column reorder in the CLI** changes the text table for anyone scraping it; `--json` is the stable contract and is only widened.

## Scope and decision log

- **D1: counts come from `runs`, not `run_usage_totals`.** A usage-joined rate hides every failure that never spent a token, which is the infra class the number exists to reveal.
- **D2: exclude `chat` and `judge`; keep token sums as they are.** Failure rate is a factory-work metric and follows the Runs page; usage is a spend metric and already counts judge tokens. The two counts are shown side by side, never as a ratio of each other.
- **D3: window on `created_at`,** the same axis as the usage 7-day figures on the same line. Revisit only if both move together.
- **D4: `plan_rejected` is out of the numerator, in the denominator, with its own bar segment.** Rejecting a plan is the owner's decision, not a factory failure; hiding it entirely would understate how much work was thrown away, so it stays visible as its own outcome.
- **D5: merge per-user rows by user id, zero-filling usage.** Keeps the sum-to-factory invariant and stops a broken-token user from vanishing from the admin table. Outcome-only rows append after the cost-sorted usage rows, so the heaviest-first order the existing handler test asserts is untouched; the outcomes query joins `users` for the email.
- **D6: rate is client-computed, one decimal, `—` at zero.** Server sends integers only; both surfaces share one formatting rule so web and CLI cannot disagree.
- **D7: placement A (inside the usage cards), accepted from the mock.** Same scopes, same windows, no new row of cards for a figure glanced at rarely. Placement B (own cards) rejected.
- **D8: table order User · Runs · Failed · Fail rate · Tokens · Out · Cost · Share** (user decision 2026-09-12, in two steps: first "Cost last, before the Share percentage", then "Failed and Fail rate right after Runs"); the CLI mirrors it.
- **D9: no small-sample threshold** (user decision 2026-09-12). The draft's `†` marker below 30 finished runs was dropped: the denominator is already on screen next to the rate, a cut-off is arbitrary, and a hover `title` with the exact fraction on the table cell covers the "is this 3 of 22?" question without a rule.
- **Out of scope:** a per-repo rate, a time series / sparkline, a link from a cause to the filtered runs list, a Slack alert on a rising rate, exposing `fail_origins` per user in the table. Each is a follow-up issue if wanted.
