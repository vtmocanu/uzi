# PRD #2560: Price-table freshness reminders and Codex price coverage

**Status**: Complete (in-scope M1 and M2). The weekly maintainer-local workflow follow-up shipped separately in PR #2595 (`.github/workflows/pricing-freshness.yml` plus `scripts/pricing-freshness-issue.sh`). Child 2 of 2 under umbrella #2558 (Codex run cost). Independent of its sibling PRD #2559 (API-equivalent cost for Codex subscription runs); they overlap textually only in `docs/run-cost.md`, its `api/internal/uzidocs/embed/` mirror and `CHANGELOG.md`.

Planning facts were read at `main` `140b5c767`. External facts were checked by the maintainer on 2026-10-09 and are recorded here, because the implementing worker has no open-web access.

## Problem

At planning time, uzi priced runs from two hand-maintained tables, and nothing reminded anyone to re-check either:

- **Codex**: `agent/src/codex/codex-pricing.ts` (`TABLE`, version `openai-standard-2026-10-01`, four models). The table was introduced in `9d5d53b51`; `gpt-6-sol` was added in `a2970cdf9`/`3a25e7387` and `gpt-6.1-sol` in `842ddb865`, each alongside a Codex runtime bump. No row has been re-verified since it was added.
- **Anthropic**: `api/internal/anthropicprice/anthropicprice.go` (`AnthropicPriceFetchedAt = "2026-10-03"`, used for the estimated tail of interrupted Claude sessions).

OpenAI publishes no machine-readable price list: `/v1/models` and the Codex app-server `model/list` carry no rates, and `/organization/costs` reports spend, not rates. So rates are copied by hand, and three things fail silently:

- A rate OpenAI changes keeps pricing runs at the old figure.
- A model the fleet starts using has no row, so its runs read "cost unavailable". The bundled Codex catalog already lists `gpt-5.5`, `gpt-5.6-luna`, `gpt-5.6-terra` and `gpt-6-luna`, none of them priced.
- The `gpt-5.6-sol` promotional review date (2026-11-21) passes and its runs turn unavailable.

The existing nudges (`task nudge:roles`, `task nudge:builtins`) required someone to run them; a nudge alone would not provide an in-app signal.

## Outcome

1. The Codex price table lives in one JSON file with per-model `verified_at` and official source URLs. The agent prices from it with no rate change; the API embeds a byte-identical mirror.
2. `task nudge:pricing` reports every Codex model verified more than 30 days ago, every Codex promotional row whose review date is within 14 days or passed, and the Anthropic table when its fetch date is more than 30 days old. Advisory: findings exit 0 and change no rate; malformed inputs exit 2. The nudge is not a gate member.
3. The admin Health tab gains a `pricing.codex` check that warns when a Codex model with recent usage has no currently valid price in this release's table, distinguishing "no price" from "promotional price expired".
4. A maintainer-local follow-up (Out of scope) adds the weekly GitHub workflow that runs the nudge and opens or updates one issue.

Acceptance examples (all dates UTC):

1. A Codex row with `verified_at` 31 days before `--today`: `task nudge:pricing` prints a line naming the model, its `verified_at` and its source URLs, and exits 0. At exactly 30 days it prints nothing for that row.
2. `--today 2026-11-07` with `gpt-5.6-sol` `promo_review_date: 2026-11-21` (14 days away): reported as due. `--today 2026-11-06` (15 days): not reported. `--today 2026-11-21` and later: reported as passed. Pricing behaviour on or after that date (unavailable) is unchanged.
3. `AnthropicPriceFetchedAt` 31 days before `--today`: reported, naming the Anthropic source URL.
4. In the last 7 days Codex usage folded for `gpt-5.5` (no row) in 3 distinct runs, and, with the clock on 2026-11-21, for `gpt-5.6-sol` (expired promotion) in 2 runs. `uzi admin health` shows `pricing.codex` at warning severity with evidence `gpt-5.5: 3 runs, no price` and `gpt-5.6-sol: 2 runs, promotional price expired`. With no such usage the check is `ok`, worded "no recent Codex usage on unpriced models".
5. Editing `agent/src/codex/codex-pricing.json` without `task codex-pricing:sync` reddens `gate:api`, naming the mirror.
6. A malformed JSON edit (negative rate, `2026-02-30`, missing `sources`) fails the agent load-time validator and the api loader with a named error; neither silently yields an empty table.

## Out of scope

- **The weekly GitHub workflow file.** A uzi worker cannot push `.github/workflows/**` (worker PAT lacks the `workflow` scope; `.claude/rules/prds.md`), so it is a maintainer-local change after this PRD merges. Contract for that change: `on: schedule` weekly plus `workflow_dispatch`; `permissions: {contents: read, issues: write}` only (do not copy `roles-manifest-refresh.yml`'s push/PAT permissions); a `concurrency` group; runs `task nudge:pricing -- --json`; finds its issue by a fixed label **and** a bot-owned body marker, never editing a human issue that merely carries the label; updates the body only when findings change; closes its issue with a comment when findings clear, and reopens it when they recur; issue content limited to repository pricing metadata.
- Adding rows for the unpriced catalog models. The Health check reports their use; a maintainer adds a row after verifying its rates.
- Live or automatic price fetching or overwriting. A human always verifies rates against official pages.
- Recording why an individual run's cost was unavailable. The Health check describes this release's coverage of observed models, not each run's cause.
- Moving the Anthropic table to JSON or adding an Anthropic Health check: the nudge reads its existing date constant only.
- Any change to rates, tiers, the 272K threshold, service-tier assumptions or fail-closed rules.
- The subscription-run pricing change: sibling PRD #2559.

## Modules and seams

### Codex price data (`agent/src/codex/codex-pricing.json`, new, canonical)

- Shape: `{ "version": "openai-standard-2026-10-01", "input_tier_threshold_tokens": 272000, "models": { "<model>": { "verified_at": "YYYY-MM-DD", "promo_review_date"?: "YYYY-MM-DD", "sources": ["https://..."], "low": { "uncached_input", "cached_input", "cache_write", "output" }, "high": { same } } } }`, USD per 1,000,000 tokens.
- Initial content is the current `TABLE` verbatim (no rate changes), plus:

| model | verified_at | promo_review_date | sources |
|---|---|---|---|
| `gpt-6-astra` | 2026-09-13 | none | `https://developers.openai.com/api/docs/pricing`, `https://developers.openai.com/api/docs/models/gpt-6-astra` |
| `gpt-5.6-sol` | 2026-09-13 | 2026-11-21 | `https://developers.openai.com/api/docs/pricing`, `https://developers.openai.com/api/docs/models/gpt-5.6-sol` |
| `gpt-6-sol` | 2026-09-23 | none | `https://developers.openai.com/api/docs/pricing`, `https://developers.openai.com/api/docs/models/gpt-6-sol` |
| `gpt-6.1-sol` | 2026-10-01 | none | `https://developers.openai.com/api/docs/pricing`, `https://developers.openai.com/api/docs/models/gpt-6.1-sol` |

  Every URL above resolved on 2026-10-09 and its rates matched the current table (`gpt-5.6-sol`'s page states the promotional pricing holds "at least through November 21, 2026"). The `verified_at` dates are the existing ones from the file's comments, not 2026-10-09: this PRD moves data, it does not re-verify.
- `codex-pricing.ts` imports it with `import table from "./codex-pricing.json" with { type: "json" };` (`resolveJsonModule` is on and `module` is `NodeNext` in `agent/tsconfig.json`; this is the first JSON import in `agent/src`). It validates once at module load and throws a named error on: a missing or extra top-level field; `input_tier_threshold_tokens` not a positive integer; a model with no `sources` or a non-`https` source; any rate not a finite number >= 0; a date that is not a real `YYYY-MM-DD` calendar date (strict: `2026-02-30` fails). It then builds the same `TABLE`, `CODEX_PRICE_TABLE_VERSION`, `CODEX_INPUT_TIER_THRESHOLD_TOKENS` and per-row promotional dates. `priceCodexResponse`'s signature and behaviour are unchanged.
- The worker images copy `agent/src` whole (`templates/base/Dockerfile`, `templates/jvm/Dockerfile`: `COPY agent/src ./src`), so the JSON ships without packaging changes.
- A pricing-only edit changes `agent/src`, so `scripts/worker-tag-autobump.sh` bumps the worker tag at the next release and the fleet rolls (a brief capacity dip, #1470). That roll is how a price change reaches workers; document it.
- Comments in the moved code refer to "the pinned Codex version", not a version number (Renovate PR #2532 moves it to 0.160.0; its token-usage notification schema is byte-identical to 0.159.3's).

### API mirror (`api/internal/codexprice/`, new; named after `api/internal/anthropicprice`)

- `go:embed` cannot reach outside the api module, so `task codex-pricing:sync` copies the canonical JSON to `api/internal/codexprice/codex-pricing.json` (the `task docs:sync` precedent; never a gate member, since it writes the tree). A Go test asserts byte equality with `../../../agent/src/codex/codex-pricing.json`, resolved from the package's test directory as `api/internal/uzidocs`'s test does. Add the file to the cross-boundary table in `.claude/rules/go.md`.
- The loader applies the same validation as the agent and fails package init loudly on malformed data; there is no empty-table fallback.
- Exposes two functions over the same loaded table: `Coverage(model string, now time.Time) Status`, returning `Priced`, `NoPrice` (no row) or `PromoExpired` (row whose `promo_review_date` is on or before `now`'s UTC date), matching the agent's `promoReviewPassed` boundary; and `PricedModels(now time.Time) []string`, the sorted keys whose `Coverage` is `Priced`. Health derives `@priced` from `PricedModels`, never from its own model list. Nothing in the api prices runs.

### Nudge (`scripts/pricing-freshness.sh`, `task nudge:pricing`)

- Reads the Codex JSON with `jq`, doing date math inside `jq` (`strptime`/`mktime`), not `date -d` (maintainers run macOS). Reads `AnthropicPriceFetchedAt` and `AnthropicPriceSourceURL` from `api/internal/anthropicprice/anthropicprice.go` with an anchored `awk` match.
- Rules, UTC calendar days: a Codex row whose `verified_at` is strictly more than 30 days before today; a Codex `promo_review_date` 14 or fewer days after today, or on or before today; the Anthropic fetch date strictly more than 30 days before today.
- Output: one line per finding (subject, date, reason, source URLs); `--json` prints the findings array; `--today YYYY-MM-DD` overrides the date for tests. Exit 0 with or without findings; exit 2 when either source is missing, unparseable or fails validation (instrument broken, the repo's convention). Never a member of `task gate`.

### Health check (`api/internal/healthsvc`)

- New fixed check id `pricing.codex` in `checkMeta`: scope `instance`, group `groupHousekeeping`, title "Codex price coverage", doc slug `admin-health` (an operator page, which `web/src/lib/docs.ts` routes for admins; `run-cost` is contributor-audience and does not route). Appended last in `Service.Evaluate`'s registry order, after `release.check`.
- New Store method and query in `api/internal/store/queries/health.sql`, taking an injected `@cutoff` (the Service's clock minus 7 days, never database `now()`) and `@priced` (`codexprice.PricedModels` at that clock): `SELECT model, COUNT(DISTINCT run_id) AS runs FROM run_usage WHERE harness = 'codex' AND updated_at >= @cutoff AND model <> ALL(@priced::text[]) GROUP BY model ORDER BY runs DESC, model ASC LIMIT 11`. `run_usage.harness` exists (migration 00226), so no join. Filtering priced models in SQL before the limit means supported models can never fill the window and hide an unpriced one.
- `updated_at` is the last fold time of the usage row, including redelivery, not exact inference time; the check's wording says "recent Codex usage", not "used in the last 7 days".
- Evidence: one `HealthEvidenceDTO` per returned model (at most 10), `label` = model and `value` = `N runs, no price` or `N runs, promotional price expired` (from `Coverage`), both through the existing `safe()` bound (`maxEvidenceBytes`); an eleventh row becomes a single `and more` evidence row.
- Severity: `warn` with any finding; `ok` otherwise, with summary "no recent Codex usage on unpriced models"; `unknown` on query failure, including the five-second query deadline; no `danger` band. Successful reads and failures use the same ten-minute cache.
- Cost: `Evaluate` runs on every admin Health GET and once a minute per replica in the episode reconciler, and there is no index on `run_usage.updated_at`. Memoize this check's result in the Service for 10 minutes (single shared result, injected clock), so the scan runs at most six times an hour per replica. The refresh is synchronized so concurrent evaluations never start more than one scan; a query failure is cached as `unknown` and never leaves an expired `ok` in place. The warning can therefore appear up to 10 minutes late; `docs/admin-health.md` says so. No new index or migration.
- Version skew: the check compares against the api's embedded table while workers price with their own copy. The two normally agree (a table change rolls the fleet via the autobump above); during a roll or under a per-cluster `workers.image.tag` override they can differ for that window. Accepted and documented.

### Docs and records

- `docs/admin-health.md`: a row for `pricing.codex` in the check table.
- `docs/run-cost.md` (contributor): an "Updating prices" section: where both tables live, verify against official pages, bump `version` and the row's `verified_at` (or `AnthropicPriceFetchedAt`), run `task codex-pricing:sync`, and that a Codex change reaches runs only after a release and worker roll. Mention the nudge and the Health check.
- `task docs:sync` and commit the mirror; `specs/human.md`: one line for the price-coverage Health check, tagged `(AI-synced YYYY-MM-DD)`; `CHANGELOG.md` `[Unreleased]` line.

## Testing decisions

Red-before/green-after for each new behaviour.

- Agent: every existing `agent/test/codex-pricing.test.ts` case unchanged and green (proves the move changed no rate); new validator cases (negative rate, `2026-02-30`, missing `sources`, non-https source, extra field) each throw.
- API `codexprice`: byte-equality mirror test; loader rejects each malformed case; a `Coverage` differential table: unknown model gives `NoPrice`; `gpt-5.6-sol` at 2026-11-20 gives `Priced`, at 2026-11-21 and 2026-11-22 gives `PromoExpired`; `gpt-6.1-sol` gives `Priced`; `PricedModels` is sorted, includes `gpt-5.6-sol` on 2026-11-20 and excludes it on 2026-11-21.
- API `healthsvc` unit test with a fake Store (follow `healthsvc_test.go`): warn with both reason labels, the 10-row cap plus `and more`, ok, unknown on store error, memoization (two evaluations within 10 minutes call the Store once; after 10 minutes, twice; concurrent evaluations on an expired cache call it once; a store error after an `ok` yields `unknown`, not the stale `ok`).
- Live-DB test for the query in an existing `*LiveDB` package: claude rows excluded, cutoff boundary, priced filter, distinct-run count, ordering.
- Registry inventories and presentation tallies were updated from the planning baseline of 17 checks to the completed 18-check registry: `api/internal/healthsvc/scope_test.go`, `api/internal/healthsvc/healthsvc_test.go` (ordered `wantIDs`), `api/internal/handler/health_admin_livedb_test.go`, `web/src/mocks/data/health.ts`, `web/src/components/HealthOverviewCard.test.tsx`, `web/src/pages/AdminHealth.test.tsx` (per-group tallies). Every fake implementing the healthsvc Store interface gains the new method.
- Script: `scripts/pricing-freshness.test.sh` driven by `--today`: 30 versus 31 days, promotional 15/14/0/-1 days, Anthropic 30 versus 31 days, malformed JSON and a missing Go constant each exit 2. Wire it into `gate:repo` beside the existing `scripts/*.test.sh` entries.

## Milestones

### M1: One JSON Codex price source, and a freshness nudge for both tables

Blocked by: none.

The JSON move with validation, the `codexprice` mirror, sync task and equality test, `Coverage`, the nudge script and task with its test, the `run-cost.md` section, `docs:sync`, changelog.

Acceptance: examples 1, 2, 3, 5 and 6; every existing `codex-pricing.test.ts` case unchanged and green; `task gate:agent`, `task gate:api`, `task gate:repo`, `task check-docs:web` green.

### M2: Health warns on recent Codex usage of unpriced models

Blocked by: M1 (needs `codexprice.Coverage`).

The `pricing.codex` check, Store method and query, memoization, the registry inventory updates (Go and web), `docs/admin-health.md` row, `specs/human.md` line, `docs:sync`.

Acceptance: example 4; `uzi admin health --all` lists the check; `task gate:api`, `task gate:web`, `task check-docs:web` green.

## Completion and verification record

All functional work in M1 and M2 is complete. The following direct evidence
was supplied and verified by the lead. The final documentation unit also
passed repository, docs and embedded-mirror checks at `5b00e197`.

- [x] M1 completed at `2c5bc9e9`: canonical JSON and API mirror, validation,
  sync and byte-equality guard, coverage helpers, freshness nudge and docs.
  Full agent, API, repository and docs checks passed (log suffixes E1XVHf,
  73PkDR, jCpweD and S6dPbQ, each EXIT=0).
- [x] M2 backend completed at `665ba3d6`: seven-day fold query, distinct-run
  counts, filtering before the limit, reason labels, evidence cap, five-second
  query deadline and ten-minute cache for successful reads and failures.
  Full API passed (J4m1ew, EXIT=0). The full throwaway store integration run
  passed (of5dtT, EXIT=0), including
  `TestRecentUnpricedCodexModelsLiveDB`; 2465 tests passed, zero skipped.
- [x] M2 presentation completed at `6ca777575e2000f3d0877e8b1822a6becb026ddf`:
  18-check inventories, Health and Overview evidence with docs links, CLI
  bounded continuation rows in default attention and `--all`, JSON unchanged.
  Full API passed (so667s, EXIT=0), and full web passed, including docs
  (xD6MK7, EXIT=0). Focused CLI race tests, 48 web tests and typecheck passed.
- [x] Acceptance examples **1–3** directly verified by M1 hermetic freshness
  tests: Codex 30/31-day boundary, promotional 15/14/0/past boundaries and
  Anthropic 30/31-day boundary. Rates and recorded verification dates were
  preserved; these checks did not perform a new price verification.
- [x] Acceptance examples **5–6** directly verified by M1 mirror-drift and
  malformed-schema tests in the agent and API. Implemented validation also
  rejects malformed raw UTF-8 and unpaired Unicode surrogate escapes before
  replacement decoding, including overwritten string values; final decoded
  duplicate fields retain ordinary last-key-wins semantics.
- [x] Acceptance example **4** directly verified by health-service and CLI
  tests for both reason labels, warning and empty-result wording, plus the
  named live-DB test for harness/cutoff/filter/distinct/order behavior.
  Isolated SQL mutations removing DISTINCT or limiting before filtering
  compiled and made the actual named live test fail on expected rows; the
  baseline passed with zero skips. Cache TTL and copy-control mutations also
  failed as expected.
- [x] Final M2 documentation and records cover operator behavior, price-update
  guidance, instance scope, the human coverage decision and the Unreleased
  entry; the completed PRD is filed under prds/done.
- [x] Final M2 records validation at `5b00e197`: `task gate:repo`,
  `task check-docs:web` and uncached `TestEmbeddedDocsMatchSource` passed
  (log suffixes bFjUKo, FpLdCp and wGWunB, each EXIT=0).
- [x] **Shipped separately (PR #2595):** weekly maintainer-local workflow follow-up,
  including its bot-owned issue lifecycle; this PRD's own run changed no workflow file.
  The other exclusions above remain out of scope.

## Decision Log

- **D1. Hand-maintained, versioned tables; no live source.** OpenAI publishes no machine-readable rate catalog (checked 2026-10-09). Rejected: scraping pricing pages (fragile, and an automatic overwrite of a value that prices runs); a community price list (unofficial, new runtime dependency and egress).
- **D2. JSON as the single Codex source, mirrored into the api.** The nudge and the api both need the data and neither should parse TypeScript. Rejected: TS-parsing in the script; a second hand-kept Go table.
- **D3. Health compares observed models against this release's table.** It answers "is a model in use that we cannot price" with no schema change and no worker-protocol change. Rejected: a per-row unavailability reason on `run_usage` (schema change, fold precedence, old workers send none, and it still misses globally incomplete resume accounting); a priced-model catalog reported by workers at claim or heartbeat (more moving parts, no historical evidence).
- **D4. Age triggers review, never disables a rate.** A stale but correct rate keeps pricing; only an unknown model or a passed promotional date stays unavailable, as today.
- **D5. Health honours promotional expiry.** Otherwise `gpt-5.6-sol` runs would turn unavailable on 2026-11-21 while Health read ok.
- **D6. Memoize instead of indexing.** One 10-minute shared result bounds the scan without a migration. Rejected: a new `run_usage(updated_at)` index (a schema change for an advisory check).
- **D7. The weekly workflow is maintainer-local.** Worker PATs cannot push workflow files; including it would lose the whole branch at push.
- **D8. Per-model `verified_at`.** Adding one model must not make older rows look freshly checked.
- **D9. The nudge also covers the Anthropic table, by its existing date constant.** Same staleness problem, near-zero cost; the user chose to include it. Rejected: migrating it to JSON in this PRD.
- **D10. Human-approved instance scope and coverage record.** The approved final serial M2 plan explicitly extends the current instance enumerations with `pricing.codex`, superseding the older statement that every check outside `db`, `controller.report`, `loops` and `fleet.roll` is owner-scoped. The human requirements record describes read-only recent Codex coverage against the same release table, no rate fetch, warning findings and unknown query failures, both cached ten minutes per replica. (AI-synced 2026-10-09)
