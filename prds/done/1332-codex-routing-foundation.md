# PRD #1332: Dark Codex routing foundation (#1106 M5A)

**Issue**: [#1332](https://github.com/vtmocanu/uzi/issues/1332) · **Priority**: High (safety boundary before public routing)
**Parent**: [#1106](../1106-codex-harness-phase1.md), M5A only.
**Status**: M5A implemented and reviewed on branch agent/issue-1332; all six milestones complete, all gates green (gate:api, gate:agent, gate:repo, gate:web, ./e2e/run-store-it.sh) and the D7 dark negative inventory verified — Codex unreachable from every public path. Unblocks authoring M5B (undispatched).
**Depends on**: #1106 M1-M4, complete through PR #1309.
**Blocks**: M5B atomic public activation, then parent M6-M7.
**Area**: `api/internal/store`, `api/internal/workersvc`, `api/internal/capability`, `agent/src/codex`, worker registration and usage-accounting tests.
**Baseline**: `e19f93d0` (2026-09-13). Recheck symbols and assign migration numbers above the live head at merge.
**Scope guard**: Neither implementation nor validation may create, modify or commit `.github/workflows/**`. Existing CI reaches component gates through `Taskfile.yml`.

## Problem and outcome

Parent #1106 M5 combines data integrity, credential routing, worker compatibility, usage accounting, web UX, CLI parity and e2e enablement. Its preceding M3 and M4 children were large, and a backend-versus-frontend split would be unsafe: backend-first could expose Codex to direct API clients without the required selection and status surfaces, while frontend-first would send a request contract the API does not support.

Split at the public-enablement boundary instead. This child lands the schema, resolver contract, accounting and positive worker-capability negotiation while Codex stays dark. No public request, schedule, chat action, web control or CLI flag may select Codex after M5A. A later M5B child will wire the resolver into every approved run origin and atomically land the public API, web, CLI and offline e2e surfaces.

## Scope

### In

- Additive harness columns for runs, usage, users and schedules, with every current run-creation query explicitly preserving Claude.
- A pure, exhaustively tested D11 harness resolver that is not reachable from a public creation path in this child.
- A positive `codex_harness_v1` worker protocol capability and fail-closed claim placement for every Codex-indicating row.
- Codex per-thread/per-model token accounting, subscription semantics and API-key metered cost from a bounded, versioned price table.
- Live-DB proof for old-worker compatibility, claim placement, run-origin defaults and same-subscription concurrency.
- Negative acceptance proving that all public Codex selectors and response fields remain absent.

### Out

- Any public request field, route or service call that can select Codex; any `default_harness` or schedule-harness writer.
- Web start eligibility, Run Defaults, split Start, schedule pickers, badges, model/effort controls, cost presentation, failure copy or Codex mock scenarios.
- `uzi run create --harness`, schedule harness flags, or harness fields in CLI output.
- The Codex-selected compose e2e phase. It belongs to M5B with the public activation seam.
- Chart egress, fleet rollout, real provider calls, the real API-key repeat or dev-cluster acceptance. Parent M6 owns them.
- User documentation and #229 repointing. Parent M7 owns them.
- Chat execution on Codex, mixed-harness runs, local/custom endpoints, rate-limit meters or changes to the accepted M1 refresh architecture.

## Binding decisions

### D1: M5 splits at one atomic public-enablement boundary

M5A is dark foundation only. M5B depends on M5A and owns the first production path that can resolve, bind, claim or display a user-selected Codex run. M5B must merge its server-side routing, public contracts, web legibility, CLI parity and offline Codex e2e together. Do not split by backend and frontend, and do not enable a route in M5A on the theory that the web still hides it.

The web predicate is advisory, not a server control. `httpx.DecodeJSON` currently rejects unknown fields, and the issue-run request has no `harness` member. Preserve that structural rejection. A raw API client sending `{"harness":"codex"}` must still receive `400`; adding a request field that is later ignored is not dark.

### D2: Add schema now, write Claude at every production origin

Add the following columns with additive migrations and matching CHECK constraints:

- `runs.harness text NOT NULL DEFAULT 'claude' CHECK (harness IN ('claude','codex'))`;
- `run_usage.harness text NOT NULL DEFAULT 'claude' CHECK (harness IN ('claude','codex'))`;
- `run_usage.cost_status text NOT NULL DEFAULT 'metered' CHECK (cost_status IN ('metered','subscription','unreported'))`, with non-metered rows constrained to the numeric storage placeholder `cost_usd=0`;
- `users.default_harness text NULL CHECK (default_harness IN ('claude','codex'))`;
- `run_schedules.harness text NULL CHECK (harness IN ('claude','codex'))`.

Migration numbers in this PRD are intentionally unspecified. Assign the next free numbers above the live head at merge. Follow the repository's `NOT VALID` plus sibling `VALIDATE CONSTRAINT` pattern where validation can scan a populated table. Backfill any pre-existing Codex-bound row using M1's deletion-proof sentinel (`codex_material_revision IS NOT NULL`), not `codex_secret_id`, which the alias FK nulls on deletion. Then add and validate a coherence CHECK requiring `harness='codex'` whenever either `codex_material_revision` or `codex_secret_id` is non-NULL. All other historical runs remain Claude; a Codex row may legitimately retain `harness='codex'` after its alias ID becomes NULL.

`run_usage.harness` and `cost_status` are descriptive, not part of its primary key. One run has one immutable harness; adding it to `(run_id, session_id, model, lineage_epoch)` would merely permit duplicate rows for an impossible harness transition. Derive the inserted usage harness from the persisted `runs.harness`, never from a worker-supplied field. Existing rows backfill to `cost_status='metered'`, preserving Claude's provider-reported accounting. In C1, the fold defaults from the persisted harness and ignores an untrusted marker: Claude is metered under its existing provider-reported `costUSD`, while every Codex row is unreported until C4b validates a closed marker.

Define `UpsertRunUsage` conflict behavior explicitly. Equal statuses retain that status; any disagreement or any existing/incoming `unreported` resolves to `unreported`. The resulting `cost_usd` is `GREATEST(existing,incoming)` only when the resulting status is `metered`, otherwise zero. This prevents a redelivery from combining an unreported status with a positive metered number and violating the CHECK or inventing completeness.

Replace `run_usage_totals` so it exposes the new dimensions and follows the same conservative fold: all metered rows sum as metered, all subscription rows report subscription with numeric zero, any unreported row or mixed metered/subscription rows make the total unreported. C1 also widens every internal cross-run cost query to return metered cost separately plus `subscription_run_count` and `unreported_run_count`; existing numeric totals may remain for compatibility but cannot represent completeness. M5A does not add those counts to public API DTOs or contract fixtures, so its production response shape stays unchanged while only Claude runs are publicly creatable. M5B adds and consumes the public fields in the web Run Usage/admin/dashboard surfaces, CLI/TUI output and judge-cost context rather than presenting a partial sum as total.

List `harness` explicitly in every current production `INSERT INTO runs` statement and pass literal/resolved `claude` from every production origin in M5A. Do not rely on the database default: the silent case is an INSERT whose column list was never updated, where the database default masks that omitted origin. Pin every origin with a `LiveDB`-suffixed test. C1 also owns every store read/upsert query that C3 and C4 consume and regenerates sqlc once; in Phase 2, C2 is the only author allowed to edit `runtime.sql` or generated store code. The two nullable preference columns receive no public writer and remain NULL outside direct test setup.

`FreezeCodexBinding`, retained for internal conformance and test-server paths, must atomically mark its row `harness='codex'` with the binding, but it gains no handler or route. Keep `harness` out of `FreezeRunCodexBinding`'s exact-unchanged retry comparison: an idempotent replay still matches the original M1 snapshot while the same guarded UPDATE reasserts Codex. The three raw test-server inserts in `api/cmd/codexm3btestserver/main.go` may begin from the Claude default only because every path immediately reaches that atomic freeze; C1 tests all three resulting rows as Codex. They are separate from the twelve sqlc production insert origins.

### D3: Old workers fail closed through positive protocol capability negotiation

Add `codex_harness_v1` to the worker **protocol** capability vocabulary, beside the existing protocol-version capabilities. Do not add it to the user-selectable scheduler vocabulary (`docker`, `jvm`) or to `required_capabilities`.

Extend the shared install/assert path to leave a root-owned runtime receipt beside `/opt/uzi-codex/0.153.2/`, identically in both base and JVM images. The receipt binds the lock digest, architecture, exact `codex-cli 0.153.2` version, and SHA-256 plus mode/path for the installed binary and required package members that the build already verified; it contains no credential or mutable HOME state. At startup, validate the receipt, stat the absolute members and recompute their digests without executing Codex, searching PATH, starting app-server or using the network. Advertisement follows only a successful probe. A stripped, hand-built, corrupt or mismatched image omits the capability and continues serving Claude; an old image omits it naturally. The receipt/image change reaches the fleet only through parent M6's worker-tag roll, which is expected and cannot be mistaken for M5A public activation.

A row is Codex-indicating when `runs.harness='codex'`, `codex_material_revision IS NOT NULL` **or** `codex_secret_id IS NOT NULL`. The material revision is M1's deletion-proof binding sentinel; the alias FK may null only the secret ID. Any Codex-indicating row is claimable only by a worker advertising `codex_harness_v1`. Checking all three fails closed on inconsistent or legacy internal data instead of silently sending its Codex credential block to an old worker that ignores the unknown JSON and runs Claude.

Claim assembly treats `runs.harness` as authoritative. A Codex harness enters the Codex assembly branch even when `codex_secret_id` was nulled by alias deletion; an unavailable/incomplete binding returns `errCredentialUnavailable` and never emits a Claude claim or falls back to an Anthropic token. A coherence violation indicated by either M1 sentinel also fails closed. Pin alias deletion with a `LiveDB` regression: a capable worker may claim the row only into the existing terminal credential-unavailable path, never execute Claude.

Mirror the placement predicate in all three decisions:

1. claimant admission;
2. the fleet-spread peer subquery, so an incapable peer cannot reserve the run by being preferred;
3. fleet-availability/queued-reason resolution.

This protocol gate sits outside the `capability_aware` setting, `required_capabilities`, and owner clearing paths. Neither a kill-switch nor `ClearRunRequiredCapabilities` may bypass it. An old worker must continue to claim ordinary Claude runs with its current byte-compatible payload. Vocabulary removal is a calibrated failure case: because `FilterProtocol` silently drops unknown strings, removing `codex_harness_v1` must make the capable-worker claim test fail. The queued-reason path uses the existing prose-constant style, with a fixed user-visible explanation such as `no Codex-capable worker is online`, rather than an internal-looking code or indefinite generic wait. It is reachable only for internal Codex rows in M5A.

### D4: Implement D11 as a tested server contract, not a reachable route

Implement one pure resolver for M5B to consume. It covers the parent D11 order exactly:

1. an explicit harness is used or returns `no_credential_for_harness`;
2. otherwise use a usable user default;
3. otherwise use the sole available harness;
4. otherwise choose Claude when both are available and no usable default exists;
5. no usable credential retains the existing refusal/failure behavior.

Codex credential selection then uses M1's named-default, immutable binding and auth-mode rules. Explicit selection never falls back to another harness, and a failed or missing subscription never spends an API key. Multiple imports of one canonical account share refresh coordination without acquiring a run-long lock.

M5A exercises every resolver branch through unit and live-store fixtures only. Every production caller still supplies/writes Claude directly; run creation, chat `start_run`, prompt/sweep schedule fire and all other run kinds do not call a Codex-producing branch. The public request and response DTOs, API-contract fixtures, web API types and CLI commands gain no harness member here. M5B owns the integration and error mapping.

### D5: Account Codex usage without inventing cumulative or price semantics

The pinned 0.153.2 app-server protocol exposes `thread/tokenUsage/updated` with `threadId`, `turnId` and `{total,last,modelContextWindow}`. Each usage breakdown contains `inputTokens`, `cachedInputTokens`, `cacheWriteInputTokens`, `outputTokens`, `reasoningOutputTokens` and `totalTokens`. This is established in the pinned source at commit `657a993cbee87acf52d14b758ce49dbd46d1b8eb`, not inferred from the current loose numeric `turn.usage` decoder:

- [app-server v2 usage notification and breakdown](https://github.com/openai/codex/blob/657a993cbee87acf52d14b758ce49dbd46d1b8eb/codex-rs/app-server-protocol/src/protocol/v2/thread.rs)
- [core token usage types](https://github.com/openai/codex/blob/657a993cbee87acf52d14b758ce49dbd46d1b8eb/codex-rs/protocol/src/protocol.rs)
- [Responses usage conversion](https://github.com/openai/codex/blob/657a993cbee87acf52d14b758ce49dbd46d1b8eb/codex-rs/codex-api/src/sse/responses.rs)

M5A must add an immutable `threadId -> configured model` accounting table: the production registry does not currently retain model identity. Capture the root mapping at construction and the child mapping when `spec.model ?? provider.model` is selected and the child thread ID is returned. Unknown, stale or unregistered threads cannot be attributed to the root model by guess.

Track cumulative snapshots per authorized thread. Charge/report only non-negative deltas after each root or child thread's starting snapshot. Use `last` for one upstream response's pricing and `total` for replay/dedup/recovery checks; never sum repeated cumulative totals. A duplicate, stale resume replay or out-of-order notification cannot increase usage. A gap where the cumulative delta cannot be reconciled to observed response records retains recoverable token totals but marks dollar cost unreported rather than fabricating a price. Root completion already waits for required child/callback settlement; usage finalization must use that settled thread set.

Aggregate the reconciled records by actual configured model and emit the existing result-frame `modelUsage` shape extended with a closed `costStatus` marker. `costUSD` is present only for metered records. For backward compatibility, a Claude row with the existing numeric `costUSD` and no marker resolves to metered; a Codex row with a missing, unknown or inconsistent marker resolves to unreported. `foldRunUsage` persists both the run-derived harness and the conservative cost status so a numeric storage placeholder cannot be presented as real zero. Preserve the lineage-epoch idempotency contract: redelivery of the same result frame must land in the same row, and a fresh turn/worker leg must not overwrite a prior turn's usage. Reasoning tokens are a subset of output and are never added twice.

Subscription usage keeps neutral `HarnessCost{kind:'subscription'}` semantics and persists `cost_status='subscription'`. Its numeric dollar placeholder is zero only because no per-token charge exists for that credential mode; readers use the status and do not relabel it as metered zero. API-key usage uses `HarnessCost{kind:'metered', source:'price_table'}` only when every required bucket, model, service tier and per-response threshold is known. Otherwise it persists `cost_status='unreported'`; tokens remain, but the numeric placeholder is never exposed as `$0`.

The initial versioned table is `openai-standard-2026-09-13`, verified against the official [API pricing table](https://developers.openai.com/api/docs/pricing) and model pages on 2026-09-13. Rates are USD per one million tokens:

| Model | Context | Uncached input | Cached input | Cache write | Output |
|---|---:|---:|---:|---:|---:|
| `gpt-6-astra` | input <= 272K | 10.00 | 1.00 | 12.50 | 50.00 |
| `gpt-6-astra` | input > 272K | 20.00 | 2.00 | 25.00 | 75.00 |
| `gpt-5.6-sol` | input <= 272K | 4.00 | 0.40 | 5.00 | 20.00 |
| `gpt-5.6-sol` | input > 272K | 8.00 | 0.80 | 10.00 | 30.00 |

The >272K threshold applies to total input tokens for one upstream response and changes the rates for that entire response. Compute uncached input as `max(input - cached - cache_write, 0)` after validating that the two detail buckets do not exceed input. Output already includes reasoning. Uzi does not request Batch, Flex or Fast service tiers in phase 1; any observed non-standard/unknown tier is unreported, not priced as Standard.

The official `gpt-5.6-sol` page says its promotional pricing is available at least through 2026-11-21. Record that review boundary with the table. After that date, that model's API-key cost becomes unreported until a maintainer re-verifies and versions the row; M5B must recheck the official table before public API-key activation. A stale or unknown table must never silently report zero or an old dollar amount.

### D6: Concurrent subscriptions remain concurrent

M5A adds no seat-held state, unique index, whole-run claim predicate or run-long lease. Two runs bound to one canonical subscription, including two separately saved imports, can be active concurrently subject only to existing worker/user capacity. M1's short refresh-operation coordination, revisions, CAS/idempotence and recovery remain the only subscription-specific serialization.

Prove this with separate live database transactions. A test that creates two rows serially without overlapping claims is not discriminating. Calibrate the claim and usage protections with restored one-at-a-time mutations, watching the named regression fail for the intended reason before the final green run.

### D7: Dark means structural, tested and workflow-free

M5A acceptance includes a negative inventory, not a promise:

- public create and schedule JSON containing `harness` is rejected as unknown;
- no harness or cost-status/count field appears in public DTOs, API-contract fixtures or `web/src/lib/apiTypes.ts`;
- no CLI `--harness` flag or output column exists;
- the M5B handoff names every reader that must consume cost status/counts: web Run Usage, admin usage and dashboard cards, CLI/TUI run output and judge cost context;
- `hasAnthropicToken`, `anthropicTokenCount` and `startRunGate` remain unchanged;
- `FreezeCodexBinding` has no public route;
- the only production `agent/` changes are the shared runtime receipt/probe plus protocol-capability advertisement, and usage normalization/accounting;
- no `.github/workflows/**` path appears in the child diff.

A test-only internal Codex-bound row may be claimed by a capable worker to prove the dark foundation. That is not public enablement.

## Verified implementation anchors

These are repository-local facts at baseline and may be rechecked offline:

- All four harness columns are absent. Current migration head is `00223_recovery_archive.sql`.
- M1's `FreezeCodexBinding` (`api/internal/workersvc/codexauthz.go`) has no public caller; non-test calls are confined to the M3b test server. Its SQL deliberately uses `codex_material_revision`, not the deletable secret ID, as the initialized sentinel.
- Claim assembly currently keys only on `run.CodexSecretID.Valid`. The agent selects Codex only when `claim.secrets.codex` exists; therefore a deleted alias can currently make a previously Codex-bound row assemble and execute as Claude unless M5A changes assembly to trust `runs.harness` and fail closed.
- Worker protocol capabilities flow from the static list in `agent/src/worker.ts` through registration into `workers.protocol_capabilities`; no current boot probe verifies the out-of-PATH Codex binary before advertisement. The build-time lock/assert helpers are deleted after install, so C2 must intentionally retain the new root-owned runtime receipt. `completion_interlock_v1` supplies the claimant, peer-spread and fleet-count precedent in `api/internal/store/queries/runtime.sql`.
- There are twelve sqlc `INSERT INTO runs` statements across issue, chat, CI-fix, MR-rework, judge, self-improve, prompt/schedule and task query files. Three additional raw inserts live in the M3b test server and then call `FreezeCodexBinding`.
- The production registry holds thread authority but no thread-to-model map. Child creation selects a model and receives a thread ID without retaining that association.
- `normalizeCodexUsage` currently keeps a bounded numeric wire object but leaves neutral tokens empty. The transport treats `thread/tokenUsage/updated` as generic activity, the reducer emits no `modelUsage`, and `foldRunUsage` skips Codex result frames entirely.
- `run_usage.cost_usd` is non-null with default zero and has no semantic marker; `run_usage` is keyed by `(run_id, session_id, model, lineage_epoch)`. The fold uses persisted `init` frame count and `GREATEST` to deduplicate one iteration leg.
- `./e2e/run-store-it.sh` runs only tests whose names end in `LiveDB`, and only from `store`, `handler`, `forgesvc`, `schedsvc` and `workersvc`. Put every new live test in those existing packages with that exact suffix; adding a package would also require a forbidden workflow edit.

## Milestones and dependency plan

Each milestone records exact commands, revision, exit status and named tests. Run each component gate once per unchanged tree to a log. C1 owns all migrations, every store query C3/C4a/C4b consume, and the initial generated sqlc output. During Phase 2, C2 is the only author allowed to edit `runtime.sql` or generated store code; C3 and C4a consume the frozen C1 query surface so their parallel ownership is real.

| Phase | Milestone | Depends on | Files/ownership | Repo |
|---|---|---|---|---|
| 1 (sequential) | C1: additive harness/cost schema and explicit Claude origins | M1-M4 | migrations, all production run INSERTs, resolver reads, usage upsert/view, generated store code, store `LiveDB` tests | uzi |
| 2 (parallel) | C2: positive worker capability and old-worker claim/assembly safety | C1 | shared installer + both worker templates' runtime receipt, capability probe/advertisement, claim/peer/fleet predicates, claim assembly, worker-skew and claim tests; sole Phase-2 store-query writer | uzi |
| 2 (parallel) | C3: pure D11 resolver contract, still unreachable publicly | C1 | new workersvc resolver/tests consuming C1 reads; M1 credential-selection fixtures only | uzi |
| 2 (parallel) | C4a: Codex token decode, reconciliation and attribution | C1 | transport notification, thread-model accounting map, reducer projection, token-only agent/API/fixture tests; no store-query edits | uzi |
| 3 (sequential) | C4b: cost semantics and versioned pricing | C1 + C4a | cost marker/fold, price table, rollup behavior, pricing and stale/unreported tests | uzi |
| 4 (sequential) | C5: integrated dark/concurrency acceptance and handoff | C2-C4b | live-DB integration, negative public-surface inventory, Taskfile only if existing targets need composition, child/parent progress | uzi |

- [x] **C1: Additive schema and explicit Claude preservation.** Add/validate the five columns and binding-coherence CHECK under D2; backfill harness from `codex_material_revision` and old cost status to metered. Keep the usage PK unchanged, implement conservative row-conflict and run-rollup status rules, add cross-run subscription/unreported counts, and regenerate sqlc. Add all resolver reads and the widened usage upsert now; its initial harness-based fold forces every Codex row to unreported and preserves Claude as metered. Update all twelve production run inserts to write Claude explicitly; make `FreezeCodexBinding` atomically set Codex without changing its exact-retry comparison, and cover all three raw test-server flows. A table-driven `LiveDB` test reaches every creation query and proves `runs.harness='claude'`; test deleted-alias backfill, preference NULL defaults, run-derived usage harness, status-conflict dominance, cross-run counts and old Claude accounting. Migration numbering/additivity and generated-code no-diff checks are mandatory. Gates: `task gate:api`, `task gate:repo`, `./e2e/run-store-it.sh`.
- [x] **C2: Positive capability and fail-closed placement/assembly.** Retain the root-owned lock/member-digest receipt from the shared installer in both worker templates, add `codex_harness_v1` as protocol-only vocabulary, validate receipt/member stat and digests without executing Codex before advertising, and enforce D3 at claimant, peer-spread and fleet-count sites. A failed probe omits only Codex capability and leaves Claude service available. Make claim assembly follow `runs.harness`; deleted or unusable Codex bindings take the terminal credential-unavailable path and never receive an Anthropic token. Extend old-worker skew and claim-placement `LiveDB` tests: empty-capability workers never receive Codex-indicating rows, cannot reserve them as preferred peers, and still claim Claude byte-compatibly; a capable worker can claim the same internal Codex fixture; alias deletion never changes harness. Assert the queued reason is the fixed prose `no Codex-capable worker is online`; prove the capability setting and owner-clear paths cannot bypass the gate. Calibrate claimant, peer, fleet-count, assembly fallback and protocol-vocabulary removal separately. Gates: `task gate:agent`, `task gate:api`, `./e2e/run-store-it.sh`.
- [x] **C3: Exhaustive resolver contract without activation.** Implement the pure D11 resolver against C1's read queries and all credential/default/deletion/mismatch combinations, including explicit no-fallback, sole-harness choice, both-with-null-default Claude choice, canonical duplicate imports and missing named credential. Keep it uncalled by public origins. Add negative request-contract tests proving `harness` remains an unknown field for issue starts and schedules; inventory chat/start-run and schedule-fire fan-in for M5B. Any database-backed case ends in `LiveDB`. Gates: `task gate:api`, `./e2e/run-store-it.sh`.
- [x] **C4a: Codex token accounting, cost deliberately unreported.** Decode `thread/tokenUsage/updated` as a typed notification rather than generic activity. Build the missing immutable root/child `threadId -> model` map, reconcile per-thread `total`/`last` across replay and resume, and aggregate settled root/child deltas by model into the result-frame shape. Every Codex entry carries `costStatus='unreported'` in this milestone; do not smuggle pricing into C4a. Distinguish per-response, turn and cumulative totals with duplicate, out-of-order, missed-update, resumed-thread, unknown-thread, mixed-model-child and result-redelivery fixtures. Persist token rows through C1's upsert and re-record cross-module usage fixtures from test output, never by hand. Gates: `task gate:agent`, `task gate:api`, `task gate:web`, `./e2e/run-store-it.sh`.
- [x] **C4b: Subscription and API-key cost semantics.** Add the closed `costStatus` projection/fold from D5. Subscription rows become `subscription`; fully observed supported Standard API-key responses use the versioned price table and become `metered`; every unknown/stale/inconsistent case stays `unreported`. Cover each cache bucket, the per-response >272K boundary, Sol review date, unknown model/tier, mixed status, missing response update and rollup dominance. Calibrate removal of the marker and unreported-dominance rule before the final green. Gates: `task gate:agent`, `task gate:api`, `task gate:web`, `./e2e/run-store-it.sh`.
- [x] **C5: Concurrency and dark acceptance.** Prove with overlapping separate transactions in `LiveDB` tests that two same-account subscription runs can be active without a seat lock and retain M1 refresh safety. Run the D7 negative inventory and calibrated positive controls. Confirm the branch changes no public DTO/fixture/web/CLI selector, no existing Claude result or claim payload, no user-visible behavior and no workflow file. Run final gates once on the joined revision and record evidence. M5A completion unblocks authoring M5B; it does not dispatch, enable or deploy M5B. Gates: `task gate:api`, `task gate:agent`, `task gate:web`, `task gate:repo`, `./e2e/run-store-it.sh`.

## Success criteria

1. Every current production run origin writes `harness='claude'`; historical ordinary rows read Claude and any internal Codex-bound row is coherently marked Codex. Existing Claude usage remains `cost_status='metered'`.
2. A Codex-indicating run cannot be claimed, reserved through peer preference or counted available by a worker lacking a successful lock-bound `codex_harness_v1` startup probe. Claude claims remain byte-compatible, and the incapable-fleet reason is explicit.
3. The D11 resolver passes the complete credential/default/explicit-selection matrix, while every public route, DTO, web type and CLI still lacks a Codex selector.
4. Codex root and subagent usage creates idempotent per-model `run_usage` rows with persisted harness and cost status. Resume/replay cannot double count, and missing evidence cannot fabricate tokens or dollars.
5. Subscription usage persists and rolls up as `cost_status='subscription'`, not metered zero. A fully observed supported API-key response persists and rolls up as `metered` with the versioned Standard cost, including cache and >272K rules; unknown model/tier, stale promotional table or irreconcilable records remain `unreported` and are never persisted or aggregated as metered zero. M5B owns presentation.
6. Two runs bound to the same canonical Codex subscription can be active concurrently, including duplicate saved imports; no run-long seat lock exists.
7. A direct API request cannot bypass the dark boundary. Codex selection remains impossible outside internal tests until M5B.
8. No `.github/workflows/**` file changes in implementation or validation. All required gates and live-DB positive controls pass on the joined revision.

## Risks and stop conditions

- **Silent sqlc omission**: one unmodified insert compiles and falls back to Claude. C1's live matrix must name every insert origin; a count without paths is insufficient.
- **Incomplete placement mirror**: claimant-only gating can let an incapable preferred peer reserve the run forever. All three D3 sites and mutations are mandatory.
- **Usage replay ambiguity**: cumulative totals, per-response `last` and resumed-thread replay are different facts. If the pinned protocol cannot reconcile them without double counting, stop and revise the accounting key before emitting `modelUsage`.
- **Mixed-model attribution**: aggregate usage without the new thread-to-model binding cannot be priced. Preserve tokens as unreported rather than assigning them to the root model.
- **Semantic zero collapse**: numeric `cost_usd=0` cannot distinguish subscription from missing pricing. `cost_status`, the result-frame marker and unreported-dominant rollup are one C1/C4 contract; omitting any part blocks acceptance.
- **False capability advertisement**: a static capability claim can outlive or misrepresent its image contents. Probe the pinned absolute binary/layout before registration and omit capability on any mismatch.
- **Stale pricing**: the table is a dated input, not timeless truth. Unknown tiers/models and the Sol review boundary fail to unreported; M5B cannot claim API-key cost acceptance until a maintainer re-verifies it.
- **Partial activation**: any public harness request field, preference writer, `FreezeCodexBinding` route, web/CLI selector or Codex-producing resolver call is an M5B change and blocks M5A acceptance.
- **Workflow-scope trap**: use existing Task targets and in-memory/synthetic fixtures. A real workflow path in the branch diff blocks finalize.

## Progress log

- 2026-09-13: The user approved splitting parent M5 into two sequential children at the public-enablement boundary after M3/M4 proved too large. Child #1332 owns only the dark foundation; M5B will own the atomic server/web/CLI/e2e activation. Positive `codex_harness_v1` protocol negotiation is the selected old-worker strategy. No implementation run has been dispatched.
- 2026-09-14: M5A implemented on branch `agent/issue-1332`. All six milestones (C1–C5) landed and reviewed clean; every gate green and the D7 dark negative inventory verified (no public harness/cost selector in DTOs/apiTypes/CLI, `FreezeCodexBinding` unrouted, no `.github/workflows/**` change, the D11 resolver has zero production callers). **M5B reader handoff** — the internal store/query fields M5B must surface (all present internally, absent from public DTOs): per-run `cost_status` and lifetime/last-7 `subscription_run_count` / `unreported_run_count`, consumed by the web Run Usage panel, admin usage + dashboard cards, CLI/TUI run output, and judge cost context; and the D11 resolver (`workersvc.resolveRunHarness`) to be wired into every run-creation fan-in (issue create, chat `start_run`, prompt/sweep schedule fire, self-improve, CI-fix, MR-rework, judge, task/then-fix/task-review), with `FreezeCodexBinding` still gaining no public route. **Landing collision with #1344's run_scope migration, reconciled** — this branch's migrations, drafted as `00224/00225/00226`, collided with main's #1344 run_scope migration pair (merged as `00224_run_scope_reduced_stop_kind.sql`/`00225_validate_run_scope_reduced_stop_kind.sql`). At the landing rebase onto the merged tree, this branch's three migrations were renumbered above that live head to `00226_harness_cost_status.sql`/`00227_validate_harness_cost_status.sql`/`00228_run_usage_totals_cost_status.sql`, sqlc was regenerated against the merged schema, and #1344's edits to the shared files (`runtime.sql`, migrations dir, generated sqlc, contract fixtures) were adopted verbatim rather than reverted.
