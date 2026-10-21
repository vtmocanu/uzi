# PRD #3250: Choose a run's model and reasoning effort when starting it

**Issue:** #3250 (child P of epic #1703)
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Draft r3, 2026-10-21. Source anchors re-checked against origin/main `522aab1bacad0d67710be0cc23927d8db724c5ce` (since r2's `587b66d74` only agent tests and the new `schedsvc` CI scout changed; the scout writes no `runs.model`, and the three `scheduleModel(sched)` call sites in `scheduler.go` are unchanged apart from line numbers).
**Priority:** Medium (`priority::normal`).
**Parent:** #1703. Source: the epic's 2026-10-20 comment "per-run model and effort override at start". The user approved it superseding the epic's earlier "never a per-run override" lock (2026-10-21).
**Blocked by:** child #3244 and child #3245.
- **H:** P consumes H's frozen seam: §3 `resolveLeadSettings`, §6 `resolveLeadRequest` and the `request_cases` matrix, §8 presentation, §9 contract. It adds `run_override` values to that seam and builds no second reporting mechanism.
- **C:** P's new explicit model input uses C's five-id Claude-lane guard `agenttmpl.ValidateClaudeLaneModel` and C's fixture-pinned `agenttmpl.CuratedCodexModels`.

**Related:**
- **Q-claude** is blocked by P. It extends P's run-choice precedence into the plan phase and owns the both-phases test (D7). It also adds `phase_switch_unavailable` to P's hold-reason presentation set; Q's hold is a worker outcome and does not use P's claim path.
- **Q-codex** inherits both through Q-claude.
- **F:** lands last (shared design v4.1 §1); see "If F is on the base".
- **K:** the start preview, read-only; K renders P's start-preview fields.
- **N:** template effort pins win over P for pinned subagents (D13).
- **B:** owns `EffortSettingCellDTO`, which P uses only when F is on the base.
- **#3151:** adds Automation rows, including auto-approve, to the dialog P builds.

**Delivery:**
- One PRD, one uzi run, one PR, two serial milestones: M1 covers the API, claim, hold, CLI and run page; M2 is the web Run options dialog. Default harness.
- Migrations: draft 00352 adds the columns and backfill; draft 00356 validates the two CHECKs. Neither number is allocated in shared design v4.1 §7, so the coordinator should confirm them. Assign the final numbers at landing with `task migration:renumber`, keeping the pair adjacent and ordered.
- No implementation or validation step touches `.github/workflows/**`. LiveDB tests need Docker.
- P adds no HTTP route, so it adds no `route_limiter_mounts_test.go` or `cli_auth_livedb_test.go` row. When F is on the base, P extends F's existing start-preview route.

**Mock:** none (user decision 2026-10-21, P4). The PRD text is the presentation spec. `prds/mockups/1703-model-config/AfterStartSurfaces.dc.html` shows K's start-row preview and H's run header, which P extends but does not redraw.

## Problem

A user starting a run cannot choose the model or the reasoning effort for that one run. Both come from Settings (one lane per harness) and are read when a worker claims the run.

To give one hard issue `xhigh` today, the user changes their account default, starts the run, waits for the claim and changes the default back. Every run claimed in between also gets `xhigh`.

A CLI user and the dispatching skill cannot express "this issue needs `high`" at all: `uzi run create` has `--harness` but no model or effort flag. Only schedules can fix a model onto a run, and nothing can fix effort.

## Verified baseline (origin/main 522aab1ba)

| Seam | Anchor (symbol, file) | Current behavior |
|---|---|---|
| Create request | `Handler.CreateRun` inline `req` struct, `api/internal/handler/runs_lifecycle.go` | Accepts `issue_iid`, `wait_on_limit`, `mr_rework_enabled`, `plan_md`, `agent_selection`, `planned_commit`, `require_base`, `force`, `credential_override`, `harness`. No model or effort. `httpx.DecodeJSON` sets `DisallowUnknownFields` (`api/internal/httpx/respond.go`). The `WaitOnLimit` comment records "the user ruled against a start-run modal on 2026-07-27". |
| Create auth | `mountRepoRoutes`, `api/internal/handler/routes_repos.go` | `POST /repos/{id}/runs` is in the `mw.RequireUser` group: session cookie or CLI Bearer token. No auth change is needed for the CLI write. |
| Create service | `StartRunForUser` (`api/internal/workersvc/composite.go`) → `Service.CreateRun` → `Service.createRun` (`api/internal/workersvc/service.go`) | The manual path passes `model=nil`. `createRun` resolves the harness through `createRunResolved` / `createRunAtomic` (`harness_create.go`, `harness_resolver.go`). It validates a raw credential override against the D11-resolved harness inside the create transaction (`validateCredentialOverrideOn`); an invalid fact rolls back with no run row, no Codex freeze and no queued notify. A cheap pre-transaction read plus an authoritative in-transaction check is the existing pattern (`CountActiveBranchRunsForRef`). |
| Run model writers | migration `00118_schedule_run_model.sql`; queries `CreateRun` (`runtime.sql`), `CreatePromptRun` (`schedules.sql`), `CreateSelfImproveRun` (`selfimprove.sql`) | `runs.model text NULL` is written only by schedule origins: `CreateScheduledRun` and `CreateScheduledAutopilotRun` pass `scheduleModel(sched)` into `createRun` (`schedsvc/scheduler.go`); scheduled prompt runs and `CreateCIRepairRun` → `createPromptRunShared` write through `CreatePromptRun`; self-improve writes through `CreateSelfImproveRun`. The re-run command below prints exactly those three files. History check: no other INSERT ever wrote `runs.model`. No `runs.effort` column exists. |
| Backfill trigger | `completed_publication_run_guard` (row-level BEFORE INSERT OR UPDATE, last replaced in `00320_completed_publication_downgrade.sql`) | It returns NULL on UPDATE only for completed → completed with a changed `completion_final_head`, or through the permit branch. A backfill touching only new columns passes it (the dba review re-created both trigger functions and executed the backfill). |
| CHECK on `runs` | `00204_run_paused.sql` / `00205_validate_run_paused_checks.sql`, `00226_harness_cost_status.sql` / `00227_validate_harness_cost_status.sql` | Repo convention for a CHECK on `runs`: add it `NOT VALID` with an explicit name, then `VALIDATE CONSTRAINT` in a sibling migration. This avoids a full scan under ACCESS EXCLUSIVE. |
| Claim lead resolution (after H) | H §3: `resolveLeadSettings(LeadSettingsInput) LeadSettings` in `api/internal/workersvc/lead_settings.go`, called by `assembleClaim` | Pure function, no error return. Inputs: `Harness`, `RunModel`, `LaneModel`, `UserEffort`. Outputs: `Model`, `ModelSource`, `Effort`, `EffortSource`, `DroppedRunModel`. `assembleClaim` records `harness_model_fallback` for `DroppedRunModel` and sets `default_model_source` / `default_effort_source` only when `leadcfg.LeadLane` holds. |
| Claim parks | `finishRunClaim` / `finishRunClaimTx` (`claim_recovery.go`); `ParkCredentialDisabledRun` (`api/internal/store/queries/credential_disabled.sql`) | `errCredentialDisabled` is the one non-terminal, non-transient classification. It is excluded from the early return and decided by the `credDisabled` arm, which bypasses the authority rewrite and the hold class and returns `"paused"`, so `finishRunClaim` calls `publishSwept(run.ID, "paused")`. `ParkCredentialDisabledRun` hard-codes `hold_reason = 'credential_disabled'` behind the fence `worker_id`, `claim_generation`, `claim_released_at IS NULL`, `status = 'claimed'`. `errToolPackagesRejected` fails terminally. |
| Hold-reason surfaces | `HoldReasonBaseAttestationMissing` (`base_attestation_hold.go`, #3194); `ResumePausedRun` (`runtime.sql`, excludes `credential_disabled` and `base_attestation_missing` by name); web `ui.tsx` tone and label maps, `apiTypes.ts` hold-reason docs, `runBadge.ts` (`effectiveRunStatus`, `PARK_LABELS`, `needsHumanAttention`), `RunView.tsx` `CompletionStatePanel` (`held` deny-list) and the Resume panel exclusion list; CLI `run.go` hold constants, `run_render.go` `pausedHoldNotice`, `run_wait.go`, `run_steer.go`; `api/internal/uzicli/skill/SKILL.md` | `hold_reason` is free text with no CHECK (00328 comment). An unknown reason is resumable by default and would render as a completion hold in `CompletionStatePanel`. |
| Compatibility rules | `harnessModelCompatible`, `codexModels` (`harness_create.go`); after C, `agenttmpl.CuratedCodexModels` (4), `CodexRoleModels` (5, adds `gpt-6-luna`), `ValidateClaudeLaneModel`, all pinned to `fixtures/codex-models/models.json` | `harnessModelCompatible` (schedule pins, four-id guard): Codex accepts only the curated four; Claude accepts anything outside them. `ValidateClaudeLaneModel` (C) runs `ValidateModel`, maps `""` / whitespace to `""`, and rejects all five role ids. `ClaimRun` SQL's custom-Codex capability CASE treats a non-curated `runs.model` as absent. |
| Value validators | `agenttmpl.ValidateModel` (`model.go`), `ValidateEffort` / `EffortLevels` (`effort.go`) | Both trim, then treat blank as inherit. Model: a single token, at most 100 bytes, with no control, Cf or space characters. Effort: exactly one of `low medium high xhigh max`, case-sensitive. |
| Worker use | `agent/src/sdk-executor.ts`, `agent/src/codex/codex-executor.ts`; after H, `resolveLeadRequest` (`agent/src/lead-config-report.ts`) | Both harnesses apply the claim's single `default_model` / `default_effort` pair to planning and implementation. Claude: the model reaches subagents without their own model pin; effort is the SDK top-level effort, inherited by every subagent. Codex: every role gets the request effort. The worker forwards `default_*_source` unchanged. |
| CLI create | `newRunCreateCmd` (`api/cmd/uzi/run_lifecycle.go`); `HTTPClient.CreateRun` (`api/internal/uzicli/client_runs.go`); `Client` interface (`client.go`) | Tri-state flags are sent only when `Changed`. `statusError` / `serverErrReason` surface only a body's `error` / `reason`. |
| CLI and TUI read | `renderRunDetail` (`run_render.go`); `printRunFields` (`run.go`); `api/cmd/uzi/tui_*.go` | `HARNESS` row only; `--field` reads any top-level DTO scalar; the TUI renders no run model. After H: the `LEAD` / `LEAD_SOURCE` rows. |
| Run DTOs | `RunDTO`, `RunListItemDTO` (`apitypes/run.go`); `RunSummaryItemDTO` / `RunSummaryOf` with `TestRunSummaryItemDTOKeys`; `runToDTO` (`handler/runs_dto.go`); `fixtures/api-contract/run.full.json`, `run.zero.json`, `run_list_item.full.json`, `run_list_item.zero.json`; web `interface Run` (`web/src/lib/apiTypes.ts`) | `model` is documented as frozen "by the schedule". The summary item is an explicit third copy, and its key test fails until each new list field is placed or omitted on purpose. |
| Run page (after H) | H §8: `leadConfigView` (`web/src/lib/leadConfig.ts`) / `runpresentation.LeadConfigView` with adapters `leadConfigViewInput(run)` / `runpresentation.LeadConfigViewInputFromRun(run)`; `fixtures/lead-config/contract.json` (`labels`, `display_cases`, `request_cases`) | H's adapter sets `preClaim.model = run.model` with `modelSource = "schedule_pin"`; labels come from H's table (`run_override` → "this run"). |
| Web start | `IssueCard.tsx` (inline `HarnessPicker`, `TokenPicker`, Start run; native `draggable` root), `Board.tsx` (per-card `cardHarness` / `cardCredential`; preconditions re-read every 10 s), `IssueView.tsx`, `startRun.ts` `startRunWithCredential`, `api.createRun` | Positional arguments, with exact call arity pinned in `Board.test.tsx` and `IssueView.test.tsx`. `Modal.tsx` is the accessible dialog shell: not a portal, Escape always calls `onClose`, and only the backdrop is guarded by `closeOnBackdrop`. In "Other" mode with an empty input, `ModelSelect` emits `""`. |
| Mock mode | `createRun`, `web/src/mocks/mockApi/runs.ts` | Mirrors the client's positional arguments; a second implementation of the create contract. |
| Chat and Slack start | `StartChatRun` (`handler/chat.go`) → `StartRunForUserByPath`; `RunRequestCard.tsx` → `api.startRunFromChat`; chat tool `start_run` (`agent/src/uzi-tools.ts`) | The chat start route accepts an optional `harness`, which the web card does not send. No model or effort anywhere. |
| Specs | `specs/human.md` Feature #35 ("starting a run stays one click and inherits the user default", user 2026-07-27); Feature #17 | P keeps one-click Start, so #35 still holds. |
| Stale doc | `ARCHITECTURE.md`, bullet "The `lead` is the main thread" | It names `users.default_model` and an SDK default effort of `high`. Both are stale: per-harness lanes since PRD #1551, and an unset effort resolves to `medium` since #1157. |
| F (lands last; usually absent) | F's `api/internal/workersvc/effective_settings.go` `StartPreview`, `api/internal/handler/effective_settings.go`, `api/internal/apitypes/effective_settings.go`, `fixtures/api-contract/effective_settings*.json`, `web/src/mocks/mockApi/settings.ts` (recorded responses), the claim-equivalence test | Start-preview request `{harness, credential_override}`; response `{version, harness, refusal, refusal_detail, credential}`; strict 1 KiB body. |

Re-run command for the "Run model writers" row (verified at 522aab1ba):

```sh
for f in api/internal/store/queries/*.sql; do awk '/INSERT INTO runs/{p=1;b=""} p{b=b $0 "\n"} p&&/VALUES|SELECT|RETURNING/{p=0; if (b ~ /[ ,(\n]model[ ,)\n]/) print FILENAME}' "$f"; done | sort -u
```

## Outcome

An owner can pick the model, the reasoning effort, or both for one issue run when starting it: in the web Run options dialog (board card and issue page), or with `uzi run create --model --effort`.

- **At create:** the choice is validated against the run's resolved harness (C's guards) and rejected there if incompatible, then frozen on the run row.
- **At every claim** (resume, requeue, reclaim): the frozen choice is delivered, in both phases.
- **If a later claim cannot honor it:** the run is held visibly. It never silently runs on something else.
- **Display:** the choice shows with H's label "this run" in H's run-page chip and in the `LEAD` / `LEAD_SOURCE` rows, before and after the claim.
- **Defaults:** settings left on Default keep today's claim-time resolution. Start run without opening Options sends today's request body.

### Acceptance examples

1. **CLI, frozen across a requeue.** A Claude user runs `uzi run create --repo R --issue 42 --effort xhigh --model opus`. Result:
   - 201. `uzi run get <id>` shows H's `LEAD` row with `opus · xhigh` and a `LEAD_SOURCE` row naming "this run" for both (before the claim through P's pre-claim display cases; after it from H's rows).
   - `--json` carries `"model":"opus","effort":"xhigh","model_source":"run_override","effort_source":"run_override"`.
   - The user then sets Claude effort to `low` in Settings, the worker is lost and the run requeues. The reclaim's claim still carries `default_effort: "xhigh"` and `default_effort_source: "run_override"`.
2. **Incompatible model rejected at create.** A user whose effective default harness is Codex runs `uzi run create --repo R --issue 42 --model opus` (no `--harness`). Result:
   - HTTP 422 with `code: "model_incompatible_with_harness"` and `harness: "codex"`. The `error` text names the resolved harness and both remedies, with the Codex list generated from the curated set (for example "opus is not a Codex model, and this run resolved to the codex harness; start it on the Claude harness or choose one of: gpt-5.6-sol, gpt-6-astra, gpt-6-sol, gpt-6.1-sol").
   - The CLI prints that text. No run row exists and the board does not move.
   - `--harness claude --model gpt-6-luna` gets the same 422 (`harness: "claude"`) before the forge issue read, because C's guard rejects every Codex role id on the Claude lane.
3. **Web, effort only.** On a board card the user clicks Options, leaves Model on "Default · your settings", sets Effort to `high`, and clicks Start run in the dialog.
   - The request body carries `effort` and no `model`.
   - Before the claim, the run page chip reads `lead: model resolves at claim · high`, with "high: this run".
   - After the claim it shows the lane model with "your Claude default" and `high` with "this run".
   - Changing the Claude worker model in Settings before the claim changes the model the run gets; changing Claude effort does not change its effort.
4. **One-click Start unchanged.** Clicking Start run on a card without opening Options sends a request body identical to today's, asserted at the wire. The open-MR confirm and force retry behave as today, and a forced retry from the dialog re-sends the same model and effort.
5. **Unhonorable at claim, held.** A queued P run carries `model_source = 'run_override'` with a Codex model that the claiming release no longer treats as curated. Result:
   - At claim, `resolveLeadSettings` reports it as unhonorable. The run parks `paused` with `hold_reason = 'run_override_unavailable'` and one message naming the setting, value and harness. No turn runs and no fallback model is used.
   - Resume requeues the run; the next claim parks it again.
   - A schedule-fired run with the same incompatible pin still falls back to the lane with the existing `harness_model_fallback` note, and its claim's `default_model_source` is the lane's (`user_default`), not `schedule_pin`.

## Out of scope

- **Chat start card, Slack start card and the chat `start_run` tool:** they keep the account defaults for model and effort (D12).
- **Autopilot, schedules, sweeps, self-improve and prompt jobs:** behavior unchanged. Schedules keep a model pin with its soft fallback and the four-id guard, and have no effort pin. Their rows only gain the `schedule_pin` provenance stamp.
- **Moving existing worker lanes onto C's five-id guard:** C's follow-up, not P.
- **Other run kinds:** task/handoff, `mr_rework`, `ci_fix`, judge, cross-check children, review and `/api/v1` jobs neither accept nor inherit a run choice (D17).
- **Changing model or effort after create:** no edit endpoint, no run-page control.
- **"Apply model also to agents" for a run choice:** stays a schedule-only toggle (D13).
- **Custom Codex model IDs per run:** user decision P3.
- **Auto-approve, wait on limit, MR rework and CI autofix rows in the dialog:** #3151 (user decision P2).
- **The value and source shown in the "Default" option, and the "If started now" preview UI:** child F and child K. The 2026-10-20 comment's "Default · value (source)" lands with them; P ships "Default · your settings". When F is on the base, P supplies F's start-preview inputs, and K renders them.
- **Planning vs implementation pair, and its `phase_switch_unavailable` hold:** Q-claude and Q-codex. Q-claude extends P's precedence and owns the both-phases test (D7). Q's hold is a worker outcome that does not use P's claim path; its reason joins P's presentation set.
- **Recording and displaying requested and observed values per claim:** child H.
- **TUI:** it renders no run model today. If H adds TUI rendering, P's label reaches it through `runpresentation.LeadConfigView`.
- **A provider rejecting a custom Claude ID at runtime** (`model_not_found`): existing run-failure handling.
- **The dispatching skill choosing effort per issue** (`.agents/skills/uzi-watcher`): a separate tiny issue, filed after P lands.
- **Admin limits on which models users may pick:** out of scope for the whole epic.

## Modules and seams

### 1. Run start options at create: `api/internal/workersvc`, `api/internal/handler/runs_lifecycle.go`

Caller-facing behavior (the coder chooses the names; the behavior is binding):

- **Input:** optional raw `model` and `effort` from the create request. Absent is distinct from present.
- **Shape step (pure, before the forge issue read):**
  - P's inputs are not trimmed. A present value is a 400 if it is empty, contains any whitespace (leading, trailing or interior), or fails `agenttmpl.ValidateModel` / `ValidateEffort` on the exact string.
  - This rejection applies only to these new inputs. Every existing surface keeps trim-then-blank (shared design v4.1 §5).
  - Bounds: model at most 100 bytes, effort one of five literals, body capped by `httpx`.
  - The model token is later echoed on the admin agents surface and in the CLI, so `ValidateModel`'s Cf/control rejection is load-bearing.
- **Honorability predicate:** `runOverrideModelHonorable(harness, model)` is used at create and at claim (module 2), and never for schedule pins (they keep `harnessModelCompatible`).
  - Claude: `agenttmpl.ValidateClaudeLaneModel(model)` succeeds, which rejects all five Codex role ids, `gpt-6-luna` included.
  - Codex: `model` is a member of `agenttmpl.CuratedCodexModels`.
- **Fast-fail (pre-check):** when the request also names an explicit `harness`, D11 cannot fall back, so the predicate is evaluated in the handler before the forge issue read and answers the same 422.
- **Authoritative harness step (inside the create transaction, after D11):** the predicate on the resolved harness must hold. Otherwise the typed refusal rolls back the whole create: no run row, no Codex binding freeze, no queued notify. It runs after the existing credential-override validation and pin lock, so every credential refusal keeps its precedence.
- **Output:** the same `CreateRun` INSERT writes `model` plus `model_source = 'run_override'` and/or `effort` plus `effort_source = 'run_override'`. An absent setting leaves its pair NULL.
- **Source threading:** `createRun` gains an explicit model-source argument, set by each public entrypoint and never inferred from `triggerSource` (D11).
  - `CreateRun` passes `run_override` when the request carried a model.
  - `CreateScheduledRun` and `CreateScheduledAutopilotRun` pass `schedule_pin` when the schedule supplies a model.
  - Every other caller passes none.
  - `CreatePromptRun` and `CreateSelfImproveRun` stamp `schedule_pin` whenever they write a model.
- **Errors (typed sentinels mapped once beside `writeStartRunError`):**
  - Invalid model: 400 with the validator's message, or "model must not contain whitespace; omit it to use your default".
  - Invalid effort: 400 `effort must be one of low, medium, high, xhigh, max`.
  - Incompatible model: 422 `{error, code: "model_incompatible_with_harness", harness}`. The `error` string is surface-neutral and complete, and its Codex list is built from the curated set (`codexCuratedModelsSlice`, sorted), so it cannot drift. The CLI and the web display it unparsed. `code` follows the `issue_has_open_mr` / `no_credential_for_harness` precedent.
- **Invariants:**
  - Only `Handler.CreateRun` can produce `run_override`. `StartRunForUserByPath`, autopilot and schedule producers pass no run choice.
  - No row is created with a `run_override` model the predicate rejects for its frozen `runs.harness`.
- **Tested through:** the HTTP handler (live DB).

### 2. Claim-time precedence and the hold: H's `resolveLeadSettings`, `assembleClaim`, `finishRunClaim`

- **Edit point (unconditional): H §3/§9's `resolveLeadSettings` / `LeadSettingsInput`** in `api/internal/workersvc/lead_settings.go`.
  - `LeadSettingsInput` gains `RunModelSource` (`runs.model_source`), `RunEffort` and `RunEffortSource`.
  - `LeadSettings` gains a result field `Unhonorable` (nil, or `{Setting: "model"|"effort", Value, Harness}`). The function stays pure and gains no error return.
  - If F is on the base it calls this same function, so the edit point does not change.
- **Model, `RunModelSource = run_override`:** checked before the schedule branch.
  - If `runOverrideModelHonorable(Harness, RunModel)` holds: `Model = RunModel`, `ModelSource = run_override`.
  - Otherwise `Unhonorable` is set. It never goes through `DroppedRunModel` (H §9), so no fallback note is written and no lane is substituted.
- **Model, `schedule_pin` or legacy (a model with NULL source):** H's existing branch, unchanged. A compatible pin gives `schedule_pin`; an incompatible one sets `DroppedRunModel`, and the lane supplies the model with the lane's source (`user_default`, or empty so the worker decides `template` / `builtin`).
- **Effort, `RunEffortSource = run_override`:** if `RunEffort` is in `agenttmpl.EffortLevels`, then `Effort = RunEffort` and `EffortSource = run_override`, ahead of the user column and ahead of child B's resolution when B is on the base. Otherwise `Unhonorable` is set. With a NULL `RunEffort`, H's (or B's) path is unchanged.
- **`assembleClaim`:**
  - Maps a non-nil `Unhonorable` to the typed `errRunOverrideUnavailable` (nonsecret: setting, value, harness) before any further assembly.
  - Otherwise sets `default_model` / `default_effort` as today, and `default_*_source` only when `leadcfg.LeadLane` holds (H §3).
  - Manual issue runs are always lead-lane (`createRun` binds no egress profile), so a P run never loses its source today. A future egress-bound issue run would still get its values, without the chip.
  - The hold keys on the run columns, not on the lane.
- **The hold (`finishRunClaim` / `finishRunClaimTx`):**
  - **Classification:** `errRunOverrideUnavailable` is a third non-terminal, non-transient arm beside `errCredentialDisabled`, at both sites:
    - the early return (`!transient && !claimAssemblyTerminal && !errors.Is(..., errCredentialDisabled)` gains the new sentinel);
    - a decision arm like `credDisabled`, which bypasses the authority rewrite and the hold class and returns `"paused"`, so `finishRunClaim` publishes `paused`.
    - The job-kind rewrite and the Claude-checker mapping do not apply: neither kind can carry a run choice.
  - **Park query:** a sibling `ParkRunOverrideUnavailableRun` (beside `ParkCredentialDisabledRun`). It uses the same fence (`worker_id`, `claim_generation`, `claim_released_at IS NULL`, `status = 'claimed'`) and the same release side (`claim_released_at`, capability revoked, health cleared), and sets `hold_reason = 'run_override_unavailable'`. `credential_disabled`'s query is left untouched.
  - **Shared constant:** `HoldReasonRunOverrideUnavailable`, beside `HoldReasonBaseAttestationMissing`. Q-claude adds `phase_switch_unavailable` beside it.
  - **Message timing:** the one run message (kind `run_override_unavailable`, payload `{setting, value, harness, note}`) is written inside the park transaction, advancing `last_seq` there. A claim that `finishRunClaimTx` finds no longer exact goes idle with no message.
  - **Owner exits:** Cancel; or Resume through `ResumePausedRun`. Resume is allowed by default, because the query excludes only `credential_disabled` and `base_attestation_missing` by name; keep it that way deliberately. Two consequences, both stated and tested:
    - (a) the park set `claim_released_at`, so Resume takes `ResumePausedRun`'s released-claim branch. Per the reviewer's reading that branch nulls `worker_id` (no same-worker affinity); the test asserts whatever the branch does.
    - (b) `hold_reason` stays `run_override_unavailable` through queued and claimed until `SetRunRunning` clears it.
    - The next claim re-checks and parks again if the choice is still unhonorable.
  - **No automatic promoter:** enumerate every `hold_reason` predicate (`git grep -n "hold_reason" -- api/internal/store/queries/`) and record per site whether the new reason is included, excluded or unaffected. Expected: owner resume and cancel are included; the credential-disabled, limit and provider promoters key on their own reasons and are unaffected.
  - **Hold-reason presentation set (P owns it; Q-claude extends it).** P replaces the scattered per-site lists with one closed set per language, so a later claim-time hold is a one-entry addition:
    - **Fixture:** `fixtures/hold-reasons/presentation.json`, hand-authored. One entry per reason that is not a completion hold: the existing `credential_disabled`, `provider_unavailable` and `base_attestation_missing` (their current behavior, pinned unchanged), plus `run_override_unavailable`. Each entry gives `{reason, non_completion: true, owner_resumable: bool, tone, web_label, cli_notice}`. `run_override_unavailable` is `owner_resumable: true`, tone `warning`, label "model choice unavailable: owner hold".
    - **Web home:** `web/src/lib/runBadge.ts` exports the set and two predicates: `isNonCompletionHold(reason)` and `isOwnerResumableHold(reason)`. Every consumer reads them instead of a literal list: `ui.tsx` tone and label maps; `effectiveRunStatus`, `PARK_LABELS` and `needsHumanAttention`; `RunView.tsx` `CompletionStatePanel` (its `held` deny-list becomes `!isNonCompletionHold`); and `RunView.tsx`'s Resume panel exclusion (keeps `credential_disabled` and `base_attestation_missing` excluded, and offers Resume for `isOwnerResumableHold`). `apiTypes.ts` hold-reason docs point at the set.
    - **Go CLI home:** the hold constants in `api/cmd/uzi/run.go` become the same set with the same two predicates. `run_render.go` `pausedHoldNotice`, the `run wait` / follow notices in `run_get.go` / `run_wait.go`, and `run_steer.go`'s refusal text all read it.
    - **Pinning:** a web test and a Go CLI test each iterate the fixture and assert, per entry, the predicates, tone or label, and notice text, with every entry asserted visited. A literal hold-reason list left in a consumer is a review finding.
    - **Server side is not in the set:** `ResumePausedRun`'s by-name exclusions and the promoters stay SQL predicates (see "No automatic promoter"). The set governs presentation only.
    - **Docs:** `api/internal/uzicli/skill/SKILL.md` and `docs/cli.md` list the new reason.
    - **Q-claude** adds `phase_switch_unavailable` as one fixture entry plus its own label and notice text. It adds no new list.
  - **Reachability:** only when a release narrows the curated set, C's role set or `EffortLevels` while a P run waits. Tests seed such a row directly.
- **Subagents (D13):** P sets the lead's model and the run's effort.
  - Subagents without their own model pin follow the lead model.
  - Subagents without an explicit effort pin inherit P's effort: Claude through the SDK top-level effort, Codex through the request effort.
  - Child N's explicit template effort pins win for their subagent. P is not an "override every subagent" control.
- **Both phases (D7):** today the claim has one pair, so the run choice already covers planning and implementation.
  - P lands first: Q-claude is blocked by P. P ships the single-pair behavior, and P's claim tests assert the frozen pair reaches the one `default_model` / `default_effort` pair.
  - Q-claude extends it. Its `resolvePlanPhase` (`claim_phase.go`, called from `resolveLeadSettings`) reads P's `RunModelSource` / `RunEffort` / `RunEffortSource` inputs, so the plan-phase pair takes the run choice over `user_planning_default`, independently for model and effort.
  - Q-claude owns the run-choice-wins-both-phases test. Q-codex inherits the rule through the same function. P carries no plan-phase code and no conditional clause.
- **Re-claims:** create-time values are read from the row on every claim. Unset settings keep today's claim-time reads.
- Update the `DefaultEffort` doc comment in `claim.go`.

### 3. Consuming child H (frozen seam, H §9)

- **Claim fields:** P causes `default_model_source` and/or `default_effort_source = "run_override"` (module 2). The worker's `resolveLeadRequest` forwards them unchanged, and `run_lead_configs` CHECKs already accept the value. P needs no agent code change.
- **Request matrix:** P appends `run_override` cases to the `request_cases` key of `fixtures/lead-config/contract.json`, the single matrix, read by `LEAD_REQUEST_CASES` in `agent/test/lead-config-report.test.ts`:
  - Claude claim model `run_override`;
  - Codex claim model `run_override`;
  - effort `run_override` (explicit mode);
  - effort-only with the Claude template model (source `template`);
  - effort-only on Codex with the provider default (source `builtin`).
- **Display:** P widens `preClaim` in both adapters (`leadConfigViewInput(run)` / `runpresentation.LeadConfigViewInputFromRun(run)`) with `effort` / `effortSource` and a `modelSource` taken from `run.model_source`, treating a model with NULL source as `schedule_pin`. It appends these cases to `display_cases`, each asserted exercised by H's tests:
  - DC-P1 queued, model and effort `run_override`: chip `lead: opus · xhigh`; "this run" for both.
  - DC-P2 queued, effort only: `lead: model resolves at claim · xhigh`; "xhigh: this run".
  - DC-P3 queued, model-only `run_override`: `lead: opus · effort resolves at claim` with "this run", never "fixed by schedule".
  - DC-P4 claimed, no rows (H rule 3): as DC-P1 to DC-P3, so the frozen choice stays visible.
  - DC-P5 rows present with requested sources `run_override`: "this run" source lines (H rule 5).
  - DC-P6 paused on `run_override_unavailable` before any report: the chip as DC-P1 to DC-P3; the hold text says the choice cannot run on this harness and offers Cancel and Resume.
  - DC-P7 queued schedule pin with `model_source = 'schedule_pin'`: unchanged "fixed by schedule" (regression guard).
  - DC-P8 queued legacy row, model with NULL `model_source` (an old binary wrote it after a rollback): rendered as `schedule_pin` (dba 7).
- **Labels:** run-level labels come only from H's table (`run_override` → "this run").
- **CLI:** values reach `uzi run get` only through H's `LEAD` / `LEAD_SOURCE` rows. There are no `MODEL` or `EFFORT` rows. `--json` carries the run fields and `lead_config`.
- **No second path:** P never writes `run_lead_configs`, never infers from usage frames, and adds no badge beside H's chip.

### 4. Run DTO additions: `api/internal/apitypes/run.go`, `runToDTO`

- `RunDTO` and `RunListItemDTO` gain:
  - `effort *string`;
  - `model_source *string` (`run_override` | `schedule_pin`, passed through, NULL for legacy);
  - `effort_source *string` (`run_override`).
- `Model`'s doc comment loses "by the schedule".
- `RunSummaryItemDTO` deliberately omits all three: the summary is the polling shape and shows no model today. Add them to `TestRunSummaryItemDTOKeys`'s omission list with that reason.
- Re-record `fixtures/api-contract/run.full.json`, `run.zero.json`, `run_list_item.full.json` and `run_list_item.zero.json`.
- The web `Run` interface in `web/src/lib/apiTypes.ts` gains the same nullable fields.

### 5. CLI: `api/cmd/uzi/run_lifecycle.go`, `api/internal/uzicli`

- `uzi run create --model <alias|id>` and `--effort low|medium|high|xhigh|max`, each sent only when `Changed` and passed verbatim.
- Client-side usage errors, exiting before any request: an empty or whitespace-containing `--model`; an `--effort` outside the exact closed set. Harness compatibility is the server's.
- The 422 prints the server's `error` text as-is.
- `uzicli.Client.CreateRun` gains the two values. Every `Client` fake updates (compile-enforced).
- `uzi run get` gains no new rows (module 3). The hold reason gets its line and its `run wait` / follow notice (module 2).
- Version skew: a new CLI sending `model` / `effort` to an older server gets `400 invalid request body`, so the choice never silently drops.

### 6. Web Run options dialog (M2): `web/src/components/RunOptionsDialog.tsx`, `web/src/lib/startRun.ts`

- **Options object:** one exported type, for example `RunStartOptions`, with:
  - `harness: HarnessSelection`;
  - `credential: CredentialSelection`;
  - `model: string` (`""` = Default);
  - `effort: string` (`""` = Default).
- **Body builder:** one builder omits every Default value and every value whose row is hidden for the effective harness: the token when the run will start on Codex, and the harness when its picker is not shown. `api.createRun` and the mock take this object. The start helper keeps the open-MR confirm and force retry, re-sending the same object.
- **Byte-identity, pinned at the wire:** the default body equals today's, asserted on `request(...)`'s body. The positional-arity assertions in `Board.test.tsx` and `IssueView.test.tsx` are updated deliberately to the object form.
- **Mount:** once at the `Board` and `IssueView` level, keyed by issue iid, never inside the draggable `IssueCard`.
- **Rows, in order:**
  1. Harness: `HarnessPicker`, only when `bothHarnessesUsable`.
  2. Anthropic token: `TokenPicker`, hidden when `effectiveHarnessIsCodex`.
  3. Model: `ModelSelect` with `harness` = effective harness and `defaultLabel` "Default · your settings". Custom entry is offered for Claude only, and the Claude list excludes every Codex role id (C's web `CODEX_ROLE_MODELS`).
  4. Reasoning effort: `EffortSelect`, `defaultLabel` "Default · your settings".
- **Harness changes:** when the user changes the effective harness, Model resets to Default and effort is kept. A precondition refresh (the board's 10 s poll) re-evaluates row visibility but resets nothing; the builder drops a now-hidden value.
- **States:**
  - "Other" with an empty or whitespace-containing input disables Start run and shows an inline error.
  - While submitting: Start run is disabled, `closeOnBackdrop={false}`, and `onClose` is a no-op (Escape included).
  - The `startRunGate` reason disables Start run, is shown as text, and is referenced by `aria-describedby`.
  - Server errors appear in a `role="alert"` region inside the dialog, which stays open with the choices kept. This covers the 422 text, and a 400 from an older api during a roll.
- **Start rows (user decision P1):** board card and issue page show `[Start run]` (one click, defaults) and a secondary `Options` button (accessible name "Run options for #<iid>") under the same gate. The inline pickers and `Board.tsx`'s `cardHarness` / `cardCredential` maps are removed.
- **Seam for #3151:** #3151 adds its Automation fields to the options type and builder, plus an "Automation" section below P's rows. P adds no plugin mechanism.
- **Mock mode:** `createRun` takes the options, enforces the shared fixture cases and stamps the four fields. Mock run data gains the DC-P1 to DC-P3, DC-P6 and DC-P8 shapes.

### 7. Migrations (drafts 00352 and 00356)

- **00352 (`api/internal/store/migrations/00352_run_start_options.sql`), Up:**
  1. In one `ALTER TABLE runs`: add the plain columns `effort text`, `model_source text` and `effort_source text`. `effort` is validated at the write surface (the `00285` precedent).
  2. `ADD CONSTRAINT runs_model_source_check CHECK (model_source IN ('run_override','schedule_pin')) NOT VALID` and `ADD CONSTRAINT runs_effort_source_check CHECK (effort_source IN ('run_override')) NOT VALID`. A NOT VALID CHECK is still enforced on new writes and on the backfill's UPDATEs.
  3. Backfill `model_source = 'schedule_pin'` where `model IS NOT NULL`.
  4. No pairing CHECK (D11).
- **00352, Down:** drop the two constraints, then the three columns.
- **00356 (validate, the `00205` / `00227` shape):** `VALIDATE CONSTRAINT` for both, under SHARE UPDATE EXCLUSIVE; its Down is a no-op.
- Regenerate sqlc. `CreateRun` lists the new columns explicitly (the `runtime.sql` silently-omittable warning).

### If F is on the base (shared design v4.1 §1; F r2 "Extended by")

F lands last, so it is usually not on P's base. If it is, P extends F's files for its own fields in the same PR:

- **Start-preview inputs** (`api/internal/workersvc/effective_settings.go` `StartPreview`, `api/internal/handler/effective_settings.go`, `api/internal/apitypes/effective_settings.go`):
  - The request gains optional `model` and `effort`, validated exactly like create through module 1's shape step and `runOverrideModelHonorable` on the resolved harness. Shape errors are a 400; incompatibility returns `refusal: model_incompatible_with_harness`.
  - When an input is present, the response gains `worker_model` (C's `ModelSettingCellDTO`) and/or `worker_effort` (B's `EffortSettingCellDTO`, mode `explicit`) with `source = run_override`. Absent inputs add nothing (F's GET already carries the defaults).
  - The 1 KiB body bound still holds with a 100-byte model; assert it.
- **Fixtures:** `fixtures/api-contract/effective_settings*.json`: re-record the contract `full` / `zero` and add behavior-corpus cases (an override per harness, effort only, an incompatible model).
- **Web contract half:** `web/src/lib/apiTypes.ts` and the recorded responses in `web/src/mocks/mockApi/settings.ts`. K renders them.
- **Labels:** settings-surface labels come from C's `fixtures/model-settings/contract.json` `source_labels`, keyed by (field, token). Add (model, `run_override`) and (effort, `run_override`), both "this run", read through `modelSettingSource` / `modelSettingSourceLabel`.
- **Claim-equivalence test:** add a P-run case (preview equals claim) and a now-unhonorable case (held, never substituted).
- **Request matrix:** the `request_cases` additions (module 3) are made whether or not F is present.

## Testing decisions

- **Create seam, live DB.** A handler test modeled on `api/internal/handler/runs_credential_livedb_test.go` and `harness_route_bearer_livedb_test.go`:
  - Bearer and cookie callers.
  - Columns and sources stamped.
  - The 422 rolls back: no run row, no Codex binding, no notify.
  - Shape 400s and the explicit-harness 422 occur before the forge `GetIssue` (forge fake call count).
  - Credential-override refusals keep precedence over the model 422.
- **Shared fixture (the differential pin for the mock).** `fixtures/run-start-options/cases.json`, hand-authored, one case per behavior:
  - 201: Claude + `opus`; Claude + `claude-x-1`; Codex + `gpt-6-sol`; `max` on each harness; effort only; model only.
  - 422: Claude + `gpt-6-sol`; Claude + `gpt-6-luna`; Codex + `opus`; Codex + `gpt-foo`; Codex + `gpt-6-luna`.
  - 400: `HIGH`; `ultra`; empty model; `" opus"`; `"opus "`; `"   "`; effort `" high"`; a 101-byte model; a model with U+202E.
  - The Go handler test and a web mock test (beside `web/src/mocks/mockApi.test.ts`, following the `fixtures/run-kinds/registry.json` pattern) both iterate it and assert every case ran. The 422 cases assert `code` and `harness`, not the remedy list.
- **Resolver, unit.** `resolveLeadSettings` table cases for every `run_override` branch, `Unhonorable` per setting, precedence over the schedule branch, and the dropped-schedule-pin source. These are H's table plus P's rows.
- **Claim seam, live DB.** Modeled on `api/internal/workersvc/codex_effort_claim_livedb_test.go` and `harness_claim_repair_livedb_test.go`:
  - The claim carries the frozen pair and `default_*_source = "run_override"` on Claude and Codex.
  - Settings changes between create and claim, and between requeue and reclaim, leave a frozen setting alone and change an unset one.
  - A seeded unhonorable `run_override` model (and, separately, an out-of-set effort) parks on `run_override_unavailable`, with the message written in the park transaction and no payload. A not-exact claim leaves neither park nor message.
  - An identical incompatible schedule pin falls back with `harness_model_fallback`, and `default_model_source` equals the lane's source.
  - Each of the three schedule-origin INSERT paths stamps `schedule_pin`.
  - No automatic promoter moves the held run. Owner Resume requeues it, with `worker_id` and `hold_reason` asserted per module 2 (a) and (b), and the next claim parks it again.
- **Mutation checks:**
  - remove the effort branch: the claim test fails;
  - restore the soft fallback for `run_override`: the hold test fails;
  - remove the in-transaction predicate: the create test fails;
  - swap C's guard for `harnessModelCompatible`: the `gpt-6-luna` case fails.
- **Migration, live DB.** Modeled on `api/internal/store/base_diverged_migration_livedb_test.go`:
  - Seed, using `SET LOCAL session_replication_role = replica` to bypass the insert guard: a completed run with a non-NULL 40-hex `completion_final_head` and a contract version; a completed run without a head; a queued run; a failed run (each with a model); and a queued run without a model.
  - After Up, assert per row: the four model rows read `schedule_pin`, the completed row's head is unchanged, and the model-less row stays NULL.
  - Assert both constraints exist as NOT VALID after 00352 and validated after 00356, and that an out-of-set source is refused (SQLSTATE 23514) in both states.
  - Down drops cleanly.
- **H display and request matrix:** P's DC-P1 to DC-P8 and its `request_cases` run through H's Go and web tests.
- **CLI:** modeled on `api/cmd/uzi/run_create_harness_test.go` (flags sent only when changed, verbatim; usage errors before a request; the 422 text printed) and `run_render_harness_cost_test.go` (the hold line).
- **Web:**
  - `web/src/lib/startRun.test.ts`: wire-level default body; the force retry re-sends the options; hidden rows are dropped.
  - New `RunOptionsDialog.test.tsx`: visibility rules; a user harness change resets the model, the poll does not; a blank Other blocks Start; submitting guards, Escape included; the gate reason and the error are announced.
  - `IssueCard.test.tsx`, `IssueView.test.tsx`, `Board.test.tsx`: Options opens the page-level dialog; the one-click Start body is unchanged.
  - `RunView.test.tsx`: the hold renders its own text, not a completion panel; Resume is offered.
  - `runBadge.test.ts` and `ui.test.tsx`: tone, label and attention for the new reason (the #3194 cases).
  - Hold-reason presentation set: the web and Go CLI fixture tests over `fixtures/hold-reasons/presentation.json` (every entry visited). The three existing reasons render byte-identically before and after the refactor (existing `RunView`, `runBadge`, `ui` and CLI render tests stay green unchanged).
- **E2E sweep:** `git grep -F` the changed literals (`frozen by its schedule`, the create body keys, `run_override_unavailable`) across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`, and report any affected phase left unrun.
- **CI time:** unmeasured. The new tests are about twenty fixture-driven creates in one live-DB test, plus the claim and migration tests. Report the measured `test-api` job time before and after in the PR.

## Milestones

### M1: run choice through API, claim, hold, CLI and run page

**Blocked by:** child #3244 and child #3245 (PRD-level).

**Scope:**
- Migrations 00352 and 00356 (drafts), with the backfill and explicit schedule stamping.
- Modules 1 to 5, including the `run_override_unavailable` hold and the hold-reason presentation set (web and CLI homes, `fixtures/hold-reasons/presentation.json`) that replaces the per-site lists.
- Mock mode stamps the new fields and renders the display cases.
- The shared fixture plus its Go half.
- The "If F is on the base" items, when F is on the base.
- Docs:
  - `docs/cli.md`: synopsis, a `--model` / `--effort` bullet, the new hold.
  - `docs/worker-model.md`: a precedence item "a model you chose when starting the run", and that the run is held rather than given another model when the choice cannot run.
  - `docs/worker-effort.md`: replace "The setting is resolved when a worker claims the run, including a scheduled or already queued run and a resumed run; there is no per-schedule effort freeze" with the run choice frozen at start, the claim-time setting for runs without one, and that subagents without an effort pin inherit it.
  - `ARCHITECTURE.md`: correct the lead bullet.
  - `task docs:sync`.
- `specs/human.md` Feature #17 entry, tagged `(AI-synced YYYY-MM-DD)`: run choice at start, frozen, create-validated against the resolved harness, held if unhonorable later, wins both phases, unpinned subagents inherit its effort, resume and requeue keep it.
- Changelog fragment (Added, cite #3250).

**Acceptance:** examples 1, 2 and 5 pass against a live-DB server; DC-P1 to DC-P8 and P's `request_cases` pass in H's Go and web tests; schedule-fired runs are unchanged except their source; `task gate:api`, `gate:web` and `gate:repo` are green.

**Safe alone:** yes. The web start surfaces are untouched and send no new fields.

### M2: web Run options dialog

**Blocked by:** M1.

**Scope:**
- Prefactor first, with no behavior change: the options object, the body builder, the `api.createRun` and mock signatures, the start helper, and the wire-level body pins.
- Module 6: the dialog and the start rows on the board card and issue page; remove the inline pickers and the per-card maps.
- The mock `createRun` enforces the fixture (the web half).
- Update the `WaitOnLimit` / `MrReworkEnabled` field comments in `Handler.CreateRun`: Start run stays one click, and the optional Run options dialog from PRD #3250 carries harness, token, model and effort, not wait-on-limit or MR rework.
- `docs/board.md` (starting a run, Options); `task docs:sync`.
- `specs/human.md` entry: an optional Run options dialog beside one-click Start, tagged `(AI-synced YYYY-MM-DD)`.

**Acceptance:** examples 3 and 4 pass in the web suite and in mock mode; keyboard-only operation works (open, choose, start; Escape restores focus to Options when not submitting); `task gate:web` is green.

**Safe alone:** yes (after M1).

## Risks

- **Riskiest assumption: H r3 keeps the names P edits.** These are `resolveLeadSettings` / `LeadSettingsInput`, the `request_cases` key and the two view adapters. C r3 must likewise keep `ValidateClaudeLaneModel` and `CuratedCodexModels`. Validate first in M1 by locating them on the base. If a name moved, follow the landed code.
- **Q-claude depends on P's inputs and presentation set.** If P renames `RunModelSource` / `RunEffort` / `RunEffortSource` or the set's predicates during review, Q-claude must follow. Q-claude is blocked by P, so it sees the landed names.
- **New state, readers and writers:**
  - **Column writers:** `createRun` (`run_override` from `CreateRun`, `schedule_pin` from the two scheduled entrypoints), `CreatePromptRun`, `CreateSelfImproveRun`, the backfill. There is no UPDATE path.
  - **Column readers:** `assembleClaim` through `resolveLeadSettings`; `runToDTO` (`RunDTO` and `RunListItemDTO`; the summary is omitted on purpose); the four wire fixtures; H's two adapters; the web `Run` type; the mock API; the admin agents surface (`model` pass-through); F's `StartPreview` when F is present. Judge, cross-check, job, chat, task, `mr_rework` and `ci_fix` claims do not read them.
  - **Hold reason `run_override_unavailable`:** the writer is `ParkRunOverrideUnavailableRun`. The readers are every `hold_reason` predicate (enumerated in M1) and the named web, CLI, skill and doc surfaces.
  - **Entry point:** `POST /repos/{id}/runs` only, for cookie and Bearer callers, plus F's start-preview when present.
- **Rollout orders:**
  - api and web are separate Deployments (chart) and separate compose services. A new web on an old api gets `400 invalid request body`, shown in the dialog (fails closed). An old web on a new api is unchanged.
  - Workers need nothing new beyond H.
  - A new CLI on an old server gets a 400; an old CLI on a new server is unchanged.
  - The api is a Recreate singleton that migrates at boot. The two-step CHECK avoids the scan under ACCESS EXCLUSIVE; the backfill itself is a filtered UPDATE.
- **Rollback (goose Down):**
  - Queued P runs lose their effort choice and resolve the user's effort at the next claim.
  - P model rows read as schedule-fixed on the old binary (DC-P8 renders the same).
  - A held run stays paused with a reason the old binary does not know. The old web shows it as a completion hold (cosmetic); the owner can cancel or resume it.
  - An old binary on the new schema without Down still inserts (there is no pairing CHECK), leaving new schedule rows with a NULL source.
  - Accepted as rare.
- **Claude silently downgrading an unsupported effort:** reported by H as requested vs observed. P neither blocks nor clamps.

## Decision Log

- **User decision 2026-10-21 (P1).** Move the inline harness and token pickers into the Run options dialog; one-click Start run with defaults stays.
- **User decision 2026-10-21 (P2).** Auto-approve at start goes to #3151.
- **User decision 2026-10-21 (P3).** Only curated Codex models are allowed per run; Claude custom IDs are allowed.
- **User decision 2026-10-21 (P4).** No mock; the PRD text is the spec.
- **User decision 2026-10-21.** An explicit run choice wins in both phases (D7).
- **D1. Freeze the choice on the run row, reusing `runs.model`.** The claim already honors a frozen `runs.model`; effort gets a sibling column. Rejected: a run-options table (a join per claim for two nullable values).
- **D2. Validate shape before the forge read, and compatibility authoritatively inside the create transaction, with a pre-check when the harness is explicit.** The harness is known only after D11 resolution (the credential-override precedent, PRD #1429 D5). Rejected: a pre-transaction harness guess for the implicit case; a claim-time soft fallback.
- **D3. Use C's guards for P's new input: Claude through `ValidateClaudeLaneModel` (all five Codex role ids rejected, `gpt-6-luna` included); Codex through `CuratedCodexModels` (user decision P3). The same predicate runs at create and at claim.** New explicit override inputs use the five-id guard (coordinator ruling r3), so P is blocked by C. Schedule pins and the existing worker lanes keep the four-id `harnessModelCompatible` (C's follow-up). On Codex, `gpt-6-luna` is outside the curated set, which the `ClaimRun` capability gate also uses, so P's Codex rule needs no SQL change. Rejected: custom Codex IDs per run (widens the gate in several queries); the four-id guard for P (would let `gpt-6-luna` reach a Claude claim).
- **D4. Hold visibly (`run_override_unavailable`) a `run_override` that cannot be honored at claim; schedule pins keep their soft fallback.** An explicit choice must never be silently replaced. `resolveLeadSettings` reports it as a result field (`Unhonorable`), because the function is pure. The hold reuses the classification shape of the `credential_disabled` park, with its own sibling park query and the message written inside the park transaction. Rejected: falling back to the lane (silent substitution); a terminal failure like `errToolPackagesRejected` (loses Resume for a condition a rollback can clear); an error return from `resolveLeadSettings` (breaks H's pure contract); reusing `DroppedRunModel` (H §9 forbids it).
- **D5. Effort is the closed five-level set on both harnesses, with no model/effort pair matrix.** Codex reports a bad pair as a provider error and Claude downgrades; both are visible through H. Rejected: a pair matrix on uzi's side (a drifting copy of provider knowledge).
- **D6. The choice is frozen at create; resume, requeue and reclaim never re-resolve it; there is no edit after create.** Unset settings keep claim-time resolution. Rejected: a run-page edit control.
- **D7. The run choice wins in both phases, independently for model and effort.** Today's single pair satisfies it. Q-claude, which is blocked by P, extends it into `resolvePlanPhase` and owns the both-phases test (coordinator ruling r3).
- **D8. Start run stays one click; Run options is an optional dialog.** This honors the user's 2026-07-27 ruling. Rejected: a modal gating every start.
- **D9. One dialog on the existing `Modal`, mounted at page level, with one options type and one body builder.** Rejected: inline pickers on the narrow card; a row-plugin mechanism.
- **D10. The Default options read "Default · your settings" until F/K supply the value and source.** When F is present, P feeds F's start-preview inputs. Rejected: computing the default in the browser.
- **D11. Stamp `schedule_pin` explicitly through an entrypoint-set source argument, backfill existing rows, and add no pairing CHECK.** This keeps one spelling per source. A pairing CHECK would turn an old binary running on the new schema after a rollback without Down into failed schedule fires. `effort_source` has one allowed value and is derivable; it is kept only because the shared design specifies the pair. Rejected: deriving the source from `trigger_source`; leaving schedule rows NULL.
- **D12. Chat and Slack start cards carry no model or effort.** The chat request is model-authored, and a model must not choose a run's spend. Rejected: forwarding model or effort through `start_run`.
- **D13. P sets the lead model and the run's effort; unpinned subagents inherit them; explicit subagent pins win.** That means template model pins today and child N's effort pins later. Rejected: an "apply to every subagent" switch.
- **D14. A shared, hand-authored fixture pins the mock to the server.** Rejected: snapshotting demo data.
- **D15. No new worker capability or protocol field beyond H's.**
- **D16. CLI flags are sent verbatim and only when changed; obvious mistakes are usage errors; the CLI prints the server's `error`.**
- **D17. Successor runs do not inherit a run choice.**
- **D18. P's new inputs reject whitespace instead of trimming it; existing surfaces keep trim-then-blank.** (Shared design v4.1 §5.)
- **D19. Run-level display goes through H's `LEAD` / `LEAD_SOURCE` rows and chip, with H's label "this run"; settings-surface cells (F's start-preview) use C's (field, token) label table.** These are the two label domains of the r3 ruling. Rejected: separate `MODEL` / `EFFORT` rows and a "chosen at start" label.
- **D20. Add the CHECKs `NOT VALID`, then `VALIDATE` them in a sibling migration.** This follows the repo convention for `runs` (00204/00205, 00226/00227) and avoids a full scan under ACCESS EXCLUSIVE at boot. Rejected: inline CHECKs.
- **D21. P owns one hold-reason presentation set per language, pinned to `fixtures/hold-reasons/presentation.json`.** Coordinator ruling r3: the web and CLI deny-lists drift one site at a time (the `CompletionStatePanel` case), and Q-claude needs to add a second claim-time hold. Rejected: adding the new reason to each literal list (Q would repeat the same edit at every site).
