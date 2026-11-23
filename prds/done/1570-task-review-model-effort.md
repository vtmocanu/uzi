# PRD #1570: Configurable task-review model and effort per harness

**Issue:** #1570  
**Parent:** #1703 (consistent model configuration across Claude and Codex).  
**Status:** Done, 2026-11-22: M1 and M2 implemented on branch `agent/issue-1570` (one PR). Draft r4 was written against main `0095b4361` (r2 folded the independent architect review; r3 records the owner's decision D4; r4 folds the buddy's resolver review). Full suites remain exact-head PR CI (Landing obligations).
**Priority:** Normal.  
**Blocked by:** none.  
**Delivery:** one PRD, one uzi run, one PR, two serial milestones. Use the in-run/CI boundary in AGENTS.md *Run economy*. Neither implementation nor validation touches `.github/workflows/**`. Landing allocated `00509_task_review_model.sql` and `00510_task_review_effort.sql`; run `task migration:renumber` if main advances before landing.

**Scope gate:** one independently valuable outcome: an owner can choose which model and reasoning effort the optional handoff review pass uses on each harness, and see it in the effective-settings view. An unset review model keeps today's built-in; an unset review effort follows the owner's worker effort for that harness (owner decision D4). Model and effort are separate slices because each is useful alone.

## Problem

`uzi handoff --review` queues a read-only review of the task run's diff. The owner cannot choose its model or effort:

- **Codex** reviews always run on the curated `CODEX_TASK_REVIEW_MODEL = "gpt-6-sol"` (`agent/src/codex/task-review-model.ts`), selected in `reviewCodex()` (`agent/src/review-runner.ts` ~L417-454).
- **Claude** reviews send no model, so the SDK default runs (`review()` / `runModel()`, `review-runner.ts` ~L393-410, ~L456-471; `ClaudeTaskReviewModel = ""`, `api/internal/agenttmpl/worker_default.go` L12-18).
- **Neither** sends an effort. Both review claims already carry `config.default_effort` (the owner's ordinary effort), and a Claude review also carries `config.default_model` (the Claude lane) (`claim_assembly.go` ~L451-561; `ClaimConfig`, `claim.go` ~L674-709). The review runner ignores both (`review-runner.ts` `review()` / `reviewCodex()`; the ~L430-433 comment covers the Codex case).
- The effective-settings view shows `task_review_model` / `task_review_effort` cells for both harnesses marked `not configurable` / `not sent` (`api/internal/handler/effective_settings.go` `reviewModelCell` ~L407-421, `reviewEffortCell` ~L424-428).

## Verified baseline (main `0095b4361`)

- A task review is `runs.kind='task'` with `review_target_run_id` set (`trigger_source='task_review'`, migration 00180). It inherits the target's harness (`maybeEnqueueTaskReview`, `api/internal/workersvc/judge_enqueue.go` ~L383-431). `isReviewRun := run.ReviewTargetRunID.Valid` (`claim_assembly.go` L439); the Codex review branch skips the lane read (L451).
- `agent/src/model-pass.ts` `runReadOnlyModelPass` already accepts `model?` and `effort?: EffortLevel` (L60-63) on both the Claude (L469-470) and Codex (L585-586) paths.
- Precedent for a per-claim model field: the judge reads `claim.judge_model` (`judge-runner.ts` L522, L563), a top-level claim field (`claim.go` L347-349, set at `judge.go` L1289).
- Per-harness model lanes on `users`: `judge_model` / `codex_judge_model`, `summary_model` / `codex_summary_model`; written through `PUT /api/me/model-settings` (closed `modelSettingsAllowlist`, `api/internal/handler/model_settings.go` L33-44, one transaction under `LockUserSettingsRow`) and `PUT /api/me/settings`. Cells via `judgeModelCells` (`model_setting_cells.go` L21-42).
- Stored effort precedent: `planning_claude_effort` (00498), NULL = inherit, validated by `agenttmpl.ValidateEffort` (five levels, rejects `runtime`). `ValidateDefaultEffort` additionally admits `runtime` (no level sent, `EffortRuntime`).
- Validators: Claude lane `ValidateClaudeLaneModel` (custom IDs allowed, Codex role IDs refused). Codex summary lane `validateCodexSummaryModel` accepts `CodexRoleModels` (curated plus `gpt-6-luna`), pinned to `fixtures/codex-models/models.json`.
- The Codex advice renderer is closed-curated: `resolveModel` (`agent/src/codex/render.ts` ~L332-346) drops an advice model outside `CODEX_ROLE_MODELS` with an `unknown_model` diagnostic; `resolveEffort` (~L351-360) drops anything but the five levels with `unknown_effort`.
- Failure today: a model error inside `review()` / `reviewCodex()` becomes `fallbackTaskReview(...)`, a posted review with `status:"failed"` and summary "The reviewer model call did not complete: …"; the run still completes (`review-runner.ts` ~L360-366, ~L399-409, ~L534-536).
- CLI: `modelSettingSpecs` (`api/cmd/uzi/settings_model_write.go` L57-67) holds `worker-model`, `effort`, `judge-model`, `summary-model`, `judge-runs-on`; labels in `modelSettingLabels` (`settings_models.go` L28-78) are pinned both ways to `fixtures/model-settings/contract.json` `source_labels`. `fixtures/model-settings/task-review.json` pins the built-ins (`TestTaskReviewBuiltinsMatchFixture`, `agent/test/task-review-model.test.ts`).
- Capability precedent: `template_effort_v1` (`api/internal/capability/capability.go` ~L404-408): the api withholds the new key from a worker that lacks it and records a feed notice; the run proceeds (`claim_assembly.go` ~L510).
- Web: `web/src/pages/RunDefaults.tsx` saves through `api.putMySettings`; judge and summary sections read `judge_models` / `summary_models` cells.
- Docs: `docs/worker-model.md` ~L322-328 ("Task review uses its own built-in model", cites #1570), `docs/worker-effort.md` ~L142-144, `docs/configuration.md` ~L439-456, `docs/cli.md` ~L3628-3629 and the `uzi settings set` list, `docs/handoff.md` ~L92-98.

## Outcome

On **Run defaults**, a new **Task review** section has, per harness, a model field and an effort select. An empty model keeps today's built-in; an empty effort follows your worker effort for that harness. The CLI sets the same values. The worker sends the chosen model and effort to the review model call, and the effective-settings view shows what a review will use with its source.

Acceptance examples:

1. An owner sets the Codex review model to `gpt-6.1-sol` and effort `high`. Their next Codex task's `--review` pass calls the model with `gpt-6.1-sol` / `high`. In `uzi settings get` (columns `SETTING HARNESS STORED DEFAULT RESOLVED SOURCE`, `settings_models.go` ~L23) the `task review model` / `codex` row resolves `gpt-6.1-sol` with source `your choice`, and the `task review effort` / `codex` row resolves `high` with source `your choice`.
2. An owner sets no review settings and has worker effort Claude `high`, Codex unset. A Claude review sends no model and effort `high`; a Codex review runs `gpt-6-sol` with the built-in worker effort `medium`. An owner whose Codex worker effort is Runtime default gets no effort on a Codex review. The effective review effort cells show the inherited value with the worker effort's source.
3. An owner types `gpt-6-astra` into the Claude review model field. The PUT is refused with the existing Claude-lane validation message; nothing is stored. A Codex value outside `CodexRoleModels` is refused likewise.
4. A review claimed by a worker that predates this PRD runs on the built-in model and effort, and the run feed carries a notice naming the withheld setting.

## Out of scope

- A one-run review override on `uzi handoff` (flags or web). Later, if asked for.
- An instance-wide (admin) review default tier.
- Changing judge, summary, cross-check or PR-description helper models or efforts; they stay separate settings.
- Custom (non-curated) Codex review models: the advice renderer is closed-curated.

## Modules and seams

- **Storage** (`users`): four nullable text columns, `task_review_model`, `codex_task_review_model` (M1 migration) and `task_review_effort`, `codex_task_review_effort` (M2 migration). No CHECK, backfill or index (validation is in the write path, as for the planning pair). Down drops them.
- **Write API**: `PUT /api/me/model-settings` allowlist keys `task_review_model`, `codex_task_review_model`, `task_review_effort`, `codex_task_review_effort` (new `modelSettingsWrite` fields, `steps` entries and `q.SetUser...` calls in `applyModelSettings`), plus `PUT /api/me/settings`, with shared decoders in `settings_decode.go`. Tri-state per field: omitted = unchanged, `null`/blank = clear, value = set. Validation: Claude model `ValidateClaudeLaneModel`; Codex model `agenttmpl.CodexRoleModels`; effort `agenttmpl.ValidateDefaultEffort` (five levels or `runtime`; `runtime` stores `runtime` and sends no level even when the worker effort is set; blank or null clears, so the review effort inherits again). Invalid input → 400 with the existing field-error shape, nothing written (one transaction).
- **Read API**: `userSettingsDTO` adds the four raw fields plus cell arrays `task_review_models` / `task_review_efforts`, as with `planning_claude_effort` + `planning_efforts`. Update `fixtures/api-contract/user_settings.{full,zero}.json` and `settings_user_settings.{full,zero}.json` (`contract_test.go` ~L80, `web/src/lib/apiContract.test.ts`), `web/src/lib/apiTypes.ts` `UserSettings`, and the mock's `mySettingsResponse` (`web/src/mocks/mockApi/settings.ts` ~L724). `fixtures/lead-config` needs no change (no review case); `fixtures/model-settings/task-review.json` is unchanged (built-ins do not change).
- **Effective view**: `predictLead(..., review=true)` gains a read of the four columns; `reviewModelCell` / `reviewEffortCell` report the review model with source `user_default` or `builtin`. Review effort cell: `Effort` is the stored review preference (NULL when inherited); `DefaultEffort` / `DefaultSource` describe the ordinary worker effort that a reset selects; `ResolvedEffort` / `Source` / `Mode` describe the final review resolution (the claim's rule; no lead-template tier, which applies to the lead only). Reuse the existing `model`/`effort` source labels; no new `modelSettingLabels` rows. Review model cells drop the `not_configurable` note entirely; an unset Claude model cell keeps `sdk_default`, `not_sent` remains only when the resolved effort sends no level (Runtime default). Keep the `not_configurable` token in `fixtures/model-settings/contract.json`, `api/cmd/uzi/settings_models.go` and `web/src/lib/modelSettings.ts` so an older server still renders. The Codex review effort cell's `CatalogDefault` (`effective_settings.go` ~L283) uses the configured Codex review model. Sites that change: `effective_settings_livedb_test.go` ~L439-473, `api/cmd/uzi/settings_effective_test.go` ~L67, `fixtures/api-contract/effective_settings.behavior.json` (all four scenarios) and its verbatim copy `web/src/mocks/data/effectiveSettings.ts` (pinned by `mockApi.effectiveSettings.test.ts`). The corpus is recorded by `TestEffectiveSettingsCorpusLiveDB`, which needs Postgres; if the worker has none, apply the deterministic edit to both copies and leave the LiveDB proof pending exact-head PR CI. The view predicts a capable worker and is not adjusted per worker, as for template effort.
- **Claim**: for a review run (`isReviewRun`), `assembleClaim` adds two top-level `ClaimResponse` fields beside `judge_model` / `summary_model` (`claim.go` ~L349-350): `task_review_model` (omitted when unset) and `task_review_effort`, the RESOLVED review effort level (resolution below; omitted when it is Runtime default), only on a review claim. No run override or lead-template tier applies. Assembly validates the stored model again (Codex against `agenttmpl.CodexRoleModels`, Claude through `ValidateClaudeLaneModel`); an invalid stored model is omitted, so the built-in model applies and the effective cell reports `builtin`. Effort: select the nonblank review preference, otherwise the ordinary per-harness worker preference, and resolve through `ResolveHarnessEffort`; Runtime default omits the wire level, and a corrupt nonblank preference resolves to built-in `medium`, which is sent. Claim and effective view share this resolution. Add `TaskReviewSettingsV1` (`task_review_settings_v1`) to `capability.go` `protocolVocabulary` and `protocolOrder`. A worker without it gets neither field; whenever a resolved field is withheld, including an inherited built-in `medium` (no notice when both model and resolved effort would be omitted), assembly appends a `status` feed line with event `task_review_settings_omitted`, last in assembly like `recordTemplateEffortOmittedNote` (`template_effort_note.go`, `claim_assembly.go` ~L1185), fenced on `claim_generation`. No placement change, no requeue: an old worker runs the built-in, which is today's behaviour.
- **Agent**: `review-runner.ts` reads `claim.task_review_model` and `claim.task_review_effort` and passes them to `runReadOnlyModelPass` (`model`, `effort`). Absent → today's values (Claude none; Codex `CODEX_TASK_REVIEW_MODEL`; effort none). The runner must keep ignoring `config.default_model` and `config.default_effort` on a review claim; the inheritance is resolved server-side into `task_review_effort`. The worker advertises `task_review_settings_v1` in `agent/src/worker.ts` beside `template_effort_v1` (~L1051).
- **CLI**: `modelSettingSpecs` gains `task-review-model` and `task-review-effort` (`--harness claude|codex`), with `set`/`reset` help (reusing the existing source labels); `task-review-effort` takes the five levels or `runtime` (`parseEffort`); `reset` clears it so it inherits again; `uzi settings get` effective rows show the configured value and source.
- **Web**: Run defaults **Task review** section: per harness a model input (Claude: free text with the existing Codex-ID warning; Codex: select of `CodexRoleModels` plus "Default · gpt-6-sol (built-in)") and an effort select ("Default · follows worker effort (<resolved level or Runtime default>)", Runtime default, and the five levels). Saves through `putMySettings`. Mock API parity in `web/src/mocks/mockApi/settings.ts`.
- **Failure visibility**: an unavailable or rejected configured model surfaces in the posted review (`status:"failed"`, summary names the model call failure), as today. On a provider-bound Claude review (PRD #4320), a configured model the binding's map does not resolve fails at spawn with `provider_model_unmapped` (`agent/src/provider-binding.ts` `resolveProviderModel` ~L184-192, via `claude-advice-harness.ts` ~L229); the result is the existing `failed` review with the error redacted by `boundedClaimError`. There is no create-time check, matching the other advice models (`provider_source.go` `checkProviderModels` comment ~L370-378). No new health reason.
- **Rollout**: new worker + old api → absent fields, built-in runs. New api + old worker → withheld + notice; the old worker keeps the built-in model and sends no effort (today's behaviour). Hosted fleets gain the capability when `workers.image.tag` moves. Both milestones ship in this one PR: the capability means "reads both fields", so a worker that advertises it must read both.
- **E2E**: unaffected (`git grep -n -i "task.review\|handoff.*review\|review_target" -- e2e` finds only a comment in `run-store-it.sh` ~L194); still sweep changed literals per AGENTS.md.

Resource bounds: the new input is the owner's own setting, bounded by the existing validators (`MaxModelLen=100`, closed effort vocabulary, closed Codex set). No new irreversible or cross-owner action.

## Testing decisions

- Go handler tests beside the judge/summary model-settings tests: tri-state writes, refusal cases (Codex ID in Claude field, non-curated Codex model, bad effort, control characters), one-transaction rollback.
- Claim assembly tests: review claim carries the fields only for a capable worker; withheld + notice otherwise; the notice fires for an inherited `medium` withheld from an old worker and not when both fields would be omitted; an invalid stored model is omitted; a corrupt nonblank effort resolves to built-in `medium`, is sent to a capable worker, and reports source `builtin`; an unset review effort ships the owner's worker effort (level, built-in `medium`, or omitted for Runtime default), and an explicit `runtime` review effort ships none even with a worker effort set; non-review claims never carry them.
- Effective-settings handler + both contract-fixture copies: built-in vs `user_default` cells for both harnesses, `not_configurable` gone, the Codex catalog default following the configured model; update the existing LiveDB suite (`effective_settings_livedb_test.go` ~L439-473).
- Agent `review-runner` tests: model/effort passed through for both harnesses (an inherited effort arrives as `task_review_effort`); a review claim carrying only the legacy `config.default_model` / `config.default_effort` sends no model and no effort on Claude, and `gpt-6-sol` with no effort on Codex; a provider-bound Claude review with an unmapped configured model posts the `failed` review; capability advertised.
- CLI tests with the label fixture both-ways pin; web Vitest for the section (save, reset, validation warning) and a focused browser case.
- Each new behaviour gets a regression test watched red on the base, then green.

## Milestones

### M1: Task-review model per harness (done)

Blocked by: none.  
Storage (two model columns), write/read API and fixtures, claim field with re-validation, `task_review_settings_v1` capability and notice, agent pass-through, effective model cells, CLI `task-review-model`, Run defaults model controls, mock API, docs (`worker-model.md` section rewritten, `configuration.md`, `cli.md`, `handoff.md`; `task docs:sync`), `specs/human.md` line, changelog fragment, the stale comments in `task-review-model.ts` and `worker_default.go`.  
Acceptance: examples 1 (model half), 2 (model half), 3, 4 pass; tests above for model; `check-docs:web` clean.

### M2: Task-review effort per harness (done)

Blocked by: M1.  
Storage (two effort columns, own migration), write/read API and fixtures, claim field under the same capability, agent pass-through, effective effort cells, CLI `task-review-effort`, Run defaults effort selects, docs (`worker-effort.md`, `configuration.md`, `cli.md`).  
Acceptance: examples 1 (effort half) and 2 (inherited effort) pass; an explicit `runtime` review effort sends no level; tests above for effort.

## Landing obligations

- Exact-head PR CI: full `test:api`, `test:web`, `test:web-browser`, `gate:repo`, and for `agent/**`: `full agent suite: exact-head PR CI`.

## Owner acceptance

None required; no hosted-only behaviour.

## Decision Log

- 2026-11-22 D1: storage as per-harness `users` columns copying the judge/summary lane pattern, not the `user_cross_check_pins` table. Reason: one owner value per harness, no stage key; the lane pattern already has a one-line CLI spec, an allowlist entry and a cell builder. Rejected: a new pins table (more moving parts for the same data).
- D2: Codex review model limited to `CodexRoleModels`. Reason: the advice renderer drops anything else (`render.ts` `resolveModel`), so accepting a custom ID would silently run the provider default. Rejected: widening the renderer (out of scope; affects every advice pass).
- D3: a new capability `task_review_settings_v1` withholds the fields from older workers with a feed notice rather than requeueing. Reason: an old worker running the built-in is exactly today's behaviour, so availability is never reduced; the notice keeps the effective view honest. Rejected: placement gating (adds SQL and health surface for no safety gain).
- D4 (owner decision, 2026-11-22): an unset review effort follows the owner's worker effort for the reviewing harness, including Runtime default, as recorded on #1570 from #2073 (https://github.com/vtmocanu/uzi/issues/1570#issuecomment-5945824909). This changes behaviour for owners who set nothing: reviews start sending their worker effort (built-in `medium` when that is unset). The changelog fragment states it. An explicit review effort (a level or `runtime`) overrides. Rejected: unset sends nothing (keeps today exactly but contradicts the recorded requirement).
- D5: no one-run handoff override and no instance tier in this PRD. Reason: smallest design meeting the issue's outcome; both remain possible additions on the same fields.
- D7: the review fields are top-level claim fields beside `judge_model` / `summary_model`, and the runner keeps ignoring `config.default_model` / `config.default_effort`. Reason: `config.default_model` is the worker lane, which would silently replace the Claude review's SDK default, and a single resolved review field keeps the claim and the view on one rule. The inheritance is resolved on the server into `task_review_effort`, so the agent has one source and the effective view uses the same resolver.
- D8: stored values are re-validated at claim time and an invalid one falls back to the built-in. Reason: a curated Codex model can be removed later, and the renderer would then run the provider default instead of today's built-in.
- D6: unavailable configured model fails visibly through the existing failed-review summary, not a new health reason. Reason: the review run already posts that failure on the target run; a run-level health reason would duplicate it.
- D9 (implementation, 2026-11-22): the claim omits `task_review_model` whenever the resolution is the built-in (unset or invalid stored value), Codex included, so the worker's own built-in applies and the wire carries only an owner-configured, re-validated model. The omission notice names each withheld setting; its text says the old worker runs its built-in model and sends no effort. Rejected: shipping `gpt-6-sol` explicitly on every capable Codex review (same behaviour, wider wire, and it contradicts "omitted when unset").
