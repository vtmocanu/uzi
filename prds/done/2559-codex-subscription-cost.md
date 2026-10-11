# PRD #2559: API-equivalent cost for Codex subscription runs

**Status**: Done. Merged in PR #2592 (`8f3416a5`, 2026-10-09). M1 was published under the owner's M1.c1 waiver for the local agent gate (three local runs each failed one different, unchanged recovery/cancellation test); the required agent CI then passed on the final PR head and on `main`. Child 1 of 2 under umbrella #2558 (Codex run cost). Independent of its sibling PRD #2560 (price-table freshness).

Planning facts were read at `main` `140b5c767` (after #2552, which rewrote the aggregate disclosure wording).

## Problem

Before this change, a Codex run on a ChatGPT subscription login showed no cost anywhere: the run page headline read "Subscription", table cells read `sub`, `uzi run get` printed `subscription`, the judge said "no per-token dollar figure applies", and the Self/Admin usage totals left these runs out with a "+ N Codex sub runs" disclosure. A Claude run on a subscription token, by contrast, showed a dollar figure on every surface: the Claude SDK prices token usage client-side from a price table bundled in the SDK, and uzi folds that `total_cost_usd` as `metered` (`deriveUsageCost`, `api/internal/workersvc/usage_fold.go`, Claude arm).

So an owner comparing harnesses saw Claude runs cost dollars and Codex runs cost nothing, and could not compare the two harnesses' spend on the same work.

## Outcome

A new Codex subscription run is priced exactly like a Codex API-key run: each upstream response is priced against uzi's pinned Standard table (`priceCodexResponse`, `agent/src/codex/codex-pricing.ts`), every existing fail-closed rule is kept, and each model entry is posted as `costStatus: "metered"` with its `costUSD`. The server already stores such an entry as `cost_status = 'metered'`, so the run renders on every surface that shows a metered cost, as a Claude subscription run does today. This applies to both producers of Codex model usage: lead runs (`codex-harness.ts`) and plan cross-check checker runs (`cross-check.ts`).

`metered` is redefined, in docs, comments and user-facing copy, as **a recorded API-equivalent cost** for both harnesses, not proof of a charge.

Rows stored with `cost_status = 'subscription'` (runs recorded before this change, and runs on workers not yet rolled to it) render as "no cost estimate recorded": tokens shown, no dollar figure, never `$0`. These rows have no estimate to add to dollar totals. Distinct priced rows on the same run retain their partial costs in aggregates even when the run rolls up as `unreported`; no per-run reader shows that partial as dollars. Aggregates disclose the incomplete total and count the run as unreported (maintainer decision (2026-10-09)). The figure compares work, not a bill; subscription users may consume purchased credits.

Acceptance examples:

1. A Codex subscription run, model `gpt-6.1-sol`, every response priced and reconciled. The posted `modelUsage["gpt-6.1-sol"]` has `costStatus: "metered"` and a numeric `costUSD`; `uzi run get --json` has `usage.cost_status == "metered"`; the run page headline is a dollar figure with an "API-equivalent" sub-label; the run counts in the Self usage dollar total and not in its disclosure.
2. The same run on a model absent from the table (for example `gpt-5.5`), or a `gpt-5.6-sol` run on or after its promotional review date (2026-11-21). `cost_status == "unreported"`, tokens retained, every surface reads "Unavailable"/`n/a`, never `$0`.
3. A Codex run stored with `cost_status = 'subscription'`. The run page reads "No estimate" with a sub-label "no cost estimate recorded"; table cells and the TUI read `n/a`; `uzi run get` prints `no estimate`; the judge line says no cost estimate was recorded; aggregates exclude it and disclose it as "+ N runs without a cost estimate".
4. A run whose earlier leg folded `subscription` (old worker) and later leg folded `metered` (new worker) under distinct session/epoch keys, even on the same model, rolls up as `unreported`. Tokens and the priced leg's partial cost remain stored; Self/Admin lifetime and recent aggregates include the partial and count the run as unreported. Per-run readers show unavailable cost without dollars. A status conflict on the same `(run_id, session_id, model, lineage_epoch)` key instead zeros that row alone; distinct priced rows remain intact (maintainer decision (2026-10-09)).
5. A Codex API-key run: pricing unchanged; only its sub-label wording changes.
6. A plan cross-check checker run on a Codex subscription login: its posted `modelUsage` entries are `metered` with `costUSD`, and the cross-check panel shows a dollar figure.

## Out of scope

- Claude cost computation or storage: Claude rows stay `metered` from the SDK figure.
- Recording the credential mode on Claude runs, or a charged-versus-API-equivalent split for either harness. Codex's mode stays recoverable from `runs.codex_auth_mode` (write-once, `00202_run_codex_binding.sql`).
- Backfilling older Codex subscription runs: per-response breakdowns were never stored, and the >272K tier applies per response, so stored per-model totals cannot reproduce the price.
- Adding rows for models the table does not price (the Codex catalog also lists `gpt-5.5`, `gpt-5.6-luna`, `gpt-5.6-terra`, `gpt-6-luna`); sibling PRD #2560's Health check reports their use.
- Price-table freshness and live pricing: sibling PRD #2560.
- Capacity, limit-park and account classification: they read `runs.codex_auth_mode`, not `cost_status`, and the true subscription auth mode keeps flowing to them unchanged.
- Wire contract: the `subscription` enum value, the `*_subscription_run_count` DTO fields and the `fixtures/api-contract/` fixtures stay; only rendering changes.

## Modules and seams

### Agent producers

- `CodexUsageAccountant.aggregateByModel(pricing)` (`agent/src/codex/token-accounting.ts`): present pricing context now prices either auth mode, replacing the old API-key gate and subscription emit branch. Preserve the evidence-completeness check, per-thread reconciliation, per-response `priceCodexResponse` and dominance. Keep: `pricing` omitted gives `unreported`; globally incomplete resume accounting still returns `undefined`.
- `deriveCodexRunCost(entries)` now has no subscription short-circuit or `authMode` parameter (`agent/tsconfig.json` sets `noUnusedParameters`). Its caller at `codex-harness.ts` (`decodeTerminal`) retains the guard: `this.authMode === undefined` gives `unreported`.
- `agent/src/codex/cross-check.ts` calls `accountant.aggregateByModel({ authMode: binding.authMode, ... })` directly; it inherits the change and needs its own test.
- The producer-side `subscription` member is removed from `CodexCostStatus` (`token-accounting.ts`) and `HarnessCost` (`agent/src/harness.ts`); knip does not see dead union members. The server/wire legacy status remains. `authMode` stays threaded everywhere else.
- The server folds only the per-model markers (`resolveCostStatusMarker` / `deriveUsageCost`, `usage_fold.go`); `metrics.cost` has no server reader. The per-model `modelUsage` entry is the contract that matters.

### Server fold

- No logic change. `deriveUsageCost` already honours a Codex `metered` marker with a present, finite, in-range `costUSD`, and still maps legacy `subscription` to that status with numeric zero storage for old workers. Its comment describes no estimate recorded, rather than no per-token charge.
- Keep the `UpsertRunUsage` same-key status-conflict rule (`api/internal/store/queries/runtime.sql`): conflicting statuses resolve to `unreported` and zero that row's cost. Keep the mixed-status rule in `run_usage_totals`: distinct legacy and metered rows make the run `unreported` while retaining the priced subtotal. Self/Admin queries include that subtotal and count the run as unreported. Example 4 depends on both boundaries; SQL, schema and DTOs stay unchanged (maintainer decision (2026-10-09)).
- Rollout orders: new workers with an old api are safe (the old fold already accepts a Codex `metered` marker). A new api with old workers stores new subscription runs as `subscription` until the fleet rolls (worker image pinned separately via `workers.image.tag`; a cordoned drain can take up to 24h); they render "no cost estimate recorded", which is true. This is why that wording carries no time claim.

### Readers and copy

Legacy `subscription` wording becomes "no estimate"; `metered` copy says API-equivalent. Shared helper first, then every caller that appends its own text:

- Web helper `web/src/lib/costStatus.ts`: `costHeadline` ("Subscription" to "No estimate"), `costSubLabel` (subscription: "no cost estimate recorded"; metered: "API-equivalent · Claude SDK" for claude, "API-equivalent · uzi price table" for codex, "API-equivalent" otherwise), `costCellText` ("sub" to "n/a"), `aggregateDisclosure` (text "+ N run(s) without a cost estimate", title "Runs without a cost estimate: tokens only"). Unreported disclosure states "cost incomplete; recorded partial costs included"; aggregates preserve the stored partial subtotal.
- Web callers: `web/src/components/RunUsage.tsx` (also rename the Claude tail label "Estimated, not metered" to "Estimated, not in the total", since metered now means estimated), `web/src/components/UsageCards.tsx` (heading "Metered cost" to "Cost (API-equivalent)"), `web/src/pages/RunsList.tsx` (hard-coded "subscription" literal near line 461), `web/src/components/PlanCrossCheck.tsx` (the " · subscription usage" suffix), `web/src/pages/runView/JudgePanel.tsx` (judge's own usage strip), `web/src/components/RunEvent.tsx` (comment).
- Web mock data: `web/src/mocks/data/judge.ts` (a Claude judge mock stored as `subscription`, already wrong since Claude is always metered: make it `metered`), `web/src/mocks/data/runs.ts` (cross-check usage mock), `web/src/mocks/mockApi/runs.ts` (aggregate counts); keep at least one mock row with `subscription` so the legacy rendering stays visible in mock mode.
- CLI/TUI: `api/cmd/uzi/tui_cost.go`, `tui_board_rows.go`, `tui_board.go`, `tui_detail.go`, `tui_detail_meters.go`, `plan_cross_check_render.go` (reuses `costDetailCell`), `admin.go` (`adminUsageCost`, which discloses runs without an estimate and unreported runs, with recorded partial costs included), and `uzi run get`'s COST row.
- Judge: `agent/src/judge-runner.ts` `renderTargetCostLine`: metered line says "(API-equivalent)" instead of "(metered)"; subscription line says no cost estimate was recorded.
- Comments: `web/src/lib/apiTypes.ts` (CostStatus and the SelfUsage cost_status doc), `agent/src/protocol.ts` (target cost status), `api/internal/apitypes/usage.go`.
- `api/internal/workersvc/plan_cross_check_summary.go` keeps its status fold unchanged (no wording).

### Docs and records

- `docs/run-cost.md` "Metered, subscription, and unreported cost": `metered` means a recorded API-equivalent cost (Claude: SDK-computed; Codex: uzi's pinned Standard table, either credential mode); `subscription` is a legacy status meaning no estimate was recorded; subscription users may still consume purchased credits beyond included usage, so the figure is a comparison, not a bill. Update the disclosure examples to the new strings.
- `docs/cli.md`, `docs/cross-check.md`, `docs/run-activity.md`, `docs/worker-model.md`: align wording; `task docs:sync` updates the embedded mirrors.
- `adr/1106-codex-harness.md`: correct the obsolete API-key "remains unreported" claim; align pertinent current tail presentation wording in `adr/2014-run-usage-estimated-tail.md` without changing its accounting boundary.
- `specs/human.md` cost decision: update the API-equivalent meaning and maintainer-attributed partial preservation, tagged `(AI-synced 2026-10-09)`.
- Record cost-only supersessions of PRD #1332's neutral-subscription-cost decision and active PRD #1551's line 110 rule (subscription always reports `subscription` regardless of a price row) in this Decision Log. Leave those PRDs and their non-cost requirements unchanged.
- `CHANGELOG.md` `[Unreleased]` line.

## Testing decisions

Red-before/green-after is required for each changed behaviour; unchanged-behaviour controls are labelled as such.

- Agent, `token-accounting` tests (follow the existing api-key cases): a subscription run with identical responses yields the same per-model `costUSD` as an api-key run; subscription with an unknown model, `gpt-5.6-sol` on 2026-11-21, a malformed bucket, or an unreconciled thread yields `unreported` with tokens retained; no pricing context yields `unreported`.
- Agent, harness terminal test: a subscription terminal's `usage.wire.modelUsage[model]` has `costStatus: "metered"` and `costUSD` (primary assertion); `metrics.cost.kind == "metered"` secondary; missing auth context gives `unreported`.
- Agent, cross-check test: a subscription checker's posted `modelUsage` entries are `metered` with `costUSD`.
- Agent, judge `renderTargetCostLine` tests updated to the new lines.
- Server: `TestUsageFoldPricedSubscriptionAndLegacyLegsLiveDB` in `api/internal/workersvc/usage_fold_cost_marker_livedb_test.go` covers example 4 with distinct same-model session/epoch legs: `unreported`, partial cost $0.00291 and tokens retained. Assert literal Self/Admin lifetime/recent totals and counts, legacy-only zero storage and old-worker unreported behavior. Existing store controls in `api/internal/store/harness_cost_status_livedb_test.go` cover same-key conflict suppression and distinct-model partial preservation ($0.07 intact). A Codex `metered` marker case is an unchanged-behaviour control. This preserves partial subtotals per maintainer decision (2026-10-09), not a new SQL rule.
- Web: `costStatus` tests (headline, sub-label, cell, disclosure), `UsageCards.test.tsx`, `PlanCrossCheck`, `JudgePanel`, `RunsList` render tests; keep every "never `$0` for a non-metered row" assertion.
- CLI/TUI: update `plan_cross_check_test.go`, `run_render_harness_cost_test.go`, `uxlab_gen_test.go`, `tui_usage_test.go`, `admin_usage_window_test.go` to the new wording.

## Milestones

### M1: Codex subscription runs record an API-equivalent cost

Blocked by: none.

Both agent producers, the type cleanup, every reader and mock above, docs/ADR/spec/changelog, and the tests, in one slice: after it, a new subscription run shows a dollar figure everywhere and a stored `subscription` row reads "no estimate" everywhere.

Acceptance: examples 1 to 6 hold in tests; `task gate:agent`, `task gate:web`, `task gate:api`, `task gate:repo` green; `task check-docs:web` green; `TestEmbeddedDocsMatchSource` green after `task docs:sync`. The human owner's publication-only waiver of the local agent-gate criterion (M1.c1) is recorded below; that gate has not passed locally. Required final-head agent CI remains a merge condition.

## Verification checklist (2026-10-09)

Evidence below composes producer, persistence, aggregate and renderer tests;
it is **not an actual-provider end-to-end cost test**. The implementation and
documentation have passed fixed-commit reviewer/tester coverage. The local
full agent gate has not passed.

On 2026-10-09, the human owner authorized waiving M1's full local
`task gate:agent` criterion (M1.c1) so M1 can complete for PR publication.
This supersedes the earlier decision to keep M1 pending before publication;
it does not waive the merge condition. GitHub CI's required agent test jobs
must pass on the final PR head, enforced by the landing session. If the
completion interlock blocks on M1.c1, the owner will record its accept
decision. Recovery code and the failing tests are unchanged by this task;
no further agent-gate investigation is authorized. Keep this PRD active and
do not move it to `prds/done/` while final-head agent CI remains unresolved.

- [x] Unit A (`ea707` + fix `192413`): producer pricing coverage has literal
  $1.278 = $0.293 low-tier + $0.985 high-tier in
  `agent/test/codex-cost-accounting.test.ts`; lead terminal coverage has
  $0.013 in `agent/test/codex-harness.test.ts`; checker coverage has
  `gpt-6.1-sol` input 300/cache-read 600/cache-write 100/output 200 =
  $0.00291 in `agent/test/codex-cross-check.test.ts`. Unknown models,
  expired promotional rows and bad buckets fail closed with tokens retained.
- [x] Unit B (`cbb62` + fix `59edd`): shared renderer helper, RunUsage,
  UsageCards, RunsList, JudgePanel and PlanCrossCheck cover status suppression
  and DTO-matching mocks, including a $0.61 mixed partial aggregate. Supplied
  full web gate log `jfMj3U`: EXIT=0.
- [x] Unit C (`4b6bac`): supplied API gate log `FkNRt8`: EXIT=0.
  `TestCostReadersAPIEstimate` and `TestSpendBasisFitsRail` in
  `api/cmd/uzi/api_equivalent_cost_test.go` preserve true zero and suppress
  per-run dollars for non-metered statuses.
- [x] Isolated throwaway Docker/Postgres log `z9K0G1`: both
  `TestUsageFoldPricedSubscriptionAndLegacyLegsLiveDB` and the existing
  `TestUsageFoldHonorsCodexCostMarkerLiveDB` passed, exit 0, zero skips. The new
  test persists metered $0.002910; distinct same-model legacy/new session and
  epoch rows total `unreported` with $0.00291 and retained tokens.
  Self/Admin lifetime/recent literal totals are $0.00582 with asserted counts;
  legacy-only zero and old-worker unreported controls pass. Existing store
  same-key/distinct-model controls were reviewer-confirmed PASS with the
  $0.07 partial intact.
- [x] UnitD: `task docs:sync` exited 0 (mkdir/remove/copy of top-level
  Markdown mirrors); `task check-docs:web` log `6V3g1R` exited 0 (74 docs,
  house-style length warnings only); focused `TestEmbeddedDocsMatchSource`
  log `hxp1z9` exited 0. Owned current cost assertions were checked against
  producer, renderer, same-key upsert and run/aggregate fold boundaries.
- [x] Repository gate (`task gate:repo`, log `Lwce5J`) exited 0, including
  tracked-source secret and static-analysis checks. Documentation reviewer and
  tester found no mandatory findings over `4b6bacf1..5b9a3060`.
- [x] Standalone `task test:codex-m4` (log `XElIOs`) exited 0: 183 tests
  passed, zero skips/cancellations; its required Claude/U, Codex/U and Codex/P
  completeness matrix passed. This checks conformance, not a real-provider
  cost run.
- [x] M1 complete for publication under the human owner's M1.c1 waiver;
  this is acceptance of the documented local verification gap, not a test pass.
- [ ] GitHub CI's required agent test jobs pass on the final PR head before merge.
- [ ] Passing full local `task gate:agent` (M1.c1 waived for publication).
  Three completed
  full runs (`7ML8PD`, `ELKW5e`, `JlKdkk`) each reported 11,226 passes,
  one failure, three skips and no cancellations, then exited 201. They failed
  different assertions in unchanged plan-replay/cancellation test files:
  - `7ML8PD` (`task gate:agent`, default test concurrency):
    `worker shutdown mid-revision, reclaimed with worker-side resume_lineage_break`
    in `agent/test/runner-gate-verdict-interruption.test.ts:675`.
  - `ELKW5e` (`task gate:agent`, `UZI_AGENT_TEST_CONCURRENCY=2`):
    `explicit close cancels startup; request signal cancels active pass`
    in `agent/test/codex-pr-description-eval.test.ts:279`.
  - `JlKdkk` (`task gate:agent`, `UZI_AGENT_TEST_CONCURRENCY=4`,
    `UV_THREADPOOL_SIZE=16`):
    `a stub (Codex-shaped) executor never re-presents: its first gate waits for the replayed backlog, and a replayed approve goes stale at it (finding 6; round 4 finding 1)`
    in `agent/test/runner-gate-verdict-interruption.test.ts:1538`.

  Cost tests passed in all three. The failed cases passed focused BASE/HEAD
  comparisons. All 72 plan-replay tests and all 14 evaluator tests also passed
  on BASE. A typechecked baseline delayed-exit fixture reproduced the
  evaluator's double-exit assertion, but natural baseline failure was not
  reproduced and natural causes of the full-suite failures remain unconfirmed.
  CPU contention is a hypothesis, not an established cause. API, web, repo,
  isolated live-DB, docs and Codex conformance passed; none marks the full
  agent gate as passed or substitutes for the required PR-head agent jobs.

  A further serial attempt on 2026-10-09 (`D59AGw`,
  `UZI_AGENT_TEST_CONCURRENCY=1`, `UV_THREADPOOL_SIZE=16`) was stopped by
  the runtime's 60-minute command deadline while unit tests were still running.
  The command tree was reaped; the log ends with
  `task: Signal received: "terminated"` and has no final test summary or
  recorded gate exit status.
  This attempt is incomplete, neither a pass nor a fourth completed failing
  run. The three completed failures above remain the local full-run results.

## Risks

- **Model-id match.** A subscription run is priced only when its recorded model id exactly matches a table key; otherwise it reads "Unavailable". Checked before filing: the last 12 Codex runs on the hosted instance recorded only `gpt-6.1-sol` (26 per-model entries), which the table prices.
- **Rollout skew.** Covered under Server fold; neither order shows a wrong figure.

## Decision Log

- **D1. Store the estimate as `metered`, not a new column or status.** Claude subscription runs already store an SDK-computed API-equivalent figure as `metered`; matching it is the smallest change and existing metered readers render it. Rejected: a nullable `estimated_cost_usd` column with `subscription` kept (new schema, DTO fields and render path on every surface, and inconsistent with Claude). Cost-only supersession of [PRD #1332](1332-codex-routing-foundation.md)'s neutral-subscription-cost decision and [PRD #1551](../1551-per-harness-worker-model-defaults.md)'s line 110 subscription-always-`subscription` rule regardless of a price row. Their auth, model, effort, capacity and other requirements are unchanged; neither record is edited here.
- **D2. Price per response on the worker, not at read time.** The >272K tier changes all four rates for a whole response, so stored per-model totals cannot reproduce the figure.
- **D3. Keep every fail-closed rule.** Unknown model, passed promotional date, malformed cache split, unreconciled thread, missing auth context and incomplete resume accounting stay `unreported`. Rejected: the Claude SDK's default-rate fallback for unknown models.
- **D4. Stored `subscription` rows read "no cost estimate recorded", contributing no dollar estimate.** Distinct priced rows on a mixed run remain in aggregates under D5. No time claim, because rows from not-yet-rolled workers are also `subscription`. Rejected: hiding them; `$0`; "recorded before estimates" (false during a roll).
- **D5. Mixed legacy/new legs fold to `unreported`; aggregates preserve recorded partials (maintainer decision (2026-10-09)).** Distinct fold keys retain their metered rows, including same-model legs under distinct session/epoch keys. The run's cost is unavailable on per-run readers; Self/Admin totals include its recorded partial and count it as unreported, disclosing incompleteness. Same-key status conflicts zero that row only. No SQL, schema or DTO change; do not discard valid partial evidence or present it as a complete run estimate.
- **D6. No wire contract change.** Historical rows keep `subscription` and old workers still send it until the fleet rolls.
- **D7. User-facing copy says "API-equivalent".** Redefining `metered` without changing copy that implies a charge ("your OpenAI credential", "Metered cost", "Estimated, not metered", the judge's "(metered)") would mislead. The judge line feeds cost-efficiency recommendations, so it states the basis explicitly.
