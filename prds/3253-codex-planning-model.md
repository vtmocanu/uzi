# PRD #3253: Codex runs plan on a planning model and effort, with usage attributed to the model that did the work

**Issue:** #3253
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Draft r4, refreshed 2026-11-21. Child Q-codex of epic #1703 (r1 was the single Q draft). Current baseline: origin/main `10876dee680859e4e468ae5f004475f44b31a80a`; Codex runtime pin `0.167.0`.
**Priority:** `priority::normal`.
**Parent:** #1703.
**Blocked by:** #4580 ([accounting child](done/4580-codex-usage-model-segments.md)), whose merge supplies the tested segment interface. #3246 and #3252 are closed and their code is on this base. #3244's resolver/reporting seam merged in PR #3695; its remaining owner landing acceptance is not an unfinished-code dependency. #3245, #3247 and #3250 are already on base.

**Related:**
- P: a run choice wins in both phases.
- F: see section 7.
- B: Runtime default effort and `EffortSettingCellDTO` (D6).
- PRD #3068: Codex fresh-thread consent, relevant only to the unselected section-5 fallback. Recheck its landed status if that fallback is redesigned; it is not a prerequisite for the selected cold same-thread mechanism.
- [ADR-2555](../adr/2555-run-usage-attribution.md): Codex response attribution, amended here.
- ADR-0195: per-model usage fold.

**Delivery:**
- One uzi run, one PR, Codex harness because this changes the Codex SDK/app-server integration, after #4580 merges. The owner approved splitting accounting first on 2026-11-21.
- Risk class: billing and data integrity. Use reviewer, tester and auditor (CLAUDE.md "Run economy").
- No implementation or validation writes under `.github/workflows/`.
- Migration `NNNNN_user_planning_codex.sql` gets the next free number at implementation and is renumbered at landing with `task migration:renumber`; fix postflight references (dba 17). Main is at `00499` on this baseline; read the live head rather than reserving a number.
- LiveDB tests need Docker.
- **Dispatch check:** D1 selects same-conversation switching at the cold approval epoch and closes the live effort decision. **Q-claude D18 was reviewed in PR #4548 and implemented by PR #4571; this prerequisite is met. Do not dispatch until #4580 merges.** This proof does not authorize a warm-epoch alternative or an unreviewed fresh-conversation fallback.

## Problem

Q-claude lets Claude runs plan on a planning pair and implement on the run pair. Codex users want the same, for example planning on `gpt-6-astra` at `high` and implementing on `gpt-6.1-sol` at `medium`.

On Codex the naive switch would misattribute usage. The usage accountant binds each thread to one model for the whole claim. If the implement epoch resumes the plan thread on another model, every implementation token is priced and attributed to the planning model, in run cost, in per-model usage and in the ADR-2555 attribution records.

## Verified baseline (origin/main 10876dee)

| Seam | Anchor (symbol, file) | Current behavior |
|---|---|---|
| Request | `buildRunRequest(ctx, phase, …)`, `codexEffort` (`agent/src/codex/codex-executor.ts`) | Phase-aware for grants, roster and prompt, but model (`ctx.config?.default_model ?? provider.model`) and effort are identical for both phases. `modelSource: "worker_default"` marks a server-shipped root; it is the only provenance that lets a custom ID through `render.ts` `resolveModel`. |
| Approval boundary | "NEW-ROOT RESUME at plan approval" in `if (!preApproved && ctx.gatePlan)`, `startProviderEpoch` (`codex-executor.ts`) | Every approval (human, auto, cross-check) persists the session and starts a new epoch (a new harness instance) that `thread/resume`s the plan thread, then disposes the old epoch. This runs before the first `reportIteration`, so the run is still `awaiting_approval` at the resume (#1766 B1 comment). A pre-approved claim starts in the implement phase. |
| Per-turn model | `runTurn`, `resumeThread`, `startTurnRpc` (`agent/src/codex/codex-harness.ts`) | `currentModel` comes from the rendered lead model on every turn and is sent on `thread/resume` and on every `turn/start` (with `effort`). |
| Replay boundary | `recordUsageNote` with `resumeReplay` (`codex-harness.ts`): replay snapshots are buffered, then `accountant.record(threadId, resumeReplay, true)` at the root `turn_started` note | Notes before `turn_started` are historical replay; notes after it are the new turn. |
| Accountant | `CodexUsageAccountant` (`agent/src/codex/token-accounting.ts`): `registerThread`, `record`, `aggregateByModel`, `markIncomplete` | See the details below this table. |
| Server fold | ADR-0195, `api/internal/workersvc/usage_fold.go` | MAX per (run, model, lineage); two models on one claim fold independently. |
| Roles | `resolveRoleModel` and per-role effort (`agent/src/codex/render.ts`) | Unpinned roles take the request model; every role takes the request effort. |
| Effort persistence (Q-spike refresh, 2026-11-20) | `TurnStartParams.effort` (openai/codex `086ccc833bc9cf01024ad2dd6af7dbd421b6bf9a`, `codex-rs/app-server-protocol/src/protocol/v2/turn.rs`) | Omission/null preserves an explicit override warm. Live 0.167.0 cold resume resets the override to Runtime default (`effort: null` present); see D1 for the observations and policy scope. |
| Custom-model mirrors | the `codex_custom_model_v1` CASE in `ClaimRun` (claimant and fleet-spread peer clauses), `ListActiveRunsForHealth` (`CodexCustomRoot`), `CountOnlineWorkersClaimableForRun`, `ListUnplaceableQueuedRunsForEphemeral`, both `ListSaturationQueuedRunsForEphemeral` sites (`queries/runtime.sql`); `CountOnlineWorkersSatisfyingCustomCodex`; `reasonNoCustomCodexCapableWorker` and the docker-probe filter (`health.go`); `errCustomModelCapabilityMissing` (`service.go`) | The effective root is a cross-check pin, a curated frozen `runs.model`, or `users.default_codex_model`. No planning model is considered. |
| Capability advertising | `codexHarness?.advertise` block in `agent/src/worker.ts` | Codex protocol capabilities, `codex_custom_model_v1` included, are pushed here. |
| Shipped Q-claude pieces | `resolvePlanPhase` (`claim_phase.go`), called from inside H's `resolveLeadSettings` (`LeadSettingsInput.PlanningModel`/`PlanningEffort`, result `LeadSettings.Plan`); `ClaimConfig.PlanPhase`; the phase-capability predicate in every mirror; `enterPhaseSwitchHold` and `SetRunPhaseSwitchHold` (`phase_switch_unavailable`); the atomic `SetUserPlanningClaude` planning-pair write (no locked cross-field combination helper); the `planning_models` / `planning_efforts` cells; the Planning card; H `resolveLeadRequest` with `phase` and plan cases in `request_cases` (`fixtures/lead-config/contract.json`); ADR 3252-phase-model-switch | Claude only. |

Accountant details on the current base (changed by prerequisite #4580 before this PRD starts):
- **One per claim leg.** It is created in the epoch-shared context and handed to every epoch.
- **`registerThread`.** The first registration wins, and `ThreadAccount.model` is readonly. The root is registered only on a harness instance's first turn.
- **`record`.**
  - Adoption gate: a note is adopted only if its magnitude is strictly greater than the last one.
  - Regression gate: a note whose input, cached, cache-write, output or uncached input is lower than the last adopted note is refused with `advancing_regression`. `reasoningOutputTokens` and `totalTokens` are not checked.
  - `adopted.model = acct.model`.
- **`aggregateByModel`.** Charged usage is `diffClamp(maxTotal, baseline)` per thread, with each column clamped at 0. Responses are reconciled per thread, then priced with `acct.model`.
- **`markIncomplete`.** Sets `attributionQuarantined` on a resumed thread that has no baseline.

## Outcome

Settings gain an optional **Codex planning model** and **Codex planning effort** ("Same as the run" by default). A Codex run of a plan-phase kind:
- plans on the planning pair;
- switches to the run pair at the approval epoch, on the same thread;
- uses the planning pair again in a plan phase on a later claim.

Preserve Q-claude's precedence (P's `run_override` wins in both phases), claim-time provenance, capability and reporting contracts while extending their currently Claude-only implementations to Codex. D1/D6 permit the explicit-planning/Runtime-default implementation effort combination; there is no active combination rejection or locked cross-field helper to reuse.

Per-model usage, cost and ADR-2555 `usage_record` events attribute each token to the model that produced it. Token totals are conserved in every column. No duplicate, stale or replayed notification is counted. This holds across resume and reclaim. A delta that cannot be attributed is kept conservative and never assigned to the implementation model.

Acceptance examples (each one a test):

1. **Switch and attribution.**
   - Setup: Codex planning `gpt-6-astra` / `high`; worker model `gpt-6.1-sol` / `medium`.
   - Plan turns send `gpt-6-astra` / `high`.
   - The implement epoch resumes the same thread. The `thread/resume` response reports `gpt-6.1-sol`, and turns send `gpt-6.1-sol` / `medium`.
   - Result-frame `modelUsage` lists plan tokens under `gpt-6-astra` and implement tokens under `gpt-6.1-sol`. In every column the sum equals the thread's cumulative delta for the claim.
   - Each `usage_record` carries its own response's model.
   - H shows `lead: plan gpt-6-astra · high → implement gpt-6.1-sol · medium`.
2. **Dropped final plan note, larger historical replay (R2-5).**
   - The plan epoch's last usage note is never adopted before disposal.
   - At the implement epoch the boundary replay advances the cumulative. That advance lands in the plan segment. The plan model reports `unreported` with its proven tokens retained.
   - The implement segment starts after the boundary, reconciles and is metered.
   - No plan token is counted under `gpt-6.1-sol`.
3. **Reclaim after approval.** A later claim resumes on the implement model. The new accountant registers the resumed thread with the implement model, and proved replay is baselined, not charged. There is no cut on that claim.
4. **Reclaim that re-plans.**
   - The re-plan claim's epoch 0 registers the plan model, and approval cuts at the implement epoch's replay boundary.
   - A thread still waiting for proved replay rebinds its empty segment and charges nothing.
   - A quarantined thread stays quarantined: both models are `unreported`, with proven tokens retained.
5. **Custom planning model, mixed fleet.**
   - Setup: a custom Codex planning ID. Worker X has phase support but no `codex_custom_model_v1`; worker Y has both.
   - Only Y claims. With only X online, health says "no worker supporting custom Codex models is online". There is no claim/requeue loop.
6. **Runtime did not switch (hold).**
   - Trigger: the implement epoch's `thread/resume` response `model` differs from the intended implementation model. That covers the planning model, any other model, a missing field or an unrecognized value.
   - The worker sends no `turn/start` and makes no segment cut. It takes Q-claude's hold (`phase_switch_unavailable`) from `awaiting_approval`.
   - Resume and Cancel both work. No implementation turn runs on the planning pair.
7. **Runtime-default combination.** For D1's cold approval epoch, explicit planning effort plus Runtime default implementation effort is accepted at save in both orders. The implementation epoch omits effort, and the runtime reports a present null default rather than retaining the planning override. Do not silently supply a numeric effort. A warm alternative would retain the override and requires new design/proof before dispatch; it is not covered by this allowance. Ordinary model/effort validation and an atomic planning-pair update still apply; no additional cross-field rejection or locking subsystem is required.

## Out of scope

- Everything in Q-claude's out-of-scope list.
- Per-role effort for Codex subagents (child N).
- The advice harness, cross-check, judge and summary accountants, which never switch models.
- Changing `RequestWorkerPause`.

## Modules and seams

### 1. Settings, claim and mirrors for Codex

- **Migration** (`NNNNN_user_planning_codex.sql`, next free number): `ALTER TABLE users ADD COLUMN planning_codex_model text, ADD COLUMN planning_codex_effort text;`. Down drops them. Same conventions as Q-claude D11.
- **Store:** `SetUserPlanningCodex` and a strict `GetUserPlanningCodex`. Use an atomic model/effort update with explicit field-presence flags, following `SetUserPlanningClaude`; the shipped handler calls it directly and has no locked cross-field write helper. D1's cold mechanism has no forbidden explicit-planning/Runtime-default combination; accept it for `planning_codex_effort` and `default_codex_effort` in either save order (B's Codex `runtime` setter included).
- **Settings DTO and cells:**
  - `planning_codex_model` and `planning_codex_effort` on `userSettingsDTO` and `apitypes.UserSettingsDTO`.
  - Codex entries in the `planning_models` (C's `ModelSettingCellDTO`) and `planning_efforts` (B's `EffortSettingCellDTO`) arrays.
  - C's (field, token) labels.
- **PUT validation:**
  - Model: `validateModel` plus `rejectClaudeAliasInCodexLane`. A validated custom ID is accepted, as on the worker lane. New explicit override inputs use C's guards; the Codex lane guard is unchanged.
  - Effort: `validateEffort`.
  - The shared `fixtures/settings/planning-defaults-cases.json` gains the Codex cases: Claude alias rejected, custom ID accepted, and the allowed Runtime-default combination in both save orders.
- **Claim:**
  - `assembleClaim` fills `LeadSettingsInput.PlanningModel` / `PlanningEffort` from the Codex columns for Codex claims. `resolveLeadSettings` calls a deliberately extended `resolvePlanPhase` (currently returns nil unless Claude and rejects Codex role IDs): select model validation by harness, preserve frozen-value precedence and existing Claude results, and `LeadSettings.Plan` ships as `ClaimConfig.PlanPhase`.
  - Shipping requires `codex_phase_models_v1`. Add it to `api/internal/capability` (constant, protocol vocabulary, `protocolOrder`, and protocol-filter tests; keep the scheduler vocabulary golden unchanged). The agent advertises it inside the existing `codexHarness?.advertise` block in `agent/src/worker.ts`.
- **Phase-capability mirrors:** Q-claude's predicate widens from `harness = 'claude'` to both harnesses, each with its own capability and planning columns. It stays one predicate in every mirror. The SQL form is `IS NOT NULL` with no trim (dba 16).
- **Custom planning model:**
  - In every custom-model clause (the seven SQL sites, `CountOnlineWorkersSatisfyingCustomCodex`, the health rung and the docker-probe filter), a run needs `codex_custom_model_v1` when either effective root is custom:
    - today's root;
    - or, for a lead-lane run with no curated frozen `runs.model`, a `planning_codex_model` that is non-NULL and not in the curated set.
  - `errCustomModelCapabilityMissing` also covers `PlanPhase.Model`.
  - Non-looping: the SQL is a superset of the assembly check, so a requeue happens only on a settings race, and the next `ClaimRun` excludes the worker.
  - The health reason is unchanged.
- **Web:** a Codex row on the Planning card.
  - "Codex planning model": `ModelSelect`, `harness="codex"`, `allowCustom`, default label from the cell.
  - "Codex planning effort".
  - Shown under the existing `showCodexLane` rule, with the same single Save.
- **CLI:** Codex rows in C's "Model settings" table.

### 2. Agent: per-phase request

- `buildRunRequest` takes the model and effort for its `phase` from H's `resolveLeadRequest({…, phase})`. Extend its current `phase === "plan" && harness === "claude"` gate to Codex deliberately, keeping default/provider fallback and server-owned custom-model provenance.
- `request_cases` in `fixtures/lead-config/contract.json` gains the Codex plan cases: planning model and effort, frozen model, custom planning ID, absent `plan_phase`.
- `modelSource: "worker_default"` is kept whenever the chosen model was server-shipped and non-empty, so a custom planning ID passes `render.ts` like a custom worker ID.
- Codex always sends a model, so Q-claude's "unsupported phase configuration" rule never fires here.
- Roles follow the request model and effort per phase (D5).
- Reporting uses H's `enterPhase` at the `driveCodexTurn` call site. The returned `PhaseEntry` is passed to `observe`. H already decodes the `thread/resume` response `model` per epoch (`codex_thread_response`).

### 3. Use the accounting child's segment interface

#4580 owns the accountant implementation, seven-column regression gate, per-segment reconciliation/pricing and ADR-2555 amendment. Reuse its `beginModelSegment(threadId, model)` contract; do not reimplement that child here.

**Harness use (`codex-harness.ts`), in this order:**
1. Establish the independently proved cold approval epoch and verify its `thread/resume` response model (section 4). On failure, no implementation turn or cut occurs.
2. Ensure the prior epoch cannot deliver further usage into the shared accountant before the new epoch's accounting boundary. On this baseline, ordinary approval starts the new epoch and swaps safety before disposing the old one (`codex-executor.ts:4702-4719`); only the captured path disposes first. That is current behavior, not a preexisting proof of the desired ordering. Implement and test the necessary quiescence/disposal handoff while preserving failed-start cleanup, guarded sink ownership and `CodexCredentialDeferredError` behavior. Do not assume that changing a comment or blindly moving disposal is sufficient.
3. Send `turn/start` only after the guard passes. At the first root `turn_started` note, record buffered historical `resumeReplay` into the previous segment, then call `beginModelSegment` exactly once for the new model (also when no replay is pending). No late old-epoch note may cross that cut.
4. Later turns of the same instance must refuse a changed lead model before `turn/start`, with "codex lead model changed inside a provider epoch". Child threads keep their own registration and are never cut.

Reclaim creates a new claim accountant and registers the phase it starts in; proved replay is a baseline, not a charge. An empty unproved segment rebinds without charging; quarantine remains sticky. Preserve cross-claim lineage and the per-(run, model, lineage) server fold.

### 4. Approval transition and the hold (D1 = same conversation)

- **Switch.** The implement epoch `thread/resume`s the plan thread with the implement model, then `turn/start`s with the implement effort. The model never changes inside an epoch.
- **Pre-provider guard (buddy Q4, R2-9).** Q-spike case 9's Codex control establishes that `thread/resume` precedes any provider request. Compare the `thread/resume` response `model` with the **intended implementation model** (the implement `LeadRequest` model), as an exact match on canonical Codex IDs.
  - **Match:** continue.
  - **Mismatch, missing field, or a value that fails H's model-token sanitization:** hold. This is fail-closed: the switch is unconfirmed.
  - **Echo case:** If Q-spike establishes that the response is only an uninformative echo, the same-conversation mechanism is not dispatchable under this verification contract. Select and review another mechanism through section 5. Do not send implementation work while the switch is unconfirmed.
- **Hold.** Extend Q-claude's `enterPhaseSwitchHold` (quiesce, preserve, release) and `POST /api/worker/runs/{id}/phase-switch-hold`. Its status fence admits `awaiting_approval`, but `SetRunPhaseSwitchHold` currently hardcodes `harness = 'claude'`: explicitly admit the intended Codex lane while preserving kind, worker, generation, unreleased-claim, review and egress fences. Preserve first-transition approval-wait budget banking and the idempotent re-read that does not rerun the UPDATE. Keep paused/queued readopt exclusions and the lost-ACK/owner-Resume race protection from #3252.
  - Quiesce disposes the implement epoch through the existing guarded sink. Report the correct phase: the runner's outer `PhaseSwitchUnverifiedError` catch currently handles the plan-phase case, so an implement mismatch must not be mislabeled or bypass guarded capture/cleanup.
  - The preserved session id is the plan thread.
  - No segment cut has happened (R2-11).
  - The 409 path, Resume and Cancel are as in Q-claude.
- **Planning pair.** No path implements on the planning pair. Keep the D1 cold `thread/resume` response-model guard; Claude D18's `getSettings` prompt barrier is not the Codex mechanism, and post-dispatch settings notifications are not admission proof.

### 5. If D1 = fresh conversation (redesign gate before dispatch)

These are requirements only:
- `thread/start` a new thread on the implement model at the approval epoch. The new root registers fresh with that model, so no segment is needed.
- Carry Q-claude section 6's continuation package, and persist the new thread id.
- If PRD #3068 is on the base and the run carries its policy-refusal marker, #3068's consent rule governs.
- Missing consent or an incomplete package takes the same hold. Never implement on the plan thread.

### 6. Docs, ADRs, changelog

- **Docs:**
  - `docs/worker-model.md`, `docs/worker-effort.md`: the Codex planning pair and D1's allowed Runtime-default combination, and that custom planning models need `codex_custom_model_v1`.
  - `docs/run-cost.md`: per-model attribution across the switch, and the malformed-sequence `unreported` case.
  - `docs/hosted-workers.md`: the custom-planning capability.
  - Then `task docs:sync`.
- **Specs:** `specs/human.md` Feature #17 line, tagged `(AI-synced YYYY-MM-DD)`.
- **ADRs:**
  - ADR 3252-phase-model-switch gains a Codex section: the cut happens only at an epoch's replay boundary, segments and the extended regression gate, the mid-epoch refusal, and the resume-response guard.
  - [ADR-2555](../adr/2555-run-usage-attribution.md): retain #4580's segment-accounting amendment and document this PR's real replay-boundary integration without duplicating the child design.
- **Changelog:** a fragment.

### 7. Extend the effective-settings seam already on base

F and the Claude planning extensions are on this base. Extend their files unconditionally with the Codex cases:
- `api/internal/handler/effective_settings.go`, `api/internal/workersvc/effective_settings.go` (`PredictLead` through `resolveLeadSettings`) and `api/internal/apitypes/effective_settings.go`: Codex planning cells (C model cell, B effort cell, `user_planning_default`) and the plan/implement pair in the start and schedule previews;
- `fixtures/api-contract/effective_settings*.json`;
- the recorded-response mock in `web/src/mocks/mockApi/settings.ts` and `web/src/lib/apiTypes.ts`;
- F's rows in C's table and `fixtures/model-settings/contract.json`;
- the claim-equivalence test.


## Testing decisions

- **Accountant prerequisite:** #4580 supplies unit/property and mutation coverage. This PR adds the real harness replay-before-cut integration and keeps that suite passing.
- **Harness, integrated** (`codex-harness.test.ts`, the shared accountant across two harness instances). The plan epoch drops its last note, the implement epoch resumes with a larger replay and then runs a turn (example 2). Also covered:
  - the cut is called once, after the boundary record and after the old epoch is disposed;
  - the mid-epoch refusal;
  - the guard: mismatch, missing field and unrecognized value hold with no `turn/start`, and a match proceeds.
- **Executor** (`codex-executor.test.ts`, app-server fakes):
  - plan request on plan, clarification and revise turns;
  - implement request on the approval epoch, a pre-approved start, a reclaim and checkpoint epochs (unchanged model, no cut);
  - `modelSource` with a custom planning ID;
  - the hold from `awaiting_approval`.
- **Real runtime.** Q-spike's `e2e/codex-m4/phase-model-switch.test.ts` case 6 asserts today's misattribution; this PRD rewrites that assertion to the segmented attribution. Add a larger-replay case to the same file in the same lifecycle. Report the `test:codex-m4` shard-1 time delta and `agent-stage M4 duration_seconds`.
- **Server (LiveDB):**
  - `plan_phase` on Codex lead-lane claims;
  - the mixed fleet (X: phase without custom; Y: both) through every mirror, the satisfier counts and health; follow the custom-model cases in `api/internal/store/ephemeral_lease_protocol_livedb_test.go`;
  - the D1 Runtime-default combination allowance for the Codex fields, both save orders, through the atomic planning-pair setter and ordinary default-effort setter;
  - `SetRunPhaseSwitchHold` from `awaiting_approval` on a Codex run of each kind;
  - settings handler and store tests.
- **Through H.** A Codex run with a planning pair yields plan and implement rows with the expected sources.
- **E2E sweep.** As in Q-claude, plus the Codex literals.

## Milestone

### M1. Codex planning pair end to end

**Blocked by:** #4580 merged.

- Sections 1-7, including unconditional effective-settings extensions and the real harness use of the child's segment interface.
- Dispatch checklist in the PR: child merge SHA, unchanged pinned cold guard evidence, and any #3068 applicability limited to the unselected section-5 fallback. Never activate that fallback without new design review.
- Acceptance: examples 1-7 are tests; Q-spike case 6 is rewritten to segmented attribution; real replay and late-old-epoch ordering, failed-start/credential-deferred cleanup, Codex hold and lost-ACK/Resume controls pass.
- In-run checks: targeted touched agent tests with the repository timeout and concurrency limits, `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`, `task check:api`, `task check:web`, `task check-docs:web`, `task scan:secrets`, and `task sast:semgrep`. Run heavy commands serially; no full suites in-run.
- Focused LiveDB: start an owned `cdr-*` PostgreSQL container, set `UZI_TEST_DATABASE_URL` for each test command only, and use `go test -p 1 -race -count=1 -v` with explicit touched-package/test selectors. Record named RUN/PASS counts and require zero SKIP/FAIL (including nested tests). Ordinary checks run with the DSN unset. Tear down only that container by exact name.
- Sweep changed contract literals across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`; update stale expectations and run `task test:e2e-lib` and `task test:forge-fake`. Name affected full phases left to CI. Record behavioral red/green and named mutants separately from unchanged base-pass controls.

## Landing obligations (outside milestones)

- full agent suite: exact-head PR CI
- Applicable full API/web/repository suites and security/docs checks: exact-head PR CI.
- Pinned `test:codex-m4` real-runtime proof and packaged/macOS coverage as applicable to the touched seam; report its execution environment. No live credentials are needed for the offline provider characterization.
- Measure affected unit files/jobs before and after, the `test:codex-m4` shard-1 delta and `agent-stage M4 duration_seconds`, and critical-path impact. Separate queue/cache effects and identify unmeasured values. No implementation or validation writes to workflows.

## Decision Log

- **D1. Mechanism: same conversation at the cold approval epoch (#3246, live evidence 2026-11-20).** User-run native ChatGPT-login probes on Codex 0.167.0: the full matrix and corrected effort-only report are retained in the durable #3246 evidence comment linked below. Implementation provenance is the squash-merge of the Q-spike PR. All twelve original and eight focused turns completed and returned the nonce. Both probes reported unchanged auth-file mtime; credentials were subsequently removed by the operator. The model/effort mechanism is established for this cold shape; the Q-claude guard dependency is now met by PR #4571; #4580 remains the dispatch blocker.
  - **Switch/re-plan/reclaim:** the warm process ran `gpt-6-astra`/high -> `gpt-6.1-sol`/medium -> Astra/high. A second process sharing the same selected CODEX_HOME resumed the recorded thread with Sol, returned `model=gpt-6.1-sol`, and completed the retained-nonce control without another turn-level model override. The credential-free pinned test independently checked actual provider model B in that no-override control and zero provider requests before `turn/start`.
  - **Warm and cold reports differ.** Warm `thread/resume(model=B)` reported A/high. The independent no-turn-model control also sent A, showing that warm resume did not apply B; an explicit B on `turn/start` switched the request. Cold resume reported B and sent B without a repeated model override. The model-verification mechanism is the cold epoch, not a warm-resume echo or the subsequent `turn/start` override.
  - **Model omission is not a production setting:** `buildRunRequest` uses `leadRequestFor` / `resolveLeadRequest` (`agent/src/lead-config-report.ts`) and always supplies the provider/model fallback. Model omission above is a test-only resume-effectiveness control.
  - **Case 5, closed by the corrected live report:** after Astra/high, warm Sol turns with omitted effort and explicit null both reported `effortPresent=true, effort=high`. After resetting to Astra/high and restarting the app-server, cold Sol resume reported `resumeEffortPresent=true, resumeEffort=null`; the subsequent omitted/null Sol turns both reported `effortPresent=true, effort=null`. Thus warm omission keeps the override; cold omission resets it to Runtime default. The live numeric default is not reported and must not be called low; low was observed only on the offline provider wire. Settings notifications are post-dispatch observations, not an admission barrier. The older report's `unreported` conflation is superseded.
  - **Save-time decision:** the explicit-planning/Runtime-default implementation combination needs no rejection for this cold approval-epoch mechanism. Preserve omission on `turn/start`; do not replace it with a guessed numeric effort. A warm alternative is outside this decision and needs fresh proof/review.
  - **Current attribution is still wrong, as expected:** every adopted switched live usage note was attributed to Astra, including cold post-replay Sol work. The passing characterization pins that current behavior for this PRD to invert when adding segments; Q-spike changes no accountant behavior. Totals are cumulative and replay must not be charged again.
  - **Provider acceptance/cache:** the initial Astra switch-group turn reported 32 reasoning tokens; subsequent Sol and cold resumed turns completed without rejection. Raw live reasoning-item forwarding was not inspected. The first warm Sol response reported last input 14,275, cache read/write 0 and output 30; the first cold Sol response reported last input 23,058, cache read/write 0 and output 30, with complete pricing-field evidence. Use `last`, not cumulative totals, for those observations. Other Astra turns had cache hits; this one probe does not establish a general cross-model cache rule.
  - Sanitized full and corrected live observations: [Q-spike evidence](https://github.com/vtmocanu/uzi/issues/3246#issuecomment-7038584349).
- **D2. Segments are cut only at an epoch's first-turn replay boundary; any later change gets a typed refusal.** This keeps the shared accountant's cross-epoch baseline and dedup. Rejected:
  - a new accountant per epoch, which loses the retained baseline and duplicate protection;
  - re-registering under a new key, which breaks the thread identity ADR-2555 relies on;
  - re-pointing the model in place, which charges earlier tokens to the new model.
- **D3. Reconciliation and pricing are per segment.** Per-thread reconciliation would push both models to `unreported` on every split thread.
- **D4. With no adopted usage the cut rebinds in place; quarantine sticks across segments.** Neither state has a cumulative to cut at.
- **D5. Codex roles follow their phase's request model and effort.** Pinned roles keep their pins.
- **D6. Runtime default is allowed for the selected cold mechanism.** D1's live evidence shows that the planning override is cleared on cold resume, so the user decision of 2026-10-21 does not activate a save-time rejection here. The same evidence shows warm persistence; changing to a warm mechanism requires new proof and review of its reset/rejection rule. Rejected: silently substituting a numeric default or dropping the planning effort at claim time.
- **D7. The custom planning model joins every custom-model mirror.** Rejected: an assembly-only check, which can loop.
- **D8. Hold visibly through Q-claude's active-run hold, never implement on the planning pair.** User decision 2026-10-21 (buddy Q2); fold-r3.
- **D9. Split.** User decision 2026-10-21: this PRD is separate from Q-claude because the accountant is a billing-integrity seam that needs its own run and review wave.
- **D10. Segmented attribution and the per-column conservation gate ship first in child #4580.** The slice is safe alone and reviewable in isolation; it no longer belongs to this PR's milestones. P and B are on the base through Q-claude's blockers, so they need no condition here.
- **D11. The cut follows the replay boundary (R2-5, buddy Q3).** Replay reconciles the plan's work, so its advance belongs to the plan segment. An unpriceable advance degrades only the plan model.
- **D12. Extend the regression gate instead of clamping per segment (R2-6).** Monotone cumulatives make conservation exact by construction. A dip is malformed data and should fail closed. Rejected: clamping each segment delta, which over-counts across a dip.
- **D13. The guard compares against the intended implementation model and fails closed on a missing or unrecognized response (buddy Q4, R2-9).** An echo makes the same-conversation mechanism not dispatchable.
- **D14. Owner-approved split, 2026-11-21.** Extract the independent accounting outcome to [#4580](done/4580-codex-usage-model-segments.md), one run and PR first. Block this Codex planning PRD on that child's merge. #1703 indexes both outcomes. Rejected: one large PR combining the accounting change and end-to-end planning. This PRD owns production cut ordering, cold verification, settings/mirrors and hold integration.
- **D15. Refresh to shipped Q-claude, 2026-11-21.** Extend the actual Claude-only resolution/hold gates and atomic setter rather than an imaginary locked combination-rule helper. Preserve budget banking and race fences. Keep the Codex-specific cold-resume guard; D18 is the Claude proof, not a portable app-server API.
