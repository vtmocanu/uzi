# PRD #3252: Claude runs plan on a planning model and effort, then implement on the run's

**Issue:** #3252
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Done (implemented 2026-11-21; was Draft r3 2026-10-21) (child Q-claude of epic #1703; r1 was the single Q draft). Anchors pinned to origin/main `522aab1ba`; no file cited here changed between a50b0366c and 522aab1ba (checked with `git diff --name-only`).\
**Implementation:** Implemented 2026-11-21 on branch agent/issue-3252 (M1 and M2); pending exact-head CI. Landing-time notes are in D19.
**Priority:** `priority::normal`.
**Parent:** #1703.
**Blocked by:** #3246 (mechanism evidence, D1), #3244 (lead resolver, recording and reporting seam), #3245 (five-id Claude lane guard, settings cells, label table, settings table renderer), #3247 (Runtime default effort and `apitypes.EffortSettingCellDTO`), #3250 (`run_override` model and effort on the run, and the hold-reason presentation set #3250 creates).
**Related:**
- Q-codex: the Codex half, blocked by this PRD.
- P (blocker): a run model or effort chosen at start wins in both phases (D5). This PRD adds `phase_switch_unavailable` to P's hold-reason presentation set; it does not use P's park path (D8).
- B (blocker): the Runtime default effort and `apitypes.EffortSettingCellDTO`; see D9 and section 4.
- F: resolver; see section 10.
- Continuation invariants: #3195, #3184, #3169.

**Delivery:**
- One uzi run, one PR, default harness, after Q-spike, H, C, B and P merge.
- No implementation or validation writes under `.github/workflows/`.
- Implemented migrations: `00498_user_planning_claude.sql` (planning columns) and `00499_placement_phase_models.sql` (placement function); both numbers are draft and are renumbered at landing (D19). Originally: migration `00353_user_planning_claude.sql` was a draft number. (Main was at `00346` when drafted, 2026-10-21; read the live head at landing.) Assign the next free number at landing with `task migration:renumber`. Landing step: rewrite any hard-coded migration number in LiveDB tests that the renumber helper reports but does not rewrite (dba 17).
- LiveDB tests need Docker.
- **Dispatch check:** Q-spike's 2026-11-20 live probe demonstrated same-conversation switching (D1). The init-based guard it promised does not hold; section 6 replaces it with a settings barrier (`getSettings()` while the prompt is withheld), characterized credential-free on the tested pin (D18). Dispatchable on that pin once this revision is reviewed. A different SDK/CLI pin must pass section 6's pin-sensitivity test first. A fresh-conversation fallback still requires its own design and review pass.

## Problem

A maintainer wants Claude runs to plan on a strong pair (for example `opus` at `high`) and implement on a cheaper or faster one (for example `sonnet` at `medium`).

Today a run resolves one model and one effort at claim and uses them for every lead turn: planning, clarification, revision, implementation and review. The only workaround is to change the worker defaults, wait for the plan gate, then change them back. That still does nothing for the running session, because its claim already carried the old pair.

## Verified baseline (origin/main 522aab1ba)

| Seam | Anchor (symbol, file) | Current behavior |
|---|---|---|
| Claim payload | `ClaimConfig.DefaultModel`, `ClaimConfig.DefaultEffort` (`api/internal/workersvc/claim.go`); `default_model`, `default_effort` (`agent/src/protocol.ts`) | One model and one effort per claim. |
| Lead resolution | `assembleClaim` (`api/internal/workersvc/claim_assembly.go`): `GetUserHarnessModelDefaults`, `run.Model` screened by `harnessModelCompatible`, `recordHarnessModelFallbackNote` (`harness_create.go`); `readUserDefaultEffort` (`claim_effort.go`) | A compatible frozen `runs.model` (schedule) wins, else the owner's lane. An incompatible pin is dropped with a note. Effort comes from the per-harness column, NULL = `medium`. H extracts this block into `resolveLeadSettings(LeadSettingsInput) LeadSettings` (`lead_settings.go`) and adds `default_model_source` / `default_effort_source`. |
| Lead template fallback | `resolveLeadModel` (`agent/src/sdk-executor.ts`) = `configModel \|\| templateModel`; builtin `api/internal/agenttmpl/builtins/lead.md` has `model: opus` | An omitted `default_model` is not "unresolved": the worker sends the lead template's model. The server cannot see that (H D2). |
| Claimability mirrors | `-- name: ClaimRun` (one statement: a claimant clause plus a fleet-spread peer clause on `p.protocol_capabilities`), `ListActiveRunsForHealth` (computed `CodexCustomRoot`), `CountOnlineWorkersClaimableForRun`, `ListUnplaceableQueuedRunsForEphemeral`, `ListSaturationQueuedRunsForEphemeral` (two sites) in `api/internal/store/queries/runtime.sql`; `CountOnlineWorkersSatisfyingCustomCodex` (`runtime.sql`, declared on the narrow querier interface in `service.go`); `reasonNoCustomCodexCapableWorker`, its queued-reason rung and the docker-probe capability filter (`api/internal/workersvc/health.go`) | Seven SQL sites, one satisfier count and the Go health filters. The only model-dependent clause is Codex custom-model. |
| Post-claim re-check | `errCustomModelCapabilityMissing` (`service.go`), checked in `assembleClaim` | Requeues on a settings race; the next SQL pass excludes the worker. |
| User storage | `users.default_claude_model`, `default_effort` (and Codex twins); `SetUserHarnessModels`, `SetUserDefaultEffort`, `SetUserDefaultCodexEffort` (`api/internal/store/queries/users.sql`) | The lanes are written by one conditional UPDATE; each effort by a single-column UPDATE. The only caller of the effort setters is `PutMySettings` (`git grep -n SetUserDefaultEffort`). No planning columns. |
| Settings API | `userSettingsDTO`, `PutMySettings` (`api/internal/handler/user_settings.go`); `validateModel`, `validateEffort` (`api/internal/handler/agent_templates.go`); routes in `routes_me.go` | Tri-state (absent = unchanged, null = clear, value = set). PUT is cookie-only (`mw.RequireAuth`); GET also accepts `uzc_` (`mw.RequireUser`). |
| Claude plan turns | `phaseSetup` (`sdk-executor.ts`): `baseConfig` with `model` / `effort` set only when present | `baseConfig` drives the first plan turn, revise turns and clarification rounds (`drivePlanningTurn`). |
| Claude approval | `phaseRunLoop`: `implementConfig = { ...baseConfig, agents, systemPrompt, preToolUse, mcpServers }`; `resumeId = plan.sessionId ?? resumeId` | Implementation resumes the plan session with the same model and effort. |
| Prompt delivery | `promptStream` (`agent/src/sdk-messages.ts`) used by `claude-harness.ts`; `buildSdkOptions` | The prompt is yielded immediately; model and effort options are set per `query()` only when present. |
| Subagents | `applySubagentModelOverride(…, leadModel)` on both rosters | The own roster is mutated in place before the plan copy; the implement re-apply is guarded by `&& leadModel`. |
| Re-plan paths | `preApproved`, `resumeAtGate` (`phasePlanGate`); `planApproved`, `m4Candidate`, `ownerHistoryIncomplete` (`agent/src/runner.ts`) | No path re-enters planning within one claim. A later claim re-plans when approval does not carry over. |
| Kinds | dispatch in `agent/src/worker.ts`; `gatePlan` wired unconditionally in `runner.ts`; `auto_approve` baked true in `queries/task.sql`, `workersvc/mr_rework.go` | The main runner plans for `issue`, `ci_fix`, `self_improve`, `prompt`, `mr_rework` and non-review `task`, interactive or not. |
| Worker holds | `RequestWorkerPause` (`park_run.go`): park_run_v1, `running`, non-interactive `issue` only. `SetRunBaseAttestationHold` (`queries/base_attestation_hold.sql`, `WorkerRunBaseAttestationHold`): generation-fenced `running → paused`, keeps clone and custody. `enterCompletionHold`, `enterWallPark` (`runner.ts`): capture-first sinks (preserve session and clone, quiesce, checkpoint, pin generation, settle custody). `ResumePausedRun` (`runtime.sql`) | The pause path does not fit the plan-phase kinds this PRD covers. The fenced holds and capture-first sinks are the reusable pieces. `ResumePausedRun` refuses only `credential_disabled`, `base_attestation_missing`, gated `completion_blocked` and out-of-budget `budget_exhausted`; any other reason resumes, and a worker-side park keeps `worker_id` for a same-worker resume. |
| Hold-reason surfaces | web `web/src/components/ui.tsx`, `web/src/lib/pauseOrigin.ts`, `web/src/lib/runBadge.ts`; CLI `api/cmd/uzi/run_render.go`, `run.go`, `run_wait.go`, `run_steer.go`; agent `client.ts`, `protocol.ts` | Each known `hold_reason` has text and actions. |
| Web | `RunDefaults` (`web/src/pages/RunDefaults.tsx`); `ModelSelect` / `EffortSelect` `defaultLabel`; mock `web/src/mocks/mockApi/settings.ts` | No planning control. |
| CLI | `newSettingsCmd` (`api/cmd/uzi/settings.go`); C adds the "Model settings" table `SETTING HARNESS STORED DEFAULT RESOLVED SOURCE` | Read-only. |
| Docs | `docs/worker-model.md`, `docs/worker-effort.md` | Stale: worker-model.md "Separate from reasoning effort" says effort "is shared across both harnesses"; worker-effort.md says "There is no CLI settings command". |

## Outcome

Settings carry an optional **Claude planning model** and **Claude planning effort**, each "Same as the run" by default. A Claude run of a plan-phase kind:

- uses the planning pair for every lead turn before plan approval;
- switches to the run pair, in the same session, at approval;
- uses the planning pair again in a plan phase on a later claim (a re-plan).

Both pairs are resolved together at claim and stay fixed for that claim. An explicit run choice wins in both phases: a schedule's pinned model, or child P's start-time model or effort.

A worker that cannot honour the planning pair never claims such a run. A switch the runtime does not make holds the run visibly, resumable and cancellable; the run never implements on the planning pair. H shows both phases.

Acceptance examples (each one a test):

1. **Switch at approval.**
   - Setup: worker model `sonnet`, Claude effort `medium`, planning `opus` / `high`; an issue run with no pin.
   - Plan phase: H's plan row requests `opus` / `high`, both sources `user_planning_default` (run label "your planning default").
   - After approval: the implement turn resumes the same session with `sonnet` / `medium`. H appends an implement row with sources `user_default`.
   - The run page shows H's split chip `lead: plan opus · high → implement sonnet · medium`.
2. **Empty lane, builtin template (R2-2).**
   - Setup: Claude lane NULL, builtin `lead` template (`model: opus`), planning model `fable`.
   - The claim carries `plan_phase.model = fable`. The plan turn sends `fable`; the implement turn sends `opus` (source `template`).
   - No unsupported phase configuration is reported: the implementation model resolves from the template.
3. **Schedule pin wins the model, planning still sets effort.** With a schedule pinning `fable`, both phases request `fable` (`schedule_pin`). The plan phase requests effort `high` (`user_planning_default`), implementation `medium`.
4. **Reclaim re-plans.** A human-approved run loses its tree and session and is re-gated on a later claim. That claim's plan turn uses the planning pair as stored at that claim. Approval switches to the run pair as stored at that claim.
5. **Old workers.**
   - Setup: an owner with any Claude planning value set.
   - A worker predating this PRD (pre-H or post-H) never claims that owner's lead-lane Claude runs; the run stays queued.
   - Health says "no worker supporting separate planning settings is online", and hosted placement provisions a current worker.
   - Owners with no planning value are unaffected.
6. **Runtime did not switch (hold).**
   - Trigger: before any provider request of the first implement turn, the runtime reports the planning model while a different implementation model was requested.
   - The worker quiesces the runtime and preserves the session, the approved plan and the work. It releases through the existing custody machinery and parks the run `paused` with `hold_reason = phase_switch_unavailable`.
   - It covers all six plan-phase kinds, interactive runs and a cross-check-approved run.
   - Resume re-claims; Cancel ends the run. No implementation turn runs on the planning pair.
7. **Runtime-default combination rejected at save** (only when Q-spike case 5 recorded "keeps" for Claude).
   - Case (a): Claude effort is `runtime`, and the user saves Claude planning effort `high`.
   - Case (b): Claude planning effort is `high`, and the user saves Claude effort `runtime`.
   - Both answer 400 naming both fields, with this text: "A planning effort needs an explicit Claude effort: choose a level for Claude effort, or set planning effort to Same as the run."
   - Nothing is written.
   - A persisted violation (written before the rule, by a rollback or directly) fails claim assembly with a non-retrying configuration error naming both fields and their remedies; no planning effort is substituted.
8. **Same as the run.** Clearing both fields (PUT `null`) returns `null` on GET. The next claim carries no `plan_phase` key, byte-identical to today.

## Out of scope

- Codex (Q-codex).
- A schedule or per-run planning pin (user decision); child P's run choice applies to both phases instead. Cross-harness planning.
- Owned by other children:
  - CLI setters (child O; `PUT /me/settings` stays cookie-only);
  - the Models tab (I) and labels (A);
  - the resolver and previews (F, J2, K);
  - per-role planning settings, template effort (N), per-harness template pins (G).
- No planning pair for chat, judge, cross_check, task review, job or isolated research runs, nor for summaries.
- Re-reading settings at approval.
- Cross-check checker defaults (D13).
- A `runtime` planning effort: planning offers the five explicit levels.
- Broadening `RequestWorkerPause` (D8).

## Modules and seams

### 1. Which claims carry a planning pair

Lead-lane claims only: those on which H sets `default_effort_source` (`leadcfg.LeadLane`). These are claims where:

- `checkerResolution == nil`;
- the run is not a review run;
- the kind is one of `issue`, `ci_fix`, `self_improve`, `prompt`, `mr_rework`, `task`;
- `egress_profile_id IS NULL`.

Chat and judge have their own assembly. A seeded or pre-approved claim never enters the plan phase.

A classification test over `fixtures/run-kinds/registry.json` requires every kind to be marked planning or non-planning, so a new kind is placed deliberately.

### 2. Server: plan-phase resolution (`api/internal/workersvc/claim_phase.go`, new)

`resolvePlanPhase(in LeadSettingsInput, implement LeadSettings) *ClaimPhaseSettings` is a pure function, called from inside H's `resolveLeadSettings` (not from `assembleClaim`). It receives the implement result `resolveLeadSettings` has just computed. This keeps the single entry point for lead precedence (fold-r3 ruling; H §9 uses the same names).

- `LeadSettingsInput` gains `PlanningModel` and `PlanningEffort` (`pgtype.Text`). `assembleClaim` fills them from the owner's Claude planning columns, read by a strict new `GetUserPlanningClaude` (blank = unset), beside its other `LeadSettingsInput` reads.
- `LeadSettings` gains `Plan *ClaimPhaseSettings` (H §9's name).
- `assembleClaim` copies `LeadSettings.Plan` into `ClaimConfig.PlanPhase`, as it does with the other `LeadSettings` fields. It makes no second precedence decision.
- Rule, per setting, independently:
  - a frozen run value (`runs.model`, compatible: schedule pin or P's `run_override`; P's frozen `runs.effort`) wins in both phases;
  - otherwise a set planning value applies to the plan phase;
  - otherwise the plan value equals the implement value.
- An incompatible schedule pin is dropped as today (`DroppedRunModel`), then the rule applies.
- The planning model is **always shipped** when the rule selects it, including when `default_model` is omitted (R2-2, buddy Q1). Whether it can be honoured is decided on the worker from what it will send (section 5).
- Output: `nil` when no plan value came from the planning lane. Otherwise `ClaimPhaseSettings{Model *string, Effort *string, ModelSource, EffortSource}` with H's tokens: `user_planning_default` for a planning-lane value, the implement source otherwise. Shipped as `ClaimConfig.PlanPhase` (`json:"plan_phase,omitempty"`), Claude claims only here.
- **Capability `claude_phase_models_v1`.**
  - Server: constant, protocol vocabulary, `protocolOrder` (or `FilterProtocol` strips it) and the golden `testdata/vocabulary.json`.
  - Agent: protocol self-report in `agent/src/worker.ts`.
  - With `PlanPhase` non-nil and the capability missing, `assembleClaim` returns a new `errPhaseModelsCapabilityMissing` and the claim requeues, as with the custom-Codex precedent.
- **Persisted combination violation (claim time).** If persisted settings violate the active planning-effort/Runtime-default constraint, reject claim assembly with a non-retrying configuration error naming both fields and their remedies. Deliver no substituted planning effort. The save-time validator remains the normal prevention path.
- **Errors.** The planning read is strict: an assembly error takes the existing requeue path. Nothing is re-read at approval.

### 3. Server: claimability, capacity, health and hosted placement (the mirrors)

**One predicate:** a run needs `claude_phase_models_v1` when all of these hold:

- its harness is `claude`;
- it is a lead-lane kind (section 1, written in SQL: kind list, `review_target_run_id IS NULL`, `egress_profile_id IS NULL`);
- `u.planning_claude_model IS NOT NULL OR u.planning_claude_effort IS NOT NULL`.

There is no trim in SQL: the PUT stores trimmed values or NULL, so the SQL predicate is a superset of the Go `TrimSpace` check (dba 16). Each site adds one correlated PK lookup on `users`, like the existing `default_codex_model` subselects; negligible, not measured.

The predicate goes into every mirror:

- `ClaimRun`: the claimant clause and the fleet-spread peer clause;
- `CountOnlineWorkersClaimableForRun`;
- `ListUnplaceableQueuedRunsForEphemeral`;
- both `ListSaturationQueuedRunsForEphemeral` sites;
- health, in three pieces mirroring the custom-Codex reason (R2-8):
  - a computed `PhaseModelsRequired` column in `ListActiveRunsForHealth`;
  - a new satisfier count `CountOnlineWorkersSatisfyingPhaseModels`, declared on the narrow querier interface in `service.go` and implemented by the test fakes, and called in the queued-reason rung;
  - the docker-probe capability filter beside `CodexCustomModelV1`'s.
- New reason `reasonNoPhaseModelsCapableWorker = "no worker supporting separate planning settings is online"`, ordered after the custom-Codex reason.

**Over-approximation, accepted.** The predicate does not evaluate frozen values or plan state. A moot planning value still needs a current worker, and any worker of this release qualifies.

**Non-looping invariant.** `assembleClaim` re-checks only when `PlanPhase` is non-nil. That implies the predicate held at assembly, so it held at claim unless the owner set a value in between. After the requeue, `ClaimRun` sees the value and excludes the worker.

### 4. Server: settings storage, cells and API

- **Migration** (draft `00353_user_planning_claude.sql`).
  - Up: `ALTER TABLE users ADD COLUMN planning_claude_model text, ADD COLUMN planning_claude_effort text;` Down drops them.
  - Nullable, no backfill, no index, no CHECK (the 00285 convention). Old binaries ignore the columns.
- **Stored fields.** `userSettingsDTO` and `apitypes.UserSettingsDTO` gain `planning_claude_model`, `planning_claude_effort` (null = "Same as the run").
- **Read-only cells** on GET and the PUT response (cross 18):
  - `planning_models: [ModelSettingCellDTO]`, C's shape. `model` is the stored value. `default_model` / `default_source` are the implement lane's resolution: lane, else the lead template's model with `template` when the server can name it, else `builtin`. `resolved_model` / `source` are the planning value with `user_planning_default` when set, else the default pair.
  - `planning_efforts: [EffortSettingCellDTO]`, B's shape `{harness, effort, default_effort, default_source, resolved_effort, source, mode}`, filled the same way.
  - Claude entry only here; Q-codex adds the Codex entries. Both cell types are consumed as landed by C and B.
  - The PUT request rejects both arrays as unknown fields.
- **Labels** come from C's `fixtures/model-settings/contract.json` via `modelSettingSource` / `modelSettingSourceLabel`, keyed by (field, token). This PRD adds rows for fields `planning_model` and `planning_effort`:
  - `user_planning_default` = "your choice";
  - the default rendered "Same as the run · <value> (<label of default_source>)".
  - Run-level labels stay H's.
- **PUT validation.**
  - Model: C's `agenttmpl.ValidateClaudeLaneModel` (the five-id guard, fold-r3 ruling; cross 11). Custom Claude IDs are accepted.
  - Effort: `validateEffort` (five levels).
  - Blank after trim clears (existing semantics). A 400 names the field, before any write.
- **Writes and the combination rule** (user decision 2026-10-21; active only when Q-spike recorded "keeps" for Claude).
  - Every write of `planning_claude_effort`, `default_effort` (B's `runtime` setter included) and, in Q-codex, the Codex twins goes through one handler function in one transaction:
    1. `SELECT … FROM users WHERE id = $1 FOR UPDATE`;
    2. compute the post-write values;
    3. reject the combination "planning effort set + that harness's effort `runtime`" with the 400 of example 7 (`invalid_settings`, fields `planning_claude_effort` and `default_effort`);
    4. otherwise write with `SetUserPlanningClaude` (a conditional UPDATE in the `SetUserHarnessModels` shape) and the existing effort setter in the same transaction.
  - At 522aab1ba `PutMySettings` is the only caller of the effort setters. B lands first, so this PRD routes B's landed `runtime` setter path through this function. Child O's setters must call it too (stated in O's issue).
  - Two concurrent saves serialize on the row lock, so neither can create the combination. A LiveDB test proves it with `pg_blocking_pids` and no sleeps.
  - If Q-spike recorded "reverts", the rule is not added and the combination works.
- **Mock and contract.** The web mock is a second implementation of this PUT validation, so it is a contract. A shared `fixtures/settings/planning-defaults-cases.json` is read by a Go handler test (M1) and `web/src/mocks/mockApi.planningDefaults.test.ts` (M2). One case per rule, each asserted exercised: absent keeps, null clears, value sets, blank clears, a `role` Codex id rejected, custom Claude accepted, over-length model, unknown effort, and the combination in both orders when active.
- **Routes.** No new settings route. The hold route is in section 6.

### 5. Agent: per-phase lead request (through H's seam)

- **Protocol.** `ClaimConfig.plan_phase?: { model?: string; effort?: EffortLevel; model_source: string; effort_source: string }` in `agent/src/protocol.ts`. Absent means both phases use the claim's lead pair.
- **Resolver.** H's `resolveLeadRequest({config, harness, templateModel, providerDefaultModel})` gains `phase`. For `plan` it reads `config.plan_phase` when present, else returns exactly the implement result.
- **Unresolved implementation model** (R2-2, buddy Q1). Configured planning values are never replaced merely because the implementation side cannot be restored on the selected conversation mechanism. If omission cannot restore an unresolved implementation model, do not start planning on a substituted model: fail visibly as an unsupported phase configuration, naming the remedies of setting an implementation model or clearing the planning model.
- **Cases.** Every case is appended to H's `fixtures/lead-config/contract.json` `request_cases` (read by `LEAD_REQUEST_CASES`), with no second matrix:
  - plan with planning model and effort;
  - plan with frozen model;
  - empty lane + template;
  - no template model (unsupported phase configuration, when Q-spike recorded that omission cannot restore the implementation model);
  - absent `plan_phase`.
- **Executor binding (D2).** The plan block's turns take the plan request: first plan, clarification rounds, missing-plan nudge, revise, plan in-place restart. Every turn after approval or on a pre-approved start takes the implement request. `implementConfig` sets `model` and `effort` from it and omits an unresolved key (never `model: undefined`).
- **Subagents.**
  - With "apply model to agents", each phase's roster takes that phase's lead model. The plan override is applied to the plan copy, not to the shared `assembled.subagents`, so no plan model leaks into implement subagents (adapter test).
  - Unpinned subagents inherit the phase lead model and effort; template pins (and N's explicit effort pins) win.
- **Reporting.** `LeadConfigReporter.enterPhase("plan", planRequest)` and `enterPhase("implement", implementRequest)` at H's `driveTurn` call site. The returned `PhaseEntry` is passed to `observe`. No other reporting path.

### 6. Approval transition and the hold (D1 = same conversation)

**Mechanism.** The implement turn resumes the plan session with `implementConfig`'s pair, as proved by Q-spike (D1 cases 1 to 3, 7).

**Switch verification: the settings barrier** (redesign 2026-11-20, replacing the init-based (b)/(c) options; evidence in D1 and the #3246 follow-up characterization).

Q-spike case 9 showed that the SDK init frame is emitted only after the prompt is consumed, and aborting on init still let one provider request through. Init is therefore reporting only, never the guard. The guard is the CLI's `get_settings` control request. It is answered while the prompt is withheld, and it reports `applied.model` (canonical id) and `applied.effort`, which matched the first wire request in every tested shape.

The first implement turn of a claim:

1. **Withhold the prompt.** Start the resumed plan session with `implementConfig`'s pair and a gated prompt stream (a variant of `promptStream` in `sdk-messages.ts`). Nothing is yielded yet.
2. **Read the applied settings.** Call `getSettings()` once, with a bounded timeout. Feature-detect it: the method exists at runtime on the pinned SDK but is absent from the public `Query` typing, so a missing method is a verification failure, never a pass.
3. **Compare strictly.** Compare `applied.model` with the canonical id expected for `implementConfig.model`, resolved independently by uzi from the requested alias (for example `opus` to `claude-opus-5-5`); never take the expectation from the runtime's own answer. Compare `applied.effort` with the requested explicit level. For Runtime default, omit `options.effort` and compare against an independently established omission result for the selected model and runtime pin (the tested SDK 0.3.319 / CLI 2.1.319 reports and sends `medium` in the characterized Opus and Sonnet cases). Do not compare it to the request-side null, and do not record an observed `medium` as null. If the expected default cannot be established independently, hold. Keep the requested Runtime-default provenance separate from the observed applied value; `medium` here is pin-specific characterization, not a promise for every model, provider or policy.
4. **Release or hold.** On a match, release the prompt exactly once. On any other outcome, abort the query, reap the runtime, and take `phase_switch_unavailable` before any implementation work. That covers a missing, malformed or timed-out answer, an unexpected third model, an effort mismatch, or an unsupported method.
5. **Init never opens the prompt gate** or repairs a failed settings verification. Record it through H after release. If it contradicts the admitted model/effort pair, stop the query, reap the runtime, and take `phase_switch_unavailable` before further implementation work. This cannot undo a provider request already admitted; do not claim that it can. Missing optional init fields are not a contradiction. Adapter negative test: settings match, gate opens, contradictory init arrives, then no further tool or implementation work happens and the run takes the existing hold.

The same barrier applies to a pre-approved reclaim that has no planning init in the current claim (a new process resuming the approved session), and to a switch back to the planning pair after a re-plan. Proven shapes (credential-free, SDK 0.3.319 / bundled CLI 2.1.319): fresh, resume, switch-back and new-host reclaim; an alias override appearing in both settings and wire; omitted-model and omitted-effort resumes; 500 ms of withholding with zero provider requests; three mismatch controls aborting before init with zero requests through query and child settlement.

**Pin sensitivity.** Because `getSettings()` is not public typing, add a characterization test against the pinned bundled CLI that asserts: the method exists; `applied.model` and `applied.effort` are present; their values equal the first wire request; and withholding produces zero provider requests. Any SDK or CLI bump that breaks it fails that test before it can reach a worker. On a worker whose SDK lacks the method, phase verification is unavailable (hold), never silently unverified.

If this barrier stops holding on a future pin, the fresh-conversation fallback below goes through the redesign gate; do not proceed without the check.

Verification must cover the intended implementation model, including supported aliases, unexpected third-model results and pre-approved reclaims that have no planning init in the current claim. A missing or unverifiable result does not establish a successful switch. Q-spike records the comparison method and barrier in D1. An unconfirmed switch takes `phase_switch_unavailable` before implementation work.

**The hold** (fold-r3, v5 §1; R2-1). This is an active-run, generation-fenced worker outcome, not `RequestWorkerPause`, which stays unchanged.

- **Worker: `enterPhaseSwitchHold`** (`runner.ts`), modelled on `enterCompletionHold` / `enterWallPark`, in this order:
  1. **Quiesce.** Stop the query and reap the runtime through the existing guarded sink.
  2. **Preserve.** Set `preserveSession` and `preserveRecoveryClone`. Keep the approved session id (the plan session, never a new one), the approved plan and selection already persisted at the gate, and the retained work. Attempt the existing checkpoint publication while claim credentials are usable, and pin the recovery generation. Never claim verified durability without proof.
  3. **Release.** Post the hold. On 200, settle custody through the existing parked-disposition protocol and close the batcher, exactly as `enterWallPark` does on an accepted park.
- **Status line, posted first:** "Paused: the runtime kept the planning model <m> instead of switching to <m'>. Resume retries on a new claim; Cancel ends the run."
- **Server: `POST /api/worker/runs/{id}/phase-switch-hold`** (`WorkerRunPhaseSwitchHold`, modelled on `WorkerRunBaseAttestationHold`).
  - Calls `SetRunPhaseSwitchHold :one` (new `api/internal/store/queries/phase_switch_hold.sql`): `status → paused`, `hold_reason = 'phase_switch_unavailable'`, `hold_captured_head` from the request, `open_question_id = NULL`, health reset (mandatory, as in that statement).
  - Fence: `WHERE id = @id AND worker_id = @worker_id AND claim_released_at IS NULL AND claim_generation = @claim_generation AND status IN ('running','awaiting_approval')` plus the lead-lane kind predicate. There is no `plan_approved` column, so plan approval is checked by the service, in the same transaction under the row lock (`FOR NO KEY UPDATE`), before the UPDATE. It uses the derivation `assembleClaim` already computes: `run.AutoApprove || HumanPlanApproved || run.PlanSource == seeded`, where a cross-check pass is an auto approval. An unapproved run gets 409 with its real status. Including `awaiting_approval` covers an approval whose `running` report has not been made yet (the Codex approval epoch; Claude if it verifies before reporting).
  - Pending-pause columns are untouched. `worker_id` is kept, so a same-worker resume is possible.
  - Answers: 200 with the run when applied, or already applied for this generation (lost-ACK retry, answered from a re-read without touching `status_since` or budgets); 409 with the real status when the fence refuses; 404 when the run is no longer this worker's.
  - Body: `DecodeJSONStrictBounded`, `claim_generation` required (400 `claim_generation_required`), `refuseJobRuns`. Rows are added to `route_limiter_mounts_test.go` (no limiter, like the sibling hold routes) and to `jobRefusedRoutes`. No CLI-token route, so no `cli_auth_livedb_test.go` row.
- **409 path.** The run was cancelled, reclaimed or released meanwhile. The worker runs no implement turn. It keeps clone and custody per the existing park answer rules, and ends the flight through the existing superseded or terminal handling. Never a retry on the planning pair.
- **Resume and Cancel.** `ResumePausedRun` already admits the reason (it refuses only the reasons listed in the baseline); a test pins that. The next claim's phase follows server state (`resumePhaseFor` in `api/internal/workersvc/claim_assembly.go`): an approved run implements directly and re-verifies; a plan-phase hold taken on an unapproved run shows the plan gate again when a plan and session exist (a revise re-plans and re-verifies), or re-plans when there is no plan. Cancel is the existing path. No promoter or extension writer leaves this hold automatically.
- **Shared with P.** `phase_switch_unavailable` is added as an entry in P's fixture `fixtures/hold-reasons/presentation.json`, the closed hold-reason presentation set P created beside its `run_override_unavailable`. The set's members are the web texts in `ui.tsx` / `pauseOrigin.ts` / `runBadge.ts`, the CLI renderers in `run_render.go` / `run.go` / `run_wait.go` / `run_steer.go`, and the agent `client.ts` / `protocol.ts` unions. The park statements, routes and paths stay separate: P parks an undelivered claim through `finishRunClaimTx`; this parks an active run.
- **Display.** H `display_cases` gains DC-Q1: paused on `phase_switch_unavailable`, plan rows present, implement row requested-only ("not reported").
- **Fallback (D1 = fresh conversation): requirements for the redesign gate.**
  - A separate prompt input. Do not reuse `gateApprovedWithoutSession`, which means "no session existed", excludes cross-check approvals and gates `priorWork`.
  - The adopted plan: for a cross-check approval, the canonical bundle from `validateCheckedPlanBundle`.
  - The frozen milestones and gate revision.
  - The selected roster.
  - Owner follow-ups and planning clarification answers as fenced history within the #3169 bounds. Today only `renderPriorOwnerHistory` renders follow-ups, so a renderer is needed for clarification answers.
  - The decisions memo.
  - #3184 receipt and restart rules; the new session id persisted at first init.
  - An incomplete package takes the same hold. Never implement on the planning pair.

### 7. CLI (M1)

- `uzi settings get` gains two rows in C's "Model settings" table, through C's renderer: SETTING `planning model` and `planning effort`, HARNESS `claude`, STORED / DEFAULT / RESOLVED / SOURCE from the cells.
- `--json` carries the stored fields and the cells.
- The hold reason renders through the shared set (section 6).
- Setters are child O's.

### 8. Web: Run defaults "Planning" card (M2; child I moves it later)

- **Placement:** after "Reasoning effort".
- **Intro copy:** "Use a different model and reasoning effort while a run plans. When the plan is approved the run continues on your worker model and effort above; if it plans again, it uses these. Planning stays on the run's harness. Handoff (task) runs also plan first. A model chosen by a schedule or when starting the run applies to both phases."
- **Claude row:**
  - "Claude planning model": `ModelSelect`, `harness="claude"`, `defaultLabel` "Same as the run · <default_model> (<label>)" from the cell.
  - "Claude planning effort": `EffortSelect`, with the same pattern from the effort cell.
- **Save:** one "Save planning defaults" button, one PUT with dirty fields only. Disabled while clean or busy; a model warning blocks it. The server's combination 400 is shown under both fields.
- **Mock:** the store follows section 4, and its test reads the shared fixture. There is no design mock; the PRD text is the reference.

### 9. Docs, specs, ADR, changelog

- **Docs:**
  - M1:
    - `docs/worker-model.md`: new "Planning model and effort" section, incl. the task-run plan turn and its cost, and the empty-lane template behaviour; fix the stale shared-effort sentence.
    - `docs/worker-effort.md`: planning effort and the combination rule; fix the stale "no CLI settings command".
    - `docs/cli.md`: the rows.
    - `docs/scheduling.md`: a pinned model also applies to planning.
    - `docs/run-activity.md`: the notes and the hold line.
    - `docs/run-pause.md`: `phase_switch_unavailable`, Resume and Cancel.
    - `docs/hosted-workers.md`: the health reason.
  - M2: the card.
  - `task docs:sync` in each milestone that edits docs.
- **Specs:** `specs/human.md` Feature #17 lines `(AI-synced YYYY-MM-DD)`.
- **ADR** 3252-phase-model-switch (not backticked until it exists): the switch only at the plan-approval boundary, the pair fixed per claim, the per-harness mechanism and evidence, the barrier choice, "never implement on the planning pair" and the hold. Q-codex appends the Codex section.
- **Changelog:** one fragment (M1), extended in M2.

### 10. If F is on the base, extend it

F is blocked by B, B2, C, D and G, not by this PRD, so either order can happen. If F is on the base, extend F's files, as its "Extended by: Q-claude" line lists:

- `api/internal/handler/effective_settings.go` (composition), `api/internal/workersvc/effective_settings.go` (`PredictLead` gains the plan/implement pair through `resolveLeadSettings`) and `api/internal/apitypes/effective_settings.go`: planning cells per lane (C model cell, B effort cell, source `user_planning_default`), and the plan/implement pair in the start and schedule previews;
- `fixtures/api-contract/effective_settings*.json` (contract and behaviour corpus);
- the recorded-response mock in `web/src/mocks/mockApi/settings.ts` and the types in `web/src/lib/apiTypes.ts`;
- F's rows in C's table and C's `fixtures/model-settings/contract.json`;
- the claim-equivalence test;
- plan cases in H's `request_cases` (already added by section 5).

If F lands later, F's line covers it.

## Testing decisions

- **Pure rule.** A table test for `resolvePlanPhase`: each precedence row, the persisted-combination configuration error (non-retrying, both fields and remedies named, no plan effort delivered), frozen model with planning effort, nil output, the always-shipped model with `default_model` omitted. Follow `harness_model_compat_test.go`. H's `resolveLeadSettings` tests gain the planning inputs and assert `LeadSettings.Plan`.
- **Claim (LiveDB).** Follow `codex_effort_claim_livedb_test.go`:
  - `plan_phase` on each lead-lane kind and absent on every excluded kind;
  - the byte-identical wire with nothing set (goldens `testdata/claim_skills_wire.json` and the CI wire goldens, `agent/test/claim-skills-contract.test.ts`);
  - the capability requeue;
  - the non-looping pair: two workers (capable and incapable) online; a settings write between `ClaimRun` and assembly.
- **Mirrors.** One LiveDB matrix runs the same fixture runs and workers through every mirror query plus the Go health filter and satisfier count, and asserts identical eligibility. Follow the custom-model cases in `api/internal/store/ephemeral_lease_protocol_livedb_test.go`. Plus a health-reason test and a querier-fake compile check.
- **Settings.**
  - Handler tests follow `user_settings_codex_effort_test.go`; store LiveDB follows `user_codex_effort_livedb_test.go`.
  - The combination rule: both orders, and concurrent saves blocking on the row lock (`pg_blocking_pids`, no sleeps).
  - Cells.
  - `apitypes/wire_test.go`; C's label fixture rows.
  - The CLI table test follows C's renderer tests and `api/cmd/uzi/settings_test.go`.
  - The shared fixture, Go half.
- **Hold (LiveDB plus worker).**
  - `SetRunPhaseSwitchHold` for each of the six kinds, interactive and not, from `running` and from `awaiting_approval`.
  - A cross-check-approved run (`auto_approve` false, no human approval).
  - The fence refusals: other generation, released, other worker. A lost-ACK repeat.
  - `ResumePausedRun` admits it; Cancel works.
  - Worker side (`runner` test with the fake API): quiesce, then preserve, then release ordering; the 409 path runs no turn and keeps custody.
- **Agent (existing fakes).** `agent/test/sdk-executor.test.ts`:
  - the plan request on plan, clarification, revise and plan-restart turns;
  - the implement request after approval, pre-approved, after reclaim and on implement restart;
  - the reclaim re-plan;
  - absent `plan_phase` byte-identical;
  - "apply model to agents" per phase, no leak;
  - the unsupported phase configuration: no implementation model resolves and omission cannot restore one, so the run fails visibly before any plan turn, naming both remedies, and no plan turn runs on a substituted model;
  - the chosen barrier: zero provider calls before the verdict, and a negative with differing observations.
  - `request_cases` gains the plan cases.
- **Through H.** A run with a planning pair yields plan and implement rows with the expected requested values and sources; DC-Q1.
- **Web (M2).** `RunDefaults.test.tsx`: the card, default labels from the cells, the dirty-only PUT, the warning, the combination 400 under both fields; the mock fixture test.
- **E2E sweep.** `git grep -F` `default_model`, `default_effort`, the note and health literals and `phase_switch_unavailable` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`. There were no hits at 522aab1ba. Name any unrun phase.
- **CI time.** Report the measured `test:api` and agent-shard deltas.

## Milestones

### M1. Claude planning pair, end to end through the API and CLI

**Blocked by:** #3246 (D1 = same conversation for Claude), #3244, #3245, #3247, #3250.

- **Scope:** sections 1 to 7, 9 (M1 parts), 10 if F is on the base. That is:
  - the settings API, combination rule and cells;
  - claim resolution and capability eligibility in every mirror;
  - the executor switch, the barrier and the hold;
  - H reporting;
  - the `uzi settings get` rows;
  - the docs, ADR, specs and changelog.
- **Use:** an owner sets planning values through the existing cookie-authenticated `PUT /me/settings` (the shipped web client's API call, or curl with the session). Every run, health, the CLI read and the run page then behave as specified.
- **Dispatch-time checklist** (the worker checks each item and records it in the PR):
  - F on the base: section 10.
  - Q-spike D1 for Claude: the barrier choice (section 6) and whether the combination rule is active (section 4).
- **Fixed by the blockers (no condition):** pass P's landed `runs.effort` and `effort_source` into `resolvePlanPhase` and own `TestResolvePlanPhaseRunChoiceWinsBothPhases` against P's `run_override` (Q-claude lands after P); route B's setter through section 4's function and use B's `EffortSettingCellDTO`; add `phase_switch_unavailable` to P's hold-reason set.
- **Acceptance:** examples 1 to 8 except the web halves of 7 and 8; Codex claims byte-identical; `task gate:api`, `gate:agent`, `gate:repo`, `check-docs:web` green.

### M2. Web Planning card

**Blocked by:** M1.

- **Scope:** section 8, the web half of the shared fixture test, the hold-reason web texts if M1 left them to the shared set's web members, and the docs for the card.
- **Acceptance:** `RunDefaults.test.tsx` and the mock fixture test pass; `task gate:web` and `check-docs:web` are green.

No prefactor milestone: H already extracts `resolveLeadSettings`, so nothing behaviour-neutral remains to split out.

## Decision Log

- **D1. Mechanism: same conversation is provider-compatible; the init guard is not established, and section 6's settings barrier (D18) replaces it (Q-spike #3246, user-run evidence 2026-11-20).** SDK 0.3.319 and bundled CLI 2.1.319, using the selected subscription credential. Implementation provenance is the squash-merge of the Q-spike PR; the durable #3246 evidence comment linked below retains the live reports. This is provider/runtime compatibility evidence; dispatch rests on D18's barrier.
  - **Switch, reclaim and re-plan:** fresh SDK query/CLI invocations resumed the requested session on `claude-opus-5-5`/high -> `claude-sonnet-5-5`/medium -> Opus/high. All nonce controls passed. The credential-free real-runtime test independently compared init model and applied effort with provider request fields and checked the history nonce.
  - **Omitted model keeps the session model.** The live fresh default was Sonnet 5.5. Explicit Opus resumed as Opus with no `options.model`; a fresh query afterward still selected Sonnet. The dummy API-key fixture's fresh default was Opus, so defaults must not be generalized across the tested authentication modes.
  - **Omitted effort reverts to the observed medium default.** Explicit Opus/high resumed with no `options.effort` and reported applied medium. The conditional save-time rejection for "explicit planning effort + Runtime default implementation effort" is therefore not required for this tested Claude runtime. This does not relax model verification or other validation.
  - **Usage and provider acceptance:** the switched result's cumulative `modelUsage` retained separate Opus and Sonnet keys. The first Opus turn exposed one thinking block/32 thinking tokens; Sonnet completed the resumed conversation without a provider rejection and returned the nonce. Raw live provider payloads were not inspected, so forwarding versus transformation of individual prior thinking blocks is not independently established.
  - **First post-switch cost observation:** Sonnet reported uncached `inputTokens=2`, cache-read 0, one-hour cache creation 570 and output 117 (including 88 thinking tokens). Input here excludes cache-creation/read buckets; repeated cumulative snapshots must not be summed. This is one observed turn, not a general cache guarantee.
  - **Barrier, measured separately with dummy credentials:** on a newly launched CLI resuming an explicit plan session, `get_settings` replied while the prompt was withheld. After a 1,000 ms observation window the order was settings-ready -> gate-release -> prompt -> init -> HTTP request arrival. Init was not observed before prompt admission; aborting a separate resumed query on init still produced one Messages request at the fake after query/child settlement. Init preceding measured request arrival is not a zero-request abort guarantee. Neither proposed init guard is established. D18 records the reviewed replacement.
  - Sanitized live observations and verification boundaries: [Q-spike evidence](https://github.com/vtmocanu/uzi/issues/3246#issuecomment-7038584349).
- **D2. The pair is bound to the executor phase, not a latch.** "Re-plan reverts" needs no special case. Rejected: an "approved" flag, which a reclaim re-plan would read wrong.
- **D3. `plan_phase` is optional and omitted when the planning lane contributes nothing,** so the wire is byte-identical for everyone else.
- **D4. Both pairs resolve at claim; nothing is re-read at approval** (shared design §2). A new claim re-resolves.
- **D5. Precedence per setting:** frozen run value, then planning lane (plan phase only), then the run's own resolution. User decision 2026-10-21: an explicit run choice wins in both phases.
- **D6. The capability is a claim requirement in every mirror; a missing capability waits and never degrades.** This is the established pattern (`codex_custom_model_v1`, `completion_interlock_v1`), with no silent substitution. Rejected: r1's degrade-with-note.
- **D7. The predicate over-approximates** to keep every mirror simple and identical.
- **D8. Hold visibly through an active-run, generation-fenced park with reason `phase_switch_unavailable`, resumable and cancellable, for every admitted kind** (fold-r3, v5 §1 (a), buddy Q2).
  - Rejected: `RequestWorkerPause`, which admits only non-interactive issue runs and must not be broadened.
  - Rejected: P's claim-assembly park, since this run is active.
  - Rejected: a terminal failure (v5 §1 (b)), which loses Resume.
  - Rejected: continuing on the plan model.
- **D9. Unresolvable implementation side.**
  - Model: decided on the worker from what it will send (R2-2, buddy Q1). The planning model is always shipped and never replaced. If no implementation model resolves and omission cannot restore one on the selected mechanism, the run fails visibly as an unsupported phase configuration, naming both remedies.
  - Effort: rejected at save when Q-spike proves omission keeps the planning effort (user decision 2026-10-21). A persisted violation fails claim assembly with a non-retrying configuration error naming both fields; no substituted planning effort is delivered.
  - Rejected: replacing a configured planning value with the run's value plus a note (silent substitution).
  - Rejected: the r2 server-side drop on an omitted `default_model`, which fired for every empty-lane owner.
- **D10. Lead precedence stays in H's home.** `resolvePlanPhase` is called from inside `resolveLeadSettings` (result `LeadSettings.Plan`), and the worker goes through `resolveLeadRequest` with cases in H's `request_cases`.
- **D11. Two nullable columns, one transaction for every effort-related write.** The combination needs both values under one row lock.
- **D12. Settings cells use C's model cell and B's effort cell; labels come from C's (field, token) table.** Run-level labels are H's.
- **D13. Cross-check checker defaults unchanged.** User decision 2026-10-21 (Q2).
- **D14. Split into Q-spike, Q-claude and Q-codex.** User decision 2026-10-21.
- **D15. CLI is read-only here; setters are child O's,** which must call section 4's write function.
- **D16. The web mock's validation is a contract,** pinned by a shared case fixture.
- **D17. Milestones are vertical** (coordinator correction): M1 is the whole behaviour through the API and CLI; M2 adds the web card. Rejected: R2-4's "inert machinery, then settings and UI" split, which is layer-shaped.
- **D18. The switch guard is a settings barrier, not init** (redesign 2026-11-20, after Q-spike #3246). The pinned CLI answers `get_settings` while the prompt is withheld, and its `applied.model`/`applied.effort` matched the first wire request in every characterized shape (fresh, resume, switch-back, new-host reclaim, alias override, omitted model/effort), with zero provider requests during withholding and on three mismatch aborts. Init arrives only after prompt admission, so it reports but never gates. `getSettings()` is runtime-present but absent from public typing, so a pin-sensitivity characterization guards every SDK/CLI bump, and a missing method holds. Rejected: abort-on-init (lets a request through); an unverified switch; the fresh-conversation fallback while the barrier holds.
- **D19. Implementation record and divergences from the draft (2026-11-21, branch agent/issue-3252).** Recorded so the PRD does not describe different work than the branch contains.
  - **Migrations.** `00498_user_planning_claude.sql` and `00499_placement_phase_models.sql`; the head was read at implementation and both are renumbered at landing with `task migration:renumber`. `fn_placement_admitting_workers` is replaced through migration 00499, and the ephemeral demand gate admits phase-only runs (a run needing only `claude_phase_models_v1`). The vocabulary golden is untouched.
  - **Health.** The health cross-check loop gained a term for the phase capability, so the health reason reads "no worker supporting separate planning settings is online". Over-approximation (D7) can strand a run pinned to a custodian that cannot claim it, after the short resume-affinity window that follows a capability requeue (its length was not verified against `WORKER_AFFINITY_GRACE` or `RequeueClaimAssemblyExact`).
  - **Late effort re-derivation.** The effort expectation for the barrier is re-derived late, when the switch is verified, not at claim.
  - **Combination rule inactive.** Q-spike found an omitted effort reverts (D1), so the explicit-planning-effort plus Runtime-default-implementation rule is not shipped. These passages are inactive and superseded by this entry: D11 (one transaction for the combination), section 2's persisted violation, section 4's writes, section 8's combination 400, the testing decisions that exercise the rule, and M1 Delivery's related steps.
  - **Validation boundary.** The run-economy validation boundary supersedes the PRD's in-run `gate:api` / `gate:agent` / `gate:repo` / `gate:web` acceptance for this run. Those gates are not claimed here; they are exact-head CI at landing. CI time is read from exact-head CI, not asserted here (in-run, the pin test file grew from about 8.7 s to about 12.9 s locally).
  - **Characterized alias table.** All four aliases were characterized (`opus` to `claude-opus-5-5`, `sonnet` to `claude-sonnet-5-5`, `haiku` to `claude-haiku-5-5`, `fable` to `claude-fable-5-1`; omitted-effort defaults `medium`, `medium`, `medium`, `high`), so example 3 proceeds. Provider runs take the alias expectation from `ANTHROPIC_DEFAULT_<ALIAS>_MODEL` verbatim. With no effort sent, the expected effort comes from the model the runtime reports, so a provider run with no implementation effort holds unless the provider maps the alias to one of the four characterized ids. The provider check is create-time only (residual).
  - **Hold.** The hold request carries a `phase` field, used only to decide whether the plan-approval check applies; nothing stores the phase. A plan-phase hold is admitted without an approval check; the approval check applies only to the implement phase, and an unapproved run gets a 409 with reason `plan_not_approved`. A held run is excluded from readoption. In both `paused` and `queued`, late worker reports are refused for AwaitingInput, AwaitingFollowup, AwaitingApproval, Completed, Failed, FailedPlanRejected and SupersedeRunByWorker. CancelRunByWorker is exempt because it carries owner intent. RecordCompletionAttempt is left unguarded, like `base_attestation_missing`.
  - **Outcome classification.** The worker classifies the server's answer, including an implement-to-plan re-post and the lost-ACK versus Resume race. An unknown outcome is retried with no worker deadline of its own: the quiesced flight stays registered and retries, ownership read first, until a definitive answer (held, owner-resumed, `plan_not_approved` re-post, claim gone, confirmed cancel), worker shutdown, or a run paused at this claim with another hold reason and the claim kept (`server_parked`, defensive: no server-only producer known; the wall-clock sweep's ParkRunsAtWall releases the claim and so ends as claim-gone, and it skips interactive and awaiting-approval rows, whose unconfirmed holds retry until shutdown, cancel or a claim change; the flight ends without posting and the feed says "The server paused the run first (<reason>); this worker keeps the work."). A worker that shuts down or dies with the hold unapplied ends non-terminal with the clone and custody kept. Its next heartbeat snapshot then no longer lists the run, and the server's missing-from-snapshot handling (`Heartbeat` in `api/internal/workersvc/service.go`) takes over, but only if the hold was not applied (the run is still `running` or `awaiting_*`): on the first heartbeat that omits the run once its last status change (`status_since`) is older than the stale window plus one heartbeat interval, it is requeued while the run's requeues are under `RUN_MAX_REQUEUES`. Over the cap it parks in `recovery_wait` (cause `worker_requeue_exhausted`) when recovery evidence exists (checkpoint tip, available capture, publication enrollment, or uncertain or unknown evidence; `classifyWorkerExhaustion` in `api/internal/store/worker_exhaustion.go`), the usual case for a held run, which keeps its clone and custody; only without evidence is it failed with "worker lost the execution; exceeded re-queue budget" (a pending owner cancel is applied as cancelled). If the hold was applied but never confirmed, the run is `paused` on `phase_switch_unavailable` and stays held for the owner (Resume or Cancel). The stale-worker sweep applies only when the worker itself is gone. An owner Cancel during an unconfirmed hold reports cancelled. Process death is scoped to the existing stale-worker policy; no new route.
  - **Unsupported configuration.** A planning model with no implementation model is refused before any turn, regardless of approval state (a fresh pre-approved start with no session is refused too). A resumed gate is refused even if the owner would just approve. The refusal is skipped on provider runs (`!ctx.provider` in `agent/src/sdk-executor.ts`), because the provider map always yields an explicit model or fails with `provider_model_unmapped`.
  - **Documentation and not shipped.** Docs, ADR 3252-phase-model-switch, specs and two changelog fragments (M1 behavior, M2 card) are written. Not shipped: the Codex half (Q-codex #3253), a CLI setter (D15, child O), and any real-gateway or subscription-auth validation of the alias table. The PRD has been moved to `prds/done/`.
