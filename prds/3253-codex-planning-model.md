# PRD #3253: Codex runs plan on a planning model and effort, with usage attributed to the model that did the work

**Issue:** #3253
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Draft r3, 2026-10-21. Child Q-codex of epic #1703 (r1 was the single Q draft). Anchors are pinned to origin/main `522aab1ba`. No file cited here changed between a50b0366c and 522aab1ba.
**Priority:** `priority::normal`.
**Parent:** #1703.
**Blocked by:** #3246 (D1 evidence), #3244 (resolver and reporting seam), #3252 (planning resolution, claim field, mirrors predicate, hold, card, cells, ADR). #3245, #3247 and #3250 are on the base through #3252's blockers.

**Related:**
- P: a run choice wins in both phases.
- F: see section 7.
- B: Runtime default effort and `EffortSettingCellDTO` (D6).
- PRD #3068: Codex fresh-thread consent. It had not landed at 522aab1ba (only the PRD existed).
- [ADR-2555](../adr/2555-run-usage-attribution.md): Codex response attribution, amended here.
- ADR-0195: per-model usage fold.

**Delivery:**
- One uzi run, one PR, default harness, after Q-spike, H and Q-claude merge.
- Risk class: billing and data integrity. Use reviewer, tester and auditor (CLAUDE.md "Run economy").
- No implementation or validation writes under `.github/workflows/`.
- Migration `00355_user_planning_codex.sql` is a draft number (main ends at `00346`; Q-claude drafts `00353`). Renumber at landing with `task migration:renumber`, then fix any migration number hard-coded in a test (dba 17).
- LiveDB tests need Docker.
- **Dispatch check:** dispatch only if Q-spike recorded D1 = "same conversation" for Codex. Otherwise section 5's requirements are designed and reviewed first.

## Problem

Q-claude lets Claude runs plan on a planning pair and implement on the run pair. Codex users want the same, for example planning on `gpt-6-astra` at `high` and implementing on `gpt-6.1-sol` at `medium`.

On Codex the naive switch would misattribute usage. The usage accountant binds each thread to one model for the whole claim. If the implement epoch resumes the plan thread on another model, every implementation token is priced and attributed to the planning model, in run cost, in per-model usage and in the ADR-2555 attribution records.

## Verified baseline (origin/main 522aab1ba)

| Seam | Anchor (symbol, file) | Current behavior |
|---|---|---|
| Request | `buildRunRequest(ctx, phase, …)`, `codexEffort` (`agent/src/codex/codex-executor.ts`) | Phase-aware for grants, roster and prompt, but model (`ctx.config?.default_model ?? provider.model`) and effort are identical for both phases. `modelSource: "worker_default"` marks a server-shipped root; it is the only provenance that lets a custom ID through `render.ts` `resolveModel`. |
| Approval boundary | "NEW-ROOT RESUME at plan approval" in `if (!preApproved && ctx.gatePlan)`, `startProviderEpoch` (`codex-executor.ts`) | Every approval (human, auto, cross-check) persists the session and starts a new epoch (a new harness instance) that `thread/resume`s the plan thread, then disposes the old epoch. This runs before the first `reportIteration`, so the run is still `awaiting_approval` at the resume (#1766 B1 comment). A pre-approved claim starts in the implement phase. |
| Per-turn model | `runTurn`, `resumeThread`, `startTurnRpc` (`agent/src/codex/codex-harness.ts`) | `currentModel` comes from the rendered lead model on every turn and is sent on `thread/resume` and on every `turn/start` (with `effort`). |
| Replay boundary | `recordUsageNote` with `resumeReplay` (`codex-harness.ts`): replay snapshots are buffered, then `accountant.record(threadId, resumeReplay, true)` at the root `turn_started` note | Notes before `turn_started` are historical replay; notes after it are the new turn. |
| Accountant | `CodexUsageAccountant` (`agent/src/codex/token-accounting.ts`): `registerThread`, `record`, `aggregateByModel`, `markIncomplete` | See the details below this table. |
| Server fold | ADR-0195, `api/internal/workersvc/usage_fold.go` | MAX per (run, model, lineage); two models on one claim fold independently. |
| Roles | `resolveRoleModel` and per-role effort (`agent/src/codex/render.ts`) | Unpinned roles take the request model; every role takes the request effort. |
| Effort persistence | `TurnStartParams.effort` (openai/codex source at `092d3acd`, `thread.rs`) | "Override the reasoning effort for subsequent turns." A single `Option`, so `null` cannot reset it. Q-spike case 5 confirms this on 0.162.1. |
| Custom-model mirrors | the `codex_custom_model_v1` CASE in `ClaimRun` (claimant and fleet-spread peer clauses), `ListActiveRunsForHealth` (`CodexCustomRoot`), `CountOnlineWorkersClaimableForRun`, `ListUnplaceableQueuedRunsForEphemeral`, both `ListSaturationQueuedRunsForEphemeral` sites (`queries/runtime.sql`); `CountOnlineWorkersSatisfyingCustomCodex`; `reasonNoCustomCodexCapableWorker` and the docker-probe filter (`health.go`); `errCustomModelCapabilityMissing` (`service.go`) | The effective root is a cross-check pin, a curated frozen `runs.model`, or `users.default_codex_model`. No planning model is considered. |
| Capability advertising | `codexHarness?.advertise` block in `agent/src/worker.ts` | Codex protocol capabilities, `codex_custom_model_v1` included, are pushed here. |
| Q-claude pieces (after it lands) | `resolvePlanPhase` (`claim_phase.go`), called from inside H's `resolveLeadSettings` (`LeadSettingsInput.PlanningModel`/`PlanningEffort`, result `LeadSettings.Plan`); `ClaimConfig.PlanPhase`; the phase-capability predicate in every mirror; `enterPhaseSwitchHold` and `SetRunPhaseSwitchHold` (`phase_switch_unavailable`); the effort write function with the combination rule; the `planning_models` / `planning_efforts` cells; the Planning card; H `resolveLeadRequest` with `phase` and plan cases in `request_cases` (`fixtures/lead-config/contract.json`); ADR 3252-phase-model-switch | Claude only. |

Accountant details at 522aab1ba:
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

Q-claude's precedence (P's `run_override` wins in both phases), claim-time resolution, capability requirement, hold, combination rule and reporting apply to Codex unchanged.

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
7. **Runtime-default combination.** With Q-spike case 5 recording "keeps" for Codex, the combination "planning effort `high` and Codex effort `runtime`" is rejected at save in both orders, through Q-claude's locked write function, with the Codex fields named. Persisted settings violating the active combination rule cause a visible, non-retrying configuration error before a claim is delivered. The configured planning effort is never silently replaced. Ordinary saves prevent this state through Q-claude's locked settings-write function.

## Out of scope

- Everything in Q-claude's out-of-scope list.
- Per-role effort for Codex subagents (child N).
- The advice harness, cross-check, judge and summary accountants, which never switch models.
- Changing `RequestWorkerPause`.

## Modules and seams

### 1. Settings, claim and mirrors for Codex

- **Migration** (draft `00355_user_planning_codex.sql`): `ALTER TABLE users ADD COLUMN planning_codex_model text, ADD COLUMN planning_codex_effort text;`. Down drops them. Same conventions as Q-claude D11.
- **Store:** `SetUserPlanningCodex` and a strict `GetUserPlanningCodex`. Writes go through Q-claude's locked effort write function, so the combination rule covers `planning_codex_effort` and `default_codex_effort` (B's Codex `runtime` setter included).
- **Settings DTO and cells:**
  - `planning_codex_model` and `planning_codex_effort` on `userSettingsDTO` and `apitypes.UserSettingsDTO`.
  - Codex entries in the `planning_models` (C's `ModelSettingCellDTO`) and `planning_efforts` (B's `EffortSettingCellDTO`) arrays.
  - C's (field, token) labels.
- **PUT validation:**
  - Model: `validateModel` plus `rejectClaudeAliasInCodexLane`. A validated custom ID is accepted, as on the worker lane. New explicit override inputs use C's guards; the Codex lane guard is unchanged.
  - Effort: `validateEffort`.
  - The shared `fixtures/settings/planning-defaults-cases.json` gains the Codex cases: Claude alias rejected, custom ID accepted, and the combination in both orders when active.
- **Claim:**
  - `assembleClaim` fills `LeadSettingsInput.PlanningModel` / `PlanningEffort` from the Codex columns for Codex claims. `resolveLeadSettings` calls `resolvePlanPhase` unchanged, and `LeadSettings.Plan` ships as `ClaimConfig.PlanPhase`.
  - Shipping requires `codex_phase_models_v1`. Add it to `api/internal/capability` (constant, vocabulary, `protocolOrder`, golden). The agent advertises it inside the existing `codexHarness?.advertise` block in `agent/src/worker.ts`.
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

- `buildRunRequest` takes the model and effort for its `phase` from H's `resolveLeadRequest({…, phase})`.
- `request_cases` in `fixtures/lead-config/contract.json` gains the Codex plan cases: planning model and effort, frozen model, custom planning ID, absent `plan_phase`.
- `modelSource: "worker_default"` is kept whenever the chosen model was server-shipped and non-empty, so a custom planning ID passes `render.ts` like a custom worker ID.
- Codex always sends a model, so Q-claude's "unsupported phase configuration" rule never fires here.
- Roles follow the request model and effort per phase (D5).
- Reporting uses H's `enterPhase` at the `driveCodexTurn` call site. The returned `PhaseEntry` is passed to `observe`. H already decodes the `thread/resume` response `model` per epoch (`codex_thread_response`).

### 3. Accountant model segments (`agent/src/codex/token-accounting.ts`)

**Caller-facing interface:** one new method, `beginModelSegment(threadId: string, model: string): void`. Everything else is internal.

**Regression gate (R2-6).** `record`'s gate gains `reasoningOutputTokens` and `totalTokens`. A note that lowers any of the seven columns below the last adopted note is refused with the existing `advancing_regression` cause. Every adopted cumulative is then monotone in every column, so segment deltas telescope exactly.

This is a behaviour change, but only for malformed note sequences. Such a thread is now marked incomplete, so its model is `unreported` with proven tokens retained. Today such a note would be adopted with a dip. A test pins the change.

**ThreadAccount.** The readonly `model` becomes an ordered `segments` list. Each segment has:
- `model`;
- `start`, the cumulative where it begins (`undefined` for segment 0, meaning the thread baseline);
- its own `responses`;
- its own `pricingEvidenceComplete`.

The current segment is the last one. `registerThread` is unchanged: the first registration wins and creates segment 0.

**`beginModelSegment`:**

| Thread state | Effect |
|---|---|
| Unknown thread | No-op |
| Current segment model equals `model` | No-op |
| No adopted usage this claim (`maxTotal === undefined`), including a resumed thread still awaiting proved replay | Rebind the current segment's model in place. It has no responses. A later proved replay baselines as today and is never charged. |
| Adopted usage exists | Append a segment whose `start` is the current `maxTotal`. |
| `attributionQuarantined` thread | The cut or rebind is recorded, and the thread stays quarantined. Nothing is attributed. |

**`record`:**
- The adoption gate is unchanged apart from the two added columns.
- An adopted note's `last` joins the current segment's responses.
- `adopted.model` is the current segment's model, so ADR-2555 `usage_record` events carry the right model.
- A note older than the cut has magnitude at or below the segment start, so it is never adopted into the new segment.

**`aggregateByModel`, per segment:**
- Charged usage is `segmentEnd - segmentStart`. The end is the next segment's `start`, or `maxTotal` for the last segment. Segment 0 starts at the baseline. With the monotone gate these differences are non-negative in every column without clamping.
- Reconciliation (`responsesReconcileDelta`) and pricing (`priceCodexResponse(segment.model, …)`) run on each segment's own responses against its own delta.
- Dominance per model is unchanged.
- A thread's incompleteness, from any cause, taints every model it has a segment on. That keeps unattributable deltas conservative.

**Invariants, each pinned by a test:**
1. Conservation: in every column, the sum of segment deltas equals `maxTotal - baseline`.
2. No usage is counted twice across a cut.
3. Replay is never charged, before or after a rebind.
4. Quarantine and incompleteness spread to every model the thread has a segment on.
5. `usage_record` carries the model per response.
6. A boundary-replay advance stays in the segment before the cut.

**Harness use (`codex-harness.ts`), in this order (R2-5, R2-11):**
1. The approval epoch's `thread/resume` returns, and the switch is verified (section 4). On a hold, nothing below runs.
2. `turn/start` is sent.
3. At the root `turn_started` note, right after the boundary `record(threadId, resumeReplay, true)` (or at that same point when no replay is pending), the harness calls `beginModelSegment(threadId, currentModel)`. This happens once per harness instance, on its first turn.
4. The previous epoch was already disposed before step 1, and a test pins that ordering. No late note from the old process can reach the accountant after the cut. Replay that reconciles the plan's work lands in the plan segment.

On any later turn of the same instance, a change of lead `currentModel` raises a typed refusal before `turn/start`. The run fails closed with "codex lead model changed inside a provider epoch". Child threads keep their own registration and are never cut.

**Reclaim:** a new claim creates a new accountant, and its epoch 0 registers whichever phase it starts in. Cross-claim lineage is unchanged (ADR-0632, ADR-1079, ADR-1562), and the server fold is per (run, model, lineage).

### 4. Approval transition and the hold (D1 = same conversation)

- **Switch.** The implement epoch `thread/resume`s the plan thread with the implement model, then `turn/start`s with the implement effort. The model never changes inside an epoch.
- **Pre-provider guard (buddy Q4, R2-9).** Q-spike case 9's Codex control establishes that `thread/resume` precedes any provider request. Compare the `thread/resume` response `model` with the **intended implementation model** (the implement `LeadRequest` model), as an exact match on canonical Codex IDs.
  - **Match:** continue.
  - **Mismatch, missing field, or a value that fails H's model-token sanitization:** hold. This is fail-closed: the switch is unconfirmed.
  - **Echo case:** If Q-spike establishes that the response is only an uninformative echo, the same-conversation mechanism is not dispatchable under this verification contract. Select and review another mechanism through section 5. Do not send implementation work while the switch is unconfirmed.
- **Hold.** Use Q-claude's `enterPhaseSwitchHold` (quiesce, preserve, release) and `POST /api/worker/runs/{id}/phase-switch-hold`. Its fence already admits `awaiting_approval`, which is the Codex state at this point.
  - Quiesce disposes the implement epoch through the existing guarded sink.
  - The preserved session id is the plan thread.
  - No segment cut has happened (R2-11).
  - The 409 path, Resume and Cancel are as in Q-claude.
- **Planning pair.** No path implements on the planning pair.

### 5. If D1 = fresh conversation (redesign gate before dispatch)

These are requirements only:
- `thread/start` a new thread on the implement model at the approval epoch. The new root registers fresh with that model, so no segment is needed.
- Carry Q-claude section 6's continuation package, and persist the new thread id.
- If PRD #3068 is on the base and the run carries its policy-refusal marker, #3068's consent rule governs.
- Missing consent or an incomplete package takes the same hold. Never implement on the plan thread.

### 6. Docs, ADRs, changelog

- **Docs:**
  - `docs/worker-model.md`, `docs/worker-effort.md`: the Codex planning pair, the combination rule, and that custom planning models need `codex_custom_model_v1`.
  - `docs/run-cost.md`: per-model attribution across the switch, and the malformed-sequence `unreported` case.
  - `docs/hosted-workers.md`: the custom-planning capability.
  - Then `task docs:sync`.
- **Specs:** `specs/human.md` Feature #17 line, tagged `(AI-synced YYYY-MM-DD)`.
- **ADRs:**
  - ADR 3252-phase-model-switch gains a Codex section: the cut happens only at an epoch's replay boundary, segments and the extended regression gate, the mid-epoch refusal, and the resume-response guard.
  - [ADR-2555](../adr/2555-run-usage-attribution.md) is amended: "The root uses the lead lane and its registered model" becomes "the model of the root thread's current segment", plus one paragraph on segment cuts.
- **Changelog:** a fragment.

### 7. If F is on the base, extend it

Extend F's files with the Codex cases, per F's "Extended by: Q-codex":
- `api/internal/handler/effective_settings.go`, `api/internal/workersvc/effective_settings.go` (`PredictLead` through `resolveLeadSettings`) and `api/internal/apitypes/effective_settings.go`: Codex planning cells (C model cell, B effort cell, `user_planning_default`) and the plan/implement pair in the start and schedule previews;
- `fixtures/api-contract/effective_settings*.json`;
- the recorded-response mock in `web/src/mocks/mockApi/settings.ts` and `web/src/lib/apiTypes.ts`;
- F's rows in C's table and `fixtures/model-settings/contract.json`;
- the claim-equivalence test.

If F lands later, F's line covers it.

## Testing decisions

- **Accountant** (`agent/test/codex-token-accounting.test.ts`, `codex-cost-accounting.test.ts`). One case per `beginModelSegment` row and per invariant:
  - fresh-thread cut;
  - resumed-without-baseline rebind, then proved replay;
  - quarantined cut;
  - duplicate, stale and older-than-cut notes;
  - A→B→A;
  - per-segment reconciliation, both success and failure on one segment only;
  - per-segment pricing;
  - incompleteness spread;
  - `adopted.model`;
  - the extended regression gate, a `reasoningOutputTokens` dip and a `totalTokens` dip, each refused;
  - conservation over randomized monotone sequences in all seven columns, with randomized cut points;
  - boundary replay assigned to the earlier segment.

  Each new behaviour also gets a mutation check, watched red: per-thread reconciliation, `acct.model` left in `record`, the cut before the replay boundary, and the regression gate without the two columns.
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
  - the combination rule for the Codex fields, both orders, under the row lock;
  - `SetRunPhaseSwitchHold` from `awaiting_approval` on a Codex run of each kind;
  - settings handler and store tests.
- **Through H.** A Codex run with a planning pair yields plan and implement rows with the expected sources.
- **E2E sweep.** As in Q-claude, plus the Codex literals.

## Milestones

### M1. Codex usage attributed per model segment, with exact per-column conservation

**Blocked by:** #3246, #3244, #3252.

- Behaviour delivered: Codex accounting refuses a note that lowers any of the seven columns (`advancing_regression`, so the model is `unreported` with proven tokens retained), and the accountant can attribute a thread's usage to successive model segments.
- Scope: section 3's accountant part, its tests and the ADR-2555 amendment.
- Safe alone: no caller cuts a segment yet, so for well-formed note sequences totals, cost and `usage_record` events are byte-identical (pinned by the existing accounting tests). Only malformed sequences with a reasoning or total-token dip change outcome, as tested.
- **Acceptance:** accounting tests and the new invariants are green, and the mutation checks are recorded.

### M2. Codex planning pair end to end

**Blocked by:** M1.

- Sections 1, 2, 3 (harness use), 4 and 6; section 7 if F is on the base.
- **Dispatch-time checklist** (recorded in the PR):
  - F on the base?
  - Q-spike D1 for Codex: is the guard informative, and is the combination rule active?
  - PRD #3068 on the base? (section 5 only.)
- **Acceptance:** examples 1 to 7 are tests; Q-spike's case 6 is rewritten; `task gate:api`, `gate:web`, `gate:agent`, `test:codex-m4`, `gate:repo` and `check-docs:web` are green.

## Decision Log

- **D1. Mechanism: same conversation at the approval epoch, per Q-spike evidence.** *Filled by Q-spike*, including whether the resume response reflects the sent model and the combination rule. Dispatch only on "same conversation".
- **D2. Segments are cut only at an epoch's first-turn replay boundary; any later change gets a typed refusal.** This keeps the shared accountant's cross-epoch baseline and dedup. Rejected:
  - a new accountant per epoch, which loses the retained baseline and duplicate protection;
  - re-registering under a new key, which breaks the thread identity ADR-2555 relies on;
  - re-pointing the model in place, which charges earlier tokens to the new model.
- **D3. Reconciliation and pricing are per segment.** Per-thread reconciliation would push both models to `unreported` on every split thread.
- **D4. With no adopted usage the cut rebinds in place; quarantine sticks across segments.** Neither state has a cumulative to cut at.
- **D5. Codex roles follow their phase's request model and effort.** Pinned roles keep their pins.
- **D6. The runtime-default combination is rejected at save when Q-spike proves effort persists** (user decision 2026-10-21); a persisted violation is a visible, non-retrying configuration error. Rejected: the r2 claim-time drop as the primary rule.
- **D7. The custom planning model joins every custom-model mirror.** Rejected: an assembly-only check, which can loop.
- **D8. Hold visibly through Q-claude's active-run hold, never implement on the planning pair.** User decision 2026-10-21 (buddy Q2); fold-r3.
- **D9. Split.** User decision 2026-10-21: this PRD is separate from Q-claude because the accountant is a billing-integrity seam that needs its own run and review wave.
- **D10. Segmented attribution and the per-column conservation gate ship first, as their own behaviour slice (M1).** The slice is safe alone and reviewable in isolation. P and B are on the base through Q-claude's blockers, so they need no condition here.
- **D11. The cut follows the replay boundary (R2-5, buddy Q3).** Replay reconciles the plan's work, so its advance belongs to the plan segment. An unpriceable advance degrades only the plan model.
- **D12. Extend the regression gate instead of clamping per segment (R2-6).** Monotone cumulatives make conservation exact by construction. A dip is malformed data and should fail closed. Rejected: clamping each segment delta, which over-counts across a dip.
- **D13. The guard compares against the intended implementation model and fails closed on a missing or unrecognized response (buddy Q4, R2-9).** An echo makes the same-conversation mechanism not dispatchable.
