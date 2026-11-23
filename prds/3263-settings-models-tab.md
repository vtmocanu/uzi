# PRD #3263: Settings Models, one grid and one atomic save

**Issue:** #3263. **Parent:** #1703. **Status:** Draft r2, anchors refreshed 2026-11-23.  
**Baseline:** main `939c0e2b`, including merged #4599 (Codex planning) and #4608 (task-review settings).  
**Blocked by:** none. The prerequisite implementations are landed. #3247 remains open only for maintainer-owned hosted catalog timing; its settings/runtime implementation landed in #3672, and that observation does not block this editor.  
**Delivery:** one PRD, one run, one PR, three serial vertical slices. No migration, new column, worker capability or runtime policy. Neither implementation nor validation changes `.github/workflows/**`.

**Design-mock contract:** `prds/mockups/1703-model-config/AfterModels.dc.html`, `prds/mockups/1703-model-config/AfterRunDefaults.dc.html` and section 1 of `prds/mockups/1703-model-config/index.html` are presentation references, not validation or runtime specifications. Compare the result in light/dark and narrow layouts. The PR must include **Mock deviations**, naming the added planning, cross-check and task-review rows, current source vocabulary, and the summary-routing exception below.

## Problem

Model choices are scattered across Run defaults and a separately saved cross-check grid. A user cannot prepare the whole configuration, review saved predictions and save it together. A single browser call to `PUT /me/settings` does not fix this: that handler performs separate writes.

There is already a smaller backend foundation: `PUT /me/model-settings` validates a closed set of fields, locks the user's settings row and commits its writes, including cross-check pins, in one transaction. Extend that route instead of adding another settings store or batch service.

## Outcome and acceptance examples

Settings has **Models** between Appearance and Run defaults. One grid contains default harness; Claude/Codex worker model and effort; Claude/Codex planning model and effort; judge routing and models; summary models; task-review models and efforts; and independent Plan cross-check and Code cross-check model/effort rows. A sticky Save bar shows the number of changed editable values and saves every dirty value in one transaction.

1. Change the default harness, Claude planning effort, Codex task-review model and both cross-check stages, then Save. All changes commit together. The returned canonical preferences become the clean baseline; returned effective cells show the saved values and sources. A validation error or pre-commit write failure leaves all preferences and pins unchanged.
2. Switch the default harness twice. Both families' stored choices survive. Default clears a user override, never an instance default. A saved prediction may differ from the draft: label it **Saved effective value** and mark its row unsaved, rather than presenting it as a prediction of an unsubmitted edit.
3. A planning Default means Same as the run, using the existing per-harness planning resolver, including the lead-template effort tier. Explicit run choices still win in both phases; compatible schedule model pins override defaults. This UI does not recompute precedence.
4. Judge Runs on Claude dims the Codex judge model with an accessible reason, but retains its stored and draft values. Same as run keeps both families active. Missing credentials or a helper's disabled toggle do not prevent preparing its choices. Summary routing is read-only **Same as run**; task review inherits the target's harness.
5. An unset task-review effort follows the ordinary worker effort for its family, including Runtime default, without the lead-template tier. Cross-check effort retains its own existing default and does not inherit worker effort. Planning effort still rejects Runtime default. No validator is broadened to make the grid uniform.
6. A dirty page blocks tab/sidebar/command-menu navigation and browser back/forward, offering Stay or Discard changes. Reload/close uses the native unsaved-changes prompt. Successful save, explicit discard and clean pages do not prompt. A viewer change clears the previous viewer's draft and ignores their late requests.
7. Run defaults retains behavioral settings. Its Judge section keeps enable/enforcement and credential binding, with a read-only routing/model/credential summary and **Change in Models** link. Model controls are no longer independently writable there.

Every input has a row/family accessible name and `aria-describedby` linking its saved prediction, reason or field error. Narrow layouts stack labeled cells; content bottom padding clears the sticky bar. Dirty count counts semantic field changes, not trimmed whitespace or a changed prediction.

## Out of scope

New precedence, model catalogs, provider execution, helper credentials, instance worker defaults, per-run override editors, generic form infrastructure, optimistic concurrency/versioning, and removal of existing settings APIs or CLI commands.

**Summary routing is deliberately not added.** The #3257 decision split Summaries Runs on into #4156. Summaries currently execute inline on the run's engine and credential; a selector would require execution machinery, which #3263 excludes. Keep summary model editing and read-only current routing; record this mock deviation. #4156 is not a prerequisite for this tab.

## Modules and resolved seams

### A. Atomic write boundary

At this baseline, `api/internal/handler/model_settings.go` L33-48 already allows the following 14 root keys. Add the five missing keys:

| Group | Keys | Work |
| --- | --- | --- |
| Harness | `default_harness` | Add |
| Worker models | `default_claude_model`, `default_codex_model` | Reuse |
| Worker effort | `default_effort`, `default_codex_effort` | Reuse |
| Planning | `planning_claude_model`, `planning_claude_effort`, `planning_codex_model`, `planning_codex_effort` | Add |
| Judge | `judge_runs_on`, `judge_model`, `codex_judge_model` | Reuse |
| Summary | `summary_model`, `codex_summary_model` | Reuse |
| Task review | `task_review_model`, `codex_task_review_model`, `task_review_effort`, `codex_task_review_effort` | Reuse #4608 |
| Cross-check | `cross_check_pins` | Reuse four stage/family cells, model and effort independently |

This is 19 root keys, with 18 scalar preferences and at most four cross-check cells. Omitted keys/members stay unchanged; explicit null clears an override using the existing decoder semantics. Do not accept the ambiguous legacy `default_model` alias or behavior/consent/credential fields. The legacy model mirror must nevertheless be updated internally as today.

`writeModelSettings` / `applyModelSettings` already use one transaction and `LockUserSettingsRow`, then write pins last in stable stage/family order (`model_settings.go` L225-331). Keep that lock order and reuse the transaction-bound query object for every setter. Planning uses the existing pair setters; no nested committing pin transaction. For harness changes, reuse the proposed-harness resolver before Begin and pass its result and harness presence/value to `harnessModelParams` under the lock. Read omitted lane values through the transaction, not a global-pool settings builder while the row is locked. Preserve both family lanes and the compatibility mirror.

Validate all supplied known fields before opening the transaction. Return a deterministic additive `errors` array with stable field identity, plus stage/family/member identity for pin errors. Retain the existing top-level `error` and `field` for the first error so old clients continue to work. Do not parse human error messages to identify cells. Keep strict JSON, request-size bounds, unknown-root rejection, unique stage/family validation and existing authentication: the route already permits the current user's cookie+CSRF or scoped CLI bearer (`routes_me.go` L300; `cli_auth_livedb_test.go` L617-663). Do not widen generic `PutMySettings` authentication.

On success return `{settings, effective}`: the existing canonical settings DTO plus the existing effective-settings DTO, both read after commit. Extend its DTO/client/mock types additively. Preserve `settings_commit_unknown` and `settings_saved_readback_failed`; a failed post-commit prediction read must also say the save committed. The UI retains the draft, explains uncertainty and offers explicit reload/reconciliation, never claiming rollback or blindly retrying. This is transactional writing, not a new cross-request snapshot/CAS guarantee; preserve the existing harness/credential resolution boundary.

### B. Predictions, sources and validation

Use `buildEffectiveSettings` / `PredictLead`, not a second browser precedence resolver (`api/internal/handler/effective_settings.go` L179-291, including both planning and task-review rows; task-review cell builders at L406-424). `/me/settings` supplies stored values and existing setting cells; `/me/effective-settings` supplies saved predictions and sources. Initial loading and save responses are viewer-fenced. Prediction failure shows unavailable, not a guessed fallback; credential refusal is data and does not disable editing.

Current Run defaults only loads secrets and `/me/settings` (`web/src/pages/RunDefaults.tsx` L488-493); the new Models page also reads effective settings. Use server source labels and preserve fixed-at/credential-kind semantics. Effective-settings intentionally contains no credential ID, label or secret; Judge's credential summary continues using existing owned-token/account metadata, never a lead credential prediction presented as a judge binding.

Reuse the existing validators and catalog data. Worker/planning Codex model fields allow their existing custom IDs. Judge Codex keeps its curated set; summary/task-review Codex keep `CodexRoleModels`. Claude fields keep their existing family restrictions/custom-ID rules. Cross-check keeps its own model validation and default effort. Validate dirty values even in dimmed cells; never discard them silently. Keep Runtime default distinct from clearing an override.

### C. Page, draft and routing

Add a Models page and typed `putMyModelSettings` wrapper (`web/src/lib/api.ts` L876-889 currently only exposes general settings writes). One parent owns stored baseline, draft, semantic dirty set, errors and save state. Refactor `web/src/components/CrossCheckDefaults.tsx` L24-49 into controlled rendering/validation for this draft; remove its separate request and Save. Reuse existing controls/catalogs rather than copy entire cards. Disable editing and duplicate Save while a save is pending. Fence success and failure to the initiating viewer/request; never install a late prior-viewer response.

Move controls from Run defaults without changing their policy. Keep autopilot, plan/code cross-check enables, usage/wall/wait limits, judge enable/enforcement and credential controls, summary-now behavior, CI fixes, attribution, MR rework and harness overflow behavior there. Link to Models. Move only the model/effort pin grid, not cross-check enables.

Register `/settings/models` in `web/src/App.tsx` L115-119, `components/SettingsShell.tsx` L17-24 and `components/navSections.ts` L26-33, preserving the requested order. Retarget model-specific links from Start/Schedule preview to Models; behavior-specific links remain Run defaults. Update route/nav/search tests and mock scenarios.

There is no existing unsaved-navigation guard. The app uses `BrowserRouter` (`web/src/main.tsx` L3,35-39), while the pinned router's `useBlocker` requires a data router. Use the supported minimal bootstrap adaptation: `createBrowserRouter` / `RouterProvider` with one wildcard root wrapping the existing AuthProvider/App tree, retaining the existing descendant route manifest, layouts and auth guards. No new loaders or route-data architecture. Models uses a route-local blocker and `beforeunload`; tests use a memory data router. Avoid click interception, patched history and compensating popstate navigation. Same-page anchor navigation need not discard the draft. Authentication loss/logout must not trap a user in an old viewer's form.

### D. CLI and contracts

Existing CLI model setters already call this atomic route. `api/internal/uzicli/client_account.go` L194-237 requires the `settings` member and retains the raw envelope; additive `effective` and error details need no new commands. Keep old single-field writes and JSON output working, with compatibility tests. New standalone planning/harness CLI setters are not necessary to deliver this tab and are not added here. Document the expanded API allowlist and return envelope without changing `/api/v1` or the general settings route.

Sweep old settings URLs, removed labels and any changed response/error literals across tracked code and `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`. Keep old API consumers working. Update affected assertions/fakes in this PR; run `task test:e2e-lib` and `task test:forge-fake` if those contracts change, naming affected end-to-end phases left to CI.

## Testing decisions

- Handler/LiveDB: mixed scalar/pin save and explicit resets; invalid fields produce cell-specific errors with zero writes; force the last pin write to fail and prove rollback of every earlier write, including harness and legacy mirror. Preserve omitted lanes, existing commit-unknown/readback failure tests and cookie/CLI ownership/auth coverage. Required LiveDB cases must run with zero skips.
- Resolver compatibility: both planning families, template-aware planning Default, worker Runtime default, task-review inheritance, four independent cross-check cells, dimmed choices and the distinct model validators. Use existing resolvers; this is not a runtime policy rewrite.
- Web: canonical post-save baseline, mixed dirty count, errors at the correct family/row, saved-vs-draft prediction wording, reset semantics, delayed save/load across viewer changes, failure reconciliation and pending-save disabling. Prove the mixed save uses one request and has no independent card saves.
- Navigation/browser: supported router bootstrap preserves protected routes/login redirects; Stay/Discard for tab/sidebar/back/forward and native unload listener lifecycle; clean save/discard removes guards; auth/viewer transitions clear stale drafts. Event-driven readiness, no added timing sleeps.
- Presentation: keyboard operation, accessible row/family names and descriptions, dimmed reason, light/dark comparison and narrow stacked layout with Save-bar clearance. Cover Judge's read-only summary and link, and behavior controls remaining in Run defaults.
- API client/CLI: old one-field payload and response parsing remain compatible with additive predictions/errors. Mock API follows the same atomic validation and response shape; it must not mutate state before validation succeeds.

For each bug/regression seam, watch the focused test fail with the relevant change absent, then pass with it restored. Compatibility and layout coverage need not manufacture mutants unrelated to their claim.

## Milestones

### M1. Worker and planning configuration in Models

**Blocked by:** none; #4599 and #4608 are merged in the verified baseline.

Deliver Models route/nav, one draft/Save bar, worker/harness/planning controls and saved predictions. Extend the atomic endpoint by the five missing keys, aggregate field errors and additive predictions. Add the supported navigation guard. Existing helper controls remain usable until M2/M3; do not ship the intermediate slices separately.

Acceptance: examples 1-3 and 6 for these rows; transaction rollback/auth/compatibility and router guard tests pass. Run `task check:api` plus `go test -race -count=1` on the affected Go packages (including the required LiveDB rollback cases), touched web unit/browser files and `task check:web`. Run heavy checks serially, one Go package or browser file per command.

### M2. All helper and cross-check choices share that save

**Blocked by:** M1.

Add judge routing/models, summary models/read-only routing, task-review models/efforts and both cross-check stages to the same draft and transaction. Remove the cross-check component's private save path. Preserve catalog restrictions, inherited/default values and dormant family choices.

Acceptance: examples 1, 4 and 5 across every group; last-write rollback and mixed helper/worker save tests; `task check:api`, touched focused Go (`-race -count=1`)/web/browser tests and `task check:web`, serially. No helper execution change or worker capability.

### M3. Run defaults becomes behavior-only, with clear navigation and documentation

**Blocked by:** M2.

Remove duplicate model editors/saves from Run defaults. Keep its behavior controls and Judge credential controls; add the read-only summary and Models links. Update model-specific preview links, API/settings/CLI documentation as affected, embedded mirrors, human specs (dated AI-synced), and a Changed changelog fragment. Compare the supplied mocks and record deviations in the PR.

Acceptance: all examples; touched regression/browser files, `task check:web`, `task docs:sync`, `task check-docs:web`, `task check:changelog-entry`, plus affected E2E contract checks described above. Run `task scan:secrets` and `task sast:semgrep` for the changed input-decoding surface; a missing scanner is pending evidence, not a pass. No workflow edits and no full component gate in-run.

## Landing obligations (outside milestones)

Exact-head PR CI supplies the full repository/API/web validation, including the complete web unit/browser suites. Confirm required LiveDB coverage actually ran. Recheck integrated settings contracts and migration numbering at landing; this PR itself adds no migration. No owner-only acceptance or external deployment is required to close #3263.

## Decision Log

- **D1, 2026-11-22:** Keep one PRD/run/PR. The user-visible outcome is one coherent editor and save. The existing transaction already covers every helper/pin group; five additive keys and a bounded page/router change complete it. An API-only split would mostly duplicate an already available boundary and leave the user outcome blocked on a second run. No new runtime subsystem or migration is justified.
- **D2:** Reuse `PUT /me/model-settings`, retaining its auth, lock order, compatibility mirror and uncertain-commit semantics. General `PutMySettings` remains compatible for other clients but is not the Models save path.
- **D3:** #4599 and #4608 landed first, so the verified baseline contains both planning families and task review. The first Models release uses this complete row set.
- **D4:** Follow the recorded #3257/#4156 split: summary Runs on remains read-only until helper execution supports another engine. This is a documented mock/issue-scope correction, not a new routing policy.
- **D5:** Show saved predictions separately from unsaved edits. No draft-preview endpoint or duplicated browser precedence engine is needed. Reconcile uncertain saves explicitly; no new CAS/versioning subsystem.
- **D6:** Use the router's supported blocker with a minimal bootstrap adaptation. No existing guard can be reused; retaining BrowserRouter and patching navigation would add fragile parallel routing behavior.
- **D7:** Dependency means landed implementation, not closing an unrelated maintainer observation. #3247's PRD records implemented M1/M2 and pending hosted timing; #3263 neither performs nor waives that observation.
