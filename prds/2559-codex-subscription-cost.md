# PRD #2559: API-equivalent cost for Codex subscription runs

**Status**: Draft. Child 1 of 2 under umbrella #2558 (Codex run cost). Independent of its sibling PRD #2560 (price-table freshness). The two overlap textually only in `docs/run-cost.md`, its `api/internal/uzidocs/embed/` mirror and `CHANGELOG.md`; in-flight #2555 (run usage card) also touches `web/src/components/RunUsage.tsx` and `codex-harness.ts`, so expect landing conflicts there.

Planning facts were read at `main` `140b5c767` (after #2552, which rewrote the aggregate disclosure wording).

## Problem

A Codex run on a ChatGPT subscription login shows no cost anywhere: the run page headline reads "Subscription", table cells read `sub`, `uzi run get` prints `subscription`, the judge says "no per-token dollar figure applies", and the Self/Admin usage totals leave these runs out with a "+ N Codex sub runs" disclosure. A Claude run on a subscription token, by contrast, shows a dollar figure on every surface: the Claude SDK prices token usage client-side from a price table bundled in the SDK, and uzi folds that `total_cost_usd` as `metered` (`deriveUsageCost`, `api/internal/workersvc/usage_fold.go`, Claude arm).

So an owner comparing harnesses sees Claude runs cost dollars and Codex runs cost nothing, and cannot compare the two harnesses' spend on the same work.

## Outcome

A new Codex subscription run is priced exactly like a Codex API-key run: each upstream response is priced against uzi's pinned Standard table (`priceCodexResponse`, `agent/src/codex/codex-pricing.ts`), every existing fail-closed rule is kept, and each model entry is posted as `costStatus: "metered"` with its `costUSD`. The server already stores such an entry as `cost_status = 'metered'`, so the run renders on every surface that shows a metered cost, as a Claude subscription run does today. This applies to both producers of Codex model usage: lead runs (`codex-harness.ts`) and plan cross-check checker runs (`cross-check.ts`).

`metered` is redefined, in docs, comments and user-facing copy, as **a recorded API-equivalent cost** for both harnesses, not proof of a charge.

Rows stored with `cost_status = 'subscription'` (runs recorded before this change, and runs on workers not yet rolled to it) render as "no cost estimate recorded": tokens shown, no dollar figure, never `$0`, excluded from dollar totals.

Acceptance examples:

1. A Codex subscription run, model `gpt-6.1-sol`, every response priced and reconciled. The posted `modelUsage["gpt-6.1-sol"]` has `costStatus: "metered"` and a numeric `costUSD`; `uzi run get --json` has `usage.cost_status == "metered"`; the run page headline is a dollar figure with an "API-equivalent" sub-label; the run counts in the Self usage dollar total and not in its disclosure.
2. The same run on a model absent from the table (for example `gpt-5.5`), or a `gpt-5.6-sol` run on or after its promotional review date (2026-11-21). `cost_status == "unreported"`, tokens retained, every surface reads "Unavailable"/`n/a`, never `$0`.
3. A Codex run stored with `cost_status = 'subscription'`. The run page reads "No estimate" with a sub-label "no cost estimate recorded"; table cells and the TUI read `n/a`; `uzi run get` prints `no estimate`; the judge line says no cost estimate was recorded; aggregates exclude it and disclose it as "+ N runs without a cost estimate".
4. A run whose earlier legs folded `subscription` (old worker) and later legs folded `metered` (new worker). The existing conflict rule makes it `unreported`; no partial figure.
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

- `CodexUsageAccountant.aggregateByModel(pricing)` (`agent/src/codex/token-accounting.ts`): `priceApiKey` gates per-response pricing on `authMode === "api_key"`, and the emit loop returns `costStatus: "subscription"` for a subscription run. Change both so any present pricing context prices: same evidence-completeness check, per-thread reconciliation, per-response `priceCodexResponse` and dominance. Keep: `pricing` omitted gives `unreported`; globally incomplete resume accounting still returns `undefined`.
- `deriveCodexRunCost(entries, authMode)`: remove the subscription short-circuit and the now-unused `authMode` parameter (`agent/tsconfig.json` sets `noUnusedParameters`); update its caller at `codex-harness.ts` (`decodeTerminal`) and keep the call-site guard `this.authMode === undefined` gives `unreported`.
- `agent/src/codex/cross-check.ts` calls `accountant.aggregateByModel({ authMode: binding.authMode, ... })` directly; it inherits the change and needs its own test.
- Remove the `subscription` member from `CodexCostStatus` (`token-accounting.ts`) and from `HarnessCost` (`agent/src/harness.ts`) once nothing produces it; knip does not see dead union members, so do it by hand. `authMode` stays threaded everywhere else.
- The server folds only the per-model markers (`resolveCostStatusMarker` / `deriveUsageCost`, `usage_fold.go`); `metrics.cost` has no server reader. The per-model `modelUsage` entry is the contract that matters.

### Server fold

- No logic change. `deriveUsageCost` already honours a Codex `metered` marker with a present, finite, in-range `costUSD`, and still maps `subscription` to `subscription`/`$0` for old workers. Update its doc comment ("that credential mode has no per-token charge").
- Keep the `UpsertRunUsage` status-conflict rule (`api/internal/store/queries/runtime.sql`) and the mixed-status rule in `run_usage_totals`; example 4 depends on them.
- Rollout orders: new workers with an old api are safe (the old fold already accepts a Codex `metered` marker). A new api with old workers stores new subscription runs as `subscription` until the fleet rolls (worker image pinned separately via `workers.image.tag`; a cordoned drain can take up to 24h); they render "no cost estimate recorded", which is true. This is why that wording carries no time claim.

### Readers and copy

Legacy `subscription` wording becomes "no estimate"; `metered` copy says API-equivalent. Shared helper first, then every caller that appends its own text:

- Web helper `web/src/lib/costStatus.ts`: `costHeadline` ("Subscription" to "No estimate"), `costSubLabel` (subscription: "no cost estimate recorded"; metered: "API-equivalent · Claude SDK" for claude, "API-equivalent · uzi price table" for codex, "API-equivalent" otherwise), `costCellText` ("sub" to "n/a"), `aggregateDisclosure` (text "+ N run(s) without a cost estimate", title "Runs without a cost estimate: tokens only").
- Web callers: `web/src/components/RunUsage.tsx` (also rename the Claude tail label "Estimated, not metered" to "Estimated, not in the total", since metered now means estimated), `web/src/components/UsageCards.tsx` (heading "Metered cost" to "Cost (API-equivalent)"), `web/src/pages/RunsList.tsx` (hard-coded "subscription" literal near line 461), `web/src/components/PlanCrossCheck.tsx` (the " · subscription usage" suffix), `web/src/pages/runView/JudgePanel.tsx` (judge's own usage strip), `web/src/components/RunEvent.tsx` (comment).
- Web mock data: `web/src/mocks/data/judge.ts` (a Claude judge mock stored as `subscription`, already wrong since Claude is always metered: make it `metered`), `web/src/mocks/data/runs.ts` (cross-check usage mock), `web/src/mocks/mockApi/runs.ts` (aggregate counts); keep at least one mock row with `subscription` so the legacy rendering stays visible in mock mode.
- CLI/TUI: `api/cmd/uzi/tui_cost.go`, `tui_board_rows.go`, `tui_board.go`, `tui_detail.go`, `tui_detail_meters.go`, `plan_cross_check_render.go` (reuses `costDetailCell`), `admin.go` (`adminUsageCost`, which prints `subscription/unreported costs excluded: subscription_runs=`), and `uzi run get`'s COST row.
- Judge: `agent/src/judge-runner.ts` `renderTargetCostLine`: metered line says "(API-equivalent)" instead of "(metered)"; subscription line says no cost estimate was recorded.
- Comments: `web/src/lib/apiTypes.ts` (CostStatus and the SelfUsage cost_status doc), `agent/src/protocol.ts` (target cost status), `api/internal/apitypes/usage.go`.
- `api/internal/workersvc/plan_cross_check_summary.go` keeps its status fold unchanged (no wording).

### Docs and records

- `docs/run-cost.md` "Metered, subscription, and unreported cost": `metered` means a recorded API-equivalent cost (Claude: SDK-computed; Codex: uzi's pinned Standard table, either credential mode); `subscription` is a legacy status meaning no estimate was recorded; subscription users may still consume purchased credits beyond included usage, so the figure is a comparison, not a bill. Update the disclosure examples to the new strings.
- `docs/cli.md`, `docs/cross-check.md`, `docs/run-activity.md`, `docs/worker-model.md`: align wording. `task docs:sync`, commit the mirror.
- `adr/1106-codex-harness.md` lines 524-525 wrongly say API-key cost "remains unreported": correct to the current behaviour.
- `specs/human.md` line 339 ("subscription and unreported runs are disclosed separately from cost"): update to the new meaning, tagged `(AI-synced YYYY-MM-DD)`.
- This supersedes `prds/done/1332-codex-routing-foundation.md` line 115 (subscription usage keeps a neutral subscription cost); say so in the Decision Log, not by editing the done PRD.
- `CHANGELOG.md` `[Unreleased]` line.

## Testing decisions

Red-before/green-after is required for each changed behaviour; unchanged-behaviour controls are labelled as such.

- Agent, `token-accounting` tests (follow the existing api-key cases): a subscription run with identical responses yields the same per-model `costUSD` as an api-key run; subscription with an unknown model, `gpt-5.6-sol` on 2026-11-21, a malformed bucket, or an unreconciled thread yields `unreported` with tokens retained; no pricing context yields `unreported`.
- Agent, harness terminal test: a subscription terminal's `usage.wire.modelUsage[model]` has `costStatus: "metered"` and `costUSD` (primary assertion); `metrics.cost.kind == "metered"` secondary; missing auth context gives `unreported`.
- Agent, cross-check test: a subscription checker's posted `modelUsage` entries are `metered` with `costUSD`.
- Agent, judge `renderTargetCostLine` tests updated to the new lines.
- Server: a live-DB fold test for example 4 (one `subscription` leg then one `metered` leg on one run gives `unreported`, cost 0), in an existing `workersvc` `*LiveDB` file. A `deriveUsageCost` case for a Codex `metered` marker is an unchanged-behaviour control.
- Web: `costStatus` tests (headline, sub-label, cell, disclosure), `UsageCards.test.tsx`, `PlanCrossCheck`, `JudgePanel`, `RunsList` render tests; keep every "never `$0` for a non-metered row" assertion.
- CLI/TUI: update `plan_cross_check_test.go`, `run_render_harness_cost_test.go`, `uxlab_gen_test.go`, `tui_usage_test.go`, `admin_usage_window_test.go` to the new wording.

## Milestones

### M1: Codex subscription runs record an API-equivalent cost

Blocked by: none.

Both agent producers, the type cleanup, every reader and mock above, docs/ADR/spec/changelog, and the tests, in one slice: after it, a new subscription run shows a dollar figure everywhere and a stored `subscription` row reads "no estimate" everywhere.

Acceptance: examples 1 to 6 hold in tests; `task gate:agent`, `task gate:web`, `task gate:api`, `task gate:repo` green; `task check-docs:web` green; `TestEmbeddedDocsMatchSource` green after `task docs:sync`.

## Risks

- **Model-id match.** A subscription run is priced only when its recorded model id exactly matches a table key; otherwise it moves from "Subscription" to "Unavailable". Checked before filing: the last 12 Codex runs on the hosted instance recorded only `gpt-6.1-sol` (26 per-model entries), which the table prices.
- **Rollout skew.** Covered under Server fold; neither order shows a wrong figure.

## Decision Log

- **D1. Store the estimate as `metered`, not a new column or status.** Claude subscription runs already store an SDK-computed API-equivalent figure as `metered`; matching it is the smallest change and every surface already renders it. Rejected: a nullable `estimated_cost_usd` column with `subscription` kept (new schema, DTO fields and render path on every surface, and inconsistent with Claude). Supersedes the neutral-subscription-cost rule in the PRD #1332 record.
- **D2. Price per response on the worker, not at read time.** The >272K tier changes all four rates for a whole response, so stored per-model totals cannot reproduce the figure.
- **D3. Keep every fail-closed rule.** Unknown model, passed promotional date, malformed cache split, unreconciled thread, missing auth context and incomplete resume accounting stay `unreported`. Rejected: the Claude SDK's default-rate fallback for unknown models.
- **D4. Stored `subscription` rows read "no cost estimate recorded", excluded from dollar totals.** No time claim, because rows from not-yet-rolled workers are also `subscription`. Rejected: hiding them; `$0`; "recorded before estimates" (false during a roll).
- **D5. Mixed legacy/new legs fold to `unreported`.** The existing conflict rule already does it; a partial figure would understate the run.
- **D6. No wire contract change.** Historical rows keep `subscription` and old workers still send it until the fleet rolls.
- **D7. User-facing copy says "API-equivalent".** Redefining `metered` without changing copy that implies a charge ("your OpenAI credential", "Metered cost", "Estimated, not metered", the judge's "(metered)") would mislead. The judge line feeds cost-efficiency recommendations, so it states the basis explicitly.
